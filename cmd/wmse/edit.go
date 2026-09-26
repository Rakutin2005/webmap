package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A keySource turns input into key presses. It is the whole of the difference
// between a typed session and a piped one: both read the same keys, one from a
// terminal in raw mode and one from a buffer, so everything above this line is
// shared, and a session driven from a test exercises the same editing code a
// person does.
type keySource interface {
	// readKey returns the next key press, or io.EOF when the input is done.
	readKey() (key, error)
	// readByte returns the next input byte. The parser needs it to collect an
	// escape sequence and a multi-byte rune, which are longer than one key.
	readByte() (byte, error)
	// unreadByte puts a byte back. An escape sequence is recognised by its
	// second byte, so a byte that turns out not to belong to one is a keystroke
	// of its own and must not be swallowed.
	unreadByte(c byte)
	// hasMore reports whether another byte is already available, without
	// blocking. It is how the tail of an escape sequence is told from a bare
	// Escape key: the first byte of both is the same, and only a second byte
	// says which was meant.
	hasMore() bool
}

type keyCode uint8

// The keys a session acts on. Everything else is either a rune to insert or
// ignored, because a key with no meaning here should leave the line alone
// rather than do something surprising.
const (
	keyRune keyCode = iota
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyUp
	keyDown
	keyTab
	keyInterrupt
	keyEndOfInput
	keyCtrlA
	keyCtrlE
	keyCtrlU
	keyCtrlK
	keyCtrlW
	keyCtrlL
	keyEscape
)

type key struct {
	code keyCode
	r    rune
}

// errInterrupted is returned for a line abandoned with Ctrl-C. It is not an
// error the session reports: the shell drops the line and draws a new prompt,
// and so does this.
var errInterrupted = errors.New("line abandoned")

// decodeByte turns one input byte into a key, reading the rest of an escape
// sequence or of a multi-byte rune behind it. The source is asked whether more
// is coming rather than waiting, so a single byte is always enough to be decoded
// on its own.
func decodeByte(s keySource, c byte) (key, error) {
	switch c {
	case '\r', '\n':
		return key{code: keyEnter}, nil
	case 0x7f, 0x08:
		return key{code: keyBackspace}, nil
	case '\t':
		return key{code: keyTab}, nil
	case 0x1b:
		return decodeEscape(s), nil
	case 0x03:
		return key{code: keyInterrupt}, nil
	case 0x04:
		return key{code: keyEndOfInput}, nil
	case 0x01:
		return key{code: keyCtrlA}, nil
	case 0x05:
		return key{code: keyCtrlE}, nil
	case 0x0b:
		return key{code: keyCtrlK}, nil
	case 0x0c:
		return key{code: keyCtrlL}, nil
	case 0x15:
		return key{code: keyCtrlU}, nil
	case 0x17:
		return key{code: keyCtrlW}, nil
	case ' ':
		return key{code: keyRune, r: ' '}, nil
	}
	if c < 0x20 {
		// Some other control key. It means nothing here, and swallowing it
		// silently is the same thing a shell does.
		return key{code: keyEscape}, nil
	}
	if c < utf8.RuneSelf {
		return key{code: keyRune, r: rune(c)}, nil
	}
	return key{code: keyRune, r: decodeRune(s, c)}, nil
}

// decodeRune assembles a multi-byte rune from the bytes that follow its first.
func decodeRune(s keySource, first byte) rune {
	buf := []byte{first}
	for len(buf) < utf8.UTFMax && s.hasMore() {
		next, err := s.readByte()
		if err != nil {
			break
		}
		buf = append(buf, next)
		if utf8.FullRune(buf) {
			break
		}
	}
	r, _ := utf8.DecodeRune(buf)
	return r
}

