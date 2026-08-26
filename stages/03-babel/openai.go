// Stage 03 — the OpenAI protocol adapter.
//
// Everything in this file is one vendor's opinion about how a conversation is
// shaped: the system prompt is a message, a tool result is a message, tool
// arguments are a JSON string nested inside JSON, and a tool definition lives
// one level down under `function`. None of those are facts about language
// models. They are facts about this wire, and they are quarantined here, behind
// the Provider interface in provider.go, so the agent loop never learns any of
// them.
//
// The parsing half was carved out of stage 02's sse.go; the SSE framing it used
// to sit next to now lives in sse.go, which knows nothing about any of this.
// Nothing about the parser changed in the move except its return type — the
// observed behaviours it was built around (§B4 frames 11 and 13, the id latch,
// the unaligned argument fragments) are the same behaviours, and the comments
// that explain them are the same comments.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The provider
// ---------------------------------------------------------------------------

// openaiProvider holds the three things that vary between endpoints speaking
// this protocol. There is no vendor SDK here and no account with anyone in
// particular: a local llama.cpp server, a gateway, and OpenAI itself differ by
// a URL and a model string.
type openaiProvider struct {
	baseURL string
	apiKey  string
	model   string
}

func newOpenAIProvider(baseURL, apiKey, model string) *openaiProvider {
	return &openaiProvider{
		// Trimmed here as well as in config.go, because a provider constructed
		// directly in a test would otherwise POST to `.../v1//chat/completions`
		// — which some servers route and some 404, so the bug appears only on
		// the endpoint you did not test against.
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
	}
}

// Compile-time proof that this file honours provider.go. Without it the first
// evidence of a signature drift is a build failure inside config.go's switch,
// pointing at the wrong file.
var _ Provider = (*openaiProvider)(nil)

func (p *openaiProvider) Protocol() string { return "openai" }
func (p *openaiProvider) Model() string    { return p.model }

// ---------------------------------------------------------------------------
// Request: the neutral conversation, rendered into this vendor's shape
// ---------------------------------------------------------------------------

