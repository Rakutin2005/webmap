package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// edited runs a sequence of key presses through a line editor and returns the
// line it produced with everything the editor drew. Driving the editor with the
// keys it would have received is the only way to test it that means anything:
// a test that calls the editing methods directly would pass even if the keys
// were never wired to them.
func edited(t *testing.T, keys string, setup func(*lineEditor)) (line, drawn string, err error) {
	t.Helper()
	var out bytes.Buffer
	ed := &lineEditor{
		src:    newPipeSource(strings.NewReader(keys)),
		out:    &out,
		prompt: func() string { return "> " },
	}
	if setup != nil {
		setup(ed)
	}
	line, err = ed.read()
	return line, out.String(), err
}

// line is edited for its result, and fails the test if the editor reported an
// error the caller did not ask for.
func line(t *testing.T, keys string) string {
	t.Helper()
	got, _, err := edited(t, keys, nil)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("editing %q: %v", keys, err)
	}
	return got
}

// The editing keys: the ones a person uses to fix a line rather than retype it.
func TestEditorEditsTheLine(t *testing.T) {
	const (
		left  = "\x1b[D"
		home  = "\x1b[H"
		end   = "\x1b[F"
		del   = "\x1b[3~"
		cr    = "\r"
		bs    = "\x7f"
		ctrlU = "\x15"
		ctrlK = "\x0b"
		ctrlW = "\x17"
		ctrlA = "\x01"
		ctrlE = "\x05"
	)
	for _, c := range []struct{ name, keys, want string }{
		{"plain", "ls" + cr, "ls"},
		{"backspace", "lsx" + bs + cr, "ls"},
		{"delete forward", "ab" + left + del + cr, "a"},
		{"insert in the middle", "bc" + left + "X" + cr, "bXc"},
		{"home then end", "bc" + home + "X" + end + "Y" + cr, "XbcY"},
		{"ctrl-a and ctrl-e", "bc" + ctrlA + "X" + ctrlE + "Y" + cr, "XbcY"},
		{"clear to start", "ls -junk" + left + left + left + left + ctrlU + cr, "junk"},
		{"clear to end", "ls -junk" + left + left + left + left + left + left + ctrlK + cr, "ls"},
		{"kill word", "cd /a/b junk" + ctrlW + cr, "cd /a/b "},
		// Escape abandons the line, and the letter typed next must survive it:
		// a key that is not part of a sequence is a keystroke of its own.
		{"escape abandons", "junk" + "\x1b" + "info" + cr, "info"},
		{"a multi-byte rune", "日本" + cr, "日本"},
	} {
		if got := line(t, c.keys); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The line is drawn as it is edited, with the cursor where the person is
// typing: without that, a recalled command or a completion would be invisible
// until it was run.
func TestEditorDrawsTheLineAndTheCursor(t *testing.T) {
	// The cursor is moved back into the line with a carriage return and a
	// right-move, so a line edited in the middle can be seen to be edited in the
	// middle: two lefts into "> abc" put the cursor at column 3 of 5.
	_, drawn, err := edited(t, "abc\x1b[D\x1b[D\r", nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(drawn, "> abc") {
		t.Errorf("the drawn line does not carry the prompt and the text: %q", drawn)
	}
	if !strings.Contains(drawn, "\r\x1b[2C") {
		t.Errorf("the cursor was not moved back into the line: %q", drawn)
	}
	// A cursor at the end of the line must not be moved at all: a carriage
	// return there would drop it at the start of the line.
	if got, _, _ := edited(t, "ab\r", nil); !strings.HasSuffix(got, "ab") {
		t.Errorf("the line is %q", got)
	}
	_, drawn, _ = edited(t, "ab\r", nil)
	if strings.Contains(drawn, "\r\r\n") {
		t.Errorf("the cursor was sent to the start of the line on Enter: %q", drawn)
	}
}

// A shorter line must erase what a longer one left behind, or the tail of the
// old line stays on the screen and the prompt lies about what has been typed.
func TestEditorErasesWhatALongerLineLeft(t *testing.T) {
	var out bytes.Buffer
	ed := &lineEditor{
		src:    newPipeSource(strings.NewReader("abcdef\x15\r")),
		out:    &out,
		prompt: func() string { return "> " },
	}
	if _, err := ed.read(); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out.String(), "\r> abcdef") {
		t.Errorf("the full line was not drawn: %q", out.String())
	}
	// Six spaces are what clears the six characters that were there.
	if !strings.Contains(out.String(), "      ") {
		t.Errorf("the shorter line did not erase the longer one: %q", out.String())
	}
}

// Tab completes: one candidate whole, several as far as they agree, and nothing
// more when there is nothing more to say.
func TestEditorTabCompletes(t *testing.T) {
	comp := func(cands ...string) func([]rune, int, string) []string {
		return func([]rune, int, string) []string { return cands }
	}
	for _, c := range []struct {
		name      string
		keys      string
		complete  func([]rune, int, string) []string
		wantLine  string
		wantDrawn []string
		notDrawn  string
	}{
		{
			name:     "one candidate is inserted whole",
			keys:     "l\t\r",
			complete: comp("ls "),
			wantLine: "ls ",
		},
		{
			name:     "several are completed as far as they agree",
			keys:     "cd /c\t\r",
			complete: comp("/complex/1", "/complex/2"),
			wantLine: "cd /complex/",
		},
		{
			name:      "and when there is nothing more, they are written out",
			keys:      "cd /complex/\t\r",
			complete:  comp("/complex/1", "/complex/2"),
			wantLine:  "cd /complex/",
			wantDrawn: []string{"/complex/1", "/complex/2"},
		},
		{
			name:     "a candidate the word does not lead to is not offered",
			keys:     "cd /c\t\r",
			complete: comp("/other/1"),
			wantLine: "cd /c",
		},
		{
			name:     "no candidate leaves the line alone",
			keys:     "zz\t\r",
			complete: comp("ls ", "cd "),
			wantLine: "zz",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			ed := &lineEditor{
				src:      newPipeSource(strings.NewReader(c.keys)),
				out:      &out,
				prompt:   func() string { return "> " },
				complete: c.complete,
			}
			got, err := ed.read()
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("read: %v", err)
			}
			if got != c.wantLine {
				t.Errorf("line is %q, want %q", got, c.wantLine)
			}
			for _, want := range c.wantDrawn {
				if !strings.Contains(out.String(), want+"\n") {
					t.Errorf("the drawn output does not offer %q: %q", want, out.String())
				}
			}
			if c.notDrawn != "" && strings.Contains(out.String(), c.notDrawn) {
				t.Errorf("the drawn output should not contain %q: %q", c.notDrawn, out.String())
			}
		})
	}
}

