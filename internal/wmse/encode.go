package wmse

import (
	"sort"

	"apimap/internal/contract"
	"apimap/internal/linker"
)

// Per-link record flags. Most links have no tag, no content type, no parameter
// variants and no API detail, so one byte replaces four varints on the majority
// of records.
const (
	lfHasParams  uint8 = 1 << 0
	lfHasVarSet  uint8 = 1 << 1
	lfHasDetails uint8 = 1 << 2
	lfHasTag     uint8 = 1 << 3
	lfHasUnres   uint8 = 1 << 4
	lfHasPage    uint8 = 1 << 5
	// lfSynthesized marks a node that exists only to give a relation an
	// endpoint. It is one bit, and it is what lets a reader print the same link
	// table the scan printed rather than the same link table plus every page
	// nothing ever linked to.
	lfSynthesized uint8 = 1 << 6
)

// Endpoint-level flags.
const (
	efUnobserved uint8 = 1 << 0
	efInferred   uint8 = 1 << 1
)

// Field-level flags.
const (
	ffConstant  uint8 = 1 << 0
	ffInferred  uint8 = 1 << 1
	ffNullable  uint8 = 1 << 2
	ffHasConst  uint8 = 1 << 3
	ffHasSample uint8 = 1 << 4
)

// writeMeta emits the flat scan description.
func (st *encoderState) writeMeta() {
	var w byteWriter
	keys := make([]string, 0, len(st.snap.Meta))
	for k := range st.snap.Meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.uvarint(uint64(len(keys)))
	for _, k := range keys {
		w.uvarint(uint64(st.str(k)))
		w.uvarint(uint64(st.str(st.snap.Meta[k])))
	}
	st.put(SecMeta, w.buf)
}

