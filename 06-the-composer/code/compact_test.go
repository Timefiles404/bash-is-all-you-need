package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// padded returns a string of exactly n characters that begins with prefix.
//
// The budget fixtures need exact message sizes: plan()'s arithmetic is only
// worth asserting if the test can predict where the budget boundary lands
// instead of guessing at it.
func padded(prefix string, n int) string {
	if len(prefix) >= n {
		return prefix[:n]
	}
	return prefix + strings.Repeat("x", n-len(prefix))
}

// bashArgs builds a well-formed bash tool-call payload of exactly n characters.
func bashArgs(n int) string {
	const head, tail = `{"command":"`, `"}`
	body := n - len(head) - len(tail)
	if body < 1 {
		body = 1
	}
	return head + strings.Repeat("e", body) + tail
}

// convFixture is the conversation every cut-point test runs against: three
// human turns, an assistant message that both talks and issues two parallel
// tool calls, a single user message carrying both results, and a plain reply.
//
// The index map, because every test below reasons about it:
//
//	 0 user       "how big is this repo?"      ILLEGAL — a user turn
//	 1 assistant   text + two parallel calls   legal
//	 2 user        both tool results           ILLEGAL — would orphan two results
//	 3 assistant   text + one call             legal
//	 4 user        one tool result             ILLEGAL — would orphan a result
//	 5 assistant   text                        legal
//	 6 user       "now count the tests"        ILLEGAL — a user turn
//	 7 assistant   one call, no text           legal
//	 8 user        one tool result             ILLEGAL — would orphan a result
//	 9 assistant   text                        legal
//	10 user       "and the docs?"              ILLEGAL — a user turn
//	11 assistant   text + one call             legal
//	12 user        one tool result             ILLEGAL — would orphan a result
//	13 assistant   text                        legal
func convFixture() []Msg {
	return []Msg{
		// 0
		TextMsg(RoleUser, "how big is this repo?"),
		// 1 — text AND tool calls in one assistant message, which is the shape
		// a naive "assistant messages are just text" cutter gets wrong.
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "I'll count the files and the lines at the same time."},
			{Kind: BlockToolCall, ID: "toolu_aa1", Name: "bash", Args: `{"command":"find . -name '*.go' | wc -l"}`},
			{Kind: BlockToolCall, ID: "toolu_bb2", Name: "bash", Args: `{"command":"find . -name '*.go' | xargs wc -l | tail -1"}`},
		}},
		// 2 — one message answers both calls; that is the Anthropic shape.
		{Role: RoleUser, Blocks: []Block{
			ToolResultBlock("toolu_aa1", "21\n[exit 0 · 12ms]"),
			ToolResultBlock("toolu_bb2", "  9184 total\n[exit 0 · 31ms]"),
		}},
		// 3
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "21 files, 9184 lines. Checking how much of that is tests."},
			{Kind: BlockToolCall, ID: "toolu_cc3", Name: "bash", Args: `{"command":"wc -l *_test.go | tail -1"}`},
		}},
		// 4
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_cc3", "  3120 total\n[exit 0 · 9ms]")}},
		// 5
		TextMsg(RoleAssistant, "About a third of the repo is tests: 3120 of 9184 lines."),
		// 6
		TextMsg(RoleUser, "now count the tests themselves"),
		// 7 — a tool call with no accompanying text.
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: "toolu_dd4", Name: "bash", Args: `{"command":"grep -c '^func Test' *_test.go"}`},
		}},
		// 8
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_dd4", "cache_test.go:8\ntrace_test.go:11\n[exit 0 · 7ms]")}},
		// 9
		TextMsg(RoleAssistant, "19 test functions across two files."),
		// 10
		TextMsg(RoleUser, "and the docs?"),
		// 11
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Counting the prose too."},
			{Kind: BlockToolCall, ID: "toolu_ee5", Name: "bash", Args: `{"command":"wc -w docs/*.md | tail -1"}`},
		}},
		// 12
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_ee5", "  41022 total\n[exit 0 · 5ms]")}},
		// 13
		TextMsg(RoleAssistant, "41k words of docs against 9k lines of code."),
	}
}

