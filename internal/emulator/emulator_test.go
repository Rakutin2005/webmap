package emulator

import (
	"strings"
	"testing"
	"time"
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
