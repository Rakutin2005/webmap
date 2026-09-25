// Package contract builds a request-side contract for every API endpoint the
// tool discovers: which HTTP methods and headers are sent, what URL arguments
// exist and in what format, what request bodies are used and in which format
// every field is. Fields carry the concrete observed values as evidence, so the
// formats are reproducible and can be used to generate request bodies later.
package contract

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"apimap/internal/color"
)

const (
	// maxValues caps how many distinct evidence values a field keeps.
	maxValues = 16
	// maxRaw caps how many unique request renditions are shown per endpoint.
	maxRaw = 12
	// maxBodyLen caps captured body bytes passed into inference.
	maxBodyLen = 4096
)

// NameValue is an ordered header/param assignment.
type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Observation is one discovered HTTP request (method, headers, body, query).
type Observation struct {
	URL     string
	Method  string
	Headers []NameValue
	Body    string
	// ResponseFields holds the response schema paths statically inferred from
	// the code that consumes the endpoint's response.
	ResponseFields []ResponseField
	// EndpointOnly marks an endpoint path found in the code without a
	// resolvable request around it: no method, parameters or calls are known.
	EndpointOnly bool
	// MethodInferred marks a verb that was derived from the call shape rather
	// than stated by the code, so the contract can label it as a guess.
	MethodInferred bool
}

// ResponseField is one field path read off an endpoint's response, with the
// kind inferred from how the code uses it (array, object, number, date, …).
type ResponseField struct {
	Path string
	Kind string
}

// Field describes one parameter or body field: its inferred format and the
// observed values that produced the inference.
type Field struct {
	Name     string
	Kind     string // int,uint,float,bool,str,enum,token,date,json,array,null,unknown
	Values   []string
	Constant bool
	ConstVal string
	Nullable bool
	// Inferred marks a field whose name came from static analysis while no
	// value was ever observed for it, so no type or constness can be claimed.
	Inferred bool
}

// BodyFormat is one detected request-body schema for an endpoint.
type BodyFormat struct {
	Kind    string // json, jsonrpc, form, text, xml, none, array
	MIME    string
	Fields  []Field
	Methods []string // jsonrpc method names
	Sample  string
}

// Endpoint aggregates every observation that hit one URL path.
type Endpoint struct {
	Path    string
	Methods []string
	Query   []Field
	Headers []Field
	Bodies  []BodyFormat
	// Response is the statically inferred response schema of this endpoint.
	Response []ResponseField
	Calls    int
	// Unobserved marks an endpoint that was only seen as a path in the code:
	// no request was resolved, so method/params/calls are unknown.
	Unobserved bool
	// MethodInferred means every captured verb for this endpoint was derived
	// from the call shape, not stated by the code.
	MethodInferred bool
	// Raw holds one masked rendition per unique request (context/evidence).
	Raw []string
}

// addResponseField merges one inferred response field, keeping the first
// non-generic kind seen for a path.
func addResponseField(fields []ResponseField, f ResponseField) []ResponseField {
	for i := range fields {
		if fields[i].Path != f.Path {
			continue
		}
		if fields[i].Kind == "any" && f.Kind != "" && f.Kind != "any" {
			fields[i].Kind = f.Kind
		}
		return fields
	}
	if f.Path == "" {
		return fields
	}
	return append(fields, f)
}

// tokenish matches names that usually carry secrets/session values.
var tokenish = regexp.MustCompile(`(?i)sessid|csrf|_token|\btoken\b|x-csrf|apitoken|api[_-]?key|secret|signature|\bsign\b|\bhash\b|\bsid\b|session`)

// longRandom matches hex/base64-ish values that look like hashes or session
// ids. The alphanumeric class deliberately excludes "/", "." and space so MIME
// types and JWT-shaped values never trip it. RE2 has no lookahead, so the
// "must contain a digit" rule for the base64 class is applied separately so a
// single long word ("applicationxwwwformurlencoded") can't be a false token.
var hexLong = regexp.MustCompile(`^[0-9a-fA-F]{24,}$`)
var b64Long = regexp.MustCompile(`^[A-Za-z0-9_-]{32,}={0,2}$`)

var datePrefixes = []string{
	time.RFC3339, "2006-01-02", "2006/01/02", "02.01.2006", time.RFC1123,
	time.RFC1123Z, "15:04", "2006-01-02 15:04:05",
}

