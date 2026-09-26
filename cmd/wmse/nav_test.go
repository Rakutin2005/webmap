package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// navFixture is a site shaped for the navigation model: a page with children, a
// directory of pages, a directory that names its own index, and a directory that
// does not. Between them they cover every decision the session makes about what
// kind of place it is standing at, and each case is here for exactly one of
// them, so a rule that changes shows up as one failing test rather than a
// general drift.
func navFixture(t *testing.T) string {
	t.Helper()
	page := func(url string, depth int, links int) wmse.Page {
		return wmse.Page{URL: url, Depth: depth, ContentType: "text/html; charset=utf-8", Links: links}
	}
	link := func(href, resolved, src string) linker.Link {
		return linker.Link{
			HREF: href, Resolved: resolved, Domain: "example.com",
			Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative,
			Depth: 1, SourceURL: src, Tag: "a",
		}
	}
	const root = "https://example.com/"
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{
			wmse.MetaTool:    "webmap",
			wmse.MetaVersion: "1.3.3",
			wmse.MetaFormat:  "v1",
			wmse.MetaTarget:  root,
			wmse.MetaScope:   "recursive depth<=5 threads=32",
			wmse.MetaCommand: "webmap -r -o test.wmse -url " + root,
			wmse.MetaCreated: "2026-09-25T10:11:12Z",
		},
		Links: []linker.Link{
			{HREF: root, Resolved: root, Domain: "example.com", Category: linker.CategoryWebPage, LinkType: linker.LinkTypeAbsolute},
			link("/a", root+"a", root),
			link("/dir", root+"dir", root),
			link("/withindex", root+"withindex", root),
			link("/noindex", root+"noindex", root),
			// /a has children of its own, so standing at /a and standing at the
			// root must answer differently: the root's links are not /a's.
			link("child", root+"a/child", root+"a"),
			link("other", root+"a/other", root+"a"),
			// A directory of pages, none of which is an index.
			link("/dir/1", root+"dir/1", root),
			link("/dir/2", root+"dir/2", root),
			// The index of a directory that has one. Nothing is at /withindex
			// itself, so the session has to reach through to find the page.
			link("/withindex/index.html", root+"withindex/index.html", root),
			// A path that was never fetched. It is a page by classification but
			// no document exists for it, which is what keeps /noindex a
			// directory rather than a page with no links.
			link("/noindex/deep/1", root+"noindex/deep/1", root),
		},
		Pages: []wmse.Page{
			page(root, 0, 4),
			page(root+"a", 1, 2),
			page(root+"dir/1", 1, 0),
			page(root+"dir/2", 1, 0),
			page(root+"withindex/index.html", 1, 0),
		},
	}
	return writeFixture(t, snap)
}

// stand is a session over the navigation fixture, with the commands run.
func stand(t *testing.T, cmds ...string) (*session, string) {
	t.Helper()
	s := openSession(t, navFixture(t))
	var b strings.Builder
	s.out = &b
	s.runLines(newPipeSource(strings.NewReader(strings.Join(cmds, "\n") + "\n")))
	return s, b.String()
}

// TestSessionOpensOnTheTargetAsAPage: a site root is a document, so the session
// opens standing on it and `ls` answers about that document.
func TestSessionOpensOnTheTargetAsAPage(t *testing.T) {
	s, _ := stand(t)
	if s.pos != "https://example.com/" {
		t.Errorf("opened at %q, want the target", s.pos)
	}
	if s.dir {
		t.Errorf("the target is a page, so the session opened in directory mode")
	}
}

// TestSessionLsListsOnlyTheCurrentPage is the rule the whole page/directory split
// exists for: `ls` on a page is that page's own links, not the site's.
func TestSessionLsListsOnlyTheCurrentPage(t *testing.T) {
	_, atRoot := stand(t, "ls")
	// The root's four links, each named once.
	for _, want := range []string{"/a", "/dir", "/withindex", "/noindex"} {
		if !strings.Contains(atRoot, want) {
			t.Errorf("ls at the root did not list %s:\n%s", want, atRoot)
		}
	}
	if strings.Contains(atRoot, "/a/child") {
		t.Errorf("ls at the root listed a link from another page:\n%s", atRoot)
	}

	_, atA := stand(t, "cd /a", "ls")
	if !strings.Contains(atA, "/a/child") || !strings.Contains(atA, "/a/other") {
		t.Errorf("ls at /a did not list its own links:\n%s", atA)
	}
	// The decisive check: a link of the root's, and of no other page, must be
	// absent. If it is present, `ls` is listing the site and not the page.
	for _, other := range []string{"/dir", "/withindex", "/noindex"} {
		if strings.Contains(atA, other) {
			t.Errorf("ls at /a listed %s, which belongs to another page:\n%s", other, atA)
		}
	}
}

