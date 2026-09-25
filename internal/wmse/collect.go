package wmse

import (
	"sort"
	"strings"

	"apimap/internal/linker"
)

// Normalize makes a snapshot internally consistent before it is written.
//
// A scan collects links from several sources that each have their own idea of
// what a node is: a page parser names the page it was on, a bundle analyzer
// names the script, the sandbox names the page it emulated, and the entry point
// is not a link at all because nothing ever linked to it. A file whose
// relations point at URLs that are not nodes has a graph with dangling edges,
// and an offline explorer is exactly the place where that hurts. So the entry
// point and any page that only ever appears as a source get a node of their
// own, and duplicate records for the same URL are merged instead of written
// twice.
func (s *Snapshot) Normalize(target string) error {
	// One index for the whole pass. A scan that found fifty thousand links has
	// fifty thousand source URLs to resolve, and looking each of them up by
	// walking the slice would make saving quadratic in the size of the scan -
	// the one operation that must never be the slow part of a crawl.
	idx := make(map[string]int, len(s.Links)+1)
	for i := range s.Links {
		idx[LinkKey(&s.Links[i])] = i
	}
	if target != "" {
		s.ensurePage(idx, target, 0)
	}
	// The loop re-reads the length each pass, and a page this adds has no
	// source of its own, so nothing is queued twice.
	for i := 0; i < len(s.Links); i++ {
		if src := s.Links[i].SourceURL; src != "" {
			s.ensurePage(idx, src, 0)
		}
	}
	s.mergeLinks()
	s.dedupeEdges()
	s.recordQueries()
	return nil
}

// recordQueries gives a link the query string it was requested with, when the
// scan did not already record one for it.
//
// The report annotates such a row with "(params: page=2; q=term)" and the
// recovered-params section deliberately skips any name a link already shows, so
// that a parameter is reported once and in the place that has the evidence. A
// file that dropped the query would break both: the annotation would be gone
// from the rows, and the params section would report the same name a second time
// as a finding, with nothing to tell the two apart.
func (s *Snapshot) recordQueries() {
	for i := range s.Links {
		l := &s.Links[i]
		if len(l.ParamVariants) > 0 {
			continue
		}
		if q := rawQuery(LinkKey(l)); q != "" {
			l.ParamVariants = []linker.ParamVariant{{Query: q}}
		}
	}
}

// rawQuery returns the query string of a URL, without the leading "?" and
// without the fragment.
func rawQuery(u string) string {
	i := strings.IndexByte(u, '?')
	if i < 0 {
		return ""
	}
	q := u[i+1:]
	if j := strings.IndexByte(q, '#'); j >= 0 {
		q = q[:j]
	}
	return q
}

// ensurePage returns the node id of a URL, adding a bare page node when the
// scan never recorded that URL as a link of its own. Such a node is
// deliberately empty: it asserts only that the page exists, which is all the
// relations that point at it ever claimed.
func (s *Snapshot) ensurePage(idx map[string]int, url string, depth int) int {
	if at, ok := idx[url]; ok {
		if depth > 0 {
			s.Links[at].Depth = depth
		}
		return at
	}
	s.Links = append(s.Links, linker.Link{
		HREF:     url,
		Resolved: url,
		Category: linker.CategoryWebPage,
		LinkType: linker.LinkTypeAbsolute,
		Depth:    depth,
		// The scan never listed this as a link - nothing linked to it, or it
		// is the entry point, which is not a link at all - so a reader must
		// not either.
		Synthesized: true,
	})
	idx[url] = len(s.Links) - 1
	return len(s.Links) - 1
}

// mergeLinks collapses records for the same URL into one. Two parsers reaching
// the same URL is normal, and the one that noticed the API detail is worth as
// much as the one that noticed the tag, so neither is allowed to erase the
// other: the first record wins and the rest only fill gaps.
func (s *Snapshot) mergeLinks() {
	if len(s.Links) < 2 {
		return
	}
	idx := make(map[string]int, len(s.Links))
	out := make([]linker.Link, 0, len(s.Links))
	for i := range s.Links {
		l := s.Links[i]
		at, seen := idx[LinkKey(&l)]
		if !seen {
			idx[LinkKey(&l)] = len(out)
			out = append(out, l)
			continue
		}
		mergeInto(&out[at], l)
	}
	s.Links = out
}

// mergeInto folds src into dst without overwriting anything dst already knows.
func mergeInto(dst *linker.Link, src linker.Link) {
	if dst.HREF == "" {
		dst.HREF = src.HREF
	}
	if dst.Resolved == "" {
		dst.Resolved = src.Resolved
	}
	if dst.Domain == "" {
		dst.Domain = src.Domain
	}
	if dst.Tag == "" {
		dst.Tag = src.Tag
	}
	if dst.SourceURL == "" {
		dst.SourceURL = src.SourceURL
	}
	if dst.Category == linker.CategoryUnknown {
		dst.Category = src.Category
	}
	if dst.LinkType == linker.LinkTypeUnknown {
		dst.LinkType = src.LinkType
	}
	if dst.Class == linker.ClassNormal {
		dst.Class = src.Class
	}
	// The shallowest sighting wins: it is the one closest to the entry point.
	if src.Depth > 0 && (dst.Depth == 0 || src.Depth < dst.Depth) {
		dst.Depth = src.Depth
	}
	// A node the snapshot added to give a relation an endpoint stops being
	// synthesized as soon as a real record of the same URL turns up: something
	// did link to it, and the scan listed it.
	dst.Synthesized = dst.Synthesized && src.Synthesized
	dst.HasParams = dst.HasParams || src.HasParams
	dst.ParamVariants = unionVariants(dst.ParamVariants, src.ParamVariants)
	dst.APIDetails = unionDetails(dst.APIDetails, src.APIDetails)
}

func unionVariants(a, b []linker.ParamVariant) []linker.ParamVariant {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v.Query] = true
	}
	for _, v := range b {
		if !seen[v.Query] {
			seen[v.Query] = true
			a = append(a, v)
		}
	}
	return a
}

func unionDetails(a, b []linker.APIDetail) []linker.APIDetail {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a))
	for _, d := range a {
		seen[d.MatchSource+"\x00"+d.HTTPMethod] = true
	}
	for _, d := range b {
		k := d.MatchSource + "\x00" + d.HTTPMethod
		if !seen[k] {
			seen[k] = true
			a = append(a, d)
		}
	}
	return a
}

// dedupeEdges drops relations that say the same thing twice. The derived ones
// can collide with a caller's own assertions, and a graph that repeats an edge
// reports a multiplicity it does not have.
func (s *Snapshot) dedupeEdges() {
	if len(s.Edges) == 0 {
		return
	}
	sort.SliceStable(s.Edges, func(i, j int) bool {
		a, b := s.Edges[i], s.Edges[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	out := s.Edges[:0]
	for i, e := range s.Edges {
		if i > 0 {
			p := s.Edges[i-1]
			if p.Kind == e.Kind && p.From == e.From && p.To == e.To {
				// Keep the larger weight: it is the stronger statement.
				if e.Weight > p.Weight {
					out[len(out)-1] = e
				}
				continue
			}
		}
		out = append(out, e)
	}
	s.Edges = out
}
