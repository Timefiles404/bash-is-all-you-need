//go:build windows

// Raw mode, Windows edition.
//
// Same contract as term_unix.go, and almost nothing else in common. Three
// differences change the design rather than just the code:
//
//  1. There is no termios. There are two *console modes*, one for the input
//     handle and one for the output handle, and they are bit flags rather than
//     a struct — so "restore" means remembering two uint32s.
//
//  2. ANSI is opt-in on both ends. The input handle has to be told to encode
//     keys as escape sequences (ENABLE_VIRTUAL_TERMINAL_INPUT) and the output
//     handle has to be told to interpret them (ENABLE_VIRTUAL_TERMINAL_PROCESSING).
//     Without the second, a TUI prints its escape codes as text — which is the
//     single most common "my Go TUI is broken on Windows" report, and it is one
//     API call.
//
//  3. **There is no SIGWINCH.** Nothing tells you the window was resized. The
//     Win32 way is to read WINDOW_BUFFER_SIZE_EVENT records from the console
//     input queue — but ENABLE_VIRTUAL_TERMINAL_INPUT is what turns that queue
//     into a byte stream, and having asked for bytes you no longer get records.
//     So this file polls. It is not a shortcut; it is what you are left with
//     after choosing the VT path, and the VT path is the one that makes the
//     keyboard behave like every other platform.
package main

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

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

	// Input: strip the line discipline, add VT encoding.
	//
	// ENABLE_PROCESSED_INPUT is the ISIG of Windows — with it on, Ctrl-C raises
	// a console control event instead of arriving as a byte.
	newIn := oldIn
	newIn &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT
	newIn |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT

	// Quick Edit is the one nobody expects. It is on by default, it makes the
	// mouse select text instead of reaching the application, and turning it off
	// requires setting ENABLE_EXTENDED_FLAGS in the same call — clear
	// QUICK_EDIT without it and the console ignores you, silently. A mouse UI
	// that "does not receive clicks on Windows" is usually this and nothing
	// else.
	newIn |= windows.ENABLE_EXTENDED_FLAGS
	newIn &^= windows.ENABLE_QUICK_EDIT_MODE

	if err := windows.SetConsoleMode(inH, newIn); err != nil {
		return nil, err
	}

	// Output: interpret ANSI, and stop wrapping at the last column.
	//
	// DISABLE_NEWLINE_AUTO_RETURN matters for the bottom-right cell. Writing a
	// character there normally scrolls the screen by one line, which on an
	// alternate screen means the frame the UI just drew slides up out of place.
	newOut := oldOut | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING |
		windows.ENABLE_PROCESSED_OUTPUT | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(outH, newOut); err != nil {
		// The input handle is already changed. Put it back before returning, or
		// a failure here leaves the user with no echo and no explanation — the
		// exact outcome this whole file exists to prevent.
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

// termSize reads the console screen buffer info.
//
// Note that it uses the *window* rectangle, not the buffer Size. They are
// different on Windows in a way they never are on Unix: a console can have a
// 9,000-line scrollback buffer behind an 80x25 window, and Size describes the
// buffer. Measure the buffer and the UI draws 9,000 rows into a 25-row hole.
func termSize(out *os.File) (int, int, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(out.Fd()), &info); err != nil {
		return 0, 0, err
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	h := int(info.Window.Bottom-info.Window.Top) + 1
	return w, h, nil
}

// watchResize polls, because nothing will tell us.
//
// Four times a second: fast enough that a resize feels immediate, slow enough
// that an idle UI is not measurably running. The comparison against the last
// observed size is what keeps this edge-triggered like its Unix counterpart, so
// the event loop cannot tell the two apart — which is the point of the shared
// signature.
//
// The honest summary of the difference: on Unix a resize costs nothing until it
// happens; here it costs a syscall every 250ms forever. That is the price of
// the VT input path, and it is worth paying, because the alternative is a
// separate Windows key decoder and a different set of bugs on the platform
// where users are least likely to be able to diagnose them.
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
