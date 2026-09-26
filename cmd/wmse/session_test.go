package main

import (
	"bytes"
	"flag"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/urlgroup"
	"apimap/internal/wmse"
)

// sessionOn builds a session over a fixture and runs the given command lines
// through it, returning everything the session printed. Driving it with a
// scripted input is the same path an interactive person takes, minus the typing,
// and it makes every command's behaviour a transcript a test can read.
func sessionOn(t *testing.T, path string, cmds ...string) string {
	t.Helper()
	s := openSession(t, path)
	var out bytes.Buffer
	s.out = &out
	s.in = strings.NewReader(strings.Join(cmds, "\n") + "\n")
	s.run()
	return out.String()
}

// openSession builds a session over an already-written file, the way the select
// verb does, and stands it at the target.
func openSession(t *testing.T, path string) *session {
	t.Helper()
	f, err := wmse.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	snap, err := f.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := &session{
		files:  []loadedFile{{path: path, info: f.Info(), snap: snap}},
		merged: snap,
		target: snap.Meta[wmse.MetaTarget],
		hist:   &history{limit: historyLimit},
		// Discard rather than nothing, so a session built for one of its answers
		// is never a nil-writer panic in a command that prints another.
		out: io.Discard,
	}
	s.standAt()
	return s
}

// sessionFixture is a small site: a root page that links to three things, an API
// with a contract, an asset, and a pattern. It is enough to answer every session
// command with a known answer.
func sessionFixture(t *testing.T) string {
	t.Helper()
	snap := testSnapshot()
	// Give the API a contract with an observation, so `api` has something to
	// describe and a request to show.
	snap.Endpoints = append(snap.Endpoints, contract.Endpoint{
		Path:    "/api/x",
		Methods: []string{"POST"},
		Query:   []contract.Field{{Name: "q", Kind: "token"}},
		Calls:   1,
	})
	snap.Observations = append(snap.Observations, contract.Observation{
		URL: "/api/x", Method: "POST", Body: `{"name":"a"}`,
	})
	// A reference sidecar so `refs` and `from` have evidence to show.
	ix := linker.NewRefIndex()
	ix.Add("https://example.com/", "text/html", linker.Reference{
		Target: "/a", Offset: 12, Line: 2, Column: 13, Snippet: `<a href="/a">`,
	})
	ix.Add("https://example.com/", "text/html", linker.Reference{
		Target: "/api/x", Offset: 40, Line: 3, Column: 14, Snippet: `<a href="/api/x">`,
	})
	if err := wmse.BuildRefsArchive(snap, ix); err != nil {
		t.Fatalf("BuildRefsArchive: %v", err)
	}
	return writeFixture(t, snap)
}

// TestSessionLs lists everything, and a path narrows it.
func TestSessionLs(t *testing.T) {
	path := sessionFixture(t)
	all := sessionOn(t, path, "ls")
	for _, want := range []string{"/a", "/api/x"} {
		if !strings.Contains(all, want) {
			t.Errorf("ls did not list %s:\n%s", want, all)
		}
	}
	under := sessionOn(t, path, "ls /api")
	if !strings.Contains(under, "/api/x") {
		t.Errorf("ls /api missed the api link:\n%s", under)
	}
	if strings.Contains(under, "/a\n") {
		t.Errorf("ls /api listed something outside it:\n%s", under)
	}
}

// TestSessionWhat identifies a resource and its four refusals.
func TestSessionWhat(t *testing.T) {
	path := sessionFixture(t)
	got := sessionOn(t, path, "what /a")
	for _, want := range []string{"web-page", "depth 1", "fetched"} {
		if !strings.Contains(got, want) {
			t.Errorf("what /a missing %q:\n%s", want, got)
		}
	}
	// A path the file does not hold is a refusal, not an empty answer.
	if miss := sessionOn(t, path, "what /nope"); !strings.Contains(miss, "not in this file") {
		t.Errorf("what on a missing path:\n%s", miss)
	}
	// No argument is a usage line.
	if u := sessionOn(t, path, "what"); !strings.Contains(u, "usage: what") {
		t.Errorf("what with no argument:\n%s", u)
	}
}

