package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bash-is-all-you-need/external/tui/settings"
	"bash-is-all-you-need/external/tui/term"
)

// Config is everything the shell needs from the program it fronts.
//
// Every field is a function rather than a value because the shell outlives each
// of them: the provider can change mid-session, so can the working directory,
// and a status bar built from values captured at startup is a status bar that
// lies from the first /open onward.
type Config struct {
	// Title is the leftmost field of the status bar: "stage 12".
	Title string

	// Banner is written into the scrollback before the first frame. This is
	// where the host says what it is and what is missing.
	Banner []string

	// Submit runs one user turn. It is called on its own goroutine with a
	// context the user can cancel, and must return when the turn is over.
	Submit func(ctx context.Context, line string) error

	// Commands are the host's slash commands, added to the ones below.
	Commands []Command

	// Status supplies the /status report. Called from the goroutine running
	// the command, which is not the goroutine a turn runs on — whatever it
	// reads has to be safe to read while a turn is in flight.
	Status func() []Section

	// Segments supplies the status fields after Title, most important first;
	// the status row drops them from the right until the line fits. Called on
	// every frame, so it must be cheap and must not block.
	//
	// Plain text in the Label and Value: this row applies its own colour, and
	// an escape sequence arriving inside a field would end that colour early
	// and leave the rest of the line in whatever state it chose.
	Segments func() []Segment

	// Ready reports whether a turn can run at all, and if not, what the user
	// should do about it. This is the path a double-clicked binary takes: no
	// provider, no key, no environment — and the answer has to be a sentence
	// naming the command that fixes it, not an exit code.
	Ready func() (ok bool, why string)

	// InterruptCause is the cancellation cause used when the user interrupts.
	// The host passes its own error value so that its existing error triage
	// keeps classifying an interrupt as an interrupt rather than as a stall.
	InterruptCause error

	// Uninterruptible, when non-empty, is the reason a running turn cannot be
	// stopped, and it turns off the offer to stop one.
	//
	// A host whose Submit does not read its context has to set this. In this
	// repo that is stages 06 to 09: cancellation is stage 10's idea, and their
	// runTurn takes no context at all, so Escape would cancel something nothing
	// reads. The hint row would then advertise a key that does nothing, and a UI
	// that lies about a key is worse than a UI that never mentions it.
	Uninterruptible string

	// OnExit runs after the terminal has been restored, with the real stdout —
	// so a session summary lands on the screen the user keeps rather than on
	// the alternate screen that is being thrown away.
	OnExit func(w io.Writer)

	// Open switches the working directory. The path is absolute and has already
	// been checked to exist and be a directory. Returning a message shows it;
	// returning an error leaves the host to decide what state it is in. Nil
	// removes /open.
	Open func(dir string) (string, error)

	// Settings is where /provider-* writes. Nil removes those commands, and
	// with them the only way to configure a binary that was started by
	// double-clicking it.
	Settings *settings.Store

	// Env names the variables /provider-* writes; see EnvNames.
	Env EnvNames

	// Protocols are the accepted values for /provider-protocol. Defaults to
	// openai and anthropic.
	Protocols []string

	// Reconfigure is called after a setting changes, so the host can rebuild
	// whatever depended on it. Its message is shown; its error is reported as a
	// warning, because by then the value is already saved.
	Reconfigure func() (string, error)

	In  *os.File // default os.Stdin
	Out *os.File // default os.Stdout

	// MaxScrollback is how many logical lines the pane keeps. Default 5000.
	MaxScrollback int
}

// Segment is one field of the status row under the composer.
//
// Split into a label and a value because they want different weights: the label
// is furniture you read once and then stop seeing, the value is the thing you
// are actually checking. A single pre-formatted string cannot express that, and
// a row where "model" and "mimo-v2.5" are equally loud is a row nobody reads.
type Segment struct {
	Label string // dim, and omitted when empty
	Value string
	Tone  Tone
}

// Tone is what a status value means, not what colour it is.
//
// The host says "this is a warning"; the shell decides what a warning looks
// like. That indirection earns its keep in one place — a host that hard-coded
// escape sequences would have to know whether colour is on at all, which is a
// question only this package can answer.
type Tone uint8

const (
	ToneNormal Tone = iota
	ToneAccent      // the thing that identifies this session
	ToneGood
	ToneWarn
	ToneBad
	ToneMuted
)

func (s style) tone(t Tone, text string) string {
	switch t {
	case ToneAccent:
		return s.cyan(text)
	case ToneGood:
		return s.green(text)
	case ToneWarn:
		return s.yellow(text)
	case ToneBad:
		return s.red(text)
	case ToneMuted:
		return s.dim(text)
	}
	return text
}

type runState int

const (
	stIdle runState = iota
	stRunning
	stAsking
)

