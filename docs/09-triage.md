# Stage 09 — Triage

One idea:

> **An error is a decision, not a string.**

An agent that has just failed a model call has exactly three moves. Wait and
send the same bytes again. Send them somewhere else. Stop and say why. Every
line of `triage.go` exists to turn a failure into one of those three.

That sounds like a small `if`. It is a whole file because the two rules everyone
writes first are both **wrong on the endpoint this repo was built against**, and
wrong in opposite directions:

```
"401 means the key is bad, so stop."
    A wrong MODEL NAME returns 401 here, with the same envelope shape as a
    revoked key. Stopping is right for one of them and throws away a working
    session for the other.

"5xx is transient, so retry with backoff."
    A malformed request body returns 500 here. That is a bug in the client, and
    a policy keyed on the status retries it forever: it never succeeds and it
    never stops.
```

Neither gets fixed by reading the status code harder. Both are fixed by carrying
enough of the failure to decide with — which is the difference between a struct
and `fmt.Errorf`.

---

## What stage 08 left on the floor

Before this stage the whole of the agent's error handling was six lines:

```go
res, err := a.call(turn, msgs)
if err != nil {
    a.bus.Error("%v", err)
    return msgs
}
```

Three things are wrong with that, and only one of them is "no retry".

**The status code does not survive.** `a.call` returned
`fmt.Errorf("http %d: %s", ...)`, so the only way for a caller to find out that
this was a 429 is `strings.Contains(err.Error(), "429")`. People really write
that, and it is wrong on the day a message body happens to contain the number.

**The partial result was thrown away.** Both adapters end with

```go
if err != nil {
    // No KindResponseEnd: the response did not end, it broke.
    return res, err
}
```

— a non-nil result *alongside* a non-nil error, deliberately, with a comment
saying so since stage 02. Every stage up to this one bound that value and never
looked at it. Those tokens were generated. Generated tokens are billed.

**Compaction had its own, worse copy.** Stage 05's summariser does its own POST,
and its error path read no response body at all: a failing compaction could only
ever report `http 500` and nothing else. Two paths that classify the same failure
differently are two paths you cannot write one policy for.

---

## Phase first, status second

```go
const (
    phaseBuild   callPhase = "build"   // we could not even render the request
    phaseConnect callPhase = "connect" // no response: DNS, refused, TLS, reset
    phaseStatus  callPhase = "status"  // a response arrived and it was not 200
    phaseStream  callPhase = "stream"  // 200, then the body broke
)
```

The classifier looks at the phase before the status, because the phase answers
the question that decides everything else: **was anything generated?** Nothing
generated means a retry is free. Something generated means the first attempt is
already on the invoice and the retry is a second one.

`CallError` is what a decision can be made from:

```go
type CallError struct {
    Phase      callPhase
    Status     int           // 0 when there was no response
    Type       string        // the provider's error.type, verbatim
    Message    string
    Body       string        // first 8 KiB, verbatim
    RetryAfter time.Duration // what the server asked for, 0 if it did not
    Partial    *CallResult   // what the adapter had when the stream broke
    Err        error         // so errors.Is still works
}
```

`Body` looks redundant next to `Type` and `Message` and earns its place most
often. §D11 recorded a 400 that came back with **no error envelope at all** —
a 24-byte echo of the request, `{"model":"qwen3.7-plus"}`. For that response
`Type` and `Message` are both empty and the raw body is the only evidence there
is. The message names the absence rather than printing `http 400: ` with nothing
after the colon, because an empty tail reads like a bug in the agent and
"`http 400 with no error envelope: {"model":"qwen3.7-plus"}`" points at the
server.

`Type` is never normalised. It is the field that separates a wrong model name
from a revoked key when the status cannot, so normalising it would throw away
the evidence for the decision it drives.

---

## The table

Every row traceable to `docs/wire-notes.md`, and the rows that are *not*
observed behaviour say so. This is `triage_test.go`'s table, which is the same
table:

