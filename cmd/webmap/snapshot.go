package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"apimap/internal/categorizer"
	"apimap/internal/color"
	"apimap/internal/config"
	"apimap/internal/contract"
	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// Version is the tool version recorded in a saved scan. It is a constant rather
// than a build flag so that a file always names the build that produced it.
const Version = "1.0.0"

// saveSnapshot writes the whole scan to a static-explorer file.
//
// Everything the report prints is included, plus the parts it throws away: the
// pages that were fetched, the raw observations behind each contract, and the
// relations between everything. A file that only kept what the terminal output
// showed would be smaller to write but would have thrown away the evidence,
// and the evidence is the reason to save a scan at all.
func saveSnapshot(path string, cfg *config.Config, allLinks []linker.Link, started time.Time) {
	snap := buildSnapshot(path, cfg, allLinks, started)
	opts := wmse.DefaultOptions()
	if cfg.OutputRaw {
		opts.Compress = false
	}
	info, err := wmse.Write(path, snap, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Snapshot error: %v\n", err)
		return
	}
	fmt.Printf("\nSnapshot saved: %s (%s)\n", path, humanBytes(info.TotalBytes))
	if cfg.Color {
		printSnapshotBreakdown(info, true)
	} else {
		printSnapshotBreakdown(info, false)
	}
	// The file keeps the requests as they were sent, which is the point of
	// keeping them and is also the one way it can hurt the person who made it.
	// The request headers that were configured are stored as names only, but
	// a captured Authorization header, cookie or CSRF token in a request
	// body is evidence and is kept as-is. Saying so once, here, is better than
	// having the file discovered in a shared folder later.
	if snap.Meta[wmse.MetaSessionData] == "true" {
		fmt.Fprintf(os.Stderr, "\nNote: %s holds the requests this scan really sent, which can include session\n"+
			"      material (tokens, cookies, ids). Treat the file as a secret.\n", path)
	}
}

