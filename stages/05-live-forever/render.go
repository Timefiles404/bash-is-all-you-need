// Stage 02 — the instrument panel.
//
// This is a Subscriber and nothing else. It has no access to the agent, no
// knowledge of HTTP, and no clock of its own: every number it prints arrives in
// an Event. That constraint is not tidiness, it is the feature — it is exactly
// why `replay` can reproduce a session down to the millisecond without a
// network, and why what you saw live and what you read back are guaranteed to
// be the same thing.
//
// If you catch yourself wanting `time.Now()` in this file, the number you want
// belongs in an event.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// prices are per million tokens. Zero means "unknown", and unknown prints as a
// dash rather than as $0.00 — a made-up zero is worse than no number, because
// it is the number people quote.
type prices struct {
	in, out, cacheRead, cacheWrite float64
}

func (p prices) known() bool {
	return p.in > 0 || p.out > 0 || p.cacheRead > 0 || p.cacheWrite > 0
}

// cost prices one call. The three input rates are separate because they differ
// by an order of magnitude: cache reads are ~0.1x the base rate and cache
// writes ~1.25x, so a session that looks expensive by token count can be cheap,
// and vice versa. Collapsing them into one "input" number is the single most
// common way agent cost reporting goes wrong.
func (p prices) cost(u Usage) float64 {
	m := func(tok int, rate float64) float64 { return float64(tok) * rate / 1e6 }
	return m(u.Input, p.in) + m(u.CacheWrite, p.cacheWrite) + m(u.CacheRead, p.cacheRead) + m(u.Output, p.out)
}

type renderer struct {
	out    io.Writer
	color  bool
	prices prices
	window int // model context window, for the watermark; 0 = unknown

	// Session totals. These are the numbers nobody can answer about their own
	// agent, which is the reason this file exists.
	session     Usage
	sessionCost float64
	calls       int
	commands    int

	// showRequest turns on the request inspector: the full JSON body, printed
	// before each call. It is off by default because it is enormous, and worth
	// turning on the first time a model does something inexplicable — nine
	// times in ten the answer is that the prompt did not contain what you
	// assumed it contained.
	showRequest bool

	// Per-call streaming state.
	ttft      int64
	openBlock string // "text" | "reasoning" | "" — what we are mid-stream of
	sawOutput bool

	// Compaction state. inCompaction re-routes the summariser's streamed text
	// into a marked gutter, so a paragraph the model wrote *about* the session
	// can never be mistaken for a paragraph it wrote *in* the session.
	inCompaction bool
	compactions  int
	saved        int // tokens compaction has removed from the prompt, cumulative

	// Bytes and tokens seen on the wire, for the live characters-per-token
	// ratio. This is the whole answer to "how do I count tokens without a
	// tokenizer": every response tells you the exact token count of the request
	// you just sent, and you know how many bytes that request was.
	wireBytes  int
	wireTokens int

	// lastUsage latches whatever the most recent KindUsage carried.
	//
	// This exists because of a real integration bug. Usage and the end of the
	// response are two different events, emitted by the same component but not
	// at the same moment, and the first version of this renderer read usage
	// only off KindResponseEnd — which produced a panel full of zeroes. A
	// renderer should not care which event a number rode in on; it should
	// remember the last value it was told and use that.
	lastUsage Usage
}

func newRenderer(out io.Writer, color bool, p prices, window int) *renderer {
	return &renderer{out: out, color: color, prices: p, window: window}
}

// ---------------------------------------------------------------------------
// Colour. Deliberately tiny: four semantic roles, no theme system.
// ---------------------------------------------------------------------------

const (
	cReset = "\x1b[0m"
	cDim   = "\x1b[2m"
	cCmd   = "\x1b[36m" // cyan: things the agent did
	cWarn  = "\x1b[33m"
	cErr   = "\x1b[31m"
	cFull  = "\x1b[31m" // red: tokens billed at full price
	cWrite = "\x1b[33m" // yellow: cache writes, ~1.25x
	cRead  = "\x1b[32m" // green: cache reads, ~0.1x
)

func (r *renderer) c(code, s string) string {
	if !r.color {
		return s
	}
	return code + s + cReset
}

func (r *renderer) p(format string, args ...any) {
	fmt.Fprintf(r.out, format, args...)
}

// closeBlock ends whatever streaming region is open. Streaming means text
// arrives without newlines at predictable places, so the renderer — not the
// model — owns the layout.
func (r *renderer) closeBlock() {
	if r.openBlock != "" {
		r.p("\n")
		r.openBlock = ""
	}
}

