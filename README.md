# WebMap

A CLI web crawler and link analyzer that builds a detailed map of a website's
URLs, categorizes every link (pages, static assets, APIs), and discovers
JavaScript API endpoints — including ones that are built at runtime or
obfuscated.

WebMap combines classic crawling with a **semantic engine**: it executes page
JavaScript in an instrumented JS runtime and records the real, fully-resolved
URL reached by every network call.

## Features

- **Deep crawling** — recursive page parsing with depth limits, per-host
  header/cookie control, and Cloudflare/WAF handling.
- **Link categorization** — every link tagged as `web-asset`, `web-page`, or
  `API`, with fine-grained asset subtypes (JS, CSS, image, font, media…).
- **URL patterns** — large families of similar URLs fold into typed patterns
  (`/news/{id: int}/comments/{id: int}`), so a site with 30 000 URLs reads as a
  few hundred lines instead of a wall of noise.
- **API discovery** — static analysis of JS (`-j`) resolves calls through
  `fetch`, axios, XHR, jQuery, BX.ajax, Vue props, component factories, and
  config objects, and is driven by call *shape* rather than by library names, so
  an unfamiliar or renamed client is still recognized.
- **API contracts** (`-apic`) — for each endpoint: methods actually used,
  query/header/body formats with observed values, and a response schema
  inferred from the code that consumes the response.
- **Parameter attribution** — names written into a `URLSearchParams` or
  `FormData` builder are tied to the request they feed, so a contract lists the
  parameters that endpoint accepts even when the crawler never saw a URL with
  them.
- **Semantic engine** (`-emulate`) — runs the page in a sandboxed browser
  (goja). Framework-agnostic and obfuscation-proof: obfuscated code must
  deobfuscate itself at runtime, so the real network sink is captured.
- **Session handling** — cookie jar persists `Set-Cookie` across the crawl.
- **Rich output** — text with optional ANSI coloring, ASCII graph (`-G`),
  markdown report directory (`-M`). Progress goes to stderr, so the report on
  stdout stays pipeable.

## Install

Requires Go 1.26+.

```bash
make build
# or
go build -o webmap ./cmd/webmap
```

Other targets: `make test`, `make vet`, `make fmt`, `make lint`, `make check`
(fmt + vet + test), `make clean`.

## Usage

```bash
webmap [flags] -url <target>
```

Quick example — scan a site, skip TLS errors, follow links recursively, analyze
JS for APIs, and show full API details:

```bash
./webmap -k -r -rdepth 5 -j -apic -url https://example.com
```

### Flags

