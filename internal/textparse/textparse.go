// Package textparse recovers the links that documents carry as text rather than
// as markup.
//
// WebMap's ordinary parser reads markup: an <a href>, an import statement, a
// CSS @import. A .txt file, a PDF or a Word document has none of those. Its
// links are the http(s), //host and www. strings in its content, hidden behind
// a container format. -parse-text decodes the container first and then reads
// the URLs out of the text.
//
// An extension is a hint, not a fact: a .txt routinely holds a PDF, a zipped
// Office file or a block of JavaScript. Detection therefore combines the URL
// extension, the content type and the bytes themselves (magic numbers), and
// every decoder the signals point at runs, with their links merged.
package textparse

import (
	"regexp"
	"strings"

	"apimap/internal/contract"
	"apimap/internal/jsanalyzer"
	"apimap/internal/linker"
)

// Kind is the container format a document was recognised as.
type Kind int

const (
	// KindNone means no container was recognised.
	KindNone Kind = iota
	// KindText is content that is already text (or becomes text by decoding a
	// simple escape scheme): .txt, .csv, .json, .xml, .md, .rtf, ...
	KindText
	// KindPDF is a PDF document.
	KindPDF
	// KindZip is a zipped document container: OOXML and ODF (.docx, .odt,
	// .xlsx, .pptx, ...).
	KindZip
	// KindLegacy is a binary OLE2 office file (.doc, .xls, .ppt) or RTF.
	KindLegacy
)

// Result is everything -parse-text recovered from one resource.
type Result struct {
	// Kinds lists the containers that were detected, in processing order; it is
	// non-empty exactly when at least one parser ran.
	Kinds []Kind
	// Links are the links found in the document text, plus the endpoints a
	// code-in-text parser found when the text looked like JavaScript.
	Links []linker.Link
	// Observations and Params carry the API evidence a JS analyzer produced for
	// text that turned out to be code, so the report can show contracts and
	// parameter names for it just as it does for a real .js file.
	Observations []contract.Observation
	Params       []linker.ParamRef
}

// textExtensions are the plain-text and document extensions that -parse-text
// treats as link-bearing documents. Markup and code assets (html, css, js) keep
// their own, better analyzers and are not listed here.
var textExtensions = map[string]bool{
	".txt": true, ".text": true, ".log": true, ".csv": true, ".tsv": true,
	".md": true, ".markdown": true, ".rst": true, ".adoc": true, ".asciidoc": true,
	".json": true, ".jsonl": true, ".ndjson": true, ".xml": true, ".rss": true,
	".atom": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".cfg": true, ".conf": true, ".config": true, ".env": true, ".properties": true,
	".sql": true, ".srt": true, ".vtt": true, ".sub": true, ".tex": true, ".bib": true,
	".pdf": true, ".eml": true, ".vcf": true, ".ics": true,
}

// legacyExtensions are the binary OLE2 office files and RTF, whose text is
// stored as byte strings and is scanned rather than parsed.
var legacyExtensions = map[string]bool{
	".doc": true, ".dot": true, ".xls": true, ".ppt": true, ".rtf": true,
}

// zipExtensions are the zipped document containers.
var zipExtensions = map[string]bool{
	".docx": true, ".docm": true, ".dotx": true, ".dotm": true,
	".odt": true, ".ott": true, ".ods": true, ".ots": true, ".odp": true,
	".otp": true, ".odg": true, ".otg": true,
	".xlsx": true, ".xlsm": true, ".pptx": true, ".pptm": true,
}

