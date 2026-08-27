package tui

import (
	"fmt"
	"strings"
	"testing"

	"bash-is-all-you-need/tui/term"
)

// rowsAt is view() with the two return values a test rarely cares about
// dropped, so the assertions below read as claims about what is on screen.
func rowsAt(s *scrollback, w, h int) []string {
	rows, _, _ := s.view(w, h, 0)
	return rows
}

// ---------------------------------------------------------------------------
// The partial line
// ---------------------------------------------------------------------------

// The host's renderer streams a reply a few tokens at a time and does not send a
// newline until the paragraph ends. If an unterminated write were invisible the
// pane would sit empty for the whole of the first sentence.
func TestAWriteWithNoTrailingNewlineIsVisibleInTheView(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte("abc"))

	rows := rowsAt(s, 20, 5)
	if len(rows) != 1 || rows[0] != "abc" {
		t.Fatalf("view showed %q, expected one row %q", rows, "abc")
	}
}

func TestTheNextWriteContinuesThePartialLineRatherThanStartingANewOne(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte("abc"))
	s.Write([]byte("def"))

	rows, total, _ := s.view(20, 5, 0)
	if total != 1 {
		t.Errorf("view reported %d rows in total, expected 1: the second write started a new line", total)
	}
	if len(rows) != 1 || rows[0] != "abcdef" {
		t.Fatalf("view showed %q, expected one row %q", rows, "abcdef")
	}
}

func TestAPartialLineCountsTowardsTheStatsButIsNotYetALoggedLine(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte("done\n"))
	s.Write([]byte("still writing"))

	lines, dropped := s.stats()
	if lines != 2 || dropped != 0 {
		t.Fatalf("stats() = (%d, %d), expected (2, 0)", lines, dropped)
	}
}

// ---------------------------------------------------------------------------
// Line endings
// ---------------------------------------------------------------------------

// Both halves of the claim in add(): "\r\n" is one ending, and a lone "\r"
// rewrites the line in place the way a progress counter does.
func TestCRLFIsOneLineEndingAndABareCRRewritesTheLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write string
		want  []string
	}{
		{"lf", "one\ntwo\n", []string{"one", "two"}},
		{"crlf", "one\r\ntwo\r\n", []string{"one", "two"}},
		{"bare cr rewrites", "50%\r100%\ndone\n", []string{"100%", "done"}},
		{"mixed", "a\r\nb\rc\nd\n", []string{"a", "c", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScrollback(100)
			s.Write([]byte(tc.write))

			rows := rowsAt(s, 20, 10)
			if strings.Join(rows, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("writing %q showed %q, expected %q", tc.write, rows, tc.want)
			}
		})
	}
}

// The panel draws blank separators between calls, so a wrapper that returned no
// rows for an empty line would collapse the spacing the panel was designed
// around.
func TestAnEmptyLogicalLineStillOccupiesOneRow(t *testing.T) {
	if got := wrapLine("", 20); len(got) != 1 || got[0] != "" {
		t.Errorf("wrapLine(%q, 20) = %q, expected one empty row", "", got)
	}
	if got := wrapLine("", 0); len(got) != 1 || got[0] != "" {
		t.Errorf("wrapLine(%q, 0) = %q, expected one empty row even at zero width", "", got)
	}

	s := newScrollback(100)
	s.Write([]byte("a\n\nb\n"))

	rows, total, _ := s.view(20, 10, 0)
	if total != 3 {
		t.Errorf("view reported %d rows, expected 3: the blank line was dropped", total)
	}
	if strings.Join(rows, "|") != "a||b" {
		t.Fatalf("view showed %q, expected [a, \"\", b]", rows)
	}
}

// ---------------------------------------------------------------------------
// The wrap cache
// ---------------------------------------------------------------------------

// The cache is keyed on one width. If a resize did not invalidate it the pane
// would keep drawing the old geometry until something else happened to push a
// line.
func TestChangingTheWidthInvalidatesTheWrapCache(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte(strings.Repeat("x", 30) + "\n"))

	wide := rowsAt(s, 20, 10)
	if len(wide) != 2 || wide[0] != strings.Repeat("x", 20) || wide[1] != strings.Repeat("x", 10) {
		t.Fatalf("at width 20 the pane showed %q, expected rows of 20 and 10 columns", wide)
	}

	narrow := rowsAt(s, 10, 10)
	if len(narrow) != 3 {
		t.Fatalf("at width 10 the pane showed %d rows (%q), expected 3 — the width-20 cache was reused", len(narrow), narrow)
	}
	for i, r := range narrow {
		if r != strings.Repeat("x", 10) {
			t.Errorf("row %d at width 10 is %q, expected 10 columns", i, r)
		}
	}

	// And back again, so the invalidation is not one-way.
	if again := rowsAt(s, 20, 10); len(again) != 2 {
		t.Errorf("back at width 20 the pane showed %d rows (%q), expected 2", len(again), again)
	}
}

