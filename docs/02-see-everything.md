# Stage 02 — See Everything

Stages 00 and 01 built an agent that works. This one is about the fact that you
cannot see what it is doing.

That sounds like a complaint about logging. It is not. Stage 01 printed plenty.
The problem is that printing is the *only* thing it did with what it knew: the
moment a line scrolled off your terminal, the fact was gone. You could not
replay it, diff it, total it, or hand it to someone else.

So this chapter makes one structural change, and then collects the consequences.

---

## The one change

> **The agent core prints nothing. It emits events. Everything you can see is a
> subscriber.**

```
agent core ──emit──▶ Bus ──┬──▶ renderer     (terminal, instrumented)
                           └──▶ TraceWriter  (session.jsonl, one event per line)

replay:  session.jsonl ──▶ Replay ──▶ the same renderer, no network, no API key
```

Nearly everything else in this stage falls out of that:

| You get | Because |
|---|---|
| A permanent record of every session | the trace file is just another subscriber |
| Replay with no API key | a trace fed back through the same renderer |
| `--plain` vs. the stage-06 TUI | a choice of subscriber, not a fork of the code |
| Tests that assert on behaviour | assert on an event sequence, not on scraped stdout |
| A request inspector | the request bytes are already an event |

The transferable lesson is not "use an event bus". It is that **observability is
a shape you choose at the beginning, not logging you add at the end.** Every
`fmt.Printf` in stage 01 was a place where the only record of a fact was a
character on a screen.

The bus dispatches **synchronously, under a lock**. That is slower than a
channel per subscriber and it is deliberate: it makes ordering total and
identical for everyone, so the trace file and your terminal can never disagree
about what happened first. A trace that can disagree with what you saw is not
evidence.

---

## The instrument panel

Here is one call from a real session:

```
  ┌─ call 5 · stop
  │ in 1066   ███████████████████   full 106 · write 0 · read 960
  │ out 117    TTFT 2943ms · total 4533ms · 73.6 tok/s
  │ $0.000201    session $0.000856 over 5 calls
  └ context 1066 / 131072 (0.8%)
```

Four lines, and each answers a question most agents cannot.

### `in 1066` — and why no API field says 1066

This is the number people mean by "how big is my context now", and **you cannot
read it off any single field the API returns.**

On an Anthropic-style protocol, `input_tokens` is only the *uncached remainder*.
An agent that has been running for an hour can honestly report `input_tokens:
18` while sending 18,000. The real prompt size is `input + cache_write +
cache_read`, which is what `Usage.Prompt()` computes and what this line shows.

On an OpenAI-style protocol the accounting runs the **opposite direction**:
`prompt_tokens` is the full figure and `cached_tokens` is nested *inside* it. So
normalising means subtracting:

```go
Input     = prompt_tokens - cached_tokens   // billed at full price
CacheRead = cached_tokens
```

Copy `prompt_tokens` straight across instead and `Prompt()` reports 698 for a
506-token prompt. Note when that bug is invisible: **the error is exactly the
size of the cache hit**, so it is zero on a cold request, looks perfect in
testing, and gets steadily worse the better your caching works.

### The bar — three colours because you are billed three rates

```
███████████████████        red = full price · yellow = cache write (~1.25x) · green = cache read (~0.1x)
```

A table of three numbers is readable. A bar is *glanceable*, and the thing worth
noticing is a **change in proportion between turns**. When the green disappears,
something invalidated your cache — and you want to see that on the turn it
happens, not on a bill at the end of the month. Stage 04 is entirely about
keeping that bar green.

### `TTFT 2943ms · total 4533ms · 73.6 tok/s`

TTFT and throughput are separate numbers because **they fail for separate
reasons**. A slow first token is a queue, a cold cache, or a long prompt. Slow
throughput is the model itself. One number averaging both tells you nothing
actionable — and note the first call of the session below, at 13 seconds TTFT
against 1.2 seconds for the second: that is a cold start, and averaging would
have hidden it.

### `$0.000201` — or a dash

If you do not pass `--price-in` / `--price-out`, this line reads
`cost — (set --price-* to price this run)`.

**A made-up zero is worse than no number**, because zero is the number people
quote. The agent does not know your rates, so it declines to invent them.

---

## The session summary, and the number stage 04 exists to move

