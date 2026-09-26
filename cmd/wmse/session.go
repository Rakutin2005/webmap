package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"apimap/internal/linker"
	"apimap/internal/wmse"
)

// A session is an interactive exploration of one or more saved scans. It is the
// same data the `read` report shows, navigated a question at a time: the report
// answers "what did you find", the session answers "where am I, what is here,
// what references it".
//
// The session stands at a place rather than at a target. A place is a page or a
// directory, and the two are not the same question - a page answers "what does
// this document link to", a directory answers "what is under here" - so which
// one the session is at is state it keeps, and the prompt shows it.
type session struct {
	// files are the loaded scans, in the order they were loaded. The first is
	// the one the session opened with; `extend` appends.
	files []loadedFile
	// merged is the union of every loaded scan's data, so a query spans all of
	// them. A scan's own headers stay in loadedFile for `info`.
	merged *wmse.Snapshot
	// target is the scope the scan ran under, and the base a path resolves
	// against before the session has moved anywhere.
	target string
	// pos is where the session is standing: a URL.
	pos string
	// dir says that pos is a directory rather than a page. It is false
	// whenever a page answers for pos, because a page always wins.
	dir bool
	// out is where the session writes; in tests it is a buffer.
	out io.Writer
	// in is the command source; a session can be driven by a script.
	in io.Reader
	// pages and nodes are lazily-built indexes over merged, so a session that
	// only runs `ls` never pays for the lookups the other commands need.
	pages pageIndex
	// pagePaths is the same pages by path alone, so a page reached with a query
	// is still the page a person means by its path.
	pagePaths map[string]string
	nodes     map[string]int
	// refs caches the parsed .ref sidecars by source, so `from` and `refs` do
	// not re-parse an archive entry per lookup.
	refs map[string]*wmse.RefFile
	// paths caches the completable paths, and is dropped by `extend` because
	// that is the one command that adds to them.
	paths []string
	// hist is what the up and down keys walk.
	hist *history
	// line accumulates the line being read when there is no line editor to draw
	// it, which is the piped and scripted path.
	line []rune
	// interactive says whether to draw a prompt, a banner and a line editor. A
	// piped or scripted session turns it off so its output is just the answers.
	interactive bool
	// forceColour colorizes wherever the output goes. See colour.
	forceColour bool
	// keyPath is the -i the session was opened with, kept so  can re-seal a
	// file the session opened as ciphertext.
	keyPath string
	// keys is the one reader the session has. The line editor reads from it and a
	// question asked mid-command reads from it too, because two readers on one file
	// is two readers competing: whichever buffered first would swallow the other's
	// input, and a question would eat the commands typed after it.
	keys keySource
	// editWarned remembers that the person has been told what writing a signed file
	// does to its signature, and said yes. Being asked once is a decision; being
	// asked about every save after it is a nuisance.
	editWarned bool
}

// loadedFile is one scan the session has open, kept whole so `info` can show its
// header and `extend` can report what each contributed.
type loadedFile struct {
	path string
	info *wmse.FileInfo
	snap *wmse.Snapshot
}

// reload re-reads a file the session has open, after a command has changed it on
// disk. Without it the session's own view is the file as it was, and a command that
// signed or cleared a signature would leave `info` describing something that is no
// longer there.
func (s *session) reload(f *loadedFile) error {
	opened, err := wmse.Open(f.path)
	if err != nil {
		return err
	}
	snap, err := opened.Load()
	if err != nil {
		return err
	}
	f.snap = snap
	f.info = opened.Info()
	s.nodes, s.pages, s.pagePaths, s.paths, s.refs = nil, nil, nil, nil, nil
	if len(s.files) > 0 && s.files[0].snap == f.snap {
		s.merged = snap
	}
	return nil
}

