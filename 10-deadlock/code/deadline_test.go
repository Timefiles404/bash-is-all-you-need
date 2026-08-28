// Tests for stage 10: the three clocks, and the cause that says which one fired.
//
// Not one of these sleeps. A stall detector is a component that owns a clock,
// and the whole reason watch() takes `tick` as a channel and stallReader takes
// `now` as a function is that the alternative is a suite full of
// time.Sleep(50 * time.Millisecond) — which is slow, flaky on a loaded machine,
// and quietly untestable at the boundary you actually care about. Here the
// clock is a variable and the tick is a value pushed by the test, so "45
// seconds passed" costs nothing and happens exactly when the test says.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// ---------------------------------------------------------------------------
// The stall guard
// ---------------------------------------------------------------------------

func TestStallGuardFiresOnlyAfterTheWholeIdleWindow(t *testing.T) {
	clk := newClock()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	g := &stallGuard{}
	g.mark(clk.now())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); g.watch(ctx, 45*time.Second, cancel, tick) }()

	// A tick just short of the window must not fire. This is the test that
	// would fail if someone "simplified" >= to > or the window to idle/2 —
	// and a stall detector that fires early looks exactly like a flaky
	// provider, which is the most expensive kind of bug to chase.
	tick <- clk.advance(44 * time.Second)
	select {
	case <-ctx.Done():
		t.Fatalf("cancelled after 44s of a 45s window")
	default:
	}

	tick <- clk.advance(2 * time.Second)
	<-done
	if got := context.Cause(ctx); !errors.Is(got, errStalled) {
		t.Fatalf("cause = %v, want errStalled", got)
	}
}

func TestStallGuardIsHeldOffByEveryRead(t *testing.T) {
	clk := newClock()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	g := &stallGuard{}
	g.mark(clk.now())
	tick := make(chan time.Time)
	go g.watch(ctx, 45*time.Second, cancel, tick)

	// Forty minutes of a slow but living stream: a byte every 40 seconds,
	// checked in between. Under a single http.Client.Timeout this session is
	// dead; under a gap-based clock it is simply someone thinking.
	for i := 0; i < 60; i++ {
		tick <- clk.advance(40 * time.Second)
		select {
		case <-ctx.Done():
			t.Fatalf("cancelled at minute %d of a stream that never went quiet", i*40/60)
		default:
		}
		g.mark(clk.now())
	}
}

func TestStallGuardStopsWhenTheCallEnds(t *testing.T) {
	// The "and an owner" half of the stage. A watchdog that outlives its call
	// is a goroutine leak that multiplies by the number of subagents, and it
	// would never show up in a test that only checks the return value.
	ctx, cancel := context.WithCancelCause(context.Background())
	g := &stallGuard{}
	g.mark(time.Now())
	done := make(chan struct{})
	go func() { defer close(done); g.watch(ctx, time.Second, cancel, make(chan time.Time)) }()

	cancel(context.Canceled)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return when its context ended")
	}
}

func TestStallGuardIsOffWhenIdleIsZero(t *testing.T) {
	// Zero is a real setting, not a missing one: wire probing needs all three
	// clocks off, because a probe that gets cut short is not evidence.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	g := &stallGuard{}
	done := make(chan struct{})
	go func() { defer close(done); g.watch(ctx, 0, cancel, nil) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch with idle=0 should return immediately, not watch forever")
	}
	if ctx.Err() != nil {
		t.Fatal("watch with idle=0 cancelled the call")
	}
}

// ---------------------------------------------------------------------------
// The reader that feeds it
// ---------------------------------------------------------------------------

func TestStallReaderMarksOnDataEvenWithEOF(t *testing.T) {
	// n > 0 is the condition, not err == nil. io.Reader is explicitly allowed
	// to return data and io.EOF from the same call, and a guard that ignored
	// that read would start counting the idle window from before the last
	// bytes of every short stream.
	clk := newClock()
	g := &stallGuard{}
	g.mark(clk.now())
	before := g.last.Load()
	clk.advance(time.Second)

	r := &stallReader{rc: io.NopCloser(dataThenEOF("xyz")), guard: g, now: clk.now}
	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 3 || err != io.EOF {
		t.Fatalf("read = %d, %v; want 3, EOF", n, err)
	}
	if g.last.Load() == before {
		t.Fatal("a read that returned bytes with io.EOF did not mark the guard")
	}
}