```
  5 calls · 5 commands
  prompt tokens billed: 3941  (full 869 · write 0 · read 3072)
  output tokens: 419
  cost: $0.000856
  re-send ratio: 3.7x (billed 3941 for a final context of 1066)
```

The last line is the one to keep. Stage 00's docs recorded this ratio at **4.2x
with no visibility at all**. Here it is 3.7x — but now you can also see that
**3072 of those 3941 tokens were cache reads**, at roughly a tenth of the price.
The re-send tax is real; most of it was already cheap, and nobody could have
told you that before this chapter.

---

## The trace

One JSON object per line:

```json
{"seq":1,"t":"2026-08-27T03:15:34.33+08:00","kind":"user_message","text":"run python stats.py…"}
{"seq":2,"t":"2026-08-27T03:15:34.34+08:00","kind":"turn_start","turn":1}
{"seq":3,"t":"2026-08-27T03:15:34.34+08:00","kind":"request","turn":1,"request":{"model":"mimo-v2.5",…}}
```

That five-turn session is 196 events and 40KB.

**JSONL is not a style choice, it is the crash contract.** A JSON array needs a
closing bracket that a killed process never writes — so the file documenting the
crash would be unparseable *because of* the crash. Line-delimited means every
completed line is independently valid, and a half-written final line costs you
one event instead of all of them. `ReadTrace` treats a truncated tail as the
**normal** shape of a killed session, recovers everything before it, and reports
what happened as an event in the stream rather than as an error that would
tempt a caller to throw away the 195 events explaining the crash.

**"Flush every line" means unbuffered, and explicitly not `fsync`.** One write
per event costs microseconds into the page cache and already survives SIGKILL,
panic and `os.Exit`. `fsync` additionally survives a power cut and costs
0.1–10ms — *on every text delta, inside the bus lock*. Three orders of
magnitude, to defend a much rarer failure. Knowing where the line is drawn is
more useful than being told "we flush".

**"Never block the bus" is not "never do I/O".** The obvious fix is an async
writer, and a queue has exactly two behaviours when full: block the producer
(the thing you were avoiding) or drop events (a trace that lies by omission,
under precisely the load you most wanted recorded). The real rule is *no
unbounded wait* — no fsync, no network, no lock held across a channel send.
There is deliberately no goroutine in `trace.go`.

**`bufio.Scanner` is a trap for trace readers.** It caps a token at 64KB and
fails the *entire* read with `ErrTooLong` — and the single most valuable line in
a trace, the request body, is the one that crosses 64KB around turn thirty.
`bufio.Reader.ReadBytes` has no cap, and hands back a newline-less final line
together with `io.EOF`, which *is* the truncation signal.

**Forward compatibility is mostly a thing you don't do.** `ReadTrace` never
validates `kind` against the constants in `events.go`. Validating would mean
every kind added in a later stage silently breaks replay of files written after
it.

---

## Replay

```sh
agent --replay session.jsonl              # original timing
agent --replay session.jsonl --speed 0    # instant
agent --replay session.jsonl --step       # Enter for each event
```

No API key. No network. No shell. Run with the environment stripped:

```
trace · 196 events · 5 turns · 5 commands · 25.34s
tokens · prompt 3941 (full 869 · write 0 · read 3072) · output 419
replay · instant
```

…and the session reproduces exactly, because the renderer never had a clock of
its own. **Every number it prints arrived in an event.** If you ever find
yourself wanting `time.Now()` in `render.go`, the number you want belongs in an
event instead.

This is what makes the repo teachable: a student with no credentials can study
a real session, and you can debug someone else's run from the file they sent
you.

Timing detail worth knowing: recorded gaps are **capped at 5 seconds before**
`Speed` scales them. Everything replay exists to convey lives under 5s — TTFT,
delta pacing, a command's wall clock. Anything longer is a human being idle,
which the timestamps already report better than a wait does. Capping *before*
scaling means `--speed 2` still halves the worst case, and `--speed 0.5` can
still stretch a pause for someone who wants to feel it.

---

## What streaming actually costs you

Streaming is not "the same response, arriving gradually". It is a different data
shape, and you have to rebuild the old one.

**You must reassemble the assistant message.** The history needs the message the
API *would* have returned non-streamed. Forgetting this is why streaming agents
mysteriously "lose" their tool calls: the deltas rendered fine, and nothing ever
went back into `messages`.

**The parser has to survive this protocol as it actually is.** Every one of
these is recorded with raw evidence in [wire-notes.md](wire-notes.md) §B4–B7,
and every one is a real trap:

| Observed | Consequence |
|---|---|
| The usage chunk has `"choices": []` | `choices[0]` panics. The most likely bug in the file. |
| `id` and `function.name` arrive in exactly **one** chunk, `null` in all later ones | Latch on first sight; never overwrite with null |
| `arguments` fragments are **not** JSON-aligned (`"{\"command\": "`, `"\""`, `"ls"`, `" -la /srv"`, `"/app"`) | Accumulate raw bytes keyed by `index`; never parse a fragment |
| Every field is emitted explicitly as `null` rather than omitted | "Key present" tells you nothing. Test values |
| A frame arrives **after** `data: [DONE]` | Skip the sentinel, keep draining to EOF |
| The stream carries no `event:` lines at all | …but `readSSE` still supports them, because stage 03's second protocol uses them |

The `[DONE]` decision deserves its own note, because "stop at the sentinel" is
what the spec says and it is wrong here for three reasons: the post-`[DONE]`
frame is real data; abandoning a body with bytes left in it stops the HTTP
transport reusing the connection, quietly adding a TLS handshake per turn; and
if usage ever moves behind the sentinel, an early-stopping client reports zero
tokens and is confidently wrong.

**TTFT gates on real payload, not on the first frame.** The first chunk carries
`content: ""`. Counting it turns TTFT into time-to-first-*byte* and flatters
every request on a thinking model.

---

## The integration bug this chapter shipped and then fixed

Worth keeping, because it is the characteristic failure of event-driven designs
and it does not look like a crash.

The first working version printed **two panels per call** — one full of zeroes,
then one with real numbers. The cause: the stream parser emitted
`KindResponseEnd` (it knows when the response ended), *and* the agent loop
emitted its own (it knows the usage). **Two components each believed they owned
the same event.**

Two fixes, and both are the general lesson:

1. **One owner per event.** The parser keeps `KindResponseEnd`, because it is
   the component that knows whether the response ended *cleanly* — and on a
   mid-stream error it deliberately emits nothing, so a trace never records a
   clean ending that did not happen.
2. **A renderer should not care which event a number rode in on.** It latches
   the last `KindUsage` it saw and uses that. Reading usage only off
   `KindResponseEnd` was the fragile assumption underneath the zeroes.

The same pass removed a second duplication: the command footer printed an exit
code and duration that the tool-result text already ended with. Now only the
tool result is shown — **exactly the bytes the model was given.** Showing a
human a nicer summary than the model received is precisely the divergence this
stage exists to eliminate.

---

## Exercises

1. **Break the ordering guarantee.** Make `Bus.Emit` dispatch in a goroutine.
   Watch the trace and the terminal disagree about ordering, intermittently.
   Then put it back.
2. **`jq` the trace.** `jq -r 'select(.kind=="command_start") | .command'` gives
   you every command a session ran. This is why the format is boring on purpose.
3. **Turn on `--show-request` and read the first request in full.** Most
   "why did the model do that" questions die here: the prompt did not contain
   what you assumed.
4. **Replay someone else's trace.** Delete your API key from the environment
   first, to prove to yourself that replay needs nothing.
5. **Add an event kind.** Emit it, render it, confirm old traces still replay.
   Then try renaming an existing kind and watch what it does to a recorded
   session — that is why the constants carry a warning.
6. **Find the cold start.** In the run above, call 1 has TTFT 13042ms and call 2
   has 1239ms. Explain the difference, then check the cache column and see
   whether it agrees with you.

---

## What you can answer now

**Why does the agent core print nothing at all?**
Because a `fmt.Printf` makes the terminal the only record of a fact, and the
moment that line scrolls away the fact is gone — you cannot replay it, diff it,
total it, or hand it to anyone. Emitting an event instead means the terminal,
the trace file, the tests and the later full-screen interface are all
subscribers to one sequence. That is why `--plain` versus a TUI is a choice of
subscriber rather than a fork of the code.

**Why is the bus deliberately synchronous and under a lock?**
Because it makes the ordering total and identical for everyone, so the trace
file and your terminal can never disagree about what happened first. A channel
per subscriber would be faster and would let the two drift apart under exactly
the load you most want recorded. A trace that can disagree with what you saw is
not evidence, and evidence is what this stage is for.

