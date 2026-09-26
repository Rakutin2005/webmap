package wmse

import (
	"sort"
	"strconv"
	"strings"

	"apimap/internal/contract"
	"apimap/internal/linker"
)

// interner is the single vocabulary the format speaks. It is written twice over
// the same snapshot: once by a collector, which only learns which literals the
// file needs, and once by the encoder, which has the sorted table and can
// answer with a position in it. Because both passes run the identical walk, the
// table is complete before the first id is handed out and no id ever has to be
// rewritten.
type interner interface {
	// Str returns the table id of one literal.
	Str(s string) uint32
	// StrList interns an ordered list of literals (method sets, observed
	// values, member lists) and returns its dictionary id.
	StrList(items []string) uint32
	// Pairs interns an ordered list of literal pairs and returns the id of the
	// pair list (headers, response fields, request details).
	Pairs(items [][2]string) uint32
}

// collector records the literals a walk touches. Pass one.
type collector struct {
	strs   map[string]struct{}
	lists  [][]string
	pairs  [][][2]string
	seenL  map[string]uint32
	seenP  map[string]uint32
	nLists int
	nPairs int
}

// newCollector creates the pass-one interner. Dictionary ids start at 1: zero
// is reserved to mean "no list", which is what lets a record omit a field
// entirely instead of storing a length for it.
func newCollector() *collector {
	return &collector{
		strs:  map[string]struct{}{},
		seenL: map[string]uint32{},
		seenP: map[string]uint32{},
		lists: make([][]string, 1),
		pairs: make([][][2]string, 1),
	}
}

func (c *collector) Str(s string) uint32 {
	c.strs[s] = struct{}{}
	return 0
}

// StrList records an ordered list of literals. The elements have to reach the
// table too: a dictionary entry is a list of table positions, so an element
// only a list refers to would otherwise dangle.
func (c *collector) StrList(items []string) uint32 {
	if len(items) == 0 {
		return 0
	}
	for _, s := range items {
		c.Str(s)
	}
	key := joinStrings(items)
	if id, ok := c.seenL[key]; ok {
		return id
	}
	id := uint32(len(c.lists))
	c.lists = append(c.lists, append([]string(nil), items...))
	c.seenL[key] = id
	return id
}

// Pairs records an ordered list of literal pairs, with the same rule as
// StrList: both halves are table entries.
func (c *collector) Pairs(items [][2]string) uint32 {
	if len(items) == 0 {
		return 0
	}
	for _, p := range items {
		c.Str(p[0])
		c.Str(p[1])
	}
	key := joinPairs(items)
	if id, ok := c.seenP[key]; ok {
		return id
	}
	id := uint32(len(c.pairs))
	c.pairs = append(c.pairs, append([][2]string(nil), items...))
	c.seenP[key] = id
	return id
}

// encoder is the second pass: it hands out real ids from a table that was
// sorted up front, and builds the shared dictionaries.
type encoder struct {
	idx map[string]uint32

	lists     [][]uint32
	listsSeen map[string]uint32

	pairs     [][2]uint32
	pairsSeen map[[2]uint32]uint32

	// pairLists are ordered lists of pair ids, which is how a whole set of
	// headers or response fields is stored once and referenced by many
	// records.
	pairLists     [][]uint32
	pairListsSeen map[string]uint32

	// nodeIndex is the url -> link node map, needed to resolve relation edges.
	nodeIndex map[string]uint32

	// hostCount is the per-domain link tally, which the string table alone
	// cannot express.
	hostCount map[uint32]uint32
}

func newEncoder(idx map[string]uint32) *encoder {
	return &encoder{
		idx:           idx,
		listsSeen:     map[string]uint32{},
		pairsSeen:     map[[2]uint32]uint32{},
		pairListsSeen: map[string]uint32{},
		// Slot zero is the "absent" dictionary entry, so ids start at one.
		lists:     make([][]uint32, 1, 16),
		pairLists: make([][]uint32, 1, 16),
		nodeIndex: map[string]uint32{},
		hostCount: map[uint32]uint32{},
	}
}

func (e *encoder) Str(s string) uint32 {
	id, ok := e.idx[s]
	if !ok {
		// Unreachable: the collector saw every literal the walk reaches.
		panic("wmse: string not collected: " + s)
	}
	return id
}

func (e *encoder) StrList(items []string) uint32 {
	if len(items) == 0 {
		return 0
	}
	ids := make([]uint32, len(items))
	for i, s := range items {
		ids[i] = e.Str(s)
	}
	key := joinIDs(ids)
	if id, ok := e.listsSeen[key]; ok {
		return id
	}
	id := uint32(len(e.lists))
	e.lists = append(e.lists, ids)
	e.listsSeen[key] = id
	return id
}

func (e *encoder) Pairs(items [][2]string) uint32 {
	if len(items) == 0 {
		return 0
	}
	ids := make([]uint32, 0, len(items))
	for _, p := range items {
		ids = append(ids, e.Pair(p[0], p[1]))
	}
	key := joinIDs(ids)
	if id, ok := e.pairListsSeen[key]; ok {
		return id
	}
	id := uint32(len(e.pairLists))
	e.pairLists = append(e.pairLists, ids)
	e.pairListsSeen[key] = id
	return id
}

