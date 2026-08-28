package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"bash-is-all-you-need/external/tui/term"
)

// The compact view, the key that turns it off, and the two things that hang off
// the same classification: what is rendered as Markdown, and what the status row
// and the composer's border report.

// ---------------------------------------------------------------------------
// What the compact view folds
// ---------------------------------------------------------------------------

// Hiding a run of instrumentation silently would be worse than showing it:
// output that stops mid-way with no mark reads as the program having failed, and
// the reader has no way to learn that a key would bring it back. So the count is
// part of the claim, and so is the key.
func TestARunOfDetailLinesFoldsToOnePlaceholderThatCountsThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"one line", 1, "  ⋯ 1 line hidden · ctrl-o"},
		{"a run of many", 7, "  ⋯ 7 lines hidden · ctrl-o"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScrollback(100, style{})
			s.setClass(ClassDetail)
			for i := 0; i < tc.n; i++ {
				s.Write([]byte(fmt.Sprintf("panel row %d\n", i)))
			}

			rows := rowsAt(s, 40, 20)
			if len(rows) != 1 {
				t.Fatalf("%d detail lines drew %d rows (%q), expected one placeholder", tc.n, len(rows), rows)
			}
			if rows[0] != tc.want {
				t.Errorf("the placeholder reads %q, expected %q", rows[0], tc.want)
			}
		})
	}
}

// Only ClassDetail is instrumentation. The shell's own output and the model's
// prose are what the compact view exists to leave on the screen, so neither may
// be folded whatever else is around it.
func TestPlainAndProseLinesAreNeverFolded(t *testing.T) {
	s := newScrollback(100, style{})
	s.Write([]byte("the shell said this\n"))
	s.setClass(ClassProse)
	s.Write([]byte("the model said this\n"))

	if got := strings.Join(rowsAt(s, 40, 20), "|"); got != "the shell said this|the model said this" {
		t.Errorf("the compact view shows %q, expected both lines", got)
	}
	if n := s.folded(); n != 0 {
		t.Errorf("folded() = %d with nothing but plain and prose in the pane, expected 0", n)
	}
}

// The open run has to be forgotten when an unfolded line interrupts it. Left
// set, the second run keeps rewriting the first run's placeholder: one row
// counting every detail line in the pane, drawn above output that came after
// most of them, and the second run's own rows never appear at all.
func TestTwoRunsOfDetailSeparatedByAPlainLineGetTwoPlaceholders(t *testing.T) {
	s := newScrollback(100, style{})
	s.setClass(ClassDetail)
	s.Write([]byte("first panel\nsecond panel\n"))
	s.setClass(ClassPlain)
	s.Write([]byte("between them\n"))
	s.setClass(ClassDetail)
	s.Write([]byte("third panel\nfourth panel\nfifth panel\n"))

	want := []string{
		"  ⋯ 2 lines hidden · ctrl-o",
		"between them",
		"  ⋯ 3 lines hidden · ctrl-o",
	}
	if got := rowsAt(s, 40, 20); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the compact view shows\n  %q\nexpected\n  %q", got, want)
	}
	if n := s.folded(); n != 5 {
		t.Errorf("folded() = %d, expected the 5 detail lines behind the two placeholders", n)
	}
}

// Ctrl-O's whole promise: the lines are hidden, not lost.
func TestTheFullViewShowsEveryLineAndTheCompactViewPutsThePlaceholdersBack(t *testing.T) {
	s := newScrollback(100, style{})
	s.setClass(ClassDetail)
	s.Write([]byte("panel one\npanel two\n"))
	s.setClass(ClassPlain)
	s.Write([]byte("a command echo\n"))

	compact := strings.Join(rowsAt(s, 40, 20), "|")
	if compact != "  ⋯ 2 lines hidden · ctrl-o|a command echo" {
		t.Fatalf("the compact view shows %q", compact)
	}

	s.setDetail(true)
	full := strings.Join(rowsAt(s, 40, 20), "|")
	if full != "panel one|panel two|a command echo" {
		t.Errorf("the full view shows %q, expected every line", full)
	}
	if strings.Contains(full, "hidden") {
		t.Errorf("a placeholder survived into the full view: %q", full)
	}

	s.setDetail(false)
	if again := strings.Join(rowsAt(s, 40, 20), "|"); again != compact {
		t.Errorf("back in the compact view the pane shows %q, expected %q", again, compact)
	}
}

