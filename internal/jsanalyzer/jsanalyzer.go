package jsanalyzer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"apimap/internal/contract"
	"apimap/internal/linker"
)

func Parse(jsContent string, sourceURL string, fullInfo bool) (result []linker.Link, obs []contract.Observation) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			obs = nil
		}
	}()
	ctx := newContext(sourceURL, fullInfo)
	tokens := tokenize(jsContent)
	p := newParser(tokens)
	stmts := p.parseProgram()

	ctx.analyze(stmts)

	// Token-level scan for HTTP-client calls (client.get/post/...) that the AST
	// analysis cannot reach — either the parser bailed out on the surrounding
	// minified code, or the client object is an unrecognized short name.
	ctx.scanCalls(tokens, jsContent)

	// Robust fallback: many endpoints are built at runtime (e.g.
	// fetch(this.ajaxUrl + '?' + params)) or live inside config/JSON blobs the
	// call-graph analysis can't trace back. Harvest endpoint-looking string
	// literals directly from the token stream so those still surface. This runs
	// on tokens (not the AST) so it works even when parsing bails out on minified
	// or exotic syntax.
	ctx.harvestTokens(tokens)
	return ctx.links, ctx.obs
}

// harvestTokens scans every string and template-literal token for values that
// strongly look like API endpoints and adds them as links.
func (c *context) harvestTokens(tokens []token) {
	// Snapshot endpoints already resolved by the call-graph analysis so we can
	// skip harvesting fragments that are merely substrings of a fuller,
	// already-reported URL (e.g. the "https://api.example.com" piece of a
	// concatenated "https://api.example.com/v1/users").
	traced := make([]string, 0, len(c.links)*2)
	for _, l := range c.links {
		traced = append(traced, l.HREF)
		if l.Resolved != "" && l.Resolved != l.HREF {
			traced = append(traced, l.Resolved)
		}
	}
	for _, t := range tokens {
		switch t.typ {
		case tokString:
			c.harvestString(t.value, traced)
		case tokTemplate:
			// Template parts are separated by \x00 where interpolations were
			// removed. Check each static part, and also the leading part which
			// is often the endpoint base (e.g. `/api/users/${id}`).
			for _, part := range strings.Split(t.value, "\x00") {
				c.harvestString(part, traced)
			}
		}
	}
}

func (c *context) harvestString(s string, traced []string) {
	s = strings.TrimSpace(s)
	if !looksLikeAPIEndpoint(s) {
		return
	}
	for _, tr := range traced {
		if tr != s && strings.Contains(tr, s) {
			return
		}
	}
	c.addLink(s, "js-str", "string-literal", "")
}

// tokenCallMethods maps a member name on an HTTP client object to the request
// method it issues. Used by the token-level fallback scanner, which catches
// calls on clients that carry arbitrary minified names (e, t, s, ...) that the
// AST call-graph analysis cannot resolve.
var tokenCallMethods = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT",
	"delete": "DELETE", "patch": "PATCH",
	"head": "HEAD", "options": "OPTIONS",
}

