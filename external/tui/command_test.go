package tui

import (
	"context"
	"io"
	"strings"
	"testing"
)

// reg builds a registry from bare names, which is all most of these cases need.
func reg(names ...string) *registry {
	r := &registry{}
	for _, n := range names {
		r.add(Command{Name: n})
	}
	return r
}

func namesOf(cs []Command) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return strings.Join(out, " ")
}

// ---------------------------------------------------------------------------
// find
// ---------------------------------------------------------------------------

// Without the exact-match-first rule, adding /provider-url would silently break
// /provider — the sort of regression that only shows up in a bug report from
// someone else. The order the two are registered in must not matter either.
func TestAnExactMatchBeatsALongerPrefix(t *testing.T) {
	for _, order := range [][]string{
		{"/provider", "/provider-url"},
		{"/provider-url", "/provider"},
	} {
		r := reg(order...)
		c, hits := r.find("/provider")
		if c.Name != "/provider" {
			t.Errorf("registered as %v, find(\"/provider\") returned %q with candidates %q, expected /provider outright", order, c.Name, namesOf(hits))
		}
		if hits != nil {
			t.Errorf("registered as %v, find(\"/provider\") also returned candidates %q, expected none", order, namesOf(hits))
		}
	}
}

// Prefix matching is still what makes /prov reach /provider, so long as only one
// command can be meant.
func TestAnUnambiguousPrefixResolvesToItsOneCommand(t *testing.T) {
	r := reg("/provider", "/status", "/help")
	if c, hits := r.find("/prov"); c.Name != "/provider" || hits != nil {
		t.Errorf("find(\"/prov\") returned %q with candidates %q, expected /provider", c.Name, namesOf(hits))
	}
	if c, hits := r.find("/provider-"); c.Name != "" || len(hits) != 0 {
		t.Errorf("find(\"/provider-\") returned %q with candidates %q, expected nothing: no command starts with it", c.Name, namesOf(hits))
	}
}

func TestAnAmbiguousPrefixReturnsNoCommandAndTheSortedCandidates(t *testing.T) {
	// Registered out of order on purpose: the candidate list is what the user
	// reads, and an unsorted one reads as a bug.
	r := reg("/settings-forget", "/status", "/settings")

	c, hits := r.find("/s")
	if c.Name != "" {
		t.Errorf("find(\"/s\") resolved to %q, expected no command", c.Name)
	}
	if got := namesOf(hits); got != "/settings /settings-forget /status" {
		t.Errorf("find(\"/s\") offered %q, expected \"/settings /settings-forget /status\"", got)
	}
}

func TestFindOnANameNothingStartsWithReturnsNothingAtAll(t *testing.T) {
	r := reg("/help", "/status")
	if c, hits := r.find("/zzz"); c.Name != "" || len(hits) != 0 {
		t.Errorf("find(\"/zzz\") returned %q with candidates %q, expected nothing", c.Name, namesOf(hits))
	}
}

func TestNamesAreReportedSorted(t *testing.T) {
	r := reg("/zeta", "/alpha", "/mid")
	if got := strings.Join(r.names(), " "); got != "/alpha /mid /zeta" {
		t.Errorf("names() = %q, expected them sorted", got)
	}
}

// ---------------------------------------------------------------------------
// complete
// ---------------------------------------------------------------------------