// TestSessionStandAtAPage: a page's links come from the file's own relation, so
// the count is the one the scan recorded.
func TestSessionStandAtAPage(t *testing.T) {
	_, out := stand(t, "cd /a", "ls")
	if !strings.Contains(out, "2 links on this page") {
		t.Errorf("ls at /a did not report the page's own link count:\n%s", out)
	}
}

// TestSessionStandAtADirectory: with no document at the path, the session is in a
// directory, and `ls` answers with the paths that match it.
func TestSessionStandAtADirectory(t *testing.T) {
	s, out := stand(t, "cd /dir", "ls")
	if !s.dir {
		t.Errorf("/dir holds no page, so the session should be in directory mode")
	}
	if !strings.Contains(out, "/dir/1") || !strings.Contains(out, "/dir/2") {
		t.Errorf("ls in /dir did not list what is under it:\n%s", out)
	}
	if strings.Contains(out, "/a/child") {
		t.Errorf("ls in /dir listed a path outside it:\n%s", out)
	}
}

// TestSessionIndexPageWins: /withindex has no page of its own but its index does,
// so the session stands on the index, the way a browser would.
func TestSessionIndexPageWins(t *testing.T) {
	s, out := stand(t, "cd /withindex", "ls")
	if s.dir {
		t.Errorf("an existing index is a page, so the session should be on the index")
	}
	if s.pos != "https://example.com/withindex/index.html" {
		t.Errorf("stood at %q, want the index page", s.pos)
	}
	if !strings.Contains(out, "index.html") {
		t.Errorf("the prompt or listing should name the index:\n%s", out)
	}
}

// TestSessionDirectoryWithoutAnIndex: /noindex is the case the rule is written
// for. Nothing is at the path, so it is a directory, and `ls` lists everything
// whose path matches it - which is the answer even though no index exists.
func TestSessionDirectoryWithoutAnIndex(t *testing.T) {
	s, out := stand(t, "cd /noindex", "ls")
	if !s.dir {
		t.Errorf("no page and no index at /noindex, so it is a directory")
	}
	if !strings.Contains(out, "/noindex/deep/1") {
		t.Errorf("ls should list the paths matching the directory:\n%s", out)
	}
}

// TestSessionLsDashDForcesTheDirectoryView is the override: where a page answers
// for a path, `ls -d` still answers as a directory, and the session moves there
// because that is what the command is for.
func TestSessionLsDashDForcesTheDirectoryView(t *testing.T) {
	s, out := stand(t, "ls -d /a")
	if !s.dir || s.pos != "https://example.com/a" {
		t.Errorf("ls -d should stand in the directory at /a, got %q dir=%v", s.pos, s.dir)
	}
	if !strings.Contains(out, "/a/child") {
		t.Errorf("ls -d /a should list the paths under it:\n%s", out)
	}
	if strings.Contains(out, "on this page") {
		t.Errorf("ls -d listed a page where a directory was asked for:\n%s", out)
	}
}

// TestSessionLsPathDoesNotMove: `ls <path>` answers about a path and leaves the
// prompt where it was, which is the difference from `cd` and from `ls -d`.
func TestSessionLsPathDoesNotMove(t *testing.T) {
	s, out := stand(t, "cd /dir", "ls /a")
	if s.pos != "https://example.com/dir" {
		t.Errorf("ls with a path moved the session to %q", s.pos)
	}
	if !strings.Contains(out, "/a/child") {
		t.Errorf("ls /a should have listed /a's links:\n%s", out)
	}
}

