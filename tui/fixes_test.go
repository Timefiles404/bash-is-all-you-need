package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"bash-is-all-you-need/tui/term"
)

// Every test in this file pins a defect that shipped and was found by the suite
// next door rather than by using the program. They are kept together because
// what they have in common is worth more than where they belong: each one is a
// claim a comment was already making, in code that did not keep it.

// A question outlives the turn that asked it, and the shell has to survive that.
//
// The failure was not a display glitch. finish() reset the state to idle with a
// question still pending, so no keypress reached askKey, the answer went into
// the composer, and because the ask channel is nil while a question is pending,
// every later question in the session blocked until shutdown — one lost race and
// the permission gate is gone for good.
func TestAQuestionThatOutlivesItsTurnStillGetsAnswered(t *testing.T) {
	asked := make(chan struct{})
	answered := make(chan string, 1)

	// The turn fails on purpose, and the failure text is the signal.
	//
	// Everything else about "the turn has finished" is invisible from outside:
	// with the question still up, a correct shell draws exactly the same frame
	// before and after. finish() prints a turn's error into the pane, and it does
	// so after the state assignment being tested — so the text appearing is a
	// race-free proof that the assignment has already happened. Without it the
	// test answers the question before finish() runs and passes either way.
	const finished = "turn-finished-marker"

	var app *App
	s := newShell(t, Config{
		Submit: func(ctx context.Context, line string) error {
			// The question is asked by a goroutine that outlives Submit. In this
			// repo dispatch() joins its subagents, so the shape is not reachable
			// today; the state machine must not depend on that.
			go func() {
				ans, ok := app.Ask("run? [y/n] ")
				if ok {
					answered <- ans
				} else {
					answered <- "(closed)"
				}
			}()
			<-asked
			return errors.New(finished)
		},
	})
	app = s.app
	s.start()

	s.send("go\r")
	s.waitFor("the question to reach the screen", func() bool { return s.shows("run? [y/n]") })
	close(asked) // the turn now finishes while the question is still up
	s.waitFor("the turn to finish", func() bool { return s.shows(finished) })

	// The turn's completion must not take the question off screen.
	if !s.shows("run? [y/n]") {
		t.Fatalf("the question vanished when the turn finished; the frame was:\n%s", s.screen())
	}

	s.send("y\r")
	select {
	case got := <-answered:
		if got != "y" {
			t.Fatalf("Ask returned %q, want \"y\"", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Ask never returned after the answer was typed; the frame was:\n%s", s.screen())
	}

	// And the gate still works afterwards, which is the half that was lost.
	go func() {
		ans, ok := app.Ask("again? [y/n] ")
		if ok {
			answered <- ans
		} else {
			answered <- "(closed)"
		}
	}()
	s.waitFor("a second question", func() bool { return s.shows("again? [y/n]") })
	s.send("n\r")
	select {
	case got := <-answered:
		if got != "n" {
			t.Fatalf("the second Ask returned %q, want \"n\"", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second Ask never returned: the ask channel was left disabled")
	}
}

// Cancelling the session with a question on screen must not cost the drain
// timeout.
//
// Ask watches its reply channel and the shell's shutdown, never its own context,
// so a turn parked on a permission prompt cannot be cancelled. drain() waited
// for a turn that could not move and paid the full two seconds every time.
func TestCancellingWithAQuestionOnScreenReturnsImmediately(t *testing.T) {
	var app *App
	stopped := make(chan bool, 1)
	s := newShell(t, Config{
		Submit: func(ctx context.Context, line string) error {
			_, ok := app.Ask("run? [y/n] ")
			stopped <- ok
			return nil
		},
	})
	app = s.app
	s.start()

	s.send("go\r")
	s.waitFor("the question", func() bool { return s.shows("run? [y/n]") })

	start := time.Now()
	s.cancel()
	select {
	case <-s.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not return after its context was cancelled")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("shutdown took %v with a question on screen; drain() is waiting for a "+
			"turn that cannot move, which is the common path and not the pathological one", d)
	}
	if ok := <-stopped; ok {
		t.Error("Ask reported success while the shell was shutting down; the gate must deny, not allow")
	}
}

// A mouse report is not an answer to a note.
func TestScrollingDoesNotEraseTheNoteItWasSentToRead(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	s.send("\x03") // Ctrl-C on an empty prompt arms the quit and sets a note
	s.waitFor("the quit note", func() bool { return s.shows("press ctrl-c again") })

	// SGR wheel-up at row 1, column 1.
	s.send("\x1b[<64;1;1M")
	// Give the loop a frame to process it, then assert the note survived.
	before := len(s.scr.all())
	s.waitFor("a repaint after the wheel", func() bool { return len(s.scr.all()) > before })
	if !s.shows("press ctrl-c again") {
		t.Fatalf("the wheel erased the note; the frame was:\n%s", s.screen())
	}
}

// ---------------------------------------------------------------------------
// The scrollback's CRLF, split
// ---------------------------------------------------------------------------

// A line ending split across two writes is still a line ending.
//
// The look-ahead only ever saw the current chunk, so a CR at the end of one
// write was resolved immediately as a rewrite and the line was thrown away. The
// chunk boundary is wherever a network read landed, so nothing about this is
// exotic — and the comment above the code claimed it could not happen.
func TestALineEndingSplitAcrossTwoWritesIsStillOneLineEnding(t *testing.T) {
	one := newScrollback(64)
	one.Write([]byte("hello\r\n"))
	rowsA, _, _ := one.view(20, 4, 0)

	two := newScrollback(64)
	two.Write([]byte("hello\r"))
	two.Write([]byte("\n"))
	rowsB, _, _ := two.view(20, 4, 0)

	if strings.Join(rowsA, "|") != strings.Join(rowsB, "|") {
		t.Fatalf("one write gave %q, two writes gave %q — the same bytes must produce the same lines",
			rowsA, rowsB)
	}
	if len(rowsB) != 1 || rowsB[0] != "hello" {
		t.Fatalf("split CRLF produced %q, want one row \"hello\"", rowsB)
	}
}

// A carriage return at the end of a chunk still rewrites the line when what
// follows is not a newline. Holding the CR must not turn a rewrite into an
// append.
func TestACarriageReturnAtAChunkBoundaryStillRewritesTheLine(t *testing.T) {
	s := newScrollback(64)
	s.Write([]byte("50%\r"))
	// Before the next write the line is unchanged, which is also what a terminal
	// shows at this moment: the cursor has moved, nothing has been overwritten.
	if rows, _, _ := s.view(20, 4, 0); len(rows) != 1 || rows[0] != "50%" {
		t.Fatalf("after a trailing CR the pane showed %q, want [\"50%%\"]", rows)
	}
	s.Write([]byte("99%\n"))
	rows, _, _ := s.view(20, 4, 0)
	if len(rows) != 1 || rows[0] != "99%" {
		t.Fatalf("the rewrite produced %q, want one row \"99%%\"", rows)
	}
}

// A CR that is never followed by anything leaves the line readable.
func TestATrailingCarriageReturnAloneLosesNothing(t *testing.T) {
	s := newScrollback(64)
	s.Write([]byte("done\r"))
	rows, _, _ := s.view(20, 4, 0)
	if len(rows) != 1 || rows[0] != "done" {
		t.Fatalf("got %q, want [\"done\"] — the text is not overwritten until something overwrites it", rows)
	}
}

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

// A frame never claims more rows than the window has.
//
// maxInputRows caps the composer against itself and said nothing about the
// window, so a prompt wrapping past the window's height made frame() hand back
// more lines than FrameBytes would draw: the top of the composer was drawn
// instead of the rows around the caret, and the cursor escape pointed past the
// last row.
func TestAFrameNeverExceedsTheWindowEvenWhenTheComposerCannotFit(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{8, 3}, {8, 4}, {8, 5}, {12, 3}, {20, 3}, {40, 6}, {80, 24},
	} {
		a := New(Config{Out: notATerminal(t)})
		a.setSize(tc.w, tc.h)
		// Long enough to wrap well past any of these windows.
		a.ed.insert(strings.Repeat("abcdefgh ", 12))

		lines, cr, _, _ := a.frame()
		if len(lines) > tc.h {
			t.Errorf("%dx%d: frame returned %d lines for a %d-row window",
				tc.w, tc.h, len(lines), tc.h)
		}
		if cr >= tc.h {
			t.Errorf("%dx%d: the caret is on row %d of a %d-row window, so the cursor escape "+
				"points off the screen", tc.w, tc.h, cr, tc.h)
		}
	}
}

// The status bar never goes blank because the host left Title empty.
func TestANarrowBarFallsBackToTheFirstFieldThatHasSomethingInIt(t *testing.T) {
	got := segments([]string{"", "openai", "12.4k tok"}, 3, " · ")
	if got == "" {
		t.Fatal("segments returned an empty bar; the fallback exists to prevent exactly that")
	}
	if !strings.HasPrefix("openai", got) {
		t.Fatalf("segments returned %q, want a prefix of \"openai\"", got)
	}
	if term.DispWidth(got) > 3 {
		t.Fatalf("segments returned %q, which is wider than the 3 columns it was given", got)
	}
}
