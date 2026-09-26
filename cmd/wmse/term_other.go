//go:build !unix

package main

import (
	"errors"
	"os"
)

// A terminal is not something this build can take over. The session still runs -
// it reads whole lines and prints answers - so the whole of the difference on a
// platform without termios is the loss of line editing, completion and history.

var errNotATerminal = errors.New("not a terminal")

type terminal struct{}

func openTerminal(*os.File) (*terminal, error) { return nil, errNotATerminal }

func (*terminal) restore() {}

// newTTYSource has the same answer as openTerminal here, and the session takes
// the same fallback.
func newTTYSource(*os.File) (keySource, *terminal, error) {
	return nil, nil, errNotATerminal
}