// TestSessionRelativePathsResolveAgainstThePosition: standing in a directory, a
// name typed inside it lands inside it, and at a page it lands beside it.
func TestSessionRelativePathsResolveAgainstThePosition(t *testing.T) {
	s, _ := stand(t, "cd /dir", "cd 1")
	if s.pos != "https://example.com/dir/1" {
		t.Errorf("cd 1 in /dir went to %q, want the page inside it", s.pos)
	}
	s, _ = stand(t, "cd /dir/1", "cd ..")
	if s.pos != "https://example.com/dir" {
		t.Errorf("cd .. from /dir/1 went to %q, want the containing directory", s.pos)
	}
	// "." is the place itself, as in a shell. Left to URL resolution it names the
	// containing directory, so `what .` would answer about somewhere else and
	// `cd .` would move when it should not.
	s, out := stand(t, "cd /a", "cd .", "what .")
	if s.pos != "https://example.com/a" {
		t.Errorf("cd . moved the session to %q", s.pos)
	}
	if !strings.Contains(out, "kind      web-page") {
		t.Errorf("what . did not describe the place the session is at:\n%s", out)
	}
}

// TestSessionCdWithNoPathReturnsToTheTarget: there is always a way back.
func TestSessionCdWithNoPathReturnsToTheTarget(t *testing.T) {
	s, _ := stand(t, "cd /dir/1", "cd")
	if s.pos != "https://example.com/" {
		t.Errorf("cd with no path went to %q, want the target", s.pos)
	}
}