// The wrap cache is keyed on the view mode as well as on the width, and the
// check that says so is easy to leave out because nothing else notices: rows
// built for the other mode are still rows, so a stale cache reads as a key that
// did nothing rather than as a bug.
func TestTogglingTheViewModeInvalidatesTheWrapCache(t *testing.T) {
	s := newScrollback(100, style{})
	s.setClass(ClassDetail)
	s.Write([]byte("panel one\npanel two\n"))

	// The first view is what builds the cache. Without it there is nothing
	// stale to serve and the test would pass against a cache that is never
	// invalidated at all.
	if got := rowsAt(s, 40, 20); len(got) != 1 {
		t.Fatalf("the compact view shows %q, expected one placeholder", got)
	}

	s.setDetail(true)
	if got := rowsAt(s, 40, 20); strings.Join(got, "|") != "panel one|panel two" {
		t.Fatalf("after the toggle the pane shows %q, expected both lines", got)
	}

	// And the rebuilt cache is still incremental: a line written after the
	// toggle joins the rows already there rather than starting the pane again.
	s.Write([]byte("panel three\n"))
	if got := rowsAt(s, 40, 20); strings.Join(got, "|") != "panel one|panel two|panel three" {
		t.Errorf("after a write in the full view the pane shows %q, expected all three lines", got)
	}

	s.setDetail(false)
	if got := rowsAt(s, 40, 20); strings.Join(got, "|") != "  ⋯ 3 lines hidden · ctrl-o" {
		t.Errorf("back in the compact view the pane shows %q, expected one placeholder for all three", got)
	}
}

// folded() is what the composer's border and /status report. In the full view
// nothing is hidden, and a count of lines that are all on screen would send a
// reader looking for output already in front of them.
func TestFoldedCountsTheHiddenLinesOnlyWhileTheyAreHidden(t *testing.T) {
	s := newScrollback(100, style{})
	s.setClass(ClassDetail)
	for i := 0; i < 12; i++ {
		s.Write([]byte("panel\n"))
	}
	s.setClass(ClassPlain)
	s.Write([]byte("visible\n"))

	if n := s.folded(); n != 12 {
		t.Errorf("folded() = %d in the compact view, expected the 12 detail lines", n)
	}
	s.setDetail(true)
	if n := s.folded(); n != 0 {
		t.Errorf("folded() = %d in the full view, expected 0: nothing is hidden there", n)
	}
	s.setDetail(false)
	if n := s.folded(); n != 12 {
		t.Errorf("folded() = %d back in the compact view, expected 12 again", n)
	}
}

// The hidden count is maintained as lines arrive rather than recomputed, and the
// drop path is where that goes wrong: the batch decrements it in a loop, and an
// off-by-one there is invisible until the number goes negative.
func TestTheHiddenCountSurvivesTheDropOldestPath(t *testing.T) {
	const capLines = 16
	s := newScrollback(capLines, style{})

	// Seven classes to a cycle against a drop batch of two, so each drop takes a
	// different mix of them. A pattern whose period divided the batch would take
	// the same lines every time, and a decrement that was one out would then be
	// wrong by a constant this test could accidentally agree with.
	cycle := []Class{ClassDetail, ClassPlain, ClassDetail, ClassProse, ClassDetail, ClassDetail, ClassPlain}
	name := map[Class]string{ClassPlain: "plain", ClassProse: "prose", ClassDetail: "detail"}
	for i := 0; i < 200; i++ {
		c := cycle[i%len(cycle)]
		s.setClass(c)
		s.Write([]byte(name[c] + "\n"))
	}
	if _, dropped := s.stats(); dropped == 0 {
		t.Fatalf("nothing was dropped from a pane capped at %d lines, so this test is not about the drop path", capLines)
	}

	// Counted from the full view rather than from the counter under test, so the
	// two sides of the comparison are arrived at independently: this is the
	// number of lines a reader would actually be missing.
	s.setDetail(true)
	held := 0
	for _, r := range rowsAt(s, 40, 4*capLines) {
		if r == "detail" {
			held++
		}
	}
	s.setDetail(false)

	if held == 0 {
		t.Fatal("no detail lines were retained, so there is nothing here to count")
	}
	if n := s.folded(); n != held {
		t.Errorf("folded() = %d, expected %d — the pane still holds that many detail lines", n, held)
	}
}

