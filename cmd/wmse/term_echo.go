//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// hideEcho turns off a terminal's echo for as long as the session is reading
// something that must not appear on the screen, and returns the function that puts
// it back.
//
// It is a separate helper rather than part of the line editor because the two have
// opposite lifetimes: the editor takes the terminal for the whole session, while a
// password needs it for one line. A password that is typed visibly is a password in
// a screenshot, a log, and the shoulder of whoever is standing behind you.
func hideEcho(f *os.File) (func(), error) {
	fd := int(f.Fd())
	saved, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	quiet := *saved
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ECHONL
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &quiet); err != nil {
		return nil, err
	}
	return func() {
		_ = unix.IoctlSetTermios(fd, ioctlWriteTermios, saved)
	}, nil
}
