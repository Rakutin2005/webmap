package jsanalyzer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"apimap/internal/contract"
	"apimap/internal/linker"
)

func Parse(jsContent string, sourceURL string, fullInfo bool) (result []linker.Link, obs []contract.Observation) {
	result, obs, _ = ParseWithParams(jsContent, sourceURL, fullInfo)
	return result, obs
}

// ParseWithParams behaves like Parse and additionally reports the parameter
// names recovered from request builders (URLSearchParams/FormData), each
// classified by kind and by what it feeds. Names written into a builder that is
// then passed to a request are also attached to that endpoint as inferred query
// parameters, so the contract lists them without inventing a value.
func ParseWithParams(jsContent string, sourceURL string, fullInfo bool) (result []linker.Link, obs []contract.Observation, params []linker.ParamRef) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			obs = nil
			params = nil
		}
	}()
	ctx := newContext(sourceURL, fullInfo)
	tokens := tokenize(jsContent)
	p := newParser(tokens)
	stmts := p.parseProgram()

	// Declarations first, then fields, then requests: a request can only be
	// attributed once the builders and addresses around it are known, and in
	// real code they are often written after the request itself.
	ctx.collectDeclarations(stmts)
	ctx.scanParamBuilders(tokens)
	ctx.linkScopeFields()
	ctx.analyze(stmts)
	ctx.resolvePending()

	// Token-level passes for signals the AST analysis cannot reach: minified
	// code where the parser gives up, and clients with arbitrary names.
	ctx.scanRequestSites(tokens)
	ctx.scanResponseFields(tokens)
	ctx.bindBuilderParams(tokens)
	ctx.scanCalls(tokens, jsContent)

	// Robust fallback: many endpoints are built at runtime (e.g.
	// fetch(this.ajaxUrl + '?' + params)) or live inside config/JSON blobs the
	// call-graph analysis can't trace back. Harvest endpoint-looking string
	// literals directly from the token stream so those still surface.
	ctx.harvestTokens(tokens)
	ctx.flushXHR()
	ctx.attachResponseFields()
	ctx.attachInferredQuery()
	return ctx.links, ctx.obs, ctx.paramRefs
}

