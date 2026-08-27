# Stage 05 — Live Forever

An agent has three memory horizons, and needs all three:

```
within a request   the messages array                        stages 00–04
within a session   compaction, when the array outgrows the window
across sessions    a file
```

The third one takes a paragraph. The second takes the rest of the chapter, and
ends somewhere you may not expect: **compaction did not save money.** It cost
25% more, on identical work, in a controlled run. That result is not a bug in
the implementation, and it changes what compaction is for.

---

## Long-term memory is a file

`AGENTS.md` and `MEMORY.md` in the working directory, read at startup, appended
to the system prompt. The agent writes to the second one with the tool it
already has:

```sh
printf '\n- the test suite needs CGO_ENABLED=1 or three packages fail to link\n' >> MEMORY.md
```

That is the entire feature. No store, no embedding, no retrieval step, no
similarity search over a corpus that `grep` would have handled.

The split is by **author**, not by content. `AGENTS.md` is written by a human
for the agent — conventions, build commands, what not to touch. `MEMORY.md` is
written by the agent for its future self. Keeping them apart means a person can
review what the agent decided to remember without wading through their own
instructions, and can delete a bad memory with an editor. An agent that writes
into the human's file eventually argues with it.

Three things fall out of choosing a file, and the last is the interesting one.

**A file is greppable, diffable, reviewable, versionable and editable by hand.**
Five properties a vector index does not have, in exchange for a similarity
search you did not need.

**The mechanism being trivial does not make the policy question go away.** Left
entirely to the model's discretion, memory gets written approximately never —
nothing in the current turn rewards it. Every agent that actually accumulates
useful memory has an explicit trigger. This one has `/remember`, and the system
prompt carries the rule that decides whether the file is worth reading in six
months:

> Record what you learned, not what you did.

That is the difference between a knowledge base and a diary.

**Memory is context injection, and injected context is believed.** Watch what
happened the first time this was run for real. The agent was started in
`sandbox/s05` with a copy of the repository's `AGENTS.md` — a file that
describes the *repository*, mentioning paths like `docs/wire-notes.md`. Asked to
count the lines in `wire-notes.md`, which was sitting right there in the working
directory, it ran:

```
  $ wc -l docs/wire-notes.md
  │ wc: docs/wire-notes.md: No such file or directory
```

Nothing malfunctioned. The memory file said `docs/`, and the model trusted a
statement in its system prompt over the directory it was standing in. That is
correct behaviour, and it is the risk: **a memory file is not notes, it is an
assertion the model will act on before it looks.** Which makes the cheap
mitigations worth doing — date every line so a stale one can be spotted and
deleted, keep memories scoped to where they are true, and keep the file short
enough that a human will actually read it.

**Memory written this session is not visible until the next one** — and that is
a cache decision, not an oversight. Memory lives in the system prompt, so
re-reading the file mid-session would rewrite the prefix and invalidate
everything stage 04 earned. A turn of latency in exchange for a session of cache
hits is the right side of the trade. It is worth knowing you made it.

---

## Where context is allowed to go

This is the half of the chapter that is not about compaction, and it is the half
you will use every day.

Every piece of injected context is one of two things, and the difference is not
what it contains but **how often its value changes**:

| | goes | cost |
|---|---|---|
| **stable for the session** — memory files, cwd, OS, shell | the system prompt, before the cache breakpoint | its tokens, once, no matter how long the session runs |
| **volatile** — the clock, git HEAD, the working tree's dirtiness | frozen into a message when that message is created | its tokens, once per turn, permanently |

The second row is the one people get wrong, and they get it wrong in the
direction that costs money. The instinct is to keep volatile context *fresh* —
recompute the timestamp on every request so the model always knows the time.
That is stage 04's `--break-cache` experiment, and it measured 3.4x.

The resolution is that "fresh" and "in the prefix" are the two things you cannot
have together — and freshness is the one you can give up almost for free:

> **Inject once and freeze. Never recompute what is already in the prefix.**

A snapshot taken when the user pressed Enter is accurate for the whole turn it
belongs to, and it stays in history unchanged afterwards, which is exactly what
a byte-stable prefix means. The model gets fresh information every turn *and*
the cache survives, because each turn's snapshot is a different permanent line
rather than the same line with a moving value. Here is what one looks like in
the middle of a real conversation, three turns after it was taken:

```
  [3] user
  │ <now>2026-08-27 04:38:53 +0800</now>
  │ <git branch="main" dirty="3">Stage 04: the cache</git>
  │
  │ 2. sed -n '91,180p' wire-notes.md
```

Note also which side of the line `cwd` is on. It is stable — because the shell
is not persistent, so `cd` inside a command cannot move it (stage 00). Give the
agent a persistent shell and cwd becomes volatile and has to move rows. A change
in the execution model propagates straight into the cache layout.

---

## Counting tokens without a tokenizer

Compaction needs to know how big the prompt is *before* sending it. The usual
advice is to vendor a tokenizer. For this job that is the wrong tool: it is a
large per-model dependency, it disagrees with the server about the framing
overhead of tool schemas and message envelopes, and it tells you nothing you
cannot get for free — because **every response already contains the exact token
count of the prompt you just sent.**

So measure instead. Note how many characters you sent, note how many tokens the
server said that was, keep the ratio, update it every call.

```go
func (a *agent) runTurn(msgs []Msg) []Msg {
    sentChars := convChars(msgs) + base
    res, err := a.call(turn, msgs)
    ...
    a.comp.est.observe(sentChars, res.Usage.Prompt())
}
```

The subtle part, and the reason it works at all: **the estimate does not need to
be accurate, it needs to be consistent.** It is only ever asked "are we near the
wall", and it is calibrated against the same character count it is later asked
to convert. Systematic bias — JSON envelopes, tool schemas, the system prompt —
is absorbed into the ratio rather than accumulated as error. What would break it
is measuring one thing and estimating another.

The instrument panel prints the live ratio next to the context watermark, so you
can watch it settle:

```
  └ context 5893 / 12000 (49.1%) · ≈3.7 B/tok
```

It started that session at 3.3 and climbed to 3.7 as the conversation moved from
the system prompt into Markdown — an 11% drift within one session, in one
direction. Any fixed divisor is wrong at one end or the other; a measured one is
wrong at neither.

### Does it actually work?

Three compactions in a real run. Each one predicts the size of the conversation
it just produced; the very next call reports what it actually cost:

```
  predicted ~3556   billed 2842   +25.1%
  predicted ~3823   billed 3624    +5.5%
  predicted ~3332   billed 3256    +2.3%
```

The first is poor and the reason is worth more than the number: at that point
the estimator had seven samples, taken while the prompt was mostly the system
prompt, and it was being asked about a conversation that had become mostly
Markdown. It converges within two more compactions and stays inside 6%.

A synthetic check pins it harder. Against a fake provider that charges
`chars/2.9` plus a 700-token envelope the estimator never sees directly, ten
calibration rounds later it predicted **21,708 tokens against 21,389 billed —
1.5% out**, having never been told either the divisor or the overhead.

For a threshold check, 6% is free accuracy. Set your threshold at 70% rather
than 78% and you have paid for the error with a margin you wanted anyway.

---

## Where you are allowed to cut

This is the part that produces a real bug in real agents, and the shape of the
bug is why it is hard to find.

Compaction replaces `msgs[:cut]` with a summary. Not every `cut` is legal:

**1. `msgs[cut]` must not contain a tool result.** Its matching tool call lives
in `msgs[cut-1]`, which is about to be deleted. A tool result whose call is gone
is an orphan, and both protocols reject it — OpenAI with *"messages with role
'tool' must be a response to a preceding message with tool_calls"*, Anthropic
with an unexpected `tool_use_id`.

**2. `msgs[cut]` must be an assistant message.** The summary is injected as a
user message, so cutting before another user message produces two user messages
in a row. Some endpoints merge them, some reject them, and the ones that merge
do it differently from each other.

Both collapse into one rule you can hold in your head:

> **A conversation may only be cut immediately before an assistant turn.**

Now the reason it is hard to find. Compaction happens at turn 14. The malformed
request goes out on turn 15. The error names the request builder. Nothing in the
stack trace mentions the compactor, and the conversation that reproduces it no
longer exists, because it was compacted. **The bug is a hundred lines away from
its symptom and one turn late.**

So it is guarded by an invariant rather than by a test case. `validConversation`
is written from the protocol's rules, deliberately *not* from the cutting logic,
and the property test asserts the two agree for every index:

```go
for i := range msgs {
    if canCutBefore(msgs, i) {
        got := validConversation(append([]Msg{summaryMsg("s")}, msgs[i:]...))
        // must be ""
    }
}
```

