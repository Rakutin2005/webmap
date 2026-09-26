package wmse

import (
	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/urlgroup"
)

// Meta is one scan setting, stored as a name/value pair. The set is kept
// deliberately flat and textual: a file has to explain how it was produced even
// if nothing in this version of the reader understands a given key, and a flat
// map is what keeps it forward compatible.
type Meta map[string]string

// MetaKeys used by the writer. They are part of the file's contract: a reader
// that does not know a key still shows it, and a future writer can add keys
// without touching this version's layout.
const (
	MetaTool        = "tool"
	MetaVersion     = "tool_version"
	MetaFormat      = "format"
	MetaCreated     = "created"
	MetaTarget      = "target"
	MetaScope       = "scope"
	MetaElapsed     = "elapsed"
	MetaRequests    = "requests"
	MetaPages       = "pages_crawled"
	MetaLinks       = "links"
	MetaEndpoints   = "endpoints"
	MetaPatterns    = "patterns"
	MetaParams      = "params"
	MetaCookies     = "cookies"
	MetaHeadersKept = "header_hosts"
	// MetaCommand is the invocation that produced the file, and MetaOutput the
	// path it was given. A saved file is often the only artefact left of a
	// scan, so the reader can be told how to reproduce it.
	MetaCommand = "command"
	MetaOutput  = "output"
	// MetaSessionData marks a file that holds requests exactly as they were
	// sent, and may therefore hold session material. It is a warning the
	// writer sets and the reader shows; the file itself is unchanged either
	// way, because the evidence is the reason to keep the file.
	MetaSessionData = "session_data"
	// MetaMaxDepth is the depth the saved crawl was allowed to reach, which is
	// not the same as how deep it got: a crawl that stopped at its limit has
	// pages it never fetched, and a reader asked for more than this is asking
	// for data that was never collected. Zero is the honest value for a scan
	// that did not crawl, since only the entry point was fetched.
	MetaMaxDepth = "max_depth"
	// MetaArchiveFiles is the number of files in the sidecar archive, so a
	// reader can say "this file kept 3 reference files" without opening it.
	MetaArchiveFiles = "archive_files"
	// MetaArchiveName is the source URL a .ref file was written for, kept as a
	// per-file key prefix (archive_ref:<url>) so the reader can map a link back
	// to its reference file by source.
	MetaArchiveName = "archive_ref:"
)

// Stats mirrors categorizer.Stats. It is stored rather than recomputed so a
// reader can trust the numbers the scan reported even if it applies a
// different counting rule to the links it loaded.
type Stats struct {
	Total      int
	Resolved   int
	Unresolved int
	WithParams int
	ByCategory map[linker.Category]int
	ByLinkType map[linker.LinkType]int
	ByClass    map[linker.URLClass]int
	ByTag      map[string]int
}

// Group is one URL pattern with the variable slots it folded.
type Group struct {
	Domain  string
	Pattern string
	Count   int
	Vars    []urlgroup.Var
	// Members are the concrete URLs the pattern absorbed. On disk membership is
	// an edge (EdgePattern), because it is a relation, not a field.
	Members []string
}

// Param is a parameter name recovered from a request builder, with the pages
// whose code it came from kept so the count means "documents", not "hits".
type Param struct {
	linker.ParamRef
	// Docs are the documents (script or page URLs) a name was seen in.
	Docs []string
}

// Call is one network call the sandbox intercepted.
type Call struct {
	URL       string
	RawURL    string
	Method    string
	Initiator string
	Type      string
	Body      string
	Headers   [][2]string
}

// Emulation is the semantic engine's aggregate over a whole scan.
type Emulation struct {
	Scripts   int
	Calls     int
	Abandoned int64
	Disabled  bool
	ByType    map[string]int
	Errors    []string
	List      []Call
}

// Edge is one relation between two nodes of the graph.
type Edge struct {
	Kind uint8
	From uint32
	To   uint32
	// Weight is how many times the relation was observed; zero reads as one.
	Weight uint32
}

// Page is a URL the crawler actually fetched, with the depth it was reached at
// and what the server said it was. Knowing this is what separates "linked to"
// from "walked", which no amount of link data can tell you afterwards.
type Page struct {
	URL         string
	Depth       int
	ContentType string
	// Links is how many distinct links this page contributed.
	Links int
	// Status is the HTTP status code, or zero when the fetch was cut short.
	Status int
}

// Snapshot is the complete state of a scan. It is the in-memory model both
// sides of the format speak: the writer takes one, the reader returns one, and
// nothing about the file layout leaks into it.
type Snapshot struct {
	// Meta describes the scan: tool, target, timestamp, elapsed time and every
	// flag that shaped the result.
	Meta Meta

	Stats  Stats
	Links  []linker.Link
	Pages  []Page
	Edges  []Edge
	Groups []Group
	Params []Param

	Endpoints    []contract.Endpoint
	Observations []contract.Observation

	Emulation *Emulation

	// Archive holds the files the scan kept beside its graph, as an
	// uncompressed tar: today the per-source .ref files, one per source
	// document, each saying where in that document every link was found. It is
	// opaque here - the format does not parse it - so that the file can carry
	// file kinds the graph has no concept of, and so a reader that wants one
	// file does not have to understand the rest.
	Archive []byte
	// ArchiveNames lists the entries in Archive, in the order they were
	// written, so a reader can show what is inside without walking the tar.
	ArchiveNames []string

	// Signature is what the file says about itself: who signed it, with what, and
	// the signature. It is nil for a file that was not signed, and that is not the
	// same thing as a file whose signature does not verify - a reader has to be able
	// to tell "this claims nothing" from "this is broken", because the second is an
	// accusation and the first is not.
	Signature *Signature
}

// LinkKey is the identity of a link node: the absolute URL when it is known,
// the raw reference otherwise. Two records for the same URL are the same node.
func LinkKey(l *linker.Link) string {
	if l.Resolved != "" {
		return l.Resolved
	}
	return l.HREF
}
