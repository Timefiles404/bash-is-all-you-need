// Stage 02 — replay.
//
// A trace read back through the same Subscriber the live agent used. That is
// the whole trick, and it is only available because the core prints nothing: if
// the renderer took an *event* rather than a print statement, then a recorded
// event is indistinguishable from a live one and replay is fifty lines instead
// of a second implementation of the UI.
//
// It is also the one part of this repo that works with no API key, no network
// and no money, which makes it the honest answer to "I want to study a real
// agent session" — including sessions you never paid for.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TraceNoticePrefix marks the events ReadTrace *synthesises* rather than reads,
// so a renderer (or a test) can tell "the trace said this" apart from "the
// agent said this".
const TraceNoticePrefix = "[trace] "

// maxReplayGap caps how long replay waits between two recorded events.
//
// A trace records real gaps, and a real session contains a human who went to
// lunch between two prompts. Reproducing a 41-minute gap faithfully is not
// fidelity, it is a hang: the student sees a frozen terminal and kills it.
//
// Five seconds is picked so that everything replay exists to convey survives
// untouched — TTFT (0.3–2s), the pacing of text deltas (milliseconds), a
// command's wall clock (usually under 5s) — while everything above it is a
// person being idle, which the timestamps already report better than a wait
// does. The cap applies to the *recorded* gap, before Speed scales it, so
// `--speed 2` still halves it and a deliberate `--speed 0.5` can still stretch
// it to ten seconds.
const maxReplayGap = 5 * time.Second

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// ReadTrace loads a trace file.
//
// The error return is for a file that cannot be read at all. A trace whose last
// line stops mid-object is *not* one of those cases — it is the normal shape of
// an agent that was killed, which is the exact session you most want to look
// at. Returning an error there would invite the reflex `if err != nil { fatal }`
// and throw away the four hundred events that explain the crash.
//
// So damage is reported *in the event stream instead*: everything recoverable
// comes back, followed by a synthetic KindNotice saying what was wrong and how
// many events were recovered. Replay then shows it in its natural place, at the
// end of the session, where a student reading the replay will actually see it.
func ReadTrace(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("trace: cannot read %s: %w", path, err)
	}
	defer f.Close()

	// bufio.Reader, not bufio.Scanner. Scanner caps one token at 64KB and fails
	// the *entire* read with ErrTooLong on the first line that exceeds it — and
	// the most valuable line in any trace, the request body, is precisely the
	// one that grows past 64KB somewhere around turn thirty. ReadBytes has no
	// cap, and it returns a final line with no trailing '\n' together with
	// io.EOF, which is exactly the signal that a writer died mid-line.
	r := bufio.NewReaderSize(f, 64*1024)

	var (
		events    []Event
		corrupt   int // complete lines that did not parse: real damage
		truncated int // bytes in an unterminated final line: the ordinary crash
	)
	for {
		line, rerr := r.ReadBytes('\n')
		atEOF := rerr == io.EOF

		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var e Event
			switch {
			case trimmed[0] != '{':
				// JSONL lines are objects. Anything else is somebody's log
				// output that ended up in the same file.
				corrupt++

			case json.Unmarshal(trimmed, &e) != nil:
				if atEOF {
					// No trailing newline *and* it does not parse. The writer
					// emits object+'\n' in a single write, so this is a write
					// that the kernel only partly committed before the process
					// died. Expected, not corruption.
					truncated = len(trimmed)
				} else {
					// A complete line that does not parse is different: the
					// bytes after it survived, so this is damage in the middle
					// of an otherwise intact file. Skip it and say so.
					corrupt++
				}

			default:
				// Note what is deliberately *not* checked here: e.Kind is never
				// validated against the constants in events.go. A trace written
				// by a newer build carries kinds this binary has never heard
				// of, and rejecting them would mean every future kind silently
				// breaks replay of every file recorded after it. Unknown kinds
				// load, replay, and reach the renderer, which is free to print
				// them raw. (The one real limit: unknown *fields* are dropped
				// on decode — harmless while nothing here re-serialises.)
				events = append(events, e)
			}
		}

		if rerr != nil {
			if !atEOF {
				// A genuine I/O failure. Hand back what was recovered anyway;
				// partial evidence still beats none.
				return events, fmt.Errorf("trace: reading %s after %d events: %w", path, len(events), rerr)
			}
			break
		}
	}

	if truncated > 0 || corrupt > 0 {
		events = append(events, traceDamageNotice(path, events, truncated, corrupt))
	}
	return events, nil
}

// traceDamageNotice builds the synthetic event that tells the reader what the
// file was missing. It borrows the last real event's timestamp and turn so that
// a time-ordered renderer puts it where it belongs rather than at the epoch.
func traceDamageNotice(path string, events []Event, truncated, corrupt int) Event {
	e := Event{Kind: KindNotice}
	if n := len(events); n > 0 {
		e.Seq = events[n-1].Seq + 1
		e.T = events[n-1].T
		e.Turn = events[n-1].Turn
	} else {
		e.Seq = 1
		e.T = time.Now()
	}

	var parts []string
	if truncated > 0 {
		parts = append(parts, fmt.Sprintf("ends in a %d-byte partial line (the agent was killed mid-write)", truncated))
	}
	if corrupt > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable line(s) skipped", corrupt))
	}
	e.Text = fmt.Sprintf("%s%s %s — %d event(s) recovered",
		TraceNoticePrefix, filepath.Base(path), strings.Join(parts, "; "), len(events))
	return e
}

