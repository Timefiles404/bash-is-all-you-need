package tui

import (
	"strings"
	"unicode"

	"bash-is-all-you-need/tui/term"
)

const (
	maxHistory = 500

	// maxInputRows caps how tall the composer may grow. Past this the buffer
	// scrolls inside its own pane, because an input box that keeps growing
	// eventually eats the scrollback that tells you what you are replying to.
	maxInputRows = 8

	// tabWidth is what a tab becomes on the way in.
	//
	// A literal tab cannot be allowed into the buffer. Every column in this UI
	// is computed from character widths, and a tab's width is not a property of
	// the character — it is a property of the terminal's tab stops, which this
	// process cannot see. One pasted tab and the caret sits somewhere other
	// than where the frame drew it.
	tabWidth = 4
)

// editor is the input line: a rune buffer, a caret, and a history.
//
// Runes rather than bytes throughout. A caret measured in bytes lands inside a
// multi-byte character on the first non-ASCII input, and the repo's own test
// corpus is full of CJK.
type editor struct {
	buf []rune
	cur int

	hist  []string
	hpos  int    // == len(hist) while editing the live buffer
	stash string // the live buffer, held while browsing history

	kill string // the last killed text, for Ctrl-Y
}

func newEditor() *editor { return &editor{} }

func (e *editor) text() string { return string(e.buf) }

func (e *editor) empty() bool { return len(e.buf) == 0 }

func (e *editor) setText(s string) {
	e.buf = []rune(s)
	e.cur = len(e.buf)
}

func (e *editor) clear() {
	e.buf, e.cur = nil, 0
	e.hpos, e.stash = len(e.hist), ""
}