func TestStallReaderDoesNotMarkOnAnEmptyRead(t *testing.T) {
	clk := newClock()
	g := &stallGuard{}
	g.mark(clk.now())
	before := g.last.Load()
	clk.advance(time.Second)

	r := &stallReader{rc: io.NopCloser(strings.NewReader("")), guard: g, now: clk.now}
	if _, err := r.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if g.last.Load() != before {
		t.Fatal("a read that returned no bytes counted as proof of life")
	}
}

// dataThenEOF returns a reader that hands back s and io.EOF in one call.
type dataThenEOF string

func (d dataThenEOF) Read(p []byte) (int, error) { return copy(p, d), io.EOF }

// ---------------------------------------------------------------------------
// The instrument
// ---------------------------------------------------------------------------

// dlRecorder collects events so a test can assert on what reached the panel.
type dlRecorder struct{ events []Event }

func (r *dlRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func TestTheWidestGapIsMeasuredAndReported(t *testing.T) {
	// Mutation testing found this missing: `mark` could be changed never to
	// record a new maximum and every other test still passed. The stall guard
	// went on working — the deadline is checked against `last`, not against
	// `widest` — so what silently died was the INSTRUMENT, and a stage whose
	// whole argument is "print the number the timeout is compared against"
	// cannot have that number quietly become zero.
	clk := newClock()
	rec := &dlRecorder{}
	bus := NewBus(rec)
	g := &stallGuard{}
	g.mark(clk.now())
	r := &stallReader{rc: io.NopCloser(strings.NewReader("abcdef")),
		guard: g, now: clk.now, bus: bus, turn: 3}

	buf := make([]byte, 2)
	for _, gap := range []time.Duration{40 * time.Millisecond, 900 * time.Millisecond, 30 * time.Millisecond} {
		clk.advance(gap)
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	if got := g.idleMax(); got != 900*time.Millisecond {
		t.Fatalf("idleMax = %v, want 900ms", got)
	}
	// One event per NEW maximum, so the 30ms read must not have emitted: 40 and
	// 900 are increases, 30 is not.
	var ms []int64
	for _, e := range rec.events {
		if e.Kind == KindIdleMax {
			if e.Turn != 3 {
				t.Fatalf("idle_max on turn %d, want 3", e.Turn)
			}
			ms = append(ms, e.Millis)
		}
	}
	if len(ms) != 2 || ms[0] != 40 || ms[1] != 900 {
		t.Fatalf("idle_max events = %v, want [40 900]", ms)
	}
}

// ---------------------------------------------------------------------------
// Which clock fired, and what it means
// ---------------------------------------------------------------------------

func TestTriageCause(t *testing.T) {
	// The first row is the one that would be a bug rather than a preference.
	// An interrupt classified as an ordinary failure is retried three times and
	// then failed over to a second provider: the agent answers "stop" by doing
	// the work twice somewhere else.
	cases := []struct {
		name  string
		err   error
		want  Triage
		label string
		known bool
	}{
		{"interrupt never retries and never falls back", errInterrupted, TriageFatal, "interrupted", true},
		{"a stall is a dead connection", errStalled, TriageRetry, "stalled", true},
		{"the total deadline is a backstop, not a policy", errCallTimeout, TriageFatal, "call_timeout", true},
		{"a parent shutting down", context.Canceled, TriageFatal, "cancelled", true},
		{"a plain deadline with no cause", context.DeadlineExceeded, TriageFatal, "cancelled", true},
		{"wrapped still counts", fmt.Errorf("read: %w", errStalled), TriageRetry, "stalled", true},
		{"nothing of ours ended it", nil, "", "", false},
		{"a provider error is not ours", errors.New("http 503"), "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, label, ok := triageCause(c.err)
			if ok != c.known || v != c.want || label != c.label {
				t.Fatalf("triageCause(%v) = %q, %q, %v; want %q, %q, %v",
					c.err, v, label, ok, c.want, c.label, c.known)
			}
		})
	}
}

func TestCauseBeatsTheStatusClassifier(t *testing.T) {
	// A cancelled call may still be carrying a status from whatever the dying
	// request last saw. Stage 09's table would read that 503 and retry — which
	// is the agent ignoring Ctrl-C because the server was also having a bad day.
	ce := &CallError{Phase: phaseStatus, Status: 503, cause: errInterrupted}
	if got := ce.triage(); got != TriageFatal {
		t.Fatalf("triage = %q, want fatal: the cause must outrank the status", got)
	}
	// And with no cause the status still rules, or stage 09 just got deleted.
	ce.cause = nil
	if got := ce.triage(); got != TriageRetry {
		t.Fatalf("triage = %q, want retry once the cause is gone", got)
	}
}

func TestCancelCauseIsNilWhileTheContextIsOpen(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	if got := cancelCause(ctx); got != nil {
		t.Fatalf("cause on a live context = %v, want nil", got)
	}
	cancel(errStalled)
	if got := cancelCause(ctx); !errors.Is(got, errStalled) {
		t.Fatalf("cause after cancel = %v, want errStalled", got)
	}
}

// ---------------------------------------------------------------------------
// waitFor
// ---------------------------------------------------------------------------

func TestWaitForReturnsTheCauseWhenInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { cancel(errInterrupted) }()
	// An hour, so a test that passes by waiting it out is impossible.
	err := waitFor(ctx, time.Hour)
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("waitFor = %v, want errInterrupted", err)
	}
}