// bodyCarryingMethods are the HTTP verbs whose argument list can contain a
// request body (the second argument).
var bodyCarryingMethods = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// urlArgShape reports whether a string/template argument is an endpoint-shaped
// URL reference (absolute path, absolute URL, relative path, or a template that
// begins with such a prefix). Literal prose or plain identifiers ("next",
// "name", ...) are rejected.
func urlArgShape(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, p := range []string{"/", "./", "../", "http://", "https://", "//"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return strings.HasPrefix(s, "{") && strings.Contains(s, "/")
}

// scanCalls is a flat token-stream fallback that finds HTTP-client calls of the
// form client.get("..."), client.post("/api/...", body), etc. It runs in
// addition to the AST call-graph analysis so that calls remain visible even
// when the parser bails out on minified/exotic syntax or the client object has
// an unrecognized name. Only calls whose first argument is a string/template
// URL literal are reported; bodies are captured from a literal second argument
// (string or object/array literal).
func (c *context) scanCalls(tokens []token, jsContent string) {
	for i := 0; i < len(tokens); i++ {
		if tokens[i].typ != tokIdent {
			continue
		}
		method, ok := tokenCallMethods[tokens[i].value]
		if !ok {
			continue
		}
		if i == 0 || tokens[i-1].typ != tokPunct || tokens[i-1].value != "." {
			continue
		}
		if i+1 >= len(tokens) || tokens[i+1].typ != tokPunct || tokens[i+1].value != "(" {
			continue
		}
		j := i + 2
		if j >= len(tokens) {
			continue
		}
		url := c.tokenArgURL(tokens[j])
		if !urlArgShape(url) {
			continue
		}
		body := ""
		if bodyCarryingMethods[method] && j+2 < len(tokens) && tokens[j+1].typ == tokPunct && tokens[j+1].value == "," {
			switch v := tokens[j+2]; {
			case v.typ == tokString || v.typ == tokTemplate:
				if s := strings.TrimSpace(v.value); !urlArgShape(s) {
					body = s
				}
			case v.typ == tokPunct && (v.value == "{" || v.value == "["):
				if end := matchOpenLiteral(tokens, j+2); end > j+2 {
					body = sliceSource(jsContent, tokens[j+2].pos, tokenEndRune(tokens, end))
				}
			}
		}
		c.addObs(url, method, nil, body)
	}
}

// matchOpenLiteral scans forward from a balanced open token ({ or [), skipping
// string/template/regexp tokens, and returns the index of the matching close.
func matchOpenLiteral(tokens []token, start int) int {
	if start >= len(tokens) {
		return -1
	}
	open, close := "{", "}"
	if tokens[start].value == "[" {
		open, close = "[", "]"
	}
	depth := 1
	for i := start + 1; i < len(tokens); i++ {
		t := tokens[i]
		if t.typ == tokString || t.typ == tokTemplate || t.typ == tokRegexp {
			continue
		}
		if t.typ != tokPunct {
			continue
		}
		switch t.value {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// tokenEndRune returns the rune offset just past token i.
func tokenEndRune(tokens []token, i int) int {
	if i < 0 || i >= len(tokens) {
		return 0
	}
	return tokens[i].pos + len([]rune(tokens[i].value))
}

// sliceSource extracts the raw source text between two rune offsets.
func sliceSource(jsContent string, start, end int) string {
	if jsContent == "" || end <= start {
		return ""
	}
	runes := []rune(jsContent)
	if start < 0 || end > len(runes) {
		return ""
	}
	return strings.TrimSpace(string(runes[start:end]))
}

// tokenArgURL extracts an endpoint reference from a string or template token
// argument. Template interpolations are rendered as {var}/{…} placeholders,
// matching the convention used by the AST analysis.
func (c *context) tokenArgURL(t token) string {
	switch t.typ {
	case tokString:
		return strings.TrimSpace(t.value)
	case tokTemplate:
		parts := strings.Split(t.value, "\x00")
		if len(t.interps) == 0 {
			return strings.TrimSpace(strings.Join(parts, ""))
		}
		var b strings.Builder
		n := len(t.interps) + 1
		if len(parts) > n {
			n = len(parts)
		}
		for i := 0; i < n; i++ {
			if i > 0 && i-1 < len(t.interps) {
				src := t.interps[i-1]
				if strings.ContainsAny(src, " ()+*/&|?:,<>={}!\"'`;\\") {
					b.WriteString("{…}")
				} else {
					b.WriteString("{" + src + "}")
				}
			}
			if i < len(parts) {
				b.WriteString(parts[i])
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

type context struct {
	sourceURL string
	fullInfo  bool
	vars      map[string]string
	props     map[string]map[string]string
	varInits  map[string]expr
	links     []linker.Link
	seen      map[string]bool
	obs       []contract.Observation
	obsSeen   map[string]bool
}

func newContext(sourceURL string, fullInfo bool) *context {
	return &context{
		sourceURL: sourceURL,
		fullInfo:  fullInfo,
		vars:      map[string]string{},
		props:     map[string]map[string]string{},
		varInits:  map[string]expr{},
		seen:      map[string]bool{},
		obsSeen:   map[string]bool{},
	}
}

func exprPath(e expr) string {
	switch n := e.(type) {
	case *identExpr:
		return n.name
	case *memberExpr:
		return exprPath(n.object) + "." + n.property
	default:
		return ""
	}
}

func (c *context) addLink(href, tag, matchSource, method string) {
	if href == "" || c.seen[href] || isLikelyNotAPI(href) {
		return
	}
	c.seen[href] = true
	link := linker.Link{
		HREF:      href,
		Resolved:  resolveEndpoint(href, c.sourceURL),
		Category:  linker.CategoryAPI,
		LinkType:  linker.LinkTypeAbsolute,
		SourceURL: c.sourceURL,
		Tag:       tag,
	}
	if method != "" {
		link.APIDetails = []linker.APIDetail{{
			MatchSource: matchSource,
			HTTPMethod:  strings.ToUpper(method),
		}}
	}
	c.links = append(c.links, link)
}

// maxObs caps how many request observations one script may contribute so the
// contract inference stays bounded on huge minified bundles.
const maxObs = 4096

// addObs feeds a captured request (method, headers, body) into the contract
// observation pool, resolved against the script's source URL.
func (c *context) addObs(rawURL, method string, headers []contract.NameValue, body string) {
	if len(c.obs) >= maxObs {
		return
	}
	u := resolveEndpoint(strings.TrimSpace(rawURL), c.sourceURL)
	if u == "" {
		return
	}
	key := strings.ToUpper(method) + "|" + u + "|" + body
	for _, h := range headers {
		key += "|" + h.Name + ":" + h.Value
	}
	if c.obsSeen[key] {
		return
	}
	c.obsSeen[key] = true
	c.obs = append(c.obs, contract.Observation{
		URL:     u,
		Method:  stringOr(method, "GET"),
		Headers: headers,
		Body:    body,
	})
}

func stringOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// initConfig captures the shape of a fetch/Request init object literal.
type initConfig struct {
	method  string
	body    string
	headers []contract.NameValue
}

// parseInit extracts method/body/headers from a fetch init expression (a
// direct object literal or a var holding one).
func (c *context) parseInit(e expr) initConfig {
	var cfg initConfig
	if id, ok := e.(*identExpr); ok {
		if o2, ok := c.varInits[id.name]; ok {
			if oe, ok := o2.(*objectExpr); ok {
				e = oe
			}
		}
	}
	oe, ok := e.(*objectExpr)
	if !ok {
		return cfg
	}
	for _, p := range oe.properties {
		switch strings.ToLower(p.key) {
		case "method", "type":
			cfg.method = c.evalExpr(p.value)
		case "body", "data":
			cfg.body = c.bodyExpr(p.value)
		case "headers":
			cfg.headers = c.headersExpr(p.value)
		}
	}
	return cfg
}

// bodyExpr renders a request-body expression: object literals compile to JSON,
// strings pass through, unknown/ident/dynamic values collapse to null so the
// format analysis still sees a placeholder.
func (c *context) bodyExpr(e expr) string {
	if e == nil {
		return ""
	}
	if v, ok := c.objValue(e); ok {
		switch v := v.(type) {
		case string:
			s := strings.TrimSpace(v)
			if s == "" {
				return "null"
			}
			return s
		case map[string]any, []any:
			b, err := json.Marshal(v)
			if err == nil {
				return string(b)
			}
		}
	}
	if s := strings.TrimSpace(c.evalExpr(e)); s != "" {
		return s
	}
	return "null"
}

// headersExpr converts a headers value (object literal or recorded props) into
// ordered assignments.
func (c *context) headersExpr(e expr) []contract.NameValue {
	var out []contract.NameValue
	switch e := e.(type) {
	case *objectExpr:
		for _, p := range e.properties {
			v := c.evalExpr(p.value)
			out = append(out, contract.NameValue{Name: p.key, Value: v})
		}
	case *identExpr:
		if props, ok := c.props[e.name]; ok {
			for k, v := range props {
				out = append(out, contract.NameValue{Name: k, Value: v})
			}
		}
	}
	return out
}

// objValue converts a syntactically known expression into a JSON-able Go
// value. Dynamic values (idents, calls, member lookups) resolve through the
// tracked var/string table; anything unresolvable fails so callers can fall
// back to a literal.
func (c *context) objValue(e expr) (any, bool) {
	switch e := e.(type) {
	case *stringExpr:
		return e.value, true
	case *numberExpr:
		if i, err := strconv.ParseInt(e.value, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(e.value, 64); err == nil {
			return f, true
		}
		return nil, false
	case *boolExpr:
		return e.value, true
	case *nullExpr:
		return nil, true
	case *arrayExpr:
		arr := make([]any, 0, len(e.elements))
		for _, el := range e.elements {
			v, ok := c.objValue(el)
			if !ok {
				v = nil
			}
			arr = append(arr, v)
		}
		return arr, true
	case *objectExpr:
		m := make(map[string]any, len(e.properties))
		for _, p := range e.properties {
			v, ok := c.objValue(p.value)
			if !ok {
				v = nil
			}
			m[p.key] = v
		}
		return m, true
	case *binaryExpr:
		if e.op == "+" {
			l, lk := c.objValue(e.left)
			r, rk := c.objValue(e.right)
			if lk && rk {
				return fmt.Sprint(l) + fmt.Sprint(r), true
			}
		}
	case *templateExpr:
		var b strings.Builder
		for _, p := range e.parts {
			v, ok := c.objValue(p)
			if !ok {
				return nil, false
			}
			b.WriteString(fmt.Sprint(v))
		}
		return b.String(), true
	case *identExpr:
		if v, ok := c.vars[e.name]; ok {
			return v, true
		}
	case *callExpr:
		// JSON.stringify({...})
		if me, ok := e.callee.(*memberExpr); ok && me.property == "stringify" {
			if len(e.args) > 0 {
				if v, ok := c.objValue(e.args[0]); ok {
					b, err := json.Marshal(v)
					if err == nil {
						return string(b), true
					}
				}
			}
		}
		if s := c.evalCall(e); s != "" {
			return s, true
		}
	case *memberExpr:
		if s := c.evalMember(e); s != "" {
			return s, true
		}
	case *unaryExpr:
		if s := c.evalExpr(e); s != "" {
			return s, true
		}
	}
	return nil, false
}

func (c *context) analyze(stmts []stmt) {
	for _, s := range stmts {
		c.analyzeStmt(s)
	}
}

func (c *context) analyzeStmt(s stmt) {
	switch s := s.(type) {
	case *varDecl:
		c.analyzeVarDecl(s)
	case *exprStmt:
		c.analyzeExpr(s.expr)
	case *blockStmt:
		c.analyze(s.stmts)
	case *ifStmt:
		c.analyzeStmt(s.consequent)
		if s.alternate != nil {
			c.analyzeStmt(s.alternate)
		}
	case *forStmt:
		if s.init != nil {
			c.analyzeStmt(s.init)
		}
		if s.body != nil {
			c.analyzeStmt(s.body)
		}
	case *whileStmt:
		if s.body != nil {
			c.analyzeStmt(s.body)
		}
	case *funcDecl:
		if s.body != nil {
			c.analyze(s.body.stmts)
		}
	case *returnStmt:
	case *breakStmt:
	case *continueStmt:
	}
}

func (c *context) analyzeVarDecl(d *varDecl) {
	val := c.evalExpr(d.init)
	if val != "" {
		c.vars[d.name] = val
	}
	switch d.init.(type) {
	case *objectExpr, *arrayExpr, *callExpr:
		c.varInits[d.name] = d.init
	}
	if oe, ok := d.init.(*objectExpr); ok {
		props := make(map[string]string)
		for _, p := range oe.properties {
			v := c.evalExpr(p.value)
			if v != "" {
				props[p.key] = v
			}
		}
		if len(props) > 0 {
			c.props[d.name] = props
		}
	}
	if me, ok := d.init.(*memberExpr); ok {
		path := exprPath(me)
		if tracked, exists := c.props[path]; exists {
			c.props[d.name] = tracked
		}
		val := c.evalExpr(d.init)
		if val != "" {
			c.vars[d.name] = val
		}
	}
	if fe, ok := d.init.(*funcExpr); ok && fe.body != nil {
		c.analyze(fe.body.stmts)
	}
}

var configPropKeys = map[string]bool{
	"url": true, "uri": true, "endpoint": true, "api": true, "baseURL": true,
	"ajaxUrl": true, "ajax_url": true,
}

func (c *context) analyzeExpr(e expr) {
	switch e := e.(type) {
	case *callExpr:
		c.analyzeCall(e)
	case *newExpr:
		match, objName, methodName, httpMethod := c.resolveNewExpr(e)
		if match != callNone {
			c.applyMatch(match, e.args, objName, methodName, httpMethod)
		}
	case *objectExpr:
		c.analyzeConfigObj(e)
	case *binaryExpr:
		if e.op == "=" {
			if id, ok := e.left.(*identExpr); ok {
				val := c.evalExpr(e.right)
				if val != "" {
					c.vars[id.name] = val
				}
				if oe, ok := e.right.(*objectExpr); ok {
					props := make(map[string]string)
					for _, p := range oe.properties {
						v := c.evalExpr(p.value)
						if v != "" {
							props[p.key] = v
						}
					}
					if len(props) > 0 {
						c.props[id.name] = props
					}
				}
			}
			if me, ok := e.left.(*memberExpr); ok {
				path := exprPath(me)
				if oe, ok := e.right.(*objectExpr); ok {
					props := make(map[string]string)
					for _, p := range oe.properties {
						v := c.evalExpr(p.value)
						if v != "" {
							props[p.key] = v
						}
					}
					if len(props) > 0 {
						c.props[path] = props
					}
				}
				if id, ok := e.right.(*identExpr); ok {
					if tracked, exists := c.props[id.name]; exists {
						c.props[path] = tracked
					}
				}
				if me2, ok := e.right.(*memberExpr); ok {
					rightPath := exprPath(me2)
					if tracked, exists := c.props[rightPath]; exists {
						c.props[path] = tracked
					}
					val := c.evalExpr(e.right)
					if val != "" {
						c.props[path] = map[string]string{"": val}
					}
					// also store as simple property value
					objPath := exprPath(me.object)
					if tracked, ok := c.props[objPath]; ok {
						if v, ok := tracked[me.property]; ok {
							c.vars[me.property] = v
						}
					}
				}
				val := c.evalExpr(e.right)
				if val != "" {
					if id, ok := me.object.(*identExpr); ok {
						c.vars[id.name+"."+me.property] = val
					}
				}
			}
			if fe, ok := e.right.(*funcExpr); ok && fe.body != nil {
				c.analyze(fe.body.stmts)
			}
		}
	case *seqExpr:
		for _, ex := range e.exprs {
			c.analyzeExpr(ex)
		}
	case *funcExpr:
		if e.body != nil {
			c.analyze(e.body.stmts)
		}
	}
}

var apiPatterns = []struct {
	obj      string
	prop     string
	httpProp int
}{
	{"fetch", "", -1},
	{"axios", "", -1},
	{"$", "ajax", 0},
	{"$", "get", 0},
	{"$", "post", 0},
	{"$", "getJSON", 0},
	{"$", "put", 0},
	{"$", "delete", 0},
	{"BX", "ajax", 0},
	{"BX", "ajax.post", 0},
	{"BX", "ajax.get", 0},
	{"BX", "ajax.runAction", 0},
	{"BX", "ajax.runComponentAction", 0},
	{"axios", "get", 0},
	{"axios", "post", 0},
	{"axios", "put", 0},
	{"axios", "delete", 0},
	{"axios", "patch", 0},
	{"superagent", "get", 0},
	{"superagent", "post", 0},
	{"superagent", "put", 0},
	{"superagent", "delete", 0},
	{"superagent", "patch", 0},
	{"request", "get", 0},
	{"request", "post", 0},
	{"request", "put", 0},
	{"request", "delete", 0},
	{"request", "patch", 0},
	{"got", "get", 0},
	{"got", "post", 0},
	{"got", "put", 0},
	{"got", "delete", 0},
	{"got", "patch", 0},
}

var methodPropPatterns = []string{
	"get", "post", "put", "delete", "patch", "request",
}

type callMatch int

const (
	callNone       callMatch = iota
	callDirect               // fetch(url), axios(url)
	callMethod               // obj.method(url)
	callConfig               // obj({url: url, method: ...})
	callXHROpen              // xhr.open(method, url)
	callNewRequest           // new Request(url)
)

var chainMethods = map[string]bool{
	"end": true, "subscribe": true, "then": true, "catch": true,
	"finally": true, "exec": true, "send": true, "done": true, "fail": true,
}

func (c *context) analyzeCall(ce *callExpr) {
	if me, ok := ce.callee.(*memberExpr); ok {
		if chainMethods[me.property] {
			if innerCall, ok := me.object.(*callExpr); ok {
				c.analyzeCall(innerCall)
				return
			}
		}
	}

	if fe, ok := ce.callee.(*funcExpr); ok {
		if fe.body != nil {
			c.analyze(fe.body.stmts)
		}
		return
	}

	match, objName, methodName, httpMethod := c.resolveCall(ce)
	if match == callNone {
		return
	}
	c.applyMatch(match, ce.args, objName, methodName, httpMethod)
}

func memberChain(node expr) []string {
	var parts []string
	for {
		switch n := node.(type) {
		case *identExpr:
			parts = append([]string{n.name}, parts...)
			return parts
		case *memberExpr:
			parts = append([]string{n.property}, parts...)
			node = n.object
		default:
			parts = append([]string{"?"}, parts...)
			return parts
		}
	}
}

func dottedPath(parts []string) string {
	return strings.Join(parts, ".")
}

func (c *context) resolveCall(ce *callExpr) (match callMatch, objName, methodName, httpMethod string) {
	switch callee := ce.callee.(type) {
	case *identExpr:
		name := callee.name
		if name == "fetch" {
			return callDirect, "fetch", "", "GET"
		}
		if name == "axios" {
			return callDirect, "axios", "", "GET"
		}
		if name == "Request" || name == "request" {
			if len(ce.args) > 0 {
				// request({url, method, data}) is the superagent/axios-style
				// options form; new Request('/path') is the fetch-style form.
				if _, ok := ce.args[0].(*objectExpr); ok {
					return callConfig, name, name, "GET"
				}
				if id, ok := ce.args[0].(*identExpr); ok {
					if o2, ok := c.varInits[id.name]; ok {
						if _, ok := o2.(*objectExpr); ok {
							return callConfig, name, name, "GET"
						}
					}
				}
			}
			return callNewRequest, "Request", "", "GET"
		}

	case *memberExpr:
		chain := memberChain(callee)
		fullPath := dottedPath(chain)

		if len(chain) >= 2 {
			objName = chain[0]

			if objName == "$" || objName == "BX" || objName == "axios" ||
				objName == "superagent" || objName == "request" || objName == "got" {
				if len(chain) == 2 {
					propName := chain[1]
					if propName == "ajax" {
						return callConfig, objName, propName, "GET"
					}
					for _, m := range methodPropPatterns {
						if propName == m {
							return callMethod, objName, propName, strings.ToUpper(propName)
						}
					}
				}
				if len(chain) >= 3 && objName == "BX" {
					if chain[1] == "ajax" {
						subMethod := chain[2]
						if subMethod == "post" || subMethod == "get" {
							method := "POST"
							if subMethod == "get" {
								method = "GET"
							}
							return callMethod, "BX", "ajax." + subMethod, method
						}
						if chain[2] == "runAction" || chain[2] == "runComponentAction" {
							return callMethod, "BX", "ajax." + chain[2], "POST"
						}
					}
				}
			}
		}

		lastProp := chain[len(chain)-1]

		if lastProp == "open" {
			return callXHROpen, fullPath, "open", ""
		}

		if lastProp == "sendBeacon" {
			return callMethod, fullPath, "sendBeacon", "POST"
		}

		for _, m := range methodPropPatterns {
			if lastProp == m {
				return callMethod, fullPath, lastProp, strings.ToUpper(lastProp)
			}
		}

		if lastProp == "request" || lastProp == "ajax" {
			return callConfig, fullPath, lastProp, "GET"
		}

	case *newExpr:
		if id, ok := callee.callee.(*identExpr); ok && id.name == "Request" {
			return callNewRequest, "Request", "", "GET"
		}
		if id, ok := callee.callee.(*identExpr); ok && id.name == "XMLHttpRequest" {
			return callNone, "", "", ""
		}
		if me, ok := callee.callee.(*memberExpr); ok {
			if id, ok := me.object.(*identExpr); ok && id.name == "XMLHttpRequest" {
				return callNone, "", "", ""
			}
		}
	}

	return callNone, "", "", ""
}

func (c *context) analyzeConfigObj(oe *objectExpr) {
	urlVal := ""
	method := ""
	var body string
	var headers []contract.NameValue
	for _, prop := range oe.properties {
		switch {
		case configPropKeys[prop.key]:
			urlVal = c.evalExpr(prop.value)
			if urlVal == "" {
				if id, ok := prop.value.(*identExpr); ok {
					urlVal = c.vars[id.name]
				}
			}
		case prop.key == "method" || prop.key == "type":
			method = c.evalExpr(prop.value)
		case prop.key == "body" || prop.key == "data":
			body = c.bodyExpr(prop.value)
		case prop.key == "headers":
			headers = c.headersExpr(prop.value)
		}
	}
	if urlVal != "" {
		c.addLink(urlVal, "js-api", "config", method)
		m := strings.ToUpper(strings.TrimSpace(method))
		if m == "" {
			m = "GET"
		}
		c.addObs(urlVal, m, headers, body)
	}
}

func (c *context) evalExpr(e expr) string {
	if e == nil {
		return ""
	}
	switch e := e.(type) {
	case *stringExpr:
		return e.value
	case *templateExpr:
		var parts []string
		for _, p := range e.parts {
			s := c.evalExpr(p)
			if s == "" {
				return ""
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "")
	case *interpExpr:
		if v, ok := c.vars[e.src]; ok {
			return v
		}
		if strings.ContainsAny(e.src, " \t()[]{}+*/&|?:,<>") {
			return ""
		}
		if inner := dottedExpr(e.src); inner != nil {
			return c.evalExpr(inner)
		}
		return ""
	case *identExpr:
		if val, ok := c.vars[e.name]; ok {
			return val
		}
		return ""
	case *binaryExpr:
		if e.op == "+" {
			left := c.evalExpr(e.left)
			right := c.evalExpr(e.right)
			if left != "" && right != "" {
				return left + right
			}
		}
		if e.op == "=" {
			if id, ok := e.left.(*identExpr); ok {
				val := c.evalExpr(e.right)
				if val != "" {
					c.vars[id.name] = val
				}
				return val
			}
		}
		return ""
	case *callExpr:
		return c.evalCall(e)
	case *memberExpr:
		return c.evalMember(e)
	case *arrayExpr:
		var parts []string
		for _, el := range e.elements {
			s := c.evalExpr(el)
			if s == "" {
				return ""
			}
			parts = append(parts, s)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "")
		}
		return ""
	case *unaryExpr:
		if e.op == "+" {
			return c.evalExpr(e.expr)
		}
		return ""
	case *numberExpr:
		return ""
	case *boolExpr:
		return ""
	case *regexpExpr:
		return ""
	case *nullExpr:
		return ""
	}
	return ""
}

// dottedExpr builds an expression from a simple dotted path ("cfg.baseUrl")
// so template interpolations such as ${cfg.baseUrl} can be resolved through
// the tracked variable/property tables. Returns nil for anything more complex.
func dottedExpr(s string) expr {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, " ") {
		return nil
	}
	segments := strings.Split(s, ".")
	if len(segments) == 0 || segments[0] == "" {
		return nil
	}
	var e expr
	for i, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil
		}
		if i == 0 {
			e = &identExpr{name: seg}
		} else {
			e = &memberExpr{object: e, property: seg}
		}
	}
	return e
}

// placeholder renders an unresolvable sub-expression as a readable path
// placeholder ("{id}") so a dynamic endpoint shape is not lost. Bare idents
// used as whole arguments are deliberately excluded from this treatment by
// resolveURLArg — a bare unknown var is likely a base/abstraction, not a path.
func (c *context) placeholder(e expr) string {
	switch e := e.(type) {
	case *interpExpr:
		if strings.ContainsAny(e.src, " ()+*/&|?:,<>={}!\"'`;\\") {
			return "{…}"
		}
		return "{" + e.src + "}"
	case *identExpr:
		return "{" + e.name + "}"
	case *memberExpr:
		return "{" + exprPath(e) + "}"
	}
	return ""
}

// resolveURLArg returns the best-effort literal for an endpoint argument.
// Fully-resolvable expressions evaluate normally; otherwise static fragments
// are kept and unresolvable interpolations become "{var}" placeholders
// (e.g. "/api/admin/recorders/{n}/sync"). Unknown stand-alone idents yield ""
// so opaque bases like this.ajaxUrl never surface as junk links.
func (c *context) resolveURLArg(e expr) string {
	if s := c.evalExpr(e); s != "" {
		return s
	}
	switch e := e.(type) {
	case *templateExpr:
		var b strings.Builder
		for _, p := range e.parts {
			if se, ok := p.(*stringExpr); ok {
				b.WriteString(se.value)
				continue
			}
			if s := c.evalExpr(p); s != "" {
				b.WriteString(s)
			} else if ph := c.placeholder(p); ph != "" {
				b.WriteString(ph)
			} else {
				return ""
			}
		}
		return b.String()
	case *binaryExpr:
		if e.op == "+" {
			left := c.evalExpr(e.left)
			if left != "" {
				right := c.evalExpr(e.right)
				if right != "" {
					return left + right
				}
				// static prefix + dynamic tail: keep the prefix
				return left
			}
			right := c.evalExpr(e.right)
			if right != "" {
				if strings.HasPrefix(right, "/") {
					if ph := c.placeholder(e.left); ph != "" {
						return ph + right
					}
				}
			}
		}
	}
	return ""
}

func (c *context) evalCall(e *callExpr) string {
	ce := e
	if id, ok := ce.callee.(*identExpr); ok && id.name == "join" && len(ce.args) == 1 {
		return ""
	}

	if me, ok := ce.callee.(*memberExpr); ok {
		if me.property == "join" {
			left := c.evalExpr(me.object)
			if left != "" {
				sep := c.evalExpr(ce.args[0])
				if sep == "" {
					sep = ","
				}
				_ = sep
				parts := strings.Split(left, ",")
				var cleaned []string
				for _, p := range parts {
					p = strings.TrimSpace(p)
					p = strings.Trim(p, "'\" ")
					if p != "" {
						cleaned = append(cleaned, p)
					}
				}
				return strings.Join(cleaned, "")
			}
			if arr, ok := me.object.(*arrayExpr); ok {
				var parts []string
				for _, el := range arr.elements {
					s := c.evalExpr(el)
					if s == "" {
						return ""
					}
					parts = append(parts, s)
				}
				if len(parts) > 0 {
					return strings.Join(parts, "")
				}
			}
		}
	}

	return ""
}

func (c *context) evalMember(e *memberExpr) string {
	obj := c.evalExpr(e.object)
	if obj == "" {
		if id, ok := e.object.(*identExpr); ok {
			if val, exists := c.vars[id.name]; exists {
				obj = val
			} else {
				path := exprPath(e)
				if tracked, exists := c.props[id.name]; exists {
					if v, ok := tracked[e.property]; ok {
						return v
					}
				}
				if tracked, exists := c.props[path]; exists {
					if v, ok := tracked[""]; ok {
						return v
					}
				}
				if tracked, exists := c.props[id.name+"."+e.property]; exists {
					if v, ok := tracked[""]; ok {
						return v
					}
				}
				return ""
			}
		} else if me, ok := e.object.(*memberExpr); ok {
			path := exprPath(me)
			if tracked, exists := c.props[path]; exists {
				if v, ok := tracked[e.property]; ok {
					return v
				}
			}
			return ""
		} else {
			return ""
		}
	}
	path := exprPath(e)
	if tracked, exists := c.props[path]; exists {
		if v, ok := tracked[""]; ok {
			return v
		}
	}
	return ""
}

type tokenType int

const (
	tokEOF tokenType = iota
	tokIdent
	tokString
	tokTemplate
	tokNumber
	tokPunct
	tokKeyword
	tokOp
	tokRegexp
)

type token struct {
	typ     tokenType
	value   string
	pos     int
	parts   []string // tokTemplate: static segments ("" when the scan ended before the terminator)
	interps []string // tokTemplate: raw interpolation sources, in order
}

func tokenize(src string) []token {
	var tokens []token
	i := 0
	runes := []rune(src)

	advance := func(n int) {
		i += n
	}

	emit := func(typ tokenType, val string) {
		tokens = append(tokens, token{typ: typ, value: val, pos: i})
	}

	for i < len(runes) {
		ch := runes[i]

		if ch == 0 {
			break
		}

		if unicode.IsSpace(ch) {
			advance(1)
			continue
		}

		if ch == '/' && i+1 < len(runes) {
			next := runes[i+1]
			if next == '/' {
				j := i + 2
				for j < len(runes) && runes[j] != '\n' {
					j++
				}
				advance(j - i)
				continue
			}
			if next == '*' {
				j := i + 2
				depth := 1
				for j < len(runes) && depth > 0 {
					if runes[j] == '/' && j+1 < len(runes) && runes[j+1] == '*' {
						depth++
						j += 2
					} else if runes[j] == '*' && j+1 < len(runes) && runes[j+1] == '/' {
						depth--
						j += 2
					} else {
						j++
					}
				}
				advance(j - i)
				continue
			}

			if i > 0 {
				prev := runes[i-1]
				if prev == '=' || prev == '(' || prev == ',' || prev == '[' || prev == '!' ||
					prev == '&' || prev == '|' || prev == '?' || prev == ':' ||
					prev == ';' || prev == '{' || prev == 0 {
					j := i + 1
					for j < len(runes) {
						r := runes[j]
						if r == '/' {
							j++
							break
						}
						if r == '\\' {
							j += 2
							continue
						}
						j++
					}
					emit(tokRegexp, string(runes[i:j]))
					advance(j - i)
					continue
				}
			}
		}

		if ch == '\'' || ch == '"' {
			quote := ch
			j := i + 1
			for j < len(runes) {
				if runes[j] == '\\' {
					j += 2
					continue
				}
				if runes[j] == quote {
					j++
					break
				}
				j++
			}
			raw := string(runes[i:j])
			unquoted := raw[1 : len(raw)-1]
			unquoted = unescapeJS(unquoted)
			emit(tokString, unquoted)
			advance(j - i)
			continue
		}

		if ch == '`' {
			j := i + 1
			staticParts := []string{}
			var interps []string
			partStart := j
			for j < len(runes) {
				if runes[j] == '\\' {
					j += 2
					continue
				}
				if runes[j] == '$' && j+1 < len(runes) && runes[j+1] == '{' {
					if partStart < j {
						staticParts = append(staticParts, string(runes[partStart:j]))
					}
					depth := 1
					interpStart := j + 2
					j += 2
					for j < len(runes) && depth > 0 {
						if runes[j] == '{' {
							depth++
						} else if runes[j] == '}' {
							depth--
						}
						j++
					}
					interps = append(interps, string(runes[interpStart:j-1]))
					partStart = j
					continue
				}
				if runes[j] == '`' {
					if partStart < j {
						staticParts = append(staticParts, string(runes[partStart:j]))
					}
					emit(tokTemplate, strings.Join(staticParts, "\x00"))
					if len(interps) > 0 {
						tokens[len(tokens)-1].parts = staticParts
						tokens[len(tokens)-1].interps = interps
					}
					j++
					break
				}
				j++
			}
			advance(j - i)
			continue
		}

		if unicode.IsLetter(ch) || ch == '_' || ch == '$' {
			j := i
			for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j]) || runes[j] == '_' || runes[j] == '$') {
				j++
			}
			word := string(runes[i:j])
			if isKeyword(word) {
				emit(tokKeyword, word)
			} else {
				emit(tokIdent, word)
			}
			advance(j - i)
			continue
		}

		if unicode.IsDigit(ch) {
			j := i
			for j < len(runes) && (unicode.IsDigit(runes[j]) || runes[j] == '.' || runes[j] == 'x' || runes[j] == 'X') {
				j++
			}
			if j > i && runes[j-1] == '.' && j < len(runes) && unicode.IsLetter(runes[j]) {
				j--
			}
			emit(tokNumber, string(runes[i:j]))
			advance(j - i)
			continue
		}

		operators := []string{"===", "!==", "==", "!=", "<=", ">=", "=>", "||", "&&", "++", "--", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>=", ">>>=", "??", "?."}
		matched := false
		for _, op := range operators {
			if i+len(op) <= len(runes) && string(runes[i:i+len(op)]) == op {
				emit(tokOp, op)
				advance(len(op))
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		if ch == '+' || ch == '-' || ch == '*' || ch == '/' || ch == '%' ||
			ch == '=' || ch == '!' || ch == '<' || ch == '>' || ch == '&' ||
			ch == '|' || ch == '^' || ch == '~' || ch == '?' {
			op := string(ch)
			if i+1 < len(runes) {
				next := runes[i+1]
				switch {
				case ch == '=' && next == '>':
					op = "=>"
				case ch == '=' && next == '=':
					op = "=="
				case ch == '!' && next == '=':
					op = "!="
				case ch == '<' && next == '=':
					op = "<="
				case ch == '>' && next == '=':
					op = ">="
				case ch == '&' && next == '&':
					op = "&&"
				case ch == '|' && next == '|':
					op = "||"
				}
				if len(op) > 1 {
					if i+2 < len(runes) {
						third := runes[i+2]
						if (op == "==" && third == '=') || (op == "!=" && third == '=') {
							op = string(ch) + string(next) + string(third)
						}
					}
					emit(tokOp, op)
					advance(len([]rune(op)))
					continue
				}
			}
			emit(tokOp, op)
			advance(1)
			continue
		}

		if ch == '(' || ch == ')' || ch == '[' || ch == ']' || ch == '{' ||
			ch == '}' || ch == ';' || ch == ',' || ch == '.' || ch == ':' {
			emit(tokPunct, string(ch))
			advance(1)
			continue
		}

		advance(1)
	}

	emit(tokEOF, "")
	return tokens
}

func unescapeJS(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '\\':
				b.WriteByte('\\')
			case '\'':
				b.WriteByte('\'')
			case '"':
				b.WriteByte('"')
			case '`':
				b.WriteByte('`')
			case '0':
				b.WriteByte(0)
			case 'x':
				if i+3 < len(s) {
					hex := s[i+2 : i+4]
					if val, err := strconv.ParseUint(hex, 16, 8); err == nil {
						b.WriteByte(byte(val))
						i += 3
					}
				}
			case 'u':
				if i+5 < len(s) {
					hex := s[i+2 : i+6]
					if val, err := strconv.ParseUint(hex, 16, 32); err == nil {
						b.WriteRune(rune(val))
						i += 5
					}
				}
			default:
				b.WriteByte(s[i+1])
			}
			i += 2
		} else {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

func isKeyword(word string) bool {
	switch word {
	case "const", "let", "var", "function", "return", "if", "else",
		"for", "while", "do", "switch", "case", "break", "continue",
		"new", "this", "typeof", "delete", "void", "in", "of",
		"try", "catch", "finally", "throw", "class", "extends",
		"import", "export", "default", "from", "async", "await",
		"yield", "true", "false", "null", "undefined", "instanceof":
		return true
	}
	return false
}

type parser struct {
	tokens []token
	pos    int
}

func newParser(tokens []token) *parser {
	return &parser{tokens: tokens, pos: 0}
}

func (p *parser) peek() token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return token{typ: tokEOF}
}

func (p *parser) advance() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) expect(typ tokenType, value string) token {
	t := p.peek()
	if t.typ == tokEOF {
		return t
	}
	if t.typ == typ && (value == "" || t.value == value) {
		p.advance()
	}
	return t
}

func (p *parser) parseProgram() []stmt {
	var stmts []stmt
	for p.peek().typ != tokEOF {
		s := p.parseStmt()
		if s != nil {
			stmts = append(stmts, s)
		} else {
			p.advance()
		}
	}
	return stmts
}

func (p *parser) parseStmt() stmt {
	t := p.peek()

	switch t.typ {
	case tokKeyword:
		switch t.value {
		case "const", "let", "var":
			return p.parseVarDecl()
		case "function":
			return p.parseFuncDecl()
		case "return":
			return p.parseReturnStmt()
		case "if":
			return p.parseIfStmt()
		case "for":
			return p.parseForStmt()
		case "while":
			return p.parseWhileStmt()
		case "do":
			return p.parseDoWhileStmt()
		case "switch":
			return p.parseSwitchStmt()
		case "try":
			return p.parseTryStmt()
		case "break":
			p.advance()
			p.expect(tokPunct, ";")
			return &breakStmt{}
		case "continue":
			p.advance()
			p.expect(tokPunct, ";")
			return &continueStmt{}
		case "new":
			return p.parseExprStmt()
		case "true", "false", "null", "undefined", "typeof", "void", "delete":
			return p.parseExprStmt()
		case "import", "export", "class":
			if t.value == "class" {
				p.advance()
				for p.peek().typ != tokPunct || p.peek().value != "{" {
					if p.peek().typ == tokEOF {
						return nil
					}
					p.advance()
				}
				p.skipBlock()
				p.skipSemicolons()
				return nil
			}
			if t.value == "import" {
				p.advance()
				for p.peek().typ != tokPunct || p.peek().value != ";" {
					if p.peek().typ == tokEOF {
						return nil
					}
					p.advance()
				}
				p.advance()
				return nil
			}
			if t.value == "export" {
				p.advance()
				return p.parseStmt()
			}
			return nil
		case "async":
			p.advance()
			if p.peek().typ == tokKeyword && p.peek().value == "function" {
				return p.parseFuncDecl()
			}
			return p.parseExprStmt()
		default:
			return p.parseExprStmt()
		}

	case tokPunct:
		if t.value == ";" {
			p.advance()
			return nil
		}
		if t.value == "{" {
			if p.isObjectLiteral() {
				return p.parseExprStmt()
			}
			return p.parseBlockStmt()
		}
		return p.parseExprStmt()

	case tokString, tokTemplate, tokNumber, tokIdent, tokOp, tokRegexp:
		return p.parseExprStmt()

	case tokEOF:
		return nil

	default:
		p.advance()
		return nil
	}
}

func (p *parser) parseVarDecl() stmt {
	t := p.advance()
	kind := t.value

	first := p.parseSingleDeclarator(kind)
	if p.peek().typ != tokPunct || p.peek().value != "," {
		p.skipSemicolons()
		return first
	}

	list := []stmt{first}
	for p.peek().typ == tokPunct && p.peek().value == "," {
		p.advance()
		list = append(list, p.parseSingleDeclarator(kind))
	}
	p.skipSemicolons()
	return &blockStmt{stmts: list}
}

// parseSingleDeclarator parses one "name[ = init]" pair of a var/let/const
// declaration. Destructuring and other exotic patterns fall back to consuming
// a single token so parsing always makes progress.
func (p *parser) parseSingleDeclarator(kind string) *varDecl {
	nameTok := p.expect(tokIdent, "")
	name := nameTok.value
	if name == "" {
		p.advance()
	}
	var init expr
	if p.peek().typ == tokOp && p.peek().value == "=" {
		p.advance()
		init = p.parseExpr(0)
	}
	return &varDecl{name: name, init: init, kind: kind}
}

func (p *parser) parseFuncDecl() *funcDecl {
	p.advance()
	nameTok := p.expect(tokIdent, "")
	name := nameTok.value

	p.expect(tokPunct, "(")
	for p.peek().typ != tokPunct && p.peek().value != ")" {
		if p.peek().typ == tokEOF {
			return &funcDecl{name: name}
		}
		p.advance()
	}
	p.expect(tokPunct, ")")

	body := &blockStmt{}
	if p.peek().typ == tokPunct && p.peek().value == "{" {
		body = p.parseBlockStmt()
	}

	return &funcDecl{name: name, body: body}
}

func (p *parser) parseReturnStmt() *returnStmt {
	p.advance()
	if p.peek().typ == tokPunct && p.peek().value == ";" {
		p.advance()
		return &returnStmt{}
	}
	expr := p.parseExpr(0)
	p.skipSemicolons()
	return &returnStmt{expr: expr}
}

func (p *parser) parseIfStmt() *ifStmt {
	p.advance()
	p.expect(tokPunct, "(")
	condition := p.parseExpr(0)
	p.expect(tokPunct, ")")

	consequent := p.parseStmt()

	var alternate stmt
	if p.peek().typ == tokKeyword && p.peek().value == "else" {
		p.advance()
		alternate = p.parseStmt()
	}

	return &ifStmt{
		condition:  condition,
		consequent: consequent,
		alternate:  alternate,
	}
}

func (p *parser) parseForStmt() *forStmt {
	p.advance()
	p.expect(tokPunct, "(")

	var init stmt
	if p.peek().typ == tokKeyword && (p.peek().value == "const" || p.peek().value == "let" || p.peek().value == "var") {
		init = p.parseVarDecl()
	} else {
		init = p.parseExprStmt()
	}

	if p.peek().typ == tokOp && (p.peek().value == "in" || p.peek().value == "of") {
		p.advance()
		p.parseExpr(0)
	} else {
		p.parseExpr(0) // condition
		if p.peek().typ == tokPunct && p.peek().value == ";" {
			p.advance()
		}
		p.parseExpr(0) // update
	}
	if p.peek().typ == tokPunct && p.peek().value == ")" {
		p.advance()
	}

	var body stmt
	if p.peek().typ == tokPunct && p.peek().value == "{" {
		body = p.parseBlockStmt()
	} else {
		body = p.parseStmt()
	}

	return &forStmt{init: init, body: body}
}

func (p *parser) parseWhileStmt() *whileStmt {
	p.advance()
	p.expect(tokPunct, "(")
	condition := p.parseExpr(0)
	p.expect(tokPunct, ")")

	var body stmt
	if p.peek().typ == tokPunct && p.peek().value == "{" {
		body = p.parseBlockStmt()
	} else {
		body = p.parseStmt()
	}

	return &whileStmt{condition: condition, body: body}
}

func (p *parser) parseDoWhileStmt() *whileStmt {
	p.advance()
	var body stmt
	if p.peek().typ == tokPunct && p.peek().value == "{" {
		body = p.parseBlockStmt()
	} else {
		body = p.parseStmt()
	}
	if p.peek().typ == tokKeyword && p.peek().value == "while" {
		p.advance()
		p.expect(tokPunct, "(")
		p.parseExpr(0)
		p.expect(tokPunct, ")")
	}
	return &whileStmt{body: body}
}

func (p *parser) parseSwitchStmt() stmt {
	p.advance()
	p.expect(tokPunct, "(")
	p.parseExpr(0)
	p.expect(tokPunct, ")")
	p.skipBlock()
	return nil
}

func (p *parser) parseTryStmt() stmt {
	p.advance()
	p.skipBlock()
	if p.peek().typ == tokKeyword && p.peek().value == "catch" {
		p.advance()
		p.skipBlock()
	}
	if p.peek().typ == tokKeyword && p.peek().value == "finally" {
		p.advance()
		p.skipBlock()
	}
	return nil
}

func (p *parser) parseBlockStmt() *blockStmt {
	p.expect(tokPunct, "{")
	var stmts []stmt
	for p.peek().typ != tokPunct || p.peek().value != "}" {
		if p.peek().typ == tokEOF {
			break
		}
		s := p.parseStmt()
		if s != nil {
			stmts = append(stmts, s)
		} else {
			p.advance()
		}
	}
	p.expect(tokPunct, "}")
	return &blockStmt{stmts: stmts}
}

func (p *parser) parseExprStmt() *exprStmt {
	expr := p.parseExpr(0)
	p.skipSemicolons()
	return &exprStmt{expr: expr}
}

func (p *parser) skipBlock() {
	if p.peek().typ == tokPunct && p.peek().value == "{" {
		p.advance()
		depth := 1
		for depth > 0 {
			if p.peek().typ == tokEOF {
				return
			}
			if p.peek().typ == tokPunct {
				if p.peek().value == "{" {
					depth++
				} else if p.peek().value == "}" {
					depth--
				}
			}
			if depth > 0 {
				p.advance()
			}
		}
		p.advance()
	}
}

func (p *parser) skipSemicolons() {
	for p.peek().typ == tokPunct && p.peek().value == ";" {
		p.advance()
	}
}

func (p *parser) parseExpr(minPrec int) expr {
	t := p.peek()

	var left expr

	switch t.typ {
	case tokString:
		p.advance()
		left = &stringExpr{value: t.value}

	case tokTemplate:
		p.advance()
		parts := strings.Split(t.value, "\x00")
		if len(t.interps) == 0 {
			if len(parts) == 1 {
				left = &stringExpr{value: parts[0]}
			} else {
				var exprs []expr
				for _, part := range parts {
					exprs = append(exprs, &stringExpr{value: part})
				}
				left = &templateExpr{parts: exprs}
			}
		} else {
			// Interpolations alternate with static segments:
			// P0 ${I0} P1 ${I1} … Pk. The last static segment is dropped by the
			// tokenizer when it is empty, so iterate over the maximum of the
			// two lengths to keep every interpolation.
			n := len(t.interps) + 1
			if len(parts) > n {
				n = len(parts)
			}
			var exprs []expr
			for i := 0; i < n; i++ {
				if i > 0 {
					exprs = append(exprs, &interpExpr{src: t.interps[i-1]})
				}
				if i < len(parts) {
					exprs = append(exprs, &stringExpr{value: parts[i]})
				}
			}
			left = &templateExpr{parts: exprs}
		}

	case tokNumber:
		p.advance()
		left = &numberExpr{value: t.value}

	case tokIdent:
		p.advance()
		if p.peek().typ == tokOp && p.peek().value == "=>" {
			p.advance()
			left = p.parseArrowBody()
		} else {
			left = &identExpr{name: t.value}
		}

	case tokOp:
		if t.value == "!" || t.value == "~" || t.value == "+" || t.value == "-" || t.value == "typeof" || t.value == "void" || t.value == "delete" {
			p.advance()
			expr := p.parseExpr(precUnary)
			left = &unaryExpr{op: t.value, expr: expr}
		} else if t.value == "++" || t.value == "--" {
			p.advance()
			expr := p.parseExpr(precUnary)
			left = &unaryExpr{op: t.value, expr: expr, prefix: true}
		} else {
			p.advance()
			left = &identExpr{name: t.value}
		}

	case tokRegexp:
		p.advance()
		left = &regexpExpr{value: t.value}

	case tokKeyword:
		switch t.value {
		case "new":
			p.advance()
			callee := p.parseNewCallee()
			var args []expr
			if p.peek().typ == tokPunct && p.peek().value == "(" {
				args = p.parseArgs()
			}
			left = &newExpr{callee: callee, args: args}
		case "async":
			p.advance()
			if p.peek().typ == tokPunct && p.peek().value == "(" && p.isArrowAhead() {
				p.consumeArrowHeader()
				left = p.parseArrowBody()
			} else if p.peek().typ == tokIdent {
				p.advance()
				if p.peek().typ == tokOp && p.peek().value == "=>" {
					p.advance()
					left = p.parseArrowBody()
				} else {
					left = &identExpr{name: t.value}
				}
			} else {
				left = &identExpr{name: t.value}
			}
		case "true":
			p.advance()
			left = &boolExpr{value: true}
		case "false":
			p.advance()
			left = &boolExpr{value: false}
		case "null":
			p.advance()
			left = &nullExpr{}
		case "undefined":
			p.advance()
			left = &identExpr{name: "undefined"}
		case "function":
			p.advance()
			if p.peek().typ == tokIdent {
				p.advance()
			}
			p.expect(tokPunct, "(")
			for p.peek().typ != tokPunct || p.peek().value != ")" {
				if p.peek().typ == tokEOF {
					break
				}
				p.advance()
			}
			p.expect(tokPunct, ")")
			left = &funcExpr{body: p.parseBlockStmt()}
		case "typeof", "void", "delete":
			p.advance()
			expr := p.parseExpr(precUnary)
			left = &unaryExpr{op: t.value, expr: expr}
		default:
			p.advance()
			left = &identExpr{name: t.value}
		}

	case tokPunct:
		if t.value == "(" {
			p.advance()
			if p.isArrowAhead() {
				p.consumeArrowHeader()
				left = p.parseArrowBody()
			} else {
				expr := p.parseExpr(0)
				p.expect(tokPunct, ")")
				left = expr
			}
		} else if t.value == "[" {
			p.advance()
			var elements []expr
			for p.peek().typ != tokPunct || p.peek().value != "]" {
				if p.peek().typ == tokEOF {
					break
				}
				e := p.parseExpr(0)
				elements = append(elements, e)
				if p.peek().typ == tokPunct && p.peek().value == "," {
					p.advance()
				}
			}
			p.expect(tokPunct, "]")
			left = &arrayExpr{elements: elements}
		} else if t.value == "{" {
			p.advance()
			var props []property
			for p.peek().typ != tokPunct || p.peek().value != "}" {
				if p.peek().typ == tokEOF {
					break
				}
				key := ""
				isMethod := false

				if p.peek().typ == tokKeyword && p.peek().value == "async" {
					savePos := p.pos
					p.advance()
					if p.peek().typ == tokIdent && p.pos < len(p.tokens)-1 &&
						p.tokens[p.pos+1].typ == tokPunct && p.tokens[p.pos+1].value == "(" {
						key = p.peek().value
						p.advance()
						isMethod = true
					} else {
						p.pos = savePos
					}
				}

				if !isMethod {
					switch p.peek().typ {
					case tokString:
						key = p.peek().value
						p.advance()
					case tokIdent:
						key = p.peek().value
						p.advance()
					case tokNumber:
						key = p.peek().value
						p.advance()
					case tokKeyword:
						if p.peek().value == "async" {
							key = "async"
							p.advance()
						} else if p.peek().value == "get" || p.peek().value == "set" {
							key = p.peek().value
							p.advance()
						} else {
							p.advance()
							continue
						}
					default:
						p.advance()
						continue
					}
				}

				if p.peek().typ == tokPunct && p.peek().value == "(" {
					p.parseArgs()
					body := p.parseBlockStmt()
					props = append(props, property{key: key, value: &funcExpr{body: body}})
				} else if p.peek().typ == tokPunct && p.peek().value == ":" {
					p.advance()
					val := p.parseExpr(0)
					props = append(props, property{key: key, value: val})
				} else if p.peek().typ == tokPunct && p.peek().value == "," {
					props = append(props, property{key: key, value: &identExpr{name: key}})
				} else if p.peek().typ == tokPunct && p.peek().value == "}" {
					props = append(props, property{key: key, value: &identExpr{name: key}})
				}
				if p.peek().typ == tokPunct && p.peek().value == "," {
					p.advance()
				}
				if p.peek().typ == tokPunct && p.peek().value == "}" {
					break
				}
			}
			p.expect(tokPunct, "}")
			left = &objectExpr{properties: props}
		} else if t.value == ";" {
			p.advance()
			return nil
		} else {
			p.advance()
			left = &identExpr{name: t.value}
		}

	case tokEOF:
		return nil

	default:
		p.advance()
		return nil
	}

	for {
		t = p.peek()

		if t.typ == tokPunct && t.value == "(" {
			args := p.parseArgs()
			left = &callExpr{callee: left, args: args}
			continue
		}

		if t.typ == tokPunct && t.value == "." {
			p.advance()
			propTok := p.peek()
			if propTok.typ == tokIdent || propTok.typ == tokKeyword {
				p.advance()
				left = &memberExpr{object: left, property: propTok.value}
				continue
			}
			break
		}

		if t.typ == tokPunct && t.value == "[" {
			p.advance()
			index := p.parseExpr(0)
			p.expect(tokPunct, "]")
			if id, ok := index.(*stringExpr); ok {
				left = &memberExpr{object: left, property: id.value}
			} else if id, ok := index.(*identExpr); ok {
				left = &memberExpr{object: left, property: id.name}
			} else {
				left = &memberExpr{object: left, property: "[]"}
			}
			continue
		}

		if t.typ == tokPunct && t.value == "?" {
			p.advance()
			p.expect(tokPunct, ".")
			propTok := p.peek()
			if propTok.typ == tokIdent || propTok.typ == tokKeyword || propTok.typ == tokString {
				p.advance()
				if propTok.typ == tokString {
					left = &memberExpr{object: left, property: propTok.value}
				} else {
					left = &memberExpr{object: left, property: propTok.value}
				}
				continue
			}
			if p.peek().typ == tokPunct && p.peek().value == "(" {
				args := p.parseArgs()
				left = &callExpr{callee: left, args: args}
			}
			break
		}

		if t.typ == tokOp && isBinaryOp(t.value) {
			prec := opPrecedence(t.value)
			if prec < minPrec {
				break
			}
			p.advance()
			right := p.parseExpr(prec + 1)
			left = &binaryExpr{left: left, op: t.value, right: right}
			continue
		}

		if t.typ == tokOp && (t.value == "++" || t.value == "--") {
			p.advance()
			left = &unaryExpr{op: t.value, expr: left, prefix: false}
			continue
		}

		break
	}

	if t := p.peek(); t.typ == tokOp && t.value == "?" {
		p.advance()
		consequent := p.parseExpr(0)
		p.expect(tokOp, ":")
		alternate := p.parseExpr(0)
		left = &binaryExpr{left: left, op: "?", right: &binaryExpr{left: consequent, op: ":", right: alternate}}
	}

	return left
}

// isArrowAhead reports whether the current position holds the start of an
// arrow-function parameter list "(…)" whose matching ")" is directly followed
// by "=>". It only scans tokens and never advances the parser.
func (p *parser) isArrowAhead() bool {
	depth := 1
	for i := p.pos; i < len(p.tokens); i++ {
		t := p.tokens[i]
		if t.typ == tokPunct {
			switch t.value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
				if depth == 0 {
					return i+1 < len(p.tokens) && p.tokens[i+1].typ == tokOp && p.tokens[i+1].value == "=>"
				}
			}
		}
		if t.typ == tokEOF {
			return false
		}
	}
	return false
}

// consumeArrowHeader skips the parameter list "(…)" and the following "=>" of
// an arrow function. The caller guarantees isArrowAhead() was true.
func (p *parser) consumeArrowHeader() {
	if p.peek().typ == tokPunct && p.peek().value == "(" {
		p.advance()
	}
	depth := 1
	for depth > 0 {
		t := p.peek()
		if t.typ == tokEOF {
			return
		}
		if t.typ == tokPunct {
			switch t.value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
		}
		p.advance()
	}
	p.advance() // "=>"
}

// parseArrowBody parses the body of an arrow function (block or expression).
func (p *parser) parseArrowBody() *funcExpr {
	if p.peek().typ == tokPunct && p.peek().value == "{" {
		return &funcExpr{body: p.parseBlockStmt()}
	}
	e := p.parseExpr(0)
	return &funcExpr{body: &blockStmt{stmts: []stmt{&exprStmt{expr: e}}}}
}

func (p *parser) isObjectLiteral() bool {
	save := p.pos
	defer func() { p.pos = save }()

	if p.peek().typ != tokPunct || p.peek().value != "{" {
		return false
	}
	p.advance()

	for {
		t := p.peek()
		if t.typ == tokEOF || (t.typ == tokPunct && t.value == "}") {
			return false
		}
		if t.typ == tokString || t.typ == tokIdent || t.typ == tokNumber {
			p.advance()
			if p.peek().typ == tokPunct && p.peek().value == ":" {
				return true
			}
			continue
		}
		break
	}
	return false
}

func (p *parser) parseNewCallee() expr {
	var left expr
	t := p.peek()
	switch t.typ {
	case tokIdent:
		p.advance()
		left = &identExpr{name: t.value}
	default:
		left = p.parseExpr(precUnary)
	}
	for {
		t = p.peek()
		if t.typ == tokPunct && t.value == "." {
			p.advance()
			propTok := p.peek()
			if propTok.typ == tokIdent || propTok.typ == tokKeyword {
				p.advance()
				left = &memberExpr{object: left, property: propTok.value}
				continue
			}
			break
		}
		if t.typ == tokPunct && t.value == "[" {
			p.advance()
			index := p.parseExpr(0)
			p.expect(tokPunct, "]")
			if id, ok := index.(*stringExpr); ok {
				left = &memberExpr{object: left, property: id.value}
			} else if id, ok := index.(*identExpr); ok {
				left = &memberExpr{object: left, property: id.name}
			} else {
				left = &memberExpr{object: left, property: "[]"}
			}
			continue
		}
		if t.typ == tokPunct && t.value == "?" {
			p.advance()
			if p.peek().typ == tokPunct && p.peek().value == "." {
				p.advance()
				propTok := p.peek()
				if propTok.typ == tokIdent || propTok.typ == tokKeyword {
					p.advance()
					left = &memberExpr{object: left, property: propTok.value}
					continue
				}
			}
			break
		}
		break
	}
	return left
}

func (p *parser) parseArgs() []expr {
	p.expect(tokPunct, "(")
	var args []expr
	for p.peek().typ != tokPunct || p.peek().value != ")" {
		if p.peek().typ == tokEOF {
			break
		}
		e := p.parseExpr(0)
		args = append(args, e)
		if p.peek().typ == tokPunct && p.peek().value == "," {
			p.advance()
		}
	}
	p.expect(tokPunct, ")")
	return args
}

const (
	precLowest = iota
	precAssign
	precTernary
	precOr
	precAnd
	precBitOr
	precBitXor
	precBitAnd
	precEquals
	precCompare
	precShift
	precAdd
	precMult
	precUnary
	precCall
)

func opPrecedence(op string) int {
	switch op {
	case "=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>=", ">>>=":
		return precAssign
	case "?":
		return precTernary
	case "||":
		return precOr
	case "&&":
		return precAnd
	case "|":
		return precBitOr
	case "^":
		return precBitXor
	case "&":
		return precBitAnd
	case "==", "!=", "===", "!==":
		return precEquals
	case "<", ">", "<=", ">=", "in", "instanceof":
		return precCompare
	case "<<", ">>", ">>>":
		return precShift
	case "+", "-":
		return precAdd
	case "*", "/", "%":
		return precMult
	default:
		return 0
	}
}

func isBinaryOp(op string) bool {
	switch op {
	case "=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>=", ">>>=",
		"||", "&&", "|", "^", "&",
		"==", "!=", "===", "!==",
		"<", ">", "<=", ">=", "in", "instanceof",
		"<<", ">>", ">>>",
		"+", "-", "*", "/", "%",
		"??":
		return true
	}
	return false
}

type expr interface {
	exprNode()
}

type stmt interface {
	stmtNode()
}

type stringExpr struct {
	value string
}

func (*stringExpr) exprNode() {}

type templateExpr struct {
	parts []expr
}

func (*templateExpr) exprNode() {}

// interpExpr is a single template-literal interpolation. The raw source text
// is kept verbatim so dynamic endpoint shapes survive even when the value
// cannot be resolved (evalExpr) and fall back to a {placeholder} form.
type interpExpr struct {
	src string
}

func (*interpExpr) exprNode() {}

type numberExpr struct {
	value string
}

func (*numberExpr) exprNode() {}

type identExpr struct {
	name string
}

func (*identExpr) exprNode() {}

type binaryExpr struct {
	left  expr
	op    string
	right expr
}

func (*binaryExpr) exprNode() {}

type unaryExpr struct {
	op     string
	expr   expr
	prefix bool
}

func (*unaryExpr) exprNode() {}

type callExpr struct {
	callee expr
	args   []expr
}

func (*callExpr) exprNode() {}

type newExpr struct {
	callee expr
	args   []expr
}

func (*newExpr) exprNode() {}

type memberExpr struct {
	object   expr
	property string
}

func (*memberExpr) exprNode() {}

type arrayExpr struct {
	elements []expr
}

func (*arrayExpr) exprNode() {}

type objectExpr struct {
	properties []property
}

func (*objectExpr) exprNode() {}

type property struct {
	key   string
	value expr
}

type regexpExpr struct {
	value string
}

func (*regexpExpr) exprNode() {}

type boolExpr struct {
	value bool
}

func (*boolExpr) exprNode() {}

type nullExpr struct{}

func (*nullExpr) exprNode() {}

type seqExpr struct {
	exprs []expr
}

func (*seqExpr) exprNode() {}

type varDecl struct {
	name string
	init expr
	kind string
}

func (*varDecl) stmtNode() {}

type exprStmt struct {
	expr expr
}

func (*exprStmt) stmtNode() {}

type blockStmt struct {
	stmts []stmt
}

func (*blockStmt) stmtNode() {}

type ifStmt struct {
	condition  expr
	consequent stmt
	alternate  stmt
}

func (*ifStmt) stmtNode() {}

type forStmt struct {
	init stmt
	body stmt
}

func (*forStmt) stmtNode() {}

type whileStmt struct {
	condition expr
	body      stmt
}

func (*whileStmt) stmtNode() {}

type funcDecl struct {
	name string
	body *blockStmt
}

func (*funcDecl) stmtNode() {}

type funcExpr struct {
	body *blockStmt
}

func (*funcExpr) exprNode() {}

type returnStmt struct {
	expr expr
}

func (*returnStmt) stmtNode() {}

type breakStmt struct{}

func (*breakStmt) stmtNode() {}

type continueStmt struct{}

func (*continueStmt) stmtNode() {}

func looksLikeURL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "//") ||
		strings.HasPrefix(s, "www.") {
		return true
	}
	return strings.Contains(s, "/")
}

func resolveEndpoint(endpoint string, baseURL string) string {
	if endpoint == "" {
		return endpoint
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	if strings.HasPrefix(endpoint, "//") {
		return "https:" + endpoint
	}
	if strings.HasPrefix(endpoint, "/") {
		u, _ := url.Parse(baseURL)
		if u != nil {
			return u.Scheme + "://" + u.Host + endpoint
		}
	}
	if baseURL != "" {
		u, _ := url.Parse(baseURL)
		if u != nil {
			base := u.Scheme + "://" + u.Host
			if !strings.HasSuffix(base, "/") {
				base += "/"
			}
			return base + endpoint
		}
	}
	return endpoint
}

// apiEndpointSignals are substrings that, when present in a path- or URL-shaped
// string, strongly indicate a dynamic/API endpoint rather than a static asset
// or plain page. Used by the string-harvesting fallback.
var apiEndpointSignals = []string{
	"/api/", "/api.", "_api", "/rest", "restapi", "/rpc", "/graphql", "graphql",
	"/ajax", "ajax.php", "ajax_", "-ajax", "/xhr", "/fetch/",
	"/v1/", "/v2/", "/v3/", "/v4/",
	"wp-json", "admin-ajax", "xmlrpc.php",
	"/bitrix/services/", "/bitrix/tools/", "/services/main/ajax",
	"?action=", "&action=", "action.php", "handler.php", "gateway.php",
	"service.php", "endpoint.php", "route.php", "soap.php",
	"/webhook", "/endpoint", "/gateway", "/handler", "/callback",
	"/socket.io", "/sse/", "/oauth", "/token", "/graphiql",
}

// placeholderStripRE removes dynamic {…} path placeholders so the API-shape
// test can evaluate a template endpoint like /api/admin/cameras/{e}/snapshot.
var placeholderStripRE = regexp.MustCompile(`\{[^}]*\}`)

// apiEndpointShape reports whether a resolved call target (which may still
// carry {var} placeholders for dynamic segments) carries an API signal.
func apiEndpointShape(s string) bool {
	if strings.Contains(s, "{") {
		return looksLikeAPIEndpoint(placeholderStripRE.ReplaceAllString(s, ""))
	}
	return looksLikeAPIEndpoint(s)
}

// looksLikeAPIEndpoint applies a strict test suitable for the harvesting
// fallback, where we have no call-site context. It requires the string to be
// path- or URL-shaped, not a static asset, and to carry an explicit API signal
// (either a configured API pattern or one of apiEndpointSignals).
func looksLikeAPIEndpoint(s string) bool {
	if len(s) < 4 || len(s) > 512 {
		return false
	}
	// Reject anything with whitespace or markup/expression characters — those
	// are almost never a single clean endpoint literal.
	if strings.ContainsAny(s, " \t\r\n<>(){}\"'`\\|") {
		return false
	}
	pathish := strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "//")
	if !pathish {
		return false
	}
	if isLikelyNotAPI(s) {
		return false
	}
	lower := strings.ToLower(s)
	// Dynamic .php handlers are endpoints when they carry a query string.
	if strings.Contains(lower, ".php") && strings.Contains(s, "?") {
		return true
	}
	if linker.MatchesAPIPattern(s) {
		return true
	}
	for _, sig := range apiEndpointSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

func isLikelyNotAPI(s string) bool {
	if s == "" {
		return true
	}
	fileIndicators := []string{
		".html", ".htm", ".css",
		".png", ".jpg", ".jpeg",
		".gif", ".svg", ".ico", ".woff", ".woff2", ".ttf", ".eot",
		".mp4", ".webm", ".mp3", ".pdf", ".doc", ".docx", "#",
		"javascript:", "mailto:", "tel:", "data:", "about:", "chrome:",
	}
	strictSuffix := []string{
		".js", ".mjs", ".ts",
	}
	lower := strings.ToLower(s)
	for _, ind := range fileIndicators {
		if strings.HasSuffix(lower, ind) || strings.Contains(lower, ind) {
			return true
		}
	}
	for _, ind := range strictSuffix {
		if strings.HasSuffix(lower, ind) {
			return true
		}
	}
	for _, h := range headerNames {
		if lower == h {
			return true
		}
	}
	if len(s) < 3 {
		return true
	}
	if !strings.HasPrefix(s, "/") &&
		!strings.HasPrefix(s, "./") &&
		!strings.HasPrefix(s, "../") &&
		!strings.HasPrefix(s, "http://") &&
		!strings.HasPrefix(s, "https://") &&
		!strings.HasPrefix(s, "//") &&
		!strings.Contains(s, "/") {
		return true
	}
	return false
}

var headerNames = []string{
	"content-length", "content-type", "content-encoding", "set-cookie",
	"cookie", "accept", "authorization", "user-agent", "cache-control",
	"origin", "referer", "x-requested-with", "x-csrf-token",
	"getallresponseheaders", "getresponseheader",
}

func (c *context) resolveNewExpr(ne *newExpr) (match callMatch, objName, methodName, httpMethod string) {
	switch callee := ne.callee.(type) {
	case *identExpr:
		if callee.name == "Request" {
			return callNewRequest, "Request", "", "GET"
		}
		if callee.name == "WebSocket" {
			return callNewRequest, "WebSocket", "", "WS"
		}
		if callee.name == "EventSource" {
			return callNewRequest, "EventSource", "", "GET"
		}
		if callee.name == "XMLHttpRequest" {
			return callNone, "", "", ""
		}
	case *memberExpr:
		if id, ok := callee.object.(*identExpr); ok && id.name == "XMLHttpRequest" {
			return callNone, "", "", ""
		}
	}
	return callNone, "", "", ""
}

func (c *context) applyMatch(match callMatch, args []expr, objName, methodName, httpMethod string) {
	switch match {
	case callDirect, callNewRequest:
		if len(args) > 0 {
			// axios({url, method, data}) / request({url, ...}): the options
			// object form is a config, not a URL.
			if oe, ok := args[0].(*objectExpr); ok {
				c.analyzeConfigObj(oe)
				return
			}
			if id, ok := args[0].(*identExpr); ok {
				if o2, ok := c.varInits[id.name]; ok {
					if oe, ok := o2.(*objectExpr); ok {
						c.analyzeConfigObj(oe)
						return
					}
				}
			}
		}
		var url string
		if len(args) > 0 {
			url = c.resolveURLArg(args[0])
		}
		if url != "" {
			c.addLink(url, "js-api", objName+"."+methodName, httpMethod)
		}
		// fetch(url, init) / new Request(url, init): capture method/body/headers.
		method := httpMethod
		var body string
		var headers []contract.NameValue
		if len(args) > 1 {
			cfg := c.parseInit(args[1])
			if cfg.method != "" {
				method = cfg.method
			}
			body, headers = cfg.body, cfg.headers
		}
		if url != "" {
			c.addObs(url, method, headers, body)
		}

	case callMethod:
		var url string
		if len(args) > 0 {
			url = c.resolveURLArg(args[0])
		}
		if url != "" {
			// Routers and non-HTTP libs also expose .get('/path'); for GET
			// calls require an explicit API signal so page routes do not
			// masquerade as endpoints. Other verbs are call-specific enough.
			if strings.EqualFold(methodName, "get") && !apiEndpointShape(url) {
				break
			}
			c.addLink(url, "js-api", objName+"."+methodName, httpMethod)
		}
		// Bitrix RPC dispatch: BX.ajax.runAction('Namespace.Method', {data:{...}})
		// POSTs to /bitrix/services/main/ajax.php with the action in the body.
		if objName == "BX" && strings.HasPrefix(methodName, "ajax.runAction") {
			action := ""
			if len(args) > 0 {
				action = strings.TrimSpace(c.evalExpr(args[0]))
			}
			var payload map[string]any
			if len(args) > 1 {
				cfg := c.parseInit(args[1])
				if cfg.body != "" {
					var m map[string]any
					if json.Unmarshal([]byte(cfg.body), &m) == nil {
						payload = m
					}
				}
			}
			if payload == nil {
				payload = map[string]any{}
			}
			payload["action"] = stringOr(action, "<action>")
			payload["mode"] = "ajax"
			b, _ := json.Marshal(payload)
			c.addObs(resolveEndpoint("/bitrix/services/main/ajax.php", c.sourceURL),
				"POST", []contract.NameValue{{Name: "x-requested-with", Value: "XMLHttpRequest"}}, string(b))
			return
		}
		// obj.get/post/... First arg is the endpoint; a second object argument
		// may carry method/headers/body (axios-style), otherwise args[1] is the
		// payload for POST/PUT/PATCH.
		m := strings.ToUpper(stringOr(httpMethod, "GET"))
		var headers []contract.NameValue
		var body string
		if len(args) > 1 {
			cfg := c.parseInit(args[1])
			headers = cfg.headers
			if cfg.method != "" {
				m = cfg.method
			}
			if cfg.body != "" {
				body = cfg.body
			} else if m == "POST" || m == "PUT" || m == "PATCH" {
				body = c.bodyExpr(args[1])
			}
		}
		if url != "" {
			c.addObs(url, m, headers, body)
		}

	case callConfig:
		if len(args) > 0 {
			if oe, ok := args[0].(*objectExpr); ok {
				c.analyzeConfigObj(oe)
			} else if id, ok := args[0].(*identExpr); ok {
				if val, exists := c.vars[id.name]; exists && looksLikeURL(val) {
					c.addLink(val, "js-api", objName+"."+methodName, "")
				}
				if o2, ok := c.varInits[id.name]; ok {
					if oe, ok := o2.(*objectExpr); ok {
						c.analyzeConfigObj(oe)
					}
				}
			}
		}

	case callXHROpen:
		var url string
		if len(args) > 1 {
			url = c.resolveURLArg(args[1])
			if httpMethod == "" && len(args) > 0 {
				httpMethod = c.evalExpr(args[0])
			}
		}
		if url != "" {
			c.addLink(url, "js-api", "XHR.open", httpMethod)
			c.addObs(url, httpMethod, nil, "")
		}
	}
}
