package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// jsonIsOpen — the discrimination that decides who gets blamed
// ---------------------------------------------------------------------------

// faultCut and faultNotJSON both mean "this did not parse", and they must not be
// reported as the same thing: one is the model running out of budget mid-value,
// the other is the model (or a gateway) sending something that was never JSON.
// Telling a truncated model that its JSON was invalid is a false diagnosis, and
// the model answers a false diagnosis by re-sending the same too-long command.
func TestJSONIsOpenSeparatesTruncationFromGarbage(t *testing.T) {
	open := []string{
		`{"command": "find`,
		`{"command": "find /srv/app -name '*.go' -not -path '*/vendor`,
		`{`,
		`{"command":`,
		`{"a":1,`,
		`[1,2`,
		`{"command":"c:\\`, // cut between a backslash and what it escapes
		`{"command": "ls", "shell":`,
		// A bare truncated value, with no container to be open. The unclosed
		// STRING is the only evidence here, which is what makes this the case
		// that proves the string tracking is load-bearing rather than
		// redundant with the bracket depth.
		`"find /srv`,
	}
	for _, s := range open {
		if !jsonIsOpen(s) {
			t.Errorf("jsonIsOpen(%q) = false; this is a truncation and would be reported to the model as "+
				"malformed JSON, which is a diagnosis it cannot act on", s)
		}
	}

	closed := []string{
		`description: survey the docs`,
		`I will run: echo hi`,
		`{]`,
		`{"command":"ls"}`,
		`{}`,
		``,
		`<tool_call>`,
		`{"command":"ls"} trailing words`,
		// A brace INSIDE a string value. Complete, and a scanner that does not
		// track strings counts the `{` and calls it truncated — so an
		// executable command would be refused as cut off.
		`{"command":"echo {"}`,
		// An ESCAPED quote inside the value. A scanner that ignores escapes
		// ends the string early, opens a new one at the closing quote, and
		// reports this complete call as truncated.
		`{"command":"a\""}`,
	}
	for _, s := range closed {
		if jsonIsOpen(s) {
			t.Errorf("jsonIsOpen(%q) = true; a closed payload would be reported as a truncation, so the model "+
				"is told to retry something that will fail identically", s)
		}
	}
}

// The classification has to survive a quote inside a string, because every
// argument this agent handles is a shell command and shell commands are full of
// quotes. §A3c's payloads all contain them.
func TestJSONIsOpenHandlesQuotesInsideStrings(t *testing.T) {
	// Complete: the inner quotes are escaped and the value is terminated.
	if jsonIsOpen(`{"command":"grep -Hn \"TODO(security)\" ."}`) {
		t.Error("a complete call whose value contains escaped quotes was read as truncated")
	}
	// Truncated after an escaped quote — still inside the string.
	if !jsonIsOpen(`{"command":"grep -Hn \"TODO`) {
		t.Error("a call truncated inside an escaped-quote value was read as complete")
	}
	// A closing brace INSIDE the string must not close the object.
	if !jsonIsOpen(`{"command":"find . -exec grep x {} +`) {
		t.Error("a brace inside the string value was counted as closing the object")
	}
}

// ---------------------------------------------------------------------------
// checkCall on the bash tool
// ---------------------------------------------------------------------------

func TestCheckCallOnTheBashTool(t *testing.T) {
	def := bashToolDef()

	cases := []struct {
		name      string
		raw       string
		wantFault argFault
		wantCmd   string
	}{
		{"a normal call", `{"command":"ls -la"}`, faultNone, "ls -la"},
		{"the §A3c truncation shape", `{"raw_arguments":"{\"command\": \"find"}`, faultCut, ""},
		{"truncated mid-command", `{"command":"go test ./sta`, faultCut, ""},
		{"prose", `I will list the files`, faultNotJSON, ""},
		{"the §A2 markup", `<tool_call>` + "\n" + `<function=bash>`, faultNotJSON, ""},
		{"missing the required field", `{"shell":"bash"}`, faultSchema, ""},
		{"command as a number", `{"command":42}`, faultSchema, ""},
		{"command as an array", `{"command":["echo","hi"]}`, faultSchema, ""},
		{
			// §E13: this is what the endpoint returns when asked for an array —
			// the array serialised INTO the declared type. It is schema-valid
			// and it is not a shell command, which is the limit of schema
			// validation stated as a test.
			"the array serialised into a string is accepted",
			`{"command":"[\"echo\",\"hi\"]"}`, faultNone, `["echo","hi"]`,
		},
		{"a zero-argument call is a missing required field", ``, faultSchema, ""},
		{"an explicitly empty object, likewise", `{}`, faultSchema, ""},
	}

	for _, c := range cases {
		got := checkCall(def, c.raw)
		if got.Fault != c.wantFault {
			t.Errorf("%s: fault = %q, want %q (detail %q)", c.name, got.Fault, c.wantFault, got.Detail)
			continue
		}
		if c.wantFault == faultNone && strArg(got, "command") != c.wantCmd {
			t.Errorf("%s: command = %q, want %q", c.name, strArg(got, "command"), c.wantCmd)
		}
	}
}