// Parse decodes a resource with every parser its signals point at and returns
// the union of what they found. The result's Kinds is empty when nothing ran.
//
// fullInfo selects the JS analyzer's full API detail, the same switch -apif
// turns on for scripts fetched as scripts.
func Parse(body, sourceURL, rawURL, contentType string, fullInfo bool) (res Result) {
	kinds := DetectKinds(body, rawURL, contentType)
	if len(kinds) == 0 {
		return Result{}
	}
	res.Kinds = kinds

	seen := make(map[string]bool)
	var code string
	for _, kind := range kinds {
		text := decode(body, kind)
		if text == "" {
			continue
		}
		if kind == KindText {
			code = text
		}
		for _, l := range linker.ParseTextURLs(text, sourceURL, tagFor(kind)) {
			if seen[l.HREF] {
				continue
			}
			seen[l.HREF] = true
			res.Links = append(res.Links, l)
		}
	}

	// A .txt can hold code, and a file named as a document can hold markup with
	// a script inside it. When the text hints at either, the JS analyzer - the
	// independent parser for it - runs as well.
	if code != "" {
		var sources []string
		switch {
		case looksLikeMarkup(code):
			// The scan's markup pass already read the <a href> and <script src>
			// of any fetched body, but a script written inline in a plain-text
			// file was never looked at, so only the inline bodies run here.
			sources = inlineScripts(code)
		case looksLikeCode(code):
			sources = []string{code}
		}
		if len(sources) > 0 {
			js := strings.Join(sources, "\n")
			jsLinks, obs, params := jsanalyzer.ParseWithParams(js, sourceURL, fullInfo)
			templates := linker.AnalyzeJS(js, sourceURL)
			for _, l := range append(jsLinks, templates...) {
				if l.HREF == "" || seen[l.HREF] {
					continue
				}
				seen[l.HREF] = true
				if l.SourceURL == "" {
					l.SourceURL = sourceURL
				}
				res.Links = append(res.Links, l)
			}
			res.Observations = obs
			res.Params = params
		}
	}
	return res
}

// IsTextURL reports whether a URL names a document -parse-text should fetch.
// It is extension-only because no content type is known before the fetch.
func IsTextURL(rawURL string) bool {
	return extKind(rawURL) != KindNone
}

// DetectKinds returns the containers a resource's signals point at. The URL
// extension and content type are read first, then the bytes: a magic number
// overrides a misleading extension, and content that simply looks textual is
// treated as text even when it was named as something else.
func DetectKinds(body, rawURL, contentType string) []Kind {
	ct := normalizeContentType(contentType)
	html := isHTMLContentType(ct)

	set := make(map[Kind]bool)
	if !html {
		if k := extKind(rawURL); k != KindNone {
			set[k] = true
		}
		if k := contentTypeKind(ct); k != KindNone {
			set[k] = true
		}
	}
	// The bytes are the strongest signal: a .txt holding "%PDF-" is a PDF, and
	// a .pdf holding no NUL byte is text whatever the server claimed.
	if k := sniffKind(body); k != KindNone {
		set[k] = true
	}
	if !html && len(body) > 0 && looksTextual(body) {
		set[KindText] = true
	}
	if len(set) == 0 {
		return nil
	}
	// A stable order keeps the output reproducible: binary containers first,
	// then text.
	var out []Kind
	for _, k := range []Kind{KindPDF, KindZip, KindLegacy, KindText} {
		if set[k] {
			out = append(out, k)
		}
	}
	return out
}

// sniffKind recognises a container from its magic number.
func sniffKind(body string) Kind {
	switch {
	case strings.HasPrefix(body, "%PDF-"):
		return KindPDF
	case strings.HasPrefix(body, "{\\rtf"):
		return KindLegacy
	case isOLE2(body):
		return KindLegacy
	case isZip(body):
		return KindZip
	}
	return KindNone
}

func isZip(body string) bool {
	if len(body) < 4 || body[0] != 'P' || body[1] != 'K' {
		return false
	}
	// PK\x03\x04 (a member), PK\x05\x06 (the empty archive end) and
	// PK\x07\x08 (a spanned marker).
	return body[2] == 3 || body[2] == 5 || body[2] == 7
}

func isOLE2(body string) bool {
	if len(body) < 8 {
		return false
	}
	magic := []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	for i, b := range magic {
		if body[i] != b {
			return false
		}
	}
	return true
}

// decode turns a document body into text according to its container.
func decode(body string, kind Kind) string {
	switch kind {
	case KindText:
		return body
	case KindPDF:
		return extractPDF(body)
	case KindZip:
		return extractZip(body)
	case KindLegacy:
		return extractLegacy(body)
	}
	return ""
}

// tagFor names the document kind in the report. The tag is what tells a reader
// that a link came from a PDF's text and not from an <a href>.
func tagFor(kind Kind) string {
	switch kind {
	case KindPDF:
		return "pdf-url"
	case KindZip, KindLegacy:
		return "doc-url"
	default:
		return "text-url"
	}
}

// normalizeContentType lowercases a media type and drops its parameters.
func normalizeContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct
}

func isHTMLContentType(ct string) bool {
	return ct == "text/html" || ct == "application/xhtml+xml"
}

