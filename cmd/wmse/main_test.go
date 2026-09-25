package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"apimap/internal/config"
	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/urlgroup"
	"apimap/internal/wmse"
)

// scanFlags is the flag set of ./webmap, spelled out. The reader registers
// through the same code as the scan, so the two cannot disagree about a name or
// a default - which makes this list the part worth checking: it is the promise,
// and a flag quietly dropped from config.Register would be a broken promise that
// no amount of shared code would notice.
var scanFlags = []string{
	"-G", "-H", "-M", "-T", "-a", "-apic", "-apic-raw", "-apif", "-b", "-bitrix",
	"-cache", "-cdn", "-cf", "-color", "-cookie", "-emu-maxjobs", "-emu-maxjs",
	"-emu-maxleaks", "-emu-timeout", "-emu-workers", "-emulate", "-f", "-follow",
	"-full", "-group-count", "-headers-all-hosts", "-j", "-k", "-nogroup",
	"-noise", "-o", "-o-raw", "-patterns", "-r", "-rdepth", "-react", "-rlimit",
	"-str", "-t", "-url", "-waf", "-wp",
}

// capture runs fn with stdout redirected and returns what it printed. The views
// write straight to stdout, which is right for a program and awkward for a test:
// without this, every assertion on them would be an assertion on the test log,
// which passes whether or not the view worked.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// result is one read request, answered: what went to stdout, what went to
// stderr, and what the command returned.
type result struct {
	stdout string
	stderr string
	err    error
}

// out returns stdout with its blank lines dropped, which is what the assertions
// care about and what a person reads.
func (r result) out() string {
	var keep []string
	for _, l := range strings.Split(r.stdout, "\n") {
		if strings.TrimSpace(l) != "" {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// exercise runs a verb with the scan's flags and the file, and returns both
// streams. It goes through run rather than through the views directly, so the
// tests cover the parts a user actually hits: the flags, the scope they select,
// the complaints the file cannot answer, and the exit status.
func exercise(t *testing.T, verb, path string, args ...string) result {
	t.Helper()
	savedOut, savedErr := os.Stdout, os.Stderr
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = ow, ew
	stdoutC := make(chan string, 1)
	stderrC := make(chan string, 1)
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, or); stdoutC <- b.String() }()
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, er); stderrC <- b.String() }()

	runErr := run(verb, append(append([]string{}, args...), path))

	os.Stdout, os.Stderr = savedOut, savedErr
	_ = ow.Close()
	_ = ew.Close()
	res := result{stdout: <-stdoutC, stderr: <-stderrC, err: runErr}
	_ = or.Close()
	_ = er.Close()
	return res
}

// opened runs everything a read does before the views start, and hands back the
// state a real command would have built. A view tested against a hand-made
// options is tested against something no command can produce.
func opened(t *testing.T, path string, args ...string) *options {
	t.Helper()
	cfg, fs := newFlagSet()
	got, err := parseRead(fs, append(append([]string{}, args...), path))
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	if got != path {
		t.Fatalf("parse %v: got path %q, want %q", args, got, path)
	}
	cfg.Apply(fs)
	f, err := wmse.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	snap, err := f.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	o := &options{cfg: cfg, snap: snap, file: f, path: path, depth: readDepth(cfg, fs)}
	o.setup()
	return o
}