// ---------------------------------------------------------------------------
// Summarising
// ---------------------------------------------------------------------------

// TraceSummary is the at-a-glance header shown before a replay starts.
type TraceSummary struct {
	Events     int
	Turns      int
	Commands   int
	Duration   time.Duration
	TotalUsage Usage // summed across all KindUsage events
	Errors     int

	// Stage 09. Counted apart from Errors on purpose: a call_error that was
	// retried and then succeeded is not an error the session suffered, it is a
	// failure the session absorbed. Folding the two together would make every
	// robust session look broken, and a header nobody believes is a header
	// nobody reads.
	CallErrors int
	Retries    int
	Fallbacks  int

	// Stage 12. Tool calls answered without running anything.
	//
	// It has to be its own number rather than being folded into Commands,
	// because Commands counts command_start events and a cache hit emits none —
	// so a session of ten tool calls with four hits honestly reports six
	// commands. Without this field that header would look like a session that
	// lost four calls somewhere.
	CacheHits int
}

// Summarize reduces a whole session to the six numbers worth seeing first.
func Summarize(events []Event) TraceSummary {
	var s TraceSummary
	s.Events = len(events)

	var first, last time.Time
	for _, e := range events {
		switch e.Kind {
		case KindTurnStart:
			// Counted at the *start*, not the end. The traces worth reading are
			// the ones that stop mid-turn: a session killed during turn 12 did
			// twelve turns, and counting turn_end would report eleven and hide
			// the one turn you opened the file to look at.
			//
			// Also not "the number of distinct e.Turn values": Turn restarts at
			// 1 for every user message (see events.go), so distinct values
			// undercount every session longer than one prompt.
			s.Turns++

		case KindCommandStart:
			s.Commands++ // same argument: the command that never returned counts

		case KindResultCache:
			if e.Verdict == string(cacheHit) {
				s.CacheHits++
			}

		case KindError:
			s.Errors++

		case KindCallError:
			s.CallErrors++

		case KindRetry:
			s.Retries++

		case KindProvider:
			// The session-start event carries no triage verdict; only a fallback
			// does. Counting every provider event would report one fallback on
			// every clean session ever recorded.
			if e.Triage != "" {
				s.Fallbacks++
			}

		case KindUsage:
			// Gated on the kind, not merely on Usage != nil, so that a future
			// event carrying a usage snapshot alongside something else cannot
			// silently double the totals.
			if e.Usage != nil {
				// Every field is summed separately, and the prompt total is
				// derived from them afterwards. Summing "the input tokens" is
				// the exact bug the Usage doc comment warns about: a cached turn
				// reports Input 18 while really sending 18,000, so a total built
				// from Input alone is out by three orders of magnitude — and
				// plausible enough that nobody ever re-checks it.
				s.TotalUsage = addUsage(s.TotalUsage, *e.Usage)
			}
		}

		// Min/max rather than first-and-last-element: an event with a zero T
		// (hand-built, or from a future writer that omits it) would otherwise
		// make the duration a 55-year negative number.
		if !e.T.IsZero() {
			if first.IsZero() || e.T.Before(first) {
				first = e.T
			}
			if e.T.After(last) {
				last = e.T
			}
		}
	}
	if !first.IsZero() {
		s.Duration = last.Sub(first)
	}
	return s
}

// PromptTokens is what people mean when they ask how much this session sent.
//
// It is Prompt(), never Input. See the Usage doc comment in events.go: Input is
// only the uncached remainder, and reading it as the total is the single most
// common way an agent's own token accounting turns out to be a lie.
func (s TraceSummary) PromptTokens() int { return s.TotalUsage.Prompt() }

// String renders the header. Two lines, no colour: it has to be readable when a
// student pipes replay into a file.
func (s TraceSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "trace · %s · %s · %s · %s",
		tracePlural(s.Events, "event"), tracePlural(s.Turns, "turn"),
		tracePlural(s.Commands, "command"), traceDur(s.Duration))
	if s.CacheHits > 0 {
		fmt.Fprintf(&b, " · %d cached", s.CacheHits)
	}
	if s.Errors > 0 {
		fmt.Fprintf(&b, " · %s", tracePlural(s.Errors, "error"))
	}
	if s.CallErrors > 0 {
		fmt.Fprintf(&b, " · %s", tracePlural(s.CallErrors, "failed call"))
	}
	if s.Retries > 0 {
		// Spelled out rather than routed through tracePlural, which appends an
		// "s": "2 retrys" in a header is exactly the kind of detail that makes
		// a reader stop trusting the numbers printed next to it.
		word := "retries"
		if s.Retries == 1 {
			word = "retry"
		}
		fmt.Fprintf(&b, " · %d %s", s.Retries, word)
	}
	if s.Fallbacks > 0 {
		fmt.Fprintf(&b, " · %s", tracePlural(s.Fallbacks, "fallback"))
	}
	if s.PromptTokens() > 0 || s.TotalUsage.Output > 0 {
		// The split, always. One "prompt tokens: 18231" number hides which of
		// the three prices those tokens were billed at, and the three differ by
		// more than 10x.
		fmt.Fprintf(&b, "\ntokens · prompt %d (full %d · write %d · read %d) · output %d",
			s.PromptTokens(), s.TotalUsage.Input, s.TotalUsage.CacheWrite,
			s.TotalUsage.CacheRead, s.TotalUsage.Output)
	}
	return b.String()
}