// ---------------------------------------------------------------------------
// Ctrl-O through the loop
// ---------------------------------------------------------------------------

// ctrlO is the byte a terminal sends for Ctrl-O: the decoder reads control bytes
// 0x01..0x1a as the matching lowercase letter with Ctrl set.
const ctrlO = "\x0f"

// instruments writes n detail lines into a running shell's pane and puts the
// class back where it found it, so that anything the loop itself echoes
// afterwards is not folded along with them.
func instruments(a *App, n int) {
	a.SetClass(ClassDetail)
	for i := 0; i < n; i++ {
		a.Printf("panel row %d\n", i)
	}
	a.SetClass(ClassPlain)
}

func TestCtrlOSwitchesThePaneBetweenTheCompactViewAndTheFullOne(t *testing.T) {
	s := newShell(t, Config{})
	s.start()

	instruments(s.app, 3)
	s.waitFor("the placeholder", func() bool { return s.hasRow("  ⋯ 3 lines hidden · ctrl-o") })
	if s.shows("panel row 0") {
		t.Fatalf("the detail lines are on screen before ctrl-o was pressed:\n%s", s.screen())
	}

	s.send(ctrlO)
	s.waitFor("the folded lines to come back", func() bool {
		f := s.shot()
		return f.shows("panel row 0") && f.shows("panel row 2")
	})
	f := s.shot()
	if f.shows("hidden · ctrl-o") {
		t.Errorf("a placeholder is still on screen in the full view:\n%s", f.text)
	}
	if !f.shows("showing everything") {
		t.Errorf("the hint row does not say which view the key just moved to:\n%s", f.text)
	}

	s.send(ctrlO)
	s.waitFor("the lines to fold away again", func() bool {
		f := s.shot()
		return f.hasRow("  ⋯ 3 lines hidden · ctrl-o") && !f.shows("panel row 0")
	})
	if !s.shows("compact view") {
		t.Errorf("the hint row does not name the view ctrl-o went back to:\n%s", s.screen())
	}
}

// The key is checked before the state machine, and this is the whole reason for
// that: the moment you want the detail is the moment something is going wrong in
// front of you, and a key that only worked at an idle prompt would ask you to
// wait for the thing you are trying to watch.
func TestCtrlOWorksWhileATurnIsRunningRatherThanBeingRefusedAsTyping(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)

	s := newShell(t, Config{Submit: func(ctx context.Context, line string) error {
		entered <- struct{}{}
		// Either signal will do. The context is watched as well as the release
		// channel so that a failed assertion, which leaves the release to a
		// deferred close, cannot park this goroutine past the end of the test.
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}})
	s.start()

	instruments(s.app, 4)
	s.waitFor("the placeholder", func() bool { return s.hasRow("  ⋯ 4 lines hidden · ctrl-o") })

	s.send("go\r")
	recv(t, entered, "Submit to be called")
	// A hidden caret is how the loop says the keyboard is no longer going into
	// the prompt, so it is the frame-level proof that the turn has started.
	s.waitFor("the turn to be what is on screen", func() bool { return s.caretHidden() })

	s.send(ctrlO)
	s.waitFor("the detail to come back mid-turn", func() bool { return s.shows("panel row 0") })
	f := s.shot()
	if f.shows("a turn is running") {
		t.Errorf("ctrl-o was refused as typing while the turn ran:\n%s", f.text)
	}
	if !f.shows("showing everything") {
		t.Errorf("the hint row does not report the view the key changed to:\n%s", f.text)
	}
}

