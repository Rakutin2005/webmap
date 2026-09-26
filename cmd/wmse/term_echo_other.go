//go:build !unix

package main

import (
	"errors"
	"os"
)

// errNoEchoControl says the terminal cannot be quiet, which the caller turns into a
// word to the person rather than a refusal.
var errNoEchoControl = errors.New("this terminal cannot hide what is typed")

// hideEcho is not something this build can do. A password is still asked for, and
// the session says that it will be visible: telling the person is better than a
// failure, and a failure would leave them typing a password into a terminal that is
// showing it.
func hideEcho(*os.File) (func(), error) { return nil, errNoEchoControl }
