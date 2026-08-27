package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// traceRecorder is the Subscriber every test asserts against: the point of the
// event bus is that "what the user saw" is a list you can compare, not a string
// you have to scrape.
type traceRecorder struct{ events []Event }

func (r *traceRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

// traceSameEvent compares two events the way the file format actually promises.
//
// reflect.DeepEqual on the whole struct is wrong here, and the reason is worth
// knowing: time.Now() carries a monotonic reading and a local *time.Location,
// while a timestamp parsed back out of JSON has neither. The two values name the
// same instant and are never deeply equal. Compare instants with Equal, then
// compare everything else structurally.
func traceSameEvent(a, b Event) bool {
	if !a.T.Equal(b.T) {
		return false
	}
	a.T, b.T = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// traceSample is one realistic session: a user message, a streamed reply, a
// tool call that runs a command, cached token accounting, and an error. Seq and
// T are left zero on purpose — the Bus stamps them, and forging them in a test
// would be testing the test.
func traceSample() []Event {
	return []Event{
		{Kind: KindUserMessage, Text: "how big is this repo?"},
		{Kind: KindTurnStart, Turn: 1},
		{Kind: KindRequest, Turn: 1, Request: json.RawMessage(
			`{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"how big is this repo?"}]}`)},
		{Kind: KindFirstToken, Turn: 1, Millis: 812},
		{Kind: KindReasoningDelta, Turn: 1, Text: "cheapest check is wc -l"},
		{Kind: KindTextDelta, Turn: 1, Text: "Let me count the lines."},
		{Kind: KindToolCallStart, Turn: 1, ToolID: "call_01", ToolName: "bash"},
		{Kind: KindToolArgsDelta, Turn: 1, ToolID: "call_01", Text: `{"command":"find . -na`},
		{Kind: KindToolCallReady, Turn: 1, ToolID: "call_01", Command: `find . -name '*.go' | xargs wc -l`},
		{Kind: KindGateVerdict, Turn: 1, ToolID: "call_01", Verdict: "allow"},
		{Kind: KindCommandStart, Turn: 1, ToolID: "call_01", Command: `find . -name '*.go' | xargs wc -l`},
		{Kind: KindCommandEnd, Turn: 1, ToolID: "call_01", ExitCode: 0, Millis: 143, Bytes: 2048},
		{Kind: KindToolResult, Turn: 1, ToolID: "call_01", Text: "  1204 total\n[exit 0 · 143ms]", Bytes: 30},
		// The shape this whole repo exists to make visible: 18 tokens billed at
		// full price, 17,967 read from cache. Anything that reports "18 input
		// tokens" as the size of this call is off by a factor of a thousand.
		{Kind: KindUsage, Turn: 1, Usage: &Usage{Input: 18, CacheRead: 17967, Output: 214, Reasoning: 96}},
		{Kind: KindResponseEnd, Turn: 1, FinishReason: "tool_calls", Millis: 2210},
		{Kind: KindTurnEnd, Turn: 1},
		{Kind: KindTurnStart, Turn: 2},
		{Kind: KindRequest, Turn: 2, Request: json.RawMessage(`{"model":"claude-opus-5","messages":[]}`)},
		{Kind: KindUsage, Turn: 2, Usage: &Usage{Input: 512, CacheWrite: 4096, Output: 88}},
		{Kind: KindNotice, Turn: 2, Text: "context is 22% full"},
		{Kind: KindError, Turn: 2, Text: "http 529: overloaded"},
		{Kind: KindResponseEnd, Turn: 2, FinishReason: "stop", Millis: 1180},
		{Kind: KindTurnEnd, Turn: 2},
	}
}

// traceRecordSession emits traceSample through a real Bus with a TraceWriter
// attached, and returns the path plus exactly what the bus delivered.
func traceRecordSession(t *testing.T) (string, []Event) {
	t.Helper()

	// A subdirectory that does not exist yet: real traces are written to
	// traces/<date>/, so NewTraceWriter creating its parent is part of the
	// contract, not a convenience.
	path := filepath.Join(t.TempDir(), "traces", "session.jsonl")
	w, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	if w.Path() != path {
		t.Errorf("Path() = %q, want %q", w.Path(), path)
	}

	rec := &traceRecorder{}
	bus := NewBus(w, rec)
	for _, e := range traceSample() {
		bus.Emit(e)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, rec.events
}

// ---------------------------------------------------------------------------
// The trace file
// ---------------------------------------------------------------------------

func TestTraceRoundTrip(t *testing.T) {
	path, want := traceRecordSession(t)

	got, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d events, wrote %d", len(got), len(want))
	}

	for i := range want {
		if got[i].Seq != i+1 {
			t.Errorf("event %d: Seq = %d, want %d (the file must preserve bus order)", i, got[i].Seq, i+1)
		}
		if !traceSameEvent(got[i], want[i]) {
			t.Errorf("event %d (%s) did not survive the round trip:\n got %+v\nwant %+v",
				i, want[i].Kind, got[i], want[i])
		}
	}

	// Spot-check the fields a sloppy schema loses silently: the pointer-valued
	// Usage, and the raw request body.
	usage := got[13]
	if usage.Kind != KindUsage || usage.Usage == nil {
		t.Fatalf("event 13 = %+v, want a usage event with a Usage payload", usage)
	}
	if usage.Usage.Input != 18 || usage.Usage.CacheRead != 17967 || usage.Usage.Reasoning != 96 {
		t.Errorf("Usage = %+v, want {Input:18 CacheRead:17967 Output:214 Reasoning:96}", *usage.Usage)
	}

	// Request is a json.RawMessage, so byte equality is the real assertion —
	// and it only holds because the bodies in traceSample are compact and free
	// of <, > and &, which encoding/json escapes on the way out. Anything that
	// re-indents or re-marshals a captured body has stopped being a record of
	// what was sent.
	wantBody := traceSample()[2].Request
	if !bytes.Equal(got[2].Request, wantBody) {
		t.Errorf("request body changed:\n got %s\nwant %s", got[2].Request, wantBody)
	}
}

func TestTraceTruncatedFinalLine(t *testing.T) {
	path, want := traceRecordSession(t)

	// Chop the last line in half: exactly what a SIGKILL between write(2) and
	// the end of the buffer leaves behind.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) != len(want) {
		t.Fatalf("file has %d lines, want %d", len(lines), len(want))
	}
	last := lines[len(lines)-1]
	maimed := append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n')
	maimed = append(maimed, last[:len(last)/2]...) // no trailing newline: the giveaway
	if err := os.WriteFile(path, maimed, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace on a truncated trace must not fail: %v", err)
	}

	// Everything before the wound, plus one synthetic notice explaining it.
	recovered := len(want) - 1
	if len(got) != recovered+1 {
		t.Fatalf("got %d events, want %d recovered + 1 notice", len(got), recovered)
	}
	for i := 0; i < recovered; i++ {
		if !traceSameEvent(got[i], want[i]) {
			t.Errorf("event %d was damaged by the recovery:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}

	notice := got[len(got)-1]
	if notice.Kind != KindNotice {
		t.Fatalf("last event is %s, want a %s explaining the truncation", notice.Kind, KindNotice)
	}
	if !strings.HasPrefix(notice.Text, TraceNoticePrefix) {
		t.Errorf("notice %q must carry %q so a renderer can tell it from an agent notice",
			notice.Text, TraceNoticePrefix)
	}
	// "Report the situation" means saying how much survived, in the text a
	// human reads — not in an error that a caller is likely to fatal on.
	for _, substr := range []string{"partial line", "22 event(s) recovered"} {
		if !strings.Contains(notice.Text, substr) {
			t.Errorf("notice %q does not mention %q", notice.Text, substr)
		}
	}
	if notice.Seq != want[recovered-1].Seq+1 {
		t.Errorf("notice Seq = %d, want %d (it must continue the sequence)", notice.Seq, want[recovered-1].Seq+1)
	}
}

func TestTraceUnknownKindStillLoads(t *testing.T) {
	// Hand-written rather than recorded: this is a file from a *future* build of
	// the agent, with a kind and a field this binary has never heard of. A
	// reader that validates kinds against its own constants breaks replay for
	// every trace recorded after the next feature lands, which is the opposite
	// of what a durable file format is for.
	path := filepath.Join(t.TempDir(), "future.jsonl")
	body := strings.Join([]string{
		`{"seq":1,"t":"2026-08-27T09:15:00.123456789Z","kind":"user_message","text":"hi"}`,
		`{"seq":2,"t":"2026-08-27T09:15:01Z","kind":"subagent_spawn","text":"reviewer","depth":2,"budget":{"usd":0.5}}`,
		`{"seq":3,"t":"2026-08-27T09:15:02Z","kind":"turn_end","turn":1}`,
		``, // a stray blank line, which is not damage
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (unknown kinds must load, blank lines must not count)", len(got))
	}
	if got[1].Kind != "subagent_spawn" {
		t.Errorf("Kind = %q, want the unknown kind preserved verbatim", got[1].Kind)
	}
	if got[1].Text != "reviewer" {
		t.Errorf("Text = %q — known fields must survive alongside unknown ones", got[1].Text)
	}
	if got[0].T.UTC().Nanosecond() != 123456789 {
		t.Errorf("timestamp lost precision: %s", got[0].T.UTC().Format(time.RFC3339Nano))
	}
}

func TestTraceWriterDegradesInsteadOfDying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doomed.jsonl")
	w, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	var warnings []string
	w.warn = func(format string, args ...any) { warnings = append(warnings, format) }

	w.OnEvent(Event{Seq: 1, T: time.Now(), Kind: KindUserMessage, Text: "recorded fine"})

	// Yank the file out from under the writer: a full disk, an unmounted
	// volume, an operator running `rm` on the trace directory.
	if err := w.f.Close(); err != nil {
		t.Fatalf("closing the underlying file: %v", err)
	}
	for i := 0; i < 50; i++ {
		w.OnEvent(Event{Seq: i + 2, Kind: KindTextDelta, Text: "into the void"})
	}

	if len(warnings) != 1 {
		t.Errorf("got %d warnings, want exactly 1 — a broken trace must be reported once, not 50 times", len(warnings))
	}
	if w.dropped != 50 {
		t.Errorf("dropped = %d, want 50", w.dropped)
	}
	closeErr := w.Close()
	if closeErr == nil || !strings.Contains(closeErr.Error(), "50 event(s)") {
		t.Errorf("Close() = %v, want an error naming the 50 lost events", closeErr)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil (main defers it, a signal handler may also call it)", err)
	}

	// And the events that made it are still readable: degrading kept the file
	// valid instead of leaving a half-written line behind.
	got, err := ReadTrace(path)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadTrace = %d events, %v; want the 1 event written before the failure", len(got), err)
	}
}

// ---------------------------------------------------------------------------
// Summarize
// ---------------------------------------------------------------------------

func TestSummarizeUsesPromptNotInput(t *testing.T) {
	_, events := traceRecordSession(t)
	s := Summarize(events)

	if s.Events != len(events) {
		t.Errorf("Events = %d, want %d", s.Events, len(events))
	}
	if s.Turns != 2 {
		t.Errorf("Turns = %d, want 2", s.Turns)
	}
	if s.Commands != 1 {
		t.Errorf("Commands = %d, want 1", s.Commands)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1", s.Errors)
	}

	// The two usage events are {18, cache_read 17967, out 214} and
	// {512, cache_write 4096, out 88}: a real shape, where the uncached
	// remainder is 2% of what was actually sent.
	want := Usage{Input: 530, CacheWrite: 4096, CacheRead: 17967, Output: 302, Reasoning: 96}
	if s.TotalUsage != want {
		t.Errorf("TotalUsage = %+v, want %+v", s.TotalUsage, want)
	}

	// This is the assertion the whole struct exists for. Summing Input gives
	// 530 — a number that looks like tokens, sorts like tokens, and is wrong by
	// 22,063 of them.
	if s.PromptTokens() != 22593 {
		t.Errorf("PromptTokens() = %d, want 22593 (Input + CacheWrite + CacheRead)", s.PromptTokens())
	}
	if s.PromptTokens() == s.TotalUsage.Input {
		t.Fatalf("PromptTokens() must not be the sum of Input alone")
	}

	// The header a student reads before a replay has to show the split, or the
	// cheap tokens and the expensive ones look identical.
	header := s.String()
	for _, substr := range []string{"prompt 22593", "full 530", "write 4096", "read 17967", "output 302", "1 error"} {
		if !strings.Contains(header, substr) {
			t.Errorf("header %q is missing %q", header, substr)
		}
	}
}

func TestSummarizeEmptyAndClockSafety(t *testing.T) {
	if s := Summarize(nil); s.Events != 0 || s.Duration != 0 || s.PromptTokens() != 0 {
		t.Errorf("Summarize(nil) = %+v, want a zero summary", s)
	}

	// One event with no timestamp at all (hand-built, or written by a future
	// version). Duration must not become a 55-year interval measured from the
	// zero time.
	base := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	s := Summarize([]Event{
		{Seq: 1, Kind: KindNotice}, // T is zero
		{Seq: 2, T: base, Kind: KindTurnStart},
		{Seq: 3, T: base.Add(90 * time.Second), Kind: KindTurnEnd},
	})
	if s.Duration != 90*time.Second {
		t.Errorf("Duration = %s, want 1m30s", s.Duration)
	}
	if got := traceDur(s.Duration); got != "1m30s" {
		t.Errorf("traceDur = %q, want %q", got, "1m30s")
	}
}

// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------

func TestReplayDeliversEveryEventInOrder(t *testing.T) {
	path, want := traceRecordSession(t)
	events, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}

	rec := &traceRecorder{}
	var out bytes.Buffer
	if err := Replay(events, rec, ReplayOpts{Speed: 0}, nil, &out); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(rec.events) != len(want) {
		t.Fatalf("replayed %d events, want %d", len(rec.events), len(want))
	}
	for i := range want {
		if !traceSameEvent(rec.events[i], want[i]) {
			// Replay must not restamp T: the recorded clock is the evidence.
			t.Fatalf("replayed event %d differs from the recorded one:\n got %+v\nwant %+v",
				i, rec.events[i], want[i])
		}
	}
	if !strings.Contains(out.String(), "trace · 23 events · 2 turns · 1 command") {
		t.Errorf("replay header missing or wrong:\n%s", out.String())
	}
}

func TestReplayFilterShowsOnlyMatchingEvents(t *testing.T) {
	_, events := traceRecordSession(t)

	rec := &traceRecorder{}
	var out bytes.Buffer
	opts := ReplayOpts{
		Speed:  0,
		Filter: func(e Event) bool { return e.Kind == KindCommandStart || e.Kind == KindCommandEnd },
	}
	if err := Replay(events, rec, opts, nil, &out); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(rec.events) != 2 {
		t.Fatalf("delivered %d events, want 2", len(rec.events))
	}
	if rec.events[0].Kind != KindCommandStart || rec.events[1].Kind != KindCommandEnd {
		t.Errorf("got kinds %s,%s; want command_start,command_end", rec.events[0].Kind, rec.events[1].Kind)
	}
	// The header still describes the whole session, so a filtered view can
	// never be mistaken for the session itself.
	if !strings.Contains(out.String(), "23 events") || !strings.Contains(out.String(), "showing 2 of 23") {
		t.Errorf("filtered header should summarise the whole trace and say what is hidden:\n%s", out.String())
	}
}

// traceLineFeeder hands out exactly one line per Read and counts them, so the
// test can assert how much input Replay *consumed* rather than how much it was
// offered. A bufio.Reader built inside the loop would read ahead and eat the
// user's next keystrokes; this is how that bug is caught.
type traceLineFeeder struct {
	lines []string
	n     int
}

func (f *traceLineFeeder) Read(p []byte) (int, error) {
	if f.n >= len(f.lines) {
		return 0, io.EOF
	}
	line := f.lines[f.n]
	if len(p) < len(line) {
		return 0, io.ErrShortBuffer // only reachable if this helper is the bug
	}
	f.n++
	return copy(p, line), nil
}

func TestReplayStepConsumesOneLinePerEvent(t *testing.T) {
	_, events := traceRecordSession(t)
	events = events[:5]

	feeder := &traceLineFeeder{lines: []string{"\n", "\n", "\n", "\n", "\n", "\n"}} // one spare
	rec := &traceRecorder{}
	var out bytes.Buffer
	if err := Replay(events, rec, ReplayOpts{Step: true}, feeder, &out); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(rec.events) != 5 {
		t.Fatalf("delivered %d events, want 5", len(rec.events))
	}
	if feeder.n != 5 {
		t.Errorf("consumed %d lines for 5 events, want exactly 5", feeder.n)
	}
	for _, prompt := range []string{"[1/5 user_message] ", "[5/5 reasoning_delta] "} {
		if !strings.Contains(out.String(), prompt) {
			t.Errorf("step prompt %q missing from:\n%s", prompt, out.String())
		}
	}

	// Input running out is Ctrl-D, and Ctrl-D stops the replay rather than
	// silently playing the rest unattended.
	short := &traceLineFeeder{lines: []string{"\n", "\n"}}
	rec2 := &traceRecorder{}
	var out2 bytes.Buffer
	if err := Replay(events, rec2, ReplayOpts{Step: true}, short, &out2); err != nil {
		t.Fatalf("Replay after EOF: %v", err)
	}
	if len(rec2.events) != 2 {
		t.Errorf("delivered %d events after 2 lines of input, want 2", len(rec2.events))
	}
	if !strings.Contains(out2.String(), "[replay stopped after 2 of 5 events]") {
		t.Errorf("replay should say why it stopped:\n%s", out2.String())
	}

	// And "q" quits without consuming the rest.
	quit := &traceLineFeeder{lines: []string{"\n", "q\n", "\n", "\n"}}
	rec3 := &traceRecorder{}
	if err := Replay(events, rec3, ReplayOpts{Step: true}, quit, io.Discard); err != nil {
		t.Fatalf("Replay with quit: %v", err)
	}
	if len(rec3.events) != 1 || quit.n != 2 {
		t.Errorf("q delivered %d events after %d lines, want 1 event after 2 lines", len(rec3.events), quit.n)
	}
}

func TestReplayClampsAbsurdGaps(t *testing.T) {
	// A session where the user went to lunch between two events. Replayed at
	// wall-clock speed with no cap this sleeps for 41 minutes.
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Seq: 1, T: base, Kind: KindTurnEnd},
		{Seq: 2, T: base.Add(41 * time.Minute), Kind: KindUserMessage, Text: "back"},
		{Seq: 3, T: base.Add(41*time.Minute + 30*time.Millisecond), Kind: KindTurnStart},
	}

	rec := &traceRecorder{}
	started := time.Now()
	// Speed scales the *capped* gap, so a large speed shrinks even the worst
	// case: 5s / 1000 = 5ms.
	if err := Replay(events, rec, ReplayOpts{Speed: 1000}, nil, io.Discard); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	elapsed := time.Since(started)

	if len(rec.events) != 3 {
		t.Fatalf("delivered %d events, want 3", len(rec.events))
	}
	if elapsed > 2*time.Second {
		t.Errorf("replay took %s — the %s gap cap is not being applied before Speed scales it",
			elapsed, maxReplayGap)
	}
}

func TestReplayRejectsNilSubscriber(t *testing.T) {
	if err := Replay(nil, nil, ReplayOpts{}, nil, io.Discard); err == nil {
		t.Error("Replay with no subscriber should fail loudly: there is nowhere for the events to go")
	}
}
