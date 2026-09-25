package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"apimap/internal/categorizer"
	"apimap/internal/color"
	"apimap/internal/config"
	"apimap/internal/contract"
	"apimap/internal/emulator"
	"apimap/internal/fetcher"
	"apimap/internal/graph"
	"apimap/internal/jsanalyzer"
	"apimap/internal/linker"
	"apimap/internal/markdown"
	"apimap/internal/patterns"
	"apimap/internal/progress"
	"apimap/internal/urlgroup"
)

var activePatterns *patterns.PatternSet

var lastEmuSummary *emulator.Summary

// activeGroups holds the URL patterns confirmed over the whole scan. It feeds
// both the "URL Patterns" output section and the API-contract canonicalization
// (concrete URLs matching a pattern are folded into the pattern endpoint).
var activeGroups []urlgroup.Group

// obsPool accumulates request observations from static JS analysis and browser
// emulation across the whole scan; contract.Infer turns it into endpoint
// contracts at the end.
var (
	obsMu   sync.Mutex
	obsPool []contract.Observation
)

// poolObs merges freshly collected observations into the global pool.
func poolObs(_ bool, obs []contract.Observation) {
	obsMu.Lock()
	defer obsMu.Unlock()
	obsPool = append(obsPool, obs...)
}

// fragMu guards the discovered runtime search-parameter names. These come from
// URLSearchParams-style .set()/.append() builders in raw bundles and reveal
// which query keys dynamic endpoints are assembled with.
var (
	fragMu       sync.Mutex
	fragParams   []string
	fragParamSet map[string]bool
)

// poolFragments collects runtime query-parameter names so a single
// "Dynamic query params" block can be printed at the end of the scan.
func poolFragments(params []string) {
	if len(params) == 0 {
		return
	}
	fragMu.Lock()
	defer fragMu.Unlock()
	if fragParamSet == nil {
		fragParamSet = make(map[string]bool)
	}
	for _, p := range params {
		if !fragParamSet[p] {
			fragParamSet[p] = true
			fragParams = append(fragParams, p)
		}
	}
}

// renderFragmentFindings prints the runtime endpoint-formation findings that
// raw-bundle analysis recovered (dynamic URL templates and query-param names).
func renderFragmentFindings(cfg *config.Config) {
	fragMu.Lock()
	params := append([]string(nil), fragParams...)
	fragMu.Unlock()
	if len(params) == 0 {
		return
	}
	sort.Strings(params)
	if cfg.Color {
		fmt.Println(color.Colorize(color.Bold, "\n=== Dynamic query params ==="))
	} else {
		fmt.Println("\n=== Dynamic query params ===")
	}
	for _, p := range params {
		fmt.Printf("  %s\n", p)
	}
}

// renderContractSection prints the inferred API contract section.
func renderContractSection(cfg *config.Config) {
	obsMu.Lock()
	obs := append([]contract.Observation(nil), obsPool...)
	obsMu.Unlock()
	if len(obs) == 0 {
		return
	}
	obs = canonizeObservations(obs)
	eps := contract.Infer(obs)
	if len(eps) == 0 {
		return
	}
	if cfg.Color {
		fmt.Println(color.Colorize(color.Bold, "\n=== API Contracts ==="))
	} else {
		fmt.Println("\n=== API Contracts ===")
	}
	if cfg.Color {
		fmt.Print(contract.RenderColored(eps, cfg.APIContractRaw))
	} else {
		fmt.Print(contract.Render(eps, cfg.APIContractRaw))
	}
}

// canonizeObservations rewrites observation URLs that match a confirmed URL
// pattern to the pattern itself, so concrete instances (/complex/9223/contacts
// and /complex/9224/contacts) collapse into one contract endpoint
// (/complex/{id}/contacts).
func canonizeObservations(obs []contract.Observation) []contract.Observation {
	if len(activeGroups) == 0 {
		return obs
	}
	out := append([]contract.Observation(nil), obs...)
	for i := range out {
		if p := urlgroup.MatchURL(out[i].URL, activeGroups); p != "" {
			out[i].URL = p
		}
	}
	return out
}

// Emulation resource controls, initialized from config when -emulate is set.
var (
	emuLimits emulator.Limits
	emuSem    chan struct{} // bounds concurrent page emulations (memory/CPU)
)

func initEmulationLimits(cfg *config.Config) {
	workers := cfg.EmulateWorkers
	if workers <= 0 {
		workers = min(4, runtime.NumCPU())
	}
	if workers < 1 {
		workers = 1
	}
	emuSem = make(chan struct{}, workers)
	emuLimits = emulator.Limits{
		Timeout:      time.Duration(cfg.EmulateTimeout) * time.Millisecond,
		MaxJS:        cfg.EmulateMaxJS * 1024,
		MaxJobs:      cfg.EmulateMaxJobs,
		MaxAbandoned: cfg.EmulateMaxLeaks,
	}
}