// ---------------------------------------------------------------------------
// The Subscriber implementation. One switch; each case is a screen decision.
// ---------------------------------------------------------------------------

func (r *renderer) OnEvent(e Event) {
	switch e.Kind {

	case KindUserMessage:
		r.p("\n%s %s\n", r.c(cDim, "you >"), e.Text)

	case KindTurnStart:
		r.ttft, r.sawOutput = 0, false

	case KindRequest:
		r.wireBytes += len(e.Request)
		if r.showRequest {
			var pretty bytes.Buffer
			if json.Indent(&pretty, e.Request, "  │ ", "  ") == nil {
				r.p("\n  %s\n  │ %s\n", r.c(cDim, "┌─ request ─────────"), pretty.String())
			}
			r.p("  %s\n", r.c(cDim, fmt.Sprintf("└─ %s", humanBytes(len(e.Request)))))
		}

	case KindFirstToken:
		r.ttft = e.Millis

	case KindReasoningDelta:
		// Thinking is shown, dimmed and marked. Hiding it is the default in most
		// products and the wrong default here: a student who cannot see the
		// model reasoning cannot tell a bad plan from a bad tool.
		if r.openBlock != "reasoning" {
			r.closeBlock()
			r.p("%s ", r.c(cDim, "\n  ·"))
			r.openBlock = "reasoning"
		}
		r.p("%s", r.c(cDim, e.Text))
		r.sawOutput = true

	case KindTextDelta:
		if r.inCompaction {
			// The summary is shown, not hidden. A compaction you cannot read is
			// a compaction you cannot debug, and "the agent forgot something"
			// is almost always "the summary dropped it" — which you can only
			// find out by having watched the summary go by.
			if r.openBlock != "compact" {
				r.closeBlock()
				r.p("  %s ", r.c(cDim, "\n  ≡"))
				r.openBlock = "compact"
			}
			r.p("%s", r.c(cDim, e.Text))
			return
		}
		if r.openBlock != "text" {
			r.closeBlock()
			r.p("\n")
			r.openBlock = "text"
		}
		r.p("%s", e.Text)
		r.sawOutput = true

	case KindToolCallStart:
		r.closeBlock()

	case KindToolCallReady:
		r.p("\n%s %s\n", r.c(cCmd, "  $"), e.Command)

	case KindGateVerdict:
		if e.Verdict != "allow" {
			r.p("  %s\n", r.c(cWarn, "["+e.Verdict+"] "+e.Text))
		}

	case KindCommandEnd:
		// Counted, not printed. The tool result that follows already ends with
		// the exit code and duration, because that text was written for the
		// model — and showing you a different summary than the model got is
		// exactly the kind of divergence this stage exists to eliminate.
		r.commands++

	case KindToolResult:
		if strings.TrimSpace(e.Text) != "" {
			r.p("%s\n", r.c(cDim, indentLines(e.Text)))
		}

	case KindUsage:
		if e.Usage != nil {
			r.calls++
			r.wireTokens += e.Usage.Prompt()
			r.lastUsage = *e.Usage
			r.session = addUsage(r.session, *e.Usage)
			r.sessionCost += r.prices.cost(*e.Usage)
		}

	case KindResponseEnd:
		r.closeBlock()
		r.renderPanel(e)

	case KindMemoryLoaded:
		r.p("  %s\n", r.c(cDim, fmt.Sprintf("≡ memory: %s (%s)", e.Path, humanBytes(e.Bytes))))

	case KindCompactStart:
		r.closeBlock()
		r.inCompaction = true
		r.p("\n  %s\n", r.c(cWarn, fmt.Sprintf("≡ compacting: %d messages, ~%d tokens — %s",
			e.MsgsBefore, e.TokensBefore, e.Text)))

	case KindCompactEnd:
		r.closeBlock()
		r.inCompaction = false
		r.compactions++
		r.saved += e.TokensBefore - e.TokensAfter
		pct := 0.0
		if e.TokensBefore > 0 {
			pct = float64(e.TokensBefore-e.TokensAfter) * 100 / float64(e.TokensBefore)
		}
		r.p("  %s\n", r.c(cWarn, fmt.Sprintf("≡ compacted: %d → %d messages · ~%d → ~%d tokens (-%.0f%%) · %dms",
			e.MsgsBefore, e.MsgsAfter, e.TokensBefore, e.TokensAfter, pct, e.Millis)))

	case KindCacheInvalidated:
		// Printed at the moment it is caused, not when it shows up. The cost
		// lands on the NEXT call, as a bar that suddenly goes red, and without
		// this line that looks like a regression rather than a consequence.
		r.p("  %s\n", r.c(cErr, "! "+e.Text))

	case KindNotice:
		r.p("  %s\n", r.c(cWarn, e.Text))

	case KindError:
		r.closeBlock()
		r.p("  %s\n", r.c(cErr, "error: "+e.Text))
	}
}