// A permission prompt is one of the moments the detail matters most — the
// question is about a command whose arguments are in the panel the compact view
// folded away — and a key pressed to read it must not also answer it.
func TestCtrlOWorksWhileAQuestionIsPendingAndDoesNotAnswerIt(t *testing.T) {
	asked := make(chan struct{}, 1)
	got := make(chan string, 1)

	var app *App
	s := newShell(t, Config{Submit: func(ctx context.Context, line string) error {
		asked <- struct{}{}
		a, _ := app.Ask("run rm -rf /? [y/n] ")
		got <- a
		return nil
	}})
	app = s.app
	s.start()

	instruments(app, 2)
	s.waitFor("the placeholder", func() bool { return s.hasRow("  ⋯ 2 lines hidden · ctrl-o") })

	s.send("go\r")
	recv(t, asked, "Submit to be called")
	s.waitFor("the question", func() bool { return s.shows("run rm -rf /?") })

	s.send(ctrlO)
	s.waitFor("the detail to come back with the question still up", func() bool {
		f := s.shot()
		return f.shows("panel row 0") && f.shows("run rm -rf /?")
	})
	if len(got) != 0 {
		t.Fatalf("ctrl-o answered the permission prompt with %q", <-got)
	}

	// And the prompt still takes an answer afterwards, so the key was consumed
	// rather than the question being left in a state nothing can reach.
	s.send("y\r")
	if a := recv(t, got, "Ask to return"); a != "y" {
		t.Errorf("Ask returned %q, expected the answer typed after ctrl-o", a)
	}
}

// ---------------------------------------------------------------------------
// Markdown, where it meets the pane
// ---------------------------------------------------------------------------

// The renderer is chosen by class rather than by looking at the text, so the
// same characters are prose on one line and output on the next. The shell's own
// output is full of asterisks and backticks that mean something else.
func TestOnlyProseLinesAreRenderedAsMarkdown(t *testing.T) {
	const text = "**bold** and `code`"

	prose := newScrollback(100, style{on: true})
	prose.setClass(ClassProse)
	prose.Write([]byte(text + "\n"))

	rows := rowsAt(prose, 60, 5)
	if len(rows) != 1 {
		t.Fatalf("the prose line drew %d rows (%q), expected one", len(rows), rows)
	}
	if !strings.Contains(rows[0], "\x1b[") {
		t.Errorf("the prose line is %q, expected it rendered", rows[0])
	}
	if visible(rows[0]) != text {
		t.Errorf("the rendered line reads %q, expected %q: the markers are dimmed, not removed", visible(rows[0]), text)
	}

	plain := newScrollback(100, style{on: true})
	plain.Write([]byte(text + "\n"))
	if got := rowsAt(plain, 60, 5); len(got) != 1 || got[0] != text {
		t.Errorf("the same bytes written as output are %q, expected %q untouched", got, text)
	}
}

// A model writes a paragraph as one logical line and sends no newline until the
// end of it, so the line being streamed is usually the whole of what the reader
// is watching. Rendering it only once it was complete would leave the reply in
// raw asterisks for as long as the model took to finish the sentence.
func TestThePartialLineIsRenderedWhileItIsStillBeingWritten(t *testing.T) {
	prose := newScrollback(100, style{on: true})
	prose.setClass(ClassProse)
	prose.Write([]byte("**bold**"))

	rows := rowsAt(prose, 60, 5)
	if len(rows) != 1 {
		t.Fatalf("the partial line drew %d rows (%q), expected one", len(rows), rows)
	}
	if !strings.Contains(rows[0], "\x1b[") {
		t.Errorf("the partial line is %q, expected it rendered like the complete ones", rows[0])
	}
	if visible(rows[0]) != "**bold**" {
		t.Errorf("the partial line reads %q, expected the markers left where they are", visible(rows[0]))
	}

	plain := newScrollback(100, style{on: true})
	plain.Write([]byte("**bold**"))
	if got := rowsAt(plain, 60, 5); len(got) != 1 || got[0] != "**bold**" {
		t.Errorf("an unclassified partial line is %q, expected %q untouched", got, "**bold**")
	}
}

