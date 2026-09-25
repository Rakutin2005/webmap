package main

import (
	"flag"
	"io"

	"apimap/internal/config"
)

// newFlagSet puts the scan's flags on a flag set of this command's own.
//
// The definitions come from config.Register, the same function the scan uses, so
// "the reader takes the same arguments" is a property of the code rather than a
// promise in a help text: a flag cannot be spelled differently here, cannot
// change its default here, and cannot lose its description here without the scan
// changing too. The flag package's own output is dropped because it prints from
// two different paths that want different streams; main prints the usage block.
func newFlagSet() (*config.Config, *flag.FlagSet) {
	fs := flag.NewFlagSet("wmse", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return config.Register(fs), fs
}

// readDepth returns the depth the tree should stop at, which is -rdepth when the
// reader was given it and the whole tree when it was not.
//
// The flag's own default of 5 belongs to the scan and stays there, because there
// it is a limit on requests. Here a saved crawl cannot be deeper than the depth
// it was made with, so "everything" and "5" are the same tree, and a file that
// was crawled deeper would be silently cut short. -rdepth 3 still means three
// levels: that is a request to see less than the file holds, not a claim about
// what it holds.
func readDepth(cfg *config.Config, fs *flag.FlagSet) int {
	if wasSet(fs, "rdepth") {
		return cfg.RecursiveDepth
	}
	return 0
}

// wasSet reports whether a flag was given on the command line, as opposed to
// being left at its default.
func wasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