// The boundary must not smuggle a whole truncated payload into the history: the
// detail is replayed on every subsequent request for the rest of the session.
// The property is that the detail's size does not depend on the payload's, which
// is stronger than any particular limit: clip() adds an elision marker, so the
// exact byte count is an implementation detail and the *independence* is not.
func TestFaultDetailIsBounded(t *testing.T) {
	frag := "echo alpha bravo charlie delta; "
	short := checkCall(bashToolDef(), `{"command":"`+strings.Repeat(frag, 200))
	long := checkCall(bashToolDef(), `{"command":"`+strings.Repeat(frag, 20000))

	if short.Fault != faultCut || long.Fault != faultCut {
		t.Fatalf("faults = %q / %q, want both %q", short.Fault, long.Fault, faultCut)
	}
	// Not exactly equal: clip's marker states how many bytes it elided, so the
	// length grows with the DIGITS of the payload size. Logarithmic is bounded;
	// linear is not, and linear is what this test exists to catch.
	if grew := len(long.Detail) - len(short.Detail); grew > 8 {
		t.Errorf("detail grew %d bytes when the payload grew 100x (%d -> %d); it goes into the message array "+
			"and is re-sent every turn thereafter, so its size must not track the payload's",
			grew, len(short.Detail), len(long.Detail))
	}
	if len(long.Detail) > 2*maxDetail {
		t.Errorf("detail is %d bytes against a %d-byte budget", len(long.Detail), maxDetail)
	}
}

// ---------------------------------------------------------------------------
// The schema subset
// ---------------------------------------------------------------------------

// §E13 measured that nothing upstream enforces `enum`. If nothing here does
// either, the keyword is decoration in a request that pays tokens for it.
func TestSchemaEnforcesEnumBecauseNobodyElseDoes(t *testing.T) {
	tool := Tool{
		Name: "runner",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
				"shell":   map[string]any{"type": "string", "enum": []any{"bash", "sh"}},
			},
			"required": []string{"command", "shell"},
		},
	}

	// The exact body §E13 got back from the OpenAI route.
	got := checkCall(tool, `{"command": "echo hi", "shell": "powershell"}`)
	if got.Fault != faultSchema {
		t.Fatalf("fault = %q, want %q — the endpoint returned this verbatim with a 200 and a normal "+
			"finish_reason, so this check is the only one there is", got.Fault, faultSchema)
	}
	if !strings.Contains(got.Detail, "shell") || !strings.Contains(got.Detail, "powershell") {
		t.Errorf("the detail names neither the field nor the value: %q", got.Detail)
	}

	if got := checkCall(tool, `{"command": "echo hi", "shell": "sh"}`); got.Fault != faultNone {
		t.Errorf("an in-enum value was rejected as %q (%s)", got.Fault, got.Detail)
	}
}

// `integer` has to match a JSON number, because json.Unmarshal has already
// collapsed integers and floats into float64 by the time the check runs — the
// distinction is not observable, so refusing it would reject every integer
// argument any tool ever declares.
//
// This is also where the deliberate looseness is pinned: 5.0 and an
// out-of-range value both pass, because a refusal costs a full round trip to fix
// what a clamp at the tool fixes for nothing.
func TestIntegerPropertiesAcceptNumbers(t *testing.T) {
	tool := Tool{Name: "reader", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		},
		"required": []string{"limit"},
	}}

	for _, raw := range []string{`{"limit":20}`, `{"limit":5.0}`, `{"limit":9999}`, `{"limit":-1}`} {
		if got := checkCall(tool, raw); got.Fault != faultNone {
			t.Errorf("checkCall(%s) = %q (%s); an integer declaration that rejects numbers rejects every "+
				"integer argument, because json.Unmarshal has already made them all float64",
				raw, got.Fault, got.Detail)
		}
	}
	// A string is still a string.
	if got := checkCall(tool, `{"limit":"20"}`); got.Fault != faultSchema {
		t.Errorf(`checkCall({"limit":"20"}) = %q, want %q`, got.Fault, faultSchema)
	}
}

