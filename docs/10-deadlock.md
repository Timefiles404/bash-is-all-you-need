# Stage 10 — Deadlock

Stage 09 finished by writing its own limit down and naming this stage as the
fix:

> **No deadline on a model call.** `http.Client{Timeout: 10 * time.Minute}` is
> the only clock, and it covers the entire body read: a slow-but-alive stream at
> minute ten dies mid-generation, and nothing can cancel a call in flight.
> `BuildRequest` returns a context-free `*http.Request`, so the *interface* is
> the barrier. Stage 10 is where that changes.

Half of that is mechanical. Go's convention for cancelling work in progress is
to pass a `context.Context` — a value carrying a "stop now" signal — into every
function that can block. Threading one through this agent touches many files
and changes nothing you have to think about afterwards.

The other half is the subject of this chapter: **one timeout is the wrong shape
for a streamed response.** No value of that single number is correct, and
seeing why takes the first two sections. Everything after that is about the
numbers that replace it — where they come from, how to measure one without
fooling yourself, and what has to happen when one of them fires.

**Before you start.** You need stage 07's subagents, because the wait this
stage is named after is the one a parent does on its children; stage 09's
triage, because three new failure modes arrive here and have to be classified
by the machinery that stage built; and stage 01's process groups, because
cancelling a command is not the same thing as killing a process.

**What you will build.** Three separate clocks over one model call, a watchdog
that can tell a slow stream from a dead one, a way for each clock to say which
of them fired, and an instrument that prints the quantity the watchdog is
actually comparing against. One new file of about three hundred lines, plus a
context parameter on everything in the program that blocks.

---

## Where we are starting from

This is the entire timing policy in stage 09:

```go
httpc: &http.Client{Timeout: 10 * time.Minute}
```

`http.Client.Timeout` is a deadline on the whole request: DNS lookup, TCP
connection, TLS handshake, sending the request, receiving the response headers,
and reading the response body to the end. That last item is the problem. On a
streaming endpoint the body does not arrive and then finish — it arrives one
frame at a time for as long as the model keeps generating, and the read ends
when the model stops talking.

So a client timeout on a streaming call is not a timeout at all. It is a cap on
how long the model is permitted to speak.

The rest of the program is in the same state for the same reason: nothing in it
can be told to stop. Stage 09's two blocking signatures take no context —

```go
BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error)
func runBash(shell, command string, timeout time.Duration) execResult
```

— so neither a request in flight nor a running command can be cancelled, and
the backoff between retry attempts is a `time.Sleep`, which cannot be
interrupted at all. The agent had exactly one way to end a wait early, and it
was to kill the process.

---

## Why one number cannot work

Set aside the plumbing and look only at the number. A single timeout over a
streamed call is being asked two questions that have nothing to do with each
other:

```
how long may a healthy generation take?      minutes, and unbounded in
                                             principle — a long answer is
                                             not a fault

how long may a DEAD connection look alive?   milliseconds; there is no good
                                             reason to wait at all
```

Pick ten minutes, and a stream that dies silently in second three holds the
turn hostage for the remaining 597 — a blank terminal, then an error.

Pick ten seconds, and every long answer is killed mid-sentence. That is worse
than it sounds, because the tokens generated before the cut were generated: the
provider served a request and will invoice you for it. Stage 09's re-bill line
on the session panel exists to price exactly this mistake.

No number in between is right, because the two questions do not have a shared
answer. They need separate clocks.

---

## Three clocks

```
  dial ── TLS ── headers ──┬── byte ── byte ─────── byte ── [DONE]
                           │        ↑           ↑
  ├──── connect ───────────┤        └── gap ────┘
  │                        │
  └──────────────── total ─┴─────────────────────────────────────┘
```

| clock | flag | what it is for |
|---|---|---|
| connect | `--connect-timeout 30s` | headers must arrive. Nothing has been generated yet, so this is the one failure where a retry is free. |
| idle | `--stall-timeout 45s` | the **gap between bytes**, not the duration. The only clock that can tell a slow stream from a dead one. |
| total | `--call-timeout 15m` | a backstop on the whole call. Not a policy — a guard against a provider that dribbles one byte per idle period forever. |

