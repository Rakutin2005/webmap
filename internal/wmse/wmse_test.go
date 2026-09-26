package wmse

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/urlgroup"
)

// build a snapshot that exercises every section: pages, relations, patterns,
// contracts with all field shapes, raw observations, recovered parameters and
// an emulation aggregate.
func testSnapshot() *Snapshot {
	root := "https://example.com/"
	snap := &Snapshot{
		Meta: Meta{
			MetaTool:   "webmap",
			MetaTarget: root,
			"flags":    "-k -r -rdepth 5 -j -apic",
		},
		Stats: Stats{
			Total: 4, Resolved: 3, Unresolved: 1, WithParams: 1,
			ByCategory: map[linker.Category]int{linker.CategoryWebPage: 2, linker.CategoryAPI: 1, linker.CategoryWebAsset: 1},
			ByLinkType: map[linker.LinkType]int{linker.LinkTypeAbsolute: 3, linker.LinkTypeRelative: 1},
			ByClass:    map[linker.URLClass]int{linker.ClassNormal: 3, linker.ClassCDN: 1},
			ByTag:      map[string]int{"api": 1, "js": 1},
		},
		Links: []linker.Link{
			{
				HREF: root, Resolved: root, Domain: "example.com",
				Category: linker.CategoryWebPage, LinkType: linker.LinkTypeAbsolute, Depth: 0,
			},
			{
				HREF: "/api/users?id=7", Resolved: "https://example.com/api/users?id=7",
				Domain: "example.com", Category: linker.CategoryAPI, LinkType: linker.LinkTypeRelative,
				Depth: 1, HasParams: true, SourceURL: root, Tag: "api",
				ParamVariants: []linker.ParamVariant{{Query: "id=7"}},
				// The arguments are the third field of a call signature and
				// the file has to keep them: -apif prints them, and a
				// format that dropped them would make the flag unanswerable
				// from a saved scan.
				APIDetails: []linker.APIDetail{{MatchSource: "call", HTTPMethod: "GET", Arguments: "id, name"}},
			},
			{
				HREF: "/static/app.js", Resolved: "https://example.com/static/app.js",
				Domain: "example.com", Category: linker.CategoryWebAsset, LinkType: linker.LinkTypeRelative,
				Depth: 1, SourceURL: root, Class: linker.ClassNormal, Tag: "js",
			},
			{
				HREF: "https://cdn.example.net/lib.js", Resolved: "https://cdn.example.net/lib.js",
				Domain: "cdn.example.net", Category: linker.CategoryWebAsset,
				LinkType: linker.LinkTypeAbsolute, Depth: 1, SourceURL: root, Class: linker.ClassCDN,
			},
		},
		Pages: []Page{
			{URL: root, Depth: 0, ContentType: "text/html; charset=utf-8", Links: 3},
			{URL: "https://example.com/api/users?id=7", Depth: 1, ContentType: "application/json", Links: 0},
		},
		Groups: []Group{{
			Domain:  "example.com",
			Pattern: "https://example.com/complex/{id: int}/contacts",
			Count:   2,
			Vars:    []urlgroup.Var{{Kind: "int", Values: []string{"9223", "9224"}, Nums: []int64{9223, 9224}}},
			Members: []string{"https://example.com/complex/9223/contacts", "https://example.com/complex/9224/contacts"},
		}},
		Endpoints: []contract.Endpoint{{
			Path:    "https://example.com/api/users",
			Methods: []string{"GET", "POST"},
			Calls:   12,
			Query: []contract.Field{
				{Name: "id", Kind: "int", Values: []string{"7"}},
				{Name: "q", Kind: "str", Inferred: true},
			},
			Headers: []contract.Field{
				{Name: "x-requested-with", Kind: "str", Values: []string{"XMLHttpRequest"}, Constant: true, ConstVal: "XMLHttpRequest"},
			},
			Bodies: []contract.BodyFormat{{
				Kind: "json", MIME: "application/json",
				Fields:  []contract.Field{{Name: "name", Kind: "str"}},
				Methods: []string{"user.get"},
				Sample:  `{"name":"<name>"}`,
			}},
			Response:   []contract.ResponseField{{Path: "items[]", Kind: "array"}, {Path: "meta.total", Kind: "number"}},
			Raw:        []string{"GET /api/users?id=7"},
			Unobserved: false,
		}},
		Observations: []contract.Observation{{
			URL: "https://example.com/api/users?id=7", Method: "GET",
			Headers:        []contract.NameValue{{Name: "x-requested-with", Value: "XMLHttpRequest"}},
			Body:           `{"name":"a"}`,
			ResponseFields: []contract.ResponseField{{Path: "items[]", Kind: "array"}},
			InferredQuery:  []contract.NameValue{{Name: "q", Value: ""}},
		}, {
			URL: "https://example.com/ghost", Method: "", EndpointOnly: true,
		}},
		Params: []Param{{
			ParamRef: linker.ParamRef{
				Endpoints: []string{"https://example.com/api/users"},
				Carrier:   "this.ajaxUrl", Name: "page", Kind: linker.ParamQuery,
				Owner: linker.OwnerRequest, Count: 3,
			},
			Docs: []string{"https://example.com/static/app.js"},
		}},
		Emulation: &Emulation{
			Scripts: 2, Calls: 2, Abandoned: 1,
			ByType: map[string]int{"fetch": 1, "xhr": 1},
			Errors: []string{"TypeError: x is not a function"},
			List: []Call{{
				URL: "https://example.com/api/users?id=7", RawURL: "/api/users?id=7",
				Method: "GET", Initiator: "https://example.com/static/app.js",
				Type: "fetch", Headers: [][2]string{{"accept", "application/json"}},
			}},
		},
	}
	return snap
}

