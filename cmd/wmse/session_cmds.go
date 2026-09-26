package main

import (
	"errors"
	"flag"
	"fmt"
	"path"
	"sort"
	"strings"

	"apimap/internal/color"
	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// field returns the URL of a link node the way the session prints it: the path,
// with the host shown when it is not the current target's, because two paths on
// two hosts are two places.
func (s *session) field(full string) string {
	if full == "" {
		return ""
	}
	if s.target != "" && sameHost(full, s.target) {
		return pathOf(full)
	}
	return full
}

func pathOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j:]
		}
		return "/"
	}
	return u
}

// pathOnly is the path with the query and fragment removed. It is what makes
// "what /complex/9223/contacts" find the link the scan recorded as
// "/complex/9223/contacts?sort=asc": the two are the same resource, and a person
// naming a path does not remember which query it was reached with.
func pathOnly(u string) string {
	p := pathOf(u)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "/"
	}
	return p
}

// ---------- ls and cd ----------

// cmdCd moves the session to a path. A page is preferred over a directory, so a
// path with a document at it becomes that document, and only a path without one
// becomes a directory - which is the mode `ls` then follows.
//
// With no path it returns to the scan's target, so there is always a way back to
// the beginning without typing it.
func (s *session) cmdCd(args []string) {
	raw := s.target
	if len(args) > 0 && args[0] != "" {
		raw = args[0]
	}
	p := s.placeAt(s.resolve(raw))
	s.pos, s.dir = p.url, p.dir
	fmt.Fprintf(s.out, "  %s %s\n", s.pos, s.dim(s.placesAs()))
}

// cmdLs lists what is at a place. A page has links and a directory has children,
// and the session knows which it is standing at, so `ls` never has to be told
// which question is being asked.
//
// A path argument lists that place and leaves the session where it was, which is
// what `ls` does in a shell: moving is `cd`'s job, and one command that does both
// is one command whose result is hard to predict. `ls -d` is the exception, and
// deliberately so: looking at a path as a directory is a decision about where to
// stand, so it moves.
func (s *session) cmdLs(args []string) {
	dir := false
	var rest []string
	for _, a := range args {
		switch a {
		case "-d", "--dir":
			dir = true
		default:
			rest = append(rest, a)
		}
	}
	target := s.pos
	if len(rest) > 0 && rest[0] != "" {
		target = s.resolve(rest[0])
	}
	if dir {
		target = strings.TrimSuffix(target, "/")
		if target == "" {
			target = "/"
		}
		s.pos, s.dir = target, true
		s.listDir(s.pos)
		return
	}
	p := s.placeAt(target)
	if p.dir {
		s.listDir(p.url)
		return
	}
	s.listPage(p.url)
}

// listPage lists the links found on a page. The file records, for every link,
// the page it was discovered on, so this is that page's own outgoing list and not
// a re-derivation from URL shapes. The links a page does not have are not
// guessed at: a page the crawl never fetched has no recorded links, and saying so
// is more useful than showing the site's other pages as if they were its.
func (s *session) listPage(url string) {
	children := s.childrenOf(url)
	fmt.Fprintf(s.out, "\n  %s %s\n", s.bold(s.field(url)), s.dim("("+plural(len(children), "link")+" on this page)"))
	if len(children) == 0 {
		if s.ensurePageIndex()[url] == nil {
			fmt.Fprintf(s.out, "  %s\n", s.dim("the crawl did not fetch this page, so no links on it were recorded"))
		} else {
			fmt.Fprintf(s.out, "  %s\n", s.dim("no links were found on this page"))
		}
		return
	}
	for _, l := range children {
		fmt.Fprintf(s.out, "%s\n", s.rowFor(l))
	}
}

// listDir lists the paths under a directory. Everything under it is listed, at
// any depth, because a directory is not a level but a prefix: the paths that
// match it are the answer, and stopping at one level would hide what is two deep.
func (s *session) listDir(url string) {
	prefix := strings.TrimSuffix(pathOnly(url), "/")
	if prefix == "" {
		prefix = "/"
	}
	rows := s.underPrefix(prefix)
	fmt.Fprintf(s.out, "\n  %s %s\n", s.bold(prefix), s.dim("("+plural(len(rows), "path")+")"))
	if len(rows) == 0 {
		fmt.Fprintf(s.out, "  %s\n", s.dim("nothing in the file has this path"))
		return
	}
	for _, r := range rows {
		fmt.Fprintf(s.out, "  %s\n", s.field(r))
	}
}

// childrenOf returns the links discovered on a page, from the file's own
// "found on" relation. The relation is the record, so it is what is read; a link
// that carries the page in its own field is included too, so that a file written
// before the relation existed still answers.
func (s *session) childrenOf(url string) []*linker.Link {
	byPath := pathOnly(url)
	seen := map[string]bool{}
	var out []*linker.Link
	appendIf := func(l *linker.Link) {
		key := wmse.LinkKey(l)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, l)
	}
	if node := s.nodeIndex(url); node >= 0 {
		for _, e := range s.merged.Edges {
			if e.Kind != wmse.EdgeSource || int(e.From) != node {
				continue
			}
			if int(e.To) < len(s.merged.Links) {
				appendIf(&s.merged.Links[e.To])
			}
		}
	}
	for i := range s.merged.Links {
		l := &s.merged.Links[i]
		if l.Synthesized {
			continue
		}
		src := l.SourceURL
		if src == "" {
			continue
		}
		if src == url || pathOnly(src) == byPath {
			appendIf(l)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return wmse.LinkKey(out[i]) < wmse.LinkKey(out[j])
	})
	return out
}

