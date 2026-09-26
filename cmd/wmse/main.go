// Command wmse is WebMap's Static Explorer: it answers a WebMap command from a
// scan that was saved to a file, instead of from the network.
//
// A scan writes everything it found into one file with `webmap -o`, and this is
// the same command pointed at that file. The flags are the scan's own flags,
// registered from the same code, so the two cannot drift apart, and they mean
// what they meant there: -apic prints the contracts, -r prints the crawl, -waf
// reveals the URLs the original run hid, -rdepth cuts the tree where the reader
// is asked to rather than where the scan stopped.
//
//	webmap -r -apic -emulate -o scan.wmse -url https://example.com
//	wmse read scan.wmse -r -apic -emulate
//
// Nothing is fetched. A flag that asks for something the file does not hold is an
// error naming the flag and the metadata of the scan that made the file, never a
// silently empty section: an empty report reads like a site with nothing in it,
// and the whole point of the file is to tell those apart.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

const usage = `wmse - WebMap Static Explorer

Usage:
  wmse read <file> [flags]    the scan's report, recomputed from the file
  wmse info <file> [flags]    how the file is laid out, and what the scan put in it
  wmse json <file> [flags]    the whole snapshot as JSON, for other tools
  wmse select <file> [flags]  an interactive session over the file

-i <key-file> is the only flag the reader has of its own, and it is the only one
outside the scan's list: a file encrypted with an rsa or aes key is opened with it,
and one encrypted with a password is not - there is no file to point at a password
at, so wmse select asks for it and the other verbs say that they cannot.

The flags are the scan's flags: the same names, the same meanings, the same
defaults, so a scan command line can be replayed against its own file by changing
the verb. A saved file cannot be crawled and is never written, so the flags that
only ever configured the crawl are accepted and ignored.

Read:
  -r            the recursive crawl, as a tree (-rdepth cuts it)
  -apic         inferred API contracts
  -apic-raw     the request evidence behind them (implies -apic)
  -apif         API links as method and args
  -emulate      what the sandbox executed and called
  -G            the relation graph as adjacency lists
  -j            the endpoints that came out of JavaScript
  -refs         where each link was found, from the .ref sidecar the scan's
                -refs wrote (file, byte offset, line:col, surrounding text)
  -nogroup      leave out the URL pattern section

Scope and shape:
  -url string   the entry point to report on (default: the file's target)
  -a            every domain, not just that one
  -f, -follow   additional domains (comma-separated, subdomains included)
  -rdepth int   tree depth to stop at (0 = the whole tree)
  -rlimit int   the scan's request budget; here, lines of report (0 = no limit)
  -t            show link tags
  -waf -cdn     show WAF / CDN URLs
  -cache -noise show cache / noise URLs
  -full         all four URL classes at once
  -color        colorize output

Accepted, and ignored:
  -k -cf -str -M -T -group-count -H -b -cookie -headers-all-hosts -o -o-raw
  -patterns -bitrix -wp -react -emu-timeout -emu-workers -emu-maxjs -emu-maxjobs
  -emu-maxleaks

Flags go on either side of the file, so both of these work:
  wmse read scan.wmse -r -rdepth 3
  wmse read -r scan.wmse

The select session (wmse select <file>) reads one command per line at a prompt
showing where it is standing and the name of the place. The session stands on a
page or in a directory, and which one it is decides what ls answers:

  explore@https://example.com/ [/] >            a page: ls lists its links
  explore@https://example.com/ [users] > cd /users
  explore@https://example.com/users [users] >   a page, so ls lists its links
  explore@https://example.com/api [api] >       a directory, so ls lists the
                                                paths under it
  explore@https://example.com/api [api] > ls -d /users
                                                ask for a path as a directory
                                                whatever page answers for it

  cd [path]        move; a page is preferred over a directory, and a directory
                   with an index is reached through it
  ls [path]        the current page's links, or the paths under a directory
  ls -d [path]     ask for a path as a directory, whatever page answers for it
  what <path>      what a path is: kind, class, where it was seen
  from <path>      the pages that reference a resource
  api [path]       an endpoint's contract, or why there is none
  find <kind> <g>  search by kind and glob
  read [flags]     the scan's whole report, with the reader's flags
  refs [path]      where links were found, from the .ref sidecar
  info             the header of every loaded file
  extend <file>    open another scan and query across both
  history          what has been typed this session

read is wmse read, run from here: the same report, the same sections and the same
gates, over the files this session has open. It takes the reader's flags, so
"show me the whole thing again, this time with -r" needs no second command line.
read -h lists them.

With no flags, read starts from the flags the scan itself ran with, which the file
records in its metadata - so read alone shows the report as that scan saw it, with
the tree, the contracts and the references it asked for. A flag typed here is
applied on top, since a flag you do not pass is one that takes its default, and
the default is now the scan's own value. The report says which of the two it used.

Tab completes - commands, then whatever the command takes: paths after cd, kinds
after find, flags after read, files after extend. The arrows move along the line
and walk the history. Ctrl-C abandons a line, Ctrl-D on an empty one leaves. A
piped session reads the same lines without line editing.

Four commands go outside the file, and each says which it is before it happens:

  source <path> <file>  write a file out. The archive is asked first, so a file
                         the scan kept is written without the network; what is not
                         kept is fetched, and is NOT added to the archive - save
                         is the command that grows the file.
  save <path>            download a file into this file's own archive and rewrite
                         the file. The one command that changes the file you
                         opened: the size is said first, and the write is atomic.
  req|request <path> [a] run curl against a path, with curl's own arguments passed
                         through untouched and no shell between. The request is
                         shown before it is made; curl's status is named, not
                         printed as a number.
  continue [flags]       scan further with the scan's own arguments. NOT a resume:
                         no frontier is stored, so a deeper run re-fetches what it
                         already covered and goes further. The scope becomes where
                         you are standing, and the flags you type are applied last.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch verb := os.Args[1]; verb {
	case "read", "info", "json", "select":
		if err := run(verb, os.Args[2:]); err != nil {
			// A help request is not a failure. `webmap -h` exits 0 as well,
			// so the reader must not answer it with an error line.
			if errors.Is(err, flag.ErrHelp) {
				fmt.Print(usage)
				os.Exit(0)
			}
			// A bad flag is answered the way the scan answers one: the
			// message, then the flag list, then a status of 2.
			var fe flagError
			if errors.As(err, &fe) {
				fmt.Fprintf(os.Stderr, "wmse: %v\n\n%s", fe.err, usage)
				os.Exit(2)
			}
			// A request the file cannot answer has already said so, once per
			// flag, next to the metadata of the run that made it. Saying it
			// again in a summary line would only bury the useful part.
			var un unavailable
			if errors.As(err, &un) {
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "wmse: %v\n", err)
			os.Exit(1)
		}
	// -h and -help are the spellings WebMap uses everywhere else. --help is
	// accepted as well, because Go's flag package accepts it and `webmap
	// --help` already works; a reader that refused it would be the one command
	// in the set where muscle memory failed.
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "wmse: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// flagError is a failure that came from the flags rather than from the file, so
// main can answer it with the flag list. `webmap` does the same for a bad flag.
type flagError struct{ err error }