// A detail line still being written is folded on the same rule as a finished
// one, and this pins which of the two possible behaviours is intended.
//
// The other one is what the code did first: fold only completed lines, which
// draws the partial in full and then makes it disappear the instant its newline
// arrives. Both are defensible in isolation — you can watch the line being
// written — but a view whose whole job is to hide detail should not show detail
// and then take it away, because the reader is told something is there and then
// told it is not.
func TestADetailLineIsFoldedWhileItIsStillBeingWrittenToo(t *testing.T) {
	s := newScrollback(100, style{})
	s.Write([]byte("visible\n"))
	s.setClass(ClassDetail)
	s.Write([]byte("half a panel line, no newline yet"))

	rows := rowsAt(s, 60, 5)
	for _, r := range rows {
		if strings.Contains(r, "half a panel") {
			t.Errorf("the compact view drew an unfinished detail line: %q\nall rows: %q", r, rows)
		}
	}

	// And the full view still shows it, so what folding hides is the only
	// difference between the two modes.
	s.setDetail(true)
	if got := rowsAt(s, 60, 5); !strings.Contains(strings.Join(got, "\n"), "half a panel") {
		t.Errorf("the full view hid an unfinished detail line: %q", got)
	}
}

// Previewing must not advance the fence state. A partial line is rendered again
// on every frame, so a chunk that happens to start with three backticks would
// otherwise toggle the fence thirty times a second, and the rest of the session
// would come out dimmed or not depending on where the repaints landed.
func TestPreviewingAPartialFenceDoesNotOpenIt(t *testing.T) {
	s := newScrollback(100, style{on: true})
	s.setClass(ClassProse)
	s.Write([]byte("```"))

	// An odd number of frames, so a preview that toggled the fence would leave
	// it in the opposite state to the right one rather than in an accidental
	// match.
	for i := 0; i < 5; i++ {
		rowsAt(s, 60, 5)
	}

	s.Write([]byte("\n"))        // the fence opens here, once
	s.Write([]byte("a *b* c\n")) // inside it, so the asterisks are not emphasis
	s.Write([]byte("```\n"))     // and closes here
	s.Write([]byte("after\n"))

	rows := rowsAt(s, 60, 10)
	if len(rows) != 4 {
		t.Fatalf("the pane holds %q, expected four lines", rows)
	}
	if want := s.st.dim("a *b* c"); rows[1] != want {
		t.Errorf("the line inside the fence is %q, expected %q: the fence was not open when it arrived", rows[1], want)
	}
	if rows[3] != "after" {
		t.Errorf("the line after the closing fence is %q, expected it untouched: the fence is still open", rows[3])
	}
}

// The pane can be cleared in the middle of a code block, and the line that would
// have closed the fence goes with it. Left alone, the flag would dim the rest of
// the session.
func TestClearingThePaneClosesAnOpenFence(t *testing.T) {
	s := newScrollback(100, style{on: true})
	s.setClass(ClassProse)
	s.Write([]byte("```\ninside the block\n"))

	rows := rowsAt(s, 60, 5)
	if len(rows) != 2 || rows[1] != s.st.dim("inside the block") {
		t.Fatalf("the pane shows %q, expected the second line dimmed inside the fence", rows)
	}

	s.clear()
	s.Write([]byte("ordinary prose\n"))
	if got := rowsAt(s, 60, 5); len(got) != 1 || got[0] != "ordinary prose" {
		t.Errorf("after clear the pane shows %q, expected %q: the fence outlived the lines that opened it", got, "ordinary prose")
	}
}

