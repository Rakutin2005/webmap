package main

import "testing"

func TestHostInFollowList(t *testing.T) {
	follow := []string{"crmsvoydom.kz", "api.example.com"}
	cases := []struct {
		host string
		want bool
	}{
		{"crmsvoydom.kz", true},
		{"www.crmsvoydom.kz", true},  // subdomain
		{"crmsvoydom.kz:8443", true}, // with port
		{"api.example.com", true},
		{"deep.api.example.com", true},
		{"example.com", false},      // parent of a listed subdomain is not implied
		{"notcrmsvoydom.kz", false}, // must be a dot-boundary suffix, not substring
		{"svoydom.kz", false},
		{"other.com", false},
	}
	for _, c := range cases {
		if got := hostInFollowList(c.host, follow); got != c.want {
			t.Errorf("hostInFollowList(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if hostInFollowList("anything", nil) {
		t.Errorf("empty follow list must match nothing")
	}
}