// underPrefix returns the paths at or below a prefix, sorted.
func (s *session) underPrefix(prefix string) []string {
	prefix = pathOnly(prefix)
	exact := strings.TrimSuffix(prefix, "/")
	if exact == "" {
		exact = "/"
	}
	prefix = exact + "/"
	var out []string
	for i := range s.merged.Links {
		l := &s.merged.Links[i]
		if l.Synthesized {
			continue
		}
		full := wmse.LinkKey(l)
		if p := pathOnly(full); p == exact || strings.HasPrefix(p, prefix) {
			out = append(out, full)
		}
	}
	sort.Strings(out)
	return out
}

// rowFor renders one link in the scan's table columns.
//
// A domain longer than its column is cut with an ellipsis rather than left long,
// for the reason the report's own table gives: a value wider than its column
// pushes everything after it sideways, and a table whose rows do not line up is
// harder to read than one missing three characters. The row still carries the
// whole URL, so nothing is lost that the row was for.
func (s *session) rowFor(l *linker.Link) string {
	cat := l.Category.String()
	if l.Tag != "" {
		cat = l.Tag
	}
	line := fmt.Sprintf("  %-9s %s %s %s", l.LinkType.String(),
		s.column(nil, trunc(cat, 12), 12),
		s.column(s.dim, trunc(l.Domain, 24), 24), s.field(wmse.LinkKey(l)))
	if l.HasParams {
		line += " " + s.dim("(params)")
	}
	if l.Class != linker.ClassNormal {
		line += " " + s.dim("["+l.Class.String()+"]")
	}
	if l.Depth > 0 {
		line += " " + s.dim(fmt.Sprintf("[d%d]", l.Depth))
	}
	return line
}

// ---------- what ----------

// cmdWhat identifies a resource: what kind of thing it is, how it was
// classified, and where it was seen. It is the "I have a URL, what is it?"
// question, which the link table answers only if you already know to look.
func (s *session) cmdWhat(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.usage("what <path>")
		return
	}
	full := s.resolve(args[0])
	l, ok := s.nodeFor(full)
	if !ok {
		s.notFound(full)
		return
	}
	fmt.Fprintf(s.out, "\n  %s\n", s.bold(s.field(full)))
	fmt.Fprintf(s.out, "    kind      %s\n", l.Category.String())
	if l.Category == linker.CategoryWebAsset {
		fmt.Fprintf(s.out, "    asset     %s\n", color.AssetLabel(color.ClassifyAsset(l.HREF)))
	}
	if l.Class != linker.ClassNormal {
		fmt.Fprintf(s.out, "    class     %s\n", l.Class.String())
	}
	if l.LinkType == linker.LinkTypeRelative && l.Resolved != "" {
		fmt.Fprintf(s.out, "    resolves  %s\n", l.Resolved)
	}
	if l.Depth > 0 {
		fmt.Fprintf(s.out, "    found at  depth %d, on %s\n", l.Depth, s.field(l.SourceURL))
	} else if l.SourceURL != "" {
		fmt.Fprintf(s.out, "    found on  %s\n", s.field(l.SourceURL))
	}
	if p := s.ensurePageIndex()[full]; p != nil {
		fmt.Fprintf(s.out, "    fetched   %s", p.ContentType)
		if p.Links > 0 {
			fmt.Fprintf(s.out, " (%d links)", p.Links)
		}
		fmt.Fprintln(s.out)
	}
	if l.HasParams {
		fmt.Fprintf(s.out, "    params    yes\n")
	}
	// The contract and the references are the two things that make a resource
	// interesting, so name them even though the commands below print them.
	if s.contractFor(full) != nil {
		fmt.Fprintf(s.out, "    %s\n", s.dim("has a contract: try `api "+s.field(full)+"`"))
	}
	if parents := s.parentsOf(full); len(parents) > 0 {
		fmt.Fprintf(s.out, "    %s\n", s.dim(fmt.Sprintf("referenced by %d page(s): try `from %s`", len(parents), s.field(full))))
	}
	if n := s.refCountFor(full); n > 0 {
		fmt.Fprintf(s.out, "    %s\n", s.dim(fmt.Sprintf("found at %d place(s): try `refs %s`", n, s.field(full))))
	}
}

// pageIndex is built once per session so `what` can answer whether a resource
// was actually fetched without walking the page list each time.
type pageIndex map[string]*wmse.Page

func (s *session) ensurePageIndex() pageIndex {
	if s.pages == nil {
		s.pages = make(pageIndex, len(s.merged.Pages))
		for i := range s.merged.Pages {
			p := &s.merged.Pages[i]
			if _, dup := s.pages[p.URL]; !dup {
				s.pages[p.URL] = p
			}
		}
	}
	return s.pages
}

// ---------- api ----------

