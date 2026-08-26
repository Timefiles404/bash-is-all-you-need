// Tests for the Anthropic protocol adapter.
//
// Every stream fixture below is built from docs/wire-notes.md §B6 and §B7 —
// bytes this endpoint actually sent, not bytes invented to make a parser look
// correct. That distinction is the whole reason the fixtures are here: a
// fixture written from the specification tests your reading of the
// specification, and this endpoint contradicts the specification in at least
// four places that matter to this file (pings outside the stream, no [DONE], a
// wrong usage report in message_start, and a `</think>` tag leaking into
// visible text).
//
// Where a frame's envelope had to be reconstructed — the wire notes record some
// events as a bare `delta` object or as a name in a sequence listing — the
// comment above it says so. The values inside are always verbatim.
//
// No network, no API key, no `-short` skips. The whole file runs on a plane.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// The adapter must satisfy the contract in provider.go. Asserting it here means
// a signature drift is a compile error in the test build, next to the tests
// that explain what each method owes the caller.
var _ Provider = (*anthropicProvider)(nil)

// ---------------------------------------------------------------------------
// Frame builders.
//
// The wire notes print the argument fragments as bare values and the deltas as
// bare `delta` objects, so these helpers rebuild the envelope around a verbatim
// value. json.Marshal does the string escaping, which is exactly how the
// gateway produced the bytes in the first place — writing `\"` by hand in a Go
// literal is how a fixture stops matching the wire.
// ---------------------------------------------------------------------------

func anthQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func anthArgsDelta(index int, partial string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, anthQuote(partial))
}

func anthTextDelta(index int, text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, anthQuote(text))
}

func anthThinkingDelta(index int, thinking string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`, index, anthQuote(thinking))
}

func anthToolStart(index int, id, name string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, index, anthQuote(id), anthQuote(name))
}

func anthBlockStop(index int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
}

func anthMessageDelta(stopReason string, input, output, cacheWrite, cacheRead int) string {
	return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s},"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}`,
		anthQuote(stopReason), input, output, cacheWrite, cacheRead)
}

// ---------------------------------------------------------------------------
// §B6 / §B7 fixtures.
// ---------------------------------------------------------------------------

const (
	// VERBATIM §B6. Note what is NOT in it: no stop_reason, no cache counters,
	// and an input_tokens figure that the very next usage report contradicts.
	b6MessageStart = `{"type":"message_start","message":{"id":"msg_e3f9307e-2dc9-41f0-a70e-cca934593aa0","type":"message","role":"assistant","model":"qwen3.7-plus","content":[],"usage":{"input_tokens":56,"output_tokens":0}}}`

	// VERBATIM §B6 — the tool_use announcement. `input` is an empty object and
	// the id/name appear here and in no other frame.
	b6ToolStart0 = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ff07c814f3f34014aa526469","name":"bash","input":{}}}`

	// VERBATIM §B6 — usage that disagrees with message_start about the same
	// request, and the only frame carrying stop_reason or cache counters.
	b6MessageDelta = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":291,"output_tokens":63,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

	// VERBATIM §B6 — the trailing ping, carrying `cost` as an extra key.
	b6PingWithCost = `{"type":"ping","cost":"0"}`

	// RECONSTRUCTED shape: §B6 lists `ping` and `message_stop` in the event
	// sequence but prints a body only for the trailing ping.
	b6Ping        = `{"type":"ping"}`
	b6MessageStop = `{"type":"message_stop"}`

	// VERBATIM §B7 — a thinking block, its (always empty) signature delta, and
	// the text block that opens at the NEXT index once it closes.
	b7ThinkingStart   = `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`
	b7ThinkingDelta   = `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let"}}`
	b7SignatureDelta  = `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`
	b7BlockStop0      = `{"type":"content_block_stop","index":0}`
	b7TextStart1      = `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`
	b7TextDeltaFirst  = `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"To calculate"}}`
	b7TextDeltaSecond = `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" 17 ×"}}`
)

// b6ArgFragments are the six `partial_json` values §B6 recorded, in order.
//
// The first is the EMPTY STRING, fragment three ends mid-path (`/srv`) and
// fragment four resumes it (`/app`). At no point is a fragment parseable JSON,
// which is why the adapter concatenates raw bytes and never inspects them.
var b6ArgFragments = []string{
	``,
	`{"command": "ls`,
	` -la /srv`,
	`/app`,
	`"`,
	`}`,
}

// b6WantArgs is what §B6 says those fragments concatenate to.
const b6WantArgs = `{"command": "ls -la /srv/app"}`

// b6LeakText is the gateway's `</think>` leak, VERBATIM §B6 (and §A3b, which
// caught the identical string in a non-streaming response). A newline, the bare
// closing tag, two more newlines — an entire user-visible text block containing
// no model output at all.
const b6LeakText = "\n</think>\n\n"

// b6FullStream is §B6's two-tool-call stream end to end:
//
//	ping message_start
//	content_block_start content_block_delta x6 content_block_stop  (index 0, tool_use)
//	content_block_start content_block_delta   content_block_stop   (index 1, text)
//	content_block_start content_block_delta x6 content_block_stop  (index 2, tool_use)
//	message_delta message_stop ping
//
// The index-2 block is CONSTRUCTED — §B6 records its position, type and delta
// count but not its id or arguments — and it is deliberately given a different
// command, so a parser that shares one buffer across blocks produces visible
// garbage instead of a subtle mix-up.
func b6FullStream() []string {
	frames := []string{
		b6Ping, // BEFORE message_start. The spec says this cannot happen.
		b6MessageStart,
		b6ToolStart0,
	}
	for _, frag := range b6ArgFragments {
		frames = append(frames, anthArgsDelta(0, frag))
	}
	frames = append(frames,
		anthBlockStop(0),

		// Index 1: the `</think>` leak, as its own text content block.
		b7TextStart1,
		anthTextDelta(1, b6LeakText),
		anthBlockStop(1),

		// Index 2: the second tool call.
		anthToolStart(2, "toolu_5ae0ccdc34f44d30a2217c5e", "bash"),
	)
	for _, frag := range []string{``, `{"command": "wc`, ` -l /srv`, `/app/main`, `.go"`, `}`} {
		frames = append(frames, anthArgsDelta(2, frag))
	}
	frames = append(frames,
		anthBlockStop(2),
		b6MessageDelta,
		b6MessageStop,
		b6PingWithCost, // AFTER message_stop, carrying `cost`.
	)
	return frames
}