// extKind classifies a URL by its file extension, ignoring query and fragment.
func extKind(rawURL string) Kind {
	u := rawURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	dot := strings.LastIndexByte(u, '.')
	if dot < 0 || dot == len(u)-1 {
		return KindNone
	}
	ext := strings.ToLower(u[dot:])
	switch {
	case ext == ".pdf":
		return KindPDF
	case legacyExtensions[ext]:
		return KindLegacy
	case textExtensions[ext]:
		return KindText
	case zipExtensions[ext]:
		return KindZip
	}
	return KindNone
}

// contentTypeKind classifies a media type when the extension gave no answer.
func contentTypeKind(ct string) Kind {
	switch ct {
	case "application/pdf", "application/x-pdf":
		return KindPDF
	case "application/msword", "application/vnd.ms-word",
		"application/rtf", "text/rtf",
		"application/vnd.ms-excel", "application/vnd.ms-powerpoint":
		return KindLegacy
	case "application/zip", "application/x-zip-compressed",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.template",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.oasis.opendocument.text",
		"application/vnd.oasis.opendocument.spreadsheet",
		"application/vnd.oasis.opendocument.presentation",
		"application/vnd.oasis.opendocument.graphics":
		return KindZip
	case "application/json", "application/xml", "application/javascript",
		"application/x-javascript", "application/x-httpd-php",
		"application/x-sh", "application/x-yaml", "application/x-ndjson",
		"application/ld+json":
		return KindText
	}
	if strings.HasPrefix(ct, "text/") {
		return KindText
	}
	if strings.HasSuffix(ct, "+xml") || strings.HasSuffix(ct, "+json") {
		return KindText
	}
	return KindNone
}

// looksTextual reports whether a body reads as text rather than as a binary
// container. Bytes at or above 0x80 are accepted so UTF-8 and single-byte
// encodings both qualify; a NUL is the one byte no text format uses, and a run
// dense with other control bytes is binary.
func looksTextual(body string) bool {
	if body == "" {
		return false
	}
	sample := body
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	printable := 0
	for i := 0; i < len(sample); i++ {
		c := sample[i]
		if c == 0 {
			return false
		}
		if c == '\n' || c == '\r' || c == '\t' || (c >= 0x20 && c < 0x7f) || c >= 0x80 {
			printable++
		}
	}
	return printable*100/len(sample) >= 90
}

// jsHints are the tokens that distinguish source code from prose, JSON or
// configuration. Two hits are required so that a document that merely mentions
// one of the words is not run through the JS analyzer.
var jsHints = []string{
	"function", "=>", "const ", "let ", "var ", "fetch(",
	"XMLHttpRequest", "require(", "import ", "export ",
	"document.", "window.", ".then(", "async ", "await ",
}

// looksLikeCode reports whether text is plausibly JavaScript source. Markup is
// excluded because a page's own pass reads it and running the JS analyzer over
// tags would only add noise; a markup body is handled by inlineScripts instead.
func looksLikeCode(text string) bool {
	if looksLikeMarkup(text) {
		return false
	}
	sample := text
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	hits := 0
	for _, h := range jsHints {
		if strings.Contains(sample, h) {
			hits++
		}
	}
	return hits >= 2
}

// looksLikeMarkup reports whether text is markup rather than source, judging by
// what it opens with. A leading comment is skipped so that an HTML file whose
// first line is one is still recognised, and the tag names checked are the ones
// a fetched body actually starts with. A script that merely mentions a tag in a
// string does not begin with one, so it is not mistaken for a page.
func looksLikeMarkup(text string) bool {
	t := strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	for strings.HasPrefix(t, "<!--") {
		end := strings.Index(t, "-->")
		if end < 0 {
			return true
		}
		t = strings.TrimSpace(t[end+len("-->"):])
	}
	if t == "" || t[0] != '<' {
		return false
	}
	low := strings.ToLower(t)
	for _, open := range []string{"<!doctype", "<html", "<head", "<body", "<div", "<script", "<a ", "<meta", "<?xml", "<svg", "<table", "<form"} {
		if strings.HasPrefix(low, open) {
			return true
		}
	}
	return false
}

// inlineScriptRegex matches a script element's body, case-insensitively and
// across newlines.
var inlineScriptRegex = regexp.MustCompile(`(?is)<script[^>]*>(.*?)</script>`)

// inlineScripts returns the non-empty bodies of the <script> elements in a
// markup document. A script that only points at an external file has no body
// and contributes nothing here; its src was already read as a link.
func inlineScripts(markup string) []string {
	var out []string
	for _, m := range inlineScriptRegex.FindAllStringSubmatch(markup, -1) {
		if body := strings.TrimSpace(m[1]); body != "" {
			out = append(out, body)
		}
	}
	return out
}
