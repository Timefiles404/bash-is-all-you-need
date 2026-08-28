package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bash-is-all-you-need/tui/term"
)

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

// fakeScreen is the loop's terminal.
//
// A *term.Terminal cannot be constructed without a real tty, which is the whole
// reason loop() takes an interface. Everything is behind one mutex: loop calls
// Size, Write and Resize from its own goroutine, but the assertions read the
// recorded frames from the test goroutine, and a command goroutine writing into
// the pane can provoke a repaint at any moment.
type fakeScreen struct {
	mu     sync.Mutex
	w, h   int
	frames []string

	resize chan struct{}
}

func newFakeScreen(w, h int) *fakeScreen {
	return &fakeScreen{w: w, h: h, resize: make(chan struct{}, 1)}
}

func (f *fakeScreen) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, f.h
}

// resizeTo changes the size and rings the bell the loop listens on, in that
// order: the other way round and the loop can read the old size.
func (f *fakeScreen) resizeTo(w, h int) {
	f.mu.Lock()
	f.w, f.h = w, h
	f.mu.Unlock()
	select {
	case f.resize <- struct{}{}:
	default:
	}
}

func (f *fakeScreen) Resize() <-chan struct{} { return f.resize }

func (f *fakeScreen) Frame(lines []string, w, h int) { f.record(term.FrameBytes(lines, w, h)) }

func (f *fakeScreen) Write(p []byte) (int, error) {
	f.record(string(p))
	return len(p), nil
}

func (f *fakeScreen) record(s string) {
	f.mu.Lock()
	f.frames = append(f.frames, s)
	f.mu.Unlock()
}

func (f *fakeScreen) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		return ""
	}
	return f.frames[len(f.frames)-1]
}

func (f *fakeScreen) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.frames...)
}

// rowsOf turns a recorded frame back into the lines that were drawn, with the
// escape sequences stripped, so an assertion can talk about text and a failure
// message can be read by a human.
func rowsOf(frame string) []string {
	var b strings.Builder
	for i := 0; i < len(frame); {
		if n := term.ANSILen(frame, i); n > 0 {
			i += n
			continue
		}
		b.WriteByte(frame[i])
		i++
	}
	return strings.Split(b.String(), "\r\n")
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

// notATerminal is what Config.Out is pointed at in these tests. A regular file
// is not a character device, so colorEnabled says no and every frame is plain
// text — otherwise the assertions below would pass or fail depending on whether
// `go test` happened to inherit a tty.
func notATerminal(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func newTestApp(t *testing.T, cfg Config) *App {
	t.Helper()
	if cfg.Out == nil {
		cfg.Out = notATerminal(t)
	}
	if cfg.In == nil {
		cfg.In = cfg.Out
	}
	a := New(cfg)
	if a.st.on {
		t.Fatal("colour is on, so the frames carry escape sequences and these assertions are not about what they say they are")
	}
	return a
}

// shell drives one App's loop over a fake screen.
type shell struct {
	t   *testing.T
	app *App
	scr *fakeScreen

	keys   chan []byte
	cancel context.CancelFunc

	exited chan struct{}
	err    error // read only after exited is closed
}

func newShell(t *testing.T, cfg Config) *shell {
	t.Helper()
	return &shell{
		t:      t,
		app:    newTestApp(t, cfg),
		scr:    newFakeScreen(60, 12),
		keys:   make(chan []byte, 8),
		exited: make(chan struct{}),
	}
}

func (s *shell) start() {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		s.err = s.app.loop(ctx, s.scr, s.keys)
		close(s.exited)
	}()
	s.t.Cleanup(func() {
		cancel()
		// What Run does once loop returns. Without it a goroutine parked in Ask
		// is never released, and the test would hang rather than fail.
		s.app.shutdown()
		select {
		case <-s.exited:
		case <-time.After(2 * time.Second):
			s.t.Error("the loop did not return within 2s of its context being cancelled")
		}
	})
	// Nothing is observable until the first frame is on the screen.
	s.waitFor("the first frame", func() bool { return s.scr.last() != "" })
}

// send writes raw bytes into the key channel, the way term.ReadLoop would.
func (s *shell) send(keys string) {
	s.t.Helper()
	select {
	case s.keys <- []byte(keys):
	case <-s.exited:
		s.t.Fatalf("the loop had already exited when %q was sent", keys)
	case <-time.After(2 * time.Second):
		s.t.Fatalf("the loop did not accept %q within 2s", keys)
	}
}

// waitFor polls a condition to a deadline. A bare sleep would be a flaky suite
// on any machine slower than the one it was written on.
func (s *shell) waitFor(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out after 2s waiting for %s\nthe last frame was:\n%s", what, s.screen())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// shot is one frame, snapshotted once.
//
// Two assertions about "the last frame" made with two separate reads can
// straddle a repaint and then describe two different frames, which is how a
// suite like this becomes flaky on a loaded machine. Every predicate below reads
// one snapshot.
type shot struct {
	raw  string
	rows []string
	text string
}

func (s *shell) shot() shot {
	raw := s.scr.last()
	rows := rowsOf(raw)
	return shot{raw: raw, rows: rows, text: strings.Join(rows, "\n")}
}

func (f shot) shows(text string) bool { return strings.Contains(f.text, text) }

// hasRow reports whether some row is exactly want, ignoring the trailing spaces
// a truncated line is padded with — and ignoring the composer's border, so that
// a test which cares what is in the prompt can say so without also restating
// how the prompt is framed.
func (f shot) hasRow(want string) bool {
	want = strings.TrimRight(want, " ")
	for _, r := range f.rows {
		if strings.TrimRight(unbox(r), " ") == want {
			return true
		}
	}
	return false
}

// unbox strips the composer's border from a row, if it has one.
func unbox(r string) string {
	if !strings.HasPrefix(r, "│ ") {
		return r
	}
	r = strings.TrimPrefix(r, "│ ")
	return strings.TrimSuffix(strings.TrimRight(r, " "), " │")
}

// hasRowExactly keeps the trailing spaces, for the one claim that is about them.
func (f shot) hasRowExactly(want string) bool {
	for _, r := range f.rows {
		if r == want {
			return true
		}
	}
	return false
}

// caretAt returns the 1-based row and column the frame parks the cursor at.
//
// The composer's border pads every row to the full width, so a claim about a
// trailing space in the prompt can no longer be made against the text of a row.
// Where the caret is says the same thing and says it more directly.
var caretEsc = regexp.MustCompile(`\x1b\[(\d+);(\d+)H`)

func (f shot) caretAt() (row, col int, ok bool) {
	m := caretEsc.FindAllStringSubmatch(f.raw, -1)
	if len(m) == 0 {
		return 0, 0, false
	}
	last := m[len(m)-1]
	row, _ = strconv.Atoi(last[1])
	col, _ = strconv.Atoi(last[2])
	return row, col, true
}

// A hidden cursor is how the loop says the keyboard is not going into the
// prompt, which is the one piece of loop state a frame reports exactly.
func (f shot) caretHidden() bool { return strings.HasSuffix(f.raw, term.CursorHide) }
func (f shot) caretShown() bool  { return strings.HasSuffix(f.raw, term.CursorShow) }

func (s *shell) screen() string          { return s.shot().text }
func (s *shell) shows(text string) bool  { return s.shot().shows(text) }
func (s *shell) hasRow(want string) bool { return s.shot().hasRow(want) }
func (s *shell) caretHidden() bool       { return s.shot().caretHidden() }
func (s *shell) caretShown() bool        { return s.shot().caretShown() }

// keepSending offers keys until cond holds, or fails at the deadline.
//
// For the one thing in the loop a test cannot observe: an undecodable buffer is
// only dropped when the escape timer expires, and keys that arrive before that
// are dropped with it. Polling a condition is not enough — the input has to be
// re-offered.
func (s *shell) keepSending(keys, what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out after 2s waiting for %s\nthe last frame was:\n%s", what, s.screen())
		}
		s.send(keys)
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *shell) running() bool {
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

func (s *shell) waitExit() error {
	s.t.Helper()
	select {
	case <-s.exited:
		return s.err
	case <-time.After(2 * time.Second):
		s.t.Fatalf("the loop did not return within 2s\nthe last frame was:\n%s", s.screen())
		return nil
	}
}

// neverShowed asserts that text was not in any byte written to the screen. The
// raw frames, not the stripped rows: the claim is about what reached the
// terminal.
func (s *shell) neverShowed(text string) {
	s.t.Helper()
	for i, f := range s.scr.all() {
		if strings.Contains(f, text) {
			s.t.Fatalf("frame %d put %q on the terminal:\n%s", i, text, strings.Join(rowsOf(f), "\n"))
		}
	}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out after 2s waiting for %s", what)
		var zero T
		return zero
	}
}

// ---------------------------------------------------------------------------
// The five claims
// ---------------------------------------------------------------------------

// A prompt typed mid-turn would either be swallowed — a keyboard that does
// nothing looks broken — or queued, which silently changes what the next turn is
// about at a moment when the user is watching output and not the composer.
func TestTypingIsRefusedOutLoudWhileATurnIsRunning(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan string, 4)
	s := newShell(t, Config{
		Submit: func(ctx context.Context, line string) error {
			entered <- line
			<-release
			return nil
		},
	})
	s.start()

	s.send("hello\r")
	if got := recv(t, entered, "Submit to be called"); got != "hello" {
		t.Fatalf("Submit was called with %q, expected %q", got, "hello")
	}
	s.waitFor("the hint row to name the key that works", func() bool {
		f := s.shot()
		if !f.shows("interrupt") {
			return false
		}
		if !f.caretHidden() {
			t.Error("the caret is still shown while a turn runs; a hidden cursor is the signal that the keyboard is not going into the prompt")
		}
		return true
	})

	s.send("xyz")
	s.waitFor("the refusal to be said out loud", func() bool { return s.shows("a turn is running") })

	close(release)
	s.waitFor("the turn to end", func() bool { return s.caretShown() })

	// Typing works again, and starts from an empty buffer: had the refused keys
	// been queued rather than dropped, the composer would read "> xyzok".
	s.send("ok")
	s.waitFor("the composer to take a new line", func() bool { return s.hasRow("> ok") })

	s.neverShowed("xyz")
}