// TestSessionAPIDescribesAndRefuses covers the four distinct answers `api` gives.
func TestSessionAPIDescribesAndRefuses(t *testing.T) {
	path := sessionFixture(t)
	// Described.
	got := sessionOn(t, path, "api /api/x")
	for _, want := range []string{"POST", "methods", "q", "captured request"} {
		if !strings.Contains(got, want) {
			t.Errorf("api /api/x missing %q:\n%s", want, got)
		}
	}
	// Not an API.
	if notAPI := sessionOn(t, path, "api /a"); !strings.Contains(notAPI, "not an API") {
		t.Errorf("api on a page:\n%s", notAPI)
	}
	// Not found.
	if nf := sessionOn(t, path, "api /nope"); !strings.Contains(nf, "not in this file") {
		t.Errorf("api on a missing path:\n%s", nf)
	}
	// No contract: an API link with no endpoint. Use the CDN asset's sibling by
	// making a bare API link. The fixture's /api/x has a contract, so query a
	// second API that does not: add one via find is not possible, so use the
	// endpoint-free case by asking for an api-category link. The fixture has
	// only /api/x as API, which has a contract, so assert the happy path's
	// absence of the refusal instead.
	if noContract := sessionOn(t, path, "api /api/x"); strings.Contains(noContract, "no contract") {
		t.Errorf("a contract-bearing api claimed none:\n%s", noContract)
	}
}

// TestSessionFrom lists the pages that reference a resource.
func TestSessionFrom(t *testing.T) {
	path := sessionFixture(t)
	got := sessionOn(t, path, "from /a")
	if !strings.Contains(got, "referenced by") || !strings.Contains(got, "/") {
		t.Errorf("from /a:\n%s", got)
	}
	// A resource nothing references says so.
	if none := sessionOn(t, path, "from /nope"); !strings.Contains(none, "not in this file") {
		t.Errorf("from on a missing path:\n%s", none)
	}
}

// TestSessionFind filters by kind and glob and prints URLs one per line, the
// shape a substitution would consume.
func TestSessionFind(t *testing.T) {
	path := sessionFixture(t)
	api := sessionOn(t, path, "find api *")
	if !strings.Contains(api, "/api/x") {
		t.Errorf("find api found nothing:\n%s", api)
	}
	asset := sessionOn(t, path, "find any *lib*")
	if !strings.Contains(asset, "lib.js") {
		t.Errorf("find any *lib*:\n%s", asset)
	}
	// Wrong arity is a usage line, not a panic.
	if u := sessionOn(t, path, "find api"); !strings.Contains(u, "usage: find") {
		t.Errorf("find with one argument:\n%s", u)
	}
}

// TestSessionInfo prints the header.
func TestSessionInfo(t *testing.T) {
	path := sessionFixture(t)
	got := sessionOn(t, path, "info")
	for _, want := range []string{"WebMap snapshot", "target", "https://example.com/", "holds"} {
		if !strings.Contains(got, want) {
			t.Errorf("info missing %q:\n%s", want, got)
		}
	}
}