func unescape(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}

// IsToken reports whether a value should never be echoed verbatim (session
// ids, CSRF tokens, hashes) and is masked in all output modes.
func IsToken(name, value string) bool {
	if tokenish.MatchString(name) {
		return true
	}
	v := trimQuotes(strings.TrimSpace(value))
	if v == "" {
		return false
	}
	hasDigit := strings.ContainsAny(v, "0123456789")
	// Real hashes/session ids contain digits; a pure letter run is a word.
	if hexLong.MatchString(v) && hasDigit {
		return true
	}
	if b64Long.MatchString(v) && hasDigit {
		return true
	}
	return false
}

func trimQuotes(s string) string { return strings.Trim(s, `"'`) }

// Shield renders a token value; non-tokens pass through.
func Shield(name, value string) string {
	if IsToken(name, value) {
		return "<" + name + ">"
	}
	return value
}

// Infer groups raw observations per endpoint and derives their contract.
func Infer(obs []Observation) []Endpoint {
	groups := map[string]*Endpoint{}
	for _, o := range obs {
		u, err := url.Parse(o.URL)
		if err != nil || u == nil || u.Path == "" {
			continue
		}
		path := strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
		if path == "" {
			continue
		}
		e := groups[path]
		if e == nil {
			e = &Endpoint{Path: path}
			groups[path] = e
		}
		if o.EndpointOnly {
			e.Unobserved = true
		} else {
			e.Methods = addString(e.Methods, strings.ToUpper(o.Method))
			e.Calls++
			if o.MethodInferred {
				e.MethodInferred = true
			} else {
				// A proven verb anywhere makes the endpoint's method known.
				e.MethodInferred = false
			}
		}
		for _, h := range o.Headers {
			e.Headers = addHeader(e.Headers, h)
		}
		for k, vs := range u.Query() {
			e.Query = addValue(e.Query, k, vs...)
		}
		mergeBody(e, o.Body, contentType(o.Headers))
		for _, f := range o.ResponseFields {
			e.Response = addResponseField(e.Response, f)
		}
		if len(e.Raw) < maxRaw {
			e.Raw = addString(e.Raw, renderRequest(o))
		}
	}

	out := make([]Endpoint, 0, len(groups))
	for _, e := range groups {
		// An endpoint only counts as unobserved when no request was resolved
		// for it; a bare path literal must not mask real captured calls.
		e.Unobserved = e.Unobserved && len(e.Methods) == 0 && e.Calls == 0
		e.Query = finalizeFields(e.Query)
		e.Headers = finalizeFields(e.Headers)
		for i := range e.Bodies {
			b := &e.Bodies[i]
			b.Fields = finalizeFields(b.Fields)
			b.Sample = buildSample(*b)
		}
		e.sort()
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// sort puts display slices in a stable order and body formats by kind.
func (e *Endpoint) sort() {
	sort.Strings(e.Methods)
	sort.Slice(e.Query, func(i, j int) bool { return e.Query[i].Name < e.Query[j].Name })
	sort.Slice(e.Headers, func(i, j int) bool { return e.Headers[i].Name < e.Headers[j].Name })
	sort.Slice(e.Response, func(i, j int) bool { return e.Response[i].Path < e.Response[j].Path })
	sort.Slice(e.Bodies, func(i, j int) bool {
		if e.Bodies[i].Kind != e.Bodies[j].Kind {
			return e.Bodies[i].Kind < e.Bodies[j].Kind
		}
		return e.Bodies[i].MIME < e.Bodies[j].MIME
	})
}

func renderRequest(o Observation) string {
	var b strings.Builder
	b.WriteString(o.Method)
	b.WriteString(" ")
	u, err := url.Parse(o.URL)
	if err != nil || u == nil {
		b.WriteString(o.URL)
	} else {
		b.WriteString(u.Path)
		if u.RawQuery != "" {
			b.WriteString("?")
			b.WriteString(redactQuery(u.RawQuery))
		}
	}
	body := strings.TrimSpace(o.Body)
	if body != "" {
		if len(body) > 200 {
			body = body[:200] + "…"
		}
		b.WriteString(" : ")
		b.WriteString(redactBody(body))
	}
	if len(o.Headers) > 0 {
		var hs []string
		for _, h := range o.Headers {
			hs = append(hs, Shield(h.Name, h.Value))
		}
		b.WriteString(" [")
		b.WriteString(strings.Join(hs, ", "))
		b.WriteString("]")
	}
	return b.String()
}

// redactQuery masks any token-like parameter values in-place.
func redactQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		if eq := strings.IndexByte(p, '='); eq > 0 {
			k, v := p[:eq], p[eq+1:]
			parts[i] = k + "=" + Shield(k, unescape(v))
		}
	}
	return strings.Join(parts, "&")
}