var errHostInterrupt = errors.New("the host's own interrupt")

// The host passes its own error value so that its existing error triage keeps
// classifying an interrupt as an interrupt rather than as a stall.
func TestEscapeInterruptsARunningTurnWithTheHostsOwnCause(t *testing.T) {
	entered := make(chan struct{}, 1)
	cause := make(chan error, 1)
	s := newShell(t, Config{
		InterruptCause: errHostInterrupt,
		Submit: func(ctx context.Context, line string) error {
			entered <- struct{}{}
			<-ctx.Done()
			cause <- context.Cause(ctx)
			return context.Cause(ctx)
		},
	})
	s.start()

	s.send("go\r")
	recv(t, entered, "Submit to be called")
	s.send("\x1b")

	got := recv(t, cause, "the turn's context to be cancelled")
	if !errors.Is(got, errHostInterrupt) {
		t.Errorf("the turn was cancelled with %v, expected the configured cause %v", got, errHostInterrupt)
	}
	if errors.Is(got, context.Canceled) {
		t.Errorf("the turn was cancelled with context.Canceled; the host's triage would then read an interrupt as a stall")
	}
	s.waitFor("the hint row to report the interrupt", func() bool { return s.shows("interrupted") })
}

// With no cause configured the loop still has to cancel with something, and
// context.Canceled is the value every caller already handles.
func TestWithNoConfiguredCauseAnInterruptFallsBackToContextCanceled(t *testing.T) {
	entered := make(chan struct{}, 1)
	cause := make(chan error, 1)
	s := newShell(t, Config{
		Submit: func(ctx context.Context, line string) error {
			entered <- struct{}{}
			<-ctx.Done()
			cause <- context.Cause(ctx)
			return ctx.Err()
		},
	})
	s.start()

	s.send("go\r")
	recv(t, entered, "Submit to be called")
	s.send("\x1b")

	if got := recv(t, cause, "the cancellation"); !errors.Is(got, context.Canceled) {
		t.Errorf("the turn was cancelled with %v, expected context.Canceled", got)
	}
}

// Without Secret the key is typed in plain view into the composer, echoed into
// the output pane, kept in the line history, and written to the real terminal
// when the pane is reprinted on the way out — four copies of a credential from
// one convenience command. All four are checked here.
func TestASecretCommandsArgumentNeverBecomesReadable(t *testing.T) {
	const key = "hunter2"
	gotArg := make(chan string, 1)
	s := newShell(t, Config{Commands: []Command{{
		Name: "/secret-thing", Args: "<key>", Secret: true,
		Run: func(_ context.Context, arg string, _ io.Writer) error {
			gotArg <- arg
			return nil
		},
	}}})
	s.start()

	s.send("/secret-thing " + key)
	s.waitFor("the composer to draw the argument as bullets", func() bool {
		return s.hasRow("> /secret-thing " + strings.Repeat("•", len(key)))
	})
	s.neverShowed(key)

	s.send("\r")
	if arg := recv(t, gotArg, "the command to run"); arg != key {
		t.Errorf("the command received %q, expected %q — the masking must be display-only", arg, key)
	}
	s.waitFor("the echo in the pane", func() bool {
		return s.hasRow("› /secret-thing " + strings.Repeat("•", 8))
	})
	s.neverShowed(key)

	s.waitFor("the command to finish", func() bool { return s.caretShown() })
	s.send("\x1b[A")
	s.waitFor("history to offer the command name", func() bool { return s.hasRow("> /secret-thing") })
	if s.hasRow("> /secret-thing " + strings.Repeat("•", len(key))) {
		t.Error("history put the masked argument back in the composer; only the name is worth remembering")
	}
	s.neverShowed(key)

	// The transcript is reprinted on the real screen on the way out, which is the
	// fourth copy.
	var out strings.Builder
	s.app.dumpTranscript(&out)
	if strings.Contains(out.String(), key) {
		t.Errorf("the transcript written back to the real terminal contains the key:\n%s", out.String())
	}
}

// The permission gate cannot read stdin any more — the shell owns it — so it
// hands over the question and waits, and every key pressed in the meantime is
// routed to the answer rather than to a prompt.
func TestAskRoutesThePermissionQuestionThroughTheShell(t *testing.T) {
	type reply struct {
		answer string
		ok     bool
	}
	asked := make(chan struct{}, 1)
	got := make(chan reply, 1)

	var app *App
	s := newShell(t, Config{Submit: func(ctx context.Context, line string) error {
		asked <- struct{}{}
		a, ok := app.Ask("run rm -rf /? [y/n] ")
		got <- reply{a, ok}
		return nil
	}})
	app = s.app
	s.start()

	s.send("delete everything\r")
	recv(t, asked, "Submit to be called")

	s.waitFor("the question and the answers it accepts", func() bool {
		f := s.shot()
		if !f.shows("run rm -rf /? [y/n]") || !f.shows("y allow") {
			return false
		}
		if !f.caretShown() {
			t.Error("the caret is hidden while a question is pending; the keyboard is going into the answer, so it must be visible")
		}
		return true
	})

	s.send("y\r")
	r := recv(t, got, "Ask to return")
	if r.answer != "y" || !r.ok {
		t.Errorf("Ask returned (%q, %v), expected (\"y\", true)", r.answer, r.ok)
	}
}