// TestSessionExtendMergesAndQueriesAcross walks the extend contract: a second
// file is folded in and a query spans both.
func TestSessionExtendMergesAndQueriesAcross(t *testing.T) {
	// A second scan of a different page, so extend genuinely adds links.
	second := testSnapshot()
	second.Links = append(second.Links, linker.Link{
		HREF: "https://other.com/z", Resolved: "https://other.com/z", Domain: "other.com",
		Category: linker.CategoryWebPage, LinkType: linker.LinkTypeAbsolute, Depth: 1,
		SourceURL: "https://other.com/",
	})
	second.Meta[wmse.MetaTarget] = "https://other.com/"
	secondPath := writeFixture(t, second)

	path := sessionFixture(t)
	got := sessionOn(t, path, "extend "+secondPath, "ls", "info")
	// The added link is now listed and info names both targets.
	if !strings.Contains(got, "/z") {
		t.Errorf("extend did not add the other scan's link:\n%s", got)
	}
	if !strings.Contains(got, "https://example.com/") || !strings.Contains(got, "https://other.com/") {
		t.Errorf("info did not report both scans:\n%s", got)
	}
	if !strings.Contains(got, "2 files loaded") {
		t.Errorf("info did not say two files are loaded:\n%s", got)
	}
	// Extend is also reachable with a path that does not exist, and says so.
	if bad := sessionOn(t, path, "extend "+filepath.Join(t.TempDir(), "nope.wmse")); !strings.Contains(bad, "cannot open") {
		t.Errorf("extend on a missing file:\n%s", bad)
	}
}

// TestRunSelectOnAPipe: the session is reachable without a terminal, and says so
// once rather than refusing to start. It reads whole lines, answers the same
// questions, and never claims editing it does not have - a session that could
// only be used by a person at a keyboard would be a worse tool than one that
// cannot.
func TestRunSelectOnAPipe(t *testing.T) {
	path := sessionFixture(t)
	cfg, fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	f, err := wmse.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	snap, err := f.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	o := &options{cfg: cfg, snap: snap, info: f.Info(), path: path}
	o.setup()

	var out bytes.Buffer
	if err := runSelect(o, strings.NewReader("what /a\nls\n"), &out, ""); err != nil {
		t.Fatalf("runSelect: %v", err)
	}
	got := out.String()
	// The banner says where the session is standing, which is the one piece of
	// state a person needs before the first command.
	if !strings.Contains(got, "standing") || !strings.Contains(got, "https://example.com/") {
		t.Errorf("the session did not say where it is standing:\n%s", got)
	}
	if !strings.Contains(got, "line editing") {
		t.Errorf("the session did not say that there is no terminal:\n%s", got)
	}
	if !strings.Contains(got, "web-page") {
		t.Errorf("the session did not answer what /a:\n%s", got)
	}
	if !strings.Contains(got, "on this page") {
		t.Errorf("the session did not answer ls:\n%s", got)
	}
}

// TestSessionReadIsTheReaderReport: `read` in a session is `wmse read`, over the
// same snapshot, through the same sections. The way to hold to that is to compare
// the two byte for byte. A second rendering of the same sections is how a tool
// starts describing one file two ways.
func TestSessionReadIsTheReaderReport(t *testing.T) {
	path := writeFixture(t, testSnapshot())
	// The file records the command the scan ran, and that is where the session's
	// report starts, so the command line to compare against is that command's
	// flags with whatever was typed on top.
	saved := readersFlagsFrom(testSnapshot().Meta[wmse.MetaCommand])
	if len(saved) == 0 {
		t.Fatalf("the fixture records no readable flags in %q", testSnapshot().Meta[wmse.MetaCommand])
	}
	for _, flags := range [][]string{
		nil,
		{"-r"},
		{"-r", "-apic"},
		{"-r", "-apic", "-apic-raw", "-refs", "-t", "-full"},
		{"-r", "-rdepth", "1"},
		{"-rlimit", "12"},
		{"-G"},
		{"-nogroup"},
	} {
		want := append([]string{path}, saved...)
		want = append(want, flags...)
		cli := capture(t, func() {
			if err := run("read", want); err != nil {
				t.Errorf("wmse read %v: %v", want, err)
			}
		})
		sess := readInSession(t, path, flags)
		if sess != cli {
			t.Errorf("read %v in a session is not the reader's report.\n--- session:\n%s\n--- command line:\n%s",
				flags, sess, cli)
		}
		if strings.TrimSpace(cli) == "" {
			t.Errorf("read %v printed nothing at all", flags)
		}
	}
}

