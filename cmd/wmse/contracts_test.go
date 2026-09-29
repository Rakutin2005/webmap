package main

import (
	"strings"
	"testing"

	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// A file holds the same URL in two spellings, and a person only ever sees the
// readable one: a link keeps the percent-encoding the fetch resolved it to, so a
// template route is stored as /cabinet/documents/%7Bt%7D/sign/, while a
// contract keeps /cabinet/documents/{t}/sign/ because that is what the report
// prints. Asking about the second used to be answered "not in this file".
func TestAPIFindsAContractByThePathTheReportPrinted(t *testing.T) {
	const root = "https://example.com"
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{
			wmse.MetaTool: "webmap", wmse.MetaVersion: "1.3.3", wmse.MetaFormat: "v1",
			wmse.MetaTarget: root + "/", wmse.MetaCreated: "2026-09-29T10:11:12Z",
		},
		Links: []linker.Link{{
			HREF: "/cabinet/documents/%7Bt%7D/sign/", Resolved: root + "/cabinet/documents/%7Bt%7D/sign/",
			Domain: "example.com", Category: linker.CategoryDynamic, LinkType: linker.LinkTypeRelative,
			Depth: 2, SourceURL: root + "/assets/app.js", Tag: "a",
		}},
		Endpoints: []contract.Endpoint{{
			Path: root + "/cabinet/documents/{t}/sign/", Methods: []string{"POST"}, Calls: 1,
		}},
		Observations: []contract.Observation{{
			URL: root + "/cabinet/documents/{t}/sign/", Method: "POST",
		}},
	}
	// Both spellings, and the trailing slash the link carries but the report
	// leaves off, have to reach the same contract.
	for _, ask := range []string{
		"/cabinet/documents/{t}/sign",
		"/cabinet/documents/{t}/sign/",
		"/cabinet/documents/%7Bt%7D/sign/",
	} {
		s := openSession(t, writeContractFixture(t, snap))
		var b strings.Builder
		s.out = &b
		s.runLines(newPipeSource(strings.NewReader("api " + ask + "\n")))
		if !strings.Contains(b.String(), "methods: POST") {
			t.Errorf("api %s did not reach the contract:\n%s", ask, b.String())
		}
		if !strings.Contains(b.String(), "1 captured request") {
			t.Errorf("api %s did not reach the captured request:\n%s", ask, b.String())
		}
	}
}

