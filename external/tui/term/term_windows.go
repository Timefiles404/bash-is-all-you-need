//go:build windows

package term

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// savedState is two console modes, one per handle. There is no termios here.
type savedState struct {
	in, out uint32
}

func enterRaw(in, out *os.File) (*savedState, error) {
	inH := windows.Handle(in.Fd())
	outH := windows.Handle(out.Fd())

	var oldIn, oldOut uint32
	if err := windows.GetConsoleMode(inH, &oldIn); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(outH, &oldOut); err != nil {
		return nil, err
	}

	// ENABLE_PROCESSED_INPUT is the ISIG of Windows: with it on, Ctrl-C raises a
	// console control event instead of arriving as a byte.
	newIn := oldIn
	newIn &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT
	newIn |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT

	// Quick Edit is on by default and makes the mouse select text instead of
	// reaching the application. Clearing it needs ENABLE_EXTENDED_FLAGS in the
	// same call, or the console ignores the request silently.
	newIn |= windows.ENABLE_EXTENDED_FLAGS
	newIn &^= windows.ENABLE_QUICK_EDIT_MODE

	if err := windows.SetConsoleMode(inH, newIn); err != nil {
		return nil, err
	}

	// Without ENABLE_VIRTUAL_TERMINAL_PROCESSING a TUI prints its escape codes as
	// text. DISABLE_NEWLINE_AUTO_RETURN is for the bottom-right cell: writing
	// there otherwise scrolls the screen and slides the frame out of place.
	newOut := oldOut | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING |
		windows.ENABLE_PROCESSED_OUTPUT | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(outH, newOut); err != nil {
		// The input handle is already changed. Put it back, or a failure here leaves
		// the user with no echo and no explanation.
		windows.SetConsoleMode(inH, oldIn)
		return nil, err
	}
	return &savedState{in: oldIn, out: oldOut}, nil
}

func leaveRaw(in, out *os.File, s *savedState) error {
	if s == nil {
		return nil
	}
	err := windows.SetConsoleMode(windows.Handle(in.Fd()), s.in)
	if err2 := windows.SetConsoleMode(windows.Handle(out.Fd()), s.out); err == nil {
		err = err2
	}
	return err
}

// termSize uses the *window* rectangle, not the buffer Size: a console can have
// a 9,000-line scrollback buffer behind an 80x25 window, and measuring the
// buffer draws 9,000 rows into a 25-row hole.
func termSize(out *os.File) (int, int, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(out.Fd()), &info); err != nil {
		return 0, 0, err
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	h := int(info.Window.Bottom-info.Window.Top) + 1
	return w, h, nil
}

// watchResize polls, because nothing will tell us: there is no SIGWINCH, and
// ENABLE_VIRTUAL_TERMINAL_INPUT is exactly what turns the console input queue
// into a byte stream, so the WINDOW_BUFFER_SIZE_EVENT records are gone.
// Comparing against the last size keeps this edge-triggered like the Unix
// counterpart, so the event loop cannot tell the two apart.
func watchResize(out *os.File) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		lastW, lastH, _ := termSize(out)
		for {
			select {
			case <-t.C:
				w, h, err := termSize(out)
				if err != nil || (w == lastW && h == lastH) {
					continue
				}
				lastW, lastH = w, h
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return ch, func() { close(done) }
}
