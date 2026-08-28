// Stage 02 — the event bus.
//
// This file is the architectural claim of the whole repo:
//
//	The agent core prints NOTHING. It emits events. Everything you can see —
//	the plain terminal output, the JSONL trace, the replay viewer, and later the
//	TUI — is a subscriber.
//
// That one constraint buys most of what the rest of the project needs. The
// trace file is a subscriber, so history is recorded for free. Replay is the
// trace read back through the same renderer, so a session can be studied with
// no API key. `--plain` versus a TUI is a choice of subscriber, not a fork of
// the code. Tests assert on an event sequence instead of scraping stdout.
//
// The lesson worth taking away is not "use an event bus". It is that
// observability is a shape you choose at the start, not logging you sprinkle on
// at the end. Stage 00 and 01 wrote fmt.Printf into the loop, and every one of
// those calls is a place where the only record of what happened was a character
// on a terminal that scrolled away.
package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Kind identifies what happened. Keep these stable: they are written into trace
// files, and a renamed kind silently breaks replay of every session recorded
// before the rename.
type Kind string

const (
	// Conversation shape
	KindUserMessage Kind = "user_message" // the human said something
	KindTurnStart   Kind = "turn_start"   // one model round begins
	KindTurnEnd     Kind = "turn_end"     // the model stopped asking for tools

	// The model call
	KindRequest        Kind = "request"         // the exact bytes about to be sent
	KindFirstToken     Kind = "first_token"     // TTFT lands here
	KindTextDelta      Kind = "text_delta"      // visible assistant text
	KindReasoningDelta Kind = "reasoning_delta" // thinking, where the model streams it
	KindUsage          Kind = "usage"           // token accounting for one call
	KindResponseEnd    Kind = "response_end"    // finish_reason and timings

	// Tool use
	KindToolCallStart Kind = "tool_call_start" // id + name arrive (once, early)
	KindToolArgsDelta Kind = "tool_args_delta" // raw argument fragments
	KindToolCallReady Kind = "tool_call_ready" // arguments complete and validated
	KindGateVerdict   Kind = "gate_verdict"    // allowed / denied / aborted
	KindCommandStart  Kind = "command_start"
	KindCommandEnd    Kind = "command_end"
	KindToolResult    Kind = "tool_result" // exactly what the model will be told

	// Everything else
	KindNotice Kind = "notice" // something the user should know, not an error
	KindError  Kind = "error"
)

// Usage is one call's token accounting, in the only shape that is not a lie.
//
// The trap this struct exists to avoid: on an Anthropic-style protocol,
// `input_tokens` is *only the uncached remainder* — an agent that ran for an
// hour can report 18 input tokens while actually sending 18,000. The total is
// Input + CacheWrite + CacheRead, and the renderer must show the split, because
// the three cost wildly different amounts (roughly 1x, 1.25x and 0.1x).
//
// An OpenAI-style protocol accounts in the opposite direction: prompt_tokens is
// the full figure and cached_tokens is nested *inside* it. Stage 03 is where
// that conversion lives; this struct is already in the normalised form, which is
// why it has no field called "prompt_tokens".
type Usage struct {
	Input      int `json:"input"`                 // billed at full price
	CacheWrite int `json:"cache_write,omitempty"` // ~1.25x
	CacheRead  int `json:"cache_read,omitempty"`  // ~0.1x
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning,omitempty"` // subset of Output, where reported
}

// Prompt returns everything that was sent, which is the number people mean when
// they ask "how big is my context now" — and the number you cannot get by
// reading any single field the API returns.
func (u Usage) Prompt() int { return u.Input + u.CacheWrite + u.CacheRead }

// Event is deliberately one flat struct rather than an interface hierarchy.
//
// A sum type would be more elegant in Go and much worse here: it needs custom
// JSON unmarshalling to replay, and it hides the shape of the data behind a
// type switch. Flat means a trace line is readable with your eyes, `jq` works
// on it without a schema, and adding a field is one line. `omitempty` keeps the
// lines short.
type Event struct {
	Seq  int       `json:"seq"` // monotonic; the only ordering you should trust
	T    time.Time `json:"t"`
	Kind Kind      `json:"kind"`

	Turn int `json:"turn,omitempty"` // which model round inside the current user message

	// Text carries whatever this kind is about: a delta fragment, a notice, an
	// error message, the user's message.
	Text string `json:"text,omitempty"`

	// Tool use
	ToolID   string `json:"tool_id,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Command  string `json:"command,omitempty"`
	Verdict  string `json:"verdict,omitempty"`

	// Command outcome
	ExitCode  int  `json:"exit_code,omitempty"`
	TimedOut  bool `json:"timed_out,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
	Bytes     int  `json:"bytes,omitempty"`

	// Model call outcome
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`

	// Millis is the duration this event reports: TTFT on first_token, wall
	// clock on command_end and response_end.
	Millis int64 `json:"ms,omitempty"`

	// Request is the full JSON body about to be sent. It is what makes the
	// request inspector possible, and it is the single most useful thing in a
	// trace when you are trying to work out why a model did something: it is
	// the only record of what the model actually saw.
	Request json.RawMessage `json:"request,omitempty"`
}

// Subscriber receives every event, in order.
type Subscriber interface {
	OnEvent(Event)
}

// Bus fans events out to subscribers.
//
// Dispatch is synchronous and under a lock, which is a deliberate choice: it
// makes ordering total and identical for every subscriber, so the trace file
// and the terminal can never disagree about what happened first. An async bus
// with per-subscriber queues would scale better and would make the trace stop
// being evidence. A renderer that needs to be slow should buffer internally.
type Bus struct {
	mu   sync.Mutex
	seq  int
	subs []Subscriber
}

func NewBus(subs ...Subscriber) *Bus { return &Bus{subs: subs} }

func (b *Bus) Subscribe(s Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, s)
}

// Emit stamps the event and delivers it. Seq and T are assigned here so no
// caller can forge them, and so a replayed trace can be compared against a live
// run event for event.
func (b *Bus) Emit(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if e.T.IsZero() {
		e.T = time.Now()
	}
	for _, s := range b.subs {
		s.OnEvent(e)
	}
}

// Helpers for the common shapes, so the agent core reads as prose rather than
// as struct literals.

func (b *Bus) Notice(format string, args ...any) {
	b.Emit(Event{Kind: KindNotice, Text: fmt.Sprintf(format, args...)})
}

func (b *Bus) Error(format string, args ...any) {
	b.Emit(Event{Kind: KindError, Text: fmt.Sprintf(format, args...)})
}
