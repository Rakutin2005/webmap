package jsanalyzer

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"apimap/internal/linker"
)

func Parse(jsContent string, sourceURL string, fullInfo bool) (result []linker.Link) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
		}
	}()
	ctx := newContext(sourceURL, fullInfo)
	tokens := tokenize(jsContent)
	p := newParser(tokens)
	stmts := p.parseProgram()

	ctx.analyze(stmts)

	// Robust fallback: many endpoints are built at runtime (e.g.
	// fetch(this.ajaxUrl + '?' + params)) or live inside config/JSON blobs the
	// call-graph analysis can't trace back. Harvest endpoint-looking string
	// literals directly from the token stream so those still surface. This runs
	// on tokens (not the AST) so it works even when parsing bails out on minified
	// or exotic syntax.
	ctx.harvestTokens(tokens)
	return ctx.links
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

type context struct {
	sourceURL string
	fullInfo  bool
	vars      map[string]string
	props     map[string]map[string]string
	links     []linker.Link
	seen      map[string]bool
}

func newContext(sourceURL string, fullInfo bool) *context {
	return &context{
		sourceURL: sourceURL,
		fullInfo:  fullInfo,
		vars:      map[string]string{},
		props:     map[string]map[string]string{},
		seen:      map[string]bool{},
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
				return callNewRequest, "Request", "", "GET"
			}
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
	for _, prop := range oe.properties {
		if configPropKeys[prop.key] {
			urlVal = c.evalExpr(prop.value)
			if urlVal == "" {
				if id, ok := prop.value.(*identExpr); ok {
					urlVal = c.vars[id.name]
				}
			}
		}
		if prop.key == "method" || prop.key == "type" {
			method = c.evalExpr(prop.value)
		}
	}
	if urlVal != "" {
		c.addLink(urlVal, "js-api", "config", method)
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
	typ   tokenType
	value string
	pos   int
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
			parts := []string{}
			partStart := j
			for j < len(runes) {
				if runes[j] == '\\' {
					j += 2
					continue
				}
				if runes[j] == '$' && j+1 < len(runes) && runes[j+1] == '{' {
					if partStart < j {
						parts = append(parts, string(runes[partStart:j]))
					}
					depth := 1
					j += 2
					for j < len(runes) && depth > 0 {
						if runes[j] == '{' {
							depth++
						} else if runes[j] == '}' {
							depth--
						}
						j++
					}
					partStart = j
					continue
				}
				if runes[j] == '`' {
					if partStart < j {
						parts = append(parts, string(runes[partStart:j]))
					}
					emit(tokTemplate, strings.Join(parts, "\x00"))
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
	t := p.advance()
	if t.typ == tokEOF {
		return t
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

func (p *parser) parseVarDecl() *varDecl {
	t := p.advance()
	kind := t.value

	nameTok := p.expect(tokIdent, "")
	name := nameTok.value

	p.expect(tokOp, "=")

	init := p.parseExpr(0)

	p.skipSemicolons()

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
		if len(parts) == 1 {
			left = &stringExpr{value: parts[0]}
		} else {
			var exprs []expr
			for _, part := range parts {
				exprs = append(exprs, &stringExpr{value: part})
			}
			left = &templateExpr{parts: exprs}
		}

	case tokNumber:
		p.advance()
		left = &numberExpr{value: t.value}

	case tokIdent:
		p.advance()
		left = &identExpr{name: t.value}

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
		case "async":
			p.advance()
			left = &identExpr{name: "async"}
		default:
			p.advance()
			left = &identExpr{name: t.value}
		}

	case tokPunct:
		if t.value == "(" {
			p.advance()
			expr := p.parseExpr(0)
			p.expect(tokPunct, ")")
			left = expr
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
		var url string
		if len(args) > 0 {
			url = c.evalExpr(args[0])
			if url == "" {
				if id, ok := args[0].(*identExpr); ok {
					url = c.vars[id.name]
				}
			}
		}
		if url != "" {
			c.addLink(url, "js-api", objName+"."+methodName, httpMethod)
		}

	case callMethod:
		var url string
		if len(args) > 0 {
			url = c.evalExpr(args[0])
			if url == "" {
				if id, ok := args[0].(*identExpr); ok {
					url = c.vars[id.name]
				}
			}
		}
		if url != "" {
			c.addLink(url, "js-api", objName+"."+methodName, httpMethod)
		}

	case callConfig:
		if len(args) > 0 {
			if oe, ok := args[0].(*objectExpr); ok {
				c.analyzeConfigObj(oe)
			} else if id, ok := args[0].(*identExpr); ok {
				if val, exists := c.vars[id.name]; exists && looksLikeURL(val) {
					c.addLink(val, "js-api", objName+"."+methodName, "")
				}
			}
		}

	case callXHROpen:
		var url string
		if len(args) > 1 {
			url = c.evalExpr(args[1])
			if url == "" {
				if id, ok := args[1].(*identExpr); ok {
					url = c.vars[id.name]
				}
			}
			if httpMethod == "" && len(args) > 0 {
				httpMethod = c.evalExpr(args[0])
			}
		}
		if url != "" {
			c.addLink(url, "js-api", "XHR.open", httpMethod)
		}
	}
}