func main() {
	cfg := config.Parse()
	if cfg.URL == "" {
		fmt.Fprintln(os.Stderr, "Error: URL is required")
		os.Exit(1)
	}

	activePatterns = patterns.Defaults()
	for _, preset := range cfg.Presets {
		switch preset {
		case "bitrix":
			patterns.Merge(activePatterns, patterns.Bitrix())
		case "wp":
			patterns.Merge(activePatterns, patterns.WordPress())
		case "react":
			patterns.Merge(activePatterns, patterns.ReactSPA())
		}
	}
	for _, pf := range cfg.PatternFiles {
		if pf == "bitrix" {
			patterns.Merge(activePatterns, patterns.Bitrix())
			continue
		}
		if pf == "wp" {
			patterns.Merge(activePatterns, patterns.WordPress())
			continue
		}
		if pf == "react" {
			patterns.Merge(activePatterns, patterns.ReactSPA())
			continue
		}
		p, err := patterns.LoadFile(pf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: load patterns %s: %v\n", pf, err)
			continue
		}
		patterns.Merge(activePatterns, p)
	}
	linker.SetPatterns(activePatterns.APIPatterns, activePatterns.AssetPatterns, activePatterns.WAFPatterns, activePatterns.CDNDomains, activePatterns.CachePatterns, activePatterns.AssetExtensions)

	if cfg.Color && len(cfg.Presets) == 0 && len(cfg.PatternFiles) == 0 {
		fmt.Fprintln(os.Stderr, patterns.MessageOfTheDay())
	}

	if cfg.Emulate {
		initEmulationLimits(cfg)
	}

	f, err := fetcher.New(cfg.URL, cfg.SkipTLS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	applyRequestHeaders(f, cfg)

	progressDone := make(chan struct{})
	p := progress.New(1)
	p.Start(progressDone)
	p.AddTarget(1)

	if !cfg.Recursive {
		result, err := f.Fetch("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		allLinks := linker.Parse(result.Body, result.URL)
		enrichLinks(allLinks, f)
		if cfg.AnalyzeJS {
			if isJSURL(result.URL) {
				jsLinks, jsObs := jsanalyzer.Parse(result.Body, result.URL, cfg.APIFull)
				enrichLinks(jsLinks, f)
				fragLinks, fragParams := linker.AnalyzeJS(result.Body, result.URL)
				enrichLinks(fragLinks, f)
				allLinks = append(allLinks, jsLinks...)
				allLinks = append(allLinks, fragLinks...)
				poolObs(true, jsObs)
				poolFragments(fragParams)
			}
			ct := result.Headers.Get("Content-Type")
			if strings.Contains(ct, "html") || strings.Contains(ct, "text/html") {
				var combined string
				for _, inline := range extractInlineScripts(result.Body) {
					combined += inline + "\n"
				}
				if combined != "" {
					jsLinks, jsObs := jsanalyzer.Parse(combined, result.URL, cfg.APIFull)
					enrichLinks(jsLinks, f)
					fragLinks, fragParams := linker.AnalyzeJS(combined, result.URL)
					enrichLinks(fragLinks, f)
					allLinks = append(allLinks, jsLinks...)
					allLinks = append(allLinks, fragLinks...)
					poolObs(true, jsObs)
					poolFragments(fragParams)
				}
			}
		}
		if cfg.Emulate {
			emuLinks := emulatePage(result.Body, result.URL, f)
			allLinks = append(allLinks, emuLinks...)
			lastEmuSummary = aggregatedEmuSummary()
		}
		displayLinks := filterHidden(allLinks, cfg)
		displayLinks = groupParamLinks(displayLinks)
		sortLinks(displayLinks)
		displayLinks = mergeAPIDetails(displayLinks)
		displayLinks = applyGrouping(allLinks, displayLinks, cfg)
		printResults(allLinks, displayLinks, activeGroups, cfg)
		if cfg.APIContract {
			renderContractSection(cfg)
		}
		renderFragmentFindings(cfg)

		p.IncrementRequest()
		p.Increment()
		close(progressDone)
		p.Finish()

		if cfg.Graphical {
			graphDir := "graph.wmap"
			g := graph.New(allLinks, cfg.URL)
			g.ShowWAF = cfg.WAF
			g.ShowCDN = cfg.CDN
			g.ShowCache = cfg.Cache
			g.ShowNoise = cfg.Noise
			if err := g.SaveToDirectory(graphDir); err != nil {
				fmt.Fprintf(os.Stderr, "Graph error: %v\n", err)
			} else {
				fmt.Printf("Graph saved: %s/index.html\n", graphDir)
			}
		}
		return
	}

	depth := cfg.RecursiveDepth
	if depth < 1 {
		depth = 3
	}

	allLinks := crawl(f, cfg, depth, p, cfg.Threads, cfg.RequestLimit, cfg.AllDomains)
	if cfg.Emulate {
		// Per-page emulation happened inside the crawl; collect the aggregate.
		lastEmuSummary = aggregatedEmuSummary()
	}
	displayLinks := filterHidden(allLinks, cfg)
	displayLinks = groupParamLinks(displayLinks)
	sortLinks(displayLinks)
	displayLinks = mergeAPIDetails(displayLinks)
	displayLinks = applyGrouping(allLinks, displayLinks, cfg)

	close(progressDone)
	p.Finish()

	printResults(allLinks, displayLinks, activeGroups, cfg)
	if cfg.APIContract {
		renderContractSection(cfg)
	}
	renderFragmentFindings(cfg)

	if cfg.Markdown || cfg.Graphical {
		g := graph.New(allLinks, cfg.URL)
		g.ShowWAF = cfg.WAF
		g.ShowCDN = cfg.CDN
		g.ShowCache = cfg.Cache
		g.ShowNoise = cfg.Noise
		hasGraph := cfg.Graphical

		if cfg.Markdown {
			stats := categorizer.Calculate(allLinks, cfg.ShowTags)
			dirName := markdown.DirectoryName(cfg.URL)
			markdown.Generate(dirName, cfg.URL, allLinks, stats, cfg, hasGraph)
			fmt.Printf("\nMarkdown generated: %s/\n", dirName)
		}

		if cfg.Graphical {
			graphDir := "graph.wmap"
			if cfg.Markdown {
				dirName := markdown.DirectoryName(cfg.URL)
				graphDir = filepath.Join(dirName, "graph.wmap")
			}
			if err := g.SaveToDirectory(graphDir); err != nil {
				fmt.Fprintf(os.Stderr, "Graph error: %v\n", err)
			} else {
				fmt.Printf("Graph saved: %s/index.html\n", graphDir)
			}
		}
	}
}

func isClassHidden(class linker.URLClass, cfg *config.Config) bool {
	switch class {
	case linker.ClassWAF:
		return !cfg.WAF
	case linker.ClassCDN:
		return !cfg.CDN
	case linker.ClassCache:
		return !cfg.Cache
	case linker.ClassNoise:
		return !cfg.Noise
	}
	return false
}

func splitQuery(rawURL string) (base string, query string) {
	idx := strings.IndexByte(rawURL, '?')
	if idx < 0 {
		return rawURL, ""
	}
	rest := rawURL[idx+1:]
	if rest == "" {
		return rawURL, ""
	}
	return rawURL[:idx], "?" + rest
}

func removeQuery(rawURL string) string {
	base, _ := splitQuery(rawURL)
	return base
}

func formatParamVariants(variants []linker.ParamVariant) string {
	if len(variants) == 0 {
		return ""
	}
	paramValues := make(map[string]map[string]bool)
	seenQueries := make(map[string]bool)
	for _, v := range variants {
		q := v.Query
		if seenQueries[q] {
			continue
		}
		seenQueries[q] = true
		q = strings.TrimPrefix(q, "?")
		pairs := strings.Split(q, "&")
		for _, pair := range pairs {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 && kv[0] != "" {
				if paramValues[kv[0]] == nil {
					paramValues[kv[0]] = make(map[string]bool)
				}
				if kv[1] != "" {
					paramValues[kv[0]][kv[1]] = true
				}
			}
		}
	}
	var parts []string
	for name, vals := range paramValues {
		if len(vals) > 0 {
			valList := make([]string, 0, len(vals))
			for v := range vals {
				valList = append(valList, v)
			}
			sort.Strings(valList)
			if len(valList) <= 5 {
				parts = append(parts, fmt.Sprintf("%s=%s", name, strings.Join(valList, ",")))
			} else {
				parts = append(parts, fmt.Sprintf("%s=%s+...", name, strings.Join(valList[:5], ",")))
			}
		} else {
			parts = append(parts, name)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// applyGrouping folds discovered URLs into patterns, drops pattern members
// from the displayed link list, and records the groups for the pattern and
// API-contract output. Grouping is skipped when disabled via -nogroup.
func applyGrouping(allLinks, displayLinks []linker.Link, cfg *config.Config) []linker.Link {
	if cfg.NoGroup {
		return displayLinks
	}
	activeGroups = urlgroup.BuildLinks(allLinks, cfg.GroupCount)
	return urlgroup.WithoutMembers(displayLinks, activeGroups)
}

// renderURLPatternsPlain builds the "URL Patterns" section: each line is one
// annotation cluster "(type: name)" followed by the comma-joined patterns that
// share it, with instance counts and the merged variable ranges.
func renderURLPatternsPlain(groups []urlgroup.Group) string {
	if len(groups) == 0 {
		return ""
	}
	clusters := map[string][]urlgroup.Group{}
	var order []string
	for _, g := range groups {
		ann := g.Annotation()
		if _, ok := clusters[ann]; !ok {
			order = append(order, ann)
		}
		clusters[ann] = append(clusters[ann], g)
	}
	sort.Strings(order)

	var b strings.Builder
	b.WriteString("\n=== URL Patterns ===\n")
	for _, ann := range order {
		gs := clusters[ann]
		var parts []string
		for _, g := range gs {
			parts = append(parts, fmt.Sprintf("%s (%d)", g.Pattern, g.Count))
		}
		line := "  " + ann + " " + strings.Join(parts, ", ")
		if ranges := urlgroup.ClusterRanges(gs); len(ranges) > 0 {
			line += "  [" + strings.Join(ranges, ", ") + "]"
		}
		b.WriteString(wrapLine(line, 120) + "\n")
	}
	return b.String()
}

// kindColor maps a grouping kind to its output color.
func kindColor(kind string) color.Code {
	switch kind {
	case "int":
		return color.Green
	case "string":
		return color.Yellow
	case "uuid":
		return color.Purple
	case "hash":
		return color.Cyan
	case "base64":
		return color.DarkPurple
	default:
		return color.White
	}
}

// renderURLPatternsColored is the colorized variant of renderURLPatternsPlain.
func renderURLPatternsColored(groups []urlgroup.Group) string {
	if len(groups) == 0 {
		return ""
	}
	clusters := map[string][]urlgroup.Group{}
	var order []string
	for _, g := range groups {
		ann := g.Annotation()
		if _, ok := clusters[ann]; !ok {
			order = append(order, ann)
		}
		clusters[ann] = append(clusters[ann], g)
	}
	sort.Strings(order)

	var b strings.Builder
	b.WriteString("\n" + color.Colorize(color.Bold, "=== URL Patterns ===") + "\n")
	for _, ann := range order {
		gs := clusters[ann]
		var parts []string
		for _, g := range gs {
			parts = append(parts, color.Colorize(color.White, g.Pattern)+" "+color.Colorizef(color.Dim, "(%d)", g.Count))
		}
		var kp []string
		for i, v := range gs[0].Vars {
			kind := v.Kind
			if kind == "" {
				kind = "string"
			}
			kp = append(kp, color.Colorize(kindColor(kind), kind)+color.Colorize(color.Bold, ": "+urlgroup.VarLabel(i)))
		}
		coloredAnn := "(" + strings.Join(kp, ", ") + ")"
		line := "  " + coloredAnn + " " + strings.Join(parts, ", ")
		if ranges := urlgroup.ClusterRanges(gs); len(ranges) > 0 {
			var rp []string
			for _, r := range ranges {
				rp = append(rp, color.Colorize(color.Dim, r))
			}
			line += "  [" + strings.Join(rp, ", ") + "]"
		}
		b.WriteString(wrapLine(line, 120) + "\n")
	}
	return b.String()
}

// wrapLine soft-wraps a long line at word boundaries close to the width limit.
func wrapLine(s string, width int) string {
	if len(s) <= width {
		return s
	}
	var b strings.Builder
	cur := 0
	for _, word := range strings.Split(s, " ") {
		if cur > 0 && cur+len(word)+1 > width && cur < width {
			b.WriteString("\n     ")
			cur = 5
		} else if cur > 0 {
			b.WriteString(" ")
			cur++
		}
		b.WriteString(word)
		cur += len(word)
	}
	return b.String()
}

func groupParamLinks(links []linker.Link) []linker.Link {
	type groupedInfo struct {
		link     *linker.Link
		variants []linker.ParamVariant
		seenBase bool
	}
	groups := make(map[string]*groupedInfo)
	result := make([]linker.Link, 0, len(links))

	for _, l := range links {
		resolved := l.Resolved
		if resolved == "" {
			resolved = l.HREF
		}
		base, rawQuery := splitQuery(resolved)
		groupKey := fmt.Sprintf("%s|%d", base, l.Class)

		if rawQuery == "" {
			if existing, ok := groups[groupKey]; ok {
				existing.seenBase = true
				existing.link.HREF = l.HREF
				existing.link.Resolved = l.Resolved
				existing.link.SourceURL = l.SourceURL
				existing.link.Tag = l.Tag
				existing.link.APIDetails = l.APIDetails
			} else {
				result = append(result, l)
			}
			continue
		}

		if g, ok := groups[groupKey]; ok {
			g.variants = append(g.variants, linker.ParamVariant{Query: rawQuery})
			if !g.seenBase {
				g.link.ParamVariants = g.variants
			}
		} else {
			parent := l
			parent.HREF = removeQuery(l.HREF)
			parent.Resolved = base
			parent.HasParams = false
			parent.ParamVariants = []linker.ParamVariant{{Query: rawQuery}}
			groups[groupKey] = &groupedInfo{
				link:     &parent,
				variants: []linker.ParamVariant{{Query: rawQuery}},
			}
		}
	}

	for _, g := range groups {
		g.link.ParamVariants = g.variants
		result = append(result, *g.link)
	}

	return result
}

func enrichLinks(links []linker.Link, f *fetcher.Fetcher) {
	for i := range links {
		if links[i].Resolved == "" {
			// Resolve relative links against the page they were found on, not the
			// global base URL — essential when crawling followed/other domains
			// (and more correct for deep paths on the same domain).
			if base := links[i].SourceURL; base != "" {
				links[i].Resolved = resolveAgainst(base, links[i].HREF)
			}
			if links[i].Resolved == "" {
				links[i].Resolved = f.ResolveURL(links[i].HREF)
			}
		}
		links[i].Domain = extractDomain(links[i].Resolved)
	}
}

// resolveAgainst resolves href relative to pageURL (handles absolute, //host,
// /path, relative and ../ forms). Returns "" if pageURL is not a usable base.
func resolveAgainst(pageURL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil || base == nil || base.Host == "" {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

func filterHidden(links []linker.Link, cfg *config.Config) []linker.Link {
	filtered := make([]linker.Link, 0, len(links))
	for _, l := range links {
		switch l.Class {
		case linker.ClassWAF:
			if !cfg.WAF {
				continue
			}
		case linker.ClassCDN:
			if !cfg.CDN {
				continue
			}
		case linker.ClassCache:
			if !cfg.Cache {
				continue
			}
		case linker.ClassNoise:
			if !cfg.Noise {
				continue
			}
		}
		filtered = append(filtered, l)
	}
	return filtered
}

// applyRequestHeaders parses -H/-b flags into HTTP headers, attaches them to the
// fetcher, and (unless -headers-all-hosts) scopes them to the target and
// -follow domains so auth tokens/cookies don't leak to third-party hosts.
func applyRequestHeaders(f *fetcher.Fetcher, cfg *config.Config) {
	if len(cfg.Headers) == 0 && cfg.Cookie == "" {
		return
	}
	h := http.Header{}
	for _, raw := range cfg.Headers {
		key, val, ok := strings.Cut(raw, ":")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if !ok || key == "" {
			fmt.Fprintf(os.Stderr, "Warning: ignoring malformed -H %q (expected \"Key: Value\")\n", raw)
			continue
		}
		h.Add(key, val)
	}
	if cfg.Cookie != "" {
		if existing := h.Get("Cookie"); existing != "" {
			h.Set("Cookie", existing+"; "+cfg.Cookie)
		} else {
			h.Set("Cookie", cfg.Cookie)
		}
	}
	if len(h) == 0 {
		return
	}
	f.SetHeaders(h)

	if cfg.HeadersAllHosts {
		return
	}
	scope := append([]string{stripPort(extractDomain(cfg.URL))}, cfg.FollowDomains...)
	f.SetHeaderScope(func(host string) bool { return hostInFollowList(host, scope) })
}

func stripPort(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// hostInFollowList reports whether host matches any -follow domain, either
// exactly or as a subdomain (so "example.com" also matches "api.example.com").
// A leading port is stripped before comparison.
func hostInFollowList(host string, follow []string) bool {
	if len(follow) == 0 {
		return false
	}
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, d := range follow {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func extractDomain(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

func sortLinks(links []linker.Link) {
	sort.Slice(links, func(i, j int) bool {
		if links[i].Class != links[j].Class {
			return links[i].Class < links[j].Class
		}
		if links[i].Category != links[j].Category {
			return links[i].Category < links[j].Category
		}
		if links[i].Domain != links[j].Domain {
			return links[i].Domain < links[j].Domain
		}
		return links[i].HREF < links[j].HREF
	})
}

func mergeAPIDetails(links []linker.Link) []linker.Link {
	byHREF := make(map[string]*linker.Link)
	result := make([]linker.Link, 0, len(links))
	for i := range links {
		key := links[i].HREF
		if existing, ok := byHREF[key]; ok {
			if len(links[i].APIDetails) > 0 {
				existing.APIDetails = append(existing.APIDetails, links[i].APIDetails...)
			}
			if len(links[i].ParamVariants) > 0 {
				existing.ParamVariants = append(existing.ParamVariants, links[i].ParamVariants...)
			}
		} else {
			byHREF[key] = &links[i]
			result = append(result, links[i])
		}
	}
	return result
}

func crawl(f *fetcher.Fetcher, cfg *config.Config, maxDepth int, p *progress.ProgressBar, maxWorkers int, requestLimit int, allDomains bool) []linker.Link {
	if maxDepth < 1 {
		maxDepth = 3
	}
	if maxWorkers < 1 {
		maxWorkers = 32
	}

	type pageTask struct {
		url   string
		depth int
	}

	type resultWithTask struct {
		task        pageTask
		links       []linker.Link
		contentType string
	}

	sem := make(chan struct{}, maxWorkers)
	taskCh := make(chan pageTask, 100)
	resultCh := make(chan resultWithTask, 100)

	visited := make(map[string]bool)
	seenLinks := make(map[string]bool)
	allLinks := []linker.Link{}
	pendingURLs := []pageTask{{cfg.URL, 0}}

	// Detector confirms URL patterns incrementally across crawl batches; URLs
	// absorbed by a confirmed pattern are dropped from the crawl queue.
	var detector *urlgroup.Detector
	if !cfg.NoGroup {
		detector = urlgroup.NewDetector(cfg.GroupCount)
	}

	totalRequests := 0

	for i := 0; i < maxWorkers; i++ {
		go func() {
			for task := range taskCh {
				sem <- struct{}{}
				url := task.url
				result, _ := f.Fetch(url)
				p.IncrementRequest()
				var links []linker.Link
				contentType := ""
				if result != nil {
					contentType = result.Headers.Get("Content-Type")
					links = linker.Parse(result.Body, url)
					enrichLinks(links, f)
					if cfg.AnalyzeJS {
						if isJSURL(url) {
							jsLinks, jsObs := jsanalyzer.Parse(result.Body, url, cfg.APIFull)
							enrichLinks(jsLinks, f)
							fragLinks, fragParams := linker.AnalyzeJS(result.Body, url)
							enrichLinks(fragLinks, f)
							links = append(links, jsLinks...)
							links = append(links, fragLinks...)
							poolObs(true, jsObs)
							poolFragments(fragParams)
						}
						if strings.Contains(contentType, "html") || strings.Contains(contentType, "text/html") {
							var combined string
							for _, inline := range extractInlineScripts(result.Body) {
								combined += inline + "\n"
							}
							if combined != "" {
								jsLinks, jsObs := jsanalyzer.Parse(combined, url, cfg.APIFull)
								enrichLinks(jsLinks, f)
								fragLinks, fragParams := linker.AnalyzeJS(combined, url)
								enrichLinks(fragLinks, f)
								links = append(links, jsLinks...)
								links = append(links, fragLinks...)
								poolObs(true, jsObs)
								poolFragments(fragParams)
							}
						}
					}
					if cfg.Emulate && (strings.Contains(contentType, "html") || strings.Contains(contentType, "text/html")) {
						base := url
						if result.URL != "" {
							base = result.URL
						}
						emuLinks := emulatePage(result.Body, base, f)
						links = append(links, emuLinks...)
					}
				}
				<-sem
				resultCh <- resultWithTask{task, links, contentType}
			}
		}()
	}

	contentTypes := make(map[string]string)
	active := 0

	var batchCandidates []string
	candidateSeen := map[string]bool{}

	for len(pendingURLs) > 0 || active > 0 {
		for len(pendingURLs) > 0 && len(sem) < maxWorkers {
			canStart := requestLimit <= 0 || totalRequests < requestLimit
			if !canStart {
				break
			}
			next := pendingURLs[0]
			pendingURLs = pendingURLs[1:]
			taskCh <- next
			active++
			totalRequests++
		}

		if active == 0 {
			break
		}

		rt := <-resultCh
		active--
		p.Increment()

		if rt.contentType != "" {
			contentTypes[rt.task.url] = rt.contentType
		}

		for _, link := range rt.links {
			key := link.HREF
			if seenLinks[key] {
				continue
			}
			seenLinks[key] = true
			allLinks = append(allLinks, link)

			if isClassHidden(link.Class, cfg) {
				continue
			}
			shouldCrawl := link.Category == linker.CategoryWebPage || (cfg.AnalyzeJS && link.Category == linker.CategoryWebAsset && isJSURL(link.HREF))
			if !shouldCrawl {
				continue
			}
			resolved := link.Resolved
			if resolved == "" {
				resolved = f.ResolveURL(link.HREF)
			}
			if resolved == "" || visited[resolved] || rt.task.depth >= maxDepth {
				continue
			}
			isAllowed := allDomains
			if !isAllowed {
				baseURL := f.GetBaseURL()
				if baseURL == nil {
					continue
				}
				baseURLParsed, _ := url.Parse(baseURL.String())
				targetURLParsed, _ := url.Parse(resolved)
				if baseURLParsed == nil || targetURLParsed == nil {
					continue
				}
				isAllowed = baseURLParsed.Host == targetURLParsed.Host ||
					hostInFollowList(targetURLParsed.Host, cfg.FollowDomains)
			}
			if !isAllowed {
				continue
			}
			if !candidateSeen[resolved] {
				batchCandidates = append(batchCandidates, resolved)
				candidateSeen[resolved] = true
			}
		}

		// Drop URLs that a now-confirmed pattern absorbs from this batch's
		// crawl queue so the pattern's instances are not fetched individually.
		blocked := map[string]bool{}
		if detector != nil && len(batchCandidates) > 0 {
			for _, u := range detector.Feed(batchCandidates) {
				blocked[u] = true
			}
		}
		for _, resolved := range batchCandidates {
			if blocked[resolved] {
				continue
			}
			pendingURLs = append(pendingURLs, pageTask{resolved, rt.task.depth + 1})
			visited[resolved] = true
			p.AddTarget(1)
		}
		batchCandidates = batchCandidates[:0]
		candidateSeen = map[string]bool{}
	}

	close(taskCh)
	close(resultCh)

	for i := range allLinks {
		resolved := allLinks[i].Resolved
		if resolved == "" {
			resolved = f.ResolveURL(allLinks[i].HREF)
		}
		if ct, ok := contentTypes[resolved]; ok && strings.Contains(ct, "json") {
			allLinks[i].Category = linker.CategoryAPI
		}
	}

	return allLinks
}

func applyCustomTags(links []linker.Link) {
	if activePatterns == nil || len(activePatterns.Tags) == 0 {
		return
	}
	for i := range links {
		u := links[i].Resolved
		if u == "" {
			u = links[i].HREF
		}
		if t := activePatterns.MatchTag(u); t != "" {
			links[i].Tag = t
		}
	}
}

func printResults(allLinks, displayLinks []linker.Link, groups []urlgroup.Group, cfg *config.Config) {
	applyCustomTags(allLinks)
	applyCustomTags(displayLinks)
	stats := categorizer.Calculate(allLinks, cfg.ShowTags)

	if cfg.Color {
		printColorResults(allLinks, displayLinks, stats, groups, cfg)
	} else {
		printPlainResults(allLinks, displayLinks, stats, groups, cfg)
	}
	printEmulationSummary(cfg)
}

func printPlainResults(allLinks, displayLinks []linker.Link, stats *categorizer.Stats, groups []urlgroup.Group, cfg *config.Config) {
	baseHost := extractDomain(cfg.URL)

	scope := "Same Domain Only"
	if cfg.AllDomains {
		scope = "All Domains"
	}
	fmt.Printf("=== WebMap Analysis (%s) ===\n", scope)
	fmt.Printf("Total links: %d\n\n", stats.Total)

	fmt.Println("By Category:")
	for cat, count := range stats.ByCategory {
		fmt.Printf("  %s: %d\n", cat.String(), count)
	}

	fmt.Println("\nBy Link Type:")
	for lt, count := range stats.ByLinkType {
		fmt.Printf("  %s: %d\n", lt.String(), count)
	}

	fmt.Printf("\nWith query params: %d\n", stats.WithParams)

	fmt.Println("\nBy URL Class:")
	classOrder := []linker.URLClass{linker.ClassNormal, linker.ClassWAF, linker.ClassCDN, linker.ClassCache, linker.ClassNoise}
	for _, c := range classOrder {
		count := stats.ByClass[c]
		switch c {
		case linker.ClassNormal:
			fmt.Printf("  %s: %d\n", c.String(), count)
		case linker.ClassWAF:
			note := ""
			if !cfg.WAF {
				note = " (hidden — use -waf to show)"
			}
			fmt.Printf("  %s: %d%s\n", c.String(), count, note)
		case linker.ClassCDN:
			note := ""
			if !cfg.CDN {
				note = " (hidden — use -cdn to show)"
			}
			fmt.Printf("  %s: %d%s\n", c.String(), count, note)
		case linker.ClassCache:
			note := ""
			if !cfg.Cache {
				note = " (hidden — use -cache to show)"
			}
			fmt.Printf("  %s: %d%s\n", c.String(), count, note)
		case linker.ClassNoise:
			note := ""
			if !cfg.Noise {
				note = " (hidden — use -noise to show)"
			}
			fmt.Printf("  %s: %d%s\n", c.String(), count, note)
		}
	}

	if !cfg.Graphical && !cfg.Markdown && len(displayLinks) > 0 {
		fmt.Println("\n=== All Links ===")
		for _, link := range displayLinks {
			domain := link.Domain
			if domain == "" {
				domain = baseHost
			}
			catStr := link.Category.String()
			if cfg.ShowTags && link.Tag != "" {
				catStr = link.Tag
			}
			typeStr := link.LinkType.String()

			paramsInfo := ""
			if len(link.ParamVariants) > 0 {
				if p := formatParamVariants(link.ParamVariants); p != "" {
					paramsInfo = " (params: " + p + ")"
				} else {
					paramsInfo = " (params)"
				}
			} else if link.HasParams {
				paramsInfo = " (params)"
			}

			classTag := ""
			if link.Class != linker.ClassNormal {
				classTag = " [" + link.Class.String() + "]"
			}

			if link.Category == linker.CategoryAPI && cfg.APIFull && len(link.APIDetails) > 0 {
				seen := make(map[string]bool)
				for _, d := range link.APIDetails {
					method := d.HTTPMethod
					if method == "" {
						method = "ANY"
					}
					key := method + d.Arguments
					if seen[key] {
						continue
					}
					seen[key] = true
					args := cleanArgs(d.Arguments)
					if args != "" {
						fmt.Printf("  %-6s %-18s args: %s%s%s\n", method, link.HREF, args, paramsInfo, classTag)
					} else {
						fmt.Printf("  %-6s %-18s%s%s\n", method, link.HREF, paramsInfo, classTag)
					}
				}
			} else {
				fmt.Printf("  %-6s %-14s %-22s %-36s %s%s%s\n", typeStr, catStr, domain, link.HREF, link.Resolved, paramsInfo, classTag)
			}
		}
		fmt.Println(strings.Repeat("-", 100))
	}

	fmt.Println("\n=== Hidden Classes ===")
	hasHidden := false
	if n := stats.ByClass[linker.ClassWAF]; n > 0 && !cfg.WAF {
		fmt.Printf("  WAF:  %d URL(s) hidden (-waf to show)\n", n)
		hasHidden = true
	}
	if n := stats.ByClass[linker.ClassCDN]; n > 0 && !cfg.CDN {
		fmt.Printf("  CDN:  %d URL(s) hidden (-cdn to show)\n", n)
		hasHidden = true
	}
	if n := stats.ByClass[linker.ClassCache]; n > 0 && !cfg.Cache {
		fmt.Printf("  Cache: %d URL(s) hidden (-cache to show)\n", n)
		hasHidden = true
	}
	if n := stats.ByClass[linker.ClassNoise]; n > 0 && !cfg.Noise {
		fmt.Printf("  Noise: %d URL(s) hidden (-noise to show)\n", n)
		hasHidden = true
	}
	if !hasHidden {
		fmt.Println("  none")
	}

	patternsSection := renderURLPatternsPlain(groups)
	if patternsSection != "" {
		fmt.Print(patternsSection)
	}
}

func printColorResults(allLinks, displayLinks []linker.Link, stats *categorizer.Stats, groups []urlgroup.Group, cfg *config.Config) {
	baseHost := extractDomain(cfg.URL)

	scope := "Same Domain Only"
	if cfg.AllDomains {
		scope = "All Domains"
	}
	fmt.Println(color.Colorize(color.Bold, "=== WebMap Analysis ("+scope+") ==="))

	fmt.Printf("\n%s\n", color.Colorizef(color.Cyan, "Total links: %d", stats.Total))

	fmt.Printf("\n%s\n", color.Colorize(color.Bold, "By Category:"))
	for cat, count := range stats.ByCategory {
		var c color.Code
		switch cat {
		case linker.CategoryAPI:
			c = color.Cyan
		case linker.CategoryDynamic:
			c = color.Purple
		case linker.CategoryWebPage:
			c = color.Green
		case linker.CategoryWebAsset:
			c = color.Yellow
		default:
			c = color.White
		}
		fmt.Printf("  %s\n", color.Colorizef(c, "%s: %d", cat.String(), count))
	}

	fmt.Printf("\n%s\n", color.Colorize(color.Bold, "By Link Type:"))
	for lt, count := range stats.ByLinkType {
		var c color.Code
		switch lt {
		case linker.LinkTypeAbsolute:
			c = color.Cyan
		case linker.LinkTypeRelative:
			c = color.Yellow
		case linker.LinkTypeWeb:
			c = color.DarkYellow
		default:
			c = color.White
		}
		fmt.Printf("  %s\n", color.Colorizef(c, "%s: %d", lt.String(), count))
	}

	fmt.Printf("\n%s\n", color.Colorizef(color.DarkYellow, "With query params: %d", stats.WithParams))

	fmt.Printf("\n%s\n", color.Colorize(color.Bold, "\nBy URL Class:"))
	classOrder := []linker.URLClass{linker.ClassNormal, linker.ClassWAF, linker.ClassCDN, linker.ClassCache, linker.ClassNoise}
	for _, c := range classOrder {
		count := stats.ByClass[c]
		classColor := color.White
		switch c {
		case linker.ClassNormal:
			classColor = color.Green
		case linker.ClassWAF:
			classColor = color.Red
		case linker.ClassCDN:
			classColor = color.Yellow
		case linker.ClassCache:
			classColor = color.DarkYellow
		case linker.ClassNoise:
			classColor = color.DarkGray
		}
		switch c {
		case linker.ClassNormal:
			fmt.Printf("  %s\n", color.Colorizef(classColor, "%s: %d", c.String(), count))
		case linker.ClassWAF:
			note := ""
			if !cfg.WAF {
				note = " (hidden)"
			}
			fmt.Printf("  %s\n", color.Colorizef(classColor, "%s: %d%s", c.String(), count, note))
		case linker.ClassCDN:
			note := ""
			if !cfg.CDN {
				note = " (hidden)"
			}
			fmt.Printf("  %s\n", color.Colorizef(classColor, "%s: %d%s", c.String(), count, note))
		case linker.ClassCache:
			note := ""
			if !cfg.Cache {
				note = " (hidden)"
			}
			fmt.Printf("  %s\n", color.Colorizef(classColor, "%s: %d%s", c.String(), count, note))
		case linker.ClassNoise:
			note := ""
			if !cfg.Noise {
				note = " (hidden)"
			}
			fmt.Printf("  %s\n", color.Colorizef(classColor, "%s: %d%s", c.String(), count, note))
		}
	}

	if !cfg.Graphical && !cfg.Markdown && len(displayLinks) > 0 {
		fmt.Printf("\n%s\n", color.Colorize(color.Bold, "\n=== All Links ==="))

		for _, link := range displayLinks {
			domain := link.Domain
			if domain == "" {
				domain = baseHost
			}
			dt := color.ClassifyDomain(domain, baseHost)

			catStr := link.Category.String()
			if cfg.ShowTags && link.Tag != "" {
				catStr = link.Tag
			}

			var catCode color.Code
			var catIcon string
			switch link.Category {
			case linker.CategoryAPI:
				catCode = color.Cyan
				catIcon = "API"
			case linker.CategoryDynamic:
				catCode = color.Purple
				catIcon = "DYN"
			case linker.CategoryWebPage:
				catCode = color.Green
				catIcon = "PAG"
			case linker.CategoryWebAsset:
				ast := color.ClassifyAsset(link.HREF)
				catCode = color.AssetColor(ast)
				catIcon = strings.ToUpper(color.AssetLabel(ast))
			default:
				catCode = color.White
				catIcon = "?"
			}

			var typeCode color.Code
			switch link.LinkType {
			case linker.LinkTypeAbsolute:
				typeCode = color.Cyan
			case linker.LinkTypeRelative:
				typeCode = color.Yellow
			case linker.LinkTypeWeb:
				typeCode = color.DarkYellow
			default:
				typeCode = color.White
			}

			typeStr := color.Colorizef(typeCode, "%-8s", link.LinkType.String())

			catFormatted := color.Colorizef(catCode, "%-5s %-14s", catIcon, catStr)
			domainFormatted := color.Colorizef(color.DomainColor(dt), "%-22s", domain)
			hrefFormatted := color.Colorizef(color.Dim, "%-36s", truncate(link.HREF, 36))
			resolvedFormatted := color.Colorizef(color.Dim, "%s", link.Resolved)

			paramsInfo := ""
			if len(link.ParamVariants) > 0 {
				if p := formatParamVariants(link.ParamVariants); p != "" {
					paramsInfo = " " + color.Colorizef(color.DarkYellow, "(params: %s)", p)
				} else {
					paramsInfo = " " + color.Colorizef(color.DarkYellow, "(params)")
				}
			} else if link.HasParams {
				paramsInfo = " " + color.Colorizef(color.DarkYellow, "(params)")
			}

			classTag := ""
			if link.Class != linker.ClassNormal {
				var cc color.Code
				switch link.Class {
				case linker.ClassWAF:
					cc = color.Red
				case linker.ClassCDN:
					cc = color.Yellow
				case linker.ClassCache:
					cc = color.DarkYellow
				case linker.ClassNoise:
					cc = color.DarkGray
				}
				classTag = " " + color.Colorizef(cc, "[%s]", link.Class.String())
			}

			if link.Category == linker.CategoryAPI && cfg.APIFull && len(link.APIDetails) > 0 {
				seen := make(map[string]bool)
				for _, d := range link.APIDetails {
					method := d.HTTPMethod
					if method == "" {
						method = "ANY"
					}
					key := method + d.Arguments
					if seen[key] {
						continue
					}
					seen[key] = true
					methodC := color.Colorizef(color.Cyan, "%-6s", method)
					hrefC := color.Colorizef(color.Dim, "%-30s", link.HREF)
					args := cleanArgs(d.Arguments)
					if args != "" {
						argC := color.Colorizef(color.DarkYellow, "args: %s", args)
						fmt.Printf("  %s %s %s%s%s\n", methodC, hrefC, argC, paramsInfo, classTag)
					} else {
						fmt.Printf("  %s %s%s%s\n", methodC, hrefC, paramsInfo, classTag)
					}
				}
			} else {
				fmt.Printf("  %s %s %s %s %s%s%s\n",
					typeStr, catFormatted, domainFormatted, hrefFormatted, resolvedFormatted, paramsInfo, classTag)
			}
		}
	}

	fmt.Println("\n  " + color.Colorizef(color.Red, "Hidden Classes:"))
	hasHidden := false
	if n := stats.ByClass[linker.ClassWAF]; n > 0 && !cfg.WAF {
		fmt.Printf("    WAF:   %d (%s)\n", n, color.Colorizef(color.DarkYellow, "-waf to show"))
		hasHidden = true
	}
	if n := stats.ByClass[linker.ClassCDN]; n > 0 && !cfg.CDN {
		fmt.Printf("    CDN:   %d (%s)\n", n, color.Colorizef(color.DarkYellow, "-cdn to show"))
		hasHidden = true
	}
	if n := stats.ByClass[linker.ClassCache]; n > 0 && !cfg.Cache {
		fmt.Printf("    Cache: %d (%s)\n", n, color.Colorizef(color.DarkYellow, "-cache to show"))
		hasHidden = true
	}
	if n := stats.ByClass[linker.ClassNoise]; n > 0 && !cfg.Noise {
		fmt.Printf("    Noise: %d (%s)\n", n, color.Colorizef(color.DarkYellow, "-noise to show"))
		hasHidden = true
	}
	if !hasHidden {
		fmt.Println("    none")
	}

	patternsSection := renderURLPatternsColored(groups)
	if patternsSection != "" {
		fmt.Print(patternsSection)
	}
}

func cleanArgs(args string) string {
	args = strings.TrimSpace(args)
	args = strings.TrimLeft(args, ",")
	args = strings.TrimSpace(args)
	args = strings.ReplaceAll(args, "\n", " ")
	args = strings.ReplaceAll(args, "\r", "")
	for strings.Contains(args, "  ") {
		args = strings.ReplaceAll(args, "  ", " ")
	}
	if len(args) < 2 || (len(args) < 5 && !strings.ContainsAny(args, "{[/")) {
		return ""
	}
	return truncate(args, 70)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func printEmulationSummary(cfg *config.Config) {
	if !cfg.Emulate || lastEmuSummary == nil {
		return
	}
	s := lastEmuSummary

	scope := "Same Domain Only"
	if cfg.AllDomains {
		scope = "All Domains"
	}

	if cfg.Color {
		fmt.Println(color.Colorize(color.Bold, "\n=== Browser Emulation Results ("+scope+") ==="))
		fmt.Printf("%s\n", color.Colorizef(color.Cyan, "Scripts executed: %d", s.Scripts))
		fmt.Printf("%s\n", color.Colorizef(color.Cyan, "Network calls intercepted: %d", s.Calls))

		typeOrder := []string{"fetch", "xhr", "websocket", "eventsource", "beacon", "image", "script", "link", "iframe", "form", "navigation", "media", "object", "other"}
		for _, t := range typeOrder {
			if n := s.ByType[t]; n > 0 {
				fmt.Printf("  %s\n", color.Colorizef(color.Yellow, "%s: %d", t, n))
			}
		}
		if s.Errors > 0 {
			fmt.Printf("%s\n", color.Colorizef(color.Red, "Script errors: %d", s.Errors))
		}
		if s.Abandoned > 0 {
			fmt.Printf("%s\n", color.Colorizef(color.Red, "Abandoned native hangs: %d (emulation may be disabled)", s.Abandoned))
		}

		if len(s.List) > 0 || s.Errors > 0 {
			fmt.Println()
			for _, c := range s.List {
				typeColor := color.Cyan
				switch c.Type {
				case "fetch":
					typeColor = color.Cyan
				case "xhr":
					typeColor = color.Yellow
				case "websocket":
					typeColor = color.Purple
				case "beacon":
					typeColor = color.DarkYellow
				case "image":
					typeColor = color.Green
				}
				fmt.Printf("  %s %s %s\n",
					color.Colorizef(typeColor, "%-10s", c.Type),
					color.Colorizef(color.Dim, "%-6s", c.Method),
					c.URL)
			}
			for _, e := range lastEmuSummary.LoadErrors() {
				fmt.Printf("  %s%s\n", color.Colorizef(color.Red, "ERROR"), color.Colorizef(color.Dim, "  %s", e))
			}
		}
	} else {
		fmt.Printf("\n=== Browser Emulation Results (%s) ===\n", scope)
		fmt.Printf("Scripts executed: %d\n", s.Scripts)
		fmt.Printf("Network calls intercepted: %d\n", s.Calls)

		typeOrder := []string{"fetch", "xhr", "websocket", "eventsource", "beacon", "image", "script", "link", "iframe", "form", "navigation", "media", "object", "other"}
		for _, t := range typeOrder {
			if n := s.ByType[t]; n > 0 {
				fmt.Printf("  %s: %d\n", t, n)
			}
		}
		if s.Errors > 0 {
			fmt.Printf("Script errors: %d\n", s.Errors)
		}
		if s.Abandoned > 0 {
			fmt.Printf("Abandoned native hangs: %d\n", s.Abandoned)
		}

		if len(s.List) > 0 || s.Errors > 0 {
			fmt.Println()
			for _, c := range s.List {
				fmt.Printf("  %-10s %-6s %s\n", c.Type, c.Method, c.URL)
			}
			for _, e := range lastEmuSummary.LoadErrors() {
				fmt.Printf("  ERROR  %s\n", e)
			}
		}
	}
}

// jsCache memoizes fetched external script bodies so shared bundles are only
// downloaded once across a whole-site crawl.
var jsCache = struct {
	mu sync.Mutex
	m  map[string]string
}{m: map[string]string{}}

func fetchScript(f *fetcher.Fetcher, u string) string {
	jsCache.mu.Lock()
	if body, ok := jsCache.m[u]; ok {
		jsCache.mu.Unlock()
		return body
	}
	jsCache.mu.Unlock()

	// Bound the fetch: a slow/hung script server must not stall an emulation
	// worker (and thus the emuSem-serialized pipeline). Abandon after 8s.
	ch := make(chan string, 1)
	go func() {
		res, err := f.Fetch(u)
		if err == nil && res != nil {
			ch <- res.Body
			return
		}
		ch <- ""
	}()
	body := ""
	select {
	case body = <-ch:
	case <-time.After(8 * time.Second):
		body = ""
	}

	jsCache.mu.Lock()
	jsCache.m[u] = body
	jsCache.mu.Unlock()
	return body
}

// collectPageScripts returns all scripts of a page in document order: inline
// blocks and external src files (fetched), interleaved as they appear. Ordered,
// same-realm execution lets libraries initialize before app code uses them.
func collectPageScripts(pageHTML, pageURL string, f *fetcher.Fetcher) []emulator.Script {
	var scripts []emulator.Script
	rest := pageHTML
	inlineIdx := 0
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx+7:]
		closeIdx := strings.Index(rest, ">")
		if closeIdx < 0 {
			break
		}
		tag := rest[:closeIdx]
		lowerTag := strings.ToLower(tag)

		// Skip non-JS script types (application/json, text/template, etc.).
		isJS := true
		if strings.Contains(lowerTag, "type=") &&
			!strings.Contains(lowerTag, "text/javascript") &&
			!strings.Contains(lowerTag, "type=\"module\"") &&
			!strings.Contains(lowerTag, "type='module'") &&
			!strings.Contains(lowerTag, "application/javascript") {
			isJS = false
		}

		if src := extractSrcAttr(tag); src != "" {
			if isJS {
				resolved := f.ResolveURL(src)
				body := fetchScript(f, resolved)
				trimmed := strings.TrimSpace(body)
				if body != "" && !strings.HasPrefix(trimmed, "<") {
					scripts = append(scripts, emulator.Script{Code: body, URL: resolved})
				}
			}
			rest = rest[closeIdx+1:]
			continue
		}

		endIdx := strings.Index(rest, "</script>")
		if endIdx < 0 {
			break
		}
		content := strings.TrimSpace(rest[closeIdx+1 : endIdx])
		if content != "" && isJS {
			inlineIdx++
			scripts = append(scripts, emulator.Script{
				Code: content,
				URL:  fmt.Sprintf("inline:%s#%d", pageURL, inlineIdx),
			})
		}
		rest = rest[endIdx+9:]
	}
	return scripts
}

func extractSrcAttr(tag string) string {
	for _, q := range []string{`src="`, `src='`} {
		if i := strings.Index(tag, q); i >= 0 {
			r := tag[i+len(q):]
			end := strings.IndexByte(r, q[len(q)-1])
			if end >= 0 {
				return strings.TrimSpace(r[:end])
			}
		}
	}
	return ""
}

// ephemeralParams are query keys used purely for cache-busting; they carry no
// endpoint identity, so we drop them when de-duplicating emulated calls (a
// widget rebuilding "app.js?t=<Date.now()>" on every page must collapse to one).
var ephemeralParams = map[string]bool{
	"t": true, "_": true, "v": true, "ver": true, "version": true,
	"rnd": true, "rand": true, "random": true, "ts": true, "cb": true,
	"nocache": true, "cache": true, "cachebuster": true, "_dc": true, "r": true,
}

func stripEphemeralParams(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.RawQuery == "" {
		return rawURL
	}
	q := u.Query()
	for k := range q {
		if ephemeralParams[k] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// emuAgg accumulates emulation results across all crawled pages for the summary.
var emuAgg = struct {
	mu       sync.Mutex
	scripts  int
	byType   map[string]int
	errList  []string
	errSeen  map[string]bool
	list     []emulator.NetworkCall
	seenCall map[string]bool
}{byType: map[string]int{}, seenCall: map[string]bool{}, errSeen: map[string]bool{}}

// maxEmuErrors caps stored emulation errors so a deep crawl (same script failing
// on every page) can't flood output or memory.
const maxEmuErrors = 40

// normalizeEmuError reduces an error to its type+message, dropping the source
// URL/line, so the same failure across many pages (and many inline scripts)
// dedupes to a single entry.
func normalizeEmuError(e string) string {
	if i := strings.Index(e, " at "); i >= 0 {
		e = e[:i]
	}
	if i := strings.IndexByte(e, '\n'); i >= 0 {
		e = e[:i]
	}
	// Drop the leading "<scriptURL>: " so only the error type+message remains.
	for _, tok := range []string{"TypeError", "ReferenceError", "SyntaxError", "RangeError", "aborted", "skipped", "panic"} {
		if i := strings.Index(e, tok); i >= 0 {
			return strings.TrimSpace(e[i:])
		}
	}
	return strings.TrimSpace(e)
}

// emulatePage runs the full semantic engine over one page and returns the
// discovered endpoints as links attributed to that page.
func emulatePage(pageHTML, pageURL string, f *fetcher.Fetcher) []linker.Link {
	// Collect scripts first: downloading external bundles is network-bound and
	// would idle an emulation slot (and thus the whole pool) behind a slow
	// script server. The slot is then held only during VM execution, which is
	// where the memory/CPU footprint actually comes from.
	scripts := collectPageScripts(pageHTML, pageURL, f)
	if len(scripts) == 0 {
		return nil
	}
	if emuSem != nil {
		emuSem <- struct{}{}
		defer func() { <-emuSem }()
	}

	sb := emulator.NewWithLimits(pageURL, emuLimits)
	sb.SetPageHTML(pageHTML)
	sb.Run(scripts)
	sum := sb.Summary()

	// Requests that carry data feed the API-contract inference pool.
	if len(sum.List) > 0 {
		var obs []contract.Observation
		for _, c := range sum.List {
			switch c.Type {
			case "fetch", "xhr", "beacon", "websocket", "eventsource":
				method := c.Method
				if method == "" {
					method = "GET"
				}
				body := c.Body
				if len(body) > 4096 {
					body = body[:4096]
				}
				var headers []contract.NameValue
				for _, h := range c.Headers {
					headers = append(headers, contract.NameValue{Name: h.Name, Value: h.Value})
				}
				obs = append(obs, contract.Observation{URL: c.URL, Method: method, Headers: headers, Body: body})
			}
		}
		if len(obs) > 0 {
			poolObs(true, obs)
		}
	}

	emuAgg.mu.Lock()
	emuAgg.scripts += sum.Scripts
	for t, n := range sum.ByType {
		emuAgg.byType[t] += n
	}
	for _, e := range sum.ErrorList {
		if len(emuAgg.errList) >= maxEmuErrors {
			break
		}
		key := normalizeEmuError(e)
		if emuAgg.errSeen[key] {
			continue
		}
		emuAgg.errSeen[key] = true
		emuAgg.errList = append(emuAgg.errList, e)
	}
	emuAgg.mu.Unlock()

	var emuLinks []linker.Link
	for _, c := range sum.List {
		norm := stripEphemeralParams(c.URL)
		emuAgg.mu.Lock()
		dup := emuAgg.seenCall[norm]
		if !dup {
			emuAgg.seenCall[norm] = true
			nc := c
			nc.URL = norm
			emuAgg.list = append(emuAgg.list, nc)
		}
		emuAgg.mu.Unlock()
		if dup {
			continue
		}
		// Resource loads (script/image/link/media) are part of the web map but
		// are not API endpoints; only sinks that carry data are tagged API.
		cat := linker.CategoryAPI
		switch c.Type {
		case "script", "image", "link", "media", "object", "navigation", "iframe":
			cat = linker.CategoryWebAsset
		}
		emuLinks = append(emuLinks, linker.Link{
			HREF:      norm,
			Resolved:  norm,
			Category:  cat,
			LinkType:  linker.LinkTypeAbsolute,
			Class:     linker.ClassNormal,
			Tag:       "emu:" + c.Type,
			SourceURL: pageURL,
		})
	}
	return emuLinks
}

// aggregatedEmuSummary builds a combined summary from all emulated pages.
func aggregatedEmuSummary() *emulator.Summary {
	emuAgg.mu.Lock()
	defer emuAgg.mu.Unlock()
	if emuAgg.scripts == 0 && len(emuAgg.list) == 0 {
		return nil
	}
	byType := make(map[string]int, len(emuAgg.byType))
	for t, n := range emuAgg.byType {
		byType[t] = n
	}
	list := make([]emulator.NetworkCall, len(emuAgg.list))
	copy(list, emuAgg.list)
	errList := make([]string, len(emuAgg.errList))
	copy(errList, emuAgg.errList)
	return &emulator.Summary{
		Scripts:   emuAgg.scripts,
		Calls:     len(list),
		ByType:    byType,
		Errors:    len(errList),
		ErrorList: errList,
		List:      list,
		Abandoned: emulator.AbandonedReports(),
	}
}

func extractInlineScripts(html string) []string {
	var scripts []string
	for {
		idx := strings.Index(html, "<script")
		if idx < 0 {
			break
		}
		html = html[idx+7:]
		srcIdx := strings.Index(html, "src=")
		closeIdx := strings.Index(html, ">")
		if closeIdx < 0 {
			break
		}
		if srcIdx >= 0 && srcIdx < closeIdx {
			html = html[closeIdx+1:]
			continue
		}
		beforeClose := strings.ToLower(html[:closeIdx])
		if strings.Contains(beforeClose, "type=") &&
			!strings.Contains(beforeClose, "type=\"text/javascript\"") &&
			!strings.Contains(beforeClose, "type='text/javascript'") &&
			!strings.Contains(beforeClose, "type=\"module\"") &&
			!strings.Contains(beforeClose, "type='module'") {
			html = html[closeIdx+1:]
			continue
		}
		endIdx := strings.Index(html, "</script>")
		if endIdx < 0 {
			break
		}
		content := html[closeIdx+1 : endIdx]
		content = strings.TrimSpace(content)
		if content != "" {
			scripts = append(scripts, content)
		}
		html = html[endIdx+9:]
	}
	return scripts
}

func isJSURL(u string) bool {
	lower := strings.ToLower(u)
	return strings.HasSuffix(lower, ".js") ||
		strings.HasSuffix(lower, ".mjs") ||
		strings.HasSuffix(lower, ".cjs") ||
		strings.HasSuffix(lower, ".jsx") ||
		strings.HasSuffix(lower, ".ts") ||
		strings.HasSuffix(lower, ".tsx") ||
		strings.HasSuffix(lower, ".mts") ||
		strings.HasSuffix(lower, ".cts")
}