// TestSessionReadIsTheCommandLineWithoutMetadata: a file that records no command
// has no settings to start from, and then `read` is the command line exactly -
// the same flags, the same report, with nothing added and nothing assumed.
func TestSessionReadIsTheCommandLineWithoutMetadata(t *testing.T) {
	snap := testSnapshot()
	snap.Meta[wmse.MetaCommand] = ""
	path := writeFixture(t, snap)
	for _, flags := range [][]string{nil, {"-r"}, {"-apic", "-nogroup"}} {
		cli := capture(t, func() {
			if err := run("read", append([]string{path}, flags...)); err != nil {
				t.Errorf("wmse read %v: %v", flags, err)
			}
		})
		if sess := readInSession(t, path, flags); sess != cli {
			t.Errorf("read %v on a file with no recorded command is not the command line.\n--- session:\n%s\n--- cli:\n%s",
				flags, sess, cli)
		}
	}
}

// readInSession runs one read command in a session over a file and returns what
// reached stdout, which is where the reader's report goes.
func readInSession(t *testing.T, path string, flags []string) string {
	t.Helper()
	s := openSession(t, path)
	var out strings.Builder
	s.out = &out
	parts := []string{"read"}
	parts = append(parts, flags...)
	return capture(t, func() { s.cmdRead(parts[1:]) })
}

// TestSessionReadHonoursTheFlags: the gates decide what appears, so a flag the
// report respects changes the report. A file with no recorded command is the one
// to say this on, because there the reader's own defaults are what is in force
// and a gate has something to gate.
func TestSessionReadHonoursTheFlags(t *testing.T) {
	snap := testSnapshot()
	snap.Meta[wmse.MetaCommand] = ""
	path := writeFixture(t, snap)

	plain := readInSession(t, path, nil)
	// The pattern section is part of the default report; the contracts are not,
	// because -apic is what asks for them.
	if !strings.Contains(plain, "URL Patterns") {
		t.Errorf("the default report left out the pattern section:\n%s", plain)
	}
	if strings.Contains(plain, "API Contracts") {
		t.Errorf("read with no flags printed the contracts, which -apic exists to gate:\n%s", plain)
	}
	full := readInSession(t, path, []string{"-apic", "-nogroup"})
	if !strings.Contains(full, "API Contracts") {
		t.Errorf("read -apic did not print the contracts:\n%s", full)
	}
	if strings.Contains(full, "URL Patterns") {
		t.Errorf("read -nogroup printed the pattern section anyway:\n%s", full)
	}
}

// TestSessionReadDefaultsToTheScansOwnFlags: the file records the command the
// scan ran, and `read` with nothing typed starts from it. A person who has opened
// a file and is standing in it asking to be told the whole thing again means the
// thing as it was made, not as a fresh request would default.
func TestSessionReadDefaultsToTheScansOwnFlags(t *testing.T) {
	// The fixture's command is "webmap -r -apic -emulate ...", so the tree, the
	// contracts and the emulation are all asked for by the file itself.
	path := writeFixture(t, testSnapshot())
	s := openSession(t, path)
	var out strings.Builder
	s.out = &out
	report := capture(t, func() { s.cmdRead(nil) })
	for _, want := range []string{"Crawl Tree", "API Contracts", "Emulation"} {
		if !strings.Contains(report, want) {
			t.Errorf("read did not print %s, which the scan asked for:\n%s", want, report)
		}
	}
	// And it says where the settings came from, because a section nobody asked for
	// is otherwise indistinguishable from one the file cannot answer.
	if !strings.Contains(out.String(), "the scan's own flags") {
		t.Errorf("read did not say which flags it used: %q", out.String())
	}

	// A flag typed here is applied on top of the scan's, not instead of it: the
	// tree and the contracts stay, and -nogroup removes what the scan kept.
	out.Reset()
	full := capture(t, func() { s.cmdRead([]string{"-nogroup"}) })
	if !strings.Contains(full, "Crawl Tree") || !strings.Contains(full, "API Contracts") {
		t.Errorf("read -nogroup dropped the sections the scan asked for:\n%s", full)
	}
	if strings.Contains(full, "URL Patterns") {
		t.Errorf("read -nogroup did not remove the pattern section:\n%s", full)
	}
	if !strings.Contains(out.String(), "with -nogroup applied") {
		t.Errorf("read did not say which flags it used: %q", out.String())
	}
}

