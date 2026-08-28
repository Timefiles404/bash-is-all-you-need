// Stage 03 — the Anthropic protocol adapter.
//
// The other half of Babel. This file and openai.go implement the same
// interface, are driven by the same loop, and agree about almost nothing:
//
//	                 OpenAI                    Anthropic (this file)
//	system prompt    messages[0]               a top-level `system` field
//	tool results     one role:"tool" message   tool_result blocks in ONE user message
//	tool arguments   a JSON *string*           an `input` JSON *object*
//	tool schema      nested under `function`   flat, `input_schema`
//	stop reason      finish_reason             stop_reason
//	cached tokens    inside prompt_tokens      *additional* to input_tokens
//	stream end       a `[DONE]` sentinel       the connection closing
//
// None of those seven vocabularies appears anywhere outside this file and
// openai.go. That is the architectural claim of the stage: the vendor's words
// stop at the adapter boundary.
//
// Every deviation handled below is recorded with evidence in
// docs/wire-notes.md. Where the observed bytes and the published spec disagree
// — and on this endpoint they disagree in half a dozen separate places — the
// observation wins, because the observation is what will be on the wire at 3am.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// anthropicVersion is a spec-required header that this gateway does not
// actually require (§D11: the call succeeds without it). Sent anyway. Omitting
// it costs nothing today and costs an afternoon the day someone points this
// agent at the real api.anthropic.com and gets an error about a header that is
// not in the code to grep for.
const anthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens is used when the caller passes a non-positive
// budget. `max_tokens` is mandatory on this protocol, and §D11 records exactly
// what omitting it buys: HTTP 400 with the body `{"model":"qwen3.7-plus"}` — no
// `type`, no `error`, no message. Code that logs `resp.Error.Message` logs an
// empty string. Defaulting here is not politeness, it is the difference between
// a diagnosable failure and a silent one.
const anthropicDefaultMaxTokens = 4096

// ---------------------------------------------------------------------------
// The provider
// ---------------------------------------------------------------------------

// anthropicProvider speaks the Messages protocol.
//
// It deliberately holds no *http.Client. BuildRequest returns a request and
// ParseStream reads an io.Reader, so this type performs no I/O at all.
// Transport policy — timeouts, proxies, redirects, connection pooling — is
// identical for both protocols and belongs to the caller; a client per adapter
// is two places to forget to set a timeout. The side effect is that both
// adapters are pure enough to drive from a strings.Reader, which is why
// anthropic_test.go needs no network and no API key.
type anthropicProvider struct {
	baseURL string
	apiKey  string
	model   string

	// cacheBreakpoints turns cache_control placement on. It exists as a switch
	// only so the chapter can measure the difference; there is no reason to run
	// an agent with it off.
	cacheBreakpoints bool
}

func newAnthropicProvider(baseURL, apiKey, model string) *anthropicProvider {
	return &anthropicProvider{
		// A trailing slash in AGENT_BASE_URL would render "{base}//messages".
		// Some gateways 404 that; this one answers 500 with the generic
		// "Internal server error" envelope (§D11) — an hour of debugging caused
		// by one character in a .env file. Trim it once, here, instead of
		// asking every config to be careful.
		baseURL:          strings.TrimRight(baseURL, "/"),
		apiKey:           apiKey,
		model:            model,
		cacheBreakpoints: true,
	}
}

// withCacheBreakpoints toggles prefix pinning. Used by --no-cache to produce
// the control arm of the experiment in docs/04-the-cache.md.
func (p *anthropicProvider) withCacheBreakpoints(on bool) *anthropicProvider {
	p.cacheBreakpoints = on
	return p
}

func (p *anthropicProvider) Protocol() string { return "anthropic" }
func (p *anthropicProvider) Model() string    { return p.model }

