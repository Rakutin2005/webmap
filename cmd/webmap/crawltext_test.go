package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"apimap/internal/config"
	"apimap/internal/fetcher"
	"apimap/internal/progress"
)

// -parse-text must fetch linked text documents and read their links, and it
// must read code that is only pretending to be a document: a .txt holding
// JavaScript is analyzed for endpoints just as a .js file would be.
func TestCrawlParsesTextDocuments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>` +
			`<a href="/notes.txt">notes</a>` +
			`<a href="/code.txt">code</a>` +
			`</body></html>`))
	})
	mux.HandleFunc("/notes.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("see https://notes.example.com/help for help"))
	})
	mux.HandleFunc("/code.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("var base='http://code.example.com';\n" +
			"function go(){ return fetch('/api/from-text-code'); }\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	run := func(parseText bool) map[string]bool {
		cfg := &config.Config{ParseText: parseText, URL: srv.URL}
		f, err := fetcher.New(cfg.URL, false)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, l := range crawl(f, cfg, 1, progress.New(1), 4, 0, false) {
			found[l.HREF] = true
		}
		return found
	}

	on := run(true)
	for _, want := range []string{"https://notes.example.com/help", "/api/from-text-code"} {
		if !on[want] {
			t.Errorf("-parse-text: %s not discovered; got %v", want, on)
		}
	}

	off := run(false)
	for _, unwanted := range []string{"https://notes.example.com/help", "/api/from-text-code"} {
		if off[unwanted] {
			t.Errorf("without -parse-text, %s must not be discovered; got %v", unwanted, off)
		}
	}
}
