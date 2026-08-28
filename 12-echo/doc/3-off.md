# Stage 12 · part 3: wiring it in, and defaulting it off

[← back to stage 12](README.md) · previous: [2 · key and witnesses](2-witness.md)

> A cache that never hits is behaviourally identical to one that works: correct
> results, no errors, no log line, tests green. So the panel prints a line full
> of zeros, and the flag defaults to off.

---

## The problem

The mechanism is right. Now it has to go somewhere, and every placement decision
has a wrong version that looks fine.

Put the lookup before the permission gate and a repeated command is served
without the user being asked — which is defensible right up to the moment
somebody wanted to say no this time.

Emit a `command_end` on a hit and the trace claims a process ran that did not.
Do not emit anything and the session summary cannot count hits.

And the largest one: **how does anybody know the cache is working?** Not from
correctness, because a broken cache is also correct. Not from errors, because it
has none.

---

## The idea

Three placements, and one default.

| decision | choice |
|---|---|
| where the lookup goes | after the gate, in the default branch |
| what a hit emits | one `result_cache` event, and no command events |
| what the panel shows | the line, even when every number is zero |
| the flag | off |

---

## Building it

### Step 1: the lookup goes after the gate

```go
v, why := a.g.ask(command)
a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
switch v {
// ...
default:
	texts[i] = a.runCommand(ctx, turn, c.ID, command)
}
```

The gate is asked *every* time, including for a command it has already approved
this session. Not because the second run is more dangerous — it is the same
command — but because "the user is asked about every command the model requests"
is a promise, and a cache is an implementation detail that must not quietly
weaken it.

```go
if got := rec.count(KindGateVerdict); got != 2 {
	t.Fatalf("gate verdicts = %d, want 2: the second command was served without being asked about", got)
}
```

### Step 2: a hit emits no command events at all

```go
look := a.echo.lookup(a.cfg.shell, a.cfg.wd, command, a.cfg.maxOutput, a.cfg.env)
if look.verdict == cacheHit {
	a.bus.Emit(Event{
		Kind: KindResultCache, Turn: turn, ToolID: callID, Command: command,
		Verdict: string(cacheHit), Bytes: len(look.text), Millis: look.millis,
	})
	return look.text
}
```

No `command_start`, no `command_end`. Nothing ran, so a trace that said otherwise
would describe an event that did not happen — and stage 02's whole claim is that
a trace is evidence.

The consequence shows up in the replay header, and there is a test for it:

```go
if got := Summarize(rec.events).Commands; got != 1 {
	t.Errorf("the replay header reports %d commands, want 1", got)
}
if got := Summarize(rec.events).CacheHits; got != 1 {
	t.Errorf("the replay header reports %d cache hits, want 1", got)
}
```

Three identical tool calls, one command, two hits. The header says exactly that.

This is also the reason [part 1](1-audit.md)'s audit tool is blind to its own
product: no `command_end`, nothing to audit.

### Step 3: the other three verdicts get an event too, but only when the cache is on

```go
if a.echo != nil {
	a.bus.Emit(Event{
		Kind: KindResultCache, Turn: turn, ToolID: callID, Command: command,
		Verdict: string(look.verdict), Text: look.reason,
	})
}
```

A miss, a stale and a refusal are all worth recording — a refusal names the rule
that did not understand the command, which is the to-do list from part 1.

The `a.echo != nil` guard matters: with the cache off, this must emit **nothing**,
so that a trace from a cache-off session is byte-comparable with one from before
this chapter existed.

### Step 4: emit first, then store

```go
a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: callID, Command: command})
r := runBash(ctx, a.cfg.shell, command, a.cfg.timeout)
rendered, truncated := r.render(a.cfg.maxOutput)
a.bus.Emit(Event{
	Kind: KindCommandEnd, Turn: turn, ToolID: callID, Command: command,
	ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
	Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
})
a.echo.store(look, command, rendered, r)
```