Each of the three may be set to zero, which switches that clock off. Zero is a
real setting rather than a placeholder: the wire probing that produced
`docs/wire-notes.md` is done with all three off, because a probe that gets cut
short is not evidence of anything.

The connect clock is `http.Transport.ResponseHeaderTimeout`, which stops at the
response headers and leaves the body alone. The client's own `Timeout` is now
**zero**, and that is the change this stage exists to make:

```go
httpc := &http.Client{
    Transport: &http.Transport{
        Proxy:                 http.ProxyFromEnvironment,
        ResponseHeaderTimeout: dl.connect,
    },
}
```

A test exists whose only job is to keep that field at zero, because a
plausible-looking future edit that put a value back would silently restore the
cap on how long the model may talk.

The total clock is a `context.WithTimeoutCause` around one attempt. Fifteen
minutes is far past anyone's patience, so it is not a policy about how long a
user should wait. It is there because a provider that sends one byte every
forty seconds forever would never trip the idle clock at all.

The middle clock is the load-bearing one, and it is the whole reason this is
hard. **A live stream and a hung socket look identical from the outside.** No
bytes are arriving in either. The only thing separating them is that one of
them will produce another byte at some point in the future, and the only
evidence you can ever have about that is how long it has already been. An idle
timeout is a bet that a silence of a given length means death, and everything
in the next section is about finding a length that makes the bet safe.

---

## How to find out what the idle window should be

Stage 09 established that this repo has no recorded evidence about timeouts.
Searching all 731 lines of `wire-notes.md` for the terms that would carry it —
`429|Retry-After|timeout|502|503|504|408` — returns a single hit, and that hit
is the wire notes' own advice rather than captured bytes. So the idle window is
a number that has to be measured before it can be defended.

There are three ways to measure it. Two are cheap, look correct, and give wrong
answers. All three are worth walking, because the reason the first two fail is
more useful than the number the third one produces.

### Method one: measure the gaps between events in a trace

Anyone holding stage 02's trace file reaches for this first, and the reasoning
is good. Every event in a trace carries a timestamp, the trace exists precisely
so that questions about a session can be answered afterwards without rerunning
anything, and the gap between two consecutive streaming events looks exactly
like the quantity the watchdog watches. No new code at all.

Measured across three real sessions, it reports gaps of up to **9099 ms** and
**11067 ms**.

Both figures are fabrications, and there is a way to notice before believing
them. One of those sessions had been run with `--stall-timeout 5s` and had
completed normally. A measurement claiming a nine-second silence, taken from a
session that a five-second watchdog did not interrupt, contradicts a run — and
when a measurement and a run disagree, the run is the one that happened.

The flaw is in the filter. Writing this analysis means listing the event kinds
that count as "the stream is alive", and a list written from memory omits
kinds. This one listed `text_delta` and `usage` and left out `reasoning_delta`
and `tool_args_delta` — which in that session were **223** and **310** of the
events. The nine-second silence had been streaming a thinking block the whole
way through.

The general shape of that failure is worth naming, because it recurs: **a
measurement built from a filtered view of a stream inherits the filter's
omissions, and the omissions do not show up as holes in the result.** A missing
event kind does not produce missing data; it produces a longer gap, which reads
as a finding. The repair is to take the list of kinds from `events.go` rather
than from memory, which leads straight to the second method.

### Method two: infer it from the time to first token

With the event kinds read out of the source file — 993 gaps across 11 calls:

| | p50 | p90 | p99 | max |
|---|---:|---:|---:|---:|
| gap between stream frames | 30 ms | 62 ms | 294 ms | **804 ms** |
| time to first token | 2159 ms | 3309 ms | — | **16448 ms** |

The top row is comfortable: once a stream has started it never goes quiet for
as long as a second, anywhere in the sample, which would make a very small idle
window look safe.

The bottom row is not. "Time to first token" — often shortened to TTFT — is the
delay between sending a request and the first piece of generated text coming
back, and here it reached 16.4 seconds, twenty times the widest mid-stream gap.

The conclusion that follows looks inevitable. The guard starts when the body is
opened, so its very first window has to cover the model's entire thinking time
before any content exists. So the idle window is not sized by the mid-stream
gaps at all; it is sized by TTFT, and it must be at least 16.4 seconds plus a
margin.

