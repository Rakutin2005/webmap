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
| `-refs` | Record, for every link, where in the source document it was found — file, byte offset, line:col, and the text around it — into a `.ref` file per source inside the `-o` file's archive section. Requires `-o`; without it the flag is an error, not a silent no-op. |

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
- with `-refs`, a sidecar archive holding one `.ref` file per source document,
  each listing where in that document every link was found: the byte offset, the
  line and column, and the text around the match;
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

The references are files rather than fields, for the same reason the graph is a
graph. One link found in five places has five references and one node, and only
a per-document list holds that without either losing four or pretending they are
the same fact; and the evidence is worth keeping where a person can read it
without the tool. A `.ref` file names its source in its first line and has one
line per finding:

```
# source: https://example.com/index.html
# type: text/html; charset=utf-8
# references: 9
120	5:10	...<body> <a href="/complex/9223/contacts?sort=asc">c1</a> <a href="/comp...
```

The archive that holds them is stored uncompressed inside the container, which
deflates it: compressing twice would pay for the same bytes twice. Its entry
names are interned like any other literal, so "what is in here" is answerable
without inflating the payload.

Header **values** configured with `-H`/`-b` are not stored (their names are, and
`session_data=true` marks a file that may hold captured session material); the
requests the scan actually sent are stored as captured, because they are the
evidence.

### Format

A property graph in one file: `magic + version + header length + flags +
directory`, then twelve independent sections (`meta`, `stats`, `strings`,
`links`, `edges`, `groups`, `endpoints`, `observations`, `params`, `emulation`,
`dicts`, `archive`), each with its own codec and length in the directory, and
each deflated independently. Sections are decoded on demand, so a reader that
only wants the links never inflates the emulation log, and the `archive` section
is absent unless `-refs` wrote it. A section the reader does not know is skipped,
so a file written by a later version is still readable.

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
- **`-refs` shows what the scan recorded.** The scan uses it to *capture* where
  each link was found; the reader uses it to *show* it, listing the `.ref` files
  in the archive and the findings for the entry point. A file with no archive
  says the scan ran without `-refs` rather than printing an empty section.
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

### The `select` session

`wmse select <file>` opens an interactive session on the same data, reading one
command per line. It is the report navigated by hand, not a second data model:
every command is a line you could have typed, and a session can be piped
(`… | wmse select f`) or scripted, so the same answers are reachable without a
terminal.

#### Where the session is standing

A session stands at a *place*, and a place is either a page or a directory. The
two are not the same question — a page answers "what does this document link
to", a directory answers "what is under here" — so which one the session is at is
state it keeps, and the prompt shows it:

```
explore@https://example.com/ [/] >              a page
explore@https://example.com/users [users] >     a page
explore@https://example.com/api [api] >         a directory
```

The name in brackets is the last segment of the path, which is a page's file name
in one mode and a directory's name in the other, so `/dir/2` reads as `2` and
`/dir` reads as `dir`. A path is resolved against the position rather than the
scope, so `cd 1` inside `/dir` means the page next to this one; `..` goes up one
level the way a browser's does, which is not what RFC 3986's own rule for `..`
against a base that is not a directory would give.

A **page always wins**, and the order is the whole of the rule:

1. The scan fetched a document at the path → that page. A link merely *classified*
   as a page does not count: a link to `/dir` says the scan saw an href, not that
   anything is at `/dir`, and a page the crawl never fetched has no recorded
   links, so standing there would answer everything with "nothing".
2. Otherwise, if the path names an index that was fetched (`/dir/`, `/dir/index.html`,
   `/dir/index.htm`, `/dir/index.php`, `/dir/index.aspx`) → that index, which is
   what a visitor's browser reaches.
3. Otherwise → a directory, and `ls` lists every path matching it at any depth,
   whether or not an index exists. A directory is a prefix, not a level, so
   stopping at one level would hide what is two deep.

When a path is asked for and the document was recorded under a query, the
position becomes the URL the file recorded: a position that was only a guess would
make every later lookup of it a lookup of something the file never held.

#### Commands

Argument splitting is quote-aware, so a glob with a space in it works.

- `cd [path]` moves, choosing page or directory by the rule above. With no path it
  returns to the scan's target.
- `ls` answers for the current place: a page's own links (from the file's "found
  on" relation, not a re-derivation), or the paths under a directory.
- `ls [path]` answers about another place **without moving** — one command that
  both answers and moves is one whose result is hard to predict.
- `ls -d [path]` answers as a directory whatever page answers for the path, **and
  moves**, because choosing how to look at a path is a decision about where to
  stand.
