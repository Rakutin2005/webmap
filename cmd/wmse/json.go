package main

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"

	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// jsonEncoder writes the snapshot as JSON.
//
// It is hand-written rather than a struct marshal for two reasons: the keys
// should read like the rest of the tool ("url" rather than "HREF"), and the
// output has to stream, because a saved scan of a large site is big and a
// reader that has to hold the whole document twice is not much use when the
// point is piping it somewhere else.
type jsonEncoder struct {
	w *bufio.Writer
	// written has one entry per open container, saying whether that container
	// already holds something. It is all the separator logic there is: a comma
	// goes in front of every member after the first.
	written []bool
	// afterKey says the last thing written was a key, so the value about to be
	// written belongs to it and must not take a separator of its own.
	afterKey bool
	// err is the first write failure; encoding stops and it is reported once.
	err error
}

func newJSONEncoder(w io.Writer) *jsonEncoder {
	return &jsonEncoder{w: bufio.NewWriterSize(w, 1<<16)}
}

func (e *jsonEncoder) close() error {
	if e.err != nil {
		return e.err
	}
	return e.w.Flush()
}

func (e *jsonEncoder) put(s string) {
	if e.err != nil {
		return
	}
	_, e.err = e.w.WriteString(s)
}

// sep emits the comma a new member needs, and records that this container is no
// longer empty. It is called exactly once per member, by whoever starts it: a
// key for an object member, elem for an array element, open for a container.
func (e *jsonEncoder) sep() {
	n := len(e.written)
	if n == 0 {
		return
	}
	if e.written[n-1] {
		e.put(",")
	}
	e.written[n-1] = true
}

// valueSep emits the separator a value needs. A value that belongs to a key
// already has its comma, so only an unclaimed one asks for a separator.
func (e *jsonEncoder) valueSep() {
	if e.afterKey {
		e.afterKey = false
		return
	}
	e.sep()
}

// open starts a container. It is used both as an object's member value and as
// an array element, so it cannot tell which separator rule applies and defers
// to valueSep.
func (e *jsonEncoder) open(c byte) {
	e.valueSep()
	e.put(string(c))
	e.written = append(e.written, false)
}

func (e *jsonEncoder) shut(c byte) {
	if len(e.written) == 0 {
		return
	}
	e.written = e.written[:len(e.written)-1]
	e.put(string(c))
}

func (e *jsonEncoder) objStart() { e.open('{') }
func (e *jsonEncoder) objEnd()   { e.shut('}') }
func (e *jsonEncoder) arrStart() { e.open('[') }
func (e *jsonEncoder) arrEnd()   { e.shut(']') }

// key starts an object member. The value that follows is written with sval, not
// with sep, because the comma belongs to the pair and not to the value.
func (e *jsonEncoder) key(k string) {
	e.sep()
	e.put(jsonString(k))
	e.put(":")
	e.afterKey = true
}

func (e *jsonEncoder) sval(v string) {
	e.valueSep()
	e.put(jsonString(v))
}

// elem writes one array element.
func (e *jsonEncoder) elem(v string) {
	e.sep()
	e.put(v)
}

func (e *jsonEncoder) str(k, v string) {
	e.key(k)
	e.sval(v)
}

func (e *jsonEncoder) num(k string, v int) {
	e.key(k)
	e.valueSep()
	e.put(strconv.Itoa(v))
}

func (e *jsonEncoder) boolean(k string, v bool) {
	e.key(k)
	e.valueSep()
	if v {
		e.put("true")
		return
	}
	e.put("false")
}

func (e *jsonEncoder) stringArray(k string, vs []string) {
	e.key(k)
	e.arrStart()
	for _, v := range vs {
		e.elem(jsonString(v))
	}
	e.arrEnd()
}

// strMap writes a map of string keys to int counts, sorted so the output is
// stable between runs of the same file.
func (e *jsonEncoder) strCountMap(k string, m map[string]int) {
	e.key(k)
	e.objStart()
	for _, name := range sortedKeys(m) {
		e.num(name, m[name])
	}
	e.objEnd()
}

// viewJSON writes the whole snapshot as one JSON document.
//
// The `json` verb is the escape hatch of the two others: the report is meant to
// be read by a person and drops what a person does not need (which page a link
// was seen on, every observation behind a contract), while the whole point of
// the file is that nothing the scan learned is thrown away. Anything the report
// had to leave out is here, for whoever is going to do with it what a person
// would not.
//
// It writes nothing to stdout before the document is finished, so a piped
// consumer either gets a complete document or a write error.
func viewJSON(snap *wmse.Snapshot, w io.Writer) error {
	e := newJSONEncoder(w)
	if err := e.encode(snap); err != nil {
		return err
	}
	e.put("\n")
	return e.close()
}

