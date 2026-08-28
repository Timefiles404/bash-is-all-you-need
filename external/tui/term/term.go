// Package term is the terminal layer for this repo's interactive shell: raw
// mode, one-frame-per-repaint output, key decoding, and display width.
//
// It is a lift of stages/06-the-composer's term.go, keys.go and width.go. The
// stage copies are the teaching version — they carry the essays on the
// escape-timing ambiguity, the restore contract and the three ways to count a
// string — and they stay where they are. This copy exists so the shell that
// ships is not part of the lesson. The two are behaviour-identical by contract:
// a behaviour change here must be mirrored in the stage, or the chapter stops
// being true.
//
// One rule the whole package rests on: once raw mode is entered, os.Exit and
// log.Fatal are bugs. They skip deferred functions, so they leave the user at a
// shell with no echo and no cursor, and the error they print is invisible under
// the alternate screen.
package term

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// Escape sequences the caller emits itself.
const (
	AltScreenOn  = "\x1b[?1049h"
	AltScreenOff = "\x1b[?1049l"
	CursorHide   = "\x1b[?25l"
	CursorShow   = "\x1b[?25h"
	ClearLine    = "\x1b[K"
	CursorHome   = "\x1b[H"
)

// Paired so the enable and disable lists can be read against each other: the
// commonest way a terminal is left broken is a mode enabled in one place and
// disabled in another that does not run.
const (
	// 1006 is the one that matters: the X10 encoding packs the coordinate into one
	// byte as 32 + column, which breaks past column 223 — on a wide terminal, the
	// right-hand half of the screen.
	mouseOn  = "\x1b[?1000h\x1b[?1006h"
	mouseOff = "\x1b[?1006l\x1b[?1000l"

	// Without bracketed paste a pasted paragraph is delivered as if typed, which
	// in a keyboard-driven UI means each character runs a command.
	pasteOn  = "\x1b[?2004h"
	pasteOff = "\x1b[?2004l"

	// Synchronised output. Terminals that do not know these ignore them, which is
	// why they are safe to send unconditionally.
	syncOn  = "\x1b[?2026h"
	syncOff = "\x1b[?2026l"
)

// Terminal owns the mutated global state and knows how to give it back.
type Terminal struct {
	in    *os.File
	out   *os.File
	saved *savedState // platform-specific; see term_unix.go / term_windows.go

	resize     <-chan struct{}
	stopResize func()

	// mu guards closed. Close is reachable from the defer, the signal handler and
	// the panic path at once, and an unguarded flag lets two of them past the
	// check — restoring twice and calling leaveRaw on a terminal that has moved
	// on. Only `go test -race` finds this: it needs a signal during shutdown.
	mu     sync.Mutex
	closed bool
}

// Open takes raw mode and turns on everything the UI needs. Alternate screen
// first, and Close's order is the exact reverse, so a failure after it prints on
// a screen the user is about to get back rather than over their scrollback.
func Open(in, out *os.File) (*Terminal, error) {
	saved, err := enterRaw(in, out)
	if err != nil {
		return nil, fmt.Errorf("could not enter raw mode: %w (is stdin a terminal?)", err)
	}
	t := &Terminal{in: in, out: out, saved: saved}
	io.WriteString(out, AltScreenOn+CursorHide+mouseOn+pasteOn)
	t.resize, t.stopResize = watchResize(out)
	return t, nil
}

// Close restores everything, and is safe to call more than once.
//
// Idempotence is load-bearing: a second restore re-enables the cursor on a
// terminal that has already moved on, and a second ioctl on a closed fd is an
// error nobody will ever read.
func (t *Terminal) Close() error {
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
	io.WriteString(t.out, pasteOff+mouseOff+CursorShow+AltScreenOff)
	return leaveRaw(t.in, t.out, t.saved)
}

// Size returns the current width and height in cells.
//
// 80x24 is the fallback because a guess that is too small is merely cramped,
// while one that is too large wraps every line and corrupts the frame.
func (t *Terminal) Size() (int, int) {
	w, h, err := termSize(t.out)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// Resize delivers a token every time the window changes size. It coalesces:
// dragging a window edge produces one notification per pixel-row of movement and
// they all mean the same thing.
func (t *Terminal) Resize() <-chan struct{} { return t.resize }

// Write writes straight to the out file, unframed, for a caller that needs to
// emit its own sequences.
func (t *Terminal) Write(p []byte) (int, error) {
	return t.out.Write(p)
}

// Frame writes one complete frame.
func (t *Terminal) Frame(lines []string, w, h int) {
	io.WriteString(t.out, FrameBytes(lines, w, h))
}

// FrameBytes builds one complete frame. Split out of Frame so it can be tested
// without a terminal — the escape sequences are the part that goes wrong.
//
// It deliberately does not clear the screen: a \x1b[2J before every frame is the
// classic flicker, because for one refresh the terminal has nothing on it. Homing
// and erasing each line as it is rewritten leaves every cell either overwritten
// or explicitly cleared. One buffer and one write, wrapped in the
// synchronised-output markers; line-by-line writes are a visible sweep down the
// screen rather than a repaint.
func FrameBytes(lines []string, w, h int) string {
	var b strings.Builder
	b.Grow(w*h + 64)
	b.WriteString(syncOn)
	b.WriteString(CursorHome)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			// TruncCols, not slicing: a line that overflows by one column wraps,
			// which pushes every line below it down by one and turns a cosmetic bug
			// into a corrupted frame.
			b.WriteString(TruncCols(lines[i], w))
		}
		b.WriteString(ClearLine)
		if i < h-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString(syncOff)
	return b.String()
}

// ReadLoop pumps stdin into a channel.
//
// A goroutine rather than a read timeout (VMIN/VTIME, or an overlapped read on
// Windows) because the event loop has to wait on input, resize and the Escape
// timer at once, and select over channels is the only form of that which is the
// same on both platforms. It leaks by design at exit: there is no portable way to
// interrupt a blocking read on stdin, and a fake cancel would return while the
// read still held the fd.
func ReadLoop(in *os.File) <-chan []byte {
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

// With runs fn with the terminal set up, and restores it however fn ends: normal
// return, error, panic, or SIGINT/SIGTERM.
//
// The caller prints fn's error AFTER the restore, on the user's real screen. A
// panic is re-raised after the restore: that costs a stack frame and buys a
// stack trace on a terminal that can display it.
//
// The signal handler re-raises rather than exiting, because a process killed by
// SIGTERM should report that — its parent may be a shell, a supervisor, or a
// test harness that distinguishes signal death from a non-zero exit.
func With(in, out *os.File, fn func(*Terminal) error) (err error) {
	t, err := Open(in, out)
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
		// If the re-raise did not take — it cannot on Windows, where these signals
		// are synthesised rather than delivered — fall back to an exit code, but
		// only once the terminal is already restored.
		os.Exit(130)
	}()

	defer func() {
		signal.Stop(sigs)
		close(sigs)
		t.Close()
		if r := recover(); r != nil {
			panic(r)
		}
	}()

	return fn(t)
}
