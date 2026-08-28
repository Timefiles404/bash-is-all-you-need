package tui

import (
	"strings"
	"testing"

	"bash-is-all-you-need/external/tui/term"
)

// plain is the colourless style, so these tests assert about layout rather than
// about escape sequences. The colour path has its own home in style.go.
var plain = style{on: false}

// ---------------------------------------------------------------------------
// renderStatus
// ---------------------------------------------------------------------------

// Sections that align only internally look like separate tables that happen to
// be adjacent, and then the eye stops comparing rows across them — which is the
// main thing this report is for.
func TestTheLabelColumnIsMeasuredAcrossEverySectionNotPerSection(t *testing.T) {
	const longest = "a-much-longer-label"
	out := renderStatus([]Section{
		{Title: "shell", Rows: []Row{{Name: "colour", Value: "VALUE-ONE"}}},
		{Title: "provider", Rows: []Row{{Name: longest, Value: "VALUE-TWO"}}},
	}, 120, plain)

	column := func(token string) int {
		for _, l := range out {
			if i := strings.Index(l, token); i >= 0 {
				return i
			}
		}
		t.Fatalf("renderStatus never drew %q:\n%s", token, strings.Join(out, "\n"))
		return -1
	}

	short, long := column("VALUE-ONE"), column("VALUE-TWO")
	if short != long {
		t.Errorf("the short section's value starts at column %d and the long section's at %d; the label column was measured per section:\n%s", short, long, strings.Join(out, "\n"))
	}
	if want := 4 + len(longest) + 2; short != want {
		t.Errorf("the value column is %d, expected %d (four of indent, the longest label, two of gap)", short, want)
	}
}

func TestRenderStatusPutsTheNoteAfterTheValueAndSkipsItWhenEmpty(t *testing.T) {
	out := renderStatus([]Section{{Rows: []Row{
		{Name: "output pane", Value: "40 lines", Note: "16 older lines dropped"},
		{Name: "colour", Value: "yes"},
	}}}, 120, plain)

	if len(out) != 2 {
		t.Fatalf("renderStatus produced %d lines, expected 2:\n%s", len(out), strings.Join(out, "\n"))
	}
	if !strings.HasSuffix(out[0], "40 lines   16 older lines dropped") {
		t.Errorf("the noted row is %q, expected the value then three spaces then the note", out[0])
	}
	if !strings.HasSuffix(out[1], "yes") {
		t.Errorf("the unnoted row is %q, expected it to end at the value", out[1])
	}
}

// A section heading with nothing under it is a heading that lies about there
// being something to read.
func TestASectionWithNoRowsIsSkippedEntirely(t *testing.T) {
	out := renderStatus([]Section{
		{Title: "empty"},
		{Title: "real", Rows: []Row{{Name: "a", Value: "1"}}},
	}, 120, plain)

	if strings.Contains(strings.Join(out, "\n"), "empty") {
		t.Errorf("the empty section's heading was still drawn:\n%s", strings.Join(out, "\n"))
	}
	if len(out) != 3 {
		t.Errorf("renderStatus produced %d lines, expected a blank line, a heading and one row:\n%s", len(out), strings.Join(out, "\n"))
	}
}

// A report line that overflows the window wraps, which pushes every line below
// it down and turns a table into a mess.
func TestEveryStatusLineIsCutToTheWindowWidth(t *testing.T) {
	out := renderStatus([]Section{{Title: "shell", Rows: []Row{
		{Name: "settings file", Value: strings.Repeat("/a-long-path", 12)},
	}}}, 30, plain)

	for i, l := range out {
		if w := term.DispWidth(l); w > 30 {
			t.Errorf("line %d is %d columns wide, expected at most 30: %q", i, w, l)
		}
	}
}

