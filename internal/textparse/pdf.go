package textparse

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"io"
	"strings"
)

// extractPDF recovers the text-bearing parts of a PDF: the string operators in
// its content streams (which are usually FlateDecode-compressed) and the URI
// annotation values. It does not reconstruct a page layout; link discovery only
// needs the URLs, and those are visible in the strings.
func extractPDF(body string) string {
	var sb strings.Builder
	data := []byte(body)
	// The uncompressed scaffolding - metadata, annotations, /URI actions - is
	// already text, so the raw bytes are scanned before any stream is inflated.
	sb.Write(data)
	sb.WriteByte('\n')
	inflatePDFStreams(data, &sb)
	appendPDFStrings(data, &sb)
	return sb.String()
}

// inflatePDFStreams walks the "stream ... endstream" regions and appends the
// inflated bytes of the ones that are zlib/deflate compressed. Streams with a
// filter we do not implement are skipped: a wrong guess would only add noise,
// while the raw scan already covers the uncompressed parts.
func inflatePDFStreams(data []byte, sb *strings.Builder) {
	const streamKw = "stream"
	const endKw = "endstream"
	total := 0
	idx := 0
	for idx < len(data) && total < maxDecoded {
		s := bytes.Index(data[idx:], []byte(streamKw))
		if s < 0 {
			break
		}
		s += idx
		// "endstream" also ends in "stream"; that occurrence opens nothing.
		if s >= len("end") && string(data[s-len("end"):s]) == "end" {
			idx = s + len(streamKw)
			continue
		}
		start := s + len(streamKw)
		if start < len(data) && data[start] == '\r' {
			start++
		}
		if start < len(data) && data[start] == '\n' {
			start++
		}
		e := bytes.Index(data[start:], []byte(endKw))
		if e < 0 {
			break
		}
		e += start
		if dec, err := inflate(data[start:e]); err == nil && len(dec) > 0 {
			if remaining := maxDecoded - total; len(dec) > remaining {
				dec = dec[:remaining]
			}
			sb.Write(dec)
			sb.WriteByte('\n')
			total += len(dec)
		}
		idx = e + len(endKw)
	}
}

var errNoInflate = errors.New("textparse: stream is not zlib/deflate")

// inflate tries the two compressions a PDF stream actually uses: zlib (the
// FlateDecode default) and raw deflate.
func inflate(raw []byte) ([]byte, error) {
	raw = bytes.TrimRight(raw, "\r\n")
	if r, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		defer r.Close()
		if out, err := io.ReadAll(io.LimitReader(r, maxDecoded)); err == nil {
			return out, nil
		}
	}
	r := flate.NewReader(bytes.NewReader(raw))
	defer r.Close()
	if out, err := io.ReadAll(io.LimitReader(r, maxDecoded)); err == nil {
		return out, nil
	}
	return nil, errNoInflate
}

// appendPDFStrings decodes the parenthesised and hex string literals that a
// content stream uses to place text. Scanning the inflated bytes already finds
// URLs written plainly; this catches the ones an escape sequence or a hex
// encoding would otherwise hide.
func appendPDFStrings(data []byte, sb *strings.Builder) {
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '(':
			if s, next, ok := readPDFLiteral(data, i); ok {
				sb.WriteString(s)
				sb.WriteByte(' ')
				i = next - 1
			}
		case '<':
			if i+1 < len(data) && data[i+1] != '<' {
				if s, next, ok := readPDFHex(data, i); ok {
					sb.WriteString(s)
					sb.WriteByte(' ')
					i = next - 1
				}
			}
		}
	}
}

// readPDFLiteral reads a PDF literal string starting at the '(' at start. It
// understands the escapes the format defines, including the octal ones used
// for bytes outside ASCII.
func readPDFLiteral(data []byte, start int) (string, int, bool) {
	var sb strings.Builder
	depth := 0
	for i := start; i < len(data); i++ {
		c := data[i]
		switch c {
		case '\\':
			if i+1 >= len(data) {
				return sb.String(), i + 1, true
			}
			i++
			switch e := data[i]; e {
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case '(', ')', '\\':
				sb.WriteByte(e)
			case '\r':
				if i+1 < len(data) && data[i+1] == '\n' {
					i++
				}
			case '\n':
				// A backslash before a newline is a line continuation.
			default:
				if e >= '0' && e <= '7' {
					val := int(e - '0')
					for n := 0; n < 2 && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '7'; n++ {
						i++
						val = val*8 + int(data[i]-'0')
					}
					sb.WriteByte(byte(val))
				} else {
					sb.WriteByte(e)
				}
			}
		case '(':
			depth++
			if depth > 1 {
				sb.WriteByte(c)
			}
		case ')':
			depth--
			if depth == 0 {
				return sb.String(), i + 1, true
			}
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String(), len(data), true
}

// readPDFHex reads a hex string starting at the '<' at start.
func readPDFHex(data []byte, start int) (string, int, bool) {
	var hex []byte
	for i := start + 1; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '>':
			out := make([]byte, 0, len(hex)/2)
			for j := 0; j+1 < len(hex); j += 2 {
				out = append(out, hexVal(hex[j])<<4|hexVal(hex[j+1]))
			}
			return string(out), i + 1, true
		case isHex(c):
			hex = append(hex, c)
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0:
			// whitespace between hex digits is allowed
		default:
			return "", i + 1, false
		}
	}
	return "", len(data), false
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
