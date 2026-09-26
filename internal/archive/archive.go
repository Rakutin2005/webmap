// Package archive writes a set of named files as a tar, tar.gz or zip
// container.
//
// It exists because several commands need to hand a bundle of files to a person
// or to another tool, and because the .wmse container embeds a tar of reference
// files. One writer means one set of rules about what a safe entry name is, so
// an archive this package produced can always be extracted without a path
// escaping the destination.
//
// The embedded form is an uncompressed tar: the .wmse container compresses its
// sections, so compressing again inside the section would pay twice for the same
// bytes. The compressed forms are for writing a standalone file.
package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Entry is one file to put into the archive.
type Entry struct {
	// Name is the path inside the archive. It is sanitised on the way in, so a
	// caller may pass a URL or a file name without pre-cleaning it.
	Name string
	Data []byte
}

// Format is a container to write.
type Format int

const (
	// Tar is an uncompressed tar, the form embedded in a .wmse section.
	Tar Format = iota
	// TarGz is a gzip-compressed tar.
	TarGz
	// Zip is a zip archive, for Windows consumers and double-click extraction.
	Zip
)

// Format names a container. "tar", "tar.gz", "tgz" and "zip" are recognised; the
// empty string is Tar.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "tar":
		return Tar, nil
	case "tar.gz", "tgz", "targz":
		return TarGz, nil
	case "zip":
		return Zip, nil
	default:
		return Tar, fmt.Errorf("unknown archive format %q: want tar, tar.gz or zip", s)
	}
}

// String returns the format's usual name.
func (f Format) String() string {
	switch f {
	case TarGz:
		return "tar.gz"
	case Zip:
		return "zip"
	default:
		return "tar"
	}
}

// Ext returns the file extension that usually goes with the format, including
// the leading dot.
func (f Format) Ext() string {
	switch f {
	case TarGz:
		return ".tar.gz"
	case Zip:
		return ".zip"
	default:
		return ".tar"
	}
}

// epoch is the modification time stamped on every entry. A fixed time keeps the
// output byte-for-byte reproducible, which is what lets two scans of the same
// site produce the same archive and lets a test compare bytes instead of walking
// a structure.
var epoch = time.Unix(0, 0).UTC()

// Build lays the entries out as an uncompressed tar and returns both the bytes
// and the name each entry ended up under. The names come back separately
// because a caller that stores the tar in a container also has to record what
// is inside it, and deriving the names twice risks them disagreeing.
//
// This is the form embedded in a .wmse section, where the container compresses
// the section; a caller that wants a standalone file should use Write.
func Build(entries []Entry) (raw []byte, names []string, err error) {
	var buf bytes.Buffer
	names, err = plan(entries)
	if err != nil {
		return nil, nil, err
	}
	if err := writeTar(&buf, entries, names); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), names, nil
}

// TarBytes builds an uncompressed tar from the entries and returns its bytes.
func TarBytes(entries []Entry) ([]byte, error) {
	raw, _, err := Build(entries)
	return raw, err
}

// Entries unpacks a tar built by Build or TarBytes back into its entries.
//
// It is what adding to an archive needs. A tar cannot have a member appended and
// still be a tar that lists itself correctly, so a new member means reading the
// old ones out and building the whole thing again - which means being able to
// read one, and being able to read one is worth having on its own.
//
// The order is the order in the archive, and a name that appears twice comes back
// twice: this reports what is stored, and deciding which of two entries with one
// name wins is the caller's business, not this function's. Only regular files are
// returned; a directory or any other kind of member is skipped rather than
// returned as an entry with no data, because there is nothing to rebuild it from.
func Entries(raw []byte) ([]Entry, error) {
	tr := tar.NewReader(bytes.NewReader(raw))
	var out []Entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("archive: read: %w", err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("archive: read %s: %w", h.Name, err)
		}
		out = append(out, Entry{Name: h.Name, Data: data})
	}
}

