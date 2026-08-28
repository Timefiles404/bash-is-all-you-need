// Stage 06 — three views of one session, and why they have to disagree.
//
// A trace holds two different stories and most tools show you only the first:
//
//	GOD   what happened. Every event, in order, with its timings, its token
//	      counts, its exit codes and its gate verdicts. Nothing is hidden,
//	      including the things that were never sent to the model.
//
//	MODEL what the model saw. Not a reconstruction — the *actual bytes*, decoded
//	      out of the request event stage 02 has been recording since it existed.
//
//	WIRE  those bytes, unmodified, for when the answer is in the punctuation.
//
// The reason to build all three is that the gap between the first two is where
// agent bugs live. Every one of these is invisible unless you can put the two
// side by side:
//
//   - The model reasoned for four hundred tokens and none of it is in the next
//     request, because thinking is dropped from the history.
//   - The user typed nine words and the model received nine words plus an
//     environment block it never mentioned.
//   - A tool printed 40kB and the model was given 8kB with a truncation marker.
//   - Thirty turns happened and the model can see three messages, because the
//     other twenty-seven were compacted into a paragraph.
//
// That last one is the whole reason this chapter follows stage 05. After a
// compaction, **the history that happened and the history the model has are
// different objects**, and an agent that starts behaving strangely is usually
// an agent whose model view no longer contains the thing you are asking it
// about. You cannot debug that from a chat log.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Decoding a recorded request
// ---------------------------------------------------------------------------

// wireBlock is one piece of content, in whichever protocol produced it.
type wireBlock struct {
	Kind   string // "text" | "thinking" | "tool_call" | "tool_result"
	Text   string
	ID     string
	Name   string
	Args   string
	Cached bool // this block carried a cache_control marker
}

type wireMsg struct {
	Role   string
	Blocks []wireBlock
}

// wireView is a request body, decoded far enough to read.
//
// Deliberately not built on the adapters' own structs. Those types describe
// what this agent *sends*; this one has to be able to read a trace recorded by
// a different build, or a hand-written request, or a protocol whose adapter was
// deleted three versions ago. A viewer that can only parse what its own encoder
// produces stops working exactly when you need it — after a change.
type wireView struct {
	Protocol   string
	Model      string
	MaxTokens  int
	System     []wireBlock
	Messages   []wireMsg
	Tools      []string
	Bytes      int
	CacheMarks int
	Err        string
}

// decodeRequest sniffs the protocol and decodes.
//
// The sniff is structural, not a version header: a top-level `system` key means
// the Anthropic shape, its absence means the OpenAI shape. That is exactly the
// difference stage 03 called DISAGREEMENT 1, and it is the most reliable
// discriminator precisely because it is the one thing neither protocol can
// imitate without becoming the other.
func decodeRequest(raw json.RawMessage) wireView {
	v := wireView{Bytes: len(raw)}
	var probe struct {
		Model     string          `json:"model"`
		MaxTokens int             `json:"max_tokens"`
		System    json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		v.Err = "not JSON: " + err.Error()
		return v
	}
	v.Model, v.MaxTokens = probe.Model, probe.MaxTokens
	if len(probe.System) > 0 {
		v.Protocol = "anthropic"
		viewAnthropicRequest(raw, &v)
	} else {
		v.Protocol = "openai"
		viewOpenAIRequest(raw, &v)
	}
	for _, b := range v.System {
		if b.Cached {
			v.CacheMarks++
		}
	}
	for _, m := range v.Messages {
		for _, b := range m.Blocks {
			if b.Cached {
				v.CacheMarks++
			}
		}
	}
	return v
}