// redactBody masks token field values inside a rendered request body.
func redactBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			for k, v := range obj {
				if s, ok := v.(string); ok && IsToken(k, s) {
					obj[k] = "<" + k + ">"
				}
			}
			if out, err := json.Marshal(obj); err == nil {
				return string(out)
			}
		}
	}
	parts := strings.Split(trimmed, "&")
	for i, p := range parts {
		if eq := strings.IndexByte(p, '='); eq > 0 {
			k, v := p[:eq], p[eq+1:]
			parts[i] = k + "=" + Shield(k, v)
		}
	}
	return strings.Join(parts, "&")
}

func contentType(headers []NameValue) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, "content-type") {
			return strings.ToLower(strings.TrimSpace(h.Value))
		}
	}
	return ""
}

func addString(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// addHeader records a header assignment with value evidence.
func addHeader(list []Field, h NameValue) []Field {
	name := strings.ToLower(strings.TrimSpace(h.Name))
	if name == "" {
		return list
	}
	return addFieldValue(list, name, strings.TrimSpace(h.Value))
}

func addValue(list []Field, name string, vals ...string) []Field {
	for _, v := range vals {
		list = addFieldValue(list, name, v)
	}
	return list
}

func addFieldValue(list []Field, name, value string) []Field {
	// A "<name>" value is the placeholder the static pass substitutes when it
	// recovers a field name from a concatenated form body. It proves the field
	// exists, not what it holds, so it must not become an observed value —
	// otherwise a runtime capture of the same field turns into an enum holding
	// both the real value and the placeholder.
	inferred := isPlaceholderValue(name, value)
	for i := range list {
		if list[i].Name != name {
			continue
		}
		if inferred {
			list[i].Inferred = true
			return list
		}
		list[i].Values = addString(list[i].Values, value)
		return list
	}
	if inferred {
		return append(list, Field{Name: name, Inferred: true})
	}
	return append(list, Field{Name: name, Values: []string{value}})
}

// isPlaceholderValue reports whether a value is the synthetic "<name>" marker.
func isPlaceholderValue(name, value string) bool {
	if name == "" {
		return false
	}
	if value == "<"+name+">" {
		return true
	}
	return len(value) > 2 && strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") &&
		!strings.ContainsAny(value, " \t")
}

// finalizeFields computes kinds, constants and nullable flags.
func finalizeFields(fields []Field) []Field {
	for i := range fields {
		f := &fields[i]
		f.Kind = inferKind(f.Name, f.Values)
		if f.Inferred && len(f.Values) == 0 {
			// Name recovered from code, nothing observed: claim no type.
			f.Kind = "unknown"
		}
		if len(f.Values) == 1 {
			f.Constant = true
			f.ConstVal = f.Values[0]
		}
	}
	return fields
}

// inferKind returns the format inferred for a field from its observed values.
func inferKind(name string, values []string) string {
	if len(values) == 0 {
		return "unknown"
	}
	if IsToken(name, strings.Join(values, "")) {
		return "token"
	}
	clean := make([]string, 0, len(values))
	blanks := 0
	for _, v := range values {
		if v == "null" {
			continue
		}
		if v == "" {
			blanks++
			continue
		}
		clean = append(clean, v)
	}
	if len(clean) == 0 {
		// "?debug" with no value is a flag, not a null.
		if blanks > 0 {
			return "bool"
		}
		return "null"
	}
	if allBool(clean) {
		return "bool"
	}
	allInt, neg := true, false
	for _, v := range clean {
		if _, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err != nil {
			allInt = false
			break
		}
		if strings.HasPrefix(strings.TrimSpace(v), "-") {
			neg = true
		}
	}
	if allInt {
		if neg {
			return "int"
		}
		return "uint"
	}
	if allFloat(clean) {
		return "float"
	}
	if allDate(clean) {
		return "date"
	}
	if len(clean) == 1 {
		return "str"
	}
	if len(clean) <= 8 {
		return "enum"
	}
	return "str"
}

func allBool(vs []string) bool {
	for _, v := range vs {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "false", "0", "1", "on", "off", "yes", "no":
		default:
			return false
		}
	}
	return true
}