// testSnapshot is the fixture: a small site with one of everything a file can
// hold, so that a section that stops being printed is a change and not an
// accident of the fixture.
func testSnapshot() *wmse.Snapshot {
	return &wmse.Snapshot{
		Meta: wmse.Meta{
			wmse.MetaTool:        "webmap",
			wmse.MetaVersion:     "1.0.0",
			wmse.MetaFormat:      "v1",
			wmse.MetaTarget:      "https://example.com/",
			wmse.MetaScope:       "recursive depth<=5 threads=32",
			wmse.MetaCommand:     "webmap -r -apic -emulate -o test.wmse -url https://example.com/",
			wmse.MetaCreated:     "2026-09-25T10:11:12Z",
			wmse.MetaElapsed:     "1.2s",
			"analyze_js":         "true",
			wmse.MetaSessionData: "true",
		},
		Links: []linker.Link{
			{HREF: "https://example.com/", Resolved: "https://example.com/", Category: linker.CategoryWebPage, LinkType: linker.LinkTypeAbsolute},
			{HREF: "/a", Resolved: "https://example.com/a", Domain: "example.com", Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/", Tag: "a"},
			{HREF: "/api/x", Resolved: "https://example.com/api/x", Domain: "example.com", Category: linker.CategoryAPI, LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/", HasParams: true,
				APIDetails: []linker.APIDetail{{HTTPMethod: "POST", Arguments: "id, name"}}},
			// Hidden by default, the way the scan hid it, and revealed by
			// exactly the flag the scan used. It sits on a subdomain of the
			// target so that the class filter is what hides it, not the
			// scope: a link that was out of scope would test the wrong rule.
			{HREF: "https://static.example.com/lib.js", Resolved: "https://static.example.com/lib.js", Domain: "static.example.com", Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeAbsolute, Class: linker.ClassCDN, Depth: 1, SourceURL: "https://example.com/"},
		},
		Pages: []wmse.Page{
			{URL: "https://example.com/", Depth: 0, ContentType: "text/html; charset=utf-8", Links: 3},
			{URL: "https://example.com/a", Depth: 1, ContentType: "text/html"},
		},
		// The stats are deliberately wrong: a reader that trusted what the
		// file recorded would report these numbers instead of counting the
		// links the flags selected.
		Stats: wmse.Stats{Total: 99, ByCategory: map[linker.Category]int{
			linker.CategoryWebPage: 42, linker.CategoryAPI: 57,
		}},
		Groups: []wmse.Group{{
			Domain:  "example.com",
			Pattern: "https://example.com/p/{id: int}",
			Count:   2,
			Vars:    []urlgroup.Var{{Kind: "int", Values: []string{"1", "2"}, Nums: []int64{1, 2}}},
			Members: []string{"https://example.com/p/1", "https://example.com/p/2"},
		}},
		Endpoints: []contract.Endpoint{{
			Path:    "/api/x",
			Methods: []string{"POST"},
			Calls:   1,
			Query:   []contract.Field{{Name: "q", Kind: "token", Constant: true, ConstVal: "s3cr3t"}},
			Bodies: []contract.BodyFormat{{
				Kind:   "json",
				Fields: []contract.Field{{Name: "name", Kind: "str", Values: []string{"a"}}},
			}},
			Raw: []string{"POST /api/x"},
		}},
		Observations: []contract.Observation{{
			URL:     "/api/x",
			Method:  "POST",
			Body:    `{"name":"a"}`,
			Headers: []contract.NameValue{{Name: "X-Token", Value: "s3cr3t"}},
		}},
		Params: []wmse.Param{{
			ParamRef: linker.ParamRef{Name: "token", Kind: linker.ParamQuery, Owner: linker.OwnerRequest, Endpoints: []string{"/api/x"}},
			Docs:     []string{"https://example.com/app.js"},
		}, {
			// No endpoints and not in any link: the one the params section
			// exists to report.
			ParamRef: linker.ParamRef{Name: "page", Kind: linker.ParamQuery, Owner: linker.OwnerRequest},
			Docs:     []string{"https://example.com/app.js", "https://example.com/bundle.js"},
		}},
		Emulation: &wmse.Emulation{
			Scripts: 1,
			Calls:   1,
			ByType:  map[string]int{"fetch": 1},
			List: []wmse.Call{{
				URL: "https://example.com/api/x", Method: "POST", Type: "fetch",
				Headers: [][2]string{{"X-Token", "s3cr3t"}},
			}},
			Errors: []string{"TypeError: t is not a function"},
		},
	}
}

// testFile writes the fixture to a temporary file and returns its path. It goes
// through the real writer, so the reader is tested against the format as it is
// produced rather than against something shaped like it.
func testFile(t *testing.T) string {
	t.Helper()
	snap := testSnapshot()
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	path := filepath.Join(t.TempDir(), "test.wmse")
	if _, err := wmse.Write(path, snap, wmse.DefaultOptions()); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestReaderTakesTheScansArguments is the promise the whole command is built on:
// the reader's arguments are the scan's arguments, one for one. The names are
// listed here rather than read from the flag set, because a list read from the
// flag set would agree with itself no matter what the flag set contained.
func TestReaderTakesTheScansArguments(t *testing.T) {
	_, fs := newFlagSet()
	var got []string
	fs.VisitAll(func(f *flag.Flag) { got = append(got, f.Name) })
	if len(got) != len(scanFlags) {
		t.Errorf("the reader has %d flags, the scan has %d:\n got %v\nwant %v", len(got), len(scanFlags), got, scanFlags)
	}
	have := map[string]bool{}
	for _, n := range got {
		have[n] = true
	}
	for _, want := range scanFlags {
		if !have[strings.TrimPrefix(want, "-")] {
			t.Errorf("the scan has %s and the reader does not", want)
		}
	}
}

// TestUsageOnlyMentionsFlagsThatExist keeps the help honest in the other
// direction: a flag documented here that the reader does not take is a
// documented lie, and a reader that answers a request it cannot fill is worse
// than one that never promised to.
func TestUsageOnlyMentionsFlagsThatExist(t *testing.T) {
	_, fs := newFlagSet()
	known := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { known[f.Name] = true })
	for _, tok := range usageFlags(usage) {
		if !known[strings.TrimPrefix(tok, "-")] {
			t.Errorf("usage documents %s, which the reader does not take", tok)
		}
	}
}

// TestUsageSpellsFlagsWithOneDash pins the spelling. Every flag in WebMap is
// written with a single dash, so a reader that documented itself with two would
// be the one tool in the set you had to remember. The text is hand-written, so
// nothing else would catch the drift.
func TestUsageSpellsFlagsWithOneDash(t *testing.T) {
	if i := strings.Index(usage, "--"); i >= 0 {
		t.Errorf("usage spells a flag with two dashes: %q", usage[max(0, i-30):min(len(usage), i+30)])
	}
}

// usageFlags pulls the flag names out of a usage block, so the assertions above
// test the tokens a reader would actually type rather than the prose.
func usageFlags(u string) []string {
	fields := strings.FieldsFunc(u, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",();:", r)
	})
	var out []string
	for _, f := range fields {
		f = strings.Trim(f, ".;\"'")
		if strings.HasPrefix(f, "-") && len(f) > 1 {
			out = append(out, f)
		}
	}
	return out
}

