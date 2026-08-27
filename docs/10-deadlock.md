# Stage 10 — Deadlock

Stage 09 ended with a limit written down and this stage named as the fix:

> **No deadline on a model call.** `http.Client{Timeout: 10 * time.Minute}` is
> the only clock, and it covers the entire body read: a slow-but-alive stream at
> minute ten dies mid-generation, and nothing can cancel a call in flight.
> `BuildRequest` returns a context-free `*http.Request`, so the *interface* is
> the barrier. Stage 10 is where that changes.

Threading a `context.Context` through the call graph is the boring half. The
half worth a chapter is that **one timeout is the wrong shape for a stream**, and
this chapter is mostly an argument with my own measurements about what the right
number is — three times, because the first two answers were wrong.

---

## Why one number cannot work

`http.Client.Timeout` covers dial, TLS, headers and the whole body read. On a
streaming endpoint the body read lasts as long as the model keeps talking, so
that single number is being asked two unrelated questions at once:

```
how long may a healthy generation take?      minutes, and unbounded in
                                             principle — a long answer is
                                             not a fault

how long may a DEAD connection look alive?   milliseconds; there is no good
                                             reason to wait at all
```

Ten minutes, and a stream that dies silently in second three holds the turn
hostage for the remaining 597. Ten seconds, and every long answer is killed
mid-sentence having generated — and been billed for — everything up to the cut.
Stage 09's re-bill line exists to price exactly that mistake.

So: three clocks.

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

The connect clock is `http.Transport.ResponseHeaderTimeout`, which stops at the
headers and leaves the body alone. The client's own `Timeout` is now **zero**,
and that is the change: a non-zero value there is a cap on how long the model
may speak.

The middle clock is the load-bearing one, and it is the whole reason this is
hard. A live stream and a hung socket look identical from outside: no bytes are
arriving in either. The only thing separating them is that one of them will
produce another byte, and the only evidence you can ever have about that is how
long it has already been.

---

## Setting the number, three times

The idle window is a number in a default. Stage 09 admitted the repo had no
evidence about timeouts anywhere in `wire-notes.md`, so this one had to be
measured. It took three attempts and the first two were wrong.

### Attempt 1: measure the gaps in the trace

Every event in a trace carries a timestamp, so the gap between consecutive
streaming events looks like exactly the quantity the watchdog watches. Measured
over three real sessions, it reported gaps up to **9099 ms** and **11067 ms**.

That is a fabrication, and the thing that exposed it was a run rather than a
review: one of those sessions had been run with `--stall-timeout 5s` and had
completed normally. A measurement that contradicts a run is wrong.

The bug was the filter. It listed `text_delta` and `usage` and forgot
`reasoning_delta` and `tool_args_delta` — which in that session were **223** and
**310** of the events. The "9-second silence" had been streaming a thinking
block the whole way through.

### Attempt 2: measure them again, properly

With the event kinds taken from `events.go` instead of from memory — 993 gaps
across 11 calls:

| | p50 | p90 | p99 | max |
|---|---:|---:|---:|---:|
| gap between stream frames | 30 ms | 62 ms | 294 ms | **804 ms** |
| time to first token | 2159 ms | 3309 ms | — | **16448 ms** |

Once a stream starts it never goes quiet for as long as a second. But the wait
for the *first* frame reached 16.4 seconds, which is 20× the widest mid-stream
gap. The conclusion looked obvious: the guard starts when the body is opened, so
its first window has to cover the model's entire thinking time, and the idle
window is really sized by TTFT.

That was wrong too.

### Attempt 3: print the number instead of inferring it

The quantity the deadline is compared against is the gap between **reads on the
socket**, and no amount of arithmetic on the trace produces it, because the
trace only contains frames the adapter chose to emit. So stage 10 measures it
where it happens — in the reader wrapping the body — and puts it on the panel
next to TTFT:

```
  ┌─ call 1 · tool_calls
  │ in 1743   ████████████████████  full 1743 · write 0 · read 0
  │ out 38     TTFT 4156ms · total 5830ms · 22.7 tok/s · idle max 1119ms
```

`idle max 1119ms` on a call whose TTFT was 4156 ms. The socket was **not**
silent while the model thought — the first byte arrives well before the first
token. Attempt 2's conclusion died on the first call that printed the number.

Across 14 calls:

| | min | median | max |
|---|---:|---:|---:|
| widest byte-level silence per call | 72 ms | 252 ms | **5001 ms** |