// TestSessionSegment is the name the prompt shows: the last piece of the path,
// which is a page's file name in one mode and a directory's name in the other.
func TestSessionSegment(t *testing.T) {
	for _, c := range []struct{ url, want string }{
		{"https://example.com/", "/"},
		{"https://example.com/a", "a"},
		{"https://example.com/dir", "dir"},
		{"https://example.com/dir/1", "1"},
		{"https://example.com/index.html", "index.html"},
		{"https://example.com/dir/1?sort=asc", "1"},
		{"https://example.com/dir/", "dir"},
		{"", "/"},
	} {
		s := &session{pos: c.url}
		if got := s.segment(); got != c.want {
			t.Errorf("segment(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestSessionPromptShowsWhereAndWhat: the prompt carries the position and the
// name, so a person reading it knows both where they are and what kind of place
// it is without running anything.
func TestSessionPromptShowsWhereAndWhat(t *testing.T) {
	s, _ := stand(t, "cd /dir/1")
	if got := s.promptLine(); got != "explore@https://example.com/dir/1 [1] > " {
		t.Errorf("prompt is %q", got)
	}
}

// TestSessionCompleteOffersTheRightThingPerCommand is what makes Tab worth
// having: a path after `cd`, a flag after `ls`, a kind after `find`, and a file
// on disk after `extend` are four different sets.
func TestSessionCompleteOffersTheRightThingPerCommand(t *testing.T) {
	s, _ := stand(t)
	// A command name, completed with a trailing space so the next word starts
	// ready to be typed. The list is unfiltered: the editor narrows it to the
	// word being typed, so that the same list serves every prefix.
	names := s.complete([]rune("cd "), 0, "")
	if !contains(names, "cd ") {
		t.Errorf("completing a first word did not offer cd: %q", names)
	}
	for _, n := range names {
		if !strings.HasSuffix(n, " ") {
			t.Errorf("command %q was offered without the space that ends it", n)
		}
	}
	// A path, for a command that takes one.
	if got := s.complete([]rune("what /a"), 6, "/a"); len(got) == 0 || got[0] != "/a" {
		t.Errorf("completing /a gave %q", got)
	}
	// A bare name matches a segment anywhere in a path, because a name is the
	// part of a path a person remembers.
	if got := s.complete([]rune("what child"), 6, "child"); len(got) != 1 || got[0] != "/a/child" {
		t.Errorf("completing the bare word `child` gave %q", got)
	}
	// The -d flag, which is the one flag `ls` takes.
	if got := s.complete([]rune("ls -"), 4, "-"); len(got) != 1 || got[0] != "-d" {
		t.Errorf("completing a flag after ls gave %q", got)
	}
	// The kinds `find` accepts, and nothing for its second argument, which is a
	// glob to be written rather than chosen from a list.
	if got := s.complete([]rune("find "), 5, ""); len(got) == 0 || got[0] != "any" {
		t.Errorf("completing a kind gave %q", got)
	}
	if got := s.complete([]rune("find api "), 9, ""); got != nil {
		t.Errorf("completing a glob offered %q", got)
	}
	// A command that takes no argument offers nothing rather than a guess.
	if got := s.complete([]rune("info "), 5, ""); got != nil {
		t.Errorf("completing after info offered %q", got)
	}
}

// TestSessionExtendRefreshesCompletions: the completable paths come from the
// merged data, so a scan added halfway through a session must add to them.
func TestSessionExtendRefreshesCompletions(t *testing.T) {
	s, _ := stand(t)
	before := len(s.completePath("z"))
	second := testSnapshot()
	second.Links = append(second.Links, linker.Link{
		HREF: "https://example.com/z", Resolved: "https://example.com/z",
		Domain: "example.com", Category: linker.CategoryWebPage,
		LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/",
	})
	s.files = append(s.files, loadedFile{path: "x", snap: second})
	s.merged = mergeSnapshots(s.merged, second)
	s.nodes, s.pages, s.paths = nil, nil, nil
	if after := len(s.completePath("z")); after <= before {
		t.Errorf("after extend, completing z offered %d paths, was %d", after, before)
	}
}

// TestSessionHistoryCommand writes down what was typed, so the list the arrows
// walk can be read as well as remembered.
func TestSessionHistoryCommand(t *testing.T) {
	_, out := stand(t, "cd /dir", "ls", "history")
	if !strings.Contains(out, "cd /dir") || !strings.Contains(out, "ls") {
		t.Errorf("history did not list the session's commands:\n%s", out)
	}
}

// TestSessionPageReachedWithAQueryIsTheSamePlace: a page the scan fetched as
// /item?id=1 is the page a person means by /item. Every other command already
// answers for the bare path, and navigation that insisted on the query would be
// the one place where the same name meant two things.
func TestSessionPageReachedWithAQueryIsTheSamePlace(t *testing.T) {
	link := linker.Link{
		HREF: "/q?sort=asc", Resolved: "https://example.com/q?sort=asc",
		Domain: "example.com", Category: linker.CategoryWebPage,
		LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/",
	}
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{wmse.MetaTarget: "https://example.com/"},
		Links: []linker.Link{
			link,
			{HREF: "/q", Resolved: "https://example.com/q", Domain: "example.com",
				Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative,
				Depth: 1, SourceURL: "https://example.com/"},
		},
		Pages: []wmse.Page{
			{URL: "https://example.com/", Depth: 0, ContentType: "text/html"},
			{URL: "https://example.com/q?sort=asc", Depth: 1, ContentType: "text/html", Links: 1},
		},
	}
	s := openSession(t, writeFixture(t, snap))
	var b strings.Builder
	s.out = &b
	s.runLines(newPipeSource(strings.NewReader("cd /q\nls\n")))
	if s.dir {
		t.Errorf("a page fetched with a query should still be a page when named by its path")
	}
	// The position is the URL the file recorded, not the one that was typed: a
	// position that was only a guess would make every later lookup of it a lookup
	// of something the file never held.
	if s.pos != "https://example.com/q?sort=asc" {
		t.Errorf("stood at %q, want the URL the file recorded", s.pos)
	}
	if !strings.Contains(b.String(), "on this page") {
		t.Errorf("ls at the bare path did not list the page:\n%s", b.String())
	}
	if strings.Contains(b.String(), "did not fetch") {
		t.Errorf("the page was fetched, so ls must not say it was not:\n%s", b.String())
	}
}

// TestColumnPadsByVisibleWidth: the padding a column adds must be padding on the
// screen, not bytes in the string. A width verb counts an escape sequence's bytes
// as columns, so padding a styled value pads nothing visible and the column after
// it drifts left by a different amount on every row.
// TestRowsLineUpWhateverTheRowHolds: every row of a table has to put the thing
// after the columns in the same place, or the table is a list rather than a
// table. Three things used to break it - a styled value counted as its escape
// bytes, a domain wider than its column, and a category wider than its own - and
// each is a case here.
func TestRowsLineUpWhateverTheRowHolds(t *testing.T) {
	s := &session{forceColour: true}
	rows := []string{}
	urls := []string{}
	for _, l := range []*linker.Link{
		{Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, HREF: "/a", Resolved: "https://example.com/a", Domain: "example.com", Depth: 1, Tag: "a"},
		{Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeAbsolute, HREF: "https://long.example.net/x.js", Resolved: "https://long.example.net/x.js", Domain: "a-very-long-subdomain.example.net", Tag: "static"},
		// A tag longer than its column, which pushes the rest of the row sideways.
		{Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeRelative, HREF: "/b", Resolved: "https://example.com/b", Domain: "e.com", Tag: "a-rather-long-tag-name"},
		{Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeRelative, HREF: "/c", Resolved: "https://example.com/c", Domain: "example.com", Class: linker.ClassCDN, Depth: 3, HasParams: true},
	} {
		row := s.rowFor(l)
		rows = append(rows, row)
		urls = append(urls, visibleOnly(row))
	}
	// Without this the test would pass on the unstyled rows and prove nothing
	// about the path a person at a terminal actually sees.
	for i, row := range rows {
		if !strings.Contains(row, "\033[") {
			t.Fatalf("row %d was not styled, so this is not testing the coloured path: %q", i, row)
		}
	}
	// The URL is the last fixed-width thing on a row, so it is where the columns
	// have to end. Rows carrying a class and a depth append after it, so what is
	// compared is where each URL begins.
	cols := map[int]int{}
	for _, u := range urls {
		k := strings.Index(u, "https://")
		if k < 0 {
			k = strings.Index(u, "/")
		}
		cols[k]++
	}
	if len(cols) != 1 {
		t.Errorf("the URLs of a table start at %d different columns: %q", len(cols), urls)
	}
}

// visibleOnly strips the escape sequences, so a test can count columns.
func visibleOnly(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == 0x1b:
			esc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestColumnPadsByVisibleWidth(t *testing.T) {
	styled := func(v string) string { return "\033[2m" + v + "\033[0m" }
	s := &session{}
	const width = 24
	for _, in := range []string{"", "ab", "3d.svoydom.kz", "svoydom.kz", "web.telegram.org", "a-very-long-domain-name-indeed.example.com"} {
		want := max(width, len(in))
		if got := visibleWidth(s.column(styled, in, width)); got != want {
			t.Errorf("column(%q) is %d columns wide, want %d", in, got, want)
		}
	}
	// Two rows with values of different lengths must end their columns at the same
	// place, which is the property the ragged table broke.
	short := visibleWidth(s.column(styled, "ab", width))
	long := visibleWidth(s.column(styled, "web.telegram.org", width))
	if short != long {
		t.Errorf("two domains of different lengths padded to %d and %d columns", short, long)
	}
	// And the styling is still there, so the column is dim rather than plain.
	if !strings.Contains(s.column(styled, "ab", width), "\033[") {
		t.Errorf("the column lost its styling")
	}
}

// visibleWidth counts the columns a string takes on a screen, which is its runes
// without the escape sequences a terminal does not draw.
func visibleWidth(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == 0x1b:
			esc = true
		default:
			n++
		}
	}
	return n
}

// TestSessionNodeForPrefersThePageItFetched: the same path can carry two links -
// one found on a page and one the writer added to give a relation an endpoint -
// and the one a person means is the document that was read, not the stub. The
// file deduplicates links by URL, so the tie is built here rather than written
// and read back: what is under test is the choice between two nodes, not the
// writer's decision to keep one of them.
func TestSessionNodeForPrefersThePageItFetched(t *testing.T) {
	s := &session{
		target: "https://example.com/",
		merged: &wmse.Snapshot{Links: []linker.Link{
			// The API link comes first, so that array order cannot be what
			// decides the answer.
			{HREF: "/thing", Resolved: "https://example.com/thing", Domain: "example.com",
				Category: linker.CategoryAPI, LinkType: linker.LinkTypeRelative,
				Depth: 1, SourceURL: "https://example.com/"},
			{HREF: "/thing", Resolved: "https://example.com/thing", Domain: "example.com",
				Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative,
				Depth: 1, SourceURL: "https://example.com/"},
		}},
	}
	l, ok := s.nodeFor("https://example.com/thing")
	if !ok {
		t.Fatalf("the path is in the file")
	}
	if l.Category != linker.CategoryWebPage {
		t.Errorf("nodeFor chose the %s link, want the page", l.Category)
	}
}

// TestFindKindsAreTheOnesTheReportUses: a search means here what it means in the
// report, so the kinds are the tool's own vocabulary, and a name that is not one
// of them matches nothing rather than everything.
func TestFindKindsAreTheOnesTheReportUses(t *testing.T) {
	js := &linker.Link{Category: linker.CategoryWebAsset, HREF: "/a/app.js"}
	css := &linker.Link{Category: linker.CategoryWebAsset, HREF: "/a/app.css"}
	img := &linker.Link{Category: linker.CategoryWebAsset, HREF: "/a/logo.png"}
	page := &linker.Link{Category: linker.CategoryWebPage, HREF: "/a"}
	api := &linker.Link{Category: linker.CategoryAPI, HREF: "/a/x"}
	for _, c := range []struct {
		kind string
		l    *linker.Link
		want bool
	}{
		{"js", js, true}, {"js", css, false}, {"js", img, false}, {"js", page, false},
		{"css", css, true}, {"image", img, true}, {"img", img, true}, {"image", js, false},
		{"asset", js, true}, {"asset", page, false},
		{"html", page, true}, {"page", page, true}, {"html", api, false},
		{"api", api, true}, {"api", page, false},
		{"any", page, true}, {"any", api, true}, {"", api, true},
		{"nonsense", page, false}, {"nonsense", api, false},
	} {
		if got := kindMatches(c.kind, c.l); got != c.want {
			t.Errorf("kindMatches(%q, %s) = %v, want %v", c.kind, c.l.HREF, got, c.want)
		}
	}
}

// completeFile is the one completion that looks outside the file, because the
// argument is a file on disk. It offers the names in a directory and marks the
// directories, so a completed word says what it is.
func TestCompleteFileOffersTheNamesOnDisk(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.wmse", "two.wmse", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := completeFile(dir + "/")
	want := []string{dir + "/notes.txt", dir + "/one.wmse", dir + "/sub/", dir + "/two.wmse"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("completeFile offered %q, want %q", got, want)
	}
	// A prefix narrows it, and a name that is not there offers nothing rather
	// than falling back to the whole directory.
	if got := completeFile(dir + "/tw"); len(got) != 1 || got[0] != dir+"/two.wmse" {
		t.Errorf("completeFile on a prefix gave %q", got)
	}
	if got := completeFile(dir + "/zzz"); got != nil {
		t.Errorf("completeFile on a name that is not there gave %q", got)
	}
	if got := completeFile(dir + "/missing/"); got != nil {
		t.Errorf("completeFile in a directory that is not there gave %q", got)
	}
}

// TestSessionHistoryIsEmptyAtFirst: a session that has run nothing says so,
// rather than printing an empty list that looks like a failure. The command that
// asks is itself in the history by the time it answers, which is what a shell
// does and what makes the list a record rather than a transcript.
func TestSessionHistoryIsEmptyAtFirst(t *testing.T) {
	s := openSession(t, navFixture(t))
	var b strings.Builder
	s.out = &b
	s.cmdHistory(nil)
	if !strings.Contains(b.String(), "nothing typed yet") {
		t.Errorf("history on a session that has run nothing:\n%s", b.String())
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestAPageOnOneHostIsNotAPageOnAnother: a document is identified by its host and
// its path, and the path on its own is not enough, because every site on the
// internet has a document at "/".
//
// An index keyed on the path alone would decide that any host in the file is
// standing on the target's own home page, and a session pointed at one site would
// then answer questions about another - which is the failure that looks like
// nothing at all, because every answer it gives is about a real URL.
func TestAPageOnOneHostIsNotAPageOnAnother(t *testing.T) {
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{wmse.MetaTarget: "https://example.com/"},
		Links: []linker.Link{
			{HREF: "/", Resolved: "https://example.com/", Domain: "example.com", Category: linker.CategoryWebPage, LinkType: linker.LinkTypeAbsolute},
			{HREF: "/", Resolved: "https://other.test/", Domain: "other.test", Category: linker.CategoryWebPage, LinkType: linker.LinkTypeAbsolute},
		},
		Pages: []wmse.Page{
			{URL: "https://example.com/", Depth: 0, ContentType: "text/html"},
			{URL: "https://other.test/", Depth: 1, ContentType: "text/html"},
		},
	}
	s := openSession(t, writeFixture(t, snap))

	if got := s.pageAt("https://example.com/"); got != "https://example.com/" {
		t.Errorf("the target's own page was not found: %q", got)
	}
	if got := s.pageAt("https://other.test/"); got != "https://other.test/" {
		t.Errorf("the other host's page was not found: %q", got)
	}
	// A host the file never saw is not a page, even though its path is one it has.
	if got := s.pageAt("https://unseen.test/"); got != "" {
		t.Errorf("a host the file never saw was reported as a page: %q", got)
	}
	// And standing on another host's page puts the session there, rather than on
	// the target's home page.
	p := s.placeAt("https://other.test/")
	if p.url != "https://other.test/" || p.dir {
		t.Errorf("another host's page placed the session at %q (dir=%v)", p.url, p.dir)
	}
}