// writeTar emits the entries as an uncompressed tar to w, using the already
// planned names.
func writeTar(w io.Writer, entries []Entry, names []string) error {
	tw := tar.NewWriter(w)
	for i, e := range entries {
		h := &tar.Header{
			Name:     names[i],
			Mode:     0o644,
			Size:     int64(len(e.Data)),
			ModTime:  epoch,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("archive: %s: %w", names[i], err)
		}
		if _, err := tw.Write(e.Data); err != nil {
			return fmt.Errorf("archive: %s: %w", names[i], err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("archive: close: %w", err)
	}
	return nil
}

// Write writes the entries to w in the given format.
func Write(w io.Writer, entries []Entry, format Format) error {
	names, err := plan(entries)
	if err != nil {
		return err
	}
	switch format {
	case Zip:
		zw := zip.NewWriter(w)
		for i, e := range entries {
			fh := &zip.FileHeader{Name: names[i], Method: zip.Deflate, Modified: epoch}
			fw, err := zw.CreateHeader(fh)
			if err != nil {
				return fmt.Errorf("archive: %s: %w", names[i], err)
			}
			if _, err := fw.Write(e.Data); err != nil {
				return fmt.Errorf("archive: %s: %w", names[i], err)
			}
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("archive: close: %w", err)
		}
		return nil
	case TarGz:
		gz := gzip.NewWriter(w)
		if err := writeTar(gz, entries, names); err != nil {
			gz.Close()
			return err
		}
		if err := gz.Close(); err != nil {
			return fmt.Errorf("archive: close gzip: %w", err)
		}
		return nil
	default:
		return writeTar(w, entries, names)
	}
}

// plan sanitises entry names and resolves collisions, returning the name to use
// for each entry in the input order.
//
// The rules are the ones that keep an extracted archive inside its destination:
// no absolute paths, no "..", no drive letters, no empty name, no control
// characters, and a name that is a path under the archive root. A name that
// sanitises to nothing, or that collides with an earlier entry, gets a numeric
// suffix, because dropping the file or overwriting an earlier one would both be
// worse than a longer name.
func plan(entries []Entry) ([]string, error) {
	used := make(map[string]int, len(entries))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := sanitize(e.Name)
		if name == "" {
			return nil, fmt.Errorf("archive: entry name %q is empty after sanitising", e.Name)
		}
		if n := used[name]; n > 0 {
			// Keep the earlier entry and disambiguate this one.
			ext := path.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			for {
				n++
				candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
				if used[candidate] == 0 {
					name = candidate
					break
				}
			}
		}
		used[name]++
		names = append(names, name)
	}
	return names, nil
}

// sanitize turns an arbitrary label (often a URL) into a safe archive path.
func sanitize(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	// A URL keeps its scheme as part of the first segment; strip it so the
	// path reads as a host directory rather than "https:".
	if i := strings.Index(name, "://"); i >= 0 {
		name = name[i+3:]
	}
	name = path.Clean("/" + name)
	name = strings.TrimPrefix(name, "/")
	// Clean already removed "..", but a name made only of dots can survive as
	// "." or "/".
	if name == "." || name == "/" {
		name = ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// Control characters have no business in a file name.
		case r == ':' && b.Len() == 1:
			// A leading "C:" would be a drive letter on extraction.
		default:
			b.WriteRune(r)
		}
	}
	name = b.String()
	// A tar segment cannot be empty or a bare dot after cleaning.
	segs := strings.Split(name, "/")
	clean := segs[:0]
	for _, s := range segs {
		if s == "" || s == "." {
			continue
		}
		clean = append(clean, s)
	}
	return strings.Join(clean, "/")
}

// Names returns the sanitised, de-duplicated names the entries would be stored
// under, without writing anything. It is what a manifest lists.
func Names(entries []Entry) []string {
	names, err := plan(entries)
	if err != nil {
		return nil
	}
	return names
}

// Sort orders entries by name, which is what the writer does before writing so
// that two runs producing the same set of files produce the same bytes.
func Sort(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}
