package jsanalyzer

import (
	"strings"
	"testing"

	"apimap/internal/contract"
	"apimap/internal/linker"
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

// A builder filled inside a method and consumed by a request elsewhere is the
// common shape: the fields belong to that request even though no single
// expression names both. When the code keeps the address in a property, the
// carrier is the truthful answer - the request is real, its literal URL is
// assigned somewhere the analyzer cannot see.
func TestScopeBuilderAttribution(t *testing.T) {
	js := `
var app = {
  ajaxUrl: window.AJAX_URL,
  buildParams: function(page) {
    var p = new URLSearchParams();
    p.append('action', 'getApartments');
    p.append('page', page);
    p.append('iblock_ids[]', 7);
    return p;
  },
  load: function() {
    var params = this.buildParams(1);
    fetch(this.ajaxUrl + '?' + params.toString(), {headers: {'X-Requested-With': 'XMLHttpRequest'}});
  }
};
`
	_, _, params := ParseWithParams(js, "https://h/page", true)
	want := map[string]bool{"action": false, "page": false, "iblock_ids[]": false}
	for _, p := range params {
		if _, tracked := want[p.Name]; !tracked {
			continue
		}
		if p.Owner != linker.OwnerRequest {
			t.Errorf("param %q owner = %s, want request", p.Name, p.OwnerLabel())
			continue
		}
		if p.Carrier != "this.ajaxUrl" {
			t.Errorf("param %q carrier = %q, want this.ajaxUrl", p.Name, p.Carrier)
			continue
		}
		want[p.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("param %q missing or unattributed: %+v", name, params)
		}
	}
}

// The same shape with a literal address must reach the contract instead.
func TestScopeBuilderBindsToEndpoint(t *testing.T) {
	js := `
var app = {
  ajaxUrl: '/local/ajax/search.php',
  buildParams: function(page) {
    var p = new URLSearchParams();
    p.append('action', 'getApartments');
    p.append('page', page);
    return p;
  },
  load: function() {
    var params = this.buildParams(1);
    fetch(this.ajaxUrl + '?' + params.toString());
  }
};
`
	_, obs, params := ParseWithParams(js, "https://h/page", true)
	byName := map[string]string{}
	for _, p := range params {
		byName[p.Name] = p.OwnerLabel()
	}
	if byName["action"] != "request" || byName["page"] != "request" {
		t.Fatalf("expected both names attributed to the request, got %v (%+v)", byName, params)
	}
	eps := contract.Infer(obs)
	if len(eps) != 1 {
		t.Fatalf("expected one endpoint, got %d: %+v", len(eps), eps)
	}
	if !strings.HasSuffix(eps[0].Path, "/local/ajax/search.php") {
		t.Fatalf("unexpected endpoint %q", eps[0].Path)
	}
	query := map[string]bool{}
	for _, q := range eps[0].Query {
		query[q.Name] = q.Inferred
	}
	for _, name := range []string{"action", "page"} {
		if !query[name] {
			t.Errorf("param %q missing from contract query: %+v", name, eps[0].Query)
		}
	}
}

// Class bodies used to be skipped wholesale, which meant every endpoint,
// builder and response schema inside a class was invisible. Components and
// controllers are written this way, so it is not an edge case.
func TestClassBodyAnalysis(t *testing.T) {
	js := `
class SearchWidget {
  constructor() {
    this.ajaxUrl = '/local/ajax/search.php';
  }
  buildParams(page) {
    var p = new URLSearchParams();
    p.append('action', 'getApartments');
    p.append('page', page);
    return p;
  }
  load() {
    var params = this.buildParams(1);
    return fetch(this.ajaxUrl + '?' + params.toString(), {
      method: 'POST',
      headers: {'X-Requested-With': 'XMLHttpRequest'}
    });
  }
}
new SearchWidget().load();
`
	_, obs, params := ParseWithParams(js, "https://h/page", true)
	eps := contract.Infer(obs)
	if len(eps) != 1 {
		t.Fatalf("expected one endpoint from the class body, got %d: %+v", len(eps), eps)
	}
	if !strings.HasSuffix(eps[0].Path, "/local/ajax/search.php") {
		t.Fatalf("unexpected endpoint %q", eps[0].Path)
	}
	query := map[string]bool{}
	for _, q := range eps[0].Query {
		query[q.Name] = q.Inferred
	}
	for _, name := range []string{"action", "page"} {
		if !query[name] {
			t.Errorf("param %q missing from the class-built contract: %+v", name, eps[0].Query)
		}
	}
	for _, p := range params {
		if p.Name == "action" && p.Owner != linker.OwnerRequest {
			t.Errorf("param %q owner = %s, want request", p.Name, p.OwnerLabel())
		}
	}
}

// A class that cannot be fully understood must still yield what it can: a
// parse failure anywhere may not cost the endpoints already read.
func TestClassBodyPartialParse(t *testing.T) {
	js := `
class Broken {
  static counter = 0;
  #secret = 1;
  static { this.counter = 1; }
  get value() { return this.#secret; }
  load() {
    fetch('/api/from-broken-class', {method: 'POST'});
    this.somethingWeird(]);
  }
}
new Broken().load();
`
	_, obs, _ := ParseWithParams(js, "https://h/page", true)
	found := false
	for _, o := range obs {
		if strings.HasSuffix(o.URL, "/api/from-broken-class") {
			found = true
		}
	}
	if !found {
		t.Errorf("endpoint from the class body was lost: %+v", obs)
	}
}

// Frameworks nest their methods ("methods: { load() {...} }"), so the builder
// and the request that consumes it routinely sit two object levels apart.
func TestNestedMethodAttribution(t *testing.T) {
	js := `
var app = {
  el: '#app',
  data: { page: 1 },
  methods: {
    buildParams: function(page) {
      var p = new URLSearchParams();
      p.append('action', 'getApartments');
      p.append('page', page);
      return p;
    },
    load: function() {
      var params = this.buildParams(this.page);
      return fetch(this.ajaxUrl + '?' + params.toString());
    }
  }
};
app.ajaxUrl = '/local/ajax/search.php';
app.methods.load();
`
	_, obs, params := ParseWithParams(js, "https://h/page", true)
	byName := map[string]string{}
	for _, p := range params {
		byName[p.Name] = p.OwnerLabel()
	}
	for _, name := range []string{"action", "page"} {
		if byName[name] != "request" {
			t.Errorf("param %q owner = %q, want request (all: %v)", name, byName[name], byName)
		}
	}
	eps := contract.Infer(obs)
	if len(eps) != 1 {
		t.Fatalf("expected one endpoint, got %d: %+v", len(eps), eps)
	}
	query := map[string]bool{}
	for _, q := range eps[0].Query {
		query[q.Name] = q.Inferred
	}
	for _, name := range []string{"action", "page"} {
		if !query[name] {
			t.Errorf("param %q missing from contract: %+v", name, eps[0].Query)
		}
	}
}
