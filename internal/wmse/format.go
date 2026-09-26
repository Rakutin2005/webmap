// Package wmse implements WebMap's Static Explorer format: a compact binary
// container for a finished scan, plus the offline explorer that reads it.
//
// # What the file holds
//
// A scan produces the same information in five different shapes, and each of
// them repeats the last: every link carries its own resolved URL, the page it
// came from, its domain and its tag; every endpoint contract restates the
// paths of the requests behind it. The format is built around that
// redundancy instead of copying it:
//
//   - One string graph. Every literal in the file - URL, domain, tag, method,
//     query, body, error text, pattern variable kind - is interned exactly
//     once and referenced everywhere else by its id (see secStrings). Two
//     links that share a domain, a query, or a verb therefore pay for it
//     once. Ids are positions in a lexicographically sorted table, which is
//     what makes front coding possible.
//   - Location instead of repetition. Records do not embed literals; they
//     embed the table coordinates of the things they refer to, and repeated
//     sub-structures (method sets, value lists, response schemas, API detail
//     sets, parameter-variant sets) live in shared dictionaries keyed by
//     content (see secDicts), so a thousand endpoints that only differ in path
//     cost a thousand paths and nothing else.
//   - A relation graph. "Found on", "is a member of pattern", "has contract",
//     "parameter feeds" and "lives on host" are not stored as nested data but
//     as edges in one sorted, run-length and delta encoded section (secEdges),
//     so traversal order on disk matches traversal order in memory.
//
// Everything else follows from those three: the string table is sorted and
// front coded, every number is a varint (deltas where a sorted run allows it),
// every closed value set is a one byte code, and each section is compressed
// independently so a reader can decompress only the parts it is asked for.
//
// # Layout
//
//	magic      8 bytes  "WMAPSBX\x1a"
//	version    varint  format version
//	headerLen  varint  bytes from here to the first section payload
//	flags      varint  bit 0: sections use the flate codec
//	dirCount   varint  number of directory entries
//	directory  dirCount x { id, codec, off, storedLen, rawLen }
//
//	off is relative to the first payload byte, so the directory costs a few
//	bytes per section regardless of file size.
package wmse

import (
	"errors"
	"fmt"
)

// Magic identifies a WebMap Static Explorer file.
const Magic = "WMAPSBX\x1a"

// FormatVersion is the on-disk layout version. It is bumped whenever the
// meaning of a byte changes, so an old reader fails loudly instead of silently
// misreading a newer file.
const FormatVersion uint64 = 1

// Sections. The identifiers are also the physical order of the payloads, which
// lets a streaming reader walk the file without consulting the directory.
const (
	SecMeta uint8 = iota + 1
	SecStats
	SecStrings
	SecLinks
	SecEdges
	SecGroups
	SecEndpoints
	SecObservations
	SecParams
	SecEmulation
	SecDicts
	// SecArchive is a tar of the files the scan kept alongside its graph: today
	// the per-source .ref files that say where each link was found. It is a
	// separate section, and uncompressed within it, so that adding a kind of
	// file later is a change to one section rather than to every link record,
	// and so a reader that does not want the files never inflates them.
	SecArchive
	// SecSignature holds a signature over the rest of the file, and SecSealed marks
	// a file that is encrypted rather than merely signed. Both are separate
	// sections rather than fields in the metadata, so a reader that does not care
	// about either never has to look, and a file that has neither is byte for byte
	// what it was before these existed.
	SecSignature
	SecSealed
	secCount
)

// sectionNames is used by `wmse read --info` and by error messages.
var sectionNames = [secCount]string{
	SecMeta:         "meta",
	SecStats:        "stats",
	SecStrings:      "strings",
	SecLinks:        "links",
	SecEdges:        "edges",
	SecGroups:       "groups",
	SecEndpoints:    "endpoints",
	SecObservations: "observations",
	SecParams:       "params",
	SecEmulation:    "emulation",
	SecDicts:        "dicts",
	SecArchive:      "archive",
	SecSignature:    "signature",
	SecSealed:       "sealed",
}

// SectionName returns the readable name of a section id.
func SectionName(id uint8) string {
	if id < secCount && sectionNames[id] != "" {
		return sectionNames[id]
	}
	return fmt.Sprintf("section%d", id)
}

