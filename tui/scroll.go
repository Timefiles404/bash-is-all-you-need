package tui

import (
	"strings"
	"sync"

	"bash-is-all-you-need/tui/term"
)

// maxLineBytes caps one logical line.
//
// Not a display concern. A logical line is re-wrapped whenever the window
// changes width, and a single four-megabyte line — one `cat` of a minified
// bundle is enough — turns a resize into a hang. Truncating at write time is
// the only place the cost is paid once.
const maxLineBytes = 64 << 10

// scrollback is the pane above the status bar: an io.Writer that any goroutine
// may write to, and a windowed view of it that the render loop reads.
//
// The host program's existing renderer writes here unchanged, which is the
// reason this type exists in this shape rather than as a list of structured
// messages. That renderer streams model text a token at a time, with no
// newline until the paragraph ends, so "the current line" has to be a real
// state this type carries rather than something reconstructed per frame.
type scrollback struct {
	mu sync.Mutex

	lines   []string // complete logical lines, oldest first
	partial string   // written but not yet terminated by a newline
	dropped int      // logical lines discarded to stay under maxLines

	// cr records that the previous write ended on a carriage return, so what it
	// means is not decided yet. See add.
	cr bool

	maxLines int

	// The wrap cache. rows is every element of lines wrapped to width w, and
	// wrapped counts how many of lines it already covers — so appending a line
	// wraps one line, not the whole pane. Streaming a reply writes hundreds of
	// times a second, and re-wrapping five thousand lines each time is the
	// difference between a UI and a space heater.
	w       int
	rows    []string
	wrapped int
}

func newScrollback(maxLines int) *scrollback {
	if maxLines < 16 {
		maxLines = 16
	}
	return &scrollback{maxLines: maxLines}
}

func (s *scrollback) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.add(string(p))
	s.mu.Unlock()
	return len(p), nil
}

// add appends text, splitting it into logical lines. Caller holds mu.
//
// A chunk may end mid-rune: the SSE decoder hands over whatever arrived, and a
// multi-byte character can straddle two network reads. Nothing here needs to
// care, because partial is a byte-wise string and the rune completes when its
// remaining bytes land. Only the width of the incomplete tail is briefly wrong,
// for one frame, and it corrects itself.
func (s *scrollback) add(text string) {
	// Finish the decision the previous write could not make.
	//
	// This is the case the comment below used to claim was impossible. A CR at
	// the very end of a chunk has no next byte to look at, and the earlier
	// version resolved it immediately as a rewrite — so "hello\r" followed by
	// "\n", which is one line ending split across two writes, threw "hello"
	// away and pushed a blank line. The chunk boundary is wherever a network
	// read happened to land, so nothing about it is exotic.
	if s.cr {
		s.cr = false
		if text == "" {
			return
		}
		if text[0] == '\n' {
			s.push()
			text = text[1:]
		} else {
			s.partial = ""
		}
	}
	for text != "" {
		i := strings.IndexAny(text, "\n\r")
		if i < 0 {
			s.partial += text
			return
		}
		s.partial += text[:i]
		if text[i] == '\n' {
			s.push()
			text = text[i+1:]
			continue
		}
		if i+1 == len(text) {
			// The chunk ends on the CR, so nothing is decided. Hold the line as
			// it stands — which is also what a terminal shows at this moment —
			// and let the next write say what it meant.
			s.cr = true
			return
		}
		// A bare carriage return rewrites the line in place — how a progress
		// counter is written to a terminal. "\r\n" is a line ending and must
		// not be read as a rewrite of nothing, which would drop the line.
		if text[i+1] == '\n' {
			s.push()
			text = text[i+2:]
			continue
		}
		s.partial = ""
		text = text[i+1:]
	}
}

// push moves partial into lines. Caller holds mu.
func (s *scrollback) push() {
	line := s.partial
	s.partial = ""
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes] + " …(line truncated)"
	}
	s.lines = append(s.lines, line)
	if len(s.lines) <= s.maxLines {
		// Only extend the cache once a width is known. Before the first frame
		// there is none — the banner is written before the terminal is even
		// open — and appending nothing while incrementing the count would leave
		// the cache claiming to cover a line it had skipped.
		if s.w > 0 {
			s.rows = append(s.rows, wrapLine(line, s.w)...)
			s.wrapped++
		}
		return
	}
	// Dropping the oldest line shifts every index the wrap cache is built on,
	// so a drop invalidates it. One drop per line written would therefore
	// re-wrap the whole pane on every line once the cap is reached; dropping a
	// batch makes that cost amortised and invisible.
	cut := s.maxLines / 8
	if cut < 1 {
		cut = 1
	}
	s.lines = append([]string(nil), s.lines[cut:]...)
	s.dropped += cut
	s.rows, s.wrapped = nil, 0
}

// wrapLine wraps one logical line, and never returns nothing.
//
// An empty line has to occupy a row. The panel draws blank separators between
// calls, and a wrapper that returns no rows for them collapses the spacing the
// panel was designed around — the output stops looking like the output the
// chapters show.
func wrapLine(line string, w int) []string {
	if line == "" {
		return []string{""}
	}
	rows := term.WrapCols(line, w)
	if len(rows) == 0 {
		return []string{""}
	}
	return rows
}

// syncRows brings the wrap cache up to date. Caller holds mu.
func (s *scrollback) syncRows(w int) {
	if w != s.w {
		s.w, s.rows, s.wrapped = w, nil, 0
	}
	for s.wrapped < len(s.lines) {
		s.rows = append(s.rows, wrapLine(s.lines[s.wrapped], w)...)
		s.wrapped++
	}
}

// view returns the rows to draw in a pane w columns wide and h rows tall,
// scrolled `up` rows above the bottom.
//
// The returned up is the value actually used after clamping, and the caller is
// expected to store it back: that is what makes "scroll past the top" and
// "scroll past the bottom" impossible to express, rather than something every
// key handler has to remember to check.
func (s *scrollback) view(w, h, up int) (rows []string, total, clamped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w <= 0 || h <= 0 {
		return nil, 0, 0
	}
	s.syncRows(w)

	// The partial line is deliberately not in the cache: it changes on every
	// write, and caching a value that is invalid by the time it is read costs
	// an allocation to be wrong.
	var tail []string
	if s.partial != "" {
		tail = wrapLine(s.partial, w)
	}
	total = len(s.rows) + len(tail)
	at := func(i int) string {
		if i < len(s.rows) {
			return s.rows[i]
		}
		return tail[i-len(s.rows)]
	}

	if max := total - h; up > max {
		up = max
	}
	if up < 0 {
		up = 0
	}
	end := total - up
	start := end - h
	if start < 0 {
		start = 0
	}
	rows = make([]string, 0, end-start)
	for i := start; i < end; i++ {
		rows = append(rows, at(i))
	}
	return rows, total, up
}

// clear empties the pane. It does not touch the conversation: the reason to
// have a command for it is a screen full of one long command's output, and
// deleting the model's memory to tidy the screen would be a surprising way to
// answer that.
func (s *scrollback) clear() {
	s.mu.Lock()
	s.lines, s.partial, s.rows, s.wrapped, s.cr = nil, "", nil, 0, false
	s.mu.Unlock()
}

// stats reports what the pane is holding, for /status.
func (s *scrollback) stats() (lines, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.lines)
	if s.partial != "" {
		n++
	}
	return n, s.dropped
}
