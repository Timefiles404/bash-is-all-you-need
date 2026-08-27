// Stage 06 — the composer: the event loop, and what a TUI actually is.
//
// Strip away the frameworks and a terminal UI is three functions and a select:
//
//	bytes → key       decoding what the terminal sent      (keys.go)
//	state + key → state   what that means                  (this file)
//	state → lines     what it should look like             (views.go)
//
// The loop below is thirty lines. Everything that makes a TUI hard lives in the
// three files around it: the terminal has to be given back (term.go), the
// keyboard speaks a language with an ambiguity in it (keys.go), and a column is
// not a byte (width.go). A framework hides all three, which is fine until one
// of them misbehaves and you have no idea which one.
//
// It is also, deliberately, a *reader* rather than a chat window. The trace is
// the source of truth — stage 02 made sure of that — so the composer works with
// no key, no network and no model, on a session recorded weeks ago or on one
// running in another terminal right now (`r` re-reads the file). Everything you
// can see here, you can see about a session you did not run.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type viewKind int

const (
	viewGod viewKind = iota
	viewModel
	viewWire
)

func (v viewKind) String() string {
	return [...]string{"GOD", "MODEL", "WIRE"}[v]
}

type composer struct {
	path string
	s    *session

	view viewKind
	call int // selected call index
	top  int // first visible body line

	w, h  int
	lines []string // the current view, rendered
	help  bool
	note  string // one-shot status message
}

// escTimeout is how long the loop waits before deciding a lone ESC was the
// Escape key rather than the start of a sequence.
//
// keys.go explains why the decoder cannot make this call itself. The number is
// a policy: too short and a slow ssh link turns arrow keys into Escapes; too
// long and Escape feels broken. 50ms is the value most terminal applications
// converged on, and it is the reason pressing Escape in vim has always felt
// very slightly late.
const escTimeout = 50 * time.Millisecond

func runComposer(path string) error {
	events, err := ReadTrace(path)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("%s contains no events", path)
	}
	c := &composer{path: path, s: indexSession(path, events)}

	return withTerminal(os.Stdin, os.Stdout, func(t *terminal) error {
		c.w, c.h = t.Size()
		c.relayout()
		c.draw(t)

		in := readLoop(t.in)
		var buf []byte
		var escTimer <-chan time.Time

		for {
			select {
			case chunk, ok := <-in:
				if !ok {
					return nil // stdin closed
				}
				buf = append(buf, chunk...)
				// Drain every COMPLETE key. What is left is a prefix, and the
				// only correct response to a prefix is to wait for more bytes.
				for len(buf) > 0 {
					k, n, ok := decodeKey(buf)
					if !ok {
						break
					}
					buf = buf[n:]
					if !c.handle(k) {
						return nil
					}
				}

			case <-escTimer:
				// The wait is over: whatever is in the buffer is all there is.
				// decodeKeyFinal resolves the lone ESC that decodeKey refused
				// to guess at.
				for len(buf) > 0 {
					k, n, ok := decodeKeyFinal(buf)
					if !ok {
						break
					}
					buf = buf[n:]
					if !c.handle(k) {
						return nil
					}
				}

			case <-t.resize:
				// Ask for the size; do not trust the notification to carry it.
				// The channel says "it changed", and by the time we look it may
				// have changed again — which is exactly why the notification
				// carries no payload.
				c.w, c.h = t.Size()
				c.relayout()
			}

			// A nil channel blocks forever, so this one line arms and disarms
			// the Escape timer. Bytes still in the buffer mean an unresolved
			// prefix; an empty buffer means nothing is pending.
			if len(buf) > 0 {
				escTimer = time.After(escTimeout)
			} else {
				escTimer = nil
			}
			c.draw(t)
		}
	})
}

// bodyHeight is the scrollable area: everything but the two chrome rows.
func (c *composer) bodyHeight() int { return max(1, c.h-3) }

func (c *composer) relayout() {
	switch c.view {
	case viewGod:
		c.lines, _ = c.s.godView(c.w, 0)
	case viewModel:
		c.lines = c.s.modelView(c.call, c.w)
	case viewWire:
		c.lines = c.s.wireView(c.call, c.w)
	}
	c.clamp()
}