// convFixtureToolIDs are every tool-call id in convFixture, for the test that
// says none of them may reach the summariser.
var convFixtureToolIDs = []string{"toolu_aa1", "toolu_bb2", "toolu_cc3", "toolu_dd4", "toolu_ee5"}

// toolTailFixture ends in tool results with nothing after them — the state a
// conversation is in the instant its commands finish, which is exactly when the
// wall check runs and compaction is considered.
func toolTailFixture() []Msg {
	return []Msg{
		TextMsg(RoleUser, "run the suite and the vet pass"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Both, in parallel."},
			{Kind: BlockToolCall, ID: "toolu_tail1", Name: "bash", Args: `{"command":"go test ./..."}`},
			{Kind: BlockToolCall, ID: "toolu_tail2", Name: "bash", Args: `{"command":"go vet ./..."}`},
		}},
		{Role: RoleUser, Blocks: []Block{
			ToolResultBlock("toolu_tail1", "ok  bash-is-all-you-need  0.412s\n[exit 0]"),
			ToolResultBlock("toolu_tail2", "[exit 0]"),
		}},
	}
}

// budgetFixture is a uniform conversation for plan()'s arithmetic: every
// message is exactly 400 characters, which is exactly 100 tokens once the
// estimator is pinned at 4.0 characters per token. That lets a test aim the
// keep budget at a chosen index and know where it will land.
//
// The repeating block is user / assistant-call / tool-result / assistant, so
// indices where i%4 is 0 or 2 are illegal cut points and the odd ones are legal.
func budgetFixture(blocks int) []Msg {
	var msgs []Msg
	for i := range blocks {
		id := fmt.Sprintf("toolu_b%02d", i)
		msgs = append(msgs,
			TextMsg(RoleUser, padded(fmt.Sprintf("step %d: ", i), 400)),
			Msg{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockToolCall, ID: id, Name: "bash", Args: bashArgs(396)},
			}},
			Msg{Role: RoleUser, Blocks: []Block{ToolResultBlock(id, padded(fmt.Sprintf("output %d: ", i), 400))}},
			TextMsg(RoleAssistant, padded(fmt.Sprintf("done %d: ", i), 400)),
		)
	}
	return msgs
}

// pinnedCompactor returns a compactor whose estimator has been calibrated to
// exactly 4 characters per token, so the tests can do the budget arithmetic on
// paper instead of chasing the 3.6 cold start.
func pinnedCompactor(window int, threshold, keepRatio float64) *compactor {
	c := newCompactor(window, threshold, keepRatio)
	c.est.observe(4000, 1000)
	return c
}

