package jsanalyzer

import (
	"strings"
	"testing"

	"apimap/internal/contract"
)

// Requests must be recognised by the shape of the call, not by the name of the
// client: wrappers, aliases, minified identifiers and computed member access all
// have to work, and the verb must be labelled as inferred when the code does
// not state it.
func TestStructuralRequestDetection(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		wantURL    string
		wantMethod string
		inferred   bool
	}{
		// A payload alone is not a POST: every common client defaults to GET.
		{"minified identifier", `q("/api/a",{f:1});`, "/api/a", "GET", true},
		{"custom client object", `myHttp.post("/api/b",{id:1});`, "/api/b", "POST", false},
		{"computed member", `api["post"]("/api/c",{id:1});`, "/api/c", "POST", false},
		{"unknown verb", `transport("/api/d",{id:1});`, "/api/d", "GET", true},
		{"config object", `request({url:"/api/e", method:"PUT", data:{x:1}});`, "/api/e", "PUT", false},
		{"config method DELETE", `zz.send({url:"/api/f", method:"DELETE"});`, "/api/f", "DELETE", false},
		{"string concatenation", `get("/api/"+"g?x=1");`, "/api/g?x=1", "GET", false},
		{"read verb puts payload in query", `client.load("/api/h",{page:2});`, "/api/h?page=2", "GET", false},
		{"IIFE with inner helper", `(function(e,t){function n(r){return fetch(r,{method:"POST",body:t})}n("/api/i",{a:1});n("/api/j",{b:2})})(window);`, "/api/i", "GET", true},
		{"deeply nested config", `function a(){function b(){function c(){return $.ajax({url:"/api/k",type:"POST",data:{d:4}})}}}`, "/api/k", "POST", false},
		{"unbalanced braces", `if(x){if(y){if(z){}}}` + `post("/api/l",{c:3});`, "/api/l", "POST", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, obs := Parse(tc.src, "https://h/", true)
			var hit *contract.Observation
			for i := range obs {
				if strings.Contains(obs[i].URL, tc.wantURL) {
					hit = &obs[i]
					break
				}
			}
			if hit == nil {
				t.Fatalf("%s not found in %+v", tc.wantURL, obs)
			}
			if hit.EndpointOnly {
				t.Fatalf("%s reported as a bare path, not a request", tc.wantURL)
			}
			if hit.Method != tc.wantMethod {
				t.Errorf("method = %q, want %q", hit.Method, tc.wantMethod)
			}
			if hit.MethodInferred != tc.inferred {
				t.Errorf("MethodInferred = %v, want %v", hit.MethodInferred, tc.inferred)
			}
		})
	}
}

// A stray semicolon used to desynchronise the parser: parseStmt consumed the
// token and parseProgram advanced again, silently dropping everything that
// followed. Minified bundles are full of them.
func TestParserSurvivesStraySemicolons(t *testing.T) {
	src := `function n(e){return e}; n("/api/after-semicolon",{a:1});`
	_, obs := Parse(src, "https://h/", true)
	for _, o := range obs {
		if strings.Contains(o.URL, "/api/after-semicolon") {
			return
		}
	}
	t.Fatalf("call after a stray semicolon was lost: %+v", obs)
}

// A plain string list is not a request: no call encloses the literals.
func TestArrayOfURLsIsNotARequest(t *testing.T) {
	_, obs := Parse(`var routes=["/api/a","/api/b"];`, "https://h/", true)
	for _, o := range obs {
		if !o.EndpointOnly {
			t.Errorf("array literal reported as a request: %+v", o)
		}
	}
}

// Response schemas must be inferred without a parse tree: on minified bundles
// the AST pass collapses, and the response shape is the most valuable part of
// an AJAX contract.
func TestResponseSchemaFromTokens(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"promise chain", `fetch("/api/a").then(r=>r.json()).then(d=>{d.items.length; d.meta.total;});`,
			[]string{"items.length", "meta.total"}},
		{"jQuery trailing callback", `$.post("/api/b",{a:1}, function(res){ res.data.id; });`,
			[]string{"data.id"}},
		{"jQuery success property", `$.ajax({url:"/api/c", data:{a:1}, success:function(r){ r.rows.length; r.total; }});`,
			[]string{"rows.length", "total"}},
		{"deferred done", `client.get("/api/d").done(function(r){ r.list.length; });`,
			[]string{"list.length"}},
		{"minified bracket access", `e("/api/e").then(function(t){ t["meta"]["count"]; });`,
			[]string{"meta", "meta.count"}},
		{"no callback", `fetch("/api/f");`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, obs := Parse(tc.src, "https://h/", true)
			got := map[string]bool{}
			for _, o := range obs {
				for _, f := range o.ResponseFields {
					got[f.Path] = true
				}
			}
			for _, want := range tc.want {
				if !got[want] {
					t.Errorf("response field %q missing, got %v", want, keys(got))
				}
			}
		})
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A client instance created with a base URL reports the URL that is actually
// requested, and the token scan must not re-report the unresolved relative path.
func TestClientInstanceBaseURL(t *testing.T) {
	src := `const api = axios.create({baseURL:"/api/v2"}); api.get("/users",{params:{page:1}});`
	_, obs := Parse(src, "https://h/", true)
	if len(obs) != 1 {
		t.Fatalf("expected one observation, got %d: %+v", len(obs), obs)
	}
	if obs[0].URL != "https://h/api/v2/users?page=1" {
		t.Errorf("url = %q, want https://h/api/v2/users?page=1", obs[0].URL)
	}
}