// Tab inserts the longest common prefix, so that typing continues from the last
// character every candidate agrees on.
func TestCompleteReturnsTheLongestCommonPrefix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cmds       []string
		prefix     string
		wantCommon string
		wantHits   string
	}{
		{"a shared stem", []string{"/provider-url", "/provider-model", "/help"}, "/prov", "/provider-", "/provider-model /provider-url"},
		{"one candidate completes fully", []string{"/provider-url", "/help"}, "/prov", "/provider-url", "/provider-url"},
		{"nothing shared past the slash", []string{"/alpha", "/beta"}, "/", "/", "/alpha /beta"},
		{"an exact name is its own completion", []string{"/help", "/helpful"}, "/help", "/help", "/help /helpful"},
		{"no candidates", []string{"/help"}, "/z", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common, hits := reg(tc.cmds...).complete(tc.prefix)
			if common != tc.wantCommon {
				t.Errorf("complete(%q) returned the common prefix %q, expected %q", tc.prefix, common, tc.wantCommon)
			}
			if got := namesOf(hits); got != tc.wantHits {
				t.Errorf("complete(%q) offered %q, expected %q", tc.prefix, got, tc.wantHits)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// splitCommand
// ---------------------------------------------------------------------------

func TestSplitCommandSeparatesTheNameFromTheWholeRestOfTheLine(t *testing.T) {
	for _, tc := range []struct{ line, name, arg string }{
		{"/open", "/open", ""},
		{"/open .", "/open", "."},
		{"  /open   /some/dir  ", "/open", "/some/dir"},
		{"/open\t/some/dir", "/open", "/some/dir"},
		{"/provider-model gpt-4o mini", "/provider-model", "gpt-4o mini"},
		{"", "", ""},
		{"   ", "", ""},
	} {
		name, arg := splitCommand(tc.line)
		if name != tc.name || arg != tc.arg {
			t.Errorf("splitCommand(%q) = (%q, %q), expected (%q, %q)", tc.line, name, arg, tc.name, tc.arg)
		}
	}
}

// ---------------------------------------------------------------------------
// Masking
// ---------------------------------------------------------------------------

// maskFrom runs on every frame against the partly-typed line. The claim it makes
// is that an unfinished name counts as secret if any secret command starts with
// it, because otherwise the first characters of the key are visible for exactly
// as long as it takes to finish typing the command name.
func TestMaskFromCoversAnIncompleteSecretCommandName(t *testing.T) {
	r := &registry{}
	r.add(
		Command{Name: "/provider-apikey", Secret: true},
		Command{Name: "/provider-url"},
		Command{Name: "/help"},
	)

	for _, tc := range []struct {
		line string
		want int
		why  string
	}{
		{"/provider-apikey", -1, "no argument has been typed yet, so there is nothing to mask"},
		{"/provider-apikey ", 17, "the space is the argument's first column, even before a character lands in it"},
		{"/provider-apikey sk-abc", 17, "the key itself"},
		{"/prov sk-abc", 6, "an unfinished name that a secret command starts with must already mask"},
		{"/p sk-abc", 3, "one character is enough to be a prefix of the secret command"},
		{"/provider-url https://x", -1, "a command that is found and is not secret"},
		{"/help me", -1, "a command with no secret to hide"},
		{"/nothing like this", -1, "no command matches and none is secret"},
		{"hello there", -1, "not a command at all"},
		{"", -1, "an empty line"},
		{"/", -1, "a lone slash, with no space, has no argument yet"},
		{"/ sk-abc", 2, "a lone slash is a prefix of the secret command"},
	} {
		if got := r.maskFrom(tc.line); got != tc.want {
			t.Errorf("maskFrom(%q) = %d, expected %d: %s", tc.line, got, tc.want, tc.why)
		}
	}
}

// The offset is in runes, because that is what editor.render indexes by. A byte
// offset would start masking mid-character on the first CJK argument.
func TestMaskFromCountsRunesNotBytes(t *testing.T) {
	r := &registry{}
	r.add(Command{Name: "/密钥", Secret: true})

	// "/密钥" is three runes and seven bytes.
	if got := r.maskFrom("/密钥 abc"); got != 4 {
		t.Errorf("maskFrom(%q) = %d, expected 4 runes (a byte offset would say %d)", "/密钥 abc", got, len("/密钥")+1)
	}
}

// secret decides on the command, not on the argument, so a line that names a
// secret command is kept out of the echo and the history whatever follows it.
func TestSecretIsDecidedByTheCommandAlone(t *testing.T) {
	r := &registry{}
	r.add(
		Command{Name: "/provider-apikey", Secret: true},
		Command{Name: "/help"},
	)

	for _, tc := range []struct {
		line string
		want bool
	}{
		{"/provider-apikey sk-abc", true},
		{"/provider-apikey", true},
		{"/provider-a", true}, // resolved by prefix
		{"/help", false},
		{"/help /provider-apikey", false},
		{"/nosuch sk-abc", false},
		{"plain text", false},
	} {
		if got := r.secret(tc.line); got != tc.want {
			t.Errorf("secret(%q) = %v, expected %v", tc.line, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// onOff
// ---------------------------------------------------------------------------

// An empty argument means "flip it", which is what people type; an explicit
// value means set it, which is what a paste from the docs does. Anything else is
// an error rather than a guess, because guessing wrong on /yolo runs commands
// nobody approved.
func TestOnOffFlipsWhenEmptyAndRefusesAnythingItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		arg     string
		current bool
		want    bool
		wantErr bool
	}{
		{"", false, true, false},
		{"", true, false, false},
		{"on", false, true, false},
		{"yes", false, true, false},
		{"true", false, true, false},
		{"1", false, true, false},
		{"off", true, false, false},
		{"no", true, false, false},
		{"false", true, false, false},
		{"0", true, false, false},
		{"ON", false, true, false},
		{"  Off  ", true, false, false},
		{"maybe", true, true, true},
		{"maybe", false, false, true},
		{"2", true, true, true},
		{"onn", false, false, true},
	} {
		got, err := onOff(tc.arg, tc.current)
		if (err != nil) != tc.wantErr {
			t.Errorf("onOff(%q, %v) returned err %v, expected an error: %v", tc.arg, tc.current, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("onOff(%q, %v) = %v, expected %v", tc.arg, tc.current, got, tc.want)
		}
		if tc.wantErr && got != tc.current {
			t.Errorf("onOff(%q, %v) failed but returned %v; a failed parse must leave the value alone", tc.arg, tc.current, got)
		}
	}
}

// ---------------------------------------------------------------------------
// help
// ---------------------------------------------------------------------------

func TestHelpListsTheGroupsInTheOrderTheyWereFirstSeenWithUngroupedLast(t *testing.T) {
	r := &registry{}
	r.add(
		Command{Name: "/b", Group: "second", Help: "bee"},
		Command{Name: "/a", Group: "first", Help: "ay"},
		Command{Name: "/c", Group: "second", Help: "see"},
		Command{Name: "/d", Help: "dee"},
	)

	out := strings.Join(r.help("", 80), "\n")
	iSecond, iFirst, iD := strings.Index(out, "second"), strings.Index(out, "first"), strings.Index(out, "/d")
	if iSecond < 0 || iFirst < 0 || iD < 0 {
		t.Fatalf("help left something out:\n%s", out)
	}
	if !(iSecond < iFirst && iFirst < iD) {
		t.Errorf("help ordered the sections as second=%d first=%d ungrouped=%d, expected first-seen order with the ungrouped commands last:\n%s", iSecond, iFirst, iD, out)
	}
	if strings.Index(out, "/b") > strings.Index(out, "/c") {
		t.Errorf("within a group the commands are not sorted:\n%s", out)
	}
}

// A truncated help line is the one line in the program that must not need a
// wider terminal to be understood, so on a narrow window the description moves
// to its own line instead.
func TestHelpMovesTheDescriptionOntoItsOwnLineWhenTheWindowIsNarrow(t *testing.T) {
	r := &registry{}
	r.add(Command{Name: "/provider-protocol", Args: "<openai|anthropic>", Help: "set and save the wire protocol"})

	wide := r.help("", 120)
	if len(wide) != 1 {
		t.Fatalf("at 120 columns help produced %d lines, expected one two-column line:\n%s", len(wide), strings.Join(wide, "\n"))
	}
	if !strings.Contains(wide[0], "/provider-protocol") || !strings.Contains(wide[0], "wire protocol") {
		t.Errorf("the wide line is %q, expected the name and the description together", wide[0])
	}

	narrow := r.help("", 40)
	if len(narrow) != 2 {
		t.Fatalf("at 40 columns help produced %d lines, expected the name and the description on separate lines:\n%s", len(narrow), strings.Join(narrow, "\n"))
	}
	if strings.Contains(narrow[0], "wire protocol") {
		t.Errorf("the narrow name line is %q, expected the description to have moved off it", narrow[0])
	}
	if !strings.Contains(narrow[1], "wire protocol") {
		t.Errorf("the narrow second line is %q, expected the description", narrow[1])
	}
}

func TestHelpOnOneTopicAcceptsItWithOrWithoutTheSlash(t *testing.T) {
	r := &registry{}
	r.add(Command{Name: "/status", Help: "everything this session does"})

	for _, topic := range []string{"/status", "status", "/stat"} {
		out := strings.Join(r.help(topic, 80), "\n")
		if !strings.Contains(out, "/status") || !strings.Contains(out, "everything this session does") {
			t.Errorf("help(%q) produced %q, expected the command and its description", topic, out)
		}
	}
}

func TestHelpOnAnUnknownOrAmbiguousTopicSaysWhichItIs(t *testing.T) {
	r := &registry{}
	r.add(Command{Name: "/settings"}, Command{Name: "/settings-forget"}, Command{Name: "/help"})

	if out := strings.Join(r.help("/zzz", 80), "\n"); !strings.Contains(out, "no command matches /zzz") {
		t.Errorf("help on an unknown topic produced %q, expected \"no command matches /zzz\"", out)
	}

	out := strings.Join(r.help("/set", 80), "\n")
	if !strings.Contains(out, "/set is ambiguous") {
		t.Errorf("help on an ambiguous topic produced %q, expected it to say so", out)
	}
	for _, want := range []string{"/settings", "/settings-forget"} {
		if !strings.Contains(out, want) {
			t.Errorf("help on an ambiguous topic left out %q:\n%s", want, out)
		}
	}
}

func TestLabelAppendsTheArgumentPlaceholderOnlyWhenThereIsOne(t *testing.T) {
	if got := label(Command{Name: "/clear"}); got != "/clear" {
		t.Errorf("label of a command with no argument = %q, expected %q", got, "/clear")
	}
	if got := label(Command{Name: "/open", Args: "<dir>"}); got != "/open <dir>" {
		t.Errorf("label = %q, expected %q", got, "/open <dir>")
	}
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

// A host command with the same name shadows nothing: the registry keeps both and
// the first exact match wins, so a duplicate is a bug the host author sees
// immediately rather than a silent override.
func TestADuplicateNameKeepsBothCommandsAndTheFirstOneWins(t *testing.T) {
	first := make(chan struct{}, 1)
	r := &registry{}
	r.add(Command{Name: "/x", Help: "the builtin", Run: func(context.Context, string, io.Writer) error {
		first <- struct{}{}
		return nil
	}})
	r.add(Command{Name: "/x", Help: "the host's"})

	c, _ := r.find("/x")
	if c.Help != "the builtin" {
		t.Errorf("find(\"/x\") resolved to the one helped %q, expected the first registered", c.Help)
	}
	if len(r.cmds) != 2 {
		t.Errorf("the registry holds %d commands, expected both of them to be kept", len(r.cmds))
	}
	if err := c.Run(context.Background(), "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Error("the first registration's Run was not the one that ran")
	}
}

func TestAGroupIsRecordedOnceInFirstSeenOrder(t *testing.T) {
	r := &registry{}
	r.add(
		Command{Name: "/a", Group: "beta"},
		Command{Name: "/b", Group: "alpha"},
		Command{Name: "/c", Group: "beta"},
		Command{Name: "/d"},
	)
	if got := strings.Join(r.groups, " "); got != "beta alpha" {
		t.Errorf("groups = %q, expected \"beta alpha\": an empty group is not a group and a repeat is not a new one", got)
	}
}