`store` last. It re-hashes the witnesses (part 2, step 6) and may decide to store
nothing at all — and the events describing what happened must not depend on that
decision.

### Step 5: what comes back must be the exact same bytes — including a duration that is now a lie

```go
status := fmt.Sprintf("\n[exit %d · %s]", r.ExitCode, r.Duration.Round(time.Millisecond))
```

The stored text includes that footer, and a hit hands it back unchanged. So the
model is told a call took 92 ms when it actually took microseconds.

There is a test that the two are identical:

```go
if results[0] != results[1] {
	t.Errorf("the cached result differs from the one the command produced:\n first: %q\nsecond: %q",
		results[0], results[1])
}
```

And this deliberately breaks the rule stated two paragraphs up. **A trace that
describes things that did not happen is decoration, not evidence** — and here the
*model's* view is given a duration that did not happen.

The reason: a cache whose output differs from the real thing is a cache that
changes behaviour, and then a bug reproduces only on a cold cache. The trace,
which is the human's evidence, records the truth via `result_cache`. The model's
transcript records the bytes. Those are two different audiences and this is the
one place they get different answers.

### Step 6: the parent and its children share one cache, by pointer

```go
echo: a.echo,
```

```go
child := a.newChild("kid", func() string { return "sys" })
if child.echo != a.echo {
	t.Fatal("the child got a different result cache; three children reading the same file would " +
		"miss on every one of them")
}
```

By pointer, not by copy, because the working tree is one fact all of them
observe. This is also the fan-out case that is the only place the feature pays —
and the fourth time `newChild` is the file where a stage's feature does or does
not reach a subagent. Stage 08's sandbox and stage 10's deadlines both failed
here; this one has a test.

### Step 7: off is a branch, not a second implementation

```go
if rc == nil {
	return cacheLookup{verdict: cacheRefused, reason: "cache disabled"}
}
```

```go
if rc == nil || look.key == "" {
	return
}
```

A nil receiver, handled in two methods. The alternative — an interface with a
no-op implementation — means two code paths, and the one nobody runs is the one
that rots.

```go
if got := rec.count(KindResultCache); got != 0 {
	t.Errorf("result_cache events = %d with the cache off, want 0", got)
}
```

### Step 8: print the zeros

```go
if lookups := r.cacheHits + r.cacheMisses + r.cacheStale + r.cacheRefused; lookups > 0 {
	r.p("  result cache: %d hits / %d lookups (%d refused · %d stale) · %s not re-read · %s not re-run\n",
		r.cacheHits, lookups, r.cacheRefused, r.cacheStale,
		humanBytes(r.cacheBytes), time.Duration(r.cacheSaved)*time.Millisecond)
}
```

Gated on *lookups*, not on hits. So a session where the cache is on and hit
nothing prints:

```text
  result cache: 0 hits / 12 lookups (0 refused · 0 stale) · 0B not re-read · 0s not re-run
```

That line is the entire reason this part exists.

A cache that never hits produces correct results, no errors, no log line, and
green tests. It is indistinguishable from one that works, and the field report
that makes this concrete is a `git log --name-only` behind a 15-second TTL,
refetched every 30 seconds: hit rate exactly **zero**, a full 0.3-second command
every time, **for months**, with nothing wrong and nothing said.

Twelve lookups and zero hits is a sentence saying *this feature is doing nothing
for you*. Suppressing it when the numbers are boring is how a feature survives
being useless.

### Step 9: default off

```go
useCache    = flag.Bool("cache", false, "serve a repeated read-only command from a result cache instead of running it")
```

```go
if *useCache {
	a.echo = newResultCache(*cacheMax, *cacheBytes, *cacheTTL)
}
```

`false`, because part 1 measured 3.7% and 401 ms, and the measurements below say
the best constructible case is 0.3% of model time.

The TTL is off by default too. A time-based expiry is a guess about the world;
the witnesses are an observation of it, and having both means the guess can only
make things worse.

---

## Run it

