# Stage 04 — The Cache

Stage 03 ended on an asymmetry it refused to explain: the same task, the same
size of conversation, and one protocol's cache bar was green while the other's
was entirely red.

This chapter fixes it, and then spends most of its length on the part that
matters more — that **prompt caching is a discipline, not a feature.** The code
is about forty lines. The rules around those forty lines are worth far more than
the lines.

---

## The mechanism, in one paragraph

The rendered prompt is `tools`, then `system`, then `messages`, in that order.
Caching is a **prefix match**: a `cache_control` marker says *everything up to
here is a reusable prefix*. Two consequences follow immediately, and everything
else in this chapter is a consequence of them:

- A marker only helps if everything **before** it is byte-identical next time.
- A byte that changes early invalidates every marker after it — so the ordering
  of stable-to-volatile content matters more than the markers do.

Four markers are allowed per request. This adapter places two:

```
tools ─────────┐
system ────────┴─▶ [1] frozen for the whole session
messages
  turn 1 …
  turn N ──────────▶ [2] rolling: everything up to the newest turn
```

Marker 1 pays for itself on every request after the first. **Marker 2 is the one
that matters in an agent**, because each turn re-sends the entire conversation:
without it, every turn re-reads the whole history at full price. That is the
re-send ratio stage 00 measured at 4.2x and could not explain.

---

## Lesson zero: the marker is not the mechanism

The first run of this chapter's experiment produced this:

```
  │ in 528    ████████████████████  full 528 · write 0 · read 0
  │ in 647    ████████████████████  full 647 · write 0 · read 0
  │ in 746    ████████████████████  full 746 · write 0 · read 0
```

`cache_control` was on. Nothing cached. No error, no warning, no complaint.

**The prompt was too short.** There is a minimum cacheable prefix — model
dependent, commonly 1,024 to 4,096 tokens — and below it a marker is silently
ignored. Not rejected: *ignored*, with `cache_creation_input_tokens: 0` and a
200 OK.

Two things to take from that. Short agent sessions do not cache at all, so
"caching is on" is not a thing you can know from your config — only from your
usage numbers. And **the absence of an error is not evidence of success**, which
is the recurring theme of this entire repo.

---

## The experiment

One task, three arms, on the same 56KB file (~10,000 tokens once read into
context):

```sh
agent --yolo --max-output 60000                # A: cache_control on
agent --yolo --max-output 60000 --no-cache     # B: no markers (implicit only)
agent --yolo --max-output 60000 --break-cache  # C: a fresh timestamp per request
```

### Arm A — explicit `cache_control`

```
  │ in 535    ████████████████████  full 535 · write 0     · read 0
  │ in 10348  █▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓  full 6   · write 10342 · read 0
  │ in 10484  ▓░░░░░░░░░░░░░░░░░░░  full 6   · write 136   · read 10342  99% cached
  │ in 10655  ▓░░░░░░░░░░░░░░░░░░░  full 6   · write 171   · read 10478  98% cached
  │ in 10805  ▓░░░░░░░░░░░░░░░░░░░  full 6   · write 150   · read 10649  99% cached
  │ in 11114  ▓░░░░░░░░░░░░░░░░░░░  full 6   · write 309   · read 10799  97% cached
  │ in 11276  ▓░░░░░░░░░░░░░░░░░░░  full 6   · write 162   · read 11108  99% cached

  prompt tokens billed: 65217  (full 571 · write 11270 · read 53376)
```

Read the read column: **10342, 10478, 10649, 10799, 11108** — each one is exactly
the previous turn's prompt. The rolling marker is doing its job; every turn reads
what the last turn wrote, and writes only the delta.

### Arm B — no markers, implicit caching only

```
  │ in 10348  ███░░░░░░░░░░░░░░░░   full 1900 · write 0 · read 8448   82% cached
  │ in 10793  ███░░░░░░░░░░░░░░░░   full 2089 · write 0 · read 8704   81% cached
  │ in 11018  ████░░░░░░░░░░░░░░░   full 2570 · write 0 · read 8448   77% cached
  │ in 11570  ████░░░░░░░░░░░░░░░   full 2866 · write 0 · read 8704   75% cached
  │ in 11671  █░░░░░░░░░░░░░░░░░░   full 791  · write 0 · read 10880  93% cached

  prompt tokens billed: 55935  (full 10751 · write 0 · read 45184)
```

Caching still happens — this gateway caches implicitly. But look at the read
column: **8448, 8704, 8448, 8704, 10880.** Every one is a multiple of 64
(132×64, 136×64, 170×64), and they **go down as well as up** while the
conversation only grows.

That is the difference in one line. The implicit cache matches on **64-token
block boundaries** and re-decides every request; `cache_control` **pins the exact
prefix**, and the hits become monotonic and predictable. Explicit caching buys
you *determinism* — the ability to tell a regression from noise.

### Arm C — a timestamp in the system prompt

```
  │ in 10387  █▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓  full 6 · write 10381 · read 0
  │ in 10497  █▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓  full 6 · write 10491 · read 0
  │ in 16352  █▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓  full 6 · write 16346 · read 0
  │ in 16458  █▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓  full 6 · write 16452 · read 0

  prompt tokens billed: 54267  (full 597 · write 53670 · read 0)
```

**`read 0` on every single call.** Every turn writes a brand new cache entry that
nobody will ever read, and pays the write premium to do it.

Convert all three to full-price-equivalent tokens using the common multipliers
(write ≈1.25x, read ≈0.1x):

