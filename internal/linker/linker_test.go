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
