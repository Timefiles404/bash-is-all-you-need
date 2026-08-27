package tui

import (
	"strings"
	"testing"

	"bash-is-all-you-need/tui/term"
)

// at builds an editor holding text with the caret at rune offset cur, which is
// what nearly every case below needs and what a literal struct cannot express
// without repeating the rune conversion.
func at(text string, cur int) *editor {
	e := newEditor()
	e.setText(text)
	e.cur = cur
	return e
}

// ---------------------------------------------------------------------------
// Caret arithmetic
// ---------------------------------------------------------------------------

// The caret is a rune offset, not a byte offset. A byte caret lands inside a
// multi-byte character on the first non-ASCII input, and this repo's own test
// corpus is full of CJK.
func TestTheCaretCountsRunesAndNotBytes(t *testing.T) {
	e := newEditor()
	e.insert("你好")
	e.insert("ab")

	if e.text() != "你好ab" {
		t.Fatalf("text() = %q, expected %q", e.text(), "你好ab")
	}
	if e.cur != 4 {
		t.Fatalf("caret is at %d, expected 4 runes (a byte caret would say %d)", e.cur, len("你好ab"))
	}

	e.left()
	e.left()
	if e.cur != 2 {
		t.Errorf("after two lefts the caret is at %d, expected 2", e.cur)
	}
	e.backspace()
	if e.text() != "你ab" {
		t.Errorf("backspace over a CJK character left %q, expected %q — it took bytes, not a rune", e.text(), "你ab")
	}
	if e.cur != 1 {
		t.Errorf("after backspace the caret is at %d, expected 1", e.cur)
	}

	e.left()
	e.left()
	if e.cur != 0 {
		t.Errorf("left at the start moved the caret to %d, expected it to stop at 0", e.cur)
	}
	for i := 0; i < 10; i++ {
		e.right()
	}
	if e.cur != 3 {
		t.Errorf("right past the end moved the caret to %d, expected it to stop at 3", e.cur)
	}
	e.del()
	if e.text() != "你ab" {
		t.Errorf("del at the end changed the text to %q, expected it to do nothing", e.text())
	}
}

func TestWordMotionSkipsTheSeparatorsAndThenTheWord(t *testing.T) {
	e := at("你好 abc/def", 10)

	e.wordLeft()
	if e.cur != 7 {
		t.Errorf("wordLeft from the end went to %d, expected 7 (the start of \"def\")", e.cur)
	}
	e.wordLeft()
	if e.cur != 3 {
		t.Errorf("wordLeft again went to %d, expected 3 (the start of \"abc\", past the slash)", e.cur)
	}
	e.wordLeft()
	if e.cur != 0 {
		t.Errorf("wordLeft over the CJK word went to %d, expected 0", e.cur)
	}
	e.wordLeft()
	if e.cur != 0 {
		t.Errorf("wordLeft at the start went to %d, expected it to stay at 0", e.cur)
	}

	e.wordRight()
	if e.cur != 2 {
		t.Errorf("wordRight went to %d, expected 2 (the end of the CJK word)", e.cur)
	}
	e.wordRight()
	if e.cur != 6 {
		t.Errorf("wordRight went to %d, expected 6 (the end of \"abc\")", e.cur)
	}
	e.wordRight()
	if e.cur != 10 {
		t.Errorf("wordRight went to %d, expected 10 (the end of the buffer)", e.cur)
	}
	e.wordRight()
	if e.cur != 10 {
		t.Errorf("wordRight at the end went to %d, expected it to stay at 10", e.cur)
	}
}

// The wrapped row is a fact about the window width; the logical line is a fact
// about what was typed. Binding Home to the former means the same key does
// different things after a resize.
func TestHomeAndEndStopAtTheLogicalLineNotTheBuffer(t *testing.T) {
	e := at("one\ntwo\nthree", 5)

	e.home()
	if e.cur != 4 {
		t.Errorf("home from the middle of line two went to %d, expected 4 — it ran to the start of the buffer", e.cur)
	}
	e.end()
	if e.cur != 7 {
		t.Errorf("end from the start of line two went to %d, expected 7 — it ran to the end of the buffer", e.cur)
	}

	// On the first and last lines the two must still stop at the buffer's ends.
	e = at("one\ntwo", 1)
	e.home()
	if e.cur != 0 {
		t.Errorf("home on the first line went to %d, expected 0", e.cur)
	}
	e = at("one\ntwo", 5)
	e.end()
	if e.cur != 7 {
		t.Errorf("end on the last line went to %d, expected 7", e.cur)
	}
}