// writeStats emits the aggregate counts.
//
// The closed value sets are written as full slots rather than as the keys that
// happen to be present: a zero count and a missing count must be the same
// thing, or a round trip would quietly lose a category.
func (st *encoderState) writeStats() {
	s := st.snap.Stats
	var w byteWriter
	w.uvarint(uint64(s.Total))
	w.uvarint(uint64(s.Resolved))
	w.uvarint(uint64(s.Unresolved))
	w.uvarint(uint64(s.WithParams))

	w.uvarint(uint64(catSlots))
	for cat := linker.CategoryUnknown; cat < catSlots; cat++ {
		w.uvarint(uint64(s.ByCategory[cat]))
	}
	w.uvarint(uint64(typeSlots))
	for lt := linker.LinkTypeUnknown; lt < typeSlots; lt++ {
		w.uvarint(uint64(s.ByLinkType[lt]))
	}
	w.uvarint(uint64(classSlots))
	for cl := linker.ClassNormal; cl < classSlots; cl++ {
		w.uvarint(uint64(s.ByClass[cl]))
	}
	tags := make([]string, 0, len(s.ByTag))
	for t := range s.ByTag {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	w.uvarint(uint64(len(tags)))
	for _, t := range tags {
		w.uvarint(uint64(st.str(t)))
		w.uvarint(uint64(s.ByTag[t]))
	}
	st.put(SecStats, w.buf)
}

// Slot counts for the closed enumerations in Stats. They are the reason a
// count is stored as a code rather than as a word.
const (
	catSlots   = linker.CategoryDynamic + 1
	typeSlots  = linker.LinkTypeWeb + 1
	classSlots = linker.ClassNoise + 1
)

// writeLinks emits every discovered link as a fixed record, ordered so that
// neighbours in the file are neighbours in the site.
func (st *encoderState) writeLinks() {
	e := st.enc
	links := st.links
	for i := range links {
		if links[i].Domain != "" {
			e.hostCount[e.Str(links[i].Domain)]++
		}
	}

	var w byteWriter
	w.uvarint(uint64(len(links)))
	for i := range links {
		l := &links[i]

		var flags uint8
		if l.HasParams {
			flags |= lfHasParams
		}
		if l.Resolved == "" {
			flags |= lfHasUnres
		}
		if l.Tag != "" {
			flags |= lfHasTag
		}
		if len(l.ParamVariants) > 0 {
			flags |= lfHasVarSet
		}
		if len(l.APIDetails) > 0 {
			flags |= lfHasDetails
		}
		page, isPage := st.pageSet[LinkKey(l)]
		if isPage {
			flags |= lfHasPage
		}
		if l.Synthesized {
			flags |= lfSynthesized
		}

		w.byte(flags)
		w.uvarint(uint64(e.Str(l.HREF)))
		w.uvarint(uint64(e.Str(l.Resolved)))
		w.uvarint(uint64(e.Str(l.Domain)))
		w.uvarint(uint64(l.Category))
		w.uvarint(uint64(l.LinkType))
		w.uvarint(uint64(l.Class))
		w.uvarint(uint64(l.Depth))
		if flags&lfHasTag != 0 {
			w.uvarint(uint64(e.Str(l.Tag)))
		}
		if flags&lfHasVarSet != 0 {
			varSet := make([]uint32, 0, len(l.ParamVariants))
			for _, pv := range l.ParamVariants {
				varSet = append(varSet, e.StrList([]string{pv.Query}))
			}
			w.uvarint(uint64(len(varSet)))
			for _, id := range varSet {
				w.uvarint(uint64(id))
			}
		}
		if flags&lfHasDetails != 0 {
			pairs := make([][2]string, 0, len(l.APIDetails))
			args := make([]string, 0, len(l.APIDetails))
			for _, d := range l.APIDetails {
				pairs = append(pairs, [2]string{d.MatchSource, d.HTTPMethod})
				args = append(args, d.Arguments)
			}
			w.uvarint(uint64(e.Pairs(pairs)))
			// The argument lists go in beside the pairs, in the same order:
			// one list of the scan's own strings, which is what keeps a
			// repeated call signature to one interned id instead of one per
			// link. The pairs themselves stay pairs, because the match source
			// and the method are two fields and a third would be paid for by
			// every header and response field the pair table also holds.
			w.uvarint(uint64(e.StrList(args)))
		}
		if flags&lfHasPage != 0 {
			w.uvarint(uint64(page.Depth))
			w.uvarint(uint64(e.Str(page.ContentType)))
			w.uvarint(uint64(page.Links))
		}
	}
	st.put(SecLinks, w.buf)
}

// writeEdges emits the relation graph, one delta run per kind.
func (st *encoderState) writeEdges() {
	edges := st.deriveEdges()

	var w byteWriter
	byKind := make([][]Edge, EdgeCount)
	for _, ed := range edges {
		if int(ed.Kind) == 0 || int(ed.Kind) >= EdgeCount {
			continue
		}
		byKind[ed.Kind] = append(byKind[ed.Kind], ed)
	}
	// The count is of kinds actually present, so a scan with no relations at
	// all costs one byte rather than a run header per kind.
	present := 0
	for _, es := range byKind {
		if len(es) > 0 {
			present++
		}
	}
	w.uvarint(uint64(present))
	for kind := 1; kind < EdgeCount; kind++ {
		es := byKind[kind]
		if len(es) == 0 {
			continue
		}
		sort.SliceStable(es, func(i, j int) bool {
			if es[i].From != es[j].From {
				return es[i].From < es[j].From
			}
			return es[i].To < es[j].To
		})
		w.uvarint(uint64(kind))
		runs := buildRuns(es)
		w.uvarint(uint64(len(runs)))
		var prevFrom int64
		for _, run := range runs {
			w.intvarint(int64(run.from) - prevFrom)
			prevFrom = int64(run.from)
			w.uvarint(uint64(run.weight))
			w.uvarint(uint64(len(run.tos)))
			var prevTo int64
			for _, to := range run.tos {
				w.intvarint(int64(to) - prevTo)
				prevTo = int64(to)
			}
		}
	}
	st.put(SecEdges, w.buf)
}

type edgeRun struct {
	from   uint32
	weight uint32
	tos    []uint32
}

// buildRuns groups a sorted edge list by source. One run costs two varints of
// overhead no matter how many targets it carries, so a page with fifty outgoing
// links costs a fraction of what fifty records would.
func buildRuns(edges []Edge) []edgeRun {
	var runs []edgeRun
	for _, e := range edges {
		if len(runs) == 0 || runs[len(runs)-1].from != e.From {
			runs = append(runs, edgeRun{from: e.From, weight: e.Weight, tos: []uint32{e.To}})
			continue
		}
		runs[len(runs)-1].tos = append(runs[len(runs)-1].tos, e.To)
	}
	return runs
}

// deriveEdges fills in the relations the model does not carry explicitly, so
// the writer has one place that decides what is a relation. It appends to a
// working list rather than to the snapshot, so writing a snapshot twice does
// not double the graph.
func (st *encoderState) deriveEdges() []Edge {
	links := st.links
	var edges []Edge

	// A page and the links it contributed.
	for i := range links {
		l := &links[i]
		from, ok := st.node(LinkKey(l))
		if !ok {
			continue
		}
		if l.SourceURL != "" {
			if src, ok := st.node(l.SourceURL); ok {
				edges = append(edges, Edge{Kind: EdgeSource, From: src, To: from})
			}
		}
	}

	// The crawl tree itself: a queued page sits one level below the page that
	// linked it, and the edge only exists if the crawler really fetched it.
	pages := make(map[string]bool, len(st.snap.Pages))
	for _, p := range st.snap.Pages {
		pages[p.URL] = true
	}
	for i := range links {
		l := &links[i]
		if l.SourceURL == "" || !pages[LinkKey(l)] {
			continue
		}
		to, ok := st.node(LinkKey(l))
		if !ok {
			continue
		}
		if from, ok := st.node(l.SourceURL); ok {
			edges = append(edges, Edge{Kind: EdgeCrawled, From: from, To: to})
		}
	}

	// Pattern membership.
	for gi := range st.snap.Groups {
		for _, m := range st.snap.Groups[gi].Members {
			if to, ok := st.node(m); ok {
				edges = append(edges, Edge{Kind: EdgePattern, From: uint32(gi), To: to})
			}
		}
	}

	// A link and the contract inferred for it. Endpoints are keyed by URL, so
	// this is a string identity test, not a guess.
	epByPath := make(map[string]int, len(st.snap.Endpoints))
	for ei := range st.snap.Endpoints {
		if _, dup := epByPath[st.snap.Endpoints[ei].Path]; !dup {
			epByPath[st.snap.Endpoints[ei].Path] = ei
		}
	}
	for i := range links {
		ei, ok := epByPath[LinkKey(&links[i])]
		if !ok {
			continue
		}
		if from, ok := st.node(LinkKey(&links[i])); ok {
			edges = append(edges, Edge{Kind: EdgeContract, From: from, To: uint32(ei)})
		}
	}

	// A recovered parameter name and the endpoint it feeds.
	for pi := range st.snap.Params {
		for _, ep := range st.snap.Params[pi].Endpoints {
			ei, ok := epByPath[ep]
			if !ok {
				continue
			}
			edges = append(edges, Edge{Kind: EdgeParam, From: uint32(pi), To: uint32(ei)})
		}
	}

	// Whatever the collector already asserted stays: it is not re-derived.
	return append(edges, st.snap.Edges...)
}

// writeGroups emits the URL patterns; membership lives in the edge section.
func (st *encoderState) writeGroups() {
	var w byteWriter
	w.uvarint(uint64(len(st.snap.Groups)))
	for i := range st.snap.Groups {
		g := &st.snap.Groups[i]
		w.uvarint(uint64(st.str(g.Pattern)))
		w.uvarint(uint64(st.str(g.Domain)))
		w.uvarint(uint64(g.Count))
		w.uvarint(uint64(len(g.Vars)))
		for _, v := range g.Vars {
			w.uvarint(uint64(st.str(v.Kind)))
			w.uvarint(uint64(st.enc.StrList(v.Values)))
			// Sort a copy: the pattern's own slices are the caller's.
			nums := append([]int64(nil), v.Nums...)
			sort.Slice(nums, func(a, b int) bool { return nums[a] < nums[b] })
			w.uvarint(uint64(len(nums)))
			var prev int64
			for _, n := range nums {
				w.intvarint(n - prev)
				prev = n
			}
		}
		// The member list is also written as edges, but only for members that
		// have a link node. A pattern whose members were all filtered away has
		// edges and no members, and the list is what keeps that case honest.
		w.uvarint(uint64(st.enc.StrList(g.Members)))
	}
	st.put(SecGroups, w.buf)
}

// writeEndpoints emits the inferred API contracts, field for field. Everything
// that is a list of literals points at the dictionaries, so an endpoint that
// only differs in its path costs a path.
func (st *encoderState) writeEndpoints() {
	e := st.enc
	var w byteWriter
	w.uvarint(uint64(len(st.snap.Endpoints)))
	for i := range st.snap.Endpoints {
		ep := &st.snap.Endpoints[i]
		var flags uint8
		if ep.Unobserved {
			flags |= efUnobserved
		}
		if ep.MethodInferred {
			flags |= efInferred
		}
		w.byte(flags)
		w.uvarint(uint64(e.Str(ep.Path)))
		w.uvarint(uint64(ep.Calls))
		w.uvarint(uint64(e.StrList(ep.Methods)))
		writeFields(&w, e, ep.Query)
		writeFields(&w, e, ep.Headers)
		w.uvarint(uint64(len(ep.Bodies)))
		for _, b := range ep.Bodies {
			w.uvarint(uint64(e.Str(b.Kind)))
			w.uvarint(uint64(e.Str(b.MIME)))
			w.uvarint(uint64(e.StrList(b.Methods)))
			w.uvarint(uint64(e.Str(b.Sample)))
			writeFields(&w, e, b.Fields)
		}
		w.uvarint(uint64(e.Pairs(pairsOfResponse(ep.Response))))
		w.uvarint(uint64(e.StrList(ep.Raw)))
	}
	st.put(SecEndpoints, w.buf)
}

func writeFields(w *byteWriter, e *encoder, fields []contract.Field) {
	w.uvarint(uint64(len(fields)))
	for i := range fields {
		f := &fields[i]
		var flags uint8
		if f.Constant {
			flags |= ffConstant
		}
		if f.Inferred {
			flags |= ffInferred
		}
		if f.Nullable {
			flags |= ffNullable
		}
		if f.ConstVal != "" {
			flags |= ffHasConst
		}
		w.byte(flags)
		w.uvarint(uint64(e.Str(f.Name)))
		w.uvarint(uint64(e.Str(f.Kind)))
		w.uvarint(uint64(e.StrList(f.Values)))
		if flags&ffHasConst != 0 {
			w.uvarint(uint64(e.Str(f.ConstVal)))
		}
	}
}

// writeObservations emits the raw request evidence behind the contracts. They
// are derivable from the contracts but not the other way round: a contract
// collapses many calls, and losing them would make the file unable to answer
// "what did the first call actually send".
func (st *encoderState) writeObservations() {
	e := st.enc
	var w byteWriter
	w.uvarint(uint64(len(st.snap.Observations)))
	for i := range st.snap.Observations {
		o := &st.snap.Observations[i]
		w.uvarint(uint64(e.Str(o.URL)))
		w.uvarint(uint64(e.Str(o.Method)))
		w.byte(obsFlags(o))
		w.uvarint(uint64(e.Str(o.Body)))
		w.uvarint(uint64(e.Pairs(nameValuePairs(o.Headers))))
		w.uvarint(uint64(e.Pairs(pairsOfResponse(o.ResponseFields))))
		w.uvarint(uint64(e.Pairs(nameValuePairs(o.InferredQuery))))
	}
	st.put(SecObservations, w.buf)
}

const (
	ofEndpointOnly uint8 = 1 << 0
	ofInferred     uint8 = 1 << 1
)

func obsFlags(o *contract.Observation) uint8 {
	var f uint8
	if o.EndpointOnly {
		f |= ofEndpointOnly
	}
	if o.MethodInferred {
		f |= ofInferred
	}
	return f
}

// writeParams emits the parameter names recovered from request builders.
func (st *encoderState) writeParams() {
	e := st.enc
	var w byteWriter
	w.uvarint(uint64(len(st.snap.Params)))
	for i := range st.snap.Params {
		p := &st.snap.Params[i]
		w.uvarint(uint64(e.Str(p.Name)))
		w.uvarint(uint64(e.Str(p.Carrier)))
		w.uvarint(uint64(p.Kind))
		w.uvarint(uint64(p.Owner))
		w.uvarint(uint64(p.Count))
		w.uvarint(uint64(e.StrList(p.Endpoints)))
		w.uvarint(uint64(e.StrList(p.Docs)))
	}
	st.put(SecParams, w.buf)
}

// writeEmulation emits the sandbox aggregate and the calls it intercepted.
func (st *encoderState) writeEmulation() {
	var w byteWriter
	em := st.snap.Emulation
	if em == nil {
		w.uvarint(0)
		st.put(SecEmulation, w.buf)
		return
	}
	w.uvarint(1)
	w.uvarint(uint64(em.Scripts))
	w.uvarint(uint64(em.Calls))
	w.uvarint(uint64(em.Abandoned))
	var flags uint8
	if em.Disabled {
		flags = 1
	}
	w.byte(flags)

	types := make([]string, 0, len(em.ByType))
	for t := range em.ByType {
		types = append(types, t)
	}
	sort.Strings(types)
	w.uvarint(uint64(len(types)))
	for _, t := range types {
		w.uvarint(uint64(st.str(t)))
		w.uvarint(uint64(em.ByType[t]))
	}
	w.uvarint(uint64(len(em.Errors)))
	for _, err := range em.Errors {
		w.uvarint(uint64(st.str(err)))
	}
	w.uvarint(uint64(len(em.List)))
	for _, c := range em.List {
		w.uvarint(uint64(st.str(c.URL)))
		w.uvarint(uint64(st.str(c.RawURL)))
		w.uvarint(uint64(st.str(c.Method)))
		w.uvarint(uint64(st.str(c.Initiator)))
		w.uvarint(uint64(st.str(c.Type)))
		w.uvarint(uint64(st.str(c.Body)))
		w.uvarint(uint64(st.enc.Pairs(nameValuePairs(callHeaders(c)))))
	}
	st.put(SecEmulation, w.buf)
}