The 45-second default is a **9× margin** over the worst silence ever observed.

And note what that sample says about attempt 2's theory: across those 14 calls
TTFT topped out at 4157 ms while the widest byte-level silence was 5001 ms. The
socket went quieter *mid-stream* than it ever did before the first token. The two
quantities are not related the way the trace made them look.

### What the 5001 ms actually was

The call that produced it is worth reading, because the two clocks disagree by a
factor of four:

```
idle max     650 ms      ← running maximum, byte level
idle max    5000 ms
idle max    5001 ms
event gap  18509 ms      ← content frames, same stretch of the same call
```

An 18.5-second silence in the content stream, spanned by byte-level gaps of
almost exactly five seconds. Something arrives on that socket roughly every five
seconds while the model is thinking, and it produces no event.

The natural reading is a keep-alive on a five-second cadence, which would mean
the idle clock is really measuring *"the provider stopped sending keep-alives"* —
and that any value above five seconds is safe with enormous margin while any
value below it kills every thinking model.

**I could not capture the frames, and so the repo does not claim this.** Two
`curl` probes went after it — one with a prompt asking for a long silent think,
one with a tool definition and a command hard enough to compose — and both
streamed continuously: 152 and 1134 lines, **zero** gaps over one second and
**zero** SSE comment lines. The effect is recorded by the instrument; the bytes
that caused it are not in `wire-notes.md`, and until they are, the five-second
figure is an inference and not evidence. Stage 09's `Retry-After` sits in exactly
the same position.

---

## The cause matters more than the deadline

All three clocks expire by cancelling the same context, so by the time an error
surfaces they are indistinguishable — every one of them is `context.Canceled` —
and they need three different decisions. So each one cancels **with a cause**,
and the classifier reads the cause rather than the error.

The fourth cause is the one that would be a bug if it were missed:

```
interrupted    fatal, and the one that must not be retried OR fallen back
stalled        retry — a dead connection that took the idle window to prove it
call timeout   fatal — it was alive and simply too slow, and the same request
               will be too slow again while each attempt pays for the tokens
               generated before the cut
parent gone    fatal — the turn, or the program, is shutting down
```

Stage 09 classifies a failed call into retry / fall back / stop. An interrupt
that is merely "an error" is retried three times and then failed over to a
second provider: the agent answers **stop** by doing the work again somewhere
else, on a cold prompt cache, at 9.7× the price stage 09 measured. `triageCause`
runs before the status table for that one reason, and
`TestCauseBeatsTheStatusClassifier` pins it — a cancelled call can still be
carrying a 503 from whatever the dying request last saw.

The pleasing part is what did **not** have to change. Three new failure modes
arrived and the taxonomy absorbed them without a fourth verdict. That is the
best evidence stage 09 picked the right shape.

---

## Where the name comes from

Not a lock-ordering deadlock. The thing this stage removes is plainer and far
more common: **a wait nobody can end.**

Stage 07 fans subagents out and joins them with `wg.Wait()`. Until now every one
of those waits was unbounded all the way down — the parent waits on the
children, each child waits on a model call, and the model call waits on a socket
with a ten-minute cap and no way to be cancelled. One quiet TCP connection and
the whole tree is stuck, with Ctrl-C landing on a parent that is not listening.

One context fixes every level of that at once, which is why it is one idea and
not five: the child's call becomes cancellable, so the child returns, so
`wg.Wait()` returns, so the turn ends. **Nothing in `dispatch()` was changed.**
`TestACancelledParentDoesNotWaitOnItsChildrenForever` is that sentence as an
assertion.

The same reasoning picks the wrong tool for commands. `exec.CommandContext` is
the obvious answer and it signals `cmd.Process` and nothing else — discarding
the process-group containment stage 01 built a platform file for on each OS. So
the context goes into `runBash`'s existing `select` instead and reuses
`g.kill()`, and the tree dies the way it already did on a timeout. The two exits
stay distinguishable because they mean different things to the model — these
are the two status lines `render` can produce, not a capture:

```
[TIMED OUT after <d> — the process tree was killed]
[CANCELLED after <d> — the session is stopping and the process tree was killed]
```

A timeout is about this command and says "try something narrower". A
cancellation is about the session: there is no next command, and advice would be
an instruction the model cannot carry out.

---

## Why a timestamp and not `timer.Reset`

The obvious stall detector is a `time.Timer` reset on every read. It is also
wrong, in the way that costs the most to find: `Reset` races with the fire it is
trying to prevent. The timer expires, its function is already queued, and the
`Reset` that would have stopped it lands a microsecond late. The call is failed
as stalled although the bytes did arrive.