A check written from the same assumptions as the code will always agree with the
code. One written from the other end will not, and that is the entire value of
having it. Mutation-testing this pair caught all four ways of breaking it,
including the one that matters most — dropping the tool-result check leaves a
suite that still passes on any conversation whose cut point happens to land
somewhere safe.

### The floor

Compaction has a lower limit, and below it your problem is a different problem:

```
cannot compact: the newest message alone is ~11400 tokens against a keep budget
of 3000 — lower --max-output or use a command that filters
```

A single message larger than the whole keep budget cannot be compacted around.
That is an output-size problem — stage 01's truncation limit, or a command that
filters instead of dumping.

The first version of that code printed the same sentence for a second, different
situation: a budget with room for exactly one message, where the fix is
`--keep`, not `--max-output`. **An error message is a claim about causation.**
Get it wrong and you have not been unhelpful, you have been misleading — the
reader changes the setting you named, it does not help, and they conclude the
diagnosis was right and the situation is hopeless. It now distinguishes them.

---

## The summary

The summariser gets its own system prompt, no tools at all, and a *flattened*
transcript rather than a message array.

Flattening is the non-obvious choice. Passing the real array looks more faithful
and behaves worse: given a conversation, a model **continues** it — it answers
the last question again, or issues the next tool call. Flattening changes the
task from "converse" to "read this document". It also lets long tool output be
truncated before it is paid for, and it lets the call carry no tool definitions,
so a tool call is not merely discouraged but impossible.

The selection criterion is the part worth stealing:

> **Keep what would cost tool calls to rediscover.**

That is an economic test, not a semantic one, and it is far easier for a model
to apply than "keep what is important". A file path that took three greps to
find is worth a line. A paragraph of the agent's own narration is worth nothing,
because regenerating it costs nothing.

Long tool output is clipped from the **middle**, not the head. A build log puts
the error at the end; a stack trace puts the cause at the end. Keeping both ends
keeps what the command announced and what it concluded, and loses the
repetitive middle — which is the part that was long.

### A bug found by reading a real summary

The first version produced this, verbatim:

```
4. STATE
- Not done: Chunks 2–8 were never run.
```

It was false, and specifically false in a way that only shows up in production.
At the moment that summary was written, chunk 2 **had** run — its call and its
output were in the four messages being *kept*, which the summariser never saw.
So the model received a paragraph asserting something had never happened,
directly above the evidence that it had.

The summariser is shown a prefix and asked for a summary, and it answers as
though it had seen the whole session. Nothing in the prompt told it otherwise.
One sentence fixed it:

> You are reading only the EARLIER part of the session. More recent messages are
> being kept verbatim and will appear immediately after your summary, and you
> cannot see them. So never write that something was "never done" or "not
> started" as a statement about the session — say "as of the end of this
> transcript".

Same task, same model, next run:

```
4. STATE — Chunk 1 has been read (twice). Chunks 2–8 remain outstanding
           as of the end of this transcript.
```

The general lesson is not about summaries. **A component that sees part of the
system will describe the whole system unless you tell it what it is looking at**
— and it will sound equally confident either way.

---

## From a real run

One task, three policies, and the input was byte-identical across all three
arms: ten user messages, verified from the traces.

```sh
agent --yolo --window 12000 --compact-at 0.5  --keep 0.25   # tight
agent --yolo --window 12000 --compact-at 0.85 --keep 0.35   # roomy
agent --yolo --window 12000 --no-compact                    # none
```

| arm | compactions | prompt sent | full price | cache read | output | at 0.1x read | wall |
|---|---:|---:|---:|---:|---:|---:|---:|
| tight | 3 | 92,553 | 30,601 | 61,952 | 3,673 | **36,796** | 105s |
| roomy | 1 | 102,315 | 21,739 | 80,576 | 2,004 | **29,797** | 69s |
| none | 0 | 121,850 | 12,858 | 108,992 | 865 | **23,757** | 58s |

Read the third and fourth columns against each other. **The arm that sent the
most tokens paid for the fewest**, because it never broke its cache: 89% of
`none`'s entire prompt spend was cache reads at a tenth of the price.

`roomy` and `none` ran the **same eight commands** — no confound at all — and
compaction cost:

- **+25% in full-price-equivalent tokens**
- **+132% in output tokens** (summaries are output, and output is the expensive side)
- **+19% wall clock**