// insert adds text at the caret, sanitised.
//
// Bracketed paste delivers a whole paragraph as one key, so this is the path a
// pasted file arrives on and it has to survive whatever is in it. Control
// characters other than newline are dropped rather than escaped: they would be
// invisible in the composer and then very visible in the prompt.
func (e *editor) insert(text string) {
	var rs []rune
	for _, r := range text {
		switch {
		case r == '\n':
			rs = append(rs, '\n')
		case r == '\r':
			// Paste from a Windows editor arrives CRLF. The LF is handled
			// above; dropping the CR here is what stops every pasted line from
			// ending in a stray character.
		case r == '\t':
			for i := 0; i < tabWidth; i++ {
				rs = append(rs, ' ')
			}
		case unicode.IsControl(r):
		default:
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return
	}
	e.buf = append(e.buf[:e.cur], append(rs, e.buf[e.cur:]...)...)
	e.cur += len(rs)
}

func (e *editor) insertRune(r rune) { e.insert(string(r)) }

func (e *editor) backspace() {
	if e.cur == 0 {
		return
	}
	e.buf = append(e.buf[:e.cur-1], e.buf[e.cur:]...)
	e.cur--
}

func (e *editor) del() {
	if e.cur >= len(e.buf) {
		return
	}
	e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
}

func (e *editor) left() {
	if e.cur > 0 {
		e.cur--
	}
}

func (e *editor) right() {
	if e.cur < len(e.buf) {
		e.cur++
	}
}

// home and end work on the logical line, not the wrapped row.
//
// The wrapped row is a fact about the window width; the logical line is a fact
// about what was typed. Binding Home to the former means the same key does
// different things after a resize.
func (e *editor) home() {
	for e.cur > 0 && e.buf[e.cur-1] != '\n' {
		e.cur--
	}
}

func (e *editor) end() {
	for e.cur < len(e.buf) && e.buf[e.cur] != '\n' {
		e.cur++
	}
}

func (e *editor) wordLeft() {
	for e.cur > 0 && isBoundary(e.buf[e.cur-1]) {
		e.cur--
	}
	for e.cur > 0 && !isBoundary(e.buf[e.cur-1]) {
		e.cur--
	}
}

func (e *editor) wordRight() {
	for e.cur < len(e.buf) && isBoundary(e.buf[e.cur]) {
		e.cur++
	}
	for e.cur < len(e.buf) && !isBoundary(e.buf[e.cur]) {
		e.cur++
	}
}

func (e *editor) killWordLeft() {
	at := e.cur
	e.wordLeft()
	e.kill = string(e.buf[e.cur:at])
	e.buf = append(e.buf[:e.cur], e.buf[at:]...)
}

// killToEnd and killToStart stop at the line, not at the buffer, so Ctrl-K in a
// multi-line prompt deletes the line you are on instead of everything below it.
func (e *editor) killToEnd() {
	at := e.cur
	e.end()
	if e.cur == at {
		return
	}
	e.kill = string(e.buf[at:e.cur])
	e.buf = append(e.buf[:at], e.buf[e.cur:]...)
	e.cur = at
}

func (e *editor) killToStart() {
	at := e.cur
	e.home()
	if e.cur == at {
		return
	}
	e.kill = string(e.buf[e.cur:at])
	e.buf = append(e.buf[:e.cur], e.buf[at:]...)
}

func (e *editor) yank() { e.insert(e.kill) }

func isBoundary(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("/\\.,;:()[]{}\"'`", r)
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

func (e *editor) remember(line string) {
	if line == "" {
		return
	}
	// Consecutive duplicates only. Deduplicating the whole history would make
	// Up unpredictable: the same key would skip a different number of entries
	// depending on what was typed hours ago.
	if n := len(e.hist); n > 0 && e.hist[n-1] == line {
		e.hpos = len(e.hist)
		return
	}
	e.hist = append(e.hist, line)
	if len(e.hist) > maxHistory {
		e.hist = append([]string(nil), e.hist[len(e.hist)-maxHistory:]...)
	}
	e.hpos = len(e.hist)
}

// histPrev walks back through history, reporting whether it moved.
//
// The live buffer is stashed on the way out and restored on the way back in, so
// browsing history and changing your mind costs nothing — the commonest reason
// people stop using history is having lost a half-written line to it once.
func (e *editor) histPrev() bool {
	if e.hpos == 0 {
		return false
	}
	if e.hpos == len(e.hist) {
		e.stash = e.text()
	}
	e.hpos--
	e.setText(e.hist[e.hpos])
	return true
}

func (e *editor) histNext() bool {
	if e.hpos >= len(e.hist) {
		return false
	}
	e.hpos++
	if e.hpos == len(e.hist) {
		e.setText(e.stash)
		e.stash = ""
		return true
	}
	e.setText(e.hist[e.hpos])
	return true
}

// multiline reports whether the buffer has more than one logical line, which is
// what decides whether Up moves the caret or browses history.
func (e *editor) multiline() bool {
	for _, r := range e.buf {
		if r == '\n' {
			return true
		}
	}
	return false
}

// lineUp and lineDown move the caret between logical lines, keeping the column.
func (e *editor) lineUp() bool {
	col := e.column()
	start := e.lineStart(e.cur)
	if start == 0 {
		return false
	}
	prev := e.lineStart(start - 1)
	e.cur = min(prev+col, start-1)
	return true
}

func (e *editor) lineDown() bool {
	col := e.column()
	end := e.lineEnd(e.cur)
	if end >= len(e.buf) {
		return false
	}
	next := end + 1
	e.cur = min(next+col, e.lineEnd(next))
	return true
}

func (e *editor) column() int { return e.cur - e.lineStart(e.cur) }
func (e *editor) lineStart(i int) int {
	for i > 0 && e.buf[i-1] != '\n' {
		i--
	}
	return i
}

func (e *editor) lineEnd(i int) int {
	for i < len(e.buf) && e.buf[i] != '\n' {
		i++
	}
	return i
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// render lays the buffer out in w columns and reports where the caret landed.
//
// Wrapping is done here rather than by term.WrapCols because the caret has to
// come out of the same walk. Two passes — one to wrap, one to find the caret —
// is how a caret ends up a column away from the character it is on, every time
// a wide rune or an escape sequence changes one pass and not the other.
//
// Runes at or after maskFrom are drawn as bullets; a negative maskFrom draws
// everything. Masking happens at draw time and never touches the buffer, so
// what gets submitted is what was typed.
func (e *editor) render(prompt, cont string, w, maskFrom int) (rows []string, caretRow, caretCol int) {
	if w < 8 {
		w = 8
	}
	contW := term.DispWidth(cont)
	var (
		b   strings.Builder
		col = term.DispWidth(prompt)
	)
	b.WriteString(prompt)
	caretRow, caretCol = 0, col

	wrap := func() {
		rows = append(rows, b.String())
		b.Reset()
		b.WriteString(cont)
		col = contW
	}
	for i, r := range e.buf {
		if i == e.cur {
			caretRow, caretCol = len(rows), col
		}
		if r == '\n' {
			wrap()
			continue
		}
		if maskFrom >= 0 && i >= maskFrom {
			r = '•'
		}
		rw := term.RuneWidth(r)
		if col+rw > w {
			wrap()
		}
		b.WriteRune(r)
		col += rw
	}
	if e.cur >= len(e.buf) {
		// The caret sits after the last character, and if that filled the row
		// it belongs on the next one — otherwise it is drawn in the last
		// column, on top of the character it is supposed to be after.
		if col >= w {
			wrap()
		}
		caretRow, caretCol = len(rows), col
	}
	rows = append(rows, b.String())
	return rows, caretRow, caretCol
}

// window trims rendered rows to at most n, keeping the caret's row visible.
func window(rows []string, caretRow, n int) (out []string, caret int) {
	if n <= 0 || len(rows) <= n {
		return rows, caretRow
	}
	start := caretRow - n + 1
	if start < 0 {
		start = 0
	}
	if start > len(rows)-n {
		start = len(rows) - n
	}
	return rows[start : start+n], caretRow - start
}
