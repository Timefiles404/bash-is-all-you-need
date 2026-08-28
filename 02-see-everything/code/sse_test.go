// Tests for the SSE reader and the OpenAI stream parser.
//
// Every frame constant below is copied out of external/wire-notes.md — these are
// bytes this endpoint actually sent, not bytes invented to make a parser look
// good. That is the entire point: a fixture you wrote from the specification
// tests your reading of the specification, and this endpoint does not match the
// specification (see §B4 frames 11 and 13). Where a fixture had to be
// reconstructed or invented, the comment above it says so and says why.
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
	parOpener1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_second","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	parOpener0 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	parArg1a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"echo "}}]}}]}`
	parArg0a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"ls"}}]}}]}`
	parArg1b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"two\"}"}}]}}]}`
	parArg0b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":" -la\"}"}}]}}]}`
)

var parallelToolCallStream = []string{
	b4RoleOpener,
	parOpener1, parOpener0,
	parArg1a, parArg0a,
	parArg1b, parArg0b,
	b4Finish,
	b4Usage,
	b4Done,
	b4PostDone,
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// sseBody renders payloads the way §B4 shows this endpoint rendering them:
// `data: <payload>` then one blank line, LF-terminated (the doc shows it with
// `cat -A`, where every line ends `$` and no `^M` appears).
func sseBody(frames ...string) io.Reader {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return strings.NewReader(b.String())
}

// sseRecorder is a Subscriber that keeps everything, which is the cheapest
// possible demonstration of why the agent core emits events instead of
// printing: the test asserts on the event sequence and never touches stdout.
type sseRecorder struct{ events []Event }

func (r *sseRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func (r *sseRecorder) kinds() []Kind {
	out := make([]Kind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *sseRecorder) count(k Kind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (r *sseRecorder) first(k Kind) (Event, bool) {
	for _, e := range r.events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

// ---------------------------------------------------------------------------
// readSSE: framing only.
// ---------------------------------------------------------------------------

func TestReadSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []sseFrame
	}{
		{
			// The shape this stage actually meets: `data:` and nothing else.
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
			// What stage 03 needs (§B6). Nothing in this stage produces it.
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
			// is the payload parser's job, which is what keeps this half
			// reusable for a protocol that has no sentinel at all (§B6).
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

func TestReadSSEStopsOnCallbackError(t *testing.T) {
	boom := errors.New("subscriber gave up")

	seen := 0
	err := readSSE(sseBody("a", "b", "c"), func(sseFrame) error {
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

func TestReadSSEHandlesLinesOverScannerLimit(t *testing.T) {
	// bufio.Scanner would fail this at 64KB with ErrTooLong. A single delta
	// this large is not hypothetical: it is what one `cat` of a big file,
	// echoed back through a tool result, looks like on the way out.
	huge := strings.Repeat("x", 200*1024)

	var got []sseFrame
	if err := readSSE(sseBody(huge), func(f sseFrame) error {
		got = append(got, f)
		return nil
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 1 || got[0].Data != huge {
		t.Fatalf("large frame not reassembled: %d frames, %d bytes", len(got), len(got[0].Data))
	}
}

// ---------------------------------------------------------------------------
// parseOpenAIStream: the recorded streams, end to end.
// ---------------------------------------------------------------------------

func TestParseOpenAIStream(t *testing.T) {
	cases := []struct {
		name          string
		frames        []string
		wantText      string
		wantReasoning string
		wantTools     []streamToolCall
		wantFinish    string
		wantUsage     Usage
	}{
		{
			// The headline case: §B4 verbatim, all 13 frames.
			name:       "B4 tool call, all thirteen frames",
			frames:     b4ToolCallStream,
			wantFinish: "tool_calls",
			wantUsage:  b4WantUsage,
			wantTools: []streamToolCall{{
				ID:   "call_8d4f0377bc594026a4765cfc",
				Name: "bash",
				Args: b4WantArgs,
			}},
		},
		{
			// §B7: two fields on one delta object must land in two places.
			name:          "B7 reasoning and text are kept apart",
			frames:        b7ReasoningStream,
			wantText:      "17 * 23 = 391",
			wantReasoning: "Okay, the user is asking for the product of 17 and ",
			wantFinish:    "stop",
			wantUsage:     b4WantUsage,
		},
		{
			// The frame that panics a choices[0] parser, on its own.
			name:      "usage frame alone, choices is an empty array",
			frames:    []string{b4Usage},
			wantUsage: b4WantUsage,
		},
		{
			// §B4 frame 13, on its own: empty choices AND an unknown top-level
			// key. Arriving after the sentinel is what makes it easy to never
			// have tested this.
			name:   "post-DONE cost frame alone",
			frames: []string{b4Done, b4PostDone},
		},
		{
			// Draining past the sentinel is only defensible if it actually
			// picks something up. Move usage behind [DONE] and this is the
			// difference between correct accounting and a silent zero.
			name:       "frames after the sentinel are still read",
			frames:     []string{b4RoleOpener, b4Finish, b4Done, b4Usage, b4PostDone},
			wantFinish: "tool_calls",
			wantUsage:  b4WantUsage,
		},
		{
			// Parallel calls: independent accumulation, ascending index order.
			name:       "two parallel tool calls interleaved",
			frames:     parallelToolCallStream,
			wantFinish: "tool_calls",
			wantUsage:  b4WantUsage,
			wantTools: []streamToolCall{
				{ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
				{ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
			},
		},
		{
			// A stream that produced nothing at all still has to come back
			// clean rather than half-initialised.
			name:       "role opener and finish only",
			frames:     []string{b4RoleOpener, b4Finish, b4Done},
			wantFinish: "tool_calls",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOpenAIStream(sseBody(tc.frames...), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("parseOpenAIStream: %v", err)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text\n got %q\nwant %q", got.Text, tc.wantText)
			}
			if got.Reasoning != tc.wantReasoning {
				t.Errorf("Reasoning\n got %q\nwant %q", got.Reasoning, tc.wantReasoning)
			}
			if got.FinishReason != tc.wantFinish {
				t.Errorf("FinishReason got %q, want %q", got.FinishReason, tc.wantFinish)
			}
			if got.Usage != tc.wantUsage {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.wantUsage)
			}
			if !reflect.DeepEqual(got.ToolCalls, tc.wantTools) {
				t.Errorf("ToolCalls\n got %#v\nwant %#v", got.ToolCalls, tc.wantTools)
			}
		})
	}
}

// TestB4ArgsReassembleIntoValidJSON is the payoff for never parsing a fragment.
// Not one of the seven pieces in §B4 is valid JSON on its own; the concatenation
// is, and that is the only place a parse is allowed to happen.
func TestB4ArgsReassembleIntoValidJSON(t *testing.T) {
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.ToolCalls))
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(got.ToolCalls[0].Args), &args); err != nil {
		t.Fatalf("assembled args are not valid JSON: %v (%q)", err, got.ToolCalls[0].Args)
	}
	if args.Command != "ls -la /srv/app" {
		t.Errorf("command got %q, want %q", args.Command, "ls -la /srv/app")
	}
}

// TestToolIDSurvivesTheNullChunks is the id-latching regression, stated on its
// own so the failure message names the actual disease. Frames 3–9 all carry
// `"id":null`; an unguarded assignment leaves this empty and the tool call
// becomes unanswerable, because the API requires that id back in the reply.
func TestToolIDSurvivesTheNullChunks(t *testing.T) {
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].ID != "call_8d4f0377bc594026a4765cfc" {
		t.Errorf("tool id was lost to a later null chunk: got %q", got.ToolCalls[0].ID)
	}
	if got.ToolCalls[0].Name != "bash" {
		t.Errorf("tool name was lost to a later null chunk: got %q", got.ToolCalls[0].Name)
	}
}

// TestParallelToolCallsComeBackInIndexOrder runs the same stream many times
// because Go randomises map iteration order on purpose. One pass would catch a
// missing sort roughly half the time — a test that fails one commit in two is
// worse than no test, because it teaches people to re-run CI. Twenty passes
// puts a false pass at about one in a million.
func TestParallelToolCallsComeBackInIndexOrder(t *testing.T) {
	want := []streamToolCall{
		{ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
		{ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
	}

	for i := 0; i < 20; i++ {
		got, err := parseOpenAIStream(sseBody(parallelToolCallStream...), NewBus(), 1, time.Now())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if !reflect.DeepEqual(got.ToolCalls, want) {
			t.Fatalf("pass %d: out of index order\n got %#v\nwant %#v", i, got.ToolCalls, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Usage normalisation — the direction reversal.
// ---------------------------------------------------------------------------

func TestUsageNormalisation(t *testing.T) {
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
			name: "no cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 506, Output: 26},
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

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOpenAIStream(sseBody(tc.in, b4Done), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("parseOpenAIStream: %v", err)
			}
			if got.Usage != tc.want {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.want)
			}
		})
	}
}

// TestUsagePromptRoundTrips states the invariant that makes the reversal
// checkable without doing the subtraction again: whatever the split, Prompt()
// has to equal the prompt_tokens the endpoint reported.
func TestUsagePromptRoundTrips(t *testing.T) {
	got, err := parseOpenAIStream(sseBody(b4Usage), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if got.Usage.Prompt() != 506 {
		t.Errorf("Prompt() got %d, want 506 (the prompt_tokens on the wire)", got.Usage.Prompt())
	}
	if got.Usage.CacheWrite != 0 {
		t.Errorf("CacheWrite got %d, want 0: this protocol reports no write figure", got.Usage.CacheWrite)
	}
}

// ---------------------------------------------------------------------------
// The event stream. These are the tests the event bus exists for.
// ---------------------------------------------------------------------------

func TestEventSequenceForB4ToolCall(t *testing.T) {
	rec := &sseRecorder{}
	if _, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(rec), 7, time.Now()); err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
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

	if end, ok := rec.first(KindResponseEnd); !ok {
		t.Error("no response_end")
	} else if end.FinishReason != "tool_calls" {
		t.Errorf("response_end finish reason got %q, want %q", end.FinishReason, "tool_calls")
	}
}

func TestEventSequenceForB7Reasoning(t *testing.T) {
	rec := &sseRecorder{}
	if _, err := parseOpenAIStream(sseBody(b7ReasoningStream...), NewBus(rec), 2, time.Now()); err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
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

func TestParallelToolCallEventsAreRoutableByID(t *testing.T) {
	rec := &sseRecorder{}
	if _, err := parseOpenAIStream(sseBody(parallelToolCallStream...), NewBus(rec), 1, time.Now()); err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
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

func TestTTFTMeasuresFromTheRequest(t *testing.T) {
	// Pretend the request went out 1.5s ago. Backdating `started` is how this
	// gets asserted without a sleep: TTFT is a duration since a caller-supplied
	// instant, so the test can choose the instant.
	started := time.Now().Add(-1500 * time.Millisecond)

	rec := &sseRecorder{}
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(rec), 1, started)
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
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

func TestTTFTIsZeroWhenNothingStreamed(t *testing.T) {
	// The role opener carries `content: ""` and the finish chunk carries an
	// empty delta. Neither is output, so there is no first token to time —
	// and reporting a plausible-looking TTFT for a response that produced
	// nothing is worse than reporting none.
	rec := &sseRecorder{}
	got, err := parseOpenAIStream(sseBody(b4RoleOpener, b4Finish, b4Usage, b4Done), NewBus(rec), 1, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
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

func TestMalformedFrameIsSurvivedAndReported(t *testing.T) {
	// One bad frame in the middle must not cost us a tool call that had already
	// completed. It must also not pass unnoticed — a notice puts it in the
	// trace, where it can be found later.
	frames := append([]string{}, b4ToolCallStream[:2]...)
	frames = append(frames, `{"choices":[{"delta":`) // truncated JSON
	frames = append(frames, b4ToolCallStream[2:]...)

	rec := &sseRecorder{}
	got, err := parseOpenAIStream(sseBody(frames...), NewBus(rec), 1, time.Now())
	if err != nil {
		t.Fatalf("a single bad frame should not fail the stream: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Args != b4WantArgs {
		t.Errorf("tool call damaged by the bad frame: %#v", got.ToolCalls)
	}
	if n := rec.count(KindNotice); n != 1 {
		t.Errorf("bad frame produced %d notices, want 1", n)
	}
}

func TestTruncatedStreamReturnsPartialResultAndError(t *testing.T) {
	boom := errors.New("connection reset by peer")

	// A complete tool call, then the socket dies before the finish chunk. This
	// is the streaming version of the stage-01 truncation lesson: the danger is
	// not the error, it is a caller that ignores it and ships a result with no
	// finish_reason as though the model had stopped on purpose.
	good := sseBody(b4ToolCallStream[:9]...)
	rec := &sseRecorder{}

	got, err := parseOpenAIStream(io.MultiReader(good, iotest.ErrReader(boom)), NewBus(rec), 1, time.Now())
	if !errors.Is(err, boom) {
		t.Fatalf("want the read error back, got %v", err)
	}
	if got == nil {
		t.Fatal("partial result was thrown away: the caller cannot tell a dead stream from an empty one")
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Args != b4WantArgs {
		t.Errorf("partial result lost the completed tool call: %#v", got.ToolCalls)
	}
	if got.FinishReason != "" {
		t.Errorf("FinishReason got %q, want empty: the stream never sent one", got.FinishReason)
	}
	if n := rec.count(KindResponseEnd); n != 0 {
		t.Errorf("response_end emitted %d times for a broken stream, want 0", n)
	}
}

func TestNilBusIsTolerated(t *testing.T) {
	// Parsing with no subscribers is how a test, or a batch tool, uses this
	// without standing up a bus.
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), nil, 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Args != b4WantArgs {
		t.Errorf("nil bus changed the result: %#v", got.ToolCalls)
	}
}