// ---------------------------------------------------------------------------
// The request wire format
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`

	// System is a TOP-LEVEL FIELD, not a message. This is the most visible
	// difference between the two protocols and the reason Provider.BuildRequest
	// takes the system prompt as its own parameter: the neutral form cannot
	// pick either shape without smuggling one vendor's design into the core.
	//
	// An ARRAY of text blocks, not the plain string stage 03 used. That change
	// is this chapter: a block can carry `cache_control`, a string cannot.
	// §C8 measured the upgrade turning a run-to-run-variable 64-token-block hit
	// into a stable exact-prefix hit of 9,775 tokens.
	System []anthropicContent `json:"system,omitempty"`

	Messages []anthropicMessage `json:"messages"`

	// Tools carry no `function` wrapper on this protocol. Omitted entirely when
	// empty rather than sent as `[]`, because a present-but-empty tools array is
	// a different prompt prefix from an absent one, and a different prefix is a
	// cache miss.
	Tools []anthropicTool `json:"tools,omitempty"`

	Stream bool `json:"stream"`
}

type anthropicMessage struct {
	Role string `json:"role"`

	// Content is always an array of blocks, never the string shorthand the spec
	// also allows. One shape is one code path: the shorthand needs a custom
	// marshaller and has to become an array the moment a message carries a tool
	// result or a tool call anyway.
	Content []anthropicContent `json:"content"`
}

// anthropicContent is one content block. One struct with an omitempty field per
// block type, rather than an interface — for the same reason Event is one flat
// struct (events.go): the JSON stays readable in the request inspector, and
// adding a block type is one field instead of a type plus a marshaller.
type anthropicContent struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`

	// Input is RAW BYTES, spliced through untouched.
	//
	// The neutral Block.Args is a raw JSON string (provider.go says why); this
	// protocol wants an object. Decoding into map[string]any and re-encoding
	// would produce an equivalent object with the keys in a different order —
	// Go sorts map keys, the model emitted them in its own order — and a
	// different byte sequence is a different prompt prefix, which is a cache
	// miss on every replayed turn. json.RawMessage is the only field type that
	// turns a string into an object by doing nothing at all.
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`

	// CacheControl marks this block as the end of a cacheable prefix.
	//
	// A pointer with omitempty so an unmarked block serialises to exactly the
	// bytes it did before stage 04. That matters more than it looks: if adding
	// the feature changed the bytes of every *unmarked* block, turning caching
	// on would invalidate the very prefix it was meant to preserve.
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl pins a prefix. `ephemeral` is the only type; the 5
// minute TTL is the default and shows up in the response as the nested
// `cache_creation.ephemeral_5m_input_tokens` counter (wire-notes §C8).
type anthropicCacheControl struct {
	Type string `json:"type"`
}

func ephemeral() *anthropicCacheControl { return &anthropicCacheControl{Type: "ephemeral"} }

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// InputSchema, not `parameters`, and no `function` object around it. The
	// JSON Schema inside is identical; only the envelope differs.
	//
	// A map is safe here despite Go's randomised map iteration: encoding/json
	// sorts map keys when marshalling, so the rendered schema is byte-stable
	// from run to run. That matters more than it looks — the tools block sits
	// near the front of the prompt, inside the cached prefix, so an unstable
	// rendering would quietly cost a full cache write on every single request.
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicRawArguments mirrors the synthetic object this gateway produces
// itself when a tool call is truncated (§A3c: `input` is replaced by
// `{"raw_arguments":"<invalid JSON text>"}`). See anthropicToolInput.
type anthropicRawArguments struct {
	RawArguments string `json:"raw_arguments"`
}

// ---------------------------------------------------------------------------
// BuildRequest
// ---------------------------------------------------------------------------

// BuildRequest renders the neutral conversation onto this wire.
//
// It returns the marshalled body alongside the request because the caller emits
// it as KindRequest — the request inspector, and the only record in a trace of
// what the model actually saw. Reading it back off the request would mean
// draining req.Body and rebuilding it, so the bytes are handed over directly.
//
// This adapter does not emit the event itself: BuildRequest has no bus, and
// giving it one would make the "pure function, no I/O, no side effects"
// property that makes both adapters testable disappear for one convenience.
func (p *anthropicProvider) BuildRequest(ctx context.Context, system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	if len(msgs) == 0 {
		// The gateway's answer to this is a 400 with no error envelope (§D11).
		// Fail here, where the message can say something useful.
		return nil, nil, fmt.Errorf("anthropic: refusing to send a request with no messages")
	}
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	wireMsgs, err := anthropicMessages(msgs)
	if err != nil {
		return nil, nil, err
	}
	if p.cacheBreakpoints {
		markRollingBreakpoint(wireMsgs)
	}

	body, err := anthropicMarshal(anthropicRequest{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    p.systemBlocks(system),
		Messages:  wireMsgs,
		Tools:     anthropicTools(tools),
		Stream:    true,
	})
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	// Header.Set canonicalises these to X-Api-Key / Anthropic-Version on the
	// way out. HTTP field names are case-insensitive, so that is correct and
	// invisible; it is noted only because a reader comparing this against the
	// docs will see different capitalisation on the wire.
	//
	// Note the auth scheme: `x-api-key`, NOT `Authorization: Bearer`. Sending
	// the OpenAI header here produces the AuthError envelope from §D11 with
	// "Missing API key." — which reads like a config problem and is actually a
	// protocol confusion.
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")

	return req, body, nil
}

// anthropicMarshal encodes the body with HTML escaping switched OFF.
//
// This is not cosmetic. json.Marshal escapes `<`, `>` and `&` into their
// six-character Unicode escapes (u003c, u003e, u0026, each behind a backslash)
// — a rule that exists so a JSON document can be pasted inside an HTML script
// tag without closing it early.
// This agent's whole job is running shell commands, and a shell command is
// mostly those three characters: `2>&1`, `>/tmp/out`, `<<EOF`. Escaped, they
// are semantically identical and byte-wise different, which means:
//
//   - the request inspector shows the user `ls \u003e /tmp/out`, and
//   - the cached prefix changes the moment a redirect appears in a replayed
//     tool call, for no reason at all.
//
// Encoder.Encode also appends a newline that json.Marshal does not; it is
// trimmed so the KindRequest bytes are exactly the bytes POSTed.
//
// One thing this does NOT preserve: insignificant whitespace inside a spliced
// json.RawMessage. encoding/json compacts it, so a model's `{"command": "ls"}`
// is sent as `{"command":"ls"}`. Key ORDER — the part that actually breaks
// caching, and the part Go would destroy if the args were round-tripped through
// a map — survives exactly.
func anthropicMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// anthropicTools renders tool definitions. Flat: {name, description,
// input_schema}. The OpenAI adapter wraps the same three fields in
// {"type":"function","function":{...}}, which is the entire difference.
func anthropicTools(tools []Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema
		if schema == nil {
			// `input_schema` is required and must describe an object. A tool
			// that takes no arguments still needs the envelope, and sending
			// `null` here is a 400 on the real API.
			schema = map[string]any{"type": "object"}
		}
		out = append(out, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out
}

// anthropicMessages is the translation this file is most likely to get wrong,
// so it is the one with the most tests.
//
// The neutral form has no RoleTool (provider.go explains why). A tool result is
// a *block*, and this protocol answers N tool calls with N tool_result blocks
// inside ONE user message — not N messages, which is what the OpenAI adapter
// emits. Sending them as separate messages produces consecutive user turns and,
// on the real API, an error about tool_use blocks without matching results.
//
// So tool results accumulate into `pending` and are flushed as a single user
// message, and the run is closed by whatever comes next — including the end of
// the conversation, which is the common case: the last thing in `msgs` when the
// loop calls back into the model is exactly a run of fresh tool results.
//
// Two ordering rules are baked in, both required by the protocol:
//
//   - tool_result blocks come FIRST in their user message, before any text the
//     same turn carries;
//   - if the run is immediately followed by a user message of its own, the two
//     merge rather than producing two user turns in a row.
func anthropicMessages(msgs []Msg) ([]anthropicMessage, error) {
	var (
		out     []anthropicMessage
		pending []anthropicContent // tool_result blocks not yet flushed
	)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, anthropicMessage{Role: string(RoleUser), Content: pending})
		pending = nil
	}

	for _, m := range msgs {
		if m.Role == RoleSystem {
			// Loud, not lenient. The system prompt is a top-level field on this
			// protocol and Provider.BuildRequest passes it separately for
			// exactly that reason; a system Msg here means the caller built the
			// conversation the OpenAI way. Quietly re-labelling it "user" would
			// send a subtly different prompt and produce a subtly worse agent,
			// which is the hardest class of bug to ever notice.
			return nil, fmt.Errorf("anthropic: a system message in msgs — this protocol takes the system prompt as a top-level field, pass it as BuildRequest's system argument")
		}

		var own []anthropicContent

		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolResult:
				pending = append(pending, anthropicContent{
					Type:      "tool_result",
					ToolUseID: b.ID,
					// Content is a plain string. The spec also allows an array
					// of blocks here (for images, or for is_error), and this
					// agent has only ever one thing to say: what the shell
					// printed.
					Content: b.Text,
				})

			case BlockText:
				// Empty text blocks are rejected by the real API ("text content
				// blocks must be non-empty"), and an empty one carries nothing
				// anyway.
				if b.Text == "" {
					continue
				}
				own = append(own, anthropicContent{Type: "text", Text: b.Text})

			case BlockToolCall:
				own = append(own, anthropicContent{
					Type:  "tool_use",
					ID:    b.ID,
					Name:  b.Name,
					Input: anthropicToolInput(b.Args),
				})

			case BlockThinking:
				// DROPPED ON PURPOSE, and this is a decision, not an oversight.
				//
				// The spec says a thinking block must be replayed with the
				// `signature` the model returned, or the API rejects it. On this
				// endpoint the signature is ALWAYS the empty string — in
				// non-streaming responses (§A3b), in `signature_delta` frames
				// (§B7), everywhere. There is no signature to round-trip, so a
				// replayed thinking block is a block that cannot validate.
				//
				// Sending nothing loses the model's private reasoning from the
				// next turn's context, which is a real cost. Sending an unsigned
				// block risks a 400 that kills the session. The trace still has
				// every thinking token (KindReasoningDelta), so nothing is lost
				// from the record — only from the prompt.
			}
		}

		if len(own) == 0 {
			// A message that rendered to nothing must not become an empty
			// content array: `content: []` is a 400 on the real API, and an
			// assistant turn that was pure thinking renders to exactly that.
			continue
		}

		if len(pending) > 0 && m.Role == RoleUser {
			// Merge rather than flush: two user messages in a row is a shape
			// this protocol dislikes, and tool_result blocks are required to
			// come first in the message that carries them.
			merged := make([]anthropicContent, 0, len(pending)+len(own))
			merged = append(merged, pending...)
			merged = append(merged, own...)
			own = merged
			pending = nil
		} else {
			flush()
		}

		out = append(out, anthropicMessage{Role: string(m.Role), Content: own})
	}

	flush()

	if len(out) == 0 {
		return nil, fmt.Errorf("anthropic: every message rendered empty; nothing to send")
	}
	return out, nil
}

// anthropicToolInput converts the neutral raw-JSON-string Args into this
// protocol's `input` object, passing the bytes through untouched.
//
// The two edge cases are the interesting part:
//
//   - Empty Args. A model that calls a zero-argument tool sends "", and `input`
//     is required. `{}` is the honest rendering.
//
//   - Args that are not valid JSON. §A3c is the reason this can happen: a tool
//     call truncated at max_tokens comes back with `input` replaced by
//     `{"raw_arguments":"{\"command\": \"find"}` — genuinely invalid JSON,
//     unterminated mid-string — while `stop_reason` still cheerfully says
//     "tool_use". If that ever round-trips back into a request, splicing it raw
//     produces a malformed body, and §D11 records what this gateway does with a
//     malformed body: HTTP 500, "Internal server error". A client bug wearing a
//     server fault's clothes, which a retry policy keyed on 5xx will retry
//     forever.
//
//     So invalid bytes are wrapped in the gateway's own truncation shape. The
//     body stays valid, the evidence survives verbatim inside the string, and
//     the model sees a structure this endpoint already produces.
func anthropicToolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(trimmed)) {
		wrapped, err := json.Marshal(anthropicRawArguments{RawArguments: args})
		if err != nil {
			// Marshalling a struct of one string cannot fail; if it somehow
			// does, an empty object is still a valid request.
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(wrapped)
	}
	return json.RawMessage(trimmed)
}

// ---------------------------------------------------------------------------
// The stream wire format
// ---------------------------------------------------------------------------

// anthropicStreamEvent is one `data:` payload. Every event type on this
// protocol decodes into this one struct — the alternative is a two-pass decode
// (read `type`, then unmarshal again into the right struct), which doubles the
// parsing cost of every frame to save a few unused pointer fields.
//
// The pointers matter: `Delta` appears on both content_block_delta (carrying
// text/thinking/partial_json) and message_delta (carrying stop_reason), and nil
// is how "this event had no delta at all" stays distinguishable from "it had an
// empty one".
type anthropicStreamEvent struct {
	Type string `json:"type"`

	// Index ties a content_block_* event to its block. Parallel tool calls
	// interleave, so this is the only thing keeping one call's argument
	// fragments out of another's buffer — the same role `index` plays in the
	// OpenAI adapter's tool_calls array.
	Index int `json:"index"`

	// Message is present on message_start only. Its usage is READ BY NOTHING;
	// see the loop below for why.
	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`

	ContentBlock *anthropicStreamBlock `json:"content_block"`
	Delta        *anthropicStreamDelta `json:"delta"`

	// Usage on message_delta — the only trustworthy usage on this wire.
	Usage *anthropicUsage `json:"usage"`

	// Cost is a non-standard key smuggled onto the trailing ping (§B6, §C10).
	//
	// Typed as RawMessage, not string, on purpose: §C10 found it is always a
	// JSON *string* ("0"), and if it ever arrives as a number a `string` field
	// would fail to unmarshal — taking the WHOLE frame down with it, not just
	// the field. One optional non-standard key must never be able to break the
	// parse of everything around it.
	Cost json.RawMessage `json:"cost"`

	// Error appears on `event: error` frames. Not observed on this gateway
	// (§D11's errors all arrive as HTTP status codes before the stream opens),
	// but the spec streams overloaded_error and api_error mid-body, and a
	// stream that dies must not be recorded as one that finished.
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStreamBlock struct {
	Type string `json:"type"` // "text", "thinking", "tool_use"

	// ID and Name arrive HERE and nowhere else — content_block_start is the only
	// event that names a tool call. Miss it and you have arguments you cannot
	// attribute and a tool_use_id you cannot answer with.
	ID   string `json:"id"`
	Name string `json:"name"`

	// Text and Thinking are "" on every observed content_block_start (§B6, §B7);
	// the content arrives as deltas. Read anyway — see the loop.
	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// Input is an EMPTY OBJECT here, always (§B6). The real arguments arrive as
	// input_json_delta fragments. A parser that trusts this field gets `{}` for
	// every tool call and executes nothing.
	Input json.RawMessage `json:"input"`
}

