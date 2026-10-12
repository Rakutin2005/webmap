package textparse

import (
	"archive/zip"
	"bytes"
	"io"
	"path"
	"strings"
	"unicode/utf16"
)

// maxDecoded bounds one decompressed container part, and the total decoded from
// one document, so a hostile file cannot turn a small download into unbounded
// memory.
const maxDecoded = 16 << 20

// zipTextEntry reports whether a part inside a zipped office document can hold
// text or addresses. OOXML and ODF keep prose in XML parts and hyperlink
// targets in .rels parts; both end up here.
func zipTextEntry(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".xml", ".rels", ".txt", ".json", ".csv", ".md", ".html", ".htm":
		return true
	}
	return false
}

// extractZip reads the text and relationship parts of a zipped office document
// (.docx, .odt, .xlsx, .pptx, ...). The parts are XML, so URLs appear either as
// element text or as attribute values (a relationship Target); both are visible
// to the URL scan without unzipping to disk.
func extractZip(body string) string {
	data := []byte(body)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	var sb strings.Builder
	total := 0
	for _, f := range zr.File {
		if !zipTextEntry(f.Name) {
			continue
		}
		limit := int64(maxDecoded - total)
		if limit <= 0 {
			break
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		part, err := io.ReadAll(io.LimitReader(rc, limit))
		rc.Close()
		if err != nil {
			continue
		}
		sb.Write(part)
		sb.WriteByte('\n')
		total += len(part)
	}
	return sb.String()
}

// extractLegacy scans a binary office container or an RTF file for text. An
// OLE2 .doc stores its prose as UTF-16LE and its metadata as ASCII; scanning
// both interpretations recovers the URLs without a full compound-file parser.
// Reading more than the URLs would be guesswork, and there is no need for it.
func extractLegacy(body string) string {
	var sb strings.Builder
	sb.WriteString(body)
	sb.WriteByte('\n')
	sb.WriteString(decodeUTF16LE([]byte(body)))
	return sb.String()
}

// decodeUTF16LE decodes a byte run as little-endian UTF-16. Random binary turns
// into replacement characters, which the URL scan ignores; text stored as
// UTF-16, which is how a legacy .doc stores its prose, comes back readable.
func decodeUTF16LE(data []byte) string {
	if len(data) > maxDecoded {
		data = data[:maxDecoded]
	}
	n := len(data) / 2
	if n == 0 {
		return ""
	}
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		u[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}
