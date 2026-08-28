// Stage 10 — Deadlock: every wait gets a deadline and an owner.
//
// Stage 09 wrote its own limit down and named this stage as the fix:
//
//	"No deadline on a model call. http.Client{Timeout: 10 * time.Minute} is
//	 the only clock, and it covers the entire body read: a slow-but-alive
//	 stream at minute ten dies mid-generation, and nothing can cancel a call
//	 in flight."
//
// The plumbing half of that — a context.Context reaching every blocking call —
// is dull and mechanical. The interesting half is that **one timeout is the
// wrong SHAPE for a streamed response**, and no value of that one number fixes
// it.
//
// # Why one number cannot work
//
// http.Client.Timeout covers dial, TLS, headers and the whole body read. On a
// streaming endpoint the body read lasts as long as the model keeps talking, so
// that single number is being asked to answer two unrelated questions at once:
//
//	how long may a healthy generation take?    → minutes, and unbounded in
//	                                             principle; a long answer is
//	                                             not a fault
//	how long may a DEAD connection look alive? → milliseconds; there is no
//	                                             good reason to wait
//
// Set it to ten minutes and a stream that dies silently in second three holds
// the turn hostage for the remaining 597. Set it to ten seconds and every long
// answer is killed mid-sentence, having generated — and been billed for —
// everything up to the cut. Stage 09's re-bill line exists to price exactly
// that mistake.
//
// # Three clocks instead
//
//	  dial ── TLS ── headers ──┬── byte ── byte ─────── byte ── [DONE]
//	                           │        ↑           ↑
//	  ├──── connect ───────────┤        └── gap ────┘
//	  │                        │
//	  └──────────────── total ─┴─────────────────────────────────────┘
//
//	connect   headers must arrive within it. Nothing has been generated yet,
//	          so this is the one failure where a retry is free.
//	idle      the GAP between bytes, not the duration. This is the only clock
//	          that can tell a slow stream from a dead one.
//	total     a backstop on the whole call. Not a policy — a guard against a
//	          provider that dribbles one byte per idle period forever.
//
// The middle one is the load-bearing idea. A live stream and a hung socket look
// identical from the outside: no bytes are arriving in either. The only thing
// that separates them is that one of them will produce another byte, and the
// only evidence you can have about that is how long it has already been.
//
// # Why the cause matters more than the deadline
//
// All three expire by cancelling the same context, so by the time the error
// surfaces they are indistinguishable — every one of them is
// context.Canceled — and they need three different decisions. So each one
// cancels WITH A CAUSE, and triage reads the cause rather than the error.
//
// The fourth cause is the one that would be a bug if it were missed: the user
// pressing Ctrl-C. Stage 09 classifies a failed call into retry / fall back /
// stop, and an interrupt that is merely "an error" gets retried three times and
// then fails over to a second provider — which is the agent responding to
// "stop" by working harder. See triageCause.
package main

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// The four reasons a call's context gets cancelled from inside this program.
// They are values, not strings, because triageCause switches on them and a
// typo in a string would silently fall through to the default.
var (
	// errStalled means no bytes arrived for the idle timeout. The stream was
	// established, so something may well have been generated and billed.
	errStalled = errors.New("the provider stopped sending mid-stream")

	// errCallTimeout means the call outlived its total budget while still
	// producing bytes. Distinct from a stall on purpose: this one was alive.
	errCallTimeout = errors.New("the call ran past its total deadline")

	// errInterrupted means a human asked for it to stop.
	errInterrupted = errors.New("interrupted")
)

// deadlines is the whole timing policy: three durations, and each is allowed to
// be zero, which switches that clock off. Zero is a real setting rather than a
// placeholder — the wire-probing in external/wire-notes.md is done with all three
// off, because a probe that gets cut short is not evidence.
type deadlines struct {
	connect time.Duration // headers must arrive within this
	idle    time.Duration // longest tolerated gap between bytes
	total   time.Duration // backstop on the entire call
}