// term.WrapCols re-opens the active SGR state on every continuation row, and
// scrollback has to preserve that: a colour that vanishes from row two onward is
// how a repaint mid-paragraph loses it.
func TestColourSurvivesWrappingOntoTheSecondRow(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte("\x1b[31m" + strings.Repeat("a", 30) + "\x1b[0m\n"))

	rows := rowsAt(s, 20, 10)
	if len(rows) != 2 {
		t.Fatalf("view showed %d rows (%q), expected 2", len(rows), rows)
	}
	for i, r := range rows {
		if !strings.Contains(r, "\x1b[31m") {
			t.Errorf("row %d is %q, expected it to carry \x1b[31m — the colour was not re-opened", i, r)
		}
	}
	if term.DispWidth(rows[0]) != 20 {
		t.Errorf("row 0 measures %d columns, expected 20: the escape sequence was counted as text", term.DispWidth(rows[0]))
	}
}

// ---------------------------------------------------------------------------
// The caps
// ---------------------------------------------------------------------------

// Dropping one line per write would re-wrap the whole pane on every line once
// the cap was reached, so the drop happens in batches. What a caller may rely on
// is only that the newest line survives, the oldest does not, and the loss is
// reported.
func TestTheLineCapDropsInBatchesAndKeepsTheNewestLine(t *testing.T) {
	const capLines = 32
	s := newScrollback(capLines)

	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	s.Write([]byte(b.String()))
	s.Write([]byte("newest\n"))

	lines, dropped := s.stats()
	if dropped == 0 {
		t.Errorf("after 201 lines with a cap of %d, stats() reports 0 dropped", capLines)
	}
	if lines > capLines+capLines/8 {
		t.Errorf("the pane holds %d lines, which is past the cap of %d plus one batch", lines, capLines)
	}
	if lines+dropped != 201 {
		t.Errorf("%d held + %d dropped = %d, expected the 201 lines written", lines, dropped, lines+dropped)
	}

	all := strings.Join(rowsAt(s, 40, capLines+capLines/8+2), "|")
	if !strings.Contains(all, "newest") {
		t.Errorf("the newest line is not in the view: %q", all)
	}
	if strings.Contains(all, "line 0000") {
		t.Errorf("the oldest line is still in the view: %q", all)
	}
	if !strings.Contains(all, fmt.Sprintf("line %04d", 200-lines+1)) {
		t.Errorf("the %d held lines do not end at line 199: %q", lines, all)
	}
}

// A logical line is re-wrapped on every resize, so one `cat` of a minified
// bundle would turn a resize into a hang. The truncation happens once, at write
// time, and says that it happened.
func TestALogicalLineLongerThanTheByteCapIsTruncatedWithAMarker(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte(strings.Repeat("z", maxLineBytes+1000) + "\n"))

	s.mu.Lock()
	got := s.lines[0]
	s.mu.Unlock()

	if !strings.HasSuffix(got, " …(line truncated)") {
		t.Fatalf("the stored line ends %q, expected the truncation marker", got[max(0, len(got)-24):])
	}
	if want := maxLineBytes + len(" …(line truncated)"); len(got) != want {
		t.Errorf("the stored line is %d bytes, expected %d (%d of payload plus the marker)", len(got), want, maxLineBytes)
	}
}

// A line at exactly the cap is not over it, and adding a marker to it would
// claim something was lost when nothing was.
func TestALineExactlyAtTheByteCapIsNotTruncated(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte(strings.Repeat("z", maxLineBytes) + "\n"))

	s.mu.Lock()
	got := s.lines[0]
	s.mu.Unlock()

	if len(got) != maxLineBytes || strings.Contains(got, "truncated") {
		t.Errorf("a line of exactly %d bytes was stored as %d bytes with truncated=%v", maxLineBytes, len(got), strings.Contains(got, "truncated"))
	}
}

// The floor exists so that a host asking for a two-line pane does not get a pane
// that drops a line per write.
func TestAScrollbackAsksForAtLeastSixteenLines(t *testing.T) {
	for _, ask := range []int{-1, 0, 1, 15} {
		if s := newScrollback(ask); s.maxLines != 16 {
			t.Errorf("newScrollback(%d).maxLines = %d, expected the floor of 16", ask, s.maxLines)
		}
	}
	if s := newScrollback(17); s.maxLines != 17 {
		t.Errorf("newScrollback(17).maxLines = %d, expected 17: the floor overrode a legitimate value", s.maxLines)
	}
}

// ---------------------------------------------------------------------------
// Scrolling
// ---------------------------------------------------------------------------

