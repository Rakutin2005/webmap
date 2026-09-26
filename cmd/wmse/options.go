package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"apimap/internal/categorizer"
	"apimap/internal/config"
	"apimap/internal/linker"
	"apimap/internal/urlgroup"
	"apimap/internal/wmse"
)

// options is one read request: the scan's flags, the file, and everything the
// report derives from the two.
type options struct {
	cfg  *config.Config
	snap *wmse.Snapshot
	// info is what the header says about where the data came from. It is the
	// header rather than the open file, because a report does not read the file
	// again - it reads the snapshot - and because a session can report on the
	// union of several files, which no single handle could describe.
	info *wmse.FileInfo
	path string

	// depth is what the tree stops at, which is -rdepth when the reader was
	// given it and the whole tree otherwise.
	depth int

	// target is the entry point the report is about: -url when the reader named
	// one, otherwise the target the file was made from.
	target string
	// domains is the scope - the target's host plus whatever -f/-follow added -
	// and allDomains says whether -a lifted the restriction altogether. Both are
	// only consulted when namedScope is set: without a scope flag the report
	// covers the file, which is what the run it came from reported.
	domains    []string
	allDomains bool
	namedScope bool

	// keep marks the links the report covers: in a URL class the flags do not
	// hide, and inside a named scope. A link outside it is not gone, it is simply
	// not printed, and a view that walks the graph still walks through it.
	keep []bool
	// rows is the link table as the scan would have printed it: the kept links
	// with their query variants folded into one row each, in the scan's order.
	// The file keeps one record per URL, as a scan does, and a table of those
	// reads as a site with more pages than it has.
	rows []linker.Link
	// keptCount is how many of the file's links the flags kept, before folding,
	// so the table can say what it left out without comparing a folded count to
	// an unfolded one.
	keptCount int
	// stats counts the scan's own numbers: over every link the file holds,
	// hidden classes included, because that is what "By URL Class: CDN: 3
	// (hidden - use -cdn to show)" has to be able to say.
	stats *categorizer.Stats

	// left and stopped are the -rlimit budget: one counter for the whole
	// request, seeded from the flag.
	left    int
	stopped bool

	// problems are the flags this file cannot answer, in the order they were
	// found. They are printed before the report and they make the exit status
	// non-zero.
	problems []string
	// headerShown keeps the file's identity block to one per request, whether
	// it was printed as the report's opening or as the answer to a request the
	// file could not fill.
	headerShown bool

	// What the file holds that a flag might have asked for. They are decided in
	// one place, explain, which is what keeps the report and the complaints
	// from ever disagreeing: a section prints exactly when nothing complained
	// about the flag that asks for it, and never otherwise.
	hasContracts    bool
	hasObservations bool
	hasEmulation    bool
	hasRelations    bool
	hasPages        bool
	hasJS           bool
}

// setup resolves the flags against the file: the target to report on, the scope,
// and the set of links the report covers.
func (o *options) setup() {
	o.target = o.cfg.URL
	if o.target == "" {
		o.target = o.snap.Meta[wmse.MetaTarget]
	}
	// The scan reports every link it found, on any domain, and its scope decides
	// what it fetched rather than what it shows. A saved scan cannot fetch
	// anything, so a reader with no scope flags reports the same set the run
	// reported: naming a scope with -url, -a or -f is the reader's way of asking
	// the question the crawl's own scope answered, "what would I have seen?", and
	// it is the only way -f and -url can mean anything offline.
	o.allDomains = o.cfg.AllDomains
	o.namedScope = o.allDomains || len(o.cfg.FollowDomains) > 0 ||
		(o.cfg.URL != "" && o.cfg.URL != o.snap.Meta[wmse.MetaTarget])
	if o.namedScope {
		if !o.allDomains {
			if h := scopeHost(o.target); h != "" {
				o.domains = append(o.domains, h)
			}
			o.domains = append(o.domains, o.cfg.FollowDomains...)
		}
	}
	// A negative budget is not a request for lines; the scan reads it the same
	// way, as no limit at all.
	if o.cfg.RequestLimit < 0 {
		o.cfg.RequestLimit = 0
	}
	o.left = o.cfg.RequestLimit

	// The counts are the scan's own: over every link the file holds, hidden
	// classes included, because that is what "By URL Class: CDN: 3 (hidden - use
	// -cdn to show)" has to be able to say. Filtering first would make a hidden
	// class always read as zero, and the note beside it a lie.
	o.stats = categorizer.Calculate(o.snap.Links, o.cfg.ShowTags)

	// The scan's link table lists what the crawl found, and two kinds of node are
	// not links and are not listed there: the ones the snapshot added to give a
	// relation an endpoint, and the ones a confirmed pattern already speaks for
	// in the pattern section. A table that listed them anyway would show the same
	// URLs twice in two shapes and read as more of the site than there is. The
	// tree is a different question - those pages were fetched - so the filter
	// that decides a table row does not decide what the walk descends through.
	o.keep = make([]bool, len(o.snap.Links))
	shown := make([]linker.Link, 0, len(o.snap.Links))
	for i := range o.snap.Links {
		l := &o.snap.Links[i]
		o.keep[i] = o.classVisible(l) && (!o.namedScope || o.inScope(l))
		if !o.keep[i] || l.Synthesized {
			continue
		}
		shown = append(shown, *l)
	}
	o.keptCount = len(shown)
	// -nogroup is the scan's "do not group", so with it the members stay in the
	// table: the scan shows them when grouping is off, and the table has to be
	// the same table.
	if !o.cfg.NoGroup {
		shown = o.withoutPatternMembers(shown)
	}
	o.rows = linker.MergeAPIDetails(linker.FoldParamLinks(shown))
	linker.SortForDisplay(o.rows)
}

