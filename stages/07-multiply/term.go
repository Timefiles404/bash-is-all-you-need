// Stage 06 — the terminal, and the contract you take on when you enter raw mode.
//
// A TUI needs four things from the terminal, and every one of them is a
// *global mutation of a resource you do not own*:
//
//	raw mode          keys arrive as bytes instead of lines, and Ctrl-C stops
//	                  being a signal
//	alternate screen  the user's scrollback is set aside and handed back intact
//	mouse reporting   clicks and the wheel arrive as escape sequences
//	bracketed paste   pasted text arrives wrapped, so it is not read as keys
//
// Turning them on is four `printf`s. Turning them off is the entire problem,
// because the process that turned them on is the only thing in the world that
// knows how to turn them off — and if it dies without doing so, the user is
// left at a shell with no echo, no line editing, no cursor, and mouse
// selection broken. They will type `reset` if they know to. Most people close
// the window.
//
// So this file is stage 01's lesson pointed at a different resource. Stage 01
// asked "what happens to the child process when the agent dies". This one asks
// what happens to the *terminal*, and the answer has to be the same: it is
// restored on the normal path, on the error path, on panic, and on a signal.
//
// The rule that falls out of it is worth stating on its own, because it
// silently invalidates habits that are correct everywhere else:
//
//	**once you have entered raw mode, os.Exit and log.Fatal are bugs.**
//
// They skip deferred functions. A `log.Fatalf("bad config")` three layers down
// — the most ordinary line in Go — now leaves the user's terminal broken, and
// the error message it prints is invisible because the alternate screen is
// still up.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// Escape sequences. Grouped here rather than scattered so that the enable and
// disable lists can be read against each other — the commonest way a terminal
// is left broken is a mode that gets enabled in one place and disabled in
// another that does not run.
const (
	altScreenOn  = "\x1b[?1049h"
	altScreenOff = "\x1b[?1049l"
	cursorHide   = "\x1b[?25l"
	cursorShow   = "\x1b[?25h"

	// 1000 = report button press/release. 1006 = report them in SGR encoding.
	//
	// Both are needed and 1006 is the one that matters: the original X10
	// encoding packs the coordinate into a single byte as `32 + column`, which
	// stops working at column 223. On a modern wide terminal that is not an
	// edge case, it is the right-hand half of the screen.
	mouseOn  = "\x1b[?1000h\x1b[?1006h"
	mouseOff = "\x1b[?1006l\x1b[?1000l"

	// Bracketed paste. Without it, a pasted paragraph is delivered as if the
	// user typed every character, which in a keyboard-driven UI means each
	// character runs a command.
	pasteOn  = "\x1b[?2004h"
	pasteOff = "\x1b[?2004l"

	// Synchronised output: "do not paint until I say I am done".
	//
	// Terminals that do not know this sequence ignore it, which is the reason
	// it is safe to send unconditionally. Terminals that do know it stop
	// showing half-drawn frames — the visible tearing when a repaint lands
	// between two writes.
	syncOn  = "\x1b[?2026h"
	syncOff = "\x1b[?2026l"

	clearLine  = "\x1b[K"
	cursorHome = "\x1b[H"
)

// terminal owns the mutated state and knows how to give it back.
type terminal struct {
	in    *os.File
	out   *os.File
	saved *savedState // platform-specific; see term_unix.go / term_windows.go

	resize     <-chan struct{}
	stopResize func()

	// mu guards closed. Close is reachable from three places at once — the
	// defer, the signal handler, and the panic path — and an unguarded flag
	// would let two of them past the check, restoring twice and calling
	// leaveRaw on a terminal that has already moved on. `go test -race` finds
	// this; a human never does, because it needs a signal during shutdown.
	mu     sync.Mutex
	closed bool
}

// openTerminal takes raw mode and turns on everything the UI needs.
//
// The order is deliberate and its reverse is what Close does. Alternate screen
// first, so that if anything after it fails, the failure message is printed on
// a screen the user is about to get back rather than on top of their
// scrollback.
func openTerminal(in, out *os.File) (*terminal, error) {
	saved, err := enterRaw(in, out)
	if err != nil {
		return nil, fmt.Errorf("could not enter raw mode: %w (is stdin a terminal?)", err)
	}
	t := &terminal{in: in, out: out, saved: saved}
	io.WriteString(out, altScreenOn+cursorHide+mouseOn+pasteOn)
	t.resize, t.stopResize = watchResize(out)
	return t, nil
}

