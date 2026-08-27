package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stage 11's boundary is tested in toolcall_test.go. This file tests that it is
// CALLED — from the right place, at the right time, on the right values.
//
// Mutation testing is what made this file necessary. Every check in toolcall.go
// was covered, and a mutant that deleted the call to it from dispatch survived,
// because a second guard downstream happened to catch the same payload. A unit
// test on a function proves the function works; only a test that runs the loop
// proves the loop uses it.

// ---------------------------------------------------------------------------
// A provider that reads from a script
// ---------------------------------------------------------------------------

// scriptProvider returns pre-built CallResults in order, one per model call, and
// records how many calls it served. It ignores the conversation entirely: these
// tests are about what the loop does with a response, not about what provokes
// one.
type scriptProvider struct {
	mu     sync.Mutex
	script []*CallResult
	served int
}

var _ Provider = (*scriptProvider)(nil)

func (p *scriptProvider) Protocol() string { return "script" }
func (p *scriptProvider) Model() string    { return "script-model" }

func (p *scriptProvider) BuildRequest(ctx context.Context, system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	body := []byte(`{}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://script.invalid/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	return req, body, nil
}

func (p *scriptProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	io.Copy(io.Discard, r)
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.served
	p.served++
	if i >= len(p.script) {
		// Running off the end means the loop made more calls than the test
		// expected, which is itself the failure. End the turn rather than
		// panicking, so the assertion reports the count.
		return &CallResult{Text: "script exhausted", Stop: StopEndTurn, RawStop: "end_turn"}, nil
	}
	return p.script[i], nil
}

func (p *scriptProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.served
}

// scriptAgent wires an agent to a script. The shell is real, because some of
// these tests need a command to actually run.
func scriptAgent(t *testing.T, script ...*CallResult) (*agent, *mulRecorder, *scriptProvider) {
	t.Helper()
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))
	p := &scriptProvider{script: script}
	a.lad = newLadder(rung{p: p})
	a.httpc = &http.Client{Transport: mulRoundTrip{}}
	return a, rec, p
}

func toolCall(id, name, args string) Block {
	return Block{Kind: BlockToolCall, ID: id, Name: name, Args: args}
}

func callResult(stop StopReason, raw, text string, calls ...Block) *CallResult {
	return &CallResult{Text: text, Stop: stop, RawStop: raw, Calls: calls,
		Usage: Usage{Input: 100, Output: 10}}
}

// ---------------------------------------------------------------------------
// Identity, from inside the loop
// ---------------------------------------------------------------------------

// The duplicates that matter live in DIFFERENT assistant messages, so this can
// only be tested by running more than one turn. A gateway that mints one id per
// call it ever makes is the case; the protocol rejects the whole request for it,
// naming a message index rather than the tool.
func TestRunTurnRenamesDuplicateIDsAcrossTurns(t *testing.T) {
	a, _, _ := scriptAgent(t,
		callResult(StopToolUse, "tool_use", "", toolCall("call_go_0", "bash", mulBash("echo s11-first"))),
		callResult(StopToolUse, "tool_use", "", toolCall("call_go_0", "bash", mulBash("echo s11-second"))),
		callResult(StopEndTurn, "end_turn", "done"),
	)

	msgs := a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	var ids []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolCall {
				ids = append(ids, b.ID)
			}
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 tool calls in the history, got %d (%v)", len(ids), ids)
	}
	if ids[0] == ids[1] {
		t.Fatalf("both tool calls carry the id %q; the next request is rejected for a duplicate tool_use id, "+
			"and the rejection names a message index rather than the tool", ids[0])
	}

	// The result must have moved with the call it answers. A call renamed after
	// its answer exists is an orphaned result: the same rejected request with a
	// less helpful message.
	answered := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolResult {
				answered[b.ID] = true
			}
		}
	}
	for _, id := range ids {
		if !answered[id] {
			t.Errorf("tool call %q has no result addressed to it; every call in an assistant message must be "+
				"answered or the request is rejected", id)
		}
	}
}

// ---------------------------------------------------------------------------
// The markup leak, from inside the loop
// ---------------------------------------------------------------------------

// §A2's shape: a truncated turn whose text is the gateway's own internal
// tool-call syntax. Keeping it costs twice — the human is shown gateway
// internals as if the assistant said them, and the history teaches the model
// that emitting this syntax as prose is normal here.
func TestRunTurnKeepsLeakedMarkupOutOfTheHistory(t *testing.T) {
	leak := "Looking at that now.\n\n<tool_call>\n<function=bash>\n<parameter=command>find /srv"
	a, rec, _ := scriptAgent(t, callResult(StopMaxTokens, "length", leak))

	msgs := a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind != BlockText {
				continue
			}
			for _, marker := range harnessMarkers {
				if strings.Contains(b.Text, marker) {
					t.Errorf("the history contains the gateway's %q markup:\n  %q", marker, b.Text)
				}
			}
		}
	}

	// What the model did say before the markup started is the model talking to
	// the user, and it survives.
	var kept string
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			kept = m.Text()
		}
	}
	if !strings.Contains(kept, "Looking at that now.") {
		t.Errorf("the real text before the markup was discarded too: %q", kept)
	}

	var reported bool
	for _, e := range rec.events {
		if e.Kind == KindToolCallInvalid && strings.Contains(e.Text, "markup") {
			reported = true
		}
	}
	if !reported {
		t.Error("the leak was stripped without being recorded; a repair nobody can see is a repair nobody " +
			"can measure the rate of")
	}
}

// The gate is StopMaxTokens, and the cost of that gate is that ordinary text
// mentioning the markup is left alone. That is deliberate — this repo's own
// documentation quotes `<tool_call>` — so it gets a test, not just a comment.
func TestRunTurnLeavesMarkupAloneOnACompleteTurn(t *testing.T) {
	quoted := "The gateway emits <tool_call>\n<function=bash> when it truncates."
	a, _, _ := scriptAgent(t, callResult(StopEndTurn, "end_turn", quoted))

	msgs := a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "explain")})

	var kept string
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			kept = m.Text()
		}
	}
	if kept != quoted {
		t.Errorf("a complete turn that quoted the markup was truncated:\n  got  %q\n  want %q", kept, quoted)
	}
}

// ---------------------------------------------------------------------------
// The cut fuse
// ---------------------------------------------------------------------------

// Refusing correctly is not enough. Measured against the live endpoint at
// --max-tokens 110: sixteen model calls, zero commands, every call cut. The
// model cannot see max_tokens, so "you were cut off" names a cause it has no way
// to act on, and it rewrites a command of the same length forever.
func TestCutStreakEndsTheLoop(t *testing.T) {
	cut := mulBash("echo hi")[:12] // truncated mid-value
	var script []*CallResult
	for i := 0; i < 8; i++ {
		script = append(script, callResult(StopToolUse, "tool_use", "",
			toolCall("call_"+string(rune('a'+i)), "bash", cut)))
	}
	a, rec, p := scriptAgent(t, script...)

	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if p.calls() != maxCutStreak {
		t.Errorf("the loop made %d model calls before stopping, want %d. Without the fuse it runs to the turn "+
			"budget (%d), which is what the live session did", p.calls(), maxCutStreak, a.cfg.maxTurns)
	}
	var errored bool
	for _, e := range rec.events {
		if e.Kind == KindError && strings.Contains(e.Text, "truncated") {
			errored = true
			if !strings.Contains(e.Text, "max-tokens") {
				t.Errorf("the error does not name the knob that fixes it: %q", e.Text)
			}
		}
	}
	if !errored {
		t.Error("the loop stopped without telling the human why; the model cannot fix this and the human can")
	}
}

// A turn that gets anything through resets the streak, or a session with an
// occasional truncation eventually dies for no reason.
func TestCutStreakResetsOnAProductiveTurn(t *testing.T) {
	cut := mulBash("echo hi")[:12]
	good := mulBash("echo s11-productive")

	// cut, cut, GOOD, cut, cut, then end. Five calls, never three cuts in a row.
	a, rec, p := scriptAgent(t,
		callResult(StopToolUse, "tool_use", "", toolCall("c1", "bash", cut)),
		callResult(StopToolUse, "tool_use", "", toolCall("c2", "bash", cut)),
		callResult(StopToolUse, "tool_use", "", toolCall("c3", "bash", good)),
		callResult(StopToolUse, "tool_use", "", toolCall("c4", "bash", cut)),
		callResult(StopToolUse, "tool_use", "", toolCall("c5", "bash", cut)),
		callResult(StopEndTurn, "end_turn", "done"),
	)

	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if p.calls() != 6 {
		t.Errorf("the loop made %d model calls, want 6; the fuse fired on a session that was making progress",
			p.calls())
	}
	for _, e := range rec.events {
		if e.Kind == KindError && strings.Contains(e.Text, "truncated") {
			t.Fatalf("the fuse fired despite a productive turn in between: %q", e.Text)
		}
	}
}

// One bad call among several good ones is not the pattern the fuse is for: the
// model got work done that turn. Only a turn where EVERY call was cut counts.
func TestCutStreakIgnoresATurnThatGotSomethingThrough(t *testing.T) {
	cut := mulBash("echo hi")[:12]
	good := mulBash("echo s11-mixed")

	mixed := func(n string) *CallResult {
		return callResult(StopToolUse, "tool_use", "",
			toolCall(n+"a", "bash", cut), toolCall(n+"b", "bash", good))
	}
	a, rec, p := scriptAgent(t, mixed("t1"), mixed("t2"), mixed("t3"), mixed("t4"),
		callResult(StopEndTurn, "end_turn", "done"))

	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if p.calls() != 5 {
		t.Errorf("the loop made %d model calls, want 5; a turn that ran a command counted as a cut turn",
			p.calls())
	}
	for _, e := range rec.events {
		if e.Kind == KindError && strings.Contains(e.Text, "truncated") {
			t.Fatalf("the fuse fired on turns that each ran a command: %q", e.Text)
		}
	}
}

// ---------------------------------------------------------------------------
// dispatch classifies, and the class is what reaches the trace
// ---------------------------------------------------------------------------

// The class decides who is told and how, so a refusal recorded under the wrong
// class is a refusal that will be acted on wrongly — and it is invisible if the
// only assertion is "it was refused".
func TestDispatchRecordsTheRightFaultClassPerCall(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	want := []struct {
		id, args string
		fault    argFault
	}{
		{"c1", `{"raw_arguments":"{\"command\": \"find"}`, faultCut},
		{"c2", `{"command":"go test ./sta`, faultCut},
		{"c3", `I will list the files`, faultNotJSON},
		{"c4", `{}`, faultSchema},
		{"c5", `{"command":42}`, faultSchema},
	}
	var calls []Block
	for _, w := range want {
		calls = append(calls, toolCall(w.id, "bash", w.args))
	}
	a.dispatch(context.Background(), 1, calls)

	got := map[string]string{}
	for _, e := range rec.events {
		if e.Kind == KindToolCallInvalid {
			got[e.ToolID] = e.Fault
		}
	}
	for _, w := range want {
		if got[w.id] != string(w.fault) {
			t.Errorf("%s (%s): recorded fault %q, want %q", w.id, w.args, got[w.id], w.fault)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d refusals recorded for %d invalid calls", len(got), len(want))
	}
}

// ---------------------------------------------------------------------------
// The OpenAI request side
// ---------------------------------------------------------------------------

// §E14: `arguments: ""` is an HTTP 400 on this route, and a 400 is fatal, so one
// zero-argument tool call in the history ends the session. The unit test on
// renderArgs proves the function; this proves BuildRequest calls it.
func TestOpenAIRequestNeverSendsEmptyArguments(t *testing.T) {
	p := newOpenAIProvider("http://x.invalid/v1", "k", "m")
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{toolCall("call_1", "clock", "")}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("call_1", "12:00")}},
	}
	_, body, err := p.BuildRequest(context.Background(), "sys", msgs, []Tool{bashToolDef()}, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if bytes.Contains(body, []byte(`"arguments":""`)) {
		t.Errorf("the request carries an empty arguments string, which this route answers with a 400 — "+
			"and a 400 is fatal, so the session is over:\n%s", body)
	}
	if !bytes.Contains(body, []byte(`"arguments":"{}"`)) {
		t.Errorf("a zero-argument call was not rendered as {}:\n%s", body)
	}
}