// withoutPatternMembers drops the rows a confirmed pattern already accounts for.
// It is the scan's own rule: the pattern section lists the members as the
// instances of one shape, and a table that also listed them one by one would say
// the same thing twice and make the link count read higher than the site is.
//
// The comparison is on the canonical URL - the query and the fragment dropped -
// because that is the form a member is recorded in, and a link found with a
// query and the same link found without one are the same member.
func (o *options) withoutPatternMembers(links []linker.Link) []linker.Link {
	if len(o.snap.Groups) == 0 {
		return links
	}
	members := make(map[string]bool, len(o.snap.Groups)*2)
	for i := range o.snap.Groups {
		for _, u := range o.snap.Groups[i].Members {
			if c, ok := urlgroup.Canonical(u); ok {
				members[c] = true
			}
		}
	}
	if len(members) == 0 {
		return links
	}
	out := make([]linker.Link, 0, len(links))
	for _, l := range links {
		if c, ok := urlgroup.Canonical(wmse.LinkKey(&l)); ok && members[c] {
			continue
		}
		out = append(out, l)
	}
	return out
}

// inScope reports whether a link is inside the domain scope -a, -f and -url
// select. The rule is the crawl's own: without -a a link counts when it is on
// the entry point's host or on a host that -f/-follow named, subdomains
// included.
func (o *options) inScope(l *linker.Link) bool {
	if o.allDomains {
		return true
	}
	host := scopeHost(wmse.LinkKey(l))
	if host == "" {
		// A link with no host of its own was found on a page that is in
		// scope, so it belongs to the report.
		return true
	}
	if len(o.domains) == 0 {
		// Nothing to compare against: a file with no target and no -f has no
		// domain to report on.
		return false
	}
	return hostInScope(host, o.domains)
}

// classVisible reports whether a URL class is shown. The scan hides WAF, CDN,
// cache and noise links unless asked for them, and a reader that ignored the
// flags would print a report the scan would not have printed.
func (o *options) classVisible(l *linker.Link) bool {
	switch l.Class {
	case linker.ClassWAF:
		return o.cfg.WAF
	case linker.ClassCDN:
		return o.cfg.CDN
	case linker.ClassCache:
		return o.cfg.Cache
	case linker.ClassNoise:
		return o.cfg.Noise
	}
	return true
}