| Arm | full | write | read | equivalent |
|---|---:|---:|---:|---:|
| A — explicit | 571 | 11,270 | 53,376 | **~20,000** |
| B — implicit only | 10,751 | 0 | 45,184 | **~15,300** |
| C — broken | 597 | 53,670 | 0 | **~67,700** |

**A broken cache is worse than no cache** — 3.4x worse than arm A and 4.4x worse
than arm B — because you pay the write premium on every token and collect
nothing. Nothing errors. The only symptom is the bill.

### And the honest part: B beat A

On this six-call session, the arm with no markers came out **cheapest**. That is
a real result and it deserves to be stated rather than buried.

Why: the rolling marker writes a new entry every turn, and the first write is
the whole 10K-token prefix at 1.25x. Six calls is not enough reads to amortise
that against an implicit cache that costs nothing to write and was already
getting 75–93%. The write premium needs volume — and on a long agent session,
where the same prefix is read dozens of times, the arithmetic flips.

Three caveats you should hold onto, because they are the difference between a
measurement and a slogan:

- **The multipliers are assumptions.** This gateway reports `cost: "0"` on every
  response (wire-notes §C10), so the dollar column is computed from the common
  industry ratios, not from this provider's actual bill. Token counts are
  measured; money is inferred.
- **The arms are not perfectly controlled.** The model answered slightly
  differently each run, so the totals differ. The *proportions* are the finding,
  not the absolute numbers.
- **Your break-even depends on your session length and your provider's
  multipliers.** Measure it. The point of this chapter is not that you should
  turn caching on; it is that you should be able to *tell*.

---

## Lesson two: where you compute a value decides whether it hurts

`--break-cache` did not work the first time. Injecting the timestamp **once at
startup** left the cache working perfectly — because a value that is constant
for a session is a constant prefix for that session.

The bug that actually gets shipped is `datetime.now()` inside a prompt builder
that runs on **every call**. Same line of code, same variable, thirty lines apart
in the call graph, and a 3.4x difference in cost.

So the audit question is not "is there a timestamp in my prompt". It is **"is
there anything in my prefix whose value can differ between two consecutive
requests"** — and that is a question about *where the code runs*, not about what
it says.

---

## The five disciplines

Everything the code does is small. This is the part to remember.

**1. Freeze the system prompt for the session.** Anything dynamic — the date, a
user id, a mode flag, a feature toggle — belongs *after* the last breakpoint,
in the message stream, never at the front of the prefix. A message appended at
turn 5 invalidates nothing before turn 5.

**2. Freeze the tool list.** Tools render at position zero, so adding, removing
or reordering one invalidates *everything*, markers included. Serialise them
deterministically. If you need "modes", pass the mode in a message; do not swap
the tool set.

**3. Append only; never rewrite history.** Editing an old message moves every
byte after it. This is also why compaction — stage 05 — is expensive in a way
that does not show up in its own token numbers: it rewrites the prefix, so the
turn after a compaction is a guaranteed full-price miss.

**4. Keep serialisation byte-stable.** Go sorts map keys; a model does not.
Decode-and-re-encode a tool call's arguments and the bytes move, which is why
`Block.Args` is a raw string all the way from stage 03. `TestToolArgumentKeyOrderSurvives`
exists to make that regression loud, and mutation-checking it produced the
warning verbatim:

```
tool arguments were re-serialised: keys came back sorted, so the prompt prefix
moved and every cached turn is now a miss
```

**5. Watch the read column, not the config.** Caching has no success signal. The
only way to know it is working is that `cache_read` is large and growing
monotonically. That is why the instrument panel from stage 02 exists.

---

## Two traps that produce no error at all

**The minimum prefix.** Below roughly 1K–4K tokens (model dependent), a marker is
ignored. Your careful placement does nothing and nothing tells you.

**The 20-block lookback.** A breakpoint searches backwards a limited number of
content blocks for an existing entry. An agent turn that fires many parallel
tools can add more blocks than that in one go — after which the next marker
finds nothing, and you silently pay full price. One tool per turn stays well
inside the window. A fan-out agent needs an intermediate marker, which is what
two of the four slots are still free for.

---

## A note on the bar

The three-way split is drawn with three different **glyphs**, not just three
colours:

```
████▓▓░░░░░░░░░░░░░░   █ full price   ▓ cache write   ░ cache read
```

That was not the original design. The first version used one glyph in three
colours, which looked fine in a terminal and became a featureless block the
moment the output went through `grep`, into a file, or into a CI log — which is
exactly how people look at agent output when something is wrong. **A chart that
only works in a colour terminal is blank precisely when someone is trying to
show you a problem.**

---

## Exercises

1. **Run the three arms yourself** and compare the read columns. Then run arm A
   twice as long and see whether the crossover happens.
2. **Add a fifth marker.** Watch the API reject it, and decide which of your four
   you would give up.
3. **Move the rolling marker to a fixed position** — say, always message 3 — and
   watch the hit rate decay turn by turn as the conversation grows past it.
4. **Break discipline 4 deliberately**: decode and re-encode the tool arguments.
   The bytes look identical to you and the read column collapses.
5. **Find your break-even.** Run the same task at 5, 20 and 50 turns with and
   without markers, and find the session length where explicit caching starts
   winning on your provider. That number is worth more than any advice in this
   chapter.

→ Next: [Stage 05 — Live Forever](05-live-forever.md)

→ Reference: [Wire notes](wire-notes.md) §C8–C10
