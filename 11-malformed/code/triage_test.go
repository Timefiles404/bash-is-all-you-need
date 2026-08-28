package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// roundTripEvent writes one event through the real TraceWriter and reads it back
// with the real ReadTrace, rather than json.Marshal/Unmarshal. The point is to
// exercise the path a session actually takes to disk: a field that survives a
// direct marshal but not the writer would pass a weaker test and break replay.
func roundTripEvent(t *testing.T, in Event) Event {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := NewTraceWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	NewBus(tw).Emit(in)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("read %d events back, want 1", len(evs))
	}
	return evs[0]
}

func traceLineFor(t *testing.T, in Event) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// This file tests stage 09: the classifier, the backoff, the envelope parser,
// the retry loop and the ladder.
//
// Almost none of it needs a network, and that is the design being tested as
// much as the code. The decisions live in pure functions over a *CallError, so
// the interesting cases — a 401 that means "wrong model", a 500 that means
// "our bug", a budget that runs out mid-outage — are table rows rather than
// integration fixtures. The three tests that do stand up an httptest server are
// the ones where the *transport* is the subject: a real Retry-After header, a
// real truncated body, a real error envelope served as text/plain.
//
// Every table row below is traceable to external/wire-notes.md §D11/§A3c or to RFC
// 9110. Rows that are not observed behaviour say so.

// ---------------------------------------------------------------------------
// The classifier
// ---------------------------------------------------------------------------

func TestTriageClassifiesTheObservedFailures(t *testing.T) {
	cases := []struct {
		name string
		err  CallError
		want Triage
		why  string
	}{
		// The two rows this whole stage exists for. Same status, same envelope
		// shape, opposite decisions — §D11.
		{"401 revoked key", CallError{Phase: phaseStatus, Status: 401, Type: "AuthError"}, TriageFatal,
			"a bad key cannot be fixed by waiting or by asking someone else"},
		{"401 wrong model name", CallError{Phase: phaseStatus, Status: 401, Type: "ModelError"}, TriageFallback,
			"observed: a nonexistent model id returns 401 here, not 404"},

		// A 401 with no envelope to read. Fatal is the safe reading: with no
		// error.type there is nothing that says "model", and treating an
		// unreadable auth failure as a fallback would walk the whole ladder on
		// a machine whose key is simply absent.
		{"401 no envelope", CallError{Phase: phaseStatus, Status: 401}, TriageFatal, ""},
		{"403", CallError{Phase: phaseStatus, Status: 403, Type: "AuthError"}, TriageFatal, ""},
		{"404", CallError{Phase: phaseStatus, Status: 404}, TriageFallback,
			"the route or model is not on this endpoint; another may have it"},

		{"429", CallError{Phase: phaseStatus, Status: 429}, TriageRetry, "not observed on this gateway; RFC behaviour"},
		{"408", CallError{Phase: phaseStatus, Status: 408}, TriageRetry, ""},
		{"409", CallError{Phase: phaseStatus, Status: 409}, TriageRetry, ""},

		{"400", CallError{Phase: phaseStatus, Status: 400}, TriageFatal, "ours; retrying it is how a client bug becomes an outage"},
		{"422", CallError{Phase: phaseStatus, Status: 422}, TriageFatal, ""},
		{"413", CallError{Phase: phaseStatus, Status: 413}, TriageFatal, "the bytes are the problem; only compaction changes them"},

		// The second trap. Retryable, but see the leash test below.
		{"500", CallError{Phase: phaseStatus, Status: 500, Type: "error", Message: "Internal server error"}, TriageRetry, ""},
		{"502", CallError{Phase: phaseStatus, Status: 502}, TriageRetry, ""},
		{"503", CallError{Phase: phaseStatus, Status: 503}, TriageRetry, ""},
		{"504", CallError{Phase: phaseStatus, Status: 504}, TriageRetry, ""},

		// Anything unclassified is fatal, on purpose: an unclassified failure
		// retried is a failure repeated, and the emitted event is how the
		// missing case gets discovered.
		{"418", CallError{Phase: phaseStatus, Status: 418}, TriageFatal, ""},
		{"302", CallError{Phase: phaseStatus, Status: 302}, TriageFatal, ""},

		{"build", CallError{Phase: phaseBuild}, TriageFatal, "our own bug; it will not render on the second try either"},
		{"connect", CallError{Phase: phaseConnect}, TriageRetry, "nothing generated, nothing billed: the only free retry"},

		// §A3c's neighbour: an in-stream error event. The type decides, and the
		// absence of a type means the transport died.
		{"stream broke", CallError{Phase: phaseStream}, TriageRetry, ""},
		{"stream overloaded_error", CallError{Phase: phaseStream, Type: "overloaded_error"}, TriageRetry, ""},
		{"stream api_error", CallError{Phase: phaseStream, Type: "api_error"}, TriageRetry, ""},
		{"stream rate_limit_error", CallError{Phase: phaseStream, Type: "rate_limit_error"}, TriageRetry, ""},
		{"stream invalid_request_error", CallError{Phase: phaseStream, Type: "invalid_request_error"}, TriageFatal,
			"arrived because of what we sent; sending it again produces it again"},
		{"stream authentication_error", CallError{Phase: phaseStream, Type: "authentication_error"}, TriageFatal, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.err.triage(); got != c.want {
				t.Fatalf("triage(%+v) = %q, want %q%s", c.err, got, c.want, hint(c.why))
			}
		})
	}
}

func hint(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}

// The PascalCase finding, tested as a property rather than as one row.
//
// §D11 observed `ModelError` where both protocol specs document snake_case
// (`not_found_error`, `invalid_request_error`). An equality check against
// either spelling would be right against the documentation and wrong against
// the wire, so the classifier matches a substring — and this test would fail if
// someone "tidied" it into a switch on exact values.
func TestTriageMatchesModelErrorWhateverItsCasing(t *testing.T) {
	for _, typ := range []string{"ModelError", "model_error", "MODEL_NOT_FOUND", "not_found_model_error"} {
		if got := triageStatus(401, typ); got != TriageFallback {
			t.Errorf("triageStatus(401, %q) = %q, want fallback", typ, got)
		}
	}
	// And the negative: an auth failure must not be dragged into the model
	// branch by an unrelated word.
	for _, typ := range []string{"AuthError", "authentication_error", "permission_error"} {
		if got := triageStatus(401, typ); got != TriageFatal {
			t.Errorf("triageStatus(401, %q) = %q, want fatal", typ, got)
		}
	}
}

