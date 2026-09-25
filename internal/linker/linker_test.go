package linker

import "testing"

func TestCategorizeAPI(t *testing.T) {
	cases := []struct {
		href string
		want Category
	}{
		// API endpoints (path patterns and dynamic .php handlers).
		{"/apartments/ajax.php", CategoryAPI},
		{"/bitrix/services/main/ajax.php?action=getData", CategoryAPI},
		{"/bitrix/tools/conversion/ajax_counter.php", CategoryAPI},
		{"/api/users", CategoryAPI},
		{"/rest/v2/orders", CategoryAPI},
		{"/graphql", CategoryAPI},
		{"/wp-json/wp/v2/posts", CategoryAPI},
		{"/handler.php?id=5", CategoryAPI},

		// Static assets must not be reclassified as API.
		{"/js/api.js", CategoryWebAsset},
		{"/assets/app.min.js", CategoryWebAsset},
		{"/styles/main.css", CategoryWebAsset},
		{"/img/logo.png", CategoryWebAsset},
		{"/data/manifest.json", CategoryWebAsset},

		// Plain pages.
		{"/about/team", CategoryWebPage},
		{"/", CategoryWebPage},
		{"/contacts.html", CategoryWebPage},
	}
	for _, c := range cases {
		if got := categorize(c.href); got != c.want {
			t.Errorf("categorize(%q) = %v, want %v", c.href, got, c.want)
		}
	}
}

func TestParseIgnoresMinifiedCodeFragments(t *testing.T) {
	body := "\n" +
		"var e, $e;\n" +
		"$e.set(\"created_from\", le.from);\n" +
		"$e.set(\"created_to\", le.to);\n" +
		"switch (fe) { case \"bigint\": break; case \"string\": x(); }\n" +
		"const HLS = \"IMPORT\" in z ? bme(d,z,l) : YO(d,z,n)};\n" +
		"decodeURIComponent(\"bad percent encoding (${e})\");\n" +
		"e.get(\"/api/admin/cameras\");\n" +
		"import {computed as le} from '@vue/runtime-core';\n" +
		"export default g;\n"
	links := Parse(body, "https://osi.example.com/assets/index.js")
	for _, l := range links {
		if l.Tag == "js-import" && !isValidModuleSpecifier(l.HREF) {
			t.Errorf("junk js-import survived: %q", l.HREF)
		}
	}
	// The only js-import that should survive is the real module specifier.
	found := false
	for _, l := range links {
		if l.Tag == "js-import" {
			found = true
			if l.HREF != "@vue/runtime-core" {
				t.Errorf("unexpected js-import href %q", l.HREF)
			}
		}
	}
	if !found {
		t.Error("valid import specifier was lost")
	}
}

func TestIsValidModuleSpecifier(t *testing.T) {
	valid := []string{"/assets/fragment.js", "./helpers/calc.js", "@vue/runtime-core", "vue", "axios", "https://cdn.example.com/x.js", "../../lib/j.js"}
	for _, s := range valid {
		if !isValidModuleSpecifier(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	invalid := []string{",le.from),le.to&&$e.set(", ",t.created_from),t.created_to&&e.set(", ":case", "in z?bme(d,z,l)}break}case", "${e}", "getAllResponseHeaders", "a b c"}
	for _, s := range invalid {
		if isValidModuleSpecifier(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestAnalyzeJS(t *testing.T) {
	js := "\n" +
		"const pickFrom = '/assets/' + (le.from);\n" +
		"$e.set('created_from', le.from);\n" +
		"e.append(\"created_to\", t.to);\n" +
		"upd.set('house_id', E);\n" +
		"const snap = `/api/admin/cameras/${id}/snapshot`;\n" +
		"const chunk = `/assets/${e}.js`;\n" +
		"const apiRoot = `${apiBase}/api/v2/counters`;\n"
	// Parameter names are recovered structurally by the JS analyzer now, so this
	// pass only reports URL templates.
	templates := AnalyzeJS(js, "https://osi.example.com/assets/index.js")

	hrefs := make(map[string]bool)
	for _, tm := range templates {
		hrefs[tm.HREF] = true
	}
	if !hrefs["/api/admin/cameras/{id}/snapshot"] {
		t.Errorf("api template missing; got %v", hrefs)
	}
	if !hrefs["/assets/{e}.js"] {
		t.Errorf("asset template missing; got %v", hrefs)
	}
	if !hrefs["{apiBase}/api/v2/counters"] {
		t.Errorf("apiRoot template missing; got %v", hrefs)
	}
}