// attachResponseFields copies the statically inferred response schema onto the
// observations of the endpoints it was collected for.
func (c *context) attachResponseFields() {
	if len(c.respField) == 0 {
		return
	}
	for i := range c.obs {
		set := c.respField[c.obs[i].URL]
		if len(set) == 0 {
			// The AST may have resolved query parameters the token scan did not
			// see, so fall back to matching the endpoint without its query.
			for endpoint, fields := range c.respField {
				if baseURLOf(endpoint) == baseURLOf(c.obs[i].URL) {
					set = fields
					break
				}
			}
		}
		if len(set) == 0 {
			continue
		}
		paths := make([]string, 0, len(set))
		for p := range set {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		c.obs[i].ResponseFields = make([]contract.ResponseField, 0, len(paths))
		for _, p := range paths {
			c.obs[i].ResponseFields = append(c.obs[i].ResponseFields, contract.ResponseField{Path: p, Kind: set[p]})
		}
	}
}

// builderQueryNames and builderFormNames recognise builders by their variable
// name when the construction was not seen (minified code, helper scope).
var (
	builderQueryNames = map[string]bool{
		"urlsearchparams": true, "searchparams": true, "params": true,
		"query": true, "qs": true, "sp": true, "search": true, "usp": true,
		"queryparams": true, "filters": true, "sort": true,
	}
	builderFormNames = map[string]bool{
		"formdata": true, "form": true, "fd": true, "multipart": true,
	}
)

// scanParamBuilders classifies the parameters written into request builders.
// A builder is recognised structurally (new URLSearchParams()/new FormData())
// and, failing that, by the conventional receiver names. Names that end up in a
// request are bound to that endpoint; the rest describe the page's own query
// string and are reported without being attributed to any API.
func (c *context) scanParamBuilders(tokens []token) {
	c.markLocationBuilders(tokens)
	for i := range tokens {
		t := tokens[i]
		if t.typ != tokIdent && t.typ != tokKeyword {
			continue
		}
		if t.value != "set" && t.value != "append" {
			continue
		}
		if i+1 >= len(tokens) || tokens[i+1].typ != tokPunct || tokens[i+1].value != "(" {
			continue
		}
		if i+2 >= len(tokens) || tokens[i+2].typ != tokString {
			continue
		}
		receiver := ""
		if i >= 2 && tokens[i-1].typ == tokPunct && tokens[i-1].value == "." &&
			(tokens[i-2].typ == tokIdent || tokens[i-2].typ == tokKeyword) {
			receiver = tokens[i-2].value
		}
		if receiver == "" {
			continue
		}
		name := tokens[i+2].value
		kind, known := c.builderKind[receiver]
		if !known {
			lower := strings.ToLower(receiver)
			switch {
			case builderQueryNames[lower]:
				kind = linker.ParamQuery
			case builderFormNames[lower]:
				kind = linker.ParamForm
			default:
				kind = linker.ParamUnknown
			}
		}
		// A set() on a builder marks the name as belonging to a request; on an
		// unrecognised receiver we still report it, flagged as unclassified.
		owner := linker.OwnerUnknown
		if known || builderIsQueryish(lowerKind(kind)) {
			owner = c.ownerForBuilder(receiver)
		}
		c.recordParamRef(name, kind, owner)
		c.noteBuilderField(receiver, name)
	}
}

func lowerKind(k linker.ParamKind) linker.ParamKind { return k }

// markLocationBuilders flags builders seeded from the current page URL:
// `const sp = new URLSearchParams(new URL(location.href).searchParams)`. Their
// parameters describe page state, not an API request.
func (c *context) markLocationBuilders(tokens []token) {
	for i := range tokens {
		if tokens[i].typ != tokIdent {
			continue
		}
		if tokens[i].value != "URLSearchParams" && tokens[i].value != "FormData" {
			continue
		}
		if i+1 >= len(tokens) || tokens[i+1].typ != tokPunct || tokens[i+1].value != "(" {
			continue
		}
		// Look for a location-derived seed inside the constructor.
		seeded := false
		for j := i + 2; j < len(tokens) && j < i+40; j++ {
			if tokens[j].typ == tokIdent && (tokens[j].value == "location" || tokens[j].value == "href") {
				seeded = true
				break
			}
			if tokens[j].typ == tokPunct && tokens[j].value == ")" {
				break
			}
		}
		// Assign the builder to a variable, if there is one. The "new" keyword
		// sits between the assignment and the constructor, so step over it.
		j := i - 1
		if j >= 0 && tokens[j].typ == tokKeyword && tokens[j].value == "new" {
			j--
		}
		if j >= 1 && tokens[j].typ == tokOp && tokens[j].value == "=" {
			owner := tokens[j-1]
			if owner.typ == tokIdent {
				c.formData[owner.value] = map[string]string{}
				if tokens[i].value == "FormData" {
					c.builderKind[owner.value] = linker.ParamForm
				} else {
					c.builderKind[owner.value] = linker.ParamQuery
				}
				if seeded {
					c.builderFromLocation[owner.value] = true
				}
			}
		}
	}
}

// builderIsQueryish reports whether a kind represents a query-string builder.
func builderIsQueryish(k linker.ParamKind) bool { return k == linker.ParamQuery }

// ownerForBuilder decides what a builder feeds: a request it was passed to, or
// the current page's query string.
func (c *context) ownerForBuilder(name string) linker.ParamOwner {
	if c.builderFromLocation[name] {
		return linker.OwnerLocation
	}
	for endpoint := range c.paramBound {
		if c.builderFeeds(endpoint, name) {
			return linker.OwnerRequest
		}
	}
	if c.paramBoundFor(name) {
		return linker.OwnerRequest
	}
	return linker.OwnerUnknown
}

// builderFeeds reports whether a builder variable was used by a request.
func (c *context) builderFeeds(endpoint, builder string) bool {
	return c.boundBuilders[endpoint] != nil && c.boundBuilders[endpoint][builder]
}

// paramBoundFor reports whether any endpoint consumed this builder.
func (c *context) paramBoundFor(builder string) bool {
	for _, feeds := range c.boundBuilders {
		if feeds[builder] {
			return true
		}
	}
	return false
}

// noteBuilderField accumulates a name written into a tracked builder so it can
// be attached to the request the builder is passed to.
func (c *context) noteBuilderField(builder, name string) {
	if _, known := c.formData[builder]; !known {
		return
	}
	c.formData[builder][name] = ""
	if fn := c.builderScope[builder]; fn != "" {
		c.noteScopeField(fn, name)
	}
}

// recordParamRef folds one occurrence into the classified report list.
func (c *context) recordParamRef(name string, kind linker.ParamKind, owner linker.ParamOwner, endpoints ...string) {
	if name == "" {
		return
	}
	// One name yields one report line. The same name can be seen several times
	// for a script (unattributed first, then attributed once its builder is
	// bound to a request), so the strongest owner wins and the counts add up.
	for i := range c.paramRefs {
		if c.paramRefs[i].Name != name || c.paramRefs[i].Kind != kind {
			continue
		}
		_ = i
		c.paramRefs[i].Count++
		if owner > c.paramRefs[i].Owner {
			c.paramRefs[i].Owner = owner
		}
		for _, ep := range endpoints {
			if !containsString(c.paramRefs[i].Endpoints, ep) {
				c.paramRefs[i].Endpoints = append(c.paramRefs[i].Endpoints, ep)
			}
		}
		return
	}
	c.paramRefs = append(c.paramRefs, linker.ParamRef{Name: name, Kind: kind, Owner: owner, Count: 1, Endpoints: append([]string(nil), endpoints...)})
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// bindBuilderToRequest records that a builder variable feeds an endpoint, and
// attaches the names written into it as inferred query parameters.
// pendingBinding is a request whose parameters were attributed before its
// address could be resolved.
type pendingBinding struct {
	scope     string
	urlExpr   expr
	method    string
	headers   []contract.NameValue
	body      string
	source    string
	inferredM bool
}

// bindRequestScopes attributes everything a request carries, and remembers the
// call so the binding can be completed if the address shows up later.
func (c *context) bindRequestScopes(args []expr, endpoint string, urlExpr expr) {
	c.bindRequestScopesFull(args, endpoint, urlExpr, "", nil, "", "", false)
}

func (c *context) bindRequestScopesFull(args []expr, endpoint string, urlExpr expr, method string, headers []contract.NameValue, body, source string, inferred bool) {
	carrier := c.requestCarrier(endpoint, urlExpr)
	for _, a := range args {
		if b := c.builderInExpr(a); b != "" {
			c.bindBuilderToRequest(b, endpoint)
		}
	}
	for _, sc := range c.scopesInArgs(args) {
		c.bindScopeToRequest(sc, endpoint, carrier)
		if endpoint == "" && urlExpr != nil {
			c.pending = append(c.pending, pendingBinding{
				scope: sc, urlExpr: urlExpr, method: method,
				headers: headers, body: body, source: source, inferredM: inferred,
			})
		}
	}
}

// resolvePending completes the bindings that waited for an address to appear
// later in the file.
func (c *context) resolvePending() {
	for _, p := range c.pending {
		url := c.resolveURLArg(p.urlExpr)
		if url == "" {
			continue
		}
		url = trimURLJoin(url)
		// The request itself was missed when the address was unknown, so it is
		// emitted now; otherwise the parameters would have nowhere to land.
		if url != "" && !isLikelyNotAPI(url) {
			source := p.source
			if source == "" {
				source = "js-api"
			}
			method := p.method
			if method == "" {
				method = "GET"
			}
			c.addLink(url, "js-api", source, method)
			if p.inferredM {
				c.addObsInferred(url, method, p.headers, p.body, true)
			} else {
				c.addObs(url, method, p.headers, p.body)
			}
		}
		c.bindScopeToRequest(p.scope, resolveEndpoint(url, c.sourceURL), "")
	}
	c.pending = nil
}

// collectDeclarations makes one pass over a file recording only the facts a
// request needs to be understood: which method owns which builder, which
// variable received which method's result, and what addresses properties were
// given. It runs before the request walk because real code defines things
// after using them - a component assigns app.ajaxUrl below the method that
// fetches through it - and a single pass would meet the request first and never
// learn where it goes. Nothing here records an endpoint, so running it ahead
// of the real analysis costs no accuracy.
func (c *context) collectDeclarations(stmts []stmt) {
	for _, s := range stmts {
		c.collectStmt(s)
	}
}

func (c *context) collectStmt(s stmt) {
	switch s := s.(type) {
	case *varDecl:
		if ne, ok := s.init.(*newExpr); ok {
			if id, ok := ne.callee.(*identExpr); ok {
				switch id.name {
				case "FormData", "URLSearchParams":
					kind := linker.ParamQuery
					if id.name == "FormData" {
						kind = linker.ParamForm
					}
					c.builderKind[s.name] = kind
					if _, known := c.formData[s.name]; !known {
						c.formData[s.name] = map[string]string{}
					}
					if fn := c.currentFunc(); fn != "" {
						c.builderScope[s.name] = fn
						c.scopeKind[fn] = kind
					}
				}
			}
		}
		// A variable that received a method's result stands in for whatever
		// that method builds.
		if call, ok := s.init.(*callExpr); ok {
			switch callee := call.callee.(type) {
			case *memberExpr:
				c.varScope[s.name] = callee.property
			case *identExpr:
				c.varScope[s.name] = callee.name
			}
		}
		c.collectExpr(s.init)
	case *exprStmt:
		c.collectExpr(s.expr)
	case *blockStmt:
		c.collectDeclarations(s.stmts)
	case *ifStmt:
		c.collectExpr(s.condition)
		c.collectStmt(s.consequent)
		c.collectStmt(s.alternate)
	case *forStmt:
		c.collectStmt(s.init)
		c.collectStmt(s.body)
	case *whileStmt:
		c.collectExpr(s.condition)
		c.collectStmt(s.body)
	case *returnStmt:
		c.collectExpr(s.expr)
	case *tryStmt:
		for _, b := range []*blockStmt{s.body, s.catch, s.finally} {
			if b != nil {
				c.collectDeclarations(b.stmts)
			}
		}
	case *funcDecl:
		c.collectBody(s.name, s.body)
	case *classExpr:
		for _, p := range s.props {
			c.collectExpr(p.value)
		}
	}
}

// collectBody collects a body under a method name, so builders declared inside
// it belong to that method.
func (c *context) collectBody(name string, body *blockStmt) {
	if body == nil {
		return
	}
	c.funcStack = append(c.funcStack, name)
	c.collectDeclarations(body.stmts)
	c.funcStack = c.funcStack[:len(c.funcStack)-1]
}

func (c *context) collectExpr(e expr) {
	switch e := e.(type) {
	case nil:
	case *objectExpr:
		for _, p := range e.properties {
			if fe, ok := p.value.(*funcExpr); ok {
				c.collectBody(p.key, fe.body)
				continue
			}
			c.collectExpr(p.value)
		}
	case *funcExpr:
		c.collectBody("", e.body)
	case *classExpr:
		for _, p := range e.props {
			c.collectExpr(p.value)
		}
	case *newExpr:
		if id, ok := e.callee.(*identExpr); ok {
			// A builder constructor tells the pass that a query builder is
			// being made here; the variable name is recorded by the token
			// pass, which sees the assignment.
			_ = id
		}
	case *callExpr:
		// "p.append('sort', ...)" inside a method: the name belongs to that
		// method. The token pass cannot tell two methods apart when they both
		// use a variable called p, and on a real site they do - one builds the
		// API query, the other the shareable page URL.
		if me, ok := e.callee.(*memberExpr); ok {
			switch me.property {
			case "set", "append", "delete":
				if id, ok := me.object.(*identExpr); ok {
					if _, tracked := c.builderKind[id.name]; tracked && len(e.args) > 0 {
						c.noteScopeField(c.currentFunc(), c.evalExpr(e.args[0]))
					}
				}
			}
		}
		c.collectExpr(e.callee)
		for _, a := range e.args {
			c.collectExpr(a)
		}
	case *binaryExpr:
		if e.op == "=" {
			if me, ok := e.left.(*memberExpr); ok {
				if val := c.evalExpr(e.right); val != "" {
					c.props[exprPath(me)] = map[string]string{"": val}
				}
			}
		}
		c.collectExpr(e.left)
		c.collectExpr(e.right)
	case *seqExpr:
		for _, ex := range e.exprs {
			c.collectExpr(ex)
		}
	}
}

// analyzeObjectMethods walks the methods of an object literal, descending into
// nested literals. Frameworks nest them ("methods: { load() {...} }"), and a
// request two levels down is still a request.
func (c *context) analyzeObjectMethods(oe *objectExpr) {
	for _, p := range oe.properties {
		switch v := p.value.(type) {
		case *funcExpr:
			c.funcStack = append(c.funcStack, p.key)
			if v.body != nil {
				c.analyze(v.body.stmts)
			}
			c.funcStack = c.funcStack[:len(c.funcStack)-1]
		case *objectExpr:
			c.analyzeObjectMethods(v)
		}
	}
}

// analyzeClass walks a class body. Every method is its own scope, so a builder
// declared in one can be tied to the request that consumes it in another.
func (c *context) analyzeClass(e *classExpr) {
	for _, p := range e.props {
		fe, ok := p.value.(*funcExpr)
		if !ok {
			c.analyzeExpr(p.value)
			continue
		}
		c.funcStack = append(c.funcStack, p.key)
		if fe.body != nil {
			c.analyze(fe.body.stmts)
		}
		c.funcStack = c.funcStack[:len(c.funcStack)-1]
	}
}

// linkScopeFields adds the token pass's fields to any method whose own body
// did not contribute them. The declaration pass records fields per method
// directly, which is the accurate source; this only fills the gap for builders
// whose calls the parser never reached.
func (c *context) linkScopeFields() {
	for builder, fn := range c.builderScope {
		if fn == "" {
			continue
		}
		for name := range c.formData[builder] {
			c.noteScopeField(fn, name)
		}
	}
}

// endpointPropNames are the property names that conventionally hold a request
// address. Only these are looked up by name: a minified one-off name is never
// resolved this way, so the shortcut cannot invent an endpoint.
var endpointPropNames = []string{"url", "uri", "endpoint", "api", "path", "ajax", "host", "base"}

// uniqueEndpointProp resolves "this.ajaxUrl" from an object literal declared
// elsewhere in the same script, but only when the name carries a single value.
// Two objects with different values for it means the name is ambiguous, and an
// ambiguous name resolves to nothing rather than to a guess.
func (c *context) uniqueEndpointProp(key string) (string, bool) {
	lower := strings.ToLower(key)
	conventional := false
	for _, part := range endpointPropNames {
		if strings.Contains(lower, part) {
			conventional = true
			break
		}
	}
	if !conventional {
		return "", false
	}
	found := ""
	consider := func(v string) bool {
		v = strings.TrimSpace(v)
		if v == "" || !looksLikeURL(v) {
			return true
		}
		if found != "" && found != v {
			found = ""
			return false
		}
		found = v
		return true
	}
	consistent := true
	for path, props := range c.props {
		if !consistent {
			break
		}
		// Declared as a field of an object literal ("{ajaxUrl: '/api/x'}") and
		// used through a receiver ("this.ajaxUrl"), the same value is reached
		// under different keys, so both spellings are consulted.
		if !consider(props[key]) {
			consistent = false
		}
		if i := strings.LastIndex(path, "."); i >= 0 && path[i+1:] == key {
			if !consider(props[""]) {
				consistent = false
			}
		}
	}
	if !consistent {
		return "", false
	}
	return found, found != ""
}

// isFetchLike reports the transport-level callers, where the first argument is
// the address by construction. Client methods (api.get, axios.post) are
// classified elsewhere and do not need this.
func isFetchLike(callee expr) bool {
	path := exprPath(callee)
	if i := strings.LastIndex(path, "."); i >= 0 {
		path = path[i+1:]
	}
	switch path {
	case "fetch", "ajax", "open", "send":
		return true
	}
	return false
}

// currentFunc is the method or function the analyzer is inside, or "" at
// top level.
func (c *context) currentFunc() string {
	if len(c.funcStack) == 0 {
		return ""
	}
	return c.funcStack[len(c.funcStack)-1]
}

// noteScopeField records that a method builds a parameter, so the names it
// contributes can be attributed when the method is called from a request.
func (c *context) noteScopeField(fn, name string) {
	if fn == "" || name == "" {
		return
	}
	for _, existing := range c.scopeFields[fn] {
		if existing == name {
			return
		}
	}
	c.scopeFields[fn] = append(c.scopeFields[fn], name)
}

// scopesInArgs collects the method names whose builders feed a request,
// wherever they sit in its arguments.
func (c *context) scopesInArgs(args []expr) []string {
	var out []string
	for _, a := range args {
		for _, sc := range c.scopesInExpr(a) {
			already := false
			for _, existing := range out {
				if existing == sc {
					already = true
					break
				}
			}
			if !already {
				out = append(out, sc)
			}
		}
	}
	return out
}

// requestCarrier names the URL expression a request travels through when the
// code does not spell the endpoint out. "this.ajaxUrl" is the truth here: the
// request exists, its literal address is set elsewhere.
func (c *context) requestCarrier(resolved string, arg expr) string {
	if resolved != "" {
		return ""
	}
	return exprLabel(arg)
}

// exprLabel renders the readable shape of an expression, preferring the part
// that names the resource.
func exprLabel(e expr) string {
	switch n := e.(type) {
	case nil:
		return ""
	case *stringExpr:
		return n.value
	case *identExpr:
		return n.name
	case *memberExpr:
		if base := exprLabel(n.object); base != "" {
			return base + "." + n.property
		}
		return n.property
	case *callExpr:
		if me, ok := n.callee.(*memberExpr); ok {
			return exprLabel(me) + "(...)"
		}
		return exprLabel(n.callee) + "(...)"
	case *binaryExpr:
		if n.op == "+" {
			if l := exprLabel(n.left); l != "" {
				return l
			}
			return exprLabel(n.right)
		}
	case *templateExpr:
		for _, part := range n.parts {
			if l := exprLabel(part); l != "" {
				return l
			}
		}
	case *objectExpr:
		return "{...}"
	}
	return ""
}

// scopesInExpr collects method names whose builders flow into an expression:
// a direct call (this.buildParams(1)) or a variable that received its result.
func (c *context) scopesInExpr(e expr) []string {
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		for _, existing := range out {
			if existing == name {
				return
			}
		}
		// The name is kept even when no builder is known for it yet: the
		// fields are joined onto the method after the walk, so filtering here
		// would depend on whether the method was read before its call site.
		out = append(out, name)
	}
	var walk func(expr)
	walk = func(e expr) {
		switch n := e.(type) {
		case nil:
		case *identExpr:
			add(c.varScope[n.name])
		case *memberExpr:
			if id, ok := n.object.(*identExpr); ok {
				add(c.varScope[id.name])
			}
			walk(n.object)
		case *callExpr:
			if me, ok := n.callee.(*memberExpr); ok {
				add(me.property)
			} else if id, ok := n.callee.(*identExpr); ok {
				add(id.name)
			}
			walk(n.callee)
			for _, a := range n.args {
				walk(a)
			}
		case *objectExpr:
			for _, p := range n.properties {
				walk(p.value)
			}
		case *binaryExpr:
			walk(n.left)
			walk(n.right)
		case *templateExpr:
			for _, part := range n.parts {
				walk(part)
			}
		}
	}
	walk(e)
	return out
}

func (c *context) bindBuilderToRequest(builder, endpoint string) {
	fields := c.formData[builder]
	if len(fields) == 0 {
		return
	}
	if endpoint == "" {
		return
	}
	if c.boundBuilders == nil {
		c.boundBuilders = map[string]map[string]bool{}
	}
	if c.boundBuilders[endpoint] == nil {
		c.boundBuilders[endpoint] = map[string]bool{}
	}
	if c.boundBuilders[endpoint][builder] {
		return
	}
	c.boundBuilders[endpoint][builder] = true
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	if fn := c.builderScope[builder]; fn != "" {
		for _, n := range names {
			c.noteScopeField(fn, n)
		}
	}
	c.bindFieldsToRequest(builder, names, c.builderKind[builder], endpoint, "")
}

// bindScopeToRequest attributes the parameters a method builds to the request
// consuming that method's result. The endpoint is empty when the code keeps
// the address in a property: the carrier then names the expression the query
// travels through, which is all that can be said without inventing a URL.
func (c *context) bindScopeToRequest(scope, endpoint, carrier string) {
	names := c.scopeFields[scope]
	if len(names) == 0 {
		return
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	c.bindFieldsToRequest("scope:"+scope, sorted, c.scopeKind[scope], endpoint, carrier)
}

func (c *context) bindFieldsToRequest(builder string, names []string, kind linker.ParamKind, endpoint, carrier string) {
	if kind != linker.ParamForm && endpoint != "" {
		// A URLSearchParams builder contributes query parameters.
		for _, n := range names {
			c.paramBound[endpoint] = append(c.paramBound[endpoint], contract.NameValue{Name: n})
		}
	}
	// Promote exactly the names this builder contributed, so page-state
	// parameters keep their own owner.
	contributed := map[string]bool{}
	for _, n := range names {
		contributed[n] = true
	}
	for i := range c.paramRefs {
		if !contributed[c.paramRefs[i].Name] {
			continue
		}
		if c.paramRefs[i].Owner == linker.OwnerUnknown {
			c.paramRefs[i].Owner = linker.OwnerRequest
		}
		if endpoint != "" {
			if !containsString(c.paramRefs[i].Endpoints, endpoint) {
				c.paramRefs[i].Endpoints = append(c.paramRefs[i].Endpoints, endpoint)
			}
			// The address is known now, so the fallback carrier is obsolete.
			c.paramRefs[i].Carrier = ""
		}
		if carrier != "" && c.paramRefs[i].Carrier == "" {
			c.paramRefs[i].Carrier = carrier
		}
	}
}

// builderInExpr reports a tracked builder variable anywhere inside an
// expression: builders are passed directly, inside an init object
// ({body: sp}), or folded into the URL expression (base + '?' + p).
func (c *context) builderInExpr(e expr) string {
	switch n := e.(type) {
	case nil:
		return ""
	case *identExpr:
		if _, tracked := c.builderKind[n.name]; tracked {
			return n.name
		}
	case *objectExpr:
		for _, p := range n.properties {
			if b := c.builderInExpr(p.value); b != "" {
				return b
			}
		}
	case *binaryExpr:
		if b := c.builderInExpr(n.left); b != "" {
			return b
		}
		return c.builderInExpr(n.right)
	case *callExpr:
		if b := c.builderInExpr(n.callee); b != "" {
			return b
		}
		for _, a := range n.args {
			if b := c.builderInExpr(a); b != "" {
				return b
			}
		}
	case *newExpr:
		for _, a := range n.args {
			if b := c.builderInExpr(a); b != "" {
				return b
			}
		}
	case *memberExpr:
		return c.builderInExpr(n.object)
	}
	return ""
}

// bindBuilderParams links the names written into a builder to the endpoints
// whose argument list mentions that builder. The builder usually travels inside
// an init object (fetch(url, {body: sp})), so the lookup is by token range
// rather than by argument position.
func (c *context) bindBuilderParams(tokens []token) {
	if len(c.builderKind) == 0 {
		return
	}
	for open, endpoint := range c.callURLIndex(tokens) {
		closeIdx := matchParenFrom(tokens, open)
		if closeIdx < 0 {
			continue
		}
		for i := open + 1; i < closeIdx && i < len(tokens); i++ {
			if tokens[i].typ != tokIdent {
				continue
			}
			if _, tracked := c.builderKind[tokens[i].value]; tracked {
				c.bindBuilderToRequest(tokens[i].value, endpoint)
			}
		}
	}
}

// attachInferredQuery copies builder-derived names onto the matching observation
// so the contract lists them without an observed value.
func (c *context) attachInferredQuery() {
	if len(c.paramBound) == 0 {
		return
	}
	for i := range c.obs {
		names := c.paramBound[c.obs[i].URL]
		if len(names) == 0 {
			for endpoint, fields := range c.paramBound {
				if baseURLOf(endpoint) == baseURLOf(c.obs[i].URL) {
					names = fields
					break
				}
			}
		}
		if len(names) == 0 {
			continue
		}
		c.obs[i].InferredQuery = append(c.obs[i].InferredQuery, names...)
	}
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
	c.addEndpointObs(s)
}

// addEndpointObs records that an endpoint path exists in the bundle without a
// resolvable request around it (a bare string literal). It reaches the
// contracts section as an unobserved endpoint instead of being invisible there.
func (c *context) addEndpointObs(rawURL string) {
	if len(c.obs) >= maxObs {
		return
	}
	u := resolveEndpoint(strings.TrimSpace(rawURL), c.sourceURL)
	if u == "" {
		return
	}
	c.addObsFlags(u, "", nil, "", false, true)
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
// scanRequestSites finds request calls purely by walking the token stream.
// The recursive-descent pass is precise but gives up on minified bundles (one
// unbalanced block collapses a whole file into a single node), and this pass
// does not care about parse trees at all: every call-shaped token sequence with
// an endpoint literal is reported, whatever the client is called and however
// the arguments are assembled.
func (c *context) scanRequestSites(tokens []token) {
	// Stack of currently open brackets, so a literal can be attributed to the
	// call that encloses it.
	var stack []int
	for i := range tokens {
		t := tokens[i]
		if t.typ == tokPunct {
			switch t.value {
			case "(", "[", "{":
				stack = append(stack, i)
			case ")", "]", "}":
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
			continue
		}
		if t.typ != tokString || len(stack) == 0 {
			continue
		}
		// Attribute the literal to the nearest enclosing call, skipping any
		// argument object it sits in: f("/x") and f({url: "/x"}) both count.
		open := -1
		for k := len(stack) - 1; k >= 0; k-- {
			if tokens[stack[k]].value == "(" {
				open = stack[k]
				break
			}
		}
		if open < 0 {
			continue
		}
		// A call named open()/sendBeacon() is handled by the dedicated paths.
		verb := calleeVerbBefore(tokens, open)
		receiver := calleeReceiverBefore(tokens, open)
		if verb == "open" || verb == "sendBeacon" {
			continue
		}
		value, _ := tokenStringRun(tokens, i)
		if value == "" || !looksLikeAPIEndpoint(value) {
			continue
		}
		closeIdx := matchParenFrom(tokens, open)
		if closeIdx < 0 {
			continue
		}
		// The literal must be the first argument, or the url property of a
		// config object passed as the first argument.
		isFirst := firstArgIndex(tokens, open, i) == i
		cfg := scanRequestInit(tokens, open+1, closeIdx)
		if !isFirst && !cfg.fromConfig {
			continue
		}
		if !isFirst {
			value = cfg.url
			if value == "" {
				continue
			}
		}
		if !urlArgShape(value) {
			continue
		}
		value = c.applyClientBase(receiver, value)
		method, source := inferMethod(cfg.method, verb, cfg.hasPayload, false)
		inferred := source == srcShape || source == srcDefault
		if method == "" {
			method, inferred = "GET", true
		}
		target := resolveEndpoint(value, c.sourceURL)
		if target == "" {
			continue
		}
		if cfg.payloadIsQuery || (method == "GET" && cfg.hasPayload) {
			if vals := scanObjectPairs(tokens, open+1, closeIdx, "data"); len(vals) > 0 {
				target = appendQuery(target, vals)
				cfg.body = ""
			} else {
				target = appendQuery(target, cfg.params)
			}
		}
		headers := append([]contract.NameValue{}, cfg.headers...)
		headers = append(headers, contentTypeHeader(cfg.contentType)...)
		if isLikelyNotAPI(target) {
			continue
		}
		// The AST pass yields richer records (resolved params, headers,
		// response schema), so skip endpoints it already covered.
		if c.coveredByAST(target) {
			continue
		}
		c.addLink(target, "js-api", "token-scan", method)
		c.addObsFlags(target, method, headers, cfg.body, inferred, false)
	}
}

// firstArgIndex returns the token index where the first argument of the call
// opening at openIdx starts.
func firstArgIndex(tokens []token, openIdx, limit int) int {
	i := openIdx + 1
	if i >= len(tokens) {
		return i
	}
	// Callbacks/config wrappers put the URL inside a nested expression, so step
	// over a leading balanced group when the URL is nested.
	return i
}

// calleeReceiverBefore returns the object a call is invoked on, as it appears
// in the token stream ("api" in api.get(…)), so client instance defaults apply.
func calleeReceiverBefore(tokens []token, openIdx int) string {
	i := openIdx - 1
	if i < 0 {
		return ""
	}
	switch tokens[i].typ {
	case tokIdent, tokKeyword:
		name := tokens[i].value
		if i-1 >= 0 && tokens[i-1].typ == tokPunct && tokens[i-1].value == "." {
			// Walk back over any further receiver hops to the leftmost name.
			j := i - 2
			for j >= 1 && tokens[j].typ == tokPunct && tokens[j].value == "." {
				name = tokens[j-1].value
				j -= 2
			}
			return name
		}
	case tokString:
		if i-1 >= 0 && tokens[i-1].typ == tokPunct && tokens[i-1].value == "[" {
			j := i - 2
			if j >= 0 {
				return tokens[j].value
			}
		}
	}
	return ""
}

// calleeVerbBefore returns the property name the call at openIdx is invoked on
// (a.b(), a["post"]()), or "" for a bare call.
func calleeVerbBefore(tokens []token, openIdx int) string {
	i := openIdx - 1
	if i < 0 {
		return ""
	}
	switch tokens[i].typ {
	case tokIdent, tokKeyword:
		name := tokens[i].value
		// a.b(...) — the name before "(" is the property.
		if i-1 >= 0 && tokens[i-1].typ == tokPunct && tokens[i-1].value == "." {
			return name
		}
		return ""
	case tokString:
		// a["post"](...) — the computed key closes the callee.
		if i-1 >= 0 && tokens[i-1].typ == tokPunct && tokens[i-1].value == "[" {
			return tokens[i].value
		}
		return ""
	}
	return ""
}

// matchParenFrom returns the index of the ")" closing the "(" at openIdx.
func matchParenFrom(tokens []token, openIdx int) int {
	depth := 0
	for i := openIdx; i < len(tokens); i++ {
		if tokens[i].typ != tokPunct {
			continue
		}
		switch tokens[i].value {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 && tokens[i].value == ")" {
				return i
			}
		}
	}
	return -1
}

// matchBracket returns the index of the closer matching the bracket at openIdx,
// for any bracket kind ("{" is closed by "}", not only by ")").
func matchBracket(tokens []token, openIdx int) int {
	if openIdx < 0 || openIdx >= len(tokens) || tokens[openIdx].typ != tokPunct {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(tokens); i++ {
		if tokens[i].typ != tokPunct {
			continue
		}
		switch tokens[i].value {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// tokenStringRun concatenates a run of string literals joined by "+" starting at
// i, so "/api/" + "users" resolves to one endpoint.
func tokenStringRun(tokens []token, i int) (string, int) {
	if tokens[i].typ != tokString {
		return "", i
	}
	value := tokens[i].value
	j := i + 1
	for j+1 < len(tokens) {
		if tokens[j].typ == tokOp && tokens[j].value == "+" && tokens[j+1].typ == tokString {
			value += tokens[j+1].value
			j += 2
			continue
		}
		break
	}
	return value, j
}

// requestInit is what a linear scan can learn about a call's arguments.
type requestInit struct {
	method         string
	url            string
	body           string
	headers        []contract.NameValue
	params         []contract.NameValue
	contentType    string
	hasPayload     bool
	payloadIsQuery bool
	fromConfig     bool
}

// scanRequestInit reads the protocol keys inside a call's argument list.
func scanRequestInit(tokens []token, from, to int) requestInit {
	var cfg requestInit
	depth := 0
	for i := from; i < to && i < len(tokens); i++ {
		t := tokens[i]
		if t.typ == tokPunct {
			switch t.value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
			continue
		}
		if t.typ != tokIdent && t.typ != tokKeyword {
			continue
		}
		key := t.value
		// Key must be followed by ":" to be an object property.
		if i+1 >= to || tokens[i+1].typ != tokPunct || tokens[i+1].value != ":" {
			continue
		}
		valStart := i + 2
		if valStart >= to {
			continue
		}
		lit := tokenLiteralValue(tokens[valStart])
		switch key {
		case "url", "uri", "endpoint", "api":
			// The url key sits inside the argument object, i.e. one level
			// below the call's own parentheses.
			if depth <= 1 {
				cfg.fromConfig = true
			}
			cfg.url = lit
		case "baseURL":
			// A base URL is client configuration, not an endpoint.
			cfg.url = lit
		case "method", "type":
			if lit != "" {
				cfg.method = strings.ToUpper(lit)
			}
		case "contentType":
			cfg.contentType = lit
		case "body", "data":
			cfg.hasPayload = true
			if lit != "" {
				cfg.body = lit
			}
		case "params":
			if lit != "" {
				cfg.hasPayload = true
				cfg.payloadIsQuery = true
			}
		}
	}
	return cfg
}

// scanObjectPairs reads the name/value pairs of an object literal argument
// (jQuery's `data: {page: 2}`) from the token stream.
func scanObjectPairs(tokens []token, from, to int, key string) []contract.NameValue {
	var out []contract.NameValue
	depth := 0
	for i := from; i < to-1 && i < len(tokens); i++ {
		if tokens[i].typ == tokPunct {
			switch tokens[i].value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
			continue
		}
		if tokens[i].value != key || depth != 1 {
			continue
		}
		if tokens[i+1].typ != tokPunct || tokens[i+1].value != ":" {
			continue
		}
		j := i + 2
		if j < to && tokens[j].typ == tokPunct && tokens[j].value == "{" {
			end := matchBracket(tokens, j)
			if end < 0 {
				end = to
			}
			out = append(out, scanFlatObject(tokens, j+1, end)...)
			break
		}
	}
	return out
}

// scanFlatObject reads the pairs of a brace-delimited object literal.
func scanFlatObject(tokens []token, from, to int) []contract.NameValue {
	var out []contract.NameValue
	for i := from; i < to-1 && i < len(tokens); i++ {
		if tokens[i].typ != tokIdent && tokens[i].typ != tokKeyword {
			continue
		}
		if tokens[i+1].typ != tokPunct || tokens[i+1].value != ":" {
			continue
		}
		out = append(out, contract.NameValue{
			Name:  tokens[i].value,
			Value: tokenLiteralValue(tokens[i+2]),
		})
	}
	return out
}

// tokenLiteralValue returns the literal value of a token when it is a string,
// template, number or boolean.
func tokenLiteralValue(t token) string {
	switch t.typ {
	case tokString, tokTemplate, tokNumber:
		return t.value
	case tokKeyword:
		if t.value == "true" || t.value == "false" {
			return t.value
		}
	}
	return ""
}

// coveredByAST reports whether the AST pass already produced a record for this
// endpoint. Comparison ignores the query string: the AST resolves parameters
// properly, so its version supersedes the token-level one.
func (c *context) coveredByAST(target string) bool {
	base := baseURLOf(target)
	for _, k := range c.astObs {
		if k.url == target || baseURLOf(k.url) == base {
			return true
		}
	}
	return false
}

// sameCallPath reports whether two URLs describe the same call, which happens
// when a client instance base was resolved by the AST pass but not by the token
// scan: "/users" and "/api/v2/users" are one request.
func sameCallPath(a, b string) bool {
	ab, bb := baseURLOf(a), baseURLOf(b)
	if ab == bb {
		return true
	}
	pa, pb := urlPathOf(ab), urlPathOf(bb)
	// "/" is the path of every query-only call on the site root, so it must
	// never match by suffix: / and /?svdom_georesolve=1 are distinct calls.
	if len(pb) <= 1 || pa == pb {
		return false
	}
	return strings.HasSuffix(pa, pb)
}

// urlPathOf returns the path of a URL, ignoring scheme and host.
func urlPathOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j:]
		}
		return "/"
	}
	return u
}

// baseURLOf strips the query and fragment from a URL.
func baseURLOf(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

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
		c.addObsFallback(url, method, body)
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

	// Response-shape inference: respVar maps a response callback parameter to
	// the endpoint it belongs to, respElem maps an element callback parameter
	// to its path prefix ("items[]"), and respField collects the field paths
	// read off the response together with the kind inferred from the usage.
	respVar   map[string]string
	respElem  map[string]string
	respField map[string]map[string]string
	// xhrState tracks raw XMLHttpRequest objects by variable name so open(),
	// setRequestHeader() and send() can be correlated into one AJAX contract.
	xhrState map[string]*xhrInfo
	// reqVar tracks `new Request(url, init)` values so a later fetch(req) uses
	// the same endpoint and request shape.
	reqVar map[string]initConfig
	// formData tracks variables created with new FormData()/new URLSearchParams()
	// and the fields appended to them.
	formData map[string]map[string]string
	// headersVar tracks `new Headers()` objects and their set() assignments.
	headersVar map[string]bool
	// clientInst tracks axios.create({baseURL, headers}) instances.
	clientInst map[string]initConfig
	// builderKind records what each tracked builder variable is, so a .set()
	// call can be classified as a query parameter or a form field.
	builderKind map[string]linker.ParamKind
	// builderFromLocation marks builders seeded from the page URL, whose
	// parameters describe page state rather than an API request.
	builderFromLocation map[string]bool
	// paramRefs collects the classified parameter names for the report.
	paramRefs []linker.ParamRef
	// funcStack, scopeFields, scopeKind and varScope model method boundaries:
	// a builder declared inside buildParams() belongs to buildParams(), and a
	// call to it hands those names to whichever request consumes the result.
	// Without this, every such name stayed unattributed.
	funcStack    []string
	scopeFields  map[string][]string
	scopeKind    map[string]linker.ParamKind
	builderScope map[string]string
	varScope     map[string]string
	// pending holds request bindings whose address was not known yet. Real
	// code assigns the endpoint after the method that uses it
	// ("app.ajaxUrl = '/api/...'" below the component), so these are retried
	// once the whole file has been read.
	pending []pendingBinding
	// paramCount maps a name/kind/owner key to its index in paramRefs and
	// folds repeated occurrences of the same name into one record.
	paramCount map[string]int
	// paramBound maps an endpoint to the names a builder contributed to it.
	paramBound map[string][]contract.NameValue
	// boundBuilders records which builder variables feed which endpoint.
	boundBuilders map[string]map[string]bool
	// inChain counts how many response-bearing continuations (then/done/…)
	// wrap the call being analysed; a payload plus a continuation implies POST.
	inChain int
	// astObs indexes the endpoint/method pairs the AST pass already recorded so
	// the token-level fallback does not report the same call twice.
	astObs []obsKey
}

// obsKey identifies a recorded request observation.
type obsKey struct {
	method string
	url    string
}

// formDataBody renders the fields collected on a FormData variable.
func (c *context) formDataBody(name string) string {
	fields := c.formData[name]
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"=<"+k+">")
	}
	return strings.Join(parts, "&")
}

// xhrInfo is the accumulated state of one raw XMLHttpRequest.
type xhrInfo struct {
	url     string
	method  string
	headers []contract.NameValue
	body    string
	sent    bool
}

func newContext(sourceURL string, fullInfo bool) *context {
	return &context{
		sourceURL:           sourceURL,
		fullInfo:            fullInfo,
		vars:                map[string]string{},
		scopeFields:         map[string][]string{},
		scopeKind:           map[string]linker.ParamKind{},
		builderScope:        map[string]string{},
		varScope:            map[string]string{},
		props:               map[string]map[string]string{},
		varInits:            map[string]expr{},
		seen:                map[string]bool{},
		obsSeen:             map[string]bool{},
		respVar:             map[string]string{},
		respElem:            map[string]string{},
		respField:           map[string]map[string]string{},
		xhrState:            map[string]*xhrInfo{},
		reqVar:              map[string]initConfig{},
		formData:            map[string]map[string]string{},
		headersVar:          map[string]bool{},
		clientInst:          map[string]initConfig{},
		builderKind:         map[string]linker.ParamKind{},
		builderFromLocation: map[string]bool{},
		paramBound:          map[string][]contract.NameValue{},
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
	c.addObsFlags(rawURL, method, headers, body, false, false)
}

// addObsFlags records a request observation, flagging a method that was
// inferred from the call shape and an endpoint known only as a path literal.
func (c *context) addObsFlags(rawURL, method string, headers []contract.NameValue, body string, methodInferred, endpointOnly bool) {
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
	if endpointOnly {
		key = "endpoint|" + u
	}
	if c.obsSeen[key] {
		return
	}
	c.obsSeen[key] = true
	if !endpointOnly {
		c.astObs = append(c.astObs, obsKey{method: strings.ToUpper(method), url: u})
	}
	c.obs = append(c.obs, contract.Observation{
		URL:            u,
		Method:         stringOr(method, "GET"),
		Headers:        headers,
		Body:           body,
		MethodInferred: methodInferred,
		EndpointOnly:   endpointOnly,
	})
}

// addObsFallback records an observation from the token-level scanner. When the
// AST pass already described the same endpoint the weaker token-level reading
// (raw, unevaluated body) is dropped so one call is not counted twice.
func (c *context) addObsFallback(rawURL, method, body string) {
	u := resolveEndpoint(strings.TrimSpace(rawURL), c.sourceURL)
	if u == "" {
		return
	}
	for _, seen := range c.astObs {
		if seen.method != strings.ToUpper(method) {
			continue
		}
		if seen.url == u || sameCallPath(seen.url, u) {
			return
		}
	}
	c.addObs(u, method, nil, body)
}

// addObsInferred records an observation whose verb was derived from the call
// shape rather than stated by the code; contracts label it as inferred.
func (c *context) addObsInferred(rawURL, method string, headers []contract.NameValue, body string, inferred bool) {
	c.addObsFlags(rawURL, method, headers, body, inferred, false)
}

func stringOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// initConfig captures the shape of a fetch/Request init object literal.
type initConfig struct {
	baseURL     string
	method      string
	body        string
	headers     []contract.NameValue
	params      []contract.NameValue
	contentType string
	rawJSON     bool
	hasBodyKey  bool
}

// parseInit extracts method/body/headers/params from a fetch or client init
// expression (a direct object literal or a var holding one).
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
		case "body":
			cfg.body = c.bodyExpr(p.value)
			cfg.hasBodyKey = true
		case "data":
			cfg.body = c.bodyExpr(p.value)
			cfg.hasBodyKey = true
		case "params":
			cfg.params = c.paramExpr(p.value)
		case "headers":
			cfg.headers = c.headersExpr(p.value)
		case "contenttype":
			cfg.contentType = c.evalExpr(p.value)
		case "baseurl":
			cfg.baseURL = c.evalExpr(p.value)
		case "processdata":
			// processData:false means the object is sent verbatim as JSON.
			if strings.EqualFold(strings.TrimSpace(c.evalExpr(p.value)), "false") {
				cfg.rawJSON = true
			}
		}
	}
	return cfg
}

