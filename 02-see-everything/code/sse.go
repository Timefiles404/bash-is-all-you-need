// Stage 02 — reading a streaming response.
//
// A non-streaming call gives you one JSON object and one moment: the whole
// answer lands several seconds after you asked for it. Streaming trades that
// for a sequence of fragments, and almost everything that makes an agent feel
// alive comes out of that sequence — text appearing as it is written, a
// time-to-first-token number, a tool call you can name on screen before its
// arguments have finished arriving.
//
// This file is deliberately two halves:
//
//	readSSE           knows about Server-Sent Events and nothing else. It has
//	                  never heard of OpenAI, tool calls, or tokens.
//	parseOpenAIStream knows one vendor's chunk schema and turns it into this
//	                  repo's events.
//
// Stage 03 adds the Anthropic protocol, which is an entirely different chunk
// schema carried over the *same* framing. It reuses the first half verbatim and
// writes a second parser beside the second half. Were these one function, that
// stage would be a rewrite instead of an addition — which is the whole argument
// for the split, in one sentence.
//
// Everything below is written against docs/wire-notes.md §B4/§B5/§B7, which
// recorded what this endpoint actually sends rather than what the specification
// says it should. Where the two disagree the bytes win, and each disagreement is
// commented. Those comments are the most valuable lines in the file: every one
// of them is a crash, or a silently wrong number, that a spec-reading client
// walks straight into.
//
// Not handled here, on purpose: in-band error frames. On this endpoint an error
// is a non-200 response with a JSON body (§D11), never a frame inside a 200
// stream, so there is nothing to look for.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Half one: SSE framing. Protocol-agnostic on purpose.
// ---------------------------------------------------------------------------

// sseFrame is one decoded SSE frame. Name is "" on streams that omit event:
// lines — which is every frame this stage will ever see, because the OpenAI side
// of this endpoint sends only `data:` (§B4: `grep -c '^event:'` = 0 across the
// whole stream). Name exists anyway because the Anthropic side in stage 03 does
// use `event:` lines, and a reader that has to be taught about them later is a
// reader that is wrong in between.
type sseFrame struct {
	Name string
	Data string
}