The reasoning is sound and the premise is false. **TTFT measures when content
appears. The watchdog does not watch content — it watches reads on the
socket.** Those are two different quantities, and no arithmetic on a trace can
convert one into the other, because a trace contains only the frames the
adapter chose to turn into events.

So method two fails for the same reason method one did, in better clothes.
Both measure something the trace can see. The deadline is compared against
something the trace cannot see.

### Method three: measure it where it happens

The only place the real quantity exists is the code that reads the socket. So
stage 10 measures it there.

`stallReader` wraps the response body. Every read that produced bytes stamps
the current time onto the guard, and the guard keeps a running maximum of the
gaps between stamps:

```go
func (s *stallReader) Read(p []byte) (int, error) {
    n, err := s.rc.Read(p)
    if n > 0 && s.guard.mark(s.now()) && s.bus != nil {
        s.bus.Emit(Event{Kind: KindIdleMax, Turn: s.turn,
            Millis: s.guard.idleMax().Milliseconds()})
    }
    return n, err
}
```

Two details in that condition are deliberate. The test is `n > 0` rather than
`err == nil`, because a read is allowed to return data and `io.EOF` at the same
time, and that read is proof of life like any other. And each new maximum is
emitted as its own event rather than being reported once at the end, because a
stream that stalled never reaches its end — the call whose gap you most want to
see is exactly the call that will not survive to report it.

The number then lands on the per-call panel, next to TTFT:

```
  ┌─ call 1 · tool_calls
  │ in 1743   ████████████████████  full 1743 · write 0 · read 0
  │ out 38     TTFT 4156ms · total 5830ms · 22.7 tok/s · idle max 1119ms
```

`idle max 1119ms` on a call whose TTFT was 4156 ms. The socket was **not**
silent while the model was thinking; bytes were arriving the whole time, long
before the first token did. Method two's conclusion does not survive the first
call that prints the number.

Across 14 calls:

| | min | median | max |
|---|---:|---:|---:|
| widest byte-level silence per call | 72 ms | 252 ms | **5001 ms** |

The 45-second default is a **9× margin** over the widest silence ever observed
here.

The same sample also settles the TTFT theory directly. Across those 14 calls
TTFT topped out at 4157 ms while the widest byte-level silence was 5001 ms. The
socket went quieter *mid-stream* than it ever did before the first token. The
two quantities are not merely different; they are not even ordered the way the
trace made them look.

The rule that separates the third method from the first two is short enough to
carry away: **measure the quantity the deadline is compared against, not one
that correlates with it.** If nothing records that quantity, building the
instrument is the work — and not optional, because the two cheap methods did
not fail quietly. They produced confident numbers, wrong in opposite
directions.

### What a five-second silence probably was

The call that produced the 5001 ms figure is worth reading closely, because the
two instruments disagree about it by a factor of nearly four:

```
idle max     650 ms      ← running maximum, byte level
idle max    5000 ms
idle max    5001 ms
event gap  18509 ms      ← content frames, same stretch of the same call
```

An 18.5-second silence in the content stream, spanned by byte-level gaps of
almost exactly five seconds each. Something arrives on that socket roughly
every five seconds while the model is thinking, and whatever it is, it produces
no event.

The natural reading is a keep-alive: a byte or a blank line the server sends
for no reason except to stop the connection from looking dead. If that is what
it is, then the idle clock is not really measuring "the model stopped
generating" at all — it is measuring "the provider stopped sending
keep-alives", which would mean any value above five seconds is safe with
enormous margin, and any value below it kills every call that pauses to think.

**The repo does not claim that, because the bytes are not in evidence.** Two
`curl` probes went looking for them — one with a prompt asking for a long
silent think, one with a tool definition and a command hard enough to compose —
and both streamed continuously: 152 and 1134 lines, **zero** gaps over one
second, and **zero** comment lines. Server-sent events — SSE, the streaming
format both protocols use — allows a line beginning with `:` that carries no
data and exists purely to fill silence. That is the obvious candidate for what
a keep-alive would look like, and neither probe produced one.

So the effect is recorded by the instrument and the cause is not recorded
anywhere. Until it is, the five-second cadence is an inference. Stage 09's
handling of `Retry-After` sits in exactly the same position, and being able to
tell that kind of claim from a claim backed by captured bytes is worth more
than either individual number.

---

## Detecting a stall without losing a race

