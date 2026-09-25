package wmse

import (
	"fmt"
	"os"

	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/urlgroup"
)

// File is an opened snapshot. Only the directory is read up front; sections are
// inflated on demand and then kept, so `wmse read --info` costs a few hundred
// bytes of a scan's file and `--contracts` never touches the link table.
type File struct {
	raw      []byte
	base     int // first payload byte
	info     *FileInfo
	dec      *decoder
	sections map[uint8][]byte
}

// Open reads the header and directory of a snapshot file.
func Open(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse reads the header and directory of an in-memory snapshot.
func Parse(raw []byte) (*File, error) {
	if len(raw) < len(Magic) {
		return nil, ErrTruncated
	}
	if string(raw[:len(Magic)]) != Magic {
		return nil, ErrBadMagic
	}
	r := &byteReader{buf: raw, pos: len(Magic)}
	version, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if version != FormatVersion {
		return nil, fmt.Errorf("%w: file is v%d, this build reads v%d", ErrBadVersion, version, FormatVersion)
	}
	headerLen, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if headerLen > uint64(len(raw)-r.pos) {
		return nil, ErrTruncated
	}
	dirStart := r.pos
	dir := &byteReader{buf: raw[:dirStart+int(headerLen)], pos: dirStart}

	flags, err := dir.uvarint()
	if err != nil {
		return nil, err
	}
	n, err := dir.uvarint()
	if err != nil {
		return nil, err
	}
	base := dirStart + int(headerLen)
	if base > len(raw) {
		return nil, ErrTruncated
	}

	info := &FileInfo{Version: version, Flags: flags, HeaderBytes: base}
	for i := uint64(0); i < n; i++ {
		id, err := dir.uvarint()
		if err != nil {
			return nil, err
		}
		codec, err := dir.uvarint()
		if err != nil {
			return nil, err
		}
		off, err := dir.uvarint()
		if err != nil {
			return nil, err
		}
		storedLen, err := dir.uvarint()
		if err != nil {
			return nil, err
		}
		rawLen, err := dir.uvarint()
		if err != nil {
			return nil, err
		}
		if off+storedLen > uint64(len(raw)-base) {
			return nil, ErrTruncated
		}
		info.Sections = append(info.Sections, SectionInfo{
			ID:        uint8(id),
			Name:      SectionName(uint8(id)),
			Codec:     uint8(codec),
			Offset:    int64(off),
			StoredLen: int(storedLen),
			RawLen:    int(rawLen),
		})
	}
	info.TotalBytes = int64(len(raw))

	f := &File{raw: raw, base: base, info: info, sections: map[uint8][]byte{}}
	for _, s := range info.Sections {
		f.sections[s.ID] = nil
	}
	return f, nil
}

// Info returns the header description.
func (f *File) Info() *FileInfo { return f.info }

// Has reports whether the file carries a section.
func (f *File) Has(id uint8) bool {
	_, ok := f.sections[id]
	return ok
}

// section returns a decompressed section body.
func (f *File) section(id uint8) ([]byte, error) {
	if b, ok := f.sections[id]; ok && b != nil {
		return b, nil
	}
	var si SectionInfo
	found := false
	for _, s := range f.info.Sections {
		if s.ID == id {
			si, found = s, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrMissingSec, SectionName(id))
	}
	start := f.base + int(si.Offset)
	if start+si.StoredLen > len(f.raw) {
		return nil, ErrTruncated
	}
	out, err := inflate(f.raw[start:start+si.StoredLen], si.Codec, si.RawLen)
	if err != nil {
		return nil, err
	}
	f.sections[id] = out
	return out, nil
}

// decoder resolves the ids a section is written in terms of.
type decoder struct {
	strs      []string
	lists     [][]uint32
	pairs     [][2]uint32
	pairLists [][]uint32
}

func (d *decoder) str(id uint32) (string, error) {
	if int(id) >= len(d.strs) {
		return "", fmt.Errorf("%w: string id %d", ErrBadSection, id)
	}
	return d.strs[id], nil
}

func (d *decoder) list(id uint32) ([]uint32, error) {
	if id == 0 {
		return nil, nil
	}
	if int(id) >= len(d.lists) {
		return nil, fmt.Errorf("%w: list id %d", ErrBadSection, id)
	}
	return d.lists[id], nil
}

func (d *decoder) strList(id uint32) ([]string, error) {
	ids, err := d.list(id)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		// A missing list is nil, not an empty one: the distinction is what
		// keeps a round trip byte-for-byte equal.
		return nil, nil
	}
	out := make([]string, len(ids))
	for i, x := range ids {
		if out[i], err = d.str(x); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *decoder) pairList(id uint32) ([][2]string, error) {
	if id == 0 {
		return nil, nil
	}
	if int(id) >= len(d.pairLists) {
		return nil, fmt.Errorf("%w: pair list id %d", ErrBadSection, id)
	}
	ids := d.pairLists[id]
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([][2]string, len(ids))
	for i, x := range ids {
		if int(x) >= len(d.pairs) {
			return nil, fmt.Errorf("%w: pair id %d", ErrBadSection, x)
		}
		a, err := d.str(d.pairs[x][0])
		if err != nil {
			return nil, err
		}
		b, err := d.str(d.pairs[x][1])
		if err != nil {
			return nil, err
		}
		out[i] = [2]string{a, b}
	}
	return out, nil
}

// Load decodes the whole snapshot.
func (f *File) Load() (*Snapshot, error) {
	snap := &Snapshot{Meta: Meta{}}
	if err := f.loadTables(); err != nil {
		return nil, err
	}
	for _, s := range []struct {
		name string
		id   uint8
		fn   func(*byteReader, *decoder, *Snapshot) error
	}{
		{"meta", SecMeta, readMeta},
		{"stats", SecStats, readStats},
		{"links", SecLinks, readLinks},
		{"groups", SecGroups, readGroups},
		{"endpoints", SecEndpoints, readEndpoints},
		{"observations", SecObservations, readObservations},
		{"params", SecParams, readParams},
		{"emulation", SecEmulation, readEmulation},
		{"edges", SecEdges, readEdges},
	} {
		if !f.Has(s.id) {
			continue
		}
		body, err := f.section(s.id)
		if err != nil {
			return nil, err
		}
		if err := s.fn(&byteReader{buf: body}, f.dec, snap); err != nil {
			return nil, fmt.Errorf("%s section: %w", s.name, err)
		}
	}
	snap.projectRelations()
	return snap, nil
}

// projectRelations turns the edge graph back into the fields that were derived
// from it when the file was written. "Found on" is stored as a relation rather
// than as a field, because several pages can point at the same link and a
// single field can only hold one answer; filling it back here keeps a decoded
// []linker.Link indistinguishable from the one the scan produced.
func (s *Snapshot) projectRelations() {
	if s.Links == nil {
		return
	}
	seen := make([]bool, len(s.Links))
	for _, e := range s.Edges {
		if e.Kind != EdgeSource {
			continue
		}
		if int(e.To) >= len(s.Links) || int(e.From) >= len(s.Links) || seen[e.To] {
			continue
		}
		seen[e.To] = true
		s.Links[e.To].SourceURL = LinkKey(&s.Links[e.From])
	}
}

// Outgoing returns the targets of every edge of a kind leaving one node.
func (s *Snapshot) Outgoing(kind uint8, from int) []Edge {
	var out []Edge
	for _, e := range s.Edges {
		if e.Kind == kind && int(e.From) == from {
			out = append(out, e)
		}
	}
	return out
}

// OutLinks returns the links a node points at through a relation of a kind.
// Edges whose target is not a link node are skipped, which is how a pattern
// edge to a filtered-away member is distinguished from a real one.
func (s *Snapshot) OutLinks(kind uint8, from int) []linker.Link {
	var out []linker.Link
	for _, e := range s.Edges {
		if e.Kind != kind || int(e.From) != from {
			continue
		}
		if int(e.To) < len(s.Links) {
			out = append(out, s.Links[e.To])
		}
	}
	return out
}

// NodeIndex maps a URL to its position in Snapshot.Links.
func (s *Snapshot) NodeIndex() map[string]int {
	idx := make(map[string]int, len(s.Links))
	for i := range s.Links {
		if _, dup := idx[LinkKey(&s.Links[i])]; !dup {
			idx[LinkKey(&s.Links[i])] = i
		}
	}
	return idx
}

func (f *File) loadTables() error {
	if f.dec != nil {
		return nil
	}
	strs, err := f.section(SecStrings)
	if err != nil {
		return err
	}
	dicts, err := f.section(SecDicts)
	if err != nil {
		return err
	}

	dec := &decoder{}
	r := &byteReader{buf: strs}
	n, err := r.count(1 << 28)
	if err != nil {
		return fmt.Errorf("strings: %w", err)
	}
	dec.strs = make([]string, n)
	prev := ""
	for i := 0; i < n; i++ {
		shared, err := r.uvarint()
		if err != nil {
			return fmt.Errorf("strings: %w", err)
		}
		suffix, err := r.rawString()
		if err != nil {
			return fmt.Errorf("strings: %w", err)
		}
		if int(shared) > len(prev) {
			return fmt.Errorf("strings: %w: prefix past previous", ErrBadSection)
		}
		dec.strs[i] = prev[:shared] + suffix
		prev = dec.strs[i]
	}

	dr := &byteReader{buf: dicts}
	limit := 1 << 28
	if cnt, err := dr.count(limit); err != nil {
		return fmt.Errorf("dicts: %w", err)
	} else {
		// Slot zero is the "no list" entry, so real ids start at one.
		dec.lists = make([][]uint32, cnt+1)
		for i := 0; i < cnt; i++ {
			m, err := dr.count(limit)
			if err != nil {
				return fmt.Errorf("dicts: %w", err)
			}
			ids := make([]uint32, m)
			var prev int64
			for j := 0; j < m; j++ {
				delta, err := dr.intvarint()
				if err != nil {
					return fmt.Errorf("dicts: %w", err)
				}
				prev += delta
				if prev < 0 || int(prev) >= len(dec.strs) {
					return fmt.Errorf("dicts: %w: string id %d", ErrBadSection, prev)
				}
				ids[j] = uint32(prev)
			}
			dec.lists[i+1] = ids
		}
	}
	if cnt, err := dr.count(limit); err != nil {
		return fmt.Errorf("dicts: %w", err)
	} else {
		dec.pairs = make([][2]uint32, cnt)
		var pa, pb int64
		for i := 0; i < cnt; i++ {
			da, err := dr.intvarint()
			if err != nil {
				return fmt.Errorf("dicts: %w", err)
			}
			db, err := dr.intvarint()
			if err != nil {
				return fmt.Errorf("dicts: %w", err)
			}
			pa += da
			pb += db
			if pa < 0 || pb < 0 || int(pa) >= len(dec.strs) || int(pb) >= len(dec.strs) {
				return fmt.Errorf("dicts: %w: pair out of range", ErrBadSection)
			}
			dec.pairs[i] = [2]uint32{uint32(pa), uint32(pb)}
		}
	}
	if cnt, err := dr.count(limit); err != nil {
		return fmt.Errorf("dicts: %w", err)
	} else {
		dec.pairLists = make([][]uint32, cnt+1)
		for i := 0; i < cnt; i++ {
			m, err := dr.count(limit)
			if err != nil {
				return fmt.Errorf("dicts: %w", err)
			}
			ids := make([]uint32, m)
			var prev int64
			for j := 0; j < m; j++ {
				delta, err := dr.intvarint()
				if err != nil {
					return fmt.Errorf("dicts: %w", err)
				}
				prev += delta
				if prev < 0 || int(prev) >= len(dec.pairs) {
					return fmt.Errorf("dicts: %w: pair id %d", ErrBadSection, prev)
				}
				ids[j] = uint32(prev)
			}
			dec.pairLists[i+1] = ids
		}
	}
	f.dec = dec
	return nil
}

func readMeta(r *byteReader, d *decoder, snap *Snapshot) error {
	n, err := r.count(len(r.buf) + 1)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		k, err := r.uvarint()
		if err != nil {
			return err
		}
		v, err := r.uvarint()
		if err != nil {
			return err
		}
		key, err := d.str(uint32(k))
		if err != nil {
			return err
		}
		val, err := d.str(uint32(v))
		if err != nil {
			return err
		}
		snap.Meta[key] = val
	}
	return nil
}