// runSelect opens an interactive session on the loaded scan and reads commands
// until end of input.
func runSelect(o *options, in io.Reader, out io.Writer, keyPath string) error {
	s := &session{
		files:       []loadedFile{{path: o.path, info: o.info, snap: o.snap}},
		merged:      o.snap,
		target:      o.target,
		out:         out,
		in:          in,
		hist:        &history{limit: historyLimit},
		interactive: true,
		keyPath:     keyPath,
	}
	// The session opens standing at the target, as a page if one is there and
	// as a directory otherwise, so the first `ls` answers about the site rather
	// than about nothing.
	s.standAt()
	s.reportSignature()
	s.loop()
	return nil
}

// reportSignature says what the file claims about itself, and whether the claim
// holds.
//
// This is said on opening rather than only when the file is about to be changed,
// because the two answers are different. A file whose signature does not verify is
// already not what its signer vouched for, and somebody reading it now is being
// shown something they would want to know about before they read it rather than
// after they trusted it. A file with no signature is not reported at all: it never
// claimed anything, and "not authentic" would be a much stronger claim than the one
// that is true.
func (s *session) reportSignature() {
	if len(s.files) == 0 {
		return
	}
	f := &s.files[0]
	sig, err := wmse.VerifyFile(f.path)
	if errors.Is(err, wmse.ErrUnsigned) {
		return
	}
	if err != nil {
		// A signature that cannot be checked is not the same as one that does not
		// match, and the difference is whether anybody is in a position to say.
		fmt.Fprintf(s.out, "  %s\n", s.warn("this file is signed, and the signature could not be checked: "+err.Error()))
		if sig != nil {
			fmt.Fprintf(s.out, "  %s\n", s.dim("signed by "+sig.Signer+" on "+whenOf(sig)))
		}
		return
	}
	fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("signed by %s on %s, and the signature checks out",
		sig.Signer, whenOf(sig))))
}

// standAt puts the session at its starting place: the scan's target, as a page if
// a document is there and as a directory otherwise.
func (s *session) standAt() {
	p := s.placeAt(s.target)
	s.pos, s.dir = p.url, p.dir
}

// banner is what the session says when it opens: which file, where it is
// standing, and the commands, so that a person who has never seen it knows what
// to type without leaving the prompt for a man page.
func (s *session) banner() {
	if !s.interactive {
		return
	}
	f := s.files[0]
	fmt.Fprintf(s.out, "%s  %s\n", s.bold("WebMap Static Explorer"), f.path)
	fmt.Fprintf(s.out, "  target    %s\n", s.target)
	fmt.Fprintf(s.out, "  standing  %s %s\n", s.pos, s.dim(s.placesAs()))
	fmt.Fprintf(s.out, "  loaded    %s\n", plural(len(f.snap.Links), "link"))
	if len(f.snap.ArchiveNames) > 0 {
		fmt.Fprintf(s.out, "  archive   %s\n", archiveBrief(f.snap.ArchiveNames))
	}
	fmt.Fprintf(s.out, "\n  %s\n", s.dim("cd [path] move · ls list here · ls -d list as a directory · what <path> identify it"))
	fmt.Fprintf(s.out, "  %s\n", s.dim("from <path> who references it · find <kind> <glob> search · api [path] describe · history · help quit"))
	fmt.Fprintf(s.out, "  %s\n", s.dim("Tab completes, the arrows walk the line and the history, Ctrl-C abandons a line"))
}

// placesAs names what the session is standing at, in the words the commands
// use: a page has links, a directory has children.
func (s *session) placesAs() string {
	if s.dir {
		return "(directory)"
	}
	return "(page)"
}

// loop reads one command per line until the input ends or the person quits.
func (s *session) loop() {
	s.banner()
	s.run()
}