// ---------------------------------------------------------------------------
// The status row and the composer's border
// ---------------------------------------------------------------------------

// The label is furniture you read once and then stop seeing; the value is the
// thing you are actually checking. A row where "ctx" and "12345 tokens" are
// equally loud is a row nobody reads.
func TestASegmentDrawsItsLabelDimAndItsValueInTheToneItWasGiven(t *testing.T) {
	on := style{on: true}
	seg := Segment{Label: "ctx", Value: "12345 tokens", Tone: ToneWarn}

	got := seg.render(on)
	if want := on.dim("ctx") + " " + on.yellow("12345 tokens"); got != want {
		t.Errorf("the segment renders as %q, expected %q", got, want)
	}
	// The status row measures text() and draws render(); the two disagreeing is
	// how a row is fitted by counting characters nobody can see.
	if visible(got) != seg.text() {
		t.Errorf("what is drawn (%q) is not what the fit is measured on (%q)", visible(got), seg.text())
	}
	if off := seg.render(style{}); off != "ctx 12345 tokens" {
		t.Errorf("with colour off the segment is %q, expected plain text carrying both halves", off)
	}

	// A value on its own is the common case, and it must not acquire a leading
	// space from the label that is not there.
	if got := (Segment{Value: "gpt-4o", Tone: ToneAccent}).render(on); got != on.cyan("gpt-4o") {
		t.Errorf("an unlabelled segment renders as %q, expected just the value", got)
	}

	// The host says what a value means; this package decides what that looks
	// like, which is the whole reason Tone is not a colour.
	for _, tc := range []struct {
		tone Tone
		want string
	}{
		{ToneNormal, "v"},
		{ToneAccent, on.cyan("v")},
		{ToneGood, on.green("v")},
		{ToneWarn, on.yellow("v")},
		{ToneBad, on.red("v")},
		{ToneMuted, on.dim("v")},
	} {
		if got := on.tone(tc.tone, "v"); got != tc.want {
			t.Errorf("tone %d renders as %q, expected %q", tc.tone, got, tc.want)
		}
	}
}

// The rule is drawn straight into a fixed-width frame, so "exactly w" is not a
// tidiness claim: a row one column over wraps, which pushes every line below it
// down and turns a cosmetic bug into a corrupted frame.
func TestTheRuleIsExactlyAsWideAsTheWindowWhateverTagItCarries(t *testing.T) {
	tags := []string{
		"",
		"⋯1",
		"60/100 ↓",
		"60/100 ↓ · ⋯12",
		"字字字字", // wide runes, so the tag's width is not its length
		strings.Repeat("tag", 30),
	}
	for _, w := range []int{0, 1, 2, 3, 4, 8, 12, 20, 40, 60, 120} {
		for _, tag := range tags {
			for _, ends := range [][2]rune{{'╭', '╮'}, {'╰', '╯'}} {
				got := rule(ends[0], ends[1], w, tag)
				if n := term.DispWidth(got); n != w {
					t.Errorf("rule(%q) at width %d is %d columns: %q", tag, w, n, got)
				}
			}
		}
	}
}