// requireUniform fails the test if budgetFixture has drifted away from the
// 400-characters-per-message assumption the budget arithmetic rests on.
func requireUniform(t *testing.T, msgs []Msg) {
	t.Helper()
	for i, m := range msgs {
		if got := msgChars(m); got != 400 {
			t.Fatalf("budgetFixture message %d is %d characters, not 400 — every budget number in this test is now wrong", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// The cut-point invariant
// ---------------------------------------------------------------------------

// The invariant the whole of compact.go exists to protect: if canCutBefore says
// a cut is allowed, the conversation that cut produces must be one the API will
// accept. Asserted at every index rather than a hand-picked one, because the
// bug this guards against is a cut that is legal at index 5 and orphans a tool
// result at index 9 — which a single-index test would never see.
func TestEveryLegalCutProducesASendableConversation(t *testing.T) {
	msgs := convFixture()

	legal := 0
	for i := -1; i <= len(msgs)+1; i++ {
		if i < 0 || i > len(msgs) {
			if canCutBefore(msgs, i) {
				t.Errorf("canCutBefore allowed out-of-range index %d; the slice that follows would panic", i)
			}
			continue
		}
		if !canCutBefore(msgs, i) {
			continue
		}
		legal++
		out := append([]Msg{summaryMsg("s")}, msgs[i:]...)
		if why := validConversation(out); why != "" {
			t.Errorf("cutting before message %d is allowed but produces an unsendable conversation: %s\n"+
				"the API rejects this on the NEXT request, so the error will point at the request builder, not at the compactor", i, why)
		}
	}

	// Without this the whole test passes vacuously against a canCutBefore that
	// simply returns false everywhere — which would also make compaction
	// impossible, silently.
	if legal < 4 {
		t.Fatalf("only %d of %d indices are cuttable; the fixture no longer exercises the invariant", legal, len(msgs))
	}
}

// The mirror of the test above: the two reasons a cut is refused, each present
// in the fixture and each named. A canCutBefore that never refuses anything
// would pass the invariant test only by never being asked.
func TestCutPointsAreRejectedForTheRightReason(t *testing.T) {
	msgs := convFixture()

	// Index 2 answers the two parallel calls made in message 1. Cutting here
	// deletes the calls and keeps the answers.
	const orphan = 2
	hasResult := false
	for _, b := range msgs[orphan].Blocks {
		if b.Kind == BlockToolResult {
			hasResult = true
		}
	}
	if !hasResult {
		t.Fatalf("fixture drift: message %d was supposed to carry tool results", orphan)
	}
	if canCutBefore(msgs, orphan) {
		t.Errorf("cutting before message %d is allowed, but that message answers the tool calls in message %d — "+
			"the results would be orphaned and the provider rejects an unmatched tool_use_id", orphan, orphan-1)
	}

	// Index 6 is the second human turn. The summary is injected as a user
	// message, so cutting here puts two user messages back to back.
	const userTurnIdx = 6
	if msgs[userTurnIdx].Role != RoleUser {
		t.Fatalf("fixture drift: message %d was supposed to be a user turn", userTurnIdx)
	}
	if canCutBefore(msgs, userTurnIdx) {
		t.Errorf("cutting before message %d is allowed, but it is a user message and the summary is also a user message — "+
			"the request would carry two user turns in a row, which some endpoints reject and others silently merge", userTurnIdx)
	}
}

// canCutBefore checks for tool results *and* for the role, and the reason the
// tool-result check is not redundant is this case: the neutral Msg type lets any
// block sit in any role, so the day a new adapter puts a result in an assistant
// message, a role-only check goes on returning true and ships an orphan.
func TestCanCutBeforeRejectsAToolResultWhateverRoleCarriesIt(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: "t1", Name: "bash", Args: `{"command":"ls"}`},
		}},
		// An assistant message carrying the result. Unusual, and precisely the
		// shape the role check cannot see.
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolResult, ID: "t1", Text: "a\nb\n[exit 0]"},
		}},
		TextMsg(RoleUser, "thanks"),
	}
	if canCutBefore(msgs, 2) {
		t.Error("a message holding a tool result was accepted as a cut point because its role was assistant; " +
			"the matching call is in the message about to be deleted, so the next request carries an orphan")
	}
}

// safeCut searches forward — toward dropping more — because compaction runs when
// the window is nearly full. A backward search frees less than asked for and the
// agent hits the wall again on the very next call.
func TestSafeCutNeverSearchesBackward(t *testing.T) {
	msgs := convFixture()
	moved := 0
	for k := -2; k <= len(msgs)+2; k++ {
		got := safeCut(msgs, k)
		if got < 0 {
			continue
		}
		if got < k {
			t.Errorf("safeCut(msgs, %d) = %d: it searched backward and kept more history than the caller asked to free, "+
				"so this compaction frees less than intended and the next call hits the wall again", k, got)
		}
		if !canCutBefore(msgs, got) {
			t.Errorf("safeCut(msgs, %d) = %d, which is not a legal cut point", k, got)
		}
		if got > k && k >= 1 {
			moved++
		}
	}
	if moved == 0 {
		t.Fatal("safeCut never had to move a requested index forward; the fixture no longer exercises the search")
	}
}

