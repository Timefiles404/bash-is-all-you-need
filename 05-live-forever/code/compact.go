// Stage 05 — compaction.
//
// A context window is a wall, and an agent that runs long enough always hits
// it. Compaction is what happens then: throw away most of the transcript,
// replace it with a summary, keep going.
//
// The idea takes one sentence. Everything expensive about it is in the details,
// and this file is those details:
//
//   - You cannot cut anywhere. Cut between a tool call and its result and the
//     next request is malformed — an API error several turns later, whose
//     stack trace points at the request builder rather than at the cut.
//   - You cannot know when to cut without counting tokens, and counting tokens
//     needs a tokenizer, which is a dependency this repo does not have. So it
//     calibrates an estimate against the numbers the API already reports.
//   - The summary is itself a model call. It costs tokens and seconds, and an
//     implementation that hides it is lying about the session's cost.
//   - Compaction rewrites the prompt prefix, which destroys every cache entry
//     stage 04 spent a chapter earning. It is not free. It is a trade, and the
//     only way to know whether the trade paid is to measure both sides.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Counting tokens without a tokenizer
// ---------------------------------------------------------------------------

// estimator converts characters to tokens by calibrating against the token
// counts the API already reports.
//
// The usual advice is "vendor a tokenizer". For deciding *when to compact*
// that is the wrong tool: a tokenizer is a large dependency, it is per-model,
// it disagrees with the server about the framing overhead of tool schemas and
// message envelopes, and it tells you nothing you cannot get for free — because
// every single response already contains the exact token count of the prompt
// you just sent.
//
// So: send a prompt, note how many characters it was and how many tokens the
// server said it was, and you have a ratio. Do it every call and the ratio
// converges on this conversation's actual mix of prose, code and JSON.
//
// The subtle part, and the reason this works at all: **the estimate does not
// need to be accurate, it needs to be consistent.** It is only ever used to
// answer "are we near the wall yet", and it is calibrated against the same
// character count it is later asked to convert. A systematic bias — JSON
// envelope overhead, tool schemas, the system prompt — is absorbed into the
// ratio rather than accumulated as error. What would break it is measuring one
// thing and estimating another.
type estimator struct {
	ratio float64 // characters per token
	obs   int
}

// 3.6 is a reasonable cold start for a mix of English prose, code and JSON;
// pure English runs nearer 4.0 and dense JSON nearer 2.5. It matters only for
// the first call, after which measurement takes over.
func newEstimator() *estimator { return &estimator{ratio: 3.6} }

// observe records one real measurement: chars sent, tokens billed.
func (e *estimator) observe(chars, tokens int) {
	if chars <= 0 || tokens <= 0 {
		return
	}
	r := float64(chars) / float64(tokens)
	// A ratio outside this range means the two numbers are not measuring the
	// same request — a usage event arriving for a call whose character count we
	// never took, most likely. Dropping it is better than letting one bad
	// sample drag the estimate somewhere it takes ten calls to climb back from.
	if r < 1.0 || r > 20.0 {
		return
	}
	if e.obs == 0 {
		e.ratio = r
	} else {
		// Exponential moving average, weighted toward history. The ratio drifts
		// slowly and genuinely — a session that starts in prose and moves into
		// reading JSON files really does change — so it should track, but it
		// should not lurch on one unusual turn.
		e.ratio = 0.75*e.ratio + 0.25*r
	}
	e.obs++
}

func (e *estimator) tokens(chars int) int {
	if e.ratio <= 0 {
		return chars / 4
	}
	return int(float64(chars)/e.ratio + 0.5)
}

// msgChars counts the characters a message contributes to the prompt.
//
// It counts text, tool-call arguments and tool results — everything that is
// re-sent — and ignores structural JSON. Thinking blocks are not counted,
// because this repo drops them before sending: counting them here while not
// sending them there is exactly the asymmetry that would poison the
// calibration. See runTurn: thinking never enters the history.
func msgChars(m Msg) int {
	n := 0
	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText, BlockToolResult:
			n += len(b.Text)
		case BlockToolCall:
			n += len(b.Name) + len(b.Args)
		}
	}
	return n
}

func convChars(msgs []Msg) int {
	n := 0
	for _, m := range msgs {
		n += msgChars(m)
	}
	return n
}

// ---------------------------------------------------------------------------
// Where you are allowed to cut
// ---------------------------------------------------------------------------