// cmdApi describes an API endpoint from the contract the scan inferred, or says
// precisely why there is none. The four refusals are distinct on purpose:
// "not found", "not an API", "no contract in the file" and "a contract but no
// captured request" are four different facts about the resource, and a person
// reading one of them learns something.
func (s *session) cmdAPI(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.usage("api [path]")
		return
	}
	full := s.resolve(args[0])
	// A contract can exist without a link node: a URL recovered from a bundle is
	// a finding even when the crawl never recorded it as a link. So the contract
	// is looked up first, and the link's category is only consulted to explain
	// the absence.
	ep := s.contractFor(full)
	l, hasLink := s.nodeFor(full)
	if ep == nil {
		if !hasLink {
			s.notFound(full)
			return
		}
		if l.Category != linker.CategoryAPI {
			fmt.Fprintf(s.out, "  %s is a %s, not an API\n", s.field(full), l.Category.String())
			return
		}
		fmt.Fprintf(s.out, "  %s is an API, but no contract for it is in this file\n", s.field(full))
		fmt.Fprintf(s.out, "  %s\n", s.dim("(the scan recorded the link; no request to it was captured)"))
		return
	}
	fmt.Fprintf(s.out, "\n%s\n", s.bold(ep.Path))
	renderEndpoint(s, ep)
	if obs := s.observationsFor(full); len(obs) == 0 {
		fmt.Fprintf(s.out, "\n  %s\n", s.dim("no request was captured for this endpoint: the contract is inference only"))
	} else {
		fmt.Fprintf(s.out, "\n  %s\n", s.dim(plural(len(obs), "captured request")+":"))
		for _, ob := range obs {
			fmt.Fprintf(s.out, "    %s %s\n", ob.Method, ob.URL)
		}
	}
}

// renderEndpoint prints a contract the way the scan's contract section does.
func renderEndpoint(s *session, ep *contract.Endpoint) {
	methods := strings.Join(ep.Methods, ", ")
	if ep.MethodInferred && methods != "" {
		methods += " (inferred)"
	}
	if methods == "" {
		methods = "ANY"
	}
	fmt.Fprintf(s.out, "  methods: %s\n", methods)
	if ep.Unobserved {
		fmt.Fprintf(s.out, "  %s\n", s.dim("seen as a path in code only: no request to it was captured"))
	}
	fmt.Fprintf(s.out, "  calls:   %d\n", ep.Calls)
	renderFields(s, "query", ep.Query)
	renderFields(s, "headers", ep.Headers)
	for _, b := range ep.Bodies {
		kind := b.Kind
		if b.MIME != "" {
			kind += " " + b.MIME
		}
		fmt.Fprintf(s.out, "  bodies:\n    %s\n", kind)
		renderFields(s, "", b.Fields)
	}
	if len(ep.Response) > 0 {
		fmt.Fprintf(s.out, "  returns:\n")
		for _, f := range ep.Response {
			fmt.Fprintf(s.out, "    %-24s %s\n", f.Path, f.Kind)
		}
	}
}

func renderFields(s *session, label string, fields []contract.Field) {
	if len(fields) == 0 {
		return
	}
	if label != "" {
		fmt.Fprintf(s.out, "  %s:\n", label)
	}
	for _, f := range fields {
		fmt.Fprintf(s.out, "    %-24s %s\n", f.Name, fieldKind(f))
	}
}

func fieldKind(f contract.Field) string {
	s := f.Kind
	if f.Constant {
		if f.ConstVal != "" {
			s += " " + f.ConstVal + " (const)"
		} else {
			s += " (const)"
		}
		return s
	}
	if len(f.Values) > 0 {
		return s + " e.g. " + strings.Join(f.Values, ", ")
	}
	return s
}

// contractFor finds the contract for a URL. The relation is stored as an edge
// from the link to the endpoint, so the lookup is a walk of those edges; when
// the link's URL does not match a node exactly - because it carries a query the
// path does not - it falls back to matching the endpoint's path, which is the
// thing the link resolves to and is not a guess.
func (s *session) contractFor(full string) *contract.Endpoint {
	if node := s.nodeIndex(full); node >= 0 {
		for _, e := range s.merged.Edges {
			if e.Kind == wmse.EdgeContract && int(e.From) == node {
				if int(e.To) < len(s.merged.Endpoints) {
					return &s.merged.Endpoints[e.To]
				}
			}
		}
	}
	byPath := pathOnly(full)
	for i := range s.merged.Endpoints {
		ep := &s.merged.Endpoints[i]
		if pathOnly(s.resolve(ep.Path)) == byPath {
			return ep
		}
	}
	return nil
}

func (s *session) observationsFor(full string) []contract.Observation {
	var out []contract.Observation
	for i := range s.merged.Observations {
		ob := &s.merged.Observations[i]
		if s.resolve(ob.URL) == full {
			out = append(out, *ob)
		}
	}
	return out
}

// ---------- from ----------

// cmdFrom answers "where was this referenced from": the pages that link to a
// resource. It is the reverse of the crawl and the question the link graph is
// best at, because "which page mentions /admin" is what a person actually needs
// once a scan has handed them a list of paths.
func (s *session) cmdFrom(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.usage("from <path>")
		return
	}
	full := s.resolve(args[0])
	if _, ok := s.nodeFor(full); !ok {
		s.notFound(full)
		return
	}
	parents := s.parentsOf(full)
	if len(parents) == 0 {
		fmt.Fprintf(s.out, "  %s is not referenced by any page in this file\n", s.field(full))
		if n := s.refCountFor(full); n > 0 {
			fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("it appears in %d reference file(s) though: try `refs %s`", n, s.field(full))))
		}
		return
	}
	fmt.Fprintf(s.out, "\n  %s %s\n", s.bold(s.field(full)), s.dim("is referenced by:"))
	for _, p := range parents {
		fmt.Fprintf(s.out, "    %s\n", s.field(p))
	}
}