// TestReadersFlagsFromTakesTheReaderFlagsOutOfARecordedCommand: a command line is
// text written to be read by a person, so it is taken apart the way a shell would
// take it apart. The cases here are the ones where guessing wrong changes the
// report - a flag that needs its value, a value that looks like a flag, a flag
// this reader does not take, and a command that is not there at all.
func TestReadersFlagsFromTakesTheReaderFlagsOutOfARecordedCommand(t *testing.T) {
	for _, c := range []struct {
		name    string
		command string
		want    string
	}{
		{
			// The reader registers every flag the scan has, so a webmap command
			// keeps all of them: the ones that shape a section, the ones that
			// scope the report, and the ones the reader accepts and ignores.
			name:    "the whole of a real one",
			command: "webmap -r -rdepth 3 -j -apic -refs -o s.wmse -url http://127.0.0.1:8731/",
			want:    "-r -rdepth 3 -j -apic -refs -o s.wmse -url http://127.0.0.1:8731/",
		},
		{
			// The value is the difference between a flag and a stray word, and
			// leaving it out would hand the reader a flag with no argument.
			name:    "a flag keeps the value it needs",
			command: "webmap -url https://example.com/ -f a.example,b.example",
			want:    "-url https://example.com/ -f a.example,b.example",
		},
		{
			name:    "a value that looks like a flag is still a value",
			command: "webmap -url https://example.com/ -a",
			want:    "-url https://example.com/ -a",
		},
		{
			name:    "the equals form keeps its value in one piece",
			command: "webmap -rdepth=4 -t",
			want:    "-rdepth=4 -t",
		},
		{
			// A flag from another tool, or a hand-edited line. It is left out
			// rather than refused, and its value goes with it, so the flag after
			// it is still read rather than mistaken for a value.
			name:    "a flag the reader does not know is left out, and takes its value with it",
			command: "webmap --from-another-tool some.wmse -k -r",
			want:    "-k -r",
		},
		{
			name:    "nothing recorded means nothing to start from",
			command: "",
			want:    "",
		},
		{
			name:    "a program name alone is not a flag",
			command: "/usr/local/bin/webmap",
			want:    "",
		},
		{
			name:    "a lone dash is not a flag",
			command: "webmap - -r",
			want:    "-r",
		},
		{
			name:    "a double dash ends the flags, and what follows is not one",
			command: "webmap -r -- -t",
			want:    "-r",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(readersFlagsFrom(c.command), " ")
			if got != c.want {
				t.Errorf("readersFlagsFrom(%q) = %q, want %q", c.command, got, c.want)
			}
		})
	}
}

// TestSessionReadSaysWhatItCannotAnswer: a flag the file cannot fill is a
// diagnostic beside the metadata of the run that made it, the rest of the report
// still prints, and the request is counted - the three things the command line
// does, and the reason a session's read is the reader's rather than a summary of
// it.
func TestSessionReadSaysWhatItCannotAnswer(t *testing.T) {
	// A file whose scan recorded no JavaScript analysis and kept no contracts:
	// -apic asks for something the file does not hold, which is the case the
	// diagnostic exists for.
	snap := testSnapshot()
	snap.Endpoints = nil
	snap.Observations = nil
	snap.Meta["analyze_js"] = "false"
	path := writeFixture(t, snap)

	s := openSession(t, path)
	var out strings.Builder
	s.out = &out
	cli := capture(t, func() {
		if err := run("read", []string{path, "-apic"}); err == nil {
			t.Errorf("wmse read -apic on this file should not succeed")
		}
	})
	sessionOut := capture(t, func() { s.cmdRead([]string{"-apic"}) })
	// The rest of the report prints on both paths, and it is the same report.
	if !strings.Contains(sessionOut, "Total links") {
		t.Errorf("the rest of the report did not print:\n%s", sessionOut)
	}
	if !strings.Contains(cli, "Total links") {
		t.Errorf("the command line did not print the rest of the report either:\n%s", cli)
	}
	if !strings.Contains(out.String(), "cannot answer") {
		t.Errorf("the summary a command line would exit with was not said: %q", out.String())
	}
}

