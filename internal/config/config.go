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
	// Output is the path of a static-explorer file to write. Empty means the
	// scan is only printed.
	Output string
	// OutputRaw disables section compression. The format is built to be small;
	// this exists for a reader that wants to mmap a section and would rather
	// not pay for inflation.
	OutputRaw bool
	// Refs asks the scan to record, for every link, where in the source
	// document it was discovered: the file, the byte offset, the line and
	// column, and the text around it. The references are stored as a .ref file
	// per source inside the -o file's sidecar archive, so it is only meaningful
	// with -o; without it, -refs is an error rather than a silent no-op.
	Refs bool

	// The values of the flags that only mean something once every flag has been
	// seen, and of the ones that are only an alias of another. They are fields
	// rather than locals so that Apply can resolve them, which is what lets a
	// second command register the same flags on its own flag set.
	followFlag, followAlias string
	cookieFlag, cookieAlias string
	patternFilesFlag        string
	headerFlags             headerList
	bitrix, wp, react       bool
	full                    bool
}

// Register defines every flag of a WebMap command on fs and returns the config
// they fill in.
//
// The scan and the static reader both register through here, which is what makes
// their arguments identical rather than merely similar: there is one definition
// of what -apic or -rdepth means, and a second command cannot spell it
// differently or drift away from it. Parsing the flag set and resolving the
// flags that only mean something in combination are the caller's job, in that
// order: Register, fs.Parse, then Config.Apply.
func Register(fs *flag.FlagSet) *Config {
	c := &Config{
		RecursiveDepth: 5,
		Threads:        32,
		GroupCount:     2,
		NoStringGroup:  true,
	}

	fs.StringVar(&c.URL, "url", "", "URL to parse (required)")
	fs.BoolVar(&c.SkipTLS, "k", false, "Ignore TLS certificate errors")
	fs.BoolVar(&c.Recursive, "r", false, "Recursively parse found web-pages")
	fs.IntVar(&c.RecursiveDepth, "rdepth", 5, "Max recursive depth (default: 5)")
	fs.IntVar(&c.Threads, "T", 32, "Max concurrent threads (default: 32)")
	fs.IntVar(&c.RequestLimit, "rlimit", 0, "Max total requests (0 = unlimited)")
	fs.BoolVar(&c.AllDomains, "a", false, "Parse all domains (no restrictions on target URIs)")
	fs.BoolVar(&c.ShowTags, "t", false, "Show link tags (absolute/relative/web)")
	fs.BoolVar(&c.AnalyzeJS, "j", false, "Analyze JS files for API endpoints")
	fs.BoolVar(&c.Emulate, "emulate", false, "Emulate browser: execute all JS in sandboxed goja engine, track network calls")
	fs.IntVar(&c.EmulateTimeout, "emu-timeout", 5000, "Emulation: max ms of JS execution per page (0 = unlimited)")
	fs.IntVar(&c.EmulateWorkers, "emu-workers", 0, "Emulation: max concurrent page emulations (0 = auto: min(4, CPUs))")
	fs.IntVar(&c.EmulateMaxJS, "emu-maxjs", 3072, "Emulation: skip scripts larger than this many KB (0 = unlimited)")
	fs.IntVar(&c.EmulateMaxJobs, "emu-maxjobs", 2000, "Emulation: max event-loop jobs (timers/promises) per page")
	fs.IntVar(&c.EmulateMaxLeaks, "emu-maxleaks", 2, "Emulation: native hangs to tolerate before emulation disables itself")
	fs.BoolVar(&c.Graphical, "G", false, "Generate ASCII graph")
	fs.BoolVar(&c.Markdown, "M", false, "Generate markdown output")
	fs.BoolVar(&c.Cloudflare, "cf", false, "Parse Cloudflare/WAF protected URLs too")
	fs.BoolVar(&c.WAF, "waf", false, "Show WAF/Cloudflare URLs in results")
	fs.BoolVar(&c.CDN, "cdn", false, "Show CDN URLs in results")
	fs.BoolVar(&c.Cache, "cache", false, "Show cache URLs in results")
	fs.BoolVar(&c.Noise, "noise", false, "Show noise (possible non-URL) entries in results")
	fs.BoolVar(&c.Color, "color", false, "Colorize output")
	fs.BoolVar(&c.APIFull, "apif", false, "Show full API info (methods, args, match pattern) from JS sources")
	fs.BoolVar(&c.APIContract, "apic", false, "Show inferred API contracts (endpoint methods, headers, URL/bodies formats, sample bodies)")
	fs.BoolVar(&c.APIContractRaw, "apic-raw", false, "Show request usage evidence (raw requests) in API contracts; tokens stay masked")
	fs.BoolVar(&c.NoGroup, "nogroup", false, "Disable URL pattern grouping")
	fs.BoolVar(&c.GroupStrings, "str", false, "Group by string literals too (off by default: only typed ids like int/uuid/hash/base64 fold into patterns)")
	fs.IntVar(&c.GroupCount, "group-count", 2, "Minimum number of matching URLs before they are folded into a pattern (default: 2)")
	fs.StringVar(&c.Output, "o", "", "Write a static-explorer file (wmse) with the whole scan; read it later with: wmse read <path>")
	fs.BoolVar(&c.OutputRaw, "o-raw", false, "Store the -o file without section compression (larger, but reads without inflating)")
	fs.BoolVar(&c.Refs, "refs", false, "Record where each link was found (source file, byte offset, line:col, context) in the -o file's .ref sidecar; requires -o")

	fs.StringVar(&c.followFlag, "follow", "", "Comma-separated additional domains to include in the recursive crawl (subdomains included)")
	fs.StringVar(&c.followAlias, "f", "", "Alias of -follow")

	fs.Var(&c.headerFlags, "H", "HTTP header to send with every request, curl-style \"Key: Value\" (repeatable)")
	fs.StringVar(&c.cookieFlag, "cookie", "", "Cookie header string, e.g. \"name=value; other=value2\"")
	fs.StringVar(&c.cookieAlias, "b", "", "Alias of -cookie")
	fs.BoolVar(&c.HeadersAllHosts, "headers-all-hosts", false, "Send -H/-b headers to every host (default: only target + -follow domains)")

	fs.StringVar(&c.patternFilesFlag, "patterns", "", "Comma-separated paths to JSON pattern files")
	fs.BoolVar(&c.bitrix, "bitrix", false, "Enable Bitrix CMS patterns (alias: -patterns bitrix)")
	fs.BoolVar(&c.wp, "wp", false, "Enable WordPress patterns (alias: -patterns wp)")
	fs.BoolVar(&c.react, "react", false, "Enable React SPA patterns (alias: -patterns react)")
	fs.BoolVar(&c.full, "full", false, "Show all URL classes (WAF/CDN/Cache/Noise)")

	return c
}

