package emulator

import (
	"fmt"
	"strings"

	"github.com/dop251/goja"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxDOMNodes caps how many elements buildDOM materializes into the VM. Pages
// can legitimately carry tens of thousands of nodes (inline SVG, dumped tables);
// elements beyond the cap keep working via the permissive fallback, so the cap
// only bounds memory/CPU for pathological pages.
const maxDOMNodes = 8000

// domEl is the lightweight bookkeeping node behind a real DOM element the
// sandbox builds from the page HTML. Parent/child links mirror the goja tree so
// selectors can walk ancestors cheaply.
type domEl struct {
	gobj     *goja.Object
	tag      string
	id       string
	class    []string
	attrs    map[string]string
	children []*domEl
	parent   *domEl
}

// buildDOM parses the raw page HTML into the sandbox. After it runs, document
// lookups (getElementById/querySelector/getElementsByTagName/...) return real
// elements with populated attributes, and document.body/head/documentElement
// point at the actual page structure.
func (vm *gojaVM) buildDOM(pageHTML string) {
	if strings.TrimSpace(pageHTML) == "" {
		return
	}
	tree, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return
	}
	vm.domByID = map[string]*domEl{}
	vm.domGobj = map[*goja.Object]*domEl{}
	vm.domAll = vm.domAll[:0]
	vm.domRoot = vm.walkDOM(tree)
	if vm.domRoot == nil {
		return
	}

	d, ok := vm.runtime.Get("document").(*goja.Object)
	if !ok {
		return
	}
	for tag, prop := range map[string]string{"body": "body", "head": "head", "html": "documentElement"} {
		if el := vm.findDescendant(vm.domRoot, tag); el != nil {
			d.Set(prop, el.gobj)
		}
	}
	if el := vm.findDescendant(vm.domRoot, "html"); el != nil {
		d.Set("scrollingElement", el.gobj)
	}
	d.Set("title", vm.docTitle())
	if n := len(vm.domTagAll("form")); n > 0 {
		d.Set("forms", vm.domArray(vm.domTagAll("form")))
	}
	if n := len(vm.domTagAll("img")); n > 0 {
		d.Set("images", vm.domArray(vm.domTagAll("img")))
	}
	if n := len(vm.domTagAll("a")); n > 0 {
		d.Set("links", vm.domArray(vm.domTagAll("a")))
	}
	if n := len(vm.domTagAll("script")); n > 0 {
		d.Set("scripts", vm.domArray(vm.domTagAll("script")))
	}
}

// walkDOM descends from the document node to its first element (the <html>
// element for a parsed page).
func (vm *gojaVM) walkDOM(n *html.Node) *domEl {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return vm.domNode(c)
		}
	}
	return nil
}

// domNode materializes one HTML element into the VM, recursively wiring its
// children through appendChild so childNodes/parent chains stay consistent.
func (vm *gojaVM) domNode(n *html.Node) *domEl {
	if len(vm.domAll) >= maxDOMNodes {
		return nil
	}
	tag := strings.ToLower(n.Data)
	de := &domEl{tag: tag, attrs: map[string]string{}}
	for _, a := range n.Attr {
		key := strings.ToLower(a.Key)
		de.attrs[key] = a.Val
	}
	de.id = de.attrs["id"]
	de.class = strings.Fields(de.attrs["class"])

	el := vm.runtime.NewObject()
	vm.wireElement(el, tag, de.attrs, false)
	de.gobj = el
	vm.domAll = append(vm.domAll, de)
	if de.id != "" {
		vm.domByID[de.id] = de
	}
	vm.domGobj[el] = de

	adopt, hasAdopt := goja.AssertFunction(el.Get("appendChild"))
	var text strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.ElementNode:
			sub := vm.domNode(c)
			if sub == nil {
				continue
			}
			sub.parent = de
			de.children = append(de.children, sub)
			if hasAdopt {
				adopt(goja.Undefined(), sub.gobj)
			}
		case html.TextNode:
			text.WriteString(c.Data)
		}
	}
	vm.populateDOMElement(el, de, text.String(), n)
	return de
}