| observed | the obvious rule | what it actually is | verdict |
|---|---|---|---|
| `401` + `AuthError` | fatal | correct | **fatal** |
| `401` + `ModelError` | fatal — same status! | a *model* problem; another endpoint may have it | **fallback** |
| `401`, no envelope | crash on `error.type` | nothing to classify | **fatal** |
| `404` | fallback | correct | **fallback** |
| `429` | retry | *not observed here*; RFC 9110 | **retry** |
| `413` | retry | the bytes are the problem; only compaction changes them | **fatal** |
| `400` / `422` | retry the 5xx-shaped way | ours | **fatal** |
| `500` after a malformed body | transient | **a client bug wearing a server's status** | retry, short leash |
| `503` | transient | correct | retry, full leash |
| connect refused | retry | nothing generated, nothing billed | **retry** |
| stream broke, no type | retry | the transport died | **retry** |
| stream `overloaded_error` | retry | correct | **retry** |
| stream `invalid_request_error` | retry — it is a stream error | arrived because of what we sent | **fatal** |
| anything unclassified | retry | unknown | **fatal** |

Two rows deserve their own paragraph.

**The `error.type` match is a substring, not an equality.** §D11 observed
`ModelError` and `AuthError` — PascalCase — where both protocol specs document
snake_case (`not_found_error`, `invalid_request_error`). An equality check
against either spelling is correct against the documentation and wrong against
the wire.

**Unclassified is fatal, not retry.** An unclassified failure retried is a
failure repeated. The `call_error` event it emits, carrying the status and the
verbatim `error.type`, is how the missing row gets found.

### The leash

One extra rule, one reason:

```go
func (e *CallError) leash() int {
    if e.Phase == phaseStatus && e.Status >= 500 && e.Status != http.StatusServiceUnavailable {
        return 2
    }
    return 0 // the policy's full allowance
}
```

A `503` is a real capacity signal and gets everything. A bare `500` gets two
attempts total, because on this endpoint it is at least as likely to be *our*
misconfiguration as their outage — §D11 got a 500 for a malformed body *and* for
an OpenAI-shaped body POSTed to the Anthropic route. Two attempts rides out a
blip and is far too few to hide a permanent mistake behind a retry loop.

---

## Retry-After, and the thing this repo cannot prove

`parseRetryAfter` reads both forms RFC 9110 allows — delta-seconds and an
HTTP-date — and the server's number beats the computed backoff outright,
because it is the only number in the function that came from someone who knows
when the capacity returns.

It is also **the least well-founded code in this stage, and that is worth saying
out loud.** A grep over all 731 lines of `wire-notes.md` for
`429|Retry-After|timeout|502|503|504|408` returns exactly one hit, and it is not
captured bytes — it is §D11's own prescriptive takeaway:

> Takeaway: never classify these errors by HTTP status. Retry on 429 and on
> connection failures; treat 401 as fatal-but-ambiguous and log `error.type`;
> and treat 5xx as *possibly* permanent, capping retries. Always guard against
> an error body that has no `error` field.

Good advice, and stage 09 is largely its implementation — but the gateway this
repo was probed against never actually sent a 429, never sent a `Retry-After`,
and never dropped a stream. There is no recorded `Retry-After` anywhere in this
repo's evidence. Every other claim in `docs/` rests on bytes; this one rests on
the RFC plus a local server in `triage_test.go`. Where the rest of the repo says
"observed", this says "written from the spec".

Two guards come out of reading it as a spec rather than as a wish:

```go
if secs <= 0 { return 0 }                    // "-5" is not a negative sleep
if d := t.Sub(now); d > 0 { return d }       // a date in the past means "now"
```

Without them a header the server meant as "immediately" flows into `time.Sleep`
as a negative duration, which returns instantly — turning the backoff off at
exactly the moment a server asked for one. And a server is allowed to say "an
hour", so the value is clamped: the shape of the request is honoured, the length
is not.

### Full jitter, not half

```go
return time.Duration(rnd() * float64(exp))
```