func TestLeashIsShortForABare5xxAndFullForA503(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{500, 2}, // as likely our misconfiguration as their outage (§D11)
		{502, 2},
		{504, 2},
		{503, 0}, // a real capacity signal: the full allowance
		{429, 0},
	}
	for _, c := range cases {
		e := CallError{Phase: phaseStatus, Status: c.status}
		if got := e.leash(); got != c.want {
			t.Errorf("leash(%d) = %d, want %d", c.status, got, c.want)
		}
	}
	// A stream break is not a status, so the status-shaped rule must not reach
	// it: this row is what stops leash() being written as `if Status >= 500`
	// with no phase check.
	if got := (&CallError{Phase: phaseStream}).leash(); got != 0 {
		t.Errorf("leash(stream) = %d, want 0", got)
	}
	// The same rule under the one combination that would slip past the row
	// above. Nothing in this stage sets a status on a stream error, so a
	// phase-less `if Status >= 500` looks harmless — until stage 10's watchdog
	// starts carrying the status it got its 200 from, and mid-stream breaks
	// silently inherit the short leash meant for a server error.
	if got := (&CallError{Phase: phaseStream, Status: 500}).leash(); got != 0 {
		t.Errorf("leash(stream carrying a 500) = %d, want 0 — the leash rule is about statuses, not streams", got)
	}
}

// Compaction gets a shorter leash than the session policy, whatever the session
// policy is. Every attempt re-sends the whole transcript at full price, and it
// does it while the turn that needed the space is still waiting.
func TestForCompactionShortensTheLeash(t *testing.T) {
	got := retryPolicy{attempts: 9, base: time.Second, max: time.Minute, budget: time.Hour}.forCompaction()
	if got.attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.attempts)
	}
	if got.budget != 5*time.Second {
		t.Errorf("budget = %v, want 5s", got.budget)
	}
	// A policy already stricter than the cap is left alone: the cap is a
	// ceiling, not a setting. `--retry 1` means one attempt everywhere,
	// including here.
	tight := retryPolicy{attempts: 1, base: time.Second, max: time.Second, budget: time.Second}
	if got := tight.forCompaction(); got.attempts != 1 || got.budget != time.Second {
		t.Errorf("forCompaction raised a tighter policy: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// The error envelope
// ---------------------------------------------------------------------------

func TestParseErrorBodyHandlesEveryObservedShape(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantT, wantM  string
		fromWireNotes bool
	}{
		{"anthropic envelope, both protocols (D11)",
			`{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}`,
			"AuthError", "Invalid API key.", true},
		{"model error (D11)",
			`{"type":"error","error":{"type":"ModelError","message":"Model gpt-does-not-exist-9000 is not supported"}}`,
			"ModelError", "Model gpt-does-not-exist-9000 is not supported", true},
		{"500 lowercase type (D11)",
			`{"type":"error","error":{"type":"error","message":"Internal server error"}}`,
			"error", "Internal server error", true},

		// The row that justifies keeping the raw body on CallError. A 400 with
		// no envelope at all: a 24-byte echo of the request.
		{"400 with no envelope (D11)", `{"model":"qwen3.7-plus"}`, "", "", true},

		// An OpenAI-shaped body with a `code` field, which this gateway does
		// not send but a different endpoint will. `code` is typed as `any`
		// because it arrives as both a string and null in the wild; a `string`
		// field there would make the whole envelope fail to unmarshal and lose
		// the message too.
		{"openai shape with a code", `{"error":{"message":"nope","type":"invalid_request_error","code":"model_not_found"}}`,
			"invalid_request_error", "nope", false},
		{"openai shape with a null code", `{"error":{"message":"nope","type":"invalid_request_error","code":null}}`,
			"invalid_request_error", "nope", false},
		{"openai shape with a numeric code", `{"error":{"message":"nope","type":"invalid_request_error","code":404}}`,
			"invalid_request_error", "nope", false},

		{"not json at all", `502 Bad Gateway`, "", "", false},
		{"empty", ``, "", "", false},
		{"json but not an object", `[1,2,3]`, "", "", false},
		{"error is a string, not an object", `{"error":"boom"}`, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gt, gm := parseErrorBody([]byte(c.body))
			if gt != c.wantT || gm != c.wantM {
				t.Fatalf("parseErrorBody(%s) = (%q, %q), want (%q, %q)", c.body, gt, gm, c.wantT, c.wantM)
			}
		})
	}
}