// anthropicStreamDelta covers both delta shapes: the content_block_delta
// payload (Type is text_delta / thinking_delta / input_json_delta /
// signature_delta) and the message_delta payload (StopReason).
type anthropicStreamDelta struct {
	Type string `json:"type"`

	Text     string `json:"text"`     // text_delta
	Thinking string `json:"thinking"` // thinking_delta

	// PartialJSON fragments are NOT JSON-aligned. §B6 recorded the observed
	// splits: "", `{"command": "ls`, ` -la /srv`, `/app`, `"`, `}` — the first
	// is empty, the fourth ends mid-path and the fifth resumes it. At no point
	// is a fragment parseable, so they are concatenated raw and parsed exactly
	// once, by the caller, after the stream ends.
	PartialJSON string `json:"partial_json"`

	// Signature is always "" on this endpoint (§B7), including in
	// signature_delta frames. The frame exists to satisfy the shape and carries
	// nothing, so there is no thinking block to verify or replay.
	Signature string `json:"signature"`

	StopReason   string `json:"stop_reason"`   // message_delta
	StopSequence string `json:"stop_sequence"` // absent entirely on this gateway
}

// anthropicUsage is this protocol's token accounting, in this protocol's
// direction — which is the OPPOSITE of the OpenAI one.
//
// Here `input_tokens` is ONLY the uncached remainder and the cache counters are
// *additional* to it (§C8: input_tokens 18, cache_read 9,775 for a ~9,800-token
// prompt). On the OpenAI side `prompt_tokens` is the full figure and
// `cached_tokens` is nested INSIDE it, so that adapter has to subtract. Same
// cache hit, two opposite arithmetics, one normalised struct — which is the
// entire argument for having a normalised struct.
//
// So the mapping here is a straight copy and the danger is the reverse of the
// OpenAI one: an adapter that "helpfully" subtracts cache_read from input on
// this wire reports a negative prompt on every warm call.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) normalise() Usage {
	return Usage{
		Input:      u.InputTokens,
		CacheWrite: u.CacheCreationInputTokens,
		CacheRead:  u.CacheReadInputTokens,
		Output:     u.OutputTokens,
		// Reasoning stays 0: this protocol reports no thinking-token subtotal.
		// The thinking tokens are real and inside OutputTokens — §A3a shows
		// max_tokens:10 returning output_tokens:4403, nearly all of it a
		// thinking block — there is simply no field that says how many.
		// Reporting 0 means "not reported", never "none were spent".
	}
}