// explain reports the flags this file cannot answer, with the metadata of the
// run that made it.
//
// They are errors rather than empty sections. "No contracts" and "this file was
// never scanned for contracts" look the same on screen and mean opposite things,
// and the second one is the answer to a question the reader cannot otherwise
// answer: what would I have had to run?
//
// The same pass decides what each section is allowed to print, so the report and
// the complaints can never disagree: a section is either there, or it was
// refused here, and it cannot be both or neither.
func (o *options) explain() {
	// A file with nothing in it answers nothing, and saying so once is kinder
	// than a page of per-flag complaints about an empty file.
	if len(o.snap.Links) == 0 {
		return
	}
	if o.keptCount == 0 {
		o.problems = append(o.problems, fmt.Sprintf(
			"no links in this file are in scope for %s: the file has %s",
			scopeOf(o), hostSummary(o)))
	}

	o.hasContracts = len(o.snap.Endpoints) > 0
	o.hasObservations = len(o.snap.Observations) > 0
	o.hasEmulation = o.snap.Emulation != nil
	o.hasRelations = len(o.snap.Edges) > 0
	o.hasPages = len(o.snap.Pages) > 0
	o.hasJS = jsFindings(o.snap)

	// -apic-raw is the evidence, and the evidence implies the contracts it came
	// from; with none of it there are nothing to attach the evidence to, and one
	// complaint is more useful than two.
	contracts := o.cfg.APIContract || o.cfg.APIContractRaw
	if o.cfg.APIContractRaw && !o.hasObservations {
		o.problems = append(o.problems, "-apic-raw: this file holds no request observations"+
			o.runHint("analyze_js"))
	} else if contracts && !o.hasContracts {
		o.problems = append(o.problems, "-apic: this file holds no API contracts"+
			o.runHint("analyze_js"))
	}
	if o.cfg.Emulate && !o.hasEmulation {
		o.problems = append(o.problems, "-emulate: this scan did not emulate any scripts"+
			o.runHint("emulate"))
	}
	if o.cfg.Graphical && !o.hasRelations {
		o.problems = append(o.problems, "-G: this file holds no relations between what it found")
	}
	if o.cfg.Recursive {
		switch {
		case !o.hasPages:
			o.problems = append(o.problems, "-r: this file holds no fetched pages, so there is no crawl to walk")
		case o.depth > o.crawlLimit():
			// The crawl stopped at its own limit, so the tree below it is
			// not in the file. This is the one case where asking for
			// more is a request for data that does not exist: a crawl
			// that reached depth 2 because the site ended at 2 is
			// another matter, and the file says which of the two it was.
			o.problems = append(o.problems, fmt.Sprintf(
				"-rdepth %d: this crawl was allowed depth %d, so there is nothing below that in the file",
				o.depth, o.crawlLimit()))
		}
	}
	if o.cfg.AnalyzeJS && !o.hasJS {
		o.problems = append(o.problems, "-j: this file holds no endpoints recovered from JavaScript"+
			o.runHint("analyze_js"))
	}
	if len(o.problems) == 0 {
		return
	}
	for _, p := range o.problems {
		fmt.Fprintf(os.Stderr, "wmse: %s\n", p)
	}
	// The metadata goes with the complaint: the reader asked with flags, and the
	// answer that helps is the run that would have produced what was asked for.
	viewHeader(o)
}

// runHint names the metadata that explains a missing section, so the reason for
// the absence is in the same breath as the absence.
func (o *options) runHint(keys ...string) string {
	for _, k := range keys {
		if v := o.snap.Meta[k]; v != "" && v != "false" && v != "none" {
			return fmt.Sprintf(" (the scan recorded %s=%s)", k, trunc(v, 60))
		}
	}
	return ""
}

// finish closes the request: a budget that ran out says so, because ending
// without a word reads as "that is all of it".
func (o *options) finish() error {
	if o.stopped {
		fmt.Printf("%s\n", o.dim(fmt.Sprintf("(output cut off: -rlimit %d reached)", o.cfg.RequestLimit)))
	}
	return nil
}

// scopeOf names the scope the report is about, in the scan's own words.
func scopeOf(o *options) string {
	if o.allDomains {
		return "every domain (-a)"
	}
	if len(o.domains) == 0 {
		return "no host in particular"
	}
	if len(o.domains) == 1 {
		return o.domains[0]
	}
	return strings.Join(o.domains, ", ")
}

// hostSummary lists the hosts the file does have, so a wrong scope is obvious.
func hostSummary(o *options) string {
	hosts := hostCounts(o.snap)
	if len(hosts) == 0 {
		return "no hosts at all"
	}
	parts := make([]string, 0, 3)
	for i, h := range hosts {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(hosts)-3))
			break
		}
		parts = append(parts, h.host)
	}
	return strings.Join(parts, ", ")
}

// maxPageDepth is how deep the saved crawl actually went, which is the deepest
// tree there is to print.
func maxPageDepth(snap *wmse.Snapshot) int {
	max := 0
	for _, p := range snap.Pages {
		if p.Depth > max {
			max = p.Depth
		}
	}
	return max
}

