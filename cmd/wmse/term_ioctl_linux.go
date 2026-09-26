//go:build linux

package main

import "golang.org/x/sys/unix"

// Linux names its terminal ioctls after the line discipline they act on rather
// than after the generic "get attributes" the BSDs inherited.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