func viewAnthropicRequest(raw json.RawMessage, v *wireView) {
	var body struct {
		System   []anthropicContent `json:"system"`
		Messages []struct {
			Role    string             `json:"role"`
			Content []anthropicContent `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		v.Err = err.Error()
		return
	}
	conv := func(c anthropicContent) wireBlock {
		b := wireBlock{Text: c.Text, ID: c.ID, Name: c.Name, Cached: c.CacheControl != nil}
		switch c.Type {
		case "tool_use":
			b.Kind, b.Args = "tool_call", string(c.Input)
		case "tool_result":
			b.Kind, b.ID, b.Text = "tool_result", c.ToolUseID, c.Content
		case "thinking":
			b.Kind = "thinking"
		default:
			b.Kind = "text"
		}
		return b
	}
	for _, c := range body.System {
		v.System = append(v.System, conv(c))
	}
	for _, m := range body.Messages {
		wm := wireMsg{Role: m.Role}
		for _, c := range m.Content {
			wm.Blocks = append(wm.Blocks, conv(c))
		}
		v.Messages = append(v.Messages, wm)
	}
	for _, t := range body.Tools {
		v.Tools = append(v.Tools, t.Name)
	}
}

func viewOpenAIRequest(raw json.RawMessage, v *wireView) {
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		v.Err = err.Error()
		return
	}
	for _, m := range body.Messages {
		// The system prompt is messages[0] on this protocol. Lifting it into
		// the System field is what lets the Model view render both protocols
		// with one function — and it is a small, honest lie about the wire,
		// which is why the Wire view exists next door.
		if m.Role == "system" && len(v.Messages) == 0 {
			v.System = append(v.System, wireBlock{Kind: "text", Text: m.Content})
			continue
		}
		wm := wireMsg{Role: m.Role}
		if m.Role == "tool" {
			wm.Role = "user"
			wm.Blocks = append(wm.Blocks, wireBlock{Kind: "tool_result", ID: m.ToolCallID, Text: m.Content})
		} else if m.Content != "" {
			wm.Blocks = append(wm.Blocks, wireBlock{Kind: "text", Text: m.Content})
		}
		for _, c := range m.ToolCalls {
			wm.Blocks = append(wm.Blocks, wireBlock{
				Kind: "tool_call", ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments,
			})
		}
		v.Messages = append(v.Messages, wm)
	}
	for _, t := range body.Tools {
		v.Tools = append(v.Tools, t.Function.Name)
	}
}

// ---------------------------------------------------------------------------
// The session index
// ---------------------------------------------------------------------------

// call is one model call: the request that started it and everything the
// session did until the next one.
type call struct {
	Seq     int
	Turn    int
	At      time.Time
	Request json.RawMessage
	Events  []Event // this call's slice of the stream, request included

	Usage      *Usage
	Compaction bool // this call was the summariser, not the agent
}

type session struct {
	Path   string
	Events []Event
	Calls  []call
	Start  time.Time

	// Display is Events with delta runs merged. A streamed response is a
	// thousand text_delta events of four characters each, and a viewer that
	// renders one row per event is a viewer nobody can scroll. Collapsing is
	// done ONCE, here, and every part of the God view — rendering, clicking,
	// scroll positions — reads this slice, because a line index that means one
	// thing to the renderer and another to the click handler is a bug that only
	// shows up when someone uses the mouse.
	Display []Event

	Total       Usage
	Compactions int
}

// indexSession slices the flat event stream into calls.
//
// Every call begins at a KindRequest, which is the only event guaranteed to
// exist for one — a call that died before its first token still has one. Anchor
// the index on something the failure path also produces, or your viewer goes
// blank precisely on the sessions worth viewing.
func indexSession(path string, events []Event) *session {
	s := &session{Path: path, Events: events}
	if len(events) > 0 {
		s.Start = events[0].T
	}
	inCompaction := false
	for i, e := range events {
		switch e.Kind {
		case KindCompactStart:
			inCompaction = true
		case KindCompactEnd:
			inCompaction = false
			s.Compactions++
		case KindRequest:
			s.Calls = append(s.Calls, call{
				Seq: e.Seq, Turn: e.Turn, At: e.T, Request: e.Request,
				Compaction: inCompaction,
			})
		case KindUsage:
			if e.Usage != nil {
				s.Total = addUsage(s.Total, *e.Usage)
				if n := len(s.Calls); n > 0 {
					s.Calls[n-1].Usage = e.Usage
				}
			}
		}
		if n := len(s.Calls); n > 0 {
			s.Calls[n-1].Events = append(s.Calls[n-1].Events, events[i])
		}
	}
	s.Display = collapseDeltas(events)
	return s
}

// collapseDeltas merges each run of same-kind streaming deltas into one event
// whose Text is the joined text and whose Bytes is how many arrived.
//
// The merged event keeps the Seq of the FIRST delta in the run. That choice
// matters for the click handler: clicking a collapsed row should select the
// call the run started in, and a run that straddles a boundary — which happens
// when a response ends and the next request begins — would otherwise jump you
// forward a call.
func collapseDeltas(events []Event) []Event {
	streaming := func(k Kind) bool {
		return k == KindTextDelta || k == KindReasoningDelta || k == KindToolArgsDelta
	}
	var out []Event
	for i := 0; i < len(events); i++ {
		e := events[i]
		if !streaming(e.Kind) {
			out = append(out, e)
			continue
		}
		var b strings.Builder
		n := 0
		for ; i < len(events) && events[i].Kind == e.Kind; i++ {
			b.WriteString(events[i].Text)
			n++
		}
		i--
		e.Text = b.String()
		e.Bytes = n
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// Rendering. Every view returns plain lines; the TUI does the scrolling.
// ---------------------------------------------------------------------------

const (
	sDim  = "\x1b[2m"
	sBold = "\x1b[1m"
	sOff  = "\x1b[0m"
	sUser = "\x1b[36m"
	sAsst = "\x1b[32m"
	sWarn = "\x1b[33m"
	sBad  = "\x1b[31m"
	sSys  = "\x1b[35m"
	sSel  = "\x1b[7m" // reverse video for the selected row
)

func dim(s string) string  { return sDim + s + sOff }
func bold(s string) string { return sBold + s + sOff }

// godView renders the whole event stream.
func (s *session) godView(w int, selSeq int) ([]string, int) {
	var out []string
	selLine := 0
	for _, e := range s.Display {
		if e.Seq == selSeq {
			selLine = len(out)
		}
		out = append(out, s.godLine(e, w)...)
	}
	return out, selLine
}

func (s *session) godLine(e Event, w int) []string {
	off := e.T.Sub(s.Start).Seconds()
	head := fmt.Sprintf("%5d %7.2fs ", e.Seq, off)

	// Depth gutter. This is what the God view is FOR once agents nest: the
	// terminal renderer had to give up on interleaving concurrent children
	// (see render.go), and a scrollable list does not, because it is not
	// competing for one cursor. Everything the plain output dropped is here.
	gutter := strings.Repeat("│ ", e.Depth)

	line := func(style, kind, rest string) []string {
		return []string{dim(head) + dim(gutter) + style + fmt.Sprintf("%-16s", kind) + sOff + " " + rest}
	}

	switch e.Kind {
	case KindUserMessage:
		return line(sUser, "user", bold(oneLine(e.Text, w-32)))
	case KindTurnStart:
		return line(sDim, "turn_start", dim(fmt.Sprintf("turn %d", e.Turn)))
	case KindToolCallStart:
		// Without its own case this rendered blank: the payload is in ToolID and
		// ToolName, not in Text. It earns a row because the gap between it and
		// tool_call_ready is where the arguments streamed — a long gap here means
		// the model spent real time writing one command.
		return line(sDim, "tool_call_start", dim(e.ToolName+"  "+e.ToolID))

	case KindTurnEnd:
		return line(sDim, "turn_end", dim("turn "+fmt.Sprint(e.Turn)))

	case KindRequest:
		v := decodeRequest(e.Request)
		return line(sBold, "request", dim(fmt.Sprintf("%s · %d messages · %d cache marks · %s",
			v.Protocol, len(v.Messages), v.CacheMarks, humanBytes(len(e.Request)))))
	case KindFirstToken:
		return line(sDim, "first_token", dim(fmt.Sprintf("TTFT %dms", e.Millis)))
	case KindTextDelta, KindReasoningDelta, KindToolArgsDelta:
		// A collapsed run. Both numbers are shown — how many frames arrived and
		// how much text they carried — because their ratio is the shape of the
		// stream, and a provider that suddenly sends one delta per token
		// instead of one per chunk is visible here and nowhere else.
		st := sDim
		if e.Kind == KindTextDelta {
			st = ""
		}
		tag := fmt.Sprintf("%s ×%d", e.Kind, e.Bytes)
		return line(st, tag, dim(oneLine(e.Text, w-32)))
	case KindToolCallReady:
		return line(sAsst, "tool_call", "$ "+oneLine(e.Command, w-34))
	case KindCommandStart:
		// Without its own case this fell through to the default, which prints
		// e.Text — and a command_start carries its payload in e.Command, so the
		// row rendered blank. A viewer with a silently empty row is worse than
		// one that omits the event: it looks like something happened that has
		// no description.
		return line(sDim, "command_start", dim(oneLine(e.Command, w-34)))

	case KindGateVerdict:
		st := sDim
		if e.Verdict != "allow" {
			st = sWarn
		}
		why := ""
		if e.Text != "" {
			why = " " + dim(e.Text)
		}
		return line(st, "gate", e.Verdict+why)
	case KindCommandEnd:
		st := sDim
		if e.ExitCode != 0 || e.TimedOut {
			st = sBad
		}
		extra := ""
		if e.Truncated {
			extra = sWarn + " TRUNCATED" + sOff
		}
		return line(st, "command_end", dim(fmt.Sprintf("exit %d · %dms · %s", e.ExitCode, e.Millis, humanBytes(e.Bytes)))+extra)
	case KindToolResult:
		return line(sDim, "tool_result", dim(fmt.Sprintf("%s to model", humanBytes(len(e.Text)))))
	case KindUsage:
		if e.Usage == nil {
			return nil
		}
		u := *e.Usage
		return line(sBold, "usage", dim(fmt.Sprintf("prompt %d (full %d · write %d · read %d) · out %d",
			u.Prompt(), u.Input, u.CacheWrite, u.CacheRead, u.Output)))
	case KindResponseEnd:
		return line(sDim, "response_end", dim(fmt.Sprintf("%s · %dms", e.FinishReason, e.Millis)))
	case KindCompactStart:
		return line(sWarn, "COMPACT_START", sWarn+fmt.Sprintf("%d messages, ~%d tokens — %s", e.MsgsBefore, e.TokensBefore, e.Text)+sOff)
	case KindCompactEnd:
		return line(sWarn, "COMPACT_END", sWarn+fmt.Sprintf("%d → %d messages · ~%d → ~%d tokens · %dms",
			e.MsgsBefore, e.MsgsAfter, e.TokensBefore, e.TokensAfter, e.Millis)+sOff)
	case KindCacheInvalidated:
		return line(sBad, "cache_lost", sBad+oneLine(e.Text, w-34)+sOff)
	case KindSubagentStart:
		return line(sUser, "SUBAGENT →", bold(e.ToolName)+"  "+dim(oneLine(e.Text, w-40)))

	case KindSubagentEnd:
		u := Usage{}
		if e.Usage != nil {
			u = *e.Usage
		}
		// Spent next to returned, because that ratio is the reason the subagent
		// existed and it is not visible anywhere else in the trace.
		return line(sUser, "SUBAGENT ←", dim(fmt.Sprintf("%s · %d turns · %d prompt + %d out · %dms → %s returned",
			e.ToolName, e.Turn, u.Prompt(), u.Output, e.Millis, humanBytes(e.Bytes))))

	case KindSkillsIndexed:
		return line(sSys, "skills", dim(fmt.Sprintf("%s · index %s per request · %s of bodies on disk",
			e.Text, humanBytes(e.Bytes), humanBytes(e.TokensBefore))))

	case KindMemoryLoaded:
		return line(sSys, "memory", dim(fmt.Sprintf("%s (%s)", e.Path, humanBytes(e.Bytes))))
	// Stage 09. The verdict shares the row with the failure, because in a trace
	// you are always reading backwards from a symptom, and the question is never
	// "what broke" on its own - it is "what did it do about it".
	case KindCallError:
		style := sWarn
		if e.Triage == string(TriageFatal) {
			style = sBad
		}
		return line(style, "call_error", fmt.Sprintf("%s%s%s attempt %d · %s",
			style, strings.ToUpper(e.Triage), sOff, e.Attempt, oneLine(e.Text, w-52)))

	case KindRetry:
		return line(sDim, "retry", dim(fmt.Sprintf("wait %dms · attempt %d · %s",
			e.Millis, e.Attempt, oneLine(e.Text, w-52))))

	case KindProvider:
		if e.Provider == nil {
			return line(sSys, "provider", dim("(no provider recorded)"))
		}
		why := e.Text
		if why == "" {
			why = "session start"
		}
		return line(sSys, "provider", fmt.Sprintf("%s (%s · %s) · %s",
			bold(e.Provider.Name), e.Provider.Protocol, e.Provider.Model,
			dim(oneLine(why, w-52))))

	case KindNotice:
		return line(sWarn, "notice", e.Text)
	case KindError:
		return line(sBad, "error", sBad+e.Text+sOff)
	}
	return line(sDim, string(e.Kind), dim(oneLine(e.Text, w-32)))
}

// modelView renders what the model saw on one call.
//
// The header is the point of the whole chapter: it puts "events so far" next to
// "messages the model can see" on the same line. Before a compaction those
// numbers rise together. After one, they part company permanently, and that
// divergence is the single most useful number in a long agent session.
func (s *session) modelView(idx, w int) []string {
	if idx < 0 || idx >= len(s.Calls) {
		return []string{dim("  no calls in this trace")}
	}
	c := s.Calls[idx]
	v := decodeRequest(c.Request)

	eventsSoFar := 0
	for _, e := range s.Events {
		if e.Seq <= c.Seq {
			eventsSoFar++
		}
	}
	compactionsBefore := 0
	for _, e := range s.Events {
		if e.Seq < c.Seq && e.Kind == KindCompactEnd {
			compactionsBefore++
		}
	}

	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }

	title := fmt.Sprintf("call %d of %d", idx+1, len(s.Calls))
	if c.Compaction {
		title += sWarn + "  [the summarising call, not the agent]" + sOff
	}
	add("  %s   %s", bold(title), dim(fmt.Sprintf("%s · %s · max_tokens %d · %s",
		v.Protocol, v.Model, v.MaxTokens, humanBytes(v.Bytes))))
	add("  %s", dim(fmt.Sprintf("%d events happened so far · the model can see %d messages · %d cache marks · tools: %s",
		eventsSoFar, len(v.Messages), v.CacheMarks, strings.Join(v.Tools, ","))))
	if compactionsBefore > 0 {
		add("  %s", sWarn+fmt.Sprintf("⚠ %d compaction(s) happened before this call: everything below is what SURVIVED, not what happened", compactionsBefore)+sOff)
	}
	if v.Err != "" {
		add("  %s", sBad+"could not decode this request: "+v.Err+sOff)
		return out
	}
	add("")

	if len(v.System) > 0 {
		n := 0
		for _, b := range v.System {
			n += len(b.Text)
		}
		add("  %s %s", sSys+bold("SYSTEM")+sOff, dim(fmt.Sprintf("%d chars", n)))
		for _, b := range v.System {
			out = append(out, blockLines(b, w, "  │ ")...)
			if b.Cached {
				add("  %s", sWarn+"└──◀ cache breakpoint"+sOff)
			}
		}
		add("")
	}

	for i, m := range v.Messages {
		style := sUser
		if m.Role == "assistant" {
			style = sAsst
		}
		add("  %s %s", style+bold(fmt.Sprintf("[%d] %s", i+1, m.Role))+sOff,
			dim(fmt.Sprintf("%d blocks", len(m.Blocks))))
		for _, b := range m.Blocks {
			out = append(out, blockLines(b, w, "  │ ")...)
			if b.Cached {
				add("  %s", sWarn+"└──◀ cache breakpoint (rolling)"+sOff)
			}
		}
		add("")
	}
	return out
}

// blockLines renders one content block, wrapped to the window.
func blockLines(b wireBlock, w int, prefix string) []string {
	var out []string
	body := func(s string) {
		for _, l := range wrapCols(strings.TrimRight(s, "\n"), max(20, w-len(prefix)-2)) {
			out = append(out, dim(prefix)+l)
		}
	}
	switch b.Kind {
	case "tool_call":
		cmd, err := parseBashArgs(b.Args)
		if err != nil {
			cmd = b.Args
		}
		out = append(out, dim(prefix)+sAsst+"→ "+b.Name+sOff+"  "+dim(b.ID))
		body("$ " + cmd)
	case "tool_result":
		out = append(out, dim(prefix)+sUser+"← result"+sOff+"  "+dim(b.ID))
		body(b.Text)
	case "thinking":
		out = append(out, dim(prefix)+dim("· thinking"))
		body(b.Text)
	default:
		body(b.Text)
	}
	return out
}

// wireLines pretty-prints the raw request body.
func (s *session) wireView(idx, w int) []string {
	if idx < 0 || idx >= len(s.Calls) {
		return []string{dim("  no calls in this trace")}
	}
	c := s.Calls[idx]
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, c.Request, "", "  "); err != nil {
		return []string{sBad + "not valid JSON: " + err.Error() + sOff, string(c.Request)}
	}
	out := []string{
		"  " + bold(fmt.Sprintf("call %d of %d", idx+1, len(s.Calls))) + "   " +
			dim(fmt.Sprintf("%s on the wire, exactly as sent", humanBytes(len(c.Request)))),
		"",
	}
	for _, l := range strings.Split(pretty.String(), "\n") {
		// Wrapped, not truncated: a 30kB system prompt on one line is the
		// commonest thing you want to read in this view, and a viewer that cuts
		// it at the window edge is a viewer that hides the answer.
		out = append(out, wrapCols("  "+l, max(20, w-2))...)
	}
	return out
}

// oneLine flattens whitespace so a multi-line value fits one row of the God
// view. Newlines become ⏎ rather than disappearing, because "this string
// contained newlines" is frequently the bug.
func oneLine(s string, w int) string {
	s = strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", dim("⏎ "))
	if w < 8 {
		w = 8
	}
	return truncCols(s, w)
}