func (r *renderer) commandFooter(e Event) string {
	var parts []string
	switch {
	case e.TimedOut:
		parts = append(parts, r.c(cErr, "TIMED OUT"))
	default:
		code := fmt.Sprintf("exit %d", e.ExitCode)
		if e.ExitCode != 0 {
			code = r.c(cWarn, code)
		}
		parts = append(parts, code)
	}
	parts = append(parts, fmt.Sprintf("%dms", e.Millis), humanBytes(e.Bytes))
	if e.Truncated {
		parts = append(parts, r.c(cWarn, "truncated"))
	}
	return r.c(cDim, "  └ "+strings.Join(parts, " · "))
}

// renderPanel is the per-call instrument readout, and the reason anyone should
// read this repo. Three questions it answers that a normal agent cannot:
//
//	Where did the prompt tokens go?  → the full / write / read split
//	How fast was it, really?         → TTFT separated from throughput
//	What did that cost?              → this call, and the session so far
func (r *renderer) renderPanel(e Event) {
	u := e.Usage
	if u == nil {
		u = &r.lastUsage // see lastUsage: usage rides on its own event
	}
	prompt := u.Prompt()

	label := "  ┌─ call " + fmt.Sprint(r.calls) + " · " + e.FinishReason
	if r.inCompaction {
		// The summariser is a real call on the real model at the real price.
		// Every implementation that treats compaction as an internal detail
		// leaves this call out of its own accounting, and then cannot explain
		// its bill. Labelled, counted, and in the ledger with everything else.
		label += " · COMPACTION"
	}
	r.p("\n%s\n", r.c(cDim, label))

	// Line 1 — where the prompt tokens went. The bar is the whole point: three
	// colours in the proportions you are actually being billed in.
	bar := r.cacheBar(*u)
	r.p("  %s in %s %s  %s\n",
		r.c(cDim, "│"),
		pad(fmt.Sprint(prompt), 6),
		bar,
		r.c(cDim, fmt.Sprintf("full %d · write %d · read %d%s", u.Input, u.CacheWrite, u.CacheRead, hitRate(*u))))

	// Line 2 — output and speed. TTFT and tokens/sec are separate numbers
	// because they fail separately: a slow first token is a queue or a long
	// prompt, slow throughput is the model itself.
	speed := ""
	if gen := e.Millis - r.ttft; gen > 0 && u.Output > 0 {
		speed = fmt.Sprintf(" · %.1f tok/s", float64(u.Output)*1000/float64(gen))
	}
	think := ""
	if u.Reasoning > 0 {
		think = fmt.Sprintf(" (think %d)", u.Reasoning)
	}
	r.p("  %s out %s %s\n",
		r.c(cDim, "│"),
		pad(fmt.Sprint(u.Output)+think, 6),
		r.c(cDim, fmt.Sprintf("TTFT %dms · total %dms%s", r.ttft, e.Millis, speed)))

	// Line 3 — money, or an honest dash.
	if r.prices.known() {
		r.p("  %s $%s  %s\n",
			r.c(cDim, "│"),
			pad(fmt.Sprintf("%.6f", r.prices.cost(*u)), 10),
			r.c(cDim, fmt.Sprintf("session $%.6f over %d calls", r.sessionCost, r.calls)))
	} else {
		r.p("  %s %s\n", r.c(cDim, "│"), r.c(cDim, "cost — (set --price-in/--price-out to price this run)"))
	}

	// Line 4 — how full the context is. This is the number that decides when
	// stage 05's compaction has to fire.
	ctx := fmt.Sprintf("context %d tokens", prompt)
	if r.window > 0 {
		ctx = fmt.Sprintf("context %d / %d (%.1f%%)", prompt, r.window, float64(prompt)*100/float64(r.window))
	}
	// The measured bytes-per-token of this session, which is the number that
	// makes a tokenizer unnecessary for deciding when to compact. It is printed
	// because watching it settle — 3.1 while reading JSON, 4.2 in prose — is
	// the fastest way to understand why a fixed divisor is a bad estimator and
	// a calibrated one is a fine one.
	if r.wireTokens > 0 {
		ctx += fmt.Sprintf(" · ≈%.1f B/tok", float64(r.wireBytes)/float64(r.wireTokens))
	}
	r.p("  %s %s\n", r.c(cDim, "└"), r.c(cDim, ctx))
}

