package tui

import (
	"strings"
	"testing"
	"time"

	"bash-is-all-you-need/external/tui/term"
)

func mdOn() *md { return newMD(style{on: true}) }

// visible strips the escape sequences from s, leaving the characters a terminal
// would actually draw. term.ANSILen is the same scanner the pane's own width and
// wrapping use, so this measures what the pane measures.
func visible(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if n := term.ANSILen(s, i); n > 0 {
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// mdLines is the corpus the three whole-file invariants are checked against. It
// is fed to one md in order, so the fence it opens is the fence the lines after
// it are rendered inside.
var mdLines = []string{
	"",
	"   ",
	"plain prose with nothing in it at all",
	"# A top heading",
	"## Second level with `code` in it",
	"###### Sixth level",
	"####### seven hashes are not a heading",
	"> a quoted line with **bold** in it",
	"- a bullet",
	"  * an indented bullet",
	"+ a plus bullet",
	"12) an ordered item",
	"  1. an indented ordered item",
	"---",
	"* * *",
	"___",
	"a `code span` in the middle of a sentence",
	"**bold** and *em* and __also bold__ and _also em_",
	"see [the docs](https://example.com/a_b) for more",
	"| header | second |",
	"|--------|--------|",
	"| a `b` c | **d** |",
	"a*b is not emphasis and snake_case is not either",
	"an unclosed *marker and a lone _ one and a stray ` tick",
	"日本語のテキストに**強調**を混ぜてみる",
	"trailing spaces are preserved   ",
	"```go",
	"\tfor i := range xs { *p = xs[i] }",
	"```",
	"1234567890123. is a number, not a list",
}

// ---------------------------------------------------------------------------
// The three invariants that hold for every line
// ---------------------------------------------------------------------------

// A pane can be redirected to a file, and an escape sequence that survives into
// a text file is a bug report rather than a colour.
func TestMDWithStyleOffReturnsEveryLineExactlyAsItArrived(t *testing.T) {
	m := newMD(style{})
	for _, s := range mdLines {
		if got := m.line(s); got != s {
			t.Errorf("with colour off, line(%q) = %q, expected the line untouched", s, got)
		}
	}
}

// Something upstream already styled this line. Our sequences close by resetting
// everything, so re-styling it would take the caller's colour off part way along
// its own line.
func TestMDLeavesALineThatIsAlreadyStyledAlone(t *testing.T) {
	for _, s := range []string{
		"\x1b[31malready red\x1b[0m",
		"# \x1b[1ma heading someone else styled\x1b[0m",
		"- \x1b[36mbullet\x1b[0m with **bold** after it",
		"\x1b",
	} {
		if got := mdOn().line(s); got != s {
			t.Errorf("line(%q) = %q, expected the line untouched: it already contains ESC", s, got)
		}
	}
}

// The pane wraps on display width. A renderer that changed it would tear the
// layout on the next resize, so this is the assertion that matters most here.
func TestMDNeverChangesTheDisplayWidthOfALine(t *testing.T) {
	m := mdOn()
	for _, s := range mdLines {
		got := m.line(s)
		if w, want := term.DispWidth(got), term.DispWidth(s); w != want {
			t.Errorf("line(%q) is %d columns wide, expected %d\n  rendered as %q", s, w, want, got)
		}
	}
}

// The stronger form of the same claim, and the one that explains it: markers are
// dimmed rather than stripped, so every byte of the input comes out in order and
// nothing else does.
func TestMDEmitsEveryByteOfTheLineAndNothingButEscapes(t *testing.T) {
	m := mdOn()
	for _, s := range mdLines {
		if got := visible(m.line(s)); got != s {
			t.Errorf("line(%q) draws %q, expected the same characters", s, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Fenced code
// ---------------------------------------------------------------------------

func TestMDCarriesFenceStateBetweenLinesAndClosesItOnTheNextFence(t *testing.T) {
	m := mdOn()

	if got, want := m.line("```go"), "\x1b[2m```go\x1b[0m"; got != want {
		t.Fatalf("line(%q) = %q, expected %q", "```go", got, want)
	}
	if !m.fence {
		t.Fatal("the opening fence did not set the fence state, so the block's body will be read as prose")
	}

	// Inside a fence nothing is markup: an asterisk is a glob and a hash is a
	// comment.
	for _, s := range []string{"p = *q + _r_", "# not a heading in here", "- not a bullet"} {
		if got, want := m.line(s), "\x1b[2m"+s+"\x1b[0m"; got != want {
			t.Errorf("inside a fence, line(%q) = %q, expected the whole line dim and nothing else", s, got)
		}
	}

	if got, want := m.line("```"), "\x1b[2m```\x1b[0m"; got != want {
		t.Fatalf("line(%q) = %q, expected %q", "```", got, want)
	}
	if m.fence {
		t.Fatal("the closing fence did not clear the fence state, so the rest of the reply stays dim")
	}
	if got, want := m.line("# A heading"), "\x1b[2m# \x1b[0m\x1b[1;36mA heading\x1b[0m"; got != want {
		t.Errorf("after the fence closed, line(%q) = %q, expected %q", "# A heading", got, want)
	}
}

// The pane can be cleared in the middle of a code block, and a fence flag left
// set would then dim every line of whatever came next.
func TestMDResetDropsACarriedFence(t *testing.T) {
	m := mdOn()
	m.line("```")
	if !m.fence {
		t.Fatal("the opening fence did not set the fence state")
	}

	m.reset()
	if m.fence {
		t.Fatal("reset left the fence state set")
	}
	if got, want := m.line("# A heading"), "\x1b[2m# \x1b[0m\x1b[1;36mA heading\x1b[0m"; got != want {
		t.Errorf("after reset, line(%q) = %q, expected %q", "# A heading", got, want)
	}
}

// ---------------------------------------------------------------------------
// What each construct renders as
// ---------------------------------------------------------------------------

func TestMDRendersTheBlockAndInlineConstructs(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			"top heading is bold and coloured",
			"# One",
			"\x1b[2m# \x1b[0m\x1b[1;36mOne\x1b[0m",
		},
		{
			"a deeper heading keeps the weight and drops the colour",
			"### Three",
			"\x1b[2m### \x1b[0m\x1b[1mThree\x1b[0m",
		},
		{
			"the deepest level is still a heading",
			"###### Six",
			"\x1b[2m###### \x1b[0m\x1b[1mSix\x1b[0m",
		},
		{
			// The heading's attributes and the marker's arrive as one sequence:
			// a nested reset would end the heading part way along the line.
			"inline markup inside a heading carries the heading's own attributes",
			"## A `b` c",
			"\x1b[2m## \x1b[0m\x1b[1;36mA \x1b[0m\x1b[1;36;2m`\x1b[0m\x1b[1;36;33mb\x1b[0m\x1b[1;36;2m`\x1b[0m\x1b[1;36m c\x1b[0m",
		},
		{
			"a blockquote dims its marker and its body",
			"> quoted",
			"\x1b[2m>\x1b[0m\x1b[2m quoted\x1b[0m",
		},
		{
			"a bullet is coloured and left as the character it was",
			"- item",
			"\x1b[36m-\x1b[0m item",
		},
		{
			"an ordered marker keeps its indent and its delimiter",
			"  1. item",
			"  \x1b[36m1.\x1b[0m item",
		},
		{
			"a horizontal rule is dim end to end",
			"---",
			"\x1b[2m---\x1b[0m",
		},
		{
			"a spaced rule is a rule, not a bullet",
			"* * *",
			"\x1b[2m* * *\x1b[0m",
		},
		{
			"a code span keeps its backticks",
			"a `x` b",
			"a \x1b[2m`\x1b[0m\x1b[33mx\x1b[0m\x1b[2m`\x1b[0m b",
		},
		{
			"bold keeps both pairs of asterisks",
			"**bold**",
			"\x1b[2m**\x1b[0m\x1b[1mbold\x1b[0m\x1b[2m**\x1b[0m",
		},
		{
			"emphasis is italic",
			"*em*",
			"\x1b[2m*\x1b[0m\x1b[3mem\x1b[0m\x1b[2m*\x1b[0m",
		},
		{
			"underscores are the same two constructs",
			"__also bold__",
			"\x1b[2m__\x1b[0m\x1b[1malso bold\x1b[0m\x1b[2m__\x1b[0m",
		},
		{
			// The underscore in the URL is inside the dimmed run and is never
			// looked at, which is what "no recursion" buys.
			"a link underlines its text and dims the rest",
			"[docs](https://x/a_b)",
			"\x1b[2m[\x1b[0m\x1b[4mdocs\x1b[0m\x1b[2m](https://x/a_b)\x1b[0m",
		},
		{
			"a table row dims the pipes and leaves the cells where they are",
			"| a | b |",
			"\x1b[2m|\x1b[0m a \x1b[2m|\x1b[0m b \x1b[2m|\x1b[0m",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mdOn().line(tc.in); got != tc.want {
				t.Errorf("line(%q) =\n  %q\nexpected\n  %q", tc.in, got, tc.want)
			}
		})
	}
}

// Writing `**x**` and getting literal asterisks is the whole point of a code
// span, so code has to be decided before emphasis.
func TestMDInlineCodeBeatsEmphasis(t *testing.T) {
	const in = "`**x**`"
	got := mdOn().line(in)
	want := "\x1b[2m`\x1b[0m\x1b[33m**x**\x1b[0m\x1b[2m`\x1b[0m"
	if got != want {
		t.Errorf("line(%q) =\n  %q\nexpected\n  %q", in, got, want)
	}
	if strings.Contains(got, "\x1b["+sgrBold+"m") {
		t.Errorf("line(%q) = %q: the asterisks inside the code span were read as bold", in, got)
	}
}

// A marker that opens nothing is text. Every line here must come back byte for
// byte, because a half-applied style is worse than none: it tells the reader
// there is markup where there is only prose.
func TestMDLeavesMarkersThatOpenNothingCompletelyAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"an asterisk inside a word", "a*b is not emphasis"},
		{"snake case", "snake_case_name stays a name"},
		{"multiplication", "5 * 3 = 15"},
		{"an opener with no closer", "*unclosed emphasis"},
		{"a doubled opener with no closer", "**unclosed bold"},
		{"a doubled marker with nothing after it", "the operator is **"},
		{"a backtick with no closer", "a ` tick that never closes"},
		{"brackets that are not a link", "[not a link] here"},
		{"a link with no closing paren", "[text](unclosed"},
		{"seven hashes", "####### not a heading"},
		{"too many digits to be a list", "1234567890123. is a number"},
		{"trailing spaces", "trailing spaces are kept   "},
		// Emphasis does not open straight after a multi-byte character: the byte
		// before the marker reads as a letter. See md.opens.
		{"asterisks after CJK text", "日本語に**強調**は開かない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mdOn().line(tc.in); got != tc.in {
				t.Errorf("line(%q) = %q, expected the line untouched", tc.in, got)
			}
		})
	}
}

// The scan for a closing marker remembers that it failed. Without that memory
// each of these groups walks every asterisk after it, which is quadratic in the
// number of groups: this line renders in under two milliseconds as it stands and
// did not finish in two minutes with the memory taken out.
//
// The deadline is what makes that a test rather than a slow test. It is four
// orders of magnitude above the real cost, so it says nothing about how fast the
// machine is, only that the work did not change shape.
func TestMDDoesNotRescanTheTailForEveryUnclosedMarker(t *testing.T) {
	in := strings.Repeat("*a ", 100000) + "and [b (c `d"
	done := make(chan string, 1)
	go func() { done <- mdOn().line(in) }()

	select {
	case got := <-done:
		if got != in {
			t.Errorf("a line of unclosed markers came back changed:\n  %.60q…", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rendering a line of 100000 unclosed markers took over five seconds; the failed-scan memory in closer is gone and the scan is quadratic")
	}
}