// TestFlagsWorkOnEitherSideOfThePath covers the way a person types a command.
// Go's flag package stops at the first non-flag argument, so a reader that only
// worked as "wmse read file -r" would be unusable, and a person who has typed
// "webmap -r -o file -url X" will put the flags first.
func TestFlagsWorkOnEitherSideOfThePath(t *testing.T) {
	cases := []struct {
		args  []string
		depth int
	}{
		{[]string{"scan.wmse", "-r"}, 0},
		{[]string{"-r", "scan.wmse"}, 0},
		{[]string{"scan.wmse", "-rdepth", "2", "-r"}, 2},
		{[]string{"-r", "-rdepth", "2", "scan.wmse"}, 2},
	}
	for _, c := range cases {
		cfg, fs := newFlagSet()
		path, err := parseRead(fs, c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if path != "scan.wmse" {
			t.Errorf("%v: path %q", c.args, path)
		}
		cfg.Apply(fs)
		if !cfg.Recursive {
			t.Errorf("%v: -r was not set", c.args)
		}
		if got := readDepth(cfg, fs); got != c.depth {
			t.Errorf("%v: depth %d, want %d", c.args, got, c.depth)
		}
	}
}

// TestRdepthCutsWhereAskedNotWhereTheScanStopped covers the case the flag exists
// for: the file was crawled to five, and the reader was asked for three. The
// depth is the reader's, and a smaller one is not a complaint - it is a request
// to see less than the file holds.
func TestRdepthCutsWhereAskedNotWhereTheScanStopped(t *testing.T) {
	cfg, fs := newFlagSet()
	if _, err := parseRead(fs, []string{"scan.wmse"}); err != nil {
		t.Fatal(err)
	}
	cfg.Apply(fs)
	if got := readDepth(cfg, fs); got != 0 {
		t.Errorf("without -rdepth the tree stops at %d, want the whole tree (0)", got)
	}
	cfg, fs = newFlagSet()
	if _, err := parseRead(fs, []string{"scan.wmse", "-rdepth", "3"}); err != nil {
		t.Fatal(err)
	}
	cfg.Apply(fs)
	if got := readDepth(cfg, fs); got != 3 {
		t.Errorf("with -rdepth 3 the tree stops at %d, want 3", got)
	}
}

// TestHelpIsNotAnError pins how a help request is answered. `webmap -h` prints
// help and exits 0; a reader that answered the same request with a diagnostic
// and a status of 1 would look broken next to it.
func TestHelpIsNotAnError(t *testing.T) {
	err := run("read", []string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: got %v, want flag.ErrHelp", err)
	}
	// main() must recognize that and not turn it into a diagnostic, which
	// would mean the usage block printed twice.
	var fe flagError
	if errors.As(err, &fe) {
		t.Error("-h was marked as a flag error, so main would print the usage twice")
	}
}

// TestBadFlagIsMarked covers the other half: a genuine flag failure is wrapped,
// because main answers it with the message *and* the flag list, the way the scan
// answers a bad flag.
func TestBadFlagIsMarked(t *testing.T) {
	err := run("read", []string{"-nope", "scan.wmse"})
	if err == nil {
		t.Fatal("an unknown flag should be an error")
	}
	var fe flagError
	if !errors.As(err, &fe) {
		t.Fatalf("bad flag is not a flagError: %v", err)
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Error("a bad flag must not be reported as a help request")
	}
	// The underlying error has to survive, or the message would lose the name
	// of the flag the user got wrong.
	if !strings.Contains(fe.Error(), "-nope") {
		t.Errorf("the wrapped error lost the flag name: %v", fe)
	}
}

func TestParseReadRejectsAmbiguity(t *testing.T) {
	_, fs := newFlagSet()
	if _, err := parseRead(fs, []string{"a.wmse", "b.wmse"}); err == nil {
		t.Error("two files should be an error")
	}
	_, fs = newFlagSet()
	if _, err := parseRead(fs, []string{"-r"}); err == nil {
		t.Error("no file should be an error")
	}
}

// TestRlimitIsOneBudgetForTheRequest pins the semantics of -rlimit here. In the
// scan it is a request budget; a file already holds the requests, so the same
// number has to bound the thing the reader actually produces - the lines - and it
// has to bound them across all sections at once, because "show me twenty lines"
// cannot mean twenty lines per section.
func TestRlimitIsOneBudgetForTheRequest(t *testing.T) {
	o := &options{cfg: &config.Config{RequestLimit: 3}, left: 3}
	for i := 0; i < 10; i++ {
		o.out("line")
	}
	if !o.stopped {
		t.Error("running out of budget should be recorded")
	}
	if o.left != 0 {
		t.Errorf("left: got %d, want 0", o.left)
	}
	// And it has to say so: ending in silence reads as "that is all of it".
	out := capture(t, func() { _ = o.finish() })
	if !strings.Contains(out, "-rlimit 3") {
		t.Errorf("an exhausted budget was not reported:\n%s", out)
	}
}

// TestSameHost drives the tree's rule for when a URL may print as a bare path.
func TestSameHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://x.com/a", "http://x.com/b", true},
		{"https://x.com/a", "https://y.com/a", false},
		{"https://x.com:8443/a", "https://x.com/a", false},
		{"/relative", "https://x.com/", false},
	}
	for _, c := range cases {
		if got := sameHost(c.a, c.b); got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestShortURLKeepsForeignHosts guards the readability rule: a URL on the target
// may be shortened, one anywhere else must not, because two documents with the
// same path on different hosts are two documents.
func TestShortURLKeepsForeignHosts(t *testing.T) {
	o := &options{target: "https://example.com/"}
	if got := o.shortURL("https://example.com/a/b?q=1"); got != "/a/b?q=1" {
		t.Errorf("target host: got %q, want /a/b?q=1", got)
	}
	if got := o.shortURL("https://cdn.other.com/a/b"); got != "https://cdn.other.com/a/b" {
		t.Errorf("foreign host: got %q", got)
	}
}

// TestReadMirrorsTheScanSections pins the shape of the report: the same sections,
// with the same flags asking for them, in the same order. A saved scan that read
// differently from the run that made it would defeat the point of the file.
func TestReadMirrorsTheScanSections(t *testing.T) {
	path := testFile(t)
	got := exercise(t, "read", path, "-r", "-apic", "-emulate", "-G", "-nogroup")
	if got.err != nil {
		t.Fatalf("read: %v\n%s", got.err, got.stderr)
	}
	out := got.out()
	order := []string{
		"WebMap snapshot",
		"=== WebMap Analysis (Same Domain Only) ===",
		"=== Hidden Classes ===",
		"=== Crawl Tree ===",
		"=== API Contracts ===",
		"=== Dynamic query params ===",
		"=== Browser Emulation Results (Same Domain Only) ===",
		"=== Relation Graph ===",
	}
	prev := -1
	for _, sec := range order {
		i := strings.Index(out, sec)
		if i < 0 {
			t.Fatalf("missing section %q in:\n%s", sec, out)
		}
		if i < prev {
			t.Errorf("section %q is out of the scan's order in:\n%s", sec, out)
		}
		prev = i
	}
	// -nogroup left the pattern section out, and -G left the link table out,
	// exactly as they would in the scan: two renderings of the same set is
	// noise, not depth.
	if strings.Contains(out, "=== URL Patterns ===") {
		t.Errorf("-nogroup did not drop the pattern section:\n%s", out)
	}
	if strings.Contains(out, "=== All Links ===") {
		t.Errorf("-G did not drop the link table:\n%s", out)
	}
}

// TestLinkTableIsTheDefaultReport covers the plain case, and the one shape rule
// worth stating on its own: with no flags, the report is the link table, and the
// hidden classes are named rather than silently dropped.
func TestLinkTableIsTheDefaultReport(t *testing.T) {
	path := testFile(t)
	got := exercise(t, "read", path)
	if got.err != nil {
		t.Fatalf("read: %v\n%s", got.err, got.stderr)
	}
	out := got.out()
	if !strings.Contains(out, "=== All Links ===") {
		t.Errorf("the default report has no link table:\n%s", out)
	}
	if !strings.Contains(out, "/api/x") {
		t.Errorf("an in-scope link is missing:\n%s", out)
	}
	// The CDN link is in the file and out of the report, and the report says so
	// instead of pretending the scan never saw it.
	if strings.Contains(out, "static.example.com/lib.js") {
		t.Errorf("a hidden class was printed without its flag:\n%s", out)
	}
	if !strings.Contains(out, "CDN: 1") || !strings.Contains(out, "-cdn to show") {
		t.Errorf("the hidden class was not accounted for:\n%s", out)
	}
	// And the flag that reveals it does reveal it, which is what makes the
	// hiding a default rather than a loss.
	revealed := exercise(t, "read", path, "-cdn").out()
	if !strings.Contains(revealed, "static.example.com/lib.js") {
		t.Errorf("-cdn did not reveal the class:\n%s", revealed)
	}
	if strings.Contains(revealed, "CDN: 1 (hidden") {
		t.Errorf("-cdn still reported its class as hidden:\n%s", revealed)
	}
}

// TestCountsAreTheScansOwn covers two decisions that have to agree with the scan
// rather than with what is convenient. The numbers in the analysis block are the
// ones the run printed: over every link the file holds, hidden classes included,
// so "CDN: 1 (hidden - use -cdn to show)" can be said. And they are counted here
// rather than read from the file, because a file's own counts were made by a
// different version of the counting rules and the report has to describe what it
// is about to print.
func TestCountsAreTheScansOwn(t *testing.T) {
	path := testFile(t)
	o := opened(t, path)
	if o.stats.Total != 4 {
		t.Errorf("counted %d links, want all 4 the file holds (it records 99)", o.stats.Total)
	}
	if got := o.stats.ByClass[linker.ClassCDN]; got != 1 {
		t.Errorf("ByClass: got %d CDN links, want 1 counted even though it is hidden", got)
	}
	// What the report prints is the class-filtered set, and it says so.
	shown := exercise(t, "read", path).out()
	if !strings.Contains(shown, "3 of 4 links") {
		t.Errorf("the link table did not say what it left out:\n%s", shown)
	}
	// The scan's title is a statement about the crawl, so it follows -a alone and
	// the counts are never narrowed under it.
	if !strings.Contains(shown, "Total links: 4") {
		t.Errorf("the totals were narrowed by the class filter:\n%s", shown)
	}
}

// TestScopeFlagsNarrowTheReport covers the flags whose meaning had to be decided
// offline. In the scan -url, -f and -a decide what gets fetched, and the report
// then lists every link the crawl found on any domain. A file cannot fetch
// anything, so with no scope flags the reader reports exactly the set the run
// reported, and naming a scope is the way to ask the crawl's question
// differently: what would I have seen?
func TestScopeFlagsNarrowTheReport(t *testing.T) {
	snap := testSnapshot()
	// A link on a subdomain, one on a foreign host and one on a third: all three
	// are in the file, and all three were in the run's report.
	snap.Links = append(snap.Links,
		linker.Link{HREF: "https://api.example.com/v1", Resolved: "https://api.example.com/v1", Domain: "api.example.com", Category: linker.CategoryAPI},
		linker.Link{HREF: "https://other.com/z", Resolved: "https://other.com/z", Domain: "other.com", Category: linker.CategoryWebPage},
		linker.Link{HREF: "https://third.com/q", Resolved: "https://third.com/q", Domain: "third.com", Category: linker.CategoryWebPage},
	)
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scope.wmse")
	if _, err := wmse.Write(path, snap, wmse.DefaultOptions()); err != nil {
		t.Fatal(err)
	}

	// No scope flags: the whole file, exactly as the run reported it, foreign
	// hosts and all.
	plain := exercise(t, "read", path)
	if !strings.Contains(plain.out(), "other.com/z") {
		t.Errorf("a link the run reported is missing from the report:\n%s", plain.out())
	}
	if !strings.Contains(plain.out(), "Total links: 7") {
		t.Errorf("the totals are not the run's own:\n%s", plain.out())
	}

	// -f adds a domain to the entry point's, the crawl's rule, so the target and
	// its subdomains stay in and the new one joins them.
	follow := exercise(t, "read", path, "-f", "other.com")
	if !strings.Contains(follow.out(), "other.com/z") {
		t.Errorf("-f did not add the domain:\n%s", follow.out())
	}
	if !strings.Contains(follow.out(), "narrowed to") {
		t.Errorf("-f did not say that it narrowed anything:\n%s", follow.out())
	}
	if strings.Contains(follow.out(), "third.com/q") {
		t.Errorf("-f other.com did not exclude an unnamed domain:\n%s", follow.out())
	}
	// A subdomain follows its parent, the crawl's rule.
	sub := exercise(t, "read", path, "-f", "example.com")
	if !strings.Contains(sub.out(), "api.example.com/v1") {
		t.Errorf("-f does not cover subdomains:\n%s", sub.out())
	}
	if strings.Contains(sub.out(), "other.com/z") {
		t.Errorf("-f does not replace the entry point's domain:\n%s", sub.out())
	}

	// -url moves the entry point, which is what makes a pasted scan command
	// line mean the same thing: the file supplies the links, the flag still says
	// which one to report on.
	moved := exercise(t, "read", path, "-url", "https://other.com/")
	if !strings.Contains(moved.out(), "other.com/z") {
		t.Errorf("-url did not move the entry point:\n%s", moved.out())
	}
	if strings.Contains(moved.out(), "api.example.com/v1") {
		t.Errorf("-url did not move the scope with it:\n%s", moved.out())
	}
	all := exercise(t, "read", path, "-a")
	if !strings.Contains(all.out(), "All Domains") {
		t.Errorf("-a did not title the report as the scan does:\n%s", all.out())
	}
	if strings.Contains(all.out(), "narrowed to") {
		t.Errorf("-a narrowed the report instead of widening it:\n%s", all.out())
	}
}

// TestARequestTheFileCannotAnswerIsAnError covers the rule that shapes this
// command: a flag asking for data the file does not hold is an error naming the
// flag, with the metadata of the run that made the file - not an empty section,
// which reads as a site with nothing in it. It is also checked for staying off
// stdout, because a report gets piped.
func TestARequestTheFileCannotAnswerIsAnError(t *testing.T) {
	// A file from a scan that emulated nothing and found no contracts.
	snap := testSnapshot()
	snap.Emulation = nil
	snap.Endpoints = nil
	snap.Observations = nil
	snap.Meta["analyze_js"] = "false"
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "thin.wmse")
	if _, err := wmse.Write(path, snap, wmse.DefaultOptions()); err != nil {
		t.Fatal(err)
	}

	got := exercise(t, "read", path, "-apic", "-emulate")
	if got.err == nil {
		t.Error("asking for data the file does not hold should fail")
	}
	var un unavailable
	if !errors.As(got.err, &un) {
		t.Errorf("the failure is not the one main exits 1 on: %v", got.err)
	}
	for _, want := range []string{"-apic", "-emulate"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("the error does not name %s:\n%s", want, got.stderr)
		}
	}
	// The reasons, then the metadata that explains them, then the report.
	errLine := strings.Index(got.stderr, "wmse: -apic")
	header := strings.Index(got.stdout, "WebMap snapshot")
	if errLine < 0 || header < 0 {
		t.Fatalf("missing either the reason or the header:\nstdout:\n%s\nstderr:\n%s", got.stdout, got.stderr)
	}
	if !strings.Contains(got.stdout, "https://example.com/") {
		t.Errorf("the saved metadata did not come with the error:\n%s", got.stdout)
	}
	// The parts that the file does hold are still reported: the flag that
	// failed is the one thing dropped, not the whole report.
	if !strings.Contains(got.stdout, "=== All Links ===") {
		t.Errorf("one unavailable section cost the rest of the report:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, "wmse:") {
		t.Errorf("a diagnostic leaked into the report:\n%s", got.stdout)
	}
	// And the header appears once, not once per view that wants it.
	if n := strings.Count(got.stdout, "WebMap snapshot"); n != 1 {
		t.Errorf("the file header printed %d times:\n%s", n, got.stdout)
	}
}