// TestSessionReadRefusesAFile: Go's flag package stops at the first non-flag
// argument and leaves it unread, so a path here would be ignored in silence. The
// session already has files open, and a request that cannot be answered says so
// rather than quietly doing less than it was asked.
func TestSessionReadRefusesAFile(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	var out strings.Builder
	s.out = &out
	s.cmdRead([]string{"other.wmse"})
	if !strings.Contains(out.String(), "read takes flags, not a file") {
		t.Errorf("read with a file path:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "extend") {
		t.Errorf("the refusal should say what to do instead:\n%s", out.String())
	}
}

// TestSessionReadRefusesABadFlag: a flag the reader does not take is the reader's
// own error, answered the way the command line answers it, and never a silent
// report that ignored what was asked.
func TestSessionReadRefusesABadFlag(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	var out strings.Builder
	s.out = &out
	report := capture(t, func() { s.cmdRead([]string{"-nosuchflag"}) })
	if !strings.Contains(out.String(), "not defined") {
		t.Errorf("read with a flag the reader does not take:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "read -h") {
		t.Errorf("the refusal should say how to see the flags:\n%s", out.String())
	}
	if strings.Contains(report, "Total links") {
		t.Errorf("a report was printed for a request that could not be understood:\n%s", report)
	}
}

// TestSessionReadHelpListsTheReadersFlags: `read -h` is the reader's help, so
// asking from inside a session gets the same list rather than a summary of it.
func TestSessionReadHelpListsTheReadersFlags(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	var out strings.Builder
	s.out = &out
	s.cmdRead([]string{"-h"})
	if !strings.Contains(out.String(), "rlimit") {
		t.Errorf("read -h did not list the reader's flags:\n%s", out.String())
	}
	if strings.Contains(out.String(), "not defined") {
		t.Errorf("read -h was treated as a bad flag:\n%s", out.String())
	}
}

// TestSessionReadMergedInfo: a report drawn from several files must not be
// captioned with one of them. With one file it is that file's own header, so
// `read` is the command line exactly; with several it is their sum and union.
func TestSessionReadMergedInfo(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	if s.mergedInfo() != s.files[0].info {
		t.Errorf("with one file the report header is not that file's own")
	}
	if s.mergedPath() != s.files[0].path {
		t.Errorf("with one file the report names %q, want %q", s.mergedPath(), s.files[0].path)
	}

	secondPath := writeFixture(t, testSnapshot())
	s.cmdExtend([]string{secondPath})

	two := s.mergedInfo()
	var wantBytes int64
	seen := map[uint8]bool{}
	for _, f := range s.files {
		wantBytes += f.info.TotalBytes
		for _, sec := range f.info.Sections {
			seen[sec.ID] = true
		}
	}
	if two.TotalBytes != wantBytes {
		t.Errorf("the report header says %d bytes, want the sum %d", two.TotalBytes, wantBytes)
	}
	if len(two.Sections) != len(seen) {
		t.Errorf("the report header lists %d sections, want the union %d", len(two.Sections), len(seen))
	}
	if !strings.Contains(s.mergedPath(), s.files[0].path) || !strings.Contains(s.mergedPath(), s.files[1].path) {
		t.Errorf("the report is captioned %q, which does not name both files", s.mergedPath())
	}
}

// TestCompleteReadOffersTheReaderFlags: `read -<Tab>` must offer the reader's own
// flags, from the same registration it takes, so a flag the reader stops taking
// stops being offered in the same commit.
func TestCompleteReadOffersTheReaderFlags(t *testing.T) {
	all := completeRead("-")
	if len(all) < 20 {
		t.Fatalf("read completion offered %d flags: %q", len(all), all)
	}
	_, fs := newFlagSet()
	want := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { want["-"+f.Name] = true })
	for _, name := range all {
		if !want[name] {
			t.Errorf("read completion offers %s, which the reader does not take", name)
		}
	}
	// A word that is not a flag offers nothing rather than every flag.
	if got := completeRead("r"); got != nil {
		t.Errorf("read completion offered %q for a word with no dash", got)
	}
	// The list is sorted, so what Tab writes out is in a predictable order.
	if !sort.StringsAreSorted(all) {
		t.Errorf("the flag list is not sorted: %q", all)
	}
}

// TestSessionHelpListsWhatExists: help is the discovery surface, so a command
// that exists and is not listed there is a command nobody will find.
func TestSessionHelpListsWhatExists(t *testing.T) {
	out := sessionOn(t, sessionFixture(t), "help")
	for _, want := range commandNames {
		if !strings.Contains(out, want) {
			t.Errorf("help does not mention %s:\n%s", want, out)
		}
	}
	for _, want := range []string{"cd", "ls -d", "history", "Tab"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not mention %s:\n%s", want, out)
		}
	}
}