func readStats(r *byteReader, d *decoder, snap *Snapshot) error {
	s := &snap.Stats
	s.ByCategory = map[linker.Category]int{}
	s.ByLinkType = map[linker.LinkType]int{}
	s.ByClass = map[linker.URLClass]int{}
	s.ByTag = map[string]int{}

	get := func(what string) (uint64, error) {
		v, err := r.uvarint()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", what, err)
		}
		return v, nil
	}
	var err error
	if s.Total, err = getInt(r, "total"); err != nil {
		return err
	}
	if s.Resolved, err = getInt(r, "resolved"); err != nil {
		return err
	}
	if s.Unresolved, err = getInt(r, "unresolved"); err != nil {
		return err
	}
	if s.WithParams, err = getInt(r, "withParams"); err != nil {
		return err
	}
	cnt, err := get("category count")
	if err != nil {
		return err
	}
	for i := uint64(0); i < cnt; i++ {
		v, err := get("category")
		if err != nil {
			return err
		}
		s.ByCategory[linker.Category(i)] = int(v)
	}
	cnt, err = get("link type count")
	if err != nil {
		return err
	}
	for i := uint64(0); i < cnt; i++ {
		v, err := get("link type")
		if err != nil {
			return err
		}
		s.ByLinkType[linker.LinkType(i)] = int(v)
	}
	cnt, err = get("class count")
	if err != nil {
		return err
	}
	for i := uint64(0); i < cnt; i++ {
		v, err := get("class")
		if err != nil {
			return err
		}
		s.ByClass[linker.URLClass(i)] = int(v)
	}
	cnt, err = get("tag count")
	if err != nil {
		return err
	}
	for i := uint64(0); i < cnt; i++ {
		tid, err := get("tag")
		if err != nil {
			return err
		}
		v, err := get("tag count value")
		if err != nil {
			return err
		}
		tag, err := d.str(uint32(tid))
		if err != nil {
			return err
		}
		s.ByTag[tag] = int(v)
	}
	return nil
}