// run is the command loop. It is separated from loop so a test can drive a
// session with a scripted input and read the transcript, which is the same path
// an interactive person takes.
func (s *session) run() {
	if !s.interactive {
		s.runLines(newPipeSource(s.in))
		return
	}
	src, tty, err := newTTYSource(os.Stdin)
	if err != nil {
		// No terminal to take over. The session still answers every question; it
		// just has no line editing, and saying so once is better than refusing
		// to start.
		fmt.Fprintf(s.out, "  %s\n", s.dim("(no terminal: line editing, completion and history are off)"))
		s.runLines(newPipeSource(s.in))
		return
	}
	defer tty.restore()
	s.keys = src
	ed := &lineEditor{
		src:      src,
		out:      s.out,
		hist:     s.hist,
		complete: s.complete,
		prompt:   s.promptLine,
	}
	for {
		line, err := ed.read()
		switch {
		case errors.Is(err, io.EOF):
			fmt.Fprintln(s.out)
			return
		case errors.Is(err, errInterrupted):
			// A line abandoned with Ctrl-C is not a failed session: the shell
			// drops the line and draws a new prompt, and so does this.
			continue
		case err != nil:
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if s.dispatch(line) {
			return
		}
	}
}

// runLines reads whole lines. It is the path for a pipe, a script and a test,
// and it is the same dispatch either way, so a scripted session and a typed one
// cannot answer differently.
func (s *session) runLines(src keySource) {
	for {
		k, err := src.readKey()
		if err != nil {
			return
		}
		if k.code != keyRune && k.code != keyEnter {
			continue
		}
		if k.code == keyEnter {
			line := strings.TrimSpace(string(s.line))
			s.line = s.line[:0]
			if line != "" && s.dispatch(line) {
				return
			}
			continue
		}
		s.line = append(s.line, k.r)
	}
}

// promptLine is the prompt: where the session is, and the name of the thing it
// is standing at.
//
// The name in brackets is the last segment of the path, which is a page's file
// name in one mode and a directory's name in the other, so /dir/2 reads as "2"
// and /dir reads as "dir". Repeating it is deliberate: a prompt that can be read
// at a glance is worth a few columns, and the bracket keeps the shape of the
// line the same whichever of the two the session is standing at.
func (s *session) promptLine() string {
	return fmt.Sprintf("explore@%s [%s] > ", s.pos, s.segment())
}

// segment is the name the prompt shows in brackets.
func (s *session) segment() string {
	p := strings.TrimSuffix(pathOnly(s.pos), "/")
	if p == "" {
		return "/"
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// dispatch runs one command line. It returns true when the session should end.
// The line goes into the history here, so a typed session and a scripted one
// remember the same things: the history is a record of what was asked, and where
// that record is written is not something the two paths should differ on.
func (s *session) dispatch(line string) bool {
	if s.hist != nil {
		s.hist.remember(line)
	}
	name, args := splitCommand(line)
	switch name {
	case "quit", "exit", "q":
		return true
	case "help", "?":
		s.help()
	case "cd":
		s.cmdCd(args)
	case "ls":
		s.cmdLs(args)
	case "read":
		s.cmdRead(args)
	case "source":
		s.cmdSource(args)
	case "save":
		s.cmdSave(args)
	case "req", "request":
		s.cmdRequest(args)
	case "continue":
		s.cmdContinue(args)
	case "sign":
		s.cmdSign(args)
	case "encrypt":
		s.cmdEncrypt(args)
	case "api":
		s.cmdAPI(args)
	case "what":
		s.cmdWhat(args)
	case "from":
		s.cmdFrom(args)
	case "find":
		s.cmdFind(args)
	case "info":
		s.cmdInfo(args)
	case "extend":
		s.cmdExtend(args)
	case "refs":
		s.cmdRefs(args)
	case "history":
		s.cmdHistory(args)
	default:
		fmt.Fprintf(s.out, "  %s\n", s.warn("unknown command "+name+"; try help"))
	}
	return false
}

// splitCommand takes the command name off the front of a line and returns the
// rest as quote-aware words. Quoting matters because several commands take a
// glob or a path that contains spaces, and a shell-style reader would be
// surprising in a prompt - but a reader that simply kept the quotes would make
// find "*" search for a pair of quote characters, which is worse.
func splitCommand(line string) (name string, args []string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return strings.ToLower(line[:i]), splitWords(line[i+1:])
	}
	return strings.ToLower(line), nil
}

// splitWords splits on whitespace but keeps a quoted run together, so
// `find any "v* user"` is two words, not three.
func splitWords(s string) []string {
	var words []string
	var cur strings.Builder
	inQuote := byte(0)
	started := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			inQuote = c
			started = true
		case c == ' ' || c == '\t':
			if started || cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(c)
		}
	}
	if started || cur.Len() > 0 {
		words = append(words, cur.String())
	}
	return words
}