func TestUpAndDownKeepTheColumnAndClampToTheShorterLine(t *testing.T) {
	e := at("abcdef\nxy\nghijkl", 5) // column 5 of line one

	if !e.lineDown() {
		t.Fatal("lineDown from the first line reported no move")
	}
	if e.cur != 9 {
		t.Errorf("lineDown landed at %d, expected 9 — the end of the short line, since column 5 does not exist there", e.cur)
	}
	if !e.lineDown() {
		t.Fatal("lineDown from the middle line reported no move")
	}
	if e.cur != 12 {
		t.Errorf("lineDown landed at %d, expected 12 (column 2 of line three, the column carried from the short line)", e.cur)
	}
	if e.lineDown() {
		t.Errorf("lineDown on the last line reported a move to %d, expected false", e.cur)
	}

	e = at("abcdef\nxy\nghijkl", 15)
	if !e.lineUp() {
		t.Fatal("lineUp from the last line reported no move")
	}
	if e.cur != 9 {
		t.Errorf("lineUp landed at %d, expected 9 — clamped to the end of the short line", e.cur)
	}
	if !e.lineUp() {
		t.Fatal("lineUp from the middle line reported no move")
	}
	if e.lineUp() {
		t.Errorf("lineUp on the first line reported a move to %d, expected false", e.cur)
	}
}

func TestMultilineIsWhatDecidesWhetherUpBrowsesHistory(t *testing.T) {
	if at("one line", 0).multiline() {
		t.Error("a buffer with no newline reported multiline")
	}
	if !at("one\ntwo", 0).multiline() {
		t.Error("a buffer with a newline reported single-line")
	}
	if !at("trailing\n", 0).multiline() {
		t.Error("a buffer ending in a newline reported single-line")
	}
}

// ---------------------------------------------------------------------------
// Killing and yanking
// ---------------------------------------------------------------------------

// Ctrl-K in a multi-line prompt deletes the line you are on, not everything
// below it.
func TestKillToEndAndKillToStartStopAtTheLine(t *testing.T) {
	e := at("one\ntwo\nthree", 5)
	e.killToEnd()
	if e.text() != "one\nt\nthree" {
		t.Errorf("killToEnd left %q, expected %q — it ran past the newline", e.text(), "one\nt\nthree")
	}
	if e.kill != "wo" {
		t.Errorf("killToEnd stored %q, expected %q", e.kill, "wo")
	}
	if e.cur != 5 {
		t.Errorf("killToEnd left the caret at %d, expected 5", e.cur)
	}
	e.yank()
	if e.text() != "one\ntwo\nthree" || e.cur != 7 {
		t.Errorf("yank produced %q with the caret at %d, expected the original text with the caret at 7", e.text(), e.cur)
	}

	e = at("one\ntwo\nthree", 6)
	e.killToStart()
	if e.text() != "one\no\nthree" {
		t.Errorf("killToStart left %q, expected %q — it ran past the newline", e.text(), "one\no\nthree")
	}
	if e.kill != "tw" {
		t.Errorf("killToStart stored %q, expected %q", e.kill, "tw")
	}
	if e.cur != 4 {
		t.Errorf("killToStart left the caret at %d, expected 4", e.cur)
	}
	e.yank()
	if e.text() != "one\ntwo\nthree" || e.cur != 6 {
		t.Errorf("yank produced %q with the caret at %d, expected the original text with the caret at 6", e.text(), e.cur)
	}
}

// At the line's edge there is nothing to kill, and clobbering the yank buffer
// with an empty string would throw away the thing Ctrl-Y is for.
func TestAKillWithNothingToTakeLeavesTheYankBufferAlone(t *testing.T) {
	e := at("one\ntwo", 4)
	e.kill = "kept"
	e.killToStart() // already at the start of line two
	if e.text() != "one\ntwo" || e.kill != "kept" {
		t.Errorf("killToStart at the line start gave text %q and kill %q, expected them unchanged", e.text(), e.kill)
	}
	e.cur = 7
	e.killToEnd() // already at the end of line two
	if e.text() != "one\ntwo" || e.kill != "kept" {
		t.Errorf("killToEnd at the line end gave text %q and kill %q, expected them unchanged", e.text(), e.kill)
	}
}