// Apply resolves the flags that only mean something in combination: -full is the
// four class flags at once, -f is an alias of -follow, -cookie of -b, -str turns
// off the "do not group strings" default, and a preset is the pattern file it
// names. It runs after parsing, and it is separate from Register so that a
// second command can put the same flags on its own flag set and get the same
// resolution.
func (c *Config) Apply(fs *flag.FlagSet) {
	if c.patternFilesFlag != "" {
		for _, p := range strings.Split(c.patternFilesFlag, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				c.PatternFiles = append(c.PatternFiles, p)
			}
		}
	}

	c.Headers = c.headerFlags
	if c.cookieFlag != "" {
		c.Cookie = c.cookieFlag
	}
	if c.cookieAlias != "" {
		c.Cookie = c.cookieAlias
	}

	seenFollow := map[string]bool{}
	for _, opt := range []string{c.followFlag, c.followAlias} {
		for _, d := range strings.Split(opt, ",") {
			d = strings.TrimSpace(strings.ToLower(d))
			d = strings.TrimPrefix(d, "*.") // treat "*.example.com" as "example.com"
			if d != "" && !seenFollow[d] {
				seenFollow[d] = true
				c.FollowDomains = append(c.FollowDomains, d)
			}
		}
	}

	// -full is not a class of its own: it is the four class flags, and the
	// report asks about them by those names.
	if c.full {
		c.WAF = true
		c.CDN = true
		c.Cache = true
		c.Noise = true
	}
	for _, preset := range []struct {
		name string
		set  bool
	}{{"bitrix", c.bitrix}, {"wp", c.wp}, {"react", c.react}} {
		if preset.set {
			c.Presets = append(c.Presets, preset.name)
		}
	}

	if c.GroupStrings {
		c.NoStringGroup = false
	}
}

// Parse reads the command line of the scan.
func Parse() *Config {
	c := Register(flag.CommandLine)
	flag.Parse()
	c.Apply(flag.CommandLine)
	return c
}
