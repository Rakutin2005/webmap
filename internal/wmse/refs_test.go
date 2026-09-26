package wmse

import (
	"strings"
	"testing"

	"apimap/internal/linker"
)

// TestRefsArchiveRoundTrips writes a snapshot with references, marshals and
// reloads it, and reads a .ref file back. It is the whole feature in one test:
// the scan records a reference, the file stores it, and a reader gets the
// evidence out again.
func TestRefsArchiveRoundTrips(t *testing.T) {
	ix := linker.NewRefIndex()
	body := `<a href="/admin">x</a>` + "\n" + `<img src="/logo.png">`
	ix.Add("https://example.com/index.html", "text/html", linker.Reference{
		Offset: 9, Line: 1, Column: 10, Snippet: `<a href="/admin">`,
	})
	ix.Add("https://example.com/index.html", "text/html", linker.Reference{
		Offset: 40, Line: 2, Column: 16, Snippet: `<img src="/logo.png">`,
	})

	snap := testSnapshot()
	if err := BuildRefsArchive(snap, ix); err != nil {
		t.Fatalf("BuildRefsArchive: %v", err)
	}
	if len(snap.ArchiveNames) != 1 {
		t.Fatalf("archive names: got %v, want one .ref", snap.ArchiveNames)
	}
	if got := snap.ArchiveNames[0]; !strings.HasSuffix(got, ".ref") {
		t.Errorf("archive name %q should end in .ref", got)
	}

	got := roundTrip(t, snap)
	if len(got.ArchiveNames) != 1 {
		t.Fatalf("after round trip: got %v", got.ArchiveNames)
	}
	data, err := got.ArchiveFileBySource("https://example.com/index.html")
	if err != nil {
		t.Fatalf("ArchiveFileBySource: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"# source: https://example.com/index.html",
		"# type: text/html",
		"# references: 2",
		"9\t1:10",
		"40\t2:16",
		"<a href=",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ref file missing %q:\n%s", want, text)
		}
	}
	_ = body
}

// TestRefsArchiveIsOptional: a scan without -refs writes no archive, and a file
// that has none still loads. The section is additive - a file that lacks it is a
// file, not an error.
func TestRefsArchiveIsOptional(t *testing.T) {
	snap := testSnapshot()
	if err := BuildRefsArchive(snap, nil); err != nil {
		t.Fatal(err)
	}
	if snap.Archive != nil || snap.ArchiveNames != nil {
		t.Error("no references should mean no archive")
	}
	got := roundTrip(t, snap)
	if len(got.ArchiveNames) != 0 {
		t.Errorf("a file with no archive came back with %v", got.ArchiveNames)
	}
	if _, err := got.ArchiveFile("x.ref"); err == nil {
		t.Error("reading from a file with no archive should be an error")
	}
}

// TestRefNameIsStableAndDistinct pins the naming: the same source always gets
// the same name (a reader looks a reference up by URL), and two sources that
// differ only by query do not collide.
func TestRefNameIsStableAndDistinct(t *testing.T) {
	if a, b := refName("https://example.com/a"), refName("https://example.com/a"); a != b {
		t.Errorf("naming is not stable: %q vs %q", a, b)
	}
	if a, b := refName("https://example.com/s?x=1"), refName("https://example.com/s?x=2"); a == b {
		t.Errorf("two sources differing only by query collided: %q", a)
	}
	if got := refName("https://example.com/"); !strings.HasSuffix(got, ".ref") {
		t.Errorf("root source: got %q", got)
	}
}

// TestRefsArchiveKeepsEveryDocument: several source documents each get their own
// .ref file, and a reference in one does not leak into another.
func TestRefsArchiveKeepsEveryDocument(t *testing.T) {
	ix := linker.NewRefIndex()
	ix.Add("https://example.com/a.html", "text/html", linker.Reference{Offset: 1, Line: 1, Column: 2, Snippet: "a"})
	ix.Add("https://example.com/b.html", "text/html", linker.Reference{Offset: 3, Line: 1, Column: 4, Snippet: "b"})

	snap := testSnapshot()
	if err := BuildRefsArchive(snap, ix); err != nil {
		t.Fatal(err)
	}
	if len(snap.ArchiveNames) != 2 {
		t.Fatalf("want one .ref per document, got %v", snap.ArchiveNames)
	}
	a, err := snap.ArchiveFileBySource("https://example.com/a.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a), "a") || strings.Contains(string(a), "snippet: b") {
		t.Errorf("a.html ref file has the wrong content:\n%s", a)
	}
}