// ok is false when the shell is shutting down, which the gate must treat the
// same way it treats a closed stdin: deny, do not assume.
func TestAskDeniesRatherThanHangingWhenTheShellIsShuttingDown(t *testing.T) {
	t.Run("asked after the shell is already gone", func(t *testing.T) {
		a := newTestApp(t, Config{})
		a.shutdown()

		done := make(chan struct{})
		var answer string
		var ok bool
		go func() {
			answer, ok = a.Ask("run? [y/n] ")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Ask blocked on a shell that had already shut down; a gate that cannot ask must deny, not hang")
		}
		if ok || answer != "" {
			t.Errorf("Ask returned (%q, %v), expected (\"\", false)", answer, ok)
		}
	})

	t.Run("the loop ends with a question still on screen", func(t *testing.T) {
		type reply struct {
			answer string
			ok     bool
		}
		asked := make(chan struct{}, 1)
		got := make(chan reply, 1)

		// The gate asks on its own goroutine — a subagent's permission prompt —
		// while the turn watches its context, which is the shape that lets the
		// loop's own drain() finish instead of waiting out its timeout on a turn
		// parked in Ask.
		var app *App
		s := newShell(t, Config{Submit: func(ctx context.Context, line string) error {
			asked <- struct{}{}
			go func() {
				a, ok := app.Ask("run? [y/n] ")
				got <- reply{a, ok}
			}()
			<-ctx.Done()
			return context.Cause(ctx)
		}})
		app = s.app
		s.start()

		s.send("do it\r")
		recv(t, asked, "Submit to be called")
		s.waitFor("the question to reach the composer", func() bool { return s.shows("run? [y/n]") })

		// Cancelling the context is how the host stops the shell. loop returns,
		// and Run's next statement is shutdown() — which is what releases a gate
		// that is still waiting for an answer that will never be typed.
		s.cancel()
		if err := s.waitExit(); err != nil {
			t.Errorf("the loop returned %v after its context was cancelled, expected nil", err)
		}
		s.app.shutdown()

		r := recv(t, got, "Ask to return")
		if r.ok {
			t.Errorf("Ask returned ok=true while the shell was shutting down; the gate would then run a command nobody approved")
		}
		if r.answer != "" {
			t.Errorf("Ask returned the answer %q, expected an empty one", r.answer)
		}
	})
}

// Ctrl-C is the key people press to stop a command in every other program, and
// here there is often nothing to stop; making the first press a question rather
// than an exit is the difference between quitting and losing a conversation.
func TestCtrlCOnAnEmptyPromptAsksBeforeItLeaves(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("\x03")
	s.waitFor("the note asking for a second press", func() bool { return s.shows("press ctrl-c again to leave") })
	if !s.running() {
		t.Fatal("the first ctrl-c ended the loop; it must only arm the second")
	}

	s.send("\x03")
	if err := s.waitExit(); err != nil {
		t.Errorf("two ctrl-c presses returned %v, expected a clean exit", err)
	}
}

// With something typed, Ctrl-C is "throw that away", and it does not arm the
// quit either: the next press is a first press.
func TestCtrlCOnANonEmptyPromptClearsItAndDoesNotArmTheQuit(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("half a thought")
	s.waitFor("the composer to hold the line", func() bool { return s.hasRow("> half a thought") })

	s.send("\x03")
	s.waitFor("the composer to be emptied", func() bool { return s.hasRow("> ") })
	if !s.running() {
		t.Fatal("ctrl-c on a non-empty prompt ended the loop")
	}

	s.send("\x03")
	s.waitFor("the note asking for a second press", func() bool { return s.shows("press ctrl-c again to leave") })
	if !s.running() {
		t.Error("the press that cleared the prompt was counted as the first of the two")
	}
}

// ---------------------------------------------------------------------------
// Asking
// ---------------------------------------------------------------------------

// For a permission prompt the safe answer is no, and Escape is the key that
// means "get me out of this dialogue" everywhere else.
func TestEscapeAndCtrlCAnswerAPermissionPromptSafely(t *testing.T) {
	for _, tc := range []struct{ name, keys, want string }{
		{"escape denies", "\x1b", "n"},
		{"ctrl-c stops", "\x03", "q"},
		{"enter sends what was typed", "a\r", "a"},
		{"backspace edits the answer", "ax\x7f\r", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := make(chan struct{}, 1)
			got := make(chan string, 1)
			var app *App
			s := newShell(t, Config{Submit: func(ctx context.Context, line string) error {
				asked <- struct{}{}
				a, _ := app.Ask("allow? ")
				got <- a
				return nil
			}})
			app = s.app
			s.start()

			s.send("go\r")
			recv(t, asked, "Submit to be called")
			s.waitFor("the question", func() bool { return s.shows("allow?") })

			s.send(tc.keys)
			if a := recv(t, got, "Ask to return"); a != tc.want {
				t.Errorf("%q answered the prompt with %q, expected %q", tc.keys, a, tc.want)
			}
		})
	}
}

