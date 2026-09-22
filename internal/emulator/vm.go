package emulator

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// defaultMaxJobs bounds the event loop so runaway timers/intervals cannot hang
// a run. Overridable via Limits.MaxJobs.
const defaultMaxJobs = 2000

type timerJob struct {
	fn   goja.Callable
	args []goja.Value
	seq  int64
}

type gojaVM struct {
	runtime   *goja.Runtime
	sb        *Sandbox
	base      *url.URL
	uid       atomic.Int64
	jobs      []timerJob
	jobSeq    int64
	handlers  map[string][]goja.Callable
	curScript string
	maxJobs   int
	deadline  time.Time
	stopped   *int32
}

func newVM(sb *Sandbox, base string) (*gojaVM, error) {
	runtime := goja.New()
	runtime.SetParserOptions(parser.WithDisableSourceMaps)

	vm := &gojaVM{
		runtime:  runtime,
		sb:       sb,
		handlers: map[string][]goja.Callable{},
		maxJobs:  defaultMaxJobs,
	}
	if base != "" {
		if u, err := url.Parse(base); err == nil {
			vm.base = u
		}
	}
	vm.uid.Store(0)

	if err := vm.injectGlobals(); err != nil {
		return nil, fmt.Errorf("inject globals: %w", err)
	}
	return vm, nil
}

func (vm *gojaVM) exec(code, scriptURL string) error {
	prev := vm.curScript
	vm.curScript = scriptURL
	defer func() { vm.curScript = prev }()
	_, err := vm.runtime.RunScript(scriptURL, code)
	return err
}

// runLoop drains the timer/callback queue and fires lifecycle events so
// deferred network calls (setTimeout, promise chains, onload handlers) execute.
func (vm *gojaVM) runLoop() {
	vm.dispatchGlobal("readystatechange")
	vm.dispatchGlobal("DOMContentLoaded")
	vm.dispatchGlobal("load")
	vm.dispatchGlobal("pageshow")

	count := 0
	for len(vm.jobs) > 0 && count < vm.maxJobs {
		if vm.timedOut() {
			return
		}
		job := vm.jobs[0]
		vm.jobs = vm.jobs[1:]
		count++
		vm.safeCall(job.fn, job.args...)
	}
}

// timedOut reports whether the per-page execution budget has been exhausted.
func (vm *gojaVM) timedOut() bool {
	if vm.stopped != nil && atomic.LoadInt32(vm.stopped) == 1 {
		return true
	}
	if !vm.deadline.IsZero() && time.Now().After(vm.deadline) {
		return true
	}
	return false
}

func (vm *gojaVM) safeCall(fn goja.Callable, args ...goja.Value) {
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	_, _ = fn(goja.Undefined(), args...)
}

func (vm *gojaVM) enqueue(v goja.Value, args ...goja.Value) {
	if fn, ok := goja.AssertFunction(v); ok {
		vm.jobSeq++
		vm.jobs = append(vm.jobs, timerJob{fn: fn, args: args, seq: vm.jobSeq})
	}
}

func (vm *gojaVM) dispatchGlobal(event string) {
	for _, fn := range vm.handlers[event] {
		vm.safeCall(fn)
	}
}

func (vm *gojaVM) nextID() int64 { return vm.uid.Add(1) }

// resolve turns any raw URL reference into an absolute URL against the page base.
func (vm *gojaVM) resolve(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if vm.base == nil {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return vm.base.ResolveReference(u).String()
}

// record captures a network sink hit with its fully-resolved URL.
func (vm *gojaVM) record(rawURL, method, typ string) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return
	}
	low := strings.ToLower(rawURL)
	for _, p := range []string{"data:", "blob:", "javascript:", "about:", "mailto:", "tel:", "#"} {
		if strings.HasPrefix(low, p) {
			return
		}
	}
	initiator := vm.curScript
	if initiator == "" && vm.base != nil {
		initiator = vm.base.String()
	}
	vm.sb.addCall(NetworkCall{
		URL:       vm.resolve(rawURL),
		RawURL:    rawURL,
		Method:    strings.ToUpper(method),
		Type:      typ,
		Initiator: initiator,
	})
}

func (vm *gojaVM) fn(f func(goja.FunctionCall) goja.Value) goja.Value {
	return vm.runtime.ToValue(f)
}

func (vm *gojaVM) injectGlobals() error {
	r := vm.runtime

	vm.injectConsole()
	vm.injectEncoding()
	vm.injectTimers()
	vm.injectURL()
	vm.injectFetch()
	vm.injectXHR()
	vm.injectWebSocket()
	vm.injectEventSource()
	vm.injectMedia()
	vm.injectNavigator()
	vm.injectDocument()
	vm.injectLocationAndWindow()
	vm.injectStorage()
	vm.injectMisc()

	// self-referential globals commonly probed by framework/bundler runtimes
	g := r.GlobalObject()
	r.Set("window", g)
	r.Set("self", g)
	r.Set("global", g)
	r.Set("globalThis", g)
	r.Set("top", g)
	r.Set("parent", g)
	r.Set("frames", g)
	return nil
}