// oaiMessage is one entry in `messages`.
//
// The `oai` prefix on these types is not decoration. anthropic.go declares the
// same concepts in the same package, and a bare `message` type would mean the
// two adapters race for one name — which is the file-level version of the
// mistake this whole stage exists to prevent.
type oaiMessage struct {
	Role string `json:"role"`

	// Content is omitted when empty rather than sent as null. That is stage
	// 02's shipped behaviour, kept deliberately: an assistant message that is
	// nothing but tool calls has no content, and this endpoint accepts it
	// missing. The one shape it cannot express is a *deliberately* empty tool
	// result — in practice exec.go always appends an `[exit N]` footer, so an
	// empty one never reaches here.
	Content string `json:"content,omitempty"`

	// ToolCalls is set only on assistant messages being replayed back.
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`

	// ToolCallID is set only on `role:"tool"` messages, and it is the whole
	// addressing mechanism on this protocol: the result names the call. Lose
	// the id in the stream parser and the answer has nowhere to go.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`

		// Arguments is a JSON *string* containing JSON — the standard OpenAI
		// double encoding (§A2 shows it verbatim on the response side).
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

type oaiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	Messages  []oaiMessage `json:"messages"`
	Tools     []oaiToolDef `json:"tools,omitempty"`
	Stream    bool         `json:"stream"`

	// A real OpenAI endpoint will not stream usage without this. The gateway
	// this repo was developed against sends usage either way — see
	// docs/wire-notes.md §B5, where the flag is *measurably* a no-op: same 13
	// frames, same position, same fields, with and without it. Send it anyway:
	// it costs nothing and the alternative is an agent that reports zero tokens
	// the day someone points it at a different provider.
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// BuildRequest renders the neutral conversation onto this wire.
//
// It returns the marshalled body alongside the request because the caller emits
// it as KindRequest, and the request inspector is only honest if it shows the
// bytes that were actually sent — not a re-marshalling of the same struct,
// which can differ.
//
// Four translations happen on this path — three below and the fourth in
// assistantMessage — and every one of them is a place the two protocols
// disagree. They are called out at the point they happen rather than listed at
// the top, because the disagreements are the chapter.
func (p *openaiProvider) BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	out := make([]oaiMessage, 0, len(msgs)+1)

	// DISAGREEMENT 1 — where the system prompt lives.
	//
	// Here it is just another message, first in the array, with role "system".
	// On the Anthropic protocol it is a top-level `system` field and cannot be
	// a message at all. That asymmetry is why Provider.BuildRequest takes the
	// system prompt as its own parameter: neither placement can be the neutral
	// one, so the neutral form refuses to choose.
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: system})
	}

	for _, m := range msgs {
		if m.Role == RoleAssistant {
			out = append(out, p.assistantMessage(m))
			continue
		}

		// DISAGREEMENT 2 — how a tool result is addressed.
		//
		// Each result becomes its OWN message, `role:"tool"`, naming the call
		// it answers. Three results, three messages. The Anthropic protocol
		// collapses the same three into tool_result blocks inside ONE user
		// message, and getting that backwards is an API error on both sides.
		//
		// This is exactly why provider.go has no RoleTool: picking either
		// shape as the neutral form would smuggle one vendor's design into the
		// core, so a tool result is a *block* and the adapter decides what
		// message carries it.
		sawText := false
		var text strings.Builder
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolResult:
				out = append(out, oaiMessage{
					Role:       "tool",
					ToolCallID: b.ID,
					Content:    b.Text,
				})
			case BlockText:
				sawText = true
				text.WriteString(b.Text)
			}
			// BlockThinking is dropped on the way out. There is no inbound
			// field for it on this protocol — `reasoning_content` is
			// response-only — so replaying it would either be ignored or
			// rejected depending on whose implementation is on the far end.
		}
		if sawText {
			out = append(out, oaiMessage{Role: string(m.Role), Content: text.String()})
		}
	}

	// DISAGREEMENT 3 — the tool-definition envelope.
	//
	// Here the schema is buried under `{"type":"function","function":{...}}`
	// and the schema key is called `parameters`. The Anthropic protocol puts
	// name/description at the top level and calls the schema `input_schema`.
	// The neutral Tool struct carries neither envelope, which is the only
	// reason one tool table can serve both.
	var defs []oaiToolDef
	for _, t := range tools {
		var d oaiToolDef
		d.Type = "function"
		d.Function.Name = t.Name
		d.Function.Description = t.Description
		d.Function.Parameters = t.Schema
		defs = append(defs, d)
	}

	// Encode with HTML escaping OFF, matching anthropic.go.
	//
	// Go's json.Marshal escapes <, > and & into <, > and & — a
	// browser-safety default that is actively hostile to a shell agent, where
	// those three characters are `2>&1`, `>/tmp/out` and `<<EOF`. One real
	// command becomes:
	//
	//	{"command":"grep -rn 'x' . 2>&1 | head -5 >/tmp/out"}
	//
	// The server decodes it, so the model reads the same string either way.
	// Two things are still worth the four lines. The request inspector is meant
	// to show you what you sent, and that is not readable. And whether the
	// escaping shifts a provider's cache key depends on whether it hashes raw
	// bytes or decoded content — which we do not know, and which is a reason to
	// be consistent rather than a reason to guess.
	//
	// Consistency is the real argument: two adapters emitting different bytes
	// for the same conversation is a wart in a chapter about normalising away
	// exactly that kind of difference.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(oaiRequest{
		Model:         p.model,
		MaxTokens:     maxTokens,
		Messages:      out,
		Tools:         defs,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
	}); err != nil {
		return nil, nil, err
	}
	// Encoder.Encode appends a newline that Marshal does not. Harmless to the
	// server, but it would show up in the inspector and in every trace.
	body := bytes.TrimRight(buf.Bytes(), "\n")

	req, err := http.NewRequest("POST", p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	return req, body, nil
}

// assistantMessage rebuilds the message the API would have returned
// non-streamed, because that is what has to go back in the history. Reassembly
// is the tax you pay for streaming, and forgetting it is why streaming agents
// "lose" their tool calls.
func (p *openaiProvider) assistantMessage(m Msg) oaiMessage {
	am := oaiMessage{Role: "assistant", Content: m.Text()}
	for _, b := range m.Blocks {
		if b.Kind != BlockToolCall {
			continue
		}
		var call oaiToolCall
		call.ID, call.Type = b.ID, "function"
		call.Function.Name = b.Name

		// DISAGREEMENT 4 — the type of `arguments`, and the reason Block.Args
		// is a raw string.
		//
		// This protocol wants a JSON string containing JSON, which is what the
		// stream parser accumulated, so the bytes pass straight through
		// untouched. The Anthropic side wants the same data as a JSON *object*
		// and has to unmarshal them.
		//
		// Storing the neutral form as a decoded map would make this side
		// re-serialise on every turn, and Go randomises map iteration order on
		// purpose — so the same tool call would produce different bytes each
		// time, defeating byte-level prompt caching (§C9: 9,792 of 9,815
		// tokens served from cache, all of it keyed on an exact prefix match)
		// and corrupting any argument value whose formatting mattered.
		call.Function.Arguments = b.Args

		am.ToolCalls = append(am.ToolCalls, call)
	}
	return am
}

