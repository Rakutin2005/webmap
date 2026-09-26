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
- **Static explorer** (`-o file.wmse`) — save the whole scan as one compact
  binary file, then answer the same command from it offline with `wmse`: the same
  flags, the same report, the network replaced by the file.

## Install

Requires Go 1.26+.

```bash
make            # builds ./webmap and ./wmse
# or
go build -o webmap ./cmd/webmap
go build -o wmse  ./cmd/wmse
```

Other targets: `make test`, `make vet`, `make fmt`, `make lint`, `make check`
(fmt + vet + test), `make save URL=… OUT=scan.wmse`, `make clean`.

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

**Saving**

| Flag | Description |
|------|-------------|
| `-o file.wmse` | Write the whole scan to a static-explorer file, to be read later with `wmse read` |
| `-o-raw` | Write the file without section compression (larger, but a reader can map a section without inflating it) |
| `-refs` | Record where each link was found (file, byte offset, line:col, context) into a `.ref` file per source in the file's archive. Requires `-o` |

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

## The static explorer (`-o` / `wmse`)

A terminal report is a summary: it prints what is worth printing and throws the
rest away. `-o` writes the scan to a single binary file that keeps everything,
and `wmse` reads it back with no network access and no re-scan.

`wmse` is the *same command* pointed at that file. The flags are the scan's own
flags — registered by [`config.Register`](internal/config/config.go), the one
function `webmap` uses too, so a flag cannot be spelled differently, defaulted
differently or described differently in the two — and they mean what they meant
there. Only the verb changes:

```bash
./webmap -k -r -rdepth 5 -j -apic -emulate -o scan.wmse -url https://example.com
./wmse  read -k -r -rdepth 5 -j -apic -emulate      scan.wmse
```

The reader prints the scan's report: the same sections, in the same order, with
the same gates — `-r` the crawl, `-apic` the contracts, `-apic-raw` the requests
behind them, `-apif` the API rows as method and arguments, `-emulate` what the
sandbox ran, `-G` the relation graph, `-nogroup` no pattern section, `-waf`/`-cdn`/
`-cache`/`-noise`/`-full` the URL classes, `-t` the tags, `-nogroup`, and the
dynamic query params that reached no contract. Flags go on either side of the
file, so a command line can be pasted across unchanged.

```
WebMap snapshot  scan.wmse  (1.6 KiB, 11 sections)
  produced by  webmap 1.0.0 (v1)
  target       https://example.com/
  created      2026-09-25T18:20:24Z
  scope        recursive depth<=5 threads=32
  elapsed      5ms
  command      webmap -k -r -rdepth 5 -j -apic -emulate -o scan.wmse -url https://example.com
  holds        19 links, 9 pages fetched, 1 url pattern, 6 api contracts, 8 request
               observations, 4 recovered parameters, 32 relations

=== WebMap Analysis (Same Domain Only) ===
…
```

**What a flag cannot answer is an error, not an empty section.** A request for
data the file does not hold says which flag asked for it and prints the metadata
of the run that made the file, so the next command is the one that would have
worked; the status is non-zero, because a script that asked for `-apic` on a file
without contracts must not read success. The rest of the report still prints, and
the diagnostic goes to stderr so a piped report stays clean.

```
$ wmse read scan.wmse -apic -emulate
wmse: -apic: this file holds no API contracts (the scan recorded analyze_js=false)
wmse: -emulate: this scan did not emulate any scripts
WebMap snapshot  scan.wmse  …
```

The flags that only ever configured a crawl — `-k`, `-cf`, `-j`, `-str`, `-M`,
`-T`, `-group-count`, `-H`, `-b`, `-cookie`, `-headers-all-hosts`, `-o`, `-o-raw`,
`-patterns`, `-bitrix`, `-wp`, `-react`, `-emu-*` — are accepted and ignored: no
file can be crawled or written, so there is nothing for them to do. `wmse -h` lists
them.