// parentsOf returns the pages that reference a resource, from the "found on"
// relation. The relation is stored edge-wise, so this is the reverse of
// OutLinks(EdgeSource) and it is the file's own structure, not a re-derivation.
func (s *session) parentsOf(full string) []string {
	node := s.nodeIndex(full)
	if node < 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range s.merged.Edges {
		if e.Kind == wmse.EdgeSource && int(e.To) == node {
			if int(e.From) < len(s.merged.Links) {
				src := wmse.LinkKey(&s.merged.Links[e.From])
				if !seen[src] {
					seen[src] = true
					out = append(out, src)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// nodeIndex is the node id of a URL in the link array, or -1.
func (s *session) nodeIndex(full string) int {
	if s.nodes == nil {
		s.nodes = make(map[string]int, len(s.merged.Links))
		for i := range s.merged.Links {
			if _, dup := s.nodes[wmse.LinkKey(&s.merged.Links[i])]; !dup {
				s.nodes[wmse.LinkKey(&s.merged.Links[i])] = i
			}
		}
	}
	if i, ok := s.nodes[full]; ok {
		return i
	}
	return -1
}

// ---------- find ----------

// cmdFind searches by kind and glob. It is `find` in the unix sense: a kind
// filter and a shell-style pattern, printing the URLs that match one per line so
// they can be fed to another command.
func (s *session) cmdFind(args []string) {
	parts := args
	if len(parts) != 2 {
		s.usage("find <asset|html|css|image|api|js|any> <glob>")
		return
	}
	kind, glob := strings.ToLower(parts[0]), parts[1]
	matchKind := func(l *linker.Link) bool { return kindMatches(kind, l) }
	matchGlob := func(full string) bool { return globMatch(glob, full) }

	var found []string
	for i := range s.merged.Links {
		l := &s.merged.Links[i]
		if l.Synthesized {
			continue
		}
		full := wmse.LinkKey(l)
		if matchKind(l) && matchGlob(full) {
			found = append(found, full)
		}
	}
	// A pattern can also name a folded URL shape, so a search for /complex/{id}
	// finds the instances that pattern covers.
	if len(found) == 0 {
		for i := range s.merged.Groups {
			g := &s.merged.Groups[i]
			if kindMatchesGroup(kind, g) && globMatch(glob, g.Pattern) {
				found = append(found, g.Pattern)
			}
		}
	}
	sort.Strings(found)
	for _, f := range found {
		fmt.Fprintln(s.out, s.field(f))
	}
	if len(found) == 0 {
		fmt.Fprintf(s.out, "  %s\n", s.dim("no match for "+kind+" "+glob))
	}
}

// kindMatches reports whether a link is of the requested kind. The kinds are the
// tool's own vocabulary, so a search means the same thing the report means.
func kindMatches(kind string, l *linker.Link) bool {
	switch kind {
	case "any", "":
		return true
	case "api":
		return l.Category == linker.CategoryAPI
	case "html", "page":
		return l.Category == linker.CategoryWebPage
	case "asset":
		return l.Category == linker.CategoryWebAsset
	case "css", "js", "image", "img", "font", "doc", "media", "data":
		if l.Category != linker.CategoryWebAsset {
			return false
		}
		return kindToSubtype(kind) == color.ClassifyAsset(l.HREF)
	}
	return false
}

// kindToSubtype maps a find kind to the asset subtype it names.
//
// The subtypes are compared rather than the labels the report prints, because
// the labels are shortened for width - an image is printed "img" - and a search
// that compared against them would fail over a word the same tool prints on
// another line. Both spellings are accepted for the same reason.
func kindToSubtype(kind string) color.AssetSubtype {
	switch kind {
	case "css":
		return color.AssetCSS
	case "js":
		return color.AssetJS
	case "image", "img":
		return color.AssetImage
	case "font":
		return color.AssetFont
	case "doc":
		return color.AssetDoc
	case "media":
		return color.AssetMedia
	case "data":
		return color.AssetData
	}
	return color.AssetOther
}

func kindMatchesGroup(kind string, g *wmse.Group) bool {
	switch kind {
	case "any", "":
		return true
	case "api":
		return linker.MatchesAPIPattern(g.Pattern)
	}
	return false
}

// globMatch applies a shell-style pattern to a URL, trying it against the whole
// URL and against the path, so both "v*/user" and "example.com/v*/user" work
// without the person having to know which form the scanner recorded.
func globMatch(glob, full string) bool {
	p := pathOnly(full)
	if ok, _ := path.Match(glob, p); ok {
		return true
	}
	if ok, _ := path.Match(glob, full); ok {
		return true
	}
	// A pattern that does not start at the root should also match a suffix, so
	// "v*/user" finds "/api/v1/user" without spelling the leading star.
	if !strings.HasPrefix(glob, "/") {
		segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
		for i := range segs {
			if ok, _ := path.Match(glob, strings.Join(segs[i:], "/")); ok {
				return true
			}
		}
	}
	return false
}

// ---------- info ----------

// cmdInfo prints the header of every loaded file: which it is, what it scanned,
// and what it holds. With several files loaded (after extend) it prints each,
// because a session that queries across two scans should be able to see that it
// is.
func (s *session) cmdInfo(args []string) {
	for i, f := range s.files {
		if i > 0 {
			fmt.Fprintln(s.out)
		}
		snap := f.snap
		fmt.Fprintf(s.out, "%s  %s  (%s)\n", s.bold("WebMap snapshot"), f.path,
			s.dim(fmt.Sprintf("%s, %s", humanBytes(int64(f.info.TotalBytes)), plural(len(f.info.Sections), "section"))))
		for _, row := range [][2]string{
			{"target", snap.Meta[wmse.MetaTarget]},
			{"created", snap.Meta[wmse.MetaCreated]},
			{"scope", snap.Meta[wmse.MetaScope]},
			{"command", snap.Meta[wmse.MetaCommand]},
		} {
			if row[1] != "" {
				fmt.Fprintf(s.out, "  %-10s %s\n", row[0], row[1])
			}
		}
		fmt.Fprintf(s.out, "  %-10s %d links, %d pages, %d patterns, %d contracts, %d relations\n",
			"holds", len(snap.Links), len(snap.Pages), len(snap.Groups), len(snap.Endpoints), len(snap.Edges))
		if len(snap.ArchiveNames) > 0 {
			fmt.Fprintf(s.out, "  %-10s %s\n", "archive", archiveBrief(snap.ArchiveNames))
		}
		if i == 0 && len(s.files) > 1 {
			fmt.Fprintf(s.out, "  %s\n", s.dim("("+plural(len(s.files), "file")+" loaded; queries span all of them)"))
		}
	}
}

// ---------- extend ----------

// cmdExtend opens another saved scan and folds it into the session, so a question
// can be asked across two of them - the same site before and after a change, or
// a page and the bundle it loads. The merge unions the node sets and the
// relations; where two files describe the same URL the first one loaded wins,
// because there is no more reason to prefer the later file than the earlier one.
func (s *session) cmdExtend(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.usage("extend <file>")
		return
	}
	path := args[0]
	f, err := wmse.Open(path)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("cannot open "+path+": "+err.Error()))
		return
	}
	snap, err := f.Load()
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("cannot read "+path+": "+err.Error()))
		return
	}
	before := len(s.merged.Links)
	s.merged = mergeSnapshots(s.merged, snap)
	s.nodes = nil
	s.pages = nil
	s.pagePaths = nil
	// The completable paths are built from the merged data, so they have to go
	// with it: leaving them would complete to paths the file no longer holds.
	s.paths = nil
	s.files = append(s.files, loadedFile{path: path, info: f.Info(), snap: snap})
	fmt.Fprintf(s.out, "  %s %s\n", s.bold("extended with"), path)
	fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("+%d links, %d pages, %d contracts (now %d links across %d files)",
		len(s.merged.Links)-before, len(snap.Pages), len(snap.Endpoints), len(s.merged.Links), len(s.files))))
	if len(snap.ArchiveNames) > 0 {
		fmt.Fprintf(s.out, "  %s\n", s.dim(archiveBrief(snap.ArchiveNames)+" from this scan"))
	}
}