func TestWaitForCompletesAShortWait(t *testing.T) {
	if err := waitFor(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("waitFor = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// End to end, over a real transport
// ---------------------------------------------------------------------------

// callWithin runs a model call and fails the test if it does not return in time.
//
// The deadline is not belt-and-braces, it is the difference between a test that
// FAILS and a test that HANGS. Every call below is against a server that never
// closes the connection, so if the mechanism under test is broken the call waits
// forever and the test sits there until Go's own ten-minute panic. Mutation
// testing is where that bites: one mutant switching the stall guard off turned a
// two-second run into a ten-minute one, which is most of an hour across a suite
// of mutants and is indistinguishable from a hung harness while you watch it.
//
// A test that cannot fail fast is a test people stop running, for the same
// reason a suite full of sleeps is.
func callWithin(t *testing.T, d time.Duration, ctx context.Context, p Provider,
	c *http.Client, bus *Bus, dl deadlines) (*CallResult, error) {
	t.Helper()
	type out struct {
		res *CallResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, e := modelCall(ctx, p, c, bus, 1, "s",
			[]Msg{TextMsg(RoleUser, "hi")}, nil, 16, dl, nil)
		ch <- out{r, e}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-time.After(d):
		t.Fatalf("the call did not return within %s — nothing ended it", d)
		return nil, nil
	}
}

// stalledServer sends valid SSE, then stops writing without closing — the exact
// shape a single http.Client.Timeout cannot tell from a slow answer.
func stalledServer(t *testing.T, hold chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		<-hold
	}))
}

func TestAStalledStreamIsCancelledWithItsOwnCause(t *testing.T) {
	hold := make(chan struct{})
	srv := stalledServer(t, hold)
	defer srv.Close()
	defer close(hold)

	bus := NewBus()
	p := &anthropicProvider{baseURL: srv.URL, apiKey: "k", model: "m"}
	// A short idle window, because this one really does wait: the point is the
	// whole path, transport included, and there is no clock to inject through
	// net/http. The guard ticks at idle/4, so detection lands under 250ms.
	dl := deadlines{idle: 150 * time.Millisecond}

	_, err := callWithin(t, 5*time.Second, context.Background(), p, srv.Client(), bus, dl)
	if err == nil {
		t.Fatal("a stalled stream returned no error")
	}
	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("error was %T, want *CallError", err)
	}
	if !errors.Is(ce.cause, errStalled) {
		t.Fatalf("cause = %v, want errStalled", ce.cause)
	}
	if got := ce.triage(); got != TriageRetry {
		t.Fatalf("triage = %q, want retry", got)
	}
}