// crawlLimit is the depth the saved crawl was allowed to reach, as its metadata
// records it. It is not the same number as maxPageDepth: a crawl that stopped at
// the limit has pages it did not fetch, and a -rdepth past the limit is asking
// for those, while a crawl that ended because the site did is complete however
// deep it went.
//
// A file written before the limit was recorded as a number falls back to what it
// reached, which is the honest guess: the tree will then be refused a level
// deeper than anything it holds, rather than claiming depth it never had.
func (o *options) crawlLimit() int {
	if v := o.snap.Meta[wmse.MetaMaxDepth]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return maxPageDepth(o.snap)
}

// jsFindings reports whether anything in the file came out of JavaScript: a
// bundle was parsed, a sandbox ran, and either of those leaves a mark.
func jsFindings(snap *wmse.Snapshot) bool {
	for i := range snap.Links {
		l := &snap.Links[i]
		if len(l.APIDetails) > 0 {
			return true
		}
		switch l.Tag {
		case "js-import", "url-tmpl":
			return true
		}
	}
	return snap.Emulation != nil
}

// hostInScope reports whether host is one of the scope's domains, either exactly
// or as a subdomain, so "example.com" also covers "api.example.com". The port is
// stripped from both sides: a scope names domains, and the crawl's own rule
// ignored ports too, so a link on the target's non-standard port is in scope.
func hostInScope(host string, domains []string) bool {
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		// A link with no host of its own was found on a page that is in scope,
		// so it belongs to the report.
		return true
	}
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

type hostCount struct {
	host  string
	count int
}

// hostCounts counts the file's links per host, most first. The count is over
// everything the file holds, not over what the flags keep: the hosts a scan
// touched are a fact about the scan, and the classes and the scope only decide
// what gets printed.
func hostCounts(snap *wmse.Snapshot) []hostCount {
	counts := map[string]int{}
	for i := range snap.Links {
		h := snap.Links[i].Domain
		if h == "" {
			// A node added to give a relation an endpoint has no domain field
			// but does have a URL, and calling it "relative" would be a
			// statement about it that is not true. A link with no host at all
			// is the one that is genuinely relative.
			h = hostOf(wmse.LinkKey(&snap.Links[i]))
			if h == "" {
				h = "(relative)"
			}
		}
		counts[h]++
	}
	out := make([]hostCount, 0, len(counts))
	for h, c := range counts {
		out = append(out, hostCount{host: h, count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].host < out[j].host
	})
	return out
}

// count renders "n thing" with the wording a person would use: some nouns take
// their plural in the middle of the phrase rather than at the end, so the caller
// passes both forms when they are not simply word + "s".
func count(n int, one string, many ...string) string {
	form := one
	if n != 1 {
		if len(many) > 0 {
			form = many[0]
		} else {
			form = one + "s"
		}
	}
	return fmt.Sprintf("%d %s", n, form)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// pad cuts a value to a width and pads it back out, so a column keeps its width
// whether the value in it is short or long. A width verb alone only pads: a value
// wider than the column is left wide and pushes everything after it sideways.
func pad(s string, n int) string {
	s = trunc(s, n)
	for len(s) < n {
		s += " "
	}
	return s
}

func oneline(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return trunc(s, n)
}

// shortURL trims a URL to the part that identifies it. A URL on the target
// host keeps only its path, because the host would be the same on every line
// and the paths are what differ. Any other host is kept whole: dropping it
// would make two different documents look like the same one.
func (o *options) shortURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	if o.target != "" && !sameHost(u, o.target) {
		return u
	}
	rest := u[i+3:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[j:]
	}
	return u
}

// hostOf returns the host of a URL including its port, which is what makes two
// URLs the same place: https://x.com/a and https://x.com:8443/a are two
// different places, and a report that merged them would be claiming a path on
// one is a path on the other.
func hostOf(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return ""
	}
	rest := u[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// scopeHost returns the host of a URL without its port, which is what a scope
// compares: -f names domains, and a port is not part of a domain name. It is
// also the rule the crawl itself used, which is why a link found on a
// non-standard port of the target is still in scope.
func scopeHost(u string) string {
	h := hostOf(u)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func sameHost(a, b string) bool {
	return hostOf(a) == hostOf(b)
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1024*1024*1024))
	}
}
