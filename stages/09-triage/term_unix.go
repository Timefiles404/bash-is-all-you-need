//go:build !windows

// Raw mode, Unix edition.
//
// The pairing with term_windows.go is the same shape as stage 01's
// proc_unix.go / proc_windows.go: identical contract, completely different
// mechanism, and the difference is not an implementation detail — it changes
// what the program can be told.
//
// Here the kernel tells you the window changed, by sending SIGWINCH. On Windows
// nothing tells you and you have to ask. That single asymmetry is why
// watchResize returns a channel rather than exposing a signal: a channel can be
// fed by a signal handler or by a polling loop, and the event loop cannot tell
// which one it got.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// savedState is the terminal settings as they were before we touched them. The
// whole restore contract is this struct plus the discipline to always write it
// back.
type savedState struct {
	termios unix.Termios
}

// enterRaw turns off every piece of line discipline between the keyboard and
// the program.
//
// Each flag cleared below is a service the terminal was providing that a TUI
// has to provide for itself, and the reason they are cleared one at a time
// rather than by assigning a zeroed struct is that a zeroed struct also clears
// the ones that matter for correctness rather than for behaviour — character
// size, parity — and a terminal set to 5-bit characters is a fun afternoon.
func enterRaw(in, out *os.File) (*savedState, error) {
	fd := int(in.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	raw := *old

	// Input: stop translating. ICRNL is the one people trip over — with it on,
	// the Enter key arrives as \n and there is no way to tell it apart from a
	// literal newline in a paste. IXON is the other: without clearing it,
	// Ctrl-S freezes the terminal and Ctrl-Q unfreezes it, and a user who
	// presses Ctrl-S expecting "save" concludes the program has hung.
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON

	// Output: stop translating on the way out too. With OPOST on, a lone \n
	// becomes \r\n, which quietly breaks any frame that positions the cursor
	// by column.
	raw.Oflag &^= unix.OPOST

	// Local: no echo, no line buffering, and — the big one — no signals.
	// Clearing ISIG is what makes Ctrl-C arrive as the byte 0x03 instead of as
	// SIGINT. That is the behaviour a TUI wants, and it is also the moment the
	// program becomes responsible for having a way out: a bug in the key
	// handler after this line is a program the user cannot quit.
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN

	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8

	// Block until at least one byte. The event loop reads on its own goroutine
	// (see readLoop), so blocking there costs nothing, and a blocking read is
	// far easier to reason about than VTIME's decisecond timer — which is also
	// too coarse for the ~50ms Escape disambiguation this UI needs.
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return nil, err
	}
	return &savedState{termios: *old}, nil
}

func leaveRaw(in, out *os.File, s *savedState) error {
	if s == nil {
		return nil
	}
	t := s.termios
	return unix.IoctlSetTermios(int(in.Fd()), ioctlSetTermios, &t)
}

// termSize asks the kernel, not the environment.
//
// $COLUMNS and $LINES exist and are a trap: the shell sets them for itself and
// they are stale the moment the window is resized, which is precisely the case
// you need them for.
func termSize(out *os.File) (int, int, error) {
	ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// watchResize delivers a token every time the window changes size.
//
// The channel has capacity 1 and drops rather than blocks. Dragging a window
// edge produces a SIGWINCH per pixel-row of movement, and every one of them
// means the same thing — "the size is different now, go and ask what it is".
// Queueing them would make the UI redraw a hundred stale frames after the drag
// has stopped. Coalescing is not an optimisation here; it is the correct
// semantics for an edge-triggered notification about a value you have to read
// anyway.
func watchResize(out *os.File) (<-chan struct{}, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGWINCH)
	ch := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sigs:
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return ch, func() {
		signal.Stop(sigs)
		close(done)
	}
}