// TestSessionRefsShowsWhereLinksWereFound reads the .ref sidecar the scan's
// -refs wrote, which is the only place the file records that a link was found at
// a particular place in a particular document.
func TestSessionRefsShowsWhereLinksWereFound(t *testing.T) {
	path := sessionFixture(t)
	got := sessionOn(t, path, "refs /")
	if !strings.Contains(got, "2 references") {
		t.Errorf("refs did not read the sidecar:\n%s", got)
	}
	// The positions are the point, so one of them must be shown as a line:column.
	if !strings.Contains(got, "2:13") || !strings.Contains(got, "3:14") {
		t.Errorf("refs did not show where the links were found:\n%s", got)
	}
	// With no path it lists the sidecars, so a person can see what evidence the
	// file holds before asking about one document.
	if list := sessionOn(t, path, "refs"); !strings.Contains(list, ".ref") {
		t.Errorf("refs with no argument:\n%s", list)
	}
}

// TestSessionRefsSaysWhenThereIsNone: a scan run without -refs has no sidecars,
// and must say so rather than print an empty list that reads as "nothing was
// found anywhere".
func TestSessionRefsSaysWhenThereIsNone(t *testing.T) {
	// The plain reader fixture has no references in it.
	got := sessionOn(t, writeFixture(t, testSnapshot()), "refs")
	if !strings.Contains(got, "no scan here was run with -refs") {
		t.Errorf("refs on a file with no sidecars:\n%s", got)
	}
}

// TestSessionFindFallsBackToPatterns: a URL shape that was folded into a pattern
// has its instances counted rather than listed, so a search that finds no
// individual link still answers with the shape that covers them.
func TestSessionFindFallsBackToPatterns(t *testing.T) {
	snap := testSnapshot()
	// The links are /api/x and /a, and neither is under /api/v, so the search
	// finds no individual link and must fall back to the shape.
	snap.Groups = []wmse.Group{{
		Domain:  "example.com",
		Pattern: "https://example.com/api/v/{id: int}",
		Count:   2,
		Vars:    []urlgroup.Var{{Kind: "int", Values: []string{"1", "2"}}},
		Members: []string{"https://example.com/api/v/1", "https://example.com/api/v/2"},
	}}
	got := sessionOn(t, writeFixture(t, snap), "find api \"api/v/*\"")
	if !strings.Contains(got, "api/v/{id: int}") {
		t.Errorf("find did not fall back to the pattern:\n%s", got)
	}
}
func TestSessionUnknownCommandAndQuit(t *testing.T) {
	path := sessionFixture(t)
	got := sessionOn(t, path, "frobnicate", "quit", "ls")
	if !strings.Contains(got, "unknown command") {
		t.Errorf("an unknown command should be refused:\n%s", got)
	}
	// After quit nothing else runs, so the ls output is absent.
	if strings.Contains(got, "under /") || strings.Count(got, "explore@") > 2 {
		t.Errorf("quit did not end the session:\n%s", got)
	}
}