func getInt(r *byteReader, what string) (int, error) {
	v, err := r.uvarint()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	if v > 1<<40 {
		return 0, fmt.Errorf("%s: %w: %d", what, ErrBadSection, v)
	}
	return int(v), nil
}

func readLinks(r *byteReader, d *decoder, snap *Snapshot) error {
	n, err := r.count(len(r.buf) + 1)
	if err != nil {
		return err
	}
	links := make([]linker.Link, 0, n)
	for i := 0; i < n; i++ {
		flags, err := r.byteAt()
		if err != nil {
			return err
		}
		readID := func(what string) (string, error) {
			id, err := r.uvarint()
			if err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			s, err := d.str(uint32(id))
			if err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			return s, nil
		}
		href, err := readID("href")
		if err != nil {
			return err
		}
		resolved, err := readID("resolved")
		if err != nil {
			return err
		}
		domain, err := readID("domain")
		if err != nil {
			return err
		}
		cat, err := r.uvarint()
		if err != nil {
			return err
		}
		lt, err := r.uvarint()
		if err != nil {
			return err
		}
		cl, err := r.uvarint()
		if err != nil {
			return err
		}
		depth, err := r.uvarint()
		if err != nil {
			return err
		}
		l := linker.Link{
			HREF:        href,
			Resolved:    resolved,
			Domain:      domain,
			Category:    linker.Category(cat),
			LinkType:    linker.LinkType(lt),
			Class:       linker.URLClass(cl),
			HasParams:   flags&lfHasParams != 0,
			Depth:       int(depth),
			Synthesized: flags&lfSynthesized != 0,
		}
		if flags&lfHasTag != 0 {
			if l.Tag, err = readID("tag"); err != nil {
				return err
			}
		}
		if flags&lfHasVarSet != 0 {
			m, err := r.count(len(r.buf) + 1)
			if err != nil {
				return err
			}
			for j := 0; j < m; j++ {
				id, err := r.uvarint()
				if err != nil {
					return err
				}
				qs, err := d.strList(uint32(id))
				if err != nil {
					return err
				}
				for _, q := range qs {
					l.ParamVariants = append(l.ParamVariants, linker.ParamVariant{Query: q})
				}
			}
		}
		if flags&lfHasDetails != 0 {
			id, err := r.uvarint()
			if err != nil {
				return err
			}
			pairs, err := d.pairList(uint32(id))
			if err != nil {
				return err
			}
			// The argument lists follow the pairs, in the same order, so the
			// i-th detail's arguments are the i-th entry.
			argsID, err := r.uvarint()
			if err != nil {
				return err
			}
			args, err := d.strList(uint32(argsID))
			if err != nil {
				return err
			}
			if len(args) != len(pairs) {
				return fmt.Errorf("wmse: link %d: %d api details but %d argument lists", i, len(pairs), len(args))
			}
			for j, p := range pairs {
				l.APIDetails = append(l.APIDetails, linker.APIDetail{
					MatchSource: p[0], HTTPMethod: p[1], Arguments: args[j],
				})
			}
		}
		if flags&lfHasPage != 0 {
			pd, err := r.uvarint()
			if err != nil {
				return err
			}
			ct, err := readID("content type")
			if err != nil {
				return err
			}
			pl, err := r.uvarint()
			if err != nil {
				return err
			}
			snap.Pages = append(snap.Pages, Page{
				URL:         LinkKey(&l),
				Depth:       int(pd),
				ContentType: ct,
				Links:       int(pl),
			})
		}
		links = append(links, l)
	}
	snap.Links = links
	return nil
}

