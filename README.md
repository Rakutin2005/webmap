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
- **API discovery** — static analysis of JS files (`-j`) resolves calls through
  `fetch`, axios, XHR, jQuery, BX.ajax, Angular, superagent, and config objects.
- **Semantic engine** (`-emulate`) — runs the page in a sandboxed browser
  (goja). Framework-agnostic and obfuscation-proof: obfuscated code must
  deobfuscate itself at runtime, so the real network sink is captured.
- **Session handling** — cookie jar persists `Set-Cookie` across the crawl.
- **Rich output** — text with optional ANSI coloring, ASCII graph (`-G`),
  markdown report directory (`-M`).

## Install

Requires Go 1.26+.

```bash
make build
# or
go build -o webmap ./cmd/webmap
```

## Usage

```bash
webmap [flags] -url <target>
```

Quick example — scan a site, skip TLS errors, follow links recursively, analyze
JS for APIs, and show full API details:

```bash
./webmap -k -r -rdepth 5 -j -emulate -url https://example.com -apif
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
| `-f / -follow d1,d2,...` | Also crawl these comma-separated domains recursively (subdomains included) |
| `-cf` | Parse Cloudflare/WAF protected URLs too (default: skip) |
| `-color` | Colorize output with ANSI colors |
| `-apif` | Show full API info: HTTP method, arguments, match pattern |
| `-emulate` | Semantic engine: execute all page JS in an instrumented runtime |
| `-emu-workers N` | Max concurrent page emulations (default: `min(4, CPUs)`) |
| `-emu-timeout MS` | Max ms of JS execution per page (default 5000, 0 = unlimited) |
| `-emu-maxjs KB` | Skip scripts larger than this (default 3072 KB, 0 = unlimited) |
| `-emu-maxjobs N` | Max event-loop jobs (timers/promises) per page (default 2000) |

## The semantic engine

Static techniques (regex / AST) cannot resolve endpoints built at runtime or
obfuscated: `fetch(atob("..."))`, obfuscator.io string arrays, `URLSearchParams`
/ `new URL` builders, or `this.ajaxUrl` references. The `-emulate` engine
instead **runs** the JavaScript in a sandboxed browser and records the
fully-resolved URL that reaches each network sink.

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
internal/emulator    Semantic JS engine (goja sandbox + instrumented sinks)
internal/fetcher     HTTP fetching and HTML parsing
internal/graph       ASCII graph rendering
internal/jsanalyzer  Static JS analysis: tokenizer, parser, semantic resolver
internal/linker      Link model and categories
internal/markdown    Markdown report generation
internal/patterns    URL/domain classification rules
```

See [SPEC.md](SPEC.md) for the detailed functional specification and acceptance
criteria.

## License

[MIT](LICENSE)