func (c *composer) clamp() {
	maxTop := max(0, len(c.lines)-c.bodyHeight())
	c.top = min(max(0, c.top), maxTop)
}

// handle applies one key. Returning false quits.
func (c *composer) handle(k key) bool {
	c.note = ""
	switch k.Kind {
	case keyCtrlC, keyCtrlD:
		return false
	case keyEsc:
		if c.help {
			c.help = false
			return true
		}
		return false

	case keyUp:
		c.top--
	case keyDown:
		c.top++
	case keyPageUp:
		c.top -= c.bodyHeight()
	case keyPageDown:
		c.top += c.bodyHeight()
	case keyHome:
		c.top = 0
	case keyEnd:
		c.top = len(c.lines)

	case keyMouse:
		switch k.Mouse.Button {
		case 64: // wheel up
			c.top -= 3
		case 65: // wheel down
			c.top += 3
		case 0:
			if k.Mouse.Press {
				c.clickAt(k.Mouse.Y)
			}
		}

	case keyRune:
		switch k.Rune {
		case 'q':
			return false
		case 'g', '1':
			c.setView(viewGod)
		case 'm', '2':
			c.setView(viewModel)
		case 'w', '3':
			c.setView(viewWire)
		case 'j':
			c.top++
		case 'k':
			c.top--
		case ' ':
			c.top += c.bodyHeight()
		case 'n', ']':
			c.selectCall(c.call + 1)
		case 'p', '[':
			c.selectCall(c.call - 1)
		case 'r':
			c.reload()
		case '?':
			c.help = !c.help
		}

	case keyTab:
		c.setView((c.view + 1) % 3)
	}
	c.relayout()
	return true
}

func (c *composer) setView(v viewKind) {
	if v == c.view {
		return
	}
	c.view = v
	c.top = 0
}

func (c *composer) selectCall(i int) {
	if i < 0 || i >= len(c.s.Calls) {
		return
	}
	c.call = i
	c.top = 0
	if c.view == viewGod {
		// In the God view, moving between calls scrolls to the call rather than
		// changing what is displayed — the God view has no notion of a
		// "current" call, and pretending otherwise would make n/p mean two
		// different things depending on which view you were in.
		if _, ln := c.s.godView(c.w, c.s.Calls[i].Seq); ln > 0 {
			c.top = max(0, ln-2)
		}
	}
}

// clickAt maps a screen row to a call, in the God view.
//
// This is the reason the mouse is worth wiring up at all: in a two-thousand
// line event stream, "show me what the model saw at the moment this went wrong"
// is a click, and any other input mechanism is a search.
func (c *composer) clickAt(row int) {
	if c.view != viewGod {
		return
	}
	// draw() puts the header at screen row 1 and the rule at row 2, so the
	// first body line is row 3 and body line i is row 3+i. This was `row - 2`
	// and selected the line below the one clicked — the kind of bug that looks
	// like the mouse being imprecise rather than like arithmetic, which is why
	// it survives so long: nobody clicks the same pixel twice to check.
	idx := c.top + row - 3
	if idx < 0 || idx >= len(c.lines) {
		return
	}
	// Walk the event stream to the event at that line, then find its call.
	seq := 0
	n := 0
	for _, e := range c.s.Display {
		ls := len(c.s.godLine(e, c.w))
		if n+ls > idx {
			seq = e.Seq
			break
		}
		n += ls
	}
	for i := len(c.s.Calls) - 1; i >= 0; i-- {
		if c.s.Calls[i].Seq <= seq {
			c.call = i
			c.note = fmt.Sprintf("selected call %d — press m for what the model saw", i+1)
			return
		}
	}
}

