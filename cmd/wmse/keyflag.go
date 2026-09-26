package main

import (
	"fmt"
	"os"
	"strings"

	"apimap/internal/wmse"
)

// takeKeyFlag takes -i out of a read request's arguments and returns the path it
// names.
//
// It is taken out by hand rather than registered, because the reader's flags are
// the scan's flags one for one and that is checked: a reader-only flag put in the
// flag set would either break that promise or quietly become a flag the scan does
// not have. The spellings accepted are the ones Go's own flag package accepts for a
// string flag, because a person who has used -o anywhere else will type -i=key and
// should not be surprised here.
func takeKeyFlag(args []string) (string, []string) {
	key := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-i" || a == "--i":
			if i+1 < len(args) {
				key = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-i="):
			key = strings.TrimPrefix(a, "-i=")
		case strings.HasPrefix(a, "--i="):
			key = strings.TrimPrefix(a, "--i=")
		default:
			rest = append(rest, a)
		}
	}
	return key, rest
}

// openSnapshot opens a file, decrypting it first if it is sealed.
//
// A sealed file is opened by exactly the same code as one that was never sealed,
// once the bytes are back: there is no second reader and no second format, so
// nothing about a scan changes by having been encrypted - it is only the first few
// hundred bytes of the file that mean something different.
//
// The password is only asked for when there is a session to ask in. A `read` on the
// command line has nobody to ask, and a tool that tried would sit there looking
// broken; it is told what it needs instead.
func openSnapshot(path, keyPath string, canAsk bool) (*wmse.Snapshot, *wmse.FileInfo, error) {
	sealed, alg := wmse.SealedState(path)
	if !sealed {
		f, err := wmse.Open(path)
		if err != nil {
			return nil, nil, err
		}
		snap, err := f.Load()
		if err != nil {
			return nil, nil, err
		}
		return snap, f.Info(), nil
	}
	if alg == wmse.SealPassword && !canAsk {
		return nil, nil, fmt.Errorf("this file is encrypted with a password, and there is nobody here to ask: wmse select %s asks for it", path)
	}
	if alg != wmse.SealPassword && keyPath == "" {
		return nil, nil, fmt.Errorf("this file was encrypted with %s, so it needs a key: wmse ... %s -i <key>", alg, path)
	}
	snap, info, err := openSealed(path, keyPath, os.Stdin, os.Stdout)
	if err != nil {
		return nil, nil, err
	}
	return snap, info, nil
}