// The accumulator must reconcile the three wire dialects, not append blindly.
// The unit test on mergeArgs proves the function; this proves the stream parser
// uses it, by feeding it the re-send dialect through real SSE frames.
func TestOpenAIStreamHandlesTheReSendDialect(t *testing.T) {
	frame := func(args string) string {
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "bash", "arguments": args},
					}},
				},
			}},
		})
		return "data: " + string(payload) + "\n\n"
	}
	stream := frame("") + frame(`{"comm`) + frame(`and":"ls"`) + frame(`}`) +
		// the whole value again, the way a re-sending gateway ends a stream
		frame(`{"command":"ls"}`) +
		`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	p := newOpenAIProvider("http://x.invalid/v1", "k", "m")
	res, err := p.ParseStream(strings.NewReader(stream), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(res.Calls))
	}
	got := res.Calls[0].Args
	if !json.Valid([]byte(got)) {
		t.Fatalf("the accumulated arguments do not parse: %q\nBlind appending produces `{...}{...}`, whose "+
			"error names a byte offset and nothing about the cause", got)
	}
	if got != `{"command":"ls"}` {
		t.Errorf("arguments = %q, want %q", got, `{"command":"ls"}`)
	}
}

// ---------------------------------------------------------------------------
// The stage 10 correction
// ---------------------------------------------------------------------------

// newChild never copied `dl`, so a subagent got a zero deadlines struct — and
// both guardBody and waitFor treat <= 0 as "no watchdog". Stage 10's entire
// subject did not apply to subagents, and nothing failed to say so.
func TestChildInheritsItsParentsDeadlines(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")
	a.dl = deadlines{connect: 7 * time.Second, idle: 11 * time.Second, total: 13 * time.Minute}

	child := a.newChild("kid", func() string { return "sys" })

	if child.dl != a.dl {
		t.Errorf("child deadlines = %+v, parent = %+v.\nA zero deadlines struct means every clock is off, so "+
			"the child runs with no stall detection and no total-call backstop — and the one child that hangs "+
			"forever is exactly what stage 10 exists to prevent", child.dl, a.dl)
	}
}

// The id set is NOT shared: a child has its own message array, so its ids only
// have to be unique within it, and a shared map would have concurrent children
// contending on every tool call.
func TestChildGetsItsOwnIDSet(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")
	a.seenIDs["call_parent"] = true

	child := a.newChild("kid", func() string { return "sys" })
	if child.seenIDs == nil {
		t.Fatal("the child has no id set; uniqueIDs would write into a nil map and panic")
	}
	if child.seenIDs["call_parent"] {
		t.Error("the child inherited the parent's ids")
	}
	child.seenIDs["call_child"] = true
	if a.seenIDs["call_child"] {
		t.Error("the child's ids leaked into the parent's set; the two maps are shared")
	}
}