- `read [flags]` is `wmse read`, run from inside the session: the same report, the
  same sections and the same gates, from the same code - a second rendering of the
  same sections is how a tool starts describing one file two ways. A flag the file
  cannot answer says so on stderr beside the metadata of the run that made it, the
  rest of the report still prints, and the request is counted, because a session
  has no exit status and the summary is all it can give.

  **Its defaults are the scan's own flags**, taken from the command line the file
  records in its metadata. A person who has opened a file and is standing in it
  asking to be told the whole thing again means the thing as it was made, not as a
  fresh request would default. A flag typed on the `read` line is applied *on top*,
  which is what a flag is: one you do not pass takes its default, and the default
  is now the scan's value. The report names which of the two it used, because a
  section nobody asked for is otherwise indistinguishable from one the file cannot
  answer.

  The command line keeps its own defaults: `wmse read <file>` with no flags is
  still the reader's documented default report, which is what its flags hint
  exists to describe.

  A recorded command is text written to be read by a person, so it is taken apart
  the way a shell would: the first word is the program, a word starting with a
  dash is a flag, a bare `--` ends the flags, and a flag's own declaration says
  whether the next word is its value. The reader registers every flag the scan has,
  so a `webmap` command yields all of them - the ones that shape a section, the
  ones that scope the report, and the ones the reader accepts and ignores. A flag
  it does not know comes from another tool or a hand-edited line, and is left out
  with its value rather than refused: the report is about sections, and a flag with
  no meaning here has no section to show. A file that records no command has no
  settings to start from, and the report falls back to the reader's own defaults
  rather than inventing them.
- `api [path]` describes an endpoint from its contract, or gives one of four
  distinct refusals — *not found*, *not an API*, *an API with no contract in the
  file*, and *a contract but no captured request* — because those are four
  different facts and a reader of one of them learns something. A contract is
  looked up before the link's category, so a URL recovered from a bundle answers
  even where no link was recorded.
- `what <path>` identifies a resource: its kind, class, resolved form, the depth
  and page it was seen on, and pointers to the commands that go deeper.
- `from <path>` lists the pages that reference a resource, from the file's own
  "found on" relation.
- `find <kind> <glob>` filters by kind (`asset`, `html`, `css`, `image`, `api`,
  `js`, `any`, …) and a unix glob, printing URLs one per line so a later
  substitution can consume them.
- `refs [path]` reads the `.ref` sidecar `-refs` wrote, showing where each link was
  found.
- `info` prints the header of every loaded file.
- `extend <file>` opens another saved scan and folds it in, so a question can span
  two of them; the node sets and relations are unioned and the link relations
  re-pointed, and `info` then reports every file.
- `history` writes down what has been typed; the up and down keys walk the same
  list.

#### The line itself

A typed session has a line editor, so a correction is visible before it is run
rather than after:

- **Tab** completes the word under the cursor. One candidate is inserted whole,
  several are completed as far as they agree, and when there is nothing more to
  insert they are written out. Candidates are per command, because a path after
  `cd`, a flag after `ls`, a kind after `find` and a file on disk after `extend`
  are four different sets. A bare word matches any *segment* of a path, because a
  name is the part of a path a person remembers: `cont` finds
  `/complex/9223/contacts`.
- **Arrows** move along the line, and up and down walk the history. Walking past
  the newest entry returns to the line that was being typed.
- **Ctrl-A/E/U/K/W**, Home, End and Delete work as they do in a shell. **Escape**
  abandons the line. **Ctrl-C** abandons the line without leaving the session, and
  **Ctrl-D** on an empty line leaves it — the shell's two exits, kept apart.
- Escape sequences are read in every spelling terminals use (`ESC [ A`, `ESC O H`,
  `ESC [ 3 ~`, `ESC [ 1 ~`, …), and a byte that turns out not to continue one is
  handed back, so pressing Escape and then typing does not lose a letter.
- History is kept for the session and **written nowhere**: a session that promises
  to touch nothing but the file it was opened on should not leave a trail in the
  home directory.
- With no terminal to take over — a pipe, a script, a test — the session says so
  once and reads whole lines. It loses the editing and nothing else: the same
  dispatch answers either way, so a scripted session and a typed one cannot
  disagree.

Commands that build artifacts from what the session has — `build`, and the `$(…)`
substitution and `if` forms — are later releases.

#### The four commands that go outside the file

The rest of a session reads the file and nothing else. These four do not, and each
one says which it is before it happens, because the difference between "I read
this" and "I fetched this, and put it in your scan" is the difference between
reading a document and changing one.

