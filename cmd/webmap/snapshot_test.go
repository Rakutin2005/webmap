package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"apimap/internal/config"
	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// TestCommandLineHidesCredentials guards the one place a saved file could leak
// something the header policy deliberately drops. The command line is stored so
// the scan can be reproduced, and a reproduction that still needs the token
// pasted into it is one the reader can be told to fetch; a file that quietly
// kept the token would be a file that gets mailed around.
func TestCommandLineHidesCredentials(t *testing.T) {
	restore := os.Args
	defer func() { os.Args = restore }()

	os.Args = []string{
		"webmap", "-url", "https://example.com/",
		"-H", "Authorization: Bearer supersecret",
		"-b", "session=abc123",
		"-j", "-r",
		"-H=X-Api-Key: kkkk",
		"--cookie=other=zzz",
	}
	got := commandLine()
	for _, secret := range []string{"supersecret", "abc123", "kkkk", "zzz"} {
		if strings.Contains(got, secret) {
			t.Errorf("command line kept %q: %s", secret, got)
		}
	}
	// The flags themselves are the reproducible part and must survive.
	for _, want := range []string{"-url", "https://example.com/", "-H", "-b", "-j", "-r"} {
		if !strings.Contains(got, want) {
			t.Errorf("command line lost %q: %s", want, got)
		}
	}
	if n := strings.Count(got, "<hidden>"); n != 4 {
		t.Errorf("hidden values: got %d, want 4: %s", n, got)
	}
}

// TestScanMetaKeepsHeaderNames checks the other half of the same promise: the
// names of the headers a scan sent are stored, because they explain the scan,
// and the values are not.
func TestScanMetaKeepsHeaderNames(t *testing.T) {
	cfg := &config.Config{
		URL: "https://example.com/",
		// A colon-less argument is recorded as "(raw)" rather than verbatim: a
		// curl-style header can be written without one, and a value that has no
		// colon in it is exactly the shape that would leak if it were stored.
		Headers: []string{"Authorization: Bearer supersecret", "X-Api-Key: kkkk", "malformed"},
	}
	meta := scanMeta(cfg, "out.wmse", time.Now())
	names := meta["request_headers"]
	if !strings.Contains(names, "Authorization") || !strings.Contains(names, "X-Api-Key") {
		t.Errorf("header names: got %q", names)
	}
	if !strings.Contains(names, "(raw)") {
		t.Errorf("a colon-less header should be recorded as unnameable: got %q", names)
	}
	for _, secret := range []string{"supersecret", "kkkk", "malformed"} {
		if strings.Contains(names, secret) {
			t.Errorf("header value %q stored in %q", secret, names)
		}
	}
	if meta[wmse.MetaSessionData] != "true" {
		t.Errorf("session_data: got %q, want true when headers were sent", meta[wmse.MetaSessionData])
	}
}

func TestScanMetaSessionDataOnlyWhenCredentialsWereUsed(t *testing.T) {
	plain := &config.Config{URL: "https://example.com/"}
	if got := scanMeta(plain, "out.wmse", time.Now())[wmse.MetaSessionData]; got != "false" {
		t.Errorf("session_data: got %q, want false for a scan that sent no credentials", got)
	}
	cookie := &config.Config{URL: "https://example.com/", Cookie: "a=b"}
	if got := scanMeta(cookie, "out.wmse", time.Now())[wmse.MetaSessionData]; got != "true" {
		t.Errorf("session_data: got %q, want true when a cookie was sent", got)
	}
	emulated := &config.Config{URL: "https://example.com/", Emulate: true}
	if got := scanMeta(emulated, "out.wmse", time.Now())[wmse.MetaSessionData]; got != "true" {
		t.Errorf("session_data: got %q, want true when the sandbox ran the site's own code", got)
	}
}

// TestBuildSnapshotDescribesTheFile is the end-to-end check on the writer's
// input: everything the report knew has to reach the file, including the parts
// the report filtered away, and the counts in the meta have to describe the file
// rather than the scan.
func TestBuildSnapshotDescribesTheFile(t *testing.T) {
	restore := os.Args
	defer func() { os.Args = restore }()
	os.Args = []string{"webmap", "-url", "https://example.com/", "-o", "out.wmse"}

	pageLog = map[string]wmse.Page{
		"https://example.com/":     {URL: "https://example.com/", Depth: 0, ContentType: "text/html", Links: 1},
		"https://example.com/a.js": {URL: "https://example.com/a.js", Depth: 1, ContentType: "application/javascript"},
	}
	defer func() { pageLog = map[string]wmse.Page{} }()

	cfg := &config.Config{URL: "https://example.com/"}
	links := []linker.Link{
		{HREF: "/a.js", Resolved: "https://example.com/a.js", Domain: "example.com",
			Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeRelative, Depth: 1,
			SourceURL: "https://example.com/"},
		{HREF: "https://cdn.other/x.css", Resolved: "https://cdn.other/x.css", Domain: "cdn.other",
			Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeAbsolute, Depth: 1,
			Class: linker.ClassCDN, SourceURL: "https://example.com/"},
	}
	snap := buildSnapshot("out.wmse", cfg, links, time.Now())

	if snap.Meta[wmse.MetaTarget] != "https://example.com/" {
		t.Errorf("target: got %q", snap.Meta[wmse.MetaTarget])
	}
	if got := snap.Meta[wmse.MetaLinks]; got != "3" {
		// Two links plus the entry point, which nothing linked to.
		t.Errorf("links: got %q, want 3 including the entry point", got)
	}
	if got := snap.Meta[wmse.MetaPages]; got != "2" {
		t.Errorf("pages: got %q, want 2", got)
	}
	// A class the run hid is still in the file: that is one of the reasons to
	// save a scan rather than keep the terminal output.
	if snap.Stats.ByClass[linker.ClassCDN] != 1 {
		t.Errorf("hidden class lost: byClass=%v", snap.Stats.ByClass)
	}
	found := false
	for i := range snap.Links {
		if snap.Links[i].Class == linker.ClassCDN {
			found = true
		}
	}
	if !found {
		t.Error("the CDN link did not survive into the snapshot")
	}
	// The entry point must be a node, or the tree would have no root.
	if _, err := os.Stat("out.wmse"); err == nil {
		t.Error("buildSnapshot should not write the file")
	}
}

// TestRecordPageKeepsTheShallowestSighting covers a page the crawler reaches
// twice - once through a link, once because a pattern admitted it. The tree
// should show the shortest path, because that is the one a person can walk.
func TestRecordPageKeepsTheShallowestSighting(t *testing.T) {
	pageLog = map[string]wmse.Page{}
	defer func() { pageLog = map[string]wmse.Page{} }()

	recordPage("https://example.com/", 0, "", 3)
	recordPage("https://example.com/", 2, "text/html", 9)
	recordPage("https://example.com/", 1, "application/xhtml+xml", 1)

	got := pageLog["https://example.com/"]
	if got.Depth != 0 {
		t.Errorf("depth: got %d, want 0", got.Depth)
	}
	// A later sighting that knows more fills the gap; one that knows less does
	// not overwrite. The first thing the crawler was told is usually the best
	// thing it was told, and an empty value is not an improvement on it.
	if got.ContentType != "text/html" {
		t.Errorf("content type: got %q, want the first non-empty sighting", got.ContentType)
	}
	if got.Links != 9 {
		t.Errorf("links: got %d, want the largest count seen", got.Links)
	}
	recordPage("", 0, "text/html", 1)
	if len(pageLog) != 1 {
		t.Errorf("an empty URL was recorded: %v", pageLog)
	}
}