// anthropicBlockAccum is the in-flight state of one content block, keyed by the
// stream's `index`.
type anthropicBlockAccum struct {
	index int
	kind  string // "tool_use", "text", "thinking"
	id    string
	name  string
	args  strings.Builder
}

// anthropicHarnessResidue reports whether a text delta is the gateway's leaked
// `</think>` tag rather than something the model meant to say.
//
// THE DECISION, stated once and enforced in one place: residue is DROPPED from
// user-visible text and REPORTED as a notice. Not silently swallowed, not
// rendered.
//
// What is being handled: this gateway's thinking extraction sometimes fails and
// the closing tag falls through into a real `text` content block. §A3b caught it
// non-streaming (`{"type":"text","text":"\n</think>\n\n"}`) and §B6 caught the
// same thing streaming, at content block index 1. It is not the model's output;
// it is the harness leaking through the seam.
//
// Rendering it would put `</think>` in front of a user's answer. Dropping it
// without a word would mean the trace shows text that never arrived and nobody
// ever learns the gateway is broken. A notice does both jobs: the terminal stays
// clean, and the JSONL keeps the evidence with a pointer to the wire note.
//
// The test is deliberately NARROW: the whole delta, trimmed, must be exactly the
// tag. A substring rule (`strings.ReplaceAll(text, "</think>", "")`) would
// silently mangle a model explaining how think-tags work — a real thing to ask
// a coding agent — and quietly corrupting genuine output to tidy up vendor
// garbage is a worse failure than passing one stray tag through. A tag split
// across two deltas would also slip past this; §B6 shows it arriving whole, and
// buffering every text delta on the chance it might be half a tag would add
// latency to every token of every response to catch a case never observed.
func anthropicHarnessResidue(s string) bool {
	switch strings.TrimSpace(s) {
	case "</think>", "<think>":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// ParseStream
// ---------------------------------------------------------------------------

// ParseStream consumes an Anthropic-protocol SSE body, emitting events as they
// arrive, and returns the assembled result.
//
// It emits the same event kinds as the OpenAI adapter, in the same order, with
// the same meanings — which is what lets every renderer, the trace writer and
// replay stay completely protocol-blind. A subscriber cannot tell which
// provider produced the stream it is drawing, and that is the point.
//
// `started` is when the REQUEST went out, not when this function was called.
// TTFT is a property of the round trip; measuring from the moment the response
// header arrived hides exactly the latency you were trying to see.
//
// On a mid-stream failure this returns the partial result AND the error. A
// stream that died after a complete tool call is a different situation from one
// that produced nothing, and the caller can only tell them apart if it is handed
// what did arrive.
func (p *anthropicProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	res := &CallResult{}

	// emit stamps the turn on every event so no call site can forget it, and
	// tolerates a nil bus so the parser can be used as a pure function.
	emit := func(e Event) {
		if bus == nil {
			return
		}
		e.Turn = turn
		bus.Emit(e)
	}

	var (
		text      strings.Builder
		thinking  strings.Builder
		blocks    = map[int]*anthropicBlockAccum{}
		firstSeen bool
	)

	// markFirstToken fires once, on the first byte of real model output.
	//
	// `ping` explicitly does not count, and on this protocol that is not a
	// hypothetical: §B6 records a ping arriving BEFORE message_start, so a TTFT
	// measured from the first frame would be measuring a keepalive. Neither
	// does message_start, which carries no content. Tool-call structure does
	// count — a content_block_start for tool_use is the model having decided
	// which tool to call — and so does thinking, which on a reasoning model is
	// genuinely the first thing generated.
	markFirstToken := func() {
		if firstSeen {
			return
		}
		firstSeen = true
		res.TTFT = time.Since(started)
		emit(Event{Kind: KindFirstToken, Millis: res.TTFT.Milliseconds()})
	}

	// addText is the ONLY path visible text takes, so the `</think>` decision
	// (see anthropicHarnessResidue) is made in exactly one place. Two call sites
	// reach it — content_block_start, which has carried text in no observed
	// stream but is specified to, and text_delta, which carries all of it — and
	// two copies of a filter are two filters that drift apart.
	addText := func(s string) {
		if s == "" {
			return
		}
		// Marked BEFORE the residue check: the bytes did arrive, and TTFT is a
		// measurement of the round trip, not a judgement about what came back.
		markFirstToken()
		if anthropicHarnessResidue(s) {
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("dropped gateway harness residue from visible text: %q (docs/wire-notes.md §A3b, §B6)", s)})
			return
		}
		text.WriteString(s)
		emit(Event{Kind: KindTextDelta, Text: s})
	}

	// addThinking is its own path, and the separation is the point: §B7 warns
	// that code treating every content block as text renders the model's private
	// reasoning to the user. A different Kind means every subscriber decides for
	// itself.
	addThinking := func(s string) {
		if s == "" {
			return
		}
		markFirstToken()
		thinking.WriteString(s)
		emit(Event{Kind: KindReasoningDelta, Text: s})
	}

	// blockAt returns the accumulator for an index, creating it if a delta
	// arrives for a block whose content_block_start was never seen. That should
	// be impossible; if it happens, keeping the fragments beats discarding a
	// tool call because one frame went missing.
	blockAt := func(index int) *anthropicBlockAccum {
		b := blocks[index]
		if b == nil {
			b = &anthropicBlockAccum{index: index}
			blocks[index] = b
		}
		return b
	}

	err := readSSE(r, func(f sseFrame) error {
		payload := strings.TrimSpace(f.Data)
		if payload == "" {
			return nil
		}

		var ev anthropicStreamEvent
		if jerr := json.Unmarshal([]byte(payload), &ev); jerr != nil {
			// One malformed frame must not destroy a turn that has already
			// produced a valid tool call. Surface it as a notice — visible in
			// the trace, survivable in the loop — and carry on. Returning an
			// error here is the tidier-looking choice and the worse one.
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("skipped an SSE frame that was not JSON: %v (%.120s)", jerr, payload)})
			return nil
		}

		// Two sources name the event: the `event:` line and the payload's own
		// `type`. The payload wins because it is the thing that survives a
		// proxy that normalises framing; the `event:` line is the fallback for
		// the reverse case. They have agreed in every frame observed, and the
		// day they do not is a day worth surviving.
		kind := ev.Type
		if kind == "" {
			kind = f.Name
		}

		switch kind {
		case "ping":
			// §B6: pings bracket the stream — one before message_start, one
			// after message_stop — as well as appearing as ordinary keepalives.
			// Tolerated in any position, and counted as nothing.
			//
			// The trailing one is also where `cost` hides, which is the reason
			// this parser keeps reading past message_stop instead of returning
			// there (the same argument sseDoneSentinel makes for the OpenAI
			// side: draining is free and stopping early loses data and the
			// keep-alive connection).
			if len(ev.Cost) > 0 {
				if c := strings.Trim(string(ev.Cost), `"`); c != "" && c != "0" {
					// §C10 saw only "0" here. A non-zero figure would be the
					// first real cost signal this endpoint has ever emitted, so
					// it goes in the trace rather than on the floor.
					emit(Event{Kind: KindNotice, Text: fmt.Sprintf("gateway reported cost %s on the trailing ping", c)})
				}
			}

		case "message_start":
			// DELIBERATELY IGNORED — including, and especially, its usage.
			//
			// §B6 caught message_start reporting input_tokens:56 and
			// message_delta reporting input_tokens:291 FOR THE SAME REQUEST.
			// The non-streaming call with the same prompt agreed with 291. The
			// spec says message_start is authoritative; on this endpoint it is
			// simply wrong, and it also never carries the cache counters, so a
			// parser that reads it under-reports input by 5x and reports a cache
			// hit rate of zero forever.
			//
			// There is no fallback to it if message_delta never arrives, either.
			// A missing number can be seen and chased; a plausible wrong one
			// gets into a cost dashboard and stays there.

		case "content_block_start":
			if ev.ContentBlock == nil {
				return nil
			}
			b := blockAt(ev.Index)
			b.kind = ev.ContentBlock.Type

			// Latch id/name: this event is the only place they appear.
			if ev.ContentBlock.ID != "" {
				b.id = ev.ContentBlock.ID
			}
			if ev.ContentBlock.Name != "" {
				b.name = ev.ContentBlock.Name
			}

			switch b.kind {
			case "tool_use":
				// Announce as soon as the call is identifiable. `input` on this
				// event is `{}` (§B6) and is pointedly not read: the arguments
				// live in the fragments.
				markFirstToken()
				emit(Event{Kind: KindToolCallStart, ToolID: b.id, ToolName: b.name})

			case "text":
				// Observed as "" every time (§B6, §B7). Read anyway rather than
				// assumed empty — dropping model output to match a fixture is
				// how a paragraph goes missing the day a gateway changes.
				addText(ev.ContentBlock.Text)

			case "thinking":
				addThinking(ev.ContentBlock.Thinking)
			}

		case "content_block_delta":
			if ev.Delta == nil {
				return nil
			}

			switch ev.Delta.Type {
			case "text_delta":
				addText(ev.Delta.Text)

			case "thinking_delta":
				addThinking(ev.Delta.Thinking)

			case "input_json_delta":
				b := blockAt(ev.Index)
				if b.kind == "" {
					b.kind = "tool_use"
				}
				// §B6: the FIRST fragment is the empty string. It carries
				// nothing, so it is not a token and not a trace line.
				if ev.Delta.PartialJSON == "" {
					return nil
				}
				markFirstToken()
				b.args.WriteString(ev.Delta.PartialJSON)
				emit(Event{
					Kind:     KindToolArgsDelta,
					ToolID:   b.id,
					ToolName: b.name,
					Text:     ev.Delta.PartialJSON,
				})

			case "signature_delta":
				// §B7: emitted, always empty, nothing to round-trip. Ignored
				// explicitly rather than by falling into the default branch, so
				// it does not generate a notice on every thinking block — and so
				// that the next reader knows it was considered.

			default:
				emit(Event{Kind: KindNotice, Text: fmt.Sprintf("unknown content_block_delta type %q at index %d", ev.Delta.Type, ev.Index)})
			}

		case "content_block_stop":
			// Nothing to do. The block's content is already accumulated, and the
			// index it closes may reopen at a later index for a different block.
			// Tool arguments are parsed exactly once, by the caller, after the
			// whole stream ends — a fragment boundary is not a JSON boundary
			// (§B6) and neither is this event.

		case "message_delta":
			// THE ONLY TRUSTWORTHY FRAME ON THIS STREAM. Both the stop reason
			// and every usage figure — including the cache counters, which
			// appear nowhere else — come from here and from nowhere else.
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				// Latched, not assigned: a second message_delta with a null
				// stop_reason would otherwise erase the one that mattered.
				res.RawStop = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				res.Usage = ev.Usage.normalise()
				// Emit a COPY. Handing out &res.Usage aliases the event to a
				// field the caller can still write to, and a subscriber that
				// serialises lazily would record whatever it later became.
				sent := res.Usage
				emit(Event{Kind: KindUsage, Usage: &sent})
			}

		case "message_stop":
			// NOT a reason to stop reading. §B6 records a ping after it,
			// carrying `cost`, and there is no `[DONE]` sentinel on this
			// protocol at all — the stream ends when the connection closes,
			// which readSSE reports as EOF. Returning here would abandon a body
			// with bytes still in it, which also stops the HTTP transport
			// returning the connection to the pool: a fresh TLS handshake every
			// turn, for the rest of the session, with nothing to show for it.

		case "error":
			// Not observed on this gateway (§D11 errors all arrive as an HTTP
			// status before the stream opens), but the spec streams
			// overloaded_error and api_error mid-body. Returning an error stops
			// readSSE, and the tail of this function then returns the partial
			// result WITHOUT a KindResponseEnd.
			//
			// Returned as a *CallError from stage 09 onward, carrying the
			// provider's own error.type. That field is the whole difference
			// between retrying `overloaded_error` and giving up on it, and a
			// fmt.Errorf string forces the caller to recover it by substring
			// match on a message the provider is free to reword.
			if ev.Error != nil {
				return &CallError{Phase: phaseStream, Type: ev.Error.Type, Message: ev.Error.Message}
			}
			// No error object: nothing to classify, so nothing is claimed. An
			// empty Type routes this to the transport branch of triage(), which
			// retries — the right guess for a malformed frame from a provider
			// that was mid-response, and a guess the trace records as such.
			return &CallError{Phase: phaseStream, Message: fmt.Sprintf("stream error event with no error object: %.200s", payload)}

		default:
			// A new event type is information, not a failure. Noticing it puts
			// it in the trace where someone can read it; ignoring it silently is
			// how a protocol change goes unnoticed for a month.
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("ignored unknown stream event %q", kind)})
		}

		return nil
	})

	res.Text = text.String()
	res.Thinking = thinking.String()

	// Tool calls in ASCENDING BLOCK INDEX order, not arrival order. §B6's
	// two-call stream puts tool_use at indices 0 and 2 with a text block between
	// them, and Go randomises map iteration on purpose, so without this sort the
	// order of a parallel tool call differs from run to run — the kind of bug
	// that reproduces once a week and gets blamed on the model.
	var indices []int
	for i, b := range blocks {
		if b.kind == "tool_use" {
			indices = append(indices, i)
		}
	}
	sort.Ints(indices)
	for _, i := range indices {
		b := blocks[i]
		res.Calls = append(res.Calls, Block{
			Kind: BlockToolCall,
			ID:   b.id,
			Name: b.name,
			Args: b.args.String(),
		})
	}

	// RawStop keeps the literal wire string and Stop the normalised one, and the
	// gap between them is evidence (provider.go says why). §A3c is this
	// protocol's specific reason to care: a tool call truncated at max_tokens
	// arrives with stop_reason "tool_use" and an unusable `input`, so RawStop
	// can never be the only thing a caller checks.
	//
	// normaliseStop runs unconditionally, including on "": a stream that ended
	// without a message_delta maps to StopUnknown, which the agent loop reports
	// instead of continuing. Leaving Stop as the empty string would invent a
	// fourth state that no switch handles.
	res.Stop = normaliseStop(res.RawStop)

	if err != nil {
		// No KindResponseEnd: the response did not end, it broke. Emitting one
		// would tell every subscriber a clean lie, and the trace is supposed to
		// be evidence.
		return res, err
	}

	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.RawStop, // the literal wire string, not the normalised one
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}