- **`source <path> <file-to-save>`** writes a file to disk. The archive is asked
  first, and a file it holds is written from it — no network, and the same bytes
  every time, which is the point of having kept them. A path the archive does not
  hold is fetched from the server instead, and is deliberately **not** archived:
  `source` writes to a path the person named, `save` is the command that grows the
  file, and one command doing both would turn a read into a write of a file that
  may be somebody's scan of an hour's work. A failed download leaves no file: the
  write goes through a temporary name in the same directory and is renamed over
  the target, so a download that dies part way cannot replace a whole file with
  half of one.
- **`save <path>`** downloads a file into the file's own archive and rewrites the
  file. This is the one command that changes the file the session was opened on,
  so the size is said before the write, and the write is atomic — an interrupted
  save leaves the scan as it was. The scan's findings are untouched: a saved file
  is a copy of what the server sent, kept so `source` can answer for it later
  without the network. Saving the same path twice replaces the entry rather than
  adding a second one under the same name, which is a tar that extracts one of them
  and leaves the other behind.
- **`req|request <path> [curl arguments]`** runs curl, with the arguments passed to
  it as its own argv — no shell between — so a value containing a space, a quote or
  a semicolon is one argument and not a command. curl is run rather than imitated:
  the arguments a person writes here are curl's own, and a Go implementation of
  that subset would agree on the common cases and disagree on the rest, which is
  the worst of both, because the request that went out would not be the one that
  was asked for and nothing would say so. The request is shown before it is made,
  and curl's exit status is translated — "the server answered with an HTTP error"
  rather than "exit status 22".
- **`continue [flags]`** scans further, with the scan's own arguments. **It is not
  a resume**, and the difference is worth stating rather than leaving to be
  discovered: the scan records no frontier of what it has already fetched, so a
  deeper run re-fetches what the last one covered and goes further. The result is a
  superset, and the requests for the part already seen are the price. The three
  words that must not be inherited are the ones that would send the result
  somewhere else — the output file and the scope — and everything else is the
  scan's own, with the flags typed here applied last. The new findings are merged
  into the session and the file the run wrote is removed: the point is to keep
  exploring what is open, and the file the session was opened on is not touched.

A fetch carries over the one setting it cannot do without — whether the scan
skipped TLS verification — recovered from the recorded command rather than asked
for, because a file scanned with `-k` against a certificate nobody trusts is a
file whose every request would otherwise be refused, and the person who scanned it
already answered the question. User-supplied headers go to the target's own host
only: a file may also name a third party, and a token must not follow one there.

The archive is described as what it holds. `save` puts downloaded files in beside
the reference files, so the places that say how many files it has count the two
kinds separately rather than calling them all references.

#### Signing and encrypting a file

A scan is a record of somebody else's site, and a file that has been signed is a
claim about who vouched for it. Both are properties of the file rather than of the
session that made it, so both live in the file.

**`sign <rsa|x509|gpg|none> <path-to-key>`** writes a `signature` section. The
signature covers the file's own bytes **with that section's range taken out** — not
the sections re-encoded, and not the snapshot it was built from. Two properties
follow, and both are the reason for that definition:

- Re-signing works, because the bytes being signed do not contain the answer, so
  the answer can be replaced without the data changing underneath it.
- A later version of the writer cannot invalidate a signature without changing a
  fact in the file, which re-encoding would allow.

Signing is a fixed point: the signature's own size is part of the file, and the file
is what is signed, so the writer marshals, signs, and marshals again until the size
stops moving. A file whose signature would not settle is refused rather than signed
with a signature that does not match it.

- `rsa` is RSA-PSS over SHA-256 with a PEM private key.
- `x509` is the same signature, made with the key inside a certificate and **named
  by that certificate's distinguished name**. A certificate is the public half and
  does not contain the key, so `x509` needs a file holding both — a bundle — and a
  certificate alone is refused with that as the reason rather than a verification
  failure.
- `gpg` is a detached OpenPGP signature, made and checked by gpg. gpg is run rather
  than reimplemented: a signature only this tool can verify is a checksum with a key
  attached.
- `none` removes the signature. That is not the same as leaving a broken one: "this
  file claims nothing" and "this file's claim is broken" are different answers, and a
  reader has to be able to tell them apart. An unsigned file reports
  `ErrUnsigned`, never "invalid".

A signature is verified by finding the key beside the file and checking it. This
says *"this file has not changed since it was signed by the holder of this key"*,
which is a real property. It does not say the file is trustworthy, and it could not:
a file that carried its own public key would be signing itself, which proves only
that whoever wrote it had a key.