// canCutBefore reports whether `[summary] + msgs[i:]` is a conversation the API
// will accept.
//
// Two conditions, and the first one is the bug that everybody ships:
//
//  1. **msgs[i] must not contain a tool result.** Its matching tool call lives
//     in msgs[i-1], which compaction is about to delete. A tool result whose
//     call is gone is an orphan, and both protocols reject it — OpenAI with
//     "messages with role 'tool' must be a response to a preceding message with
//     tool_calls", Anthropic with an unexpected `tool_use_id`. The failure
//     surfaces on the *next* request, so the traceback points at the request
//     builder and the actual mistake is a hundred lines away in the compactor.
//
//  2. **msgs[i] must be an assistant message.** The summary is injected as a
//     user message, so cutting before another user message produces two user
//     messages in a row. Some endpoints merge them, some reject them, and the
//     ones that merge do it differently from each other.
//
// Both conditions collapse into a rule that is easy to hold in your head:
// **a conversation may only be cut immediately before an assistant turn.**
// Assistant messages never carry tool results, so condition 2 implies
// condition 1 — but they are checked separately anyway, because the day
// somebody adds a new block kind is the day the implication stops holding and
// a single combined check would go on returning true.
func canCutBefore(msgs []Msg, i int) bool {
	if i <= 0 || i >= len(msgs) {
		return false
	}
	for _, b := range msgs[i].Blocks {
		if b.Kind == BlockToolResult {
			return false
		}
	}
	return msgs[i].Role == RoleAssistant
}

// safeCut returns the smallest legal cut index at or after want, or -1.
//
// It searches *forward* — toward dropping more — on purpose. Compaction is
// triggered because the window is nearly full, so the failure mode that must
// not happen is freeing less than intended and immediately needing to compact
// again. Searching backward would keep more recent context and would sometimes
// free nothing at all.
func safeCut(msgs []Msg, want int) int {
	if want < 1 {
		want = 1
	}
	for i := want; i < len(msgs); i++ {
		if canCutBefore(msgs, i) {
			return i
		}
	}
	return -1
}