// readSSE calls fn for each frame until the stream ends. It must handle: frames
// with only `data:` lines, frames with `event:` + `data:`, multi-line data,
// blank-line separation, CRLF, and comment lines starting with ':'.
// Returning a non-nil error from fn stops the scan and returns that error.
//
// Note what it does *not* do: it has no idea what `[DONE]` means. A sentinel is
// a property of the payload protocol, not of the framing, and pushing that
// knowledge down here is how you end up unable to reuse the reader.
//
// Three details in the implementation are each worth a bug:
//
//  1. bufio.Reader, not bufio.Scanner. Scanner refuses tokens over 64KB by
//     default and reports that as an error at the worst possible moment — a
//     large tool result echoed back in one delta is exactly the frame that
//     trips it, and it will only ever happen in production.
//
//  2. The last line of the stream is processed *before* the EOF is acted on.
//     ReadString hands back the bytes it managed to read alongside io.EOF, so a
//     server that closes without a trailing blank line still has its final
//     frame sitting in `line`. Check the error first and you silently drop the
//     last frame of every such stream — usually the one carrying usage.
//
//  3. Line endings are stripped one at a time (`\n`, then `\r`) rather than
//     with a cutset, so data that legitimately ends in a carriage return keeps
//     it. A lone-CR terminator — permitted by the SSE spec, emitted by nobody,
//     and absent from §B4 — is out of scope; observation wins over the spec
//     here, as everywhere else in this file.
func readSSE(r io.Reader, fn func(sseFrame) error) error {
	br := bufio.NewReader(r)

	var (
		name    string
		data    []string // one entry per `data:` line; joined with "\n" on dispatch
		sawData bool     // whether *any* data line arrived, not whether it was non-empty
	)

	// dispatch delivers the frame built so far and resets the buffers.
	//
	// The spec says a frame with no data lines is not an event, and that is the
	// rule here: it makes runs of blank lines and bare keep-alive comments free,
	// rather than a stutter of empty frames. A frame with a data line that
	// happens to be empty *does* dispatch, which is a deliberate step past the
	// spec — this is a debugging tool, and a visibly empty frame teaches more
	// than a silently dropped one.
	dispatch := func() error {
		if !sawData {
			name = ""
			return nil
		}
		f := sseFrame{Name: name, Data: strings.Join(data, "\n")}
		name, data, sawData = "", data[:0], false
		return fn(f)
	}

	for {
		line, err := br.ReadString('\n')

		if line != "" {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")

			switch {
			case line == "":
				// Blank line: end of frame.
				if derr := dispatch(); derr != nil {
					return derr
				}

			case strings.HasPrefix(line, ":"):
				// Comment. Proxies and gateways send these as keep-alives so an
				// idle connection is not reaped mid-generation. They carry
				// nothing and must not end the current frame — and note this
				// case has to be tested before the field split below, or
				// `: ping` parses as a field with an empty name.

			default:
				// `field: value`, where only the FIRST colon separates and a
				// single leading space of the value is stripped. Both matter:
				// every payload here is JSON, so values are full of colons, and
				// getting the space rule wrong shifts every byte of every
				// message by one.
				field, value := line, ""
				if i := strings.IndexByte(line, ':'); i >= 0 {
					field, value = line[:i], line[i+1:]
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "event":
					name = value
				case "data":
					data = append(data, value)
					sawData = true
				}
				// `id:` and `retry:` are spec fields for reconnecting to a
				// broken stream. Neither appears in §B4, and resuming a
				// half-generated completion is not something this endpoint
				// offers, so they are ignored rather than half-supported.
			}
		}

		if err != nil {
			if err == io.EOF {
				// The stream ended. Anything still buffered is a real frame
				// that never got its terminating blank line — the Anthropic
				// side (§B6) ends exactly this way, by closing the connection
				// with no sentinel at all.
				return dispatch()
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// Half two: the OpenAI chunk schema.
// ---------------------------------------------------------------------------

// sseDoneSentinel is the frame the OpenAI protocol uses to say "that's all".
//
// DECISION: we skip it and KEEP DRAINING to EOF. It is not a stop signal here.
//
// §B4 frame 13 is a real frame that arrives *after* the sentinel:
// `{"choices":[],"cost":"0"}`. Every spec-conforming client stops reading at
// `[DONE]` and throws that away. Three reasons not to be one of them:
//
//   - Correctness. The cost frame is data this endpoint is trying to give us.
//   - Connection hygiene. Abandoning a response body with bytes still in it
//     means the HTTP transport cannot return the connection to the keep-alive
//     pool; you pay a fresh TLS handshake every turn and never notice why.
//   - Robustness. If usage ever moves after the sentinel — and on an endpoint
//     that already puts `cost` there, that is not a wild hypothesis — a client
//     that stops early reports zero tokens and is confidently wrong.
//
// Draining costs nothing: the server closes the stream immediately afterwards.
const sseDoneSentinel = "[DONE]"

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

// streamToolCall is one tool call assembled across many chunks.
type streamToolCall struct {
	ID   string
	Name string
	Args string // the concatenated raw JSON string; NOT parsed here
}

// streamResult is what one streamed model call produced.
type streamResult struct {
	Text         string
	Reasoning    string
	ToolCalls    []streamToolCall // in ascending index order
	FinishReason string
	Usage        Usage
	TTFT         time.Duration // zero if nothing ever streamed
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

// parseOpenAIStream consumes an OpenAI-protocol SSE body, emitting events onto
// bus as they arrive, and returns the assembled result.
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
func parseOpenAIStream(r io.Reader, bus *Bus, turn int, started time.Time) (*streamResult, error) {
	res := &streamResult{}

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
			if ch.FinishReason != "" {
				res.FinishReason = ch.FinishReason
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
	res.Reasoning = reasoning.String()

	// Ascending index order, not arrival order. Map iteration in Go is
	// randomised on purpose, so without this sort the order differs from run to
	// run — the kind of bug that reproduces once a week and gets blamed on the
	// model. Left nil when there are no tool calls, so a text-only result
	// compares equal to a zero streamResult.
	if len(calls) > 0 {
		ordered := make([]*sseToolAccum, 0, len(calls))
		for _, a := range calls {
			ordered = append(ordered, a)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })

		res.ToolCalls = make([]streamToolCall, 0, len(ordered))
		for _, a := range ordered {
			res.ToolCalls = append(res.ToolCalls, streamToolCall{
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

	// FinishReason is "" here if the stream ended without one — a truncation
	// this protocol reports by simply not mentioning it. Passing the empty
	// string through unchanged keeps that visible to the caller instead of
	// inventing a "stop" that never happened.
	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.FinishReason,
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}
