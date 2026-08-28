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

	// Subagents and skills (stage 07)
	KindSubagentStart Kind = "subagent_start"
	KindSubagentEnd   Kind = "subagent_end"
	KindSkillsIndexed Kind = "skills_indexed" // how many skills, and what the index cost

	// Memory and context injection (stage 05)
	KindMemoryLoaded Kind = "memory_loaded" // a memory file was read into the system prompt

	// Compaction (stage 05). Three events, not one, because they answer three
	// different questions: that it is happening, what it cost, and what it
	// broke. The third is the one every other implementation omits.
	KindCompactStart     Kind = "compact_start"
	KindCompactEnd       Kind = "compact_end"
	KindCacheInvalidated Kind = "cache_invalidated"

	// Failure and recovery (stage 09). Three kinds for the same reason
	// compaction has three: they answer different questions, and the one every
	// other implementation collapses into a log line is the one you need.
	//
	//	call_error  what broke, and what we decided about it
	//	retry       that we are waiting, how long, and whose number that is
	//	provider    who serves calls from here on, and at what prices
	//
	// KindCallError is not KindError. KindError is terminal — the session is
	// telling the human it failed. A call_error is an attempt failing with a
	// decision attached, and most of them are followed by a success. Emitting
	// them as errors would train people to ignore the word.
	KindCallError Kind = "call_error"
	KindRetry     Kind = "retry"
	KindProvider  Kind = "provider"

	// Stage 10. The widest gap between two frames of one stream, which is the
	// quantity the idle deadline is compared against.
	//
	// It is an event rather than a field on response_end because a stream that
	// STALLED never produces a response_end — and the stalled call is the one
	// whose gap you most want to see. Emitting it separately means the number
	// survives the failure it describes.
	KindIdleMax Kind = "idle_max"

	// Stage 11. A tool call that did not survive validation.
	//
	// Separate from KindToolResult even though the model is told through a tool
	// result, because the two answer different questions and only one of them
	// survives normalisation: the result says what the model was told, and this
	// says what actually arrived and which check rejected it. Fold them together
	// and the trace can no longer distinguish a tool that ran and failed from a
	// call that was never a call — and those have different fixes.
	KindToolCallInvalid Kind = "tool_call_invalid"

	// Everything else
	KindNotice Kind = "notice" // something the user should know, not an error
	KindError  Kind = "error"
)