const (
	// frameInterval caps repaints. The host's renderer streams a reply a few
	// tokens at a time, which is hundreds of writes a second; repainting on
	// each one spends the whole session redrawing text that is about to change.
	frameInterval = 33 * time.Millisecond

	// escTimeout is how long a lone ESC waits before it is treated as the
	// Escape key rather than the first byte of a sequence. The decoder
	// deliberately does not own this number — see tui/term/keys.go. 50ms is
	// where terminal applications converged: shorter and a slow link turns
	// arrow keys into Escapes, longer and Escape feels broken.
	escTimeout = 50 * time.Millisecond

	// quitWindow is how long a first Ctrl-C stays armed. Ctrl-C is the key
	// people press to stop a command in every other program, and here there is
	// often nothing to stop; making the first press a question rather than an
	// exit is the difference between quitting and losing a conversation.
	quitWindow = 3 * time.Second

	// transcriptDump caps what is written to the real screen on the way out.
	// The alternate screen takes the whole session with it when it closes, and
	// for a debugging tool that is the wrong default — but so is ten thousand
	// lines.
	transcriptDump = 2000
)

var spinner = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

type askReq struct {
	prompt string
	reply  chan string
}

type turnResult struct {
	err  error
	what string
}

// App is the shell. One per process; not reentrant.
type App struct {
	cfg Config
	st  style
	reg registry

	back *scrollback
	ed   *editor

	in, out *os.File

	// Loop-owned state. Everything below is read and written only by the
	// goroutine running loop(), which is what keeps this type free of locks
	// that would have to be held across a repaint.
	state   runState
	what    string // what is running, for the hint row
	started time.Time
	up      int    // scrollback offset, in wrapped rows
	note    string // one-shot message in the hint row
	noteBad bool
	frames  int
	quitAt  time.Time
	pending *askReq
	cancel  context.CancelCauseFunc

	// The window size is the one piece of loop state a command goroutine reads:
	// /help and /status lay their output out to the current width. Atomic rather
	// than loop-owned because the alternative is a report formatted for whatever
	// the window was at startup, and a resize while a command runs is exactly
	// when that shows.
	cols, rows atomic.Int32

	done  chan turnResult
	asks  chan *askReq
	dirty chan struct{}

	// closeOnce guards the shutdown of asks: a subagent blocked in Ask has to
	// be released when the loop ends, and the loop can end from four places.
	closeOnce sync.Once
	closed    chan struct{}
}