The obvious way to implement an idle timeout is a `time.Timer` that gets reset
on every read. It is also wrong, in the way that costs the most to find.

`Reset` races with the fire it is trying to prevent. The timer expires, the
runtime has already queued its function to run, and the `Reset` that would have
stopped it lands a microsecond late. The call is then failed as stalled
although the bytes did arrive.

The window for that race is tiny, which is exactly what makes it bad. A bug
that fires on every call gets found on the first run. A bug that fires rarely,
under load, on a busy stream, in production, looks like a flaky provider — and
you will spend the afternoon reading the provider's status page.

Comparing a timestamp cannot lose that race, because the reader writes the time
*before* the watcher reads it, so a byte that has arrived is always visible to
the next check:

```go
case now, ok := <-tick:
    if !ok {
        return
    }
    if now.Sub(time.Unix(0, g.last.Load())) >= idle {
        cancel(errStalled)
        return
    }
```

The cost of that trade is that detection is late: the watcher only looks when
the ticker fires. The tick is deliberately a quarter of the window, which
bounds the lateness — a stall is noticed between `idle` and `idle × 1.25`, and
**never before `idle`**.

Late is the safe direction and early is not. Firing late costs a fraction of a
window on a call that was already dead; firing early kills a live call and
produces a failure indistinguishable from a provider problem.
`TestStallGuardFiresOnlyAfterTheWholeIdleWindow` pins the "never before" half
specifically, because that is the half a well-meaning simplification breaks.

### Everything is tested on a clock the test controls

`watch` takes its ticks as a channel and `stallReader` takes `now` as a
function, for the same reason stage 09 injects `sleep` and `rnd`. A component
that owns a clock cannot be tested without sleeping, and a suite full of sleeps
is slow, flaky on a loaded machine, and quietly untestable at exactly the
boundary you care about. Here "forty-five seconds passed" is a value pushed
into a channel, so it costs nothing and happens precisely when the test says.

`guardBody` — the function that wires the pieces together around a real
response body — notably does **not** take an injectable clock, and the
asymmetry is deliberate. It creates a real `time.Ticker`, so a fake `now`
underneath it would have the reader stamping one timeline while the watcher
compared against another, and the guard would fire on the first tick of a
perfectly healthy stream. The pieces are tested on a controlled clock; the one
function that watches an actual socket is on the real clock by construction.

### The watchdog has an owner

```go
stream, _, stopGuard := guardBody(ctx, resp.Body, dl.idle, cancel, bus, turn)
defer stopGuard()
```

`guardBody` starts a goroutine and returns the function that ends it, and the
caller defers that function so it runs on every path out — including the
successful one. A watchdog nobody stops is a leak that scales with the number of
calls, and stage 07's subagents make that number large. The stop function also
cancels the context, which is what ends `watch` in the case where the body
never reaches EOF at all.

---

## What the guard costs

One goroutine and one ticker per in-flight call. The tick is a quarter of the
idle window — four wakeups per window, so a little over five a minute at the
45-second default. That is the reason for the quarter: frequent enough that
detection is not much later than the deadline, rare enough that a long call is
not a spin loop. Detection late by up to a quarter of the
window. One more event kind on the bus, emitted only when a new maximum is set,
which on a healthy stream happens a few times near the start and then stops.

One thing it deliberately does not cost: setting `--stall-timeout 0` turns off
the watchdog but **not** the instrument. `guardBody` still wraps the body, still
measures, and still reports the widest gap. Turning off a deadline should not
also turn off the thing that would have told you what to set it to.

---

## The cause matters more than the deadline

All three clocks expire the same way — by cancelling the call's context — so by
the time an error surfaces they are indistinguishable. Every one of them is
`context.Canceled`. And they need three different decisions.

So each clock cancels **with a cause**, and the classifier reads the cause
rather than the error. There are four causes, and the fourth is the one that
would be a bug if it were missed:

```
interrupted    fatal, and the one that must not be retried OR fallen back
stalled        retry — a dead connection that took the idle window to prove it
call timeout   fatal — it was alive and simply too slow, and the same request
               will be too slow again while each attempt pays for the tokens
               generated before the cut
parent gone    fatal — the turn, or the program, is shutting down
```

