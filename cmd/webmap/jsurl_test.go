package main

import "testing"

func TestIsJSURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://h/app.js", true},
		{"https://h/app.min.js", true},
		{"https://h/app.mjs", true},
		{"https://h/app.tsx", true},
		{"https://h/APP.JS", true},
		// Cache-busting and versioned references must still count as scripts:
		// the query does not change the asset.
		{"https://h/app.js?v=1789992068", true},
		{"https://h/app.js?1789992068", true},
		{"https://h/app.js?a=1&b=2#frag", true},
		{"https://h/app.js#frag", true},
		{"https://h/app.js/", false},
		{"https://h/app.css?v=1", false},
		{"https://h/app", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isJSURL(tc.url); got != tc.want {
			t.Errorf("isJSURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