// A nil channel is never selected, which is how one modal prompt at a time is
// expressed without a lock a subagent could block the loop on.
func TestOnlyOneQuestionIsOnScreenAtATime(t *testing.T) {
	asked := make(chan struct{}, 1)
	got := make(chan string, 2)
	var app *App
	s := newShell(t, Config{Submit: func(ctx context.Context, line string) error {
		asked <- struct{}{}
		// A subagent asking at the same time as the turn it runs inside. The
		// turn waits for it rather than returning first: see the note in
		// finish() about a question that outlives its turn.
		sub := make(chan string, 1)
		go func() {
			a, _ := app.Ask("second? ")
			sub <- a
		}()
		a, _ := app.Ask("first? ")
		got <- "first=" + a
		got <- "second=" + <-sub
		return nil
	}})
	app = s.app
	s.start()

	s.send("go\r")
	recv(t, asked, "Submit to be called")

	// Whichever question reached the loop first is the one on screen; the other
	// must not be. Which one wins the race is not this test's business.
	s.waitFor("one of the two questions", func() bool {
		f := s.shot()
		return f.shows("first?") || f.shows("second?")
	})
	f := s.shot()
	shown, waiting := "first?", "second?"
	if f.shows("second?") {
		shown, waiting = "second?", "first?"
	}
	if f.shows(shown) && f.shows(waiting) {
		t.Fatalf("both questions are on screen at once:\n%s", f.text)
	}

	s.send("y\r")
	s.waitFor("the second question to take its turn", func() bool { return s.shows(waiting) })
	s.send("n\r")

	answers := recv(t, got, "the first answer") + " " + recv(t, got, "the second answer")
	for _, want := range []string{"first=", "second="} {
		if !strings.Contains(answers, want) {
			t.Errorf("the answers were %q, expected one for %s", answers, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func TestAnUnknownCommandIsReportedAndNoTurnStarts(t *testing.T) {
	submitted := make(chan string, 1)
	s := newShell(t, Config{Submit: func(_ context.Context, line string) error {
		submitted <- line
		return nil
	}})
	s.start()

	s.send("/nope\r")
	s.waitFor("the report", func() bool {
		f := s.shot()
		if !f.shows("no such command: /nope") {
			return false
		}
		if !f.shows("/help") {
			t.Errorf("the report does not say where the list of commands is:\n%s", f.text)
		}
		return true
	})
	if len(submitted) != 0 {
		t.Errorf("an unknown command was sent to the agent as a prompt: %q", <-submitted)
	}
}

func TestAnAmbiguousCommandNameListsTheCandidatesInsteadOfGuessing(t *testing.T) {
	ran := make(chan string, 2)
	run := func(name string) func(context.Context, string, io.Writer) error {
		return func(context.Context, string, io.Writer) error { ran <- name; return nil }
	}
	s := newShell(t, Config{Commands: []Command{
		{Name: "/aardvark", Run: run("/aardvark")},
		{Name: "/aardwolf", Run: run("/aardwolf")},
	}})
	s.start()

	s.send("/aard\r")
	s.waitFor("the ambiguity report", func() bool { return s.shows("/aard is ambiguous") })
	for _, want := range []string{"/aardvark", "/aardwolf"} {
		if !s.shows(want) {
			t.Errorf("the report leaves out %q:\n%s", want, s.screen())
		}
	}
	if len(ran) != 0 {
		t.Errorf("an ambiguous name ran %q anyway", <-ran)
	}
}

func TestAnEmptyLineIsNotATurn(t *testing.T) {
	submitted := make(chan string, 4)
	s := newShell(t, Config{Submit: func(_ context.Context, line string) error {
		submitted <- line
		return nil
	}})
	s.start()

	s.send("\r")
	s.send("   \r")
	s.send("\t\r")
	s.send("real\r")

	if got := recv(t, submitted, "the one real turn"); got != "real" {
		t.Errorf("the first turn was %q, expected %q — a blank line started one", got, "real")
	}
	if len(submitted) != 0 {
		t.Errorf("%d more turns ran, expected none", len(submitted))
	}
}

// This is the path a double-clicked binary takes: no provider, no key, no
// environment — and the answer has to be a sentence naming the command that
// fixes it.
func TestReadyStopsATurnWithTheSentenceThatNamesTheFix(t *testing.T) {
	submitted := make(chan string, 1)
	s := newShell(t, Config{
		Ready:  func() (bool, string) { return false, "no API key yet — set one with /provider-apikey" },
		Submit: func(_ context.Context, line string) error { submitted <- line; return nil },
	})
	s.start()

	s.send("hello\r")
	s.waitFor("the explanation", func() bool { return s.shows("set one with /provider-apikey") })
	if len(submitted) != 0 {
		t.Errorf("the turn ran anyway, with %q", <-submitted)
	}

	// A slash command is not a turn and must still work: fixing the thing Ready
	// is complaining about is exactly what the user is here to do. The window has
	// to be tall enough to hold the whole list, or the assertion is really about
	// which end of it scrolled off.
	s.scr.resizeTo(80, 40)
	s.waitFor("the taller window", func() bool { return strings.Count(s.scr.last(), "\r\n") == 39 })

	s.send("/help\r")
	s.waitFor("/help to run", func() bool {
		f := s.shot()
		return f.shows("/clear") && f.shows("/status") && f.shows("the keyboard map")
	})
}

func TestABuildWithNoAgentWiredUpSaysSoRatherThanDoingNothing(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("hello\r")
	s.waitFor("the report", func() bool { return s.shows("no agent wired to the shell") })
}

func TestAnErrorFromATurnIsPrintedIntoThePane(t *testing.T) {
	s := newShell(t, Config{Submit: func(context.Context, string) error {
		return errors.New("the gateway said 502")
	}})
	s.start()

	s.send("hello\r")
	s.waitFor("the error in the pane", func() bool { return s.shows("the gateway said 502") })
	s.waitFor("the shell to go idle again", func() bool { return s.caretShown() })
}

func TestWhatACommandWritesLandsInThePane(t *testing.T) {
	s := newShell(t, Config{Commands: []Command{{
		Name: "/say",
		Run: func(_ context.Context, arg string, w io.Writer) error {
			fmt.Fprintf(w, "  the command said %s\n", arg)
			return nil
		},
	}}})
	s.start()

	s.send("/say hello\r")
	s.waitFor("the command's output", func() bool { return s.hasRow("  the command said hello") })
	if !s.hasRow("› /say hello") {
		t.Errorf("the line was not echoed into the pane:\n%s", s.screen())
	}
}

func TestClearEmptiesThePaneThroughTheLoop(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.app.Printf("a line worth losing\n")
	s.waitFor("the line", func() bool { return s.shows("a line worth losing") })

	s.send("/clear\r")
	s.waitFor("the pane to empty", func() bool { return !s.shows("a line worth losing") })
}

// ---------------------------------------------------------------------------
// Completion
// ---------------------------------------------------------------------------

func TestTabCompletesAUniqueCommandAndLeavesRoomForTheArgument(t *testing.T) {
	s := newShell(t, Config{Commands: []Command{{
		Name: "/deploy", Args: "<env>",
		Run: func(context.Context, string, io.Writer) error { return nil },
	}}})
	s.start()

	// The trailing space is the point: Tab on a command that takes an argument
	// should leave the caret where the argument goes.
	//
	// Asserted on the caret rather than on the row, because the composer's
	// border pads every row out to the full width and a trailing space is no
	// longer distinguishable from the padding. Where the caret actually is was
	// the claim all along.
	s.send("/dep\t")
	s.waitFor("the completion", func() bool { return s.hasRow("> /deploy") })

	f := s.shot()
	_, col, ok := f.caretAt()
	if !ok {
		t.Fatalf("the frame does not place the caret:\n%s", f.text)
	}
	// The caret escape is 1-based, so the column after everything drawn to the
	// left of the caret is that width plus one.
	if want := term.DispWidth("│ > /deploy ") + 1; col != want {
		t.Errorf("the caret is at column %d, expected %d — Tab did not leave room for the argument:\n%s", col, want, f.text)
	}
}

func TestTabOnASharedPrefixInsertsWhatTheyAgreeOnAndListsTheRest(t *testing.T) {
	s := newShell(t, Config{Commands: []Command{
		{Name: "/deploy-prod", Run: func(context.Context, string, io.Writer) error { return nil }},
		{Name: "/deploy-staging", Run: func(context.Context, string, io.Writer) error { return nil }},
	}})
	s.start()

	s.send("/dep\t")
	s.waitFor("the common prefix in the composer", func() bool { return s.hasRow("> /deploy-") })
	s.waitFor("the candidates in the pane", func() bool {
		return s.shows("/deploy-prod") && s.shows("/deploy-staging")
	})
}

func TestTabOnSomethingThatIsNotACommandSaysSoRatherThanDoingNothing(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("/zzz\t")
	s.waitFor("the note", func() bool { return s.shows("no command starts with /zzz") })

	// Tab in ordinary prose is not completion at all, and must not insert a tab
	// either — a literal tab in the buffer breaks every column this UI computes.
	s.send("\x03")
	s.send("plain text\t")
	s.waitFor("the composer to be unchanged", func() bool { return s.hasRow("> plain text") })
}

// ---------------------------------------------------------------------------
// Editing through the loop
// ---------------------------------------------------------------------------

// Alt-Enter rather than Shift-Enter: a terminal cannot report the shift state of
// Enter at all, so the chord every editor documents is the one that does not
// exist on the wire.
func TestAltEnterAddsALineInsteadOfSending(t *testing.T) {
	submitted := make(chan string, 1)
	s := newShell(t, Config{Submit: func(_ context.Context, line string) error {
		submitted <- line
		return nil
	}})
	s.start()

	s.send("first")
	s.send("\x1b\r") // ESC CR is how a terminal spells Alt-Enter
	s.send("second")
	s.waitFor("both lines in the composer", func() bool {
		return s.hasRow("> first") && s.hasRow("  second")
	})
	if len(submitted) != 0 {
		t.Fatalf("alt-enter sent the line: %q", <-submitted)
	}

	s.send("\r")
	if got := recv(t, submitted, "the turn"); got != "first\nsecond" {
		t.Errorf("the submitted line was %q, expected both rows joined by a newline", got)
	}
}

// The payload is copied out verbatim by the decoder and sanitised by the editor,
// which is the only reason a pasted file cannot submit half of itself.
func TestABracketedPasteArrivesAsOneKeyWithItsControlCharactersRemoved(t *testing.T) {
	submitted := make(chan string, 1)
	s := newShell(t, Config{Submit: func(_ context.Context, line string) error {
		submitted <- line
		return nil
	}})
	s.start()

	s.send("\x1b[200~one\tindented\r\ntwo\x1b[201~")
	s.waitFor("the paste in the composer", func() bool {
		return s.hasRow("> one    indented") && s.hasRow("  two")
	})
	if len(submitted) != 0 {
		t.Fatalf("the newline inside the paste submitted the line: %q", <-submitted)
	}

	s.send("\r")
	if got := recv(t, submitted, "the turn"); got != "one    indented\ntwo" {
		t.Errorf("the submitted line was %q, expected the tab expanded and the CR dropped", got)
	}
}

// The chords are deliberately the readline ones: this is the muscle memory
// people already have.
func TestTheReadlineChordsEditTheLine(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("some words here")
	s.waitFor("the line", func() bool { return s.hasRow("> some words here") })

	s.send("\x17") // ctrl-w, delete the word before the caret
	s.waitFor("the word to go", func() bool { return s.hasRow("> some words ") })

	s.send("\x19") // ctrl-y, put it back
	s.waitFor("the word to come back", func() bool { return s.hasRow("> some words here") })

	s.send("\x01") // ctrl-a, start of line
	s.send("\x0b") // ctrl-k, kill to the end
	s.waitFor("an empty line", func() bool { return s.hasRow("> ") })

	s.send("\x19") // ctrl-y again
	s.waitFor("the whole line back", func() bool { return s.hasRow("> some words here") })

	s.send("\x15") // ctrl-u from the end kills back to the start
	s.waitFor("the line gone", func() bool { return s.hasRow("> ") })

	s.send("keep") // ctrl-g abandons the line outright
	s.waitFor("the new line", func() bool { return s.hasRow("> keep") })
	s.send("\x07")
	s.waitFor("the line abandoned", func() bool { return s.hasRow("> ") })
}

func TestUpBrowsesHistoryOnOneLineAndMovesTheCaretOnSeveral(t *testing.T) {
	// The turn leaves a mark in the pane, so "the turn is over" is a claim about a
	// frame that certainly came after it rather than about the initial frame,
	// which looks identical to an idle one.
	var app *App
	s := newShell(t, Config{Submit: func(_ context.Context, line string) error {
		app.Printf("ran %s\n", line)
		return nil
	}})
	app = s.app
	s.start()

	s.send("first turn\r")
	s.waitFor("the turn to finish", func() bool {
		f := s.shot()
		return f.shows("ran first turn") && f.caretShown()
	})

	s.send("\x1b[A")
	s.waitFor("history", func() bool { return s.hasRow("> first turn") })

	s.send("\x1b[A")
	s.waitFor("the note at the end of the history", func() bool { return s.shows("no earlier line") })

	s.send("\x1b[B")
	s.waitFor("the live buffer back", func() bool { return s.hasRow("> ") })

	// In a multi-line prompt Up has to move the caret, or the key that navigates
	// what you are writing destroys it.
	s.send("top")
	s.send("\x1b\r")
	s.send("bottom")
	s.waitFor("two rows", func() bool { return s.hasRow("> top") && s.hasRow("  bottom") })
	s.send("\x1b[A")
	s.waitFor("both rows still there", func() bool { return s.hasRow("> top") && s.hasRow("  bottom") })
	if s.hasRow("> first turn") {
		t.Error("Up browsed history in a multi-line prompt and destroyed the buffer")
	}
}

func TestEscapeClearsTheLineAndSaysWhenThereIsNothingToInterrupt(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("something")
	s.waitFor("the line", func() bool { return s.hasRow("> something") })

	s.send("\x1b")
	s.waitFor("the line cleared", func() bool { return s.hasRow("> ") })

	s.send("\x1b")
	s.waitFor("the note", func() bool { return s.shows("nothing to interrupt") })
	if !s.running() {
		t.Error("escape on an empty prompt ended the loop")
	}
}

func TestCtrlDLeavesOnAnEmptyPromptAndDeletesForwardOnAFullOne(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("abc")
	s.send("\x01") // to the start
	s.send("\x04") // ctrl-d deletes forward
	s.waitFor("the first character gone", func() bool { return s.hasRow("> bc") })
	if !s.running() {
		t.Fatal("ctrl-d on a non-empty prompt ended the loop")
	}

	s.send("\x04")
	s.send("\x04")
	s.waitFor("an empty line", func() bool { return s.hasRow("> ") })
	s.send("\x04")
	if err := s.waitExit(); err != nil {
		t.Errorf("ctrl-d on an empty prompt returned %v, expected a clean exit", err)
	}
}

// ---------------------------------------------------------------------------
// Scrolling
// ---------------------------------------------------------------------------

func TestPageUpScrollsThePaneAndTheComposerBorderSaysWhereYouAre(t *testing.T) {
	s := newShell(t, Config{Title: "stage 12"})
	s.start()

	for i := 0; i < 60; i++ {
		s.app.Printf("line %02d\n", i)
	}
	s.waitFor("the newest line", func() bool { return s.shows("line 59") })

	s.send("\x1b[5~") // page up
	s.waitFor("the pane to scroll and the border to say so", func() bool {
		f := s.shot()
		return !f.shows("line 59") && f.shows("↓")
	})

	// Back down, and then further: the offset is clamped rather than going
	// negative, so the marker must not come back.
	for i := 0; i < 6; i++ {
		s.send("\x1b[6~") // page down
	}
	s.waitFor("the pane to come back with no scroll marker", func() bool {
		f := s.shot()
		return f.shows("line 59") && !f.shows("↓")
	})
}

func TestTheWheelScrollsAndAClickIsSwallowedRatherThanTypedIntoThePrompt(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	for i := 0; i < 40; i++ {
		s.app.Printf("row %02d\n", i)
	}
	s.send("typed")
	s.waitFor("the line and the output", func() bool {
		f := s.shot()
		return f.hasRow("> typed") && f.shows("row 39")
	})

	s.send("\x1b[<64;10;5M") // wheel up
	s.waitFor("the pane to scroll", func() bool { return !s.shows("row 39") })

	s.send("\x1b[<65;10;5M") // wheel down
	s.waitFor("the pane to come back", func() bool { return s.shows("row 39") })

	s.send("\x1b[<0;10;5M") // a left-button press
	s.send("\x1b[<0;10;5m") // and its release
	s.send("!")
	s.waitFor("the typed character", func() bool {
		f := s.shot()
		if !f.hasRow("> typed!") {
			return false
		}
		if f.shows("10;5") {
			t.Errorf("the mouse report was typed into the prompt:\n%s", f.text)
		}
		return true
	})
}

// The same key with a modifier is a different binding, and the pane has to win:
// a plain Up recalls the last line, a shifted one scrolls.
func TestShiftUpScrollsOneRowRatherThanBrowsingHistory(t *testing.T) {
	var app *App
	s := newShell(t, Config{Submit: func(_ context.Context, line string) error {
		app.Printf("ran %s\n", line)
		return nil
	}})
	app = s.app
	s.start()

	// A history entry to be recalled by mistake.
	s.send("remember me\r")
	s.waitFor("the turn to finish", func() bool {
		f := s.shot()
		return f.shows("ran remember me") && f.caretShown()
	})
	for i := 0; i < 40; i++ {
		s.app.Printf("row %02d\n", i)
	}
	s.waitFor("the output", func() bool { return s.shows("row 39") })

	s.send("\x1b[1;2A") // shift-up
	s.waitFor("one row of scroll", func() bool {
		f := s.shot()
		if f.shows("row 39") {
			return false
		}
		if !f.hasRow("> ") {
			t.Errorf("shift-up put something in the composer:\n%s", f.text)
		}
		return true
	})

	// And a plain Up is still history, so the modifier is what made the
	// difference rather than the pane having eaten the key altogether.
	s.send("\x1b[A")
	s.waitFor("history", func() bool { return s.hasRow("> remember me") })
}

// ---------------------------------------------------------------------------
// Resize and shutdown
// ---------------------------------------------------------------------------

// FrameBytes writes h-1 line breaks, so counting them is the one assertion that
// says the whole frame was rebuilt for the new size rather than the pane alone.
func TestAResizeIsPickedUpFromTheScreen(t *testing.T) {
	s := newShell(t, Config{Title: "stage 12"})
	s.start()

	rows := func() int { return strings.Count(s.scr.last(), "\r\n") + 1 }
	s.waitFor("a frame the height of the screen", func() bool { return rows() == 12 })

	s.scr.resizeTo(40, 6)
	s.waitFor("a frame the shape of the new screen", func() bool {
		f := s.shot()
		if len(f.rows) != 6 {
			return false
		}
		for i, r := range f.rows {
			if w := term.DispWidth(r); w > 40 {
				t.Errorf("row %d is %d columns wide after the resize to 40: %q", i, w, r)
			}
		}
		return true
	})

	s.scr.resizeTo(100, 20)
	s.waitFor("a frame the height of the wider screen", func() bool { return rows() == 20 })
}

// A command's error arrives on the done channel rather than from a key handler,
// so the exit sentinel has to be recognised there too — without that branch the
// command prints "exit" in red and the shell carries on.
func TestTheExitCommandEndsTheLoopRatherThanPrintingItsSentinel(t *testing.T) {
	for _, name := range []string{"/exit", "/quit"} {
		t.Run(name, func(t *testing.T) {
			s := newShell(t, Config{})
			s.start()

			s.send(name + "\r")
			if err := s.waitExit(); err != nil {
				t.Errorf("%s returned %v, expected a clean exit", name, err)
			}
			for i, f := range s.scr.all() {
				if strings.Contains(f, "  exit\n") || strings.Contains(f, "  exit"+term.ClearLine) {
					t.Errorf("frame %d printed the sentinel as an error:\n%s", i, strings.Join(rowsOf(f), "\n"))
				}
			}
		})
	}
}

// A host command may end the session too — /open on a directory that turns out
// to be gone would rather stop than continue against a path that is not there.
func TestAHostCommandCanEndTheSessionWithTheSameSentinel(t *testing.T) {
	s := newShell(t, Config{Commands: []Command{{
		Name: "/bail",
		Run:  func(context.Context, string, io.Writer) error { return fmt.Errorf("gone: %w", ErrExit) },
	}}})
	s.start()

	s.send("/bail\r")
	if err := s.waitExit(); err != nil {
		t.Errorf("a wrapped ErrExit returned %v, expected a clean exit", err)
	}
}

// Returning immediately would leave a goroutine writing into the pane while Run
// reprints it and the host prints its summary, so the transcript ends in three
// interleaved half-lines.
func TestCancellingTheContextStopsARunningTurnBeforeTheLoopReturns(t *testing.T) {
	entered := make(chan struct{}, 1)
	cause := make(chan error, 1)
	s := newShell(t, Config{
		InterruptCause: errHostInterrupt,
		Submit: func(ctx context.Context, line string) error {
			entered <- struct{}{}
			<-ctx.Done()
			cause <- context.Cause(ctx)
			return ctx.Err()
		},
	})
	s.start()

	s.send("go\r")
	recv(t, entered, "Submit to be called")

	s.cancel()
	if err := s.waitExit(); err != nil {
		t.Errorf("the loop returned %v, expected nil", err)
	}
	// A shutdown is not an interrupt, so the configured InterruptCause is not
	// what the turn is stopped with here.
	if got := recv(t, cause, "the turn to be cancelled"); !errors.Is(got, context.Canceled) {
		t.Errorf("the turn was stopped with %v, expected context.Canceled", got)
	}
}

// Not an error: it is what happens when the shell is run with its input
// redirected from a file that ran out.
func TestClosingTheKeyChannelEndsTheLoopWithoutAnError(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	close(s.keys)
	if err := s.waitExit(); err != nil {
		t.Errorf("the loop returned %v when stdin closed, expected nil", err)
	}
}

func TestCancellingTheContextEndsTheLoopWithoutAnError(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.cancel()
	if err := s.waitExit(); err != nil {
		t.Errorf("the loop returned %v when its context was cancelled, expected nil", err)
	}
}

// Bytes the decoder cannot interpret even as final have to be dropped, because
// keeping them would re-arm the escape timer forever on the same buffer.
func TestAnUndecodableSequenceIsDroppedRatherThanLoopingForever(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("\x1b[200~never terminated") // an unterminated bracketed paste
	s.send("typed into the same buffer")

	if !s.running() {
		t.Fatal("the loop exited on an unterminated paste")
	}
	// decodePaste refuses to resolve even as final, so the whole buffer is
	// dropped when the escape timer expires — and anything typed before that goes
	// with it. The claim being pinned is only that the loop comes back, not that
	// those keys survive.
	s.keepSending("z", "the composer to take a key again", func() bool { return s.shot().shows("> z") })
	if s.shows("never terminated") {
		t.Errorf("the payload of an unterminated paste was typed into the prompt:\n%s", s.screen())
	}
}

// ---------------------------------------------------------------------------
// Layout, without a loop
// ---------------------------------------------------------------------------

// frame() is loop-owned state, so these drive it directly on an App that is not
// running. No goroutine, no polling, and the geometry is exact.

// Output grows up from the composer the way it does in a terminal, instead of
// hanging from the top of an empty screen.
func TestThePaneIsPaddedAboveTheOutputRatherThanBelowIt(t *testing.T) {
	a := newTestApp(t, Config{Title: "stage 12"})
	a.setSize(40, 12)
	a.Printf("the only line\n")

	lines, _, _, _ := a.frame()
	if len(lines) != 12 {
		t.Fatalf("frame produced %d lines for a 12-row window:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// 12 rows = 7 of pane + the composer's two border rows + the composer +
	// the status row + the hint.
	if lines[6] != "the only line" {
		t.Errorf("the line is at row %d of the pane, expected the bottom one:\n%s", 6, strings.Join(lines, "\n"))
	}
	for i := 0; i < 6; i++ {
		if lines[i] != "" {
			t.Errorf("row %d is %q, expected the padding above the output to be blank", i, lines[i])
		}
	}
}

// A short window gives the chrome up in order: the hint, then the composer's
// border, then the status row.
//
// The border goes before the status row because it costs two rows to say
// something a five-row window makes obvious anyway.
func TestOnAShortWindowTheHintGoesFirstThenTheBorderThenTheStatusRow(t *testing.T) {
	tall := newTestApp(t, Config{Title: "stage 12"})
	tall.setSize(40, 12)
	all := strings.Join(mustFrame(t, tall), "\n")
	for _, want := range []string{"enter send", "stage 12", "╭", "╰"} {
		if !strings.Contains(all, want) {
			t.Fatalf("a 12-row window is missing %q from the chrome:\n%s", want, all)
		}
	}

	short := newTestApp(t, Config{Title: "stage 12"})
	short.setSize(40, 3)
	out := mustFrame(t, short)
	got := strings.Join(out, "\n")
	if strings.Contains(got, "enter send") {
		t.Errorf("a 3-row window still draws the hint row:\n%s", got)
	}
	if strings.Contains(got, "╭") {
		t.Errorf("a 3-row window still draws the composer's border:\n%s", got)
	}
	if !strings.Contains(got, "stage 12") {
		t.Errorf("a 3-row window dropped the status row before the border:\n%s", got)
	}
	if !hasPrefixRow(out, "> ") {
		t.Errorf("a 3-row window dropped the composer:\n%s", got)
	}

	// Two rows of composer on a three-row window, and the status row has to go
	// too.
	tiny := newTestApp(t, Config{Title: "stage 12"})
	tiny.setSize(40, 3)
	tiny.ed.insert("one\ntwo")
	out = mustFrame(t, tiny)
	got = strings.Join(out, "\n")
	if strings.Contains(got, "stage 12") || strings.Contains(got, "enter send") {
		t.Errorf("with a two-row composer on a three-row window the chrome was kept:\n%s", got)
	}
	if !hasPrefixRow(out, "> one") {
		t.Errorf("the composer itself was dropped:\n%s", got)
	}
}

func mustFrame(t *testing.T, a *App) []string {
	t.Helper()
	lines, cr, cc, _ := a.frame()
	if cr < 0 || cr >= len(lines) {
		t.Errorf("frame put the caret on row %d of %d lines", cr, len(lines))
	}
	if cc < 0 {
		t.Errorf("frame put the caret at column %d", cc)
	}
	return lines
}

func hasPrefixRow(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(unbox(l), prefix) {
			return true
		}
	}
	return false
}

// The row applies its own colour, so the fields have to be plain text, and the
// title is the field that must survive a narrow window.
func TestTheStatusRowKeepsTheTitleAndDropsTheHostsFieldsFromTheRight(t *testing.T) {
	a := newTestApp(t, Config{
		Title: "stage 12",
		Segments: func() []Segment {
			return []Segment{{Value: "openai"}, {Value: "gpt-4o"}, {Label: "ctx", Value: "12345 tokens"}}
		},
	})

	a.setSize(60, 12)
	wide := a.statusRow(60)
	for _, want := range []string{"stage 12", "openai", "gpt-4o"} {
		if !strings.Contains(wide, want) {
			t.Errorf("a 60-column status row leaves out %q: %q", want, wide)
		}
	}
	if w := term.DispWidth(wide); w > 60 {
		t.Errorf("the status row is %d columns wide, expected at most 60: %q", w, wide)
	}

	narrow := a.statusRow(14)
	if !strings.Contains(narrow, "stage 12") {
		t.Errorf("a 14-column status row dropped the title: %q", narrow)
	}
	if strings.Contains(narrow, "12345") {
		t.Errorf("a 14-column status row kept the token count: %q", narrow)
	}
}

// The pane's own state lives on the composer's top border rather than in the
// status row, so this is where the scroll position has to show up.
func TestTheComposerBorderReportsTheScrollPositionAndWhatIsFolded(t *testing.T) {
	a := newTestApp(t, Config{Title: "stage 12"})
	a.setSize(60, 12)

	if tag := a.boxTag(100, 40); !strings.Contains(tag, "60/100") {
		t.Errorf("the border does not report the scroll position: %q", tag)
	}
	if tag := a.boxTag(100, 0); tag != "" {
		t.Errorf("an unscrolled pane should say nothing: %q", tag)
	}

	a.back.setClass(ClassDetail)
	for i := 0; i < 3; i++ {
		a.back.Write([]byte("panel\n"))
	}
	if tag := a.boxTag(100, 0); !strings.Contains(tag, "3") {
		t.Errorf("the border does not report the folded lines: %q", tag)
	}
	a.back.setDetail(true)
	if tag := a.boxTag(100, 0); tag != "" {
		t.Errorf("nothing is folded once the full view is on: %q", tag)
	}

	// The rule must be exactly as wide as the window whether it carries a tag
	// or not: it is drawn straight into a fixed-width frame.
	for _, tag := range []string{"", "60/100 ↓", "60/100 ↓ · ⋯12"} {
		if got := term.DispWidth(rule('╭', '╮', 60, tag)); got != 60 {
			t.Errorf("rule(%q) is %d columns, expected 60", tag, got)
		}
	}
}

func TestTheHintRowShowsANoteAheadOfTheDefaultAdvice(t *testing.T) {
	a := newTestApp(t, Config{})
	a.setSize(80, 12)

	if got := a.hintRow(80); !strings.Contains(got, "enter send") {
		t.Errorf("the idle hint is %q, expected the key map", got)
	}
	a.setNote("something happened", false)
	if got := a.hintRow(80); !strings.Contains(got, "something happened") {
		t.Errorf("the hint row is %q, expected the note", got)
	}
	a.state = stAsking
	if got := a.hintRow(80); !strings.Contains(got, "something happened") {
		t.Errorf("the note lost to the state's own hint: %q", got)
	}
	a.setNote("", false)
	if got := a.hintRow(80); !strings.Contains(got, "esc = n") {
		t.Errorf("the asking hint is %q, expected the answers", got)
	}
	a.state = stRunning
	a.what = "turn"
	if got := a.hintRow(80); !strings.Contains(got, "working") || !strings.Contains(got, "esc interrupt") {
		t.Errorf("the running hint is %q, expected the elapsed clock and the interrupt key", got)
	}
	a.what = "/compact"
	if got := a.hintRow(80); !strings.Contains(got, "/compact") {
		t.Errorf("the running hint is %q, expected it to name the command", got)
	}
}

// A window narrower than the chrome must still produce exactly h rows of at most
// w columns, because FrameBytes writes whatever it is handed.
func TestEveryFrameFitsTheWindowItWasBuiltFor(t *testing.T) {
	newApp := func(typed string) *App {
		a := newTestApp(t, Config{
			Title:    "stage 12",
			Segments: func() []Segment { return []Segment{{Value: "openai"}, {Value: "gpt-4o"}} },
		})
		a.Printf("%s\n", strings.Repeat("wide output ", 40))
		a.ed.insert(typed)
		return a
	}

	// A one-row composer fits any window, including the ones below the floors.
	empty := newApp("")
	for _, wh := range [][2]int{{8, 3}, {12, 4}, {40, 12}, {200, 50}, {1, 1}, {0, 0}} {
		empty.setSize(wh[0], wh[1])
		lines, _, _, _ := empty.frame()
		w, h := max(wh[0], 8), max(wh[1], 3)
		if len(lines) != h {
			t.Errorf("at %dx%d frame produced %d lines, expected %d", wh[0], wh[1], len(lines), h)
		}
		for i, l := range lines {
			if term.DispWidth(l) > w {
				t.Errorf("at %dx%d row %d is %d columns wide: %q", wh[0], wh[1], i, term.DispWidth(l), l)
			}
		}
	}

	// And a composer that wraps, on windows with room for it.
	typed := newApp("a typed line that is also rather long, long enough to wrap more than once")
	for _, wh := range [][2]int{{20, 20}, {40, 12}, {200, 50}} {
		typed.setSize(wh[0], wh[1])
		lines, cr, _, _ := typed.frame()
		if len(lines) != wh[1] {
			t.Errorf("at %dx%d frame produced %d lines, expected %d", wh[0], wh[1], len(lines), wh[1])
		}
		if cr < 0 || cr >= len(lines) {
			t.Errorf("at %dx%d the caret is on row %d of %d", wh[0], wh[1], cr, len(lines))
		}
		for i, l := range lines {
			if term.DispWidth(l) > wh[0] {
				t.Errorf("at %dx%d row %d is %d columns wide: %q", wh[0], wh[1], i, term.DispWidth(l), l)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The plumbing
// ---------------------------------------------------------------------------

func TestNewFillsInTheDefaultsAndRegistersTheBuiltins(t *testing.T) {
	out := notATerminal(t)
	a := New(Config{Out: out})

	if a.cfg.In != os.Stdin {
		t.Error("In defaulted to something other than os.Stdin")
	}
	if a.cfg.MaxScrollback != 5000 {
		t.Errorf("MaxScrollback defaulted to %d, expected 5000", a.cfg.MaxScrollback)
	}
	if w, h := a.width(), a.height(); w != 80 || h != 24 {
		t.Errorf("the window starts at %dx%d, expected 80x24 so the first frame is not built for a zero-size screen", w, h)
	}
	for _, want := range []string{"/help", "/keys", "/clear", "/exit", "/quit", "/status"} {
		if c, _ := a.reg.find(want); c.Name != want {
			t.Errorf("%s is not registered", want)
		}
	}
	// /open and the provider commands are removed rather than left to fail, so a
	// build that cannot do them does not advertise them.
	for _, gone := range []string{"/open", "/provider-apikey", "/settings"} {
		if c, _ := a.reg.find(gone); c.Name != "" {
			t.Errorf("%s is registered with no Config field to back it", gone)
		}
	}

	withOpen := New(Config{Out: out, Open: func(string) (string, error) { return "", nil }})
	if c, _ := withOpen.reg.find("/open"); c.Name != "/open" {
		t.Error("/open is missing even though Config.Open is set")
	}
}

// The host lays its own reports out to the current width, and a report formatted
// for whatever the window was at startup is exactly what shows when the window
// is resized while a command runs.
func TestWidthAndHeightReportTheLiveWindowToTheHost(t *testing.T) {
	a := newTestApp(t, Config{})
	if w, h := a.Width(), a.Height(); w != 80 || h != 24 {
		t.Errorf("Width/Height report %dx%d before the first frame, expected 80x24", w, h)
	}
	a.setSize(133, 41)
	if w, h := a.Width(), a.Height(); w != 133 || h != 41 {
		t.Errorf("Width/Height report %dx%d after a resize to 133x41", w, h)
	}
}

func TestOutAndPrintfBothLandInThePaneAndWakeTheLoop(t *testing.T) {
	a := newTestApp(t, Config{})

	if _, err := io.WriteString(a.Out(), "from the renderer\n"); err != nil {
		t.Fatal(err)
	}
	a.Printf("from %s\n", "Printf")

	rows, _, _ := a.back.view(60, 5, 0)
	if strings.Join(rows, "|") != "from the renderer|from Printf" {
		t.Errorf("the pane holds %q, expected both writes", rows)
	}
	if len(a.dirty) != 1 {
		t.Errorf("the dirty channel holds %d pokes, expected one coalesced wake-up", len(a.dirty))
	}
}

// For a tool whose reason to exist is reading what the agent did, losing the
// transcript at exit is the wrong trade.
func TestTheTranscriptIsReprintedOnTheWayOutAndSaysWhatItLeftOut(t *testing.T) {
	a := newTestApp(t, Config{})
	for i := 0; i < transcriptDump+50; i++ {
		a.Printf("line %04d\n", i)
	}
	a.Printf("a partial tail")

	var out strings.Builder
	a.dumpTranscript(&out)
	got := out.String()

	if !strings.Contains(got, "a partial tail") {
		t.Error("the unterminated last line was left out of the transcript")
	}
	if !strings.Contains(got, fmt.Sprintf("line %04d", transcriptDump+49)) {
		t.Error("the newest line was left out of the transcript")
	}
	if strings.Contains(got, "line 0000") {
		t.Error("the transcript is not capped")
	}
	if !strings.Contains(got, "earlier lines are not shown") {
		t.Errorf("the transcript does not say that it was cut:\n%s", got[:min(len(got), 200)])
	}

	// Nothing written, nothing printed: an exit banner on an empty session is
	// noise.
	var empty strings.Builder
	newTestApp(t, Config{}).dumpTranscript(&empty)
	if empty.Len() != 0 {
		t.Errorf("an empty pane printed %q on the way out", empty.String())
	}
}

func TestExitErrTurnsTheSentinelIntoACleanReturnAndPassesEverythingElseOn(t *testing.T) {
	if err := exitErr(ErrExit); err != nil {
		t.Errorf("exitErr(ErrExit) = %v, expected nil", err)
	}
	if err := exitErr(fmt.Errorf("wrapped: %w", ErrExit)); err != nil {
		t.Errorf("exitErr on a wrapped ErrExit = %v, expected nil", err)
	}
	boom := errors.New("boom")
	if err := exitErr(boom); !errors.Is(err, boom) {
		t.Errorf("exitErr(boom) = %v, expected it passed through", err)
	}
	if err := exitErr(nil); err != nil {
		t.Errorf("exitErr(nil) = %v, expected nil", err)
	}
}

func TestElapsedReadsAsAClockAtEveryScale(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0.0s"},
		{1500 * time.Millisecond, "1.5s"},
		{59900 * time.Millisecond, "59.9s"},
		{time.Minute, "1m00s"},
		{91 * time.Second, "1m31s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour, "1h00m"},
		{90 * time.Minute, "1h30m"},
	} {
		if got := elapsed(tc.d); got != tc.want {
			t.Errorf("elapsed(%v) = %q, expected %q", tc.d, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Race bait
// ---------------------------------------------------------------------------

// The host's renderer streams from its own goroutine while the loop repaints and
// a command reads the window size. This test asserts almost nothing; it is here
// for -race.
func TestThePaneCanBeWrittenFromEveryDirectionAtOnce(t *testing.T) {
	s := newShell(t, Config{
		Title:    "stage 12",
		Segments: func() []Segment { return []Segment{{Value: "openai"}} },
		Commands: []Command{{
			Name: "/report",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				for _, l := range RenderRows([]Row{{Name: "width", Value: fmt.Sprint(80)}}, 60) {
					fmt.Fprintln(w, l)
				}
				return nil
			},
		}},
	})
	s.start()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.app.Printf("goroutine %d line %d\n", g, i)
			}
		}(g)
	}
	for i := 0; i < 20; i++ {
		s.scr.resizeTo(40+i, 8+i%6)
	}
	wg.Wait()

	// After the flood, so the command's own output is the newest thing in the
	// pane rather than something that has already scrolled away.
	s.scr.resizeTo(60, 20)
	s.send("/report\r")
	s.waitFor("the command's output", func() bool { return s.shows("width") })
	if !s.running() {
		t.Fatalf("the loop exited: %v", s.err)
	}
}