// Pair interns a single literal pair and returns its id in the pair table.
func (e *encoder) Pair(a, b string) uint32 {
	key := [2]uint32{e.Str(a), e.Str(b)}
	if id, ok := e.pairsSeen[key]; ok {
		return id
	}
	id := uint32(len(e.pairs))
	e.pairs = append(e.pairs, key)
	e.pairsSeen[key] = id
	return id
}

func joinIDs(ids []uint32) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(strconv.FormatUint(uint64(id), 10))
		b.WriteByte(',')
	}
	return b.String()
}

func joinStrings(items []string) string { return strings.Join(items, "\x00") }

func joinPairs(items [][2]string) string {
	var b strings.Builder
	for _, p := range items {
		b.WriteString(p[0])
		b.WriteByte(0x01)
		b.WriteString(p[1])
		b.WriteByte(0x02)
	}
	return b.String()
}

// walk visits every literal in the snapshot in a fixed order. Both passes call
// it, so the collector's table and the encoder's lookups cannot drift apart.
func walk(s *Snapshot, in interner) {
	// The empty string is a real entry: an absent tag, an unresolved link or
	// a missing content type is written as id 0 rather than as a special case
	// in every record.
	in.Str("")

	keys := make([]string, 0, len(s.Meta))
	for k := range s.Meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		in.Str(k)
		in.Str(s.Meta[k])
	}

	for t := range s.Stats.ByTag {
		in.Str(t)
	}

	// The sidecar archive's entry names are string-table entries like any
	// other literal, so they are interned in the collect pass and referenced by
	// id in the encode pass.
	for _, name := range s.ArchiveNames {
		in.Str(name)
	}

	for i := range s.Links {
		l := &s.Links[i]
		in.Str(l.HREF)
		in.Str(l.Resolved)
		in.Str(l.Domain)
		in.Str(l.Tag)
		for _, p := range l.ParamVariants {
			in.StrList([]string{p.Query})
		}
		if len(l.APIDetails) > 0 {
			pairs := make([][2]string, 0, len(l.APIDetails))
			args := make([]string, 0, len(l.APIDetails))
			for _, d := range l.APIDetails {
				pairs = append(pairs, [2]string{d.MatchSource, d.HTTPMethod})
				args = append(args, d.Arguments)
			}
			in.Pairs(pairs)
			in.StrList(args)
		}
	}

	for i := range s.Groups {
		g := &s.Groups[i]
		in.Str(g.Pattern)
		in.Str(g.Domain)
		for _, v := range g.Vars {
			in.Str(v.Kind)
			in.StrList(v.Values)
		}
		in.StrList(g.Members)
	}

	for i := range s.Endpoints {
		e := &s.Endpoints[i]
		in.Str(e.Path)
		in.StrList(e.Methods)
		walkFields(in, e.Query)
		walkFields(in, e.Headers)
		for _, b := range e.Bodies {
			in.Str(b.Kind)
			in.Str(b.MIME)
			in.StrList(b.Methods)
			in.Str(b.Sample)
			walkFields(in, b.Fields)
		}
		in.Pairs(pairsOfResponse(e.Response))
		in.StrList(e.Raw)
	}

	for i := range s.Observations {
		o := &s.Observations[i]
		in.Str(o.URL)
		in.Str(o.Method)
		in.Str(o.Body)
		in.Pairs(nameValuePairs(o.Headers))
		in.Pairs(pairsOfResponse(o.ResponseFields))
		in.Pairs(nameValuePairs(o.InferredQuery))
	}

	for i := range s.Params {
		p := &s.Params[i]
		in.Str(p.Name)
		in.Str(p.Carrier)
		in.StrList(p.Endpoints)
		in.StrList(p.Docs)
	}

	if s.Emulation != nil {
		em := s.Emulation
		for t := range em.ByType {
			in.Str(t)
		}
		for _, err := range em.Errors {
			in.Str(err)
		}
		for _, c := range em.List {
			in.Str(c.URL)
			in.Str(c.RawURL)
			in.Str(c.Method)
			in.Str(c.Initiator)
			in.Str(c.Type)
			in.Str(c.Body)
			in.Pairs(nameValuePairs(callHeaders(c)))
		}
	}

	for i := range s.Pages {
		in.Str(s.Pages[i].URL)
		in.Str(s.Pages[i].ContentType)
	}
}

func walkFields(in interner, fields []contract.Field) {
	for _, f := range fields {
		in.Str(f.Name)
		in.Str(f.Kind)
		in.StrList(f.Values)
		in.Str(f.ConstVal)
	}
}

func pairsOfResponse(fields []contract.ResponseField) [][2]string {
	if len(fields) == 0 {
		return nil
	}
	out := make([][2]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, [2]string{f.Path, f.Kind})
	}
	return out
}

func nameValuePairs(nv []contract.NameValue) [][2]string {
	if len(nv) == 0 {
		return nil
	}
	out := make([][2]string, 0, len(nv))
	for _, p := range nv {
		out = append(out, [2]string{p.Name, p.Value})
	}
	return out
}