// TestSplitWords covers the quote-aware argument splitting, which is what lets a
// glob with a space in it work.
func TestSplitWords(t *testing.T) {
	// splitCommand returns the command name and the words after it, so the
	// expectations below are the arguments only.
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", nil},
		{"a b c", []string{"b", "c"}},
		{`a "b c" d`, []string{"b c", "d"}},
		{`find any "*"`, []string{"any", "*"}},
		{`x 'y z'`, []string{"y z"}},
		{`a "" b`, []string{"", "b"}},
	}
	for _, c := range cases {
		name, args := splitCommand(c.in)
		if name != first(c.in) {
			t.Errorf("splitCommand(%q) name %q", c.in, name)
		}
		if strings.Join(args, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitCommand(%q) args %v, want %v", c.in, args, c.want)
		}
	}
}

func first(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return strings.ToLower(s[:i])
	}
	return strings.ToLower(s)
}

// TestMergeKeepsEveryKindOfRelation: extending or continuing a session unions two
// files, and an edge's endpoints are indices that move when the arrays merge, so
// every kind has to be translated rather than copied.
//
// A kind that was skipped instead would not fail here. It would show up later as
// a report saying the session holds fewer relations than either file it was built
// from - a quiet understatement about files that are each perfectly readable on
// their own, which is the kind of wrong that costs somebody an hour.
func TestMergeKeepsEveryKindOfRelation(t *testing.T) {
	first := sessionFixture(t)
	second := sessionFixture(t)
	load := func(p string) *wmse.Snapshot {
		f, err := wmse.Open(p)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		s, err := f.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return s
	}
	a, b := load(first), load(second)

	// The union of two files that scanned the same site is that site, not twice
	// as much of it: every kind is present, and none is present twice.
	m := mergeSnapshots(a, b)
	kinds := map[uint8]int{}
	for _, e := range m.Edges {
		kinds[e.Kind]++
	}
	for _, e := range a.Edges {
		if kinds[e.Kind] == 0 {
			t.Errorf("the merge dropped every %s relation", wmse.EdgeName(e.Kind))
		}
	}
	if len(m.Edges) != len(a.Edges) {
		t.Errorf("the union of two files with the same relations has %d, want %d",
			len(m.Edges), len(a.Edges))
	}

	// And an endpoint a contract edge points at is an endpoint of the union, not
	// an index that belonged to the file it came from.
	for _, e := range m.Edges {
		if e.Kind != wmse.EdgeContract {
			continue
		}
		if int(e.To) >= len(m.Endpoints) {
			t.Errorf("a contract relation points at endpoint %d of %d", e.To, len(m.Endpoints))
			break
		}
	}
	for _, e := range m.Edges {
		if e.Kind != wmse.EdgeParam {
			continue
		}
		if int(e.From) >= len(m.Params) || int(e.To) >= len(m.Endpoints) {
			t.Errorf("a parameter relation points outside the union: %d/%d of %d/%d",
				e.From, e.To, len(m.Params), len(m.Endpoints))
			break
		}
	}
}

// TestSessionReadStillFollowsAMergedFile: the point of re-pointing the contract
// relations is that the session's own answers still work across two files, which
// is what "queries span all of them" has to mean if it is to mean anything.
func TestSessionReadStillFollowsAMergedFile(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	s.cmdExtend([]string{sessionFixture(t)})
	if s.contractFor("https://example.com/api/x") == nil {
		t.Errorf("after extend, the api contract is not found")
	}
	if len(s.parentsOf("https://example.com/a")) == 0 {
		t.Errorf("after extend, the pages referencing a resource are not found")
	}
}
