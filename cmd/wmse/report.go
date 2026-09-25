package main

import (
	"fmt"
	"sort"
	"strings"

	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/urlgroup"
	"apimap/internal/wmse"
)

// heading prints a section title in the shape the report uses.
func (o *options) heading(title string) {
	if o.cfg.Color {
		fmt.Printf("\033[1m\n=== %s ===\033[0m\n", title)
		return
	}
	fmt.Printf("\n=== %s ===\n", title)
}

func (o *options) dim(s string) string {
	if !o.cfg.Color {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

func (o *options) bold(s string) string {
	if !o.cfg.Color {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

// warn marks the one thing a reader must not walk past.
func (o *options) warn(s string) string {
	if !o.cfg.Color {
		return s
	}
	return "\033[33m" + s + "\033[0m"
}

// viewReport prints the scan's report from the file.
//
// Every section below has a counterpart in the scan's own report, gated by the
// same flag, because a saved scan should read as the run that produced it. What
// the reader adds is the file's own information - which pages were actually
// fetched, at what depth, found on what - because that is what a terminal report
// has no room for and a file does.
//
// A section whose data the file does not hold is left out entirely, because
// explain has already answered that request with an error and the metadata that
// explains it. Printing an empty section after that error would put two
// contradictory things on the same screen.
func viewReport(o *options) {
	viewHeader(o)
	if len(o.snap.Links) == 0 {
		fmt.Printf("\n  %s\n", o.dim("this scan found nothing: the file holds no links at all"))
		return
	}
	viewAnalysis(o)
	// The scan leaves the link table out when the graph or the markdown export
	// is what was asked for, and the reader does the same rather than printing
	// two renderings of the same set.
	if !o.cfg.Graphical {
		viewAllLinks(o)
	}
	viewHiddenClasses(o)
	if !o.cfg.NoGroup {
		viewPatterns(o)
	}
	if o.cfg.Recursive && o.hasPages {
		viewTree(o)
	}
	// -apic-raw implies -apic. The scan prints its evidence inside the contract
	// section and so leaves -apic-raw on its own silent, which is a footgun
	// rather than a meaning: the flag is the contracts *plus* the requests they
	// came from, and a reader that dropped the contracts because the user asked
	// for the evidence would be answering a different question.
	contracts := o.cfg.APIContract || o.cfg.APIContractRaw
	if contracts && o.hasContracts {
		viewContracts(o)
		if o.cfg.APIContractRaw && o.hasObservations {
			// -apic-raw is the evidence, not a louder contract: the file
			// stores the requests the contracts were inferred from, and
			// this is where they are said in full.
			viewObservations(o)
		}
	}
	viewParams(o)
	if o.cfg.Emulate && o.hasEmulation {
		viewEmulation(o)
	}
	if o.cfg.Graphical && o.hasRelations {
		viewGraph(o)
	}
	viewFlagsHint(o)
}

// viewHeader says which file this is and what the run that made it was. It is
// the first thing a reader sees, and it is also what a request the file cannot
// answer is answered with, because the question behind such a request is always
// "what was actually run?".
//
// It prints once per request. A file that could not answer something is told so
// first, and the metadata came with that complaint; printing it again at the top
// of the report would just be the same six lines twice.
func viewHeader(o *options) {
	if o.headerShown {
		return
	}
	o.headerShown = true
	snap, info := o.snap, o.file.Info()
	sections := "1 section"
	if len(info.Sections) != 1 {
		sections = fmt.Sprintf("%d sections", len(info.Sections))
	}
	fmt.Printf("%s  %s  %s\n", o.bold("WebMap snapshot"), o.path,
		o.dim(fmt.Sprintf("(%s, %s)", humanBytes(int64(info.TotalBytes)), sections)))
	if v := snap.Meta[wmse.MetaTool]; v != "" {
		ver := snap.Meta[wmse.MetaVersion]
		if ver == "" {
			ver = "?"
		}
		fmt.Printf("  %-12s %s %s %s\n", "produced by", v, ver, o.dim("("+snap.Meta[wmse.MetaFormat]+")"))
	}
	for _, row := range [][2]string{
		{"target", snap.Meta[wmse.MetaTarget]},
		{"created", snap.Meta[wmse.MetaCreated]},
		{"scope", snap.Meta[wmse.MetaScope]},
		{"elapsed", snap.Meta[wmse.MetaElapsed]},
		{"command", snap.Meta[wmse.MetaCommand]},
	} {
		if row[1] != "" {
			fmt.Printf("  %-12s %s\n", row[0], row[1])
		}
	}
	fmt.Printf("  %-12s %s\n", "holds", o.holds())

	if snap.Meta[wmse.MetaSessionData] == "true" {
		// The observations and intercepted calls are stored exactly as the scan
		// sent them, so a token from the session that ran the scan is in here.
		// The configured -H and -cookie values are not, but a captured
		// Authorization header, cookie or CSRF token in a body is evidence and
		// is kept as-is.
		fmt.Printf("\n%s\n", o.warn("This file holds the requests this scan really sent, which can include"))
		fmt.Printf("%s\n", o.warn("session material (tokens, cookies, ids). Treat it as a secret."))
	}
}

// viewFlagsHint closes the report by naming the flags that shape it. It is the
// one part of the reader that exists only for discovery: a report is the thing
// most people will run with no flags at all, and a section they did not ask for
// is indistinguishable from a section the file cannot answer. So the flags go
// where the missing sections would be.
func viewFlagsHint(o *options) {
	fmt.Printf("\n  %s\n", o.dim("the flags that shape this report: -r -apic -apic-raw -apif -emulate -G -j -nogroup"))
	fmt.Printf("  %s\n", o.dim("and the ones that shape what it covers: -url -a -f -rdepth -rlimit -t -waf -cdn -cache -noise -full -color"))
}

// holds describes what the file has in it, in one line: the parts of a scan that
// a report prints and the parts it throws away.
func (o *options) holds() string {
	parts := []string{
		count(len(o.snap.Links), "link"),
		count(len(o.snap.Pages), "page fetched", "pages fetched"),
		count(len(o.snap.Groups), "url pattern", "url patterns"),
		count(len(o.snap.Endpoints), "api contract"),
		count(len(o.snap.Observations), "request observation"),
		count(len(o.snap.Params), "recovered parameter"),
		fmt.Sprintf("%d relations", len(o.snap.Edges)),
	}
	return strings.Join(parts, ", ")
}

// viewInfo answers "is this actually small, and why", plus the full record of the
// run that made the file. It reads the header and the directory, so it costs
// almost nothing even on a large file.
func viewInfo(o *options) {
	viewHeader(o)
	info := o.file.Info()

	o.heading("Storage")
	fmt.Printf("  %-14s %-6s %10s %10s %7s\n", "section", "codec", "stored", "raw", "ratio")
	fmt.Printf("  %s\n", strings.Repeat("-", 52))
	total := 0
	for _, s := range info.Sections {
		ratio := "-"
		if s.RawLen > 0 {
			ratio = fmt.Sprintf("%.0f%%", 100*float64(s.StoredLen)/float64(s.RawLen))
		}
		fmt.Printf("  %-14s %-6s %10s %10s %7s\n", s.Name, wmse.CodecName(s.Codec),
			humanBytes(int64(s.StoredLen)), humanBytes(int64(s.RawLen)), ratio)
		total += s.StoredLen
	}
	fmt.Printf("  %s\n", strings.Repeat("-", 52))
	fmt.Printf("  %-14s %-6s %10s\n", "payloads", "", humanBytes(int64(total)))
	fmt.Printf("  %-14s %-6s %10s\n", "header", "", humanBytes(int64(info.HeaderBytes)))

	// Every key the scan wrote, in full. A file is meant to explain itself to a
	// reader that knows nothing about the version that wrote it, so this is the
	// whole record, not a selection of the interesting parts.
	o.heading("Saved Metadata")
	for _, k := range sortedStringKeys(o.snap.Meta) {
		fmt.Printf("  %-24s %s\n", k, o.snap.Meta[k])
	}
	if hosts := hostCounts(o.snap); len(hosts) > 0 {
		o.heading("Hosts")
		for _, h := range hosts {
			o.out(fmt.Sprintf("  %-44s %d", h.host, h.count))
		}
	}
}

// viewAnalysis is the scan's opening block: the scope the report covers and what
// it found there.
func viewAnalysis(o *options) {
	scope := "Same Domain Only"
	if o.allDomains {
		scope = "All Domains"
	}
	o.heading(fmt.Sprintf("WebMap Analysis (%s)", scope))
	// The counts below are over everything the file holds, exactly as the scan
	// counted everything it found, so a named scope is spelled out here: the
	// numbers are the whole scan's, the table below them is the scope's.
	if o.namedScope && !o.allDomains {
		fmt.Printf("  %s\n", o.dim("narrowed to: "+scopeOf(o)))
	}
	fmt.Printf("Total links: %d\n", o.stats.Total)

	// Every category and every link type is listed, including the ones with
	// nothing in them. The scan lists them all, and a reader whose job is to be
	// the same report with the network replaced cannot improve on it by dropping
	// the lines that say "none of these": a person comparing the two outputs
	// would have to know which lines were omitted to know they were not lost.
	fmt.Println("\nBy Category:")
	for _, cat := range []linker.Category{
		linker.CategoryWebPage, linker.CategoryWebAsset, linker.CategoryAPI,
		linker.CategoryDynamic, linker.CategoryUnknown,
	} {
		fmt.Printf("  %s: %d\n", cat.String(), o.stats.ByCategory[cat])
	}

	fmt.Println("\nBy Link Type:")
	for _, lt := range []linker.LinkType{
		linker.LinkTypeRelative, linker.LinkTypeAbsolute, linker.LinkTypeWeb, linker.LinkTypeUnknown,
	} {
		fmt.Printf("  %s: %d\n", lt.String(), o.stats.ByLinkType[lt])
	}

	fmt.Printf("\nWith query params: %d\n", o.stats.WithParams)

	// The class counts are over everything in the file, hidden ones included,
	// because a hidden class is exactly the thing a reader needs to be told
	// about: it is in the file and one flag away.
	fmt.Println("\nBy URL Class:")
	for _, c := range []linker.URLClass{
		linker.ClassNormal, linker.ClassWAF, linker.ClassCDN, linker.ClassCache, linker.ClassNoise,
	} {
		fmt.Printf("  %s: %d%s\n", c.String(), o.stats.ByClass[c], o.hiddenNote(c))
	}
}

// hiddenNote is the "(hidden - use -waf to show)" the scan prints next to a class
// it filtered out, said whenever the flag is off rather than only when the class
// is non-empty: the scan says it unconditionally, and a reader that left it out
// on an empty class would be quieter than the tool it stands in for.
func (o *options) hiddenNote(c linker.URLClass) string {
	if c == linker.ClassNormal || o.classVisible(&linker.Link{Class: c}) {
		return ""
	}
	return fmt.Sprintf(" (hidden — use %s to show)", classFlag(c))
}

func classFlag(c linker.URLClass) string {
	switch c {
	case linker.ClassWAF:
		return "-waf"
	case linker.ClassCDN:
		return "-cdn"
	case linker.ClassCache:
		return "-cache"
	case linker.ClassNoise:
		return "-noise"
	}
	return ""
}

// viewAllLinks is the link table: the same rows, in the same order, folded and
// merged the way the scan folds them, plus the two fields a saved scan knows and
// the report had no room for - the depth a link was found at and the page it was
// found on. A relative URL is followed by what it resolved to, so one line says
// both the site's shape and the address to request.
//
// -t puts the tag where the scan puts it, in the category column, and -apif
// prints an API link as the method and arguments the scan prints it as.
func viewAllLinks(o *options) {
	o.heading("All Links")
	for i := range o.rows {
		l := &o.rows[i]
		if o.cfg.APIFull && l.Category == linker.CategoryAPI && len(l.APIDetails) > 0 {
			o.apiRows(l)
			continue
		}
		if !o.out("  " + o.linkRow(l)) {
			return
		}
	}
	if o.stopped {
		return
	}
	// The count says what the flags matched, not what was printed. It is the
	// unfolded count, because the point of the line is "there is more in the file
	// than this table shows", and a folded count would answer a different
	// question. -rlimit reports itself once, at the end of the request, so a
	// section with no footer of its own still says so.
	if total := len(o.snap.Links); o.keptCount != total {
		fmt.Printf("%s\n", o.dim(fmt.Sprintf("  %d of %d links", o.keptCount, total)))
	}
}

// linkRow renders one link the way the scan's table does, with the extra fields
// the file has.
func (o *options) linkRow(l *linker.Link) string {
	cat := l.Category.String()
	if o.cfg.ShowTags && l.Tag != "" {
		// The scan replaces the category with the tag under -t, because the tag
		// is the more precise of the two for a human reading the row.
		cat = l.Tag
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-6s %-11s %-24s %s", l.LinkType.String(), cat,
		trunc(l.Domain, 24), l.HREF)
	if l.Resolved != "" && l.Resolved != l.HREF {
		fmt.Fprintf(&b, " -> %s", l.Resolved)
	}
	if p := o.paramsInfo(l); p != "" {
		fmt.Fprintf(&b, " %s", o.dim(p))
	}
	if l.Class != linker.ClassNormal {
		fmt.Fprintf(&b, " %s", o.dim("["+l.Class.String()+"]"))
	}
	if l.Depth > 0 {
		fmt.Fprintf(&b, " %s", o.dim(fmt.Sprintf("[d%d]", l.Depth)))
	}
	if l.SourceURL != "" {
		fmt.Fprintf(&b, " %s", o.dim("from "+o.shortURL(l.SourceURL)))
	}
	return b.String()
}

// paramsInfo is the query annotation of a row, and it is the scan's rule rather
// than this reader's: a link that was requested with queries says which, because
// the values are what makes a URL reproducible, and a link that only claims to
// have parameters says just that.
func (o *options) paramsInfo(l *linker.Link) string {
	if len(l.ParamVariants) > 0 {
		if p := linker.FormatParamVariants(l.ParamVariants); p != "" {
			return "(params: " + p + ")"
		}
		return "(params)"
	}
	if l.HasParams {
		return "(params)"
	}
	return ""
}

// apiRows prints one row per distinct method and argument list, which is the
// scan's -apif rendering down to the shared CleanArgs that decides what counts as
// an argument list. The file kept the details, so the offline report is the
// online one.
func (o *options) apiRows(l *linker.Link) {
	type key struct{ method, args string }
	seen := map[key]bool{}
	for _, d := range l.APIDetails {
		method := d.HTTPMethod
		if method == "" {
			method = "ANY"
		}
		k := key{method, d.Arguments}
		if seen[k] {
			continue
		}
		seen[k] = true
		var b strings.Builder
		fmt.Fprintf(&b, "%-6s %-40s", method, l.HREF)
		if args := contract.CleanArgs(d.Arguments); args != "" {
			fmt.Fprintf(&b, " args: %s", args)
		}
		if p := o.paramsInfo(l); p != "" {
			fmt.Fprintf(&b, " %s", o.dim(p))
		}
		if l.Class != linker.ClassNormal {
			fmt.Fprintf(&b, " %s", o.dim("["+l.Class.String()+"]"))
		}
		if l.SourceURL != "" {
			fmt.Fprintf(&b, " %s", o.dim("from "+o.shortURL(l.SourceURL)))
		}
		o.out("  " + b.String())
	}
}

// viewHiddenClasses names the links the flags are holding back, with the flag
// that releases them. A saved scan keeps what the original run hid, and this is
// where the reader says so.
func viewHiddenClasses(o *options) {
	o.heading("Hidden Classes")
	hidden := false
	for _, c := range []linker.URLClass{
		linker.ClassWAF, linker.ClassCDN, linker.ClassCache, linker.ClassNoise,
	} {
		n := o.stats.ByClass[c]
		if n == 0 || o.classVisible(&linker.Link{Class: c}) {
			continue
		}
		fmt.Printf("  %s: %d URL(s) hidden (%s to show)\n", c.String(), n, classFlag(c))
		hidden = true
	}
	if !hidden {
		fmt.Println("  none")
	}
}

// viewPatterns prints the confirmed patterns with the values their variables took,
// which is what turns a pattern into a test case.
func viewPatterns(o *options) {
	if len(o.snap.Groups) == 0 {
		return
	}
	o.heading("URL Patterns")
	for i := range o.snap.Groups {
		g := &o.snap.Groups[i]
		// The same range column the scan prints: the clustered values of each
		// variable slot, so two readers of the same file see the same line.
		ranges := strings.Join(urlgroup.ClusterRanges([]urlgroup.Group{{
			Domain: g.Domain, Pattern: g.Pattern, Count: g.Count, Vars: g.Vars,
		}}), ", ")
		line := fmt.Sprintf("  %-22s %s  x%-4d", trunc(g.Domain, 22), g.Pattern, g.Count)
		if ranges != "" {
			line += " [" + ranges + "]"
		}
		if !o.out(line) {
			return
		}
		if len(g.Members) > 0 {
			shown := g.Members
			if len(shown) > 3 {
				shown = shown[:3]
			}
			if !o.out("         members: " + strings.Join(shown, "\n                  ")) {
				return
			}
			if len(g.Members) > 3 {
				if !o.out(fmt.Sprintf("                  (+%d more)", len(g.Members)-3)) {
					return
				}
			}
		}
	}
}

// viewParams prints the parameters that were recovered from request builders but
// reached no contract and appear in no link, which is the same division the scan
// makes: a name that is already reported elsewhere must not be reported twice in
// two places, or a reader cannot tell which of the two is the finding.
func viewParams(o *options) {
	if len(o.snap.Params) == 0 {
		return
	}
	observed := observedParamNames(o.rows)
	type group struct {
		refs []int
	}
	groups := map[string]*group{}
	var order []string
	for i := range o.snap.Params {
		p := &o.snap.Params[i]
		if observed[p.Name] || len(p.Endpoints) > 0 {
			continue
		}
		key := p.OwnerLabel() + " / " + p.KindLabel()
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}
		g.refs = append(g.refs, i)
	}
	if len(order) == 0 {
		return
	}
	o.heading("Dynamic query params")
	for _, key := range order {
		if !o.out("  " + o.dim(key)) {
			return
		}
		refs := groups[key].refs
		sort.Slice(refs, func(i, j int) bool {
			return o.snap.Params[refs[i]].Name < o.snap.Params[refs[j]].Name
		})
		for _, i := range refs {
			if !o.out(o.dim("    " + o.paramRef(o.snap.Params[i]))) {
				return
			}
		}
	}
}

// paramRef renders one recovered parameter the way the scan renders it: how many
// documents it was found in, and where it went. A parameter that feeds an
// endpoint names it, because that is the finding; one that feeds a variable says
// so, because a name attached to a carrier is a lead rather than a contract.
func (o *options) paramRef(p wmse.Param) string {
	docs := len(p.Docs)
	unit := "docs"
	if docs == 1 {
		unit = "doc "
	}
	line := fmt.Sprintf("%-26s %3d %s", p.Name, docs, unit)
	if len(p.Endpoints) > 0 {
		return line + "  -> " + strings.Join(p.Endpoints, ", ")
	}
	if p.Carrier != "" {
		return line + "  -> " + p.Carrier + " (endpoint set elsewhere)"
	}
	if docs > 0 && docs <= 3 {
		names := make([]string, 0, docs)
		for _, d := range p.Docs {
			names = append(names, shortSource(d))
		}
		sort.Strings(names)
		return line + "  in " + strings.Join(names, ", ")
	}
	return line
}

// shortSource trims a source URL to what fits on one report line: no scheme, no
// host, no query. A parameter is reported per document, and the path is what
// identifies the document.
func shortSource(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "/"); j >= 0 {
			s = s[j:]
		} else {
			s = "/"
		}
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 44 {
		s = "..." + s[len(s)-41:]
	}
	return s
}

// observedParamNames collects the query keys a link row already shows, so the
// params section does not report them a second time. The name alone is not
// enough to skip on: a row that printed the values, and the very same row that
// printed nothing about them, do not both answer "what did this endpoint take?",
// which is the question this section exists for.
func observedParamNames(links []linker.Link) map[string]bool {
	out := map[string]bool{}
	for _, l := range links {
		shown := linker.FormatParamVariants(l.ParamVariants)
		if shown == "" {
			// The row will say nothing about the queries, so these
			// names are still unaccounted for.
			continue
		}
		for _, part := range strings.Split(shown, "; ") {
			name := part
			if i := strings.IndexByte(name, '='); i > 0 {
				name = name[:i]
			}
			if i := strings.IndexByte(name, '+'); i > 0 {
				name = name[:i]
			}
			if name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// viewTree walks the crawl. The indentation is the level the crawler reached a
// page at, which is the only hierarchy a crawl actually has.
func viewTree(o *options) {
	o.heading("Crawl Tree")
	idx := o.snap.NodeIndex()
	crawled := map[int][]int{}
	for _, e := range o.snap.Edges {
		if e.Kind == wmse.EdgeCrawled {
			crawled[int(e.From)] = append(crawled[int(e.From)], int(e.To))
		}
	}
	for k := range crawled {
		sort.Ints(crawled[k])
	}

	pages := pageSet(o.snap)
	var roots []int
	for _, p := range o.snap.Pages {
		if p.Depth != 0 {
			continue
		}
		if i, ok := idx[p.URL]; ok {
			roots = append(roots, i)
		}
	}
	// A page the scan fetched but that nothing links to - a redirect target, an
	// entry point behind a pattern - still deserves a root of its own. The walk
	// below removes from this list anything it does reach, so only genuinely
	// unreachable pages are left.
	for i := range o.snap.Links {
		if pages[wmse.LinkKey(&o.snap.Links[i])] {
			roots = append(roots, i)
		}
	}
	sort.Ints(roots)

	// The walk descends past the depth limit rather than stopping at it. The
	// nodes below the cut-off are not printed, but they are still visited, and
	// that is what keeps them from being re-printed at the left margin as if the
	// site had no path to them: -rdepth is a request to show less, not a
	// statement that they are unreachable.
	seen := map[int]bool{}
	below := 0
	var walk func(i, depth int)
	walk = func(i, depth int) {
		if seen[i] {
			return
		}
		seen[i] = true
		if o.depth > 0 && depth > o.depth {
			// Counted, because a tree that stops without saying so reads
			// as the end of the site, and this is a file that holds
			// everything below.
			below++
			for _, c := range crawled[i] {
				walk(c, depth+1)
			}
			return
		}
		if !o.keep[i] {
			// A link the scope or the class filters left out is not a dead end:
			// its children are still what the reader asked for.
			for _, c := range crawled[i] {
				walk(c, depth+1)
			}
			return
		}
		l := &o.snap.Links[i]
		mark := ""
		if ct := pageType(o.snap, wmse.LinkKey(l)); ct != "" {
			mark = "  " + o.dim(shortType(ct))
		}
		o.out(strings.Repeat("  ", depth) + o.treeNode(l) + mark)
		if o.stopped {
			// Out of budget. Claim the rest of the site so the root pass does
			// not print what is left of it a second time.
			markAll(seen, len(o.snap.Links))
			return
		}
		for _, c := range crawled[i] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	if below > 0 && !o.stopped {
		fmt.Printf("%s\n", o.dim(fmt.Sprintf(
			"  (%s below -rdepth %d, and in the file: -rdepth 0 shows the whole tree)",
			count(below, "more page"), o.depth)))
	}
}

// markAll claims every node, which is what a view does when it has decided it is
// finished: anything still unclaimed would otherwise be reported again as an
// unvisited root.
func markAll(seen map[int]bool, n int) {
	for i := 0; i < n; i++ {
		seen[i] = true
	}
}

func pageSet(snap *wmse.Snapshot) map[string]bool {
	out := make(map[string]bool, len(snap.Pages))
	for _, p := range snap.Pages {
		out[p.URL] = true
	}
	return out
}

func pageType(snap *wmse.Snapshot, url string) string {
	for _, p := range snap.Pages {
		if p.URL == url {
			return p.ContentType
		}
	}
	return ""
}

func shortType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i > 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}

// treeNode renders one tree line. A node on the target host prints as its bare
// path, because otherwise every line of a one-host scan starts with the same
// scheme and host and the tree becomes unreadable. A node anywhere else keeps its
// host: two "/index.html" on two hosts are not one node, and the whole point of
// the tree is that each line is a place.
func (o *options) treeNode(l *linker.Link) string {
	u := wmse.LinkKey(l)
	p := u
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if o.target != "" && sameHost(u, o.target) {
			p = "/"
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				p = rest[j:]
			}
		} else {
			p = "//" + rest
		}
	}
	if o.cfg.Color {
		switch l.Category {
		case linker.CategoryAPI:
			return "\033[36m" + p + "\033[0m"
		case linker.CategoryDynamic:
			return "\033[35m" + p + "\033[0m"
		}
	}
	return p
}

// viewContracts prints the inferred contracts with the scan's own renderer: the
// file stores them verbatim, so the offline output is the online output.
func viewContracts(o *options) {
	o.heading("API Contracts")
	if len(o.snap.Endpoints) == 0 {
		return
	}
	if o.cfg.Color {
		fmt.Print(contract.RenderColored(o.snap.Endpoints, o.cfg.APIContractRaw))
		return
	}
	fmt.Print(contract.Render(o.snap.Endpoints, o.cfg.APIContractRaw))
}

// viewObservations prints the evidence the contracts were inferred from. It is
// the section that makes a saved scan worth more than a re-run: a contract
// collapses many calls, and only this says what the first one actually sent.
func viewObservations(o *options) {
	if len(o.snap.Observations) == 0 {
		return
	}
	o.heading("Request Observations")
	for i := range o.snap.Observations {
		ob := &o.snap.Observations[i]
		method := ob.Method
		if method == "" {
			method = "ANY"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "  %-6s %s", method, ob.URL)
		if ob.EndpointOnly {
			fmt.Fprintf(&b, "  %s", o.dim("(endpoint only: no request around it)"))
		}
		if ob.MethodInferred {
			fmt.Fprintf(&b, "  %s", o.dim("(method inferred)"))
		}
		if !o.out(b.String()) {
			return
		}
		for _, h := range ob.Headers {
			if !o.out(fmt.Sprintf("           %s: %s", h.Name, h.Value)) {
				return
			}
		}
		if ob.Body != "" {
			if !o.out(fmt.Sprintf("           body: %s", oneline(ob.Body, 160))) {
				return
			}
		}
		for _, q := range ob.InferredQuery {
			if !o.out(fmt.Sprintf("           ?%s (inferred, no value seen)", q.Name)) {
				return
			}
		}
		for _, rf := range ob.ResponseFields {
			if !o.out(fmt.Sprintf("           reads response %s (%s)", rf.Path, rf.Kind)) {
				return
			}
		}
	}
}

// viewEmulation prints what the sandbox did, in the scan's own shape.
func viewEmulation(o *options) {
	em := o.snap.Emulation
	if em == nil {
		return
	}
	scope := "Same Domain Only"
	if o.allDomains {
		scope = "All Domains"
	}
	o.heading(fmt.Sprintf("Browser Emulation Results (%s)", scope))
	fmt.Printf("Scripts executed: %d\n", em.Scripts)
	fmt.Printf("Network calls intercepted: %d\n", em.Calls)
	// The scan prints the call types in the order of the request kinds they
	// stand for, not alphabetically: a fetch is not a beacon, and the order is
	// the order a page makes them in.
	for _, t := range []string{
		"fetch", "xhr", "websocket", "eventsource", "beacon", "image", "script",
		"link", "iframe", "form", "navigation", "media", "object", "other",
	} {
		if n := em.ByType[t]; n > 0 {
			fmt.Printf("  %s: %d\n", t, n)
		}
	}
	if len(em.Errors) > 0 {
		fmt.Printf("Script errors: %d\n", len(em.Errors))
	}
	if em.Disabled {
		fmt.Printf("%s\n", o.dim("  the sandbox disabled itself: too many native hangs"))
	}
	if em.Abandoned > 0 {
		fmt.Printf("Abandoned native hangs: %d (emulation may be disabled)\n", em.Abandoned)
	}
	if len(em.List) == 0 && len(em.Errors) == 0 {
		return
	}
	fmt.Println()
	for _, c := range em.List {
		method := c.Method
		if method == "" {
			method = "GET"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "  %-10s %-6s %s", c.Type, method, c.URL)
		if c.Initiator != "" {
			fmt.Fprintf(&b, " %s", o.dim("from "+o.shortURL(c.Initiator)))
		}
		if !o.out(b.String()) {
			return
		}
		for _, h := range c.Headers {
			if !o.out(fmt.Sprintf("           %s: %s", h[0], h[1])) {
				return
			}
		}
		if c.Body != "" {
			if !o.out(fmt.Sprintf("           body: %s", oneline(c.Body, 160))) {
				return
			}
		}
	}
	for _, e := range em.Errors {
		if !o.out("  ERROR  " + oneline(e, 200)) {
			return
		}
	}
}

// viewGraph prints the relation graph. The stored edges are the file's own
// structure, so showing them is showing what is really in there rather than a
// re-derivation of it.
func viewGraph(o *options) {
	o.heading("Relation Graph")
	counts := map[uint8]int{}
	for _, e := range o.snap.Edges {
		counts[e.Kind]++
	}
	for kind := 1; kind < wmse.EdgeCount; kind++ {
		if counts[uint8(kind)] == 0 {
			continue
		}
		fmt.Printf("  %-10s %d\n", wmse.EdgeName(uint8(kind)), counts[uint8(kind)])
	}
	for _, e := range o.snap.Edges {
		var line string
		switch e.Kind {
		case wmse.EdgeSource, wmse.EdgeCrawled:
			if int(e.From) < len(o.snap.Links) && int(e.To) < len(o.snap.Links) {
				// Full URLs here, not the tree's short paths: two hosts in one
				// scan are the normal case, and "/index.html" twice is not one
				// node.
				line = fmt.Sprintf("  %-9s %s -> %s", wmse.EdgeName(e.Kind),
					wmse.LinkKey(&o.snap.Links[e.From]), wmse.LinkKey(&o.snap.Links[e.To]))
			}
		case wmse.EdgePattern:
			if int(e.From) < len(o.snap.Groups) && int(e.To) < len(o.snap.Links) {
				line = fmt.Sprintf("  %-9s %s -> %s", wmse.EdgeName(e.Kind),
					o.snap.Groups[e.From].Pattern, wmse.LinkKey(&o.snap.Links[e.To]))
			}
		case wmse.EdgeContract:
			if int(e.From) < len(o.snap.Links) && int(e.To) < len(o.snap.Endpoints) {
				line = fmt.Sprintf("  %-9s %s -> %s", wmse.EdgeName(e.Kind),
					wmse.LinkKey(&o.snap.Links[e.From]), o.snap.Endpoints[e.To].Path)
			}
		case wmse.EdgeParam:
			if int(e.From) < len(o.snap.Params) && int(e.To) < len(o.snap.Endpoints) {
				line = fmt.Sprintf("  %-9s %s -> %s", wmse.EdgeName(e.Kind),
					o.snap.Params[e.From].Name, o.snap.Endpoints[e.To].Path)
			}
		}
		if line == "" {
			continue
		}
		if e.Weight > 1 {
			line += fmt.Sprintf("  (x%d)", e.Weight)
		}
		if !o.out(line) {
			return
		}
	}
}
