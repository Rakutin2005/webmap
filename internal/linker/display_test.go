package linker

import (
	"strings"
	"testing"
)

// TestFoldParamLinksKeepsOneRowPerEndpoint covers the rule the link table rests
// on: a crawl finds one record per href, so /item?id=1 and /item?id=2 arrive as
// two and must be one row, annotated with the queries it was called with.
func TestFoldParamLinksKeepsOneRowPerEndpoint(t *testing.T) {
	got := FoldParamLinks([]Link{
		{HREF: "/item", Resolved: "https://x.com/item", Domain: "x.com"},
		{HREF: "/item?id=1", Resolved: "https://x.com/item?id=1", Domain: "x.com", HasParams: true},
		{HREF: "/item?id=2", Resolved: "https://x.com/item?id=2", Domain: "x.com", HasParams: true},
	})
	if len(got) != 1 {
		t.Fatalf("rows: got %d, want 1: %+v", len(got), got)
	}
	if got[0].HREF != "/item" {
		t.Errorf("href: got %q, want /item - the row must be the request as it stands", got[0].HREF)
	}
	if p := FormatParamVariants(got[0].ParamVariants); p != "id=1,2" {
		t.Errorf("params: got %q, want id=1,2", p)
	}
}

// TestFoldParamLinksIsOrderIndependent covers a bug this rule had. The bare-URL
// record used to stay out of the group map, so whichever of the two arrived
// first decided the row's shape - and which arrives first is not a fact about
// the site, it is whichever crawl worker finished first.
func TestFoldParamLinksIsOrderIndependent(t *testing.T) {
	records := []Link{
		{HREF: "/search", Resolved: "https://x.com/search", Domain: "x.com"},
		{HREF: "https://x.com/search?q=term", Resolved: "https://x.com/search?q=term"},
	}
	first := FoldParamLinks(records)
	second := FoldParamLinks([]Link{records[1], records[0]})
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("rows: got %d and %d, want 1 each", len(first), len(second))
	}
	if first[0].HREF != second[0].HREF || first[0].Domain != second[0].Domain {
		t.Errorf("the order of the records changed the row: %+v vs %+v", first[0], second[0])
	}
}

// TestFoldParamLinksSeparatesClasses pins the other half: two records of one
// base URL that differ in class are two findings, because a WAF response to
// /item is not the item.
func TestFoldParamLinksSeparatesClasses(t *testing.T) {
	got := FoldParamLinks([]Link{
		{HREF: "/item", Resolved: "https://x.com/item", Domain: "x.com"},
		{HREF: "/item", Resolved: "https://x.com/item", Domain: "x.com", Class: ClassWAF},
	})
	if len(got) != 2 {
		t.Errorf("rows: got %d, want 2 - one per class", len(got))
	}
}

// TestFormatParamVariants covers the annotation's shape, including the two cases
// that are easy to get wrong: a name with no value, and more values than a row
// will hold.
func TestFormatParamVariants(t *testing.T) {
	cases := []struct {
		in   []ParamVariant
		want string
	}{
		{nil, ""},
		{[]ParamVariant{{Query: "?a=1&b=2"}}, "a=1; b=2"},
		{[]ParamVariant{{Query: "?debug="}}, "debug"},
		{[]ParamVariant{{Query: "?a=1"}, {Query: "?a=2"}}, "a=1,2"},
		{[]ParamVariant{{Query: "?a=1&a=2"}}, "a=1,2"},
		{[]ParamVariant{{Query: "?b=1"}, {Query: "?a=2"}}, "a=2; b=1"},
		{[]ParamVariant{{Query: "?a=1&b=2"}, {Query: "?a=1&b=2"}}, "a=1; b=2"},
	}
	for _, c := range cases {
		if got := FormatParamVariants(c.in); got != c.want {
			t.Errorf("FormatParamVariants(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
	// Six values of one name is more than a row shows, and the cut is visible
	// rather than silent: a truncated list that looks complete is worse than a
	// long one.
	many := make([]ParamVariant, 0, 6)
	for i := 1; i <= 6; i++ {
		many = append(many, ParamVariant{Query: "?id=" + string(rune('0'+i))})
	}
	got := FormatParamVariants(many)
	if !strings.HasSuffix(got, "+...") {
		t.Errorf("six values: got %q, want a visible cut", got)
	}
	if strings.Count(got, ",") != 4 {
		t.Errorf("six values: got %q, want five of them", got)
	}
}

// TestDropQuery pins the one half of the split the fold depends on.
func TestDropQuery(t *testing.T) {
	cases := map[string]string{
		"https://x.com/a?b=1": "https://x.com/a",
		"https://x.com/a":     "https://x.com/a",
		"/a?b=1#frag":         "/a",
		"https://x.com/a?":    "https://x.com/a?",
	}
	for in, want := range cases {
		if got := DropQuery(in); got != want {
			t.Errorf("DropQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSortForDisplayIsATotalOrder matters because it is a reading order that
// falls through four fields: if two rows compared equal, their order would come
// from the sort's own instability and the table would differ between runs of the
// same file.
func TestSortForDisplayIsATotalOrder(t *testing.T) {
	links := []Link{
		{HREF: "b", Class: ClassNormal, Category: CategoryWebPage, Domain: "z.com"},
		{HREF: "a", Class: ClassNormal, Category: CategoryWebPage, Domain: "z.com"},
		{HREF: "a", Class: ClassWAF, Category: CategoryWebPage, Domain: "z.com"},
		{HREF: "a", Class: ClassNormal, Category: CategoryAPI, Domain: "z.com"},
		{HREF: "a", Class: ClassNormal, Category: CategoryWebPage, Domain: "a.com"},
	}
	SortForDisplay(links)
	// Class, then category, then domain, then the URL. Class leads because a WAF
	// or CDN URL is a different kind of finding rather than a page that happens
	// to be on another host, and mixing the two into one alphabetic run is what
	// makes a table hard to read.
	want := []string{
		"a.com/a page",
		"z.com/a page",
		"z.com/b page",
		"z.com/a API",
		"z.com/a WAF",
	}
	for i, w := range want {
		if got := links[i].Domain + "/" + links[i].HREF + " " + describe(links[i]); got != w {
			t.Errorf("row %d: got %q, want %q", i, got, w)
		}
	}
}

// describe names a row the way the assertion above reads it, so a failure shows
// which of the four keys decided the order.
func describe(l Link) string {
	switch {
	case l.Class != ClassNormal:
		return l.Class.String()
	case l.Category != CategoryWebPage:
		return l.Category.String()
	default:
		return "page"
	}
}