// Edge kinds. The relation graph is stored per kind, each kind sorted by
// source, so every kind compresses as one delta run. From and To are indices
// into the node space the kind names:
//
//	EdgeSource   link   -> link    the page a link was discovered on
//	EdgeCrawled  link   -> link    the crawl edge: page -> page it queued
//	EdgePattern  group  -> link    pattern membership
//	EdgeContract link   -> endpoint the contract inferred for it
//	EdgeParam    param  -> endpoint a recovered name feeds
const (
	EdgeSource uint8 = iota + 1
	EdgeCrawled
	EdgePattern
	EdgeContract
	EdgeParam
	edgeCount
)

var edgeNames = [edgeCount]string{
	EdgeSource:   "source",
	EdgeCrawled:  "crawled",
	EdgePattern:  "pattern",
	EdgeContract: "contract",
	EdgeParam:    "param",
}

// EdgeName returns the readable name of an edge kind.
func EdgeName(kind uint8) string {
	if kind < edgeCount && edgeNames[kind] != "" {
		return edgeNames[kind]
	}
	return fmt.Sprintf("edge%d", kind)
}

// EdgeCount is the number of edge kinds the format defines.
const EdgeCount = int(edgeCount)

// codec values stored per section in the directory.
const (
	codecRaw   uint8 = 0
	codecFlate uint8 = 1
)

// Flag bits in the file header.
const (
	flagCompressed uint64 = 1 << 0
)

// Errors returned by the format layer.
var (
	ErrBadMagic    = errors.New("not a WebMap Static Explorer file")
	ErrBadVersion  = errors.New("unsupported format version")
	ErrTruncated   = errors.New("truncated file")
	ErrBadSection  = errors.New("corrupt section")
	ErrMissingSec  = errors.New("missing required section")
	ErrUnsupported = errors.New("unsupported feature in file")
)

// zigzag maps a signed delta onto an unsigned varint so small magnitudes stay
// small in either direction. Sequential values are the common case in every
// delta-encoded run in this format.
func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

func unzigzag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }

// byteWriter is a tiny append-only buffer. It exists instead of
// bytes.Buffer because every number written here is a varint and the section
// encoders want the encode side to be a handful of lines each.
type byteWriter struct {
	buf []byte
}

func (w *byteWriter) reset() { w.buf = w.buf[:0] }

func (w *byteWriter) uvarint(v uint64) {
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

func (w *byteWriter) intvarint(v int64) { w.uvarint(zigzag(v)) }

func (w *byteWriter) byte(b byte) { w.buf = append(w.buf, b) }

func (w *byteWriter) bytes(p []byte) { w.buf = append(w.buf, p...) }

// rawString writes a length-prefixed byte run. Only used for the string table,
// where a length is cheaper than escaping a terminator.
func (w *byteWriter) rawString(s string) {
	w.uvarint(uint64(len(s)))
	w.buf = append(w.buf, s...)
}

func (w *byteWriter) len() int { return len(w.buf) }

// byteReader is the mirror image of byteWriter. Every read is bounds checked:
// a truncated or hostile file must produce an error, never a panic.
type byteReader struct {
	buf []byte
	pos int
}

func (r *byteReader) empty() bool { return r.pos >= len(r.buf) }

func (r *byteReader) uvarint() (uint64, error) {
	var v uint64
	var shift uint
	for {
		if r.pos >= len(r.buf) {
			return 0, ErrTruncated
		}
		b := r.buf[r.pos]
		r.pos++
		if shift >= 64 {
			return 0, ErrBadSection
		}
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, nil
		}
		shift += 7
	}
}

func (r *byteReader) intvarint() (int64, error) {
	v, err := r.uvarint()
	if err != nil {
		return 0, err
	}
	return unzigzag(v), nil
}

func (r *byteReader) byteAt() (byte, error) {
	if r.pos >= len(r.buf) {
		return 0, ErrTruncated
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

func (r *byteReader) rawString() (string, error) {
	n, err := r.uvarint()
	if err != nil {
		return "", err
	}
	if n > uint64(len(r.buf)-r.pos) {
		return "", ErrTruncated
	}
	s := string(r.buf[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, nil
}

// count reads a varint length and rejects values the file could not possibly
// hold, so a corrupt length cannot make the reader allocate gigabytes.
func (r *byteReader) count(limit int) (int, error) {
	n, err := r.uvarint()
	if err != nil {
		return 0, err
	}
	if n > uint64(limit) {
		return 0, fmt.Errorf("%w: %d entries declared", ErrBadSection, n)
	}
	return int(n), nil
}