// validConversation returns a description of the first structural problem in a
// message list, or "" if it is well-formed.
//
// This is deliberately an independent check rather than a restatement of
// canCutBefore. canCutBefore says where a cut is allowed; this says whether the
// result is actually sendable, derived from the protocol's rules rather than
// from the cutting logic. If the compactor is ever wrong, a check written from
// the same assumptions will agree with it. One written from the other end will
// not — which is the entire value of having it.
func validConversation(msgs []Msg) string {
	open := map[string]bool{}     // tool calls awaiting a result
	answered := map[string]bool{} // ids we have seen a result for
	for i, m := range msgs {
		if len(m.Blocks) == 0 {
			return fmt.Sprintf("message %d (%s) has no content blocks; the Anthropic protocol rejects an empty content array", i, m.Role)
		}
		if i > 0 && msgs[i-1].Role == m.Role {
			return fmt.Sprintf("messages %d and %d are both %s; roles must alternate", i-1, i, m.Role)
		}
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolCall:
				open[b.ID] = true
			case BlockToolResult:
				if !open[b.ID] {
					return fmt.Sprintf("message %d answers tool call %q, which no earlier message made — the call was cut away and its result left behind", i, b.ID)
				}
				delete(open, b.ID)
				answered[b.ID] = true
			}
		}
	}
	// An unanswered call in the *final* message is legal: that is the state a
	// conversation is in while the tools are still running. Anywhere else it is
	// the mirror image of the orphan-result bug, and it makes the model believe
	// a command it issued silently produced nothing.
	for i, m := range msgs[:max(0, len(msgs)-1)] {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolCall && !answered[b.ID] {
				return fmt.Sprintf("tool call %q in message %d is never answered", b.ID, i)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// The compactor
// ---------------------------------------------------------------------------

type compactor struct {
	window    int     // the model's context window, in tokens
	threshold float64 // compact when the estimated prompt passes this fraction of it
	keepRatio float64 // fraction of the window to leave in place afterwards
	maxTokens int     // max_tokens for the summarising call
	est       *estimator
	count     int // how many times this session has compacted
}

func newCompactor(window int, threshold, keepRatio float64) *compactor {
	return &compactor{
		window:    window,
		threshold: threshold,
		keepRatio: keepRatio,
		maxTokens: 2048,
		est:       newEstimator(),
	}
}

// due reports whether the estimated next prompt crosses the threshold.
//
// Note what it takes: an *estimate*, not the last reported usage. Using the
// last usage is the obvious implementation and it is one turn too late — the
// tool result that blows the window is already in the history by the time the
// call that would have reported it is being built. The whole point of the
// estimator is to answer the question before paying to find out.
func (c *compactor) due(estimated int) bool {
	if c.window <= 0 || c.threshold <= 0 {
		return false
	}
	return float64(estimated) >= c.threshold*float64(c.window)
}

// estimate converts a conversation plus its fixed overhead into tokens.
func (c *compactor) estimate(msgs []Msg, baseChars int) int {
	return c.est.tokens(convChars(msgs) + baseChars)
}

// plan chooses the cut index, or returns -1 with a reason.
//
// The budget is the number of tokens of *history* to leave behind. Messages are
// walked from the newest backward, accumulating, until the budget is spent;
// that index is the earliest we would like to keep, and safeCut then moves it
// forward to a legal boundary.
func (c *compactor) plan(msgs []Msg, baseChars int) (int, string) {
	if len(msgs) < 4 {
		return -1, "nothing to compact: the conversation is only " + fmt.Sprint(len(msgs)) + " messages"
	}
	budget := int(c.keepRatio * float64(c.window))
	if budget <= 0 {
		return -1, "keep budget is zero"
	}

	// Walk backward from the newest message. `want` ends as the index of the
	// oldest message that still fits inside the budget.
	kept, want := c.est.tokens(baseChars), len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		t := c.est.tokens(msgChars(msgs[i]))
		if kept+t > budget {
			break
		}
		kept += t
		want = i
	}

	// The floor. If the budget does not stretch past the newest message, there
	// is nothing useful to do: a summary plus one message is not a compaction.
	//
	// Two different things put you here, they need two different fixes, and the
	// first version of this code printed the first message for both. That is
	// worse than no message — an error that names the wrong flag sends the
	// reader to change a setting that was never the problem, and when it does
	// not help they conclude the diagnosis was right and the situation is
	// hopeless. An error message is a claim about causation; get it wrong and
	// you have not been unhelpful, you have been misleading.
	if want >= len(msgs)-1 {
		newest := c.est.tokens(msgChars(msgs[len(msgs)-1]))
		if newest > budget {
			return -1, fmt.Sprintf("cannot compact: the newest message alone is ~%d tokens against a keep budget of %d — lower --max-output or use a command that filters", newest, budget)
		}
		return -1, fmt.Sprintf("cannot compact: a keep budget of %d tokens has room for only the newest message (~%d) — raise --keep or --window", budget, newest)
	}

	cut := safeCut(msgs, want)
	if cut < 0 {
		return -1, "no legal cut point: every message from here on is a tool result or a user turn"
	}
	if cut >= len(msgs)-1 {
		return -1, "the only legal cut point would discard the whole conversation"
	}
	return cut, ""
}

// ---------------------------------------------------------------------------
// The summary
// ---------------------------------------------------------------------------

// The instruction given to the summarising call.
//
// The selection criterion in point 2 is the one worth stealing: **keep what
// would cost tool calls to rediscover.** That is an economic test, not a
// semantic one, and it is far easier for a model to apply than "keep what is
// important". A file path that took three greps to find is worth a line; a
// paragraph of the model's own narration is worth nothing, because regenerating
// it costs nothing.
const summarySystem = `You are compacting a coding-agent session transcript so the agent can continue in a smaller context window. You are not continuing the session and you are not answering the user.

Write a summary that preserves, under these headings:

1. GOAL — what the user asked for, in their words where possible, including anything they explicitly corrected, refused, or ruled out.
2. FACTS — everything discovered about this environment that would cost tool calls to rediscover: exact file paths, directory layouts, command output that mattered, version numbers, error messages verbatim, what was tried and failed.
3. DECISIONS — choices made and the reason for each, so they are not relitigated.
4. STATE — what the transcript shows was done, and what it shows was still outstanding at the point it ends.

Rules:
- You are reading only the EARLIER part of the session. More recent messages are being kept verbatim and will appear immediately after your summary, and you cannot see them. So never write that something was "never done", "not started" or "still outstanding" as a statement about the session — it may have happened in the part you cannot see. Say "as of the end of this transcript".
- Keep identifiers, paths, flags and error text EXACT. Never paraphrase a filename or a command.
- Drop narration, restatements, apologies, and anything the agent said about what it was about to do.
- If something is uncertain, say it is uncertain rather than resolving it.
- Prefer a longer FACTS section and a shorter everything else.
- Output the summary only. No preamble, no tool calls.`

// flatten renders the doomed part of the conversation as a transcript.
//
// The alternative — passing the real message array to the summarising call —
// looks more faithful and behaves worse: given a conversation, a model
// continues it. It answers the last question again, or issues the next tool
// call. Flattening changes the task from "converse" to "read this document",
// which is what we actually want, and it has two more benefits: it lets long
// tool output be truncated before it is paid for, and it makes the summariser
// call carry no tool definitions at all, so a tool call is not merely
// discouraged but impossible.
func flatten(msgs []Msg, maxBlock int) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case BlockText:
				fmt.Fprintf(&b, "[%s]\n%s\n\n", m.Role, clip(blk.Text, maxBlock))
			case BlockToolCall:
				cmd, err := parseBashArgs(blk.Args)
				if err != nil {
					cmd = blk.Args
				}
				fmt.Fprintf(&b, "[%s ran] %s\n", m.Role, clip(cmd, 400))
			case BlockToolResult:
				fmt.Fprintf(&b, "[output]\n%s\n\n", clip(blk.Text, maxBlock))
			}
		}
	}
	return b.String()
}