// populateDOMElement overlays the static attribute values onto a freshly wired
// element. URL-bearing attributes stored in accessors at wire time are skipped
// here (their static values are the crawler's job, not the emulator's).
func (vm *gojaVM) populateDOMElement(el *goja.Object, de *domEl, text string, n *html.Node) {
	attrs := de.attrs
	if de.id != "" {
		el.Set("id", de.id)
	}
	if c := attrs["class"]; c != "" {
		el.Set("className", c)
	}
	if v, ok := attrs["value"]; ok && v != "" {
		el.Set("value", v)
	}
	if t, ok := attrs["type"]; ok && t != "" {
		el.Set("type", t)
	}
	if nm, ok := attrs["name"]; ok && nm != "" {
		el.Set("name", nm)
	}
	for _, b := range []string{"checked", "selected", "disabled", "multiple", "required", "hidden", "readonly"} {
		if _, ok := attrs[b]; ok {
			prop := b
			if b == "readonly" {
				prop = "readOnly"
			}
			el.Set(prop, true)
		}
	}
	if ds, ok := el.Get("dataset").(*goja.Object); ok {
		for k, v := range attrs {
			if rest, found := strings.CutPrefix(k, "data-"); found && rest != "" && v != "" {
				ds.Set(dataAttrName(rest), v)
			}
		}
	}
	if trimmed := strings.TrimSpace(text); trimmed != "" {
		el.Set("textContent", trimmed)
		el.Set("innerText", trimmed)
	}
	var buf strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		html.Render(&buf, c)
	}
	if buf.Len() > 0 {
		el.Set("innerHTML", buf.String())
	}
	if sv := attrs["style"]; sv != "" {
		if so, ok := el.Get("style").(*goja.Object); ok {
			// Deliberate: url(...) refs in static inline styles are a discovery
			// the HTML crawler does not parse.
			so.Set("cssText", sv)
		}
	}
	for k, v := range attrs {
		if v == "" {
			continue
		}
		switch k {
		case "id", "class", "value", "type", "name", "checked", "selected", "disabled",
			"multiple", "required", "hidden", "readonly", "style", "dataset":
			continue
		case "src", "href", "action", "data", "srcset":
			continue // seeded into accessors by wireElement
		}
		// data-* land in dataset (camel) above; also expose the raw attribute so
		// getAttribute('data-...') works the way page scripts expect.
		el.Set(k, v)
	}
}

// nodeAttrs returns an element's lowercase-key attribute map.
func nodeAttrs(n *html.Node) map[string]string {
	attrs := map[string]string{}
	for _, a := range n.Attr {
		attrs[strings.ToLower(a.Key)] = a.Val
	}
	return attrs
}

// parseFragmentChildren parses an innerHTML assignment into top-level nodes
// using a <div> context, the way browsers wrap fragment parsing.
func parseFragmentChildren(s string) []*html.Node {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(s), ctx)
	if err != nil {
		return nil
	}
	return nodes
}

// detachedNode builds a runtime (registry-free) goja element tree for an
// innerHTML fragment. It lives only under the element that adopted it.
func (vm *gojaVM) detachedNode(n *html.Node) *goja.Object {
	if n == nil {
		return nil
	}
	switch n.Type {
	case html.TextNode:
		o := vm.runtime.NewObject()
		o.Set("nodeType", 3)
		o.Set("nodeName", "#text")
		o.Set("nodeValue", n.Data)
		o.Set("data", n.Data)
		o.Set("textContent", n.Data)
		return o
	case html.ElementNode:
		tag := strings.ToLower(n.Data)
		attrs := nodeAttrs(n)
		de := &domEl{tag: tag, attrs: attrs}
		de.id = attrs["id"]
		de.class = strings.Fields(attrs["class"])
		el := vm.runtime.NewObject()
		vm.wireElement(el, tag, attrs, false)
		var text strings.Builder
		adopt, hasAdopt := goja.AssertFunction(el.Get("appendChild"))
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.ElementNode:
				sub := vm.detachedNode(c)
				if sub == nil {
					continue
				}
				if hasAdopt {
					adopt(goja.Undefined(), sub)
				}
			case html.TextNode:
				text.WriteString(c.Data)
			}
		}
		vm.populateDOMElement(el, de, text.String(), n)
		return el
	}
	return nil
}