// The no-envelope case has to survive all the way to the message a human reads,
// not just to the parser. "http 400: " with an empty tail reads like a bug in
// the agent; naming the absence points at the server, and the body is right
// there in the line.
func TestErrorNamesAMissingEnvelopeAndKeepsTheBody(t *testing.T) {
	e := &CallError{Phase: phaseStatus, Status: 400, Body: `{"model":"qwen3.7-plus"}`}
	got := e.Error()
	for _, want := range []string{"400", "no error envelope", `{"model":"qwen3.7-plus"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, missing %q", got, want)
		}
	}
}

func TestUnwrapKeepsErrorsIsWorking(t *testing.T) {
	e := &CallError{Phase: phaseConnect, Err: io.ErrUnexpectedEOF, Message: "boom"}
	if !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatal("errors.Is could not see through CallError — Unwrap is missing or wrong")
	}
	ce, ok := asCallError(fmt.Errorf("wrapped: %w", e))
	if !ok || ce.Phase != phaseConnect {
		t.Fatalf("asCallError could not find a CallError through fmt.Errorf: %v %v", ce, ok)
	}
}

// ---------------------------------------------------------------------------
// Retry-After
// ---------------------------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	hdr := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return h
	}
	cases := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"absent", "", 0},
		{"delta seconds", "7", 7 * time.Second},
		{"delta seconds with spaces", "  7  ", 7 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"http date in the future", "Thu, 27 Aug 2026 12:00:30 GMT", 30 * time.Second},
		// A date already past means "now", not a negative sleep. A signed
		// duration returned here would flow into time.Sleep and return
		// instantly, turning the backoff off exactly when a server is asking
		// for one.
		{"http date in the past", "Thu, 27 Aug 2026 11:59:30 GMT", 0},
		// Unparseable is ignored rather than guessed at: the computed backoff
		// is a known-safe number and an invented one is not.
		{"garbage", "soon please", 0},
		{"float", "7.5", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRetryAfter(hdr(c.val), now); got != c.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", c.val, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Backoff
// ---------------------------------------------------------------------------

func TestWaitGrowsExponentiallyAndIsCapped(t *testing.T) {
	p := retryPolicy{base: 500 * time.Millisecond, max: 2 * time.Second}
	full := func() float64 { return 1 } // the top of the jitter interval

	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	for i, w := range want {
		if got := p.wait(i+1, 0, full); got != w {
			t.Errorf("wait(%d) = %v, want %v", i+1, got, w)
		}
	}

	// The shift must not be allowed to overflow into a negative or zero
	// duration on a long-running session. Without the `exp <= 0` guard,
	// wait(64) returns 0 and the loop stops waiting at all.
	if got := p.wait(64, 0, full); got != p.max {
		t.Errorf("wait(64) = %v, want the cap %v — the shift overflowed", got, p.max)
	}
}

// Full jitter, not half. The property is that the draw covers the whole
// interval from zero: with several subagents sharing one client and one
// endpoint, a policy whose minimum wait is exp/2 keeps its own clients
// synchronised and re-collides on every attempt.
func TestWaitUsesFullJitter(t *testing.T) {
	p := retryPolicy{base: time.Second, max: time.Minute}
	if got := p.wait(3, 0, func() float64 { return 0 }); got != 0 {
		t.Errorf("wait with rnd=0 = %v, want 0 — the jitter does not reach the bottom of the interval", got)
	}
	if got := p.wait(3, 0, func() float64 { return 0.5 }); got != 2*time.Second {
		t.Errorf("wait with rnd=0.5 = %v, want 2s (half of the 4s interval)", got)
	}
}

func TestRetryAfterBeatsTheComputedBackoffButNotTheClamp(t *testing.T) {
	p := retryPolicy{base: 500 * time.Millisecond, max: 2 * time.Second}
	full := func() float64 { return 1 }

	// The server knows when its capacity comes back and we do not, so its
	// number wins — even when it is shorter than our backoff.
	if got := p.wait(5, 250*time.Millisecond, full); got != 250*time.Millisecond {
		t.Errorf("wait with Retry-After 250ms = %v, want 250ms", got)
	}
	// But a server is also allowed to say "an hour", and an agent that honours
	// that looks hung. The shape of the request is honoured; the length is not.
	if got := p.wait(1, time.Hour, full); got != p.max*8 {
		t.Errorf("wait with Retry-After 1h = %v, want the clamp %v", got, p.max*8)
	}
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// loopFixture drives retryLoop with a scripted sequence of outcomes and no
// clock: sleep is recorded rather than performed, so a test of a 30-second
// budget runs in microseconds.
type loopFixture struct {
	t       *testing.T
	rec     *mulRecorder
	bus     *Bus
	slept   []time.Duration
	seen    []Provider // which rung each attempt actually ran against
	scripts []error    // nil means success
	n       int
}

func newLoopFixture(t *testing.T, script ...error) *loopFixture {
	rec := &mulRecorder{}
	return &loopFixture{t: t, rec: rec, bus: NewBus(rec), scripts: script}
}

func (f *loopFixture) run(pol retryPolicy, lad *ladder) (*CallResult, error) {
	return retryLoop(context.Background(), f.bus, 1, pol, lad,
		func(_ context.Context, d time.Duration) error { f.slept = append(f.slept, d); return nil },
		func() float64 { return 1 }, // top of the jitter interval: deterministic
		func(_ context.Context, p Provider) (*CallResult, error) {
			f.seen = append(f.seen, p)
			i := f.n
			f.n++
			if i >= len(f.scripts) || f.scripts[i] == nil {
				return &CallResult{Text: "ok", Usage: Usage{Input: 100, Output: 5}}, nil
			}
			return nil, f.scripts[i]
		})
}

// oneRung is a ladder with a single provider on it, which is what a session
// without --fallback has.
func oneRung(name string) *ladder {
	return newLadder(rung{p: nil, info: ProviderInfo{Name: name}})
}

func TestRetryLoopRetriesUntilItWorks(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 503}
	f := newLoopFixture(t, boom, boom, nil)
	pol := retryPolicy{attempts: 3, base: time.Second, max: time.Minute, budget: time.Minute}

	res, err := f.run(pol, oneRung("primary"))
	if err != nil {
		t.Fatalf("want success on the third attempt, got %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("res.Text = %q", res.Text)
	}
	if len(f.slept) != 2 {
		t.Fatalf("slept %v, want two waits", f.slept)
	}
	// Exponential, and in that order. A policy that reset the exponent on every
	// attempt would sleep 1s twice and pass a weaker assertion.
	if f.slept[0] != time.Second || f.slept[1] != 2*time.Second {
		t.Errorf("waits = %v, want [1s 2s]", f.slept)
	}
	if got := f.rec.count(KindCallError); got != 2 {
		t.Errorf("call_error events = %d, want 2", got)
	}
	if got := f.rec.count(KindRetry); got != 2 {
		t.Errorf("retry events = %d, want 2", got)
	}
	// The verdict is on the event, not only in the log line: stage 18's metrics
	// read it off the trace.
	for _, e := range f.rec.kind(KindCallError) {
		if e.Triage != string(TriageRetry) {
			t.Errorf("call_error carried triage %q, want retry", e.Triage)
		}
		if e.Status != 503 {
			t.Errorf("call_error carried status %d, want 503", e.Status)
		}
		// The phase has to survive the trip from CallError onto the event.
		// Without it the panel cannot tell a refused request (free) from a
		// broken stream (billed), and the re-bill figure is invented — the exact
		// bug the first live run of this stage produced.
		if e.Phase != string(phaseStatus) {
			t.Errorf("call_error carried phase %q, want status", e.Phase)
		}
	}
	// Attempt numbers have to be usable: a retry event announcing the attempt
	// it is about to make, not the one that just failed.
	if a := f.rec.kind(KindRetry)[0].Attempt; a != 2 {
		t.Errorf("first retry announced attempt %d, want 2", a)
	}
}

func TestRetryLoopStopsImmediatelyOnAFatalVerdict(t *testing.T) {
	f := newLoopFixture(t, &CallError{Phase: phaseStatus, Status: 400, Type: "invalid_request_error"}, nil)
	pol := retryPolicy{attempts: 5, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, oneRung("primary")); err == nil {
		t.Fatal("a 400 must not be retried into a success")
	}
	if f.n != 1 {
		t.Fatalf("made %d attempts, want exactly 1", f.n)
	}
	if len(f.slept) != 0 {
		t.Fatalf("slept %v on a fatal verdict", f.slept)
	}
	if got := f.rec.count(KindRetry); got != 0 {
		t.Errorf("emitted %d retry events on a fatal verdict", got)
	}
}

// The leash, end to end. Five attempts allowed by the policy, two taken,
// because a bare 500 on this gateway is as likely to be a client bug (§D11).
func TestRetryLoopHonoursTheShortLeashOnABare500(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 500, Type: "error", Message: "Internal server error"}
	f := newLoopFixture(t, boom, boom, boom, boom, boom, nil)
	pol := retryPolicy{attempts: 5, base: time.Millisecond, max: time.Second, budget: time.Minute}

	_, err := f.run(pol, oneRung("primary"))
	if err == nil {
		t.Fatal("want failure after the leash runs out")
	}
	if f.n != 2 {
		t.Fatalf("made %d attempts, want 2 (the leash), policy allowed %d", f.n, pol.attempts)
	}
	if !strings.Contains(err.Error(), "2 attempts") {
		t.Errorf("error = %q, want it to name the attempt count", err)
	}
	// And the contrast, on the same policy: 503 gets everything.
	boom503 := &CallError{Phase: phaseStatus, Status: 503}
	g := newLoopFixture(t, boom503, boom503, boom503, boom503, boom503, nil)
	if _, err := g.run(pol, oneRung("primary")); err == nil {
		t.Fatal("want failure after five attempts")
	}
	if g.n != 5 {
		t.Fatalf("503 made %d attempts, want the policy's 5", g.n)
	}
}

func TestRetryLoopStopsWhenTheBudgetRunsOut(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 503}
	f := newLoopFixture(t, boom, boom, boom, boom, boom, boom, nil)
	// Ten attempts allowed, but only 15s of waiting: the first two waits are
	// 10s and 20s, so the budget is what stops this, not the attempt count.
	pol := retryPolicy{attempts: 10, base: 10 * time.Second, max: time.Minute, budget: 15 * time.Second}

	_, err := f.run(pol, oneRung("primary"))
	if err == nil {
		t.Fatal("want failure when the budget is exhausted")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error = %q, want it to name the budget — it is the number a user will want to change", err)
	}
	if f.n != 2 {
		t.Fatalf("made %d attempts, want 2 before the budget stopped it", f.n)
	}
	total := time.Duration(0)
	for _, d := range f.slept {
		total += d
	}
	if total > pol.budget {
		t.Errorf("slept %v in total, over the %v budget", total, pol.budget)
	}
}

func TestRetryLoopDoesNotRetryAnUnclassifiedError(t *testing.T) {
	// A plain error, not a *CallError: something in the call path failed in a
	// way this stage does not model. Returned rather than retried, because an
	// unclassified failure retried is a failure repeated.
	f := newLoopFixture(t, errors.New("something else entirely"), nil)
	pol := retryPolicy{attempts: 5, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, oneRung("primary")); err == nil {
		t.Fatal("want the unclassified error returned")
	}
	if f.n != 1 {
		t.Fatalf("made %d attempts on an unclassified error, want 1", f.n)
	}
	if got := f.rec.count(KindCallError); got != 0 {
		t.Errorf("emitted %d call_error events for an error it could not classify", got)
	}
}

func TestRetryLoopHonoursRetryAfterFromTheServer(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 429, RetryAfter: 3 * time.Second}
	f := newLoopFixture(t, boom, nil)
	pol := retryPolicy{attempts: 3, base: 50 * time.Millisecond, max: time.Minute, budget: time.Minute}

	if _, err := f.run(pol, oneRung("primary")); err != nil {
		t.Fatalf("want success on the second attempt: %v", err)
	}
	if len(f.slept) != 1 || f.slept[0] != 3*time.Second {
		t.Fatalf("slept %v, want [3s] from the server's header rather than the 50ms backoff", f.slept)
	}
	// And the reason is in the line, because "waiting 3s" and "waiting 3s
	// because the server asked for 3s" lead to different debugging.
	if txt := f.rec.kind(KindRetry)[0].Text; !strings.Contains(txt, "the server asked for") {
		t.Errorf("retry text = %q, want it to attribute the delay", txt)
	}
}

// ---------------------------------------------------------------------------
// The ladder
// ---------------------------------------------------------------------------

func twoRungs() *ladder {
	return newLadder(
		rung{info: ProviderInfo{Name: "primary", Prices: priceConfig{In: 1, Out: 4}}},
		rung{info: ProviderInfo{Name: "backup", Prices: priceConfig{In: 10, Out: 40}}},
	)
}

func TestFallbackMovesToTheNextRungAndSaysSo(t *testing.T) {
	// The §D11 case: a wrong model name arrives as a 401, and the right move is
	// a different endpoint rather than giving up.
	f := newLoopFixture(t, &CallError{Phase: phaseStatus, Status: 401, Type: "ModelError"}, nil)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}
	lad := twoRungs()

	if _, err := f.run(pol, lad); err != nil {
		t.Fatalf("want success on the backup rung: %v", err)
	}
	if len(f.slept) != 0 {
		t.Errorf("slept %v before falling back — a fallback is not a wait", f.slept)
	}
	evs := f.rec.kind(KindProvider)
	if len(evs) != 1 {
		t.Fatalf("provider events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e.Provider == nil || e.Provider.Name != "backup" {
		t.Fatalf("provider event named %v, want backup", e.Provider)
	}
	if e.Triage != string(TriageFallback) {
		t.Errorf("provider event triage = %q, want fallback — a session-start event carries none", e.Triage)
	}
	// The prices travel with it. Without them the panel keeps billing the
	// backup's tokens at the primary's rates.
	if e.Provider.Prices.In != 10 {
		t.Errorf("provider event carried prices %+v, want the backup's", e.Provider.Prices)
	}
	if !strings.Contains(e.Text, "ModelError") {
		t.Errorf("provider event text = %q, want the reason in it", e.Text)
	}
}

func TestFallbackHappensAfterTheRetriesRunOutToo(t *testing.T) {
	// A retryable failure that keeps failing is worth one look at the ladder
	// before giving up: "the provider is down" and "this provider is down" are
	// different sentences.
	boom := &CallError{Phase: phaseConnect, Message: "connection refused"}
	f := newLoopFixture(t, boom, boom, boom, nil)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, twoRungs()); err != nil {
		t.Fatalf("want success on the backup rung after the retries: %v", err)
	}
	if f.n != 4 {
		t.Fatalf("made %d attempts, want 3 on the primary and 1 on the backup", f.n)
	}
	if got := f.rec.count(KindProvider); got != 1 {
		t.Errorf("provider events = %d, want 1", got)
	}
}

// A rung that has just been fallen back to gets its own full allowance of
// attempts. Carrying the previous rung's count over means a healthy backup is
// abandoned after one failure because the dead primary had already used the
// budget up.
func TestFallbackGivesTheNewRungItsOwnAttempts(t *testing.T) {
	modelErr := &CallError{Phase: phaseStatus, Status: 401, Type: "ModelError"}
	busy := &CallError{Phase: phaseStatus, Status: 503}
	f := newLoopFixture(t, modelErr, busy, busy, nil)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, twoRungs()); err != nil {
		t.Fatalf("want success: the backup was allowed one fallback and then three attempts of its own: %v", err)
	}
	if f.n != 4 {
		t.Fatalf("made %d attempts, want 4 (1 on the primary, 3 on the backup)", f.n)
	}
}

func TestTheLastRungsErrorIsTheOneReported(t *testing.T) {
	f := newLoopFixture(t,
		&CallError{Phase: phaseStatus, Status: 401, Type: "ModelError", Message: "primary has no such model"},
		&CallError{Phase: phaseStatus, Status: 401, Type: "AuthError", Message: "backup key is revoked"},
	)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}

	_, err := f.run(pol, twoRungs())
	if err == nil {
		t.Fatal("want failure once the ladder is exhausted")
	}
	// The reason the session cannot continue is the last thing that refused it,
	// not the first. Reporting the primary's error here would send someone to
	// fix a model name when the actual problem is a revoked key.
	if !strings.Contains(err.Error(), "backup key is revoked") {
		t.Errorf("error = %q, want the last rung's failure", err)
	}
}

// Two subagents failing against the same dead endpoint in the same instant must
// cost one rung, not two. An advance() that simply incremented would skip a
// healthy provider nobody ever tried.
func TestConcurrentFallbacksBurnOneRung(t *testing.T) {
	lad := newLadder(
		rung{info: ProviderInfo{Name: "a"}},
		rung{info: ProviderInfo{Name: "b"}},
		rung{info: ProviderInfo{Name: "c"}},
	)
	at, _, _ := lad.pos()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !lad.advance(at) {
				t.Error("advance reported nowhere to go with two rungs left")
			}
		}()
	}
	wg.Wait()

	_, _, info := lad.pos()
	if info.Name != "b" {
		t.Fatalf("landed on %q after eight concurrent fallbacks from rung 0, want b", info.Name)
	}
}

// Where the guard in advance() actually earns its place, and it is not the case
// the eight-goroutine test above covers: that one has every caller on the same
// rung, and `cur = from + 1` is idempotent for those by itself.
//
// The failure needs three participants and it is a rewind. A falls off rung 0
// and lands on 1. C falls off 1 and lands on 2. Then B — still holding the rung
// 0 it read before any of that — asks to fall back, and without the guard it
// writes cur = 1 and sends the next call to a provider two siblings have already
// given up on. This is the only test in the file that would notice.
func TestAdvanceNeverMovesTheLadderBackwards(t *testing.T) {
	lad := newLadder(
		rung{info: ProviderInfo{Name: "a"}},
		rung{info: ProviderInfo{Name: "b"}},
		rung{info: ProviderInfo{Name: "c"}},
	)
	if !lad.advance(0) {
		t.Fatal("advance off rung 0 failed")
	}
	if !lad.advance(1) {
		t.Fatal("advance off rung 1 failed")
	}
	if _, _, info := lad.pos(); info.Name != "c" {
		t.Fatalf("after two steps the ladder is on %q, want c", info.Name)
	}

	// The straggler. It must be told yes — there IS somewhere else to send this
	// — without the ladder losing the ground the others gained.
	if !lad.advance(0) {
		t.Error("a straggler still on rung 0 was told there is nowhere to go")
	}
	if _, _, info := lad.pos(); info.Name != "c" {
		t.Fatalf("the ladder rewound to %q; a straggler's stale rung must not undo a sibling's fallback", info.Name)
	}
}

func TestAdvanceReportsFalseAtTheEndOfTheLadder(t *testing.T) {
	lad := oneRung("only")
	if lad.advance(0) {
		t.Fatal("advance said yes on a one-rung ladder")
	}
	var nilLadder *ladder
	if nilLadder.advance(0) {
		t.Fatal("advance said yes on a nil ladder")
	}
	if _, p, _ := nilLadder.pos(); p != nil {
		t.Fatal("pos on a nil ladder returned a provider")
	}
}

// ---------------------------------------------------------------------------
// buildLadder
// ---------------------------------------------------------------------------

func TestBuildLadderValidatesEveryRungAtStartup(t *testing.T) {
	t.Setenv("TRIAGE_KEY_A", "sk-a")
	t.Setenv("TRIAGE_KEY_B", "sk-b")
	pf := &providersFile{
		Default: "a",
		Providers: map[string]providerConfig{
			"a": {Protocol: "openai", BaseURL: "https://a.example", APIKeyEnv: "TRIAGE_KEY_A", Model: "m-a", Prices: priceConfig{In: 1}},
			"b": {Protocol: "anthropic", BaseURL: "https://b.example", APIKeyEnv: "TRIAGE_KEY_B", Model: "m-b", Prices: priceConfig{In: 2}},
			"c": {Protocol: "openai", BaseURL: "https://c.example", APIKeyEnv: "TRIAGE_KEY_MISSING", Model: "m-c"},
		},
	}
	primary, err := pf.Providers["a"].build(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("primary alone", func(t *testing.T) {
		lad, err := buildLadder(pf, "a", pf.Providers["a"], primary, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(lad.rungs) != 1 {
			t.Fatalf("rungs = %d, want 1", len(lad.rungs))
		}
		if lad.rungs[0].info.Name != "a" || lad.rungs[0].info.Model != "m-a" {
			t.Fatalf("rung 0 = %+v", lad.rungs[0].info)
		}
	})

	t.Run("with a fallback", func(t *testing.T) {
		lad, err := buildLadder(pf, "a", pf.Providers["a"], primary, " b ", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(lad.rungs) != 2 || lad.rungs[1].info.Name != "b" {
			t.Fatalf("rungs = %+v", lad.rungs)
		}
		// Protocol comes off the built provider, not off the config string, so
		// a rung whose protocol and model disagree cannot exist.
		if lad.rungs[1].info.Protocol != "anthropic" {
			t.Errorf("rung 1 protocol = %q", lad.rungs[1].info.Protocol)
		}
	})

	t.Run("a duplicate is refused", func(t *testing.T) {
		// A ladder listing the same provider twice reads as extra resilience
		// and delivers none: the second rung fails for the reason the first one
		// did, and all it buys is a longer wait before giving up.
		if _, err := buildLadder(pf, "a", pf.Providers["a"], primary, "b,b", true); err == nil {
			t.Fatal("want an error for a repeated fallback")
		}
		if _, err := buildLadder(pf, "a", pf.Providers["a"], primary, "a", true); err == nil {
			t.Fatal("want an error when the fallback is the primary")
		}
	})

	t.Run("an unknown name fails at startup", func(t *testing.T) {
		_, err := buildLadder(pf, "a", pf.Providers["a"], primary, "nope", true)
		if err == nil {
			t.Fatal("want an error for an unknown provider name")
		}
	})

	t.Run("a missing key fails at startup, not during the outage", func(t *testing.T) {
		// The whole reason every rung is built eagerly. A fallback constructed
		// on demand fails on demand, and the moment it is needed is the only
		// moment it exists for.
		_, err := buildLadder(pf, "a", pf.Providers["a"], primary, "c", true)
		if err == nil {
			t.Fatal("want an error when a fallback's key env var is empty")
		}
		if !strings.Contains(err.Error(), "c") {
			t.Errorf("error = %q, want it to name the rung", err)
		}
	})

	t.Run("empty entries are skipped", func(t *testing.T) {
		lad, err := buildLadder(pf, "a", pf.Providers["a"], primary, "b,,", true)
		if err != nil || len(lad.rungs) != 2 {
			t.Fatalf("rungs=%v err=%v", lad, err)
		}
	})
}

// ---------------------------------------------------------------------------
// modelCall over a real transport
// ---------------------------------------------------------------------------

func triageProvider(t *testing.T, srv *httptest.Server) Provider {
	t.Helper()
	return newOpenAIProvider(srv.URL, "sk-test", "test-model")
}

// The envelope arrives as text/plain on this gateway (§D11), which is exactly
// why nothing in parseErrorBody looks at the content type. A client that
// branched on it would report "unparseable error" for every error the endpoint
// produces.
func TestModelCallClassifiesAnErrorServedAsTextPlain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
		w.WriteHeader(401)
		io.WriteString(w, `{"type":"error","error":{"type":"ModelError","message":"Model nope is not supported"}}`)
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	_, err := modelCall(context.Background(), triageProvider(t, srv), srv.Client(), NewBus(rec), 1, "sys",
		[]Msg{TextMsg(RoleUser, "hi")}, nil, 64, deadlines{}, nil)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *CallError", err, err)
	}
	if ce.Phase != phaseStatus || ce.Status != 401 || ce.Type != "ModelError" {
		t.Fatalf("CallError = %+v", ce)
	}
	if ce.triage() != TriageFallback {
		t.Errorf("triage = %q, want fallback", ce.triage())
	}
}

func TestModelCallReadsRetryAfterOffTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	_, err := modelCall(context.Background(), triageProvider(t, srv), srv.Client(), NewBus(rec), 1, "sys",
		[]Msg{TextMsg(RoleUser, "hi")}, nil, 64, deadlines{}, nil)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v, want a *CallError", err)
	}
	// The first response header this repo has ever read. Before stage 09 the
	// agent could not have honoured a Retry-After if one had arrived.
	if ce.RetryAfter != 2*time.Second {
		t.Fatalf("RetryAfter = %v, want 2s", ce.RetryAfter)
	}
}

// The seam this stage came to fix. Both adapters return a partial result
// alongside a stream error, on purpose, and every stage before this one bound
// that value and dropped it.
func TestModelCallKeepsThePartialWhenTheStreamBreaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A Content-Length longer than the body, so the client's read of the
		// body fails with an unexpected EOF rather than ending cleanly. A
		// stream that merely stops is not an error — sse.go flushes the last
		// frame on EOF, deliberately — so this is how a genuinely broken
		// connection is reproduced.
		w.Header().Set("Content-Length", "4096")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answ\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	res, err := modelCall(context.Background(), triageProvider(t, srv), srv.Client(), NewBus(rec), 1, "sys",
		[]Msg{TextMsg(RoleUser, "hi")}, nil, 64, deadlines{}, nil)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *CallError", err, err)
	}
	if ce.Phase != phaseStream {
		t.Fatalf("Stage = %q, want stream", ce.Phase)
	}
	if ce.Partial == nil {
		t.Fatal("Partial is nil: the accumulated text was dropped, which is the bug stage 09 fixed")
	}
	if !strings.Contains(ce.Partial.Text, "partial answ") {
		t.Errorf("Partial.Text = %q, want the text that did arrive", ce.Partial.Text)
	}
	if res == nil || res.Text != ce.Partial.Text {
		t.Errorf("the returned result and the partial disagree: %v vs %q", res, ce.Partial.Text)
	}
	if ce.triage() != TriageRetry {
		t.Errorf("triage = %q, want retry", ce.triage())
	}
	// And the trace says the response broke rather than ended: no
	// response_end, which is the signal the adapters have carried since stage
	// 02 and stage 09 finally reads.
	if got := rec.count(KindResponseEnd); got != 0 {
		t.Errorf("emitted %d response_end events for a stream that broke", got)
	}
}

// The other way a stream fails, and it takes the other branch of modelCall: the
// provider sends an `error` EVENT mid-body. anthropic.go turns that into a
// *CallError carrying the provider's own error.type, and modelCall has to
// *enrich* that rather than wrap it — keeping the type reachable by the
// classifier and attaching the partial to it.
//
// Not observed on this gateway (§D11's errors all arrive as an HTTP status
// before the stream opens) but it is in the spec, and it is the path
// `overloaded_error` takes when a provider degrades mid-response.
func TestModelCallEnrichesAnInStreamErrorWithThePartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"m","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"halfway through a "}}`,
			`{"type":"error","error":{"type":"overloaded_error","message":"capacity"}}`,
		} {
			io.WriteString(w, "data: "+frame+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	p := newAnthropicProvider(srv.URL, "sk-test", "test-model")
	_, err := modelCall(context.Background(), p, srv.Client(), NewBus(rec), 1, "sys",
		[]Msg{TextMsg(RoleUser, "hi")}, nil, 64, deadlines{}, nil)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *CallError", err, err)
	}
	if ce.Type != "overloaded_error" {
		t.Fatalf("Type = %q, want the provider's own error.type — wrapping instead of enriching loses it", ce.Type)
	}
	if ce.triage() != TriageRetry {
		t.Errorf("triage = %q, want retry: overloaded_error is the canonical retryable condition", ce.triage())
	}
	if ce.Partial == nil {
		t.Fatal("Partial is nil on the enrichment branch: the text that did arrive was dropped")
	}
	if !strings.Contains(ce.Partial.Text, "halfway through a ") {
		t.Errorf("Partial.Text = %q, want the text that arrived before the error event", ce.Partial.Text)
	}
	if got := rec.count(KindResponseEnd); got != 0 {
		t.Errorf("emitted %d response_end events for a stream that carried an error", got)
	}
}

// A retried call must rebuild its request. An *http.Request's body is a
// consumed reader after the first Do, so a loop that reused the request object
// would send zero bytes on attempt two — a retry bug that looks exactly like a
// server bug.
func TestRetriedCallsSendTheWholeBodyEveryTime(t *testing.T) {
	var mu sync.Mutex
	var lengths []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lengths = append(lengths, len(body))
		n := len(lengths)
		mu.Unlock()

		if n < 3 {
			w.WriteHeader(503)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	bus := NewBus(rec)
	p := triageProvider(t, srv)
	pol := retryPolicy{attempts: 3, base: time.Nanosecond, max: time.Millisecond, budget: time.Second}

	res, err := retryLoop(context.Background(), bus, 1, pol, newLadder(rung{p: p}),
		func(context.Context, time.Duration) error { return nil }, func() float64 { return 1 },
		func(ctx context.Context, pr Provider) (*CallResult, error) {
			return modelCall(ctx, pr, srv.Client(), bus, 1, "sys",
				[]Msg{TextMsg(RoleUser, "hi")}, nil, 64, deadlines{}, nil)
		})
	if err != nil {
		t.Fatalf("want success on the third attempt: %v", err)
	}
	if res.Text != "hi" {
		t.Errorf("res.Text = %q", res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lengths) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(lengths))
	}
	for i, n := range lengths {
		if n != lengths[0] || n == 0 {
			t.Errorf("request %d had a body of %d bytes, want %d — the retry re-sent a consumed reader",
				i+1, n, lengths[0])
		}
	}
}

// ---------------------------------------------------------------------------
// The panel
// ---------------------------------------------------------------------------

// A fallback changes the cost basis mid-session, and the panel has to follow it.
// Billing the backup's tokens at the primary's rates produces a report that is
// confidently wrong, which is worse than one that admits it does not know.
func TestThePanelRepricesOnFallback(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)

	usage := func(n int) Event {
		return Event{Kind: KindUsage, Usage: &Usage{Input: n, Output: 0}}
	}
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{
		Name: "primary", Window: 1000, Prices: priceConfig{In: 1}}}) // $1 / 1M
	r.OnEvent(usage(1_000_000)) // $1.00
	r.OnEvent(Event{Kind: KindProvider, Triage: string(TriageFallback), Text: "fell back",
		Provider: &ProviderInfo{Name: "backup", Window: 2000, Prices: priceConfig{In: 10}}}) // $10 / 1M
	r.OnEvent(usage(1_000_000)) // $10.00

	if got := r.sessionCost; got != 11 {
		t.Fatalf("sessionCost = %v, want 11 (1 at the primary's rate + 10 at the backup's)", got)
	}
	// The window travels too: the context watermark is a fraction of a number
	// that just changed.
	if r.window != 2000 {
		t.Errorf("window = %d, want the backup's 2000", r.window)
	}
	if !strings.Contains(out.String(), "backup") {
		t.Errorf("the fallback was not announced:\n%s", out.String())
	}
}