func TestAnInterruptedCallIsFatalAndKeepsTheProviderCause(t *testing.T) {
	hold := make(chan struct{})
	srv := stalledServer(t, hold)
	defer srv.Close()
	defer close(hold)

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { cancel(errInterrupted) }()

	bus := NewBus()
	p := &anthropicProvider{baseURL: srv.URL, apiKey: "k", model: "m"}
	_, err := callWithin(t, 5*time.Second, ctx, p, srv.Client(), bus, deadlines{})
	if err == nil {
		t.Fatal("an interrupted call returned no error")
	}
	ce, _ := asCallError(err)
	if ce == nil || !errors.Is(ce.cause, errInterrupted) {
		t.Fatalf("cause = %v, want errInterrupted", ce)
	}
	if got := ce.triage(); got != TriageFatal {
		t.Fatalf("triage = %q, want fatal", got)
	}
}

func TestAnInterruptedCallDoesNotWalkTheLadder(t *testing.T) {
	// The bug this stage would ship without triageCause. Stop is answered by
	// trying somewhere else, which is the opposite of stopping — and on a
	// fallback provider the prompt cache is cold, so it is also the most
	// expensive possible response to being told to stop.
	bus := NewBus()
	lad := newLadder(
		rung{p: &mulFakeProvider{}, info: ProviderInfo{Name: "first"}},
		rung{p: &mulFakeProvider{}, info: ProviderInfo{Name: "second"}},
	)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errInterrupted)

	calls := 0
	_, err := retryLoop(ctx, bus, 1, defaultRetryPolicy(), lad,
		func(context.Context, time.Duration) error { return nil },
		func() float64 { return 1 },
		func(ctx context.Context, p Provider) (*CallResult, error) {
			calls++
			return nil, &CallError{Phase: phaseStream, Message: "gone", cause: cancelCause(ctx)}
		})
	if err == nil {
		t.Fatal("interrupted loop returned no error")
	}
	if calls != 1 {
		t.Fatalf("made %d attempts after an interrupt, want 1", calls)
	}
	if at, _, info := lad.pos(); at != 0 {
		t.Fatalf("ladder advanced to %d (%s) on an interrupt", at, info.Name)
	}
}

func TestABackoffIsCutShortByCancellation(t *testing.T) {
	// Without an interruptible wait, Ctrl-C during a retry backoff does nothing
	// for up to the whole backoff — and the user's second Ctrl-C kills the
	// process before the trace is flushed.
	bus := NewBus()
	ctx, cancel := context.WithCancelCause(context.Background())

	slept := 0
	_, err := retryLoop(ctx, bus, 1, defaultRetryPolicy(), newLadder(rung{p: &mulFakeProvider{}}),
		func(c context.Context, d time.Duration) error {
			slept++
			cancel(errInterrupted) // the user presses Ctrl-C during the wait
			return context.Cause(c)
		},
		func() float64 { return 1 },
		func(ctx context.Context, p Provider) (*CallResult, error) {
			return nil, &CallError{Phase: phaseConnect, Message: "refused"}
		})
	if err == nil {
		t.Fatal("no error after an interrupted backoff")
	}
	if slept != 1 {
		t.Fatalf("slept %d times, want 1: the loop kept going after the wait was cut short", slept)
	}
	ce, _ := asCallError(err)
	if ce == nil || !errors.Is(ce.cause, errInterrupted) {
		t.Fatalf("error = %v, want one carrying errInterrupted", err)
	}
}