```sh
go build -o agent ./12-echo/code
cd sandbox && set -a && . ../.env && set +a

../agent --yolo --cache --trace on.jsonl -p "read wire-notes.md in three parts, then check part one again"
../agent --yolo         --trace off.jsonl -p "the same prompt"
```

```sh
jq -r 'select(.kind=="result_cache") | "\(.verdict) \(.command)"' on.jsonl
grep -c '"kind":"command_end"' on.jsonl off.jsonl
```

**What to watch for:**

- `on.jsonl` has fewer `command_end` events than `off.jsonl`, and the difference
  equals the hit count. That asymmetry is the feature, visible without
  arithmetic.
- The `result cache:` line at the end of the cache-on session, even if it is all
  zeros.
- No `result_cache` events at all in `off.jsonl`.

---

## Measured

### The best case that can be constructed

Four agents — a parent and three children — asked to read the same file at once:

```text
  38 calls · 44 commands
  prompt tokens billed: 499172  (full 80484 · write 0 · read 418688)
  result cache: 12 hits / 56 lookups (2 refused · 0 stale) · 63.8kB not re-read · 1.107s not re-run
```

```text
List every JSON field name#1   sed -n '1,300p' wire-notes.md      5449B   99ms
List every HTTP status code#3  sed -n '1,300p' wire-notes.md      5449B   99ms
List every HTTP status code#3  sed -n '301,600p' wire-notes.md    5447B   83ms
List all section headings#2    sed -n '301,600p' wire-notes.md    5447B   83ms
```

| | |
|---|---|
| session wall clock | 4 m 23 s |
| model time | 360,656 ms |
| command time actually spent | 3,789 ms |
| command time not spent, thanks to the cache | **1,107 ms** |

**12 of 56 lookups — a 21% hit rate**, removing 22.6% of command time. Which is
**0.3% of model time and 0.4% of wall clock.**

### In the same session, the other cache did two hundred times the work

![Ten seconds of shell, fourteen minutes of model](images/time.svg)

**418,688 of 499,172 prompt tokens (83.9%) were served from stage 04's provider
prompt cache.**

Same run, same minute. One cache saved 1.1 seconds of shell; the other saved
four hundred thousand tokens. The second needed one HTTP header, no whitelist, no
witness set and no tokenizer.

### And this cache saves no tokens at all

A hit returns the same bytes into the same transcript. In the fan-out session
above, four agents each received their own 5,449-byte copy of the same text
**with the cache hitting**.

The obvious improvement — answer a repeat with a pointer to the earlier result —
is impossible, because the model re-ran the command precisely because the earlier
result is no longer in front of it.

### The A/B run proved nothing, which is what this section is for

A cache-off rerun of the same task: **47 commands** (vs 44), three fewer model
calls, **2,300 more output tokens**, a 500 that cost a retry, and **4 m 50 s
against 4 m 23 s**.

The noise is two orders of magnitude larger than the 1.1 s effect. That
comparison cannot support a conclusion in either direction, and the only evidence
here is the internal counter.

### The case the audit pointed at did not reproduce

Part 1 found two of its four hits came from compaction. **None of the three live
sessions run for this chapter produced a single hit from compaction.**

A session tight enough to compact five times read a file in twelve sequential
chunks and never re-read one — because the summariser recorded which ranges were
done and the model believed it. **A good compactor removes the repeats this cache
was built to absorb.**

### The tool cannot see its own success

Repeating it here because it is the sharpest limitation: auditing a trace
recorded with the cache on reports 0 hits, since a hit emits no `command_end`.
Measured at 9-in-47 for the uncached arm against 0-in-44 for the cached arm that
really served 12.

The instrument built to evaluate the cache is blind to any session in which the
cache worked.

---

## Next

[Back to stage 12](README.md).

That is the last chapter that exists. The [README](../../README.md) lists what
comes after, and every one of them has this chapter's shape: a feature everybody
ships, with a number attached that nobody publishes.
