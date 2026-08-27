# Stage 07 — Multiply

Two features, and neither one is a subsystem.

```
subagents   a fresh []Msg, a different system prompt, the same everything
            else — and only TEXT comes back.
skills      a directory of Markdown files, and one paragraph saying they exist.
```

There is no scheduler, no message queue, no agent registry and no protocol
between agents. A subagent is a function call whose return value is a paragraph.

And the headline, because it is the opposite of what everyone assumes:

> **A subagent does not save tokens. It saves context.**

Measured below: the delegating run cost **20% more** and finished with a parent
context **9.6× smaller**. Those two numbers move in opposite directions, and
knowing which one you are short of is the whole of deciding when to delegate.

---

## A subagent is the same loop, called again

```go
func (a *agent) spawn(callID, description, prompt string) (string, Usage, error) {
    child := a.newChild(agentID, func() string { return subagentSystem + para + a.stable })
    msgs := child.runTurn([]Msg{TextMsg(RoleUser, prompt)})
    return lastAssistantText(msgs), child.spent, nil
}
```

That is the feature. The parent already had a loop, a bus, a compactor and a
gate; the child is the same loop with a different message array. What is shared
and what is not turns out to be the only interesting design question:

| shared | not shared |
|---|---|
| provider, HTTP client | the message array |
| the permission gate | the system prompt |
| the shell config and working directory | the compactor |
| the **bus core** — one ordered trace for the whole tree | the turn budget |

The clause that makes it worth doing is `lastAssistantText`. Everything else the
child did — every tool call, every 40kB of command output, every wrong turn it
backed out of — lives in a message array that is discarded. The parent's context
grows by the length of the report and by nothing else.

The child is told this, in its system prompt, rather than left to infer it:

> Everything you do here is discarded when you finish EXCEPT your final message.
> Your caller will never see your commands, your reasoning, or your tool output
> — only the last thing you say.

Without that paragraph a subagent writes a summary of its *process* — "I looked
at several files and found some things" — because that is what a chat turn
normally is. Told plainly that its final message is the only thing that
survives, it writes a report.

---

## From a real run: what delegation actually costs

Three Markdown files totalling 63kB. One task: read each one in full and write a
three-sentence summary. Two arms, same files, same model:

```sh
agent --yolo --max-output 60000              # delegate: one subagent per file
agent --yolo --max-output 60000 --max-depth 0  # inline: the task tool does not exist
```

```
  ╭─ subagent · Summarize wire-notes.md
  ╭─ subagent · Summarize 02-see-everything.md
  ╭─ subagent · Summarize 03-babel.md
  │ $ cat .../wire-notes.md
  │ $ cat .../02-see-everything.md
  │ $ cat .../03-babel.md
  ╰─ 2 turns ·  5133 prompt +  204 output tokens ·  6327ms →  707B returned
  ╰─ 2 turns · 11952 prompt +  237 output tokens · 12441ms →  827B returned
  ╰─ 2 turns ·  4696 prompt +  380 output tokens · 13519ms →  950B returned
```

| | delegate | inline |
|---|---:|---:|
| model calls | 9 (3 parent, 6 child) | 3 |
| prompt tokens | 25,782 | 19,715 |
| at 0.1x cache read | **22,038** | **18,390** |
| output tokens | 1,635 | 571 |
| wall clock | 39s | 18s |
| **parent context at the end** | **1,893** | **18,160** |

Delegation cost 20% more in full-price-equivalent tokens, 2.9× the output
tokens, and 2.2× the wall clock. It also ended with a parent whose entire
context was 1,893 tokens — because the 63kB of Markdown was read three times, by
three agents, none of whom were the one still holding the conversation.

Look at the three `╰─` lines again. **21,781 prompt tokens went in and 2,484
bytes came back.** That ratio is the product. Everything else is overhead you
are paying for it.

So the decision rule is not "is this task big enough to delegate". It is:

> Delegate work that **reads a lot and concludes a little**, when the reading is
> not something you will need again. If you need the intermediate output, a
> subagent is strictly worse than doing it inline — you pay for the tokens twice
> and then throw the result away.

That is also why the `task` tool's description is written about economics rather
than mechanism. A description that says what a tool *does* tells the model
nothing about when to reach for it.

---

## Concurrency, and a bill that came due three chapters early

Three subagents ran at once. Notice the trace is still a single, totally ordered
stream — every event has a `Seq` that orders it against every other event in the
tree, across every agent.

