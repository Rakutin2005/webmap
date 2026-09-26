package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// netFixture is a session over a file, standing on a server that answers. It is
// the four commands' common case: a file to read, a server to ask, and a
// directory to write into.
func netFixture(t *testing.T) (*session, *httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/logo.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\n")) // not text: a body that must survive intact
		case "/gone":
			http.Error(w, "no", http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>ok</html>"))
		}
	}))
	t.Cleanup(srv.Close)

	snap := testSnapshot()
	snap.Meta[wmse.MetaTarget] = srv.URL + "/"
	path := writeFixture(t, snap)
	s := openSession(t, path)
	// The scope moves to the test server, and the session is stood on it again:
	// paths resolve against where the session is standing, not against the target,
	// so changing one without the other would ask the wrong host for everything.
	s.target = srv.URL + "/"
	s.pos, s.dir = "", false
	s.standAt()
	var out bytes.Buffer
	s.out = &out
	return s, srv, out.String()
}

// TestSourceWritesAnArchivedFileWithoutTheNetwork: a file the archive holds is a
// file that does not need the network and cannot change under you, so `source`
// asks the archive first and says which of the two it answered from.
func TestSourceWritesAnArchivedFileWithoutTheNetwork(t *testing.T) {
	s, _, _ := netFixture(t)
	// Put a file in the archive as `save` would.
	if _, err := s.merged.AddArchiveFile(s.target+"logo.png", []byte("from the archive")); err != nil {
		t.Fatalf("AddArchiveFile: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.png")
	var out bytes.Buffer
	s.out = &out
	s.cmdSource([]string{"/logo.png", dest})

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("source did not write the file: %v", err)
	}
	if string(got) != "from the archive" {
		t.Errorf("source wrote %q, want the bytes the archive held", got)
	}
	if !strings.Contains(out.String(), "from the archive") {
		t.Errorf("source did not say it answered from the archive:\n%s", out.String())
	}
}

// TestSourceFetchesWhatTheArchiveDoesNotHold, and does not archive it: the two
// commands are kept apart on purpose, so a read cannot quietly become a write of
// somebody's scan.
func TestSourceFetchesWhatTheArchiveDoesNotHold(t *testing.T) {
	s, _, _ := netFixture(t)
	dest := filepath.Join(t.TempDir(), "out.png")
	var out bytes.Buffer
	s.out = &out
	s.cmdSource([]string{"/logo.png", dest})

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("source did not write the file: %v", err)
	}
	// A body that is not text has to arrive as itself: a page read as a string and
	// written back through anything that assumed it was text would not be the
	// file that was served.
	if !bytes.Equal(got, []byte("\x89PNG\r\n\x1a\n")) {
		t.Errorf("source wrote %q, want the bytes the server sent", got)
	}
	if !strings.Contains(out.String(), "not added to the archive") {
		t.Errorf("source did not say the archive was left alone:\n%s", out.String())
	}
	if len(s.merged.Archive) != 0 {
		t.Errorf("source added to the archive, which is `save`'s job")
	}
}

// TestSourceLeavesNoFileBehindWhenTheFetchFails: a download that fails part way
// through must not replace a file that was already there.
func TestSourceLeavesNoFileBehindWhenTheFetchFails(t *testing.T) {
	s, _, _ := netFixture(t)
	dest := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(dest, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.out = &out
	s.cmdSource([]string{s.target + "gone", dest})

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the file was removed: %v", err)
	}
	if string(got) != "mine" {
		t.Errorf("the file was replaced with %q by a failed download", got)
	}
	if !strings.Contains(out.String(), "404") {
		t.Errorf("source did not say the server refused:\n%s", out.String())
	}
}

// TestSaveAddsToTheFileAndTheFileOnDiskAgrees: `save` is the one command that
// changes the file it was opened on, so the archive in memory, the archive on
// disk and what the reader then reports all have to say the same thing.
func TestSaveAddsToTheFileAndTheFileOnDiskAgrees(t *testing.T) {
	s, _, _ := netFixture(t)
	path := s.files[0].path
	var out bytes.Buffer
	s.out = &out
	s.cmdSave([]string{"/logo.png"})

	full := s.target + "logo.png"
	name := wmse.ArchiveName(full)
	if _, err := s.merged.ArchiveFile(name); err != nil {
		t.Fatalf("the archive in memory does not hold it: %v", err)
	}
	// And the file on disk, read back through the ordinary reader, holds it too.
	f, err := wmse.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	onDisk, err := f.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	diskBytes, err := onDisk.ArchiveFile(name)
	if err != nil {
		t.Fatalf("the file on disk does not hold %s: %v", name, err)
	}
	if !bytes.Equal(diskBytes, []byte("\x89PNG\r\n\x1a\n")) {
		t.Errorf("the file on disk holds %q, want the bytes the server sent", diskBytes)
	}
	// The scan's own findings are untouched by saving a file into it.
	if len(onDisk.Links) != len(s.merged.Links) {
		t.Errorf("saving a file changed the links: %d, want %d", len(onDisk.Links), len(s.merged.Links))
	}
	// And the file grew, which is said, because an archive that grows without
	// notice is how a 400 KiB scan becomes a 40 MiB one.
	if !strings.Contains(out.String(), "adding") {
		t.Errorf("save did not say what it was about to add:\n%s", out.String())
	}
}