// paramExpr renders a query-parameter container (axios params, jQuery data on a
// GET) as ordered name/value pairs.
func (c *context) paramExpr(e expr) []contract.NameValue {
	if id, ok := e.(*identExpr); ok {
		if o2, ok := c.varInits[id.name]; ok {
			e = o2
		}
	}
	oe, ok := e.(*objectExpr)
	if !ok {
		return nil
	}
	out := make([]contract.NameValue, 0, len(oe.properties))
	for _, p := range oe.properties {
		out = append(out, contract.NameValue{Name: p.key, Value: c.evalExpr(p.value)})
	}
	return out
}

// appendQuery adds collected parameters to an endpoint URL so the contract
// inference sees them as query fields.
func appendQuery(rawURL string, params []contract.NameValue) string {
	if rawURL == "" || len(params) == 0 {
		return rawURL
	}
	values := url.Values{}
	for _, p := range params {
		if p.Name == "" {
			continue
		}
		values.Add(p.Name, p.Value)
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + values.Encode()
}

// contentTypeHeader turns a client-declared content type into a request header
// so the body format is inferred the same way as an explicit header.
func contentTypeHeader(contentType string) []contract.NameValue {
	ct := strings.TrimSpace(contentType)
	if ct == "" {
		return nil
	}
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	return []contract.NameValue{{Name: "Content-Type", Value: ct}}
}

// bodyExpr renders a request-body expression: object literals compile to JSON,
// strings pass through, known body builders ($.param, URLSearchParams,
// FormData, JSON.stringify) are evaluated, and unknown/dynamic values collapse
// to null so the format analysis still sees a placeholder.
func (c *context) bodyExpr(e expr) string {
	if e == nil {
		return ""
	}
	if s := c.builtBody(e); s != "" {
		return s
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

// builtBody evaluates the well-known request-body builders so the contract
// shows the real field names instead of "null".
func (c *context) builtBody(e expr) string {
	// new URLSearchParams({a:1}) / new FormData() / new Blob(...)
	if ne, ok := e.(*newExpr); ok {
		if id, ok := ne.callee.(*identExpr); ok && len(ne.args) > 0 {
			switch id.name {
			case "URLSearchParams":
				if vals := c.paramPairs(ne.args[0]); len(vals) > 0 {
					return encodeForm(vals)
				}
			case "FormData":
				// Fields are appended in later statements; report the ones we saw.
				return c.formDataBody(id.name)
			case "Blob", "ArrayBuffer":
				return "null"
			}
		}
	}
	ce, ok := e.(*callExpr)
	if !ok || len(ce.args) == 0 {
		return ""
	}
	me, isMember := ce.callee.(*memberExpr)
	if !isMember {
		return ""
	}
	// $.param({a:1,b:2}) serializes a plain object to form-urlencoded.
	if me.property == "param" {
		if vals := c.paramPairs(ce.args[0]); len(vals) > 0 {
			return encodeForm(vals)
		}
	}
	// JSON.stringify({a:1}) is a JSON body.
	if me.property == "stringify" {
		if v, ok := c.objValue(ce.args[0]); ok {
			switch v.(type) {
			case map[string]any, []any:
				if b, err := json.Marshal(v); err == nil {
					return string(b)
				}
			}
		}
		if s := strings.TrimSpace(c.evalExpr(ce.args[0])); s != "" {
			return s
		}
	}
	return ""
}

// paramPairs renders a plain-object argument as ordered name/value pairs.
func (c *context) paramPairs(e expr) []contract.NameValue {
	if id, ok := e.(*identExpr); ok {
		if o2, ok := c.varInits[id.name]; ok {
			e = o2
		}
	}
	oe, ok := e.(*objectExpr)
	if !ok {
		return nil
	}
	out := make([]contract.NameValue, 0, len(oe.properties))
	for _, p := range oe.properties {
		out = append(out, contract.NameValue{Name: p.key, Value: c.evalExpr(p.value)})
	}
	return out
}

// encodeForm serializes name/value pairs as a form-urlencoded body.
func encodeForm(vals []contract.NameValue) string {
	values := url.Values{}
	for _, v := range vals {
		if v.Name == "" {
			continue
		}
		values.Add(v.Name, v.Value)
	}
	if len(values) == 0 {
		return ""
	}
	return values.Encode()
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
	case *tryStmt:
		for _, b := range []*blockStmt{s.body, s.catch, s.finally} {
			if b != nil {
				c.analyze(b.stmts)
			}
		}
	case *classExpr:
		c.analyzeClass(s)
	case *funcDecl:
		c.funcStack = append(c.funcStack, s.name)
		if s.body != nil {
			c.analyze(s.body.stmts)
		}
		c.funcStack = c.funcStack[:len(c.funcStack)-1]
	case *returnStmt:
		// "return fetch(...)" is how a request is written as often as a bare
		// call; skipping the expression loses the endpoint.
		if s.expr != nil {
			c.analyzeExpr(s.expr)
		}
	case *breakStmt:
	case *continueStmt:
	}
}

func (c *context) analyzeVarDecl(d *varDecl) {
	val := c.evalExpr(d.init)
	if val != "" {
		c.vars[d.name] = val
	}
	// A variable that receives a method's result stands in for whatever that
	// method builds, so the request consuming the variable can be linked back
	// to the method.
	if call, ok := d.init.(*callExpr); ok {
		switch callee := call.callee.(type) {
		case *memberExpr:
			c.varScope[d.name] = callee.property
		case *identExpr:
			c.varScope[d.name] = callee.name
		}
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
		c.funcStack = append(c.funcStack, d.name)
		c.analyze(fe.body.stmts)
		c.funcStack = c.funcStack[:len(c.funcStack)-1]
	}
	if oe, ok := d.init.(*objectExpr); ok {
		c.analyzeObjectMethods(oe)
	}
	// var x = new XMLHttpRequest(): start tracking the instance so its
	// open()/setRequestHeader()/send() calls fold into one AJAX contract.
	if ne, ok := d.init.(*newExpr); ok {
		if isXMLHttpRequestExpr(ne.callee) {
			c.xhrState[d.name] = &xhrInfo{}
		}
		if id, ok := ne.callee.(*identExpr); ok {
			switch id.name {
			case "Request":
				cfg := initConfig{}
				if len(ne.args) > 1 {
					cfg = c.parseInit(ne.args[1])
				}
				if len(ne.args) > 0 && c.resolveURLArg(ne.args[0]) != "" {
					cfg.method = stringOr(cfg.method, "GET")
					c.reqVar[d.name] = cfg
				}
			case "":
				// A variable that receives a method's result stands in for the
				// builder that method fills.
				if call, ok := d.init.(*callExpr); ok {
					if me, ok := call.callee.(*memberExpr); ok {
						c.varScope[d.name] = me.property
					} else if id, ok := call.callee.(*identExpr); ok {
						c.varScope[d.name] = id.name
					}
				}
			case "FormData", "URLSearchParams":
				// Keep fields the token pass already collected for this
				// builder: re-declaring it must not drop them.
				if _, known := c.formData[d.name]; !known {
					c.formData[d.name] = map[string]string{}
				}
				if id.name == "FormData" {
					c.builderKind[d.name] = linker.ParamForm
				} else {
					c.builderKind[d.name] = linker.ParamQuery
				}
				if fn := c.currentFunc(); fn != "" {
					c.builderScope[d.name] = fn
					c.scopeKind[fn] = c.builderKind[d.name]
				}
			case "Headers":
				c.headersVar[d.name] = true
				c.props[d.name] = map[string]string{}
			}
		}
	}
	// const api = axios.create({baseURL, headers}): remember the defaults so
	// api.get('/x') resolves against the instance base URL.
	if ce, ok := d.init.(*callExpr); ok {
		if me, ok := ce.callee.(*memberExpr); ok && me.property == "create" && len(ce.args) > 0 {
			c.clientInst[d.name] = c.parseInit(ce.args[0])
		}
	}
	// "var resp = await fetch(...)" is how async request code is written, and
	// the request lives in the initializer. Without this the whole statement
	// was inert here and only the token pass could still see it.
	if _, isNew := d.init.(*newExpr); !isNew {
		switch d.init.(type) {
		case *callExpr, *binaryExpr, *memberExpr:
			c.analyzeExpr(d.init)
		}
	}
}

// isXMLHttpRequestExpr reports whether a `new` callee is XMLHttpRequest.
func isXMLHttpRequestExpr(callee expr) bool {
	id, ok := callee.(*identExpr)
	return ok && id.name == "XMLHttpRequest"
}

// ajaxBody builds the request body of a raw XHR send(). Form posts assembled
// by string concatenation ("a="+enc(x)+"&b="+enc(y)) are not evaluable, so the
// parameter names are recovered and rendered as placeholders — that is the part
// of an AJAX contract that carries meaning.
func (c *context) ajaxBody(arg expr, headers []contract.NameValue) string {
	if arg == nil {
		return ""
	}
	if formEncoded(headers) {
		if names := c.formFieldNames(arg); len(names) > 0 {
			parts := make([]string, 0, len(names))
			for _, n := range names {
				parts = append(parts, n+"=<"+n+">")
			}
			return strings.Join(parts, "&")
		}
	}
	return c.bodyExpr(arg)
}

// formEncoded reports whether the declared content type is form-urlencoded (or
// absent, in which case a name=value concatenation still reads as a form).
func formEncoded(headers []contract.NameValue) bool {
	for _, h := range headers {
		if !strings.EqualFold(h.Name, "content-type") {
			continue
		}
		v := strings.ToLower(h.Value)
		return strings.Contains(v, "x-www-form-urlencoded")
	}
	return true
}

// formFieldNames collects the parameter names of a concatenated form body.
func (c *context) formFieldNames(e expr) []string {
	var operands []expr
	flattenConcat(e, &operands)
	if len(operands) < 1 {
		return nil
	}
	var b strings.Builder
	for _, op := range operands {
		if s, ok := op.(*stringExpr); ok {
			b.WriteString(s.value)
			continue
		}
		b.WriteString("\x00")
	}
	joined := b.String()
	if !strings.Contains(joined, "=") {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, part := range strings.Split(joined, "&") {
		name := strings.TrimSpace(part)
		if i := strings.Index(name, "="); i >= 0 {
			name = strings.TrimSpace(name[:i])
		}
		name = strings.Trim(name, "\"'` \x00")
		if name == "" || strings.ContainsAny(name, "\x00()[]+ ") || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// flattenConcat collects the operands of a left-leaning `+` chain.
func flattenConcat(e expr, out *[]expr) {
	if be, ok := e.(*binaryExpr); ok && be.op == "+" {
		flattenConcat(be.left, out)
		flattenConcat(be.right, out)
		return
	}
	*out = append(*out, e)
}

var configPropKeys = map[string]bool{
	"url": true, "uri": true, "endpoint": true, "api": true,
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
	case *classExpr:
		c.analyzeClass(e)
	case *objectExpr:
		c.analyzeConfigObj(e)
		c.analyzeObjectMethods(e)
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
				}
				// this.ajaxUrl = "/api/search": the commonest way a class
				// spells out where it talks to. Without this the endpoint
				// stays a bare property reference and every parameter built
				// around it loses its address.
				val := c.evalExpr(e.right)
				if val != "" {
					c.props[path] = map[string]string{"": val}
					if id, ok := me.object.(*identExpr); ok {
						c.vars[id.name+"."+me.property] = val
					}
				}
				objPath := exprPath(me.object)
				if tracked, ok := c.props[objPath]; ok {
					if v, ok := tracked[me.property]; ok {
						c.vars[me.property] = v
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
		c.funcStack = append(c.funcStack, "")
		if e.body != nil {
			c.analyze(e.body.stmts)
		}
		c.funcStack = c.funcStack[:len(c.funcStack)-1]
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

// httpVerbs maps a callee's last property to the verb it issues. A property
// outside this map is not a reason to reject a call — it only means the verb
// has to be inferred from the argument shape instead.
var httpVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH",
	"delete": "DELETE", "head": "HEAD", "options": "OPTIONS",
	"del": "DELETE", "remove": "DELETE", "destroy": "DELETE",
	"create": "POST", "update": "PUT", "add": "POST", "save": "POST",
	"send": "POST", "submit": "POST", "upload": "POST", "download": "GET",
	"list": "GET", "load": "GET", "search": "GET", "query": "GET",
	"fetch": "GET", "read": "GET", "find": "GET", "count": "GET",
	"removeItem": "DELETE", "set": "POST", "insert": "POST", "replace": "PUT",
}

// nonCallers are functions that take a string but are obviously not requests;
// a small denylist keeps the deliberately wide "any call with a URL argument"
// rule from reporting every console.log('/api/…').
var nonCallers = map[string]bool{
	"log": true, "warn": true, "error": true, "info": true, "debug": true,
	"trace": true, "dir": true, "table": true, "assert": true, "time": true,
	"timeEnd": true, "group": true, "groupEnd": true, "push": true,
	"replace": false, "match": true, "test": true, "exec": true,
	"setAttribute": true, "getAttribute": true, "write": true,
	"appendChild": true, "createElement": true, "querySelector": true,
	"querySelectorAll": true, "getElementById": true, "import": true,
	"require": true, "addEventListener": true, "setTimeout": true,
	"setInterval": true, "fetchHeaders": false,
}

// methodSource explains how a verb was determined, for the contract output.
const (
	srcExplicit = "init"    // method/type field in the request object
	srcVerb     = "verb"    // callee property names the verb
	srcShape    = "shape"   // inferred from payload/continuation shape
	srcDefault  = "default" // bare URL, no evidence of a body
)

// inferMethod resolves the verb of a request call and reports how sure we are.
// A payload alone is not evidence of a POST: fetch, XHR, axios and jQuery all
// default to GET, and jQuery sends `data` as the query string unless a type is
// given. Only an explicit field, a verb-shaped property, or a response
// continuation that clearly follows a payload justify naming a verb, and
// anything weaker is reported as inferred rather than asserted silently.
func inferMethod(explicit, verb string, hasPayload, hasContinuation bool) (method, source string) {
	if m := strings.ToUpper(strings.TrimSpace(explicit)); m != "" {
		return m, srcExplicit
	}
	if m, ok := httpVerbs[strings.ToLower(verb)]; ok {
		return m, srcVerb
	}
	// A payload consumed by a response continuation reads as a submission.
	if hasPayload && hasContinuation {
		return "POST", srcShape
	}
	// Otherwise fall back to the client default and say it is a guess.
	return "GET", srcDefault
}

type callMatch int

const (
	callNone       callMatch = iota
	callDirect               // fetch(url), axios(url)
	callMethod               // obj.method(url)
	callConfig               // obj({url: url, method: ...})
	callXHROpen              // xhr.open(method, url)
	callNewRequest           // new Request(url)
	callGeneric              // any call whose first argument is a URL
)

var chainMethods = map[string]bool{
	"end": true, "subscribe": true, "then": true, "catch": true,
	"finally": true, "exec": true, "send": true, "done": true, "fail": true,
}

func (c *context) analyzeCall(ce *callExpr) {
	if me, ok := ce.callee.(*memberExpr); ok {
		// A name written into a builder belongs to the method that declares
		// the builder, which is what makes it attributable when the request
		// using it lives somewhere else.
		if id, isIdent := me.object.(*identExpr); isIdent {
			switch me.property {
			case "set", "append", "delete":
				if _, tracked := c.builderKind[id.name]; tracked && len(ce.args) > 0 {
					c.noteBuilderField(id.name, c.evalExpr(ce.args[0]))
				}
			}
		}
		// Raw XHR lifecycle: correlate setRequestHeader/send with the open()
		// recorded for the same object. Handled before the chain-method
		// shortcut below, which would otherwise swallow send().
		if name, isIdent := me.object.(*identExpr); isIdent {
			if st, tracked := c.xhrState[name.name]; tracked {
				switch me.property {
				case "setRequestHeader":
					if len(ce.args) >= 2 {
						st.headers = append(st.headers, contract.NameValue{
							Name:  c.evalExpr(ce.args[0]),
							Value: c.evalExpr(ce.args[1]),
						})
					}
					return
				case "send":
					if len(ce.args) > 0 {
						st.body = c.ajaxBody(ce.args[0], st.headers)
					}
					st.sent = true
					c.emitXHR(name.name, st)
					return
				case "abort":
					delete(c.xhrState, name.name)
					return
				}
			}
		}
		if chainMethods[me.property] {
			// fetch(url).then(r => r.json()) / client.get(url).then(res => …):
			// bind the callback parameter to the endpoint so the fields read
			// off the response become that endpoint's response schema.
			bearing := responseChains[me.property]
			if bearing {
				// A payload plus a response continuation is the shape of a POST;
				// the depth is visible to the call being analysed.
				c.inChain++
				if endpoint := c.chainEndpoint(me.object); endpoint != "" {
					c.bindResponse(ce.args, endpoint)
				}
			}
			if innerCall, ok := me.object.(*callExpr); ok {
				c.analyzeCall(innerCall)
				if bearing {
					c.inChain--
				}
				return
			}
			if bearing {
				c.inChain--
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
		// Not a request, but its arguments may hold one: component
		// factories (createApp, defineComponent, Vue.component) wrap whole
		// components - methods included - in an object argument. Not walking
		// it loses every request the component makes.
		for _, a := range ce.args {
			c.analyzeExpr(a)
		}
		// A request whose address is kept in a property
		// ("fetch(this.ajaxUrl + '?' + p)") is still a request, and the
		// parameters travelling through it can still be attributed - to the
		// expression rather than to a URL. The names would otherwise be
		// reported with no owner at all.
		if len(ce.args) > 0 && isFetchLike(ce.callee) && c.resolveURLArg(ce.args[0]) == "" {
			if scopes := c.scopesInArgs(ce.args); len(scopes) > 0 {
				carrier := exprLabel(ce.args[0])
				for _, sc := range scopes {
					c.bindScopeToRequest(sc, "", carrier)
				}
			}
		}
		return
	}
	c.applyMatch(match, ce.args, objName, methodName, httpMethod)
}

// responseChains are the chain callbacks that receive the response payload.
var responseChains = map[string]bool{
	"then": true, "done": true, "end": true, "subscribe": true, "exec": true,
}

// transparentChain are the link methods that pass the payload through.
var transparentChain = map[string]bool{
	"then": true, "catch": true, "finally": true, "json": true, "text": true,
	"blob": true, "arrayBuffer": true, "data": true, "body": true,
	"response": true, "result": true, "value": true,
}

// responseMethods are payload methods, not schema fields.
var responseMethods = map[string]bool{
	"map": true, "filter": true, "forEach": true, "reduce": true, "find": true,
	"findIndex": true, "some": true, "every": true, "sort": true, "flat": true,
	"flatMap": true, "push": true, "pop": true, "shift": true, "unshift": true,
	"splice": true, "slice": true, "concat": true, "join": true, "split": true,
	"includes": true, "indexOf": true, "lastIndexOf": true, "keys": true,
	"values": true, "entries": true, "toString": true, "trim": true,
	"replace": true, "replaceAll": true, "toFixed": true, "toPrecision": true,
	"padStart": true, "padEnd": true, "charAt": true, "substring": true,
	"substr": true, "toUpperCase": true, "toLowerCase": true, "startsWith": true,
	"endsWith": true, "repeat": true, "match": true, "getTime": true,
	"toISOString": true, "toLocaleString": true, "toDateString": true,
}

// collectionMethods iterate a payload collection; their callback parameter is
// an element of that collection.
var collectionMethods = map[string]bool{
	"map": true, "forEach": true, "filter": true, "find": true, "findIndex": true,
	"flatMap": true, "some": true, "every": true, "sort": true, "reduce": true,
}

// fieldKindHints maps a payload method to the schema kind it implies.
var fieldKindHints = map[string]string{
	"map": "array", "forEach": "array", "filter": "array", "find": "object",
	"length": "array", "then": "promise", "json": "promise",
	"toFixed": "number", "toPrecision": "number", "getTime": "date",
	"toISOString": "date", "toDateString": "date", "toUpperCase": "string",
	"toLowerCase": "string", "trim": "string", "split": "array",
	"includes": "array", "charAt": "string", "replace": "string",
	"toString": "any", "keys": "object", "entries": "array",
}

// chainEndpoint walks a promise/HTTP chain down to the request that produced
// it and returns that endpoint's absolute URL ("" when the chain holds no
// recognized request).
func (c *context) chainEndpoint(e expr) string {
	switch n := e.(type) {
	case *callExpr:
		if me, ok := n.callee.(*memberExpr); ok && transparentChain[me.property] {
			return c.chainEndpoint(me.object)
		}
		return c.requestURL(n)
	case *memberExpr:
		if transparentChain[n.property] {
			return c.chainEndpoint(n.object)
		}
	}
	return ""
}

// requestURL resolves the endpoint a request call points at without recording
// an observation.
func (c *context) requestURL(ce *callExpr) string {
	match, _, _, _ := c.resolveCall(ce)
	if match == callNone {
		return ""
	}
	if match == callXHROpen {
		if len(ce.args) > 1 {
			return resolveEndpoint(c.evalExpr(ce.args[1]), c.sourceURL)
		}
		return ""
	}
	if len(ce.args) == 0 {
		return ""
	}
	if _, ok := ce.args[0].(*objectExpr); ok {
		return ""
	}
	return resolveEndpoint(c.resolveURLArg(ce.args[0]), c.sourceURL)
}

// bindResponse binds the first callback parameter to the endpoint and records
// every field read off the response inside the callback body.
func (c *context) bindResponse(args []expr, endpoint string) {
	if len(args) == 0 || endpoint == "" {
		return
	}
	fe, ok := args[0].(*funcExpr)
	if !ok || fe.body == nil || len(fe.params) == 0 || fe.params[0] == "" {
		return
	}
	name := fe.params[0]
	prev, had := c.respVar[name]
	c.respVar[name] = endpoint
	c.collectRespStmt(fe.body, endpoint)
	if had {
		c.respVar[name] = prev
	} else {
		delete(c.respVar, name)
	}
}

// recordRespField stores one response field path with its inferred kind.
// responseCallbackMethods are the chain methods whose first callback argument
// receives the response payload. Matching is by shape, not by library, so
// promise chains, jQuery deferreds and custom wrappers all bind.
var responseCallbackMethods = map[string]bool{
	"then": true, "done": true, "success": true, "end": true,
	"complete": true, "always": true, "ok": true,
}

// scanResponseFields infers response schemas by walking the token stream: for
// every response-bearing continuation it binds the callback's first parameter
// to the endpoint of the call it is attached to and records the field paths the
// callback reads. Like the request scan it needs no parse tree, so it keeps
// working on minified bundles where the AST pass collapses.
func (c *context) scanResponseFields(tokens []token) {
	callURL := c.callURLIndex(tokens)
	if len(callURL) == 0 {
		return
	}
	for i := range tokens {
		t := tokens[i]
		if t.typ != tokIdent || !responseCallbackMethods[t.value] {
			continue
		}
		// Must be a member call: .then(...)
		if i == 0 || tokens[i-1].typ != tokPunct || tokens[i-1].value != "." {
			continue
		}
		if i+1 >= len(tokens) || tokens[i+1].typ != tokPunct || tokens[i+1].value != "(" {
			continue
		}
		// The continuation is a sibling of the call it belongs to
		// (client.get(url).done(…)), so walk back to the call's own "(".
		endpoint, callOpen := "", -1
		for k := i - 1; k >= 0; k-- {
			if tokens[k].typ == tokPunct && tokens[k].value == ")" {
				if o := matchingOpenParen(tokens, k); o >= 0 {
					endpoint, callOpen = callURL[o], o
				}
				break
			}
		}
		if endpoint == "" || callOpen < 0 {
			continue
		}
		open := i + 1
		closeIdx := matchParenFrom(tokens, open)
		if closeIdx < 0 {
			continue
		}
		cbFrom, cbTo, param := callbackBody(tokens, open+1, closeIdx)
		if param == "" {
			continue
		}
		for _, f := range memberPathsIn(tokens, cbFrom, cbTo, param) {
			c.recordRespField(endpoint, f.path, f.kind)
		}
	}
	// Callbacks passed inside the call itself: jQuery's $.post(url, data, cb)
	// and the {success: cb} config property.
	for openIdx, endpoint := range callURL {
		closeIdx := matchParenFrom(tokens, openIdx)
		if closeIdx < 0 {
			continue
		}
		if cbFrom, cbTo, param := successCallback(tokens, openIdx+1, closeIdx); param != "" {
			for _, f := range memberPathsIn(tokens, cbFrom, cbTo, param) {
				c.recordRespField(endpoint, f.path, f.kind)
			}
		}
	}
}

// successCallback locates a response callback passed as an argument of a
// request call: the {success: fn} config property, or the trailing function
// argument that jQuery-style helpers pass the response to.
func successCallback(tokens []token, from, to int) (int, int, string) {
	// Config property form: success: function (…) { … } / success: res => …
	for i := from; i < to-1 && i < len(tokens); i++ {
		if tokens[i].typ != tokIdent || !responseCallbackMethods[tokens[i].value] {
			continue
		}
		if tokens[i+1].typ != tokPunct || tokens[i+1].value != ":" {
			continue
		}
		j := i + 2
		if start, end, param := functionBody(tokens, j, to); param != "" {
			return start, end, param
		}
	}
	// Trailing function argument form.
	lastStart, lastEnd, lastParam := -1, -1, ""
	for i := from; i < to && i < len(tokens); i++ {
		if start, end, param := functionBody(tokens, i, to); param != "" {
			lastStart, lastEnd, lastParam = start, end, param
		}
	}
	return lastStart, lastEnd, lastParam
}

// functionBody matches a function or arrow at position i and returns the range
// of its body plus the name of its first parameter.
func functionBody(tokens []token, i, to int) (int, int, string) {
	if i >= to || i >= len(tokens) {
		return -1, -1, ""
	}
	// ident => …
	if tokens[i].typ == tokIdent && i+1 < to && tokens[i+1].typ == tokOp && tokens[i+1].value == "=>" {
		body := i + 2
		if body < to && tokens[body].typ == tokPunct && tokens[body].value == "{" {
			end := matchBracket(tokens, body)
			if end < 0 || end > to {
				end = to
			}
			return body + 1, end, tokens[i].value
		}
		return body, to, tokens[i].value
	}
	// function (…) { … }
	if tokens[i].typ == tokKeyword && tokens[i].value == "function" {
		j := i + 1
		if j < to && tokens[j].typ == tokIdent { // named function expression
			j++
		}
		if j < to && tokens[j].typ == tokPunct && tokens[j].value == "(" {
			end := matchParenFrom(tokens, j)
			if end < 0 {
				return -1, -1, ""
			}
			param := callbackFirstParam(tokens, j, end)
			if end+1 < to && tokens[end+1].typ == tokPunct && tokens[end+1].value == "{" {
				bodyEnd := matchBracket(tokens, end+1)
				if bodyEnd < 0 || bodyEnd > to {
					bodyEnd = to
				}
				return end + 2, bodyEnd, param
			}
		}
	}
	return -1, -1, ""
}

// callURLIndex maps the opening parenthesis of each call to the endpoint it
// requests, so a later continuation can be attributed back to it.
func (c *context) callURLIndex(tokens []token) map[int]string {
	out := map[int]string{}
	var stack []int
	for i := range tokens {
		t := tokens[i]
		if t.typ == tokPunct {
			switch t.value {
			case "(", "[", "{":
				stack = append(stack, i)
			case ")", "]", "}":
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
			continue
		}
		if t.typ != tokString || len(stack) == 0 {
			continue
		}
		open := -1
		for k := len(stack) - 1; k >= 0; k-- {
			if tokens[stack[k]].value == "(" {
				open = stack[k]
				break
			}
		}
		if open < 0 {
			continue
		}
		value, _ := tokenStringRun(tokens, i)
		if value == "" || !looksLikeAPIEndpoint(value) {
			continue
		}
		if _, seen := out[open]; seen {
			continue
		}
		if u := resolveEndpoint(value, c.sourceURL); u != "" {
			out[open] = u
		}
	}
	return out
}

// matchingOpenParen returns the index of the "(" that the ")" at closeIdx closes.
func matchingOpenParen(tokens []token, closeIdx int) int {
	depth := 0
	for i := closeIdx; i >= 0; i-- {
		if tokens[i].typ != tokPunct {
			continue
		}
		switch tokens[i].value {
		case ")", "]", "}":
			depth++
		case "(", "[", "{":
			depth--
			if depth == 0 {
				if tokens[i].value == "(" {
					return i
				}
				return -1
			}
		}
	}
	return -1
}

// callbackBody matches a callback in any spelling (arrow, function expression)
// and returns the range of its body plus the name of its payload parameter.
func callbackBody(tokens []token, from, to int) (int, int, string) {
	for i := from; i < to && i < len(tokens); i++ {
		if start, end, param := functionBody(tokens, i, to); param != "" {
			return start, end, param
		}
	}
	return -1, -1, ""
}

// callbackFirstParam returns the name a callback binds its payload to.
func callbackFirstParam(tokens []token, from, to int) string {
	for i := from; i < to-1 && i < len(tokens); i++ {
		t := tokens[i]
		// (res => …) or (res, …) => …
		if t.typ == tokPunct && t.value == "(" {
			if i+2 < len(tokens) && tokens[i+1].typ == tokIdent {
				return tokens[i+1].value
			}
			continue
		}
		// res => …
		if t.typ == tokIdent && i+1 < len(tokens) &&
			tokens[i+1].typ == tokOp && tokens[i+1].value == "=>" {
			return t.value
		}
		// function (res) { … }
		if t.typ == tokKeyword && t.value == "function" {
			continue
		}
	}
	return ""
}

// payloadReaders are the unmistakable response readers. Unlike the AST pass we
// do not strip names like "data" or "value" here: they are far more often real
// JSON fields than envelope properties.
var payloadReaders = map[string]bool{
	"json": true, "text": true, "blob": true, "arrayBuffer": true,
}

// respField is one inferred response field.
type respField struct {
	path string
	kind string
}

// memberPathsIn collects the field paths a callback reads off the payload it
// was handed, including collection elements: items.map(x => x.price) yields
// "items" and "items[].price".
func memberPathsIn(tokens []token, from, to int, root string) []respField {
	var out []respField
	seen := map[string]bool{}
	for i := from; i < to && i < len(tokens); i++ {
		if tokens[i].typ != tokIdent || tokens[i].value != root {
			continue
		}
		path := ""
		j := i + 1
		for j < to && j < len(tokens) {
			if tokens[j].typ != tokPunct {
				break
			}
			switch tokens[j].value {
			case ".":
				if j+1 >= to {
					return out
				}
				prop := tokens[j+1]
				if prop.typ != tokIdent && prop.typ != tokKeyword {
					return out
				}
				// A payload method is not a field.
				if responseMethods[prop.value] || payloadReaders[prop.value] {
					// The method ends the chain, but the field it was called
					// on has still been read.
					if path != "" && !seen[path] {
						seen[path] = true
						kind := "any"
						if k, hinted := fieldKindHints[path]; hinted {
							kind = k
						}
						out = append(out, respField{path: path, kind: kind})
					}
					return out
				}
				if path == "" {
					path = prop.value
				} else {
					path += "." + prop.value
				}
				if k, hinted := fieldKindHints[prop.value]; hinted && !responseMethods[prop.value] {
					if !seen[path] {
						seen[path] = true
						out = append(out, respField{path: path, kind: k})
					}
				}
				j += 2
				continue
			case "[":
				// t["key"] is a property read; t[expr] indexes the collection.
				if j+1 < to && tokens[j+1].typ == tokString {
					key := tokens[j+1].value
					if path == "" {
						path = key
					} else {
						path += "." + key
					}
					if !seen[path] {
						seen[path] = true
						out = append(out, respField{path: path, kind: "any"})
					}
					j += 3
					continue
				}
				if path != "" {
					idx := path + "[]"
					if !seen[idx] {
						seen[idx] = true
						out = append(out, respField{path: idx, kind: "array"})
					}
				}
				depth := 0
				for ; j < to && j < len(tokens); j++ {
					if tokens[j].typ == tokPunct && tokens[j].value == "[" {
						depth++
					} else if tokens[j].typ == tokPunct && tokens[j].value == "]" {
						depth--
						if depth == 0 {
							j++
							break
						}
					}
				}
				continue
			}
			break
		}
		if path != "" && !seen[path] {
			seen[path] = true
			out = append(out, respField{path: path, kind: "any"})
		}
	}
	return out
}

func (c *context) recordRespField(endpoint, path, kind string) {
	if path == "" || endpoint == "" {
		return
	}
	set := c.respField[endpoint]
	if set == nil {
		set = map[string]string{}
		c.respField[endpoint] = set
	}
	if prev, ok := set[path]; ok && prev != "any" {
		return
	}
	set[path] = kind
}

// respPath resolves an expression to the response field path it reads. The
// response root resolves to an empty path; element callback parameters
// resolve to their collection prefix, so x.price inside
// res.items.map(x => x.price) lands on "items[].price".
func (c *context) respPath(e expr) (string, bool) {
	switch n := e.(type) {
	case *identExpr:
		if _, found := c.respVar[n.name]; found {
			return "", true
		}
		if prefix, found := c.respElem[n.name]; found {
			return prefix, true
		}
		return "", false
	case *memberExpr:
		base, ok := c.respPath(n.object)
		if !ok {
			return "", false
		}
		if base == "" {
			return n.property, true
		}
		return base + "." + n.property, true
	case *callExpr:
		me, isMember := n.callee.(*memberExpr)
		if !isMember {
			return "", false
		}
		if responseMethods[me.property] || transparentChain[me.property] {
			// A payload method (map/json/toFixed…) is not a field itself: the
			// field is what the method is invoked on.
			return c.respPath(me.object)
		}
		base, ok := c.respPath(me)
		if !ok {
			return "", false
		}
		if len(n.args) > 0 {
			return base + "[]", true
		}
		return base, true
	}
	return "", false
}

func (c *context) collectRespStmt(s stmt, endpoint string) {
	switch n := s.(type) {
	case *blockStmt:
		for _, st := range n.stmts {
			c.collectRespStmt(st, endpoint)
		}
	case *varDecl:
		c.collectRespExpr(n.init, endpoint)
	case *exprStmt:
		c.collectRespExpr(n.expr, endpoint)
	case *ifStmt:
		c.collectRespExpr(n.condition, endpoint)
		c.collectRespStmt(n.consequent, endpoint)
		c.collectRespStmt(n.alternate, endpoint)
	case *forStmt:
		c.collectRespStmt(n.init, endpoint)
		c.collectRespStmt(n.body, endpoint)
	case *whileStmt:
		c.collectRespExpr(n.condition, endpoint)
		c.collectRespStmt(n.body, endpoint)
	case *funcDecl:
		if n.body != nil {
			c.collectRespStmt(n.body, endpoint)
		}
	case *returnStmt:
		c.collectRespExpr(n.expr, endpoint)
	}
}

func (c *context) collectRespExpr(e expr, endpoint string) {
	switch n := e.(type) {
	case *memberExpr:
		if path, ok := c.respPath(n); ok {
			kind := "any"
			if k, hinted := fieldKindHints[n.property]; hinted {
				kind = k
			}
			c.recordRespField(endpoint, path, kind)
		} else {
			c.collectRespExpr(n.object, endpoint)
		}
	case *callExpr:
		c.collectCallResp(n, endpoint)
	case *newExpr:
		for _, a := range n.args {
			c.collectRespExpr(a, endpoint)
		}
	case *binaryExpr:
		c.collectRespExpr(n.left, endpoint)
		c.collectRespExpr(n.right, endpoint)
	case *unaryExpr:
		c.collectRespExpr(n.expr, endpoint)
	case *arrayExpr:
		for _, el := range n.elements {
			c.collectRespExpr(el, endpoint)
		}
	case *objectExpr:
		for _, p := range n.properties {
			c.collectRespExpr(p.value, endpoint)
		}
	case *seqExpr:
		for _, el := range n.exprs {
			c.collectRespExpr(el, endpoint)
		}
	case *templateExpr:
		for _, part := range n.parts {
			c.collectRespExpr(part, endpoint)
		}
	case *funcExpr:
		if n.body != nil {
			for _, st := range n.body.stmts {
				c.collectRespStmt(st, endpoint)
			}
		}
	}
}

// collectCallResp records a method call on the payload and, for collection
// methods, binds the callback parameter so element property reads are recorded
// under the collection path.
func (c *context) collectCallResp(n *callExpr, endpoint string) {
	if me, isMember := n.callee.(*memberExpr); isMember {
		if path, ok := c.respPath(n); ok && path != "" {
			kind := "any"
			if k, hinted := fieldKindHints[me.property]; hinted {
				kind = k
			}
			c.recordRespField(endpoint, path, kind)
			if collectionMethods[me.property] {
				c.bindElemCallbacks(n.args, endpoint, path)
			}
		}
	}
	for _, a := range n.args {
		c.collectRespExpr(a, endpoint)
	}
}

// bindElemCallbacks binds the first parameter of collection callbacks to the
// collection prefix ("items[]") for the duration of the callback body.
func (c *context) bindElemCallbacks(args []expr, endpoint, path string) {
	prefix := strings.TrimSuffix(path, "[]") + "[]"
	for _, a := range args {
		fe, ok := a.(*funcExpr)
		if !ok || fe.body == nil || len(fe.params) == 0 || fe.params[0] == "" {
			continue
		}
		name := fe.params[0]
		prev, had := c.respElem[name]
		c.respElem[name] = prefix
		for _, st := range fe.body.stmts {
			c.collectRespStmt(st, endpoint)
		}
		if had {
			c.respElem[name] = prev
		} else {
			delete(c.respElem, name)
		}
	}
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
					if propName == "getJSON" {
						// jQuery's JSON GET shorthand: $.getJSON(url, [data], [cb])
						return callMethod, objName, propName, "GET"
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

	// Structural fallback: a call whose first argument resolves to a URL is a
	// request regardless of who is being called. This is what makes the
	// analyzer work on wrapped, aliased and minified clients instead of only
	// on a fixed list of known library names.
	if c.genericRequestArgs(ce) != "" {
		return callGeneric, calleeObjectName(ce.callee), calleeProperty(ce.callee), ""
	}

	return callNone, "", "", ""
}

// genericRequestArgs reports the endpoint of a structurally detected request
// call, or nil when the call is not a request.
func (c *context) genericRequestArgs(ce *callExpr) string {
	if len(ce.args) == 0 {
		return ""
	}
	verb := calleeProperty(ce.callee)
	if nonCallers[verb] {
		return ""
	}
	first := ce.args[0]
	// Config-object form: anything(url, method, …) / anything({url: …}).
	if oe := c.configObject(first); oe != nil {
		for _, prop := range oe.properties {
			if !configPropKeys[prop.key] {
				continue
			}
			if u := c.resolveURLArg(prop.value); u != "" {
				return u
			}
		}
		return ""
	}
	// Plain form: anything('/api/x').
	if u := c.resolveURLArg(first); u != "" {
		return u
	}
	return ""
}

// requestConfigKeys are the protocol-level keys that mark an object as a
// request description rather than a plain payload.
var requestConfigKeys = map[string]bool{
	"url": true, "uri": true, "endpoint": true, "api": true, "baseURL": true,
	"method": true, "type": true, "body": true, "data": true, "params": true,
	"headers": true, "contentType": true, "processData": true, "dataType": true,
	"responseType": true, "withCredentials": true, "credentials": true,
	"async": true, "timeout": true, "cache": true, "mode": true,
}

// isRequestConfig reports whether an object literal describes a request
// (has protocol keys) as opposed to being a payload or arbitrary data.
func isRequestConfig(oe *objectExpr) bool {
	if oe == nil {
		return false
	}
	for _, p := range oe.properties {
		if requestConfigKeys[p.key] || requestConfigKeys[strings.ToLower(p.key)] {
			return true
		}
	}
	return false
}

// readVerbs are verbs whose object argument is a query string, not a body.
var readVerbs = map[string]bool{
	"get": true, "load": true, "list": true, "search": true, "fetch": true,
	"read": true, "find": true, "query": true, "count": true, "download": true,
	"head": true, "options": true, "request": true,
}

// configObject returns an object literal for an expression that denotes one,
// following a variable holding it.
func (c *context) configObject(e expr) *objectExpr {
	if oe, ok := e.(*objectExpr); ok {
		return oe
	}
	if id, ok := e.(*identExpr); ok {
		if o2, ok := c.varInits[id.name]; ok {
			if oe, ok := o2.(*objectExpr); ok {
				return oe
			}
		}
	}
	return nil
}

// calleeObjectName returns the object a method is invoked on ("api" in
// api.get(url)), or "" for a bare call.
func calleeObjectName(callee expr) string {
	switch n := callee.(type) {
	case *memberExpr:
		chain := memberChain(n)
		if len(chain) >= 2 {
			return chain[0]
		}
	}
	return ""
}

// calleeProperty returns the last property of a call target: the member name for
// a.b(), the identifier for a(), and the string key for computed access such as
// a["post"] so obfuscated lookups resolve to the same verb.
func calleeProperty(callee expr) string {
	switch n := callee.(type) {
	case *identExpr:
		return n.name
	case *memberExpr:
		return n.property
	}
	return ""
}

func (c *context) analyzeConfigObj(oe *objectExpr) {
	cfg := c.parseInit(oe)
	// url lives in the config object itself, so it is read separately.
	urlVal := ""
	for _, prop := range oe.properties {
		if !configPropKeys[prop.key] {
			continue
		}
		urlVal = c.evalExpr(prop.value)
		if urlVal == "" {
			if id, ok := prop.value.(*identExpr); ok {
				urlVal = c.vars[id.name]
			}
		}
	}
	if urlVal == "" {
		return
	}
	method := strings.ToUpper(strings.TrimSpace(cfg.method))
	body := cfg.body
	params := cfg.params
	// jQuery sends `data` as the query string on GET/HEAD; axios keeps `data`
	// in the body and uses `params` for the query.
	if (method == "GET" || method == "HEAD" || method == "") && cfg.hasBodyKey && !cfg.rawJSON {
		if len(cfg.params) == 0 {
			params = c.paramExpr(dataValue(oe, "data"))
			body = ""
		}
	}
	if method == "" {
		method = "GET"
	}
	headers := append([]contract.NameValue{}, cfg.headers...)
	headers = append(headers, contentTypeHeader(cfg.contentType)...)
	target := appendQuery(urlVal, params)
	c.addLink(target, "js-api", "config", method)
	c.addObs(target, method, headers, body)
}

// dataValue returns the value of a named property of an object literal.
func dataValue(oe *objectExpr, key string) expr {
	for _, p := range oe.properties {
		if strings.EqualFold(p.key, key) {
			return p.value
		}
	}
	return nil
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
		return strings.TrimSpace(e.value)
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
		return trimURLJoin(s)
	}
	switch e := e.(type) {
	case *memberExpr:
		// this.ajaxUrl and friends: property assignments are already recorded
		// in props, which is what lets an endpoint assembled from a member
		// expression resolve to a literal path.
		if props, ok := c.props[exprPath(e)]; ok {
			if v := strings.TrimSpace(props[""]); v != "" {
				return v
			}
		}
		if props, ok := c.props[e.property]; ok {
			if v := strings.TrimSpace(props[""]); v != "" {
				return v
			}
		}
		if v, ok := c.uniqueEndpointProp(e.property); ok {
			return v
		}
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
			left := c.resolveURLArg(e.left)
			if left != "" {
				right := c.resolveURLArg(e.right)
				if right != "" {
					return left + right
				}
				// static prefix + dynamic tail: keep the prefix
				return left
			}
			right := c.resolveURLArg(e.right)
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

// trimURLJoin drops separators left behind when a query string is appended as
// "endpoint" + "?" + params: the tail is unresolved, but the path is not.
func trimURLJoin(s string) string {
	for strings.HasSuffix(s, "?") || strings.HasSuffix(s, "&") {
		s = strings.TrimRight(s, "?&")
	}
	return s
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
		before := p.pos
		s := p.parseStmt()
		if s != nil {
			stmts = append(stmts, s)
			continue
		}
		// A statement that produced nothing (an empty `;`, a class/import
		// header) has usually consumed its own token. Advancing again would
		// silently drop the following token and desynchronise the rest of the
		// file, so only step forward when the parser made no progress at all.
		if p.pos == before {
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
				return p.parseClassExpr()
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

// parseTryStmt keeps the bodies. Requests are wrapped in try/catch as a
// matter of course, and skipping the block threw the request away with it.
func (p *parser) parseTryStmt() stmt {
	p.advance() // try
	ts := &tryStmt{body: p.parseBlockStmt()}
	if p.peek().typ == tokKeyword && p.peek().value == "catch" {
		p.advance()
		if p.peek().typ == tokPunct && p.peek().value == "(" {
			depth := 0
			for p.peek().typ != tokEOF {
				v := p.peek().value
				p.advance()
				if v == "(" {
					depth++
				} else if v == ")" {
					depth--
					if depth == 0 {
						break
					}
				}
			}
		}
		ts.catch = p.parseBlockStmt()
	}
	if p.peek().typ == tokKeyword && p.peek().value == "finally" {
		p.advance()
		ts.finally = p.parseBlockStmt()
	}
	return ts
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
			left = p.parseArrowBody([]string{t.value})
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
				params := p.consumeArrowHeader()
				left = p.parseArrowBody(params)
			} else if p.peek().typ == tokIdent {
				p.advance()
				if p.peek().typ == tokOp && p.peek().value == "=>" {
					p.advance()
					left = p.parseArrowBody([]string{t.value})
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
		case "await", "yield":
			// "var r = await fetch(url)" is how async request code is usually
			// written. Dropping await dropped the request with it.
			p.advance()
			left = p.parseExpr(precUnary)
		default:
			p.advance()
			left = &identExpr{name: t.value}
		}

	case tokPunct:
		if t.value == "(" {
			p.advance()
			if p.isArrowAhead() {
				params := p.consumeArrowHeader()
				left = p.parseArrowBody(params)
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
						// "async load() {...}" and "get value() {...}": the
						// modifier is not the key, and treating it as one
						// silently loses the whole method - which is where
						// async request code lives.
						switch p.peek().value {
						case "async", "get", "set", "static", "asyncget", "asyncset":
							p.advance()
							switch p.peek().typ {
							case tokString, tokIdent, tokNumber, tokKeyword:
								key = p.peek().value
								p.advance()
							default:
								continue
							}
						default:
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
// an arrow function, returning the captured parameter names. The caller
// guarantees isArrowAhead() was true.
func (p *parser) consumeArrowHeader() []string {
	var params []string
	expectName := true
	if p.peek().typ == tokPunct && p.peek().value == "(" {
		p.advance()
	}
	depth := 1
	for depth > 0 {
		t := p.peek()
		if t.typ == tokEOF {
			return params
		}
		if t.typ == tokPunct {
			switch t.value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case ",":
				if depth == 1 {
					expectName = true
				}
			}
		} else if t.typ == tokIdent && depth == 1 && expectName {
			params = append(params, t.value)
			expectName = false
		}
		p.advance()
	}
	p.advance() // "=>"
	return params
}

// parseArrowBody parses the body of an arrow function (block or expression).
func (p *parser) parseArrowBody(params []string) *funcExpr {
	if p.peek().typ == tokPunct && p.peek().value == "{" {
		return &funcExpr{body: p.parseBlockStmt(), params: params}
	}
	e := p.parseExpr(0)
	return &funcExpr{body: &blockStmt{stmts: []stmt{&exprStmt{expr: e}}}, params: params}
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

// parseClassExpr parses a class body into its methods and fields. Classes are
// where modern sites keep request code - components, services, controllers -
// so skipping the body loses every endpoint, builder and response schema
// inside it. Anything unrecognised is stepped over rather than abandoned: a
// partial class is still worth far more than none.
func (p *parser) parseClassExpr() stmt {
	p.advance() // class
	name := ""
	if p.peek().typ == tokIdent {
		name = p.peek().value
		p.advance()
	}
	// extends Base / mixin(Base): everything up to the body is not interesting.
	for p.peek().typ != tokPunct || p.peek().value != "{" {
		if p.peek().typ == tokEOF {
			return nil
		}
		p.advance()
	}
	return p.parseClassBody(name)
}

func (p *parser) parseClassBody(name string) *classExpr {
	if p.peek().typ != tokPunct || p.peek().value != "{" {
		return nil
	}
	p.advance()
	cls := &classExpr{name: name}
	guard := 0
	for p.peek().typ != tokPunct || p.peek().value != "}" {
		guard++
		if p.peek().typ == tokEOF || guard > 50000 {
			p.skipBlock()
			return cls
		}
		// Modifiers: static, async, get, set, generator star, accessor.
		for {
			t := p.peek()
			if t.typ == tokPunct && t.value == "*" {
				p.advance()
				continue
			}
			if t.typ == tokKeyword && (t.value == "static" || t.value == "async" || t.value == "get" || t.value == "set" || t.value == "accessor") {
				// "static {" is a static initialization block, not a field.
				if t.value == "static" {
					save := p.pos
					p.advance()
					if p.peek().typ == tokPunct && p.peek().value == "{" {
						body := p.parseBlockStmt()
						cls.props = append(cls.props, property{key: "static", value: &funcExpr{body: body}})
						continue
					}
					p.pos = save
				}
				p.advance()
				continue
			}
			break
		}
		// Computed key: the name is dynamic, so keep the body and drop the key.
		if p.peek().typ == tokPunct && p.peek().value == "[" {
			p.advance()
			depth := 1
			for depth > 0 && p.peek().typ != tokEOF {
				v := p.peek().value
				p.advance()
				if v == "[" {
					depth++
				} else if v == "]" {
					depth--
				}
			}
			if p.peek().typ == tokPunct && p.peek().value == "(" {
				p.parseArgs()
				body := p.parseBlockStmt()
				cls.props = append(cls.props, property{key: "", value: &funcExpr{body: body}})
			} else if p.peek().typ == tokPunct && p.peek().value == "=" {
				p.advance()
				p.parseExpr(0)
				if p.peek().typ == tokPunct && p.peek().value == ";" {
					p.advance()
				}
			}
			continue
		}
		key := ""
		switch t := p.peek(); {
		case t.typ == tokPunct && t.value == "#":
			p.advance()
			key = "#" + p.peek().value
			p.advance()
		case t.typ == tokString, t.typ == tokIdent, t.typ == tokNumber, t.typ == tokKeyword:
			// "constructor" and friends arrive as keywords; a method is a
			// method whatever its name was tokenized as.
			key = t.value
			p.advance()
		default:
			p.advance()
			continue
		}
		switch {
		case p.peek().typ == tokPunct && p.peek().value == "(":
			p.parseArgs()
			body := p.parseBlockStmt()
			cls.props = append(cls.props, property{key: key, value: &funcExpr{body: body}})
		case p.peek().typ == tokPunct && p.peek().value == "=":
			p.advance()
			cls.props = append(cls.props, property{key: key, value: p.parseExpr(0)})
			if p.peek().typ == tokPunct && p.peek().value == ";" {
				p.advance()
			}
		case p.peek().typ == tokPunct && p.peek().value == ";":
			p.advance()
			cls.props = append(cls.props, property{key: key, value: &identExpr{name: key}})
		default:
			// A field with no value, or something unexpected: do not spin.
			if p.peek().typ == tokPunct && (p.peek().value == "," || p.peek().value == ";") {
				p.advance()
			}
		}
	}
	p.advance() // }
	p.skipSemicolons()
	return cls
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

// classExpr holds a parsed class body: its methods and fields are what the
// analyzer walks, since that is where the requests live.
type classExpr struct {
	name  string
	props []property
}

func (*classExpr) exprNode() {}
func (*classExpr) stmtNode() {}

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
	body   *blockStmt
	params []string
}

func (*funcExpr) exprNode() {}

type returnStmt struct {
	expr expr
}

func (*returnStmt) stmtNode() {}

// tryStmt keeps the guarded body: request code lives inside it.
type tryStmt struct {
	body    *blockStmt
	catch   *blockStmt
	finally *blockStmt
}

func (*tryStmt) stmtNode() {}

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
		if len(args) > 0 {
			// The parameters belong to this request whether or not the code
			// spells its address out.
			c.bindRequestScopesFull(args, resolveEndpoint(trimURLJoin(url), c.sourceURL), args[0],
				method, headers, body, objName+"."+methodName, method == "")
		}
		if url != "" {
			c.addLink(url, "js-api", objName+"."+methodName, httpMethod)
		}
		if url != "" {
			c.addObs(url, method, headers, body)
		}

	case callMethod:
		var url string
		if len(args) > 0 {
			url = c.resolveURLArg(args[0])
		}
		if url == "" {
			// The address is kept in a property ("fetch(this.ajaxUrl + '?' + p)").
			// The request is still real, and the parameters it carries can
			// still be attributed - to the expression, not to a URL.
			if scopes := c.scopesInArgs(args); len(scopes) > 0 {
				carrier := c.requestCarrier(url, args[0])
				for _, sc := range scopes {
					c.bindScopeToRequest(sc, "", carrier)
					c.pending = append(c.pending, pendingBinding{scope: sc, urlExpr: args[0]})
				}
			}
		}
		if url != "" {
			// Resolve a client instance base before judging the shape: "/users"
			// on an axios.create({baseURL:"/api"}) instance is /api/users.
			url = c.applyClientBase(receiverOf(objName), url)
			// Routers and non-HTTP libs also expose .get('/path'); for GET
			// calls require an explicit API signal so page routes do not
			// masquerade as endpoints. Other verbs are call-specific enough.
			if strings.EqualFold(methodName, "get") && !apiEndpointShape(url) {
				break
			}
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
		var params []contract.NameValue
		if len(args) > 1 {
			cfg := c.parseInit(args[1])
			headers = cfg.headers
			params = cfg.params
			if cfg.method != "" {
				m = strings.ToUpper(cfg.method)
			}
			if cfg.body != "" {
				body = cfg.body
			} else if m == "POST" || m == "PUT" || m == "PATCH" {
				body = c.bodyExpr(args[1])
			}
		}
		// jQuery shorthand: on GET the payload is the query string.
		if (m == "GET" || m == "HEAD") && body != "" && len(params) == 0 {
			if vals := c.paramExpr(args[1]); len(vals) > 0 {
				params = vals
				body = ""
			}
		}
		if len(params) > 0 {
			url = appendQuery(url, params)
		}
		if url != "" {
			target := resolveEndpoint(trimURLJoin(url), c.sourceURL)
			for _, a := range args {
				if b := c.builderInExpr(a); b != "" {
					c.bindBuilderToRequest(b, target)
				}
			}
			c.bindRequestScopes(args, target, args[0])
			c.addLink(url, "js-api", objName+"."+methodName, m)
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

	case callGeneric:
		c.applyGenericCall(args, methodName, objName)

	case callXHROpen:
		var url string
		if len(args) > 1 {
			url = c.resolveURLArg(args[1])
			if httpMethod == "" && len(args) > 0 {
				httpMethod = c.evalExpr(args[0])
			}
		}
		if url == "" {
			return
		}
		// objName arrives as the full call path ("request.open"); the tracked
		// XHR is keyed by the variable name alone.
		recv := objName
		if i := strings.Index(recv, "."); i > 0 {
			recv = recv[:i]
		}
		if st, tracked := c.xhrState[recv]; tracked {
			// Hold the observation until send() so the headers and body set on
			// the same object end up in the same contract.
			st.url = url
			st.method = stringOr(httpMethod, "GET")
			return
		}
		c.addLink(url, "js-api", "XHR.open", httpMethod)
		c.addObs(url, httpMethod, nil, "")
	}
}

// emitXHR records the contract observation for a tracked XMLHttpRequest.
// applyGenericCall handles a request call identified purely by its shape: a URL
// first argument, optionally followed by an init/payload object. The verb comes
// from the init object, the callee's name, or the argument shape — and the last
// of these is reported as inferred rather than silently asserted.
func (c *context) applyGenericCall(args []expr, verb, objName string) {
	if len(args) == 0 {
		return
	}
	// Config-object form.
	if oe := c.configObject(args[0]); oe != nil {
		c.analyzeConfigObj(oe)
		return
	}
	url := c.resolveURLArg(args[0])
	if url == "" {
		if scopes := c.scopesInArgs(args); len(scopes) > 0 {
			carrier := c.requestCarrier(url, args[0])
			for _, sc := range scopes {
				c.bindScopeToRequest(sc, "", carrier)
				c.pending = append(c.pending, pendingBinding{scope: sc, urlExpr: args[0]})
			}
		}
		return
	}
	url = c.applyClientBase(objName, url)
	var cfg initConfig
	hasPayload := false
	payloadIsQuery := false
	if len(args) > 1 {
		if oe := c.configObject(args[1]); oe != nil && isRequestConfig(oe) {
			cfg = c.parseInit(oe)
			hasPayload = cfg.hasBodyKey
		} else {
			// A bare object is the payload; whether it travels in the query or
			// the body follows from the verb.
			cfg.body = c.bodyExpr(args[1])
			hasPayload = strings.TrimSpace(cfg.body) != ""
			payloadIsQuery = readVerbs[strings.ToLower(verb)]
		}
	}
	method, source := inferMethod(cfg.method, verb, hasPayload, c.inChain > 0)
	inferred := source == srcShape || source == srcDefault
	if method == "" {
		method = "GET"
		inferred = true
	}
	// A payload on a GET belongs in the query string — that is where jQuery and
	// the fetch/axios defaults actually put it.
	if hasPayload && (payloadIsQuery || method == "GET") {
		if vals := c.paramExpr(args[1]); len(vals) > 0 {
			url = appendQuery(url, vals)
			cfg.body = ""
		}
	}
	headers := append([]contract.NameValue{}, cfg.headers...)
	headers = append(headers, contentTypeHeader(cfg.contentType)...)
	url = appendQuery(url, cfg.params)
	if url == "" || isLikelyNotAPI(url) {
		return
	}
	target := resolveEndpoint(trimURLJoin(url), c.sourceURL)
	for _, a := range args {
		if b := c.builderInExpr(a); b != "" {
			c.bindBuilderToRequest(b, target)
		}
	}
	c.bindRequestScopes(args, target, args[0])
	c.addLink(url, "js-api", genericMatchSource, method)
	c.addObsInferred(url, method, headers, cfg.body, inferred)
}

// receiverOf returns the object from a call's full path ("api.get" -> "api").
func receiverOf(fullPath string) string {
	if i := strings.Index(fullPath, "."); i > 0 {
		return fullPath[:i]
	}
	return fullPath
}

// applyClientBase resolves a relative endpoint against the baseURL of a
// client instance created with axios.create({baseURL: '/api'}), so instance
// calls report the URL that is actually requested.
func (c *context) applyClientBase(objName, url string) string {
	if objName == "" {
		return url
	}
	cfg, ok := c.clientInst[objName]
	if !ok {
		return url
	}
	base := strings.TrimSpace(cfg.baseURL)
	if base == "" || !strings.HasPrefix(url, "/") || strings.HasPrefix(url, "//") {
		return url
	}
	return strings.TrimRight(base, "/") + url
}

// genericMatchSource labels endpoints discovered structurally in the report.
const genericMatchSource = "call"

// emitXHR records the contract observation for a tracked XMLHttpRequest.
func (c *context) emitXHR(name string, st *xhrInfo) {
	delete(c.xhrState, name)
	if st.url == "" {
		return
	}
	method := stringOr(st.method, "GET")
	c.addLink(st.url, "js-api", "XHR.send", method)
	c.addObs(st.url, method, st.headers, st.body)
}

// flushXHR records tracked requests whose send() was never reached (for
// example a request built in a function the analyzer cannot follow), so the
// endpoint still shows up in the contracts.
func (c *context) flushXHR() {
	names := make([]string, 0, len(c.xhrState))
	for name := range c.xhrState {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c.emitXHR(name, c.xhrState[name])
	}
}