A uniform draw from `[0, exp)`, not the more common `exp/2 + rand(exp/2)`. The
difference matters here specifically: stage 07 spawns subagents that share one
`http.Client` and one endpoint, so a provider hiccup fails several calls in the
same millisecond. Half-jitter keeps those calls clustered and they re-collide on
every attempt. A retry policy that synchronises its own clients is a load
generator aimed at the service that is already struggling.

---

## The ladder

```sh
agent --provider primary --fallback backup,local
```

Every rung is built and its key checked **at startup**, before the first
request. A fallback constructed on demand fails on demand, and the moment it is
needed is the only moment it exists for; a typo in `--fallback` should cost a
startup error, not a dead session during an outage.

Three things it deliberately is not:

- **Not a load balancer.** Nothing spreads traffic and the order never changes.
- **Not a circuit breaker.** It never climbs back, because "is the primary
  healthy again" cannot be answered without spending a real call to ask. A
  fallback is a downgrade you keep — which is exactly why every switch emits an
  event. A session quietly served by the cheaper model for an hour is the
  failure this design trades for simplicity, and it is only survivable if it is
  visible.
- **Not shared-nothing.** Subagents share the ladder by pointer, because "this
  endpoint is refusing calls" is a fact about the endpoint. A child that
  discovers it should not have to teach its parent.

That last one is the only place concurrency shows up, and it produces the one
subtle line in the file:

```go
func (l *ladder) advance(from int) bool {
    if l.cur > from { return true } // a sibling already moved us
    ...
}
```

`cur = from + 1` is already idempotent for two siblings who failed on the same
rung: both write the same value. The case that needs the guard has three
participants and it is a **rewind**. A fails on rung 0 and moves to 1. C fails on
1 and moves to 2. Then B — still holding the rung 0 it read before any of that —
asks to fall back, and without the guard it writes `cur = 1` and sends the next
call to a provider two siblings have already given up on.

---

## From a real run: the same status, opposite decisions

Both of these are 401. Both carry the identical envelope shape. Both sessions
were started with the same ladder.

```sh
agent --provider bad-model --fallback opencode-oai --yolo
```

```
stage 09 · provider=bad-model (openai) · model=gpt-does-not-exist-9000

you > count the lines in two.txt with wc and tell me the number
  call failed (attempt 1, fallback): http 401 ModelError: Model gpt-does-not-exist-9000 is not supported
  provider → opencode-oai (openai · mimo-v2.5) — fell back after http 401 ModelError: ...

  ┌─ call 1 · tool_calls
  │ in 963    ████████████████████  full 963 · write 0 · read 0
  ...
The file `two.txt` has **2** lines.
  cost: $0.000424
```

```sh
agent --provider bad-key --fallback opencode-oai --yolo
```

```
  call failed (attempt 1, fatal): http 401 AuthError: Invalid API key.
  error: http 401 AuthError: Invalid API key.

  0 calls · 0 commands
```

The second one has a ladder and does not touch it, which is the whole point: a
revoked key is not a reason to go and fail somewhere else too.

---

## From a real run: what a broken stream costs

This is the measurement that corrected the code, and the correction is more
interesting than the feature.

The gateway never dropped a stream on us, so the failure had to be made. A
throwaway reverse proxy — 121 lines around `httputil.ReverseProxy`, not part of
any stage, because `triage_test.go` covers the same branches reproducibly with
`httptest` — forwards the request upstream, lets the model generate, and then
cuts the connection part-way through the body (`panic(http.ErrAbortHandler)`
after 354 bytes, with a `Content-Length` that promises more). **The provider sees a completed generation and bills
for it. The client sees a broken stream and never receives the usage frame.**

```
  call failed (attempt 1, retry): the stream broke: unexpected EOF
  retrying in 313ms (attempt 2) — the stream broke: unexpected EOF

  ┌─ call 1 · tool_calls
  │ in 963    █░░░░░░░░░░░░░░░░░░░  full 3 · write 0 · read 960  100% cached
  ...
  ── session ──────────────────────
  cost: $0.000155
  1 retry
  retried attempts re-sent ≥963 prompt tokens (≥$0.000030)
```