// cacheBar draws the prompt split as twenty cells.
//
// A table of three numbers is readable; a bar is *glanceable*, and the thing
// you want to notice is a change in proportion between turns. When the green
// suddenly disappears, something invalidated your cache — and you want to see
// that on the turn it happens, not in a bill at the end of the month.
func (r *renderer) cacheBar(u Usage) string {
	const width = 20
	total := u.Prompt()
	if total == 0 {
		return r.c(cDim, strings.Repeat("·", width))
	}
	cells := func(n int) int {
		if n == 0 {
			return 0
		}
		c := n * width / total
		if c == 0 {
			c = 1 // never let a non-zero component render as nothing
		}
		return c
	}
	full, write, read := cells(u.Input), cells(u.CacheWrite), cells(u.CacheRead)
	for full+write+read > width && full > 0 {
		full--
	}
	pad := width - full - write - read

	// Three different GLYPHS, not just three colours. The bar has to survive
	// `| grep`, a file, a CI log and a colour-blind reader — all of which are
	// how people actually look at agent output. A chart that only works in a
	// colour terminal is a chart that is blank exactly when someone is trying
	// to show you a problem.
	return r.c(cFull, strings.Repeat("█", full)) +
		r.c(cWrite, strings.Repeat("▓", write)) +
		r.c(cRead, strings.Repeat("░", read)) +
		strings.Repeat(" ", max(0, pad))
}

// SessionSummary prints the totals. The line that matters is the last one:
// tokens billed versus the size of the conversation that produced them. Stage
// 00's docs recorded that ratio at 4.2x with no caching; this is where you
// watch it move.
func (r *renderer) SessionSummary(finalPrompt int) {
	r.p("\n%s\n", r.c(cDim, "  ── session ──────────────────────"))
	r.p("  %d calls · %d commands\n", r.calls, r.commands)
	r.p("  prompt tokens billed: %d  (full %d · write %d · read %d)\n",
		r.session.Prompt(), r.session.Input, r.session.CacheWrite, r.session.CacheRead)
	r.p("  output tokens: %d\n", r.session.Output)
	if r.prices.known() {
		r.p("  cost: $%.6f\n", r.sessionCost)
	}
	if r.compactions > 0 {
		r.p("  compactions: %d · ~%d tokens removed from the prompt\n", r.compactions, r.saved)
	}
	if finalPrompt > 0 {
		r.p("  %s\n", r.c(cDim, fmt.Sprintf("re-send ratio: %.1fx (billed %d for a final context of %d)",
			float64(r.session.Prompt())/float64(finalPrompt), r.session.Prompt(), finalPrompt)))
	}
}

// ---------------------------------------------------------------------------

// hitRate reports what fraction of the prompt was served from cache.
//
// The denominator is Prompt(), never Input. On a warm call Input is only the
// uncached remainder — 18 tokens against a 17,985-token prompt in a measured
// run — so dividing by it reports a hit rate of 99.9% whether caching is
// working or not. Compute a rate against the total you were actually billed
// for, or you have built a dashboard that cannot show you a regression.
func hitRate(u Usage) string {
	total := u.Prompt()
	if total == 0 || u.CacheRead == 0 {
		return ""
	}
	return fmt.Sprintf("  %.0f%% cached", float64(u.CacheRead)*100/float64(total))
}

func addUsage(a, b Usage) Usage {
	return Usage{
		Input:      a.Input + b.Input,
		CacheWrite: a.CacheWrite + b.CacheWrite,
		CacheRead:  a.CacheRead + b.CacheRead,
		Output:     a.Output + b.Output,
		Reasoning:  a.Reasoning + b.Reasoning,
	}
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func indentLines(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  │ " + strings.ReplaceAll(s, "\n", "\n  │ ")
}

// colorEnabled reports whether to emit ANSI. Honour NO_COLOR (an actual
// cross-tool convention) and never colour a pipe — a trace piped into `less`
// or a file should be plain text.
func colorEnabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