// The clamped offset is the value the caller stores back, which is what makes
// "scrolled past the top" impossible to express rather than something every key
// handler has to remember to check.
func TestViewClampsTheScrollOffsetAtBothEnds(t *testing.T) {
	s := newScrollback(100)
	for i := 0; i < 10; i++ {
		s.Write([]byte(string(rune('a'+i)) + "\n"))
	}

	rows, total, up := s.view(20, 3, 1000)
	if total != 10 {
		t.Fatalf("total = %d, expected 10", total)
	}
	if up != 7 {
		t.Errorf("scrolling up 1000 rows clamped to %d, expected %d (total minus the pane height)", up, total-3)
	}
	if strings.Join(rows, "|") != "a|b|c" {
		t.Errorf("scrolled to the top the pane shows %q, expected the first three lines", rows)
	}

	rows, _, up = s.view(20, 3, -5)
	if up != 0 {
		t.Errorf("scrolling down past the bottom clamped to %d, expected 0", up)
	}
	if strings.Join(rows, "|") != "h|i|j" {
		t.Errorf("scrolled to the bottom the pane shows %q, expected the last three lines", rows)
	}

	// A pane taller than the content cannot be scrolled at all.
	if _, _, up = s.view(20, 40, 5); up != 0 {
		t.Errorf("with a pane taller than the content the offset clamped to %d, expected 0", up)
	}
}

func TestViewOfAnEmptyOrCollapsedPaneReturnsNothingRatherThanPanicking(t *testing.T) {
	s := newScrollback(100)
	if rows, total, up := s.view(20, 5, 0); len(rows) != 0 || total != 0 || up != 0 {
		t.Errorf("an empty pane returned (%q, %d, %d), expected (nil, 0, 0)", rows, total, up)
	}
	s.Write([]byte("something\n"))
	for _, wh := range [][2]int{{0, 5}, {20, 0}, {-1, -1}} {
		if rows, total, up := s.view(wh[0], wh[1], 3); len(rows) != 0 || total != 0 || up != 0 {
			t.Errorf("view(%d, %d, 3) returned (%q, %d, %d), expected (nil, 0, 0)", wh[0], wh[1], rows, total, up)
		}
	}
}

// The partial line scrolls with everything else: it is a row of the pane, not a
// separate widget pinned to the bottom.
func TestThePartialLineIsPartOfTheScrollableContent(t *testing.T) {
	s := newScrollback(100)
	s.Write([]byte("one\ntwo\n"))
	s.Write([]byte("three, still going"))

	rows, total, up := s.view(20, 2, 0)
	if total != 3 || up != 0 {
		t.Fatalf("view = (total %d, up %d), expected (3, 0)", total, up)
	}
	if strings.Join(rows, "|") != "two|three, still going" {
		t.Errorf("the bottom of the pane is %q, expected the partial line last", rows)
	}
	if rows, _, _ = s.view(20, 2, 1); strings.Join(rows, "|") != "one|two" {
		t.Errorf("scrolled up one row the pane shows %q, expected the partial line to have scrolled off", rows)
	}
}

// ---------------------------------------------------------------------------
// clear
// ---------------------------------------------------------------------------

// clear empties the pane and nothing else: the drop counter is a fact about the
// session, and resetting it would make /status claim nothing was ever lost.
func TestClearEmptiesThePaneAndKeepsTheDropCount(t *testing.T) {
	s := newScrollback(16)
	for i := 0; i < 60; i++ {
		s.Write([]byte("filler\n"))
	}
	_, dropped := s.stats()
	if dropped == 0 {
		t.Fatal("nothing was dropped, so this test cannot check that the count survives")
	}

	s.Write([]byte("a partial tail"))
	s.clear()

	lines, after := s.stats()
	if lines != 0 {
		t.Errorf("after clear the pane holds %d lines, expected 0 (the partial line was left behind)", lines)
	}
	if after != dropped {
		t.Errorf("after clear the drop count is %d, expected it to stay at %d", after, dropped)
	}
	if rows := rowsAt(s, 20, 5); len(rows) != 0 {
		t.Errorf("after clear the view returned %q, expected nothing", rows)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// Any goroutine may write while the render loop reads. This is here for the race
// detector rather than for its assertions.
func TestWritesFromManyGoroutinesAreSafeToViewFrom(t *testing.T) {
	s := newScrollback(64)
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				s.Write([]byte("writer\n"))
			}
		}(w)
	}
	for i := 0; i < 400; i++ {
		s.view(20+i%17, 5, i%9)
		s.stats()
	}
	for w := 0; w < 4; w++ {
		<-done
	}
	if lines, dropped := s.stats(); lines+dropped != 800 {
		t.Errorf("%d held + %d dropped = %d, expected the 800 lines written", lines, dropped, lines+dropped)
	}
}