// The tail case: when everything from `want` onward is a tool result, there is
// no legal cut and safeCut must say so rather than return the least-bad index.
// Returning an illegal index here is worse than refusing to compact, because
// refusing costs a slow turn and cutting wrong costs a malformed request.
func TestSafeCutReturnsMinusOneWhenTheTailIsAllToolResults(t *testing.T) {
	msgs := toolTailFixture()
	if why := validConversation(msgs); why != "" {
		t.Fatalf("fixture drift: the tail fixture is not a valid conversation to begin with: %s", why)
	}
	if got := safeCut(msgs, len(msgs)-1); got != -1 {
		t.Errorf("safeCut returned %d for a conversation whose tail is nothing but tool results; "+
			"there is no assistant message to cut before, so the only correct answer is -1", got)
	}
}

// ---------------------------------------------------------------------------
// validConversation
// ---------------------------------------------------------------------------

func TestValidConversationAcceptsAWellFormedConversation(t *testing.T) {
	if why := validConversation(convFixture()); why != "" {
		t.Errorf("a well-formed conversation was rejected: %s — every legal cut in this file would now be reported as a bug", why)
	}
}

// The state a conversation is in while the tools are still running: the last
// assistant message has issued a call and nothing has answered it yet. A naive
// "every call must have a result" check flags this and makes the agent refuse to
// compact exactly when it most needs to, at the top of a tool loop.
func TestValidConversationAcceptsAnInFlightToolCall(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "list /srv"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Looking."},
			{Kind: BlockToolCall, ID: "toolu_inflight", Name: "bash", Args: `{"command":"ls -la /srv"}`},
		}},
	}
	if why := validConversation(msgs); why != "" {
		t.Errorf("an unanswered tool call in the FINAL message was reported as a problem: %s\n"+
			"that is the normal state between issuing a command and running it, not an error", why)
	}
}

// The orphan: a result whose call was cut away. This is the failure compaction
// causes when it cuts in the wrong place, and it surfaces one request later.
func TestValidConversationRejectsAnOrphanToolResult(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "carry on"),
		TextMsg(RoleAssistant, "sure"),
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_gone", "  9184 total\n[exit 0]")}},
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("a tool result with no matching call was accepted; the provider will reject the request with an unexpected tool_use_id " +
			"and nothing in this codebase will have warned about it first")
	}
	if !strings.Contains(why, "toolu_gone") {
		t.Errorf("the complaint does not name the offending call id, so it cannot be found in a transcript: %s", why)
	}
}

// The mirror image of the orphan, and the more insidious one: the call survived
// but its answer was dropped, so the model believes a command it issued produced
// nothing at all.
func TestValidConversationRejectsAnUnansweredToolCall(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "count them"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: "toolu_lost", Name: "bash", Args: `{"command":"wc -l *.go"}`},
		}},
		TextMsg(RoleUser, "actually never mind"),
		TextMsg(RoleAssistant, "ok"),
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("a tool call that is never answered, and is not the final message, was accepted")
	}
	if !strings.Contains(why, "toolu_lost") {
		t.Errorf("the complaint does not name the unanswered call: %s", why)
	}
}

func TestValidConversationRejectsConsecutiveSameRole(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "one thing"),
		TextMsg(RoleUser, "and another"),
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("two user messages in a row were accepted; this is what injecting the summary before a user turn produces, " +
			"and endpoints disagree about whether to merge or reject it")
	}
	if !strings.Contains(why, "alternate") {
		t.Errorf("the complaint does not say what the rule is: %s", why)
	}
}

// The empty message: a model that returns no text and no tool call. Appending
// it produces a content array of length zero, which the Anthropic protocol
// rejects on the NEXT request rather than this one.
func TestValidConversationRejectsAnEmptyMessage(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "hello"),
		{Role: RoleAssistant},
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("a message with zero content blocks was accepted; the request fails one turn later, " +
			"with a traceback pointing at the request builder instead of at whatever appended the empty message")
	}
	if !strings.Contains(why, "1") {
		t.Errorf("the complaint does not say which message is empty: %s", why)
	}
}

// ---------------------------------------------------------------------------
// The estimator
// ---------------------------------------------------------------------------