**Why can no single API field tell you how big your prompt was?**
Because the two protocols count in opposite directions. On an Anthropic-style
protocol `input_tokens` is only the uncached remainder, so a session that has
been running for an hour can honestly report 18 while sending 18,000, and the
real figure is `input + cache_write + cache_read`. On an OpenAI-style protocol
`prompt_tokens` is already the whole number with `cached_tokens` nested inside
it, so normalising means subtracting rather than adding.

**Why is getting that normalisation wrong so hard to notice?**
Because the error is exactly the size of the cache hit. It is zero on a cold
request, so the code looks perfect in testing, and it grows as caching starts
working — the number is most wrong when the agent is running best. The recorded
version of the bug reports 698 tokens for a 506-token prompt.

**Why does the cost line print a dash instead of zero?**
Because the agent does not know your rates unless you pass `--price-in` and
`--price-out`, and a made-up zero is worse than no number: zero is the number
people quote. The line reads `cost — (set --price-* to price this run)` instead,
which names the absence and says what to do about it.

**Why is the trace JSONL rather than a JSON array?**
Because an array needs a closing bracket that a killed process never writes, so
the file documenting the crash would be unparseable because of the crash. With
one object per line, every completed line is independently valid and a
half-written final line costs you one event instead of all of them. `ReadTrace`
therefore treats a truncated tail as the normal shape of a killed session and
reports it as an event in the stream, rather than as an error that would tempt a
caller to discard the 195 events explaining the crash.

**Why flush every line and deliberately not `fsync`?**
Because the two defend different failures at very different prices. An
unbuffered write costs microseconds into the page cache and already survives
SIGKILL, a panic and `os.Exit`; `fsync` additionally survives a power cut, at
0.1 to 10ms — on every text delta, inside the bus lock. Three orders of
magnitude for a much rarer failure is the trade being declined, and knowing
where the line was drawn is more useful than being told that the writer flushes.

**Why is there no goroutine in `trace.go`?**
Because the obvious async writer needs a queue, and a full queue has exactly two
behaviours: block the producer, which is the thing the goroutine was added to
avoid, or drop events, which produces a trace that lies by omission under
precisely the load you most wanted recorded. The rule that actually matters is
not "never do I/O" but "never wait without a bound" — no fsync, no network, no
lock held across a channel send.

**Why keep reading the stream after `data: [DONE]`?**
Because stopping at the sentinel is what the specification says and it is wrong
here, for three separate reasons. A frame was observed arriving after it with
real data in it; abandoning a body with bytes left in it stops the HTTP
transport reusing the connection, quietly adding a TLS handshake to every turn;
and if usage ever moves behind the sentinel, a client that stopped early reports
zero tokens and is confidently wrong.

**Why did the first working version print two panels for every call?**
Because the stream parser emitted `KindResponseEnd`, knowing when the response
ended, and the agent loop emitted its own, knowing the usage — two components
each believing they owned one event. The fix has two halves, and both
generalise: one owner per event, with the parser keeping this one because it is
the component that knows whether the response ended cleanly; and a renderer that
latches the last usage it saw instead of reading usage off one particular event.

---

## Questions to think about

These have no answer in the repo. Each is a decision an event bus makes easy to
postpone and eventually forces.

1. The bus is synchronous, so a slow subscriber slows the agent, and nothing in
   this stage is slow. The first genuinely slow one you want — a network sink, a
   database, a live web view — has to go somewhere, and the two obvious places
   are the two this chapter rejected. Where would you put it?

2. Replay is exact because every number the renderer prints arrived in an event,
   which also means anything not in an event is gone for good. How would you
   decide what deserves one, given that a wrong answer surfaces months later,
   when somebody asks a question of an old trace?

3. A trace holds every command, every output and the full request body, which is
   what makes it worth sending to a colleague and what makes sending it
   dangerous. What would you redact, and would you do it when the event is
   emitted, when it is written, or when it is read — and what does each of those
   choices make impossible?

4. Not validating `kind` keeps old readers working as new kinds appear, which
   covers additions. What is your plan for the first time a field inside an
   existing kind has to change meaning, with a directory of old traces still on
   disk and still worth reading?

5. The panel is built for a human watching one session go past. What would you
   show for a hundred sessions a day, and which of these numbers still means
   anything once it has been averaged across all of them?

→ Next: [Stage 03 — Babel](03-babel.md)

→ Reference: [Wire notes](wire-notes.md) — the observed behaviour every claim in
this chapter rests on