// reload re-reads the trace from disk.
//
// The whole point: a trace is being appended to while the agent runs, so this
// makes the composer a live monitor in a second terminal without a single line
// of IPC. The subscriber model from stage 02 paid for this without knowing it —
// the file is the interface.
func (c *composer) reload() {
	events, err := ReadTrace(c.path)
	if err != nil {
		c.note = sBad + "reload failed: " + err.Error() + sOff
		return
	}
	before := len(c.s.Events)
	c.s = indexSession(c.path, events)
	c.note = fmt.Sprintf("reloaded: %d events (+%d)", len(events), len(events)-before)
	if c.call >= len(c.s.Calls) {
		c.call = max(0, len(c.s.Calls)-1)
	}
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

func (c *composer) draw(t *terminal) {
	body := c.bodyHeight()
	out := make([]string, 0, c.h)

	// Header.
	left := fmt.Sprintf(" %s  %s", bold("composer"), c.path)
	right := fmt.Sprintf("%d events · %d calls · %d compactions  [%s] ",
		len(c.s.Events), len(c.s.Calls), c.s.Compactions, bold(c.view.String()))
	out = append(out, joinEnds(left, right, c.w))
	out = append(out, dim(strings.Repeat("─", c.w)))

	if c.help {
		out = append(out, helpLines()...)
		for len(out) < c.h {
			out = append(out, "")
		}
		t.Frame(out, c.w, c.h)
		return
	}

	for i := 0; i < body; i++ {
		if c.top+i < len(c.lines) {
			out = append(out, c.lines[c.top+i])
		} else {
			out = append(out, "")
		}
	}

	// Footer.
	pos := "top"
	if len(c.lines) > body {
		pos = fmt.Sprintf("%d%%", min(100, (c.top+body)*100/len(c.lines)))
	}
	status := c.note
	if status == "" {
		status = dim("g god · m model · w wire · n/p call · ↑↓ scroll · r reload · ? keys · q quit")
	}
	out = append(out, joinEnds(" "+status,
		dim(fmt.Sprintf("call %d/%d · %s ", c.call+1, max(1, len(c.s.Calls)), pos)), c.w))

	t.Frame(out, c.w, c.h)
}

// joinEnds puts left at the left edge and right at the right edge.
//
// The padding is computed with dispWidth, not len. Every one of these strings
// contains ANSI escapes, and half of them can contain a path with a CJK
// directory name in it; measured in bytes the right-hand side lands somewhere
// near the middle of the screen.
func joinEnds(left, right string, w int) string {
	gap := w - dispWidth(left) - dispWidth(right)
	if gap < 1 {
		return truncCols(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func helpLines() []string {
	return []string{
		"",
		"  " + bold("views"),
		"    g / 1     " + dim("GOD    — every event that happened, including what was never sent"),
		"    m / 2     " + dim("MODEL  — the messages the model actually received on this call"),
		"    w / 3     " + dim("WIRE   — the raw request body, byte for byte"),
		"    Tab       " + dim("cycle"),
		"",
		"  " + bold("moving"),
		"    ↑ ↓ j k   " + dim("scroll one line          PgUp/PgDn/Space   scroll a page"),
		"    Home End  " + dim("jump to top / bottom     wheel             scroll three"),
		"    n / p     " + dim("next / previous model call"),
		"    click     " + dim("(GOD view) select the call containing that line"),
		"",
		"  " + bold("other"),
		"    r         " + dim("re-read the trace from disk — works on a session still running"),
		"    ? Esc     " + dim("this help"),
		"    q Ctrl-C  " + dim("quit"),
		"",
		"  " + dim("The point of three views is that they DISAGREE. What happened, what the"),
		"  " + dim("model saw, and what went on the wire are three different things, and every"),
		"  " + dim("gap between them is a bug you cannot find in a chat log."),
	}
}

// dumpComposer renders one view to a writer, with no terminal involved.
//
// It exists because the rendering functions never needed a terminal in the
// first place — views.go turns a session into []string and term.go paints
// []string — and once that separation is real, a headless mode is free. It is
// also how the TUI is tested: a UI whose output can only be produced by
// pressing a key is a UI without tests.
func dumpComposer(path, view string, call, width int, w io.Writer) error {
	events, err := ReadTrace(path)
	if err != nil {
		return err
	}
	s := indexSession(path, events)
	idx := call - 1
	var lines []string
	switch view {
	case "god":
		lines, _ = s.godView(width, 0)
	case "model":
		lines = s.modelView(idx, width)
	case "wire":
		lines = s.wireView(idx, width)
	default:
		return fmt.Errorf("unknown view %q (want god, model or wire)", view)
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	return nil
}