// The estimator has to settle on a session's real characters-per-token, because
// every compaction decision is made against it. An estimator that never
// converges compacts too early (burning cache for nothing) or too late (hitting
// the wall).
func TestEstimatorConverges(t *testing.T) {
	e := newEstimator()

	// A true ratio of 3.0 with ±8% of turn-to-turn noise, which is roughly what
	// a session moving between prose and JSON actually looks like.
	for i := range 20 {
		noise := 1.08
		if i%2 == 1 {
			noise = 0.92
		}
		tokens := 1000 + i*137
		chars := int(3.0 * noise * float64(tokens))
		e.observe(chars, tokens)
	}

	if math.Abs(e.ratio-3.0)/3.0 > 0.05 {
		t.Errorf("after 20 samples of a 3.0 conversation the ratio is %.4f, more than 5%% away — "+
			"compaction decisions are being made against a number that never learned this session", e.ratio)
	}
	if e.obs != 20 {
		t.Errorf("observation count is %d after 20 valid samples", e.obs)
	}
}

// The sanity range exists because usage events and character counts can be
// mismatched — a usage event arriving for a call whose characters were never
// measured. One such sample takes ten good calls to climb back from.
func TestEstimatorRejectsImpossibleSamples(t *testing.T) {
	e := newEstimator()
	e.observe(3000, 1000) // a real sample: ratio 3.0
	ratio, obs := e.ratio, e.obs

	for _, bad := range []struct {
		name          string
		chars, tokens int
	}{
		{"0.5 characters per token — fewer characters than tokens", 1000, 2000},
		{"50 characters per token — an order of magnitude too high", 50000, 1000},
		{"no characters", 0, 1000},
		{"no tokens", 3000, 0},
		{"negative characters", -3000, 1000},
	} {
		e.observe(bad.chars, bad.tokens)
		if e.ratio != ratio || e.obs != obs {
			t.Errorf("%s moved the estimate to %.4f (obs %d); one mismatched usage event now poisons every "+
				"compaction decision for the next ten turns", bad.name, e.ratio, e.obs)
			e.ratio, e.obs = ratio, obs
		}
	}
}

// 3.6 is a guess. The first real measurement is evidence. Averaging the two
// keeps the guess alive for a dozen turns, which is most of a short session —
// and it does it invisibly, because the ratio still looks plausible.
func TestEstimatorFirstObservationReplacesTheColdStart(t *testing.T) {
	e := newEstimator()
	if e.ratio != 3.6 {
		t.Fatalf("cold start is %.4f, not 3.6", e.ratio)
	}
	e.observe(2500, 1000) // a JSON-heavy session: 2.5 characters per token
	if e.ratio != 2.5 {
		t.Errorf("after one real measurement of 2.5 the ratio is %.4f — the 3.6 cold-start guess was blended in "+
			"rather than replaced, so the first turns of every session estimate against a number nobody measured", e.ratio)
	}
	if e.obs != 1 {
		t.Errorf("observation count is %d after one sample", e.obs)
	}
}

// The claim the chapter makes, under test.
//
// The estimate does not have to be *accurate*; it has to be *consistent*. The
// provider below charges chars/2.9 plus a flat 700-token envelope that the
// agent never sees itemised — a systematic bias of exactly the kind a vendored
// tokenizer would get wrong. Because the estimator is calibrated against the
// same quantity it is later asked to convert (convChars + baseChars), the bias
// is absorbed into the ratio instead of accumulating as error.
func TestEstimatorIsConsistentWithTheProviderItCalibratesAgainst(t *testing.T) {
	// The "server": a tokenizer the agent has no access to.
	tokenize := func(chars int) int { return int(float64(chars)/2.9) + 700 }

	c := newCompactor(200_000, 0.8, 0.3)
	const baseChars = 12_000 // the system prompt and the tool schemas

	var msgs []Msg
	msgs = calibrationTurn(msgs, "t00", 4000)
	msgs = calibrationTurn(msgs, "t01", 4000)

	// Ten turns of the real loop: send, be billed, calibrate.
	for i := range 10 {
		if i > 0 {
			msgs = calibrationTurn(msgs, fmt.Sprintf("t%02d", i+1), 4000)
		}
		sent := convChars(msgs) + baseChars
		c.est.observe(sent, tokenize(sent))
	}

	// The next turn arrives. This is the prediction the wall check is made on,
	// taken before the server is asked.
	msgs = calibrationTurn(msgs, "t99", 4000)
	got := c.estimate(msgs, baseChars)
	want := tokenize(convChars(msgs) + baseChars)

	off := math.Abs(float64(got-want)) / float64(want)
	if off > 0.10 {
		t.Errorf("the estimator predicted %d tokens for a prompt the provider billed at %d — %.1f%% out.\n"+
			"The estimate is only ever used to answer 'are we near the wall yet', and at this error the answer "+
			"is wrong by more than a turn's worth of tool output.", got, want, off*100)
	}
}

