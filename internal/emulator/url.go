package emulator

import (
	"net/url"
	"strings"

	"github.com/dop251/goja"
)

type kv struct{ key, val string }

// injectURL provides WHATWG URL and URLSearchParams so code that builds
// endpoints via these APIs (very common in modern SPAs) resolves correctly.
func (vm *gojaVM) injectURL() {
	r := vm.runtime

	r.Set("URLSearchParams", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		init := ""
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			init = call.Arguments[0].String()
		}
		obj, _ := vm.newSearchParams(init)
		return obj
	})

	r.Set("URL", func(call goja.ConstructorCall, rt *goja.Runtime) *goja.Object {
		arg := ""
		if len(call.Arguments) > 0 {
			arg = call.Arguments[0].String()
		}
		var u *url.URL
		var err error
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			if b, e := url.Parse(call.Arguments[1].String()); e == nil {
				u, err = b.Parse(arg)
			}
		}
		if u == nil {
			if vm.base != nil {
				u, err = vm.base.Parse(arg)
			} else {
				u, err = url.Parse(arg)
			}
		}
		if err != nil || u == nil {
			u = &url.URL{Path: arg}
		}
		return vm.newURLObject(u)
	})
}

// parseQuery decodes a query string ("?a=1&b=2") into an ordered key/value
// list. Used both to build a URLSearchParams and to repopulate the shared list
// when a URL's search component is reassigned.
func parseQuery(init string) []kv {
	init = strings.TrimPrefix(init, "?")
	var pairs []kv
	if init != "" {
		for _, part := range strings.Split(init, "&") {
			if part == "" {
				continue
			}
			k, v, _ := strings.Cut(part, "=")
			k = jsUnescape(k)
			v = jsUnescape(v)
			pairs = append(pairs, kv{k, v})
		}
	}
	return pairs
}

// newSearchParams returns a goja object backed by an ordered key/value list,
// plus a pointer to that list so an owning URL can rebuild its query string.
func (vm *gojaVM) newSearchParams(init string) (*goja.Object, *[]kv) {
	r := vm.runtime
	plist := parseQuery(init)
	pairs := &plist

	o := r.NewObject()
	o.Set("append", vm.fn(func(c goja.FunctionCall) goja.Value {
		*pairs = append(*pairs, kv{c.Argument(0).String(), c.Argument(1).String()})
		return goja.Undefined()
	}))
	// Serializer so passing this URLSearchParams as a fetch/Request body
	// compiles to its wire encoding during capture.
	vm.serializers[o] = func() string { return encodePairs(*pairs) }
	o.Set("set", vm.fn(func(c goja.FunctionCall) goja.Value {
		k, v := c.Argument(0).String(), c.Argument(1).String()
		found := false
		out := (*pairs)[:0]
		for _, p := range *pairs {
			if p.key == k {
				if !found {
					out = append(out, kv{k, v})
					found = true
				}
				continue
			}
			out = append(out, p)
		}
		if !found {
			out = append(out, kv{k, v})
		}
		*pairs = out
		return goja.Undefined()
	}))
	o.Set("get", vm.fn(func(c goja.FunctionCall) goja.Value {
		k := c.Argument(0).String()
		for _, p := range *pairs {
			if p.key == k {
				return r.ToValue(p.val)
			}
		}
		return goja.Null()
	}))
	o.Set("getAll", vm.fn(func(c goja.FunctionCall) goja.Value {
		k := c.Argument(0).String()
		var vals []string
		for _, p := range *pairs {
			if p.key == k {
				vals = append(vals, p.val)
			}
		}
		return r.ToValue(vals)
	}))
	o.Set("has", vm.fn(func(c goja.FunctionCall) goja.Value {
		k := c.Argument(0).String()
		for _, p := range *pairs {
			if p.key == k {
				return r.ToValue(true)
			}
		}
		return r.ToValue(false)
	}))
	o.Set("delete", vm.fn(func(c goja.FunctionCall) goja.Value {
		k := c.Argument(0).String()
		out := (*pairs)[:0]
		for _, p := range *pairs {
			if p.key != k {
				out = append(out, p)
			}
		}
		*pairs = out
		return goja.Undefined()
	}))
	o.Set("forEach", vm.fn(func(c goja.FunctionCall) goja.Value {
		if fn, ok := goja.AssertFunction(c.Argument(0)); ok {
			for _, p := range *pairs {
				vm.safeCall(fn, r.ToValue(p.val), r.ToValue(p.key))
			}
		}
		return goja.Undefined()
	}))
	toString := vm.fn(func(goja.FunctionCall) goja.Value {
		return r.ToValue(encodePairs(*pairs))
	})
	o.Set("toString", toString)
	return o, pairs
}