The last line is a number no other agent reports, and it cannot come from the
API: usage arrives at the *end* of a stream and this stream did not end. So it is
inferred at the one moment a real figure for this prompt exists — the attempt
that finally worked — and it is printed as a bound, with a `≥`, because a cold
call pays the cache **write** on its first attempt and the cheaper **read** on
the retry, so copying the successful attempt's split under-charges the first one.

There is also a consolation in that panel worth noticing: `full 3 · read 960 ·
100% cached`. **The broken attempt was not wasted — it warmed the cache**, and
the retry read it back. A broken stream is expensive the first time and cheap
every time after.

### The bug this run found in my own panel

The first live run of this stage used the *other* fault: two injected 503s. The
panel said

```
  cost: $0.000276
  retried attempts re-sent ≥1926 prompt tokens (≥$0.000301)
```

which is nonsense. The re-bill is larger than the entire session, and it is
larger because it is fiction: **those requests were refused before generation, so
the provider billed nothing for them.** A 503 at the status phase and a refused
connection are free. Only a stream that opened and died has cost anything.

That is why `Phase` is on the event and not merely inside `CallError`. `Status`
cannot substitute for it: a connect failure and a stream break both carry status
0. The panel now keeps two counters — every wait, and only the attempts that
reached the model — and the same session reports:

```
  cost: $0.000287
  2 retries
```

Retries, no re-bill. `TestARefusedRequestIsNotReBilled` exists so it stays that
way.

The general lesson is the one stage 04 and stage 05 both landed on from
different directions: an instrument that reports a plausible wrong number is
worse than one that reports nothing, because the plausible number is the one
people quote.

---

## What a fallback costs

A prompt cache is per-provider, per-model and per-prefix, so a fallback starts
cold **by construction**. The same 963-token prompt, on a provider's first sight
of it and once it is warm:

| | full | cache read | cost of the prompt |
|---|---|---|---|
| warm | 3 | 960 | **$0.0000297** |
| first sight of this prefix | 963 | 0 | **$0.000289** |

(Those two figures come from two different runs, not from adjacent turns of one
session. The cold one is the first call after the fallback above; it is cold
because that provider had not seen this prefix — which is exactly the state a
fallback produces, every time.)

**9.7× for the identical bytes.** Moving to rung 1 discards the provider, the
model and the prefix at once. The turn you rescued cost you
ten prompts' worth of cache discount, and it will keep costing that until the new
provider's cache warms — which, on a session that only had a few turns left, may
be never.

This is not an argument against falling back. It is an argument for the event:
the cost basis changed mid-session, and the panel re-prices from the
`KindProvider` event rather than from startup configuration. Without that, the
second provider's tokens are billed at the first provider's rates — a cost report
that is confidently wrong, which is worse than one that admits it does not know.

A side effect worth having: a trace now records **which endpoint produced it**,
and at what prices. Until this stage you could read every byte of an archived
session and not be able to reconstruct its cost. `--replay` on a machine with no
`providers.json` now shows real money.

---

## The compaction call is a model call too

```go
res, err := retryLoop(bus, 0, pol.forCompaction(), newLadder(rung{p: p}), ...)
```

A one-rung ladder, deliberately: this call may be retried and may not be failed
over. Switching the whole session's provider as a side effect of a summarisation
hiccup is not a recovery, it is a surprise — and the prices would change under a
user who was told "compacting".

And a shorter leash, whatever the session's policy is:

```go
if p.attempts > 2 { p.attempts = 2 }
if p.budget > 5*time.Second { p.budget = 5 * time.Second }
```

Compaction already has a safe failure — the agent continues uncompacted and says
so — so giving up early costs little, while retrying hard costs a great deal:
every attempt re-sends the whole transcript at full price, and it does it while
the turn that needed the space is still waiting.

---

## Three events, not one

```
call_error   what broke, and what we decided about it
retry        that we are waiting, how long, and whose number that is
provider     who serves calls from here on, and at what prices
```