// decodeEscape reads the rest of an escape sequence. Terminals spell the arrow
// and navigation keys as ESC [ A, ESC O H, ESC [ 3 ~ and half a dozen other
// ways, and a session that understands only the first one would have arrow keys
// that work on one terminal and do nothing on the next.
//
// A byte that does not continue a sequence is handed back rather than dropped.
// Without that, pressing Escape and then typing would lose the first letter,
// which is the sort of fault a person blames on the program and never on the
// terminal.
func decodeEscape(s keySource) key {
	if !s.hasMore() {
		return key{code: keyEscape}
	}
	c, err := s.readByte()
	if err != nil {
		return key{code: keyEscape}
	}
	if c != '[' && c != 'O' {
		s.unreadByte(c)
		return key{code: keyEscape}
	}
	if !s.hasMore() {
		return key{code: keyEscape}
	}
	f, err := s.readByte()
	if err != nil {
		return key{code: keyEscape}
	}
	switch f {
	case 'A':
		return key{code: keyUp}
	case 'B':
		return key{code: keyDown}
	case 'C':
		return key{code: keyRight}
	case 'D':
		return key{code: keyLeft}
	case 'H':
		return key{code: keyHome}
	case 'F':
		return key{code: keyEnd}
	}
	if f < '0' || f > '9' {
		s.unreadByte(f)
		return key{code: keyEscape}
	}
	// A numeric parameter, ending in '~': 1 and 7 are Home, 3 is Delete, 4 and 8
	// are End. The parameter can be more than one digit, so it is read to the
	// tilde rather than assumed to be single.
	num := int(f - '0')
	for s.hasMore() {
		n, err := s.readByte()
		if err != nil || n == '~' {
			break
		}
		if n < '0' || n > '9' {
			s.unreadByte(n)
			return key{code: keyEscape}
		}
		num = num*10 + int(n-'0')
	}
	switch num {
	case 1, 7:
		return key{code: keyHome}
	case 3:
		return key{code: keyDelete}
	case 4, 8:
		return key{code: keyEnd}
	}
	return key{code: keyEscape}
}

// pipeSource reads keys from a buffer. It is what a piped or scripted session
// uses, and it is what lets a test drive the editing code by writing the keys
// it would have pressed.
type pipeSource struct {
	r *bufio.Reader
}

func newPipeSource(r io.Reader) *pipeSource {
	return &pipeSource{r: bufio.NewReaderSize(r, 64<<10)}
}

func (p *pipeSource) readByte() (byte, error) { return p.r.ReadByte() }

func (p *pipeSource) unreadByte(c byte) { _ = p.r.UnreadByte() }

func (p *pipeSource) hasMore() bool { return p.r.Buffered() > 0 }

func (p *pipeSource) readKey() (key, error) {
	c, err := p.r.ReadByte()
	if err != nil {
		return key{}, err
	}
	return decodeByte(p, c)
}

// history is what the arrow keys walk. It is kept for the length of the session
// and written nowhere: a session that promised to touch nothing but the file it
// was opened on should not quietly leave a trail in the home directory, and
// anything that needs history across runs can ask a shell for it.
type history struct {
	lines []string
	// draft is the half-typed line set aside when the walk upwards began, so
	// walking back down lands on what was being typed rather than on the oldest
	// command in the list.
	draft string
	// pos is the entry being shown; len(lines) is the end of the list, which is
	// where the draft lives.
	pos   int
	limit int
}

// historyLimit bounds the list, so a long session cannot grow without end. The
// oldest lines go first, because the recent ones are the ones being recalled.
const historyLimit = 1000

// remember adds a line to the history. A blank line is not worth keeping, and a
// line identical to the one before it is worth keeping only once, so pressing
// Enter twice does not fill the list with the same command.
func (h *history) remember(line string) {
	if h.limit <= 0 {
		h.limit = historyLimit
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if n := len(h.lines); n > 0 && h.lines[n-1] == line {
		return
	}
	h.lines = append(h.lines, line)
	if len(h.lines) > h.limit {
		h.lines = append([]string(nil), h.lines[len(h.lines)-h.limit:]...)
	}
	h.pos = len(h.lines)
	h.draft = ""
}

// step walks the history. A negative direction is an older line and a positive
// one a newer line; walking past the newest returns to the line that was being
// typed when the walk began.
func (h *history) step(dir int, current string) (string, bool) {
	if len(h.lines) == 0 {
		return "", false
	}
	if dir < 0 {
		if h.pos == 0 {
			return "", false
		}
		if h.pos == len(h.lines) {
			h.draft = current
		}
		h.pos--
		return h.lines[h.pos], true
	}
	if h.pos >= len(h.lines) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.lines) {
		return h.draft, true
	}
	return h.lines[h.pos], true
}