func (s *session) help() {
	fmt.Fprintf(s.out, "\n  %s\n", s.bold("commands"))
	for _, c := range [][2]string{
		{"cd [path]", "move to a path; a page is preferred over a directory"},
		{"ls [path]", "list the links of the current page, or the paths of a directory"},
		{"ls -d [path]", "list a path as a directory, whatever page answers for it"},
		{"read [flags]", "the scan's whole report, with the reader's flags"},
		{"source <path> <file>", "write a file out; from the archive, or fetched"},
		{"save <path>", "download a file into this file's own archive"},
		{"req|request <path> [args]", "run curl against a path, with curl's own flags"},
		{"continue [flags]", "scan further, with the scan's own arguments"},
		{"sign <how> <key>", "sign this file, or `sign none` to remove the signature"},
		{"encrypt <how> <key>", "encrypt this file: password, rsa or aes"},
		{"what <path>", "identify what a path is: kind, class, and where it was seen"},
		{"from <path>", "list the pages that reference a resource"},
		{"api [path]", "describe an API endpoint: its contract, or why there is none"},
		{"find <kind> <glob>", "search by kind (api, js, css, image, html, any) and glob"},
		{"refs [path]", "show where links were found, from the .ref sidecar"},
		{"info", "the file header: target, scope, command, what it holds"},
		{"extend <file>", "open another scan and query across both"},
		{"history", "what has been typed this session; the arrows walk it"},
		{"help", "this list"},
		{"quit", "leave the session"},
	} {
		fmt.Fprintf(s.out, "    %s %s\n", s.bold(c[0]), s.dim("- "+c[1]))
	}
	fmt.Fprintf(s.out, "\n  %s\n", s.dim("Tab completes, the arrows move and recall, Ctrl-C abandons a line"))
}

// commandNames is the set a first word is completed from, and the set an unknown
// word is checked against, so the two can never disagree about what exists.
var commandNames = []string{
	"api", "cd", "continue", "encrypt", "extend", "find", "from", "help",
	"history", "info", "ls", "quit", "read", "refs", "req", "request", "save",
	"sign", "source", "what",
}

// findKinds is what `find` accepts as its first argument, in the tool's own
// words so that a search means here what it means in the report.
var findKinds = []string{"any", "api", "asset", "css", "doc", "font", "html", "image", "js", "media", "page"}

// complete offers the words the word being typed could become. It is the whole
// of Tab, and it is worth having per command: a path after `cd`, a page after
// `what`, a file on disk after `extend` and a kind after `find` are four
// different sets, and offering the wrong one would be worse than offering none.
func (s *session) complete(line []rune, start int, word string) []string {
	if start == 0 {
		return spaced(commandNames)
	}
	name, _ := splitCommand(string(line))
	switch name {
	case "ls":
		return s.completePath(word, "-d")
	case "cd", "what", "api", "from", "refs":
		return s.completePath(word)
	case "read", "continue":
		return completeRead(word)
	case "sign", "encrypt":
		return s.completeCrypto(line, start, word)
	case "req", "request", "source", "save":
		return s.completePath(word)
	case "extend":
		return completeFile(word)
	case "find":
		if argIndex(line, start) == 1 {
			return findKinds
		}
	}
	return nil
}

// spaced terminates every command name, so accepting a completion leaves the
// cursor ready to type the first argument rather than in the middle of the name.
func spaced(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + " "
	}
	return out
}

// argIndex is which argument the word being completed is, counting the command
// itself as the first. It is what tells `find` a kind is wanted from a glob.
func argIndex(line []rune, start int) int {
	return len(splitWords(string(line[:start])))
}

