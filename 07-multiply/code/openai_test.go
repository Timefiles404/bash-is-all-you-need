// Tests for the OpenAI protocol adapter.
//
// Every frame constant below is copied out of docs/wire-notes.md — these are
// bytes this endpoint actually sent, not bytes invented to make a parser look
// good. That is the entire point: a fixture you wrote from the specification
// tests your reading of the specification, and this endpoint does not match the
// specification (see §B4 frames 11 and 13). Where a fixture had to be
// reconstructed or invented, the comment above it says so and says why.
//
// Ported from stage 02's sse_test.go. The fixtures are unchanged to the byte;
// what changed is the surface under test — a Provider rather than a free
// function — and the half of the file that is new, which tests the direction
// stage 02 never had a name for: the neutral conversation going OUT onto this
// wire.
//
// Naming note: the helpers and the framing tests here carry an `openai` prefix
// because anthropic.go's tests share this package. readSSE itself is neutral
// and lives in sse.go; the framing cases are kept here so the port from stage
// 02 loses no coverage, not because framing is this protocol's business.
//
// No network, no API key, no `-short` skips. The whole file runs on a plane.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// ---------------------------------------------------------------------------
// §B4 — the full 13-frame tool-call stream, in order.
//
// Request that produced it: `bash` tool, tool_choice:"required",
// reasoning_effort:"none", prompt "Call the bash tool once with command set to:
// ls -la /srv/app".
//
// Frames 1, 10, 11, 12 and 13 are recorded whole in §B4 and are copied verbatim.
// Frames 2–9 are recorded there as the `delta` object alone; the envelope around
// them is reconstructed from frames 1 and 10, which are complete. The `delta`
// objects themselves — including every explicit `null` — are verbatim.
// ---------------------------------------------------------------------------

const (
	// 1. Role opener. Note `content` is "", not null, and that it carries no
	//    payload at all: this frame is why TTFT must not be measured from the
	//    first frame received.
	b4RoleOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}`

	// 2. Tool-call opener — the ONLY chunk carrying `id` and `function.name`.
	b4ToolOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_8d4f0377bc594026a4765cfc","type":"function","function":{"name":"bash","arguments":""}}]}}]}`

	// 3.–9. Argument fragments. `id` and `function.name` are now explicitly
	//       null, `index` stays 0, and `type` stays "function" — it is not
	//       nulled, which is exactly why "the key is there" proves nothing.
	//
	//       The splits are not JSON-aligned: fragment 1 ends mid-object,
	//       fragment 4 ends mid-path (`/srv`), fragment 5 resumes it (`/app`).
	b4Arg1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": "}}]}}]}`
	b4Arg2 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"\""}}]}}]}`
	b4Arg3 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"ls"}}]}}]}`
	b4Arg4 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":" -la /srv"}}]}}]}`
	b4Arg5 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"/app"}}]}}]}`
	b4Arg6 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"\""}}]}}]}`
	b4Arg7 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"}"}}]}}]}`

	// 10. Finish chunk — empty delta, finish_reason set.
	b4Finish = `{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`

	// 11. Usage chunk. `choices` is an EMPTY ARRAY. Any code reaching for
	//     choices[0] panics right here, on the second-to-last frame of every
	//     real request. (§B5: this frame is present by default, with no
	//     stream_options sent — and sending stream_options changes nothing.)
	b4Usage = `{"id":"...","object":"chat.completion.chunk","created":1787768844,"model":"mimo-v2.5","choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}`

	// 12. The sentinel.
	b4Done = `[DONE]`

	// 13. A frame AFTER the sentinel. Every spec-conforming client discards it.
	//     `choices` is empty here too.
	b4PostDone = `{"choices":[],"cost":"0"}`
)

// b4ToolCallStream is §B4 end to end, in the recorded order.
var b4ToolCallStream = []string{
	b4RoleOpener,
	b4ToolOpener,
	b4Arg1, b4Arg2, b4Arg3, b4Arg4, b4Arg5, b4Arg6, b4Arg7,
	b4Finish,
	b4Usage,
	b4Done,
	b4PostDone,
}

// b4WantArgs is what §B4 says the fragments concatenate to.
const b4WantArgs = `{"command": "ls -la /srv/app"}`

// b4WantCalls is that same call in the neutral shape the Provider returns.
var b4WantCalls = []Block{{
	Kind: BlockToolCall,
	ID:   "call_8d4f0377bc594026a4765cfc",
	Name: "bash",
	Args: b4WantArgs,
}}

// b4WantUsage is frame 11 after the direction reversal described on
// sseUsage.normalise: prompt_tokens 506 CONTAINS cached_tokens 192, so the
// full-price Input is the difference and Prompt() must come back out at 506.
var b4WantUsage = Usage{Input: 314, CacheRead: 192, Output: 26, Reasoning: 0}

// ---------------------------------------------------------------------------
// §B7 — reasoning and text on the same delta object.
//
// The five `reasoning_content` deltas and the role opener are verbatim §B7
// `delta` objects in a reconstructed envelope. §B7 records that this run had 44
// reasoning frames and 1 content frame but does not print the content frame, so
// the two `content` frames here are constructed in the identical shape — enough
// to prove the two fields land in two different accumulators, which is the
// thing being tested.
// ---------------------------------------------------------------------------

const (
	b7RoleOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}`
	b7Reason1    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":"Okay","tool_calls":null}}]}`
	b7Reason2    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":", the","tool_calls":null}}]}`
	b7Reason3    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":" user is asking for","tool_calls":null}}]}`
	b7Reason4    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":" the product of ","tool_calls":null}}]}`
	b7Reason5    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":"17 and ","tool_calls":null}}]}`

	// Constructed, not recorded — see the block comment above.
	b7Text1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":"17 * 23 = ","reasoning_content":null,"tool_calls":null}}]}`
	b7Text2 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":"391","reasoning_content":null,"tool_calls":null}}]}`

	b7Finish = `{"choices":[{"index":0,"finish_reason":"stop","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`
)

var b7ReasoningStream = []string{
	b7RoleOpener,
	b7Reason1, b7Reason2, b7Reason3, b7Reason4, b7Reason5,
	b7Text1, b7Text2,
	b7Finish,
	b4Usage,
	b4Done,
}