// ---------------------------------------------------------------------------
// Response: the streaming chunk schema
// ---------------------------------------------------------------------------

// sseChunk is one `data:` payload on the OpenAI protocol.
//
// The single most important thing about these structs: on this endpoint every
// field is emitted explicitly as `null` rather than omitted (§B4). Go's decoder
// turns `null` into the zero value for a string, nil for a slice, and a no-op
// for a struct — quietly, with no error. That is exactly what we want, and it
// is also the trap: "the key was present" tells you nothing at all here. Test
// the value. Every check below tests a value.
type sseChunk struct {
	// Choices is EMPTY on the usage frame (§B4 frame 11) and on the post-DONE
	// cost frame (frame 13). This is the likeliest place for this file to have
	// had a bug: `chunk.Choices[0]` reads fine, passes every happy-path test,
	// and panics with index-out-of-range on the second-to-last frame of every
	// real request. The loop below is a `range`, which is the fix.
	Choices []sseChoice `json:"choices"`

	// Usage is a pointer so "absent/null" and "present but all zeroes" stay
	// distinguishable. A zero-token response is a legitimate thing to report.
	Usage *sseUsage `json:"usage"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	FinishReason string   `json:"finish_reason"` // null on every chunk but the last
	Delta        sseDelta `json:"delta"`
}

// sseDelta is the incremental payload. Note that reasoning is NOT a separate
// event or block type on this protocol — it rides in this same object, in a
// sibling field, distinguished only by which of the two is non-null (§B7). In
// the run recorded there, 44 frames carried reasoning_content and 1 carried
// content.
type sseDelta struct {
	Role             string             `json:"role"`              // "assistant" on the opener, null after
	Content          string             `json:"content"`           // "" on the opener, null on most chunks
	ReasoningContent string             `json:"reasoning_content"` // §B7: thinking arrives here
	ToolCalls        []sseToolCallDelta `json:"tool_calls"`
}

type sseToolCallDelta struct {
	// Index is the position in the assistant message's tool_calls array, and it
	// is the ONLY thing tying a fragment to the call it belongs to. Parallel
	// tool calls interleave their fragments; accumulate by anything else and
	// you get one call's arguments spliced into another's.
	Index int `json:"index"`

	// ID and Function.Name arrive in exactly ONE chunk and are null in every
	// chunk after it (§B4 frame 2 versus frames 3–9). Latch them on first sight.
	ID       string `json:"id"`
	Type     string `json:"type"` // stays "function" throughout; not nulled, and not a signal
	Function struct {
		Name string `json:"name"`

		// Arguments fragments are NOT JSON-aligned. §B4 observed the splits
		// `{"command": ` / `"` / `ls` / ` -la /srv` / `/app` / `"` / `}` —
		// mid-token and mid-path. There is no point at which a fragment is
		// parseable JSON, so this is accumulated as a raw string and parsed
		// exactly once, by the caller, after the stream ends.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// sseUsage is OpenAI's token accounting, in OpenAI's direction.
type sseUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// normalise converts into this repo's Usage, and the conversion is a direction
// reversal, not a rename.
//
// The wire (§B4 frame 11):
//
//	"prompt_tokens": 506, "prompt_tokens_details": {"cached_tokens": 192}
//
// 506 is the FULL prompt. The 192 cached tokens are counted INSIDE it.
//
// This repo's Usage.Input means "billed at full price" (see events.go), so the
// cached portion has to come back OUT:
//
//	Input = 506 - 192 = 314   CacheRead = 192   →   Prompt() = 506 ✓
//
// Copy the field across unchanged and Usage.Prompt() reports 698 for a
// 506-token prompt. The error is exactly the size of the cache hit, so it is
// zero on a cold first request: it looks perfect while you are testing and gets
// worse the better your caching works. That is the whole reason this is a
// function and not a struct tag.
//
// The Anthropic side reverses it again — there `input_tokens` is only the
// uncached remainder, so it maps straight to Input with nothing subtracted. Two
// protocols, opposite conventions, one normalised struct, which is the argument
// for having a normalised struct.
func (u sseUsage) normalise() Usage {
	cached := u.PromptTokensDetails.CachedTokens

	// Clamp rather than trust. A negative Input would propagate into Prompt()
	// and into any cost estimate built on it; if the endpoint ever reports more
	// cached tokens than prompt tokens, losing the discrepancy beats exporting
	// a negative token count.
	input := u.PromptTokens - cached
	if input < 0 {
		input = 0
	}

	return Usage{
		Input:     input,
		CacheRead: cached,
		Output:    u.CompletionTokens,
		// A subset of Output, not an addition to it — §B4 reports 0 here
		// because that run used reasoning_effort:"none".
		Reasoning: u.CompletionTokensDetails.ReasoningTokens,
		// CacheWrite stays 0: this protocol's caching is implicit and there is
		// no write figure on the wire. It is not zero because nothing was
		// cached; it is zero because the concept is not reported.
	}
}

// sseToolAccum is the in-flight state for one tool call. It is not the returned
// shape because it holds two things the caller must never see: the builder, and
// whether the start event has already gone out.
type sseToolAccum struct {
	index     int
	id        string
	name      string
	args      strings.Builder
	announced bool // KindToolCallStart already emitted for this index
}

// ParseStream consumes an OpenAI-protocol SSE body, emitting events onto bus as
// they arrive, and returns the assembled result in the neutral shape.
//
// `started` is when the request went out, not when this function was called —
// TTFT is a property of the round trip, and measuring from the moment the
// response header arrived hides the entire latency you were trying to see.
//
// On a mid-stream I/O failure this returns the partial result AND the error,
// which is a deliberate break from the usual `return nil, err`. A stream that
// died after a complete tool call is a different situation from one that
// produced nothing, and the caller can only tell them apart if it is handed
// what did arrive. Callers must still check the error — a partial result with
// no finish_reason is a truncation, and stage 01 is an entire chapter about
// what happens when truncation goes unnoticed.
func (p *openaiProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	res := &CallResult{}

	// emit stamps the turn on every event, so no call site can forget it, and
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
		reasoning strings.Builder
		calls     = map[int]*sseToolAccum{}
		firstSeen bool
	)

	// markFirstToken fires once, on the first byte of actual model output.
	//
	// The role opener (§B4 frame 1) deliberately does not count: it carries
	// `content: ""` and no payload at all. Counting it would turn TTFT into
	// time-to-first-byte, which on a model that thinks for four seconds before
	// speaking is a number that looks great and means nothing. Text, reasoning
	// and tool-call structure all count — reasoning especially, since on a
	// thinking model it is genuinely the first thing generated.
	markFirstToken := func() {
		if firstSeen {
			return
		}
		firstSeen = true
		res.TTFT = time.Since(started)
		emit(Event{Kind: KindFirstToken, Millis: res.TTFT.Milliseconds()})
	}

	err := readSSE(r, func(f sseFrame) error {
		payload := strings.TrimSpace(f.Data)
		if payload == "" {
			return nil
		}
		if payload == sseDoneSentinel {
			// Skip it, keep reading. See sseDoneSentinel for why.
			return nil
		}

		var c sseChunk
		if jerr := json.Unmarshal([]byte(payload), &c); jerr != nil {
			// One malformed frame should not destroy a turn that has already
			// produced a valid tool call. Surface it as a notice — visible in
			// the trace, survivable in the loop — and carry on. Returning an
			// error here is the tidier-looking choice and the worse one.
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("skipped an SSE frame that was not JSON: %v (%.120s)", jerr, payload)})
			return nil
		}

		// range over Choices, never Choices[0]. On the usage frame and on the
		// post-DONE cost frame this array is empty and the body simply does not
		// run. That one word is the difference between this file working and
		// this file panicking on the second-to-last frame of every request.
		//
		// (`n > 1` would interleave several completions into one result. This
		// agent never asks for it, and supporting it properly means keying
		// every accumulator by choice index as well as tool index.)
		for _, ch := range c.Choices {
			d := ch.Delta

			if d.Content != "" {
				markFirstToken()
				text.WriteString(d.Content)
				emit(Event{Kind: KindTextDelta, Text: d.Content})
			}

			if d.ReasoningContent != "" {
				markFirstToken()
				reasoning.WriteString(d.ReasoningContent)
				emit(Event{Kind: KindReasoningDelta, Text: d.ReasoningContent})
			}

			for _, tc := range d.ToolCalls {
				markFirstToken()

				acc := calls[tc.Index]
				if acc == nil {
					acc = &sseToolAccum{index: tc.Index}
					calls[tc.Index] = acc
				}

				// THE LATCH. Assign only when the incoming value is non-empty.
				// Frames 3–9 of §B4 carry `"id":null,"function":{"name":null}`,
				// and a plain `acc.id = tc.ID` would blank the id on the very
				// next chunk — leaving a tool call with complete arguments that
				// cannot be answered, because the tool_call_id the API demands
				// in the reply is gone.
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}

				// Announce once, as soon as this call is identifiable at all.
				// On this endpoint id and name arrive together in one chunk, so
				// in practice the event always carries both; gating on "either
				// is non-empty" means a protocol that split them still gets an
				// announcement rather than silence.
				if !acc.announced && (acc.id != "" || acc.name != "") {
					acc.announced = true
					emit(Event{Kind: KindToolCallStart, ToolID: acc.id, ToolName: acc.name})
				}

				// The opener carries `"arguments":""`, so the empty check keeps
				// a meaningless zero-length delta out of the trace. Fragments
				// are appended raw and never inspected — see sseToolCallDelta.
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
					emit(Event{
						Kind:     KindToolArgsDelta,
						ToolID:   acc.id,
						ToolName: acc.name,
						Text:     tc.Function.Arguments,
					})
				}
			}

			// Latched the same way, for the same reason: null everywhere except
			// the finish chunk, and an unguarded assignment would erase it on
			// the frames that follow it.
			//
			// The literal string is what gets stored. Normalising here would
			// mean normalising in two places (this branch and the fallback
			// below), and the second one is always the one that rots.
			if ch.FinishReason != "" {
				res.RawStop = ch.FinishReason
			}
		}

		if c.Usage != nil {
			u := c.Usage.normalise()
			res.Usage = u

			// Emit a COPY. Handing out &res.Usage would alias the event to a
			// field the caller can still write to, and a subscriber that
			// serialises lazily (the trace writer does not; the TUI later
			// might) would record whatever it later became.
			sent := u
			emit(Event{Kind: KindUsage, Usage: &sent})
		}

		return nil
	})

	res.Text = text.String()
	res.Thinking = reasoning.String()

	// RawStop keeps the provider's literal word; Stop keeps the normalised one.
	// Both, not either: see CallResult.RawStop for the case (§A3c) where the
	// envelope lies and the gap between the two is the only evidence left.
	//
	// Done unconditionally, including when RawStop is "" — a stream that ended
	// without a finish_reason normalises to StopUnknown, which the agent loop
	// reports, rather than to the zero StopReason, which is a value no switch
	// in this repo has a case for.
	res.Stop = normaliseStop(res.RawStop)

	// Ascending index order, not arrival order. Map iteration in Go is
	// randomised on purpose, so without this sort the order differs from run to
	// run — the kind of bug that reproduces once a week and gets blamed on the
	// model. Left nil when there are no tool calls, so a text-only result
	// compares equal to a zero CallResult.
	if len(calls) > 0 {
		ordered := make([]*sseToolAccum, 0, len(calls))
		for _, a := range calls {
			ordered = append(ordered, a)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })

		res.Calls = make([]Block, 0, len(ordered))
		for _, a := range ordered {
			res.Calls = append(res.Calls, Block{
				Kind: BlockToolCall,
				ID:   a.id,
				Name: a.name,
				Args: a.args.String(),
			})
		}
	}

	if err != nil {
		// No KindResponseEnd: the response did not end, it broke. Emitting one
		// would tell every subscriber a clean lie, and the trace is supposed to
		// be evidence.
		return res, err
	}

	// RawStop is "" here if the stream ended without a finish_reason — a
	// truncation this protocol reports by simply not mentioning it. Passing the
	// empty string through unchanged keeps that visible to the caller instead
	// of inventing a "stop" that never happened; Stop stays StopUnknown, which
	// the agent loop reports rather than treating as a clean finish.
	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.RawStop,
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}