// mergeSnapshots unions two scans so the session can query across them. The node
// sets and the relations are concatenated and deduplicated; the first file wins
// where they overlap, because there is no reason to prefer the later one, and
// because a stable merge means a session answers the same question the same way
// however many files are open.
func mergeSnapshots(a, b *wmse.Snapshot) *wmse.Snapshot {
	out := &wmse.Snapshot{}
	out.Meta = a.Meta
	for k, v := range b.Meta {
		if _, ok := out.Meta[k]; !ok {
			out.Meta[k] = v
		}
	}
	seenLink := map[string]bool{}
	for _, src := range []*wmse.Snapshot{a, b} {
		for i := range src.Links {
			l := src.Links[i]
			if seenLink[wmse.LinkKey(&l)] {
				continue
			}
			seenLink[wmse.LinkKey(&l)] = true
			out.Links = append(out.Links, l)
		}
	}
	seenPage := map[string]bool{}
	for _, src := range []*wmse.Snapshot{a, b} {
		for _, p := range src.Pages {
			if seenPage[p.URL] {
				continue
			}
			seenPage[p.URL] = true
			out.Pages = append(out.Pages, p)
		}
	}
	// Endpoints are merged by path; groups and params are concatenated, which is
	// safe because they are descriptive rather than relational.
	seenEp := map[string]bool{}
	for _, src := range []*wmse.Snapshot{a, b} {
		for _, ep := range src.Endpoints {
			if seenEp[ep.Path] {
				continue
			}
			seenEp[ep.Path] = true
			out.Endpoints = append(out.Endpoints, ep)
		}
	}
	// Groups and parameters merge by what they are, not by being concatenated.
	// Concatenating them would make one group's members two groups and one
	// recovered name two names, and a params section listing every name twice is
	// a section nobody reads.
	var groupA, groupB, paramA, paramB []int
	out.Groups, groupA, groupB = mergeGroups(a.Groups, b.Groups)
	out.Params, paramA, paramB = mergeParams(a.Params, b.Params)
	out.Observations = append(append([]contract.Observation{}, a.Observations...), b.Observations...)
	out.ArchiveNames = append(append([]string{}, a.ArchiveNames...), b.ArchiveNames...)
	// The relation edges are rebuilt against the merged arrays, because an edge's
	// endpoints are indices and those move when the arrays merge.
	out.Edges = mergeEdges(a, b, out.Links, out.Endpoints, groupA, groupB, paramA, paramB)
	return out
}