// The contract is the finding a person is looking for when they search for
// APIs, and a URL recovered from a bundle is a contract without any link at all.
// Searching only the links meant the one thing the file knew best could not be
// asked for by name.
func TestFindAPISearchesContracts(t *testing.T) {
	const root = "https://example.com"
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{
			wmse.MetaTool: "webmap", wmse.MetaVersion: "1.3.3", wmse.MetaFormat: "v1",
			wmse.MetaTarget: root + "/", wmse.MetaCreated: "2026-09-29T10:11:12Z",
		},
		Links: []linker.Link{
			{HREF: "/assets/app.js", Resolved: root + "/assets/app.js", Domain: "example.com",
				Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeRelative, Depth: 1, Tag: "script"},
			{HREF: "/api/known", Resolved: root + "/api/known", Domain: "example.com",
				Category: linker.CategoryAPI, LinkType: linker.LinkTypeRelative, Depth: 1, Tag: "a"},
		},
		Endpoints: []contract.Endpoint{
			{Path: root + "/api/known", Methods: []string{"GET"}, Calls: 1},
			// Recovered from a bundle: no link was ever recorded for it.
			{Path: root + "/cabinet/documents/{t}/sign", Methods: []string{"POST"}, Calls: 1},
			{Path: root + "/page", Methods: []string{"DELETE"}, Calls: 1},
		},
	}
	s := openSession(t, writeContractFixture(t, snap))
	var b strings.Builder
	s.out = &b
	s.runLines(newPipeSource(strings.NewReader("find api *\n")))
	out := b.String()
	for _, want := range []string{"/api/known", "/cabinet/documents/{t}/sign", "/page"} {
		if !strings.Contains(out, want) {
			t.Errorf("find api * did not list the contract %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/assets/app.js") {
		t.Errorf("find api * listed a JS asset:\n%s", out)
	}
	// The same path is not printed twice when a link and a contract share it.
	if n := strings.Count(out, "/api/known"); n != 1 {
		t.Errorf("/api/known is listed %d times:\n%s", n, out)
	}
}

// `api` with nothing after it is a question about every contract, which is what
// an empty argument reads as.
func TestAPIWithoutAPathListsEveryContract(t *testing.T) {
	const root = "https://example.com"
	snap := &wmse.Snapshot{
		Meta: wmse.Meta{
			wmse.MetaTool: "webmap", wmse.MetaVersion: "1.3.3", wmse.MetaFormat: "v1",
			wmse.MetaTarget: root + "/", wmse.MetaCreated: "2026-09-29T10:11:12Z",
		},
		Endpoints: []contract.Endpoint{
			{Path: root + "/api/egovQr", Unobserved: true},
			{Path: root + "/cabinet", Methods: []string{"GET"}, MethodInferred: true},
		},
	}
	s := openSession(t, writeContractFixture(t, snap))
	var b strings.Builder
	s.out = &b
	s.runLines(newPipeSource(strings.NewReader("api\n")))
	out := b.String()
	if !strings.Contains(out, "2 contracts") {
		t.Errorf("the count is missing:\n%s", out)
	}
	if !strings.Contains(out, "/api/egovQr") || !strings.Contains(out, "path only") {
		t.Errorf("an endpoint with no captured request is not marked:\n%s", out)
	}
	if !strings.Contains(out, "GET (inferred)") {
		t.Errorf("an inferred method is not marked:\n%s", out)
	}
}

func writeContractFixture(t *testing.T, snap *wmse.Snapshot) string {
	t.Helper()
	return writeFixture(t, snap)
}

// DYNAMIC is not a kind of thing to be. It says the URL was assembled in code
// rather than read off a page, which is how the scan met it; what the resource
// is follows from the resource. A route with a contract is an API, and a
// script is a script however it was found.
func TestWhatReportsTheKindOfResource(t *testing.T) {
	const root = "https://example.com"
	fixture := &wmse.Snapshot{
		Meta: wmse.Meta{
			wmse.MetaTool: "webmap", wmse.MetaVersion: "1.3.3", wmse.MetaFormat: "v1",
			wmse.MetaTarget: root + "/", wmse.MetaCreated: "2026-09-29T10:11:12Z",
		},
		Links: []linker.Link{
			// A route assembled in code, with a contract: an API.
			{HREF: "/documents/%7Bt%7D/cancel/", Resolved: root + "/documents/%7Bt%7D/cancel/",
				Domain: "example.com", Category: linker.CategoryDynamic, LinkType: linker.LinkTypeRelative,
				Depth: 2, SourceURL: root + "/assets/api.js", Tag: "a"},
			// A route assembled in code, with nothing else said about it.
			{HREF: "/cabinet/%7Bo%7D", Resolved: root + "/cabinet/%7Bo%7D",
				Domain: "example.com", Category: linker.CategoryDynamic, LinkType: linker.LinkTypeRelative,
				Depth: 2, SourceURL: root + "/assets/app.js", Tag: "a"},
			{HREF: "/assets/index.js", Resolved: root + "/assets/index.js", Domain: "example.com",
				Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeRelative, Depth: 1, Tag: "script"},
			{HREF: "/about", Resolved: root + "/about", Domain: "example.com",
				Category: linker.CategoryWebPage, LinkType: linker.LinkTypeRelative, Depth: 1, Tag: "a"},
		},
		Endpoints: []contract.Endpoint{{
			Path: root + "/documents/{t}/cancel/", Methods: []string{"POST"}, Calls: 1,
			Bodies: []contract.BodyFormat{{Kind: "json", Fields: []contract.Field{{Name: "reason"}}}},
		}},
	}
	for _, c := range []struct{ ask, kind, extra, absent string }{
		{"/documents/{t}/cancel", "kind      API", "found as    DYNAMIC (a URL assembled in code, not read off a page)", ""},
		{"/cabinet/{o}", "kind      unknown", "found as    DYNAMIC (a URL assembled in code, not read off a page)", ""},
		{"/assets/index.js", "kind      asset: js", "", "found as"},
		{"/about", "kind      web-page", "", "found as"},
	} {
		s := openSession(t, writeContractFixture(t, fixture))
		var b strings.Builder
		s.out = &b
		s.runLines(newPipeSource(strings.NewReader("what " + c.ask + "\n")))
		out := b.String()
		if !strings.Contains(out, c.kind) {
			t.Errorf("what %s did not report %q:\n%s", c.ask, c.kind, out)
		}
		if c.extra != "" && !strings.Contains(out, c.extra) {
			t.Errorf("what %s did not say how it was found (%q):\n%s", c.ask, c.extra, out)
		}
		if c.absent != "" && strings.Contains(out, c.absent) {
			t.Errorf("what %s said %q where there was nothing to say:\n%s", c.ask, c.absent, out)
		}
	}
	// The contract is printed where the question was asked, not left behind
	// another command.
	s := openSession(t, writeContractFixture(t, fixture))
	var b strings.Builder
	s.out = &b
	s.runLines(newPipeSource(strings.NewReader("what /documents/{t}/cancel\n")))
	out := b.String()
	if !strings.Contains(out, "methods: POST") || !strings.Contains(out, "reason") {
		t.Errorf("what did not print the contract:\n%s", out)
	}
}