func (e flagError) Error() string { return e.err.Error() }
func (e flagError) Unwrap() error { return e.err }

// unavailable is returned when a flag asked for something the file does not
// hold. The reasons have already been printed, with the metadata that explains
// them, so this only has to make the exit status non-zero: a script that asked
// for -apic on a file without contracts must not read success.
type unavailable struct{ n int }

func (e unavailable) Error() string { return fmt.Sprintf("%d request(s) this file cannot answer", e.n) }

// run answers one read request. The verb chooses what is printed; the flags are
// the same for all three, because they are the scan's.
func run(verb string, args []string) error {
	// -i is taken out before the flag set sees the arguments, and it is not in the
	// flag set at all. The reader's flags are the scan's flags, one for one, and
	// that is a property the tests check; a reader-only flag belongs outside it,
	// where it cannot be mistaken for one the scan has and where its absence from
	// the parity list is visible rather than hidden.
	keyPath, args := takeKeyFlag(args)

	cfg, fs := newFlagSet()
	path, err := parseRead(fs, args)
	if err != nil {
		// Help travels back unwrapped; anything else about the flags is marked
		// so main knows to follow it with the flag list.
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return flagError{err}
	}
	cfg.Apply(fs)
	depth := readDepth(cfg, fs)

	snap, info, err := openSnapshot(path, keyPath, verb == "select")
	if err != nil {
		return err
	}

	o := &options{cfg: cfg, snap: snap, info: info, path: path, depth: depth}
	o.setup()
	// The reasons come first and on stderr, so that they are read before the
	// report rather than discovered at the end of it, and so that a report piped
	// into another tool is not polluted with diagnostics.
	o.explain()

	switch verb {
	case "json":
		return viewJSON(snap, os.Stdout)
	case "info":
		viewInfo(o)
		return finish(o)
	case "select":
		return runSelect(o, os.Stdin, os.Stdout, keyPath)
	default:
		return report(o)
	}
}

// report prints the scan's report over a loaded file and answers with a status.
// It is one function because there is one report: the verb and the session's read
// command both go through it, so a second rendering of the same sections cannot
// drift away from the first.
func report(o *options) error {
	viewReport(o)
	return finish(o)
}

// finish ends a read request: the budget's own notice about itself, and a status
// for whatever the file could not answer.
func finish(o *options) error {
	// The budget reports itself whether or not anything else went wrong: a
	// truncated report that says nothing about being truncated is the one failure
	// a reader cannot detect from its own output.
	if err := o.finish(); err != nil {
		return err
	}
	if len(o.problems) > 0 {
		return unavailable{len(o.problems)}
	}
	return nil
}

// parseRead takes the file path out of a read request, letting flags appear on
// either side of it. Re-parsing is safe: a flag set twice to the same value is
// the same value.
func parseRead(fs *flag.FlagSet, args []string) (string, error) {
	path := ""
	for {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			break
		}
		if path != "" {
			return "", fmt.Errorf("read takes one file, got %d", fs.NArg()+1)
		}
		path = fs.Arg(0)
		args = fs.Args()[1:]
	}
	if path == "" {
		return "", fmt.Errorf("read needs a file")
	}
	return path, nil
}

// out prints one line if the budget allows it. It reports whether the line was
// printed, so a caller that keeps its own count can stay in step.
//
// The -rlimit budget is one counter for the whole request, not one per section:
// "show me twenty lines" has to mean twenty lines wherever they were spent.
func (o *options) out(line string) bool {
	if o.cfg.RequestLimit > 0 {
		if o.left <= 0 {
			o.stopped = true
			return false
		}
		o.left--
	}
	fmt.Println(line)
	return true
}