// The history is what the up and down keys walk, and walking back down must
// return to the line that was being typed rather than to the top of the list.
// Each case gets its own history, because a shared one would make the walk
// depend on the order the cases happened to run in.
func TestEditorWalksTheHistory(t *testing.T) {
	// edit runs one line's worth of keys against a history holding two entries.
	edit := func(keys string) string {
		t.Helper()
		h := &history{limit: historyLimit}
		h.remember("cd /dir")
		h.remember("ls")
		var out bytes.Buffer
		ed := &lineEditor{
			src: newPipeSource(strings.NewReader(keys)), out: &out,
			hist: h, prompt: func() string { return "> " },
		}
		got, err := ed.read()
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read: %v", err)
		}
		return got
	}
	const up, down = "\x1b[A", "\x1b[B"
	if got := edit("wh" + up + up + "\r"); got != "cd /dir" {
		t.Errorf("two ups recalled %q, want the oldest line", got)
	}
	// The line being typed when the walk began comes back at the end of it.
	if got := edit("half" + up + down + "\r"); got != "half" {
		t.Errorf("walking down returned %q, want the half-typed line", got)
	}
	// There is nothing older than the oldest line, so a third up stays there
	// rather than wrapping round or inventing an entry.
	if got := edit(up + up + up + "\r"); got != "cd /dir" {
		t.Errorf("walking up past the oldest line gave %q, want it to stay", got)
	}
	// A single up reaches the newest line, not the oldest.
	if got := edit(up + "\r"); got != "ls" {
		t.Errorf("one up recalled %q, want the newest line", got)
	}
}