Same argument as stage 05's compaction triple: they answer different questions.
`call_error` is deliberately **not** `error`. An `error` is terminal — the session
telling the human it failed. A `call_error` is an attempt failing with a decision
attached, and most of them are followed by a success. Emitting them as errors
trains people to ignore the word, which is how a real one gets missed.

The composer shows the verdict on the row, because in a trace you are always
reading backwards from a symptom and the question is never "what broke" on its
own — it is "what did it do about it":

```
   1    0.00s  provider         flaky (openai · mimo-v2.5) · session start
   4    0.31s  request          openai · 1 messages · 0 cache marks · 3.2kB
   5   37.15s  call_error       RETRY attempt 1 · the stream broke: unexpected EOF
   6   37.15s  retry            wait 313ms · attempt 2 · the stream broke: unexpected EOF
   7   37.46s  request          openai · 1 messages · 0 cache marks · 3.2kB
   8   43.88s  first_token      TTFT 6414ms
  20   44.26s  usage            prompt 963 (full 3 · write 0 · read 960) · out 41
```

Two `request` events, one `usage`. That asymmetry *is* the re-bill, visible
without arithmetic.

---

## Honest limits

- **No deadline on a model call.** `http.Client{Timeout: 10 * time.Minute}` is
  the only clock, and it covers the entire body read: a slow-but-alive stream at
  minute ten dies mid-generation, and nothing can cancel a call in flight.
  `BuildRequest` returns a context-free `*http.Request`, so the *interface* is
  the barrier. Stage 10 is where that changes.
- **A retryable failure is assumed idempotent.** It is, for the reason that a
  failed model call ran no tools — but that reasoning is a property of this
  agent's shape, not of retries, and it stops holding the day a tool call is
  dispatched before the stream completes.
- **The re-bill figure is an estimate**, labelled `≥` everywhere it appears, and
  it is only ever a bound because the API cannot be asked.
- **Nothing feeds a partial back to the model.** The plan it was halfway through
  is gone, and half a tool call is worse than none. The partial is kept for the
  accounting, not for the conversation.
- **The ladder never climbs back**, and a long session can finish on rung 2
  without anyone noticing except via the event.
- **`!= http.StatusOK`, not a 2xx range.** A 202 would be classified as a
  failure. No endpoint in this repo's evidence returns one; it is still a
  latent row in the table.

Tests: 49 mutants applied to `triage.go`, `render.go`, `events.go` and
`replay.go` — every classifier branch, every guard, every json tag — and all 49
are caught. One mutant is documented in the runner as *equivalent* rather than
tested (`cur = from + 1` versus `cur = l.cur + 1`, provably equal under the guard
above them), which is the honest way to close a mutation report.

---

## Exercises

1. **Break the leash and watch it cost you.** Point `--provider` at a URL that
   always returns 500 with `--retry 8 --retry-budget 2m`, then remove the
   `leash()` clause and run it again. Time both. The second one is what "5xx is
   transient" buys.

2. **Add a 402.** `insufficient_quota` is the failure every hobby project meets
   first. Which verdict, and why is it not the same as 401? Write the row, then
   write the mutant that proves the row is load-bearing.

3. **Make the fallback climb back.** Add a half-open probe: after N successful
   calls on rung 1, send one call to rung 0 and promote it if it works. Then
   measure what the probe costs in cache misses, and decide whether you still
   want it. (This is a circuit breaker, and this is the bill for one.)

4. **Feed the partial back.** On a stream break, the partial sometimes contains
   *complete* tool calls — §A3c recorded one complete `tool_use` next to one
   truncated one. Send them anyway rather than discarding, then find a case
   where that is the wrong thing to do. It is not hard to find, which is why the
   default is to discard.

5. **Make `Retry-After` observed rather than specified.** Find any endpoint that
   really sends one, capture the bytes, and add the section to
   `wire-notes.md`. That is the only exercise here that improves the repo's
   evidence rather than its code.