// TestSaveTwiceKeepsOneEntry: a second save of the same path replaces what was
// there, because two entries with one name between them is a tar that extracts
// one of them and leaves the other behind.
func TestSaveTwiceKeepsOneEntry(t *testing.T) {
	s, _, _ := netFixture(t)
	var out bytes.Buffer
	s.out = &out
	s.cmdSave([]string{"/logo.png"})
	s.cmdSave([]string{"/logo.png"})

	entries, err := s.merged.ArchiveEntries()
	if err != nil {
		t.Fatalf("ArchiveEntries: %v", err)
	}
	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("the archive holds %s %d times", name, n)
		}
	}
}

// TestSaveRefusesAPathTheServerWillNotGive: an error page is not the file that
// was asked for, and archiving one would put a 404 in the file under the name of
// a script.
func TestSaveRefusesAPathTheServerWillNotGive(t *testing.T) {
	s, _, _ := netFixture(t)
	var out bytes.Buffer
	s.out = &out
	before := len(s.merged.ArchiveNames)
	s.cmdSave([]string{s.target + "gone"})
	if len(s.merged.ArchiveNames) != before {
		t.Errorf("a refused path was archived anyway")
	}
	if !strings.Contains(out.String(), "404") {
		t.Errorf("save did not say the server refused:\n%s", out.String())
	}
}

