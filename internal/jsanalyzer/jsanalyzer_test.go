package jsanalyzer

import (
	"strings"
	"testing"

	"apimap/internal/contract"
)

func testLinks(t *testing.T, name, js, sourceURL string, expectCount int) {
	t.Run(name, func(t *testing.T) {
		links, _ := Parse(js, sourceURL, true)
		if len(links) != expectCount {
			t.Errorf("expected %d links, got %d", expectCount, len(links))
			for _, l := range links {
				t.Logf("  HREF=%s Tag=%s", l.HREF, l.Tag)
			}
		}
	})
}

func TestParse(t *testing.T) {
	testLinks(t, "axios bare call", `axios('/api/users')`, "https://example.com", 1)
	testLinks(t, "fetch literal", `fetch('https://api.example.com/data')`, "https://example.com", 1)
	testLinks(t, "$.get shorthand", `$.get('/api/data', {id: 1})`, "https://example.com", 1)
	testLinks(t, "BX.ajax.post", `BX.ajax.post('/bitrix/tools/something.php', {a: 1})`, "https://example.com", 1)
	testLinks(t, "BX.ajax config", `BX.ajax({url: '/bitrix/services/main/ajax.php', method: 'POST'})`, "https://example.com", 1)
	testLinks(t, "$.ajax config", `$.ajax({url: '/api/items', type: 'POST'})`, "https://example.com", 1)
	testLinks(t, "axios.get", `axios.get('/api/data', {params: {id: 1}})`, "https://example.com", 1)
	testLinks(t, "superagent.get", `superagent.get('/api/data').end()`, "https://example.com", 1)
	testLinks(t, "Angular http.get", `this.http.get('/api/users').subscribe()`, "https://example.com", 1)
	testLinks(t, "new Request", `new Request('/api/data', {method: 'GET'})`, "https://example.com", 1)
	testLinks(t, "url config with endpoint key", `{endpoint: '/api/data'}`, "https://example.com", 1)
	testLinks(t, "baseURL config", `{baseURL: 'https://api.example.com'}`, "https://example.com", 1)
	testLinks(t, "template literal with var",
		"const id = 123;\nconst url = `/api/users/${id}`;\nfetch(url);",
		"https://example.com", 1)
	testLinks(t, "escaping in strings", `fetch('/api/with\'quote')`, "https://example.com", 1)
}

func TestVarRef(t *testing.T) {
	testLinks(t, "fetch(var)",
		`var endpoint = '/api/delete-account'; fetch(endpoint, {method: 'POST'});`,
		"https://example.com", 1)

	testLinks(t, "url config(var)",
		`const API_URL = '/rest/api/v2/users'; $.ajax({url: API_URL, method: 'GET'});`,
		"https://example.com", 1)

	testLinks(t, "var concat chain",
		"const BASE = '/api/v1';\nconst URL = BASE + '/users';\n"+
			"fetch(URL);",
		"https://example.com", 1)

	testLinks(t, "transitive var chain",
		"const HOST = 'https://api.example.com';\nconst BASE = HOST + '/v2';\n"+
			"const URL = BASE + '/users';\n"+
			"fetch(URL);",
		"https://example.com", 1)

	testLinks(t, "vars combined in expression",
		"const ROOT = '/api';\nconst PATH = '/auth/login';\n"+
			"fetch(ROOT + PATH, {method: 'POST'});",
		"https://example.com", 1)

	testLinks(t, "XHR.open(var)",
		`var url = '/api/data'; var xhr = new XMLHttpRequest(); xhr.open('GET', url);`,
		"https://example.com", 1)

	testLinks(t, "url config with nested obj var",
		`const opts = {url: '/api/nested'}; fetch(opts.url);`,
		"https://example.com", 1)

	testLinks(t, "multi-var with dot access",
		`const base = 'https://api.example.com'; fetch(base + '/v1/users');`,
		"https://example.com", 1)

	// Both literals are surfaced: /api/updated as a traced call (js-api) and the
	// overwritten /api/init as a harvested string literal (js-str). Endpoint
	// harvesting is intentionally context-free, so hardcoded endpoint strings
	// are reported even when dataflow would prove one is dead.
	testLinks(t, "reassignment",
		`let url = '/api/init'; url = '/api/updated'; fetch(url);`,
		"https://example.com", 2)
}

