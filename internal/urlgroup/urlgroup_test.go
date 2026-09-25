package urlgroup

import (
	"strings"
	"testing"
)

func TestBuildURLsNumericGrouping(t *testing.T) {
	raw := []string{
		"https://hatuli.ai-groundtruth.com/admin/complex/9222/contacts",
		"https://hatuli.ai-groundtruth.com/admin/complex/9223/contacts",
		"https://hatuli.ai-groundtruth.com/admin/complex/9224/contacts",
		"https://hatuli.ai-groundtruth.com/admin/static/page",
	}
	groups := BuildURLs(raw, 2)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %+v", len(groups), groups)
	}
	g := groups[0]
	if g.Pattern != "https://hatuli.ai-groundtruth.com/admin/complex/{id}/contacts" {
		t.Fatalf("unexpected pattern: %s", g.Pattern)
	}
	if g.Count != 3 {
		t.Fatalf("expected 3 members, got %d", g.Count)
	}
	if g.GroupedBy() != "int" {
		t.Fatalf("expected group by int, got %q", g.GroupedBy())
	}
	if g.Annotation() != "(int: id)" {
		t.Fatalf("unexpected annotation: %q", g.Annotation())
	}
	if got := g.VarDescriptor(0); got != "id(int): 9222..9224" {
		t.Fatalf("unexpected descriptor: %s", got)
	}
}

func TestBuildURLsStringLiteralGrouping(t *testing.T) {
	raw := []string{
		"https://host/user/about",
		"https://host/user/settings",
		"https://host/user/profile",
	}
	groups := BuildURLs(raw, 2)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %+v", len(groups), groups)
	}
	g := groups[0]
	if g.Pattern != "https://host/user/{id}" {
		t.Fatalf("unexpected pattern: %s", g.Pattern)
	}
	if g.GroupedBy() != "string" {
		t.Fatalf("expected group by string, got %q", g.GroupedBy())
	}
	if g.Annotation() != "(string: id)" {
		t.Fatalf("unexpected annotation: %q", g.Annotation())
	}
	if got := g.VarDescriptor(0); got != "id(string): about, profile, settings" {
		t.Fatalf("unexpected descriptor: %s", got)
	}
}

func TestSegmentKinds(t *testing.T) {
	cases := map[string]string{
		"9223":                                 "int",
		"550e8400-e29b-41d4-a716-446655440000": "uuid",
		"0123456789abcdef1234567890abcdef":     "hash",
		"dGhpcyBpcyBhIGJhc2U2NHBheWxvYWR6IL==": "base64",
		"U3lzdGVtL0ludGVyZmFjZQ==":             "base64",
		"my-awesome-post":                      "string",
		"about":                                "string",
		"{id}":                                 "",
		"":                                     "",
	}
	for seg, want := range cases {
		if got := segmentKind(seg); got != want {
			t.Fatalf("segmentKind(%q) = %q, want %q", seg, got, want)
		}
	}
}