func (e *jsonEncoder) encode(snap *wmse.Snapshot) error {
	e.objStart()

	e.str("format", "wmse")
	e.str("tool", snap.Meta[wmse.MetaTool])
	e.str("toolVersion", snap.Meta[wmse.MetaVersion])
	e.str("target", snap.Meta[wmse.MetaTarget])
	e.str("created", snap.Meta[wmse.MetaCreated])
	e.key("meta")
	e.objStart()
	for _, k := range sortedStringKeys(snap.Meta) {
		e.str(k, snap.Meta[k])
	}
	e.objEnd()

	e.key("stats")
	e.objStart()
	e.num("total", snap.Stats.Total)
	e.num("resolved", snap.Stats.Resolved)
	e.num("unresolved", snap.Stats.Unresolved)
	e.num("withParams", snap.Stats.WithParams)
	e.key("byCategory")
	e.objStart()
	for cat := linker.CategoryUnknown; cat <= linker.CategoryDynamic; cat++ {
		e.num(cat.String(), snap.Stats.ByCategory[cat])
	}
	e.objEnd()
	e.key("byLinkType")
	e.objStart()
	for lt := linker.LinkTypeUnknown; lt <= linker.LinkTypeWeb; lt++ {
		e.num(lt.String(), snap.Stats.ByLinkType[lt])
	}
	e.objEnd()
	e.key("byClass")
	e.objStart()
	for cl := linker.ClassNormal; cl <= linker.ClassNoise; cl++ {
		e.num(cl.String(), snap.Stats.ByClass[cl])
	}
	e.objEnd()
	e.strCountMap("byTag", snap.Stats.ByTag)
	e.objEnd()

	e.key("pages")
	e.arrStart()
	for i := range snap.Pages {
		p := &snap.Pages[i]
		e.objStart()
		e.str("url", p.URL)
		e.num("depth", p.Depth)
		e.str("contentType", p.ContentType)
		e.num("links", p.Links)
		e.objEnd()
	}
	e.arrEnd()

	e.key("links")
	e.arrStart()
	for i := range snap.Links {
		l := &snap.Links[i]
		e.objStart()
		e.str("href", l.HREF)
		e.str("url", wmse.LinkKey(l))
		e.str("domain", l.Domain)
		e.str("category", l.Category.String())
		e.str("linkType", l.LinkType.String())
		e.str("class", l.Class.String())
		e.num("depth", l.Depth)
		e.boolean("hasParams", l.HasParams)
		e.str("source", l.SourceURL)
		e.str("tag", l.Tag)
		if len(l.ParamVariants) > 0 {
			e.key("paramQueries")
			e.arrStart()
			for _, pv := range l.ParamVariants {
				e.elem(jsonString(pv.Query))
			}
			e.arrEnd()
		}
		if len(l.APIDetails) > 0 {
			e.key("apiDetails")
			e.arrStart()
			for _, d := range l.APIDetails {
				e.objStart()
				e.str("matchSource", d.MatchSource)
				e.str("method", d.HTTPMethod)
				e.str("arguments", d.Arguments)
				e.objEnd()
			}
			e.arrEnd()
		}
		e.objEnd()
	}
	e.arrEnd()

	e.key("edges")
	e.arrStart()
	for _, ed := range snap.Edges {
		e.objStart()
		e.str("kind", wmse.EdgeName(ed.Kind))
		e.num("from", int(ed.From))
		e.num("to", int(ed.To))
		if ed.Weight > 1 {
			e.num("weight", int(ed.Weight))
		}
		e.objEnd()
	}
	e.arrEnd()

	e.key("patterns")
	e.arrStart()
	for i := range snap.Groups {
		g := &snap.Groups[i]
		e.objStart()
		e.str("pattern", g.Pattern)
		e.str("domain", g.Domain)
		e.num("count", g.Count)
		e.stringArray("members", g.Members)
		e.key("variables")
		e.arrStart()
		for _, v := range g.Vars {
			e.objStart()
			e.str("kind", v.Kind)
			e.stringArray("values", v.Values)
			if len(v.Nums) > 0 {
				e.key("numbers")
				e.arrStart()
				for _, n := range v.Nums {
					e.elem(strconv.FormatInt(n, 10))
				}
				e.arrEnd()
			}
			e.objEnd()
		}
		e.arrEnd()
		e.objEnd()
	}
	e.arrEnd()

	e.key("endpoints")
	e.arrStart()
	for i := range snap.Endpoints {
		e.encodeEndpoint(&snap.Endpoints[i])
	}
	e.arrEnd()

	e.key("observations")
	e.arrStart()
	for i := range snap.Observations {
		o := &snap.Observations[i]
		e.objStart()
		e.str("url", o.URL)
		e.str("method", o.Method)
		e.boolean("endpointOnly", o.EndpointOnly)
		e.boolean("methodInferred", o.MethodInferred)
		e.str("body", o.Body)
		e.key("headers")
		e.objStart()
		for _, h := range o.Headers {
			e.str(h.Name, h.Value)
		}
		e.objEnd()
		e.key("inferredQuery")
		e.arrStart()
		for _, q := range o.InferredQuery {
			e.elem(jsonString(q.Name))
		}
		e.arrEnd()
		e.key("responseFields")
		e.arrStart()
		for _, rf := range o.ResponseFields {
			e.objStart()
			e.str("path", rf.Path)
			e.str("kind", rf.Kind)
			e.objEnd()
		}
		e.arrEnd()
		e.objEnd()
	}
	e.arrEnd()

	e.key("params")
	e.arrStart()
	for i := range snap.Params {
		p := &snap.Params[i]
		e.objStart()
		e.str("name", p.Name)
		e.str("kind", p.Kind.String())
		e.str("owner", p.Owner.String())
		e.str("carrier", p.Carrier)
		e.num("count", p.Count)
		e.stringArray("endpoints", p.Endpoints)
		e.stringArray("documents", p.Docs)
		e.objEnd()
	}
	e.arrEnd()

	if snap.Emulation != nil {
		em := snap.Emulation
		e.key("emulation")
		e.objStart()
		e.num("scripts", em.Scripts)
		e.num("calls", em.Calls)
		e.num("abandoned", int(em.Abandoned))
		e.boolean("disabled", em.Disabled)
		e.strCountMap("byType", em.ByType)
		e.stringArray("errors", em.Errors)
		e.key("intercepted")
		e.arrStart()
		for _, c := range em.List {
			e.objStart()
			e.str("url", c.URL)
			e.str("rawUrl", c.RawURL)
			e.str("method", c.Method)
			e.str("type", c.Type)
			e.str("initiator", c.Initiator)
			e.str("body", c.Body)
			e.key("headers")
			e.objStart()
			for _, h := range c.Headers {
				e.str(h[0], h[1])
			}
			e.objEnd()
			e.objEnd()
		}
		e.arrEnd()
		e.objEnd()
	}

	// The sidecar archive is opaque here: the JSON names what is inside it
	// without inlining the files, because the .ref files are the record of
	// where each link was found and a consumer that wants them reads them by
	// name out of the archive. The bytes themselves would double the document
	// for something the file already holds.
	if len(snap.ArchiveNames) > 0 {
		e.key("archive")
		e.objStart()
		e.stringArray("files", snap.ArchiveNames)
		e.num("bytes", len(snap.Archive))
		e.objEnd()
	}

	e.objEnd()
	e.put("\n")
	return e.err
}