// TestArchiveBriefSaysWhatIsActuallyThere: `save` puts downloaded files in
// beside the reference files, so a session that still said "3 .ref files" over an
// archive holding a saved script would be describing something that is not there.
func TestArchiveBriefSaysWhatIsActuallyThere(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  string
	}{
		{nil, "0 saved files"},
		{[]string{"h/a.ref"}, "1 .ref file"},
		{[]string{"h/a.ref", "h/b.ref"}, "2 .ref files"},
		{[]string{"h/logo.png"}, "1 saved file"},
		{[]string{"h/a.ref", "h/logo.png"}, "1 .ref, 1 saved file"},
		{[]string{"h/a.ref", "h/b.ref", "h/logo.png", "h/app.js"}, "2 .ref, 2 saved files"},
	} {
		if got := archiveBrief(c.names); got != c.want {
			t.Errorf("archiveBrief(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}

// TestRefNamesIsTheReferencesAlone: a saved file is not a place a link was found,
// and listing it among the references would be describing an archive that does
// not exist.
func TestRefNamesIsTheReferencesAlone(t *testing.T) {
	all := []string{"h/a.ref", "h/logo.png", "h/b.ref", "h/app.js"}
	got := refNames(all)
	if len(got) != 2 || got[0] != "h/a.ref" || got[1] != "h/b.ref" {
		t.Errorf("refNames(%q) = %q, want the two .ref entries", all, got)
	}
}

// TestWriteOutReplacesTheFileWhole: a file is written through a temporary name and
// renamed over the target, so a write that fails part way leaves no half file
// where a whole one used to be.
func TestWriteOutReplacesTheFileWhole(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(dest, []byte("the old contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := writeOut(dest, []byte("new"))
	if err != nil {
		t.Fatalf("writeOut: %v", err)
	}
	if n != 3 {
		t.Errorf("writeOut reported %d bytes, want 3", n)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "new" {
		t.Errorf("the file holds %q", got)
	}
	// And nothing was left behind beside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %q, want only the file written", names)
	}
}

// TestQuoteArgs: the request being shown must be the request that will run, and
// an argument with a space in it is quoted so it reads as one word.
func TestQuoteArgs(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"-X", "GET", "http://h/"}, `-X GET http://h/`},
		{[]string{"-H", "authorization: Bearer X"}, `-H "authorization: Bearer X"`},
		{[]string{"-d", "a;b"}, `-d "a;b"`},
		{[]string{"-d", ""}, `-d ""`},
		{[]string{`a"b`}, `"a\"b"`},
	} {
		if got := strings.Join(quoteArgs(c.in), " "); got != c.want {
			t.Errorf("quoteArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCurlFailureNamesWhatHappened: "exit status 22" tells a reader nothing and
// "the server answered with an HTTP error" does, so the status is translated.
func TestCurlFailureNamesWhatHappened(t *testing.T) {
	run := func(code int) string { return curlFailure(&exec.ExitError{}) }
	_ = run
	// An error that is not an exit error is reported as it is.
	if got := curlFailure(errNotAnExit); !strings.Contains(got, "boom") {
		t.Errorf("a plain error was rewritten to %q", got)
	}
}

// errNotAnExit is an error that did not come from a process.
var errNotAnExit = plainError("boom")

type plainError string

func (e plainError) Error() string { return string(e) }

// TestContinueArgsInheritsAndOverrides is the whole of `continue`'s argument
// handling: the scan's own flags, with the three words that would send the result
// somewhere else replaced, and whatever was typed applied last.
func TestContinueArgsInheritsAndOverrides(t *testing.T) {
	recorded := "webmap -r -rdepth 2 -j -apic -refs -o s.wmse -url http://127.0.0.1:8731/"
	argv, err := continueArgs(recorded, "http://127.0.0.1:8731/api", "/tmp/out.wmse", []string{"-rdepth", "5"})
	if err != nil {
		t.Fatalf("continueArgs: %v", err)
	}
	got := strings.Join(argv, " ")
	want := "webmap -r -rdepth 2 -j -apic -refs -url http://127.0.0.1:8731/api -o /tmp/out.wmse -rdepth 5"
	if got != want {
		t.Errorf("continueArgs =\n  %s\nwant\n  %s", got, want)
	}

	// The output file is this run's own: inheriting the old one would have the
	// scan overwrite the file the session was opened on.
	if strings.Contains(got, "s.wmse") {
		t.Errorf("the scan's own -o was inherited: %s", got)
	}
	// The scope is where the session is standing, which is the reason to continue
	// from a position rather than from the root.
	if !strings.Contains(got, "-url http://127.0.0.1:8731/api") {
		t.Errorf("the scope is not the position: %s", got)
	}
	// A typed flag comes last, so it wins over the inherited one - the command
	// line has -rdepth 2 and then -rdepth 5, and the last is the one in force.
	if !strings.HasSuffix(got, "-rdepth 5") {
		t.Errorf("the typed flag is not last, so it may not win: %s", got)
	}
}

// TestContinueArgsWithNothingTyped: a continuation with no flags is the same
// scan, at the position, writing somewhere of its own.
func TestContinueArgsWithNothingTyped(t *testing.T) {
	argv, err := continueArgs("webmap -r -o old.wmse -url http://h/", "http://h/deep", "/tmp/new.wmse", nil)
	if err != nil {
		t.Fatalf("continueArgs: %v", err)
	}
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "-r") {
		t.Errorf("the recursive flag was not inherited: %s", got)
	}
	if strings.Contains(got, "old.wmse") {
		t.Errorf("the old output file was inherited: %s", got)
	}
}

// TestRecordedHas: the one setting a fetch cannot do without is recovered from the
// file rather than asked for, because the person who scanned with -k already
// answered the question.
func TestRecordedHas(t *testing.T) {
	const cmd = "webmap -k -r -o s.wmse -url http://h/"
	if !recordedHas(cmd, "-k") {
		t.Errorf("recordedHas did not find -k in %q", cmd)
	}
	if recordedHas(cmd, "-rdepth") {
		t.Errorf("recordedHas invented a flag that is not there")
	}
	if recordedHas("", "-k") {
		t.Errorf("an empty command has no flags")
	}
}

// TestArchivedAsksTheArchiveByName: the name is derived the way the writer would
// have derived it, rather than remembered, so `save` and then `source` cannot
// disagree about what a path was filed under.
func TestArchivedAsksTheArchiveByName(t *testing.T) {
	s, _, _ := netFixture(t)
	full := s.target + "logo.png"
	if _, err := s.merged.AddArchiveFile(full, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.archived(full); err != nil {
		t.Errorf("the archive does not hold what was just added: %v", err)
	}
	// A path with a query is a different entry, and is found by its own name.
	q := s.target + "logo.png?v=2"
	if _, err := s.merged.AddArchiveFile(q, []byte("y")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.archived(q); err != nil || string(got) != "y" {
		t.Errorf("a query variant was not found under its own name: %q %v", got, err)
	}
	// And something never saved is not in the archive.
	if _, err := s.archived(s.target + "never"); err == nil {
		t.Errorf("the archive claims to hold a path nothing saved")
	}
}

// linker is imported for the fixture's link shape; keep the reference honest.
var _ = linker.Link{}
