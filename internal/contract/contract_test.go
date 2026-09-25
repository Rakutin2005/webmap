package contract

import (
	"strings"
	"testing"
)

func TestInferURLAadIntKinds(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/zhk/detail.php?ID=56360", Method: "GET"},
		{URL: "https://x.test/zhk/detail.php?ID=56390", Method: "GET"},
		{URL: "https://x.test/zhk/detail.php?ID=68108", Method: "GET"},
	}
	eps := Infer(obs)
	if len(eps) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(eps))
	}
	e := eps[0]
	if e.Path != "https://x.test/zhk/detail.php" {
		t.Errorf("path: %s", e.Path)
	}
	if len(e.Query) != 1 || e.Query[0].Kind != "uint" {
		t.Fatalf("ID kind: %+v", e.Query)
	}
	if len(e.Query[0].Values) != 3 {
		t.Errorf("ID evidence: %v", e.Query[0].Values)
	}
	if containsStrings(Render(eps, false), "56360") == false {
		t.Errorf("evidence should be rendered")
	}
}

func TestInferQueryEnumAndTokenMasking(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/ajax.php?action=user.get&sessid=abc123abc123", Method: "POST", Body: "c=x&action=user.get&sessid=abc123abc123"},
		{URL: "https://x.test/ajax.php?action=catalog.load&sessid=def456def456", Method: "POST", Body: "c=x&action=catalog.load&sessid=def456def456"},
	}
	eps := Infer(obs)
	e := eps[0]
	var action, sessid *Field
	for i := range e.Query {
		switch e.Query[i].Name {
		case "action":
			action = &e.Query[i]
		case "sessid":
			sessid = &e.Query[i]
		}
	}
	if action == nil || action.Kind != "enum" {
		t.Errorf("action kind: %+v", e.Query)
	}
	if sessid == nil || sessid.Kind != "token" {
		t.Errorf("sessid kind: %+v", e.Query)
	}
	// Token values must never be echoed, even in raw mode.
	out := Render(eps, true)
	for _, leak := range []string{"abc123abc123", "def456def456"} {
		if strings.Contains(out, leak) {
			t.Errorf("token leaked in output: %s", leak)
		}
	}
}

// TestFormatInferredFromBodies mirrors the user's example: {"number": 2},
// {"number":18}, {"number":9018} → "number" is an unsigned integer.
func TestFormatInferredFromBodies(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/api/sum", Method: "POST", Headers: []NameValue{{"content-type", "application/json"}}, Body: `{"number": 2}`},
		{URL: "https://x.test/api/sum", Method: "POST", Headers: []NameValue{{"content-type", "application/json"}}, Body: `{"number": 18}`},
		{URL: "https://x.test/api/sum", Method: "POST", Headers: []NameValue{{"content-type", "application/json"}}, Body: `{"number": 9018}`},
	}
	eps := Infer(obs)
	e := eps[0]
	if len(e.Bodies) != 1 || e.Bodies[0].Kind != "json" {
		t.Fatalf("bodies: %+v", e.Bodies)
	}
	fields := e.Bodies[0].Fields
	if len(fields) != 1 || fields[0].Name != "number" || fields[0].Kind != "uint" {
		t.Fatalf("number field: %+v", fields)
	}
	if len(fields[0].Values) != 3 || fields[0].Values[0] != "2" {
		t.Errorf("number evidence: %v", fields[0].Values)
	}
	// The generated body must use the format, not concrete data.
	if !strings.Contains(e.Bodies[0].Sample, `"number": 1`) {
		t.Errorf("sample: %s", e.Bodies[0].Sample)
	}
}

func TestJSONRPC(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/rpc", Method: "POST", Headers: []NameValue{{"content-type", "application/json"}},
			Body: `{"jsonrpc":"2.0","method":"USER.GET","params":{"id":5},"id":1}`},
		{URL: "https://x.test/rpc", Method: "POST", Headers: []NameValue{{"content-type", "application/json"}},
			Body: `{"jsonrpc":"2.0","method":"USER.LIST","params":{"page":2},"id":2}`},
	}
	eps := Infer(obs)
	e := eps[0]
	if len(e.Bodies) != 1 || e.Bodies[0].Kind != "jsonrpc" {
		t.Fatalf("bodies: %+v", e.Bodies)
	}
	if len(e.Bodies[0].Methods) != 2 {
		t.Errorf("methods: %v", e.Bodies[0].Methods)
	}
	// params fields merged across calls: id (uint), page (uint)
	if len(e.Bodies[0].Fields) != 2 {
		t.Errorf("params fields: %+v", e.Bodies[0].Fields)
	}
	if !strings.Contains(e.Bodies[0].Sample, "USER.GET") {
		t.Errorf("jsonrpc sample should enumerate methods: %s", e.Bodies[0].Sample)
	}
}