// `additionalProperties: false` is honoured by pruning rather than refusing.
// §E13 measured a model adding `timeout_ms` as the string "5000" against a schema
// that forbade it; the tool cannot read it, so dropping it changes nothing that
// runs, while a refusal costs a round trip.
func TestUndeclaredPropertiesArePrunedNotRefused(t *testing.T) {
	strict := Tool{
		Name: "runner",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"command": map[string]any{"type": "string"}},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
	got := checkCall(strict, `{"command": "echo hi", "timeout_ms": "5000"}`)
	if got.Fault != faultNone {
		t.Fatalf("fault = %q (%s); refusing costs a whole round trip to remove a key that was already "+
			"going to be ignored", got.Fault, got.Detail)
	}
	if _, still := got.Args["timeout_ms"]; still {
		t.Error("the undeclared key survived into Args; the schema said this field does not exist here, " +
			"and leaving it in makes the declaration decoration")
	}
	if len(got.Dropped) != 1 || got.Dropped[0] != "timeout_ms" {
		t.Errorf("Dropped = %v, want [timeout_ms]; an unreported drop is a silent divergence between what "+
			"the model asked for and what happened", got.Dropped)
	}

	// A schema that says nothing keeps the JSON Schema default and accepts them.
	lax := Tool{
		Name: "runner",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	}
	got = checkCall(lax, `{"command": "echo hi", "timeout_ms": "5000"}`)
	if len(got.Dropped) != 0 {
		t.Errorf("Dropped = %v on a schema that never forbade extras; the behaviour must follow the "+
			"declaration, not a house preference", got.Dropped)
	}
	if _, ok := got.Args["timeout_ms"]; !ok {
		t.Error("the extra key was removed anyway")
	}
}

// `required` arrives as []string from a Tool built in Go and as []any from one
// that has been through a JSON round trip — a replayed trace, a config file.
// A validator that understood only one form would pass every test and fail in
// replay.
func TestRequiredNamesAcceptsBothEncodings(t *testing.T) {
	for _, req := range []any{
		[]string{"command"},
		[]any{"command"},
	} {
		tool := Tool{Name: "runner", Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   req,
		}}
		if got := checkCall(tool, `{}`); got.Fault != faultSchema {
			t.Errorf("required as %T: a call missing the required field was accepted", req)
		}
		if got := checkCall(tool, `{"command":"ls"}`); got.Fault != faultNone {
			t.Errorf("required as %T: a valid call was rejected as %q", req, got.Fault)
		}
	}
}

// Map iteration order in Go is randomised, so an unsorted walk names a different
// field on each run — a bug report nobody can reproduce.
func TestSchemaViolationIsDeterministic(t *testing.T) {
	tool := Tool{Name: "runner", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"b": map[string]any{"type": "string"},
			"c": map[string]any{"type": "string"},
			"d": map[string]any{"type": "string"},
		},
	}}
	raw := `{"d":1,"c":2,"b":3,"a":4}`
	first := checkCall(tool, raw).Detail
	for i := 0; i < 40; i++ {
		if got := checkCall(tool, raw).Detail; got != first {
			t.Fatalf("detail differs between runs: %q then %q", first, got)
		}
	}
	if !strings.Contains(first, `"a"`) {
		t.Errorf("the first violation reported is %q; sorted order should reach \"a\" first", first)
	}
}

// ---------------------------------------------------------------------------
// The rule about replayed text
// ---------------------------------------------------------------------------