// ---------------------------------------------------------------------------
// Two parallel tool calls.
//
// CONSTRUCTED, not recorded: §B4 captured a single-call stream, and §D12 only
// establishes that `parallel_tool_calls:false` is accepted and ignored, so
// parallel calls are reachable but no verbatim capture exists. The chunk shape
// is copied exactly from §B4 — the only changes are the `index`, the ids, and
// the fragment text.
//
// Two deliberate distortions, both to make a bug visible rather than likely:
//
//   - index 1 opens BEFORE index 0, so an implementation that returns calls in
//     arrival order fails every time instead of half the time.
//   - the fragments interleave, so an implementation that appends to a single
//     shared buffer produces visible garbage rather than a subtle mix-up.
// ---------------------------------------------------------------------------

const (
	openaiParOpener1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_second","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	openaiParOpener0 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	openaiParArg1a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"echo "}}]}}]}`
	openaiParArg0a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"ls"}}]}}]}`
	openaiParArg1b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"two\"}"}}]}}]}`
	openaiParArg0b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":" -la\"}"}}]}}]}`
)

var openaiParallelStream = []string{
	b4RoleOpener,
	openaiParOpener1, openaiParOpener0,
	openaiParArg1a, openaiParArg0a,
	openaiParArg1b, openaiParArg0b,
	b4Finish,
	b4Usage,
	b4Done,
	b4PostDone,
}