// calibrationTurn appends one turn of history — a question, a tool call, its
// result, and a reply — totalling exactly `chars` characters as msgChars counts
// them, so the calibration arithmetic above is exact.
func calibrationTurn(msgs []Msg, id string, chars int) []Msg {
	q := chars / 4
	return append(msgs,
		TextMsg(RoleUser, padded("ask "+id+" ", q)),
		Msg{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: id, Name: "bash", Args: bashArgs(q - len("bash"))},
		}},
		Msg{Role: RoleUser, Blocks: []Block{ToolResultBlock(id, padded("out "+id+" ", q))}},
		TextMsg(RoleAssistant, padded("done "+id+" ", chars-3*q)),
	)
}

// due answers "are we near the wall", and it must answer from the estimate of
// the prompt about to be sent rather than from the last usage report, which is
// always one turn stale.
func TestDueFiresOnTheThreshold(t *testing.T) {
	c := newCompactor(100_000, 0.75, 0.3)
	for _, tc := range []struct {
		est  int
		want bool
	}{
		{0, false},
		{74_999, false},
		{75_000, true},
		{90_000, true},
	} {
		if got := c.due(tc.est); got != tc.want {
			t.Errorf("due(%d) = %v with a 100k window at 75%%; the agent will compact %s", tc.est, got,
				map[bool]string{true: "when it did not need to, burning the cache for nothing", false: "too late, after the wall"}[got])
		}
	}
	if (&compactor{window: 0, threshold: 0.75}).due(1 << 20) {
		t.Error("due fired with no configured window; an unconfigured compactor should never compact, not always compact")
	}
}

// ---------------------------------------------------------------------------
// plan
// ---------------------------------------------------------------------------

// Compacting a conversation this short would replace real content with a
// summary of nothing and cost a model call to do it.
func TestPlanRefusesAShortConversation(t *testing.T) {
	c := pinnedCompactor(10_000, 0.8, 0.3)
	cut, why := c.plan(convFixture()[:3], 0)
	if cut != -1 {
		t.Fatalf("plan chose cut %d on a 3-message conversation", cut)
	}
	if why == "" {
		t.Error("plan refused silently; the caller has nothing to report and the user sees a turn that just did not compact")
	}
	if !strings.Contains(why, "3") {
		t.Errorf("the reason does not say how short the conversation is: %q", why)
	}
}

// The floor. When the newest message alone is bigger than the whole keep
// budget, cutting history frees nothing that matters — the problem is the size
// of the output the user has to act on, not the length of the conversation.
// Saying "compact harder" here sends the reader in the wrong direction.
func TestPlanRefusesWhenTheNewestMessageIsBiggerThanTheBudget(t *testing.T) {
	msgs := budgetFixture(5)
	requireUniform(t, msgs)

	// 0.005 of a 10,000-token window is a 50-token budget; every message is 100.
	c := pinnedCompactor(10_000, 0.8, 0.005)
	cut, why := c.plan(msgs, 0)
	if cut != -1 {
		t.Fatalf("plan chose cut %d when the newest message alone does not fit the keep budget", cut)
	}
	if !strings.Contains(why, "--max-output") {
		t.Errorf("the refusal does not point at the output limit: %q\n"+
			"this is an output-size problem, and a reason that does not say so sends the reader looking at the compactor", why)
	}
}