// completePath offers the paths the file knows, written the way the session
// prints them, so a completed word is exactly the argument that would work.
func (s *session) completePath(word string, flags ...string) []string {
	if strings.HasPrefix(word, "-") {
		return flags
	}
	var out []string
	for _, p := range s.knownPaths() {
		if pathCandidateMatches(p, word) {
			out = append(out, p)
		}
	}
	return out
}

// knownPaths is every path a person could mean, in the form the session prints:
// the paths of the fetched pages and of every link, the host kept only when it
// is not the target's.
func (s *session) knownPaths() []string {
	if s.paths != nil {
		return s.paths
	}
	seen := map[string]bool{}
	var out []string
	add := func(full string) {
		if full == "" {
			return
		}
		p := s.field(full)
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for p := range s.ensurePageIndex() {
		add(p)
	}
	for i := range s.merged.Links {
		if s.merged.Links[i].Synthesized {
			continue
		}
		add(wmse.LinkKey(&s.merged.Links[i]))
	}
	sort.Strings(out)
	s.paths = out
	return out
}

// pathCandidateMatches says whether a path is a candidate for a word being
// typed. An absolute word is a path and matches by prefix. A bare word is a
// name, and a name is the part of a path a person remembers, so it matches any
// segment: `cont` finds /complex/9223/contacts and `9223` finds the same thing.
func pathCandidateMatches(p, word string) bool {
	if word == "" {
		return true
	}
	if strings.HasPrefix(word, "/") || strings.Contains(word, "://") {
		return strings.HasPrefix(p, word)
	}
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if strings.HasPrefix(seg, word) {
			return true
		}
	}
	return false
}

// completeCrypto offers the methods sign and encrypt take, and then the key files
// on disk. The methods are the tool's own words, so Tab and the error message
// cannot disagree about what they are.
func (s *session) completeCrypto(line []rune, start int, word string) []string {
	name, _ := splitCommand(string(line))
	before := strings.TrimSpace(string(line[:start]))
	if before == name {
		if name == "sign" {
			return []string{"gpg ", "none ", "rsa ", "x509 "}
		}
		return []string{"aes ", "password ", "rsa "}
	}
	// The second word is a key file. A password is not a file, so nothing is offered
	// that could be taken for one.
	return completeFile(word)
}

// completeRead offers the reader's flags, so `read -<Tab>` names them. They come
// from the same registration the reader and the scan use, so a flag the reader
// stops taking stops being offered here in the same commit.
func completeRead(word string) []string {
	if !strings.HasPrefix(word, "-") {
		return nil
	}
	_, fs := newFlagSet()
	var out []string
	fs.VisitAll(func(f *flag.Flag) { out = append(out, "-"+f.Name) })
	sort.Strings(out)
	return out
}

// completeFile offers the names in the current directory, so a file argument can
// be typed by its first letters. It is the one completion that looks outside the
// file, and only because the argument is a file on disk.
func completeFile(word string) []string {
	dir, base := ".", word
	if i := strings.LastIndexByte(word, '/'); i >= 0 {
		dir, base = word[:i+1], word[i+1:]
	}
	if dir == "" {
		dir = "/"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		full := dir + name
		if !strings.HasSuffix(full, "/") && e.IsDir() {
			full += "/"
		}
		out = append(out, full)
	}
	sort.Strings(out)
	return out
}

// resolve turns a path argument into a full URL, relative to where the session
// is standing. An argument that is already a URL is taken as it is; a path is
// joined to the current place.
//
// Resolving against the position rather than the target is what makes navigation
// feel like moving through the site: standing at /dir/2, `cd 1` means the page
// next to this one, not a path relative to the root. A directory is resolved
// with a trailing slash so that a name typed inside one lands inside it, which
// is the whole difference between `cd sub` going to /dir/sub and going to /sub.
func (s *session) resolve(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	base := s.pos
	if base == "" {
		base = s.target
	}
	if s.dir && !strings.HasSuffix(base, "/") {
		base += "/"
	}
	// "." is the place itself, the way it is in a shell. Left to URL resolution
	// it would name the containing directory, so `what .` and `ls .` would answer
	// about somewhere else, and `cd .` would move when it should not.
	if path == "." {
		return base
	}
	// ".." is handled apart from the rest, because a URL's own rule for it is not
	// the one a person means. RFC 3986 resolves ".." against a base that is not a
	// directory by discarding the last segment and then going up, so from /dir/1
	// it gives / - one level further than the page is. A browser's up-level, and
	// a shell's, go from /dir/1 to /dir. The person at the prompt means those.
	if path == ".." || strings.HasPrefix(path, "../") {
		tail := strings.TrimPrefix(path, "..")
		return s.resolveUp(base, strings.TrimPrefix(tail, "/"))
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return path
	}
	ref, err := url.Parse(path)
	if err != nil {
		return path
	}
	return u.ResolveReference(ref).String()
}