// printSnapshotBreakdown shows where the bytes went. It is the honest answer to
// "is this actually smaller", and it is the only place the format's own layout
// becomes visible to the person who made the file.
func printSnapshotBreakdown(info *wmse.FileInfo, useColor bool) {
	for _, s := range info.Sections {
		if s.StoredLen == 0 && s.RawLen == 0 {
			continue
		}
		ratio := ""
		if s.RawLen > 0 && s.StoredLen != s.RawLen {
			ratio = fmt.Sprintf("  (%.0f%% of %s raw)", 100*float64(s.StoredLen)/float64(s.RawLen), humanBytes(int64(s.RawLen)))
		}
		if useColor {
			fmt.Printf("  %s%-12s%s %10s%s\n", color.Dim, s.Name, color.Reset, humanBytes(int64(s.StoredLen)), ratio)
		} else {
			fmt.Printf("  %-12s %10s%s\n", s.Name, humanBytes(int64(s.StoredLen)), ratio)
		}
	}
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

// buildSnapshot converts the scan's live state into the format's model. It is
// the only place that knows both sides, so the format package stays free of
// the emulator and the report's own types.
func buildSnapshot(path string, cfg *config.Config, allLinks []linker.Link, started time.Time) *wmse.Snapshot {
	obsMu.Lock()
	obs := append([]contract.Observation(nil), obsPool...)
	obsMu.Unlock()
	obs = canonizeObservations(obs)

	snap := &wmse.Snapshot{
		Meta:         scanMeta(cfg, path, started),
		Links:        append([]linker.Link(nil), allLinks...),
		Stats:        snapshotStats(allLinks, cfg),
		Endpoints:    contract.Infer(obs),
		Observations: obs,
		Groups:       snapshotGroups(),
		Params:       snapshotParams(),
		Emulation:    snapshotEmulation(),
	}
	snap.Pages = snapshotPages()
	// A scan that was told to write a file must produce one even when it
	// inferred no contracts and emulated nothing, so normalization runs
	// unconditionally.
	_ = snap.Normalize(cfg.URL)
	// The counts go in after normalization, so they describe the file rather
	// than the scan: a reader can size itself from the meta section alone.
	snap.Meta[wmse.MetaLinks] = fmt.Sprint(len(snap.Links))
	snap.Meta[wmse.MetaPages] = fmt.Sprint(len(snap.Pages))
	snap.Meta[wmse.MetaEndpoints] = fmt.Sprint(len(snap.Endpoints))
	snap.Meta[wmse.MetaPatterns] = fmt.Sprint(len(snap.Groups))
	snap.Meta[wmse.MetaParams] = fmt.Sprint(len(snap.Params))
	return snap
}

// snapshotStats counts the whole discovered set, not the filtered display set.
// The report honours the class filters when it prints, but a saved scan should
// be able to answer what it found without re-running the scan with other flags.
func snapshotStats(links []linker.Link, cfg *config.Config) wmse.Stats {
	st := categorizer.Calculate(links, cfg.ShowTags)
	return wmse.Stats{
		Total:      st.Total,
		Resolved:   st.Resolved,
		Unresolved: st.Unresolved,
		WithParams: st.WithParams,
		ByCategory: st.ByCategory,
		ByLinkType: st.ByLinkType,
		ByClass:    st.ByClass,
		ByTag:      st.ByTag,
	}
}

func snapshotGroups() []wmse.Group {
	out := make([]wmse.Group, 0, len(activeGroups))
	for _, g := range activeGroups {
		out = append(out, wmse.Group{
			Domain:  g.Domain,
			Pattern: g.Pattern,
			Count:   g.Count,
			Vars:    g.Vars,
			Members: append([]string(nil), g.Members()...),
		})
	}
	return out
}

// snapshotParams pairs each recovered name with the documents it came from. The
// count a name carries is a property of the code, the document list is a
// property of the crawl, and only storing both lets a reader tell "seen in one
// bundle" from "used in forty pages".
func snapshotParams() []wmse.Param {
	fragMu.Lock()
	defer fragMu.Unlock()
	out := make([]wmse.Param, 0, len(fragParams))
	for i, p := range fragParams {
		var docs []string
		if i < len(fragSources) {
			for d := range fragSources[i] {
				docs = append(docs, d)
			}
			sort.Strings(docs)
		}
		out = append(out, wmse.Param{ParamRef: p, Docs: docs})
	}
	return out
}

func snapshotPages() []wmse.Page {
	pageMu.Lock()
	defer pageMu.Unlock()
	out := make([]wmse.Page, 0, len(pageLog))
	for _, p := range pageLog {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func snapshotEmulation() *wmse.Emulation {
	if lastEmuSummary == nil {
		return nil
	}
	s := lastEmuSummary
	em := &wmse.Emulation{
		Scripts:   s.Scripts,
		Calls:     s.Calls,
		Abandoned: s.Abandoned,
		Disabled:  s.Disabled,
		ByType:    s.ByType,
		Errors:    s.ErrorList,
		List:      make([]wmse.Call, 0, len(s.List)),
	}
	if em.ByType == nil {
		em.ByType = map[string]int{}
	}
	for _, c := range s.List {
		call := wmse.Call{
			URL:       c.URL,
			RawURL:    c.RawURL,
			Method:    c.Method,
			Initiator: c.Initiator,
			Type:      c.Type,
			Body:      c.Body,
		}
		for _, h := range c.Headers {
			call.Headers = append(call.Headers, [2]string{h.Name, h.Value})
		}
		em.List = append(em.List, call)
	}
	return em
}

// scanMeta describes how the file was produced. It is flat text on purpose: a
// file has to explain itself to a reader that knows nothing about the flags of
// the version that wrote it, and the reader is expected to show keys it does
// not recognise rather than refuse the file.
func scanMeta(cfg *config.Config, path string, started time.Time) wmse.Meta {
	elapsed := time.Since(started).Round(time.Millisecond)
	meta := wmse.Meta{
		wmse.MetaTool:     "webmap",
		wmse.MetaVersion:  Version,
		wmse.MetaFormat:   fmt.Sprintf("v%d", wmse.FormatVersion),
		wmse.MetaCreated:  time.Now().UTC().Format(time.RFC3339),
		wmse.MetaTarget:   cfg.URL,
		wmse.MetaElapsed:  elapsed.String(),
		wmse.MetaCookies:  boolWord(cfg.Cookie != ""),
		wmse.MetaRequests: fmt.Sprint(cfg.RequestLimit),
	}
	meta[wmse.MetaScope] = scopeOf(cfg)
	meta[wmse.MetaCommand] = commandLine()
	meta[wmse.MetaOutput] = path
	meta["request_headers_all_hosts"] = boolWord(cfg.HeadersAllHosts)
	// The depth the crawl was allowed to reach, as a number of its own. The
	// scope line already spells it in prose, and prose is not something to ask a
	// question of: a reader given a deeper -rdepth than this needs to know
	// whether the crawl stopped at the limit or the site ran out of pages, and
	// only the limit can tell it. Zero is the honest value for a scan that did
	// not crawl at all, since then only the entry point was fetched.
	if cfg.Recursive {
		meta[wmse.MetaMaxDepth] = fmt.Sprint(cfg.RecursiveDepth)
	} else {
		meta[wmse.MetaMaxDepth] = "0"
	}
	// Anything that makes requests carry a credential makes the saved file one
	// too: the captured requests are the evidence, and the evidence includes
	// what was sent. The flag is set when that is possible, so a reader can
	// repeat the warning to whoever opens the file next.
	meta[wmse.MetaSessionData] = boolWord(cfg.Cookie != "" || len(cfg.Headers) > 0 || cfg.Emulate)
	if len(cfg.FollowDomains) > 0 {
		meta["follow_domains"] = strings.Join(cfg.FollowDomains, ",")
	}
	if len(cfg.Presets) > 0 {
		meta["presets"] = strings.Join(cfg.Presets, ",")
	}
	if len(cfg.PatternFiles) > 0 {
		meta["pattern_files"] = strings.Join(cfg.PatternFiles, ",")
	}
	if cfg.AnalyzeJS {
		meta["analyze_js"] = "true"
	}
	if cfg.APIFull {
		meta["api_full"] = "true"
	}
	if cfg.Emulate {
		meta["emulate"] = fmt.Sprintf("timeout=%dms workers=%d maxjs=%dKB maxjobs=%d",
			cfg.EmulateTimeout, cfg.EmulateWorkers, cfg.EmulateMaxJS, cfg.EmulateMaxJobs)
	}
	if cfg.NoGroup {
		meta["grouping"] = "disabled"
	} else {
		meta["grouping"] = fmt.Sprintf("min=%d strings=%t", cfg.GroupCount, cfg.GroupStrings)
	}
	meta["classes"] = enabledClasses(cfg)
	if len(cfg.Headers) > 0 {
		// Header values are not stored: a saved scan should not become a place
		// where a bearer token sits in a file that gets mailed around. Their
		// names are enough to explain the scan.
		names := make([]string, 0, len(cfg.Headers))
		for _, h := range cfg.Headers {
			if i := strings.Index(h, ":"); i > 0 {
				names = append(names, strings.TrimSpace(h[:i]))
			} else {
				names = append(names, "(raw)")
			}
		}
		meta["request_headers"] = strings.Join(names, ",")
	}
	return meta
}

func scopeOf(cfg *config.Config) string {
	if cfg.AllDomains {
		return "all-domains"
	}
	if !cfg.Recursive {
		return "single-page"
	}
	return fmt.Sprintf("recursive depth<=%d threads=%d", cfg.RecursiveDepth, cfg.Threads)
}

func enabledClasses(cfg *config.Config) string {
	var on []string
	for _, c := range []struct {
		name string
		set  bool
	}{
		{"WAF", cfg.WAF}, {"CDN", cfg.CDN}, {"cache", cfg.Cache}, {"noise", cfg.Noise},
	} {
		if c.set {
			on = append(on, c.name)
		}
	}
	if len(on) == 0 {
		return "none"
	}
	return strings.Join(on, ",")
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// secretFlags are the flags whose value is a credential: a request header, or
// a cookie. Their names are worth recording and their values are not, so the
// command line below keeps one and drops the other.
var secretFlags = map[string]bool{
	"-H": true, "--H": true, "-b": true, "--b": true,
	"-cookie": true, "--cookie": true,
}

// commandLine rebuilds the invocation from the parsed flags, because os.Args is
// the only record of how a scan was actually started and a saved file is often
// the only artefact that will still exist later.
//
// The values of credential-bearing flags are replaced. Storing the scan's
// command is what makes a file reproducible, but a file that is worth sharing
// cannot also be the place a bearer token is written down, and the token is
// already on the command line of the shell that ran the scan.
func commandLine() string {
	if len(os.Args) == 0 {
		return ""
	}
	args := os.Args[1:]
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if secretFlags[a] {
			out = append(out, a, quoteArg("<hidden>"))
			// The value was consumed, whatever it looked like.
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if name, _, ok := strings.Cut(a, "="); ok && secretFlags[name] {
			out = append(out, name+"="+quoteArg("<hidden>"))
			continue
		}
		out = append(out, quoteArg(a))
	}
	return "webmap " + strings.Join(out, " ")
}

func quoteArg(a string) string {
	if a == "" || strings.ContainsAny(a, " \t\"'\\") {
		return fmt.Sprintf("%q", a)
	}
	return a
}
