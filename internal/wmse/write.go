package wmse

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Options control how a snapshot is turned into a file.
type Options struct {
	// Compress deflates every section that benefits from it. It is on by
	// default: the format is built to be small, and a reader that wants one
	// section still only has to inflate that section.
	Compress bool
	// Level is the flate level; zero picks one from the section size.
	Level int
}

// DefaultOptions are what Write assumes when the caller does not care.
func DefaultOptions() Options { return Options{Compress: true} }

// SectionInfo describes one section as it exists in the file. It is what
// `wmse read --info` reports and what makes a partial read possible.
type SectionInfo struct {
	ID        uint8
	Name      string
	Codec     uint8
	Offset    int64
	StoredLen int
	RawLen    int
}

// FileInfo is everything the header says about a file, without decoding a
// section. The reader fills it from the directory alone.
type FileInfo struct {
	Version  uint64
	Flags    uint64
	Sections []SectionInfo
	// HeaderBytes is the size of the magic, the version and the directory.
	HeaderBytes int
	// TotalBytes is the size of the whole file.
	TotalBytes int64
}

// CodecName returns the readable name of a section codec.
func CodecName(codec uint8) string {
	if codec == codecFlate {
		return "flate"
	}
	return "raw"
}

// Write serialises a snapshot to a file.
func Write(path string, snap *Snapshot, opts Options) (*FileInfo, error) {
	if snap == nil {
		return nil, errors.New("nil snapshot")
	}
	raw, info, err := Marshal(snap, opts)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return nil, err
	}
	return info, nil
}

// sectionImage is one section's payload, both as encoded and as stored.
type sectionImage struct {
	codec  uint8
	raw    []byte
	stored []byte
}

// Marshal produces the complete file image together with its header
// description.
//
// The two passes are the whole trick. The first only learns which literals the
// file needs; the second is handed those literals as a sorted table and answers
// with positions in it. Because both passes walk the same snapshot in the same
// order, the table is complete before the first id is handed out and no id ever
// has to be revised.
func Marshal(snap *Snapshot, opts Options) ([]byte, *FileInfo, error) {
	col := newCollector()
	walk(snap, col)

	sorted := col.sortedStrings()
	idx := make(map[string]uint32, len(sorted))
	for i, s := range sorted {
		idx[s] = uint32(i)
	}

	st := newEncoderState(snap, idx)
	walk(snap, st.enc)

	st.writeMeta()
	st.writeStats()
	st.writeStrings(sorted)
	st.writeLinks()
	st.writeEdges()
	st.writeGroups()
	st.writeEndpoints()
	st.writeObservations()
	st.writeParams()
	st.writeEmulation()
	st.writeDicts()

	// Compress before the directory is laid out, so the directory can carry
	// both the stored and the logical size of every section.
	images := make([]sectionImage, len(st.ordered))
	var flags uint64
	for i, id := range st.ordered {
		raw := st.sections[id]
		img := sectionImage{codec: codecRaw, raw: raw, stored: raw}
		if opts.Compress {
			if c := deflate(raw, opts.Level); len(c) > 0 && len(c) < len(raw) {
				img.codec, img.stored = codecFlate, c
			}
		}
		if img.codec == codecFlate {
			flags |= flagCompressed
		}
		images[i] = img
	}

	// headerLen counts the bytes between itself and the first payload, which
	// is exactly the directory blob below, so the header never needs a second
	// pass to learn its own size.
	var dir byteWriter
	dir.uvarint(flags)
	dir.uvarint(uint64(len(st.ordered)))
	off := 0
	for i, id := range st.ordered {
		dir.uvarint(uint64(id))
		dir.uvarint(uint64(images[i].codec))
		dir.uvarint(uint64(off))
		dir.uvarint(uint64(len(images[i].stored)))
		dir.uvarint(uint64(len(images[i].raw)))
		off += len(images[i].stored)
	}

	var head byteWriter
	head.bytes([]byte(Magic))
	head.uvarint(FormatVersion)
	head.uvarint(uint64(dir.len()))
	head.bytes(dir.buf)

	out := make([]byte, 0, head.len()+off)
	out = append(out, head.buf...)
	for i := range images {
		out = append(out, images[i].stored...)
	}

	info := &FileInfo{
		Version:     FormatVersion,
		Flags:       flags,
		HeaderBytes: head.len(),
		TotalBytes:  int64(len(out)),
	}
	cur := 0
	for i, id := range st.ordered {
		info.Sections = append(info.Sections, SectionInfo{
			ID:        id,
			Name:      SectionName(id),
			Codec:     images[i].codec,
			Offset:    int64(cur),
			StoredLen: len(images[i].stored),
			RawLen:    len(images[i].raw),
		})
		cur += len(images[i].stored)
	}
	return out, info, nil
}

func deflate(raw []byte, level int) []byte {
	if len(raw) < 256 {
		// Too small to earn back a stream header and a dictionary.
		return nil
	}
	if level <= 0 {
		// Small sections go all the way; a very large one would spend more
		// time than the last fraction of a percent is worth.
		if len(raw) > 4<<20 {
			level = flate.DefaultCompression
		} else {
			level = flate.BestCompression
		}
	}
	var buf bytes.Buffer
	buf.Grow(len(raw) / 2)
	w, err := flate.NewWriter(&buf, level)
	if err != nil {
		return nil
	}
	if _, err := w.Write(raw); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

func inflate(stored []byte, codec uint8, want int) ([]byte, error) {
	if codec != codecFlate {
		return stored, nil
	}
	var buf bytes.Buffer
	buf.Grow(want)
	r := flate.NewReader(bytes.NewReader(stored))
	defer r.Close()
	if _, err := io.Copy(&buf, r); err != nil {
		return nil, fmt.Errorf("%w: inflate: %v", ErrBadSection, err)
	}
	return buf.Bytes(), nil
}

// sortedStrings returns the collector's literals in the order the table stores
// them: lexicographic, which is what makes front coding worth anything.
func (c *collector) sortedStrings() []string {
	out := make([]string, 0, len(c.strs))
	for s := range c.strs {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