func readGroups(r *byteReader, d *decoder, snap *Snapshot) error {
	n, err := r.count(len(r.buf) + 1)
	if err != nil {
		return err
	}
	groups := make([]Group, 0, n)
	for i := 0; i < n; i++ {
		readID := func(what string) (string, error) {
			id, err := r.uvarint()
			if err != nil {
				return "", err
			}
			return d.str(uint32(id))
		}
		pattern, err := readID("pattern")
		if err != nil {
			return err
		}
		domain, err := readID("domain")
		if err != nil {
			return err
		}
		count, err := getInt(r, "group count")
		if err != nil {
			return err
		}
		nv, err := r.count(len(r.buf) + 1)
		if err != nil {
			return err
		}
		g := Group{Pattern: pattern, Domain: domain, Count: count}
		for j := 0; j < nv; j++ {
			kindID, err := r.uvarint()
			if err != nil {
				return err
			}
			valsID, err := r.uvarint()
			if err != nil {
				return err
			}
			kind, err := d.str(uint32(kindID))
			if err != nil {
				return err
			}
			vals, err := d.strList(uint32(valsID))
			if err != nil {
				return err
			}
			nn, err := r.count(len(r.buf) + 1)
			if err != nil {
				return err
			}
			var nums []int64
			if nn > 0 {
				nums = make([]int64, 0, nn)
			}
			var prev int64
			for k := 0; k < nn; k++ {
				delta, err := r.intvarint()
				if err != nil {
					return err
				}
				prev += delta
				nums = append(nums, prev)
			}
			g.Vars = append(g.Vars, urlgroup.Var{Kind: kind, Values: vals, Nums: nums})
		}
		mid, err := r.uvarint()
		if err != nil {
			return err
		}
		members, err := d.strList(uint32(mid))
		if err != nil {
			return err
		}
		g.Members = members
		groups = append(groups, g)
	}
	snap.Groups = groups
	return nil
}

