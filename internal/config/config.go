package config

import (
	"flag"
	"strings"
)

// headerList collects repeated -H flags (curl-style).
type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ", ") }
func (h *headerList) Set(v string) error {
	*h = append(*h, v)
	return nil
}

type Config struct {
	URL             string
	SkipTLS         bool
	Recursive       bool
	RecursiveDepth  int
	Threads         int
	RequestLimit    int
	AllDomains      bool
	ShowTags        bool
	AnalyzeJS       bool
	Emulate         bool
	Graphical       bool
	Markdown        bool
	Cloudflare      bool
	WAF             bool
	CDN             bool
	Cache           bool
	Noise           bool
	Color           bool
	APIFull         bool
	PatternFiles    []string
	Presets         []string
	FollowDomains   []string
	Headers         []string
	Cookie          string
	HeadersAllHosts bool
	EmulateTimeout  int
	EmulateWorkers  int
	EmulateMaxJS    int
	EmulateMaxJobs  int
	EmulateMaxLeaks int
	APIContract     bool
	APIContractRaw  bool
	NoGroup         bool
	NoStringGroup   bool
	GroupStrings    bool
	GroupCount      int
}

func Parse() *Config {
	c := &Config{
		RecursiveDepth: 5,
		Threads:        32,
		GroupCount:     2,
		NoStringGroup:  true,
	}

	flag.StringVar(&c.URL, "url", "", "URL to parse (required)")
	flag.BoolVar(&c.SkipTLS, "k", false, "Ignore TLS certificate errors")
	flag.BoolVar(&c.Recursive, "r", false, "Recursively parse found web-pages")
	flag.IntVar(&c.RecursiveDepth, "rdepth", 5, "Max recursive depth (default: 5)")
	flag.IntVar(&c.Threads, "T", 32, "Max concurrent threads (default: 32)")
	flag.IntVar(&c.RequestLimit, "rlimit", 0, "Max total requests (0 = unlimited)")
	flag.BoolVar(&c.AllDomains, "a", false, "Parse all domains (no restrictions on target URIs)")
	flag.BoolVar(&c.ShowTags, "t", false, "Show link tags (absolute/relative/web)")
	flag.BoolVar(&c.AnalyzeJS, "j", false, "Analyze JS files for API endpoints")
	flag.BoolVar(&c.Emulate, "emulate", false, "Emulate browser: execute all JS in sandboxed goja engine, track network calls")
	flag.IntVar(&c.EmulateTimeout, "emu-timeout", 5000, "Emulation: max ms of JS execution per page (0 = unlimited)")
	flag.IntVar(&c.EmulateWorkers, "emu-workers", 0, "Emulation: max concurrent page emulations (0 = auto: min(4, CPUs))")
	flag.IntVar(&c.EmulateMaxJS, "emu-maxjs", 3072, "Emulation: skip scripts larger than this many KB (0 = unlimited)")
	flag.IntVar(&c.EmulateMaxJobs, "emu-maxjobs", 2000, "Emulation: max event-loop jobs (timers/promises) per page")
	flag.IntVar(&c.EmulateMaxLeaks, "emu-maxleaks", 2, "Emulation: native hangs to tolerate before emulation disables itself")
	flag.BoolVar(&c.Graphical, "G", false, "Generate ASCII graph")
	flag.BoolVar(&c.Markdown, "M", false, "Generate markdown output")
	flag.BoolVar(&c.Cloudflare, "cf", false, "Parse Cloudflare/WAF protected URLs too")
	flag.BoolVar(&c.WAF, "waf", false, "Show WAF/Cloudflare URLs in results")
	flag.BoolVar(&c.CDN, "cdn", false, "Show CDN URLs in results")
	flag.BoolVar(&c.Cache, "cache", false, "Show cache URLs in results")
	flag.BoolVar(&c.Noise, "noise", false, "Show noise (possible non-URL) entries in results")
	flag.BoolVar(&c.Color, "color", false, "Colorize output")
	flag.BoolVar(&c.APIFull, "apif", false, "Show full API info (methods, args, match pattern) from JS sources")
	flag.BoolVar(&c.APIContract, "apic", false, "Show inferred API contracts (endpoint methods, headers, URL/bodies formats, sample bodies)")
	flag.BoolVar(&c.APIContractRaw, "apic-raw", false, "Show request usage evidence (raw requests) in API contracts; tokens stay masked")
	flag.BoolVar(&c.NoGroup, "nogroup", false, "Disable URL pattern grouping")
	flag.BoolVar(&c.GroupStrings, "str", false, "Group by string literals too (off by default: only typed ids like int/uuid/hash/base64 fold into patterns)")
	flag.IntVar(&c.GroupCount, "group-count", 2, "Minimum number of matching URLs before they are folded into a pattern (default: 2)")

	followLong := flag.String("follow", "", "Comma-separated additional domains to include in the recursive crawl (subdomains included)")
	followShort := flag.String("f", "", "Alias of -follow")

	var headers headerList
	flag.Var(&headers, "H", "HTTP header to send with every request, curl-style \"Key: Value\" (repeatable)")
	cookieLong := flag.String("cookie", "", "Cookie header string, e.g. \"name=value; other=value2\"")
	cookieShort := flag.String("b", "", "Alias of -cookie")
	flag.BoolVar(&c.HeadersAllHosts, "headers-all-hosts", false, "Send -H/-b headers to every host (default: only target + -follow domains)")

	patternsOpt := flag.String("patterns", "", "Comma-separated paths to JSON pattern files")
	flag.Bool("bitrix", false, "Enable Bitrix CMS patterns (alias: -patterns bitrix)")
	flag.Bool("wp", false, "Enable WordPress patterns (alias: -patterns wp)")
	flag.Bool("react", false, "Enable React SPA patterns (alias: -patterns react)")
	flag.Bool("full", false, "Show all URL classes (WAF/CDN/Cache/Noise)")

	flag.Parse()

	if *patternsOpt != "" {
		for _, p := range strings.Split(*patternsOpt, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				c.PatternFiles = append(c.PatternFiles, p)
			}
		}
	}

	c.Headers = []string(headers)
	if *cookieLong != "" {
		c.Cookie = *cookieLong
	}
	if *cookieShort != "" {
		c.Cookie = *cookieShort
	}

	seenFollow := map[string]bool{}
	for _, opt := range []string{*followLong, *followShort} {
		for _, d := range strings.Split(opt, ",") {
			d = strings.TrimSpace(strings.ToLower(d))
			d = strings.TrimPrefix(d, "*.") // treat "*.example.com" as "example.com"
			if d != "" && !seenFollow[d] {
				seenFollow[d] = true
				c.FollowDomains = append(c.FollowDomains, d)
			}
		}
	}

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "bitrix":
			if f.Value.String() == "true" {
				c.Presets = append(c.Presets, "bitrix")
			}
		case "wp":
			if f.Value.String() == "true" {
				c.Presets = append(c.Presets, "wp")
			}
		case "react":
			if f.Value.String() == "true" {
				c.Presets = append(c.Presets, "react")
			}
		case "full":
			if f.Value.String() == "true" {
				c.WAF = true
				c.CDN = true
				c.Cache = true
				c.Noise = true
			}
		}
	})

	if c.GroupStrings {
		c.NoStringGroup = false
	}

	return c
}