func TestKillWordLeftTakesTheWordBeforeTheCaret(t *testing.T) {
	e := at("some words here", 11)
	e.killWordLeft()
	if e.text() != "some here" {
		t.Errorf("killWordLeft left %q, expected %q", e.text(), "some here")
	}
	if e.kill != "words " {
		t.Errorf("killWordLeft stored %q, expected %q", e.kill, "words ")
	}
	e.yank()
	if e.text() != "some words here" {
		t.Errorf("yank produced %q, expected the original text back", e.text())
	}
}

// ---------------------------------------------------------------------------
// Paste sanitising
// ---------------------------------------------------------------------------

// Bracketed paste delivers a whole paragraph as one key, so insert is the path a
// pasted file arrives on. A literal tab cannot be allowed in: every column here
// is computed from character widths, and a tab's width is a property of the
// terminal's tab stops, which this process cannot see.
func TestInsertSanitisesWhatIsPastedIntoIt(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"crlf becomes lf", "a\r\nb", "a\nb"},
		{"a lone cr is dropped", "a\rb", "ab"},
		{"a tab becomes spaces", "a\tb", "a" + strings.Repeat(" ", tabWidth) + "b"},
		{"a bell is dropped", "a\x07b", "ab"},
		{"an escape is dropped", "a\x1b[31mb", "a[31mb"},
		{"a nul is dropped", "a\x00b", "ab"},
		{"a delete is dropped", "a\x7fb", "ab"},
		{"cjk and emoji survive", "你好🙂", "你好🙂"},
		{"a newline survives", "a\nb\n", "a\nb\n"},
		{"nothing but control characters inserts nothing", "\x07\x00\r", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEditor()
			e.insert(tc.in)
			if e.text() != tc.want {
				t.Errorf("insert(%q) gave %q, expected %q", tc.in, e.text(), tc.want)
			}
			if e.cur != len([]rune(tc.want)) {
				t.Errorf("insert(%q) left the caret at %d, expected %d", tc.in, e.cur, len([]rune(tc.want)))
			}
		})
	}
}

