package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"bash-is-all-you-need/external/tui/term"
)

// DoubleClicked reports whether this process owns the window it is printing to.
//
// The question it answers is not cosmetic. When a binary is started from
// Explorer, Windows gives it a brand new console and destroys that console the
// moment the process returns — so an error message printed on the way out is
// displayed for a few microseconds and then the window is gone. Every report of
// "it just flashes and closes" is this.
//
// On Unix the process that owns the terminal is the shell that launched us, so
// nothing disappears and this is always false.
func DoubleClicked() bool { return term.OwnConsole() }

// HoldOpen waits for a keypress, but only when leaving would take the window
// with it.
//
// Called on every path out of main, including the fatal ones. On a terminal the
// user opened themselves it does nothing at all, because pausing there would
// make the binary unusable in a script.
func HoldOpen() { holdOpen(os.Stderr, os.Stdin, DoubleClicked()) }

func holdOpen(out io.Writer, in io.Reader, own bool) {
	if !own {
		return
	}
	fmt.Fprint(out, "\n[press Enter to close this window] ")
	// A line read rather than a single raw keypress: this runs on the way out,
	// possibly after a failure that had nothing to do with the terminal, and
	// putting the terminal into raw mode at that point is one more thing that
	// can fail while trying to report a failure.
	bufio.NewReader(in).ReadString('\n')
}

// Die reports a fatal error and exits, holding the window open first.
//
// It exists so that the fifteen `fmt.Fprintln(os.Stderr, err); os.Exit(1)` pairs
// a program like this accumulates cannot each be the one that forgets, which is
// all it takes for the failure to become invisible again.
func Die(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
	HoldOpen()
	os.Exit(1)
}
