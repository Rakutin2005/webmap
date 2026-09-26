//go:build unix && !linux

package main

import "golang.org/x/sys/unix"

// Everything that is not Linux uses the BSD ioctl numbers, which are the older
// and more widely copied pair, and which every other unix here agrees on.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