func defaultDeadlines() deadlines {
	// The idle default is not a guess; see 10-deadlock/doc/, where the
	// gap between consecutive SSE frames is measured on a real session and
	// the default is set well above the widest one observed.
	return deadlines{
		connect: 30 * time.Second,
		idle:    45 * time.Second,
		total:   15 * time.Minute,
	}
}

// stallGuard watches the gap between reads and cancels when it grows too wide.
//
// # Why a timestamp and a ticker, and not timer.Reset
//
// The obvious implementation is a time.Timer reset on every read. It is also
// wrong, and wrong in a way that only shows up under load: Reset races with the
// fire it is trying to prevent. The timer expires, its function is already
// queued to run, and the Reset that would have stopped it lands a microsecond
// late. The call is then failed as stalled although the bytes did arrive.
//
// The window is small, which is exactly what makes it bad — it fires rarely,
// on a busy stream, and looks like a flaky provider.
//
// Comparing a timestamp cannot lose that race. The reader writes the time
// BEFORE the watcher reads it, so a byte that has arrived is always visible to
// the next check. The cost is that detection is late by up to one tick, and the
// tick is deliberately a quarter of the idle window: a stall is noticed
// somewhere between idle and idle*1.25, never before idle.
type stallGuard struct {
	last atomic.Int64 // UnixNano of the most recent byte

	// widest is the largest gap this call actually saw, in nanoseconds.
	//
	// A timeout whose subject is never displayed is a number somebody guessed.
	// This is the quantity the idle deadline is compared against, so the panel
	// prints it next to TTFT and a reader can see their own margin instead of
	// trusting the default — which is the same argument stage 04 makes about
	// cache markers and stage 09 makes about retries.
	widest atomic.Int64
}

// mark records a byte's arrival and reports whether it set a new widest gap.
//
// The bool is how the number reaches the panel. Emitting the widest gap after
// ParseStream returns does not work: the panel's per-call block is drawn from
// KindResponseEnd, which the adapter emits from INSIDE the stream it is
// reading, so an event sent afterwards arrives one call too late and prints
// against the wrong box. Reporting each new maximum as it happens means the
// renderer already has the final value when the response ends.
func (g *stallGuard) mark(now time.Time) bool {
	n := now.UnixNano()
	prev := g.last.Swap(n)
	if prev == 0 {
		return false
	}
	gap := n - prev
	if gap <= g.widest.Load() {
		return false
	}
	// Not compare-and-swap in a loop: only the reader goroutine calls mark, so
	// there is exactly one writer. The atomic is for the watcher and the panel,
	// which read it.
	g.widest.Store(gap)
	return true
}

// idleMax reports the widest gap seen, as a duration.
func (g *stallGuard) idleMax() time.Duration { return time.Duration(g.widest.Load()) }

// watch runs the guard until ctx ends or the gap exceeds idle.
//
// now and tick are injected for the same reason stage 09 injects sleep and rnd:
// a component that owns a clock cannot be tested without sleeping, and a suite
// full of sleeps is a suite people stop running. Here the whole stall detector
// is exercised by pushing values into a channel.
func (g *stallGuard) watch(ctx context.Context, idle time.Duration,
	cancel context.CancelCauseFunc, tick <-chan time.Time) {
	if idle <= 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-tick:
			if !ok {
				return
			}
			if now.Sub(time.Unix(0, g.last.Load())) >= idle {
				cancel(errStalled)
				return
			}
		}
	}
}

// stallReader marks the guard on every read that produced bytes.
//
// n > 0 is the condition, not err == nil: a read can return data and io.EOF
// together, and that read is proof of life like any other.
type stallReader struct {
	rc    io.ReadCloser
	guard *stallGuard
	now   func() time.Time

	// bus and turn are how the measurement leaves this file. The renderer has
	// no clock — that is stage 02's rule, and the reason replay reproduces a
	// session to the millisecond — so a duration it displays has to arrive in
	// an event, measured by whoever held the stopwatch. Here that is the only
	// object in the program touching the socket.
	bus  *Bus
	turn int
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 && s.guard.mark(s.now()) && s.bus != nil {
		s.bus.Emit(Event{Kind: KindIdleMax, Turn: s.turn,
			Millis: s.guard.idleMax().Milliseconds()})
	}
	return n, err
}