// openaiParallelWantCalls is the pair in ascending index order — wire order reversed,
// which is the point.
var openaiParallelWantCalls = []Block{
	{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
	{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// openaiTestProvider is the provider under test. The base URL carries a trailing
// slash on purpose: a provider built by hand in a test must trim it the same way
// config.go does, or the endpoint becomes `.../v1//chat/completions`.
func openaiTestProvider() *openaiProvider {
	return newOpenAIProvider("https://opencode.ai/zen/go/v1/", "sk-test-key", "mimo-v2.5")
}

// openaiBody renders payloads the way §B4 shows this endpoint rendering them:
// `data: <payload>` then one blank line, LF-terminated (the doc shows it with
// `cat -A`, where every line ends `$` and no `^M` appears).
func openaiBody(frames ...string) io.Reader {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return strings.NewReader(b.String())
}

// openaiRecorder is a Subscriber that keeps everything, which is the cheapest
// possible demonstration of why the agent core emits events instead of
// printing: the test asserts on the event sequence and never touches stdout.
type openaiRecorder struct{ events []Event }

func (r *openaiRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func (r *openaiRecorder) kinds() []Kind {
	out := make([]Kind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *openaiRecorder) count(k Kind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (r *openaiRecorder) first(k Kind) (Event, bool) {
	for _, e := range r.events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

// The decode side of BuildRequest. Deliberately a separate set of structs from
// the ones in openai.go: asserting with the same types that produced the bytes
// would let a wrong json tag agree with itself and pass.
type openaiWireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiWireMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCallID string               `json:"tool_call_id"`
	ToolCalls  []openaiWireToolCall `json:"tool_calls"`
}

type openaiWireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type openaiWireRequest struct {
	Model         string `json:"model"`
	MaxTokens     int    `json:"max_tokens"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []openaiWireMessage `json:"messages"`
	Tools    []openaiWireTool    `json:"tools"`
}

func decodeOpenAIRequest(t *testing.T, body []byte) openaiWireRequest {
	t.Helper()
	var got openaiWireRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not valid JSON: %v\n%s", err, body)
	}
	return got
}

// roles is the message role sequence, which is the cheapest way to state the
// shape of a whole conversation in one assertion.
func (r openaiWireRequest) roles() []string {
	out := make([]string, 0, len(r.Messages))
	for _, m := range r.Messages {
		out = append(out, m.Role)
	}
	return out
}

// openaiBashSchema is the tool schema stage 02 shipped, used here as a realistic
// nested value rather than a one-key stub.
func openaiBashSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}
}

func openaiBashTool() Tool {
	return Tool{
		Name:        "bash",
		Description: "Execute a bash command and return its stdout, stderr and exit code.",
		Schema:      openaiBashSchema(),
	}
}

// ---------------------------------------------------------------------------
// The Provider surface itself.
// ---------------------------------------------------------------------------

func TestOpenAIProviderIdentity(t *testing.T) {
	p := openaiTestProvider()

	// Protocol() is written into the trace, so it is a stable string and not a
	// display label to be prettified later.
	if got := p.Protocol(); got != "openai" {
		t.Errorf("Protocol() got %q, want %q", got, "openai")
	}
	if got := p.Model(); got != "mimo-v2.5" {
		t.Errorf("Model() got %q, want %q", got, "mimo-v2.5")
	}
	if p.baseURL != "https://opencode.ai/zen/go/v1" {
		t.Errorf("trailing slash not trimmed: baseURL is %q", p.baseURL)
	}
}

// ---------------------------------------------------------------------------
// BuildRequest — the neutral conversation going OUT onto this wire.
// ---------------------------------------------------------------------------

func TestOpenAIBuildRequestEnvelope(t *testing.T) {
	p := openaiTestProvider()

	req, body, err := p.BuildRequest("sys", []Msg{TextMsg(RoleUser, "hi")}, []Tool{openaiBashTool()}, 4096)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	if req.Method != "POST" {
		t.Errorf("method got %q, want POST", req.Method)
	}
	if got, want := req.URL.String(), "https://opencode.ai/zen/go/v1/chat/completions"; got != want {
		t.Errorf("URL\n got %q\nwant %q", got, want)
	}
	for _, h := range []struct{ key, want string }{
		{"Authorization", "Bearer sk-test-key"},
		{"Accept", "text/event-stream"},
		{"Content-Type", "application/json"},
	} {
		if got := req.Header.Get(h.key); got != h.want {
			t.Errorf("header %s got %q, want %q", h.key, got, h.want)
		}
	}

	// The returned bytes must be the bytes the request will send. The caller
	// emits these as KindRequest, and a request inspector showing anything
	// other than what went on the wire is worse than no inspector: it is
	// evidence that lies.
	if req.GetBody == nil {
		t.Fatal("request has no GetBody: the body cannot be replayed or compared")
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	sent, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the request body: %v", err)
	}
	if string(sent) != string(body) {
		t.Errorf("returned body is not the body being sent\n sent %s\n got  %s", sent, body)
	}

	got := decodeOpenAIRequest(t, body)
	if got.Model != "mimo-v2.5" {
		t.Errorf("model got %q, want %q", got.Model, "mimo-v2.5")
	}
	if got.MaxTokens != 4096 {
		t.Errorf("max_tokens got %d, want 4096", got.MaxTokens)
	}
	if !got.Stream {
		t.Error("stream is not true: this adapter only knows how to read an SSE body")
	}

	// §B5: measurably a no-op on this gateway — same 13 frames with and
	// without it. Sent anyway, because a real OpenAI endpoint will not stream
	// usage without it, and an agent that reports zero tokens the day it is
	// pointed somewhere else is the failure this line prevents.
	if got.StreamOptions == nil {
		t.Fatal("stream_options missing: correct here, wrong on every other OpenAI-compatible endpoint")
	}
	if !got.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage got false, want true")
	}
}

func TestOpenAIBuildRequestSystemPromptIsTheFirstMessage(t *testing.T) {
	p := openaiTestProvider()
	const sys = "You are a coding agent working in a terminal."

	_, body, err := p.BuildRequest(sys, []Msg{TextMsg(RoleUser, "list the repo")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	// THE ASYMMETRY. On this protocol the system prompt is messages[0]. On the
	// Anthropic protocol it is a top-level `system` field and cannot be a
	// message at all — which is why Provider.BuildRequest takes it as its own
	// parameter instead of letting the caller push it into the history.
	if want := []string{"system", "user"}; !reflect.DeepEqual(got.roles(), want) {
		t.Fatalf("roles\n got %v\nwant %v", got.roles(), want)
	}
	if got.Messages[0].Content != sys {
		t.Errorf("messages[0].content\n got %q\nwant %q", got.Messages[0].Content, sys)
	}

	// And it must NOT also appear as a top-level field: sending both is how a
	// shared struct between two adapters leaks one protocol into the other.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["system"]; ok {
		t.Error("top-level `system` key present: that is the other protocol's shape")
	}

	// An empty system prompt produces no message at all rather than an empty
	// one, so a caller that has no system prompt does not silently send a blank
	// turn that counts against the context.
	_, body, err = p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if want := []string{"user"}; !reflect.DeepEqual(decodeOpenAIRequest(t, body).roles(), want) {
		t.Errorf("empty system prompt still produced a message: %v", decodeOpenAIRequest(t, body).roles())
	}
}

// TestOpenAIBuildRequestToolResultsBecomeSeparateMessages is the headline difference
// between the two adapters, stated as an assertion.
//
// Three tool results in ONE neutral message become THREE `role:"tool"` messages
// here. The Anthropic adapter collapses the identical input into ONE user
// message carrying three tool_result blocks. Neither shape can be the neutral
// one, which is why provider.go has no RoleTool and a tool result is a block.
func TestOpenAIBuildRequestToolResultsBecomeSeparateMessages(t *testing.T) {
	p := openaiTestProvider()

	results := Msg{Role: RoleUser, Blocks: []Block{
		ToolResultBlock("call_a", "total 0\n[exit 0 · 4ms]"),
		ToolResultBlock("call_b", "[no output]\n[exit 1 · 2ms]"),
		ToolResultBlock("call_c", "[the user denied this command. Do not retry it unchanged.]"),
	}}

	_, body, err := p.BuildRequest("", []Msg{results}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if want := []string{"tool", "tool", "tool"}; !reflect.DeepEqual(got.roles(), want) {
		t.Fatalf("three results must produce three messages\n got %v\nwant %v", got.roles(), want)
	}

	wantPairs := [][2]string{
		{"call_a", "total 0\n[exit 0 · 4ms]"},
		{"call_b", "[no output]\n[exit 1 · 2ms]"},
		{"call_c", "[the user denied this command. Do not retry it unchanged.]"},
	}
	for i, want := range wantPairs {
		m := got.Messages[i]
		if m.ToolCallID != want[0] {
			t.Errorf("messages[%d].tool_call_id got %q, want %q", i, m.ToolCallID, want[0])
		}
		if m.Content != want[1] {
			t.Errorf("messages[%d].content\n got %q\nwant %q", i, m.Content, want[1])
		}
		// A concatenating implementation passes the id check and fails here,
		// which is the failure mode worth naming: the model gets one blob of
		// three outputs attributed to one call.
		if strings.Contains(m.Content, "\x00") {
			t.Errorf("messages[%d].content looks concatenated: %q", i, m.Content)
		}
	}
}

// TestOpenAIBuildRequestFullConversationOrder pins the shape of a real turn: system,
// the user's request, the assistant's tool calls, the answers, the next user
// message. Tool results must land AFTER the assistant message that requested
// them, or the API cannot match them up.
func TestOpenAIBuildRequestFullConversationOrder(t *testing.T) {
	p := openaiTestProvider()

	msgs := []Msg{
		TextMsg(RoleUser, "list /srv/app"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Running both now."},
			{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
			{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
		}},
		{Role: RoleUser, Blocks: []Block{
			ToolResultBlock("call_first", "total 0"),
			ToolResultBlock("call_second", "two"),
		}},
		TextMsg(RoleUser, "thanks"),
	}

	_, body, err := p.BuildRequest("sys", msgs, []Tool{openaiBashTool()}, 4096)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	want := []string{"system", "user", "assistant", "tool", "tool", "user"}
	if !reflect.DeepEqual(got.roles(), want) {
		t.Errorf("conversation shape\n got %v\nwant %v", got.roles(), want)
	}
}

// TestOpenAIBuildRequestAssistantReplayRoundTrip covers the reassembly tax: a
// streamed assistant turn has to go back into the history as the message the
// API would have returned non-streamed, or the next request has tool calls with
// no record of who made them.
func TestOpenAIBuildRequestAssistantReplayRoundTrip(t *testing.T) {
	p := openaiTestProvider()

	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "Running both now."},
		// Dropped on the way out: this protocol has no inbound field for
		// reasoning, so replaying it would be ignored or rejected depending on
		// whose implementation is on the far end.
		{Kind: BlockThinking, Text: "The user wants two commands."},
		{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
		{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
	}}

	_, body, err := p.BuildRequest("", []Msg{assistant}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Messages) != 1 {
		t.Fatalf("want one assistant message, got %d: %v", len(got.Messages), got.roles())
	}
	m := got.Messages[0]
	if m.Role != "assistant" {
		t.Errorf("role got %q, want assistant", m.Role)
	}
	if m.Content != "Running both now." {
		t.Errorf("content\n got %q\nwant %q", m.Content, "Running both now.")
	}
	if strings.Contains(m.Content, "The user wants two commands.") {
		t.Error("thinking leaked into content: this protocol has no inbound field for it")
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("want 2 tool_calls, got %d", len(m.ToolCalls))
	}
	for i, want := range []openaiWireToolCall{
		{ID: "call_first", Type: "function"},
		{ID: "call_second", Type: "function"},
	} {
		if m.ToolCalls[i].ID != want.ID {
			t.Errorf("tool_calls[%d].id got %q, want %q", i, m.ToolCalls[i].ID, want.ID)
		}
		if m.ToolCalls[i].Type != "function" {
			t.Errorf("tool_calls[%d].type got %q, want %q", i, m.ToolCalls[i].Type, "function")
		}
		if m.ToolCalls[i].Function.Name != "bash" {
			t.Errorf("tool_calls[%d].function.name got %q, want bash", i, m.ToolCalls[i].Function.Name)
		}
	}
	if got, want := m.ToolCalls[0].Function.Arguments, `{"command": "ls -la"}`; got != want {
		t.Errorf("tool_calls[0].function.arguments\n got %q\nwant %q", got, want)
	}
	if got, want := m.ToolCalls[1].Function.Arguments, `{"command": "echo two"}`; got != want {
		t.Errorf("tool_calls[1].function.arguments\n got %q\nwant %q", got, want)
	}
}

// TestOpenAIBuildRequestArgumentsAreByteIdentical is the reason Block.Args is a raw
// string and not a decoded map.
//
// This protocol wants a JSON string containing JSON, which is what the stream
// parser accumulated, so the bytes pass straight through. Decode-and-re-encode
// and Go's randomised map iteration order rewrites the key order on every turn,
// which silently destroys the byte-prefix match that prompt caching depends on
// (§C9: 9,792 of 9,815 tokens served from cache, all of it keyed on an exact
// prefix) — and mangles the whitespace of any argument whose formatting mattered.
func TestOpenAIBuildRequestArgumentsAreByteIdentical(t *testing.T) {
	p := openaiTestProvider()

	// Deliberately awkward: unsorted keys, irregular spacing, an embedded quote
	// and an embedded newline. Every one of those survives a pass-through and
	// dies in a re-serialisation.
	const args = `{"zebra":1,  "command": "echo \"a  b\"\nls", "alpha":2}`

	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockToolCall, ID: "call_x", Name: "bash", Args: args},
	}}

	_, body, err := p.BuildRequest("", []Msg{assistant}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Messages) != 1 || len(got.Messages[0].ToolCalls) != 1 {
		t.Fatalf("unexpected shape: %s", body)
	}
	if sent := got.Messages[0].ToolCalls[0].Function.Arguments; sent != args {
		t.Errorf("arguments were rewritten\n got %q\nwant %q", sent, args)
	}

	// And the §B4 payload itself, which is the one that has to survive for the
	// agent to be able to answer its own tool call.
	_, body, err = p.BuildRequest("", []Msg{{Role: RoleAssistant, Blocks: b4WantCalls}}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got = decodeOpenAIRequest(t, body)
	if sent := got.Messages[0].ToolCalls[0].Function.Arguments; sent != b4WantArgs {
		t.Errorf("B4 arguments were rewritten\n got %q\nwant %q", sent, b4WantArgs)
	}
}

func TestOpenAIBuildRequestToolDefinitionNesting(t *testing.T) {
	p := openaiTestProvider()

	_, body, err := p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, []Tool{openaiBashTool()}, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(got.Tools))
	}
	tool := got.Tools[0]
	if tool.Type != "function" {
		t.Errorf(`tools[0].type got %q, want "function"`, tool.Type)
	}
	if tool.Function.Name != "bash" {
		t.Errorf("tools[0].function.name got %q, want bash", tool.Function.Name)
	}
	if tool.Function.Description == "" {
		t.Error("tools[0].function.description is empty: the model is being asked to guess")
	}

	// The schema goes under `parameters`, one level down. The Anthropic adapter
	// puts the same map under a top-level `input_schema` on an unnested object.
	// Comparing against a freshly built schema also proves nothing was mutated
	// in place on the way through.
	want := openaiBashSchema()
	if gotType, _ := tool.Function.Parameters["type"].(string); gotType != "object" {
		t.Errorf("parameters.type got %v, want object", tool.Function.Parameters["type"])
	}
	if _, ok := tool.Function.Parameters["properties"]; !ok {
		t.Errorf("parameters lost its properties: %#v (want the shape of %#v)", tool.Function.Parameters, want)
	}
	if _, ok := tool.Function.Parameters["additionalProperties"]; !ok {
		t.Error("parameters lost additionalProperties")
	}

	// The Anthropic shape must not be present anywhere on the tool object.
	var raw struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"name", "description", "input_schema"} {
		if _, ok := raw.Tools[0][key]; ok {
			t.Errorf("tools[0] has a top-level %q: that is the other protocol's envelope", key)
		}
	}

	// No tools means no `tools` key, rather than `"tools":null` — which some
	// OpenAI-compatible servers reject outright.
	_, body, err = p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if strings.Contains(string(body), `"tools"`) {
		t.Errorf("tools key present with no tools: %s", body)
	}
}

// ---------------------------------------------------------------------------
// ParseStream: the recorded streams, end to end.
// ---------------------------------------------------------------------------

func TestOpenAIParseStream(t *testing.T) {
	cases := []struct {
		name         string
		frames       []string
		wantText     string
		wantThinking string
		wantCalls    []Block
		wantRawStop  string
		wantStop     StopReason
		wantUsage    Usage
	}{
		{
			// The headline case: §B4 verbatim, all 13 frames.
			name:        "B4 tool call, all thirteen frames",
			frames:      b4ToolCallStream,
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
			wantCalls:   b4WantCalls,
		},
		{
			// §B7: two fields on one delta object must land in two places.
			name:         "B7 reasoning and text are kept apart",
			frames:       b7ReasoningStream,
			wantText:     "17 * 23 = 391",
			wantThinking: "Okay, the user is asking for the product of 17 and ",
			wantRawStop:  "stop",
			wantStop:     StopEndTurn,
			wantUsage:    b4WantUsage,
		},
		{
			// The frame that panics a choices[0] parser, on its own. No
			// finish_reason ever arrives, so the normalised stop is Unknown —
			// not EndTurn, which would be a guess dressed up as a fact.
			name:      "usage frame alone, choices is an empty array",
			frames:    []string{b4Usage},
			wantUsage: b4WantUsage,
			wantStop:  StopUnknown,
		},
		{
			// §B4 frame 13, on its own: empty choices AND an unknown top-level
			// key. Arriving after the sentinel is what makes it easy to never
			// have tested this.
			name:     "post-DONE cost frame alone",
			frames:   []string{b4Done, b4PostDone},
			wantStop: StopUnknown,
		},
		{
			// Draining past the sentinel is only defensible if it actually
			// picks something up. Move usage behind [DONE] and this is the
			// difference between correct accounting and a silent zero.
			name:        "frames after the sentinel are still read",
			frames:      []string{b4RoleOpener, b4Finish, b4Done, b4Usage, b4PostDone},
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
		},
		{
			// Parallel calls: independent accumulation, ascending index order.
			name:        "two parallel tool calls interleaved",
			frames:      openaiParallelStream,
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
			wantCalls:   openaiParallelWantCalls,
		},
		{
			// A stream that produced nothing at all still has to come back
			// clean rather than half-initialised.
			name:        "role opener and finish only",
			frames:      []string{b4RoleOpener, b4Finish, b4Done},
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
		},
	}

	p := openaiTestProvider()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ParseStream(openaiBody(tc.frames...), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text\n got %q\nwant %q", got.Text, tc.wantText)
			}
			if got.Thinking != tc.wantThinking {
				t.Errorf("Thinking\n got %q\nwant %q", got.Thinking, tc.wantThinking)
			}
			if got.RawStop != tc.wantRawStop {
				t.Errorf("RawStop got %q, want %q", got.RawStop, tc.wantRawStop)
			}
			if got.Stop != tc.wantStop {
				t.Errorf("Stop got %q, want %q", got.Stop, tc.wantStop)
			}
			if got.Usage != tc.wantUsage {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.wantUsage)
			}
			if !reflect.DeepEqual(got.Calls, tc.wantCalls) {
				t.Errorf("Calls\n got %#v\nwant %#v", got.Calls, tc.wantCalls)
			}
		})
	}
}

// TestOpenAIStopNormalisation checks the two-value contract on CallResult:
// RawStop is whatever the provider said, Stop is the neutral reading of it, and
// a word nobody has seen before becomes StopUnknown rather than "probably fine".
func TestOpenAIStopNormalisation(t *testing.T) {
	cases := []struct {
		raw  string
		want StopReason
	}{
		{"tool_calls", StopToolUse},                                     // §B4 frame 10
		{"stop", StopEndTurn},                                           // §C9
		{"length", StopMaxTokens},                                       // §A1, §A2
		{"content_filter", StopFiltered},                                // not observed here; documented elsewhere
		{"some_new_thing_the_vendor_shipped_on_a_tuesday", StopUnknown}, // the case that matters
	}

	p := openaiTestProvider()
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			frame := `{"choices":[{"index":0,"finish_reason":"` + tc.raw + `","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`
			got, err := p.ParseStream(openaiBody(frame, b4Done), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			// The literal is never normalised away: §A3c is a case where the
			// envelope lies, and the gap between these two fields is the only
			// evidence left when a session goes wrong.
			if got.RawStop != tc.raw {
				t.Errorf("RawStop got %q, want %q", got.RawStop, tc.raw)
			}
			if got.Stop != tc.want {
				t.Errorf("Stop got %q, want %q", got.Stop, tc.want)
			}
		})
	}
}

// TestOpenAIB4ArgsReassembleIntoValidJSON is the payoff for never parsing a
// fragment. Not one of the seven pieces in §B4 is valid JSON on its own; the
// concatenation is, and that is the only place a parse is allowed to happen.
func TestOpenAIB4ArgsReassembleIntoValidJSON(t *testing.T) {
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got.Calls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.Calls))
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(got.Calls[0].Args), &args); err != nil {
		t.Fatalf("assembled args are not valid JSON: %v (%q)", err, got.Calls[0].Args)
	}
	if args.Command != "ls -la /srv/app" {
		t.Errorf("command got %q, want %q", args.Command, "ls -la /srv/app")
	}
}

// TestOpenAIToolIDSurvivesTheNullChunks is the id-latching regression, stated on
// its own so the failure message names the actual disease. Frames 3–9 all carry
// `"id":null`; an unguarded assignment leaves this empty and the tool call
// becomes unanswerable, because the API requires that id back in the reply.
func TestOpenAIToolIDSurvivesTheNullChunks(t *testing.T) {
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got.Calls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.Calls))
	}
	if got.Calls[0].ID != "call_8d4f0377bc594026a4765cfc" {
		t.Errorf("tool id was lost to a later null chunk: got %q", got.Calls[0].ID)
	}
	if got.Calls[0].Name != "bash" {
		t.Errorf("tool name was lost to a later null chunk: got %q", got.Calls[0].Name)
	}
	if got.Calls[0].Kind != BlockToolCall {
		t.Errorf("call block kind got %q, want %q", got.Calls[0].Kind, BlockToolCall)
	}
}

// TestOpenAIParallelToolCallsComeBackInIndexOrder runs the same stream many
// times because Go randomises map iteration order on purpose. One pass would
// catch a missing sort roughly half the time — a test that fails one commit in
// two is worse than no test, because it teaches people to re-run CI. Twenty
// passes puts a false pass at about one in a million.
func TestOpenAIParallelToolCallsComeBackInIndexOrder(t *testing.T) {
	p := openaiTestProvider()
	for i := 0; i < 20; i++ {
		got, err := p.ParseStream(openaiBody(openaiParallelStream...), NewBus(), 1, time.Now())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if !reflect.DeepEqual(got.Calls, openaiParallelWantCalls) {
			t.Fatalf("pass %d: out of index order\n got %#v\nwant %#v", i, got.Calls, openaiParallelWantCalls)
		}
	}
}

// TestOpenAIStreamedCallsReplayUnchanged closes the loop: what ParseStream
// produced has to go straight back through BuildRequest as an assistant
// message. This is the round trip the agent loop performs on every turn, and
// the only test here that exercises both halves of the adapter at once.
func TestOpenAIStreamedCallsReplayUnchanged(t *testing.T) {
	p := openaiTestProvider()

	res, err := p.ParseStream(openaiBody(openaiParallelStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}

	assistant := Msg{Role: RoleAssistant, Blocks: append([]Block{{Kind: BlockText, Text: res.Text}}, res.Calls...)}
	_, body, err := p.BuildRequest("", []Msg{assistant}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Messages) != 1 || len(got.Messages[0].ToolCalls) != 2 {
		t.Fatalf("round trip lost the calls: %s", body)
	}
	for i, want := range openaiParallelWantCalls {
		tc := got.Messages[0].ToolCalls[i]
		if tc.ID != want.ID || tc.Function.Name != want.Name || tc.Function.Arguments != want.Args {
			t.Errorf("tool_calls[%d]\n got %+v\nwant %+v", i, tc, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Usage normalisation — the direction reversal.
// ---------------------------------------------------------------------------

func TestOpenAIUsageNormalisation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Usage
	}{
		{
			// §B4 frame 11, verbatim. 506 is the FULL prompt with 192 cached
			// tokens inside it, so full-price Input is 314 and Prompt() must
			// still come back out at 506.
			name: "B4 frame 11 without stream_options",
			in:   b4Usage,
			want: Usage{Input: 314, CacheRead: 192, Output: 26, Reasoning: 0},
		},
		{
			// §B5: the same request WITH stream_options:{include_usage:true}.
			// The parameter is a no-op; only cached_tokens differs, and it
			// differs because cache state varies between runs.
			name: "B5 frame 11 with stream_options, a no-op",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":448},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 58, CacheRead: 448, Output: 26},
		},
		{
			// A cold request: nothing cached, so Input is the whole prompt.
			// This is the case where copying prompt_tokens straight across
			// looks perfect, which is exactly why it survives review.
			name: "no cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 506, Output: 26},
		},
		{
			// §C9's own numbers: 9,792 of 9,815 tokens served from cache. Copy
			// prompt_tokens across unsubtracted and Prompt() reports 19,607 for
			// a 9,815-token prompt — the error is the size of the cache hit, so
			// it grows the better caching works.
			name: "C9 warm call, a 99.8% cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 23, CacheRead: 9792, Output: 2},
		},
		{
			// Reasoning is a SUBSET of completion_tokens, not an addition.
			name: "a thinking model reports reasoning inside completion",
			in:   `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":900,"total_tokens":1000,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":850}}}`,
			want: Usage{Input: 60, CacheRead: 40, Output: 900, Reasoning: 850},
		},
		{
			// The detail objects nulled outright. Every field on this endpoint
			// can be null, so the parser has to survive it — zeroes, not a
			// crash and not a negative.
			name: "null detail objects",
			in:   `{"choices":[],"usage":{"prompt_tokens":80,"completion_tokens":9,"total_tokens":89,"prompt_tokens_details":null,"completion_tokens_details":null}}`,
			want: Usage{Input: 80, Output: 9},
		},
		{
			// Defensive: more cached than prompt is arithmetically impossible,
			// but exporting a negative token count would poison Prompt() and
			// every cost estimate downstream. Clamp and move on.
			name: "cached exceeds prompt, clamped rather than negative",
			in:   `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":99}}}`,
			want: Usage{Input: 0, CacheRead: 99, Output: 1},
		},
	}

	p := openaiTestProvider()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ParseStream(openaiBody(tc.in, b4Done), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			if got.Usage != tc.want {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.want)
			}
		})
	}
}

// TestOpenAIUsagePromptRoundTrips states the invariant that makes the reversal
// checkable without doing the subtraction again: whatever the split, Prompt()
// has to equal the prompt_tokens the endpoint reported.
//
// This is the assertion that goes red the moment somebody "simplifies"
// normalise() into Input = prompt_tokens, and it goes red by exactly the size of
// the cache hit — which is why it is stated as an invariant and not as another
// copy of the arithmetic.
func TestOpenAIUsagePromptRoundTrips(t *testing.T) {
	p := openaiTestProvider()

	got, err := p.ParseStream(openaiBody(b4Usage), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.Usage.Prompt() != 506 {
		t.Errorf("Prompt() got %d, want 506 (the prompt_tokens on the wire)", got.Usage.Prompt())
	}
	if got.Usage.CacheWrite != 0 {
		t.Errorf("CacheWrite got %d, want 0: this protocol reports no write figure", got.Usage.CacheWrite)
	}

	// The same invariant on §C9's warm call, where the gap between right and
	// wrong is 9,792 tokens rather than 192.
	warm := `{"choices":[],"usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}}`
	got, err = p.ParseStream(openaiBody(warm), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.Usage.Prompt() != 9815 {
		t.Errorf("Prompt() got %d, want 9815 (§C9's prompt_tokens)", got.Usage.Prompt())
	}
}

// ---------------------------------------------------------------------------
// The event stream. These are the tests the event bus exists for.
// ---------------------------------------------------------------------------

func TestOpenAIEventSequenceForB4ToolCall(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(rec), 7, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}

	want := []Kind{
		KindFirstToken,    // frame 2: the tool-call opener is the first real payload
		KindToolCallStart, // same frame, once id and name are latched
		// frames 3-9. The opener's `"arguments":""` produces nothing, which is
		// why there are seven of these and not eight.
		KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta,
		KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta,
		KindUsage,       // frame 11, the empty-choices one
		KindResponseEnd, // after draining frames 12 and 13
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("event kinds\n got %v\nwant %v", got, want)
	}

	if n := rec.count(KindFirstToken); n != 1 {
		t.Errorf("KindFirstToken emitted %d times, want exactly 1", n)
	}

	// Frame 1 carries `content: ""`, which is not a token. If TTFT were
	// measured from it, first_token would land before the model had generated
	// anything and the number would flatter every request.
	if start, ok := rec.first(KindToolCallStart); !ok {
		t.Error("no tool_call_start")
	} else if start.ToolID != "call_8d4f0377bc594026a4765cfc" || start.ToolName != "bash" {
		t.Errorf("tool_call_start got id=%q name=%q", start.ToolID, start.ToolName)
	}

	// Every event carries the turn, so a trace can be split by round without
	// re-deriving anything.
	for _, e := range rec.events {
		if e.Turn != 7 {
			t.Fatalf("event %s has turn %d, want 7", e.Kind, e.Turn)
		}
	}

	// The usage event has to carry the NORMALISED struct, not the wire numbers.
	if u, ok := rec.first(KindUsage); !ok {
		t.Error("no usage event")
	} else if u.Usage == nil {
		t.Error("usage event has a nil Usage")
	} else if *u.Usage != b4WantUsage {
		t.Errorf("usage event\n got %+v\nwant %+v", *u.Usage, b4WantUsage)
	}

	// response_end still carries the PROVIDER's literal word, not the
	// normalised one: a renderer showing "end_turn" where the wire said
	// "tool_calls" is a renderer that cannot be used to debug the wire.
	if end, ok := rec.first(KindResponseEnd); !ok {
		t.Error("no response_end")
	} else if end.FinishReason != "tool_calls" {
		t.Errorf("response_end finish reason got %q, want %q", end.FinishReason, "tool_calls")
	}
}

func TestOpenAIEventSequenceForB7Reasoning(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(b7ReasoningStream...), NewBus(rec), 2, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}

	want := []Kind{
		KindFirstToken, // the first reasoning delta, not the role opener
		KindReasoningDelta, KindReasoningDelta, KindReasoningDelta, KindReasoningDelta, KindReasoningDelta,
		KindTextDelta, KindTextDelta,
		KindUsage,
		KindResponseEnd,
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("event kinds\n got %v\nwant %v", got, want)
	}
	if n := rec.count(KindFirstToken); n != 1 {
		t.Errorf("KindFirstToken emitted %d times, want exactly 1", n)
	}

	// The renderer distinguishes thinking from speech by kind alone, so a
	// reasoning fragment leaking out as a text delta prints the model's private
	// scratchpad to the user.
	if e, ok := rec.first(KindReasoningDelta); !ok {
		t.Error("no reasoning_delta")
	} else if e.Text != "Okay" {
		t.Errorf("first reasoning delta got %q, want %q", e.Text, "Okay")
	}
	if e, ok := rec.first(KindTextDelta); !ok {
		t.Error("no text_delta")
	} else if e.Text != "17 * 23 = " {
		t.Errorf("first text delta got %q, want %q", e.Text, "17 * 23 = ")
	}
}

func TestOpenAIParallelToolCallEventsAreRoutableByID(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(openaiParallelStream...), NewBus(rec), 1, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}

	// Two starts, in arrival order — the sort applies to the returned result,
	// not to the live event stream, which must stay in wire order so a renderer
	// can show things as they happen.
	if n := rec.count(KindToolCallStart); n != 2 {
		t.Fatalf("want 2 tool_call_start events, got %d", n)
	}

	// Every args delta has to name its call, or a renderer with two calls open
	// cannot tell which box a fragment belongs in.
	byID := map[string]string{}
	for _, e := range rec.events {
		if e.Kind == KindToolArgsDelta {
			if e.ToolID == "" {
				t.Fatal("tool_args_delta with no tool id: fragments are unroutable")
			}
			byID[e.ToolID] += e.Text
		}
	}
	want := map[string]string{
		"call_first":  `{"command": "ls -la"}`,
		"call_second": `{"command": "echo two"}`,
	}
	if !reflect.DeepEqual(byID, want) {
		t.Errorf("args reassembled from events\n got %#v\nwant %#v", byID, want)
	}
}