func callHeaders(c Call) []contract.NameValue {
	if len(c.Headers) == 0 {
		return nil
	}
	out := make([]contract.NameValue, 0, len(c.Headers))
	for _, h := range c.Headers {
		out = append(out, contract.NameValue{Name: h[0], Value: h[1]})
	}
	return out
}

// sectionPayloads is the finished, still-uncompressed body of every section.
type sectionPayloads map[uint8][]byte

// encoderState carries the two passes plus the node bookkeeping that turns a
// URL into a graph node id.
type encoderState struct {
	snap     *Snapshot
	enc      *encoder
	sections sectionPayloads
	// ordered is the physical section order, fixed by the format.
	ordered []uint8

	// links is the link records in the order they are written, and nodeOf maps
	// a URL to its position in it. Edges use those positions, so both are
	// built once, here, and never derived twice.
	links  []linker.Link
	nodeOf map[string]uint32

	// pageSet lets a link record carry the page facts (depth, content type,
	// outgoing count) of the crawl that produced it, without a second lookup
	// at read time.
	pageSet map[string]Page
}

func newEncoderState(snap *Snapshot, idx map[string]uint32) *encoderState {
	st := &encoderState{
		snap:     snap,
		enc:      newEncoder(idx),
		sections: sectionPayloads{},
		ordered: []uint8{
			SecMeta, SecStats, SecStrings, SecLinks, SecEdges, SecGroups,
			SecEndpoints, SecObservations, SecParams, SecEmulation, SecDicts,
			SecArchive, SecSealed,
		},
		links:   sortedLinks(snap.Links),
		nodeOf:  make(map[string]uint32, len(snap.Links)),
		pageSet: make(map[string]Page, len(snap.Pages)),
	}
	// The signature section is added only for a file that has one. A section that
	// is present but empty would have to be read as "this file makes a claim that
	// is blank", which is a third thing - and the one nobody means.
	if snap.Signature != nil {
		st.ordered = append(st.ordered, SecSignature)
	}
	for i := range st.links {
		if _, dup := st.nodeOf[LinkKey(&st.links[i])]; !dup {
			st.nodeOf[LinkKey(&st.links[i])] = uint32(i)
		}
	}
	for _, p := range snap.Pages {
		st.pageSet[p.URL] = p
	}
	return st
}

// node is the node id of a URL, or false when the scan has no such node.
func (st *encoderState) node(url string) (uint32, bool) {
	id, ok := st.nodeOf[url]
	return id, ok
}

func (st *encoderState) put(id uint8, b []byte) { st.sections[id] = b }

func (st *encoderState) str(s string) uint32 { return st.enc.Str(s) }

// sortedLinks orders the link records so that neighbours in the file are
// neighbours in the site. Grouping by URL makes repeated domain strings and
// repeated category codes compress into each other, and it leaves the edge
// section free to assume that a related link sits close to its neighbour.
func sortedLinks(links []linker.Link) []linker.Link {
	out := make([]linker.Link, len(links))
	copy(out, links)
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := LinkKey(&out[i]), LinkKey(&out[j])
		if ki != kj {
			return ki < kj
		}
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].HREF < out[j].HREF
	})
	return out
}

// writeStrings emits the sorted, front-coded string table. Front coding is
// what pays for the fact that ids are positions: URLs in a scan share long
// prefixes ("https://host", "/static/js/"), and neighbouring table entries
// store only their difference.
func (st *encoderState) writeStrings(sorted []string) {
	var w byteWriter
	w.uvarint(uint64(len(sorted)))
	prev := ""
	for _, s := range sorted {
		shared := commonPrefix(prev, s)
		w.uvarint(uint64(shared))
		w.rawString(s[shared:])
		prev = s
	}
	st.put(SecStrings, w.buf)
}

// writeDicts emits the shared dictionaries. Each is delta encoded against its
// own previous element, which for a sorted table is nearly free.
func (st *encoderState) writeDicts() {
	e := st.enc
	var w byteWriter

	// Slot zero of both dictionaries is the reserved "absent" entry and is
	// never written: ids are one-based, so a count of real entries is all the
	// reader needs to place them.
	w.uvarint(uint64(len(e.lists) - 1))
	for _, l := range e.lists[1:] {
		w.uvarint(uint64(len(l)))
		var prev int64
		for _, id := range l {
			w.intvarint(int64(id) - prev)
			prev = int64(id)
		}
	}

	w.uvarint(uint64(len(e.pairs)))
	var pa, pb int64
	for _, p := range e.pairs {
		w.intvarint(int64(p[0]) - pa)
		pa = int64(p[0])
		w.intvarint(int64(p[1]) - pb)
		pb = int64(p[1])
	}

	w.uvarint(uint64(len(e.pairLists) - 1))
	for _, l := range e.pairLists[1:] {
		w.uvarint(uint64(len(l)))
		var prev int64
		for _, id := range l {
			w.intvarint(int64(id) - prev)
			prev = int64(id)
		}
	}

	st.put(SecDicts, w.buf)
}

func commonPrefix(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}