// Every string this file puts into a tool result is a permanent addition to the
// prompt. An imperative in one of them reads as a fresh instruction several turns
// later, once the context that made it sensible has scrolled away — so the model
// re-issues a call that was already handled.
//
// This is a mechanical guard on that rule, and it is worth having mechanically
// because the natural way to write these strings is as advice.
func TestReplayedTextContainsNoInstructions(t *testing.T) {
	imperatives := []string{
		"send ", "retry", "try again", "do not ", "don't ", "please ",
		"you should", "you must", "make sure", "instead, ", "next time",
	}

	var texts []string
	for _, f := range []argFault{faultCut, faultNotJSON, faultSchema} {
		texts = append(texts, faultText(bashToolDef(), argCheck{Fault: f, Detail: "the required \"command\" field is absent"}))
	}
	// The strings dispatch produces itself, quoted here because a test cannot
	// reach them without running a shell.
	texts = append(texts,
		"[not executed: the session was stopped]",
		"[not executed: the command was an empty string]",
		"[not executed: the prompt was blank, so there was no task to delegate]",
		"[not executed: the reply was cut off at max_tokens]",
		"[the user denied this subagent]",
		"[the user denied this command]",
		"[the user stopped the session]",
	)

	for _, txt := range texts {
		low := strings.ToLower(txt)
		for _, imp := range imperatives {
			if strings.Contains(low, imp) {
				t.Errorf("a replayed tool result contains the instruction %q:\n  %s\n"+
					"this string is re-sent on every subsequent request; an instruction in it will be "+
					"obeyed later, out of context", imp, txt)
			}
		}
	}
}

// The fault text must still identify the tool, or a turn with several calls
// produces several identical rejections and the model cannot tell which of its
// calls was refused.
func TestFaultTextNamesTheTool(t *testing.T) {
	for _, f := range []argFault{faultCut, faultNotJSON} {
		txt := faultText(bashToolDef(), argCheck{Fault: f, Detail: "x"})
		if !strings.Contains(txt, "bash") {
			t.Errorf("fault %q produced %q, which does not name the tool", f, txt)
		}
	}
}

// The cut message must not quote the fragment back. §A3c's payloads are hundreds
// of bytes of shell command, the model wrote them, and they would be replayed
// forever.
func TestCutTextDoesNotQuoteTheFragment(t *testing.T) {
	frag := "find /srv/app -name '*.go' -not -path '*/vendor"
	txt := faultText(bashToolDef(), argCheck{Fault: faultCut, Detail: frag})
	if strings.Contains(txt, "vendor") {
		t.Errorf("the truncated fragment was echoed into the history:\n  %s", txt)
	}
}

// ---------------------------------------------------------------------------
// uniqueIDs
// ---------------------------------------------------------------------------