func TestBuildURLsSlugAndTokenKinds(t *testing.T) {
	slug := BuildURLs([]string{
		"https://h/blog/my-awesome-post",
		"https://h/blog/other-topic-here",
	}, 2)[0]
	if slug.Pattern != "https://h/blog/{id}" || slug.GroupedBy() != "string" {
		t.Fatalf("slug group mismatch: %s / %q", slug.Pattern, slug.GroupedBy())
	}

	uuid := BuildURLs([]string{
		"https://h/asset/550e8400-e29b-41d4-a716-446655440000",
		"https://h/asset/6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	}, 2)[0]
	if uuid.Pattern != "https://h/asset/{id}" || uuid.GroupedBy() != "uuid" {
		t.Fatalf("uuid group mismatch: %s / %q", uuid.Pattern, uuid.GroupedBy())
	}
	if uuid.Annotation() != "(uuid: id)" {
		t.Fatalf("uuid annotation mismatch: %q", uuid.Annotation())
	}

	hash := BuildURLs([]string{
		"https://h/f/0123456789abcdef1234567890abcdef",
		"https://h/f/abcdef0123456789abcdef0123456789",
	}, 2)[0]
	if hash.Pattern != "https://h/f/{id}" || hash.GroupedBy() != "hash" {
		t.Fatalf("hash group mismatch: %s / %q", hash.Pattern, hash.GroupedBy())
	}

	b64 := BuildURLs([]string{
		"https://h/t/dGhpcyBpcyBhIGJhc2U2NHBheWxvYWR6IL==",
		"https://h/t/U3lzdGVtL0ludGVyZmFjZQ==",
	}, 2)[0]
	if b64.Pattern != "https://h/t/{id}" || b64.GroupedBy() != "base64" {
		t.Fatalf("base64 group mismatch: %s / %q", b64.Pattern, b64.GroupedBy())
	}
	if b64.Annotation() != "(base64: id)" {
		t.Fatalf("base64 annotation mismatch: %q", b64.Annotation())
	}
}

func TestBuildURLsMixedKindFallsBackToString(t *testing.T) {
	groups := BuildURLs([]string{
		"https://h/complex/9223/contacts",
		"https://h/complex/about/contacts",
	}, 2)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if got := groups[0].GroupedBy(); got != "string" {
		t.Fatalf("expected mixed to be string, got %q", got)
	}
}

func TestBuildURLsMultiVar(t *testing.T) {
	groups := BuildURLs([]string{
		"https://h/report/2024/q1",
		"https://h/report/2025/q1",
		"https://h/report/2024/q2",
		"https://h/report/2025/q2",
	}, 2)
	g := findByPattern(t, groups, "https://h/report/{id}/{id2}")
	if g.Count != 4 {
		t.Fatalf("expected 4 members, got %d", g.Count)
	}
	if g.GroupedBy() != "int,string" {
		t.Fatalf("expected int,string, got %q", g.GroupedBy())
	}
	if g.Annotation() != "(int: id, string: id2)" {
		t.Fatalf("unexpected annotation: %q", g.Annotation())
	}
	if len(g.Vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(g.Vars))
	}
	if got := g.VarDescriptor(0); got != "id(int): 2024..2025" {
		t.Fatalf("unexpected first var: %q", got)
	}
	if got := g.VarDescriptor(1); got != "id2(string): q1, q2" {
		t.Fatalf("unexpected second var: %q", got)
	}
}

func findByPattern(t *testing.T, groups []Group, pattern string) Group {
	t.Helper()
	for _, g := range groups {
		if g.Pattern == pattern {
			return g
		}
	}
	t.Fatalf("group with pattern %s not found in: %+v", pattern, groups)
	return Group{}
}

func TestMatchURL(t *testing.T) {
	groups := BuildURLs([]string{
		"http://h/admin/complex/9223/contacts",
		"http://h/admin/complex/9224/contacts",
		"http://h/admin/complex/9223/access",
		"http://h/admin/complex/9224/access",
	}, 2)
	cases := []struct{ raw, want string }{
		{"http://h/admin/complex/9223/contacts", "http://h/admin/complex/{id}/contacts"},
		{"http://h/admin/complex/9224/contacts", "http://h/admin/complex/{id}/contacts"},
		{"http://h/admin/complex/9223/access", "http://h/admin/complex/{id}/access"},
		{"http://h/admin/complex/9224/access", "http://h/admin/complex/{id}/access"},
		{"http://h/admin/static/x.js", ""},
		{"not a url", ""},
		{"http://h/admin/complex/abc/contacts", "http://h/admin/complex/{id}/contacts"},
	}
	for _, tc := range cases {
		if got := MatchURL(tc.raw, groups); got != tc.want {
			t.Errorf("MatchURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestClusterRanges(t *testing.T) {
	// The user-visible form: several patterns sharing the annotation (int: id)
	// on different literal skeletons fold into one line with a merged range.
	groups := BuildURLs([]string{
		"https://h/example/edit/40/password",
		"https://h/example/edit/41/password",
		"https://h/complex/42/access",
		"https://h/complex/43/access",
	}, 2)
	if len(groups) != 2 {
		t.Fatalf("expected 2 patterns, got %d: %+v", len(groups), groups)
	}
	got := ClusterRanges(groups)
	if len(got) != 1 || got[0] != "id: 40..43" {
		t.Fatalf("unexpected cluster ranges: %v", got)
	}
}

func TestMatches(t *testing.T) {
	groups := BuildURLs([]string{
		"https://h/complex/9223/contacts",
		"https://h/complex/9224/contacts",
	}, 2)
	if !groups[0].Matches("https://h/complex/42/contacts") {
		t.Fatal("expected numeric var position to match")
	}
	if !groups[0].Matches("https://h/complex/abc/contacts") {
		t.Fatal("expected string literal var position to match")
	}
	if groups[0].Matches("https://h/other/9223/contacts") {
		t.Fatal("literal host mismatch should not match")
	}
	if groups[0].Matches("https://h/complex/9223/other") {
		t.Fatal("literal segment mismatch should not match")
	}
	if groups[0].Matches("https://h/complex/9223/contacts/extra") {
		t.Fatal("length mismatch should not match")
	}
}

func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"https://h/a/b?x=1#f": "https://h/a/b",
		"https://h/a/b/":      "https://h/a/b",
		"http://h/c":          "http://h/c",
		"  https://h/a?y=2  ": "https://h/a",
	}
	for in, want := range cases {
		got, ok := Canonical(in)
		if !ok || got != want {
			t.Fatalf("Canonical(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := Canonical("not a url"); ok {
		t.Fatal("expected non-url to be rejected")
	}
	if _, ok := Canonical(""); ok {
		t.Fatal("expected empty to be rejected")
	}
}

func TestDetectorFeed(t *testing.T) {
	d := NewDetector(2)

	// First feed: two matching candidates confirm the pattern.
	blocked := d.Feed([]string{
		"https://h/complex/9223/contacts",
		"https://h/complex/9224/contacts",
	})
	if len(blocked) != 2 {
		t.Fatalf("expected both to be blocked (they form the pattern), got %v", blocked)
	}

	// Second feed: a concrete URL matching the confirmed pattern is blocked.
	blocked2 := d.Feed([]string{"https://h/complex/12345/contacts", "https://h/unrelated/path"})
	if len(blocked2) != 1 || blocked2[0] != "https://h/complex/12345/contacts" {
		t.Fatalf("unexpected second-feed blocks: %v", blocked2)
	}

	groups := d.Groups()
	if len(groups) != 1 || groups[0].Pattern != "https://h/complex/{id}/contacts" {
		t.Fatalf("unexpected detector groups: %+v", groups)
	}
}

func TestVarRangeNonNumeric(t *testing.T) {
	v := Var{Kind: "string", Values: []string{"about", "settings", "profile", "blog", "help", "faq", "api"}}
	got := v.Range()
	if strings.Count(got, ",") < 4 {
		t.Fatalf("expected truncated distinct list, got %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
}
