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

// newSearchParams returns a goja object backed by an ordered key/value list,
// plus a pointer to that list so an owning URL can rebuild its query string.
func (vm *gojaVM) newSearchParams(init string) (*goja.Object, *[]kv) {
	r := vm.runtime
	pairs := &[]kv{}
	init = strings.TrimPrefix(init, "?")
	if init != "" {
		for _, part := range strings.Split(init, "&") {
			if part == "" {
				continue
			}
			k, v, _ := strings.Cut(part, "=")
			k = jsUnescape(k)
			v = jsUnescape(v)
			*pairs = append(*pairs, kv{k, v})
		}
	}

	o := r.NewObject()
	o.Set("append", vm.fn(func(c goja.FunctionCall) goja.Value {
		*pairs = append(*pairs, kv{c.Argument(0).String(), c.Argument(1).String()})
		return goja.Undefined()
	}))
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
	o.Set("protocol", u.Scheme+":")
	o.Set("hostname", u.Hostname())
	o.Set("host", u.Host)
	o.Set("port", u.Port())
	o.Set("pathname", u.Path)
	o.Set("hash", func() string {
		if u.Fragment != "" {
			return "#" + u.Fragment
		}
		return ""
	}())
	o.Set("origin", u.Scheme+"://"+u.Host)
	o.Set("searchParams", spObj)
	hrefGetter := vm.fn(func(goja.FunctionCall) goja.Value { return r.ToValue(build()) })
	_ = o.DefineAccessorProperty("href", hrefGetter, vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }), goja.FLAG_TRUE, goja.FLAG_TRUE)
	searchGetter := vm.fn(func(goja.FunctionCall) goja.Value {
		q := encodePairs(*pairs)
		if q != "" {
			return r.ToValue("?" + q)
		}
		return r.ToValue("")
	})
	_ = o.DefineAccessorProperty("search", searchGetter, vm.fn(func(goja.FunctionCall) goja.Value { return goja.Undefined() }), goja.FLAG_TRUE, goja.FLAG_TRUE)
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