// A session-start provider event must not be announced as a fallback: the
// banner already named the provider, and a line saying "provider →" on every
// clean startup is how people learn to skip the block.
func TestSessionStartProviderEventIsSilent(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "primary", Prices: priceConfig{In: 3}}})

	if out.String() != "" {
		t.Errorf("session start printed %q, want nothing", out.String())
	}
	if r.prices.in != 3 {
		t.Errorf("prices were not adopted: %+v", r.prices)
	}
}

// The number no other agent reports: what the failed attempts cost.
func TestTheSummaryReportsWhatRetriesReBilled(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "p", Prices: priceConfig{In: 1_000_000}}})

	// Two streams that opened and died, then the attempt that worked and
	// finally reports a real prompt figure. The estimate is made at that moment
	// because it is the only moment a real number for this prompt exists.
	//
	// Phase matters here and not merely for tidiness: see the companion test
	// below, and the comment in render.go about the live run that got this
	// wrong.
	r.OnEvent(Event{Kind: KindCallError, Attempt: 1, Phase: string(phaseStream), Triage: string(TriageRetry), Text: "stream broke"})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 2, Millis: 500})
	r.OnEvent(Event{Kind: KindCallError, Attempt: 2, Phase: string(phaseStream), Triage: string(TriageRetry), Text: "stream broke"})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 3, Millis: 1000})
	r.OnEvent(Event{Kind: KindUsage, Usage: &Usage{Input: 10, CacheRead: 90, Output: 5}})

	if got := r.rebilled.Prompt(); got != 200 {
		t.Fatalf("rebilled = %d, want 200 (two failed attempts at the successful attempt's 100-token prompt)", got)
	}
	// The split is preserved, not collapsed into full-price input: that is what
	// makes this a defensible lower bound rather than a scare number.
	if r.rebilled.Input != 20 || r.rebilled.CacheRead != 180 {
		t.Errorf("rebilled split = %+v, want the successful attempt's shape doubled", r.rebilled)
	}
	// The per-call multiplier resets, so the next clean call is not charged
	// again. The session's retry count does not: it is a total.
	if r.billedFailures != 0 {
		t.Errorf("billedFailures = %d after the usage frame, want 0", r.billedFailures)
	}
	if r.retries != 2 {
		t.Errorf("retries = %d, want the session total 2", r.retries)
	}

	out.Reset()
	r.SessionSummary(100)
	s := out.String()
	if !strings.Contains(s, "2 retries") {
		t.Errorf("summary does not report the retry count:\n%s", s)
	}
	if !strings.Contains(s, "200 prompt tokens") {
		t.Errorf("summary does not report the re-billed tokens:\n%s", s)
	}
	// Reported as a bound, because a cold call pays the cache write on its
	// first attempt and the cheaper read on the retry, so copying the
	// successful attempt's split under-charges the first one.
	if !strings.Contains(s, "\u2265") {
		t.Errorf("summary states the estimate as exact:\n%s", s)
	}
}