**Writing a signed file asks first**, and answers twice over:

- On open, a file whose signature does not verify says so before it is read, because
  somebody reading it now is being shown something they would want to know about
  before they read it rather than after they trusted it.
- Before any command that writes the file, a validly signed file stops and asks,
  naming the signer and the date, and saying the signature is **not re-made** —
  because only the signer can do that. The answer is remembered for the session: a
  person told once and agreeing does not need telling again for every save.

A session with no terminal to ask on answers **no**. A warning nobody can answer is
not a warning, it is a hang, and "nobody was asked" is not consent.

**`encrypt <password|rsa|aes> <password-or-path-to-key>`** seals the file: a small
header naming the method, then AES-256-GCM over the whole container. It asks first,
because it is the one command that takes the file away from the person who opened
it — after it, the file on disk is ciphertext and the session holds the only readable
copy.

- `password` derives the key with PBKDF2-HMAC-SHA256 at 600 000 iterations over a
  per-file random salt. A password given on the command line is said to be in the
  shell's history and the process list, because that is where it is and the only
  moment anybody can still change it.
- `rsa` seals a **random per-file content key** to a public key with OAEP. Two files
  sealed to the same public key are two unrelated files, so one leaked key opens
  neither of the others.
- `aes` uses a 32-byte key file as it stands. A key of another length is refused
  rather than stretched, because stretching a key somebody chose by hand is a way of
  pretending a short key is a long one.

The header is **authenticated, not encrypted**: it has to be readable for a reader
to know which algorithm to use, and it is fed to the cipher as additional data so
that editing it — naming a weaker derivation than the file was sealed with — makes
decryption fail rather than succeed wrongly. A wrong password is reported as a
wrong password; AEAD cannot tell a wrong key from a tampered file, and the answer
that is true every time somebody is prompted is the useful one.

A sealed file is opened by exactly the same code as one that was never sealed, once
the bytes are back: no second reader and no second format, so nothing about a scan
changes by having been encrypted.

- `-i <path-to-key>` is the only flag the reader has of its own, and it is the only
  one outside the scan's list. The reader's flags are the scan's flags one for one
  and that is checked by a test, so a reader-only flag registered alongside them
  would either break the promise or become a flag the scan does not have. It is
  taken out of the arguments by hand, where its absence from the parity list stays
  visible.
- There is **no automatic decryption for a password**: there is no file to point at
  a password, so `wmse select` asks for one — without echo, read a byte at a time so
  the session's own input is not swallowed — and probes it, continuing if it opens
  the file and stopping if it does not. The other verbs have nobody to ask and say
  so, naming the command that can.

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
- [x] `-refs` records where each link was found, as a `.ref` file per source in the archive
- [x] `-refs` without `-o` is an error, not a silent no-op
- [x] Every URL referenced by a relation is a node, so the graph has no dangling edges
- [x] `wmse` takes the scan's flags, one for one, and means what they meant there
- [x] `wmse read` prints the scan's report, sections and gates alike
- [x] `wmse select` explores a file interactively: ls, api, what, from, find, info, extend
- [x] A session's `read` is `wmse read`, and starts from the flags the scan recorded
- [x] `source` writes a file from the archive, or fetches one without archiving it
- [x] `save` downloads a file into the scan's own archive, and the write is atomic
- [x] `req` runs curl with the arguments given, and no shell between
- [x] `continue` scans on with the scan's own arguments, and says it is not a resume
- [x] Writing a file again does not double its relations, nor its parameters
- [x] `sign` signs a file with rsa, a certificate or a gpg key, and `sign none` removes it
- [x] A signature covers the file with its own section removed, so re-signing works and editing does not
- [x] Writing a signed file warns on open and asks before any write
- [x] `encrypt` seals a file with a password, an rsa key or an aes key
- [x] A sealed file opens with `-i` for a key, and is asked for a password
- [x] A wrong password is reported as a wrong password, and stops
- [x] The session stands on a page or in a directory, and a page always wins
- [x] `ls` lists the current page's links; `ls -d` asks for a path as a directory
- [x] A typed line is edited: Tab completes, the arrows move and recall, Ctrl-C abandons a line
- [x] With no terminal the session still answers, reading whole lines
- [x] A flag asking for data the file does not hold is an error with the run's metadata
- [x] `-rdepth` cuts the tree where the reader is asked, not where the scan stopped
- [x] `wmse read` explores the file offline and round-trips every section losslessly
- [x] A 20 000-link scan serializes to 5.5 bytes per link, sublinearly
- [x] Configured credential values are absent from the file; captured requests are kept