package wmse

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"apimap/internal/archive"
	"apimap/internal/linker"
)

// BuildRefsArchive turns the references the scan collected into the sidecar
// archive: one "<source>.ref" file per source document, each listing where in
// that document every link was found. It sets Archive and ArchiveNames on the
// snapshot.
//
// The references are stored as files rather than as fields on the link records
// for two reasons. A link found in five places has five references and one node,
// and only a per-document list can hold that without either losing four or
// pretending they are the same fact. And the evidence is worth keeping in a
// form a person can read: a .ref file opens in any editor, and "which page
// mentioned /admin, and what did it look like there" is a question about
// documents, not about a database row.
func BuildRefsArchive(snap *Snapshot, ix *linker.RefIndex) error {
	if ix == nil || ix.Empty() {
		snap.Archive = nil
		snap.ArchiveNames = nil
		return nil
	}
	docs := ix.Documents()
	entries := make([]archive.Entry, 0, len(docs))
	for _, doc := range docs {
		entries = append(entries, archive.Entry{
			Name: refName(doc.URL),
			Data: renderRefFile(doc),
		})
	}
	// The archive is built uncompressed: the section it goes into is deflated
	// by the container, and compressing here would pay twice.
	raw, names, err := archive.Build(entries)
	if err != nil {
		return err
	}
	snap.Archive = raw
	snap.ArchiveNames = names
	return nil
}

// renderRefFile writes the human-readable index for one source document: a
// header naming the document, then one line per reference with what was found,
// where, and the text around it. The target is in the line because a .ref file
// is meant to be read on its own: "something was found at offset 40" is half a
// record, and "at 40, /complex/9223, here it is" is the whole of it.
func renderRefFile(doc *linker.RefSource) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# source: %s\n", doc.URL)
	if doc.ContentType != "" {
		fmt.Fprintf(&b, "# type: %s\n", doc.ContentType)
	}
	fmt.Fprintf(&b, "# references: %d\n", len(doc.Refs))
	for _, r := range doc.Refs {
		fmt.Fprintf(&b, "%d\t%d:%d\t%s\t%s\n", r.Offset, r.Line, r.Column, r.Target, r.Snippet)
	}
	return []byte(b.String())
}

// refName turns a source URL into the .ref file's name: the archive name with
// ".ref" on the end, so a reference and the document it describes sit next to
// each other in a listing and cannot be mistaken for one another.
func refName(url string) string { return ArchiveName(url) + ".ref" }

// ArchiveName is the name a URL is stored under in the archive: the host, the
// path under it, and the query folded in so two URLs that differ only by query
// stay distinct.
//
// One scheme serves everything the archive holds, because the archive is one flat
// namespace and two schemes would have to be kept from colliding. The host keeps
// its port, with the colon written as an underscore because a colon is not a legal
// character in a path on the platforms a file is opened on, and the query follows
// a "~" so it cannot be read as part of the file's extension.
func ArchiveName(rawURL string) string {
	name := rawURL
	if i := strings.Index(name, "://"); i >= 0 {
		name = name[i+3:]
	}
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		q := name[i+1:]
		if j := strings.IndexByte(q, '#'); j >= 0 {
			q = q[:j]
		}
		q = strings.NewReplacer("&", "-", "=", "-", "/", "-", " ", "-").Replace(q)
		name = name[:i] + "~" + q
	}
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.ReplaceAll(name, ":", "_")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		// A URL that is nothing but a host still needs a name, and "/" would name
		// a directory rather than a file.
		name = "root"
	}
	return name
}

// ArchiveFile returns one named file from the snapshot's sidecar archive. It is
// what a reader uses to show a single .ref, and what `source` and `save` will
// use to hand a file back to the caller.
func (s *Snapshot) ArchiveFile(name string) ([]byte, error) {
	if len(s.Archive) == 0 {
		return nil, fmt.Errorf("this file has no archive")
	}
	tr := tar.NewReader(bytes.NewReader(s.Archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no %s in this file's archive", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == name {
			return io.ReadAll(tr)
		}
	}
}

// ArchiveFileBySource returns the .ref file for a source URL, deriving the name
// the writer would have used. It is the lookup a reader does for "show me where
// this was found" without the caller having to know the naming scheme.
func (s *Snapshot) ArchiveFileBySource(url string) ([]byte, error) {
	return s.ArchiveFile(refName(url))
}

// A RefEntry is one parsed line of a .ref file: a URL that was found at a
// position in a source document, with the text around it.
type RefEntry struct {
	Target  string
	Offset  int
	Line    int
	Column  int
	Snippet string
}

// RefFile is a parsed .ref file. Parsing lives next to the format that writes it
// so the two cannot disagree about what a line means.
type RefFile struct {
	Source      string
	ContentType string
	Entries     []RefEntry
}

// ParseRef reads a .ref file. The format is line-oriented and tab-separated
// (offset, line:col, target, snippet) with '#' header lines, so that it stays
// readable in an editor; a line that does not parse is skipped rather than
// failing the whole file, because one corrupt line should not hide the rest of a
// document's evidence.
func ParseRef(data []byte) *RefFile {
	rf := &RefFile{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			key, val, ok := strings.Cut(strings.TrimSpace(line[1:]), ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(key) {
			case "source":
				rf.Source = strings.TrimSpace(val)
			case "type":
				rf.ContentType = strings.TrimSpace(val)
			}
			continue
		}
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) < 3 {
			continue
		}
		offset, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		lineNo, colNo := 0, 0
		if l, c, ok := strings.Cut(fields[1], ":"); ok {
			lineNo, _ = strconv.Atoi(l)
			colNo, _ = strconv.Atoi(c)
		}
		e := RefEntry{Target: fields[2], Offset: offset, Line: lineNo, Column: colNo}
		if len(fields) == 4 {
			e.Snippet = fields[3]
		}
		rf.Entries = append(rf.Entries, e)
	}
	return rf
}