The fourth is a human pressing Ctrl-C. Stage 09 classifies a failed call into
retry, fall back, or stop. An interrupt that arrives as merely "an error" is
retried three times and then failed over to a second provider — which is the
agent answering **stop** by doing the same work again somewhere else, on a cold
prompt cache, at the 9.7× price stage 09 measured. That is why the session's
signal handler is written out by hand instead of using `signal.NotifyContext`:
the shorter version cancels with `context.Canceled`, which is the one thing
triage cannot tell apart from a stall.

`triageCause` therefore runs **before** the status table, not after it:

```go
func (e *CallError) triage() Triage {
    if v, _, ok := triageCause(e.cause); ok {
        return v
    }
    switch e.Phase {
    // ...
```

A call this program ended is not evidence about the provider, so nothing below
has jurisdiction over it. `TestCauseBeatsTheStatusClassifier` pins the ordering,
and it is not a theoretical concern: a cancelled call can still be carrying a
503 from whatever the dying request last managed to see, and a classifier that
looked at the status first would retry a Ctrl-C.

Notice what did **not** have to change. Three new failure modes arrived and the
taxonomy absorbed all three without needing a fourth verdict. That is the best
available evidence that stage 09 picked the right shape for its decisions.

---

## Where the name comes from

Not a lock-ordering deadlock — no two goroutines here are holding resources
each other wants. The thing this stage removes is plainer and far more common:
**a wait nobody can end.**

Stage 07 fans subagents out and joins them with `wg.Wait()`. Until now every
one of those waits was unbounded all the way down. The parent waits on the
children. Each child waits on a model call. The model call waits on a socket
with a ten-minute cap and no way to be cancelled. One quiet TCP connection
anywhere in that tree and the whole tree is stuck, with Ctrl-C landing on a
parent that is not listening for it.

One context fixes every level of that at once, which is why this is one idea
rather than five: the child's call becomes cancellable, so the child returns, so
`wg.Wait()` returns, so the turn ends. **Nothing in `dispatch()` was changed.**
The function that fans out and joins did not have to learn anything about
deadlines. `TestACancelledParentDoesNotWaitOnItsChildrenForever` is that
sentence written as an assertion.

The same reasoning picks the wrong tool for commands, and it is worth following
because the obvious answer is very obvious. Go supplies `exec.CommandContext`
for exactly this: hand it a context and it kills the process when the context
ends. It kills `cmd.Process`, and nothing else — which discards the containment
stage 01 built a platform-specific file for on each operating system. The shell
dies; every process the shell backgrounded survives, reparented and running.

So the context goes into `runBash`'s existing `select` instead, and reuses the
process-group kill that was already there:

```go
select {
case waitErr = <-done:
case <-ctx.Done():
    cancelled = true
    stop()          // g.kill(), then wait up to 5s to reap
case <-time.After(timeout):
    timedOut = true
    stop()
}
```

The tree dies the way it already did on a timeout, and there is one kill path
rather than two.

The two exits stay distinguishable, because they mean different things to the
model. These are the two status lines `render` can produce:

```
[TIMED OUT after <d> — the process tree was killed]
[CANCELLED after <d> — the session is stopping and the process tree was killed]
```

A timeout is about this command, and it carries advice: try something narrower.
A cancellation is about the session. There is no next command, so advice would
be an instruction the model cannot carry out, and the line deliberately does not
offer any.

### The retry backoff is a wait too

Stage 09 slept with `time.Sleep`, which cannot be interrupted. At its defaults
that is up to eight seconds in which a program already told to stop does
nothing — which reads to a user as a hang, and gets answered with a second
Ctrl-C that kills the process before the trace is flushed. The session that
just went wrong is then also the session with no record of it. `waitFor` is a
sleep a cancellation can cut short, and it returns the cause, so an interrupt
during a backoff is reported as the interrupt it was rather than as the HTTP
503 that started the wait.

The signal handler then unregisters itself, so a second Ctrl-C is not more of
the first. The first press asks the agent to unwind: kill commands, close the
trace, print the bill. If that unwind is itself stuck, the user needs a way out
that does not depend on the code that is stuck, and the default behaviour of an
unhandled interrupt — ending the process — is exactly that way out.

---

## From a real run

Stage 10's agent, on the scratch directory from stage 00 — find the bug, fix
it, verify:

