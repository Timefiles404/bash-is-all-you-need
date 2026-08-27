package term

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// frameRows checks the frame envelope and hands back the h row bodies with their
// trailing erase-line stripped.
func frameRows(t *testing.T, lines []string, w, h int) []string {
	t.Helper()
	got := FrameBytes(lines, w, h)

	if n := strings.Count(got, syncOn); n != 1 {
		t.Fatalf("the frame contains %d synchronised-output BEGIN markers, want 1 — without "+
			"exactly one wrapping the whole frame the terminal is free to paint a half-drawn "+
			"screen", n)
	}
	if n := strings.Count(got, syncOff); n != 1 {
		t.Fatalf("the frame contains %d synchronised-output END markers, want 1 — an unclosed "+
			"one leaves the terminal holding the next frame too", n)
	}
	if n := strings.Count(got, CursorHome); n != 1 {
		t.Fatalf("the frame homes the cursor %d times, want 1 — a frame that homes twice "+
			"overwrites its own first rows", n)
	}
	if !strings.HasPrefix(got, syncOn+CursorHome) {
		t.Fatalf("the frame does not begin with BEGIN-SYNC then cursor-home: %q", frameHead(got))
	}
	if !strings.HasSuffix(got, syncOff) {
		t.Fatalf("the frame does not end with END-SYNC: %q", frameTail(got))
	}

	body := strings.TrimSuffix(strings.TrimPrefix(got, syncOn+CursorHome), syncOff)
	rows := strings.Split(body, "\r\n")
	if len(rows) != h {
		t.Fatalf("the frame has %d rows and %d line separators, want %d of each. One row too "+
			"many and the terminal scrolls, which pushes the whole UI up by a line on every "+
			"single repaint", len(rows), len(rows)-1, h)
	}
	if n := strings.Count(got, ClearLine); n != h {
		t.Fatalf("the frame erases %d lines, want %d — a row that is not erased still shows "+
			"the tail of whatever the PREVIOUS frame put there", n, h)
	}
	for i, r := range rows {
		if !strings.HasSuffix(r, ClearLine) {
			t.Fatalf("row %d = %q does not end with the erase-line sequence", i, r)
		}
		rows[i] = strings.TrimSuffix(r, ClearLine)
	}
	return rows
}

func frameHead(s string) string {
	if len(s) > 24 {
		return s[:24]
	}
	return s
}

func frameTail(s string) string {
	if len(s) > 24 {
		return s[len(s)-24:]
	}
	return s
}

func TestFrameBytesShape(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		w, h  int
	}{
		{"exactly enough lines", []string{"a", "b", "c"}, 10, 3},
		{"one row", []string{"only"}, 10, 1},
		{"fewer lines than rows", []string{"only"}, 20, 5},
		{"no lines at all", nil, 20, 4},
		{"more lines than rows", []string{"a", "b", "c", "d", "e"}, 20, 2},
		{"a tall thin window", []string{"a", "b"}, 1, 40},
		{"an ultrawide window", []string{"a"}, 400, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := frameRows(t, c.lines, c.w, c.h)
			for i, r := range rows {
				if n := DispWidth(r); n > c.w {
					t.Errorf("row %d = %q is %d columns in a %d-column window. One column of "+
						"overflow wraps, which pushes every row below it down by one and turns "+
						"a cosmetic bug into a corrupted frame", i, r, n, c.w)
				}
				if i >= len(c.lines) && r != "" {
					t.Errorf("row %d = %q, want empty — there were only %d lines, and the "+
						"tail must be erased rather than left showing the previous frame",
						i, r, len(c.lines))
				}
			}
		})
	}
}

// TestFrameBytesTruncatesInColumnsNotBytes is the bug that corrupts a whole
// frame, and the reason FrameBytes calls TruncCols rather than slicing.
func TestFrameBytesTruncatesInColumnsNotBytes(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		w     int
		wantW int
	}{
		{"plain ASCII overflow", strings.Repeat("x", 200), 10, 10},
		{"CJK, even boundary", "你好世界你好世界", 8, 8},
		// 3 columns cannot hold two wide glyphs, so one fits and the orphan column
		// is a space. Byte slicing would cut 你 in half.
		{"CJK, odd boundary", "你好世界", 3, 3},
		{"CJK, nothing fits", "你好", 1, 1},
		{"mixed", "ab你好cd", 4, 4},
		{"coloured overflow", "\x1b[31m" + strings.Repeat("y", 50) + "\x1b[0m", 12, 12},
		{"a wide status line", "总计 4 · exit 0 · 4096B · TRUNCATED", 12, 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := frameRows(t, []string{c.line}, c.w, 1)
			got := rows[0]

			if n := DispWidth(got); n != c.wantW {
				t.Errorf("FrameBytes(%q, %d, 1) row = %q, %d columns, want exactly %d. A row "+
					"one column short shears the frame as badly as one column too long",
					c.line, c.w, got, n, c.wantW)
			}
			if !utf8.ValidString(got) {
				t.Errorf("FrameBytes(%q, %d, 1) row = %q is not valid UTF-8 — the cut landed "+
					"inside a multi-byte rune and half a character went to the terminal",
					c.line, c.w, got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("FrameBytes(%q, %d, 1) row = %q contains U+FFFD", c.line, c.w, got)
			}
			if got != TruncCols(c.line, c.w) {
				t.Errorf("FrameBytes row = %q but TruncCols(%q, %d) = %q — the frame builder "+
					"is not using the column-aware truncator", got, c.line, c.w, TruncCols(c.line, c.w))
			}
			// The counter-example, spelled out: what byte slicing would produce.
			if len(c.line) >= c.w {
				if byteCut := c.line[:c.w]; byteCut == got && DispWidth(byteCut) != c.wantW {
					t.Errorf("the row equals the BYTE slice %q, which is %d columns",
						byteCut, DispWidth(byteCut))
				}
			}
		})
	}
}