// Close restores everything, and is safe to call more than once.
//
// Idempotence is not decoration here. Close is called from a defer, from the
// signal handler, and from the panic path, and on a bad day from all three; a
// second restore that re-enables the cursor on a terminal that has already
// moved on is a visible glitch, and a second ioctl on a closed fd is an error
// nobody will ever read.
func (t *terminal) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	if t.stopResize != nil {
		t.stopResize()
	}
	// Exact reverse of the enable order.
	io.WriteString(t.out, pasteOff+mouseOff+cursorShow+altScreenOff)
	return leaveRaw(t.in, t.out, t.saved)
}

// Size returns the current width and height in cells, with a usable fallback.
//
// 80x24 is the fallback because a wrong size that is too small produces a UI
// that is merely cramped, while one that is too large produces a UI that wraps
// every line and corrupts the frame. When you must guess, guess in the
// direction whose failure is recoverable.
func (t *terminal) Size() (int, int) {
	w, h, err := termSize(t.out)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// Frame writes one complete frame.
//
// Two things it deliberately does not do. It does not clear the screen: a
// `\x1b[2J` before every frame is the classic cause of flicker, because for one
// refresh the terminal genuinely has nothing on it. Instead it homes the cursor
// and erases each line as it rewrites it, so every cell is either overwritten
// or explicitly cleared and no frame is ever blank.
//
// And it does not write line by line. One buffer, one syscall — wrapped in the
// synchronised-output markers — is the difference between a repaint and a
// visible sweep down the screen.
func (t *terminal) Frame(lines []string, w, h int) {
	io.WriteString(t.out, frameBytes(lines, w, h))
}

// frameBytes builds one complete frame. Split out of Frame so it can be tested
// without a terminal — the escape sequences are the part that goes wrong.
func frameBytes(lines []string, w, h int) string {
	var b strings.Builder
	b.Grow(w*h + 64)
	b.WriteString(syncOn)
	b.WriteString(cursorHome)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			// truncCols, not slicing: a line that overflows by one column wraps,
			// which pushes every line below it down by one and turns a
			// cosmetic bug into a corrupted frame.
			b.WriteString(truncCols(lines[i], w))
		}
		b.WriteString(clearLine)
		if i < h-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString(syncOff)
	return b.String()
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// readLoop pumps stdin into a channel.
//
// A goroutine rather than a read timeout (VMIN/VTIME on Unix, or an overlapped
// read on Windows) because the event loop has to wait on three things at once —
// input, resize, and the Escape timer — and `select` over channels is the only
// form of that which is the same on both platforms. The blocking read here is
// exactly the thing that makes the select possible elsewhere.
//
// It leaks by design when the program exits: there is no portable way to
// interrupt a blocking read on stdin, and a goroutine parked in a syscall at
// process exit costs nothing. Pretending otherwise would mean a fake cancel
// that returns while the read is still holding the fd.
func readLoop(in *os.File) <-chan []byte {
	ch := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				ch <- b
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// ---------------------------------------------------------------------------
// The restore contract
// ---------------------------------------------------------------------------

// withTerminal runs fn with the terminal set up, and restores it however fn
// ends.
//
// Four exits, and a real TUI meets all four:
//
//	fn returns          the defer runs
//	fn returns an error the defer runs, and the error is printed AFTER the
//	                    restore — on the user's real screen, where they can see
//	                    and copy it, rather than on an alternate screen that is
//	                    about to be discarded
//	fn panics           the defer runs, then the panic is re-raised, so the
//	                    stack trace lands on a terminal that can display it
//	SIGINT / SIGTERM    the handler restores and re-raises the signal
//
// The last one needs the re-raise rather than os.Exit(130). A process killed by
// SIGTERM should *report* that it was killed by SIGTERM — its parent may be a
// shell, a supervisor, or a test harness that distinguishes signal death from a
// non-zero exit. Resetting the handler to the default and re-sending the signal
// to yourself is how you clean up without lying about how you died.
func withTerminal(in, out *os.File, fn func(*terminal) error) (err error) {
	t, err := openTerminal(in, out)
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s, ok := <-sigs
		if !ok {
			return
		}
		t.Close()
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		if p, e := os.FindProcess(os.Getpid()); e == nil {
			_ = p.Signal(s)
		}
		// If the re-raise did not take (it cannot on Windows, where these
		// signals are synthesised rather than delivered), fall back to an exit
		// code — but only after the terminal is already restored.
		os.Exit(130)
	}()

	defer func() {
		signal.Stop(sigs)
		close(sigs)
		t.Close()
		if r := recover(); r != nil {
			// Re-panic after the restore. The stack trace names this line
			// rather than the original, which is a real cost; the alternative
			// is a correct stack trace printed onto an alternate screen that
			// disappears, which is worse.
			panic(r)
		}
	}()

	return fn(t)
}
