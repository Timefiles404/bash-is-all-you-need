package tui

import (
	"fmt"
	"strings"
	"sync"

	"bash-is-all-you-need/external/tui/term"
)

// maxLineBytes caps one logical line.
//
// Not a display concern. A logical line is re-wrapped whenever the window
// changes width, and a single four-megabyte line — one `cat` of a minified
// bundle is enough — turns a resize into a hang. Truncating at write time is
// the only place the cost is paid once.
const maxLineBytes = 64 << 10

// Class says what a line is, which decides two things: whether the compact view
// shows it, and whether it is read as prose.
//
// The classification does not come from looking at the text. The host sets it
// from its own event stream just before the renderer writes, which is the only
// place the answer is actually known — see App.SetClass. Guessing from the
// rendered characters would work today and would quietly stop working the first
// time somebody changed a prefix in the renderer, and nothing would fail.
type Class uint8

const (
	// ClassPlain is the default: the shell's own output, command echoes,
	// anything written outside a classified region. Always shown, never
	// touched. Being the zero value is deliberate — a line nobody classified
	// is a line that stays visible.
	ClassPlain Class = iota

	// ClassProse is text the model wrote. Always shown, and rendered as
	// Markdown.
	ClassProse

	// ClassDetail is instrumentation: the per-call panel, raw command output,
	// retry and cache bookkeeping. Correct, occasionally vital, and most of the
	// screen. The compact view folds it away.
	ClassDetail
)

// sbLine is one logical line and what kind of line it is.
type sbLine struct {
	text string
	cls  Class
}

// scrollback is the pane above the composer: an io.Writer that any goroutine
// may write to, and a windowed view of it that the render loop reads.
//
// The host program's existing renderer writes here unchanged, which is the
// reason this type exists in this shape rather than as a list of structured
// messages. That renderer streams model text a token at a time, with no
// newline until the paragraph ends, so "the current line" has to be a real
// state this type carries rather than something reconstructed per frame.
type scrollback struct {
	mu sync.Mutex

	lines   []sbLine // complete logical lines, oldest first
	partial string   // written but not yet terminated by a newline
	dropped int      // logical lines discarded to stay under maxLines

	// cur is the class the next completed line will be given, and md renders
	// the ones that come out as prose. Both are written by the host's event
	// sink and read by whatever goroutine happens to be writing, so both live
	// under the same mutex as everything else here.
	cur Class
	md  *md
	st  style

	// detail is the view mode: false folds ClassDetail runs into one row each,
	// true shows everything. It is the state Ctrl-O toggles.
	detail bool

	// cr records that the previous write ended on a carriage return, so what it
	// means is not decided yet. See add.
	cr bool

	maxLines int

	// The wrap cache. rows is every element of lines wrapped to width w, and
	// wrapped counts how many of lines it already covers — so appending a line
	// wraps one line, not the whole pane. Streaming a reply writes hundreds of
	// times a second, and re-wrapping five thousand lines each time is the
	// difference between a UI and a space heater.
	//
	// rowDetail is the view mode the cache was built for. Toggling the mode
	// changes what every row is, so it invalidates the cache exactly the way a
	// width change does.
	w         int
	rows      []string
	wrapped   int
	rowDetail bool

	// The open fold, while folding. run is the index in rows of the placeholder
	// standing in for the current run of detail lines, or -1 when the last line
	// was not folded; runN is how many lines it is standing in for. Keeping the
	// index means a new detail line rewrites one row instead of re-wrapping the
	// pane, which is what lets the cache stay incremental in both modes.
	run  int
	runN int

	// detailN is how many of lines are ClassDetail. See folded.
	detailN int
}

func newScrollback(maxLines int, st style) *scrollback {
	if maxLines < 16 {
		maxLines = 16
	}
	return &scrollback{maxLines: maxLines, st: st, md: newMD(st), run: -1}
}

// setClass says what the lines written from now on are. Called by the host's
// event sink immediately before the renderer writes the lines in question.
func (s *scrollback) setClass(c Class) {
	s.mu.Lock()
	s.cur = c
	s.mu.Unlock()
}

// setDetail chooses the view mode and reports whether it changed.
func (s *scrollback) setDetail(on bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detail == on {
		return false
	}
	s.detail = on
	return true
}

// toggleDetail flips the view mode and returns what it is now.
//
// One lock acquisition rather than a read followed by a write. Only the loop
// goroutine presses keys, so reading and then setting would be correct today —
// and would be a lost update the first time anything else changed the mode,
// with no test able to see it because the window between the two would be a few
// nanoseconds wide.
func (s *scrollback) toggleDetail() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detail = !s.detail
	return s.detail
}

