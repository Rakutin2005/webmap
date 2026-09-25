# WebMap - URL Parser and Link Analyzer

## Project Overview
- **Project name**: WebMap
- **Type**: CLI utility for web page parsing and link analysis
- **Core functionality**: Parses web pages, extracts and categorizes links (web-asset, web-page, API), provides statistics and visualizations
- **Target users**: Developers doing automated testing and diagnostics of web applications

## Functionality Specification

### Core Features

1. **URL Fetching**
   - Fetch URL content with HTTP/HTTPS support
   - Handle TLS certificate validation (skip with -k flag)
   - Parse HTML, JSON, JavaScript responses

2. **Link Extraction**
   - Extract all link types:
     - Absolute URLs (https://example.com/path)
     - Relative URLs (/path, ./path, ../path)
     - Web paths (//example.com/path)
   - Parse from: `<a href>`, `<link href>`, `<script src>`, `<img src>`, `<source src>`, `<video>`, `<audio>`, `<iframe src>`, CSS `@import`, JavaScript imports

3. **Link Categorization**
   - **web-asset**: Static resources (css, js, images, fonts, icons)
   - **web-page**: HTML pages, navigation links
   - **API**: Endpoints matching API patterns (/api/, /v1/, .json, .xml, AJAX calls)

4. **Statistical Output**
   - Total links found
   - Count per category
   - Count per tag type (absolute, relative, web)
   - Timing information

### Flags

| Flag | Description |
|------|-------------|
| `-k` | Ignore TLS certificate errors |
| `-H "Key: Value"` | Send an HTTP header with every request (curl-style, repeatable). Use for auth: `-H "Authorization: Bearer …"`. |
| `-b` / `-cookie "k=v; k2=v2"` | Cookie header string. An in-memory cookie jar also persists `Set-Cookie` responses across the crawl (session continuity). |
| `-headers-all-hosts` | Send `-H`/`-b` headers to every host. Default: only the target and `-follow` domains, so auth tokens aren't leaked to third-party hosts. |
| `-r` | Recursively parse found web-pages |
| `-rdepth N` | Max recursive depth (default: 5) |
| `-t` | Tag links with type (absolute/relative/web) |
| `-j` | Analyze JS files for API endpoints |
| `-G` | Generate ASCII graph of links |
| `-M` | Generate markdown output directory |
| `-T N` | Max concurrent threads (default: 32) |
| `-rlimit N` | Max total requests (0 = unlimited) |
| `-a` | Parse all domains (no same-domain restriction) |
| `-f` / `-follow <d1,d2,...>` | Also crawl these comma-separated domains recursively (subdomains included), in addition to the target's own domain. A middle ground between same-domain-only and `-a`. |
| `-cf` | Parse Cloudflare/WAF protected URLs too (default: skip) |
| `-color` | Colorize output with ANSI colors |
| `-apif` | Show full API info: HTTP method, arguments, match pattern (child of API link) |
| `-emulate` | **Semantic engine**: execute all page JS in an instrumented JS runtime (goja) and intercept every network sink. Framework-agnostic and obfuscation-proof. Heavier than static analysis but far more accurate. |
| `-emu-workers N` | Max concurrent page emulations (default: auto = min(4, CPUs)). The main memory/CPU control — each emulation holds a JS VM. |
| `-emu-timeout MS` | Max ms of JS execution per page (default 5000, 0 = unlimited). A watchdog interrupts runaway/animation loops. |
| `-emu-maxjs KB` | Skip scripts larger than this (default 3072 KB, 0 = unlimited). |
| `-emu-maxjobs N` | Max event-loop jobs (timers/promises) per page (default 2000). |
| `-o <file.wmse>` | Write the whole scan to a static-explorer file (see below). Read it later with `wmse read <file>`, using the same flags. |
| `-o-raw` | Write the `-o` file with no section compression: larger, but a reader can map a section without inflating it. |

### Resource limits & optimizations

Emulation is bounded so it cannot exhaust memory or hang:
- **Concurrency** (`-emu-workers`) caps how many JS VMs run at once, independent of crawl `-T` threads — this is the primary memory control (unbounded fan-out across crawl workers was the crash cause).
- **Per-page timeout** (`-emu-timeout`) uses goja's interrupt to unwind infinite/animation loops; already-captured endpoints are preserved.
- **Script-size cap** (`-emu-maxjs`) skips pathologically large bundles.
- **Job cap** (`-emu-maxjobs`) bounds the event loop.
- **Graphics ignored**: `requestAnimationFrame` is a no-op, `setInterval` fires at most once, and canvas `getContext` returns null — animation/layout work yields no endpoints and only wastes cycles.
- Cache-buster query params (`t`, `_`, `v`, `ver`, `rnd`, …) are stripped from emulated URLs so timestamped resources collapse to one entry, and emulation errors are deduplicated in the summary.

## API Endpoint Discovery: Semantic Engine (`-emulate`)

Static techniques (regex / AST) cannot resolve endpoints that are built at
runtime or obfuscated: `fetch(atob("..."))`, obfuscator.io string arrays like
`fetch(_0x1a2b[0x3])`, `URLSearchParams`/`new URL` builders, `this.ajaxUrl`
references, or minified framework code. The `-emulate` engine instead **runs**
the JavaScript in a sandboxed browser environment (goja) and records the real,
fully-resolved URL that reaches each network sink — because obfuscated code must
deobfuscate itself at runtime.

**Instrumented sinks:** `fetch`, `XMLHttpRequest.open`, `WebSocket`,
`EventSource`, `navigator.sendBeacon`, `Image`/`Audio` `.src`, dynamically
injected `<script>/<link>/<iframe>` (`.src`/`.href`/`setAttribute`),
`form.action`/`submit`, `new Request`, `importScripts`, `serviceWorker.register`,
`window.open`/`location.assign`.

**Browser environment provided:** a working event loop (timers, promises,
`queueMicrotask`, `requestIdleCallback`), lifecycle events (`DOMContentLoaded`,
`load`), `atob`/`btoa`, `URL`/`URLSearchParams`, `TextEncoder`/`TextDecoder`,
`crypto.getRandomValues`, a permissive DOM (`document`, elements with real
child/sibling tracking and `cloneNode`), `localStorage`, `Intl`, observers, and
the DOM interface constructors libraries probe. This is complete enough for real
jQuery/axios/framework bundles to initialize, so their AJAX calls route through
the instrumented sinks. Scripts run in document order in a single realm, so
libraries initialize before app code uses them; every crawled page is emulated
against its own base URL and discovered endpoints are attributed to that page in
the web map.

**Boundary:** dynamic execution finds endpoints reachable from client-side state.
Endpoints whose URL depends on real *server response data* (e.g. a second call
built from the JSON of a first) cannot be resolved, since the sandbox returns
synthetic responses.

### Output Formats

1. **Text** (default): Statistics and link list with domain column
2. **Color** (-color): ANSI-colorized output with:
   - Domain coloring: target (green), subdomain (dark yellow), well-known (red), other (gray)
   - Category/type coloring: API (cyan), page (green), assets (by subtype: js=yellow, css=blue, img=purple, font=dim, media=cyan, data=dark yellow)
3. **Graph** (-G): ASCII tree/graph visualization
4. **Markdown** (-M): `[url].wmap/` directory with markdown files
5. **Static explorer file** (-o): a single binary file, read back offline by `wmse`

## Saved Scans and the Static Explorer (`-o`, `wmse`)

### Requirement

A saved scan must contain the maximum useful information available for
off-line exploitation, at the smallest size that can hold it, in a format an
offline reader can explore as a *graph* rather than a list.

### What is saved

Everything the scan produced, including what the printed report discards:

- every link, with its category, link type, URL class, tag, **the query it was
  requested with**, API details including the call's **argument list**,
  **depth in the crawl**, and the page it was found on;
- every page actually fetched, with depth, content type and link count — the only
  record of *why* a link was never followed (depth limit, class filter, domain
  rule);
- URL patterns, their members, and the values their variables were seen taking;
- inferred API contracts, and the **raw observations** they were inferred from;
- parameters recovered from request builders, with the documents they came from;
- the emulation summary and every intercepted call;
- the statistics, the flags, the target, and the command line that produced it —
  including the depth the crawl was *allowed* to reach, as a number, because a
  crawl that stopped at its limit has pages it never fetched and a reader asked
  for more has to be told which of the two it is looking at.

Two of those are there because a report drops them and a file must not: the
argument list of a call (`-apif` prints it, and folding it away would make the
flag unanswerable from a saved scan) and the query of a link (the row annotation,
and the rule that stops a recovered parameter being reported a second time). A
node the snapshot added so a relation had an endpoint is marked as such, so a
reader prints the same link table the run printed rather than the run's plus every
page nothing linked to.

Header **values** configured with `-H`/`-b` are not stored (their names are, and
`session_data=true` marks a file that may hold captured session material); the
requests the scan actually sent are stored as captured, because they are the
evidence.

### Format

A property graph in one file: `magic + version + header length + flags +
directory`, then eleven independent sections (`meta`, `stats`, `strings`,
`links`, `edges`, `groups`, `endpoints`, `observations`, `params`, `emulation`,
`dicts`), each with its own codec and length in the directory, and each
deflated independently. Sections are decoded on demand, so a reader that only
wants the links never inflates the emulation log.

Size comes from two mechanisms:

- **Graph distribution** — one interned string table for the whole file, sorted
  and front-coded; repeated sub-structures (pattern variables, contract fields,
  observed header sets) interned by content into dictionaries and referenced by a
  one-based id, with `0` reserved so an absent field costs nothing; relations
  encoded as delta- and run-length-coded edges.
- **Location** — a link is stored as its distance from the previous link in
  sorted order rather than as a repeated literal, and an edge whose endpoints did
  not change costs a few bits.

Target: 5.5 bytes per link on a 20 000-link scan — 58× smaller than the same
scan as JSON, 8× smaller than the same format with compression switched off, and
sublinear in scan size. The ratio is asserted by
`internal/wmse/perf_test.go`, so a change that quietly gives up the compression
the format earns by itself fails the build rather than the documentation.

### Reader

`wmse` is the scan with the network replaced by the file, so it takes the scan's
arguments: the same flags, the same names, the same types, the same defaults,
registered by the same `config.Register`, and spelled with one dash like every
flag in the scan. They may appear on either side of the file path, so a scan
command line replays by changing the verb:

```
webmap -r -apic -emulate -o scan.wmse -url https://example.com
wmse  read -r -apic -emulate      scan.wmse
```

The report is the scan's report: the same sections in the same order, each behind
the same flag — `-r` the crawl as a tree, `-rdepth` its cut, `-apic` the
contracts, `-apic-raw` the requests behind them, `-apif` the API rows as method
and arguments, `-emulate` the sandbox, `-G` the relation graph, `-nogroup` no
pattern section, `-waf`/`-cdn`/`-cache`/`-noise`/`-full` the URL classes, `-t` the
tags, the dynamic query params ungated, `-color`. The reader adds what a terminal
report has no room for: which file this is, what the run that made it was, which
pages were fetched, and which page each link was found on.

Rules that follow from the file being the only source of data:

- **A request the file cannot answer is an error, not an empty section.** It names
  the flag, prints the metadata of the run that made the file, leaves the rest of
  the report intact, writes the diagnostic to stderr and exits non-zero.
  "No contracts" and "this scan looked for none" must not look the same.
- **`-rdepth` is the reader's, not the file's.** A saved crawl cannot be deeper
  than the depth it was made with, so the default is the whole tree; `-rdepth 3`
  cuts at three even when the file was crawled to five, and the tree says how much
  is below the cut. Asking for more than the crawl was *allowed* to reach is an
  error naming that limit — asking for more than the pages happen to go is not,
  because a crawl that ended because the site ended is complete.
- **`-rlimit` bounds the lines of the report**, since the requests are already
  made, and it is one budget for the whole request: twenty lines means twenty
  lines, wherever they were spent. Section titles are structure rather than
  content and are not counted, and running out says so — a report that ends
  without a word reads as the whole of it.
- **The scope flags narrow the report.** The scan's own table lists every link it
  found on any domain — its scope decides what it fetched, not what it shows — so a
  reader with no scope flag reports the same set, and `-url`/`-f`/`-a` are the way
  to ask what a differently-scoped run would have seen.
- **Flags that only configured the crawl are accepted and ignored**: `-k`, `-cf`,
  `-j`, `-str`, `-M`, `-T`, `-group-count`, `-H`, `-b`, `-cookie`,
  `-headers-all-hosts`, `-o`, `-o-raw`, `-patterns`, `-bitrix`, `-wp`, `-react`,
  `-emu-*`. No file can be crawled or written.
- **What the file *is* is a verb, not a flag**: `wmse info` (layout, size per
  section, the full saved metadata, hosts) and `wmse json` (the whole snapshot, for
  another tool). Both take the same flags as `read`.
- `-h` prints the usage to stdout and exits 0; a bad flag prints the message and
  the usage to stderr and exits 2, the way the scan answers one. A missing file, a
  file that is not a snapshot, a truncated one and a corrupt section are all
  errors, never a panic.

The one place the reader's numbers differ from the run's printed ones is `With
query params`: the run counts its in-memory set before folding query variants,
while the file kept every link's query, so the reader counts the links that carry
one. It is the number the link table below it shows.

## Acceptance Criteria

- [x] Fetches URL and parses HTML content
- [x] Extracts all link types from HTML
- [x] Categorizes links correctly (web-asset, web-page, API)
- [x] Supports -k flag for TLS skip
- [x] Supports -r flag for recursive parsing
- [x] Supports -t flag for link tagging
- [x] Supports -j flag for JS API analysis
- [x] Supports -G flag for graph output
- [x] Supports -M flag for markdown output
- [x] Outputs statistics
- [x] Skips Cloudflare/WAF URLs by default (-cf to include)
- [x] Supports -color flag for ANSI-colorized output with domain/type coloring
- [x] Supports -apif flag for detailed API info (method, args, match pattern)
- [x] `-o` writes a complete, self-describing scan to one file
- [x] Every URL referenced by a relation is a node, so the graph has no dangling edges
- [x] `wmse` takes the scan's flags, one for one, and means what they meant there
- [x] `wmse read` prints the scan's report, sections and gates alike
- [x] A flag asking for data the file does not hold is an error with the run's metadata
- [x] `-rdepth` cuts the tree where the reader is asked, not where the scan stopped
- [x] `wmse read` explores the file offline and round-trips every section losslessly
- [x] A 20 000-link scan serializes to 5.5 bytes per link, sublinearly
- [x] Configured credential values are absent from the file; captured requests are kept