// The exported wrapper is what a host command uses to get /status's alignment
// without /status's headings.
func TestRenderRowsAlignsRowsWithNoHeadingAtAll(t *testing.T) {
	out := RenderRows([]Row{
		{Name: "tokens", Value: "1234"},
		{Name: "a-longer-name", Value: "5678"},
	}, 120)

	if len(out) != 2 {
		t.Fatalf("RenderRows produced %d lines, expected 2 with no heading and no blank line:\n%s", len(out), strings.Join(out, "\n"))
	}
	if strings.Index(out[0], "1234") != strings.Index(out[1], "5678") {
		t.Errorf("RenderRows did not align the values:\n%s", strings.Join(out, "\n"))
	}
	if strings.Contains(strings.Join(out, ""), "\x1b") {
		t.Errorf("RenderRows emitted an escape sequence; it is called with no style:\n%q", out)
	}
}

// ---------------------------------------------------------------------------
// segments
// ---------------------------------------------------------------------------

// The fields are given most-important-first, so a bar that has to lose something
// loses the token count rather than which provider is answering.
func TestSegmentsDropsFromTheRightUntilTheLineFits(t *testing.T) {
	fields := []string{"stage 12", "openai", "gpt-4o"}

	for _, tc := range []struct {
		w    int
		want string
	}{
		{40, "stage 12 · openai · gpt-4o"},
		{26, "stage 12 · openai · gpt-4o"},
		{25, "stage 12 · openai"},
		{17, "stage 12 · openai"},
		{16, "stage 12"},
		{8, "stage 12"},
	} {
		if got := segments(fields, tc.w, " · "); got != tc.want {
			t.Errorf("segments(w %d) = %q, expected %q", tc.w, got, tc.want)
		}
	}
}

// Dropping stops at the first field that does not fit rather than skipping it:
// the bar's fields read left to right as one sentence, and a hole in the middle
// of it is worse than a short one.
func TestSegmentsStopsAtTheFirstFieldThatDoesNotFit(t *testing.T) {
	fields := []string{"ab", strings.Repeat("W", 30), "cd"}
	if got := segments(fields, 12, " · "); got != "ab" {
		t.Errorf("segments = %q, expected %q — the field after the oversized one was picked up anyway", got, "ab")
	}
}

// On a very narrow terminal the provider name half-visible is still the answer
// to the question the bar exists to answer.
func TestSegmentsTruncatesTheFirstFieldRatherThanReturningAnEmptyBar(t *testing.T) {
	fields := []string{"stage 12", "openai"}

	if got := segments(fields, 7, " · "); got != "stage 1" {
		t.Errorf("segments(w 7) = %q, expected the first field cut to 7 columns", got)
	}
	if got := segments(fields, 1, " · "); got != "s" {
		t.Errorf("segments(w 1) = %q, expected one column of the first field", got)
	}
	if got := segments(fields, 0, " · "); got != "" {
		t.Errorf("segments(w 0) = %q, expected nothing", got)
	}
	if got := segments(nil, 10, " · "); got != "" {
		t.Errorf("segments(no fields) = %q, expected nothing", got)
	}
}

// The host builds the field list unconditionally and leaves out what it has
// nothing to say about, so an empty field must not become an empty column with
// separators either side of it.
func TestAnEmptyFieldIsLeftOutOfTheJoinRatherThanJoinedAsNothing(t *testing.T) {
	if got := segments([]string{"a", "", "b"}, 40, " · "); got != "a · b" {
		t.Errorf("segments = %q, expected %q", got, "a · b")
	}
	if got := segments([]string{"", "", ""}, 40, " · "); got != "" {
		t.Errorf("segments of nothing but empty fields = %q, expected nothing", got)
	}
}

// The separator is measured, not assumed: a two-column separator that was
// counted as one is how a bar ends up one column too wide and wraps.
func TestSegmentsMeasuresTheSeparatorItIsGiven(t *testing.T) {
	fields := []string{"aaa", "bbb"}
	if got := segments(fields, 7, "····"); got != "aaa" {
		t.Errorf("with a four-column separator segments(w 7) = %q, expected only the first field to fit", got)
	}
	if got := segments(fields, 10, "····"); got != "aaa····bbb" {
		t.Errorf("segments(w 10) = %q, expected both fields", got)
	}
}