// The correction. A request the server refused was never generated, so it was
// never billed, and charging for it turns the one honest number in this panel
// into a scare story.
//
// This test exists because the first live run of stage 09 printed "re-sent
// ≥1926 prompt tokens (≥$0.000301)" for a session whose real cost was
// $0.000276 — after two injected 503s that cost nothing at all.
func TestARefusedRequestIsNotReBilled(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "p", Prices: priceConfig{In: 1_000_000}}})

	// Refused before generation: a 503 at the status phase, and a refused
	// connection. Two retries, zero tokens.
	r.OnEvent(Event{Kind: KindCallError, Attempt: 1, Phase: string(phaseStatus), Status: 503, Triage: string(TriageRetry)})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 2, Millis: 500})
	r.OnEvent(Event{Kind: KindCallError, Attempt: 2, Phase: string(phaseConnect), Triage: string(TriageRetry)})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 3, Millis: 1000})
	r.OnEvent(Event{Kind: KindUsage, Usage: &Usage{Input: 963, Output: 41}})

	if r.rebilled.Prompt() != 0 {
		t.Fatalf("rebilled = %d after two refused requests, want 0 — nothing was generated, so nothing was billed",
			r.rebilled.Prompt())
	}
	if r.rebilledCost != 0 {
		t.Fatalf("rebilledCost = %v, want 0", r.rebilledCost)
	}
	// The retries still happened and the session still lost the time. What did
	// not happen is a charge.
	out.Reset()
	r.SessionSummary(963)
	if strings.Contains(out.String(), "re-sent") {
		t.Errorf("summary invented a re-bill for refused requests:\n%s", out.String())
	}
}

