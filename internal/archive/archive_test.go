package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

// readTar returns the names and contents of an uncompressed tar.
func readTar(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body %s: %v", h.Name, err)
		}
		out[h.Name] = string(b)
	}
	return out
}

// TestTarRoundTrips covers the embedded form: what Tar writes, a stock tar
// reader reads back unchanged. This is the path the .wmse section depends on.
func TestTarRoundTrips(t *testing.T) {
	in := []Entry{
		{Name: "example.com/index.html.ref", Data: []byte("first")},
		{Name: "example.com/static/app.js.ref", Data: []byte("second")},
	}
	raw, err := TarBytes(in)
	if err != nil {
		t.Fatalf("Tar: %v", err)
	}
	got := readTar(t, raw)
	if len(got) != 2 {
		t.Fatalf("entries: got %d, want 2", len(got))
	}
	if got["example.com/index.html.ref"] != "first" {
		t.Errorf("first: %q", got["example.com/index.html.ref"])
	}
	if got["example.com/static/app.js.ref"] != "second" {
		t.Errorf("second: %q", got["example.com/static/app.js.ref"])
	}
}

// TestTarIsDeterministic pins reproducibility: two runs over the same entries
// must produce identical bytes, or a test that compares archives is a test that
// can only walk them.
func TestTarIsDeterministic(t *testing.T) {
	in := []Entry{{Name: "a.ref", Data: []byte("x")}, {Name: "b/c.ref", Data: []byte("y")}}
	a, err := TarBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := TarBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("two runs over the same entries produced different bytes")
	}
}

// TestSanitizeKeepsExtractionInside covers the reason names are cleaned at all:
// a name that escapes the destination, or a drive letter, or a control
// character, is either a traversal or something no filesystem accepts.
func TestSanitizeKeepsExtractionInside(t *testing.T) {
	cases := map[string]string{
		"../etc/passwd":          "etc/passwd",
		"/abs/path.html":         "abs/path.html",
		"a/../../b":              "b",
		"https://example.com/x":  "example.com/x",
		"example.com/index.html": "example.com/index.html",
		"a//b":                   "a/b",
		"./a":                    "a",
		"a/b/..":                 "a",
		"..":                     "",
		"/":                      "",
		"a\x00b":                 "ab",
		"C:/win":                 "C/win",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCollisionsAreDisambiguated: two entries that clean to the same name must
// both survive. Overwriting one, or dropping it, is worse than a longer name.
func TestCollisionsAreDisambiguated(t *testing.T) {
	in := []Entry{
		{Name: "a/x.ref", Data: []byte("1")},
		{Name: "./a/x.ref", Data: []byte("2")},
		{Name: "/a/x.ref", Data: []byte("3")},
	}
	raw, err := TarBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	got := readTar(t, raw)
	if len(got) != 3 {
		t.Fatalf("entries: got %d, want 3: %v", len(got), got)
	}
	if got["a/x.ref"] != "1" {
		t.Errorf("first should keep the plain name, got %q", got["a/x.ref"])
	}
	// The other two were renamed; both are present under distinct names.
	var others int
	for name, body := range got {
		if name != "a/x.ref" && strings.HasPrefix(name, "a/x") {
			others++
			if body != "2" && body != "3" {
				t.Errorf("unexpected body %q for %s", body, name)
			}
		}
	}
	if others != 2 {
		t.Errorf("renamed entries: got %d, want 2", others)
	}
}

// TestWriteFormats reads each compressed form back with a stock reader.
func TestWriteFormats(t *testing.T) {
	in := []Entry{{Name: "x.ref", Data: []byte("hello " + strings.Repeat("world ", 50))}}

	var gz bytes.Buffer
	if err := Write(&gz, in, TarGz); err != nil {
		t.Fatalf("TarGz: %v", err)
	}
	zr, err := gzip.NewReader(&gz)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	rawGz, _ := io.ReadAll(zr)
	if got := readTar(t, rawGz); got["x.ref"] == "" {
		t.Errorf("tar.gz did not round-trip: %v", got)
	}

	var zbuf bytes.Buffer
	if err := Write(&zbuf, in, Zip); err != nil {
		t.Fatalf("Zip: %v", err)
	}
	zr2, err := zip.NewReader(bytes.NewReader(zbuf.Bytes()), int64(zbuf.Len()))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	if len(zr2.File) != 1 || zr2.File[0].Name != "x.ref" {
		t.Errorf("zip entries: %+v", zr2.File)
	}
}

// TestParseFormat covers the names a caller may pass and the error for one that
// is not a format at all.
func TestParseFormat(t *testing.T) {
	ok := map[string]Format{"": Tar, "tar": Tar, "tar.gz": TarGz, "TGZ": TarGz, "zip": Zip}
	for in, want := range ok {
		got, err := ParseFormat(in)
		if err != nil {
			t.Errorf("ParseFormat(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFormat(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseFormat("rar"); err == nil {
		t.Error("an unknown format should be an error")
	}
}

// TestEmptyArchiveIsStillReadable: an archive with no entries is valid, a tar
// with a zero-length body is not. A scan that found nothing still has to produce
// something a reader can open.
func TestEmptyArchiveIsStillReadable(t *testing.T) {
	raw, err := TarBytes(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(readTar(t, raw)) != 0 {
		t.Error("empty tar should read back as no entries")
	}
	// An entry whose name sanitises away entirely is an error, not a silent
	// skip, so a caller never ships a file it thinks it included.
	if _, err := TarBytes([]Entry{{Name: "..", Data: []byte("x")}}); err == nil {
		t.Error("an entry that sanitises to nothing should be an error")
	}
}
