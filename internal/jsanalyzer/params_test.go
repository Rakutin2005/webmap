package jsanalyzer

import (
	"testing"

	"apimap/internal/contract"
)

// Parameter names must be attributable: a name written into a builder that
// feeds a request belongs to that endpoint, while a name written into the
// page's own query string does not. The flat, unattributed list this replaces
// could not answer "which endpoint is this for?".
func TestParamBuilderClassification(t *testing.T) {
	js := `
const qs = new URLSearchParams(new URL(location.href).searchParams);
qs.set('geo', 'almaty');
qs.set('lang', 'kk');
const fd = new FormData();
fd.append('avatar', file);
const sp = new URLSearchParams();
sp.set('sort', 'price');
sp.set('limit', '20');
fetch('/api/search', {method: 'POST', body: sp});
`
	_, obs, params := ParseWithParams(js, "https://h/page", true)

	byName := map[string]paramLabels{}
	for _, p := range params {
		byName[p.Name] = paramLabels{kind: p.KindLabel(), owner: p.OwnerLabel()}
	}
	cases := []struct {
		name, kind, owner string
	}{
		{"geo", "query", "page query state"},
		{"lang", "query", "page query state"},
		{"avatar", "form", "unassigned"},
		{"sort", "query", "request"},
		{"limit", "query", "request"},
	}
	for _, tc := range cases {
		got, ok := byName[tc.name]
		if !ok {
			t.Errorf("param %q missing from %v", tc.name, params)
			continue
		}
		if got.kind != tc.kind || got.owner != tc.owner {
			t.Errorf("param %q = %s/%s, want %s/%s", tc.name, got.kind, got.owner, tc.kind, tc.owner)
		}
	}

	// Only the request-side names reach the contract, and they are flagged as
	// inferred with no invented value.
	eps := contract.Infer(obs)
	if len(eps) != 1 {
		t.Fatalf("expected one endpoint, got %d", len(eps))
	}
	query := map[string]bool{}
	for _, q := range eps[0].Query {
		query[q.Name] = q.Inferred
	}
	for _, name := range []string{"sort", "limit"} {
		if !query[name] {
			t.Errorf("request param %q missing from contract: %+v", name, eps[0].Query)
		}
	}
	for _, name := range []string{"geo", "lang", "avatar"} {
		if _, present := query[name]; present {
			t.Errorf("page/form param %q must not be attributed to an API contract", name)
		}
	}
}

type paramLabels struct{ kind, owner string }
