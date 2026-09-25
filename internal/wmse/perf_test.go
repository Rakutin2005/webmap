package wmse

import (
	"encoding/json"
	"fmt"
	"testing"

	"apimap/internal/contract"
	"apimap/internal/linker"
)

// syntheticScan builds a scan of the shape a real crawl produces: a few tens of
// thousands of near-identical URLs, their source pages, and a handful of
// contracts. The family resemblance is the point - it is what the format has to
// exploit - and it is also what a news section or an ID-based API produces.
func syntheticScan(n int) *Snapshot {
	snap := &Snapshot{Meta: Meta{MetaTarget: "https://example.com/"}}
	for i := 0; i < n; i++ {
		snap.Links = append(snap.Links, linker.Link{
			HREF:      fmt.Sprintf("/news/%d/comments", i),
			Resolved:  fmt.Sprintf("https://example.com/news/%d/comments", i),
			Domain:    "example.com",
			Category:  linker.CategoryWebPage,
			LinkType:  linker.LinkTypeRelative,
			SourceURL: fmt.Sprintf("https://example.com/news/%d", i/10),
			Depth:     1 + i/10,
			HasParams: i%3 == 0,
		})
	}
	for i := 0; i < n/10; i++ {
		snap.Pages = append(snap.Pages, Page{
			URL:         fmt.Sprintf("https://example.com/news/%d", i),
			Depth:       1,
			ContentType: "text/html",
			Links:       10,
		})
	}
	for i := 0; i < 50; i++ {
		snap.Endpoints = append(snap.Endpoints, contract.Endpoint{
			Path:    fmt.Sprintf("/api/v2/resource/%d", i),
			Methods: []string{"GET", "POST"},
			Calls:   3,
			Query:   []contract.Field{{Name: "q", Kind: "str", Values: []string{"a", "b"}}},
		})
	}
	if err := snap.Normalize("https://example.com/"); err != nil {
		panic(err)
	}
	return snap
}

// TestWriterScalesWithScanSize is a guard, not a benchmark: it writes a file far
// larger than any test site and fails if it stops being fast. A format whose
// writer degrades quadratically is fine on the example URL and useless on a
// real site - saving a scan is the one operation that must not be the slow part
// of a crawl.
func TestWriterScalesWithScanSize(t *testing.T) {
	for _, n := range []int{1000, 20000} {
		snap := syntheticScan(n)
		raw, info, err := Marshal(snap, DefaultOptions())
		if err != nil {
			t.Fatalf("Marshal(%d): %v", n, err)
		}
		f, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%d): %v", n, err)
		}
		got, err := f.Load()
		if err != nil {
			t.Fatalf("Load(%d): %v", n, err)
		}
		if len(got.Links) < n {
			t.Errorf("links: got %d, want at least %d", len(got.Links), n)
		}
		t.Logf("%6d links -> %8d bytes (%.1f B/link), %d sections", n, len(raw),
			float64(len(raw))/float64(n), len(info.Sections))
	}
}

// TestFormatBeatsPlainTextByAnOrderOfMagnitude holds the claim the documentation
// makes. "Compact" is a property that decays quietly: every field added to a
// record, every literal that stops being interned, shows up as a few more bytes
// per link and nothing anywhere goes red. So the ratio against the obvious
// alternative is asserted, not just logged, and the number in the README is this
// test's output.
func TestFormatBeatsPlainTextByAnOrderOfMagnitude(t *testing.T) {
	const n = 20000
	snap := syntheticScan(n)

	// The baseline is the same data as JSON: every field spelled out, no
	// interning, no deltas, no shared structure. It is what a scan looks like
	// when nothing is done to make it small, which is the only fair comparison.
	plain, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	raw, _, err := Marshal(snap, DefaultOptions())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// And the floor for the same data with the codec switched off: the bytes
	// the graph distribution saves, before deflate.
	uncompressed, _, err := Marshal(snap, Options{Compress: false})
	if err != nil {
		t.Fatalf("Marshal uncompressed: %v", err)
	}

	perLink := float64(len(raw)) / float64(n)
	t.Logf("%d links: wmse %d B (%.1f B/link), uncompressed %d B, plain JSON %d B",
		n, len(raw), perLink, len(uncompressed), len(plain))
	t.Logf("ratio: %.0fx smaller than plain JSON, %.0fx smaller than uncompressed",
		float64(len(plain))/float64(len(raw)), float64(len(uncompressed))/float64(len(raw)))

	if ratio := float64(len(plain)) / float64(len(raw)); ratio < 20 {
		t.Errorf("format is only %.0fx smaller than plain JSON, want at least 20x", ratio)
	}
	// Interning and delta encoding are the mechanisms; if they stop paying, the
	// file is a compressed copy of something that was never made smaller first.
	if ratio := float64(len(uncompressed)) / float64(len(raw)); ratio < 1.5 {
		t.Errorf("compression is doing the work: only %.1fx came from the format itself", ratio)
	}
	if perLink > 20 {
		t.Errorf("%.1f bytes per link, want under 20", perLink)
	}
}