func (s *scrollback) detailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detail
}

// folded reports how many lines the compact view is currently hiding, for the
// indicator that tells you they are there. A count of zero and a mode that
// hides nothing look the same to the reader, which is the honest outcome.
//
// Counted as lines arrive rather than by walking the pane, because this is read
// once per frame — thirty times a second, against five thousand lines — and a
// loop there is work done continuously to answer a question that changes once
// per line.
func (s *scrollback) folded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detail {
		return 0
	}
	return s.detailN
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
	line := sbLine{text: s.partial, cls: s.cur}
	s.partial = ""
	if len(line.text) > maxLineBytes {
		line.text = line.text[:maxLineBytes] + " …(line truncated)"
	}
	// Markdown is applied here, once, rather than per frame: a line is styled
	// the moment it is complete and then never again, and the pane redraws
	// thirty times a second. It happens before the line is stored so that
	// everything downstream — wrapping, the transcript reprint, the width
	// arithmetic — sees one kind of string.
	if line.cls == ClassProse && s.md != nil {
		line.text = s.md.line(line.text)
	}
	s.lines = append(s.lines, line)
	if line.cls == ClassDetail {
		s.detailN++
	}
	if len(s.lines) <= s.maxLines {
		// The cache is extended lazily, by syncRows, and not here. Both would
		// work; only one has to get the fold bookkeeping right, and a second
		// copy of it maintained in a different function is how the two drift.
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
	for _, l := range s.lines[:cut] {
		if l.cls == ClassDetail {
			s.detailN--
		}
	}
	s.lines = append([]sbLine(nil), s.lines[cut:]...)
	s.dropped += cut
	s.rows, s.wrapped, s.run = nil, 0, -1
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
//
// This is where folding happens, and it happens here rather than at the view
// layer for one reason: `up` is an offset in rows. If the rows a scroll offset
// counts were not the rows on the screen, every key that moves the pane would
// have to convert between two coordinate systems, and Ctrl-O would leave the
// reader somewhere unrelated to where they were looking.
func (s *scrollback) syncRows(w int) {
	if w != s.w || s.detail != s.rowDetail {
		s.w, s.rowDetail = w, s.detail
		s.rows, s.wrapped, s.run = nil, 0, -1
	}
	for s.wrapped < len(s.lines) {
		line := s.lines[s.wrapped]
		s.wrapped++
		if s.detail || line.cls != ClassDetail {
			s.run = -1
			s.rows = append(s.rows, wrapLine(line.text, w)...)
			continue
		}
		// A run of hidden lines leaves one row behind saying how many. Hiding
		// them silently would be worse than showing them: output that stops
		// mid-way with no mark reads as the program having failed, and the
		// reader has no way to learn that a key would bring it back.
		if s.run < 0 {
			s.run, s.runN = len(s.rows), 0
			s.rows = append(s.rows, "")
		}
		s.runN++
		s.rows[s.run] = s.foldRow(s.runN, w)
	}
}

// foldRow is the placeholder a folded run collapses to.
//
// Dimmed, because it is the shell talking rather than the session, and it says
// which key brings the lines back. A placeholder that only said how many lines
// were missing would leave a reader to guess, and the guess most people make is
// that the program lost them.
func (s *scrollback) foldRow(n, w int) string {
	word := "lines"
	if n == 1 {
		word = "line"
	}
	return term.TruncCols("  "+s.st.dim(fmt.Sprintf("⋯ %d %s hidden · ctrl-o", n, word)), w)
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
	// The line being written is folded on the same rule as a finished one.
	//
	// Without this it is drawn in full and then vanishes into the placeholder
	// the moment its newline lands, because folding happens per completed line.
	// Showing detail in a view whose whole job is to hide detail, and then
	// taking it away, is worse than never showing it: the reader is told
	// something is there and then told it is not.
	var tail []string
	if s.partial != "" && (s.detail || s.cur != ClassDetail) {
		text := s.partial
		if s.cur == ClassProse && s.md != nil {
			text = s.md.preview(text)
		}
		tail = wrapLine(text, w)
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
	s.run, s.runN, s.detailN = -1, 0, 0
	// The Markdown reader's only state is whether a fenced block is open, and
	// the line that would have closed it has just been deleted. Left alone, the
	// rest of the session would be rendered as code.
	if s.md != nil {
		s.md.reset()
	}
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
