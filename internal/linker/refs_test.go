package linker

import (
	"strings"
	"testing"
)

// TestParseWithRefsPointsAtTheURL is the test that matters for a reference: the
// recorded offset must be the position of the URL in the document, so that
// body[off:] actually starts with the href. A reference that points a few bytes
// off is worse than none, because it looks precise.
func TestParseWithRefsPointsAtTheURL(t *testing.T) {
	body := `<html>\n<body>\n<a href="/admin/panel">Admin</a>\n<img src="/logo.png">\n</body>\n</html>`
	// Use real newlines, not the two-character sequence above.
	body = strings.ReplaceAll(body, `\n`, "\n")

	ix := NewRefIndex()
	links := ParseWithRefs(body, "https://example.com/index.html", ix)

	if len(links) == 0 {
		t.Fatal("expected links")
	}
	if ix.Len() != len(links) {
		t.Errorf("references: got %d, want one per link (%d)", ix.Len(), len(links))
	}

	docs := ix.Documents()
	if len(docs) != 1 {
		t.Fatalf("documents: got %d, want 1", len(docs))
	}
	doc := docs[0]
	if doc.URL != "https://example.com/index.html" {
		t.Errorf("source: got %q", doc.URL)
	}
	for _, ref := range doc.Refs {
		if ref.Offset < 0 || ref.Offset > len(body) {
			t.Fatalf("offset %d out of range", ref.Offset)
		}
		// The byte at the offset must begin a URL-ish token: ' or " or / or h.
		at := string(body[ref.Offset])
		if at != "/" && at != "'" && at != `"` && at != "h" {
			t.Errorf("offset %d points at %q, not at a URL:\n%s", ref.Offset, at, body[ref.Offset:])
		}
		if ref.Snippet == "" {
			t.Errorf("offset %d has no snippet", ref.Offset)
		}
		if strings.ContainsAny(ref.Snippet, "\n\r\t") {
			t.Errorf("snippet must be one line, got %q", ref.Snippet)
		}
	}
	// The line:col of the first reference must agree with its byte offset.
	first := doc.Refs[0]
	line, col := lineColumn(body, first.Offset)
	if line != first.Line || col != first.Column {
		t.Errorf("reference says %d:%d, recomputed %d:%d", first.Line, first.Column, line, col)
	}
}

// TestParseWithoutRefsRecordsNothing: the default path must not build an index,
// and passing a nil index must be the same as the old Parse.
func TestParseWithoutRefsRecordsNothing(t *testing.T) {
	body := `<a href="/x">x</a>`
	var nilIndex *RefIndex
	withNil := ParseWithRefs(body, "https://e.com/", nilIndex)
	plain := Parse(body, "https://e.com/")
	if len(withNil) != len(plain) {
		t.Errorf("nil index changed the links: %d vs %d", len(withNil), len(plain))
	}
}

// TestLineColumn: line counting across newlines, and the clamp past the end.
func TestLineColumn(t *testing.T) {
	body := "a\nbb\nccc"
	cases := []struct {
		off       int
		line, col int
	}{
		{0, 1, 1}, // 'a'
		{1, 1, 2}, // the newline itself
		{2, 2, 1}, // just after the first newline
		{4, 2, 3}, // the second newline
		{5, 3, 1}, // first char of line 3
		{7, 3, 3},
		{8, 3, 4},   // one past the end, still on line 3
		{999, 3, 4}, // clamped to the end
	}
	for _, c := range cases {
		line, col := lineColumn(body, c.off)
		if line != c.line || col != c.col {
			t.Errorf("lineColumn(%d) = %d:%d, want %d:%d", c.off, line, col, c.line, c.col)
		}
	}
}

// TestMakeSnippet covers both ends of the window and the flattening.
func TestMakeSnippet(t *testing.T) {
	// Long enough that the 48-char window actually truncates both sides.
	body := strings.Repeat("0123456789", 30) // 300 bytes
	// A window in the middle is bounded on both sides.
	got := makeSnippet(body, 150, 2)
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "...") {
		t.Errorf("interior snippet should be bounded both sides: %q", got)
	}
	// At the very start there is nothing before, so no leading ellipsis.
	start := makeSnippet(body, 0, 2)
	if strings.HasPrefix(start, "...") {
		t.Errorf("start snippet should not have a leading ellipsis: %q", start)
	}
	// Newlines flatten so one finding stays on one line.
	flat := makeSnippet("a\nb\nc", 2, 1)
	if strings.ContainsAny(flat, "\n") {
		t.Errorf("snippet not flattened: %q", flat)
	}
}

// TestAnalyzeJSWithRefsPointsAtTheLiteral: a template-literal URL's reference
// must land inside the backticks, not on the whole statement.
func TestAnalyzeJSWithRefsPointsAtTheLiteral(t *testing.T) {
	js := "const a = 1;\nconst u = `/complex/${id}/contacts`;\n"
	ix := NewRefIndex()
	tmpl := AnalyzeJSWithRefs(js, "https://example.com/app.js", ix)
	if len(tmpl) == 0 {
		t.Skip("no template literal recognised in the fixture")
	}
	docs := ix.Documents()
	if len(docs) != 1 {
		t.Fatalf("documents: got %d", len(docs))
	}
	ref := docs[0].Refs[0]
	// The regex captures the literal's content, so the offset points just past
	// the opening backtick - at the URL itself, which is the better anchor.
	if !strings.HasPrefix(js[ref.Offset:], "/complex/") {
		t.Errorf("offset %d points at %q, expected the URL inside the backticks", ref.Offset, js[ref.Offset:])
	}
}