func allFloat(vs []string) bool {
	for _, v := range vs {
		if _, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err != nil {
			return false
		}
	}
	return true
}

func allDate(vs []string) bool {
	for _, v := range vs {
		ok := false
		for _, f := range datePrefixes {
			if _, err := time.Parse(f, strings.TrimSpace(v)); err == nil {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return len(vs) > 0
}

// placeholder maps a field format to a sample value for body generation.
func placeholder(f Field) string {
	switch f.Kind {
	case "int":
		return "0"
	case "uint":
		return "1"
	case "float":
		return "0.0"
	case "bool":
		return "false"
	case "enum":
		if len(f.Values) > 0 {
			return f.Values[0]
		}
		return "<" + f.Name + ">"
	case "token":
		return "<" + f.Name + ">"
	case "date":
		return "<date>"
	case "json":
		return "{}"
	case "array":
		return "[]"
	case "null":
		return "null"
	default:
		return "<" + f.Name + ">"
	}
}

// buildSample generates a request body from the inferred format (not from the
// raw payload), so it can drive further fuzzing/generation.
func buildSample(b BodyFormat) string {
	sortFields := func(fs []Field) []Field {
		out := append([]Field(nil), fs...)
		sort.Slice(out, func(i, j int) bool {
			ti, tj := out[i].Kind == "token", out[j].Kind == "token"
			if ti != tj {
				return ti
			}
			return out[i].Name < out[j].Name
		})
		return out
	}
	switch b.Kind {
	case "json":
		var bd strings.Builder
		bd.WriteString("{")
		fields := sortFields(b.Fields)
		for i, f := range fields {
			if i > 0 {
				bd.WriteString(", ")
			}
			fmt.Fprintf(&bd, "%q: %s", f.Name, placeholder(f))
		}
		bd.WriteString("}")
		return bd.String()
	case "array":
		if len(b.Fields) == 0 {
			return "[]"
		}
		inner := buildSample(BodyFormat{Kind: "json", Fields: b.Fields})
		return "[" + inner + "]"
	case "jsonrpc":
		methods := b.Methods
		if len(methods) == 0 {
			methods = []string{"<method>"}
		}
		var samples []string
		for _, m := range methods {
			var params strings.Builder
			params.WriteString("{")
			fields := sortFields(b.Fields)
			for i, f := range fields {
				if i > 0 {
					params.WriteString(", ")
				}
				fmt.Fprintf(&params, "%q: %s", f.Name, placeholder(f))
			}
			params.WriteString("}")
			samples = append(samples, fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s,"id":1}`, m, params.String()))
		}
		return strings.Join(samples, "  /  ")
	case "form":
		var parts []string
		for _, f := range b.Fields {
			parts = append(parts, f.Name+"="+placeholder(f))
		}
		return strings.Join(parts, "&")
	default:
		return b.Sample
	}
}

// mergeBody parses a request body and merges it into the endpoint schema.
func mergeBody(e *Endpoint, body string, ct string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	if len(body) > maxBodyLen {
		body = body[:maxBodyLen]
	}
	if strings.Contains(ct, "json") || looksJSON(body) {
		if f, ok := parseJSONBody(body, ct); ok {
			mergeFormat(e, f)
			return
		}
		// JS object literal written with shorthand keys/values (e.g.
		// {cameraId:n}); JSON decoding fails but the shape is readable.
		if f, ok := parseShorthandObject(body); ok {
			mergeFormat(e, f)
			return
		}
	}
	if strings.Contains(ct, "x-www-form-urlencoded") || strings.Contains(ct, "multipart") ||
		(strings.Contains(body, "=") && looksForm(body)) {
		mergeFormat(e, parseFormBody(body, ct))
		return
	}
	if looksXML(body) {
		mergeFormat(e, BodyFormat{Kind: "xml", MIME: ct, Sample: "<xml>"})
		return
	}
	mergeFormat(e, BodyFormat{Kind: "text", MIME: ct, Sample: "<text>"})
}

func looksJSON(b string) bool {
	b = strings.TrimSpace(b)
	return strings.HasPrefix(b, "{") || strings.HasPrefix(b, "[")
}

func looksForm(b string) bool {
	return strings.Contains(b, "=") && !strings.Contains(b, "{")
}

func looksXML(b string) bool {
	return strings.HasPrefix(strings.TrimSpace(b), "<")
}

func mergeFormat(e *Endpoint, f BodyFormat) {
	key := f.Kind + "|" + f.MIME
	for i := range e.Bodies {
		b := &e.Bodies[i]
		if key == b.Kind+"|"+b.MIME {
			for _, fd := range f.Fields {
				b.Fields = addValue(b.Fields, fd.Name, fd.Values...)
			}
			b.Methods = addStrings(b.Methods, f.Methods...)
			if f.Sample != "" && b.Sample == "" {
				b.Sample = f.Sample
			}
			return
		}
	}
	e.Bodies = append(e.Bodies, f)
}

func addStrings(list []string, vs ...string) []string {
	for _, v := range vs {
		list = addString(list, v)
	}
	return list
}

// parseJSONBody decodes a JSON request body into a schema (plain json or
// json-rpc). Field values across observations are collected by name.
func parseJSONBody(body string, ct string) (BodyFormat, bool) {
	var raw any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return BodyFormat{}, false
	}
	switch v := raw.(type) {
	case []any:
		f := BodyFormat{Kind: "array", MIME: ct, Sample: "[]"}
		for _, el := range v {
			if m, ok := el.(map[string]any); ok {
				f.Fields = appendObjectFields(f.Fields, m, "")
				break
			}
		}
		return f, true
	case map[string]any:
		isRPC := objStr(v, "jsonrpc") != "" || (objStr(v, "method") != "" && (v["params"] != nil || v["id"] != nil))
		if isRPC {
			f := BodyFormat{Kind: "jsonrpc", MIME: ct}
			if m := objStr(v, "method"); m != "" {
				f.Methods = append(f.Methods, m)
			}
			if p, ok := v["params"].(map[string]any); ok {
				f.Fields = appendObjectFields(f.Fields, p, "")
			}
			return f, true
		}
		f := BodyFormat{Kind: "json", MIME: ct}
		f.Fields = appendObjectFields(f.Fields, v, "")
		return f, true
	}
	return BodyFormat{}, true
}

func objStr(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// parseShorthandObject extracts a field list from a JS object literal whose
// keys/values are written in shorthand (e.g. {cameraId:n, startTime:r}). Raw
// JSON decoding fails on such bodies, but the pair shape is still readable and
// worth reporting as a json contract instead of opaque text.
func parseShorthandObject(body string) (BodyFormat, bool) {
	s := strings.TrimSpace(body)
	if len(s) < 2 || !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return BodyFormat{}, false
	}
	if strings.ContainsAny(s, "()=>;`") {
		return BodyFormat{}, false
	}
	inner := s[1 : len(s)-1]
	f := BodyFormat{Kind: "json", MIME: ""}
	for _, part := range splitTopLevel(inner) {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(kv[0]), `"'`)
		if key == "" {
			continue
		}
		val := normalizeJSValue(kv[1])
		if len(val) > 64 {
			val = val[:64]
		}
		f.Fields = addFieldValue(f.Fields, key, val)
	}
	return f, len(f.Fields) > 0
}

// splitTopLevel splits a comma-separated body into top-level parts, ignoring
// commas inside strings, braces and brackets.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	var inStr byte
	start := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr != 0 {
			if ch == '\\' {
				i++
				continue
			}
			if ch == inStr {
				inStr = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			inStr = ch
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// normalizeJSValue strips quotes from a shorthand value (identifiers, numbers
// and strings pass through as their readable form).
func normalizeJSValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// appendObjectFields flattens a JSON object into fields, descending at most
// two levels (nested keys become "parent.child").
func appendObjectFields(fields []Field, m map[string]any, prefix string) []Field {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := k
		if prefix != "" {
			name = prefix + "." + k
		}
		val := m[k]
		switch t := val.(type) {
		case map[string]any:
			if prefix == "" {
				fields = appendObjectFields(fields, t, name)
			}
		case []any:
			fields = addFieldValue(fields, name, "[]")
		case nil:
			fields = addFieldValue(fields, name, "null")
		case float64:
			fields = addFieldValue(fields, name, strconv.FormatFloat(t, 'f', -1, 64))
		case bool:
			fields = addFieldValue(fields, name, strconv.FormatBool(t))
		default:
			fields = addFieldValue(fields, name, fmt.Sprint(val))
		}
	}
	return fields
}

// parseFormBody decodes a k=v&k=v body into fields.
func parseFormBody(body string, ct string) BodyFormat {
	f := BodyFormat{Kind: "form", MIME: redactMIME(ct), Sample: body}
	for _, part := range strings.Split(body, "&") {
		k, v, _ := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		f.Fields = addFieldValue(f.Fields, k, unescape(v))
	}
	return f
}

func redactMIME(ct string) string {
	switch {
	case strings.Contains(ct, "multipart"):
		return "multipart/form-data"
	case strings.Contains(ct, "x-www-form-urlencoded"):
		return "application/x-www-form-urlencoded"
	}
	return ct
}

// Render produces the human-readable contract section (plain text).
func Render(eps []Endpoint, raw bool) string {
	return render(eps, raw, false)
}

// RenderColored produces the contract section with color-coded tokens:
// methods, body formats, field kinds, placeholder path segments and labels.
func RenderColored(eps []Endpoint, raw bool) string {
	return render(eps, raw, true)
}

func render(eps []Endpoint, raw bool, col bool) string {
	var b strings.Builder
	for _, e := range eps {
		b.WriteString(paintPath(col, e.Path))
		b.WriteString("\n")
		b.WriteString(paint(col, color.Dim, "  methods: "))
		switch {
		case e.Unobserved && len(e.Methods) == 0:
			// State precisely what is known: the endpoint and, when present,
			// its parameters, but no resolved request.
			detail := "endpoint path in code"
			if len(e.Query) > 0 {
				detail += " with " + strconv.Itoa(len(e.Query)) + " query parameter(s)"
			}
			if len(e.Response) > 0 {
				detail += ", response shape inferred"
			}
			b.WriteString(paint(col, color.DarkGray, "unknown ("+detail+", no request resolved)"))
		default:
			for i, m := range e.Methods {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(paint(col, methodColor(m), m))
			}
			if e.Unobserved {
				b.WriteString(paint(col, color.DarkGray, " (unobserved)"))
			} else if e.MethodInferred {
				b.WriteString(paint(col, color.DarkYellow, " (inferred from call shape)"))
			}
		}
		b.WriteString("\n")
		b.WriteString(paint(col, color.Dim, "  calls:   "))
		if e.Unobserved {
			b.WriteString(paint(col, color.DarkGray, "n/a"))
		} else {
			b.WriteString(paint(col, color.DarkGray, strconv.Itoa(e.Calls)))
		}
		b.WriteString("\n")
		b.WriteString(paint(col, color.Dim, "  type:    "))
		renderDataType(&b, e, col)
		b.WriteString("\n")
		if len(e.Query) > 0 {
			b.WriteString(paint(col, color.Dim, "  query:\n"))
			renderFields(&b, e.Query, col)
		}
		if len(e.Headers) > 0 {
			b.WriteString(paint(col, color.Dim, "  headers:\n"))
			renderFields(&b, e.Headers, col)
		}
		if len(e.Bodies) > 0 {
			b.WriteString(paint(col, color.Dim, "  bodies:\n"))
			renderBodies(&b, e.Bodies, col)
		}
		if len(e.Response) > 0 {
			b.WriteString(paint(col, color.Dim, "  response:\n"))
			renderResponseFields(&b, e.Response, col)
		}
		if raw && len(e.Raw) > 0 {
			b.WriteString(paint(col, color.Dim, "  requests:\n"))
			for _, r := range e.Raw {
				b.WriteString("    " + paintRaw(col, r) + "\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderDataType prints the observed request-data format (body kind + MIME) for
// an endpoint. When a body-bearing method (POST/PUT/PATCH) is present but no
// body was observed, it says so explicitly instead of staying silent.
func renderDataType(b *strings.Builder, e Endpoint, col bool) {
	if len(e.Bodies) > 0 {
		for i, bd := range e.Bodies {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(paint(col, bodyFormatColor(bd.Kind), bd.Kind))
			if bd.MIME != "" {
				b.WriteString(paint(col, color.Dim, " ("+bd.MIME+")"))
			}
		}
		return
	}
	if bodyCapable(e) {
		b.WriteString(paint(col, color.DarkRed, "not found (no request body observed)"))
		return
	}
	b.WriteString(paint(col, color.DarkGray, "none"))
}

// bodyCapable reports whether an endpoint has a method that normally carries a
// request body.
func bodyCapable(e Endpoint) bool {
	for _, m := range e.Methods {
		switch strings.ToUpper(m) {
		case "POST", "PUT", "PATCH":
			return true
		}
	}
	return false
}

func renderFields(b *strings.Builder, fields []Field, col bool) {
	for _, f := range fields {
		b.WriteString("    " + paint(col, color.DarkCyan, pad(f.Name, 24)))
		b.WriteString(paint(col, fieldKindColor(f.Kind), pad(f.Kind, 8)))
		if f.Inferred && len(f.Values) == 0 {
			b.WriteString(paint(col, color.DarkGray, "(name from code, no value observed)"))
			b.WriteString("\n")
			continue
		}
		switch f.Kind {
		case "token":
			suffix := "(varies)"
			if f.Constant {
				suffix = "(const)"
			}
			b.WriteString(paint(col, color.DarkRed, "<"+f.Name+"> "+suffix))
		default:
			if f.Constant {
				b.WriteString(paint(col, color.Dim, Shield(f.Name, f.ConstVal)))
				b.WriteString(paint(col, color.DarkGray, " (const)"))
			} else {
				b.WriteString(paint(col, color.Dim, joinValues(f.Values)))
			}
		}
		if f.Nullable {
			b.WriteString(paint(col, color.DarkYellow, " (nullable)"))
		}
		b.WriteString("\n")
	}
}

func joinValues(vals []string) string {
	clean := vals
	if len(clean) > 10 {
		clean = clean[:10]
	}
	out := strings.Join(clean, ", ")
	if len(vals) > 10 {
		out += fmt.Sprintf(" (+%d more)", len(vals)-10)
	}
	return out
}

// renderResponseFields prints the statically inferred response schema: one
// indented path per line with the kind inferred from its usage.
func renderResponseFields(b *strings.Builder, fields []ResponseField, col bool) {
	for _, f := range fields {
		kind := f.Kind
		if kind == "" {
			kind = "any"
		}
		b.WriteString("    " + paint(col, responseKindColor(kind), kind) + " " + paint(col, color.Dim, f.Path) + "\n")
	}
}

func responseKindColor(kind string) color.Code {
	switch kind {
	case "array":
		return color.Yellow
	case "object":
		return color.Green
	case "number":
		return color.Cyan
	case "string":
		return color.Blue
	case "date":
		return color.Purple
	case "promise":
		return color.DarkGray
	}
	return color.DarkGray
}

func renderBodies(b *strings.Builder, bodies []BodyFormat, col bool) {
	for _, bd := range bodies {
		b.WriteString("    " + paint(col, bodyFormatColor(bd.Kind), bd.Kind))
		if bd.MIME != "" {
			b.WriteString(paint(col, color.Dim, " ("+bd.MIME+")"))
		}
		b.WriteString("\n")
		if len(bd.Methods) > 0 {
			b.WriteString("      methods : ")
			for i, m := range bd.Methods {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(paint(col, color.Cyan, m))
			}
			b.WriteString("\n")
		}
		if len(bd.Fields) > 0 {
			renderFields(b, bd.Fields, col)
		}
		if bd.Sample != "" {
			b.WriteString(paint(col, color.Dim, "      sample   : "))
			b.WriteString(paint(col, color.DarkCyan, truncate(bd.Sample)))
			b.WriteString("\n")
		}
	}
}

// paint applies ANSI color only when enabled.
func paint(col bool, c color.Code, s string) string {
	if !col {
		return s
	}
	return color.Colorize(c, s)
}

// pad right-pads s to width n (on visible characters, so colors are applied
// after padding and alignment is preserved).
func pad(s string, n int) string {
	if len(s) < n {
		return s + strings.Repeat(" ", n-len(s))
	}
	return s
}

func methodColor(m string) color.Code {
	switch strings.ToUpper(m) {
	case "GET":
		return color.Green
	case "POST":
		return color.Yellow
	case "PUT":
		return color.Blue
	case "PATCH":
		return color.Purple
	case "DELETE":
		return color.Red
	default:
		return color.DarkGray
	}
}

func fieldKindColor(k string) color.Code {
	switch k {
	case "token":
		return color.DarkRed
	case "date":
		return color.Blue
	case "bool", "int", "uint", "float":
		return color.Green
	case "enum":
		return color.Cyan
	case "json", "array":
		return color.DarkCyan
	case "null", "unknown":
		return color.DarkGray
	default:
		return color.White
	}
}

func bodyFormatColor(k string) color.Code {
	switch k {
	case "json":
		return color.Cyan
	case "jsonrpc":
		return color.Purple
	case "form":
		return color.DarkYellow
	case "array":
		return color.DarkCyan
	case "text":
		return color.Gray
	case "xml":
		return color.DarkGray
	case "none":
		return color.DarkRed
	default:
		return color.White
	}
}

// pathPlaceholderRE matches dynamic {…} path segments rendered as {var}.
var pathPlaceholderRE = regexp.MustCompile(`\{[^}]*\}`)

// paintPath highlights {placeholder} segments inside an endpoint path.
func paintPath(col bool, s string) string {
	if !col {
		return s
	}
	return pathPlaceholderRE.ReplaceAllStringFunc(s, func(seg string) string {
		return color.Colorize(color.Purple, seg)
	})
}

// paintRaw colorizes one rendered request line: the method, the body marker,
// header assignments and path placeholders.
func paintRaw(col bool, r string) string {
	if !col {
		return r
	}
	sp := strings.IndexByte(r, ' ')
	head, rest := r, ""
	if sp > 0 {
		head, rest = r[:sp], r[sp:]
	}
	var b strings.Builder
	b.WriteString(color.Colorize(methodColor(head), head))
	if hm := strings.Index(rest, " : "); hm >= 0 {
		b.WriteString(paintPath(true, rest[:hm]))
		b.WriteString(color.Colorize(color.DarkGray, " : "))
		rest = rest[hm+3:]
	} else {
		b.WriteString(paintPath(true, rest))
		rest = ""
	}
	if bm := strings.Index(rest, " ["); bm >= 0 {
		b.WriteString(color.Colorize(color.DarkYellow, rest[:bm]))
		rest = rest[bm:]
		if strings.HasPrefix(rest, " [") {
			close := strings.LastIndex(rest, "]")
			if close > 0 {
				inner := rest[2:close]
				b.WriteString(color.Colorize(color.DarkGray, " ["))
				items := strings.Split(inner, ", ")
				for i, it := range items {
					if i > 0 {
						b.WriteString(color.Colorize(color.DarkGray, ", "))
					}
					b.WriteString(paintHeaderItem(it))
				}
				b.WriteString(color.Colorize(color.DarkGray, "]"))
				rest = rest[close+1:]
			}
		}
	}
	b.WriteString(rest)
	return b.String()
}

// paintHeaderItem colorizes one "Name=value" assignment in a rendered request.
func paintHeaderItem(item string) string {
	eq := strings.IndexByte(item, '=')
	if eq > 0 {
		return color.Colorize(color.Purple, item[:eq]) + color.Colorize(color.Dim, item[eq:])
	}
	return item
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// SamplesFor returns the generated request bodies for an endpoint, one per
// (method, body format) pair — the ready-to-reuse payloads for further probing.
func SamplesFor(e Endpoint) []string {
	var out []string
	for _, m := range e.Methods {
		if len(e.Bodies) == 0 {
			out = append(out, fmt.Sprintf("%s %s  (no body)", m, e.Path))
			continue
		}
		for _, bd := range e.Bodies {
			lines := strings.Split(bd.Sample, "  /  ")
			for _, s := range lines {
				out = append(out, fmt.Sprintf("%s %s  body: %s", m, e.Path, s))
			}
		}
	}
	return out
}