// tracePlural stops the header saying "1 commands". It is a silly amount of
// care for one character, and it is the first line a student sees in the one
// feature this repo advertises as usable without an API key.
func tracePlural(n int, singular string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %ss", n, singular)
}

func traceDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d >= time.Minute:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

// ---------------------------------------------------------------------------
// Replaying
// ---------------------------------------------------------------------------

type ReplayOpts struct {
	Speed  float64          // 0 = instant, 1 = original wall-clock timing, 2 = double speed
	Step   bool             // wait for Enter before each event
	Filter func(Event) bool // nil = everything
}

// Replay feeds recorded events to a Subscriber as if they were happening now.
//
// One thing it deliberately does not do is restamp Event.T. "As if they were
// happening now" is about pacing, not about lying: the recorded timestamps are
// the evidence, and a renderer that shows TTFT or a command's wall clock is
// reading them. Replay controls *when* OnEvent is called, never what it is
// called with — which is also what lets a test compare a replayed run against a
// live one event for event.
func Replay(events []Event, sub Subscriber, opts ReplayOpts, in io.Reader, out io.Writer) error {
	if sub == nil {
		return fmt.Errorf("replay: no subscriber to replay into")
	}
	if out == nil {
		out = io.Discard
	}

	shown := events
	if opts.Filter != nil {
		shown = nil
		for _, e := range events {
			if opts.Filter(e) {
				shown = append(shown, e)
			}
		}
	}

	// The header summarises the *whole* trace even when a filter is on, because
	// "this session made 47 model calls and you are looking at 3 of them" is the
	// context that stops a filtered view being mistaken for the session.
	fmt.Fprintln(out, Summarize(events))
	fmt.Fprintf(out, "replay · %s", replayMode(opts))
	if len(shown) != len(events) {
		fmt.Fprintf(out, " · showing %d of %d events", len(shown), len(events))
	}
	fmt.Fprint(out, "\n\n")

	var stepIn *bufio.Reader
	if opts.Step {
		if in == nil {
			in = strings.NewReader("")
		}
		// Built once, outside the loop. A fresh bufio.Reader per event would
		// read ahead into its own buffer and throw away everything past the
		// first line, silently eating the user's next keystrokes.
		stepIn = bufio.NewReader(in)
	}

	var prev time.Time
	for i, e := range shown {
		switch {
		case opts.Step:
			// Step wins over Speed. Waiting for a human and then also sleeping
			// the recorded gap is just a slower way to wait for the same human.
			fmt.Fprintf(out, "[%d/%d %s] ", i+1, len(shown), e.Kind)
			cont, err := readStep(stepIn)
			if err != nil {
				return fmt.Errorf("replay: reading step input: %w", err)
			}
			if !cont {
				fmt.Fprintf(out, "\n[replay stopped after %d of %d events]\n", i, len(shown))
				return nil
			}

		case opts.Speed > 0 && !prev.IsZero():
			gap := e.T.Sub(prev)
			if gap > maxReplayGap {
				gap = maxReplayGap
			}
			if gap > 0 {
				// Negative gaps are possible and are not a bug to fix here: two
				// events can share a timestamp, and a trace merged from two
				// processes can go backwards. Clamping at zero keeps replay
				// moving forward whatever the clock did.
				time.Sleep(time.Duration(float64(gap) / opts.Speed))
			}
		}

		if !e.T.IsZero() {
			prev = e.T
		}
		sub.OnEvent(e)
	}
	return nil
}

// readStep consumes exactly one line and reports whether replay should go on.
//
// Exactly one: a trace with 4,000 text deltas is unusable if a single Enter can
// be read as two steps, and unreadable if two Enters are needed for one.
func readStep(r *bufio.Reader) (bool, error) {
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	if err == io.EOF && strings.TrimSpace(line) == "" {
		// Ctrl-D, or a script that ran out of input. Stopping is the honest
		// reading of "the user closed the input"; playing the remainder
		// unattended would be a surprise.
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "q", "quit", "exit":
		return false, nil
	default:
		return true, nil
	}
}

func replayMode(opts ReplayOpts) string {
	if opts.Step {
		return "step (Enter = next, q = quit)"
	}
	if opts.Speed <= 0 {
		return "instant"
	}
	return fmt.Sprintf("%gx speed (gaps capped at %s)", opts.Speed, maxReplayGap)
}