// resolveUp goes one level up from a place, keeping whatever the reference named
// on the way: "../other" is the sibling called other.
func (s *session) resolveUp(from, tail string) string {
	u, err := url.Parse(from)
	if err != nil || u.Host == "" {
		return from
	}
	dir := path.Dir(u.Path)
	if dir == "" {
		dir = "/"
	}
	u.Path = dir
	if tail != "" {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + tail
	}
	return u.String()
}

// A place is somewhere the session can stand: a page, or a directory standing in
// for a set of paths.
type place struct {
	url string
	dir bool
}

// placeAt decides what kind of place a URL is, and a page always wins.
//
// The order is the whole of the rule. If the scan fetched a document at the
// path, then that document is the interesting answer to "what is here", and the
// files beneath it are a second question - so a page is chosen first. Failing
// that, a path written as a directory may name its own index, and reaching /dir
// through /dir/index.html is what a visitor's browser does, so an existing index
// is the page too. Only a path with no document at it is a directory, which is
// exactly the case the rule exists for: /dir with no index answers as the
// directory, and lists what is under it.
func (s *session) placeAt(full string) place {
	if p := s.pageAt(full); p != "" {
		return place{url: p}
	}
	dir := strings.TrimSuffix(full, "/")
	// A path spelled with a trailing slash names the same place as one without,
	// and a scan may have recorded it either way, so both are asked before the
	// path is given up on.
	if dir != full {
		if p := s.pageAt(dir); p != "" {
			return place{url: p}
		}
	}
	for _, cand := range indexCandidates(dir) {
		if p := s.pageAt(cand); p != "" {
			return place{url: p}
		}
	}
	return place{url: dir, dir: true}
}

// indexCandidates are the documents a directory is reached through, in the order
// a server would pick them. The list is short on purpose: a name not on it is
// still reachable by typing it, and a long list would be guessing.
func indexCandidates(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{
		dir + "/",
		dir + "/index.html",
		dir + "/index.htm",
		dir + "/index.php",
		dir + "/index.aspx",
	}
}

// pageAt returns the URL the file recorded a fetched document at, or "" if the
// scan fetched nothing there. It is a lookup and a resolution in one, because the
// URL it returns is the one the session should stand on: when a path is asked
// for and the document was recorded under a query, the recorded URL is the truth
// and the guess is not, and a position that was only a guess would make every
// later lookup of it a lookup of something the file never held.
//
// A link that was classified as a page deliberately does not count, and the
// reason is the difference between knowing a place and having been there. A link
// to /dir says the scan saw an href; it says nothing about what is at /dir. A
// page the crawl never fetched has no recorded links, so standing there as a page
// would answer every question with "nothing" and hide the paths that are
// actually under it. A page record is the file saying the document was read, and
// that is the only thing that makes a place worth standing on.
func (s *session) pageAt(full string) string {
	if s.ensurePageIndex()[full] != nil {
		return full
	}
	// The host and the path together are enough for the rest. A page fetched as
	// /item?id=1 is the page a person means by /item, and every other command in
	// the session already answers for the bare path; navigation that insisted on
	// the query would be the one place where the same name meant two things.
	if s.pagePaths == nil {
		s.pagePaths = make(map[string]string, len(s.merged.Pages))
		for p := range s.ensurePageIndex() {
			if _, dup := s.pagePaths[hostPathKey(p)]; !dup {
				s.pagePaths[hostPathKey(p)] = p
			}
		}
	}
	return s.pagePaths[hostPathKey(full)]
}