// mergeGroups unions two files' URL patterns. A pattern is the same pattern when
// its domain and its shape match, and the first one loaded wins for the counts -
// the same rule as the links, and for the same reason: a session answers the same
// question the same way however many files are open.
func mergeGroups(a, b []wmse.Group) ([]wmse.Group, []int, []int) {
	seen := map[string]int{}
	var out []wmse.Group
	var ia, ib []int
	for _, g := range a {
		key := g.Domain + "\x00" + g.Pattern
		at, dup := seen[key]
		if !dup {
			at = len(out)
			seen[key] = at
			out = append(out, g)
		}
		ia = append(ia, at)
	}
	for _, g := range b {
		key := g.Domain + "\x00" + g.Pattern
		at, dup := seen[key]
		if !dup {
			at = len(out)
			seen[key] = at
			out = append(out, g)
		}
		ib = append(ib, at)
	}
	return out, ia, ib
}

// mergeParams unions two files' recovered names. The endpoints are part of what a
// name is - the same name bound to a different endpoint is a different fact - and
// they are compared as a set, because two files that found the same name list them
// in whatever order their own crawl reached them.
func mergeParams(a, b []wmse.Param) ([]wmse.Param, []int, []int) {
	seen := map[string]int{}
	var out []wmse.Param
	var ia, ib []int
	for _, p := range a {
		key := paramKey(p)
		at, dup := seen[key]
		if !dup {
			at = len(out)
			seen[key] = at
			out = append(out, p)
		}
		ia = append(ia, at)
	}
	for _, p := range b {
		key := paramKey(p)
		at, dup := seen[key]
		if !dup {
			at = len(out)
			seen[key] = at
			out = append(out, p)
		}
		ib = append(ib, at)
	}
	return out, ia, ib
}

func paramKey(p wmse.Param) string {
	eps := append([]string(nil), p.Endpoints...)
	sort.Strings(eps)
	return strings.Join([]string{
		p.Name, p.Kind.String(), p.Owner.String(), p.Carrier, strings.Join(eps, ","),
	}, "\x00")
}

// mergeEdges re-points both files' edges at the merged arrays and unions them.
//
// An edge's endpoints are indices, and a node's index in "this file" is not its
// index in the union, so every kind has to be translated rather than copied: the
// link kinds by the link's URL, the contract and param kinds by the endpoint's
// path, and the pattern kind by the group's own position. A kind that is skipped
// would not be noticed here - it would show up as a report that says a session
// holds fewer relations than either file, which is a quiet lie about a file that
// is perfectly readable on its own.
func mergeEdges(a, b *wmse.Snapshot, links []linker.Link, endpoints []contract.Endpoint, groupA, groupB, paramA, paramB []int) []wmse.Edge {
	linkIndex := make(map[string]int, len(links))
	for i := range links {
		if _, dup := linkIndex[wmse.LinkKey(&links[i])]; !dup {
			linkIndex[wmse.LinkKey(&links[i])] = i
		}
	}
	// Endpoints merge by path, so a path is what a contract edge is translated
	// through; two files that inferred the same endpoint produce one edge, not
	// two that differ only in an index.
	epIndex := make(map[string]int, len(endpoints))
	for i := range endpoints {
		if _, dup := epIndex[endpoints[i].Path]; !dup {
			epIndex[endpoints[i].Path] = i
		}
	}
	// Groups and parameters are merged by what they are, so a file's own index
	// says nothing about where its entries ended up; the maps built alongside the
	// merged arrays are the translation, and they are what makes an edge that
	// points at the first file's parameter point at the right one in the union.
	groupAt := map[*wmse.Snapshot][]int{a: groupA, b: groupB}
	paramAt := map[*wmse.Snapshot][]int{a: paramA, b: paramB}

	seen := map[[4]uint32]bool{}
	var out []wmse.Edge
	for _, snap := range []*wmse.Snapshot{a, b} {
		for _, e := range snap.Edges {
			from, okf := mergeEdgeSide(e.From, e.Kind, true, snap, linkIndex, epIndex, groupAt[snap], paramAt[snap])
			to, okt := mergeEdgeSide(e.To, e.Kind, false, snap, linkIndex, epIndex, groupAt[snap], paramAt[snap])
			if !okf || !okt {
				continue
			}
			key := [4]uint32{uint32(e.Kind), from, to, e.Weight}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, wmse.Edge{Kind: e.Kind, From: from, To: to, Weight: e.Weight})
		}
	}
	return out
}

// mergeEdgeSide translates one end of an edge into the merged arrays. Which array
// an end lives in is decided by the kind of the edge and by which end it is,
// because the kinds point at different things: a source edge is link to link, a
// pattern edge is group to link, a contract edge is link to endpoint, and a param
// edge is parameter to endpoint.
func mergeEdgeSide(idx uint32, kind uint8, from bool, snap *wmse.Snapshot, linkIndex map[string]int, epIndex map[string]int, groupAt, paramAt []int) (uint32, bool) {
	isGroup := kind == wmse.EdgePattern && from
	isParam := kind == wmse.EdgeParam && from
	isEndpoint := kind == wmse.EdgeContract || (kind == wmse.EdgeParam && !from)

	switch {
	case isGroup:
		if int(idx) >= len(groupAt) {
			return 0, false
		}
		return uint32(groupAt[idx]), true
	case isParam:
		if int(idx) >= len(paramAt) {
			return 0, false
		}
		return uint32(paramAt[idx]), true
	case isEndpoint:
		if int(idx) >= len(snap.Endpoints) {
			return 0, false
		}
		i, ok := epIndex[snap.Endpoints[idx].Path]
		if !ok {
			return 0, false
		}
		return uint32(i), true
	}
	// Everything else is a link at both ends.
	if int(idx) >= len(snap.Links) {
		return 0, false
	}
	i, ok := linkIndex[wmse.LinkKey(&snap.Links[idx])]
	if !ok {
		return 0, false
	}
	return uint32(i), true
}