// ProviderInfo is who served a call, and what their tokens cost.
//
// It rides on KindProvider because a fallback changes the cost basis in the
// middle of a session. Without it the panel would keep pricing the second
// provider's tokens at the first provider's rates — a cost report that is
// confidently wrong, which is worse than one that admits it does not know — and
// a replayed trace would have no way to find out. It is also, incidentally, the
// first time a trace records which endpoint produced it at all.
type ProviderInfo struct {
	Name     string      `json:"name"`
	Protocol string      `json:"protocol"`
	Model    string      `json:"model"`
	Window   int         `json:"window,omitempty"`
	Prices   priceConfig `json:"prices,omitempty"`
}

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

	// Which agent emitted this. Depth 0 is the one the human is talking to;
	// a subagent is depth 1, and so on. Stamped by the Bus, not by callers, for
	// the same reason Seq is: a field a caller can forge is a field a trace
	// cannot be evidence about.
	Depth int    `json:"depth,omitempty"`
	Agent string `json:"agent,omitempty"`

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

	// Compaction accounting. Before/after pairs rather than a delta, because a
	// delta cannot tell you whether a compaction that freed 8,000 tokens
	// started from 40,000 or from 9,000 — and those are a success and a
	// misconfiguration.
	MsgsBefore   int `json:"msgs_before,omitempty"`
	MsgsAfter    int `json:"msgs_after,omitempty"`
	TokensBefore int `json:"tokens_before,omitempty"`
	TokensAfter  int `json:"tokens_after,omitempty"`

	// Failure accounting (stage 09). Status is 0 when the request never got a
	// response at all, which is a different fact from "the response was 0" and
	// the reason this is not folded into Text.
	//
	// ErrType is the provider's `error.type` verbatim, un-normalised on
	// purpose: it is the field that separates a wrong model name from a revoked
	// key when the status code cannot (§D11), so a trace that normalised it
	// would have thrown away the evidence for its own decision.
	// Phase is where in the call it broke: build | connect | status | stream.
	//
	// It is not decoration next to Status, and the difference is money. A
	// failure at `status` or `connect` was refused before the model generated
	// anything, so the attempt was free. A failure at `stream` got its 200 and
	// its tokens, and those tokens are billed whether or not the bytes arrived
	// — so this is the field that decides whether a retry cost anything, and
	// the panel gets it wrong without it. Status cannot substitute: a connect
	// failure and a stream break both carry Status 0.
	Status  int    `json:"status,omitempty"`
	Phase   string `json:"phase,omitempty"`
	ErrType string `json:"err_type,omitempty"`
	Triage  string `json:"triage,omitempty"`  // retry | fallback | fatal
	Attempt int    `json:"attempt,omitempty"` // 1-based, counted across the whole ladder

	// Provider says who serves calls from here on. Set at startup and on every
	// fallback; see ProviderInfo for why the prices travel with it.
	Provider *ProviderInfo `json:"provider,omitempty"`

	// Path names a file this event is about (a memory file, so far).
	Path string `json:"path,omitempty"`

	// Fault is which validation rejected a tool call: cut | not_json | schema
	// (stage 11). It rides beside Text rather than inside it because the class
	// is what you count and the text is what you read, and a panel that has to
	// regex its own trace to count something is a panel that will get the
	// count wrong.
	Fault string `json:"fault,omitempty"`

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
//
// Stage 07 is where that choice pays for itself, and it is worth noticing that
// the payment was made three chapters early. Subagents run concurrently, so
// there are now several goroutines producing events at once. One lock and one
// counter mean the trace is still a single totally ordered stream — every event
// has a Seq that says exactly when it happened relative to every other event,
// across every agent. An async per-subscriber bus would have given each
// subscriber a different story about a concurrent session, which is precisely
// the session you cannot reason about without one.
type busCore struct {
	mu   sync.Mutex
	seq  int
	subs []Subscriber
}

// Bus is a *view* onto a core: the same counter and the same subscribers, with
// a depth and an agent name stamped onto everything it emits.
type Bus struct {
	core  *busCore
	depth int
	agent string
}

func NewBus(subs ...Subscriber) *Bus {
	return &Bus{core: &busCore{subs: subs}}
}

// Fork returns the bus a subagent should emit on.
//
// Nothing is duplicated: the child writes into the same ordered stream as its
// parent, so one trace file holds the whole tree and `seq` orders it. The
// alternative — a trace per agent — is what most implementations do, and it
// makes the one question you actually have ("what was the parent doing while
// the child ran?") unanswerable without merging files by timestamp, which is
// exactly the thing timestamps are bad at.
func (b *Bus) Fork(agent string) *Bus {
	return &Bus{core: b.core, depth: b.depth + 1, agent: agent}
}

func (b *Bus) Depth() int { return b.depth }

func (b *Bus) Subscribe(s Subscriber) {
	b.core.mu.Lock()
	defer b.core.mu.Unlock()
	b.core.subs = append(b.core.subs, s)
}

// Emit stamps the event and delivers it. Seq, T, Depth and Agent are assigned
// here so no caller can forge them, and so a replayed trace can be compared
// against a live run event for event.
func (b *Bus) Emit(e Event) {
	b.core.mu.Lock()
	defer b.core.mu.Unlock()
	b.core.seq++
	e.Seq = b.core.seq
	if e.T.IsZero() {
		e.T = time.Now()
	}
	e.Depth, e.Agent = b.depth, b.agent
	for _, s := range b.core.subs {
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