// clip shortens a string from the MIDDLE, keeping both ends.
//
// Head-truncation is the reflex and it is wrong for command output. A build log
// puts the error at the end; a stack trace puts the cause at the end; a diff
// puts the interesting hunk anywhere. Keeping the first 60% and the last 40%
// keeps whatever the command was announcing and whatever it concluded, and
// loses the repetitive middle, which is the part that was long.
func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := max * 6 / 10
	tail := max - head
	return s[:head] + fmt.Sprintf("\n… [%d characters omitted] …\n", len(s)-max) + s[len(s)-tail:]
}

// summaryMsg wraps the summary in the message that will replace the history.
//
// It is a user message, and it is tagged. The tag matters: without it the model
// treats a wall of past-tense text as something the user just typed, and
// answers it. With it, the model treats it as briefing material — which is what
// it is.
func summaryMsg(text string) Msg {
	return TextMsg(RoleUser, "<session-summary>\nThe earlier part of this session was compacted to fit the context window. This is the summary of what happened; treat it as established fact, not as a new request.\n\n"+
		strings.TrimSpace(text)+"\n</session-summary>")
}

// run performs one compaction: summarise msgs[:cut], return the new history.
//
// Every number this produces goes onto the bus. Compaction that does not report
// its own cost is how an agent ends up with a bill nobody can reconcile: the
// summarising call is a real model call, on the real model, billed at the real
// rate, and it is invisible in every implementation that treats compaction as
// an internal detail.
func (c *compactor) run(p Provider, httpc *http.Client, bus *Bus, msgs []Msg, cut int, baseChars int) ([]Msg, error) {
	before := c.estimate(msgs, baseChars)
	bus.Emit(Event{
		Kind: KindCompactStart, MsgsBefore: len(msgs), TokensBefore: before,
		Text: fmt.Sprintf("summarising messages 0–%d, keeping %d", cut-1, len(msgs)-cut),
	})

	transcript := flatten(msgs[:cut], 4000)

	// No tools. See flatten: the summariser is not an agent and must not be
	// able to act like one.
	req, body, err := p.BuildRequest(summarySystem,
		[]Msg{TextMsg(RoleUser, "Transcript to compact:\n\n"+transcript)},
		nil, c.maxTokens)
	if err != nil {
		return msgs, err
	}
	bus.Emit(Event{Kind: KindRequest, Request: body})

	started := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		return msgs, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return msgs, fmt.Errorf("compaction call failed: http %d", resp.StatusCode)
	}
	res, err := p.ParseStream(resp.Body, bus, 0, started)
	if err != nil {
		return msgs, err
	}
	if strings.TrimSpace(res.Text) == "" {
		// Refusing to proceed is the right call. A compaction that replaces the
		// history with nothing does not fail loudly — the agent simply forgets
		// everything and carries on sounding confident.
		return msgs, fmt.Errorf("the summarising call returned no text (stop: %s)", res.RawStop)
	}

	out := append([]Msg{summaryMsg(res.Text)}, msgs[cut:]...)
	c.count++

	after := c.estimate(out, baseChars)
	bus.Emit(Event{
		Kind: KindCompactEnd, Text: res.Text,
		MsgsBefore: len(msgs), MsgsAfter: len(out),
		TokensBefore: before, TokensAfter: after,
		Millis: time.Since(started).Milliseconds(),
	})

	// The bill for this comes due on the *next* call, not this one, and it
	// arrives as a number that looks like a regression. Say so at the moment it
	// is caused, not when it shows up.
	bus.Emit(Event{
		Kind:         KindCacheInvalidated,
		TokensBefore: before,
		Text:         "the prompt prefix was rewritten — every cache entry from before this point is now unreachable, and the next call is a full-price miss",
	})
	return out, nil
}