func (e *jsonEncoder) encodeEndpoint(ep *contract.Endpoint) {
	e.objStart()
	e.str("url", ep.Path)
	e.num("calls", ep.Calls)
	e.stringArray("methods", ep.Methods)
	e.boolean("unobserved", ep.Unobserved)
	e.boolean("methodInferred", ep.MethodInferred)
	e.key("query")
	e.fields(ep.Query)
	e.key("headers")
	e.fields(ep.Headers)
	e.key("bodies")
	e.arrStart()
	for i := range ep.Bodies {
		b := &ep.Bodies[i]
		e.objStart()
		e.str("kind", b.Kind)
		e.str("mime", b.MIME)
		e.stringArray("methods", b.Methods)
		e.str("sample", b.Sample)
		e.key("fields")
		e.fields(b.Fields)
		e.objEnd()
	}
	e.arrEnd()
	e.key("response")
	e.arrStart()
	for _, rf := range ep.Response {
		e.objStart()
		e.str("path", rf.Path)
		e.str("kind", rf.Kind)
		e.objEnd()
	}
	e.arrEnd()
	if len(ep.Raw) > 0 {
		e.stringArray("evidence", ep.Raw)
	}
	e.objEnd()
}

func (e *jsonEncoder) fields(fs []contract.Field) {
	e.arrStart()
	for i := range fs {
		f := &fs[i]
		e.objStart()
		e.str("name", f.Name)
		e.str("kind", f.Kind)
		e.stringArray("values", f.Values)
		if f.Constant {
			e.boolean("constant", true)
			e.str("const", f.ConstVal)
		}
		if f.Inferred {
			e.boolean("inferred", true)
		}
		if f.Nullable {
			e.boolean("nullable", true)
		}
		e.objEnd()
	}
	e.arrEnd()
}

// jsonString quotes a string. The escapes are the ones JSON requires plus the
// control characters a raw request body can contain; everything else is passed
// through as UTF-8, which is what a reader of the file will expect to see.
func jsonString(s string) string {
	var b []byte
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b = append(b, '\\', '"')
			case '\\':
				b = append(b, '\\', '\\')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				if c < 0x20 {
					b = append(b, fmt.Sprintf(`\u%04x`, c)...)
				} else {
					b = append(b, c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// A request body captured as bytes can hold anything; an invalid
			// byte must not produce invalid JSON.
			b = append(b, `�`...)
			i++
			continue
		}
		b = append(b, s[i:i+size]...)
		i += size
	}
	b = append(b, '"')
	return string(b)
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStringKeys(m wmse.Meta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