// TestFrameBytesLineCount pins the row arithmetic on its own, because an
// off-by-one here is invisible in a screenshot and unmistakable on a terminal:
// the frame scrolls by one line per repaint.
func TestFrameBytesLineCount(t *testing.T) {
	for _, h := range []int{1, 2, 3, 24, 60} {
		got := FrameBytes([]string{"a", "b", "c"}, 10, h)
		if n := strings.Count(got, "\r\n"); n != h-1 {
			t.Errorf("FrameBytes(..., h=%d) contains %d line separators, want %d — h rows "+
				"need h-1 of them, and the h'th newline is what scrolls the terminal",
				h, n, h-1)
		}
		if n := strings.Count(got, ClearLine); n != h {
			t.Errorf("FrameBytes(..., h=%d) erases %d lines, want %d", h, n, h)
		}
	}
}

// TestFrameBytesRedrawsOverThePreviousFrame. The frame never clears the screen
// (that is the classic flicker), so every cell must be either overwritten or
// explicitly erased. Concretely: a long frame followed by a short one must not
// leave the long one's tail on screen.
func TestFrameBytesRedrawsOverThePreviousFrame(t *testing.T) {
	rows := frameRows(t, []string{"the previous frame was taller"}, 30, 6)
	for i := 1; i < len(rows); i++ {
		if rows[i] != "" {
			t.Fatalf("row %d = %q, want empty", i, rows[i])
		}
	}
	// Each empty row still carries its erase, which frameRows checks; this
	// asserts the frame does not take the cheaper route of stopping early.
	full := FrameBytes([]string{"one"}, 30, 6)
	if strings.Count(full, ClearLine) != 6 {
		t.Errorf("a 6-row frame with one line of content erases %d rows, want 6 — the other "+
			"five would still show the previous frame", strings.Count(full, ClearLine))
	}
}

// scratchTerminal is a Terminal whose out is a regular file and whose saved
// state is nil, so Close restores nothing and leaveRaw is a no-op. It is enough
// to exercise the parts that do not touch a tty.
func scratchTerminal(t *testing.T) (*Terminal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("could not create the scratch out file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return &Terminal{out: f}, path
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("could not stat %s: %v", path, err)
	}
	return fi.Size()
}

// TestCloseIsIdempotent. Close is reachable from the defer, the signal handler
// and the panic path at once, so a second call must be silent: re-enabling the
// cursor on a terminal that has already moved on is a visible glitch, and a
// second ioctl on a closed fd is an error nobody reads.
func TestCloseIsIdempotent(t *testing.T) {
	tm, path := scratchTerminal(t)

	if err := tm.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	first := fileSize(t, path)
	if first == 0 {
		t.Fatalf("the first Close wrote nothing — the restore sequence is what hands the " +
			"terminal back, and without it the user is left with no cursor and no echo")
	}

	if err := tm.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if second := fileSize(t, path); second != first {
		t.Errorf("the second Close wrote %d more bytes; want none. Close must be idempotent, "+
			"because the defer, the signal handler and the panic path all call it",
			second-first)
	}

	// A nil Terminal is the case where Open failed and a defer still runs.
	var nilT *Terminal
	if err := nilT.Close(); err != nil {
		t.Errorf("(*Terminal)(nil).Close() = %v; want nil", err)
	}
}

// TestCloseRestoresInReverseOrder. The disable list must be the exact reverse of
// the enable list; a mode enabled in one place and disabled in another that does
// not run is the commonest way a terminal is left broken.
func TestCloseRestoresInReverseOrder(t *testing.T) {
	tm, path := scratchTerminal(t)
	if err := tm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read back the scratch file: %v", err)
	}
	if got, want := string(b), pasteOff+mouseOff+CursorShow+AltScreenOff; got != want {
		t.Errorf("Close wrote %q, want %q", got, want)
	}
}

func TestWriteGoesStraightToOut(t *testing.T) {
	tm, path := scratchTerminal(t)
	n, err := tm.Write([]byte("\x1b[6n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 4 {
		t.Errorf("Write returned n=%d, want 4", n)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read back the scratch file: %v", err)
	}
	if string(b) != "\x1b[6n" {
		t.Errorf("Write put %q on the out file, want %q — Write is unframed and must not "+
			"add anything of its own", b, "\x1b[6n")
	}
}

func TestResizeReturnsTheWatchChannel(t *testing.T) {
	ch := make(chan struct{}, 1)
	tm := &Terminal{resize: ch}
	ch <- struct{}{}
	got := tm.Resize()
	if got == nil {
		t.Fatal("Resize() returned nil; the event loop selects on this channel")
	}
	select {
	case <-got:
	default:
		t.Error("Resize() did not return the terminal's own resize channel")
	}
}

// TestSizeFallsBackTo80x24. The out file here is a regular file, so termSize
// fails on every platform. 80x24 is the fallback because a guess that is too
// small is merely cramped, while one that is too large wraps every line and
// corrupts the frame.
func TestSizeFallsBackTo80x24(t *testing.T) {
	tm, _ := scratchTerminal(t)
	w, h := tm.Size()
	if w != 80 || h != 24 {
		t.Errorf("Size() on a non-terminal = %dx%d, want 80x24", w, h)
	}
}

// TestOwnConsoleDoesNotPanic. The answer depends on how the process was started,
// so there is nothing to assert about the value — only that asking is safe. On
// Unix it is always false; on Windows it is a kernel32 call that must survive a
// missing proc and a zero return.
func TestOwnConsoleDoesNotPanic(t *testing.T) {
	_ = OwnConsole()
}
