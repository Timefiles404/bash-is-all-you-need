// Stage 03 — Babel.
//
// Two protocols, one agent. This file is the language the agent speaks; the
// adapters (openai.go, anthropic.go) translate it at the wire.
//
// The rule that makes it work: **the agent loop must never contain a vendor's
// word.** No `tool_calls`, no `stop_reason`, no `input_tokens`. If one leaks
// into main.go, the second protocol stops being an adapter and becomes an `if`
// statement, and then a hundred `if` statements.
//
// What the adapters have to reconcile is not cosmetic. The two protocols
// disagree about where the system prompt goes, how tool results are addressed,
// whether tool arguments are a string or an object, what the stop reason is
// called, and — most expensively — which direction token accounting runs. The
// table in 03-babel/doc/ lists them all with observed evidence.
package main

import (
	"context"
	"io"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// The neutral conversation
// ---------------------------------------------------------------------------

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Note what is missing: there is no RoleTool.
//
// The OpenAI protocol answers a tool call with its own `role:"tool"` message,
// one per call. The Anthropic protocol answers with `tool_result` blocks inside
// a single **user** message. Picking either as the neutral form would smuggle
// one vendor's design into the core, so the neutral form has neither: a tool
// result is a *block*, and each adapter decides what message shape carries it.
// That choice is the whole reason this file uses blocks at all.

type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockThinking   BlockKind = "thinking"
	BlockToolCall   BlockKind = "tool_call"
	BlockToolResult BlockKind = "tool_result"
)

type Block struct {
	Kind BlockKind

	// Text carries the content of a text, thinking, or tool_result block.
	Text string

	// ID is the tool call's id — set on BlockToolCall, and on BlockToolResult
	// to say which call it answers.
	ID   string
	Name string // tool name, on BlockToolCall

	// Args is the tool call's arguments as a raw JSON string.
	//
	// A string, not a decoded map, and that is deliberate. One protocol sends
	// arguments as a JSON string, the other as a JSON object; the only form
	// that round-trips through both without re-serialising is the raw bytes.
	// Re-serialising would also break byte-level prompt caching, because
	// Go's map iteration order is not stable.
	Args string
}

type Msg struct {
	Role   Role
	Blocks []Block
}

// Convenience constructors, so the agent loop reads as prose.

func TextMsg(role Role, text string) Msg {
	return Msg{Role: role, Blocks: []Block{{Kind: BlockText, Text: text}}}
}

func ToolResultBlock(callID, content string) Block {
	return Block{Kind: BlockToolResult, ID: callID, Text: content}
}

// Text returns the concatenated text blocks, ignoring thinking and tools.
func (m Msg) Text() string {
	var s string
	for _, b := range m.Blocks {
		if b.Kind == BlockText {
			s += b.Text
		}
	}
	return s
}

// ToolCalls returns the tool-call blocks in order.
func (m Msg) ToolCalls() []Block {
	var out []Block
	for _, b := range m.Blocks {
		if b.Kind == BlockToolCall {
			out = append(out, b)
		}
	}
	return out
}

// Tool is a tool definition in neutral form. Each adapter renders it into its
// own schema envelope — one nests it under `function`, the other does not.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ---------------------------------------------------------------------------
// The neutral response
// ---------------------------------------------------------------------------

// StopReason is why generation stopped, normalised.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"   // the model finished talking
	StopToolUse   StopReason = "tool_use"   // it wants tools run
	StopMaxTokens StopReason = "max_tokens" // cut off
	StopFiltered  StopReason = "filtered"   // the provider blocked it
	StopUnknown   StopReason = "unknown"    // a string we have never seen
)

// CallResult is one model call, in the shape the agent loop understands.
type CallResult struct {
	Text     string
	Thinking string
	Calls    []Block // BlockToolCall, in the order the model emitted them
	Usage    Usage
	TTFT     time.Duration

	Stop StopReason

	// RawStop is the provider's literal string, kept alongside the normalised
	// value and written into the trace.
	//
	// This is not redundancy. On the gateway this repo was built against, a
	// tool call truncated at max_tokens comes back with stop_reason "tool_use"
	// and an unusable body (external/wire-notes.md §A3c) — the envelope lies. When
	// a session goes wrong, the normalised value tells you what the agent
	// believed and RawStop tells you what it was told, and the gap between them
	// is the bug. Never normalise away your only evidence.
	RawStop string
}

// ---------------------------------------------------------------------------
// The interface
// ---------------------------------------------------------------------------

// Provider is one protocol. Two implementations, ~350 lines each, and the agent
// loop cannot tell them apart.
type Provider interface {
	// Protocol names the wire format ("openai", "anthropic"), for display and
	// for the trace.
	Protocol() string

	// Model is the model id being called.
	Model() string

	// BuildRequest renders a conversation into an HTTP request. The system
	// prompt is passed separately because the protocols disagree about where it
	// belongs — a top-level field on one, the first message on the other — and
	// that disagreement must not reach the caller.
	// The context is the barrier stage 09 could not cross: a request built
	// without one cannot be cancelled, so no clock above it can reach the
	// call it starts.
	BuildRequest(ctx context.Context, system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error)

	// ParseStream consumes an SSE body, emitting events as they arrive, and
	// returns the assembled result. Both implementations use readSSE from
	// sse.go: the framing is shared, the payloads are not.
	ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error)
}

// normaliseStop maps a provider's literal stop string onto the neutral set.
//
// Unknown strings map to StopUnknown rather than to StopEndTurn, and the agent
// loop reports them instead of continuing. A state machine that maps anything
// unrecognised to "probably fine" will eventually map a refusal, a quota event,
// or a new safety stop to "probably fine".
func normaliseStop(raw string) StopReason {
	switch raw {
	case "stop", "end_turn":
		return StopEndTurn
	case "tool_calls", "tool_use":
		return StopToolUse
	case "length", "max_tokens":
		return StopMaxTokens
	case "content_filter", "refusal":
		return StopFiltered
	default:
		return StopUnknown
	}
}