func TestAPIJsNotDetected(t *testing.T) {
	links, _ := Parse(`fetch('/api.js')`, "https://example.com", true)
	for _, l := range links {
		if l.HREF == "/api.js" {
			t.Errorf("api.js should not be classified as API")
		}
	}
}

func TestArrayJoin(t *testing.T) {
	testLinks(t, "array join",
		`const ROOT = ['https://api.example.com', '/v1/users'].join(''); fetch(ROOT);`,
		"https://example.com", 1)

	testLinks(t, "array join multi element",
		`const URL = ['https://api.example.com', '/v1', '/users'].join(''); fetch(URL);`,
		"https://example.com", 1)
}

func TestEdgeCaseNoFalsePositives(t *testing.T) {
	testLinks(t, "fetch with image path", `fetch('/assets/logo.png')`, "https://example.com", 0)
	testLinks(t, "fetch with css path", `fetch('/css/main.css')`, "https://example.com", 0)
	testLinks(t, "fetch with html path", `fetch('/page.html')`, "https://example.com", 0)
	// A plain path without an API signal is not harvested.
	testLinks(t, "window.location", `window.location.href = '/redirect'`, "https://example.com", 0)
	// Comments are stripped by the tokenizer, so their contents never harvest.
	testLinks(t, "comment only", `// fetch('/api/commented')`, "https://example.com", 0)
	testLinks(t, "multiline comment", `/* fetch('/api/commented') */`, "https://example.com", 0)
}

func TestHarvestEndpointLiterals(t *testing.T) {
	// Endpoint-shaped literals are harvested regardless of call context — this
	// is what surfaces runtime-built endpoints (config blobs, this.x refs) that
	// call-graph tracing cannot resolve.
	testLinks(t, "endpoint in console.log", `console.log('/api/debug')`, "https://example.com", 1)
	testLinks(t, "ajax.php in config blob",
		`window.__DATA__ = {"ajaxUrl":"/apartments/ajax.php","region":0};`,
		"https://example.com", 1)
	testLinks(t, "runtime-built fetch surfaces base",
		`var resp = await fetch(this.ajaxUrl + '?' + this.buildParams(1).toString());`,
		"https://example.com", 0)
	testLinks(t, "bitrix service endpoint literal",
		`var u = '/bitrix/services/main/ajax.php?action=getData';`,
		"https://example.com", 1)
	// Plain non-API paths and asset paths are not harvested.
	testLinks(t, "plain page path not harvested", `var u = '/about/team';`, "https://example.com", 0)
	testLinks(t, "asset path not harvested", `var u = '/static/app.css';`, "https://example.com", 0)
}

func TestConfigCallAsSecondArg(t *testing.T) {
	testLinks(t, "$.ajax({url})",
		`$.ajax({url: '/api/test', data: {id: 1}})`,
		"https://example.com", 1)
}

func TestExecutionOrder(t *testing.T) {
	testLinks(t, "decl before use",
		`const url = '/api/after'; fetch(url);`,
		"https://example.com", 1)
}

func TestMultipleAPICalls(t *testing.T) {
	js := `fetch('/api/one');
fetch('/api/two');
fetch('/api/three');`
	testLinks(t, "multiple calls", js, "https://example.com", 3)
}

func TestEmptyString(t *testing.T) {
	links, _ := Parse("", "https://example.com", true)
	if len(links) != 0 {
		t.Errorf("expected 0 links for empty input, got %d", len(links))
	}
}

func TestOnlyComments(t *testing.T) {
	links, _ := Parse("// just a comment\n/* block */", "https://example.com", true)
	if len(links) != 0 {
		t.Errorf("expected 0 links for comments only, got %d", len(links))
	}
}

func testObs(t *testing.T, name, js, sourceURL string, wantURL, wantMethod, wantBody string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		_, obs := Parse(js, sourceURL, true)
		found := false
		for _, o := range obs {
			if o.URL != wantURL {
				continue
			}
			if wantMethod != "" && o.Method != wantMethod {
				t.Errorf("method: got %s want %s", o.Method, wantMethod)
			}
			if wantBody != "" && !strings.Contains(o.Body, wantBody) {
				t.Errorf("body: got %q want substring %q", o.Body, wantBody)
			}
			found = true
			break
		}
		if !found {
			t.Errorf("no observation for %s; obs=%+v", wantURL, obs)
		}
	})
}