// ---------------------------------------------------------------------------
// Harness.
// ---------------------------------------------------------------------------

// anthSSE renders frames the way §B6 describes this endpoint rendering them:
// `event: <name>`, `data: <payload>`, blank line. The event name is taken from
// the payload's own `type`, which is how the gateway builds it.
//
// The body ends WITHOUT a trailing blank line on the last frame, because that
// is how this stream really ends: no `[DONE]`, no terminator, just the
// connection closing. A reader that only dispatches on a blank line silently
// drops the last frame of every response — and on this protocol the last frames
// are message_delta and the cost ping.
func anthSSE(frames ...string) io.Reader {
	var b strings.Builder
	for i, f := range frames {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n", anthEventName(f), f)
		if i < len(frames)-1 {
			b.WriteString("\n")
		}
	}
	return strings.NewReader(b.String())
}

func anthEventName(payload string) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return "unknown"
	}
	return probe.Type
}

// anthRecorder keeps every event, which is the cheapest possible demonstration
// of why the agent core emits events instead of printing: these tests assert on
// an event sequence and never touch stdout.
type anthRecorder struct{ events []Event }

func (r *anthRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func (r *anthRecorder) kinds() []Kind {
	out := make([]Kind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *anthRecorder) count(k Kind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (r *anthRecorder) first(k Kind) (Event, bool) {
	for _, e := range r.events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

func (r *anthRecorder) textsOf(k Kind) []string {
	var out []string
	for _, e := range r.events {
		if e.Kind == k {
			out = append(out, e.Text)
		}
	}
	return out
}

const anthTestTurn = 7

func anthProvider() *anthropicProvider {
	return newAnthropicProvider("https://opencode.ai/zen/go/v1", "sk-test", "qwen3.7-plus")
}

// anthParse runs the adapter over a frame list and hands back everything a test
// might want to assert on.
func anthParse(t *testing.T, frames []string) (*CallResult, *anthRecorder, error) {
	t.Helper()
	rec := &anthRecorder{}
	res, err := anthProvider().ParseStream(anthSSE(frames...), NewBus(rec), anthTestTurn, time.Now())
	if res == nil {
		t.Fatal("ParseStream returned a nil result; it must always return what it assembled")
	}
	return res, rec, err
}

// ---------------------------------------------------------------------------
// The full §B6 stream.
// ---------------------------------------------------------------------------

func TestAnthropicFullB6Stream(t *testing.T) {
	res, rec, err := anthParse(t, b6FullStream())
	if err != nil {
		t.Fatalf("the recorded stream must parse cleanly: %v", err)
	}

	// A ping before message_start and a ping after message_stop, neither of
	// which is a token, a message, or a reason to stop reading.
	if got := rec.count(KindFirstToken); got != 1 {
		t.Errorf("KindFirstToken emitted %d times, want exactly 1 (a ping is not a token)", got)
	}

	if len(res.Calls) != 2 {
		t.Fatalf("got %d tool calls, want 2 (indices 0 and 2, with a text block between them)", len(res.Calls))
	}
	if res.Calls[0].ID != "toolu_ff07c814f3f34014aa526469" || res.Calls[0].Name != "bash" {
		t.Errorf("first call id/name = %q/%q, want the values from content_block_start", res.Calls[0].ID, res.Calls[0].Name)
	}
	if res.Calls[0].Args != b6WantArgs {
		t.Errorf("first call args = %q, want %q", res.Calls[0].Args, b6WantArgs)
	}
	if res.Calls[0].Kind != BlockToolCall {
		t.Errorf("call block kind = %q, want %q", res.Calls[0].Kind, BlockToolCall)
	}
	if want := `{"command": "wc -l /srv/app/main.go"}`; res.Calls[1].Args != want {
		t.Errorf("second call args = %q, want %q — fragments leaked between content blocks", res.Calls[1].Args, want)
	}

	// The only text block in this stream was the `</think>` leak, so there is
	// no visible text at all.
	if res.Text != "" {
		t.Errorf("Text = %q, want empty: the only text block was gateway residue", res.Text)
	}
	if res.Thinking != "" {
		t.Errorf("Thinking = %q, want empty: this stream has no thinking block", res.Thinking)
	}

	if res.RawStop != "tool_use" {
		t.Errorf("RawStop = %q, want the literal wire string %q", res.RawStop, "tool_use")
	}
	if res.Stop != StopToolUse {
		t.Errorf("Stop = %q, want %q", res.Stop, StopToolUse)
	}

	want := Usage{Input: 291, Output: 63}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v (message_delta, not message_start)", res.Usage, want)
	}

	end, ok := rec.first(KindResponseEnd)
	if !ok {
		t.Fatal("no KindResponseEnd on a stream that ended cleanly")
	}
	if end.FinishReason != "tool_use" {
		t.Errorf("KindResponseEnd.FinishReason = %q, want the raw wire string", end.FinishReason)
	}

	// Every event carries the turn, so a trace of a multi-turn session can be
	// sliced back apart.
	for _, e := range rec.events {
		if e.Turn != anthTestTurn {
			t.Fatalf("event %s carries turn %d, want %d", e.Kind, e.Turn, anthTestTurn)
		}
	}
}

// ---------------------------------------------------------------------------
// Tool arguments.
// ---------------------------------------------------------------------------

func TestAnthropicToolArgsReassembly(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   []Block
	}{
		{
			// The observed fragments, verbatim §B6, including the empty first
			// one and the two that split a path down the middle.
			name: "observed fragments reassemble",
			frames: func() []string {
				f := []string{b6Ping, b6MessageStart, b6ToolStart0}
				for _, frag := range b6ArgFragments {
					f = append(f, anthArgsDelta(0, frag))
				}
				return append(f, anthBlockStop(0), b6MessageDelta, b6MessageStop, b6PingWithCost)
			}(),
			want: []Block{{
				Kind: BlockToolCall,
				ID:   "toolu_ff07c814f3f34014aa526469",
				Name: "bash",
				Args: b6WantArgs,
			}},
		},
		{
			// CONSTRUCTED. Two blocks open before either closes and their
			// fragments interleave, and the higher index opens FIRST — so an
			// implementation that accumulates into one buffer, or returns calls
			// in arrival order, fails every run instead of one in two.
			name: "interleaved blocks stay separate and come back index-ordered",
			frames: []string{
				b6Ping, b6MessageStart,
				anthToolStart(2, "toolu_second", "bash"),
				anthToolStart(0, "toolu_first", "bash"),
				anthArgsDelta(2, `{"command": "echo `),
				anthArgsDelta(0, `{"command": "ls`),
				anthArgsDelta(2, `two"}`),
				anthArgsDelta(0, ` -la"}`),
				anthBlockStop(2), anthBlockStop(0),
				b6MessageDelta, b6MessageStop,
			},
			want: []Block{
				{Kind: BlockToolCall, ID: "toolu_first", Name: "bash", Args: `{"command": "ls -la"}`},
				{Kind: BlockToolCall, ID: "toolu_second", Name: "bash", Args: `{"command": "echo two"}`},
			},
		},
		{
			// A tool call whose arguments never arrived. The id and name still
			// have to survive: without the id there is no tool_use_id to answer
			// with, and the turn cannot be closed at all.
			name: "announced but empty stays announced",
			frames: []string{
				b6Ping, b6MessageStart,
				anthToolStart(0, "toolu_empty", "bash"),
				anthArgsDelta(0, ``),
				anthBlockStop(0),
				b6MessageDelta, b6MessageStop,
			},
			want: []Block{{Kind: BlockToolCall, ID: "toolu_empty", Name: "bash", Args: ""}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, rec, err := anthParse(t, tc.frames)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(res.Calls, tc.want) {
				t.Errorf("calls =\n  %+v\nwant\n  %+v", res.Calls, tc.want)
			}

			// The empty first fragment must not become a trace line: an
			// argument delta carrying no characters is noise in the request
			// inspector and in every renderer downstream.
			for _, txt := range rec.textsOf(KindToolArgsDelta) {
				if txt == "" {
					t.Error("emitted a KindToolArgsDelta with empty text; the first observed fragment is \"\" and carries nothing")
				}
			}

			// Every announcement must name its call.
			for _, e := range rec.events {
				if e.Kind == KindToolCallStart && (e.ToolID == "" || e.ToolName == "") {
					t.Errorf("KindToolCallStart missing id or name: %+v", e)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Usage. The highest-value test in this file.
// ---------------------------------------------------------------------------

func TestAnthropicUsageComesFromMessageDeltaNotMessageStart(t *testing.T) {
	cases := []struct {
		name       string
		frames     []string
		want       Usage
		wantPrompt int
		wantRaw    string
		wantStop   StopReason
	}{
		{
			// §B6, the disagreeing pair: message_start says input_tokens 56,
			// message_delta says 291, for the same request. The non-streaming
			// call with the same prompt agreed with 291. If this assertion ever
			// reads 56, the adapter is trusting the frame the spec calls
			// authoritative and this endpoint gets wrong.
			name:       "message_start says 56, message_delta says 291, 291 wins",
			frames:     []string{b6Ping, b6MessageStart, b6ToolStart0, anthArgsDelta(0, `{}`), anthBlockStop(0), b6MessageDelta, b6MessageStop, b6PingWithCost},
			want:       Usage{Input: 291, Output: 63},
			wantPrompt: 291,
			wantRaw:    "tool_use",
			wantStop:   StopToolUse,
		},
		{
			// A warm cache call, verified live: input=18, cache_creation=0,
			// cache_read=17967. This protocol's input_tokens is ONLY the
			// uncached remainder, so it maps straight across and the context
			// size is the SUM — 17,985 — a number no single field on the wire
			// reports. (§C8 measured the same shape on a smaller handbook:
			// input 18, cache_read 9,775.)
			//
			// An adapter that copied the OpenAI direction and subtracted
			// cache_read from input would report -17,949 here.
			name:       "warm cache: input is only the uncached remainder",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "ACK"), anthBlockStop(1), anthMessageDelta("end_turn", 18, 249, 0, 17967), b6MessageStop},
			want:       Usage{Input: 18, CacheRead: 17967, Output: 249},
			wantPrompt: 17985,
			wantRaw:    "end_turn",
			wantStop:   StopEndTurn,
		},
		{
			// The first call against a cold cache writes the prefix. CacheWrite
			// is its own field because it is billed at ~1.25x, not 0.1x.
			name:       "cold cache: creation tokens land in CacheWrite",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "ACK"), anthBlockStop(1), anthMessageDelta("end_turn", 18, 249, 9775, 0), b6MessageStop},
			want:       Usage{Input: 18, CacheWrite: 9775, Output: 249},
			wantPrompt: 9793,
			wantRaw:    "end_turn",
			wantStop:   StopEndTurn,
		},
		{
			// The stream died before message_delta. There is NO fallback to
			// message_start's figure: a missing number can be seen and chased,
			// a plausible wrong one ends up in a cost dashboard. And with no
			// stop_reason at all, the turn is StopUnknown — not "probably
			// fine".
			name:       "no message_delta: no usage, and an unknown stop",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "half a sen")},
			want:       Usage{},
			wantPrompt: 0,
			wantRaw:    "",
			wantStop:   StopUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, rec, err := anthParse(t, tc.frames)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Usage != tc.want {
				t.Errorf("Usage = %+v, want %+v", res.Usage, tc.want)
			}
			if got := res.Usage.Prompt(); got != tc.wantPrompt {
				t.Errorf("Usage.Prompt() = %d, want %d — the context size is Input+CacheWrite+CacheRead", got, tc.wantPrompt)
			}
			if res.RawStop != tc.wantRaw {
				t.Errorf("RawStop = %q, want %q", res.RawStop, tc.wantRaw)
			}
			if res.Stop != tc.wantStop {
				t.Errorf("Stop = %q, want %q", res.Stop, tc.wantStop)
			}

			// The KindUsage event must carry the same normalised figures, and
			// must not be emitted at all when nothing was reported.
			ev, ok := rec.first(KindUsage)
			if tc.want == (Usage{}) {
				if ok {
					t.Errorf("emitted KindUsage %+v when the stream reported none", ev.Usage)
				}
				return
			}
			if !ok {
				t.Fatal("no KindUsage event")
			}
			if ev.Usage == nil || *ev.Usage != tc.want {
				t.Errorf("KindUsage carried %+v, want %+v", ev.Usage, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Thinking.
// ---------------------------------------------------------------------------

func TestAnthropicThinkingAndTextStaySeparate(t *testing.T) {
	// §B7 verbatim: a thinking block at index 0 with its own delta type, closed
	// before the text block opens at index 1. Code that assumes index 0 is text
	// renders the model's private reasoning to the user.
	frames := []string{
		b6Ping, b6MessageStart,
		b7ThinkingStart,
		b7ThinkingDelta,
		anthThinkingDelta(0, " me multiply that."),
		b7SignatureDelta,
		b7BlockStop0,
		b7TextStart1,
		b7TextDeltaFirst,
		b7TextDeltaSecond,
		anthBlockStop(1),
		anthMessageDelta("end_turn", 291, 63, 0, 0),
		b6MessageStop, b6PingWithCost,
	}

	res, rec, err := anthParse(t, frames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := "Let me multiply that."; res.Thinking != want {
		t.Errorf("Thinking = %q, want %q", res.Thinking, want)
	}
	if want := "To calculate 17 ×"; res.Text != want {
		t.Errorf("Text = %q, want %q — thinking must never reach the visible text", res.Text, want)
	}

	if got := rec.count(KindReasoningDelta); got != 2 {
		t.Errorf("KindReasoningDelta count = %d, want 2", got)
	}
	if got := rec.count(KindTextDelta); got != 2 {
		t.Errorf("KindTextDelta count = %d, want 2", got)
	}

	// signature_delta is emitted by this gateway and always carries "" (§B7).
	// It must produce no event of any kind: it is neither text nor a notice,
	// and there is no signature to round-trip.
	for _, txt := range append(rec.textsOf(KindTextDelta), rec.textsOf(KindReasoningDelta)...) {
		if txt == "" {
			t.Error("an empty delta reached the bus; signature_delta must be ignored, not forwarded")
		}
	}
	if got := rec.count(KindNotice); got != 0 {
		t.Errorf("got %d notices, want 0 — signature_delta is expected, not unknown", got)
	}

	// The first token is the first THINKING token, not the first visible
	// character. On a reasoning model that is the honest measurement: it is
	// what the model produced first.
	if got := rec.count(KindFirstToken); got != 1 {
		t.Fatalf("KindFirstToken count = %d, want 1", got)
	}
	if rec.kinds()[0] != KindFirstToken || rec.kinds()[1] != KindReasoningDelta {
		t.Errorf("first two events were %v, want first_token then reasoning_delta", rec.kinds()[:2])
	}
}

// ---------------------------------------------------------------------------
// The `</think>` leak. §B6 deviation 4.
// ---------------------------------------------------------------------------

// THE DECISION UNDER TEST: residue is dropped from user-visible text and
// reported as a notice. Not rendered (it is not the model's output), not
// silently swallowed (the trace has to keep the evidence that the gateway is
// leaking its own harness markup).
func TestAnthropicThinkTagLeak(t *testing.T) {
	cases := []struct {
		name        string
		deltas      []string
		wantText    string
		wantNotices int
	}{
		{
			// VERBATIM §B6 / §A3b: an entire text content block whose whole
			// content is the leaked closing tag.
			name:        "the observed leak is dropped and reported",
			deltas:      []string{b6LeakText},
			wantText:    "",
			wantNotices: 1,
		},
		{
			name:        "the leak in front of real text does not eat the text",
			deltas:      []string{b6LeakText, "The answer is 391."},
			wantText:    "The answer is 391.",
			wantNotices: 1,
		},
		{
			// The opening tag has not been observed leaking, but it is the same
			// failure with the same fix.
			name:        "a bare opening tag is residue too",
			deltas:      []string{"<think>", "hello"},
			wantText:    "hello",
			wantNotices: 1,
		},
		{
			// THE FALSE POSITIVE THIS RULE EXISTS TO AVOID. A model explaining
			// think-tags — a completely reasonable thing to ask a coding agent —
			// must come through untouched. Quietly corrupting genuine output to
			// tidy up vendor garbage is the worse of the two failures, so the
			// rule is "the whole delta is the tag", not "the delta contains it".
			name:        "a tag inside a sentence is the model talking, not residue",
			deltas:      []string{"Close the block with </think> and continue."},
			wantText:    "Close the block with </think> and continue.",
			wantNotices: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := []string{b6Ping, b6MessageStart, b7TextStart1}
			for _, d := range tc.deltas {
				frames = append(frames, anthTextDelta(1, d))
			}
			frames = append(frames, anthBlockStop(1), anthMessageDelta("end_turn", 291, 63, 0, 0), b6MessageStop, b6PingWithCost)

			res, rec, err := anthParse(t, frames)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", res.Text, tc.wantText)
			}
			if got := rec.count(KindNotice); got != tc.wantNotices {
				t.Errorf("notice count = %d, want %d (notices: %q)", got, tc.wantNotices, rec.textsOf(KindNotice))
			}
			// Dropped means dropped: no renderer may ever see the tag.
			for _, txt := range rec.textsOf(KindTextDelta) {
				if strings.TrimSpace(txt) == "</think>" || strings.TrimSpace(txt) == "<think>" {
					t.Errorf("residue %q was forwarded as visible text", txt)
				}
			}
			if tc.wantNotices > 0 {
				n, _ := rec.first(KindNotice)
				if !strings.Contains(n.Text, "harness residue") {
					t.Errorf("notice text = %q, want it to say what was dropped and why", n.Text)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Event sequence.
// ---------------------------------------------------------------------------

func TestAnthropicEventSequence(t *testing.T) {
	// One stream with every payload type in it, so the assertion below is the
	// complete contract this adapter owes a renderer: same kinds, same order,
	// same meanings as the OpenAI adapter, from a wire that shares none of its
	// vocabulary.
	frames := []string{
		b6Ping,         // not a token
		b6MessageStart, // not a token, and its usage is a lie
		b7ThinkingStart,
		b7ThinkingDelta,
		b7SignatureDelta, // always empty; produces nothing
		b7BlockStop0,
		b7TextStart1,
		b7TextDeltaFirst,
		anthBlockStop(1),
		anthToolStart(2, "toolu_x", "bash"),
		anthArgsDelta(2, ``), // the empty first fragment produces nothing
		anthArgsDelta(2, `{"command": "ls"}`),
		anthBlockStop(2),
		b6MessageDelta,
		b6MessageStop,
		b6PingWithCost, // after message_stop, and still not a token
	}

	_, rec, err := anthParse(t, frames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []Kind{
		KindFirstToken,     // fired by the thinking delta, not by the ping
		KindReasoningDelta, //
		KindTextDelta,      //
		KindToolCallStart,  // id + name, once
		KindToolArgsDelta,  // one fragment, the empty one skipped
		KindUsage,          // message_delta
		KindResponseEnd,    // last
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("event kinds =\n  %v\nwant\n  %v", got, want)
	}
	if got := rec.count(KindFirstToken); got != 1 {
		t.Errorf("KindFirstToken count = %d, want exactly 1", got)
	}
	if ft, _ := rec.first(KindFirstToken); ft.Millis < 0 {
		t.Errorf("KindFirstToken.Millis = %d, want a TTFT measurement", ft.Millis)
	}
}

// ---------------------------------------------------------------------------
// Framing edge cases and survivable damage.
// ---------------------------------------------------------------------------

func TestAnthropicStreamTolerance(t *testing.T) {
	t.Run("pings anywhere, and no [DONE] anywhere", func(t *testing.T) {
		frames := []string{
			b6Ping, b6Ping, // before message_start
			b6MessageStart,
			b7TextStart1,
			b6Ping, // an ordinary mid-stream keepalive
			anthTextDelta(1, "hello"),
			anthBlockStop(1),
			anthMessageDelta("end_turn", 291, 63, 0, 0),
			b6MessageStop,
			b6PingWithCost, b6Ping, // after message_stop
		}
		res, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text != "hello" {
			t.Errorf("Text = %q, want %q", res.Text, "hello")
		}
		if got := rec.count(KindFirstToken); got != 1 {
			t.Errorf("KindFirstToken count = %d, want 1 — five pings are not five tokens", got)
		}
		if got := rec.count(KindNotice); got != 0 {
			t.Errorf("pings produced %d notices, want 0: %q", got, rec.textsOf(KindNotice))
		}
		// The trailing ping arrives AFTER message_stop, so a parser that
		// returned there would never see `cost`, and would leave bytes in the
		// socket that stop the connection going back in the keep-alive pool.
		if got := rec.count(KindUsage); got != 1 {
			t.Errorf("KindUsage count = %d, want 1", got)
		}
	})

	t.Run("a non-zero cost on the trailing ping is reported", func(t *testing.T) {
		// §C10 only ever saw the string "0". A real figure would be the first
		// cost signal this endpoint has emitted, so it goes in the trace.
		frames := []string{
			b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "hi"), anthBlockStop(1),
			anthMessageDelta("end_turn", 1, 1, 0, 0), b6MessageStop,
			`{"type":"ping","cost":"0.0042"}`,
		}
		_, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		n, ok := rec.first(KindNotice)
		if !ok || !strings.Contains(n.Text, "0.0042") {
			t.Errorf("notices = %q, want one reporting the cost", rec.textsOf(KindNotice))
		}
	})

	t.Run("a numeric cost does not break the frame", func(t *testing.T) {
		// The `cost` key is typed as json.RawMessage precisely so that a change
		// of JSON type cannot take the whole frame down with it.
		frames := []string{
			b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "hi"), anthBlockStop(1),
			anthMessageDelta("end_turn", 1, 1, 0, 0), b6MessageStop,
			`{"type":"ping","cost":0}`,
		}
		_, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, n := range rec.textsOf(KindNotice) {
			if strings.Contains(n, "not JSON") {
				t.Errorf("a numeric cost broke the frame decode: %q", n)
			}
		}
	})

	t.Run("a malformed frame is survivable, not fatal", func(t *testing.T) {
		frames := []string{
			b6Ping, b6MessageStart,
			b6ToolStart0,
			anthArgsDelta(0, `{"command": "ls -la /srv/app"}`),
			`{"type":"content_block_delta",` + `truncated`,
			anthBlockStop(0),
			b6MessageDelta, b6MessageStop,
		}
		res, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("one bad frame must not fail the turn: %v", err)
		}
		if len(res.Calls) != 1 || res.Calls[0].Args != b6WantArgs {
			t.Errorf("the complete tool call was lost to a neighbouring bad frame: %+v", res.Calls)
		}
		n, ok := rec.first(KindNotice)
		if !ok || !strings.Contains(n.Text, "not JSON") {
			t.Errorf("notices = %q, want one naming the unparseable frame", rec.textsOf(KindNotice))
		}
	})

	t.Run("an unknown event type is noticed, not fatal", func(t *testing.T) {
		frames := []string{
			b6Ping, b6MessageStart,
			`{"type":"message_thermal_throttle","index":0}`,
			b7TextStart1, anthTextDelta(1, "hi"), anthBlockStop(1),
			anthMessageDelta("end_turn", 1, 1, 0, 0), b6MessageStop,
		}
		res, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text != "hi" {
			t.Errorf("Text = %q, want %q", res.Text, "hi")
		}
		n, ok := rec.first(KindNotice)
		if !ok || !strings.Contains(n.Text, "message_thermal_throttle") {
			t.Errorf("notices = %q, want one naming the unknown event", rec.textsOf(KindNotice))
		}
	})

	t.Run("an unknown stop reason is not 'probably fine'", func(t *testing.T) {
		frames := []string{
			b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "..."), anthBlockStop(1),
			anthMessageDelta("model_context_window_exceeded", 291, 63, 0, 0), b6MessageStop,
		}
		res, _, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RawStop != "model_context_window_exceeded" {
			t.Errorf("RawStop = %q, want the literal string preserved for the trace", res.RawStop)
		}
		if res.Stop != StopUnknown {
			t.Errorf("Stop = %q, want %q — mapping unknown reasons to end_turn is how a refusal becomes a shrug", res.Stop, StopUnknown)
		}
	})

	t.Run("a nil bus is tolerated", func(t *testing.T) {
		res, err := anthProvider().ParseStream(anthSSE(b6FullStream()...), nil, 0, time.Now())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(res.Calls) != 2 {
			t.Errorf("got %d calls with a nil bus, want 2 — the parser must work as a pure function", len(res.Calls))
		}
	})
}

// ---------------------------------------------------------------------------
// Mid-stream failure.
// ---------------------------------------------------------------------------

func TestAnthropicMidStreamFailureKeepsPartialAndSkipsResponseEnd(t *testing.T) {
	t.Run("the connection dies", func(t *testing.T) {
		// Everything up to a complete tool call arrives, then the socket
		// breaks. The caller needs the partial result to tell "died after a
		// complete tool call" apart from "produced nothing".
		var b strings.Builder
		for _, f := range []string{b6Ping, b6MessageStart, b6ToolStart0, anthArgsDelta(0, b6WantArgs), anthBlockStop(0)} {
			fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", anthEventName(f), f)
		}
		body := io.MultiReader(strings.NewReader(b.String()), iotest.ErrReader(errors.New("connection reset by peer")))

		rec := &anthRecorder{}
		res, err := anthProvider().ParseStream(body, NewBus(rec), anthTestTurn, time.Now())
		if err == nil {
			t.Fatal("a broken connection must be reported as an error")
		}
		if res == nil || len(res.Calls) != 1 || res.Calls[0].Args != b6WantArgs {
			t.Fatalf("the partial result was lost: %+v", res)
		}
		if rec.count(KindResponseEnd) != 0 {
			t.Error("emitted KindResponseEnd for a response that never ended — the trace must never record a clean ending that did not happen")
		}
		if res.RawStop != "" || res.Stop != StopUnknown {
			t.Errorf("RawStop/Stop = %q/%q, want empty and %q", res.RawStop, res.Stop, StopUnknown)
		}
	})

	t.Run("an error event mid-stream", func(t *testing.T) {
		// CONSTRUCTED: §D11's errors all arrive as an HTTP status before the
		// stream opens, so this shape has not been observed on this gateway.
		// The spec streams overloaded_error mid-body, and a stream that dies
		// halfway must not be recorded as one that finished.
		frames := []string{
			b6Ping, b6MessageStart, b6ToolStart0,
			anthArgsDelta(0, b6WantArgs), anthBlockStop(0),
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		}
		res, rec, err := anthParse(t, frames)
		if err == nil {
			t.Fatal("an error event must be reported as an error")
		}
		if !strings.Contains(err.Error(), "overloaded_error") {
			t.Errorf("error = %v, want it to carry the provider's own type", err)
		}
		if len(res.Calls) != 1 {
			t.Errorf("the partial result was lost: %+v", res)
		}
		if rec.count(KindResponseEnd) != 0 {
			t.Error("emitted KindResponseEnd after a stream error")
		}
	})
}

// ---------------------------------------------------------------------------
// BuildRequest.
// ---------------------------------------------------------------------------

// anthWireBody mirrors what BuildRequest is supposed to have produced. It is
// written out separately from the structs in anthropic.go on purpose: a test
// that decodes with the same type it encoded with cannot catch a wrong json
// tag, because both halves share the mistake.
type anthWireBody struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	System    string `json:"system"`
	Stream    bool   `json:"stream"`
	Messages  []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   string          `json:"content"`
		} `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema map[string]any  `json:"input_schema"`
		Function    json.RawMessage `json:"function"` // must never appear
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"tools"`
}

func anthDecodeBody(t *testing.T, body []byte) anthWireBody {
	t.Helper()
	var got anthWireBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the request body is not valid JSON: %v\n%s", err, body)
	}
	return got
}

func anthBashTool() Tool {
	return Tool{
		Name:        "bash",
		Description: "Execute a bash command.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	}
}

func TestAnthropicBuildRequestEnvelope(t *testing.T) {
	p := anthProvider()
	req, body, err := p.BuildRequest("you are a shell", []Msg{TextMsg(RoleUser, "hello")}, []Tool{anthBashTool()}, 700)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	if p.Protocol() != "anthropic" {
		t.Errorf("Protocol() = %q, want %q", p.Protocol(), "anthropic")
	}
	if p.Model() != "qwen3.7-plus" {
		t.Errorf("Model() = %q", p.Model())
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if want := "https://opencode.ai/zen/go/v1/messages"; req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL.String(), want)
	}

	// x-api-key, NOT Authorization: Bearer. Sending the other protocol's header
	// here produces "Missing API key.", which reads like a config problem.
	for _, h := range []struct{ key, want string }{
		{"x-api-key", "sk-test"},
		{"anthropic-version", "2023-06-01"},
		{"content-type", "application/json"},
		{"accept", "text/event-stream"},
	} {
		if got := req.Header.Get(h.key); got != h.want {
			t.Errorf("header %s = %q, want %q", h.key, got, h.want)
		}
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("an Authorization header leaked in from the other protocol")
	}

	// The returned bytes must be the bytes on the wire: the caller emits them
	// as KindRequest, and a request inspector showing something other than what
	// was sent is worse than no inspector.
	sent, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reading the request body: %v", err)
	}
	if string(sent) != string(body) {
		t.Errorf("the returned body and the request body differ:\n returned %s\n sent     %s", body, sent)
	}

	got := anthDecodeBody(t, body)
	if !got.Stream {
		t.Error(`"stream": true is missing; this adapter only knows how to read an SSE body`)
	}
	if got.Model != "qwen3.7-plus" {
		t.Errorf("model = %q", got.Model)
	}
	if got.MaxTokens != 700 {
		t.Errorf("max_tokens = %d, want 700", got.MaxTokens)
	}

	// THE ASYMMETRY. The system prompt is a top-level field here; the OpenAI
	// adapter makes it messages[0]. Neither shape can be the neutral one.
	if got.System != "you are a shell" {
		t.Errorf("system = %q, want it at the top level", got.System)
	}
	for _, m := range got.Messages {
		if m.Role == "system" {
			t.Error("the system prompt was sent as a message; on this protocol it is a top-level field")
		}
	}

	// Tools are flat: {name, description, input_schema}. No `function` nesting,
	// no `parameters`.
	if len(got.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(got.Tools))
	}
	tool := got.Tools[0]
	if tool.Name != "bash" || tool.Description != "Execute a bash command." {
		t.Errorf("tool = %+v", tool)
	}
	if tool.InputSchema == nil {
		t.Error("input_schema is missing — this protocol does not call it `parameters`")
	}
	if tool.InputSchema["type"] != "object" {
		t.Errorf("input_schema.type = %v, want object", tool.InputSchema["type"])
	}
	if len(tool.Function) != 0 || len(tool.Parameters) != 0 {
		t.Errorf("the OpenAI tool envelope leaked in: function=%s parameters=%s", tool.Function, tool.Parameters)
	}

	// max_tokens is mandatory here, and §D11 shows what omitting it costs: a
	// 400 whose body is `{"model":"qwen3.7-plus"}` with no error envelope at
	// all. A non-positive budget gets a default rather than a mystery.
	_, defaulted, err := p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 0)
	if err != nil {
		t.Fatalf("BuildRequest with no budget: %v", err)
	}
	if d := anthDecodeBody(t, defaulted); d.MaxTokens != anthropicDefaultMaxTokens {
		t.Errorf("max_tokens = %d with a zero budget, want the default %d", d.MaxTokens, anthropicDefaultMaxTokens)
	}
	if d := anthDecodeBody(t, defaulted); d.System != "" || len(d.Tools) != 0 {
		t.Error("an empty system prompt or tool list must be omitted, not sent empty: a different prefix is a cache miss")
	}
}

// TestAnthropicBuildRequestCollapsesToolResults is the one this file exists
// for. Three tool results become ONE user message with three tool_result
// blocks, however the caller happened to arrange them. The OpenAI adapter emits
// one message per result; getting this backwards is the single most likely bug
// in anthropic.go.
func TestAnthropicBuildRequestCollapsesToolResults(t *testing.T) {
	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "Running three checks."},
		{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: `{"command":"ls /a"}`},
		{Kind: BlockToolCall, ID: "toolu_2", Name: "bash", Args: `{"command":"ls /b"}`},
		{Kind: BlockToolCall, ID: "toolu_3", Name: "bash", Args: `{"command":"ls /c"}`},
	}}

	cases := []struct {
		name string
		msgs []Msg
	}{
		{
			// How a loop that appends one message per completed tool naturally
			// builds it — the arrangement the OpenAI protocol wants.
			name: "one neutral message per result",
			msgs: []Msg{
				TextMsg(RoleUser, "check three paths"),
				assistant,
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", "a\n")}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_2", "b\n")}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_3", "c\n")}},
			},
		},
		{
			name: "one neutral message holding all three",
			msgs: []Msg{
				TextMsg(RoleUser, "check three paths"),
				assistant,
				{Role: RoleUser, Blocks: []Block{
					ToolResultBlock("toolu_1", "a\n"),
					ToolResultBlock("toolu_2", "b\n"),
					ToolResultBlock("toolu_3", "c\n"),
				}},
			},
		},
		{
			name: "two then one",
			msgs: []Msg{
				TextMsg(RoleUser, "check three paths"),
				assistant,
				{Role: RoleUser, Blocks: []Block{
					ToolResultBlock("toolu_1", "a\n"),
					ToolResultBlock("toolu_2", "b\n"),
				}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_3", "c\n")}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, body, err := anthProvider().BuildRequest("sys", tc.msgs, []Tool{anthBashTool()}, 700)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			got := anthDecodeBody(t, body)

			// user(text) → assistant(text + 3 tool_use) → user(3 tool_result).
			// Four or six messages here means the results did not collapse.
			if len(got.Messages) != 3 {
				var shape []string
				for _, m := range got.Messages {
					shape = append(shape, fmt.Sprintf("%s(%d blocks)", m.Role, len(m.Content)))
				}
				t.Fatalf("got %d messages %v, want 3 — consecutive tool results must collapse into ONE user message", len(got.Messages), shape)
			}

			results := got.Messages[2]
			if results.Role != "user" {
				t.Errorf("tool results were sent as role %q, want user — this protocol has no tool role", results.Role)
			}
			if len(results.Content) != 3 {
				t.Fatalf("got %d blocks in the results message, want 3", len(results.Content))
			}
			for i, want := range []struct{ id, content string }{
				{"toolu_1", "a\n"}, {"toolu_2", "b\n"}, {"toolu_3", "c\n"},
			} {
				b := results.Content[i]
				if b.Type != "tool_result" {
					t.Errorf("block %d type = %q, want tool_result", i, b.Type)
				}
				if b.ToolUseID != want.id {
					t.Errorf("block %d tool_use_id = %q, want %q (order must match the calls)", i, b.ToolUseID, want.id)
				}
				if b.Content != want.content {
					t.Errorf("block %d content = %q, want %q", i, b.Content, want.content)
				}
				if b.ID != "" {
					t.Errorf("block %d carries an `id` field; tool results are addressed by tool_use_id", i)
				}
			}

			// The assistant turn replays as one content array: text first, then
			// the three tool_use blocks in order.
			asst := got.Messages[1]
			if asst.Role != "assistant" || len(asst.Content) != 4 {
				t.Fatalf("assistant message = %s with %d blocks, want assistant with 4", asst.Role, len(asst.Content))
			}
			if asst.Content[0].Type != "text" || asst.Content[0].Text != "Running three checks." {
				t.Errorf("assistant block 0 = %+v, want the text block", asst.Content[0])
			}
			for i, id := range []string{"toolu_1", "toolu_2", "toolu_3"} {
				b := asst.Content[i+1]
				if b.Type != "tool_use" || b.ID != id || b.Name != "bash" {
					t.Errorf("assistant block %d = %+v, want tool_use %s", i+1, b, id)
				}
				if len(b.Input) == 0 {
					t.Errorf("assistant block %d has no `input`; the field is required even when empty", i+1)
				}
			}
		})
	}
}

