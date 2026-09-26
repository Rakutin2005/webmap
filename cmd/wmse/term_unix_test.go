//go:build unix

package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestRawTermiosKeepsTheOutputSideIntact is the assertion for a fault that is
// invisible in a test run and obvious on a screen: the line discipline a session
// reads its keys in must change the input side and leave the output side alone.
//
// Turning off OPOST is what a literal reading of "raw mode" does, and it takes
// ONLCR with it - the translation that turns the newline a program prints into a
// carriage return and a line feed. With it gone, every line the session prints
// steps down one row while keeping its column, so a table of links comes out as a
// staircase down the screen. Piped, the bytes look fine, which is why this has to
// be pinned by a test rather than found by looking.
func TestRawTermiosKeepsTheOutputSideIntact(t *testing.T) {
	// What a shell leaves behind, which is what a session finds.
	saved := &unix.Termios{
		Iflag: unix.ICRNL | unix.IXON | unix.BRKINT | unix.IGNBRK,
		Oflag: unix.OPOST | unix.ONLCR,
		Lflag: unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN | unix.ECHOE | unix.ECHOK,
		Cflag: unix.CSIZE | unix.PARENB | unix.CREAD | unix.CLOCAL,
	}
	raw := rawTermios(saved)

	if raw.Oflag&unix.OPOST == 0 {
		t.Errorf("OPOST is off, so ONLCR no longer runs and a printed newline will not return the carriage")
	}
	if raw.Oflag&unix.ONLCR == 0 {
		t.Errorf("ONLCR is off, so a printed newline will not return the carriage")
	}
	// The input side is the whole point: keys must arrive as themselves.
	for _, f := range []struct {
		name string
		got  uint32
		want bool
	}{
		{"ECHO", raw.Lflag & unix.ECHO, false},
		{"ICANON", raw.Lflag & unix.ICANON, false},
		{"ISIG", raw.Lflag & unix.ISIG, false},
		{"IEXTEN", raw.Lflag & unix.IEXTEN, false},
		{"ICRNL", raw.Iflag & unix.ICRNL, false},
		{"IXON", raw.Iflag & unix.IXON, false},
		{"BRKINT", raw.Iflag & unix.BRKINT, false},
	} {
		on := f.got != 0
		if on != f.want {
			t.Errorf("%s is %v, want %v", f.name, on, f.want)
		}
	}
	// An eight-bit-clean line, which is what a UTF-8 path needs to arrive intact.
	if raw.Cflag&unix.CSIZE != unix.CS8 {
		t.Errorf("the character size is not CS8, so a multi-byte rune would be mangled")
	}
	if raw.Cc[unix.VMIN] != 1 || raw.Cc[unix.VTIME] != 0 {
		t.Errorf("a read should wait for a key rather than time out: VMIN=%d VTIME=%d", raw.Cc[unix.VMIN], raw.Cc[unix.VTIME])
	}
	// The saved state must not be touched: it is what the terminal is handed back,
	// and a restore that restored the raw settings would leave a person's shell
	// without echo.
	if saved.Lflag&unix.ECHO == 0 || saved.Lflag&unix.ICANON == 0 {
		t.Errorf("rawTermios modified the state it was given")
	}
	if saved.Cc[unix.VMIN] != 0 {
		t.Errorf("rawTermios wrote its read settings into the saved state")
	}
}