func TestObservationsFetch(t *testing.T) {
	testObs(t, "fetch init object",
		`fetch('/api/order', {method: 'POST', headers: {'X-CSRF': 't1'}, body: JSON.stringify({id: 5})})`,
		"https://example.com/app.js", "https://example.com/api/order", "POST", `"id":5`)
	testObs(t, "fetch var init",
		`var opts = {method: 'PUT', body: {name: 'x'}}; fetch('/api/user', opts);`,
		"https://example.com/app.js", "https://example.com/api/user", "PUT", `"name":"x"`)
}

func TestObservationsJQueryPost(t *testing.T) {
	testObs(t, "$.post data",
		`$.post('/api/register', {login: 'bob', pass: 'pw'})`,
		"https://example.com/app.js", "https://example.com/api/register", "POST", `"login":"bob"`)
}

func TestObservationsAxiosPost(t *testing.T) {
	testObs(t, "axios.post data",
		`axios.post('/api/items', {page: 1, size: 20})`,
		"https://example.com/app.js", "https://example.com/api/items", "POST", `"page":1`)
}

func TestObservationsBitrixRunAction(t *testing.T) {
	_, obs := Parse(
		`BX.ajax.runAction('catalog.getById', {data: {id: 309}})`,
		"https://example.com/page", true)
	for _, o := range obs {
		if o.URL != "https://example.com/bitrix/services/main/ajax.php" {
			t.Errorf("URL: %s", o.URL)
		}
		if o.Method != "POST" {
			t.Errorf("method: %s", o.Method)
		}
		if !strings.Contains(o.Body, "catalog.getById") || !strings.Contains(o.Body, `"id":309`) {
			t.Errorf("body: %s", o.Body)
		}
	}
}

func TestObservationsXHR(t *testing.T) {
	testObs(t, "xhr open",
		`var x = new XMLHttpRequest(); x.open('POST', '/api/upload'); x.send();`,
		"https://example.com/app.js", "https://example.com/api/upload", "POST", "")
}

func TestTokenNotLeakedInObs(t *testing.T) {
	_, obs := Parse(
		`fetch('/api/admin', {method: 'POST', headers: {'X-Api-Token': 'abcdef1234567890abcdef'}, body: '{}'})`,
		"https://example.com/app.js", true)
	if len(obs) == 0 {
		t.Fatalf("no observations captured")
	}
	// The raw observation keeps the value as evidence; rendering must mask it.
	out := contract.Render(contract.Infer(obs), true)
	if strings.Contains(out, "abcdef1234567890abcdef") {
		t.Errorf("token value leaked into contract output:\n%s", out)
	}
}

// TestMinifiedClientMining covers the axios-like wrapper pattern seen in real
// minified SPAs, where the client instance keeps a minified name (e, g, ap)
// that no hard-coded root list can know about. Generic method-call mining must
// surface both the endpoint links and the per-call observations.
func TestMinifiedClientMining(t *testing.T) {
	js := `
var e = window.http, g = window.http2;
e.get("/api/admin/cameras").then(n => n.data);
e.post("/api/admin/recorders/" + n.sync);
`
	links, _ := Parse(js, "https://osi.example.com/app.js", true)
	urls := make(map[string]bool)
	for _, l := range links {
		urls[l.HREF] = true
		if l.Tag != "js-api" || len(l.APIDetails) == 0 {
			t.Errorf("expected js-api link with method for %q", l.HREF)
		}
	}
	if !urls["/api/admin/cameras"] {
		t.Errorf("GET endpoint missing; got %v", urls)
	}
	if !urls["/api/admin/cameras"] {
		t.Errorf("expected /api/admin/cameras link")
	}
}

