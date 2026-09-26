package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"apimap/internal/wmse"
)

// ---------- continue ----------

// cmdContinue scans further, with the scan's own arguments and whatever this line
// changes.
//
// The arguments are inherited from the command line the file records, the same
// ones `read` starts from, so a session does not have to remember how the scan
// was run in order to keep going with it. The flags typed here are applied on top,
// and in practice the one that matters is -rdepth: raising it and re-running
// reaches what the last run stopped short of.
//
// What "continues" means here is worth being exact about, because it is not a
// resume. The scan has no frontier to resume from: it does not record which URLs
// it has already fetched, and a run that re-walked the depth it had already
// covered would be indistinguishable from one that had not. So a deeper run
// re-fetches what it already saw and goes further, and the result is a superset.
// That costs the requests for the part already covered, and it is the price of
// not storing a frontier in the file - which would be a different format and a
// file that can only be continued by the tool that wrote it.
//
// The new findings are merged into the session and the scan file the run wrote is
// removed: the point is to keep exploring what is open, not to leave a second file
// on disk. The file the session was opened on is not touched.
func (s *session) cmdContinue(args []string) {
	recorded := s.merged.Meta[wmse.MetaCommand]
	if strings.TrimSpace(recorded) == "" {
		fmt.Fprintf(s.out, "  %s\n", s.warn("this file records no command, so there are no arguments to inherit"))
		fmt.Fprintf(s.out, "  %s\n", s.dim("run the scan yourself, or scan a file that recorded one"))
		return
	}
	bin, err := webmapBinary()
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		fmt.Fprintf(s.out, "  %s\n", s.dim("`continue` runs the scan, so it needs the webmap binary"))
		return
	}

	// The scan is given a file of its own, which is then merged in and removed.
	// Writing into the file the session was opened on would mean a scan that
	// failed half way left it changed, and a scan that succeeded left no record of
	// having been a separate run at all.
	dir, err := os.MkdirTemp("", "wmse-continue-*")
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "continued.wmse")

	argv, err := continueArgs(recorded, s.pos, out, args)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	fmt.Fprintf(s.out, "  %s\n", s.dim(strings.Join(quoteArgs(argv), " ")))
	fmt.Fprintf(s.out, "  %s\n", s.dim("scanning; the session waits"))

	started := time.Now()
	cmd := exec.Command(bin, argv[1:]...)
	runErr := cmd.Run()
	took := time.Since(started).Round(time.Second)
	if runErr != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("the scan failed: "+runErr.Error()))
		fmt.Fprintf(s.out, "  %s\n", s.dim("after "+took.String()))
		return
	}

	f, err := wmse.Open(out)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("the scan wrote a file that cannot be read: "+err.Error()))
		return
	}
	snap, err := f.Load()
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	before := len(s.merged.Links)
	depth := s.depthOf(s.merged)
	if d := s.depthOf(snap); d > depth {
		depth = d
	}
	s.merged = mergeSnapshots(s.merged, snap)
	s.nodes, s.pages, s.pagePaths, s.paths, s.refs = nil, nil, nil, nil, nil
	s.files = append(s.files, loadedFile{path: out, info: f.Info(), snap: snap})
	fmt.Fprintf(s.out, "  %s\n", s.bold(fmt.Sprintf("+ %d links, +%d pages, +%d contracts",
		len(s.merged.Links)-before, len(snap.Pages), len(snap.Endpoints))))
	fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("%d links across %d files, deepest %d, in %s",
		len(s.merged.Links), len(s.files), depth, took)))
}

// depthOf is how deep a file's crawl reached, which is the one number that says
// whether a continuation went further than what was already there.
func (s *session) depthOf(snap *wmse.Snapshot) int {
	deepest := 0
	for _, p := range snap.Pages {
		if p.Depth > deepest {
			deepest = p.Depth
		}
	}
	return deepest
}

// continueArgs builds the command line for a continuation: the scan's own
// arguments, with a file to write to, the scope the session is standing in, and
// the flags typed here applied on top.
//
// The recorded command is text for a person, so it is taken apart the way the rest
// of the session takes it apart, and the three words that must not be inherited
// are the ones that would send the result somewhere else: the output file and the
// scope. Everything else is the scan's own, which is the point.
func continueArgs(recorded, pos, out string, typed []string) ([]string, error) {
	flags := readersFlagsFrom(recorded)
	kept := make([]string, 0, len(flags)+len(typed)+6)
	skipValue := false
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		if skipValue {
			skipValue = false
			continue
		}
		switch strings.TrimLeft(f, "-") {
		case "o", "o-raw":
			// The output file is this run's own, or the run would overwrite the
			// file the session was opened on.
			if takesValueName("o") || takesValueName("o-raw") {
				skipValue = true
			}
			continue
		case "url":
			// The scope is where the session is standing, which is the whole
			// reason to continue from a position rather than from the root.
			kept = append(kept, "-url", pos)
			skipValue = true
			continue
		}
		kept = append(kept, f)
	}
	kept = append(kept, "-o", out)
	// The typed flags come last so they win over the inherited ones, which is what
	// a flag written after another is everywhere else.
	kept = append(kept, typed...)
	return append([]string{"webmap"}, kept...), nil
}

// takesValueName reports whether a flag of that name needs a value, read from the
// reader's own registration so the answer cannot drift from the flag set.
func takesValueName(name string) bool {
	_, fs := newFlagSet()
	return takesValue(fs.Lookup(name))
}

// webmapBinary finds the scan binary. It sits beside this one - the two are built
// from the same tree into the same directory - and the path is a fallback for the
// case where they do not.
func webmapBinary() (string, error) {
	self, err := os.Executable()
	if err == nil {
		sibling := filepath.Join(filepath.Dir(self), "webmap")
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	if path, err := exec.LookPath("webmap"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("cannot find the webmap binary")
}
