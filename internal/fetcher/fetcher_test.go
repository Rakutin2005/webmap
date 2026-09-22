package fetcher

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func echoServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "xyz"})
			return
		}
		fmt.Fprintf(w, "AUTH=%s|COOKIE=%s|KEY=%s",
			r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Api-Key"))
	}))
}

func TestHeadersSentInScope(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	f, _ := New(srv.URL, false)
	f.SetHeaders(http.Header{"Authorization": {"Bearer T"}, "X-Api-Key": {"k1"}})
	f.SetHeaderScope(func(string) bool { return true })
	res, err := f.Fetch(srv.URL + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Body, "AUTH=Bearer T") || !strings.Contains(res.Body, "KEY=k1") {
		t.Errorf("expected headers sent, got %q", res.Body)
	}
}

func TestHeadersWithheldOutOfScope(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	f, _ := New(srv.URL, false)
	f.SetHeaders(http.Header{"Authorization": {"Bearer T"}})
	f.SetHeaderScope(func(string) bool { return false }) // out of scope
	res, err := f.Fetch(srv.URL + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Body, "AUTH=|") {
		t.Errorf("auth header should have been withheld, got %q", res.Body)
	}
}

func TestCookieJarPersists(t *testing.T) {
	srv := echoServer()
	defer srv.Close()
	f, _ := New(srv.URL, false)
	if _, err := f.Fetch(srv.URL + "/set"); err != nil { // server sets sid=xyz
		t.Fatal(err)
	}
	res, err := f.Fetch(srv.URL + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Body, "COOKIE=sid=xyz") {
		t.Errorf("expected jar to resend cookie, got %q", res.Body)
	}
}