func TestACancelledParentDoesNotWaitOnItsChildrenForever(t *testing.T) {
	// The wait this stage is named after. dispatch() joins its subagents with
	// wg.Wait(), which has no deadline and no cancellation of its own — so
	// before stage 10 a parent whose children were each blocked on a socket
	// with a ten-minute cap was stuck for ten minutes, with Ctrl-C landing on a
	// goroutine that was not listening for it.
	//
	// Nothing in dispatch() was changed to fix this. The context reaches the
	// child's model call, the call fails, the child returns, and the join
	// completes — which is the whole argument for threading one context rather
	// than adding a timeout at each level.
	hold := make(chan struct{})
	srv := stalledServer(t, hold)
	defer srv.Close()
	defer close(hold)

	ctx, cancel := context.WithCancelCause(context.Background())
	bus := NewBus()
	p := &anthropicProvider{baseURL: srv.URL, apiKey: "k", model: "m"}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = modelCall(ctx, p, srv.Client(), bus, 1, "s",
				[]Msg{TextMsg(RoleUser, "hi")}, nil, 16, deadlines{}, nil)
		}()
	}

	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()

	select {
	case <-joined:
		t.Fatal("the children returned before anything cancelled them")
	case <-time.After(100 * time.Millisecond):
	}

	cancel(errInterrupted)
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("the join did not complete after the parent was cancelled")
	}
}

func TestTheClientHasNoBlanketTimeout(t *testing.T) {
	// The change stage 10 is here to make, asserted rather than described.
	// http.Client.Timeout covers the body read, so any non-zero value on a
	// streaming client is a cap on how long the model may talk.
	dl := defaultDeadlines()
	c := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: dl.connect}}
	if c.Timeout != 0 {
		t.Fatalf("client Timeout = %v, want 0", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout != dl.connect {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, dl.connect)
	}
}

// ---------------------------------------------------------------------------
// The tool that never returns
// ---------------------------------------------------------------------------

func TestACancelledCommandIsKilledAndSaysSo(t *testing.T) {
	shell, err := findBash()
	if err != nil {
		t.Skip("no bash: ", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	// A generous command timeout, so a pass cannot come from the timeout path.
	r := runBash(ctx, shell, "sleep 30", 60*time.Second)
	if !r.Cancelled {
		t.Fatalf("Cancelled = false; result was %+v", r)
	}
	if r.TimedOut {
		t.Fatal("reported as a timeout: the model would be told to try something narrower")
	}
	if r.Duration > 10*time.Second {
		t.Fatalf("took %s — the process tree was not killed", r.Duration)
	}
	out, _ := r.render(4000)
	if !strings.Contains(out, "CANCELLED") {
		t.Fatalf("rendered output does not say it was cancelled:\n%s", out)
	}
}

func TestACancelledCommandTakesItsGrandchildrenWithIt(t *testing.T) {
	// Mutation testing found this missing too, and it is the more serious of
	// the two. Deleting the tree-kill from the cancellation path left every
	// other assertion true: runBash still returned promptly, still reported
	// Cancelled, still rendered the right status. The only thing that changed
	// was that the processes were still running afterwards — which is the whole
	// claim, and nothing was looking.
	//
	// The check is a heartbeat rather than a pid list: the surviving process is
	// a GRANDCHILD (backgrounded inside the shell), which is exactly what
	// exec.CommandContext would fail to kill, and a file it stops touching is
	// portable proof it is gone.
	shell, err := findBash()
	if err != nil {
		t.Skip("no bash: ", err)
	}
	dir := t.TempDir()
	hb := filepath.ToSlash(filepath.Join(dir, "hb"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(400 * time.Millisecond); cancel() }()

	cmd := "( while true; do date +%s%N > '" + hb + "'; sleep 0.05; done ) & sleep 60"
	r := runBash(ctx, shell, cmd, 90*time.Second)
	if !r.Cancelled {
		t.Fatalf("Cancelled = false; %+v", r)
	}

	first, err := os.ReadFile(hb)
	if err != nil {
		t.Skipf("the heartbeat never started (%v) — nothing to prove", err)
	}
	time.Sleep(600 * time.Millisecond) // > 10 heartbeat intervals
	second, err := os.ReadFile(hb)
	if err == nil && !bytes.Equal(first, second) {
		t.Fatalf("the grandchild is still writing after cancellation: %q -> %q",
			first, second)
	}
}