func (vm *gojaVM) injectConsole() {
	r := vm.runtime
	noop := vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() })
	c := r.NewObject()
	for _, m := range []string{"log", "info", "warn", "error", "debug", "trace", "table", "group", "groupEnd", "dir", "assert", "count", "time", "timeEnd"} {
		c.Set(m, noop)
	}
	r.Set("console", c)
}

func (vm *gojaVM) injectEncoding() {
	r := vm.runtime

	r.Set("atob", vm.fn(func(call goja.FunctionCall) goja.Value {
		s := strings.TrimSpace(call.Argument(0).String())
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if dec, err := enc.DecodeString(s); err == nil {
				return r.ToValue(string(dec))
			}
		}
		return r.ToValue("")
	}))
	r.Set("btoa", vm.fn(func(call goja.FunctionCall) goja.Value {
		return r.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Argument(0).String())))
	}))

	encComponent := vm.fn(func(call goja.FunctionCall) goja.Value {
		s := call.Argument(0).String()
		return r.ToValue(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"))
	})
	decComponent := vm.fn(func(call goja.FunctionCall) goja.Value {
		s := call.Argument(0).String()
		if out, err := url.QueryUnescape(strings.ReplaceAll(s, "+", "%2B")); err == nil {
			return r.ToValue(out)
		}
		return r.ToValue(s)
	})
	r.Set("encodeURIComponent", encComponent)
	r.Set("decodeURIComponent", decComponent)
	// encodeURI/decodeURI are looser; the component versions are close enough
	// for URL reconstruction purposes.
	r.Set("encodeURI", encComponent)
	r.Set("decodeURI", decComponent)
	r.Set("escape", encComponent)
	r.Set("unescape", decComponent)

	textEncoder := func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("encode", vm.fn(func(c goja.FunctionCall) goja.Value {
			return rt.ToValue([]byte(c.Argument(0).String()))
		}))
		o.Set("encoding", "utf-8")
		return o
	}
	textDecoder := func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("decode", vm.fn(func(c goja.FunctionCall) goja.Value {
			arg := c.Argument(0)
			if goja.IsUndefined(arg) || goja.IsNull(arg) {
				return rt.ToValue("")
			}
			if b, ok := arg.Export().([]byte); ok {
				return rt.ToValue(string(b))
			}
			return rt.ToValue(arg.String())
		}))
		o.Set("encoding", "utf-8")
		return o
	}
	r.Set("TextEncoder", textEncoder)
	r.Set("TextDecoder", textDecoder)
}

func (vm *gojaVM) injectTimers() {
	r := vm.runtime
	setTimer := vm.fn(func(call goja.FunctionCall) goja.Value {
		var extra []goja.Value
		if len(call.Arguments) > 2 {
			extra = call.Arguments[2:]
		}
		vm.enqueue(call.Argument(0), extra...)
		return r.ToValue(int(vm.nextID()))
	})
	r.Set("setTimeout", setTimer)
	r.Set("setImmediate", setTimer)
	r.Set("queueMicrotask", vm.fn(func(call goja.FunctionCall) goja.Value {
		vm.enqueue(call.Argument(0))
		return goja.Undefined()
	}))
	noop := vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(0) })
	// setInterval fires its callback at most once (never repeats) so polling/
	// animation intervals can't spin the loop; requestAnimationFrame is dropped
	// entirely since animation frames are pure graphics and never yield endpoints.
	r.Set("setInterval", setTimer)
	r.Set("requestAnimationFrame", noop)
	r.Set("clearTimeout", noop)
	r.Set("clearInterval", noop)
	r.Set("clearImmediate", noop)
	r.Set("cancelAnimationFrame", noop)
}