func TestMinifiedTemplateEndpoint(t *testing.T) {
	_, obs := Parse(
		"g.get(`/api/admin/cameras/${e}/snapshot`, {responseType: 'blob'})",
		"https://osi.example.com/app.js", true)
	found := false
	for _, o := range obs {
		if strings.HasPrefix(o.URL, "https://osi.example.com/api/admin/cameras/") &&
			strings.Contains(o.URL, "{e}") {
			found = true
			if o.Method != "GET" {
				t.Errorf("method: %s", o.Method)
			}
		}
	}
	if !found {
		t.Errorf("template GET observation missing; obs=%+v", obs)
	}

	_, obs = Parse(
		"e.post(`/api/admin/recorders/${n}/sync`)",
		"https://osi.example.com/app.js", true)
	found = false
	for _, o := range obs {
		if strings.Contains(o.URL, "/api/admin/recorders/") && strings.Contains(o.URL, "{n}") {
			found = true
			if o.Method != "POST" {
				t.Errorf("method: %s", o.Method)
			}
		}
	}
	if !found {
		t.Errorf("template POST observation missing; obs=%+v", obs)
	}
}

func TestConfigObjectObservations(t *testing.T) {
	testObs(t, "axios config", `
axios({url: '/api/login', method: 'POST', data: {login: 'bob', pass: 'pw'}})`,
		"https://example.com/app.js", "https://example.com/api/login", "POST", `"login":"bob"`)
	testObs(t, "request options object", `
request({url: '/api/profile', method: 'PUT', headers: {'X-T': '1'}, data: {name: 'x'}})`,
		"https://example.com/app.js", "https://example.com/api/profile", "PUT", `"name":"x"`)
	testObs(t, "superagent post with headers config", `
ap.post('/api/orders', {id: 7}, {headers: {'X-A': 'b'}})`,
		"https://example.com/app.js", "https://example.com/api/orders", "POST", `"id":7`)
}

func TestGetGuardFiltersPageRoutes(t *testing.T) {
	testLinks(t, "router page route not API", `router.get('/about/team')`, "https://example.com", 0)
	testLinks(t, "router .get page", `app.get('/users')`, "https://example.com", 0)
	testLinks(t, "api .get still mined", `e.get('/api/users')`, "https://example.com", 1)
}

func TestUnresolvableIdentNotJunkLink(t *testing.T) {
	testLinks(t, "bare unknown var", `e.get(this.baseUrl)`, "https://example.com", 0)
	testLinks(t, "concat tail keeps prefix",
		`e.post('/api/user/archive/requests/' + userId)`,
		"https://example.com", 1)
}

func TestResponseFieldInference(t *testing.T) {
	src := `fetch('/api/city-poi?days=7').then(r => r.json()).then(data => {
  console.log(data.items.length);
  data.items.forEach(x => { console.log(x.price, x.address); });
  console.log(data.meta.total, data.meta.generated_at);
});
axios.get('/api/complexes-map').then(res => {
  res.data.features.map(f => f.geometry.coordinates);
  res.data.count.toFixed(2);
});`
	_, obs := Parse(src, "https://x.test/app.js", true)
	got := map[string]map[string]string{}
	for _, o := range obs {
		for _, f := range o.ResponseFields {
			if got[o.URL] == nil {
				got[o.URL] = map[string]string{}
			}
			got[o.URL][f.Path] = f.Kind
		}
	}
	fields := got["https://x.test/api/city-poi?days=7"]
	for path, kind := range map[string]string{
		"items":           "array",
		"items[].price":   "any",
		"items[].address": "any",
		"meta.total":      "any",
	} {
		if fields[path] != kind {
			t.Errorf("city-poi %s = %q, want %q (all: %v)", path, fields[path], kind, fields)
		}
	}
	if _, leaked := fields["json"]; leaked {
		t.Errorf("payload method leaked into schema: %v", fields)
	}
	mapFields := got["https://x.test/api/complexes-map"]
	if mapFields["data.features"] != "array" {
		t.Errorf("complexes-map data.features = %q (all: %v)", mapFields["data.features"], mapFields)
	}
	if mapFields["data.features[].geometry.coordinates"] == "" {
		t.Errorf("missing nested element field (all: %v)", mapFields)
	}
	if mapFields["data.count"] != "number" {
		t.Errorf("complexes-map data.count = %q, want number", mapFields["data.count"])
	}
}
