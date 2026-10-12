package linker

import "testing"

func TestParseTextURLs(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "absolute",
			text: "see https://api.example.com/v1/users for details",
			want: []string{"https://api.example.com/v1/users"},
		},
		{
			name: "protocol relative",
			text: "load //cdn.example.com/app.js now",
			want: []string{"//cdn.example.com/app.js"},
		},
		{
			name: "www",
			text: "visit www.example.com/page today",
			want: []string{"http://www.example.com/page"},
		},
		{
			name: "sentence punctuation is trimmed",
			text: "Go to https://example.com/a/b.",
			want: []string{"https://example.com/a/b"},
		},
		{
			name: "balanced parentheses survive",
			text: "see https://en.example.org/wiki/Foo_(bar) here",
			want: []string{"https://en.example.org/wiki/Foo_(bar)"},
		},
		{
			name: "unbalanced close is dropped",
			text: "(https://example.com/x)",
			want: []string{"https://example.com/x"},
		},
		{
			name: "comment slashes are not a host",
			text: "// just a comment\nnot a url",
			want: nil,
		},
		{
			name: "query is kept and marked",
			text: "https://example.com/search?q=1&page=2",
			want: []string{"https://example.com/search?q=1&page=2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			links := ParseTextURLs(tc.text, "https://source.example", "text-url")
			if len(links) != len(tc.want) {
				t.Fatalf("got %d links %v, want %d %v", len(links), hrefs(links), len(tc.want), tc.want)
			}
			for i, l := range links {
				if l.HREF != tc.want[i] {
					t.Errorf("link %d = %q, want %q", i, l.HREF, tc.want[i])
				}
				if l.Tag != "text-url" {
					t.Errorf("link %d tag = %q, want text-url", i, l.Tag)
				}
			}
		})
	}
}

func TestParseTextURLsQueryMarked(t *testing.T) {
	links := ParseTextURLs("https://example.com/search?q=1&page=2", "https://s.example", "text-url")
	if len(links) != 1 || !links[0].HasParams {
		t.Fatalf("expected one link with params, got %+v", links)
	}
}

func hrefs(links []Link) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.HREF
	}
	return out
}