func TestInsertGoesInAtTheCaretRatherThanAtTheEnd(t *testing.T) {
	e := at("ac", 1)
	e.insert("b")
	if e.text() != "abc" || e.cur != 2 {
		t.Errorf("insert at the caret gave %q with the caret at %d, expected %q and 2", e.text(), e.cur, "abc")
	}
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

// Deduplicating the whole history would make Up unpredictable: the same key
// would skip a different number of entries depending on what was typed hours
// ago. Only consecutive duplicates are collapsed.
func TestConsecutiveDuplicatesAreNotStoredTwice(t *testing.T) {
	e := newEditor()
	e.remember("ls")
	e.remember("ls")
	e.remember("ls")
	if len(e.hist) != 1 {
		t.Errorf("three identical lines stored %d entries (%q), expected 1", len(e.hist), e.hist)
	}
	e.remember("cat x")
	e.remember("ls")
	if strings.Join(e.hist, "|") != "ls|cat x|ls" {
		t.Errorf("history is %q, expected [ls, cat x, ls]: a non-consecutive repeat was dropped", e.hist)
	}
	e.remember("")
	if len(e.hist) != 3 {
		t.Errorf("remembering the empty line grew the history to %q", e.hist)
	}
}

func TestRememberingAlwaysLeavesHistoryBrowsingAtTheLiveBuffer(t *testing.T) {
	e := newEditor()
	e.remember("first")
	e.histPrev()
	e.remember("first") // the consecutive-duplicate path
	if e.hpos != len(e.hist) {
		t.Errorf("hpos is %d after remembering a duplicate, expected %d — Up would resume mid-history", e.hpos, len(e.hist))
	}
}

// The commonest reason people stop using history is having lost a half-written
// line to it once, so the live buffer is stashed on the way out and restored on
// the way back in.
func TestTheLiveBufferIsStashedByHistPrevAndRestoredByWalkingBack(t *testing.T) {
	e := newEditor()
	e.remember("first")
	e.remember("second")
	e.setText("half-written")

	if !e.histPrev() {
		t.Fatal("histPrev reported no move with two entries in the history")
	}
	if e.text() != "second" {
		t.Fatalf("histPrev gave %q, expected %q", e.text(), "second")
	}
	if e.cur != len("second") {
		t.Errorf("histPrev left the caret at %d, expected the end of the line at %d", e.cur, len("second"))
	}
	if !e.histPrev() || e.text() != "first" {
		t.Fatalf("the second histPrev gave %q, expected %q", e.text(), "first")
	}
	if e.histPrev() {
		t.Errorf("histPrev past the oldest entry reported a move, to %q", e.text())
	}

	if !e.histNext() || e.text() != "second" {
		t.Fatalf("histNext gave %q, expected %q", e.text(), "second")
	}
	if !e.histNext() {
		t.Fatal("histNext back to the live buffer reported no move")
	}
	if e.text() != "half-written" {
		t.Errorf("walking back gave %q, expected the stashed live buffer %q", e.text(), "half-written")
	}
	if e.histNext() {
		t.Errorf("histNext past the live buffer reported a move, to %q", e.text())
	}
}

func TestTheHistoryIsCappedFromTheOldestEnd(t *testing.T) {
	e := newEditor()
	for i := 0; i < maxHistory+50; i++ {
		e.remember(strings.Repeat("x", i%17+1) + string(rune('a'+i%23)))
	}
	if len(e.hist) != maxHistory {
		t.Errorf("history holds %d entries, expected the cap of %d", len(e.hist), maxHistory)
	}
	if e.hpos != len(e.hist) {
		t.Errorf("hpos is %d after the cap trimmed the history, expected %d", e.hpos, len(e.hist))
	}
}

func TestClearAbandonsAnyHistoryBrowsing(t *testing.T) {
	e := newEditor()
	e.remember("old")
	e.setText("draft")
	e.histPrev()
	e.clear()
	if e.text() != "" || e.cur != 0 {
		t.Errorf("clear left %q with the caret at %d, expected an empty buffer", e.text(), e.cur)
	}
	if e.hpos != len(e.hist) || e.stash != "" {
		t.Errorf("clear left hpos=%d stash=%q, expected %d and an empty stash", e.hpos, e.stash, len(e.hist))
	}
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

func TestRenderPutsTheCaretAfterAWideRuneNotInsideIt(t *testing.T) {
	e := at("你", 1)
	rows, cr, cc := e.render("> ", "  ", 20, -1)

	if len(rows) != 1 || rows[0] != "> 你" {
		t.Fatalf("render gave rows %q, expected [\"> 你\"]", rows)
	}
	if cr != 0 || cc != 4 {
		t.Errorf("the caret is at row %d column %d, expected row 0 column 4 (two for the prompt, two for the wide rune)", cr, cc)
	}

	// And in front of it, so a wide rune is not skipped over on the way in.
	e.cur = 0
	if _, cr, cc = e.render("> ", "  ", 20, -1); cr != 0 || cc != 2 {
		t.Errorf("with the caret before the wide rune it drew at row %d column %d, expected row 0 column 2", cr, cc)
	}
}

// Otherwise the caret is drawn in the last column, on top of the character it is
// supposed to be after.
func TestTheCaretAtTheEndOfAFullRowWrapsToTheNextRow(t *testing.T) {
	e := at(strings.Repeat("a", 6), 6)
	rows, cr, cc := e.render("> ", "  ", 8, -1)

	if len(rows) != 2 {
		t.Fatalf("render gave %d rows (%q), expected 2: the caret stayed on the full row", len(rows), rows)
	}
	if rows[0] != "> aaaaaa" || rows[1] != "  " {
		t.Errorf("render gave rows %q, expected [\"> aaaaaa\", \"  \"]", rows)
	}
	if cr != 1 || cc != 2 {
		t.Errorf("the caret is at row %d column %d, expected row 1 column 2 (the continuation indent)", cr, cc)
	}

	// One character short of full and it stays put, so the rule is "the row is
	// full", not "the row is nearly full".
	e = at(strings.Repeat("a", 5), 5)
	if rows, cr, cc = e.render("> ", "  ", 8, -1); len(rows) != 1 || cr != 0 || cc != 7 {
		t.Errorf("a row with one column spare gave %d rows and a caret at row %d column %d, expected 1 row and row 0 column 7", len(rows), cr, cc)
	}
}

func TestRenderBreaksAtAnEmbeddedNewlineAndIndentsTheContinuation(t *testing.T) {
	e := at("one\ntwo", 7)
	rows, cr, cc := e.render("> ", "..", 20, -1)

	if strings.Join(rows, "|") != "> one|..two" {
		t.Fatalf("render gave %q, expected [\"> one\", \"..two\"]", rows)
	}
	if cr != 1 || cc != 5 {
		t.Errorf("the caret is at row %d column %d, expected row 1 column 5 (past the two-column continuation and \"two\")", cr, cc)
	}

	// A buffer ending in a newline draws an empty continuation row, because that
	// is where the next character will go.
	e = at("one\n", 4)
	if rows, cr, cc = e.render("> ", "..", 20, -1); strings.Join(rows, "|") != "> one|.." || cr != 1 || cc != 2 {
		t.Errorf("a trailing newline gave %q with the caret at row %d column %d, expected [\"> one\", \"..\"] and row 1 column 2", rows, cr, cc)
	}
}

func TestRenderMasksFromTheGivenOffsetWithoutTouchingTheBuffer(t *testing.T) {
	const line = "/secret-thing hunter2"
	e := at(line, len([]rune(line)))
	rows, cr, cc := e.render("> ", "  ", 60, 14)

	if len(rows) != 1 {
		t.Fatalf("render gave %d rows (%q), expected 1", len(rows), rows)
	}
	if want := "> /secret-thing " + strings.Repeat("•", 7); rows[0] != want {
		t.Fatalf("render gave %q, expected %q", rows[0], want)
	}
	if cr != 0 || cc != term.DispWidth(rows[0]) {
		t.Errorf("the caret is at row %d column %d, expected row 0 column %d — the drawn text and the caret disagree", cr, cc, term.DispWidth(rows[0]))
	}
	if e.text() != line {
		t.Errorf("masking changed the buffer to %q; it must be display-only", e.text())
	}

	// A negative maskFrom draws everything, which is the ordinary case.
	if rows, _, _ = e.render("> ", "  ", 60, -1); rows[0] != "> "+line {
		t.Errorf("with maskFrom -1 render gave %q, expected the plain text", rows[0])
	}
}

// A window narrower than the prompt plus one character cannot be laid out at
// all, so render floors the width rather than looping forever on a rune that
// never fits.
func TestRenderFloorsAnImpossiblyNarrowWindow(t *testing.T) {
	e := at("abcdefghij", 10)
	rows, cr, _ := e.render("> ", "  ", 1, -1)
	if len(rows) == 0 {
		t.Fatal("render at width 1 gave no rows")
	}
	if cr < 0 || cr >= len(rows) {
		t.Errorf("render at width 1 put the caret on row %d of %d rows", cr, len(rows))
	}
	for i, r := range rows {
		if term.DispWidth(r) > 8 {
			t.Errorf("row %d is %d columns wide (%q), expected the floor of 8", i, term.DispWidth(r), r)
		}
	}
}

// ---------------------------------------------------------------------------
// window
// ---------------------------------------------------------------------------

// The composer scrolls inside its own pane past maxInputRows, and the one thing
// that must survive that is the row the caret is on.
func TestWindowAlwaysKeepsTheCaretRowInsideTheReturnedRows(t *testing.T) {
	rows := make([]string, 10)
	for i := range rows {
		rows[i] = string(rune('a' + i))
	}

	for _, tc := range []struct {
		caretRow int
		want     string
		wantCar  int
	}{
		{0, "abc", 0},
		{1, "abc", 1},
		{2, "abc", 2},
		{5, "def", 2},
		{8, "ghi", 2},
		{9, "hij", 2},
	} {
		out, caret := window(rows, tc.caretRow, 3)
		if strings.Join(out, "") != tc.want {
			t.Errorf("window(caretRow %d, n 3) returned %q, expected %q", tc.caretRow, out, tc.want)
		}
		if caret != tc.wantCar {
			t.Errorf("window(caretRow %d, n 3) reported the caret on row %d, expected %d", tc.caretRow, caret, tc.wantCar)
		}
		if caret < 0 || caret >= len(out) {
			t.Errorf("window(caretRow %d, n 3) put the caret on row %d of %d returned rows", tc.caretRow, caret, len(out))
		}
		if out[caret] != rows[tc.caretRow] {
			t.Errorf("window(caretRow %d, n 3) points at %q, expected the caret's own row %q", tc.caretRow, out[caret], rows[tc.caretRow])
		}
	}
}

func TestWindowLeavesRowsAloneWhenTheyAlreadyFit(t *testing.T) {
	rows := []string{"a", "b", "c"}
	for _, n := range []int{3, 4, 99} {
		out, caret := window(rows, 1, n)
		if strings.Join(out, "") != "abc" || caret != 1 {
			t.Errorf("window(3 rows, caret 1, n %d) returned %q with caret %d, expected them unchanged", n, out, caret)
		}
	}
	// A non-positive n is a window with no room; trimming to it would leave the
	// caret pointing outside the slice, so the rows come back untouched.
	if out, caret := window(rows, 2, 0); strings.Join(out, "") != "abc" || caret != 2 {
		t.Errorf("window(n 0) returned %q with caret %d, expected the rows unchanged", out, caret)
	}
}