// TestRdepthBeyondTheFileIsAnError is the other direction: asking for a depth
// the crawl was never allowed to reach is a request the file cannot fill, and it
// has to say so - otherwise a reader would take the tree for proof that the site
// ends there. A crawl that stopped at its own limit is a different case from one
// that ran out of pages, and only the recorded limit tells the two apart.
func TestRdepthBeyondTheFileIsAnError(t *testing.T) {
	snap := testSnapshot()
	snap.Pages[1].Depth = 2
	snap.Meta[wmse.MetaMaxDepth] = "3"
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deep.wmse")
	if _, err := wmse.Write(path, snap, wmse.DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	// Deeper than the crawl was allowed: those pages were never fetched.
	if got := exercise(t, "read", path, "-r", "-rdepth", "9"); got.err == nil ||
		!strings.Contains(got.stderr, "allowed depth 3") {
		t.Errorf("a depth past the crawl's limit was not reported:\n%s", got.stderr)
	}
	// Deeper than the pages reach, but within the limit: the site ended there and
	// nothing is missing.
	ok := exercise(t, "read", path, "-r", "-rdepth", "3")
	if ok.err != nil {
		t.Errorf("a depth within the crawl's limit was refused: %v\n%s", ok.err, ok.stderr)
	}
	if !strings.Contains(ok.out(), "=== Crawl Tree ===") {
		t.Errorf("the whole tree was not printed:\n%s", ok.out())
	}
}

// TestScopeThatMatchesNothingIsAnError covers the case where the flags select a
// set of links the file does not have. Silence would be read as an empty site.
func TestScopeThatMatchesNothingIsAnError(t *testing.T) {
	path := testFile(t)
	got := exercise(t, "read", path, "-url", "https://nowhere.example/")
	if got.err == nil {
		t.Error("a scope with no links in it should fail")
	}
	if !strings.Contains(got.stderr, "no links in this file are in scope") {
		t.Errorf("the error does not explain the empty scope:\n%s", got.stderr)
	}
	// It has to name what the file does have, or the reader cannot tell a wrong
	// scope from a wrong file.
	if !strings.Contains(got.stderr, "example.com") {
		t.Errorf("the error does not say which hosts the file has:\n%s", got.stderr)
	}
}

// TestLinkTableMatchesTheRuns pins the difference between the file's link set and
// the report's: the file also holds the pages a crawl visited, the nodes a
// pattern absorbs, and one record per query variant. A table that printed all of
// them would show the same URLs twice in two shapes and read as more site than
// there is, and it would not be the report the run printed.
func TestLinkTableMatchesTheRuns(t *testing.T) {
	snap := testSnapshot()
	// Two records of one endpoint, the way a crawl finds them, and a link the
	// fixture's pattern already accounts for.
	snap.Links = append(snap.Links, linker.Link{
		HREF: "/item", Resolved: "https://example.com/item", Domain: "example.com",
		Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/",
	}, linker.Link{
		HREF: "/item?id=7", Resolved: "https://example.com/item?id=7", Domain: "example.com",
		Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/", HasParams: true,
	}, linker.Link{
		HREF: "/p/1", Resolved: "https://example.com/p/1", Domain: "example.com",
		Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/",
	})
	if err := snap.Normalize("https://example.com/"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rows.wmse")
	if _, err := wmse.Write(path, snap, wmse.DefaultOptions()); err != nil {
		t.Fatal(err)
	}

	out := exercise(t, "read", path, "-cdn").out()
	table := section(out, "All Links")
	// One row for the endpoint, with the queries it was called with.
	rows := 0
	for _, l := range strings.Split(table, "\n") {
		if strings.Contains(l, "example.com/item") {
			rows++
			if !strings.Contains(l, "params: id=7") {
				t.Errorf("the row does not say what the endpoint was called with: %q", l)
			}
		}
	}
	if rows != 1 {
		t.Errorf("the two records of one endpoint produced %d rows, want 1:\n%s", rows, table)
	}
	// A member of a confirmed pattern is spoken for by the pattern section.
	if strings.Contains(table, "/p/1") {
		t.Errorf("a pattern member was also listed as a link:\n%s", table)
	}
	if !strings.Contains(section(out, "URL Patterns"), "/p/1") {
		t.Errorf("the pattern does not list its members:\n%s", out)
	}
	// And -nogroup is the scan's "do not group", so the members come back.
	ungrouped := section(exercise(t, "read", path, "-cdn", "-nogroup").out(), "All Links")
	if !strings.Contains(ungrouped, "/p/1") {
		t.Errorf("-nogroup did not put the members back in the table:\n%s", ungrouped)
	}
}

// section returns the body of one report section, so an assertion about what a
// table contains does not also match a line of another section that mentions the
// same URL.
func section(out, name string) string {
	_, body, ok := strings.Cut(out, "=== "+name+" ===")
	if !ok {
		return ""
	}
	for _, marker := range []string{"\n===", "\n---"} {
		if before, _, found := strings.Cut(body, marker); found {
			body = before
		}
	}
	return body
}

// TestApicRawShowsTheEvidence covers the flag that makes a saved scan worth more
// than a re-run: the requests the contracts were inferred from, in full.
func TestApicRawShowsTheEvidence(t *testing.T) {
	path := testFile(t)
	plain := exercise(t, "read", path, "-apic")
	if strings.Contains(plain.out(), "X-Token") {
		t.Errorf("-apic printed request evidence on its own:\n%s", plain.out())
	}
	raw := exercise(t, "read", path, "-apic-raw")
	if raw.err != nil {
		t.Fatalf("read: %v\n%s", raw.err, raw.stderr)
	}
	out := raw.out()
	// It implies -apic, so the contracts are there too.
	if !strings.Contains(out, "=== API Contracts ===") {
		t.Errorf("-apic-raw did not imply -apic:\n%s", out)
	}
	if !strings.Contains(out, "=== Request Observations ===") {
		t.Errorf("-apic-raw did not print the observations:\n%s", out)
	}
	if !strings.Contains(out, "X-Token: s3cr3t") {
		t.Errorf("a request header was lost:\n%s", out)
	}
	if !strings.Contains(out, `body: {"name":"a"}`) {
		t.Errorf("a request body was lost:\n%s", out)
	}
}

// TestTagAndAPIFullAreReportOnly checks the two flags that change a row rather
// than a section, and the one that replaces a row with several.
func TestTagAndAPIFullAreReportOnly(t *testing.T) {
	path := testFile(t)
	plain := exercise(t, "read", path).out()
	if !strings.HasPrefix(rowFor(plain, "/a"), "relative web-page") {
		t.Errorf("a tag was printed without -t:\n%s", plain)
	}
	tagged := exercise(t, "read", path, "-t").out()
	if !strings.HasPrefix(rowFor(tagged, "/a"), "relative a") {
		t.Errorf("-t did not put the tag where the category goes:\n%s", tagged)
	}
	// The arguments survived the file: the scan's -apif is the one report row
	// that needs them, and a format that dropped them would make the flag
	// unanswerable offline.
	full := exercise(t, "read", path, "-apif").out()
	if !strings.Contains(full, "POST") || !strings.Contains(full, "args: id, name") {
		t.Errorf("-apif did not print the method and args:\n%s", full)
	}
}

// rowFor returns the printed link row that mentions a path.
func rowFor(out, path string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, path) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// TestParamsSkipWhatIsAlreadyReported covers the rule the scan's own params
// section follows: a parameter that a link or a contract already shows must not
// be reported a second time in another place, or a reader cannot tell which of
// the two is the finding.
func TestParamsSkipWhatIsAlreadyReported(t *testing.T) {
	path := testFile(t)
	out := exercise(t, "read", path).out()
	if !strings.Contains(out, "=== Dynamic query params ===") {
		t.Fatalf("no params section:\n%s", out)
	}
	if !strings.Contains(out, "page") {
		t.Errorf("the parameter nothing else reports is missing:\n%s", out)
	}
	if strings.Contains(out, "  token ") {
		t.Errorf("a parameter the contracts already report was repeated:\n%s", out)
	}
}

// TestViewsDoNotPanicOnAnEmptyScan covers the degenerate file: a scan that found
// nothing still has to be readable, because "I saved it and now it crashes" is
// the worst possible first impression of a format.
func TestViewsDoNotPanicOnAnEmptyScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.wmse")
	if _, err := wmse.Write(path, &wmse.Snapshot{Meta: wmse.Meta{wmse.MetaTarget: "https://example.com/"}}, wmse.DefaultOptions()); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Every verb, with every flag that asks for a section: the empty file
	// answers none of them, and the complaint path is the one being walked.
	for _, verb := range []string{"read", "info", "json"} {
		got := exercise(t, verb, path, "-r", "-apic", "-emulate", "-G", "-rdepth", "3")
		if verb != "read" && got.err != nil {
			t.Errorf("%s on an empty file: %v", verb, got.err)
		}
	}
	got := exercise(t, "read", path)
	if !strings.Contains(got.out(), "found nothing") {
		t.Errorf("an empty file did not say so plainly:\n%s", got.out())
	}
}

// TestInfoDescribesTheFile covers the verb that answers "is this actually small,
// and what is in it": the storage table and the full record of the run.
func TestInfoDescribesTheFile(t *testing.T) {
	path := testFile(t)
	got := exercise(t, "info", path)
	if got.err != nil {
		t.Fatalf("info: %v", got.err)
	}
	out := got.out()
	for _, want := range []string{"=== Storage ===", "links", "=== Saved Metadata ===", wmse.MetaCommand} {
		if !strings.Contains(out, want) {
			t.Errorf("info does not mention %q:\n%s", want, out)
		}
	}
	// The whole point of the verb: the size, in the units a person can act on.
	if !strings.Contains(out, "B") && !strings.Contains(out, "KiB") {
		t.Errorf("info reports no size:\n%s", out)
	}
}

// TestJSONIsWellFormed parses the json verb's output back. The encoder is written
// by hand for streaming, and a hand-written encoder that emits a missing comma
// still looks plausible on screen, so the only honest test is to read it back.
func TestJSONIsWellFormed(t *testing.T) {
	path := testFile(t)
	got := exercise(t, "json", path)
	if got.err != nil {
		t.Fatalf("json: %v", got.err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.stdout)
	}
	for _, key := range []string{"format", "meta", "stats", "pages", "links", "edges", "patterns", "endpoints", "observations", "params", "emulation"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}
	links, _ := doc["links"].([]any)
	if len(links) != 4 {
		t.Errorf("links: got %d, want all 4 in the file - the report filters, this does not", len(links))
	}
	eps, _ := doc["endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("endpoints: got %d, want 1", len(eps))
	}
	ep, _ := eps[0].(map[string]any)
	if ep["url"] != "/api/x" {
		t.Errorf("endpoint url: got %v, want /api/x", ep["url"])
	}
	// The evidence has to survive verbatim, quotes and all: a saved scan whose
	// JSON re-encoded its own bodies would be a lie about what was sent.
	obs, _ := doc["observations"].([]any)
	if len(obs) != 1 {
		t.Fatalf("observations: got %d, want 1", len(obs))
	}
	o0, _ := obs[0].(map[string]any)
	if o0["body"] != `{"name":"a"}` {
		t.Errorf("observation body: got %v", o0["body"])
	}
}

// TestJSONRoundTripsThroughStandardParsers is the check a user would actually
// perform: pipe it into another tool and read it back with a stock parser.
func TestJSONRoundTripsThroughStandardParsers(t *testing.T) {
	path := testFile(t)
	got := exercise(t, "json", path)
	// Decoding into a typed shape fails if any value has the wrong JSON type,
	// which is the part a lenient map[string]any decode would let through.
	var typed struct {
		Format string `json:"format"`
		Links  []struct {
			URL      string `json:"url"`
			Category string `json:"category"`
			Depth    int    `json:"depth"`
			Source   string `json:"source"`
		} `json:"links"`
		Params []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &typed); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if typed.Format != "wmse" {
		t.Errorf("format: got %q", typed.Format)
	}
	if len(typed.Links) != 4 {
		t.Errorf("links: got %d, want 4", len(typed.Links))
	}
	if len(typed.Params) != 2 {
		t.Errorf("params: got %+v", typed.Params)
	}
	// The field the report has no column for is the one the JSON verb exists
	// for.
	found := false
	for _, l := range typed.Links {
		if l.Source != "" {
			found = true
		}
	}
	if !found {
		t.Error("no link says which page it was found on")
	}
}

// nodeLines returns the printed nodes of a view, without its heading or blanks.
func nodeLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "===") {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

// chainScan builds a three-level site: / -> /mid -> /leaf, every level a page,
// so the tree has something to cut off. The middle link is a CDN URL, which the
// report hides by default: the tree therefore has to reach /leaf by walking
// through a node it does not print.
func chainScan(t *testing.T) string {
	t.Helper()
	root, mid, leaf := "https://example.com/", "https://example.com/mid", "https://example.com/leaf"
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{wmse.MetaTool: "webmap", wmse.MetaTarget: root, wmse.MetaFormat: "v1"},
		Links: []linker.Link{
			{HREF: root, Resolved: root, Category: linker.CategoryWebPage, LinkType: linker.LinkTypeAbsolute},
			{HREF: "/mid", Resolved: mid, Domain: "example.com", Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, SourceURL: root, Class: linker.ClassCDN},
		},
		Pages: []wmse.Page{
			{URL: root, Depth: 0, ContentType: "text/html"},
			{URL: mid, Depth: 1, ContentType: "text/html"},
			{URL: leaf, Depth: 2, ContentType: "text/html"},
		},
	}
	// The relations are derived by the writer, not by Normalize, so a view can
	// only be tested against a snapshot that has been through it. Building the
	// graph in the test instead would test the test.
	snap.Links = append(snap.Links, linker.Link{
		HREF: "/leaf", Resolved: leaf, Domain: "example.com",
		Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, SourceURL: mid,
	})
	if err := snap.Normalize(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chain.wmse")
	if _, err := wmse.Write(path, snap, wmse.DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTreeDepthDoesNotInventRoots covers a bug this view had: a depth limit that
// stopped the walk left the deeper pages unvisited, which made the pass that
// reports genuinely unreachable pages print them as if the site had no path to
// them. A depth limit is a request to show less, not a claim about what is
// reachable, and a reader who cannot tell those apart cannot trust what they can
// see.
func TestTreeDepthDoesNotInventRoots(t *testing.T) {
	path := chainScan(t)
	full := capture(t, func() { viewTree(opened(t, path, "-r", "-cdn")) })
	if !strings.Contains(full, "/leaf") {
		t.Fatalf("the full tree should reach /leaf:\n%s", full)
	}
	shallow := capture(t, func() { viewTree(opened(t, path, "-r", "-cdn", "-rdepth", "1")) })
	if strings.Contains(shallow, "/leaf") {
		t.Errorf("-rdepth 1 showed a depth-2 page:\n%s", shallow)
	}
	// A cut tree that ends in silence reads as the end of the site, and this is
	// a file that holds everything below the cut.
	if !strings.Contains(shallow, "1 more page below -rdepth 1") {
		t.Errorf("the cut was not reported:\n%s", shallow)
	}
	// Below the first line, every node must hang off something. A cut-off tree
	// that reprints its deep pages at the left margin is claiming the site has
	// no path to them, which is the opposite of what -rdepth means.
	for _, line := range nodeLines(shallow)[1:] {
		if !strings.HasPrefix(line, " ") {
			t.Errorf("unindented line in a depth-limited tree: %q in\n%s", line, shallow)
		}
	}
}

// TestTreeKeepsWalkingThroughHiddenNodes is the other half of that fix: a node the
// filters removed is not a dead end, and its children are still what the reader
// asked for. Here /mid is a CDN URL, hidden unless -cdn is given, so the walk has
// to pass through it to reach /leaf.
func TestTreeKeepsWalkingThroughHiddenNodes(t *testing.T) {
	path := chainScan(t)
	got := capture(t, func() { viewTree(opened(t, path, "-r")) })
	if strings.Contains(got, "/mid") {
		t.Errorf("a hidden class was printed in the tree:\n%s", got)
	}
	// /leaf is two levels down and /mid is not printed, so the only way /leaf
	// can appear indented is if the walk descended through the node it dropped.
	if !strings.Contains(got, "  /leaf") {
		t.Errorf("the walk stopped at a hidden node instead of passing through it:\n%s", got)
	}
	for _, line := range nodeLines(got) {
		if strings.Contains(line, "/leaf") && !strings.HasPrefix(line, " ") {
			t.Errorf("/leaf lost its depth: %q in\n%s", line, got)
		}
	}
}

// TestHostsDoNotCallSynthesizedPagesRelative pins the same class of mistake in a
// different place. A node the collector added to give a relation an endpoint has
// no domain field but does have a URL, and grouping it under "relative" would be
// a claim about it that is not true.
func TestHostsDoNotCallSynthesizedPagesRelative(t *testing.T) {
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{wmse.MetaTarget: "https://example.com/"},
		Links: []linker.Link{
			{HREF: "/a", Resolved: "https://example.com/a", Domain: "example.com"},
			// No Domain: a page node the collector synthesized.
			{HREF: "https://example.com/mid", Resolved: "https://example.com/mid"},
		},
	}
	counts := hostCounts(snap)
	for _, h := range counts {
		if h.host == "(relative)" {
			t.Errorf("an absolute node was grouped as relative: %+v", counts)
		}
	}
	if len(counts) != 1 || counts[0].host != "example.com" || counts[0].count != 2 {
		t.Errorf("hosts: got %+v, want both under example.com", counts)
	}
}

// TestSessionDataWarningIsShown pins the one thing a reader must not walk past: a
// file that holds requests as they were sent may hold a token.
func TestSessionDataWarningIsShown(t *testing.T) {
	path := testFile(t)
	o := opened(t, path)
	got := capture(t, func() { viewHeader(o) })
	if !strings.Contains(got, "session material") {
		t.Errorf("a file marked session_data=true was opened without a warning:\n%s", got)
	}

	o.snap.Meta[wmse.MetaSessionData] = "false"
	quiet := capture(t, func() { viewHeader(o) })
	if strings.Contains(quiet, "session material") {
		t.Errorf("a file with no session data warned anyway:\n%s", quiet)
	}
}