// A line abandoned with Ctrl-C is not the end of the session, and an empty line
// with Ctrl-D is: the shell's two exits, which the session keeps apart.
func TestEditorInterruptAndEndOfInput(t *testing.T) {
	var out bytes.Buffer
	ed := &lineEditor{
		src:    newPipeSource(strings.NewReader("junk\x03info\r")),
		out:    &out,
		prompt: func() string { return "> " },
	}
	if _, err := ed.read(); !errors.Is(err, errInterrupted) {
		t.Fatalf("Ctrl-C on a line gave %v, want the line abandoned", err)
	}
	got, err := ed.read()
	if err != nil {
		t.Fatalf("the session should carry on after an abandoned line: %v", err)
	}
	if got != "info" {
		t.Errorf("after an abandoned line the next line is %q", got)
	}

	ed = &lineEditor{
		src:    newPipeSource(strings.NewReader("\x04")),
		out:    &out,
		prompt: func() string { return "> " },
	}
	if _, err := ed.read(); !errors.Is(err, io.EOF) {
		t.Errorf("Ctrl-D on an empty line gave %v, want the end of input", err)
	}
}

// The escape sequences a terminal sends for the navigation keys, in the several
// spellings they have. A session that understood only one spelling would have
// arrow keys that work on one terminal and do nothing on the next.
func TestDecodeEscape(t *testing.T) {
	for _, c := range []struct {
		keys string
		want keyCode
	}{
		{"\x1b[A", keyUp},
		{"\x1b[B", keyDown},
		{"\x1b[C", keyRight},
		{"\x1b[D", keyLeft},
		{"\x1bOA", keyUp},
		{"\x1bOD", keyLeft},
		{"\x1b[H", keyHome},
		{"\x1b[F", keyEnd},
		{"\x1b[1~", keyHome},
		{"\x1b[7~", keyHome},
		{"\x1b[3~", keyDelete},
		{"\x1b[4~", keyEnd},
		{"\x1b[8~", keyEnd},
		// A bare Escape, an unknown sequence and an unknown key are all left to
		// the caller rather than turned into a keystroke.
		{"\x1b", keyEscape},
		{"\x1b[Z", keyEscape},
		{"\x1b[99~", keyEscape},
		{"\x02", keyEscape},
	} {
		src := newPipeSource(strings.NewReader(c.keys))
		got, err := src.readKey()
		if err != nil {
			t.Errorf("decoding %q: %v", c.keys, err)
			continue
		}
		if got.code != c.want {
			t.Errorf("decoding %q gave %v, want %v", c.keys, got.code, c.want)
		}
	}
}

// The history keeps one copy of a repeated command, drops the blank lines, and
// stays bounded, because a long session would otherwise grow without end.
func TestHistoryRemembersOnceAndStaysBounded(t *testing.T) {
	h := &history{limit: 3}
	h.remember("")
	h.remember("  ")
	h.remember("ls")
	h.remember("ls")
	h.remember("cd /a")
	h.remember("what /b")
	h.remember("info")
	// The limit drops the oldest, so what is left is the three most recent, with
	// the repeat of "ls" never becoming a second entry.
	if got := strings.Join(h.lines, ","); got != "cd /a,what /b,info" {
		t.Errorf("history is %q, want the three most recent with no repeats", got)
	}
	if h.pos != len(h.lines) {
		t.Errorf("the history should sit at its newest line after a remember")
	}
}

// A fresh history walks to nothing rather than to an arbitrary line, so a person
// who presses up on an empty session gets silence and not a surprise.
func TestHistoryOnAnEmptyList(t *testing.T) {
	h := &history{limit: historyLimit}
	if _, ok := h.step(-1, "x"); ok {
		t.Errorf("an empty history should not step")
	}
}