```
  ── session ──────────────────────
  7 calls · 10 commands
  prompt tokens billed: 23232  (full 4736 · write 0 · read 18496)
  output tokens: 1053
  re-send ratio: 5.1x (billed 23232 for a final context of 4526)
```

Nothing on that panel is new, and that is the point: three clocks were armed
for the whole session and none of them fired. What changed is the instrument.
The per-call lines now report a margin instead of a hope, in the quantity the
deadline is compared against.

---

## What this stage does not do

- **No live Ctrl-C demonstration.** The interrupt path is covered by tests —
  the ladder does not advance, the backoff is cut short, the process tree dies
  — but there is no recorded session showing it, because scripting a signal
  into a terminal session portably is a chapter of its own.
- **No evidence for the keep-alive.** Stated above and worth repeating: the
  five-second cadence is an inference from the instrument, not bytes in
  `wire-notes.md`. If the provider stopped filling silences, a 45-second idle
  window would begin killing calls that pause for 46 seconds of thinking, and
  nothing here would warn you first.
- **The deadlines do not reach subagents.** `newChild` copies the provider
  ladder, the retry policy and the HTTP client, and not the `deadlines` struct
  — and a zero `deadlines` means all three clocks are off, because `guardBody`
  and `waitFor` both read `<= 0` as "no watchdog". A child's call is still
  cancellable, so Ctrl-C and a dying parent still reach it; what a child does
  not have is a stall detector or a total backstop. Stage 11 carries the fix,
  which is one line.