// ---------- refs ----------

// cmdRefs shows where a resource was found, from the .ref sidecar the scan's
// -refs wrote. With no path it lists the reference files; with a path it shows
// the references for that source document.
func (s *session) cmdRefs(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.listRefFiles()
		return
	}
	full := s.resolve(args[0])
	rf := s.refFileFor(full)
	if rf == nil {
		fmt.Fprintf(s.out, "  %s has no reference file\n", s.field(full))
		fmt.Fprintf(s.out, "  %s\n", s.dim("references exist only for scans run with -refs"))
		return
	}
	fmt.Fprintf(s.out, "\n  %s %s\n", s.bold(s.field(full)), s.dim("("+plural(len(rf.Entries), "reference")+")"))
	for _, e := range rf.Entries {
		fmt.Fprintf(s.out, "    %d:%d\t%s\n", e.Line, e.Column, s.dim(oneline(e.Snippet, 100)))
	}
}

func (s *session) listRefFiles() {
	var total int
	for _, f := range s.files {
		refs := refNames(f.snap.ArchiveNames)
		if len(refs) == 0 {
			continue
		}
		fmt.Fprintf(s.out, "\n  %s %s\n", s.bold(f.path), s.dim("("+plural(len(refs), ".ref file")+")"))
		for _, name := range refs {
			fmt.Fprintf(s.out, "    %s\n", name)
		}
		total += len(refs)
	}
	if total == 0 {
		fmt.Fprintf(s.out, "  %s\n", s.dim("no scan here was run with -refs, so there are no reference files"))
	}
}

// refFileFor loads and parses the .ref file for a source, caching it.
func (s *session) refFileFor(full string) *wmse.RefFile {
	if s.refs == nil {
		s.refs = map[string]*wmse.RefFile{}
	}
	if rf, ok := s.refs[full]; ok {
		return rf
	}
	// A .ref belongs to a specific scan, so the first loaded file that kept one
	// for this source wins.
	for _, f := range s.files {
		if len(f.snap.ArchiveNames) == 0 {
			continue
		}
		if data, err := f.snap.ArchiveFileBySource(full); err == nil {
			rf := wmse.ParseRef(data)
			s.refs[full] = rf
			return rf
		}
	}
	s.refs[full] = nil
	return nil
}

// refCountFor counts how many places a resource was found, using the .ref files
// when the scan kept them. It is what tells `what` that a resource is worth
// looking at more closely.
func (s *session) refCountFor(full string) int {
	n := 0
	for _, f := range s.files {
		if len(f.snap.ArchiveNames) == 0 {
			continue
		}
		if data, err := f.snap.ArchiveFileBySource(full); err == nil {
			n += len(wmse.ParseRef(data).Entries)
		}
	}
	return n
}

// ---------- read ----------

// cmdRead prints the scan's report, with the reader's own flags.
//
// This is `wmse read`, run from inside the session. "Show me the whole thing
// again, this time with -r" is a thing a person wants to do without leaving what
// they were doing, and re-implementing the report here would be a second copy of
// the sections, the order and the gates to keep in step with the first - which is
// the way a reader starts lying about a file it has already described
// differently. So it is the same code over the same snapshot, and the only
// difference is the subject: what the session has open, which after `extend` is
// more than one file.
func (s *session) cmdRead(args []string) {
	cfg, fs := newFlagSet()
	// The report starts from the settings the file was scanned under, taken from
	// the command line the file records, so that `read` alone shows what the scan
	// saw rather than the reader's own defaults. A person who has opened a file
	// and is standing in it asking to be told the whole thing again means the
	// thing as it was made, not as a fresh request would default.
	//
	// Anything typed here is applied on top, which is what a flag is: one you do
	// not pass takes its default, and the default is now the scan's own value.
	saved := s.savedFlags()
	if len(saved) > 0 {
		if err := fs.Parse(saved); err != nil {
			// A command line the reader cannot read is not a reason to refuse
			// the report; the defaults fall back to the reader's own and the
			// reason is said, because a report that quietly used other settings
			// is the thing this avoids.
			fmt.Fprintf(s.out, "  %s\n", s.warn("the scan's flags in this file's metadata could not be read: "+err.Error()))
			fmt.Fprintf(s.out, "  %s\n", s.dim("the report is using the reader's own defaults instead"))
			saved = nil
			cfg, fs = newFlagSet()
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(s.out, usage)
			return
		}
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		fmt.Fprintf(s.out, "  %s\n", s.dim("the reader takes the same flags as the scan; `read -h` lists them"))
		return
	}
	// Go's flag package stops at the first non-flag argument and leaves it
	// unread, so a file path here would be ignored in silence. A session already
	// has files open, so the one thing a person could mean by it is a mistake
	// worth naming.
	if fs.NArg() > 0 {
		fmt.Fprintf(s.out, "  %s\n", s.warn("read takes flags, not a file: "+strings.Join(fs.Args(), " ")))
		fmt.Fprintf(s.out, "  %s\n", s.dim("the session already has files open; `extend <file>` adds one, and `info` says which"))
		return
	}
	// The settings the report was rendered with are named, because a section can
	// appear that nobody asked for and a reader who cannot tell the default from
	// the request cannot tell a file from a flag.
	if len(saved) > 0 {
		if len(args) == 0 {
			fmt.Fprintf(s.out, "  %s\n", s.dim("report: the scan's own flags, from the file's metadata"))
		} else {
			fmt.Fprintf(s.out, "  %s\n", s.dim("report: the scan's flags, with "+strings.Join(args, " ")+" applied"))
		}
	}

	cfg.Apply(fs)

	o := &options{cfg: cfg, snap: s.merged, info: s.mergedInfo(), path: s.mergedPath(), depth: readDepth(cfg, fs)}
	o.setup()
	// The reasons come first and on stderr, as they do from the command line, so
	// they are read before the report rather than discovered at the end of it.
	o.explain()
	// The report is the reader's, printed the reader's way, on the reader's
	// streams: the same sections, the same order, the same gates.
	if err := report(o); err != nil {
		// The reasons are already printed, once per flag, next to the metadata
		// that explains them. A session has no exit status, so this is the whole
		// of what is added: the summary the command line would exit with rather
		// than print.
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
	}
}