That is not new work. Stage 02 chose a synchronous bus under one lock, for
ordering, when there was exactly one producer and no obvious reason to care:

```go
type busCore struct {
    mu   sync.Mutex
    seq  int
    subs []Subscriber
}

func (b *Bus) Fork(agent string) *Bus {
    return &Bus{core: b.core, depth: b.depth + 1, agent: agent}
}
```

`Fork` copies nothing. One counter, one lock, N producers. An async
per-subscriber bus — the design that "scales better" — would have given each
subscriber a different story about a concurrent session, which is precisely the
session you cannot reason about without one story.

A trace per agent is the other common choice, and it makes the single question
you actually have — *what was the parent doing while the child ran?* —
answerable only by merging files on timestamps, which is exactly what timestamps
are bad at.

### The renderer degrades instead of lying

Three children streaming tokens into one terminal cursor produces a paragraph
assembled from three different sentences. It is not merely ugly; it reads as one
agent contradicting itself.

So the plain renderer shows a subagent's *structure* — what it ran, what it
cost, what it returned — and drops the prose. Nothing is lost, because every
delta is in the trace, and stage 06's composer exists precisely because a linear
terminal is the wrong shape for a tree.

> **A renderer that cannot show something should say so by showing less, never
> by showing it wrong.**

### Concurrent execution, deterministic history

`dispatch` runs subagents concurrently and returns their results **in the order
the model asked for them**. If results were appended as they completed, the same
session replayed twice would produce two different message arrays, two different
prompt prefixes, and — per stage 04 — a cache that never hits.

> Concurrency is allowed to change how long things take. It is not allowed to
> change what the conversation says.

The permission gate gets a mutex for the same reason, and it is a sharper one
than it looks. Two goroutines writing a prompt to one terminal produce a single
interleaved line and then read one answer for both questions: **the user
approves one command and a different one runs.** That is a security bug wearing
a UI bug's clothes. `dispatch` asks every question on one goroutine before any
concurrency starts, so the lock should never contend — and it is there because
"should never" is not a property you want a permission gate to rest on.

---

## The depth fuse, and why the tool disappears

At the depth limit the `task` tool is **removed from the tool list**, not
refused at call time.

A runtime refusal costs a full round trip — the model writes a call, the harness
rejects it, the model reads the rejection and tries something else — and it
costs the tokens of a tool definition on every request that can never use it.
Worse, it is a rule the model can see is arbitrary, and models argue with
arbitrary rules by rephrasing.

A tool that is not in the list is not a rule. There is nothing to argue with and
nothing to work around, and the model plans within the tools it has.

---

## Skills are a directory and a paragraph

```
skills/
  mutation-test/SKILL.md    ---  name: … / description: … ---  then the body
  new-stage/SKILL.md
  wire-probe/SKILL.md
```

The system prompt gets the names and one-line descriptions. The bodies stay on
disk. The model reads one with `cat` when it decides it applies. There is no
skill tool, no retrieval step and no runtime — which is stage 05's observation
about memory, arriving from a different direction: once the agent has a shell,
"load this document when relevant" is not a feature you build, it is a filename.

What is load-bearing is the shape. **Progressive disclosure**: index always,
body on demand.

```
  ≡ skills: 3 skills · index 738B in every request · 6.1kB of bodies left on disk
```

That number is printed on purpose, because the index is **not free** and the
arithmetic is the whole design decision. 738 bytes sit in the prefix of every
request for the life of the session — cached at a tenth of the price after stage
04, but never zero. Forty skills is a couple of thousand tokens of permanent
overhead. A skills directory that grows without anyone pruning it is a tax
levied on every call the agent ever makes, and the only way anybody notices is
if something prints the number.

Three instructions in the index, each because of a way this goes wrong:

- **"read the body before acting"** — otherwise the model acts on the
  description, which is one line long and was written to be *selectable*, not to
  be sufficient.
- **"at most one"** — a model given five plausible skills reads all five, which
  converts a token saving into a token cost plus five round trips.
- **"if none applies, ignore this list"** — without it, a skills list reads as a
  menu the model is expected to order from, and it will find one that nearly
  fits.

The frontmatter parser is twenty lines rather than a YAML dependency. When you
own both ends of an interface, the parser is allowed to be as small as the
interface.

---

## What PTC really is

Programmatic Tool Calling is sold on one benefit: the model writes code that
calls tools, the code runs somewhere else, and **intermediate results never
enter the context**.

A shell pipeline already does that. Same task, three ways — *find the five
largest Go files under `src/`* — over 21 files:

| | model calls | commands | prompt tokens | at 0.1x read | tool output into context | wall |
|---|---:|---:|---:|---:|---:|---:|
| one file per call | 25 | 23 | 51,937 | 8,161 | 1,255 B | 80s |
| `wc -l src/*.go` | 2 | 1 | 2,097 | 1,175 | 509 B | 12s |
| `find … \| sort -rn \| head -6` | 2 | 1 | **1,608** | **686** | **157 B** | **8s** |

**Same answer. 32× the prompt tokens, 10× the wall clock.**

Read the "tool output into context" column, because that is the mechanism rather
than the symptom. The pipeline's `sort | head` did its filtering *inside the
shell*, so 157 bytes crossed into the context instead of 1,255. The intermediate
data existed; it just never became tokens.

Two things worth taking from the table.

**The middle row is an accident that makes the point.** That arm was supposed to
be the slow one — the instruction said no pipes, no chaining, no `xargs`, no
`find`. The model answered with `wc -l src/*.go`, complying exactly and doing
the entire job in one call, because **a glob is already a batch operation** and
no operator I forbade was involved. Getting fan-out for free, without asking for
it, is the property this repo has been claiming for the shell since its opening
section, and it took a failed experiment to make it visible.

**So what does PTC add over a pipeline?** Not the headline benefit — that one
is a `|`. What it adds is a real programming language where the shell has only
composition: typed tool APIs instead of text streams, conditionals and loops
over structured results, error handling that is not `$?`, and tools that are not
programs on a PATH. If your tools are CLI programs and your control flow is
"filter, sort, take", a pipeline is PTC and you have had it since 1973. If your
tools are HTTP APIs with schemas and your control flow has branches, it is not,
and the difference is worth a runtime.

---

## The version with no `task` tool at all

An agent that can run bash can run *the agent*:

```sh
agent --subagent "survey every provider adapter and report the disagreements"
```

One prompt in, one report out, no REPL. This is a complete subagent mechanism
using nothing but the tool the agent already has, and it is worth seeing how
little there is to it — recursion needs no orchestration layer, it needs a
process.

What it costs is everything on the instrument panel. A separate process has its
own bus, so its events are not in your trace; its tokens are not in your ledger;
its permission prompts fight yours for the terminal; and its failure is an exit
code rather than a stop reason. You would rebuild all of it, over a pipe, in a
format you would then have to version.

Which is the honest summary of the whole in-process/out-of-process choice:
**the shell is a perfectly good orchestrator right up until you want to know
what it cost.**

---

## Exercises

1. **Reproduce the two arms** and compare the last `context` number in each. The
   ratio is what you are buying.
2. **Delegate something whose intermediate output you need** — "find the bug and
   show me the diff" — and watch the report be useless. The failure mode of a
   subagent is not that it is wrong, it is that it is *lossy in a direction you
   did not choose*.
3. **Remove the "everything is discarded" paragraph** from `subagentSystem` and
   read three reports. Count how many describe process instead of findings.
4. **Make `dispatch` append results as they complete** instead of by index. Run
   the same session twice and diff the two traces' request bodies.
5. **Set `--max-depth 3`** and give the agent a task big enough to recurse. Then
   work out what your worst case bill is, and whether any fuse in the code would
   have stopped it.
6. **Add a fourth skill and measure the index again.** Extrapolate to forty.
   Decide what your pruning policy is *before* you need one.
7. **Run the PTC table yourself** on a directory big enough to matter, and find
   the point where the one-command version stops fitting in `--max-output`. That
   number is where a shell stops being enough and a real sandbox starts being
   the answer — which is stage 08.

---

## What you can answer now

**Why does a subagent cost more than doing the same work inline?**
Because a child is a whole agent: its own system prompt, its own turns, and a
report the parent then pays to read. Over the same three files the delegating
arm made nine model calls to the inline arm's three, and spent 25,782 prompt
tokens against 19,715 — about 20% more once cache reads are priced in, with 2.9×
the output tokens and 2.2× the wall clock.

**If delegation costs more, what is it buying?**
Context. The delegating run ended with a parent holding 1,893 tokens where the
inline run held 18,160 — 9.6× smaller — because everything the children read
lives in message arrays that were discarded. 21,781 prompt tokens went in across
the three children and 2,484 bytes came back, and that ratio is the product. The
rule that follows is to delegate work that reads a lot and concludes a little,
and never work whose intermediate output you will need.

