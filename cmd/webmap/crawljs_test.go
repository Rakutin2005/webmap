package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"apimap/internal/config"
	"apimap/internal/fetcher"
	"apimap/internal/progress"
)

// A script referenced with a cache-busting query must still be analyzed.
// The fetched result carries the query string, so a plain ".js" suffix check
// silently discarded these bundles and every endpoint inside them.
func TestCrawlAnalyzesCachedBustedJS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>` +
			`<script src="/cached.js?v=1789992068"></script>` +
			`<script src="/plain.js"></script>` +
			`</body></html>`))
	})
	mux.HandleFunc("/cached.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`fetch('/api/from-cached-bundle', {method:'POST'});`))
	})
	mux.HandleFunc("/plain.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`fetch('/api/from-plain-bundle', {method:'POST'});`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{AnalyzeJS: true, URL: srv.URL}
	f, err := fetcher.New(cfg.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, l := range crawl(f, cfg, 1, progress.New(1), 4, 0, false) {
		found[l.HREF] = true
	}
	for _, want := range []string{"/api/from-cached-bundle", "/api/from-plain-bundle"} {
		if !found[want] {
			t.Errorf("endpoint %s not discovered; got %v", want, found)
		}
	}
}