Two flags have no offline meaning of their own, so they became verbs rather than
new flags: what the file *is* (`wmse info`) and the whole snapshot for other
tools (`wmse json`). Both take the same flags.

### The `select` session

`wmse select scan.wmse` opens the same data interactively — the report navigated
by hand, one question at a time. The session stands at a *place*, and a place is
either a page or a directory, because those are two different questions: a page
answers "what does this document link to", a directory answers "what is under
here". The prompt shows both, with the name of the place in brackets:

```
explore@https://example.com/ [/] > ls
explore@https://example.com/users [users] > cd /users
explore@https://example.com/users [users] > ls
explore@https://example.com/api [api] > ls -d
```

A **page always wins**: the scan fetched a document at the path, so that page is
what `ls` describes. A path with no document of its own may still name an index
that was fetched, and then the index is the page, as a browser would reach it.
Only a path with neither is a directory, and `ls` there lists every path matching
it at any depth — whether or not an index exists, which is exactly the case
`ls -d` is for when a page does answer.

```
cd [path]        move; a page is preferred over a directory
ls [path]        the current page's links, or the paths under a directory
ls -d [path]     ask for a path as a directory, whatever page answers for it
read [flags]     the scan's whole report, with the reader's flags
what <path>      what a path is: kind, class, where it was seen
from <path>      the pages that reference a resource
api [path]       an endpoint's contract, or why there is none
find <kind> <g>  search by kind (api, js, css, image, html, any) and glob
refs [path]      where links were found, from the .ref sidecar
info             the header of every loaded file
extend <file>    open another scan and query across both
history          what has been typed this session
```

`api` gives one of four *distinct* refusals — not found, not an API, an API with
no contract in the file, and a contract with no captured request — because those
are four different facts and each one tells you something. `extend` folds a
second scan into the session so a question can span both.

**`read` is `wmse read`, run from the prompt.** Same report, same sections, same
gates, same code — so a file can never be described two ways. And with no flags it
starts from **the flags the scan itself ran with**, which the file records in its
metadata: `read` alone shows the report as that scan saw it, with the tree, the
contracts and the references it asked for. A flag you type is applied on top,
because a flag you don't pass is one that takes its default — and the default is
now the scan's own value:

```
explore@svoydom.kz [/] > read
  report: the scan's own flags, from the file's metadata
  === Crawl Tree ===
  === API Contracts ===
  === References ===
```

The report says which of the two it used, because a section nobody asked for is
otherwise indistinguishable from one the file cannot answer. `wmse read` on the
command line keeps its own documented defaults — its flags hint exists to describe
that report.

**The line is edited, not just read.** Tab completes the word under the cursor —
one candidate whole, several as far as they agree, and the rest written out when
there is nothing more to insert. Candidates are per command, so `cd` completes
paths, `find` completes kinds, and `extend` completes files on disk. The arrows
move along the line and walk the history. Ctrl-A/E/U/K/W, Home, End and Delete
work as they do in a shell, Escape abandons a line, Ctrl-C abandons it without
leaving, and Ctrl-D on an empty line leaves.

History is kept for the session and written nowhere. Piped or scripted, the
session reads whole lines and answers the same things, so nothing a terminal can
do is something only a terminal can reach.

**Four of the commands go outside the file**, and each says which it is before it
happens — the difference between reading a document and changing one:

```
explore@svoydom.kz [/] > source /static/app.js /tmp/app.js
    from the archive                        no network, the same bytes every time
explore@svoydom.kz [/] > save /admin/export.csv
    adding 412 B to scan.wmse               the file grows; the scan's findings do not
explore@svoydom.kz [/] > req /api/v2/users -H "authorization: Bearer X"
    curl -H "authorization: Bearer X" http://svoydom.kz/api/v2/users
explore@svoydom.kz [/] > continue -rdepth 6
    webmap -r -rdepth 3 -j -apic -url http://svoydom.kz/ -o /tmp/… -rdepth 6
```