// New builds the shell. It touches no terminal; Run does that.
func New(cfg Config) *App {
	if cfg.In == nil {
		cfg.In = os.Stdin
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.MaxScrollback <= 0 {
		cfg.MaxScrollback = 5000
	}
	st := style{on: colorEnabled(cfg.Out)}
	a := &App{
		cfg:    cfg,
		st:     st,
		back:   newScrollback(cfg.MaxScrollback, st),
		ed:     newEditor(),
		in:     cfg.In,
		out:    cfg.Out,
		done:   make(chan turnResult, 1),
		asks:   make(chan *askReq),
		dirty:  make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	a.cols.Store(80)
	a.rows.Store(24)
	a.reg.add(a.builtins()...)
	a.reg.add(cfg.Commands...)
	return a
}

// Out is the writer the host points its renderer at.
//
// This is the entire output seam. The host's renderer already writes lines to an
// io.Writer, which is why the shell can host a renderer written six stages
// earlier without either of them knowing about the other.
func (a *App) Out() io.Writer { return appWriter{a} }

// SetClass says what the lines written next are, so the compact view knows what
// it may fold and the pane knows what to read as prose.
//
// The host calls this from a subscriber on its own event stream, registered
// ahead of the renderer, so that by the time the renderer writes anything the
// answer is already in place. That ordering is the whole contract: this is not
// a classification of what has been written, it is a declaration about what is
// about to be.
//
// Nothing breaks if a host never calls it. Every line then arrives as
// ClassPlain, the compact view folds nothing, and the shell behaves exactly as
// it did before folding existed.
func (a *App) SetClass(c Class) { a.back.setClass(c) }

type appWriter struct{ a *App }

func (w appWriter) Write(p []byte) (int, error) {
	n, err := w.a.back.Write(p)
	w.a.poke()
	return n, err
}

func (a *App) poke() {
	select {
	case a.dirty <- struct{}{}:
	default:
	}
}

// Width and Height are the current window size, for a host command that lays
// its own output out. Safe from any goroutine; see the cols/rows fields.
func (a *App) Width() int  { return a.width() }
func (a *App) Height() int { return a.height() }

// Printf writes a line into the pane. Safe from any goroutine.
func (a *App) Printf(format string, args ...any) {
	fmt.Fprintf(appWriter{a}, format, args...)
}

// Ask puts a question in the composer and blocks until it is answered.
//
// This is how the permission gate reaches a terminal that is in raw mode. The
// gate cannot read stdin itself any more — the shell owns it — so it hands over
// the question and waits, and every key the user presses in the meantime is
// routed to the answer rather than to a prompt.
//
// ok is false when the shell is shutting down, which the gate must treat the
// same way it treats a closed stdin: deny, do not assume.
func (a *App) Ask(prompt string) (answer string, ok bool) {
	req := &askReq{prompt: prompt, reply: make(chan string, 1)}
	select {
	case a.asks <- req:
	case <-a.closed:
		return "", false
	}
	select {
	case ans, live := <-req.reply:
		return ans, live
	case <-a.closed:
		return "", false
	}
}

// Run takes the terminal, runs the shell, and gives the terminal back.
//
// An error from here means the terminal could not be opened at all, which is
// the caller's signal to fall back to a line-at-a-time prompt rather than to
// exit — a shell that refuses to start on a pipe would break every script that
// used to work.
func (a *App) Run(ctx context.Context) error {
	for _, line := range a.cfg.Banner {
		a.Printf("%s\n", line)
	}
	// Deferred rather than called after term.With returns, because term.With
	// re-raises a panic and would skip the call. Anything parked in Ask has to be
	// released however this function ends: the process is dying in that case, but
	// coupling the loop's exit to the gate's release everywhere is one line, and
	// leaving them uncoupled in one place is how a test hangs instead of failing.
	defer a.shutdown()

	err := term.With(a.in, a.out, func(t *term.Terminal) error {
		return a.loop(ctx, t, term.ReadLoop(a.in))
	})
	if err != nil {
		return err
	}
	a.dumpTranscript(a.out)
	if a.cfg.OnExit != nil {
		a.cfg.OnExit(a.out)
	}
	return nil
}

func (a *App) shutdown() {
	a.closeOnce.Do(func() { close(a.closed) })
}

// dumpTranscript reprints the pane on the real screen.
//
// The alternate screen exists so the shell can repaint without destroying what
// was on the terminal before it started, and the price is that it takes the
// whole session with it when it closes. For a tool whose reason to exist is
// reading what the agent did, losing the transcript at exit is the wrong
// trade, so it is written back out.
func (a *App) dumpTranscript(w io.Writer) {
	// Everything, including what the compact view was folding away. The
	// reprint is the record of the session, and a record that leaves out
	// whatever happened to be hidden when the window closed is not one.
	a.back.mu.Lock()
	lines := make([]string, 0, len(a.back.lines)+1)
	for _, l := range a.back.lines {
		lines = append(lines, l.text)
	}
	if a.back.partial != "" {
		lines = append(lines, a.back.partial)
	}
	dropped := a.back.dropped
	a.back.mu.Unlock()

	if len(lines) == 0 {
		return
	}
	if len(lines) > transcriptDump {
		dropped += len(lines) - transcriptDump
		lines = lines[len(lines)-transcriptDump:]
	}
	// The reprint is marked, and the blank line above the marker is not
	// decoration. What is on the terminal at this instant is whatever was there
	// before the alternate screen went up, with the cursor part-way along a row,
	// so writing straight into that puts the marker on somebody else's line.
	//
	// Without the marker the reprint is indistinguishable from new output, and
	// someone who has just watched a panel scroll past reads it a second time and
	// starts looking for the bug that produced two of them.
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n", a.st.dim("── the session above, reprinted so it outlives the alternate screen ──"))
	if dropped > 0 {
		fmt.Fprintf(w, "%s\n", a.st.dim(fmt.Sprintf("… %d earlier lines are not shown", dropped)))
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// screen is the part of a terminal the loop uses.
//
// An interface with four methods rather than *term.Terminal, for one reason: a
// *term.Terminal cannot be constructed without a real tty, and a state machine
// that can only be exercised by a human pressing keys is a state machine with no
// tests. The tests supply a screen that records frames.
type screen interface {
	Size() (int, int)
	Frame(lines []string, w, h int)
	Resize() <-chan struct{}
	Write(p []byte) (int, error)
}

func (a *App) loop(ctx context.Context, t screen, keys <-chan []byte) error {
	resize := t.Resize()
	a.setSize(t.Size())

	tick := time.NewTicker(frameInterval)
	defer tick.Stop()

	// esc is armed only while buf holds bytes that might still become a longer
	// sequence. An always-running timer would resolve a lone ESC late; a timer
	// that is never armed would never resolve it at all.
	esc := time.NewTimer(escTimeout)
	if !esc.Stop() {
		<-esc.C
	}
	escArmed := false

	var buf []byte
	paint := true
	a.draw(t)

	for {
		// Drain everything decodable before waiting again: one read can carry a
		// whole burst, and handling one key per select turn makes a paste
		// arrive at the speed of the frame ticker.
		for len(buf) > 0 {
			k, n, ok := term.DecodeKey(buf)
			if !ok {
				break
			}
			buf = buf[n:]
			if err := a.key(ctx, k); err != nil {
				return exitErr(err)
			}
			paint = true
		}

		switch {
		case len(buf) > 0 && !escArmed:
			esc.Reset(escTimeout)
			escArmed = true
		case len(buf) == 0 && escArmed:
			if !esc.Stop() {
				select {
				case <-esc.C:
				default:
				}
			}
			escArmed = false
		}

		if paint {
			a.draw(t)
			paint = false
		}

		// Only accept a new question when the last one is answered. A nil
		// channel is never selected, which is how one modal prompt at a time
		// is expressed without a lock a subagent could block the loop on.
		askCh := a.asks
		if a.pending != nil {
			askCh = nil
		}

		select {
		case b, open := <-keys:
			if !open {
				// stdin closed. Not an error: it is what happens when the shell
				// is run with its input redirected from a file that ran out.
				return nil
			}
			buf = append(buf, b...)

		case <-esc.C:
			escArmed = false
			k, n, ok := term.DecodeKeyFinal(buf)
			if !ok {
				// Bytes we cannot interpret even as final. Dropping them is the
				// only option that terminates; keeping them would re-arm the
				// timer forever on the same buffer.
				buf = nil
				continue
			}
			buf = buf[n:]
			if err := a.key(ctx, k); err != nil {
				return exitErr(err)
			}
			paint = true

		case <-resize:
			a.setSize(t.Size())
			paint = true

		case req := <-askCh:
			a.pending = req
			a.state = stAsking
			a.ed.clear()
			a.up = 0
			paint = true

		case r := <-a.done:
			a.finish(r)
			if errors.Is(r.err, ErrExit) {
				// /exit and /quit are commands, so their sentinel arrives here
				// rather than from a key handler. Without this branch the
				// command prints "exit" in red and the shell carries on, which
				// is exactly what it did until a smoke test showed the process
				// only ending because stdin had run out.
				return nil
			}
			paint = true

		case <-a.dirty:
			paint = true

		case <-tick.C:
			if a.state == stRunning {
				// The spinner and the elapsed clock are the only things on
				// screen that say the process is alive rather than wedged.
				paint = true
			}

		case <-ctx.Done():
			a.drain()
			return nil
		}
	}
}

// drain stops a turn that is still running and waits briefly for it.
//
// Reached only when the session context is cancelled from outside — a signal,
// or the host shutting down. Returning immediately would leave a goroutine
// writing into the pane while Run reprints it and the host prints its summary,
// so the transcript ends in three interleaved half-lines. The wait is bounded
// because the thing being waited for is a model call, and a shutdown that can
// be blocked by a slow server is not a shutdown.
func (a *App) drain() {
	// Release a question first, or the wait below cannot succeed. Ask watches
	// only its reply channel and the shell's shutdown, never its own context, so
	// a turn parked on a permission prompt cannot be cancelled — it would sit
	// there and every shutdown with a prompt on screen would cost the full
	// timeout. Closing the channel returns ok=false, which the gate already
	// treats the way it treats a closed stdin: it denies.
	if a.pending != nil {
		close(a.pending.reply)
		a.pending = nil
	}
	if a.cancel == nil {
		return
	}
	a.cancel(context.Canceled)
	select {
	case r := <-a.done:
		a.finish(r)
	case <-time.After(2 * time.Second):
	}
}

// exitErr turns the exit sentinel into a clean return.
func exitErr(err error) error {
	if errors.Is(err, ErrExit) {
		return nil
	}
	return err
}

func (a *App) finish(r turnResult) {
	a.what = ""
	a.cancel = nil
	// A question can outlive the turn that asked it, and falling back to idle
	// while one is on screen is not a display glitch — it is the end of the
	// permission gate for the rest of the session. The composer's own prompt
	// would be drawn over the question, no keypress would reach askKey because
	// the state is no longer stAsking, and because askCh is nil while pending is
	// set, every later Ask would block until shutdown.
	if a.pending == nil {
		a.state = stIdle
	}
	if r.err == nil || errors.Is(r.err, ErrExit) {
		return
	}
	switch {
	case errors.Is(r.err, context.Canceled), a.cfg.InterruptCause != nil && errors.Is(r.err, a.cfg.InterruptCause):
		a.setNote("interrupted", false)
	default:
		a.Printf("%s\n", a.st.red("  "+r.err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

func (a *App) key(ctx context.Context, k term.Key) error {
	// A keypress answers whatever the last note asked, so it stops being shown.
	// Notes that outlive the thing they were about are how a UI teaches people
	// to ignore it.
	// A mouse report is not an answer. Scrolling back to read the note that
	// just appeared would otherwise erase it.
	if k.Kind != term.KeyUnknown && k.Kind != term.KeyMouse {
		a.note, a.noteBad = "", false
	}

	if a.detailKey(k) {
		return nil
	}
	if a.scrollKey(k) {
		return nil
	}

	switch a.state {
	case stAsking:
		return a.askKey(k)
	case stRunning:
		return a.runningKey(k)
	default:
		return a.idleKey(ctx, k)
	}
}

// detailKey handles Ctrl-O, which switches the pane between the compact view
// and the full one.
//
// It is checked before the state machine, so it works while a turn is running
// and while a permission prompt is waiting. That is deliberate and it is most of
// the value: the moment you want the detail is the moment something is going
// wrong in front of you, and a key that only worked at an idle prompt would ask
// you to wait for the thing you are trying to watch.
//
// The scroll offset is left where it is rather than being reset. It counts rows
// and the rows have just changed underneath it, so the pane does move — but it
// moves by roughly the amount of detail that was folded near where you were
// looking, which keeps you in the same part of the session. Resetting to the
// bottom would be predictable and would throw away the reason you pressed the
// key.
func (a *App) detailKey(k term.Key) bool {
	if k.Kind != term.KeyRune || !k.Ctrl || k.Rune != 'o' {
		return false
	}
	on := a.back.toggleDetail()
	if on {
		a.setNote("showing everything · ctrl-o to fold it again", false)
	} else {
		a.setNote("compact view · ctrl-o to show everything", false)
	}
	return true
}

// scrollKey handles the bindings that mean the same thing in every state.
func (a *App) scrollKey(k term.Key) bool {
	page := a.paneHeight() - 1
	if page < 1 {
		page = 1
	}
	switch {
	case k.Kind == term.KeyPageUp:
		a.up += page
	case k.Kind == term.KeyPageDown:
		a.up -= page
	case k.Kind == term.KeyUp && (k.Shift || k.Alt):
		a.up++
	case k.Kind == term.KeyDown && (k.Shift || k.Alt):
		a.up--
	case k.Kind == term.KeyMouse && k.Mouse.Button == 64:
		a.up += 3
	case k.Kind == term.KeyMouse && k.Mouse.Button == 65:
		a.up -= 3
	case k.Kind == term.KeyMouse:
		// Any other mouse report is consumed rather than passed on: a click
		// arriving as text would insert coordinates into the prompt.
		return true
	default:
		return false
	}
	if a.up < 0 {
		a.up = 0
	}
	return true
}

func (a *App) runningKey(k term.Key) error {
	switch {
	case k.Kind == term.KeyEsc, k.Kind == term.KeyCtrlC:
		if a.cfg.Uninterruptible != "" {
			a.setNote(a.cfg.Uninterruptible, false)
			return nil
		}
		a.interrupt()
	case k.Kind == term.KeyCtrlL:
	case k.Kind == term.KeyUnknown:
	default:
		// Typing is refused while a turn is in flight, and refused out loud.
		//
		// A prompt typed mid-turn would either be swallowed — a keyboard that
		// does nothing looks broken — or queued, which silently changes what
		// the next turn is about at a moment when the user is watching output
		// and not the composer. The one key that must work here is the one
		// that stops the turn, so that is the one this says.
		if a.cfg.Uninterruptible != "" {
			a.setNote("a turn is running — "+a.cfg.Uninterruptible, false)
			return nil
		}
		a.setNote("a turn is running — esc to interrupt it", false)
	}
	return nil
}

func (a *App) askKey(k term.Key) error {
	reply := func(s string) {
		if a.pending == nil {
			return
		}
		a.pending.reply <- s
		close(a.pending.reply)
		a.pending = nil
		a.ed.clear()
		a.state = stRunning
	}
	switch k.Kind {
	case term.KeyEnter:
		reply(a.ed.text())
	case term.KeyEsc:
		// Escape is the safe answer, and for a permission prompt the safe
		// answer is no.
		reply("n")
	case term.KeyCtrlC:
		reply("q")
	case term.KeyBackspace:
		a.ed.backspace()
	case term.KeyRune:
		if k.Ctrl || k.Alt {
			return nil
		}
		a.ed.insertRune(k.Rune)
	case term.KeyPaste:
		a.ed.insert(k.Text)
	}
	return nil
}

func (a *App) idleKey(ctx context.Context, k term.Key) error {
	e := a.ed
	switch k.Kind {
	case term.KeyEnter:
		if k.Alt {
			// Alt-Enter rather than Shift-Enter: a terminal cannot report the
			// shift state of Enter at all, so the chord every editor documents
			// is the one that does not exist on the wire.
			e.insertRune('\n')
			return nil
		}
		line := strings.TrimSpace(e.text())
		if line == "" {
			e.clear()
			return nil
		}
		if a.reg.secret(line) {
			// The name is worth remembering; the argument is not. Up then only
			// reaches "/provider-apikey", which is exactly the useful half.
			name, _ := splitCommand(line)
			e.remember(name)
		} else {
			e.remember(line)
		}
		e.clear()
		a.up = 0
		return a.dispatch(ctx, line)

	case term.KeyEsc:
		if !e.empty() {
			e.clear()
			return nil
		}
		a.setNote("nothing to interrupt", false)

	case term.KeyCtrlC:
		if !e.empty() {
			e.clear()
			return nil
		}
		if time.Since(a.quitAt) < quitWindow {
			return ErrExit
		}
		a.quitAt = time.Now()
		a.setNote("press ctrl-c again to leave", false)

	case term.KeyCtrlD:
		if e.empty() {
			return ErrExit
		}
		e.del()

	case term.KeyTab:
		a.complete()

	case term.KeyBackspace:
		e.backspace()
	case term.KeyDelete:
		e.del()
	case term.KeyLeft:
		if k.Ctrl || k.Alt {
			e.wordLeft()
		} else {
			e.left()
		}
	case term.KeyRight:
		if k.Ctrl || k.Alt {
			e.wordRight()
		} else {
			e.right()
		}
	case term.KeyHome:
		e.home()
	case term.KeyEnd:
		e.end()

	case term.KeyUp:
		// History only when there is one line to replace. In a multi-line
		// prompt Up has to move the caret, or the key that navigates the thing
		// you are writing destroys it.
		if e.multiline() {
			if e.lineUp() {
				return nil
			}
		}
		if !e.histPrev() {
			a.setNote("no earlier line", false)
		}
	case term.KeyDown:
		if e.multiline() {
			if e.lineDown() {
				return nil
			}
		}
		e.histNext()

	case term.KeyPaste:
		e.insert(k.Text)

	case term.KeyRune:
		if k.Alt {
			return nil
		}
		if k.Ctrl {
			return a.control(k.Rune)
		}
		e.insertRune(k.Rune)
	}
	return nil
}

// control handles Ctrl-<letter>, which the decoder reports as a rune with Ctrl
// set. The set is deliberately the readline one: these are the chords muscle
// memory already has, and inventing new ones buys nothing.
func (a *App) control(r rune) error {
	e := a.ed
	switch r {
	case 'a':
		e.home()
	case 'e':
		e.end()
	case 'b':
		e.left()
	case 'f':
		e.right()
	case 'k':
		e.killToEnd()
	case 'u':
		e.killToStart()
	case 'w':
		e.killWordLeft()
	case 'y':
		e.yank()
	case 'g':
		e.clear()
	}
	return nil
}

func (a *App) complete() {
	line := a.ed.text()
	if !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \t") {
		return
	}
	common, hits := a.reg.complete(line)
	if len(hits) == 0 {
		a.setNote("no command starts with "+line, false)
		return
	}
	if len(hits) == 1 {
		a.ed.setText(hits[0].Name + " ")
		return
	}
	a.ed.setText(common)
	var names []string
	for _, h := range hits {
		names = append(names, h.Name)
	}
	a.Printf("  %s\n", strings.Join(names, "  "))
}

func (a *App) interrupt() {
	if a.cancel == nil {
		return
	}
	cause := a.cfg.InterruptCause
	if cause == nil {
		cause = context.Canceled
	}
	a.cancel(cause)
	a.setNote("interrupting…", false)
}

func (a *App) setNote(s string, bad bool) {
	a.note, a.noteBad = s, bad
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func (a *App) dispatch(ctx context.Context, line string) error {
	if strings.HasPrefix(line, "/") {
		name, arg := splitCommand(line)
		c, hits := a.reg.find(name)
		if c.Name == "" {
			if len(hits) == 0 {
				a.Printf("  %s\n", a.st.red("no such command: "+name+" — /help lists them"))
				return nil
			}
			var names []string
			for _, h := range hits {
				names = append(names, h.Name)
			}
			a.Printf("  %s\n", a.st.red(name+" is ambiguous: "+strings.Join(names, " ")))
			return nil
		}
		echo := line
		if c.Secret && arg != "" {
			echo = name + " " + strings.Repeat("•", 8)
		}
		a.Printf("%s\n", a.st.dim("› "+echo))
		a.start(ctx, c.Name, func(tctx context.Context) error {
			return c.Run(tctx, arg, appWriter{a})
		})
		return nil
	}

	if a.cfg.Ready != nil {
		if ok, why := a.cfg.Ready(); !ok {
			a.Printf("  %s\n", a.st.yellow(why))
			return nil
		}
	}
	if a.cfg.Submit == nil {
		a.Printf("  %s\n", a.st.red("this build has no agent wired to the shell"))
		return nil
	}
	a.start(ctx, "turn", func(tctx context.Context) error {
		return a.cfg.Submit(tctx, line)
	})
	return nil
}

// start runs work on its own goroutine and puts the shell in its running state.
//
// Commands go through the same path as a turn — including /help, which finishes
// before the next frame — because a second path would be a second place where
// interruption, the disabled composer, and the error report have to be got
// right.
func (a *App) start(ctx context.Context, what string, fn func(context.Context) error) {
	tctx, cancel := context.WithCancelCause(ctx)
	a.cancel = cancel
	a.state = stRunning
	a.what = what
	a.started = time.Now()
	go func() {
		err := fn(tctx)
		// Release the context before reporting, so a caller that leaked a
		// goroutine off it stops now rather than at process exit.
		cancel(context.Canceled)
		a.done <- turnResult{err: err, what: what}
		a.poke()
	}()
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

func (a *App) setSize(w, h int) {
	a.cols.Store(int32(w))
	a.rows.Store(int32(h))
}

func (a *App) width() int  { return int(a.cols.Load()) }
func (a *App) height() int { return int(a.rows.Load()) }

// boxPad is what the composer's border costs in columns: "│ " down the left and
// " │" down the right.
const boxPad = 4

// chromeRows is the border, the status row and the hint row — everything under
// the pane when the window has room for all of it.
const chromeRows = 4

func (a *App) paneHeight() int {
	rows, _, _ := a.inputRows()
	h := a.height() - chromeRows - rows
	if h < 1 {
		h = 1
	}
	return h
}

func (a *App) inputRows() (int, int, int) {
	prompt, cont := a.promptFor()
	rows, cr, cc := a.ed.render(prompt, cont, a.innerWidth(), a.maskFrom())
	rows, cr = window(rows, cr, composerRows(a.height()))
	return len(rows), cr, cc
}

// innerWidth is how wide the composer's text may be, once the border has taken
// its columns. Floored rather than allowed to go negative, because the editor
// divides by it.
func (a *App) innerWidth() int {
	w := a.width() - boxPad
	if w < 4 {
		w = 4
	}
	return w
}

// composerRows is how tall the composer may be in a window h rows tall.
//
// Two caps, and the window one is the load-bearing half. maxInputRows alone
// says nothing about h, so a prompt wrapping to seven rows in a three-row window
// made frame() return more lines than the window has: FrameBytes then drew the
// first three, which is the top of the composer rather than the rows around the
// caret, and the cursor escape pointed past the bottom of the screen.
func composerRows(h int) int {
	room := h - 1
	if room > maxInputRows {
		room = maxInputRows
	}
	if room < 1 {
		room = 1
	}
	return room
}

// maskFrom hides a credential while it is being typed. In the answer to a
// permission prompt there is nothing to hide, and hiding it would make a typed
// "y" unreadable.
func (a *App) maskFrom() int {
	if a.state == stAsking {
		return -1
	}
	return a.reg.maskFrom(a.ed.text())
}

func (a *App) promptFor() (prompt, cont string) {
	switch a.state {
	case stAsking:
		if a.pending != nil {
			return a.pending.prompt, "  "
		}
	case stRunning:
		return a.st.dim("> "), "  "
	}
	return "> ", "  "
}

func (a *App) draw(t screen) {
	a.frames++
	w, h := a.width(), a.height()
	lines, cr, cc, showCaret := a.frame()
	out := term.FrameBytes(lines, w, h)
	if showCaret {
		out += fmt.Sprintf("\x1b[%d;%dH", cr+1, cc+1) + term.CursorShow
	} else {
		// A hidden cursor is the honest signal that the keyboard is not going
		// into the prompt right now. It costs nothing and it is the first thing
		// anyone notices.
		out += term.CursorHide
	}
	t.Write([]byte(out))
}

func (a *App) frame() (lines []string, caretRow, caretCol int, showCaret bool) {
	w, h := a.width(), a.height()
	if w < 8 {
		w = 8
	}
	if h < 3 {
		h = 3
	}

	inner := w - boxPad
	if inner < 4 {
		inner = 4
	}
	prompt, cont := a.promptFor()
	irows, cr, cc := a.ed.render(prompt, cont, inner, a.maskFrom())
	irows, cr = window(irows, cr, composerRows(h))

	// Chrome is the composer's border, the status row and the hint row, and a
	// short window gives them up in that order: the hint, then the border, then
	// the status row. The composer is the only one you cannot work without.
	//
	// The border goes before the status row even though it is drawn closer to
	// the input, because it costs two rows and the status row costs one, and
	// what the border says — this is where you type — is the one thing about a
	// five-row window that was never in doubt. The status row is still telling
	// you which model is about to get what you type.
	hint, status, box := true, true, true
	paneH := h - len(irows) - chromeRows
	if paneH < 1 {
		hint, paneH = false, h-len(irows)-chromeRows+1
	}
	if paneH < 1 {
		box, paneH = false, h-len(irows)-1
	}
	if paneH < 1 {
		status, paneH = false, h-len(irows)
	}
	if paneH < 0 {
		paneH = 0
	}

	rows, total, up := a.back.view(w, paneH, a.up)
	a.up = up

	lines = make([]string, 0, h)
	// Pad above rather than below, so output grows up from the composer the way
	// it does in a terminal instead of hanging from the top of an empty screen.
	for i := len(rows); i < paneH; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, rows...)

	caretCol = cc
	if box {
		lines = append(lines, a.st.dim(rule('╭', '╮', w, a.boxTag(total, up))))
		caretCol += 2
	}
	base := len(lines)
	for _, r := range irows {
		if box {
			r = a.st.dim("│ ") + term.PadCols(r, inner) + a.st.dim(" │")
		}
		lines = append(lines, r)
	}
	if box {
		lines = append(lines, a.st.dim(rule('╰', '╯', w, "")))
	}
	if status {
		lines = append(lines, a.statusRow(w))
	}
	if hint {
		lines = append(lines, a.hintRow(w))
	}
	return lines, base + cr, caretCol, a.state != stRunning
}

// rule draws one horizontal border, with an optional tag inset near the right
// end. The tag is where the pane's own state goes — how far up the scrollback
// is, and how much the compact view is hiding — because the border is already
// furniture, and a reader who does not care about either can stop seeing the
// whole row at once.
func rule(left, right rune, w int, tag string) string {
	if w < 2 {
		return strings.Repeat("─", max(0, w))
	}
	mid := w - 2
	if tag == "" || term.DispWidth(tag)+6 > mid {
		return string(left) + strings.Repeat("─", mid) + string(right)
	}
	tw := term.DispWidth(tag)
	return string(left) + strings.Repeat("─", mid-tw-3) + " " + tag + " ─" + string(right)
}

// boxTag is what the composer's top border reports about the pane above it.
func (a *App) boxTag(total, up int) string {
	var parts []string
	if up > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d ↓", total-up, total))
	}
	if n := a.back.folded(); n > 0 {
		parts = append(parts, fmt.Sprintf("⋯%d", n))
	}
	return strings.Join(parts, " · ")
}

// statusRow is the line under the composer: what this session is, in colour.
//
// It sits below the input rather than above it because that is where the eye
// already is. Everything on this row answers a question you ask *about what you
// are typing* — which model will get it, which directory it will run in, how
// much of the window is left — and putting those above the box separates them
// from the thing they qualify by the height of the composer.
func (a *App) statusRow(w int) string {
	fields := []Segment{{Value: a.cfg.Title, Tone: ToneAccent}}
	if a.cfg.Segments != nil {
		fields = append(fields, a.cfg.Segments()...)
	}

	// Measured plain and styled afterwards. Escape sequences have no width, so
	// dropping fields to fit has to be decided on the text alone — deciding it
	// on the styled strings would fit a line by counting characters nobody can
	// see, and the row would run off the edge.
	plain := make([]string, 0, len(fields))
	for _, f := range fields {
		plain = append(plain, f.text())
	}
	keep := segmentsFit(plain, w-2, " · ")
	out := make([]string, 0, len(keep))
	for _, i := range keep {
		out = append(out, fields[i].render(a.st))
	}
	return term.TruncCols("  "+strings.Join(out, a.st.dim(" · ")), w)
}

func (s Segment) text() string {
	if s.Label == "" {
		return s.Value
	}
	return s.Label + " " + s.Value
}

func (s Segment) render(st style) string {
	if s.Label == "" {
		return st.tone(s.Tone, s.Value)
	}
	return st.dim(s.Label) + " " + st.tone(s.Tone, s.Value)
}

func (a *App) hintRow(w int) string {
	if a.note != "" {
		if a.noteBad {
			return term.TruncCols("  "+a.st.red(a.note), w)
		}
		return term.TruncCols("  "+a.st.yellow(a.note), w)
	}
	switch a.state {
	case stAsking:
		return term.TruncCols("  "+a.st.dim("y allow · n deny · a allow all · q stop · esc = n"), w)
	case stRunning:
		frame := spinner[(a.frames/3)%len(spinner)]
		what := a.what
		if what == "turn" {
			what = "working"
		}
		s := fmt.Sprintf("  %c %s  %s", frame, what, elapsed(time.Since(a.started)))
		if a.cfg.Uninterruptible != "" {
			return term.TruncCols(s, w)
		}
		return term.TruncCols(s+a.st.dim("  ·  esc interrupt"), w)
	default:
		return term.TruncCols("  "+a.st.dim("enter send · alt-enter newline · tab complete · /help · ctrl-c twice to leave"), w)
	}
}

func elapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