The window is tiny, which is exactly what makes it bad — it fires rarely, under
load, and looks like a flaky provider.

Comparing a timestamp cannot lose that race, because the reader writes the time
*before* the watcher reads it. The cost is that detection is late by up to one
tick, and the tick is a quarter of the window: a stall is noticed between
`idle` and `idle × 1.25`, never before `idle`.
`TestStallGuardFiresOnlyAfterTheWholeIdleWindow` pins the "never before" half,
which is the half a well-meaning simplification breaks.

Every test for this stage runs on an injected clock — `watch` takes its ticks as
a channel and `stallReader` takes `now` as a function — for the same reason stage
09 injects `sleep` and `rnd`. A component that owns a clock cannot be tested
without sleeping, and a suite full of sleeps is a suite people stop running.
"Forty minutes of a stream that never goes quiet" costs nothing here.

`guardBody`, notably, does **not** take an injectable clock, and that asymmetry
is deliberate: it creates a real ticker, so a fake `now` would have the reader
stamping one timeline while the watcher compared against another, and the guard
would fire on the first tick of a perfectly healthy stream. The pieces are
tested on a fake clock; the function that watches an actual socket is on the
real one by construction.

---

## From a real run

Stage 10's agent, on the scratch directory from stage 00 — find the bug, fix it,
verify:

```
  ── session ──────────────────────
  7 calls · 10 commands
  prompt tokens billed: 23232  (full 4736 · write 0 · read 18496)
  output tokens: 1053
  re-send ratio: 5.1x (billed 23232 for a final context of 4526)
```

Nothing about that is new, and that is the point: three clocks were armed the
whole time and none of them fired. The instrument is what changed, and it now
reports a margin rather than a hope.

---

## What this stage does not do

- **No live Ctrl-C demonstration.** The interrupt path is covered by tests —
  the ladder does not advance, the backoff is cut short, the process tree dies —
  but there is no recorded session showing it, because scripting a signal into a
  terminal session portably is a chapter of its own.
- **No evidence for the keep-alive.** Stated above and worth repeating: the
  five-second cadence is an inference from the instrument, not bytes in
  `wire-notes.md`. If the provider stopped filling silences, a 45-second idle
  window would begin killing calls that pause for 46 seconds of thinking, and
  nothing here would warn you first.
- **On Windows the cancel path's kill is redundant.** `runBash` defers
  `g.Close()`, and the Windows job object carries
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` — so closing the last handle kills the
  tree on the way out of the function, whether or not the cancellation branch
  killed it first. On Unix `Close()` is `return nil` and the kill is
  load-bearing. Found by mutation testing: deleting the kill from the cancel
  path passes both `go test` and `go test -race` here, and would leak a process
  tree on Linux. The test stays because the platform it protects is the one this
  machine cannot run.
- **`--connect-timeout` is not really one clock.** `ResponseHeaderTimeout`
  starts after the request is written, so DNS and dial sit outside it, bounded
  only by the OS. A `net.Dialer` timeout would close that, and it is one line
  this stage does not spend.
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
   read the panel: `idle max` tells you what to set instead, which is the
   argument for printing it.
2. **Set `--stall-timeout 0` and unplug your network mid-stream.** The clock is
   off, so the call waits on the socket until the OS gives up — which on Linux
   is a couple of minutes of TCP retransmits, and the turn is gone.
3. **Make the total deadline retry.** Change `errCallTimeout` to `TriageRetry`
   and run a long generation with `--call-timeout 20s`. Count what three
   attempts cost, and notice the panel cannot tell you the whole bill — usage
   arrives at the end of a stream and these streams do not end.
4. **Try `exec.CommandContext` instead.** Replace the `<-ctx.Done()` case in
   `runBash` with it, then cancel a command that has backgrounded a child. The
   shell dies and the grandchild does not, which is stage 01's chapter arriving
   by a different road.
5. **Capture the keep-alive.** Get any endpoint to go quiet mid-stream and
   record what fills the silence, then add the section to `wire-notes.md`. This
   is the exercise that improves the repo's evidence rather than its code, and
   it is the one thing this chapter asserts without proof.

→ Next: [Stage 11 — Malformed](11-malformed.md)

→ Reference: [Wire notes](wire-notes.md), [Stage 01 — Don't Die](01-dont-die.md), [Stage 09 — Triage](09-triage.md)
