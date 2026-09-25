package wmse

import (
	"testing"

	"apimap/internal/linker"
)

// nodeKeys returns the identity of every link, in order, so a test can say what
// the file contains without depending on the writer's ordering.
func nodeKeys(links []linker.Link) []string {
	out := make([]string, 0, len(links))
	for i := range links {
		out = append(out, LinkKey(&links[i]))
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestNormalizeGivesEveryRelationANode is the property the offline graph
// depends on. A relation whose endpoints are not nodes cannot be walked, and an
// explorer that cannot walk its graph is a list of lines.
func TestNormalizeGivesEveryRelationANode(t *testing.T) {
	snap := &Snapshot{
		Links: []linker.Link{
			// A link whose source page was never recorded as a link itself: the
			// crawler fetched it, but nothing in the site linked to it.
			{HREF: "/deep", Resolved: "https://example.com/deep", SourceURL: "https://example.com/mid", Depth: 1},
		},
		Pages: []Page{{URL: "https://example.com/mid", Depth: 0}},
	}
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	keys := nodeKeys(snap.Links)
	for _, want := range []string{
		"https://example.com/",     // the entry point, which nothing linked to
		"https://example.com/mid",  // a source page with no link record
		"https://example.com/deep", // the link itself
	} {
		if !contains(keys, want) {
			t.Errorf("no node for %s; nodes are %v", want, keys)
		}
	}
	// Every source edge must now point at a node.
	has := map[string]bool{}
	for _, k := range keys {
		has[k] = true
	}
	for i := range snap.Links {
		if src := snap.Links[i].SourceURL; src != "" && !has[src] {
			t.Errorf("link %s has source %s, which is not a node", LinkKey(&snap.Links[i]), src)
		}
	}
	// The two nodes the scan never listed as links say so. A reader prints a
	// link table, and a table that listed every node of a file would show the
	// entry point and every unlinked page as if the site linked to them - and
	// the report would no longer be the report of the run that made the file.
	for i := range snap.Links {
		l := &snap.Links[i]
		switch LinkKey(l) {
		case "https://example.com/", "https://example.com/mid":
			if !l.Synthesized {
				t.Errorf("%s was added by the snapshot and does not say so", LinkKey(l))
			}
		default:
			if l.Synthesized {
				t.Errorf("%s was found by the scan and is marked as synthesized", LinkKey(l))
			}
		}
	}
}

// TestNormalizeRecordsAQueries is the other half of what a link table needs. The
// scan annotates a row with the queries it was called with, and its recovered
// parameters section deliberately skips any name a row already shows, so a file
// that kept the link but not its query would drop the annotation and then report
// the same parameter a second time as a finding.
func TestNormalizeRecordsQueries(t *testing.T) {
	snap := &Snapshot{
		Links: []linker.Link{
			{HREF: "/item", Resolved: "https://example.com/item", Depth: 1},
			{HREF: "/item?id=7", Resolved: "https://example.com/item?id=7", Depth: 1, HasParams: true},
			{HREF: "/other?a=1#frag", Resolved: "https://example.com/other?a=1", Depth: 1},
		},
	}
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	byKey := map[string]linker.Link{}
	for i := range snap.Links {
		byKey[LinkKey(&snap.Links[i])] = snap.Links[i]
	}
	for url, want := range map[string]string{
		"https://example.com/item":      "",
		"https://example.com/item?id=7": "id=7",
		"https://example.com/other?a=1": "a=1",
	} {
		var got string
		if q := byKey[url].ParamVariants; len(q) > 0 {
			got = q[0].Query
		}
		if got != want {
			t.Errorf("%s: query %q, want %q", url, got, want)
		}
	}
	if got := byKey["https://example.com/item?id=7"].ParamVariants; len(got) != 1 || got[0].Query != "id=7" {
		t.Errorf("a record that already had a variant was overwritten: %v", got)
	}
	// The fragment is not part of a query: "?a=1#frag" is the request a=1, and
	// storing the fragment with it would make the same request look like two.
	if got := rawQuery("/p?a=1#frag"); got != "a=1" {
		t.Errorf("rawQuery: got %q, want a=1", got)
	}
}

// TestNormalizeMergesDuplicateRecords covers the ordinary case of two analyzers
// reaching the same URL. Keeping both would waste space; keeping only the first
// would throw away what the second one noticed.
func TestNormalizeMergesDuplicateRecords(t *testing.T) {
	url := "https://example.com/x"
	snap := &Snapshot{
		Links: []linker.Link{
			{HREF: "/x", Resolved: url, Depth: 3, Category: linker.CategoryAPI,
				APIDetails: []linker.APIDetail{{MatchSource: "call", HTTPMethod: "POST"}}},
			{HREF: "https://example.com/x", Resolved: url, Depth: 1,
				Tag: "api", HasParams: true, ParamVariants: []linker.ParamVariant{{Query: "a=1"}}},
			{HREF: "/x", Resolved: url, Depth: 2, APIDetails: []linker.APIDetail{{MatchSource: "fetch", HTTPMethod: "GET"}}},
		},
	}
	if err := snap.Normalize(url); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(snap.Links) != 1 {
		t.Fatalf("got %d records, want 1 merged: %v", len(snap.Links), nodeKeys(snap.Links))
	}
	got := snap.Links[0]
	if got.Depth != 1 {
		t.Errorf("depth: got %d, want the shallowest sighting (1)", got.Depth)
	}
	if got.Tag != "api" {
		t.Errorf("tag: got %q, want the record that had one", got.Tag)
	}
	if !got.HasParams || len(got.ParamVariants) != 1 {
		t.Errorf("params: got %v / %v", got.HasParams, got.ParamVariants)
	}
	if got.Category != linker.CategoryAPI {
		t.Errorf("category: got %v, want API", got.Category)
	}
	// Both API details survive: one analyzer knew the verb, the other knew the
	// call shape, and neither fact subsumes the other.
	if len(got.APIDetails) != 2 {
		t.Errorf("api details: got %d, want both", len(got.APIDetails))
	}
}

// TestNormalizeIsIdempotent matters because a caller may normalize, write, and
// then normalize again before a second write; a second pass that grows the file
// would mean the output depends on how many times it was saved.
func TestNormalizeIsIdempotent(t *testing.T) {
	snap := &Snapshot{
		Links: []linker.Link{
			{HREF: "/a", Resolved: "https://example.com/a", SourceURL: "https://example.com/", Depth: 1},
			{HREF: "/a", Resolved: "https://example.com/a", SourceURL: "https://example.com/", Depth: 1},
		},
		Pages: []Page{{URL: "https://example.com/", Depth: 0}},
		Edges: []Edge{{Kind: EdgeSource, From: 0, To: 1}, {Kind: EdgeSource, From: 0, To: 1}},
	}
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	first := nodeKeys(snap.Links)
	firstEdges := len(snap.Edges)
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatalf("second Normalize: %v", err)
	}
	if second := nodeKeys(snap.Links); len(second) != len(first) {
		t.Errorf("second pass changed the nodes: %v -> %v", first, second)
	}
	if len(snap.Edges) != firstEdges {
		t.Errorf("second pass changed the edges: %d -> %d", firstEdges, len(snap.Edges))
	}
}

// TestDedupeEdgesKeepsTheStrongerStatement covers a collision between a derived
// relation and one the caller asserted: a graph must not claim a multiplicity it
// does not have, but it must not lose the stronger claim either.
func TestDedupeEdgesKeepsTheStrongerStatement(t *testing.T) {
	snap := &Snapshot{Edges: []Edge{
		{Kind: EdgeCrawled, From: 0, To: 1, Weight: 1},
		{Kind: EdgeCrawled, From: 0, To: 1, Weight: 4},
		{Kind: EdgeSource, From: 0, To: 1, Weight: 1},
		{Kind: EdgeSource, From: 0, To: 2, Weight: 1},
	}}
	snap.dedupeEdges()
	if len(snap.Edges) != 3 {
		t.Fatalf("got %d edges, want 3 distinct relations: %+v", len(snap.Edges), snap.Edges)
	}
	for _, e := range snap.Edges {
		if e.Kind == EdgeCrawled && e.Weight != 4 {
			t.Errorf("crawled edge weight: got %d, want the stronger 4", e.Weight)
		}
	}
}

// TestNormalizeOnAnEmptySnapshot keeps the degenerate case honest: a scan that
// found nothing must still produce a readable file.
func TestNormalizeOnAnEmptySnapshot(t *testing.T) {
	snap := &Snapshot{}
	if err := snap.Normalize(""); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(snap.Links) != 0 {
		t.Errorf("an empty scan gained %d nodes: %v", len(snap.Links), nodeKeys(snap.Links))
	}
	got := roundTrip(t, snap)
	if len(got.Links) != 0 {
		t.Errorf("round trip produced %d links", len(got.Links))
	}
}