func TestACleanSessionSaysNothingAboutRetries(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{in: 1}, 0)
	r.OnEvent(Event{Kind: KindUsage, Usage: &Usage{Input: 100, Output: 5}})
	r.SessionSummary(100)
	if strings.Contains(out.String(), "retried") {
		t.Errorf("a clean session mentioned retries:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// The trace
// ---------------------------------------------------------------------------

// Kind strings and json tags are written into trace files, so renaming one
// silently breaks replay of every session recorded before the rename
// (events.go says so). This pins the stage 09 additions by name.
func TestStage09EventFieldsSurviveTheTrace(t *testing.T) {
	in := Event{
		Kind: KindCallError, Turn: 3, Status: 429, Phase: string(phaseStatus), ErrType: "rate_limit_error",
		Triage: string(TriageRetry), Attempt: 2, Millis: 1500,
		Provider: &ProviderInfo{Name: "backup", Protocol: "anthropic", Model: "m", Window: 2048,
			Prices: priceConfig{In: 1, Out: 2, CacheRead: 0.1, CacheWrite: 1.25}},
	}
	out := roundTripEvent(t, in)

	if out.Kind != KindCallError || out.Status != 429 || out.Phase != "status" || out.ErrType != "rate_limit_error" {
		t.Fatalf("round trip lost the failure fields: %+v", out)
	}
	if out.Triage != string(TriageRetry) || out.Attempt != 2 {
		t.Fatalf("round trip lost the decision fields: %+v", out)
	}
	if out.Provider == nil {
		t.Fatal("round trip lost the provider")
	}
	if out.Provider.Name != "backup" || out.Provider.Window != 2048 || out.Provider.Prices.CacheWrite != 1.25 {
		t.Fatalf("round trip mangled the provider: %+v", *out.Provider)
	}
	// The wire names, spelled out. A field renamed in Go is invisible; a json
	// tag renamed in Go breaks every archived trace, and this is the assertion
	// that notices.
	raw := traceLineFor(t, in)
	for _, key := range []string{`"kind":"call_error"`, `"status":429`, `"phase":"status"`, `"err_type":`, `"triage":`, `"attempt":`, `"provider":`} {
		if !strings.Contains(raw, key) {
			t.Errorf("trace line is missing %s:\n%s", key, raw)
		}
	}
}

// A survived failure is not an error the session suffered. Folding the two
// counters together makes every robust session look broken, and a header nobody
// believes is a header nobody reads.
func TestSummarizeCountsFailuresApartFromErrors(t *testing.T) {
	s := Summarize([]Event{
		{Seq: 1, Kind: KindProvider, Provider: &ProviderInfo{Name: "primary"}}, // session start: not a fallback
		{Seq: 2, Kind: KindCallError, Status: 503, Triage: string(TriageRetry)},
		{Seq: 3, Kind: KindRetry, Millis: 500},
		{Seq: 4, Kind: KindCallError, Status: 401, Triage: string(TriageFallback)},
		{Seq: 5, Kind: KindProvider, Triage: string(TriageFallback), Provider: &ProviderInfo{Name: "backup"}},
		{Seq: 6, Kind: KindRetry, Millis: 900},
		{Seq: 7, Kind: KindError, Text: "the session gave up"},
	})

	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1 — only the terminal one", s.Errors)
	}
	if s.CallErrors != 2 {
		t.Errorf("CallErrors = %d, want 2", s.CallErrors)
	}
	if s.Retries != 2 {
		t.Errorf("Retries = %d, want 2", s.Retries)
	}
	if s.Fallbacks != 1 {
		t.Errorf("Fallbacks = %d, want 1 — the session-start provider event is not a fallback", s.Fallbacks)
	}

	head := s.String()
	for _, want := range []string{"1 error", "2 failed calls", "2 retries", "1 fallback"} {
		if !strings.Contains(head, want) {
			t.Errorf("header is missing %q:\n%s", want, head)
		}
	}
}

// An empty event must stay short. Every one of the stage 09 fields is
// omitempty, and a missing tag would put six zeroes on every single line of
// every trace — the reason events.go asks for them.
func TestStage09FieldsAreOmittedWhenEmpty(t *testing.T) {
	raw := traceLineFor(t, Event{Kind: KindTurnStart, Turn: 1})
	for _, key := range []string{"status", "phase", "err_type", "triage", "attempt", "provider"} {
		if strings.Contains(raw, key) {
			t.Errorf("a turn_start line carries %q:\n%s", key, raw)
		}
	}
}