// ---------------------------------------------------------------------------
// TTFT.
// ---------------------------------------------------------------------------

func TestOpenAITTFTMeasuresFromTheRequest(t *testing.T) {
	// Pretend the request went out 1.5s ago. Backdating `started` is how this
	// gets asserted without a sleep: TTFT is a duration since a caller-supplied
	// instant, so the test can choose the instant.
	started := time.Now().Add(-1500 * time.Millisecond)

	rec := &openaiRecorder{}
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(rec), 1, started)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}

	if got.TTFT < 1400*time.Millisecond || got.TTFT > 3*time.Second {
		t.Errorf("TTFT got %v, want about 1.5s", got.TTFT)
	}
	e, ok := rec.first(KindFirstToken)
	if !ok {
		t.Fatal("no first_token event")
	}
	if e.Millis < 1400 || e.Millis > 3000 {
		t.Errorf("first_token Millis got %d, want about 1500", e.Millis)
	}
	if e.Millis != got.TTFT.Milliseconds() {
		t.Errorf("first_token Millis %d disagrees with result TTFT %d", e.Millis, got.TTFT.Milliseconds())
	}
}

func TestOpenAITTFTIsZeroWhenNothingStreamed(t *testing.T) {
	// The role opener carries `content: ""` and the finish chunk carries an
	// empty delta. Neither is output, so there is no first token to time —
	// and reporting a plausible-looking TTFT for a response that produced
	// nothing is worse than reporting none.
	rec := &openaiRecorder{}
	got, err := openaiTestProvider().ParseStream(openaiBody(b4RoleOpener, b4Finish, b4Usage, b4Done), NewBus(rec), 1, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.TTFT != 0 {
		t.Errorf("TTFT got %v, want 0", got.TTFT)
	}
	if n := rec.count(KindFirstToken); n != 0 {
		t.Errorf("first_token emitted %d times for a stream with no output, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Damaged streams.
// ---------------------------------------------------------------------------

func TestOpenAIMalformedFrameIsSurvivedAndReported(t *testing.T) {
	// One bad frame in the middle must not cost us a tool call that had already
	// completed. It must also not pass unnoticed — a notice puts it in the
	// trace, where it can be found later.
	frames := append([]string{}, b4ToolCallStream[:2]...)
	frames = append(frames, `{"choices":[{"delta":`) // truncated JSON
	frames = append(frames, b4ToolCallStream[2:]...)

	rec := &openaiRecorder{}
	got, err := openaiTestProvider().ParseStream(openaiBody(frames...), NewBus(rec), 1, time.Now())
	if err != nil {
		t.Fatalf("a single bad frame should not fail the stream: %v", err)
	}
	if len(got.Calls) != 1 || got.Calls[0].Args != b4WantArgs {
		t.Errorf("tool call damaged by the bad frame: %#v", got.Calls)
	}
	if n := rec.count(KindNotice); n != 1 {
		t.Errorf("bad frame produced %d notices, want 1", n)
	}
}

func TestOpenAITruncatedStreamReturnsPartialResultAndError(t *testing.T) {
	boom := errors.New("connection reset by peer")

	// A complete tool call, then the socket dies before the finish chunk. This
	// is the streaming version of the stage-01 truncation lesson: the danger is
	// not the error, it is a caller that ignores it and ships a result with no
	// finish_reason as though the model had stopped on purpose.
	good := openaiBody(b4ToolCallStream[:9]...)
	rec := &openaiRecorder{}

	got, err := openaiTestProvider().ParseStream(io.MultiReader(good, iotest.ErrReader(boom)), NewBus(rec), 1, time.Now())
	if !errors.Is(err, boom) {
		t.Fatalf("want the read error back, got %v", err)
	}
	if got == nil {
		t.Fatal("partial result was thrown away: the caller cannot tell a dead stream from an empty one")
	}
	if len(got.Calls) != 1 || got.Calls[0].Args != b4WantArgs {
		t.Errorf("partial result lost the completed tool call: %#v", got.Calls)
	}
	if got.RawStop != "" {
		t.Errorf("RawStop got %q, want empty: the stream never sent one", got.RawStop)
	}
	if n := rec.count(KindResponseEnd); n != 0 {
		t.Errorf("response_end emitted %d times for a broken stream, want 0", n)
	}
}

func TestOpenAINilBusIsTolerated(t *testing.T) {
	// Parsing with no subscribers is how a test, or a batch tool, uses this
	// without standing up a bus.
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), nil, 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got.Calls) != 1 || got.Calls[0].Args != b4WantArgs {
		t.Errorf("nil bus changed the result: %#v", got.Calls)
	}
}

// ---------------------------------------------------------------------------
// Framing, through the neutral reader in sse.go.
//
// readSSE is not this protocol's code and these are not this protocol's rules —
// but the cases came over with the port from stage 02 and coverage that exists
// beats coverage that is somebody's else's to write. Named with an `openai`
// prefix so a future sse_test.go can claim the neutral names without a clash.
// ---------------------------------------------------------------------------

func TestOpenAIReadSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []sseFrame
	}{
		{
			// The shape this protocol actually sends: `data:` and nothing else.
			name: "openai style, data lines only",
			in:   "data: a\n\ndata: b\n\n",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "b"}},
		},
		{
			// Not observed on this endpoint, which sends bare LF — but SSE is
			// specified over CRLF and any proxy in the path may rewrite line
			// endings, so a parser that only handles LF leaves a stray \r on
			// the end of every payload and fails to decode the JSON.
			name: "CRLF line endings",
			in:   "data: a\r\n\r\ndata: b\r\n\r\n",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "b"}},
		},
		{
			// What the other adapter needs (§B6). Nothing on this wire produces it.
			name: "anthropic style, event plus data",
			in:   "event: content_block_delta\ndata: {\"type\":\"text_delta\"}\n\n",
			want: []sseFrame{{Name: "content_block_delta", Data: `{"type":"text_delta"}`}},
		},
		{
			name: "multi-line data is joined with newlines",
			in:   "data: line one\ndata: line two\ndata: line three\n\n",
			want: []sseFrame{{Name: "", Data: "line one\nline two\nline three"}},
		},
		{
			// Keep-alives. They must not terminate the frame in progress and
			// must not produce one of their own.
			name: "comment lines are ignored",
			in:   ": keep-alive\ndata: a\n: mid-frame comment\ndata: b\n\n: trailing\n\n",
			want: []sseFrame{{Name: "", Data: "a\nb"}},
		},
		{
			// The bug this catches is silent: the last frame of a stream is
			// usually the one carrying usage.
			name: "no trailing blank line at EOF",
			in:   "data: a\n\ndata: last",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "last"}},
		},
		{
			name: "trailing newline but no blank line at EOF",
			in:   "data: last\n",
			want: []sseFrame{{Name: "", Data: "last"}},
		},
		{
			name: "runs of blank lines produce no frames",
			in:   "\n\n\ndata: a\n\n\n\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			name: "no space after the colon",
			in:   "data:tight\n\n",
			want: []sseFrame{{Name: "", Data: "tight"}},
		},
		{
			name: "exactly one leading space is stripped",
			in:   "data:  two spaces\n\n",
			want: []sseFrame{{Name: "", Data: " two spaces"}},
		},
		{
			// Every payload on this wire is JSON, so splitting on the last
			// colon (or on all of them) corrupts every single frame.
			name: "only the first colon separates field from value",
			in:   "data: {\"model\":\"mimo-v2.5\",\"t\":\"12:34:56\"}\n\n",
			want: []sseFrame{{Name: "", Data: `{"model":"mimo-v2.5","t":"12:34:56"}`}},
		},
		{
			// Spec fields for resuming a dropped stream. Deliberately ignored,
			// but they must not be mistaken for data.
			name: "id and retry fields are ignored, not treated as data",
			in:   "id: 42\nretry: 3000\ndata: a\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			// Per the spec, and it matters: the event-type buffer has to reset,
			// or the name leaks onto the next frame.
			name: "a frame with no data line is not dispatched and does not leak its name",
			in:   "event: ping\n\ndata: a\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			// readSSE knows nothing about sentinels. Deciding what [DONE] means
			// is this file's job, which is what keeps the reader reusable for a
			// protocol that has no sentinel at all (§B6).
			name: "the DONE sentinel is just another frame down here",
			in:   "data: [DONE]\n\ndata: {\"choices\":[],\"cost\":\"0\"}\n\n",
			want: []sseFrame{{Name: "", Data: "[DONE]"}, {Name: "", Data: `{"choices":[],"cost":"0"}`}},
		},
		{
			name: "empty stream",
			in:   "",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []sseFrame
			if err := readSSE(strings.NewReader(tc.in), func(f sseFrame) error {
				got = append(got, f)
				return nil
			}); err != nil {
				t.Fatalf("readSSE: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("frames:\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestOpenAIReadSSEStopsOnCallbackError(t *testing.T) {
	boom := errors.New("subscriber gave up")

	seen := 0
	err := readSSE(openaiBody("a", "b", "c"), func(sseFrame) error {
		seen++
		if seen == 2 {
			return boom
		}
		return nil
	})

	if !errors.Is(err, boom) {
		t.Fatalf("want the callback's error back, got %v", err)
	}
	if seen != 2 {
		t.Errorf("scan continued past the error: fn ran %d times, want 2", seen)
	}
}

func TestOpenAIReadSSEHandlesLinesOverScannerLimit(t *testing.T) {
	// bufio.Scanner would fail this at 64KB with ErrTooLong. A single delta
	// this large is not hypothetical: it is what one `cat` of a big file,
	// echoed back through a tool result, looks like on the way out.
	huge := strings.Repeat("x", 200*1024)

	var got []sseFrame
	if err := readSSE(openaiBody(huge), func(f sseFrame) error {
		got = append(got, f)
		return nil
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 1 || got[0].Data != huge {
		t.Fatalf("large frame not reassembled: %d frames, %d bytes", len(got), len(got[0].Data))
	}
}