func TestAnthropicBuildRequestMessageShapes(t *testing.T) {
	t.Run("a following user turn merges into the tool_result message", func(t *testing.T) {
		// Two user messages in a row is a shape this protocol dislikes, and
		// tool_result blocks are required to come first in the message that
		// carries them.
		msgs := []Msg{
			TextMsg(RoleUser, "go"),
			{Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: `{"command":"ls"}`}}},
			{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", "out")}},
			TextMsg(RoleUser, "now explain it"),
		}
		_, body, err := anthProvider().BuildRequest("sys", msgs, nil, 700)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		got := anthDecodeBody(t, body)
		if len(got.Messages) != 3 {
			t.Fatalf("got %d messages, want 3 (user, assistant, user)", len(got.Messages))
		}
		last := got.Messages[2]
		if len(last.Content) != 2 || last.Content[0].Type != "tool_result" || last.Content[1].Type != "text" {
			t.Errorf("last message content = %+v, want tool_result then text", last.Content)
		}
	})

	t.Run("thinking blocks are dropped, and a thinking-only turn vanishes", func(t *testing.T) {
		// §B7/§A3b: the signature is ALWAYS empty on this endpoint, so a
		// replayed thinking block cannot validate. Dropping it loses reasoning
		// from the next turn's context; sending it unsigned risks a 400 that
		// kills the session. The trace keeps every thinking token either way.
		msgs := []Msg{
			TextMsg(RoleUser, "go"),
			{Role: RoleAssistant, Blocks: []Block{{Kind: BlockThinking, Text: "long private plan"}}},
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockThinking, Text: "more planning"},
				{Kind: BlockText, Text: "Here is the answer."},
			}},
		}
		_, body, err := anthProvider().BuildRequest("sys", msgs, nil, 700)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		if strings.Contains(string(body), "thinking") {
			t.Errorf("a thinking block reached the wire: %s", body)
		}
		got := anthDecodeBody(t, body)
		if len(got.Messages) != 2 {
			t.Fatalf("got %d messages, want 2 — a message that renders to nothing must be skipped, not sent as content:[]", len(got.Messages))
		}
		if len(got.Messages[1].Content) != 1 || got.Messages[1].Content[0].Text != "Here is the answer." {
			t.Errorf("assistant content = %+v", got.Messages[1].Content)
		}
	})

	t.Run("a system message in msgs is a caller bug, reported loudly", func(t *testing.T) {
		// Re-labelling it "user" would send a subtly different prompt and
		// produce a subtly worse agent — the hardest class of bug to notice.
		_, _, err := anthProvider().BuildRequest("sys", []Msg{TextMsg(RoleSystem, "you are a shell"), TextMsg(RoleUser, "hi")}, nil, 700)
		if err == nil {
			t.Fatal("want an error for a system message in msgs")
		}
		if !strings.Contains(err.Error(), "top-level") {
			t.Errorf("error = %v, want it to say where the system prompt belongs", err)
		}
	})

	t.Run("no messages at all is refused before the network", func(t *testing.T) {
		// §D11: the gateway's answer to this is a 400 whose body is
		// `{"model":"qwen3.7-plus"}` — no type, no error, nothing to log.
		if _, _, err := anthProvider().BuildRequest("sys", nil, nil, 700); err == nil {
			t.Fatal("want an error for an empty conversation")
		}
	})

	t.Run("a tool with no schema still gets an input_schema", func(t *testing.T) {
		_, body, err := anthProvider().BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, []Tool{{Name: "now", Description: "the time"}}, 700)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		got := anthDecodeBody(t, body)
		if len(got.Tools) != 1 || got.Tools[0].InputSchema["type"] != "object" {
			t.Errorf("tools = %+v, want an object input_schema even with no properties", got.Tools)
		}
	})
}