func TestFormBody(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/bitrix/tools/conversion/ajax_counter.php", Method: "POST",
			Headers: []NameValue{{"content-type", "application/x-www-form-urlencoded"}},
			Body:    "sessid=abc123&action=conversion&mode=ajax"},
		{URL: "https://x.test/bitrix/tools/conversion/ajax_counter.php", Method: "POST",
			Headers: []NameValue{{"content-type", "application/x-www-form-urlencoded"}},
			Body:    "sessid=def456&action=conversion&mode=ajax"},
	}
	eps := Infer(obs)
	e := eps[0]
	if len(e.Bodies) != 1 || e.Bodies[0].Kind != "form" {
		t.Fatalf("bodies: %+v", e.Bodies)
	}
	names := map[string]string{}
	for _, f := range e.Bodies[0].Fields {
		names[f.Name] = f.Kind
	}
	if names["action"] != "str" || names["mode"] != "str" {
		t.Errorf("form fields: %+v", e.Bodies[0].Fields)
	}
	if names["sessid"] != "token" {
		t.Errorf("sessid must be token: %+v", e.Bodies[0].Fields)
	}
	for _, f := range e.Bodies[0].Fields {
		if f.Name == "action" && !f.Constant {
			t.Errorf("action is constant and should be flagged")
		}
		if f.Name == "sessid" && f.Constant {
			t.Errorf("sessid varies and must not be constant")
		}
	}
	// Generated form body keeps the format, not secrets.
	if strings.Contains(e.Bodies[0].Sample, "abc123") {
		t.Errorf("sample leaked sessid: %s", e.Bodies[0].Sample)
	}
	if !strings.Contains(e.Bodies[0].Sample, "sessid=<sessid>") {
		t.Errorf("sample: %s", e.Bodies[0].Sample)
	}
}

func TestSamplesFor(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/api/sum", Method: "POST", Headers: []NameValue{{"content-type", "application/json"}}, Body: `{"number": 2}`},
	}
	eps := Infer(obs)
	s := SamplesFor(eps[0])
	if len(s) != 1 || !strings.Contains(s[0], "number") {
		t.Errorf("samples: %v", s)
	}
}

func TestShieldLeavesPlainValues(t *testing.T) {
	if got := Shield("ID", "56360"); got != "56360" {
		t.Errorf("plain value must pass through, got %s", got)
	}
	if got := Shield("sessid", "abc123"); got != "<sessid>" {
		t.Errorf("token must be masked, got %s", got)
	}
	// MIME types must not be confused with secret-looking tokens.
	if got := Shield("Content-Type", "application/x-www-form-urlencoded"); got != "application/x-www-form-urlencoded" {
		t.Errorf("MIME value wrongly tokenized, got %s", got)
	}
	if got := Shield("Content-Type", "multipart/form-data"); got != "multipart/form-data" {
		t.Errorf("MIME value wrongly tokenized, got %s", got)
	}
	if got := Shield("Val", "dddddddddddddddddddddddddddddddddddd"); got != "dddddddddddddddddddddddddddddddddddd" {
		t.Errorf("digit-less long word wrongly tokenized, got %s", got)
	}
	if got := Shield("Val", "A0b1C2d3E4f5G6h7I8j9K0L1M2N3O4P5Q6R7S8T9U0="); got != "<Val>" {
		t.Errorf("mixed base64-like token not masked, got %s", got)
	}
}

// TestShorthandObjectFromMinifiedClient mirrors bodies mined from a minified
// bundle: raw JS object literals with bare keys and identifiers must surface as
// a named-field json contract rather than opaque "text".
func TestShorthandObjectFromMinifiedClient(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/api/stream/start", Method: "POST",
			Body: `{cameraId:n,startTime:r,endTime:s}`},
		{URL: "https://x.test/api/stream/start", Method: "POST",
			Body: `{cameraId:77,startTime:"2026-01-01T10:00:00Z"}`},
	}
	eps := Infer(obs)
	e := eps[0]
	if len(e.Bodies) != 1 || e.Bodies[0].Kind != "json" {
		t.Fatalf("bodies: %+v", e.Bodies)
	}
	if len(e.Bodies[0].Fields) != 3 {
		t.Fatalf("fields: %+v", e.Bodies[0].Fields)
	}
	out := Render(eps, true)
	for _, want := range []string{"cameraId", "startTime", "endTime"} {
		if !strings.Contains(out, want) {
			t.Errorf("field %q missing from render:\n%s", want, out)
		}
	}
}

// TestRenderColoredKeepsTokenMasking ensures ANSI coloring never leaks masked
// token values and that colored output differs from plain output.
func TestRenderColoredKeepsTokenMasking(t *testing.T) {
	obs := []Observation{
		{URL: "https://x.test/api/admin", Method: "POST",
			Headers: []NameValue{{"content-type", "application/json"}, {"X-Api-Token", "abcdef1234567890abcdef"}},
			Body:    `{"role": "admin"}`},
		{URL: "https://x.test/api/upload", Method: "POST"},
		{URL: "https://x.test/tickets/{t}", Method: "GET"},
	}
	eps := Infer(obs)
	plain := Render(eps, true)
	col := RenderColored(eps, true)
	if !strings.Contains(col, "\x1b[") {
		t.Errorf("colored render must contain ANSI codes")
	}
	for _, leak := range []string{"abcdef1234567890abcdef"} {
		if strings.Contains(plain, leak) || strings.Contains(col, leak) {
			t.Errorf("token leaked: %s", leak)
		}
	}
	if !strings.Contains(col, "not found") {
		t.Errorf("POST without body must say not found:\n%s", col)
	}
}

func containsStrings(s string, subs string) bool { return strings.Contains(s, subs) }