func (vm *gojaVM) newURLObject(u *url.URL) *goja.Object {
	r := vm.runtime
	spObj, pairs := vm.newSearchParams(u.RawQuery)

	build := func() string {
		out := &url.URL{
			Scheme:   u.Scheme,
			Host:     u.Host,
			Path:     u.Path,
			Fragment: u.Fragment,
			RawQuery: encodePairs(*pairs),
		}
		return out.String()
	}

	o := r.NewObject()

	// Accessor helper: getter reads the current value, setter mutates shared
	// state so `u.pathname = '/x'` / `u.href = ...` actually take effect
	// (previously they were silent no-ops and fetches used a stale URL).
	def := func(name string, get func() string, set func(string)) {
		_ = o.DefineAccessorProperty(name,
			vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(get()) }),
			vm.fn(func(call goja.FunctionCall) goja.Value {
				set(call.Argument(0).String())
				return goja.Undefined()
			}),
			goja.FLAG_TRUE, goja.FLAG_TRUE)
	}

	def("href", func() string {
		return build()
	}, func(v string) {
		var nu *url.URL
		var err error
		if strings.Contains(v, "://") || strings.HasPrefix(v, "//") {
			nu, err = url.Parse(v)
		} else {
			if bu, e := url.Parse(build()); e == nil {
				nu, err = bu.Parse(v)
			}
		}
		if err == nil && nu != nil {
			*u = *nu
		}
		*pairs = parseQuery(u.RawQuery)
	})
	def("search", func() string {
		q := encodePairs(*pairs)
		if q != "" {
			return "?" + q
		}
		return ""
	}, func(v string) {
		*pairs = parseQuery(v)
		u.RawQuery = strings.TrimPrefix(v, "?")
	})
	def("protocol", func() string { return u.Scheme + ":" }, func(v string) {
		u.Scheme = strings.TrimSuffix(v, ":")
	})
	def("host", func() string { return u.Host }, func(v string) { u.Host = v })
	def("hostname", func() string { return u.Hostname() }, func(v string) {
		port := u.Port()
		v = strings.TrimSuffix(v, ":")
		if port != "" {
			u.Host = v + ":" + port
		} else {
			u.Host = v
		}
	})
	def("port", func() string { return u.Port() }, func(v string) {
		if v == "" {
			u.Host = u.Hostname()
		} else {
			u.Host = u.Hostname() + ":" + v
		}
	})
	def("pathname", func() string { return u.Path }, func(v string) { u.Path = v })
	def("hash", func() string {
		if u.Fragment != "" {
			return "#" + u.Fragment
		}
		return ""
	}, func(v string) { u.Fragment = strings.TrimPrefix(v, "#") })
	def("origin", func() string {
		if u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
		return "null"
	}, func(string) {})

	o.Set("searchParams", spObj)
	o.Set("toString", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(build()) }))
	o.Set("toJSON", vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(build()) }))
	return o
}

func encodePairs(pairs []kv) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(jsEscape(p.key))
		b.WriteByte('=')
		b.WriteString(jsEscape(p.val))
	}
	return b.String()
}

func jsEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func jsUnescape(s string) string {
	if out, err := url.QueryUnescape(s); err == nil {
		return out
	}
	return s
}