// ---------------------------------------------------------------------------
// Stage 04 — where the breakpoints go, and why there.
//
// The rendered prompt is `tools`, then `system`, then `messages`, in that
// order, and caching is a PREFIX match: a cache_control marker says "everything
// up to here is a reusable prefix". Two consequences follow immediately, and
// they are the whole discipline:
//
//   - A marker only helps if everything BEFORE it is byte-identical next time.
//   - A byte that changes early invalidates every marker after it, so the
//     ordering of stable-to-volatile content matters more than the markers do.
//
// Four markers are allowed per request. This adapter places two, which is what
// an agent actually needs:
//
//	tools ─────────┐
//	system ────────┴─▶ [1] frozen for the whole session
//	messages
//	  turn 1 …
//	  turn N ──────────▶ [2] rolling: everything up to the newest turn
//
// Marker 1 pays for itself on every request after the first. Marker 2 is the
// one that matters in an agent: each turn re-sends the entire conversation, so
// without it every turn re-reads the whole history at full price — the 3.7x
// re-send ratio measured back in stage 00.
// ---------------------------------------------------------------------------

// systemBlocks renders the system prompt, pinning it as a cacheable prefix.
//
// Because `tools` renders before `system`, one marker on the last system block
// caches BOTH. That is the whole reason the tool list must be deterministic:
// re-ordering a tool changes bytes at position zero and invalidates everything,
// including this marker.
func (p *anthropicProvider) systemBlocks(system string) []anthropicContent {
	if system == "" {
		return nil
	}
	b := anthropicContent{Type: "text", Text: system}
	if p.cacheBreakpoints {
		b.CacheControl = ephemeral()
	}
	return []anthropicContent{b}
}

// markRollingBreakpoint pins the conversation so far, by marking the last
// content block of the last message.
//
// Why the LAST block of the LAST message, and not a fixed position: each turn
// appends and the marker moves with it, so turn N reads the prefix that turn
// N-1 wrote. A marker parked at a fixed offset would stop growing with the
// conversation and would cache less of it every turn.
//
// The 20-block lookback is the trap here. A breakpoint searches backwards a
// limited number of content blocks for an existing entry, and an agent turn
// that fires many parallel tools can add more blocks than that in one go —
// after which the next marker silently finds nothing and you pay full price
// with no error and no warning. One tool per turn stays far inside the window;
// a fan-out agent needs an intermediate marker, which is what two of the four
// slots are still free for.
func markRollingBreakpoint(msgs []anthropicMessage) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	if len(last.Content) == 0 {
		return
	}
	last.Content[len(last.Content)-1].CacheControl = ephemeral()
}