`tight` is worse still, and one caveat belongs with it: it ran three extra
commands (`ls -la`, `cat task.txt`, and one duplicated chunk). All three
happened **before its first compaction**, so compaction did not cause them —
that was ordinary model variance — but they do inflate its row by a few
per cent. The `roomy`/`none` pair is the clean comparison.

### The cache cliff

Within the tight arm, per call:

```
  #   kind     prompt   full   read  cached%
  6              5174   1654   3520     68%
  7              5258    138   5120     97%
  8 COMPACT      3310   3310      0      0%      ← the summarising call
  9              2842   2330    512     18%      ← the first call after
 10              2927    111   2816     96%
 ...
 14              5701    133   5568     98%
 15 COMPACT      3383   3383      0      0%
 16              3624   3112    512     14%
 17              3709    125   3584     97%
```

**Compaction is a two-call cache outage**, and it happens three times in that
table. The summarising call is a cold prompt by construction — different system
prompt, one-off user message, nothing to match. The call after it is cold
because the prefix was just rewritten. The third call is back over 95%.

This is why the agent prints the warning at the moment it is *caused* rather
than when it shows up:

```
  ≡ compacted: 15 → 5 messages · ~7714 → ~3556 tokens (-54%) · 6976ms
  ! the prompt prefix was rewritten — every cache entry from before this point
    is now unreachable, and the next call is a full-price miss
```

Without that line, the red bar on the next call looks like a regression instead
of a consequence.

### Thrashing

`tight` compacted three times where `roomy` compacted once. The arithmetic:

```
headroom = (threshold − keepRatio) × window
tight:  (0.50 − 0.25) × 12,000 = 3,000 tokens
roomy:  (0.85 − 0.35) × 12,000 = 6,000 tokens
```

One tool result at `--max-output 8000` is roughly 2,200 tokens. So `tight` had
room for **one turn** between compactions, and each of those compactions cost a
full-price read of the whole history plus a two-call cache outage plus about
seven seconds.

> The number that decides how often you compact is not the threshold. It is
> **the gap between the threshold and the keep ratio**, measured in turns of
> your actual tool output.

---

## So what is compaction *for*?

Not for saving money. It measurably does not. Every axis got worse: more
full-price tokens, far more output tokens, more wall clock, and a summary that
is lossy by construction.

> **Compaction is a survival mechanism, not an optimisation.** You compact
> because the alternative is a 400 and a dead session, not because it is
> cheaper.

Which inverts the usual design advice. If compaction only ever costs you, then
the goal is to compact **as late and as rarely as possible** — not at a
comfortable 70%, but at the last responsible moment, with as much headroom
afterwards as you can afford. And the cheapest compaction is the one that never
runs, which makes stage 01's output limit and a habit of `grep` over `cat` into
context-window features rather than politeness.

The counter-argument, stated fairly: none of these sessions came near the real
131k window. On a session that *would* hit the wall, compaction's cost is
compared against infinity, and it wins by definition. The numbers here do not
say compaction is bad. They say **it is not free, it is not a saving, and the
default threshold in most tutorials is set for the wrong objective.**

---

## Exercises

1. **Reproduce the three arms.** The absolute numbers will differ; the ordering
   of the "at 0.1x read" column is the finding.
2. **Find your own headroom.** Run one task at several `(threshold, keep)` pairs
   and plot compactions against total cost. The knee is your tool output size.
3. **Break the cut invariant on purpose.** Delete the tool-result check in
   `canCutBefore`, run a session with parallel tool calls, and watch the API
   reject a request that the compactor is nowhere near in the stack trace.
4. **Make the estimator lie.** Calibrate on `convChars` but estimate on
   `len(requestBody)` and watch the ratio become meaningless — evidence for
   "consistent, not accurate".
5. **Delete the scoping sentence** from `summarySystem` and read three summaries.
   Count how many make a claim about something they could not see.
6. **Move the volatile block into the system prompt** and compare the cache-read
   column. You have re-derived stage 04's arm C from a different direction.
7. **Give the agent a real task, twice, in two sessions**, with `/remember`
   between them. Then read `MEMORY.md` and ask whether any line would have saved
   a tool call. That question is the whole design brief for a memory system.

→ Next: [Stage 06 — The Composer](06-the-composer.md)

→ Reference: [Wire notes](wire-notes.md), [Stage 04 — The Cache](04-the-cache.md)
