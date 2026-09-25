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
		{"minified identifier", `q("/api/a",{f:1});`, "/api/a", "POST", true},
		{"custom client object", `myHttp.post("/api/b",{id:1});`, "/api/b", "POST", false},
		{"computed member", `api["post"]("/api/c",{id:1});`, "/api/c", "POST", false},
		{"unknown verb", `transport("/api/d",{id:1});`, "/api/d", "POST", true},
		{"config object", `request({url:"/api/e", method:"PUT", data:{x:1}});`, "/api/e", "PUT", false},
		{"config method DELETE", `zz.send({url:"/api/f", method:"DELETE"});`, "/api/f", "DELETE", false},
		{"string concatenation", `get("/api/"+"g?x=1");`, "/api/g?x=1", "GET", false},
		{"read verb puts payload in query", `client.load("/api/h",{page:2});`, "/api/h?page=2", "GET", false},
		{"IIFE with inner helper", `(function(e,t){function n(r){return fetch(r,{method:"POST",body:t})}n("/api/i",{a:1});n("/api/j",{b:2})})(window);`, "/api/i", "POST", true},
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