`source` asks the archive first and only fetches what the archive does not hold,
and it never archives what it fetches — `save` is the command that grows the file.
`save` is the one command that changes the file you opened, so it says the size
before writing and the write is atomic. `req` runs curl with your arguments as its
own argv, so a value with a semicolon in it is one argument and not a command.

`continue` is **not** a resume, and that is worth knowing before you rely on it:
the scan keeps no frontier of what it has already fetched, so a deeper run
re-fetches what the last one covered and goes further. The result is a superset —
nothing is missed — and the requests for the part already seen are the price. It
inherits the scan's own arguments, replaces only the two that would send the result
somewhere else (the output file and the scope, the scope being where you are
standing), and applies what you type last.

**A file can be signed and sealed**, which are properties of the file rather than
of the session, so they travel with it:

```
explore@svoydom.kz [/] > sign rsa ~/.wm/sealing.pem
  signed rsa
  by rsa sealing.pem
  key 9f2c1ab4de07f310 (sealing.pem)
explore@svoydom.kz [/] > save /admin/export.csv
  this file is signed, and writing it makes the signature invalid
  signed by rsa sealing.pem on 2026-09-26 16:01 UTC
  the signature is not re-made, because only the signer can do that
  write it anyway? [y/N] y
explore@svoydom.kz [/] > encrypt aes ~/.wm/scan.key
  encrypting scan.wmse with aes; after this the file on disk cannot be read without it
  continue? [y/N] y
```

`sign` covers the file with its own signature section left out, so a file can be
signed again after a change and cannot be edited without the signature being
reported broken — on open, and before any write, where it stops and asks. The
signature is **not** re-made, because only the signer can do that. A session with
no terminal answers no rather than proceeding: a warning nobody can answer is a
hang, and not being asked is not consent.

`encrypt` seals the file with AES-256-GCM, under a key from a password
(PBKDF2, 600 000 rounds), an RSA public key, or a 32-byte key file. It asks first,
because afterwards the file on disk is ciphertext and the session holds the only
readable copy. A sealed file reopens with `-i <key-file>`, the reader's one flag of
its own; a password-encrypted one is asked for instead, without echo, and probed.

A signature says the file has not changed since the holder of that key signed it.
That is a real property, and it is not "this file is trustworthy" — a file carrying
its own public key would only be proving that whoever wrote it had a key.

`wmse select -h` lists everything.

**What the file keeps that the report does not show:**

- **The pages actually fetched**, with their depth, content type and link count.
  This is the only record of *why* a link was never followed: depth limits, class
  filters and domain rules all drop links from the queue, and afterwards the link
  set cannot tell a page that was skipped from one that does not exist. `-r` walks
  it as a tree, and `-rdepth 3` cuts it there even when the scan went deeper — the
  depth is yours, and the tree says how much is left below the cut. Asking for a
  depth the crawl was never *allowed* to reach is an error naming that limit;
  asking for one deeper than the pages happen to go is not, because a crawl that
  stopped because the site ended is complete.
- **The hidden classes.** A run without `-cdn` suppresses CDN links when it
  prints; the file keeps them, and the reader shows them on the same flag.
- **The raw requests** behind every inferred contract, exactly as they were sent.
  A contract collapses many calls into one line; the observations say what the
  first one really looked like.
- **The relations** between everything, as a property graph — so the reader can
  walk it, not just list it.
- **Which page each link was found on**, which the link table has no column for
  and `wmse json` does.
- **With `-refs`, where inside that page.** The graph says a link was found on a
  document; only a reference says *where* — the byte offset, the line and column,
  and the text around the match. They are stored as a `.ref` file per source in
  the file's own archive, so a link found in five places keeps all five and you
  can read the evidence in any editor:

```
$ ./wmse read scan.wmse -refs
=== References ===
  2 reference file(s) in the archive section
    example.com/index.html.ref
    example.com/complex/9223/contacts~sort-asc.ref

  in https://example.com/:
    # source: https://example.com/
    # type: text/html; charset=utf-8
    # references: 9
    120	5:10	...<body> <a href="/complex/9223/contacts?sort=asc">c1</a> <a href="/comp...
```

  `-refs` without `-o` is an error: there would be nowhere to keep the references,
  and a flag that silently does nothing is worse than one that explains itself.

**Size.** The format is built to be small, and the two ideas that do most of the
work are *graph distribution* and *location*:

- **Graph distribution.** One interned string table for the whole file, sorted and
  front-coded, so a thousand URLs that share a host and a prefix cost a handful
  of bytes each. Repeated sub-structures (the variable lists inside patterns, the
  field lists inside contracts, the header sets of observed requests) are
  interned by content into dictionaries and referenced by a one-based id, with `0`
  reserved for "absent" so a record with nothing to say costs nothing.
- **Location.** A link is stored as its distance from the previous one in sorted
  order, not as a repeated literal; the relations are delta- and run-length
  encoded; an edge whose endpoints did not change costs a few bits.

Sections are then deflated individually, so a reader that wants the links never
inflates the emulation log. Measured on a 20 000-link scan — these numbers are
`TestFormatBeatsPlainTextByAnOrderOfMagnitude` in
[internal/wmse/perf_test.go](internal/wmse/perf_test.go), not estimates:

| | bytes | per link |
|---|---|---|
| the same scan as JSON | 6 380 308 | 319 |
| the same format, compression off | 831 018 | 41.6 |
| **`wmse` file** | **110 164** | **5.5** |

So the format itself is worth 8× before deflate is involved, and 58× against
plain text — and the per-link cost keeps falling as the scan grows, because a
new URL mostly brings a new suffix.

```
$ ./wmse info scan.wmse               # a 20 000-link scan
  section        codec      stored        raw   ratio
  ----------------------------------------------------
  meta           raw          67 B       67 B    100%
  stats          raw          31 B       31 B    100%
  strings        flate       3.6 KiB  473.2 KiB      1%
  links          flate     100.0 KiB  308.9 KiB     32%
  edges          flate       3.9 KiB   28.7 KiB     14%
  endpoints      flate        123 B     851 B     14%
```

**Format.** A property graph: 11 sections (meta, stats, strings, links, edges,
groups, endpoints, observations, params, emulation, dicts) behind a ~90-byte
header with a directory that names each section's codec, stored length and
offset. Unknown sections are skipped and unknown metadata keys are shown, so a
file written by a newer version is still readable. See
[internal/wmse/format.go](internal/wmse/format.go) for the section ids and edge
kinds, and `wmse info` for the layout of any file.

> A file holds the requests the scan really sent, which can include session
> material — tokens, cookies, ids. Values of `-H` and `-b` are *not* stored (the
> header names are), but a captured `Authorization` header in an observed request
> is evidence and is kept. `webmap` says so when it writes the file, and `wmse`
> repeats the warning when it opens one.

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
cmd/webmap/          CLI entry point, and the writer for saved scans
cmd/wmse/            Static explorer: the offline reader for -o files
internal/archive     tar / tar.gz / zip writer, and the entry-naming rules
internal/categorizer Link categorization
internal/color       ANSI colouring helpers
internal/config      Flags and configuration, shared by the scan and the reader
internal/contract    API contract model and rendering
internal/emulator    Semantic JS engine (goja sandbox + instrumented sinks)
internal/fetcher     HTTP fetching and HTML parsing
internal/graph       ASCII graph rendering
internal/jsanalyzer  Static JS analysis: tokenizer, parser, request/response inference
internal/linker      Link model, categories, references, and the display rules both reports print
internal/markdown    Markdown report generation
internal/patterns    URL/domain classification rules
internal/progress    Progress bar (stderr)
internal/urlgroup    URL pattern grouping
internal/wmse        The .wmse format: model, encoder, decoder
```

See [SPEC.md](SPEC.md) for the detailed functional specification and acceptance
criteria.

## License

[MIT](LICENSE)