- **On Windows the cancel path's kill is redundant.** `runBash` defers
  `g.Close()`, and the Windows job object carries
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` — so closing the last handle kills the
  tree on the way out of the function, whether or not the cancellation branch
  killed it first. On Unix `Close()` is `return nil` and the kill is
  load-bearing. Deleting the kill from the cancellation path passes both
  `go test` and `go test -race` on Windows and would leak a process tree on
  Linux, so the test that covers it protects the one platform this machine
  cannot run.
- **`--connect-timeout` is not really one clock.** `ResponseHeaderTimeout`
  starts after the request is written, so DNS and dial sit outside it, bounded
  only by the operating system. A `net.Dialer` timeout would close that, and it
  is one line this stage does not spend.
- **The total deadline is per attempt, not per turn.** Three retries of a
  15-minute call is 45 minutes. The retry *budget* from stage 09 bounds the
  waiting between attempts and not the attempts themselves, so the two numbers
  do not compose into anything a human would predict.
- **Nothing cancels a compaction independently.** It inherits the turn's
  context, so Ctrl-C during a summarising call stops the session rather than
  just the compaction. Continuing uncompacted would be the friendlier answer.

---

## Exercises

1. **Set `--stall-timeout 200ms` and watch it eat healthy calls.** The median
   observed silence is 252 ms, so this fires on roughly half your calls. Then
   read the panel: `idle max` tells you what to set instead, which is the whole
   argument for printing it.
2. **Set `--stall-timeout 0` and unplug your network mid-stream.** The clock is
   off, so the call waits on the socket until the operating system gives up —
   which on Linux is a couple of minutes of TCP retransmits, and the turn is
   gone.
3. **Make the total deadline retry.** Change `errCallTimeout` to `TriageRetry`
   and run a long generation with `--call-timeout 20s`. Count what three
   attempts cost, and notice that the panel cannot tell you the whole bill —
   usage arrives at the end of a stream and these streams do not end.
4. **Try `exec.CommandContext` instead.** Replace the `<-ctx.Done()` case in
   `runBash` with it, then cancel a command that has backgrounded a child. The
   shell dies and the grandchild does not, which is stage 01's chapter arriving
   by a different road.
5. **Capture the keep-alive.** Get any endpoint to go quiet mid-stream and
   record what fills the silence, then add the section to `wire-notes.md`. This
   is the exercise that improves the repo's evidence rather than its code, and
   it is the one thing this chapter asserts without proof.

---

## What you can answer now

**Why can one timeout not protect a streamed model call?**
Because `http.Client.Timeout` covers the body read, and on a stream the body
read lasts as long as the model is talking. That makes the number a cap on how
long the model may speak, not a guard against a broken connection. Ten minutes
lets a dead socket hold a turn for 597 wasted seconds; ten seconds kills long
answers after you have already been billed for the tokens generated.

**What are the three clocks, and which one is load-bearing?**
Connect (`--connect-timeout 30s`, headers must arrive), idle
(`--stall-timeout 45s`, the gap between bytes), and total
(`--call-timeout 15m`, a backstop on one attempt). The idle clock is the
load-bearing one: it is the only one that can distinguish a slow stream from a
dead one.

**Why can the idle window not be measured from a trace?**
Because a trace contains only the frames the adapter turned into events, and
the deadline is compared against gaps between reads on the socket. Gaps between
events reported silences of 9099 ms and 11067 ms that never happened, purely
because the filter omitted `reasoning_delta` and `tool_args_delta`.

**Why is time to first token the wrong number to size the idle window with?**
Because TTFT measures when generated content appears, and the guard watches
when bytes arrive. They are different quantities: on the same sample TTFT
topped out at 4157 ms while the widest byte-level silence was 5001 ms, so the
socket went quieter mid-stream than it ever did before the first token.

**What is the widest byte-level silence this repo has observed, and what
margin does the default leave?**
5001 ms, across 14 calls whose per-call maxima ran from 72 ms to a median of
252 ms. The 45-second default is a 9× margin over the widest one seen.

**What is the evidence for the five-second keep-alive?**
The instrument, and only the instrument: byte-level gaps of almost exactly
5000 ms spanning an 18509 ms silence in the content stream. Two `curl` probes
failed to capture the bytes causing it — 152 and 1134 lines, no gaps over a
second, no SSE comment lines — so the cadence is an inference and the chapter
says so.

**Why a timestamp and a ticker rather than resetting a `time.Timer`?**
Because `Reset` races with the fire it is trying to prevent: the timer expires,
its function is already queued, and the reset lands late, so a call is failed as
stalled although the bytes arrived. The reader writes a timestamp before the
watcher reads it, so that race cannot occur. The cost is that detection is late
by up to one tick, and the tick is a quarter of the window.

**Why must the guard fire late rather than early?**
Because firing late costs a fraction of a window on a call that was already
dead, while firing early kills a live call and produces a failure that looks
exactly like a provider problem. That is why the pinned assertion is "never
before `idle`" rather than "close to `idle`".

**Why does a cancelled call need a cause attached?**
Because all three clocks and the interrupt handler cancel the same context, and
by the time the error surfaces every one of them is `context.Canceled`. The
cause is the only thing that still knows which clock fired, and the four causes
need three different decisions — a stall retries, a total timeout stops, an
interrupt stops without falling back.

**What is the deadlock in the title?**
A wait nobody can end. The parent waits on `wg.Wait()`, each child waits on a
model call, and the call waits on a socket that could not be cancelled — so one
quiet connection froze the whole tree, including Ctrl-C. Threading one context
ends every level of that at once, and `dispatch()` did not have to change.

---

## Questions to think about

These do not have answers in the repo. They are the ones where the answer
depends on what you are building.

1. The idle window is one number for every provider, every model and every
   prompt. What would it take to set it per provider — and if you learned it
   from observed traffic instead, how would you keep a single genuinely slow
   provider from teaching the agent to be patient with dead sockets?

2. A stall is retried and a total timeout is fatal, and both of them cut off a
   generation that was being billed. That split assumes a partial response is
   worthless. Sketch an agent where it is not, and work out which of the two
   verdicts changes.

3. The total deadline is per attempt, so three retries of a 15-minute call is
   45 minutes. Design the version that is bounded per turn instead. What has to
   know about the remaining budget, and what happens to the retry policy when
   the budget is nearly spent?

4. The guard proves a connection is dead by waiting long enough that silence
   becomes evidence. Is there any other signal available to you that would
   distinguish a hung socket from a slow one sooner — and what would you be
   trusting if you used it?

5. Every clock in this chapter is enforced by the client. A provider could
   enforce its own, and some do. If the server cut a stream at its own deadline
   and said so in the stream itself before closing it, which parts of this
   stage would you delete, and which would you keep anyway?

→ Next: [Stage 11 — Malformed](11-malformed.md)

→ Reference: [Wire notes](wire-notes.md), [Stage 01 — Don't Die](01-dont-die.md), [Stage 09 — Triage](09-triage.md)
