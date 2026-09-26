//go:build unix

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// errNotATerminal is what openTerminal says when there is no terminal to take
// over - a pipe, a file, a test. It is not a failure of the session, only of
// line editing, so the caller falls back to reading whole lines and loses
// nothing but the editing.
var errNotATerminal = errors.New("not a terminal")

// A terminal is a file's line discipline, borrowed for the length of a session.
// The saved state is kept so it can be handed back: a session that left a
// person's shell without echo would be a far worse bug than not having line
// editing at all.
type terminal struct {
	fd     int
	saved  *unix.Termios
	raw    *unix.Termios
	closed bool
}

// rawTermios is the line discipline a session reads its keys in: every key
// arrives as itself, unbuffered and unechoed, so that the left, right, up and Tab
// keys can mean in a session what they mean in a shell. Without this the kernel
// eats them before the program ever sees them.
//
// The input side is what is changed. The output side is deliberately left alone,
// and ONLCR with it: that is the translation which turns the newline a program
// prints into a carriage return and a line feed. Clearing OPOST - as a literal
// reading of "raw" does - removes it, and then every line the session prints
// steps down one row while keeping its column, so a table of links comes out as a
// staircase. The editor's own cursor moves are written as an explicit carriage
// return and a right-move, which need no translation and are unaffected.
func rawTermios(saved *unix.Termios) *unix.Termios {
	raw := *saved
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	return &raw
}

// openTerminal puts a file into that line discipline, keeping the state it was
// in so it can be handed back: a session that left a person's shell without echo
// would be a far worse bug than not having line editing at all.
func openTerminal(f *os.File) (*terminal, error) {
	fd := int(f.Fd())
	saved, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, errNotATerminal
	}
	raw := rawTermios(saved)
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, raw); err != nil {
		return nil, errNotATerminal
	}
	return &terminal{fd: fd, saved: saved, raw: raw}, nil
}

// setReadTimeout switches the terminal between blocking and timed reads. A timed
// read is how the tail of an escape sequence gets collected: an arrow key sends
// three bytes and the Escape key sends one, and the only difference a program
// can see is whether a second byte arrives.
//
// The timeout is applied to the raw settings, not the saved ones, because
// restoring the original line discipline for a moment would switch echo back on
// and print a stray escape sequence to the screen.
func (t *terminal) setReadTimeout(tenths uint8) {
	if t == nil || t.closed {
		return
	}
	if tenths == 0 {
		// VMIN=1 with no timer is the only combination that waits: VMIN=0 with
		// VTIME=0 is a poll that returns nothing straight away, which would spin
		// instead of reading.
		t.raw.Cc[unix.VMIN] = 1
		t.raw.Cc[unix.VTIME] = 0
	} else {
		t.raw.Cc[unix.VMIN] = 0
		t.raw.Cc[unix.VTIME] = tenths
	}
	_ = unix.IoctlSetTermios(t.fd, ioctlWriteTermios, t.raw)
}

// restore hands the terminal its saved line discipline. It is safe to call more
// than once, so a deferred restore and an explicit one on the way out do not
// fight over the same terminal.
func (t *terminal) restore() {
	if t == nil || t.closed {
		return
	}
	t.closed = true
	_ = unix.IoctlSetTermios(t.fd, ioctlWriteTermios, t.saved)
}

// newTTYSource puts stdin into raw mode and wraps it as a key source. The two
// failures are kept apart for the caller: a terminal it could not take over is
// fine, because a session without line editing still answers every question.
func newTTYSource(f *os.File) (keySource, *terminal, error) {
	tty, err := openTerminal(f)
	if err != nil {
		return nil, nil, err
	}
	return &ttySource{f: f, tty: tty}, tty, nil
}

// ttySource reads keys from a terminal in raw mode. It differs from a buffered
// source in one place only: whether another byte is on its way is answered with
// a short timed read rather than by looking at a buffer, because a terminal has
// no buffer to look at.
type ttySource struct {
	f   *os.File
	tty *terminal
	// held is a byte read ahead of the parser. The parser asks hasMore before
	// committing to an escape sequence, and the answer is this byte.
	held []byte
}

// readKey returns the next key press.
func (t *ttySource) readKey() (key, error) {
	c, err := t.readByte()
	if err != nil {
		return key{}, err
	}
	return decodeByte(t, c)
}

// unreadByte puts a byte back at the front of what is held, so a byte read
// ahead while working out whether an escape sequence had started is not lost.
func (t *ttySource) unreadByte(c byte) {
	t.held = append([]byte{c}, t.held...)
}

// hasMore reports whether a byte is already waiting. It waits briefly rather
// than not at all, because the byte that distinguishes an arrow key from a bare
// Escape is one that has not arrived yet.
func (t *ttySource) hasMore() bool {
	if len(t.held) > 0 {
		return true
	}
	t.tty.setReadTimeout(2)
	defer t.tty.setReadTimeout(0)
	var b [1]byte
	n, err := t.f.Read(b[:])
	if n == 1 {
		t.held = append(t.held, b[0])
		return true
	}
	return err == nil && n == 0
}

// readByte returns the next input byte, preferring one already read ahead.
//
// The read blocks for a real key. A timed read that expired means the person
// paused mid-word, not that the input ended, so it is never reported as an end
// of input - that would silently close a session on a slow typist.
func (t *ttySource) readByte() (byte, error) {
	if len(t.held) > 0 {
		c := t.held[0]
		t.held = t.held[1:]
		return c, nil
	}
	t.tty.setReadTimeout(0)
	for {
		var b [1]byte
		n, err := t.f.Read(b[:])
		if n == 1 {
			return b[0], nil
		}
		if err != nil {
			return 0, err
		}
		// A zero-length read with no error cannot happen with VMIN=1 unless a
		// signal got in the way; read again rather than invent an end of input.
	}
}
