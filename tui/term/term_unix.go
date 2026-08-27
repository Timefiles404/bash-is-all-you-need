//go:build !windows

package term

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// savedState is the terminal settings as they were before we touched them.
type savedState struct {
	termios unix.Termios
}

// enterRaw turns off every piece of line discipline between the keyboard and the
// program. The flags are cleared one at a time rather than by assigning a zeroed
// struct, because a zeroed struct also clears character size and parity.
func enterRaw(in, out *os.File) (*savedState, error) {
	fd := int(in.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	raw := *old

	// With ICRNL on, Enter arrives as \n and nothing distinguishes it from a
	// literal newline in a paste. Without clearing IXON, Ctrl-S freezes the
	// terminal and a user pressing it for "save" concludes the program hung.
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON

	// With OPOST on, a lone \n becomes \r\n, which breaks any frame that positions
	// the cursor by column.
	raw.Oflag &^= unix.OPOST

	// Clearing ISIG makes Ctrl-C arrive as the byte 0x03 instead of as SIGINT. It
	// is also the moment a bug in the key handler becomes a program the user
	// cannot quit.
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN

	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8

	// Block until at least one byte. ReadLoop runs on its own goroutine, and
	// VTIME's decisecond timer is too coarse for the Escape disambiguation anyway.
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

// termSize asks the kernel, not the environment. $COLUMNS and $LINES are set by
// the shell for itself and are stale the moment the window is resized, which is
// exactly the case they would be needed for.
func termSize(out *os.File) (int, int, error) {
	ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// watchResize delivers a token every time the window changes size. Capacity 1,
// dropping rather than blocking: dragging a window edge produces a SIGWINCH per
// pixel-row of movement, so queueing them redraws a hundred stale frames after
// the drag stopped.
//
// A channel rather than the signal itself, because Windows has no SIGWINCH and
// has to poll; the event loop cannot tell which one it got.
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
