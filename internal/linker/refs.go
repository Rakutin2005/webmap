package linker

import (
	"sort"
	"strings"
	"sync"
)

// A Reference records where in a source document a link was discovered: the
// offset of the match, its line and column, and a snippet of the text around
// it. It is the difference between "this page links to /admin" and "this page
// links to /admin at line 42, here is the tag that does it" - which is the
// difference between a finding and something you can act on.
//
// References are only captured when the scan was asked for them, because they
// are only useful once the file is read by someone who wants to look at the
// evidence rather than the summary.
type Reference struct {
	// Target is the URL that was found here, exactly as the source spells it.
	// Without it a .ref file is an index of positions that says something was
	// found without saying what, which is only half the record; with it, the
	// file answers "where was this endpoint mentioned" on its own.
	Target string
	// Offset is the byte offset of the matched URL within the source
	// document. It is the precise, unambiguous position; Line and Column are
	// the same place counted in the units a person reads.
	Offset int
	Line   int
	Column int
	// Snippet is a short window of the source around the match, so a reader
	// can see the context without re-fetching the page.
	Snippet string
}

// RefSource pairs a source document with everything found in it. A scan walks
// many documents and each one is a separate subject: this is the unit a .ref
// file is written from, and the unit the report groups by.
type RefSource struct {
	// URL is the document the references are offsets into. For a link found in
	// an external script or stylesheet, this is that file, not the page that
	// referenced it.
	URL string
	// ContentType is what the server said, when the scan knew. It decides
	// whether the offsets are into HTML, a script, or something else.
	ContentType string
	// Refs are the references, ordered by offset.
	Refs []Reference
}

// add appends a reference, keeping the list ordered by offset.
func (rs *RefSource) add(ref Reference) {
	rs.Refs = append(rs.Refs, ref)
}

// RefIndex collects the references of a whole scan, keyed by source document,
// and is safe to update from the crawl's workers. It is built only when the
// scan asked for references, so a scan that did not pays nothing for it.
type RefIndex struct {
	mu      sync.Mutex
	Sources map[string]*RefSource
}

// NewRefIndex returns an empty index.
func NewRefIndex() *RefIndex {
	return &RefIndex{Sources: map[string]*RefSource{}}
}

// Add records a reference found in the document at url. The same document may be
// visited more than once (a redirect, a retried fetch), so this appends rather
// than replacing; a reader that wants one reference per link takes the first at
// the same offset.
func (ix *RefIndex) Add(url, contentType string, ref Reference) {
	if url == "" {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.Sources == nil {
		ix.Sources = map[string]*RefSource{}
	}
	rs, ok := ix.Sources[url]
	if !ok {
		rs = &RefSource{URL: url, ContentType: contentType}
		ix.Sources[url] = rs
	}
	if rs.ContentType == "" {
		rs.ContentType = contentType
	}
	rs.add(ref)
}

// SetContentType records what the server said a source document was, for the
// .ref file's header. The parser does not know the content type - it only sees
// bytes - so the crawl fills it in once the response headers are known. An
// existing value is kept, because a document fetched twice may be described
// twice and the first answer is the one the parse was done against.
func (ix *RefIndex) SetContentType(url, contentType string) {
	if url == "" || contentType == "" {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if rs, ok := ix.Sources[url]; ok && rs.ContentType == "" {
		rs.ContentType = contentType
	}
}

// Len is the total number of references across every document. The crawl prints
// it so a scan that asked for references says how many it kept.
func (ix *RefIndex) Len() int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	n := 0
	for _, rs := range ix.Sources {
		n += len(rs.Refs)
	}
	return n
}

// Documents returns every source document that contributed a reference, ordered
// by URL, which is the order the .ref files are written in so that two scans of
// the same site produce the same archive.
func (ix *RefIndex) Documents() []*RefSource {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]*RefSource, 0, len(ix.Sources))
	for _, rs := range ix.Sources {
		sort.SliceStable(rs.Refs, func(i, j int) bool { return rs.Refs[i].Offset < rs.Refs[j].Offset })
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

// Empty reports whether the index holds nothing, which is what a scan that was
// not asked for references always has.
func (ix *RefIndex) Empty() bool {
	return ix.Len() == 0
}

// snippetLen is how much text is kept on each side of a match. Enough to show
// the element or statement that contains it without turning a reference into a
// copy of the document; the file stores the result, so a wide window would be
// paid for on every link of a large site.
const snippetLen = 48

// makeSnippet returns the text around body[off:off+length], trimmed to
// snippetLen on each side and with newlines flattened so one finding stays on
// one line of a .ref file.
func makeSnippet(body string, off, length int) string {
	start := off - snippetLen
	if start < 0 {
		start = 0
	}
	end := off + length + snippetLen
	if end > len(body) {
		end = len(body)
	}
	s := body[start:end]
	if start > 0 {
		s = "..." + s
	}
	if end < len(body) {
		s += "..."
	}
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// lineColumn maps a byte offset in body to a 1-based line and column. It counts
// newlines up to the offset, which is O(offset); the crawl calls it once per
// link, so a document with many links pays a few passes over a document already
// held in memory. When the offset lands just after a newline the column is the
// position on the next line, which is what an editor shows.
func lineColumn(body string, offset int) (line, column int) {
	if offset > len(body) {
		offset = len(body)
	}
	line, column = 1, 1
	for i := 0; i < offset; i++ {
		if body[i] == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	return line, column
}