**Why is a subagent told, in its system prompt, that everything but its final
message is discarded?**
Because otherwise it writes a summary of its process — "I looked at several
files and found some things" — since that is what a chat turn normally is. A
child told plainly that its last message is the only thing that survives writes
a report instead. One paragraph changes what comes back.

**Why is there one trace for the whole tree rather than one per agent?**
Because the question you actually have is what the parent was doing while the
child ran, and a file per agent turns that into a merge on timestamps, which is
what timestamps are worst at. Stage 02's synchronous bus under one lock already
gives every event a `Seq` that orders it against every other event in the tree,
and `Fork` copies nothing. An async per-subscriber bus would hand each
subscriber a different story about exactly the session you cannot reason about
without one.

**Why does `dispatch` return subagent results in the order they were asked for
rather than as they finish?**
Because completion order is not reproducible and the message array is the
prompt. The same session replayed twice would produce two different message
arrays, two different prompt prefixes and, per stage 04, a cache that never
hits. Concurrency is allowed to change how long things take; it is not allowed
to change what the conversation says.

**Why does the permission gate hold a lock it should never need?**
Because what it prevents is not a garbled prompt: two goroutines writing to one
terminal produce a single interleaved line and then read one answer for both
questions, so the user approves one command and a different one runs. `dispatch`
asks every question on one goroutine before any concurrency starts, so the lock
should never contend. "Should never" is not a property you want a permission
gate to rest on.

**Why is the depth limit enforced by removing the `task` tool rather than
refusing the call?**
Because a runtime refusal costs a full round trip, carries the tokens of a tool
definition on every request that can never use it, and is visibly arbitrary —
and models argue with arbitrary rules by rephrasing them. A tool that is not in
the list is not a rule: there is nothing to work around, and the model plans
within what it has.

**Why does a skills index cost something even though the bodies stay on disk?**
Because the index sits in the prefix of every request for the life of the
session — 738 bytes for three skills, against 6.1kB of bodies left on disk.
Stage 04's cache makes that a tenth of the price and never zero, so forty skills
is a couple of thousand tokens of permanent overhead on every call the agent
ever makes. Nobody notices unless something prints the number.

**What does Programmatic Tool Calling add that a shell pipeline does not?**
Not its headline benefit. Keeping intermediate results out of the context is
what `sort | head` has done since 1973: the pipeline arm put 157 bytes into the
context where the file-at-a-time arm put 1,255, using 32× fewer prompt tokens
and a tenth of the wall clock for the same answer. What PTC adds is a language
where the shell has only composition — typed tool APIs instead of text streams,
branches and loops over structured results, error handling that is not `$?`, and
tools that are not programs on a PATH.

**Why not implement subagents by having the agent run the agent?**
You can — `agent --subagent "…"` is a complete subagent mechanism built from the
tool the agent already has. What it costs is the instrument panel: a separate
process has its own bus, so its events are not in your trace, its tokens are not
in your ledger, its permission prompts fight yours for the terminal, and its
failure is an exit code rather than a stop reason. The shell is a perfectly good
orchestrator right up until you want to know what it cost.

---

## Questions to think about

These do not have answers in the repo. They are the ones where the answer
depends on what you are building.

1. A child gets a fresh message array but the same working directory, so
   anything it writes to disk outlives it. Is a file the child leaves behind a
   legitimate return channel or a hole in "only text comes back"? Decide what
   you would allow, and how the parent would come to know it happened.

2. The child sees its prompt and nothing else — not the user's original request,
   not the summary a compaction produced. Which of those, if any, should it
   inherit? Work out what a child does badly when it does not know the goal, and
   what it does badly when it does.

3. The depth fuse works by making a constraint invisible. Some limits are better
   stated out loud: a model that knows it may not delegate can plan around that,
   while a model that simply lacks a tool cannot tell whether it was never
   offered one or has run out. Which of your own limits belong in the tool list,
   and which belong in the prompt?

4. The chapter's claim is that a pipeline is PTC when your tools are programs on
   a PATH and your control flow is filter, sort, take. Name the first tool in
   your own system that is not a program on a PATH. What would it take to make
   it one, and what would you lose by doing that?

5. One lock orders the whole tree, and three concurrent children never contend
   for it. Decide what you would give up first if that number were three
   hundred: the total ordering, the synchronous emit, or one process.

→ Next: [Stage 08 — Sandbox](08-sandbox.md)

→ Reference: [Stage 04 — The Cache](04-the-cache.md), [Stage 05 — Live Forever](05-live-forever.md), [Stage 06 — The Composer](06-the-composer.md)