func TestAnthropicBuildRequestArgumentBytes(t *testing.T) {
	// The reason Block.Args is a string and Input is a json.RawMessage: these
	// bytes must reach the wire unchanged. Round-tripping them through
	// map[string]any would sort the keys — Go sorts map keys on marshal, the
	// model emitted them in its own order — and a different byte sequence is a
	// different prompt prefix, which is a cache miss on every replayed turn.
	cases := []struct {
		name string
		args string
		want string // the exact substring the body must contain
	}{
		{
			// Keys deliberately NOT in alphabetical order, and a command full
			// of the characters encoding/json would escape by default:
			// `>` and `&` become u003e/u0026 unless HTML escaping is off, which
			// would corrupt every redirect in the request inspector.
			name: "key order and shell metacharacters survive byte for byte",
			args: `{"z_last":"first","command":"grep -rn 'TODO' . 2>&1 > /tmp/o"}`,
			want: `"input":{"z_last":"first","command":"grep -rn 'TODO' . 2>&1 > /tmp/o"}`,
		},
		{
			// The ONE thing encoding/json normalises when splicing a
			// RawMessage: insignificant whitespace between tokens. Key order —
			// the part that actually breaks caching — is untouched. Recorded
			// here so the behaviour is a decision and not a surprise.
			name: "insignificant whitespace is compacted, order is not",
			args: `{"command": "ls -la /srv/app"}`,
			want: `"input":{"command":"ls -la /srv/app"}`,
		},
		{
			// A model calling a zero-argument tool. `input` is required.
			name: "empty args become an empty object",
			args: ``,
			want: `"input":{}`,
		},
		{
			// §A3c: a tool call truncated at max_tokens comes back with
			// `input` replaced by `{"raw_arguments":"<invalid JSON>"}` while
			// stop_reason still says "tool_use". If that ever round-trips into
			// a request, splicing it raw would produce a malformed body — and
			// §D11 shows this gateway answering a malformed body with a 500,
			// which a retry policy keyed on 5xx retries forever. So invalid
			// bytes are wrapped in the gateway's own truncation shape: the body
			// stays valid and the evidence survives verbatim inside it.
			name: "invalid arguments are wrapped, not spliced",
			args: `{"command": "find`,
			want: `"input":{"raw_arguments":"{\"command\": \"find"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []Msg{
				TextMsg(RoleUser, "go"),
				{Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: tc.args}}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", "done")}},
			}
			_, body, err := anthProvider().BuildRequest("sys", msgs, []Tool{anthBashTool()}, 700)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body does not contain\n  %s\ngot\n  %s", tc.want, body)
			}
			if !json.Valid(body) {
				t.Errorf("the body is not valid JSON — this gateway answers a malformed body with a 500: %s", body)
			}
		})
	}
}

func TestAnthropicBuildRequestKeepsToolResultTextIntact(t *testing.T) {
	// Command output is the least sanitised thing in the whole loop: it is
	// whatever the shell printed. It must arrive as the model's `content`
	// byte for byte, including the angle brackets and ampersands that
	// encoding/json would otherwise escape.
	out := "total 4\ndrwxr-xr-x 2 root root <dir> & more\nexit 0\n"
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: `{"command":"ls"}`}}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", out)}},
	}
	_, body, err := anthProvider().BuildRequest("sys", msgs, nil, 700)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if strings.Contains(string(body), "u003c") {
		t.Errorf("tool output was HTML-escaped on the way out: %s", body)
	}
	got := anthDecodeBody(t, body)
	last := got.Messages[len(got.Messages)-1]
	if last.Content[0].Content != out {
		t.Errorf("tool_result content = %q, want %q", last.Content[0].Content, out)
	}
}

func TestAnthropicBaseURLTrailingSlash(t *testing.T) {
	// One character in a .env file; §D11 shows this gateway answering the
	// resulting double slash with an opaque 500.
	p := newAnthropicProvider("https://opencode.ai/zen/go/v1/", "k", "m")
	req, _, err := p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 10)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if want := "https://opencode.ai/zen/go/v1/messages"; req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL.String(), want)
	}
}
