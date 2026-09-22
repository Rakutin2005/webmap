package jsanalyzer

import (
	"testing"
)

func testLinks(t *testing.T, name, js, sourceURL string, expectCount int) {
	t.Run(name, func(t *testing.T) {
		links := Parse(js, sourceURL, true)
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
	links := Parse(`fetch('/api.js')`, "https://example.com", true)
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
	links := Parse("", "https://example.com", true)
	if len(links) != 0 {
		t.Errorf("expected 0 links for empty input, got %d", len(links))
	}
}

func TestOnlyComments(t *testing.T) {
	links := Parse("// just a comment\n/* block */", "https://example.com", true)
	if len(links) != 0 {
		t.Errorf("expected 0 links for comments only, got %d", len(links))
	}
}