func roundTrip(t *testing.T, snap *Snapshot) *Snapshot {
	t.Helper()
	raw, info, err := Marshal(snap, DefaultOptions())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if info.TotalBytes != int64(len(raw)) {
		t.Fatalf("info size %d != file size %d", info.TotalBytes, len(raw))
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Info().HeaderBytes != info.HeaderBytes {
		t.Errorf("header bytes %d != %d", f.Info().HeaderBytes, info.HeaderBytes)
	}
	for i, s := range info.Sections {
		got := f.Info().Sections[i]
		if got.StoredLen != s.StoredLen || got.RawLen != s.RawLen || got.Codec != s.Codec || got.Offset != s.Offset {
			t.Errorf("section %s: directory mismatch %+v vs %+v", s.Name, got, s)
		}
	}
	got, err := f.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return got
}

func TestRoundTrip(t *testing.T) {
	want := testSnapshot()
	got := roundTrip(t, want)

	if !reflect.DeepEqual(want.Meta, got.Meta) {
		t.Errorf("meta:\n got %v\nwant %v", got.Meta, want.Meta)
	}
	if want.Stats.Total != got.Stats.Total || want.Stats.WithParams != got.Stats.WithParams {
		t.Errorf("stats: got %+v", got.Stats)
	}
	// The closed enumerations come back with every slot filled, zeros
	// included, which is what the categorizer hands the report. A count of zero
	// and a missing count have to be the same thing, or a category would be
	// lost on the way through the file.
	for cat := linker.CategoryUnknown; cat < catSlots; cat++ {
		if got.Stats.ByCategory[cat] != want.Stats.ByCategory[cat] {
			t.Errorf("byCategory[%v]: got %d want %d", cat, got.Stats.ByCategory[cat], want.Stats.ByCategory[cat])
		}
	}
	for lt := linker.LinkTypeUnknown; lt < typeSlots; lt++ {
		if got.Stats.ByLinkType[lt] != want.Stats.ByLinkType[lt] {
			t.Errorf("byLinkType[%v]: got %d want %d", lt, got.Stats.ByLinkType[lt], want.Stats.ByLinkType[lt])
		}
	}
	for cl := linker.ClassNormal; cl < classSlots; cl++ {
		if got.Stats.ByClass[cl] != want.Stats.ByClass[cl] {
			t.Errorf("byClass[%v]: got %d want %d", cl, got.Stats.ByClass[cl], want.Stats.ByClass[cl])
		}
	}
	if !reflect.DeepEqual(want.Stats.ByTag, got.Stats.ByTag) {
		t.Errorf("byTag: got %v want %v", got.Stats.ByTag, want.Stats.ByTag)
	}

	// Links are stored in URL order, so compare as sets keyed by identity.
	byKey := map[string]linker.Link{}
	for _, l := range got.Links {
		byKey[LinkKey(&l)] = l
	}
	if len(byKey) != len(want.Links) {
		t.Fatalf("links: got %d want %d", len(byKey), len(want.Links))
	}
	for i := range want.Links {
		w := want.Links[i]
		g, ok := byKey[LinkKey(&w)]
		if !ok {
			t.Errorf("link %q missing", LinkKey(&w))
			continue
		}
		if !reflect.DeepEqual(w, g) {
			t.Errorf("link %q:\n got %+v\nwant %+v", LinkKey(&w), g, w)
		}
	}

	if !reflect.DeepEqual(want.Pages, got.Pages) {
		t.Errorf("pages:\n got %+v\nwant %+v", got.Pages, want.Pages)
	}
	if len(got.Groups) != 1 {
		t.Fatalf("groups: got %d", len(got.Groups))
	}
	if !reflect.DeepEqual(want.Groups[0], got.Groups[0]) {
		t.Errorf("group:\n got %+v\nwant %+v", got.Groups[0], want.Groups[0])
	}
	if !reflect.DeepEqual(want.Endpoints, got.Endpoints) {
		t.Errorf("endpoints:\n got %+v\nwant %+v", got.Endpoints, want.Endpoints)
	}
	if !reflect.DeepEqual(want.Observations, got.Observations) {
		t.Errorf("observations:\n got %+v\nwant %+v", got.Observations, want.Observations)
	}
	if !reflect.DeepEqual(want.Params, got.Params) {
		t.Errorf("params:\n got %+v\nwant %+v", got.Params, want.Params)
	}
	if !reflect.DeepEqual(want.Emulation, got.Emulation) {
		t.Errorf("emulation:\n got %+v\nwant %+v", got.Emulation, want.Emulation)
	}
}

func TestEdgesDescribeRelations(t *testing.T) {
	got := roundTrip(t, testSnapshot())
	links := make([]linker.Link, len(got.Links))
	copy(links, got.Links)
	url := func(id uint32) string { return LinkKey(&links[id]) }

	counts := map[uint8]int{}
	for _, e := range got.Edges {
		counts[e.Kind]++
	}
	// root -> /api/users (source), root -> /static/app.js, root -> cdn link,
	// and the crawl edge root -> /api/users because it was really fetched.
	if counts[EdgeSource] != 3 {
		t.Errorf("source edges: got %d want 3", counts[EdgeSource])
	}
	if counts[EdgeCrawled] != 1 {
		t.Errorf("crawled edges: got %d want 1", counts[EdgeCrawled])
	}
	if counts[EdgeParam] != 1 {
		t.Errorf("param edges: got %d want 1", counts[EdgeParam])
	}
	if counts[EdgePattern] != 0 {
		t.Errorf("pattern edges: got %d want 0 (members are not link nodes)", counts[EdgePattern])
	}

	// The contract link must be joined to the endpoint, and the param to it too.
	var joined, paramTo int
	for _, e := range got.Edges {
		switch e.Kind {
		case EdgeContract:
			joined++
			if url(e.From) != "https://example.com/api/users?id=7" {
				t.Errorf("contract edge from %q", url(e.From))
			}
			if got.Endpoints[e.To].Path != "https://example.com/api/users" {
				t.Errorf("contract edge to %q", got.Endpoints[e.To].Path)
			}
		case EdgeParam:
			paramTo = int(e.To)
		}
	}
	if joined != 0 {
		// The endpoint is keyed without the query, so this endpoint has no
		// link node; that is the whole point of keying on the path.
		t.Errorf("contract edges: got %d want 0", joined)
	}
	if paramTo != 0 || got.Params[0].Name != "page" {
		t.Errorf("param edge not attached to its endpoint")
	}
}

func TestStringTableIsInternedAndFrontCoded(t *testing.T) {
	snap := testSnapshot()
	// Realistic shape: a site whose URLs share long prefixes, plus the same
	// literal reached from many places. Front coding is only worth anything on
	// prefix-heavy data, which is what a crawl actually produces.
	for i := 0; i < 200; i++ {
		snap.Links = append(snap.Links, linker.Link{
			HREF:     fmt.Sprintf("/news/%d/comments", i),
			Resolved: fmt.Sprintf("https://example.com/news/%d/comments", i),
			Domain:   "example.com", Category: linker.CategoryWebPage,
			LinkType: linker.LinkTypeRelative, SourceURL: "https://example.com/",
		})
	}
	snap.Links = append(snap.Links, linker.Link{
		HREF: "https://example.com/api/users?id=7", Resolved: "https://example.com/api/users?id=7",
		Domain: "example.com", Category: linker.CategoryAPI, Tag: "api",
		SourceURL: "https://example.com/",
	})

	raw, _, err := Marshal(snap, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := f.section(SecStrings)
	if err != nil {
		t.Fatal(err)
	}
	r := &byteReader{buf: body}
	n, err := r.uvarint()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var stored, total int
	prev := ""
	for i := uint64(0); i < n; i++ {
		shared, _ := r.uvarint()
		suf, err := r.rawString()
		if err != nil {
			t.Fatal(err)
		}
		s := prev[:shared] + suf
		if seen[s] {
			t.Errorf("duplicate string in table: %q", s)
		}
		seen[s] = true
		stored += len(suf) + 2
		total += len(s)
		prev = s
	}
	if !seen["https://example.com/api/users?id=7"] {
		t.Errorf("url missing from table")
	}
	if !seen[""] {
		t.Errorf("empty string must be a table entry")
	}
	if stored*2 > total {
		t.Errorf("front coding ineffective: %d stored bytes for %d raw", stored, total)
	}
	if !bytes.HasPrefix(raw, []byte(Magic)) {
		t.Errorf("missing magic")
	}
}

func TestSizeGrowsSublinearlyWithURLs(t *testing.T) {
	// The point of the whole format: a family of near-identical URLs must cost
	// far less than its raw text, and the second thousand must cost less than
	// the first.
	sizes := func(n int) int {
		snap := &Snapshot{Meta: Meta{MetaTarget: "https://example.com/"}}
		for i := 0; i < n; i++ {
			snap.Links = append(snap.Links, linker.Link{
				HREF:     fmt.Sprintf("/news/%d/comments", i),
				Resolved: fmt.Sprintf("https://example.com/news/%d/comments", i),
				Domain:   "example.com", Category: linker.CategoryWebPage,
				LinkType: linker.LinkTypeRelative, SourceURL: "https://example.com/",
			})
		}
		raw, _, err := Marshal(snap, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		return len(raw)
	}
	one, hundred, thousand := sizes(1), sizes(100), sizes(1000)
	if hundred >= 50*one {
		t.Errorf("100 URLs cost %d, not much more than 100x the single-URL %d", hundred, one)
	}
	// Each extra 900 URLs must cost less per URL than the first 100 did.
	if (thousand-hundred)/900 >= (hundred-one)/99 {
		t.Errorf("per-URL cost did not fall: first 100 %d bytes each, next 900 %d each",
			(hundred-one)/99, (thousand-hundred)/900)
	}
	if thousand > 20000 {
		t.Errorf("1000 near-identical URLs cost %d bytes, expected well under 20k", thousand)
	}
}

func TestCompressionShrinks(t *testing.T) {
	snap := testSnapshot()
	for i := 0; i < 400; i++ {
		snap.Links = append(snap.Links, linker.Link{
			HREF:     fmt.Sprintf("/item/%d/", i),
			Resolved: fmt.Sprintf("https://example.com/item/%d/", i),
			Domain:   "example.com", Category: linker.CategoryWebPage,
			LinkType: linker.LinkTypeRelative, SourceURL: "https://example.com/",
		})
	}
	plain, _, err := Marshal(snap, Options{Compress: false})
	if err != nil {
		t.Fatal(err)
	}
	small, _, err := Marshal(snap, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(small) >= len(plain) {
		t.Errorf("compression did not help: %d >= %d", len(small), len(plain))
	}
	f, err := Parse(small)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Info().Sections {
		if s.Codec == codecFlate && s.StoredLen >= s.RawLen {
			t.Errorf("section %s compressed to a larger size", s.Name)
		}
	}
	got, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Links) != len(snap.Links) {
		t.Errorf("links after compression: %d want %d", len(got.Links), len(snap.Links))
	}
}

func TestUncompressedFileStillReads(t *testing.T) {
	raw, _, err := Marshal(testSnapshot(), Options{Compress: false})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte{0x78, 0x9c}) {
		t.Errorf("raw file appears to contain a flate stream")
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.Info().Flags&flagCompressed != 0 {
		t.Errorf("raw file reports compressed flag")
	}
	if _, err := f.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not a webmap file at all")); err != ErrBadMagic {
		t.Errorf("want ErrBadMagic, got %v", err)
	}
	if _, err := Parse(nil); err != ErrTruncated {
		t.Errorf("want ErrTruncated, got %v", err)
	}
	raw, _, err := Marshal(testSnapshot(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	// A truncated file must fail on the directory, not panic.
	if _, err := Parse(raw[:len(raw)/2]); err == nil {
		t.Errorf("truncated file accepted")
	}
	// A wrong version must be refused loudly.
	bad := append([]byte(nil), raw...)
	bad[8] = 99
	if _, err := Parse(bad); err == nil {
		t.Errorf("wrong version accepted")
	}
}

func TestCorruptSectionIsAnErrorNotAPanic(t *testing.T) {
	raw, _, err := Marshal(testSnapshot(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the payload of the links section in place; every varint in the
	// file must be bounds checked.
	for i := range f.info.Sections {
		if f.info.Sections[i].ID == SecLinks {
			start := f.base + int(f.info.Sections[i].Offset)
			for j := start; j < start+f.info.Sections[i].StoredLen && j < len(f.raw); j++ {
				f.raw[j] = 0xff
			}
		}
	}
	f.sections[SecLinks] = nil
	if _, err := f.Load(); err == nil {
		t.Errorf("corrupt link section accepted")
	}
}

func TestEmptySnapshot(t *testing.T) {
	snap := &Snapshot{Meta: Meta{}}
	got := roundTrip(t, snap)
	if got.Meta == nil {
		t.Errorf("meta should be non-nil")
	}
	if len(got.Links) != 0 || len(got.Endpoints) != 0 {
		t.Errorf("expected an empty snapshot")
	}
}

func TestWriteToPath(t *testing.T) {
	path := t.TempDir() + "/nested/scan.wmse"
	info, err := Write(path, testSnapshot(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Info().TotalBytes != info.TotalBytes {
		t.Errorf("size mismatch on reopen")
	}
	if _, err := f.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestSectionNamesAndEdgesAreDocumented(t *testing.T) {
	for id := uint8(1); id < secCount; id++ {
		if SectionName(id) == "" {
			t.Errorf("section %d has no name", id)
		}
	}
	for k := uint8(1); int(k) < EdgeCount; k++ {
		if EdgeName(k) == "" {
			t.Errorf("edge kind %d has no name", k)
		}
	}
	if !strings.HasPrefix(Magic, "WMAP") {
		t.Errorf("magic %q", Magic)
	}
}

// TestWritingASnapshotTwiceDoesNotDoubleItsRelations covers the round trip a
// session's save makes: a file is read, added to, and written back.
//
// The writer derives the relations from the links, and a snapshot read back from
// a file already carries the relations a previous write derived from it. Deriving
// those again reproduces them exactly, so a writer that appended the two instead
// of unioning them would double the graph of every file it was asked to re-save -
// and a session that saved a file ten times would carry a hundred graphs' worth of
// edges, none of them a discovery.
func TestWritingASnapshotTwiceDoesNotDoubleItsRelations(t *testing.T) {
	once := roundTrip(t, testSnapshot())
	twice := roundTrip(t, once)
	thrice := roundTrip(t, twice)

	if len(once.Edges) == 0 {
		t.Fatalf("the fixture derived no relations, so this proves nothing")
	}
	if len(twice.Edges) != len(once.Edges) {
		t.Errorf("writing a file again changed its relations from %d to %d",
			len(once.Edges), len(twice.Edges))
	}
	if len(thrice.Edges) != len(once.Edges) {
		t.Errorf("writing a file three times left %d relations, want %d",
			len(thrice.Edges), len(once.Edges))
	}
	// The doubling was not a reshuffle: the same relations twice over is what a
	// reader of the graph sees as a site twice as connected as it is.
	seen := map[Edge]int{}
	for _, e := range twice.Edges {
		seen[e]++
	}
	for e, n := range seen {
		if n > 1 {
			t.Errorf("the relation %v was written %d times", e, n)
			break
		}
	}
}