| Flag | Description |
|------|-------------|
| `-url` | Target URL |
| `-k` | Ignore TLS certificate errors |
| `-H "Key: Value"` | Send an HTTP header with every request (repeatable, curl-style). Use for auth: `-H "Authorization: Bearer …"` |
| `-b` / `-cookie "k=v; k2=v2"` | Cookie string. An in-memory jar also persists `Set-Cookie` responses across the crawl |
| `-headers-all-hosts` | Send `-H`/`-b` headers to every host (default: target + `-follow` domains only, so tokens aren't leaked) |
| `-r` | Recursively parse found web-pages |
| `-rdepth N` | Max recursive depth (default: 5) |
| `-t` | Tag links with type (absolute/relative/web) |
| `-j` | Analyze JS files for API endpoints |
| `-G` | Generate ASCII graph of links |
| `-M` | Generate markdown output directory |
| `-T N` | Max concurrent threads (default: 32) |
| `-rlimit N` | Max total requests (0 = unlimited) |
| `-a` | Parse all domains (no same-domain restriction) |
| `-f` / `-follow d1,d2,...` | Also crawl these comma-separated domains recursively (subdomains included) |
| `-cf` | Parse Cloudflare/WAF protected URLs too (default: skip) |
| `-color` | Colorize output with ANSI colors |

**Analysis**

| Flag | Description |
|------|-------------|
| `-apic` | Show inferred API contracts: methods, query/header/body formats, response schemas |
| `-apic-raw` | Add request-usage evidence (raw requests) to the contracts; tokens stay masked |
| `-apif` | Show full API info from JS sources: methods, args, match pattern |
| `-emulate` | Semantic engine: execute all page JS in an instrumented runtime |
| `-emu-workers N` | Max concurrent page emulations (default: `min(4, CPUs)`) |
| `-emu-timeout MS` | Max ms of JS execution per page (default 5000, 0 = unlimited) |
| `-emu-maxjs KB` | Skip scripts larger than this (default 3072 KB, 0 = unlimited) |
| `-emu-maxjobs N` | Max event-loop jobs (timers/promises) per page (default 2000) |
| `-emu-maxleaks N` | Native hangs to tolerate before emulation disables itself (default: 2) |
| `-patterns file.json,...` | Load extra URL classification rules from JSON files |
| `-bitrix` / `-wp` / `-react` | Built-in rule presets (aliases for `-patterns bitrix` etc.) |

**Grouping and filtering**

| Flag | Description |
|------|-------------|
| `-nogroup` | Disable URL pattern grouping |
| `-group-count N` | Minimum number of matching URLs before they fold into a pattern (default: 2) |
| `-str` | Also fold string literals into patterns (off by default: only typed ids like int/uuid/hash/base64 do) |
| `-full` | Show every URL class, including WAF/CDN/cache/noise |
| `-waf` / `-cdn` / `-cache` / `-noise` | Show these classes too (each is hidden by default) |

## What the report contains

```
=== WebMap Analysis ===      summary: counts by category, link type, URL class
=== All Links ===            every discovered URL, grouped by host
=== Hidden Classes ===       what was recognized and deliberately not shown
=== URL Patterns ===         similar URLs folded into typed patterns
=== API Contracts ===        per-endpoint methods, params, bodies, response schema
=== Dynamic query params === parameters found only in code, that no contract covers
```

**URL patterns.** Only IDs are folded, and their type is inferred from the
values, so a pattern says what it matched rather than hiding it:

```
absolute PAG  html  2gis.kz  https://2gis.kz/astana/geo/{id: int}  x23  [id: 70030076162579465, …]
```

**API contracts.** Methods come from the call sites; a verb inferred from the
shape of a call is marked as such. Parameters are marked by how they were seen —
`(const)` for a literal, `(flag)` for a name sent without a value, and
`(name from code, no value observed)` for a name recovered statically from a
request builder:

```
https://example.com/local/ajax/search.php
  methods: GET
  calls:   160
  query:
    action      unknown (name from code, no value observed)
    sort        str     price_asc (const)
    lang        bool    (flag)
  headers:
    x-requested-with  str  XMLHttpRequest (const)
  response:
    any id
    any name
```

**Dynamic query params.** Parameters the site builds in code never appear in a
crawled URL, so they are reported separately — and each one says what it feeds:

```
  page query state / query      rewrites the page's own URL (filters, share links)
    floors_in      26 docs
  request / query               sent to an API endpoint
    floor_max      36 docs  -> this.ajaxUrl (endpoint set elsewhere)
  unassigned / query            seen in code, no request builder found
    date_sort       4 docs
```

A name that reached a contract is not repeated here.

## The semantic engine

Static techniques (regex / AST) cannot resolve endpoints built at runtime or
obfuscated: `fetch(atob("..."))`, obfuscator.io string arrays, or values that
only exist after a server response. The `-emulate` engine instead **runs** the
JavaScript in a sandboxed browser and records the fully-resolved URL that
reaches each network sink.

**Instrumented sinks:** `fetch`, `XMLHttpRequest.open`, `WebSocket`,
`EventSource`, `navigator.sendBeacon`, `Image`/`Audio` `.src`, dynamically
injected `<script>/<link>/<iframe>`, `form.action`/`submit`, `new Request`,
`importScripts`, `serviceWorker.register`, `window.open`/`location.assign`.

**Browser environment:** working event loop (timers, promises,
`queueMicrotask`), lifecycle events (`DOMContentLoaded`, `load`),
`atob`/`btoa`, `URL`/`URLSearchParams`, a permissive DOM with real
child/sibling tracking, `localStorage`, `Intl`, observers, and DOM interface
constructors — complete enough for real jQuery/axios/framework bundles to
initialize and route their AJAX calls through the instrumented sinks. Scripts
run in document order in a single realm.

**Boundary:** dynamic execution finds endpoints reachable from client-side
state. An endpoint whose URL depends on real server-response data (e.g. a
second call built from the JSON of a first) cannot be resolved, since the
sandbox returns synthetic responses.

## Project layout

```
cmd/webmap/          CLI entry point
internal/categorizer Link categorization
internal/color       ANSI colouring helpers
internal/config      Flags and configuration
internal/contract    API contract model and rendering
internal/emulator    Semantic JS engine (goja sandbox + instrumented sinks)
internal/fetcher     HTTP fetching and HTML parsing
internal/graph       ASCII graph rendering
internal/jsanalyzer  Static JS analysis: tokenizer, parser, request/response inference
internal/linker      Link model, categories, parameter references
internal/markdown    Markdown report generation
internal/patterns    URL/domain classification rules
internal/progress    Progress bar (stderr)
internal/urlgroup    URL pattern grouping
```

See [SPEC.md](SPEC.md) for the detailed functional specification and acceptance
criteria.

## License

[MIT](LICENSE)