// hostPathKey is what identifies a document when its query is ignored: the host
// and the path. The host is not a detail of that - the path on its own is not
// enough, because every site on the internet has a document at "/", so an index
// keyed on the path alone would decide that any host in the file is standing on
// the target's own home page, and a session pointed at one site would answer for
// another.
func hostPathKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return strings.ToLower(u.Host) + "\x00" + pathOnly(rawURL)
}

// nodeFor finds the link node for a resolved URL.
//
// It first looks for an exact match, then falls back to matching on the path
// alone. The fallback is what a person needs: a link the scan recorded as
// "/item?id=1" is named "/item" in a report, in a reference file and in the
// conversation, and an exact-match-only lookup would report it as absent. When
// several links share a path (the same page reached with different queries) the
// best node wins and the caller can say there were others.
func (s *session) nodeFor(full string) (linker.Link, bool) {
	if l, ok := s.bestNode(func(key string) bool { return key == full }); ok {
		return l, true
	}
	byPath := pathOnly(full)
	return s.bestNode(func(key string) bool { return pathOnly(key) == byPath })
}

// bestNode returns the best link whose key satisfies a predicate, preferring a
// page the crawl fetched over one that only appears as a link.
func (s *session) bestNode(match func(key string) bool) (linker.Link, bool) {
	var best linker.Link
	found := false
	for i := range s.merged.Links {
		l := &s.merged.Links[i]
		if !match(wmse.LinkKey(l)) {
			continue
		}
		if !found || betterNode(l, &best) {
			best, found = *l, true
		}
	}
	return best, found
}

// betterNode prefers a page node (one the crawl fetched) over a bare link, and
// a non-synthesized node over one the file added to give a relation an endpoint.
func betterNode(cand, cur *linker.Link) bool {
	if cur.Category == linker.CategoryWebPage && cand.Category != linker.CategoryWebPage {
		return false
	}
	if cand.Category == linker.CategoryWebPage && cur.Category != linker.CategoryWebPage {
		return true
	}
	return cand.SourceURL != "" && cur.SourceURL == ""
}

// column pads a value to a visible width and then styles it. A nil style leaves
// it alone, so the same padding serves a column that is written plainly.
//
// The order is the whole point. A width verb counts the bytes of an escape
// sequence as columns, so `%-24s` on a styled string pads nothing that a person
// can see and the column after it drifts left - and the drift is different on
// every row, because the escape sequences are the same length but the values are
// not, so a table of links comes out as a ragged edge. Styling a value that has
// already been padded puts the padding inside the styling, where it takes no space
// on the screen and therefore costs nothing.
func (s *session) column(style func(string) string, v string, width int) string {
	for len(v) < width {
		v += " "
	}
	if style == nil {
		return v
	}
	return style(v)
}

func (s *session) bold(v string) string {
	if !s.colour() {
		return v
	}
	return "\033[1m" + v + "\033[0m"
}
func (s *session) dim(v string) string {
	if !s.colour() {
		return v
	}
	return "\033[2m" + v + "\033[0m"
}
func (s *session) warn(v string) string {
	if !s.colour() {
		return v
	}
	return "\033[33m" + v + "\033[0m"
}

// colour asks whether to colorize. The session is interactive, so it turns
// color on when stdout is a terminal and off when it is a pipe, so piping a
// session into another tool gives clean text.
//
// forceColour answers yes regardless, and exists so a test can exercise the
// coloured path. That path is the one a person at a terminal sees, and it is the
// one where a width verb counts a styling sequence's bytes as columns and a table
// silently stops lining up - a fault that is invisible to a test which only ever
// writes to a buffer, and obvious to anyone reading the output.
func (s *session) colour() bool {
	if s.forceColour {
		return true
	}
	f, ok := s.out.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// plural is count with a plural noun.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