// A lineEditor reads one line of input, drawing it as it is edited so that a
// completion, a recalled command or a corrected typo is visible before it is
// run. It owns the keys the line is built from and nothing else: what a word
// completes to, and what the finished line means, belong to the session.
type lineEditor struct {
	src keySource
	out io.Writer
	// hist is the list the up and down keys walk. It may be nil, and then the
	// arrow keys do nothing rather than failing.
	hist *history
	// complete returns the words the word being typed could become. It is given
	// the whole line, where the word starts and the word itself, so a command
	// can complete its own arguments and a session can complete a path.
	complete func(line []rune, start int, word string) []string
	// prompt returns the prompt to draw, so it can follow the session as it
	// moves.
	prompt func() string
	buf    []rune
	cur    int
	// drawn is how many columns the last draw covered, which is what lets a
	// shorter line erase the longer one it replaced.
	drawn int
}

// read draws the prompt and returns the finished line. It returns errInterrupted
// when the line is abandoned and io.EOF at the end of input.
func (e *lineEditor) read() (string, error) {
	e.buf = e.buf[:0]
	e.cur = 0
	e.drawn = 0
	for {
		e.draw()
		k, err := e.src.readKey()
		if err != nil {
			return "", err
		}
		done, err := e.act(k)
		if err != nil || done {
			return string(e.buf), err
		}
	}
}

// act applies one key and reports whether the line is finished.
func (e *lineEditor) act(k key) (bool, error) {
	switch k.code {
	case keyEnter:
		// The finished line is left on the screen and the cursor steps past it,
		// which is what a shell does and what makes the scrollback of a session
		// readable.
		e.cursorAt(e.drawn)
		fmt.Fprintln(e.out)
		return true, nil
	case keyInterrupt:
		e.cursorAt(e.drawn)
		fmt.Fprintln(e.out)
		return false, errInterrupted
	case keyEndOfInput:
		if len(e.buf) == 0 {
			return false, io.EOF
		}
		// Ctrl-D on a non-empty line is the forward delete a shell makes it,
		// and only an empty line is the end of input.
		e.deleteForward()
	case keyEscape:
		// Escape means "I changed my mind", which is what it means in a shell.
		e.buf = e.buf[:0]
		e.cur = 0
	case keyBackspace:
		if e.cur > 0 {
			e.remove(e.cur-1, e.cur)
		}
	case keyDelete:
		e.deleteForward()
	case keyLeft:
		if e.cur > 0 {
			e.cur--
		}
	case keyRight:
		if e.cur < len(e.buf) {
			e.cur++
		}
	case keyHome, keyCtrlA:
		e.cur = 0
	case keyEnd, keyCtrlE:
		e.cur = len(e.buf)
	case keyCtrlU:
		// Clear from the start of the line to the cursor, which is the one edit
		// worth a key: it discards a whole mistyped command in one press.
		e.remove(0, e.cur)
	case keyCtrlK:
		e.remove(e.cur, len(e.buf))
	case keyCtrlW:
		e.killWord()
	case keyCtrlL:
		fmt.Fprint(e.out, "\033[H\033[2J")
		e.drawn = 0
	case keyUp:
		e.recall(-1)
	case keyDown:
		e.recall(1)
	case keyTab:
		e.completeWord()
	case keyRune:
		e.insert(k.r)
	}
	return false, nil
}

// draw rewrites the prompt and the line, then puts the cursor back where the
// person is typing. Redrawing from the start each time is wasteful on a very
// long line and is invisible on any line a person reads; the alternative is
// tracking what moved, which is where editors get their bugs.
func (e *lineEditor) draw() {
	p := []rune(e.prompt())
	line := make([]rune, 0, len(p)+len(e.buf))
	line = append(line, p...)
	line = append(line, e.buf...)
	// Erase whatever the last draw left behind, or a shorter line would keep
	// the tail of a longer one on the screen.
	pad := e.drawn - len(line)
	if pad < 0 {
		pad = 0
	}
	fmt.Fprint(e.out, "\r"+string(line)+strings.Repeat(" ", pad))
	e.drawn = len(line)
	e.cursorAt(len(p) + e.cur)
}