// savedFlags are the reader's flags the file records in its metadata, which is
// the command line the scan ran. A file with no command recorded - one written by
// a hand, or by a version that did not record one - has none, and the report falls
// back to the reader's own defaults rather than inventing settings.
func (s *session) savedFlags() []string {
	return readersFlagsFrom(s.merged.Meta[wmse.MetaCommand])
}

// scanFlags takes the reader's flags out of a recorded command line.
//
// The command is text that was written to be read by a person, so it is taken
// apart by what a shell would do rather than by a rule of its own: the first word
// is the program, a word starting with a dash is a flag, a bare `--` ends the
// flags, and a word that is none of those is either the value of the flag before it
// or a stray. A flag's own declaration says whether it takes a value, which is the
// only way to tell `-url http://x` from two stray words - and getting it wrong
// would hand the reader a flag with no argument and refuse a report over a command
// line that was perfectly fine.
func readersFlagsFrom(command string) []string {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	_, fs := newFlagSet()
	words := splitWords(command)
	var out []string
	for i := 1; i < len(words); i++ {
		w := words[i]
		if len(w) < 2 || w[0] != '-' {
			continue
		}
		// A bare `--` ends the flags, as it does everywhere else; what follows is
		// the program's own words, not switches.
		if w == "--" {
			break
		}
		name := strings.TrimLeft(w, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		// The reader registers every flag the scan has, so from a webmap command
		// this keeps the lot: the ones that shape a section, the ones that scope
		// the report, and the ones the reader accepts and ignores. A flag it does
		// not know comes from another tool or a hand-edited line, and is left out
		// with its value rather than refused - the report is about sections, and a
		// flag with no meaning here has no section to show.
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		out = append(out, w)
		if takesValue(f) && !strings.Contains(w, "=") && i+1 < len(words) {
			i++
			out = append(out, words[i])
		}
	}
	return out
}

// takesValue reports whether a flag needs an argument, which is what tells the
// word after it from a stray one.
func takesValue(f *flag.Flag) bool {
	if f == nil {
		return false
	}
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !bf.IsBoolFlag()
}

// mergedInfo describes what a report printed here covers: every file the session
// has open. With one file it is that file's own header, so `read` on a session
// opened from one file prints exactly what `wmse read` prints for it. With
// several it is their sum and their union, because a header naming one file above
// a report drawn from three is a caption the reader has to catch out.
func (s *session) mergedInfo() *wmse.FileInfo {
	if len(s.files) == 1 {
		return s.files[0].info
	}
	merged := &wmse.FileInfo{}
	seen := map[uint8]bool{}
	for _, f := range s.files {
		if merged.Version == 0 {
			merged.Version = f.info.Version
		}
		merged.Flags |= f.info.Flags
		merged.HeaderBytes += f.info.HeaderBytes
		merged.TotalBytes += f.info.TotalBytes
		for _, sec := range f.info.Sections {
			if seen[sec.ID] {
				continue
			}
			seen[sec.ID] = true
			merged.Sections = append(merged.Sections, sec)
		}
	}
	return merged
}

// mergedPath names the files a report printed here covers, in the header's place.
func (s *session) mergedPath() string {
	if len(s.files) == 1 {
		return s.files[0].path
	}
	names := make([]string, len(s.files))
	for i, f := range s.files {
		names[i] = f.path
	}
	return strings.Join(names, " + ")
}

// ---------- history ----------

// cmdHistory writes down what has been typed this session. The up and down keys
// walk the same list; this is that list read out, for a person who wants to see
// what they have run rather than remember it.
func (s *session) cmdHistory(args []string) {
	if s.hist == nil || len(s.hist.lines) == 0 {
		fmt.Fprintf(s.out, "  %s\n", s.dim("nothing typed yet"))
		return
	}
	// The most recent last, the way a shell prints it and the way it is read.
	from := 1
	if n := len(s.hist.lines); n > 20 {
		from = n - 19
	}
	for i := from - 1; i < len(s.hist.lines); i++ {
		fmt.Fprintf(s.out, "  %s %s\n", s.dim(fmt.Sprintf("%4d", i+1)), s.hist.lines[i])
	}
	if from > 1 {
		fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("(%d earlier)", from-1)))
	}
}

// ---------- shared ----------

func (s *session) notFound(full string) {
	fmt.Fprintf(s.out, "  %s %s\n", s.warn("not in this file:"), s.field(full))
	fmt.Fprintf(s.out, "  %s\n", s.dim("the scan did not find this path; try `ls` or `find any *` to see what it did"))
}

func (s *session) usage(what string) {
	fmt.Fprintf(s.out, "  %s\n", s.dim("usage: "+what))
}
