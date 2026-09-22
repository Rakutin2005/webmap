package fetcher

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

type Fetcher struct {
	client  *http.Client
	baseURL *url.URL
	headers http.Header
	// inScope decides whether user-supplied headers/cookies may be sent to a
	// given host. Nil means "all hosts". Used to avoid leaking auth tokens to
	// third-party domains (CDNs, analytics) encountered while crawling.
	inScope func(host string) bool
}

type FetchResult struct {
	URL      string
	Status   int
	Body     string
	Headers  http.Header
	Duration time.Duration
}

func New(baseURL string, skipTLS bool) (*Fetcher, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	transport := &http.Transport{}
	if skipTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
	// An in-memory jar persists Set-Cookie across requests so a session
	// established by supplied cookies/login survives the whole crawl.
	if jar, err := cookiejar.New(nil); err == nil {
		client.Jar = jar
	}

	return &Fetcher{
		client:  client,
		baseURL: parsedURL,
		headers: http.Header{},
	}, nil
}

// SetHeaders sets the headers applied to every request (subject to scope).
func (f *Fetcher) SetHeaders(h http.Header) {
	if h != nil {
		f.headers = h
	}
}

// SetHeaderScope restricts user headers/cookies to hosts for which inScope
// returns true. Pass nil to send them to every host.
func (f *Fetcher) SetHeaderScope(inScope func(host string) bool) {
	f.inScope = inScope
}

func (f *Fetcher) Fetch(targetURL string) (*FetchResult, error) {
	var urlToFetch string
	if targetURL == "" {
		urlToFetch = f.baseURL.String()
	} else if strings.HasPrefix(targetURL, "http://") || strings.HasPrefix(targetURL, "https://") {
		urlToFetch = targetURL
	} else if strings.HasPrefix(targetURL, "//") {
		urlToFetch = f.baseURL.Scheme + ":" + targetURL
	} else if strings.HasPrefix(targetURL, "/") {
		urlToFetch = f.baseURL.Scheme + "://" + f.baseURL.Host + targetURL
	} else {
		u := f.baseURL.ResolveReference(&url.URL{Path: targetURL})
		urlToFetch = u.String()
	}

	req, err := http.NewRequest(http.MethodGet, urlToFetch, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	f.applyHeaders(req)

	start := time.Now()
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	return &FetchResult{
		URL:      urlToFetch,
		Status:   resp.StatusCode,
		Body:     string(body),
		Headers:  resp.Header,
		Duration: time.Since(start),
	}, nil
}

// applyHeaders copies the configured headers onto req, unless the request host
// is out of scope (in which case user-supplied headers/cookies are withheld).
func (f *Fetcher) applyHeaders(req *http.Request) {
	if len(f.headers) == 0 {
		return
	}
	if f.inScope != nil && !f.inScope(req.URL.Hostname()) {
		return
	}
	for key, vals := range f.headers {
		req.Header.Del(key)
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}
}

func (f *Fetcher) ResolveURL(href string) string {
	if href == "" || strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	if strings.HasPrefix(href, "//") {
		return f.baseURL.Scheme + ":" + href
	}
	if strings.HasPrefix(href, "/") {
		return f.baseURL.Scheme + "://" + f.baseURL.Host + href
	}
	u := f.baseURL.ResolveReference(&url.URL{Path: href})
	return u.String()
}

func (f *Fetcher) GetBaseURL() *url.URL {
	return f.baseURL
}