// cursorAt moves the cursor to a column of the drawn line.
func (e *lineEditor) cursorAt(col int) {
	back := e.drawn - col
	if back <= 0 {
		// Already where it belongs. A carriage return here would put the cursor
		// at the start of the line, which is the one place it must not be while
		// the person is typing at the end of it.
		return
	}
	fmt.Fprintf(e.out, "\r\033[%dC", back)
}

// insert puts a rune in at the cursor.
func (e *lineEditor) insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cur+1:], e.buf[e.cur:])
	e.buf[e.cur] = r
	e.cur++
}

// remove deletes a half-open range, keeping the cursor where it is so that
// deleting backwards does not also move the caret.
func (e *lineEditor) remove(from, to int) {
	e.buf = append(e.buf[:from], e.buf[to:]...)
}

func (e *lineEditor) deleteForward() {
	if e.cur < len(e.buf) {
		e.remove(e.cur, e.cur+1)
	}
}

// killWord removes the word before the cursor, which is the one edit a person
// makes with a finger rather than an arrow key.
func (e *lineEditor) killWord() {
	i := e.cur
	for i > 0 && unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	e.remove(i, e.cur)
}

// recall walks the history.
func (e *lineEditor) recall(dir int) {
	if e.hist == nil {
		return
	}
	line, ok := e.hist.step(dir, string(e.buf))
	if !ok {
		// The end of the list: the bell is the only thing a terminal has to say
		// "there is nothing that way".
		fmt.Fprint(e.out, "\a")
		return
	}
	e.buf = []rune(line)
	e.cur = len(e.buf)
}

// completeWord completes the word the cursor is in. One candidate is inserted
// whole; several insert the prefix they share, and when there is nothing more to
// insert they are written out, which is the only way a terminal line can show
// more than one possibility.
func (e *lineEditor) completeWord() {
	if e.complete == nil {
		return
	}
	start := e.wordStart()
	word := string(e.buf[start:e.cur])
	cands := prefixOnly(e.complete(e.buf, start, word), word)
	switch {
	case len(cands) == 0:
		fmt.Fprint(e.out, "\a")
		return
	case len(cands) == 1:
		e.replace(start, e.cur, []rune(cands[0]))
	default:
		if shared := sharedPrefix(cands); len(shared) > len(word) {
			e.replace(start, e.cur, []rune(shared))
			return
		}
		e.list(cands)
	}
}

// wordStart is where the word under the cursor begins. A quote ends a word as
// well as a space, so a quoted glob is completed as one piece.
func (e *lineEditor) wordStart() int {
	i := e.cur
	for i > 0 {
		r := e.buf[i-1]
		if unicode.IsSpace(r) || r == '\'' || r == '"' {
			break
		}
		i--
	}
	return i
}

// replace swaps a range of the line for other text, leaving the cursor after it.
func (e *lineEditor) replace(from, to int, with []rune) {
	tail := append([]rune(nil), e.buf[to:]...)
	e.buf = append(append(e.buf[:from], with...), tail...)
	e.cur = from + len(with)
}

// list writes the candidates out one per line. The next draw starts from a
// clean column, so the prompt is redrawn under them.
func (e *lineEditor) list(cands []string) {
	for _, c := range cands {
		fmt.Fprintln(e.out, c)
	}
	e.drawn = 0
}

// prefixOnly keeps the candidates that start with what has been typed. The
// session supplies its candidates, but a completion that offered something the
// line cannot accept would insert nonsense, so the check is made here where the
// rule is one rule for every command.
func prefixOnly(cands []string, word string) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if strings.HasPrefix(c, word) {
			out = append(out, c)
		}
	}
	return out
}

// sharedPrefix is the longest prefix every candidate has, which is as much as can
// be inserted without choosing between them.
func sharedPrefix(cands []string) string {
	if len(cands) == 0 {
		return ""
	}
	p := cands[0]
	for _, c := range cands[1:] {
		p = commonPrefix(p, c)
		if p == "" {
			return ""
		}
	}
	return p
}

func commonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}