// A gateway can mint one id for every call it makes. Inside a turn nothing reads
// the id but the matching result, so it works; across turns the protocol rejects
// the request for a duplicate `tool_use` id, and the rejection names a message
// index rather than the tool.
func TestUniqueIDsRenamesCollisionsAcrossTurns(t *testing.T) {
	seen := map[string]bool{}

	turn1 := []Block{{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"}}
	if n := uniqueIDs(turn1, seen); n != 0 {
		t.Fatalf("renamed %d ids on the first turn; there was nothing to collide with", n)
	}
	if turn1[0].ID != "call_go_0" {
		t.Errorf("the first use of an id was renamed to %q; only collisions should move", turn1[0].ID)
	}

	// The same id again, in a DIFFERENT assistant message. A per-turn check sees
	// nothing here, which is exactly why seen spans the session.
	turn2 := []Block{{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"}}
	if n := uniqueIDs(turn2, seen); n != 1 {
		t.Fatalf("renamed %d ids, want 1", n)
	}
	if turn2[0].ID == "call_go_0" {
		t.Error("the duplicate id survived; the next request is rejected for a duplicate tool_use id")
	}

	// And a third, so the rename is not a two-value flip-flop.
	turn3 := []Block{{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"}}
	uniqueIDs(turn3, seen)
	if turn3[0].ID == turn2[0].ID || turn3[0].ID == "call_go_0" {
		t.Errorf("third occurrence became %q, colliding again with %q", turn3[0].ID, turn2[0].ID)
	}
}

func TestUniqueIDsHandlesCollisionsWithinOneTurn(t *testing.T) {
	seen := map[string]bool{}
	calls := []Block{
		{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"},
		{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"},
		{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"},
	}
	if n := uniqueIDs(calls, seen); n != 2 {
		t.Fatalf("renamed %d of 3 identical ids, want 2", n)
	}
	ids := map[string]bool{}
	for _, c := range calls {
		if ids[c.ID] {
			t.Fatalf("duplicate id %q survived within one turn: %v", c.ID, calls)
		}
		ids[c.ID] = true
	}
}

// An id is how a result finds its call, so a call without one cannot be
// answered — and an unanswered call is a rejected request.
func TestUniqueIDsFillsInAMissingID(t *testing.T) {
	calls := []Block{{Kind: BlockToolCall, ID: "", Name: "bash"}}
	uniqueIDs(calls, map[string]bool{})
	if calls[0].ID == "" {
		t.Error("a call with no id was left without one; its result has nothing to address")
	}
}

// Gateways validate `call_id` length at 64. A rename that grows past it trades a
// duplicate-id rejection for a too-long-id rejection.
func TestUniqueIDsStaysInsideTheLengthLimit(t *testing.T) {
	// 63 characters: one under the limit, so ANY suffix pushes the rename past
	// it. A shorter base would leave room for `_2` and the cap would never be
	// reached, which is a test that passes without exercising the thing it
	// names.
	long := "call_" + strings.Repeat("x", 58)
	if len(long) != 63 {
		t.Fatalf("fixture is %d chars, not 63", len(long))
	}
	seen := map[string]bool{long: true}
	calls := []Block{{Kind: BlockToolCall, ID: long, Name: "bash"}}
	uniqueIDs(calls, seen)
	if len(calls[0].ID) > 64 {
		t.Errorf("renamed id is %d chars: %q", len(calls[0].ID), calls[0].ID)
	}
	if calls[0].ID == long {
		t.Error("the collision was not resolved")
	}
}

// Only tool calls have ids worth uniquifying, and a text block has no id at all
// — writing one would put a field on the wire that the protocol has no place
// for.
func TestUniqueIDsIgnoresNonToolCallBlocks(t *testing.T) {
	blocks := []Block{
		{Kind: BlockText, Text: "hello"},
		{Kind: BlockThinking, Text: "hmm"},
	}
	seen := map[string]bool{}
	if n := uniqueIDs(blocks, seen); n != 0 {
		t.Errorf("renamed %d ids among blocks that have none", n)
	}
	if len(seen) != 0 {
		t.Errorf("non-call blocks contributed %d ids to the seen set", len(seen))
	}
}

// ---------------------------------------------------------------------------
// stripHarnessMarkup — §A2, verbatim
// ---------------------------------------------------------------------------

// These are the exact `message.content` values the OpenAI route returned when a
// tool call was truncated, captured at three max_tokens values. All three carried
// finish_reason "length" with tool_calls as the empty array.
func TestStripHarnessMarkupOnTheObservedPayloads(t *testing.T) {
	observed := []string{
		"<tool_call>\n<function=bash>\n<parameter=",
		"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name \"*.go\" -not -path \"*/",
		"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name \"*.go\" -not -path \"*/vendor/*\" -not -path \"*/testdata/*\" -mtime -14 -exec grep -Hn \"TODO(security)\" {} +",
		"<tool_call>\n<function=b",
	}
	for _, s := range observed {
		clean, found := stripHarnessMarkup(s)
		if !found {
			t.Errorf("the gateway's own markup was not recognised: %q", s)
		}
		if clean != "" {
			t.Errorf("stripping %q left %q; the markup is the whole content here", s, clean)
		}
	}
}

// A truncated turn can have said something real before the markup started, and
// that part is the model talking to the user.
func TestStripHarnessMarkupKeepsRealTextBeforeIt(t *testing.T) {
	clean, found := stripHarnessMarkup("Let me look at that.\n\n<tool_call>\n<function=bash>")
	if !found {
		t.Fatal("markup not found")
	}
	if clean != "Let me look at that." {
		t.Errorf("clean = %q, want %q", clean, "Let me look at that.")
	}
}

// Text with no markup must come back byte-identical, or every ordinary turn is
// silently rewritten.
func TestStripHarnessMarkupLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"Here is the answer.",
		"A function=bash mention with no angle brackets.",
		"Trailing whitespace matters   ",
	} {
		clean, found := stripHarnessMarkup(s)
		if found {
			t.Errorf("markup reported in %q", s)
		}
		if clean != s {
			t.Errorf("ordinary text was rewritten: %q -> %q", s, clean)
		}
	}
}

// ---------------------------------------------------------------------------
// mergeArgs — the three dialects
// ---------------------------------------------------------------------------

func TestMergeArgsIncrementalDialect(t *testing.T) {
	// §B4's observed splitting: fragments are not JSON-aligned.
	frags := []string{`{"comm`, `and":"ec`, `ho hi"`, `}`}
	got := ""
	for _, f := range frags {
		got = mergeArgs(got, f)
	}
	if got != `{"command":"echo hi"}` {
		t.Errorf("got %q; the incremental dialect is what this endpoint actually sends", got)
	}
}

// A gateway that re-sends the complete arguments in its final chunk turns a
// blind append into `{...}{...}`, whose error names a byte offset and nothing
// about the cause.
func TestMergeArgsReSendDialect(t *testing.T) {
	got := ""
	for _, f := range []string{`{"comm`, `and":"ls"`, `}`, `{"command":"ls"}`} {
		got = mergeArgs(got, f)
	}
	if got != `{"command":"ls"}` {
		t.Errorf("got %q, want a single top-level value; a tool call's arguments are exactly one", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("the accumulated arguments do not parse: %q", got)
	}
}

func TestMergeArgsCumulativeDialect(t *testing.T) {
	got := ""
	for _, f := range []string{`{"command"`, `{"command":"l`, `{"command":"ls"}`} {
		got = mergeArgs(got, f)
	}
	if got != `{"command":"ls"}` {
		t.Errorf("got %q; each fragment was a superset of the last", got)
	}
}

// A trailing partial after a complete value is a re-send that was itself cut. The
// complete value is the one to keep — dropping it in favour of the fragment would
// turn a working call into a truncation.
func TestMergeArgsKeepsTheCompleteValueOverATrailingFragment(t *testing.T) {
	got := mergeArgs(`{"command":"ls"}`, `{"comm`)
	if got != `{"command":"ls"}` {
		t.Errorf("got %q, want the complete value", got)
	}
}

func TestMergeArgsEmptyFragmentsAreIgnored(t *testing.T) {
	if got := mergeArgs(`{"a":1}`, ""); got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
	if got := mergeArgs("", `{"a":1}`); got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
	if got := mergeArgs("", ""); got != "" {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// renderArgs — §E14's 400
// ---------------------------------------------------------------------------

// `arguments: ""` is an HTTP 400 on the OpenAI route, and a 400 is fatal, so a
// single zero-argument tool call in the history would end the session. `{}` is
// accepted. This is the symmetric half of anthropicToolInput, which stage 10 had
// on one side only.
func TestRenderArgsNeverEmitsTheEmptyString(t *testing.T) {
	for _, in := range []string{"", " ", "\t\n"} {
		if got := renderArgs(in); got != "{}" {
			t.Errorf("renderArgs(%q) = %q, want {} — the empty string is a 400 that ends the session", in, got)
		}
	}
	// Anything else passes through byte for byte: re-serialising would break
	// byte-level prompt caching, because Go randomises map iteration order.
	for _, in := range []string{`{"command":"ls"}`, `{ "command" : "ls" }`, `not json`} {
		if got := renderArgs(in); got != in {
			t.Errorf("renderArgs(%q) = %q; the bytes must pass through unchanged", in, got)
		}
	}
}

// The two protocols have to agree about a zero-argument call, in their own
// shapes: `{}` as a string here, `{}` as an object there.
func TestBothProtocolsRenderAZeroArgumentCallAsAnEmptyObject(t *testing.T) {
	if got := renderArgs(""); got != "{}" {
		t.Errorf("openai side: %q", got)
	}
	if got := string(anthropicToolInput("")); got != "{}" {
		t.Errorf("anthropic side: %q", got)
	}
}

// ---------------------------------------------------------------------------
// argsForDisplay — the lenient parse, quarantined
// ---------------------------------------------------------------------------

// A tool call must never vanish from a panel or a compaction summary: the
// transcript is the last record that the agent tried something and it did not
// work.
func TestArgsForDisplayNeverReturnsBlank(t *testing.T) {
	for _, in := range []string{
		`{"raw_arguments":""}`,
		`{"command":"  "}`,
		`{"command":""}`,
		`{"command":"go test ./sta`,
		`{}`,
		`nonsense`,
	} {
		got := argsForDisplay(in)
		if strings.TrimSpace(got) == "" {
			t.Errorf("argsForDisplay(%q) = %q; the call disappears from the transcript entirely", in, got)
		}
	}
}

func TestArgsForDisplayPrefersTheCommand(t *testing.T) {
	if got := argsForDisplay(`{"command":"ls -la"}`); got != "ls -la" {
		t.Errorf("got %q", got)
	}
	if got := argsForDisplay(`{"raw_arguments":"{\"command\": \"find"}`); got != `{"command": "find` {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// dispatch, through the boundary
// ---------------------------------------------------------------------------

// The protocol requires every tool call in an assistant message to be answered.
// Adding a validation boundary in front of dispatch is exactly the change that
// could start dropping results — a rejected call still needs its answer.
func TestDispatchAnswersEveryCallEvenWhenAllOfThemAreInvalid(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	calls := []Block{
		{Kind: BlockToolCall, ID: "c1", Name: "bash", Args: `{"raw_arguments":"{\"command\": \"find"}`},
		{Kind: BlockToolCall, ID: "c2", Name: "bash", Args: `{"command":"go test ./sta`},
		{Kind: BlockToolCall, ID: "c3", Name: "bash", Args: `I will list the files`},
		{Kind: BlockToolCall, ID: "c4", Name: "bash", Args: `{}`},
		{Kind: BlockToolCall, ID: "c5", Name: "bash", Args: `{"command":"  "}`},
		{Kind: BlockToolCall, ID: "c6", Name: "nosuchtool", Args: `{}`},
	}

	results, disp := a.dispatch(context.Background(), 1, calls)
	if disp.stop {
		t.Fatal("dispatch reported the session stopped")
	}
	if len(results) != len(calls) {
		t.Fatalf("%d results for %d calls; the provider rejects a request whose tool calls are not all answered",
			len(results), len(calls))
	}
	for i, r := range results {
		if r.ID != calls[i].ID {
			t.Errorf("result %d answers %q, not %q", i, r.ID, calls[i].ID)
		}
		if strings.TrimSpace(r.Text) == "" {
			t.Errorf("result %d (%s) is empty; the model is told nothing at all happened", i, r.ID)
		}
		if !strings.Contains(r.Text, "not executed") && !strings.Contains(r.Text, "no tool called") {
			t.Errorf("result %d (%s) does not say the call was refused: %q", i, r.ID, r.Text)
		}
	}

	// Five of the six are argument faults; the sixth is an unknown tool, which
	// is a different thing and is not one.
	var invalid int
	for _, e := range rec.events {
		if e.Kind == KindToolCallInvalid {
			invalid++
			if e.Fault == "" {
				t.Error("a tool_call_invalid event carries no fault class; the class is what you count")
			}
		}
	}
	if invalid != 5 {
		t.Errorf("%d tool_call_invalid events for 5 invalid calls; a rejection missing from the trace is a "+
			"rejection nobody can measure the rate of", invalid)
	}
}

// A refused call must not reach the shell. This is the assertion that makes the
// boundary a boundary rather than a report.
func TestDispatchRunsNothingForARefusedCall(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	// The repaired form of §A3c's truncation. If the boundary let it through,
	// this would run `find` with no arguments and list the whole tree.
	calls := []Block{
		{Kind: BlockToolCall, ID: "c1", Name: "bash", Args: `{"raw_arguments":"{\"command\": \"find"}`},
	}
	a.dispatch(context.Background(), 1, calls)

	for _, e := range rec.events {
		switch e.Kind {
		case KindCommandStart, KindCommandEnd, KindToolCallReady:
			t.Errorf("a refused call produced %q — it reached the shell", e.Kind)
		case KindGateVerdict:
			t.Error("a refused call reached the permission gate; the human is being asked about a command " +
				"the model never finished writing")
		}
	}
}

// The undeclared-key drop is a notice, not a tool result: the model asked for
// something the tool does not have, which is worth a human seeing and is not
// worth history on every subsequent request.
func TestDispatchReportsADroppedArgumentAsANoticeOnly(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	calls := []Block{
		{Kind: BlockToolCall, ID: "c1", Name: "bash",
			Args: `{"command":"echo s11-drop","timeout_ms":5000}`},
	}
	results, _ := a.dispatch(context.Background(), 1, calls)

	if !strings.Contains(results[0].Text, "s11-drop") {
		t.Errorf("the command did not run despite being valid: %q", results[0].Text)
	}
	if strings.Contains(results[0].Text, "timeout_ms") {
		t.Errorf("the dropped key was reported in the tool result, which is replayed forever: %q", results[0].Text)
	}
	var noticed bool
	for _, e := range rec.events {
		if e.Kind == KindNotice && strings.Contains(e.Text, "timeout_ms") {
			noticed = true
		}
	}
	if !noticed {
		t.Error("the dropped argument was not reported anywhere; a silent drop is a divergence between what " +
			"the model asked for and what happened")
	}
}