// The whole point of safeCut, observed from the outside: the budget boundary
// lands wherever it lands, and plan must move it to a legal cut point before
// returning it.
//
// The arithmetic: 20 messages of 100 tokens each, a 400-token budget, so the
// backward walk fits exactly the newest four and stops with the boundary at
// index 16 — a user turn, and an illegal place to cut. The answer must be 17.
func TestPlanCutsAtALegalBoundaryInsideTheBudget(t *testing.T) {
	msgs := budgetFixture(5)
	requireUniform(t, msgs)
	const budget = 400 // 0.04 × 10,000

	if canCutBefore(msgs, 16) {
		t.Fatalf("fixture drift: index 16 was supposed to be an illegal cut point, so this test no longer exercises safeCut")
	}

	c := pinnedCompactor(10_000, 0.8, 0.04)
	cut, why := c.plan(msgs, 0)
	if cut < 0 {
		t.Fatalf("plan refused a 20-message conversation that has room to cut: %s", why)
	}
	if !canCutBefore(msgs, cut) {
		t.Fatalf("plan returned index %d, which is not a legal cut point — the budget boundary was used raw instead of "+
			"being moved forward, and the next request carries an orphaned tool result", cut)
	}
	if cut <= 16 {
		t.Errorf("plan returned %d; the budget boundary is 16 and safeCut must move forward from there, never back", cut)
	}

	kept := c.est.tokens(convChars(msgs[cut:]))
	if kept > budget {
		t.Errorf("the kept tail is ~%d tokens against a keep budget of %d; compaction freed less than it promised", kept, budget)
	}
	if why := validConversation(append([]Msg{summaryMsg("s")}, msgs[cut:]...)); why != "" {
		t.Errorf("the planned compaction produces an unsendable conversation: %s", why)
	}
}

// A sweep over every plausible keep budget. Any cut plan returns must be legal,
// must leave a conversation with something in it, and must never leave fewer
// than two messages — one message plus the summary is a conversation the model
// answers as if the user had just typed the summary.
func TestPlanAlwaysLeavesASendableTail(t *testing.T) {
	msgs := budgetFixture(6)
	requireUniform(t, msgs)

	cuts, refusals := 0, 0
	for k := 1; k <= 60; k++ {
		c := pinnedCompactor(10_000, 0.8, float64(k)/100)
		cut, why := c.plan(msgs, 0)
		if cut < 0 {
			refusals++
			if why == "" {
				t.Errorf("keepRatio %.2f: plan refused with no reason", float64(k)/100)
			}
			continue
		}
		cuts++
		if !canCutBefore(msgs, cut) {
			t.Errorf("keepRatio %.2f: plan returned illegal cut %d", float64(k)/100, cut)
		}
		if len(msgs)-cut < 2 {
			t.Errorf("keepRatio %.2f: plan returned cut %d, leaving %d message(s) after the summary — "+
				"the model reads a lone summary as a fresh request from the user and answers it", float64(k)/100, cut, len(msgs)-cut)
		}
		if why := validConversation(append([]Msg{summaryMsg("s")}, msgs[cut:]...)); why != "" {
			t.Errorf("keepRatio %.2f: cut %d produces an unsendable conversation: %s", float64(k)/100, cut, why)
		}
	}
	if cuts == 0 || refusals == 0 {
		t.Fatalf("the sweep produced %d cuts and %d refusals; it needs both to be meaningful", cuts, refusals)
	}
}

// ---------------------------------------------------------------------------
// clip
// ---------------------------------------------------------------------------

// Middle-out, not head-first. A build log puts the error at the end, a stack
// trace puts the cause at the end, and a `clip` that keeps only the head passes
// every naive length assertion while throwing away the answer.
func TestClipKeepsBothEnds(t *testing.T) {
	s := "HEAD" + strings.Repeat("m", 992) + "TAIL"
	if len(s) != 1000 {
		t.Fatalf("fixture is %d characters, not 1000", len(s))
	}

	got := clip(s, 200)
	if !strings.HasPrefix(got, "HEAD") {
		t.Errorf("the beginning of the string is gone; whatever the command was announcing was dropped:\n%q", got)
	}
	if !strings.HasSuffix(got, "TAIL") {
		t.Errorf("the END of the string is gone. This is head truncation, and it discards exactly the part that matters — "+
			"the error message, the last line of the diff, the conclusion:\n%q", got)
	}
	if !strings.Contains(got, "omitted") || !strings.Contains(got, "800") {
		t.Errorf("the clip does not say that 800 characters were dropped, so the model reads the two halves as contiguous:\n%q", got)
	}
}

func TestClipIsANoOpAndNeverPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		max  int
	}{
		{"shorter than the limit", "still small", 100},
		{"exactly the limit", "0123456789", 10},
		{"zero limit means no limit", "leave me alone", 0},
		{"negative limit means no limit", "leave me alone", -5},
	} {
		if got := clip(tc.s, tc.max); got != tc.s {
			t.Errorf("%s: clip rewrote a string it had no reason to touch: %q", tc.name, got)
		}
	}

	// Byte slicing across a multi-byte rune must not panic. The result may hold
	// a replacement character; it may not take the process down mid-compaction.
	s := strings.Repeat("日本語のログ出力", 300)
	got := clip(s, 100)
	if len(got) >= len(s) {
		t.Errorf("a %d-byte multi-byte string clipped to 100 came back %d bytes", len(s), len(got))
	}
	if !strings.Contains(got, "omitted") {
		t.Errorf("the multi-byte clip lost its omission marker: %q", got)
	}
}

// ---------------------------------------------------------------------------
// flatten
// ---------------------------------------------------------------------------

// The summariser reads a transcript, not a conversation. A tool call has to
// arrive as the command that ran, because `{"command":"ls -la /srv"}` spends
// tokens on JSON punctuation and reads as a data structure rather than an action.
func TestFlattenRendersTheCommandNotTheJSON(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "list /srv"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Looking."},
			{Kind: BlockToolCall, ID: "toolu_f1", Name: "bash", Args: `{"command":"ls -la /srv/app"}`},
		}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_f1", "total 0\n[exit 0]")}},
	}

	got := flatten(msgs, 4000)
	if !strings.Contains(got, "ls -la /srv/app") {
		t.Errorf("the command the agent ran is missing from the transcript:\n%s", got)
	}
	if strings.Contains(got, `{"command"`) {
		t.Errorf("the raw JSON arguments were pasted in instead of the command:\n%s", got)
	}
	if !strings.Contains(got, "Looking.") || !strings.Contains(got, "total 0") {
		t.Errorf("text or command output went missing from the transcript:\n%s", got)
	}
}

// Small models emit broken tool calls — truncated JSON, a missing `command`
// field. The transcript is the last record of what happened, so a call that
// could not be parsed must still appear as something rather than vanish.
func TestFlattenFallsBackToRawArgsWhenTheJSONIsBroken(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"truncated mid-string", `{"command":"go test ./sta`},
		{"valid JSON, wrong shape", `{"raw_arguments":""}`},
		{"empty command", `{"command":"  "}`},
	} {
		msgs := []Msg{
			TextMsg(RoleUser, "run it"),
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockToolCall, ID: "toolu_broken", Name: "bash", Args: tc.args},
			}},
		}
		got := flatten(msgs, 4000)
		if !strings.Contains(got, tc.args) {
			t.Errorf("%s: the unparseable call left no trace in the transcript, so the summary cannot record that the "+
				"agent tried something and it did not work:\n%s", tc.name, got)
		}
	}
}

// The transcript goes to a call that has no tools defined and no history. An id
// that appears in it is an id the summary can quote, and the summary is a user
// message in the surviving conversation — where that id refers to nothing.
func TestFlattenLeaksNoToolCallIDs(t *testing.T) {
	got := flatten(convFixture(), 4000)
	for _, id := range convFixtureToolIDs {
		if strings.Contains(got, id) {
			t.Errorf("tool call id %q reached the summariser; anything it writes about that id survives into a conversation "+
				"where the call no longer exists", id)
		}
	}
	// Sanity: the transcript is not empty, so the loop above had something to
	// find nothing in.
	if len(got) < 200 {
		t.Fatalf("the flattened transcript is only %d characters; it cannot be a faithful rendering of a 14-message conversation", len(got))
	}
}