func (s *stallReader) Close() error { return s.rc.Close() }

// guardBody wraps a response body so the stream is watched while it is read,
// and returns a stop function the caller must defer.
//
// The goroutine is owned here and ends with the context, which is the "and an
// owner" half of this stage's title. A watchdog nobody stops is a leak that
// scales with the number of calls, and a subagent tree makes that hundreds.
// It deliberately does NOT take an injectable clock, even though stallGuard and
// stallReader both do. The ticker below is real, so a fake `now` would have the
// reader stamping one timeline while the watcher compared against another, and
// the guard would fire on the first tick of a perfectly healthy stream. The
// pieces are unit-tested on a fake clock; this function, which is the one that
// watches an actual socket, is on the real one by construction.
func guardBody(ctx context.Context, rc io.ReadCloser, idle time.Duration,
	cancel context.CancelCauseFunc, bus *Bus, turn int) (io.ReadCloser, *stallGuard, func()) {
	if idle <= 0 {
		// Still wrapped, and that is deliberate: with the clock off there is no
		// watchdog, but the widest gap is still worth measuring. Turning the
		// deadline off should not also turn off the instrument that would tell
		// you what to set it to.
		g := &stallGuard{}
		g.mark(time.Now())
		return &stallReader{rc: rc, guard: g, now: time.Now, bus: bus, turn: turn}, g, func() {}
	}
	g := &stallGuard{}
	g.mark(time.Now())

	// A quarter of the window: frequent enough that detection is not much
	// later than the deadline, rare enough that a 45s idle costs four wakeups
	// per window — a little over five a minute — rather than a spin.
	t := time.NewTicker(idle / 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.watch(ctx, idle, cancel, t.C)
	}()
	stop := func() {
		t.Stop()
		cancel(context.Canceled) // ends watch() even if the body never EOFs
		<-done
	}
	return &stallReader{rc: rc, guard: g, now: time.Now, bus: bus, turn: turn}, g, stop
}

// waitFor is a sleep that a cancellation can cut short.
//
// time.Sleep cannot be interrupted, so a retry backoff built on it swallows
// Ctrl-C for up to the whole wait. With stage 09's defaults that is eight
// seconds of a program that has already been told to stop, which reads to the
// user as a hang and gets answered with a second Ctrl-C — and the second one
// kills the process before the trace is flushed.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// triageCause turns a cancelled call into one of stage 09's three verdicts.
//
// It runs BEFORE the ordinary classifier, because by the time an error has been
// through the http package every one of these is the same context.Canceled and
// the shape of the failure is gone. context.Cause is the only thing that still
// knows which clock fired.
//
// The four answers, and why each is not one of the others:
//
//	interrupted  fatal, and the one that must not be retried OR fallen back.
//	             A human said stop. An agent that answers that by trying a
//	             second provider has turned the stop button into a fan-out.
//	stalled      retry. Nothing distinguishes it from the transport failures
//	             stage 09 already retries; it is a dead connection that took
//	             the idle window to prove itself dead.
//	call timeout fatal. This one was alive and simply too slow, so the same
//	             request will be too slow again — and each attempt pays for
//	             every token generated before the cut. A backstop you retry
//	             is not a backstop.
//	parent gone  fatal. The context that ended was not this call's own; the
//	             turn, or the whole program, is shutting down around it.
func triageCause(err error) (Triage, string, bool) {
	switch {
	case errors.Is(err, errInterrupted):
		return TriageFatal, "interrupted", true
	case errors.Is(err, errStalled):
		return TriageRetry, "stalled", true
	case errors.Is(err, errCallTimeout):
		return TriageFatal, "call_timeout", true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return TriageFatal, "cancelled", true
	}
	return "", "", false
}