// A tag that does not fit is dropped whole. Half of "60/100" is not a smaller
// truth, and a tag cut off inside the border reads as a corrupted frame rather
// than as a shortened label.
func TestATagTooLongForTheRuleIsDroppedRatherThanCutIntoTheBorder(t *testing.T) {
	const w = 20
	bare := "╭" + strings.Repeat("─", w-2) + "╮"

	// Where the boundary falls is arithmetic, not a claim. What is asserted is
	// that either side of it is whole: the tag is drawn as it was given, or the
	// row is nothing but border.
	for tw := 1; tw <= 16; tw++ {
		tag := strings.Repeat("x", tw)
		got := rule('╭', '╮', w, tag)
		if n := term.DispWidth(got); n != w {
			t.Errorf("a %d-column tag made a %d-column rule: %q", tw, n, got)
		}
		if !strings.Contains(got, tag) && got != bare {
			t.Errorf("a %d-column tag was cut into the border: %q", tw, got)
		}
	}

	// And both outcomes really do happen, or the loop above would be satisfied
	// by a rule that never drew a tag at all.
	if got := rule('╭', '╮', w, "⋯7"); !strings.Contains(got, "⋯7") {
		t.Errorf("a short tag was dropped from a %d-column rule: %q", w, got)
	}
	if got := rule('╭', '╮', w, strings.Repeat("tag", 30)); got != bare {
		t.Errorf("an oversized tag left something behind: %q, expected the bare border", got)
	}
}

// Every composer row is padded to the same width, so the right-hand border is a
// straight line rather than one that follows the text in and out.
func TestTheComposersBorderPadsEveryRowToTheSameWidth(t *testing.T) {
	const w = 40
	a := newTestApp(t, Config{Title: "stage 12"})
	a.setSize(w, 12)
	a.ed.insert("one\na longer second line")

	lines := mustFrame(t, a)
	top, bottom := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "╭"):
			top = i
		case strings.HasPrefix(l, "╰"):
			bottom = i
		}
	}
	if top < 0 || bottom <= top {
		t.Fatalf("the frame has no bordered composer:\n%s", strings.Join(lines, "\n"))
	}

	rows := lines[top+1 : bottom]
	if len(rows) != 2 {
		t.Fatalf("the composer drew %d rows, expected the two lines that were typed:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	for i, r := range rows {
		if n := term.DispWidth(r); n != w {
			t.Errorf("composer row %d is %d columns wide, expected %d: %q", i, n, w, r)
		}
		if !strings.HasPrefix(r, "│ ") || !strings.HasSuffix(r, " │") {
			t.Errorf("composer row %d is not inside the border: %q", i, r)
		}
	}
}

// ---------------------------------------------------------------------------
// What /status says about the view
// ---------------------------------------------------------------------------

// Somebody reading this row has usually just noticed output going missing and
// has come here to find out why, so it has to say how to get it back rather than
// only that it is gone.
func TestTheStatusReportNamesTheViewAndHowManyLinesItIsHiding(t *testing.T) {
	if got := viewMode(false); got != "compact" {
		t.Errorf("viewMode(false) = %q, expected %q", got, "compact")
	}
	if got := viewMode(true); got != "everything" {
		t.Errorf("viewMode(true) = %q, expected %q", got, "everything")
	}
	if got := foldNote(0); got != "" {
		t.Errorf("foldNote(0) = %q, expected nothing: a note saying zero lines are folded is noise", got)
	}
	if got := foldNote(3); !strings.Contains(got, "3") || !strings.Contains(got, "ctrl-o") {
		t.Errorf("foldNote(3) = %q, expected both the count and the key that undoes it", got)
	}

	a := newTestApp(t, Config{})
	a.back.setClass(ClassDetail)
	a.back.Write([]byte("panel\npanel\npanel\n"))

	row := statusRowNamed(t, a.shellStatus(), "view")
	if row.Value != "compact" || row.Note != foldNote(3) {
		t.Errorf("the view row reads %q / %q, expected the compact view and its three folded lines", row.Value, row.Note)
	}

	a.back.setDetail(true)
	row = statusRowNamed(t, a.shellStatus(), "view")
	if row.Value != "everything" || row.Note != "" {
		t.Errorf("in the full view the row reads %q / %q, expected nothing folded", row.Value, row.Note)
	}
}

func statusRowNamed(t *testing.T, secs []Section, name string) Row {
	t.Helper()
	for _, s := range secs {
		for _, r := range s.Rows {
			if r.Name == name {
				return r
			}
		}
	}
	t.Fatalf("the report has no %q row", name)
	return Row{}
}
