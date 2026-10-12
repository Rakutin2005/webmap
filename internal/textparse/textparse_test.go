package textparse

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"testing"

	"apimap/internal/linker"
)

func hasKind(kinds []Kind, want Kind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// hrefSet collects the link targets so a test can assert membership without
// depending on ordering.
func hrefSet(links []string) map[string]bool {
	m := make(map[string]bool, len(links))
	for _, l := range links {
		m[l] = true
	}
	return m
}

func TestExtKind(t *testing.T) {
	cases := []struct {
		url  string
		want Kind
	}{
		{"https://x/a.txt", KindText},
		{"https://x/a.csv?v=1", KindText},
		{"https://x/a.json", KindText},
		{"https://x/a.pdf", KindPDF},
		{"https://x/a.docx", KindZip},
		{"https://x/a.odt", KindZip},
		{"https://x/a.xlsx", KindZip},
		{"https://x/a.doc", KindLegacy},
		{"https://x/a.rtf", KindLegacy},
		{"https://x/a.html", KindNone},
		{"https://x/a.png", KindNone},
		{"https://x/noext", KindNone},
	}
	for _, tc := range cases {
		if got := extKind(tc.url); got != tc.want {
			t.Errorf("extKind(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestDetectKindsSniffsMisleadingExtension(t *testing.T) {
	// A .txt that is really a PDF: the magic number must win.
	kinds := DetectKinds("%PDF-1.7\n...", "https://x/a.txt", "text/plain")
	if !hasKind(kinds, KindPDF) {
		t.Fatalf("expected PDF kind from magic bytes, got %v", kinds)
	}
	// A .pdf that is really text: the content type and the bytes say text.
	kinds = DetectKinds("hello https://example.com", "https://x/a.pdf", "application/pdf")
	if !hasKind(kinds, KindText) {
		t.Fatalf("expected text kind for textual content, got %v", kinds)
	}
}

func TestDetectKindsHTMLIsSkipped(t *testing.T) {
	kinds := DetectKinds("<html><body><a href='/x'>y</a></body></html>", "https://x/", "text/html")
	if len(kinds) != 0 {
		t.Fatalf("HTML pages must not be treated as text documents, got %v", kinds)
	}
}

func TestParsePlainText(t *testing.T) {
	body := "readme\nsee https://api.example.com/v1/users and www.example.com too\n"
	res := Parse(body, "https://src.example/readme.txt", "https://src.example/readme.txt", "text/plain", false)
	if !hasKind(res.Kinds, KindText) {
		t.Fatalf("expected text kind, got %v", res.Kinds)
	}
	got := make([]string, 0, len(res.Links))
	for _, l := range res.Links {
		got = append(got, l.HREF)
	}
	set := hrefSet(got)
	if !set["https://api.example.com/v1/users"] {
		t.Errorf("missing absolute URL, got %v", got)
	}
	if !set["http://www.example.com"] {
		t.Errorf("missing www URL, got %v", got)
	}
}

func TestParseCodeInTextFile(t *testing.T) {
	body := `var base='http://svc.example.com';
function load() { return fetch('/api/users?page=1'); }
fetch('/api/items');
`
	res := Parse(body, "https://src.example/code.txt", "https://src.example/code.txt", "text/plain", false)
	set := hrefSet(hrefs(res.Links))
	if !set["/api/users?page=1"] {
		t.Errorf("JS analyzer should find /api/users, got %v", hrefs(res.Links))
	}
	if !set["/api/items"] {
		t.Errorf("JS analyzer should find /api/items, got %v", hrefs(res.Links))
	}
	if !set["http://svc.example.com"] {
		t.Errorf("text scan should find the URL literal, got %v", hrefs(res.Links))
	}
}

func TestParsePDFWithCompressedStream(t *testing.T) {
	var comp bytes.Buffer
	zw := zlib.NewWriter(&comp)
	zw.Write([]byte("BT /F1 12 Tf (http://pdf.example.com/page) Tj ET"))
	zw.Close()

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n1 0 obj\n<< /Filter /FlateDecode >>\nstream\n")
	pdf.Write(comp.Bytes())
	pdf.WriteString("\nendstream\nendobj\n%%EOF\n")

	res := Parse(pdf.String(), "https://src.example/f.pdf", "https://src.example/f.pdf", "application/pdf", false)
	if !hasKind(res.Kinds, KindPDF) {
		t.Fatalf("expected PDF kind, got %v", res.Kinds)
	}
	set := hrefSet(hrefs(res.Links))
	if !set["http://pdf.example.com/page"] {
		t.Errorf("expected URL from PDF stream, got %v", hrefs(res.Links))
	}
}

func TestParseDocxZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	add("word/document.xml", `<w:document><w:t>Visit https://docx.example.com/hello</w:t></w:document>`)
	add("word/_rels/document.xml.rels",
		`<Relationships><Relationship Target="https://rel.example.com/target" TargetMode="External"/></Relationships>`)
	add("word/media/image1.png", "binary-not-scanned")
	zw.Close()

	res := Parse(buf.String(), "https://src.example/f.docx",
		"https://src.example/f.docx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", false)
	if !hasKind(res.Kinds, KindZip) {
		t.Fatalf("expected zip kind, got %v", res.Kinds)
	}
	set := hrefSet(hrefs(res.Links))
	if !set["https://docx.example.com/hello"] {
		t.Errorf("expected document URL, got %v", hrefs(res.Links))
	}
	if !set["https://rel.example.com/target"] {
		t.Errorf("expected relationship URL, got %v", hrefs(res.Links))
	}
}

func TestParseLegacyUTF16(t *testing.T) {
	url := "https://legacy.example.com/doc"
	body := encodeUTF16LE("some text " + url + " more text")
	res := Parse(body, "https://src.example/f.doc", "https://src.example/f.doc", "application/msword", false)
	if !hasKind(res.Kinds, KindLegacy) {
		t.Fatalf("expected legacy kind, got %v", res.Kinds)
	}
	set := hrefSet(hrefs(res.Links))
	if !set[url] {
		t.Errorf("expected UTF-16 URL, got %v", hrefs(res.Links))
	}
}

func TestIsTextURL(t *testing.T) {
	if !IsTextURL("https://x/a.txt") {
		t.Error("txt should be a text document")
	}
	if !IsTextURL("https://x/a.pdf?download=1") {
		t.Error("pdf should be a text document")
	}
	if IsTextURL("https://x/a.js") {
		t.Error("js has its own analyzer and is not a text document")
	}
	if IsTextURL("https://x/a.png") {
		t.Error("png is not a text document")
	}
}

func TestParseMarkupWithInlineScriptInTextFile(t *testing.T) {
	// A .txt that is really a page: its href was read by the scan's markup
	// pass, but the endpoint written inline in the script was not, so the JS
	// analyzer must run on the inline body.
	body := `<!doctype html><html><body>` +
		`<a href="/from-markup">x</a>` +
		`<script>function go(){ return fetch('/api/from-inline-in-text'); }</script>` +
		`</body></html>`
	res := Parse(body, "https://src.example/page.txt", "https://src.example/page.txt", "text/plain", false)
	set := hrefSet(hrefs(res.Links))
	if !set["/api/from-inline-in-text"] {
		t.Errorf("inline script endpoint not found; got %v", hrefs(res.Links))
	}
}

func hrefs(links []linker.Link) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.HREF
	}
	return out
}

func encodeUTF16LE(s string) string {
	runes := []rune(s)
	buf := make([]byte, 0, len(runes)*2)
	for _, r := range runes {
		buf = append(buf, byte(r), byte(r>>8))
	}
	return string(buf)
}