func readFields(r *byteReader, d *decoder) ([]contract.Field, error) {
	n, err := r.count(1 << 24)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]contract.Field, 0, n)
	for i := 0; i < n; i++ {
		flags, err := r.byteAt()
		if err != nil {
			return nil, err
		}
		nameID, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		kindID, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		valsID, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		name, err := d.str(uint32(nameID))
		if err != nil {
			return nil, err
		}
		kind, err := d.str(uint32(kindID))
		if err != nil {
			return nil, err
		}
		values, err := d.strList(uint32(valsID))
		if err != nil {
			return nil, err
		}
		f := contract.Field{
			Name:     name,
			Kind:     kind,
			Values:   values,
			Constant: flags&ffConstant != 0,
			Inferred: flags&ffInferred != 0,
			Nullable: flags&ffNullable != 0,
		}
		if flags&ffHasConst != 0 {
			id, err := r.uvarint()
			if err != nil {
				return nil, err
			}
			if f.ConstVal, err = d.str(uint32(id)); err != nil {
				return nil, err
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func readEndpoints(r *byteReader, d *decoder, snap *Snapshot) error {
	n, err := r.count(len(r.buf) + 1)
	if err != nil {
		return err
	}
	eps := make([]contract.Endpoint, 0, n)
	for i := 0; i < n; i++ {
		flags, err := r.byteAt()
		if err != nil {
			return err
		}
		readID := func(what string) (string, error) {
			id, err := r.uvarint()
			if err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			s, err := d.str(uint32(id))
			if err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			return s, nil
		}
		path, err := readID("path")
		if err != nil {
			return err
		}
		calls, err := getInt(r, "calls")
		if err != nil {
			return err
		}
		mid, err := r.uvarint()
		if err != nil {
			return err
		}
		methods, err := d.strList(uint32(mid))
		if err != nil {
			return err
		}
		query, err := readFields(r, d)
		if err != nil {
			return err
		}
		headers, err := readFields(r, d)
		if err != nil {
			return err
		}
		nb, err := r.count(1 << 20)
		if err != nil {
			return err
		}
		var bodies []contract.BodyFormat
		for j := 0; j < nb; j++ {
			kind, err := readID("body kind")
			if err != nil {
				return err
			}
			mime, err := readID("body mime")
			if err != nil {
				return err
			}
			bmid, err := r.uvarint()
			if err != nil {
				return err
			}
			bm, err := d.strList(uint32(bmid))
			if err != nil {
				return err
			}
			sample, err := readID("body sample")
			if err != nil {
				return err
			}
			fields, err := readFields(r, d)
			if err != nil {
				return err
			}
			bodies = append(bodies, contract.BodyFormat{
				Kind: kind, MIME: mime, Methods: bm, Sample: sample, Fields: fields,
			})
		}
		rid, err := r.uvarint()
		if err != nil {
			return err
		}
		respPairs, err := d.pairList(uint32(rid))
		if err != nil {
			return err
		}
		rawID, err := r.uvarint()
		if err != nil {
			return err
		}
		raw, err := d.strList(uint32(rawID))
		if err != nil {
			return err
		}
		ep := contract.Endpoint{
			Path:           path,
			Methods:        methods,
			Query:          query,
			Headers:        headers,
			Bodies:         bodies,
			Calls:          calls,
			Unobserved:     flags&efUnobserved != 0,
			MethodInferred: flags&efInferred != 0,
			Raw:            raw,
		}
		for _, p := range respPairs {
			ep.Response = append(ep.Response, contract.ResponseField{Path: p[0], Kind: p[1]})
		}
		eps = append(eps, ep)
	}
	snap.Endpoints = eps
	return nil
}

func readObservations(r *byteReader, d *decoder, snap *Snapshot) error {
	n, err := r.count(len(r.buf) + 1)
	if err != nil {
		return err
	}
	obs := make([]contract.Observation, 0, n)
	for i := 0; i < n; i++ {
		readID := func(what string) (string, error) {
			id, err := r.uvarint()
			if err != nil {
				return "", err
			}
			return d.str(uint32(id))
		}
		url, err := readID("url")
		if err != nil {
			return err
		}
		method, err := readID("method")
		if err != nil {
			return err
		}
		flags, err := r.byteAt()
		if err != nil {
			return err
		}
		body, err := readID("body")
		if err != nil {
			return err
		}
		hid, err := r.uvarint()
		if err != nil {
			return err
		}
		hp, err := d.pairList(uint32(hid))
		if err != nil {
			return err
		}
		rid, err := r.uvarint()
		if err != nil {
			return err
		}
		rp, err := d.pairList(uint32(rid))
		if err != nil {
			return err
		}
		iid, err := r.uvarint()
		if err != nil {
			return err
		}
		ip, err := d.pairList(uint32(iid))
		if err != nil {
			return err
		}
		o := contract.Observation{
			URL:            url,
			Method:         method,
			Body:           body,
			EndpointOnly:   flags&ofEndpointOnly != 0,
			MethodInferred: flags&ofInferred != 0,
		}
		for _, p := range hp {
			o.Headers = append(o.Headers, contract.NameValue{Name: p[0], Value: p[1]})
		}
		for _, p := range rp {
			o.ResponseFields = append(o.ResponseFields, contract.ResponseField{Path: p[0], Kind: p[1]})
		}
		for _, p := range ip {
			o.InferredQuery = append(o.InferredQuery, contract.NameValue{Name: p[0], Value: p[1]})
		}
		obs = append(obs, o)
	}
	snap.Observations = obs
	return nil
}

func readParams(r *byteReader, d *decoder, snap *Snapshot) error {
	n, err := r.count(len(r.buf) + 1)
	if err != nil {
		return err
	}
	params := make([]Param, 0, n)
	for i := 0; i < n; i++ {
		readID := func(what string) (string, error) {
			id, err := r.uvarint()
			if err != nil {
				return "", err
			}
			return d.str(uint32(id))
		}
		name, err := readID("param name")
		if err != nil {
			return err
		}
		carrier, err := readID("carrier")
		if err != nil {
			return err
		}
		kind, err := r.uvarint()
		if err != nil {
			return err
		}
		owner, err := r.uvarint()
		if err != nil {
			return err
		}
		count, err := getInt(r, "param count")
		if err != nil {
			return err
		}
		eid, err := r.uvarint()
		if err != nil {
			return err
		}
		endpoints, err := d.strList(uint32(eid))
		if err != nil {
			return err
		}
		did, err := r.uvarint()
		if err != nil {
			return err
		}
		docs, err := d.strList(uint32(did))
		if err != nil {
			return err
		}
		params = append(params, Param{
			ParamRef: linker.ParamRef{
				Endpoints: endpoints,
				Carrier:   carrier,
				Name:      name,
				Kind:      linker.ParamKind(kind),
				Owner:     linker.ParamOwner(owner),
				Count:     count,
			},
			Docs: docs,
		})
	}
	snap.Params = params
	return nil
}

func readEmulation(r *byteReader, d *decoder, snap *Snapshot) error {
	present, err := getInt(r, "emulation present")
	if err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	em := &Emulation{ByType: map[string]int{}}
	if em.Scripts, err = getInt(r, "scripts"); err != nil {
		return err
	}
	if em.Calls, err = getInt(r, "calls"); err != nil {
		return err
	}
	ab, err := r.uvarint()
	if err != nil {
		return err
	}
	em.Abandoned = int64(ab)
	flags, err := r.byteAt()
	if err != nil {
		return err
	}
	em.Disabled = flags&1 != 0

	nt, err := r.count(1 << 20)
	if err != nil {
		return err
	}
	for i := 0; i < nt; i++ {
		tid, err := r.uvarint()
		if err != nil {
			return err
		}
		c, err := getInt(r, "call type count")
		if err != nil {
			return err
		}
		name, err := d.str(uint32(tid))
		if err != nil {
			return err
		}
		em.ByType[name] = c
	}
	ne, err := r.count(1 << 22)
	if err != nil {
		return err
	}
	for i := 0; i < ne; i++ {
		id, err := r.uvarint()
		if err != nil {
			return err
		}
		s, err := d.str(uint32(id))
		if err != nil {
			return err
		}
		em.Errors = append(em.Errors, s)
	}
	nc, err := r.count(len(r.buf) + 1)
	if err != nil {
		return err
	}
	em.List = make([]Call, 0, nc)
	for i := 0; i < nc; i++ {
		readID := func() (string, error) {
			id, err := r.uvarint()
			if err != nil {
				return "", err
			}
			return d.str(uint32(id))
		}
		var c Call
		if c.URL, err = readID(); err != nil {
			return err
		}
		if c.RawURL, err = readID(); err != nil {
			return err
		}
		if c.Method, err = readID(); err != nil {
			return err
		}
		if c.Initiator, err = readID(); err != nil {
			return err
		}
		if c.Type, err = readID(); err != nil {
			return err
		}
		if c.Body, err = readID(); err != nil {
			return err
		}
		hid, err := r.uvarint()
		if err != nil {
			return err
		}
		hp, err := d.pairList(uint32(hid))
		if err != nil {
			return err
		}
		for _, p := range hp {
			c.Headers = append(c.Headers, [2]string{p[0], p[1]})
		}
		em.List = append(em.List, c)
	}
	snap.Emulation = em
	return nil
}

func readEdges(r *byteReader, d *decoder, snap *Snapshot) error {
	nk, err := r.count(EdgeCount)
	if err != nil {
		return err
	}
	var edges []Edge
	for i := 0; i < nk; i++ {
		kindID, err := r.uvarint()
		if err != nil {
			return err
		}
		kind := uint8(kindID)
		if kind == 0 || int(kind) >= EdgeCount {
			return fmt.Errorf("%w: edge kind %d", ErrBadSection, kind)
		}
		nr, err := r.count(len(r.buf) + 1)
		if err != nil {
			return err
		}
		var prevFrom int64
		for j := 0; j < nr; j++ {
			df, err := r.intvarint()
			if err != nil {
				return err
			}
			prevFrom += df
			if prevFrom < 0 || prevFrom > 1<<32 {
				return fmt.Errorf("%w: edge source %d", ErrBadSection, prevFrom)
			}
			weight, err := r.uvarint()
			if err != nil {
				return err
			}
			nt, err := r.count(len(r.buf) + 1)
			if err != nil {
				return err
			}
			var prevTo int64
			for k := 0; k < nt; k++ {
				dt, err := r.intvarint()
				if err != nil {
					return err
				}
				prevTo += dt
				if prevTo < 0 || prevTo > 1<<32 {
					return fmt.Errorf("%w: edge target %d", ErrBadSection, prevTo)
				}
				edges = append(edges, Edge{
					Kind:   kind,
					From:   uint32(prevFrom),
					To:     uint32(prevTo),
					Weight: uint32(weight),
				})
			}
		}
	}
	snap.Edges = edges
	return nil
}
