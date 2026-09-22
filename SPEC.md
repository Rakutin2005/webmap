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