// recordFragmentResources records the resource-loading attributes a browser
// would fetch for innerHTML-injected markup: src, srcset, inline style url()
// and object/embed data. <a href> is deliberately skipped — a link injected
// via innerHTML neither navigates nor loads until it is clicked.
func (vm *gojaVM) recordFragmentResources(nodes []*html.Node) {
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			attrs := nodeAttrs(n)
			tag := strings.ToLower(n.Data)
			typ := elementResourceType(tag)
			if v := attrs["src"]; v != "" {
				vm.record(v, "GET", typ)
			}
			if v := attrs["srcset"]; v != "" {
				for _, u := range parseSrcset(v) {
					vm.record(u, "GET", typ)
				}
			}
			if v := attrs["style"]; v != "" {
				for _, u := range extractCSSURLs(v) {
					vm.record(u, "GET", typ)
				}
			}
			if (tag == "object" || tag == "embed" || tag == "applet") && attrs["data"] != "" {
				vm.record(attrs["data"], "GET", typ)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
}

// dataAttrName converts the tail of a data-* attribute into its dataset camel
// key: data-foo-bar → fooBar.
func dataAttrName(k string) string {
	parts := strings.Split(k, "-")
	var b strings.Builder
	first := true
	for _, p := range parts {
		if p == "" {
			continue
		}
		if first {
			b.WriteString(p)
			first = false
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// ---- DOM lookups -----------------------------------------------------------

func (vm *gojaVM) domQuery(sel string) *goja.Object {
	if len(vm.domAll) == 0 {
		return nil
	}
	groups, ok := selectorGroups(sel)
	if !ok {
		return nil
	}
	for _, de := range vm.domAll {
		if matchesSelector(de, groups) {
			return de.gobj
		}
	}
	return nil
}

func (vm *gojaVM) domQueryAll(sel string) []*goja.Object {
	if len(vm.domAll) == 0 {
		return nil
	}
	groups, ok := selectorGroups(sel)
	if !ok {
		return nil
	}
	var out []*goja.Object
	for _, de := range vm.domAll {
		if matchesSelector(de, groups) {
			out = append(out, de.gobj)
		}
	}
	return out
}

// domScopedAll matches a selector within the subtree of a specific element
// (el.querySelector / el.querySelectorAll). Returns nil when there is no DOM
// registry entry for the element or the selector is unsupported.
func (vm *gojaVM) domScopedAll(root *goja.Object, sel string) []*goja.Object {
	rootDe := vm.domGobj[root]
	if rootDe == nil || len(vm.domAll) == 0 {
		return nil
	}
	groups, ok := selectorGroups(sel)
	if !ok {
		return nil
	}
	var out []*goja.Object
	for _, de := range vm.domAll {
		if de == rootDe || !isSubtreeOf(de, rootDe) {
			continue
		}
		if matchesSelector(de, groups) {
			out = append(out, de.gobj)
		}
	}
	return out
}

func (vm *gojaVM) domTagAll(tag string) []*goja.Object {
	if len(vm.domAll) == 0 {
		return nil
	}
	var out []*goja.Object
	for _, de := range vm.domAll {
		if tag == "*" || de.tag == tag {
			out = append(out, de.gobj)
		}
	}
	return out
}

// domScopedTag returns descendants of a specific element that match a tag
// name (el.getElementsByTagName, "a" or "*").
func (vm *gojaVM) domScopedTag(root *goja.Object, tag string) []*goja.Object {
	rootDe := vm.domGobj[root]
	if rootDe == nil || len(vm.domAll) == 0 {
		return nil
	}
	var out []*goja.Object
	for _, de := range vm.domAll {
		if de == rootDe || !isSubtreeOf(de, rootDe) {
			continue
		}
		if tag == "*" || de.tag == tag {
			out = append(out, de.gobj)
		}
	}
	return out
}

func (vm *gojaVM) domClassAll(classes []string) []*goja.Object {
	if len(vm.domAll) == 0 {
		return nil
	}
	var out []*goja.Object
	for _, de := range vm.domAll {
		all := true
		for _, cl := range classes {
			if !strIn(de.class, cl) {
				all = false
				break
			}
		}
		if all {
			out = append(out, de.gobj)
		}
	}
	return out
}

func (vm *gojaVM) domNameAll(name string) []*goja.Object {
	if name == "" || len(vm.domAll) == 0 {
		return nil
	}
	var out []*goja.Object
	for _, de := range vm.domAll {
		if de.attrs["name"] == name {
			out = append(out, de.gobj)
		}
	}
	return out
}

func (vm *gojaVM) domArray(objs []*goja.Object) *goja.Object {
	r := vm.runtime
	arr := r.NewArray()
	for i, o := range objs {
		arr.Set(fmt.Sprintf("%d", i), o)
	}
	arr.Set("length", len(objs))
	return arr
}

func (vm *gojaVM) docTitle() string {
	for _, de := range vm.domAll {
		if de.tag == "title" {
			if v := de.gobj.Get("textContent"); v != nil {
				return strings.TrimSpace(v.String())
			}
		}
	}
	return ""
}

func (vm *gojaVM) findDescendant(root *domEl, tag string) *domEl {
	stack := []*domEl{root}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if top.tag == tag {
			return top
		}
		stack = append(stack, top.children...)
	}
	return nil
}

func isSubtreeOf(de, root *domEl) bool {
	for cur := de; cur != nil; cur = cur.parent {
		if cur == root {
			return true
		}
	}
	return false
}

// ---- CSS selector subset ---------------------------------------------------

type compoundSel struct {
	tag   string
	id    string
	class []string
	attrs []attrSel
}

type attrSel struct {
	name  string
	op    string
	value string
}

// selectorGroups splits a selector on top-level commas into ancestor chains.
// Returns ok=false for unsupported syntax (combinators > + ~, pseudo-classes)
// so callers can fall back to the permissive empty/fresh-element behavior.
func selectorGroups(sel string) ([][]*compoundSel, bool) {
	sel = strings.TrimSpace(sel)
	if sel == "" || hasUnsupportedSyntax(sel) {
		return nil, false
	}
	var groups [][]*compoundSel
	for _, part := range splitTop(sel, ',') {
		chain, ok := parseChain(part)
		if !ok {
			return nil, false
		}
		groups = append(groups, chain)
	}
	if len(groups) == 0 {
		return nil, false
	}
	return groups, true
}

// hasUnsupportedSyntax reports combinators/pseudo markers at bracket depth
// zero. Characters inside [attr="v"] or (func) selectors are legal (a value may
// contain ":" or ">").
func hasUnsupportedSyntax(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '(':
			depth++
		case ']', ')':
			if depth > 0 {
				depth--
			}
		case '>', '+', '~', ':':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// splitTop splits on sep at bracket depth zero (used for commas and spaces).
func splitTop(s string, sep byte) []string {
	var parts []string
	depth := 0
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '(':
			depth++
		case ']', ')':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, s[last:])
	return parts
}

func parseChain(s string) ([]*compoundSel, bool) {
	var out []*compoundSel
	for _, p := range splitTop(s, ' ') {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		c, ok := parseCompound(p)
		if !ok {
			return nil, false
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func parseCompound(s string) (*compoundSel, bool) {
	c := &compoundSel{}
	i, n := 0, len(s)
	for i < n {
		ch := s[i]
		switch {
		case ch == '*':
			if c.tag != "" {
				return nil, false
			}
			c.tag = "*"
			i++
		case ch == '#':
			j := i + 1
			for j < n && isIdentChar(s[j]) {
				j++
			}
			if j == i+1 || c.id != "" {
				return nil, false
			}
			c.id = s[i+1 : j]
			i = j
		case ch == '.':
			j := i + 1
			for j < n && isIdentChar(s[j]) {
				j++
			}
			if j == i+1 {
				return nil, false
			}
			c.class = append(c.class, s[i+1:j])
			i = j
		case ch == '[':
			a, used, ok := parseAttr(s[i:])
			if !ok {
				return nil, false
			}
			c.attrs = append(c.attrs, a)
			i += used
		default:
			if !isIdentStart(ch) {
				return nil, false
			}
			if c.tag != "" {
				return nil, false
			}
			j := i + 1
			for j < n && isIdentChar(s[j]) {
				j++
			}
			c.tag = strings.ToLower(s[i:j])
			i = j
		}
	}
	if c.tag == "" && c.id == "" && len(c.class) == 0 && len(c.attrs) == 0 {
		return nil, false
	}
	return c, true
}

func parseAttr(s string) (attrSel, int, bool) {
	if len(s) < 2 || s[0] != '[' {
		return attrSel{}, 0, false
	}
	i := 1
	for i < len(s) && s[i] == ' ' {
		i++
	}
	j := i
	for j < len(s) && isIdentStart(s[j]) {
		j++
	}
	for j < len(s) && isIdentChar(s[j]) {
		j++
	}
	if j == i {
		return attrSel{}, 0, false
	}
	name := strings.ToLower(s[i:j])
	i = j
	for i < len(s) && s[i] == ' ' {
		i++
	}
	op := ""
	for _, cand := range []string{"^=", "$=", "*=", "~=", "="} {
		if strings.HasPrefix(s[i:], cand) {
			op = cand
			i += len(cand)
			break
		}
	}
	if op == "" {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) || s[i] != ']' {
			return attrSel{}, 0, false
		}
		return attrSel{name: name}, i + 1, true
	}
	for i < len(s) && s[i] == ' ' {
		i++
	}
	quote := byte(0)
	if i < len(s) && (s[i] == '\'' || s[i] == '"') {
		quote = s[i]
		i++
	}
	start := i
	for i < len(s) && s[i] != ']' {
		if quote != 0 && s[i] == quote {
			break
		}
		i++
	}
	val := strings.TrimSpace(s[start:i])
	if quote != 0 && i < len(s) && s[i] == quote {
		i++
	}
	for i < len(s) && s[i] == ' ' {
		i++
	}
	if i >= len(s) || s[i] != ']' {
		return attrSel{}, 0, false
	}
	return attrSel{name: name, op: op, value: val}, i + 1, true
}

func isIdentStart(b byte) bool {
	return b == '_' || b == '-' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentChar(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func matchesSelector(de *domEl, groups [][]*compoundSel) bool {
	for _, chain := range groups {
		if matchesChain(de, chain) {
			return true
		}
	}
	return false
}

func matchesChain(de *domEl, chain []*compoundSel) bool {
	if len(chain) == 0 || !chain[len(chain)-1].matches(de) {
		return false
	}
	cur := de.parent
	for i := len(chain) - 2; i >= 0; i-- {
		for cur != nil && !chain[i].matches(cur) {
			cur = cur.parent
		}
		if cur == nil {
			return false
		}
		cur = cur.parent
	}
	return true
}

func (c *compoundSel) matches(de *domEl) bool {
	if c.tag != "" && c.tag != "*" && c.tag != de.tag {
		return false
	}
	if c.id != "" && c.id != de.id {
		return false
	}
	for _, cl := range c.class {
		if !strIn(de.class, cl) {
			return false
		}
	}
	for _, a := range c.attrs {
		v, ok := de.attrs[a.name]
		if !ok {
			return false
		}
		switch a.op {
		case "":
		case "=":
			if v != a.value {
				return false
			}
		case "^=":
			if !strings.HasPrefix(v, a.value) {
				return false
			}
		case "$=":
			if !strings.HasSuffix(v, a.value) {
				return false
			}
		case "*=":
			if !strings.Contains(v, a.value) {
				return false
			}
		case "~=":
			if !strIn(strings.Fields(v), a.value) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func strIn(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
