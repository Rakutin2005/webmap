package emulator

import (
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// run executes a single script under the given base URL and returns the
// resolved URLs of all intercepted network calls.
func run(t *testing.T, base, code string) []string {
	t.Helper()
	sb := New(base)
	sb.Run([]Script{{Code: code, URL: "inline:test"}})
	sum := sb.Summary()
	urls := make([]string, 0, len(sum.List))
	for _, c := range sum.List {
		urls = append(urls, c.URL)
	}
	if len(sum.ErrorList) > 0 {
		t.Logf("errors: %v", sum.ErrorList)
	}
	return urls
}

func has(urls []string, want string) bool {
	for _, u := range urls {
		if u == want || strings.Contains(u, want) {
			return true
		}
	}
	return false
}

func mustFind(t *testing.T, name, base, code, want string) {
	t.Run(name, func(t *testing.T) {
		urls := run(t, base, code)
		if !has(urls, want) {
			t.Errorf("expected to discover %q, got %v", want, urls)
		}
	})
}

func TestBasicSinks(t *testing.T) {
	base := "https://site.test/page"
	mustFind(t, "fetch literal", base, `fetch('/api/users')`, "https://site.test/api/users")
	mustFind(t, "fetch absolute", base, `fetch('https://api.other.com/v1/data')`, "https://api.other.com/v1/data")
	mustFind(t, "xhr", base, `var x=new XMLHttpRequest(); x.open('POST','/api/login'); x.send();`, "https://site.test/api/login")
	mustFind(t, "websocket", base, `new WebSocket('wss://site.test/socket')`, "wss://site.test/socket")
	mustFind(t, "eventsource", base, `new EventSource('/sse/stream')`, "https://site.test/sse/stream")
	mustFind(t, "beacon", base, `navigator.sendBeacon('/track', 'x')`, "https://site.test/track")
	mustFind(t, "image", base, `var i=new Image(); i.src='/pixel.gif?e=1';`, "https://site.test/pixel.gif")
	mustFind(t, "new Request", base, `fetch(new Request('/api/req'))`, "https://site.test/api/req")
}

// runCalls executes a script and returns every captured NetworkCall (with
// bodies and headers) instead of only URLs.
func runCalls(t *testing.T, base, code string) []NetworkCall {
	t.Helper()
	sb := New(base)
	sb.Run([]Script{{Code: code, URL: "inline:test"}})
	sum := sb.Summary()
	if len(sum.ErrorList) > 0 {
		t.Logf("errors: %v", sum.ErrorList)
	}
	return sum.List
}

func findCall(calls []NetworkCall, url string) *NetworkCall {
	for i := range calls {
		if calls[i].URL == url || strings.Contains(calls[i].URL, url) {
			return &calls[i]
		}
	}
	return nil
}

func TestFetchCapturesBodyAndHeaders(t *testing.T) {
	calls := runCalls(t, "https://site.test/", `
		fetch('/api/order', {
			method: 'POST',
			headers: {'X-CSRF': 'tok123', 'Content-Type': 'application/json'},
			body: JSON.stringify({user: 42, vip: true})
		});
	`)
	c := findCall(calls, "https://site.test/api/order")
	if c == nil {
		t.Fatalf("no /api/order call in %+v", calls)
	}
	if c.Method != "POST" {
		t.Errorf("method: %s", c.Method)
	}
	if strings.Contains(c.Body, "42") == false || strings.Contains(c.Body, "vip") == false {
		t.Errorf("body: %s", c.Body)
	}
	var hasCSRF bool
	for _, h := range c.Headers {
		if h.Name == "X-CSRF" && h.Value == "tok123" {
			hasCSRF = true
		}
	}
	if !hasCSRF {
		t.Errorf("headers: %+v", c.Headers)
	}
}

func TestXHRCapturesBodyAndHeaders(t *testing.T) {
	calls := runCalls(t, "https://site.test/", `
		var x = new XMLHttpRequest();
		x.open('PATCH', '/api/order/7');
		x.setRequestHeader('Content-Type', 'application/x-www-form-urlencoded');
		x.setRequestHeader('X-Requested-With', 'XMLHttpRequest');
		x.send('title=new&qty=1');
	`)
	// open() records a headerless stub; send() records the full payload.
	var c *NetworkCall
	for i := range calls {
		if strings.Contains(calls[i].URL, "/api/order/7") && calls[i].Body != "" {
			c = &calls[i]
		}
	}
	if c == nil {
		t.Fatalf("no send-record call in %+v", calls)
	}
	if c.Method != "PATCH" {
		t.Errorf("method: %s", c.Method)
	}
	if !strings.Contains(c.Body, "title=new") {
		t.Errorf("body: %s", c.Body)
	}
	var hasXrw bool
	for _, h := range c.Headers {
		if h.Name == "X-Requested-With" && h.Value == "XMLHttpRequest" {
			hasXrw = true
		}
	}
	if !hasXrw {
		t.Errorf("headers: %+v", c.Headers)
	}
}

func TestURLSearchParamsBodySerializes(t *testing.T) {
	calls := runCalls(t, "https://site.test/", `
		var p = new URLSearchParams();
		p.append('action', 'search');
		p.append('page', '2');
		fetch('/api/search', {method: 'POST', body: p});
	`)
	c := findCall(calls, "https://site.test/api/search")
	if c == nil {
		t.Fatalf("no call in %+v", calls)
	}
	if !strings.Contains(c.Body, "action=search") || !strings.Contains(c.Body, "page=2") {
		t.Errorf("body: %s", c.Body)
	}
}

func TestFormDataBodySerializes(t *testing.T) {
	calls := runCalls(t, "https://site.test/", `
		var f = new FormData();
		f.append('login', 'bob');
		f.append('pass', 'secret4');
		fetch('/api/login', {method: 'POST', body: f});
	`)
	c := findCall(calls, "https://site.test/api/login")
	if c == nil {
		t.Fatalf("no call in %+v", calls)
	}
	if !strings.Contains(c.Body, "login=bob") {
		t.Errorf("body: %s", c.Body)
	}
}

func TestDedupKeepsDistinctBodies(t *testing.T) {
	calls := runCalls(t, "https://site.test/", `
		fetch('/api/filter', {method: 'POST', body: '{"x":1}'});
		fetch('/api/filter', {method: 'POST', body: '{"x":2}'});
		fetch('/api/filter', {method: 'POST', body: '{"x":1}'});
	`)
	var n int
	for _, c := range calls {
		if strings.Contains(c.URL, "/api/filter") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("expected 2 distinct bodies for /api/filter, got %d calls: %+v", n, calls)
	}
}

func TestHeadersInstanceCaptured(t *testing.T) {
	calls := runCalls(t, "https://site.test/", `
		var h = new Headers();
		h.append('X-Token', 'abc');
		fetch('/api/me', {method: 'GET', headers: h});
	`)
	c := findCall(calls, "https://site.test/api/me")
	if c == nil {
		t.Fatalf("no call in %+v", calls)
	}
	var ok bool
	for _, hp := range c.Headers {
		if hp.Name == "X-Token" && hp.Value == "abc" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("headers: %+v", c.Headers)
	}
}

func TestDynamicElementInjection(t *testing.T) {
	base := "https://site.test/"
	mustFind(t, "script.src", base,
		`var s=document.createElement('script'); s.src='/widget/loader.js'; document.body.appendChild(s);`,
		"https://site.test/widget/loader.js")
	mustFind(t, "setAttribute", base,
		`var s=document.createElement('script'); s.setAttribute('src','/chunks/main.js');`,
		"https://site.test/chunks/main.js")
	mustFind(t, "form.action submit", base,
		`var f=document.createElement('form'); f.action='/submit/order'; f.submit();`,
		"https://site.test/submit/order")
}

func TestObfuscation(t *testing.T) {
	base := "https://site.test/"

	// base64 (atob) — a classic obfuscation the AST/regex analyzers cannot decode.
	mustFind(t, "atob base64", base,
		`fetch(atob('L2FwaS9zZWNyZXQ='))`, // "/api/secret"
		"https://site.test/api/secret")

	// obfuscator.io-style string array + index.
	mustFind(t, "string array index", base,
		`var _0x=['https://','api.','site.test','/v2/','orders'];`+
			`fetch(_0x[0]+_0x[1]+_0x[2]+_0x[3]+_0x[4]);`,
		"https://api.site.test/v2/orders")

	// hex-escaped string.
	mustFind(t, "hex escapes", base,
		`fetch('\x2f\x61\x70\x69\x2f\x68\x65\x78')`, // "/api/hex"
		"https://site.test/api/hex")

	// eval-packed code.
	mustFind(t, "eval packed", base,
		`eval("fetch('/api/evaled')")`,
		"https://site.test/api/evaled")

	// String.fromCharCode assembly.
	mustFind(t, "fromCharCode", base,
		`fetch(String.fromCharCode(47,97,112,105,47,99,99))`, // "/api/cc"
		"https://site.test/api/cc")

	// computed property + join.
	mustFind(t, "array join", base,
		`fetch(['','api','joined'].join('/'))`,
		"https://site.test/api/joined")
}

func TestRuntimeBuilt(t *testing.T) {
	base := "https://site.test/"

	// URLSearchParams-based construction (the svoydom.kz pattern).
	mustFind(t, "URLSearchParams", base,
		`var p=new URLSearchParams(); p.append('page','1'); p.set('city','astana');`+
			`fetch('/apartments/ajax.php'+'?'+p.toString());`,
		"https://site.test/apartments/ajax.php")

	// new URL + searchParams.
	mustFind(t, "URL builder", base,
		`var u=new URL('/api/search', location.origin); u.searchParams.set('q','x'); fetch(u.toString());`,
		"https://site.test/api/search")

	// value passed through object property + this.
	mustFind(t, "this.prop closure", base,
		`var o={ base:'/api/', load:function(id){ return fetch(this.base+id); } }; o.load('42');`,
		"https://site.test/api/42")

	// template literal with interpolation.
	mustFind(t, "template literal", base,
		"var id=7; fetch(`/api/items/${id}/detail`);",
		"https://site.test/api/items/7/detail")
}

func TestDeferred(t *testing.T) {
	base := "https://site.test/"
	mustFind(t, "setTimeout", base,
		`setTimeout(function(){ fetch('/api/deferred'); }, 100);`,
		"https://site.test/api/deferred")
	mustFind(t, "promise chain", base,
		`Promise.resolve('/api/promised').then(function(u){ fetch(u); });`,
		"https://site.test/api/promised")
	mustFind(t, "DOMContentLoaded", base,
		`document.addEventListener('DOMContentLoaded', function(){ fetch('/api/onready'); });`,
		"https://site.test/api/onready")
	mustFind(t, "window load", base,
		`window.addEventListener('load', function(){ fetch('/api/onload'); });`,
		"https://site.test/api/onload")
	mustFind(t, "xhr onload chains", base,
		`var x=new XMLHttpRequest(); x.open('GET','/api/first'); x.onload=function(){ fetch('/api/second'); }; x.send();`,
		"https://site.test/api/second")
}

func TestNoFalseNav(t *testing.T) {
	// data:, blob:, javascript:, and fragments must not be recorded.
	urls := run(t, "https://site.test/", `
		fetch('data:text/plain,hello');
		fetch('#anchor');
		new Image().src='javascript:void(0)';
		fetch('about:blank');
	`)
	if len(urls) != 0 {
		t.Errorf("expected no recorded calls, got %v", urls)
	}
}

func TestMultiScriptRealm(t *testing.T) {
	// A library defined in script 1 is used by app code in script 2, sharing one
	// realm (this is why ordered, same-realm execution matters).
	sb := New("https://site.test/")
	sb.Run([]Script{
		{Code: `window.API = { call: function(p){ fetch('/api'+p); } };`, URL: "lib.js"},
		{Code: `API.call('/from-lib');`, URL: "app.js"},
	})
	sum := sb.Summary()
	found := false
	for _, c := range sum.List {
		if strings.Contains(c.URL, "/api/from-lib") {
			found = true
		}
	}
	if !found {
		t.Errorf("cross-script realm call not found: %+v", sum.List)
	}
}

func TestTimeoutInterrupt(t *testing.T) {
	// An infinite loop must not hang the run: the watchdog interrupts it and the
	// endpoint queued before the loop is still captured.
	sb := NewWithLimits("https://site.test/", Limits{Timeout: 500 * time.Millisecond, MaxJobs: 2000})
	done := make(chan struct{})
	go func() {
		sb.Run([]Script{{Code: `fetch('/api/before'); while(true){}`, URL: "hang.js"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return: watchdog failed to interrupt infinite loop")
	}
	if !has(collect(sb), "https://site.test/api/before") {
		t.Errorf("call before infinite loop was lost: %v", collect(sb))
	}
}

func TestOversizedScriptSkipped(t *testing.T) {
	big := "var x='" + strings.Repeat("a", 2000) + "'; fetch('/api/big');"
	sb := NewWithLimits("https://site.test/", Limits{MaxJS: 1000}) // 1000-byte cap
	sb.Run([]Script{{Code: big, URL: "big.js"}})
	if len(collect(sb)) != 0 {
		t.Errorf("oversized script should have been skipped, got %v", collect(sb))
	}
}

func TestAnimationFrameDropped(t *testing.T) {
	// requestAnimationFrame is a no-op, so an rAF-driven loop cannot spin and its
	// callback never runs (graphics work is intentionally ignored).
	sb := New("https://site.test/")
	sb.Run([]Script{{Code: `function f(){ fetch('/api/raf'); requestAnimationFrame(f); } requestAnimationFrame(f);`, URL: "raf.js"}})
	if has(collect(sb), "/api/raf") {
		t.Errorf("requestAnimationFrame callback should not run")
	}
}

func collect(sb *Sandbox) []string {
	sum := sb.Summary()
	urls := make([]string, 0, len(sum.List))
	for _, c := range sum.List {
		urls = append(urls, c.URL)
	}
	return urls
}

func TestHardDeadlineNativeHang(t *testing.T) {
	// Catastrophic-backtracking regex runs in native (regexp2) code that a goja
	// soft-interrupt cannot unwind. The hard wall-clock deadline must still make
	// Run return, and the call issued before the regex must be captured.
	sb := NewWithLimits("https://site.test/", Limits{Timeout: 500 * time.Millisecond})
	code := `fetch('/api/before-regex'); ` +
		`var re=/(a+)+$/; re.test('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaX');`
	done := make(chan struct{})
	go func() { sb.Run([]Script{{Code: code, URL: "hang.js"}}); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not return within hard deadline: native hang not bounded")
	}
	if !has(collect(sb), "/api/before-regex") {
		t.Errorf("call before native hang was lost: %v", collect(sb))
	}
}

func TestWorkerRecorded(t *testing.T) {
	base := "https://site.test/"
	mustFind(t, "new Worker", base,
		`var w=new Worker('/workers/ingest.js'); w.postMessage('go');`,
		"https://site.test/workers/ingest.js")
	mustFind(t, "new SharedWorker", base,
		`var w=new SharedWorker('/workers/shared.js');`,
		"https://site.test/workers/shared.js")
}

func TestLocationAssignment(t *testing.T) {
	base := "https://site.test/page"
	mustFind(t, "location.href =", base,
		`location.href='/login';`,
		"https://site.test/login")
	mustFind(t, "window.location =", base,
		`window.location='https://account.other.com/sso';`,
		"https://account.other.com/sso")
	mustFind(t, "location hash only not nav", base,
		`location.hash='#section'; fetch('/api/still-runs');`,
		"https://site.test/api/still-runs")
}

func TestURLSettersReflected(t *testing.T) {
	base := "https://site.test/"
	mustFind(t, "pathname setter", base,
		`var u=new URL('/a', location.origin); u.pathname='/api/x'; fetch(u.toString());`,
		"https://site.test/api/x")
	mustFind(t, "search setter", base,
		`var u=new URL('/api/list', location.origin); u.search='?page=2'; fetch(u.toString());`,
		"https://site.test/api/list?page=2")
	mustFind(t, "href setter", base,
		`var u=new URL('/unused', location.origin); u.href='/api/final'; fetch(u);`,
		"https://site.test/api/final")
	mustFind(t, "hostname setter", base,
		`var u=new URL('/v1', location.origin); u.hostname='cdn.site.test'; fetch(u.toString());`,
		"https://cdn.site.test/v1")
}

func TestSrcsetAndCSSURLs(t *testing.T) {
	base := "https://site.test/"
	mustFind(t, "img srcset", base,
		`var i=document.createElement('img'); i.srcset='/img/a.png 1x, /img/b.png 2x';`,
		"https://site.test/img/b.png")
	mustFind(t, "srcset setAttribute", base,
		`var i=document.createElement('img'); i.setAttribute('srcset','/img/c.png 480w');`,
		"https://site.test/img/c.png")
	mustFind(t, "style.backgroundImage", base,
		`var d=document.createElement('div'); d.style.backgroundImage="url('/bg/hero.jpg')";`,
		"https://site.test/bg/hero.jpg")
	mustFind(t, "style.cssText", base,
		`var d=document.createElement('div'); d.style.cssText="background:url('/bg/sprite.png') no-repeat";`,
		"https://site.test/bg/sprite.png")
}

func TestStyleURLNotFalsePositive(t *testing.T) {
	// A plain color value must not be recorded as a resource load.
	urls := run(t, "https://site.test/",
		`var d=document.createElement('div'); d.style.backgroundImage='linear-gradient(red, blue)'; d.style.backgroundColor='#fff';`)
	if len(urls) != 0 {
		t.Errorf("non-URL style values recorded as calls: %v", urls)
	}
}

// runHTML runs code with real page HTML fed into the sandbox.
func runHTML(t *testing.T, base, pageHTML, code string) []NetworkCall {
	t.Helper()
	sb := New(base)
	sb.SetPageHTML(pageHTML)
	sb.Run([]Script{{Code: code, URL: "inline:dom-test"}})
	sum := sb.Summary()
	if len(sum.ErrorList) > 0 {
		t.Logf("errors: %v", sum.ErrorList)
	}
	return sum.List
}

func notHas(calls []NetworkCall, want string) bool {
	for _, c := range calls {
		if c.URL == want || strings.Contains(c.URL, want) {
			return false
		}
	}
	return true
}

func TestDomDrivenDiscovery(t *testing.T) {
	base := "https://site.test/"
	page := `<html><head>
		<title>My Site</title>
		<meta name="csrf-token" content="abc123">
		<meta property="og:url" content="https://site.test/share-page">
		<script type="application/json" id="cfg">{"api":"/api/engine/settings","mode":"dev"}</script>
	</head><body>
		<form id="search-form" action="/api/search"><input name="q" type="text"></form>
		<div id="app" data-endpoint="/api/app-data">
			<a class="card" data-endpoint="/api/card/1">one</a>
			<a class="card" data-endpoint="/api/card/2">two</a>
		</div>
		<img src="/img/logo.png" srcset="/img/logo@2x.png 2x, /img/logo@3x.png 3x">
		<script src="/assets/app.js"></script>
	</body></html>`

	calls := runHTML(t, base, page, `
		var csrf=document.querySelector('meta[name="csrf-token"]').getAttribute('content');
		fetch('/api/check?token='+csrf);
		var og=document.querySelector('meta[property="og:url"]').getAttribute('content');
		fetch(og);
		var cfg=JSON.parse(document.getElementById('cfg').textContent);
		fetch(cfg.api);
		fetch(document.getElementById('app').dataset.endpoint);
		var els=document.querySelectorAll('[data-endpoint]');
		for (var i=0;i<els.length;i++){
			if (els[i].getAttribute('data-endpoint').indexOf('card')>=0) fetch(els[i].getAttribute('data-endpoint'));
		}
		var f=document.getElementById('search-form');
		if (f) fetch(f.getAttribute('action'));
	`)

	for _, want := range []string{
		"https://site.test/api/check?token=abc123",
		"https://site.test/share-page",
		"https://site.test/api/engine/settings",
		"https://site.test/api/app-data",
		"https://site.test/api/card/1",
		"https://site.test/api/search",
	} {
		if !hasURLs(calls, want) {
			t.Errorf("DOM-driven discovery missed %q (calls: %v)", want, calls)
		}
	}
	// Static srcset candidates parsed from the page are recorded (new discovery).
	if !hasURLs(calls, "https://site.test/img/logo@2x.png") || !hasURLs(calls, "https://site.test/img/logo@3x.png") {
		t.Errorf("static srcset candidates not recorded: %v", calls)
	}
	// Static src/href of already-crawled resources must NOT be re-recorded.
	if !notHas(calls, "https://site.test/assets/app.js") {
		t.Errorf("static script src must not be recorded by the emulator: %v", calls)
	}
	if !notHas(calls, "https://site.test/img/logo.png") {
		t.Errorf("static img src must not be recorded by the emulator: %v", calls)
	}
}

func hasURLs(calls []NetworkCall, want string) bool {
	for _, c := range calls {
		if c.URL == want {
			return true
		}
	}
	return false
}

func TestDomFormSubmitMethod(t *testing.T) {
	base := "https://site.test/"
	page := `<html><body><form id="f" action="/api/post-form"><input name="q"></form></body></html>`
	calls := runHTML(t, base, page, `document.getElementById('f').submit();`)
	found := false
	for _, c := range calls {
		if c.URL == "https://site.test/api/post-form" {
			found = true
			if c.Method != "POST" {
				t.Errorf("form submit should be POST, got %s", c.Method)
			}
		}
	}
	if !found {
		t.Errorf("form submit action not recorded: %v", calls)
	}
}

func TestDomSelectorMatcher(t *testing.T) {
	base := "https://site.test/"
	page := `<html><body>
		<div id="app" class="box dark"><span class="inner" data-e="rev"></span></div>
		<div class="box"><span class="inner"></span></div>
		<input type="text" name="q" data-hint="search">
	</body></html>`

	calls := runHTML(t, base, page, `
		function pick(sel){ var el=document.querySelector(sel); return el ? (el.getAttribute('data-e')||el.getAttribute('data-hint')||el.id) : ''; }
		fetch('/a/'+pick('#app'));
		fetch('/b/'+pick('div.box'));
		fetch('/c/'+pick('div.dark'));
		fetch('/d/'+pick('#app .inner'));
		fetch('/f/'+pick('body div#app'));
		fetch('/g/'+pick('input[name="q"]'));
		fetch('/h/'+pick('input[type="text"][name="q"]'));
		var app=document.querySelector('#app');
		var s=app.querySelector('.inner');
		fetch('/m/'+(s?s.getAttribute('data-e'):'none'));
		var spans=app.getElementsByTagName('span');
		fetch('/n/'+(spans.length?spans[0].getAttribute('data-e'):'none'));
	`)

	for _, want := range []string{
		"/a/app", "/b/app", "/c/app", "/d/rev", "/f/app",
		"/g/search", "/h/search", "/m/rev", "/n/rev",
	} {
		if !hasURLs(calls, "https://site.test"+want) {
			t.Errorf("selector %q resolved wrong, want %q, calls: %v", want, want, calls)
		}
	}
}

func TestDomTitlePopulated(t *testing.T) {
	sb := New("https://site.test/")
	sb.SetPageHTML(`<html><head><title>My Page</title></head><body></body></html>`)
	sb.Run([]Script{{Code: `void 0`, URL: "inline:title"}})
	doc, ok := sb.vm.runtime.Get("document").(*goja.Object)
	if !ok {
		t.Fatal("document not installed")
	}
	if got := doc.Get("title").String(); got != "My Page" {
		t.Errorf("document.title = %q, want %q", got, "My Page")
	}
}

func TestDomFallbackNoHTML(t *testing.T) {
	// Without SetPageHTML every lookup must keep working through the permissive
	// element so discovery never regresses.
	base := "https://site.test/"
	urls := run(t, base, `
		var el=document.getElementById('missing');
		el.value='x';
		var qs=document.querySelectorAll('.nope');
		var a=document.querySelector('a[data-x]');
		a.setAttribute('href','/only-dynamic');
		fetch('/api/still-works');
	`)
	if !has(urls, "https://site.test/api/still-works") {
		t.Errorf("fallback broke basic discovery: %v", urls)
	}
	// The permissive fallback element still records dynamic URL assignment.
	if !has(urls, "https://site.test/only-dynamic") {
		t.Errorf("permissive fallback should still record setAttribute href: %v", urls)
	}
}

// urlList flattens intercepted calls into URL strings.
func urlList(calls []NetworkCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.URL)
	}
	return out
}

func TestInnerHTMLParsing(t *testing.T) {
	base := "https://site.test/page"
	urls := run(t, base, `
		var d = document.createElement('div');
		d.innerHTML = '<div foo="beta&amp;gamma">inner <b>bold</b></div>';
		if (d.children.length !== 1) throw new Error('children len ' + d.children.length);
		var c = d.children[0];
		if (c.getAttribute('foo') !== 'beta&gamma') throw new Error('foo attr: ' + c.getAttribute('foo'));
		if (c.tagName !== 'DIV') throw new Error('tag: ' + c.tagName);
		if (d.textContent.indexOf('inner') === -1) throw new Error('text: ' + d.textContent);
		fetch('/api/inner-ok');
	`)
	if !has(urls, "https://site.test/api/inner-ok") {
		t.Errorf("innerHTML children not built: %v", urls)
	}
}

func TestInnerHTMLRecordsResources(t *testing.T) {
	base := "https://site.test/page"
	urls := run(t, base, `
		var d = document.createElement('div');
		d.innerHTML = '<img src="/img/sub.png?v=1"><a href="/nav/no-load">x</a><span style="background:url(/css/img/bg.webp)">y</span>' +
			'<object data="/flash/anim.swf"></object>';
	`)
	if !has(urls, "https://site.test/img/sub.png") {
		t.Errorf("innerHTML src not recorded: %v", urls)
	}
	if !has(urls, "https://site.test/css/img/bg.webp") {
		t.Errorf("innerHTML style url not recorded: %v", urls)
	}
	if !has(urls, "https://site.test/flash/anim.swf") {
		t.Errorf("object data not recorded: %v", urls)
	}
	if has(urls, "/nav/no-load") {
		t.Errorf("a href must not be recorded (no click): %v", urls)
	}
}

// TestVueDecodeEntitiesPattern mirrors vue's compiler trick that failed on
// svoydom.kz: d.innerHTML='<div foo="&quot;...">'; d.children[0].getAttribute('foo').
func TestVueDecodeEntitiesPattern(t *testing.T) {
	base := "https://site.test/page"
	urls := run(t, base, `
		var d = document.createElement('div');
		var val = (function(e, raw){
			if (raw) {
				d.innerHTML = '<div foo="' + e.replace(/"/g, '&quot;') + '">';
				return d.children[0].getAttribute('foo');
			}
			d.innerHTML = e;
			return d.textContent;
		})('&quot;Hello&quot;', true);
		if (val !== '"Hello"') throw new Error('raw decode: ' + val);
		fetch('/api/vue-ok/' + encodeURIComponent(val));
	`)
	if !has(urls, "https://site.test/api/vue-ok/%22Hello%22") {
		t.Errorf("decodeEntities pattern failed: %v", urls)
	}
}

func TestTextContentAssignmentReplacesChildren(t *testing.T) {
	base := "https://site.test/page"
	urls := run(t, base, `
		var d = document.createElement('div');
		d.innerHTML = '<b>old</b>';
		d.textContent = 'plain text';
		if (d.children.length !== 0) throw new Error('children not cleared');
		if (d.textContent !== 'plain text') throw new Error('bad text: ' + d.textContent);
		fetch('/api/text-ok');
	`)
	if !has(urls, "https://site.test/api/text-ok") {
		t.Errorf("textContent assignment failed: %v", urls)
	}
}

// A3: a script that fails because a global (jQuery/$) is defined by a later
// script must be re-run once so dependency-ordered pages still yield findings.
func TestDeferredRerunAfterMissingLib(t *testing.T) {
	base := "https://site.test/page"
	sb := New(base)
	sb.Run([]Script{
		{Code: `if (!window.$) { throw new ReferenceError('$ is not defined'); } fetch('/api/uses-lib');`, URL: "inline:lib-user"},
		{Code: `window.$ = { ready: function(){} };`, URL: "inline:lib"},
		{Code: `var x = window.$; fetch('/api/after-lib2');`, URL: "inline:lib-user2"},
	})
	urls := urlList(sb.Summary().List)
	if !has(urls, "https://site.test/api/uses-lib") {
		t.Errorf("deferred rerun did not rediscover: %v", urls)
	}
	if !has(urls, "https://site.test/api/after-lib2") {
		t.Errorf("second script failed: %v", urls)
	}
	if errs := sb.Summary().ErrorList; len(errs) != 0 {
		t.Errorf("successful rerun must not leave errors: %v", errs)
	}
}

func TestDocumentLocationAlias(t *testing.T) {
	base := "https://svoydom.kz/catalog/"
	// Bitrix-style reads of document.location.* (pull client, translatorjs) must
	// not crash: document.location is aliased to window.location.
	urls := run(t, base, `
		var isSecure = document.location.href.indexOf("https") === 0;
		var parts = document.location.hostname.split('.');
		fetch('/api/was-reached');
	`)
	if !has(urls, "https://svoydom.kz/api/was-reached") {
		t.Errorf("bitrix document.location reads broke execution: %v", urls)
	}
}

func TestEmulationDisabledAfterAbandonments(t *testing.T) {
	prev := abandonedRuns.Load()
	defer abandonedRuns.Store(prev)
	abandonedRuns.Store(3)

	sb := NewWithLimits("https://site.test/", Limits{Timeout: time.Second, MaxAbandoned: 2})
	sb.Run([]Script{{Code: `fetch('/api/never')`, URL: "x.js"}})
	sum := sb.Summary()
	if sum.Scripts != 0 {
		t.Errorf("no scripts should execute when emulation is disabled, got %d", sum.Scripts)
	}
	if len(sum.ErrorList) == 0 {
		t.Error("expected a disabled-emulation error")
	}
}