func (vm *gojaVM) injectFetch() {
	r := vm.runtime
	fetchFn := vm.fn(func(call goja.FunctionCall) goja.Value {
		u := vm.argURL(call.Argument(0))
		method := "GET"
		if len(call.Arguments) > 1 {
			if opts := call.Arguments[1].ToObject(r); opts != nil {
				if m := opts.Get("method"); m != nil && !goja.IsUndefined(m) {
					method = m.String()
				}
			}
		}
		vm.record(u, method, "fetch")
		return vm.makeResponsePromise(u)
	})
	r.Set("fetch", fetchFn)

	// Request/Response/Headers constructors so `new Request(url)` and
	// framework wrappers work.
	r.Set("Request", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		u := vm.argURL(call.Argument(0))
		method := "GET"
		if len(call.Arguments) > 1 {
			if opts := call.Arguments[1].ToObject(rt); opts != nil {
				if m := opts.Get("method"); m != nil && !goja.IsUndefined(m) {
					method = m.String()
				}
			}
		}
		vm.record(u, method, "fetch")
		o := rt.NewObject()
		o.Set("url", vm.resolve(u))
		o.Set("method", strings.ToUpper(method))
		return o
	})
	r.Set("Headers", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("append", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("set", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("get", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
		o.Set("has", vm.fn(func(goja.FunctionCall) goja.Value { return rt.ToValue(false) }))
		return o
	})
}

// makeResponsePromise returns a resolved Promise exposing a minimal fetch
// Response so `.then(r => r.json())` chains keep running.
func (vm *gojaVM) makeResponsePromise(u string) goja.Value {
	r := vm.runtime
	resp := r.NewObject()
	resp.Set("ok", true)
	resp.Set("status", 200)
	resp.Set("statusText", "OK")
	resp.Set("url", vm.resolve(u))
	resp.Set("redirected", false)
	resp.Set("type", "basic")
	jsonFn := vm.fn(func(goja.FunctionCall) goja.Value {
		p, res, _ := r.NewPromise()
		res(r.NewObject())
		return r.ToValue(p)
	})
	textFn := vm.fn(func(goja.FunctionCall) goja.Value {
		p, res, _ := r.NewPromise()
		res(r.ToValue(""))
		return r.ToValue(p)
	})
	resp.Set("json", jsonFn)
	resp.Set("text", textFn)
	resp.Set("blob", textFn)
	resp.Set("arrayBuffer", textFn)
	resp.Set("clone", vm.fn(func(goja.FunctionCall) goja.Value { return resp }))

	p, resolve, _ := r.NewPromise()
	resolve(resp)
	return r.ToValue(p)
}

func (vm *gojaVM) injectXHR() {
	r := vm.runtime
	ctor := func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		x := rt.NewObject()
		listeners := map[string][]goja.Callable{}
		x.Set("readyState", 0)
		x.Set("status", 0)
		x.Set("responseText", "")
		x.Set("response", "")
		x.Set("responseType", "")
		x.Set("withCredentials", false)

		x.Set("open", vm.fn(func(c goja.FunctionCall) goja.Value {
			method := "GET"
			if len(c.Arguments) > 0 {
				method = c.Argument(0).String()
			}
			u := vm.argURL(c.Argument(1))
			x.Set("_method", method)
			x.Set("_url", u)
			vm.record(u, method, "xhr")
			return goja.Undefined()
		}))
		x.Set("setRequestHeader", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		x.Set("overrideMimeType", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		x.Set("abort", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		x.Set("getAllResponseHeaders", vm.fn(func(goja.FunctionCall) goja.Value { return rt.ToValue("") }))
		x.Set("getResponseHeader", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
		x.Set("addEventListener", vm.fn(func(c goja.FunctionCall) goja.Value {
			ev := c.Argument(0).String()
			if fn, ok := goja.AssertFunction(c.Argument(1)); ok {
				listeners[ev] = append(listeners[ev], fn)
			}
			return goja.Undefined()
		}))
		x.Set("removeEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		x.Set("send", vm.fn(func(c goja.FunctionCall) goja.Value {
			// Simulate a completed request so onload/onreadystatechange chains
			// that issue further first-order calls get a chance to run.
			x.Set("readyState", 4)
			x.Set("status", 200)
			fire := func() {
				if h := x.Get("onreadystatechange"); h != nil {
					vm.enqueue(h)
				}
				if h := x.Get("onload"); h != nil {
					vm.enqueue(h)
				}
				for _, fn := range listeners["load"] {
					vm.jobSeq++
					vm.jobs = append(vm.jobs, timerJob{fn: fn, seq: vm.jobSeq})
				}
				for _, fn := range listeners["readystatechange"] {
					vm.jobSeq++
					vm.jobs = append(vm.jobs, timerJob{fn: fn, seq: vm.jobSeq})
				}
			}
			fire()
			return goja.Undefined()
		}))
		return x
	}
	r.Set("XMLHttpRequest", ctor)
	r.Set("ActiveXObject", ctor) // legacy IE AJAX
}

func (vm *gojaVM) injectWebSocket() {
	r := vm.runtime
	r.Set("WebSocket", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		u := vm.argURL(call.Argument(0))
		vm.record(u, "WS", "websocket")
		o := rt.NewObject()
		o.Set("url", vm.resolve(u))
		o.Set("readyState", 0)
		o.Set("send", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("close", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("addEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		return o
	})
}

func (vm *gojaVM) injectEventSource() {
	r := vm.runtime
	r.Set("EventSource", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		u := vm.argURL(call.Argument(0))
		vm.record(u, "GET", "eventsource")
		o := rt.NewObject()
		o.Set("url", vm.resolve(u))
		o.Set("readyState", 0)
		o.Set("close", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("addEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		return o
	})
}

func (vm *gojaVM) injectMedia() {
	r := vm.runtime
	mk := func(typ string) func(goja.ConstructorCall, *goja.Runtime) *goja.Object {
		return func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
			o := rt.NewObject()
			if len(call.Arguments) > 0 {
				u := call.Argument(0).String()
				o.Set("src", vm.resolve(u))
				vm.record(u, "GET", typ)
			}
			vm.defineURLSetter(o, "src", typ)
			o.Set("width", 0)
			o.Set("height", 0)
			o.Set("addEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
			return o
		}
	}
	r.Set("Image", mk("image"))
	r.Set("Audio", mk("media"))
}

func (vm *gojaVM) injectNavigator() {
	r := vm.runtime
	nav := r.NewObject()
	nav.Set("userAgent", "Mozilla/5.0 (compatible; WebMapEmu/1.0)")
	nav.Set("platform", "Linux x86_64")
	nav.Set("language", "en-US")
	nav.Set("languages", r.ToValue([]string{"en-US", "en"}))
	nav.Set("onLine", true)
	nav.Set("cookieEnabled", true)
	nav.Set("doNotTrack", "1")
	nav.Set("sendBeacon", vm.fn(func(call goja.FunctionCall) goja.Value {
		u := vm.argURL(call.Argument(0))
		vm.record(u, "POST", "beacon")
		return r.ToValue(true)
	}))
	nav.Set("serviceWorker", func() *goja.Object {
		sw := r.NewObject()
		sw.Set("register", vm.fn(func(call goja.FunctionCall) goja.Value {
			u := vm.argURL(call.Argument(0))
			vm.record(u, "GET", "script")
			p, res, _ := r.NewPromise()
			res(r.NewObject())
			return r.ToValue(p)
		}))
		return sw
	}())
	r.Set("navigator", nav)
}

func (vm *gojaVM) injectDocument() {
	r := vm.runtime
	doc := r.NewObject()

	doc.Set("createElement", vm.fn(func(call goja.FunctionCall) goja.Value {
		tag := strings.ToLower(call.Argument(0).String())
		return vm.newElement(tag)
	}))
	doc.Set("createElementNS", vm.fn(func(call goja.FunctionCall) goja.Value {
		tag := strings.ToLower(call.Argument(1).String())
		return vm.newElement(tag)
	}))
	doc.Set("createTextNode", vm.fn(func(goja.FunctionCall) goja.Value { return r.NewObject() }))
	doc.Set("createComment", vm.fn(func(goja.FunctionCall) goja.Value { return r.NewObject() }))
	doc.Set("createDocumentFragment", vm.fn(func(goja.FunctionCall) goja.Value { return vm.newElement("fragment") }))

	emptyArr := vm.fn(func(goja.FunctionCall) goja.Value { return r.NewArray() })
	// Return a permissive element rather than null so that init code guarded by
	// `if (el)` proceeds far enough to register its handlers and issue calls.
	elemFn := vm.fn(func(goja.FunctionCall) goja.Value { return vm.newElement("div") })
	doc.Set("getElementById", elemFn)
	doc.Set("querySelector", elemFn)
	doc.Set("querySelectorAll", emptyArr)
	doc.Set("getElementsByTagName", emptyArr)
	doc.Set("getElementsByClassName", emptyArr)
	doc.Set("getElementsByName", emptyArr)
	doc.Set("addEventListener", vm.fn(func(call goja.FunctionCall) goja.Value {
		ev := call.Argument(0).String()
		if fn, ok := goja.AssertFunction(call.Argument(1)); ok {
			vm.handlers[ev] = append(vm.handlers[ev], fn)
		}
		return goja.Undefined()
	}))
	doc.Set("removeEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	doc.Set("dispatchEvent", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(true) }))
	doc.Set("write", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	body := vm.newElement("body")
	docEl := vm.newElement("html")
	doc.Set("body", body)
	doc.Set("head", vm.newElement("head"))
	doc.Set("documentElement", docEl)
	doc.Set("scrollingElement", docEl)
	doc.Set("currentScript", vm.newElement("script"))
	doc.Set("activeElement", body)
	doc.Set("cookie", "")
	doc.Set("title", "")
	doc.Set("referrer", "")
	doc.Set("readyState", "loading")
	doc.Set("characterSet", "UTF-8")
	doc.Set("compatMode", "CSS1Compat")
	// nodeType 9 (DOCUMENT_NODE) is required by Sizzle/jQuery's setDocument to
	// accept our document; without it jQuery aborts before initializing.
	doc.Set("nodeType", 9)
	doc.Set("nodeName", "#document")
	doc.Set("defaultView", r.GlobalObject())
	doc.Set("forms", r.NewArray())
	doc.Set("images", r.NewArray())
	doc.Set("links", r.NewArray())
	doc.Set("scripts", r.NewArray())
	doc.Set("styleSheets", r.NewArray())
	impl := r.NewObject()
	impl.Set("createHTMLDocument", vm.fn(func(goja.FunctionCall) goja.Value { return doc }))
	impl.Set("hasFeature", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(true) }))
	doc.Set("implementation", impl)
	doc.Set("createRange", vm.fn(func(goja.FunctionCall) goja.Value {
		rng := r.NewObject()
		rng.Set("setStart", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		rng.Set("setEnd", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		rng.Set("createContextualFragment", vm.fn(func(goja.FunctionCall) goja.Value { return vm.newElement("fragment") }))
		rng.Set("selectNodeContents", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		return rng
	}))
	doc.Set("getSelection", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
	doc.Set("elementFromPoint", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
	doc.Set("execCommand", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(false) }))
	doc.Set("contains", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(false) }))
	if vm.base != nil {
		doc.Set("domain", vm.base.Hostname())
		doc.Set("URL", vm.base.String())
		doc.Set("baseURI", vm.base.String())
	}
	r.Set("document", doc)
}

func (vm *gojaVM) injectLocationAndWindow() {
	r := vm.runtime
	loc := r.NewObject()
	href, origin, proto, host, hostname, path := "about:blank", "null", "about:", "", "", "/"
	if vm.base != nil {
		href = vm.base.String()
		proto = vm.base.Scheme + ":"
		host = vm.base.Host
		hostname = vm.base.Hostname()
		origin = vm.base.Scheme + "://" + vm.base.Host
		if vm.base.Path != "" {
			path = vm.base.Path
		}
	}
	loc.Set("href", href)
	loc.Set("origin", origin)
	loc.Set("protocol", proto)
	loc.Set("host", host)
	loc.Set("hostname", hostname)
	loc.Set("pathname", path)
	loc.Set("search", "")
	loc.Set("hash", "")
	loc.Set("port", "")
	loc.Set("assign", vm.fn(func(call goja.FunctionCall) goja.Value {
		vm.record(vm.argURL(call.Argument(0)), "GET", "navigation")
		return goja.Undefined()
	}))
	loc.Set("replace", vm.fn(func(call goja.FunctionCall) goja.Value {
		vm.record(vm.argURL(call.Argument(0)), "GET", "navigation")
		return goja.Undefined()
	}))
	loc.Set("reload", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	loc.Set("toString", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(href) }))
	r.Set("location", loc)

	hist := r.NewObject()
	hist.Set("pushState", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	hist.Set("replaceState", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	hist.Set("go", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	hist.Set("back", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	hist.Set("forward", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	r.Set("history", hist)

	r.Set("addEventListener", vm.fn(func(call goja.FunctionCall) goja.Value {
		ev := call.Argument(0).String()
		if fn, ok := goja.AssertFunction(call.Argument(1)); ok {
			vm.handlers[ev] = append(vm.handlers[ev], fn)
		}
		return goja.Undefined()
	}))
	r.Set("removeEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	r.Set("dispatchEvent", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(true) }))
	r.Set("open", vm.fn(func(call goja.FunctionCall) goja.Value {
		vm.record(vm.argURL(call.Argument(0)), "GET", "navigation")
		return goja.Null()
	}))
	r.Set("matchMedia", vm.fn(func(goja.FunctionCall) goja.Value {
		o := r.NewObject()
		o.Set("matches", false)
		o.Set("addListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("addEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		return o
	}))
	scr := r.NewObject()
	scr.Set("width", 1920)
	scr.Set("height", 1080)
	r.Set("screen", scr)
}

func (vm *gojaVM) injectStorage() {
	r := vm.runtime
	mk := func() *goja.Object {
		o := r.NewObject()
		store := map[string]string{}
		o.Set("getItem", vm.fn(func(c goja.FunctionCall) goja.Value {
			if v, ok := store[c.Argument(0).String()]; ok {
				return r.ToValue(v)
			}
			return goja.Null()
		}))
		o.Set("setItem", vm.fn(func(c goja.FunctionCall) goja.Value {
			store[c.Argument(0).String()] = c.Argument(1).String()
			return goja.Undefined()
		}))
		o.Set("removeItem", vm.fn(func(c goja.FunctionCall) goja.Value {
			delete(store, c.Argument(0).String())
			return goja.Undefined()
		}))
		o.Set("clear", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("key", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
		o.Set("length", 0)
		return o
	}
	r.Set("localStorage", mk())
	r.Set("sessionStorage", mk())
}

func (vm *gojaVM) injectMisc() {
	r := vm.runtime
	// importScripts (web workers) pulls further scripts — record them.
	r.Set("importScripts", vm.fn(func(call goja.FunctionCall) goja.Value {
		for _, a := range call.Arguments {
			vm.record(a.String(), "GET", "script")
		}
		return goja.Undefined()
	}))
	// AbortController/AbortSignal so fetch({signal}) code runs.
	r.Set("AbortController", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		ctrl := rt.NewObject()
		sig := rt.NewObject()
		sig.Set("aborted", false)
		sig.Set("addEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		sig.Set("removeEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		sig.Set("throwIfAborted", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		ctrl.Set("signal", sig)
		ctrl.Set("abort", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		return ctrl
	})
	// FormData/Blob minimal stubs so request-building code runs.
	r.Set("FormData", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("append", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("set", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("get", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
		return o
	})
	r.Set("Blob", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("size", 0)
		o.Set("type", "")
		return o
	})
	r.Set("getComputedStyle", vm.fn(func(goja.FunctionCall) goja.Value {
		s := r.NewObject()
		s.Set("getPropertyValue", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue("") }))
		return s
	}))

	// DOM interface constructors that libraries reference for `instanceof`
	// checks and feature detection. They only need to exist.
	emptyCtor := func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object { return rt.NewObject() }
	for _, name := range []string{
		"Node", "Element", "HTMLElement", "HTMLDocument", "Document", "DocumentFragment",
		"HTMLDivElement", "HTMLScriptElement", "HTMLAnchorElement", "HTMLImageElement",
		"HTMLInputElement", "HTMLFormElement", "HTMLIFrameElement", "SVGElement",
		"Event", "CustomEvent", "MouseEvent", "KeyboardEvent", "PointerEvent",
		"Text", "Comment", "NodeList", "HTMLCollection", "DOMParser", "XPathEvaluator",
		"FileReader", "File", "MessageChannel", "Worker",
	} {
		r.Set(name, emptyCtor)
	}

	// Observer constructors — return an object with the standard no-op methods.
	observerCtor := func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("observe", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("unobserve", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("disconnect", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("takeRecords", vm.fn(func(goja.FunctionCall) goja.Value { return rt.NewArray() }))
		return o
	}
	r.Set("MutationObserver", observerCtor)
	r.Set("IntersectionObserver", observerCtor)
	r.Set("ResizeObserver", observerCtor)
	r.Set("PerformanceObserver", observerCtor)

	// Intl — some bundles (Bitrix popup) call Intl.* during init.
	intl := r.NewObject()
	intlSub := func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("format", vm.fn(func(c goja.FunctionCall) goja.Value { return rt.ToValue(c.Argument(0).String()) }))
		o.Set("formatToParts", vm.fn(func(goja.FunctionCall) goja.Value { return rt.NewArray() }))
		o.Set("resolvedOptions", vm.fn(func(goja.FunctionCall) goja.Value { return rt.NewObject() }))
		return o
	}
	intl.Set("DateTimeFormat", intlSub)
	intl.Set("NumberFormat", intlSub)
	intl.Set("Collator", intlSub)
	intl.Set("PluralRules", intlSub)
	intl.Set("RelativeTimeFormat", intlSub)
	r.Set("Intl", intl)

	perf := r.NewObject()
	perf.Set("now", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(0) }))
	perf.Set("mark", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	perf.Set("measure", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	perf.Set("getEntriesByType", vm.fn(func(goja.FunctionCall) goja.Value { return r.NewArray() }))
	perf.Set("getEntriesByName", vm.fn(func(goja.FunctionCall) goja.Value { return r.NewArray() }))
	perf.Set("timing", r.NewObject())
	r.Set("performance", perf)
	r.Set("requestIdleCallback", vm.fn(func(call goja.FunctionCall) goja.Value {
		vm.enqueue(call.Argument(0))
		return r.ToValue(0)
	}))
	r.Set("cancelIdleCallback", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))

	// crypto — many analytics/uuid libs require getRandomValues before they run.
	crypto := r.NewObject()
	crypto.Set("getRandomValues", vm.fn(func(call goja.FunctionCall) goja.Value {
		arg := call.Argument(0)
		if obj, ok := arg.(*goja.Object); ok {
			n := 0
			if l := obj.Get("length"); l != nil && !goja.IsUndefined(l) {
				n = int(l.ToInteger())
			}
			for i := 0; i < n; i++ {
				seed := vm.nextID()
				obj.Set(fmt.Sprintf("%d", i), int((seed*1103515245+12345)&0xff))
			}
		}
		return arg
	}))
	crypto.Set("randomUUID", vm.fn(func(goja.FunctionCall) goja.Value {
		id := vm.nextID()
		return r.ToValue(fmt.Sprintf("00000000-0000-4000-8000-%012d", id))
	}))
	subtle := r.NewObject()
	subtle.Set("digest", vm.fn(func(goja.FunctionCall) goja.Value {
		p, res, _ := r.NewPromise()
		res(r.NewArray())
		return r.ToValue(p)
	}))
	crypto.Set("subtle", subtle)
	r.Set("crypto", crypto)
	r.Set("msCrypto", crypto)

	// customElements registry stub.
	ce := r.NewObject()
	ce.Set("define", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	ce.Set("get", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	ce.Set("whenDefined", vm.fn(func(goja.FunctionCall) goja.Value {
		p, res, _ := r.NewPromise()
		res(goja.Undefined())
		return r.ToValue(p)
	}))
	ce.Set("upgrade", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	r.Set("customElements", ce)

	// WeakRef/FinalizationRegistry: goja lacks these; several modern bundles
	// reference them during init.
	r.Set("WeakRef", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		held := call.Argument(0)
		o.Set("deref", vm.fn(func(goja.FunctionCall) goja.Value { return held }))
		return o
	})
	r.Set("FinalizationRegistry", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		o := rt.NewObject()
		o.Set("register", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		o.Set("unregister", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
		return o
	})
}

// newElement builds a DOM element whose URL-bearing attributes record a network
// call when assigned, covering dynamic <script>/<img>/<iframe>/<link> injection.
func (vm *gojaVM) newElement(tag string) goja.Value {
	r := vm.runtime
	el := r.NewObject()
	el.Set("tagName", strings.ToUpper(tag))
	el.Set("nodeName", strings.ToUpper(tag))
	el.Set("nodeType", 1)
	el.Set("className", "")
	el.Set("id", "")
	el.Set("innerHTML", "")
	el.Set("innerText", "")
	el.Set("textContent", "")
	el.Set("value", "")
	style := r.NewObject()
	style.Set("setProperty", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	style.Set("removeProperty", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue("") }))
	style.Set("getPropertyValue", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue("") }))
	style.Set("item", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue("") }))
	style.Set("cssText", "")
	el.Set("style", style)
	el.Set("dataset", r.NewObject())
	el.Set("children", r.NewArray())
	el.Set("childNodes", r.NewArray())

	typ := elementResourceType(tag)
	vm.defineURLSetter(el, "src", typ)
	vm.defineURLSetter(el, "href", typ)
	vm.defineURLSetter(el, "action", "form")
	vm.defineURLSetter(el, "data", typ)

	el.Set("setAttribute", vm.fn(func(call goja.FunctionCall) goja.Value {
		name := strings.ToLower(call.Argument(0).String())
		val := call.Argument(1).String()
		switch name {
		case "src":
			vm.record(val, "GET", typ)
			el.Set("src", vm.resolve(val))
		case "href":
			vm.record(val, "GET", typ)
			el.Set("href", vm.resolve(val))
		case "action":
			vm.record(val, "POST", "form")
			el.Set("action", vm.resolve(val))
		case "data":
			vm.record(val, "GET", typ)
			el.Set("data", vm.resolve(val))
		default:
			el.Set(name, val)
		}
		return goja.Undefined()
	}))
	el.Set("getAttribute", vm.fn(func(call goja.FunctionCall) goja.Value {
		name := strings.ToLower(call.Argument(0).String())
		if v := el.Get(name); v != nil {
			return v
		}
		return goja.Null()
	}))
	el.Set("setAttributeNS", vm.fn(func(call goja.FunctionCall) goja.Value {
		name := strings.ToLower(call.Argument(1).String())
		val := call.Argument(2).String()
		if name == "href" || name == "xlink:href" || name == "src" {
			vm.record(val, "GET", typ)
		}
		return goja.Undefined()
	}))
	// Minimal but real child/sibling tracking so DOM-probing library init
	// (e.g. jQuery's cloneNode/lastChild support tests) works. Sibling chains
	// stay finite (last.nextSibling == null) so traversal loops terminate.
	var children []*goja.Object
	sync := func() {
		arr := r.NewArray()
		for i, c := range children {
			arr.Set(fmt.Sprintf("%d", i), c)
		}
		arr.Set("length", len(children))
		el.Set("childNodes", arr)
		el.Set("children", arr)
		if len(children) > 0 {
			el.Set("firstChild", children[0])
			el.Set("firstElementChild", children[0])
			el.Set("lastChild", children[len(children)-1])
			el.Set("lastElementChild", children[len(children)-1])
		} else {
			el.Set("firstChild", goja.Null())
			el.Set("lastChild", goja.Null())
		}
	}
	adopt := func(v goja.Value) {
		child, ok := v.(*goja.Object)
		if !ok {
			return
		}
		if len(children) > 0 {
			prev := children[len(children)-1]
			prev.Set("nextSibling", child)
			child.Set("previousSibling", prev)
		} else {
			child.Set("previousSibling", goja.Null())
		}
		child.Set("nextSibling", goja.Null())
		child.Set("parentNode", el)
		child.Set("parentElement", el)
		children = append(children, child)
		sync()
	}
	el.Set("appendChild", vm.fn(func(call goja.FunctionCall) goja.Value {
		adopt(call.Argument(0))
		return call.Argument(0)
	}))
	el.Set("append", vm.fn(func(call goja.FunctionCall) goja.Value {
		for _, a := range call.Arguments {
			adopt(a)
		}
		return goja.Undefined()
	}))
	el.Set("insertBefore", vm.fn(func(call goja.FunctionCall) goja.Value {
		adopt(call.Argument(0))
		return call.Argument(0)
	}))
	el.Set("removeChild", vm.fn(func(call goja.FunctionCall) goja.Value {
		target, ok := call.Argument(0).(*goja.Object)
		if ok {
			out := children[:0]
			for _, c := range children {
				if c != target {
					out = append(out, c)
				}
			}
			children = out
			sync()
		}
		return call.Argument(0)
	}))
	el.Set("remove", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	el.Set("addEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	el.Set("removeEventListener", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	el.Set("querySelector", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
	el.Set("querySelectorAll", vm.fn(func(goja.FunctionCall) goja.Value { return r.NewArray() }))
	el.Set("getElementsByTagName", vm.fn(func(goja.FunctionCall) goja.Value { return r.NewArray() }))
	el.Set("submit", vm.fn(func(goja.FunctionCall) goja.Value {
		if a := el.Get("action"); a != nil && !goja.IsUndefined(a) && !goja.IsNull(a) {
			vm.record(a.String(), "POST", "form")
		}
		return goja.Undefined()
	}))
	el.Set("cloneNode", vm.fn(func(call goja.FunctionCall) goja.Value {
		clone := vm.newElement(tag)
		co := clone.(*goja.Object)
		// Copy primitive own-properties (type, value, checked, className, ...);
		// skip functions/objects so the clone keeps its own live methods.
		for _, k := range el.Keys() {
			v := el.Get(k)
			if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
				continue
			}
			if _, isFn := goja.AssertFunction(v); isFn {
				continue
			}
			switch v.ExportType().Kind().String() {
			case "string", "bool", "int64", "float64", "int":
				co.Set(k, v)
			}
		}
		deep := len(call.Arguments) > 0 && call.Argument(0).ToBoolean()
		if deep {
			if ac, ok := goja.AssertFunction(co.Get("appendChild")); ok {
				for _, child := range children {
					if cf, ok2 := goja.AssertFunction(child.Get("cloneNode")); ok2 {
						cc, _ := cf(child, r.ToValue(true))
						if cc != nil {
							ac(co, cc)
						}
					}
				}
			}
		}
		return clone
	}))
	el.Set("contains", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(false) }))
	el.Set("checked", false)
	el.Set("selected", false)
	el.Set("disabled", false)
	el.Set("readOnly", false)
	el.Set("nodeValue", goja.Null())
	el.Set("name", "")
	if tag == "input" || tag == "button" {
		el.Set("type", "text")
	} else {
		el.Set("type", "")
	}
	el.Set("options", r.NewArray())
	el.Set("files", r.NewArray())

	// Geometry/interaction stubs so layout-probing library init survives.
	rect := vm.fn(func(goja.FunctionCall) goja.Value {
		o := r.NewObject()
		for _, k := range []string{"top", "right", "bottom", "left", "width", "height", "x", "y"} {
			o.Set(k, 0)
		}
		return o
	})
	el.Set("getBoundingClientRect", rect)
	el.Set("getClientRects", vm.fn(func(goja.FunctionCall) goja.Value { return r.NewArray() }))
	for _, m := range []string{"focus", "blur", "click", "scrollIntoView", "scroll", "scrollTo", "insertAdjacentHTML", "insertAdjacentElement", "insertAdjacentText", "normalize", "before", "after", "replaceWith", "replaceChild", "setSelectionRange", "select", "requestFullscreen"} {
		el.Set(m, vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	}
	el.Set("matches", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(false) }))
	el.Set("closest", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
	el.Set("hasAttribute", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(false) }))
	el.Set("removeAttribute", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }))
	el.Set("getContext", vm.fn(func(goja.FunctionCall) goja.Value { return goja.Null() }))
	el.Set("parentNode", goja.Null())
	el.Set("parentElement", goja.Null())
	el.Set("nextSibling", goja.Null())
	el.Set("previousSibling", goja.Null())
	el.Set("firstChild", goja.Null())
	el.Set("lastChild", goja.Null())
	el.Set("firstElementChild", goja.Null())
	el.Set("nextElementSibling", goja.Null())
	el.Set("offsetParent", goja.Null())
	el.Set("offsetWidth", 0)
	el.Set("offsetHeight", 0)
	el.Set("offsetTop", 0)
	el.Set("offsetLeft", 0)
	el.Set("clientWidth", 0)
	el.Set("clientHeight", 0)
	el.Set("scrollWidth", 0)
	el.Set("scrollHeight", 0)
	el.Set("scrollTop", 0)
	el.Set("attributes", r.NewArray())
	el.Set("ownerDocument", r.Get("document"))
	el.Set("classList", func() *goja.Object {
		cl := r.NewObject()
		noop := vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() })
		cl.Set("add", noop)
		cl.Set("remove", noop)
		cl.Set("toggle", noop)
		cl.Set("contains", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(false) }))
		return cl
	}())
	return el
}

// defineURLSetter installs an accessor property that records a network call
// whenever a URL-shaped value is assigned to it (e.g. script.src = url).
func (vm *gojaVM) defineURLSetter(o *goja.Object, prop, typ string) {
	stored := ""
	getter := vm.fn(func(goja.FunctionCall) goja.Value { return vm.runtime.ToValue(stored) })
	method := "GET"
	if prop == "action" {
		method = "POST"
	}
	setter := vm.fn(func(call goja.FunctionCall) goja.Value {
		raw := call.Argument(0).String()
		stored = vm.resolve(raw)
		vm.record(raw, method, typ)
		return goja.Undefined()
	})
	_ = o.DefineAccessorProperty(prop, getter, setter, goja.FLAG_TRUE, goja.FLAG_TRUE)
}

func elementResourceType(tag string) string {
	switch tag {
	case "script":
		return "script"
	case "img", "image":
		return "image"
	case "iframe", "frame":
		return "iframe"
	case "link":
		return "link"
	case "video", "audio", "source", "track":
		return "media"
	case "a", "area":
		return "link"
	case "object", "embed":
		return "object"
	default:
		return "other"
	}
}

// argURL extracts a URL string from a fetch/XHR argument that may be a string,
// a URL object, or a Request-like object with a .url property.
func (vm *gojaVM) argURL(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	if obj, ok := v.(*goja.Object); ok {
		if u := obj.Get("url"); u != nil && !goja.IsUndefined(u) && !goja.IsNull(u) {
			return u.String()
		}
		if u := obj.Get("href"); u != nil && !goja.IsUndefined(u) && !goja.IsNull(u) {
			return u.String()
		}
	}
	return v.String()
}
