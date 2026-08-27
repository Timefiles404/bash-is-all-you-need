# Stage 11 — Malformed

Every other field a model produces is text. Wrong text is a bad answer: you read
it, you disagree with it, and the conversation moves on. **Arguments are
different, because they cross into `exec.Command`.**

A tool call is therefore the one place in the loop where "the model said
something odd" and "the machine did something" are the same event. This chapter
is about the boundary that keeps those two apart — what each protocol actually
hands you when a call goes wrong, why the obvious repair is worse than the
failure it repairs, and what a refusal has to do afterwards so that refusing
does not itself become a way of getting stuck.

The measurements point somewhere easy to miss. A model writing nonsense is not
the dangerous failure, because a shell is very good at rejecting nonsense. The
dangerous failure is a harness being helpful about it.

**Before you start.** You need stage 03's two protocol adapters, because the two
routes mishandle a broken tool call in opposite directions and one boundary has
to cover both; stage 07's subagents, because that is where the dispatch loop
lives and the boundary sits inside it; and stage 09's triage, because the reason
a bad tool call is expensive is that a `400` is correctly fatal.

**What you will build.** One function that every tool call crosses before it can
run *and* before it can be remembered, a three-value vocabulary for why a call
was refused, a repair for duplicate call ids, an argument accumulator that
survives three different wire dialects, and a fuse for the case where refusing
correctly turns out not to be enough. One new file, plus a small change
everywhere a tool call is created, replayed, or displayed.

---

## Where we are starting from

Stage 10 read a tool call's arguments with a hand-written parser per tool. This
is the whole of the bash one:

```go
func parseBashArgs(raw string) (string, error) {
    var args struct {
        Command *string `json:"command"`
    }
    if err := json.Unmarshal([]byte(raw), &args); err != nil {
        return "", fmt.Errorf("could not parse tool arguments: %v — send valid JSON", err)
    }
    if args.Command == nil {
        return "", fmt.Errorf("tool call is missing the required \"command\" field — ...")
    }
    ...
}
```

There was a second one, `parseTaskArgs`, for the subagent tool. Both work. Both
have the same three problems, and none of the three is visible from inside the
function.

**Each parser restates in Go what the request already said in JSON.** The agent
sends the model a *schema* — a machine-readable description of what a valid call
looks like: which properties exist, which are required, what type each one is.
`bashToolDef` publishes that schema; `parseBashArgs` then re-implements it by
hand. Nothing makes the two agree, and nothing notices when they stop agreeing.

**They only guard dispatch.** A call that fails the parser is not run, which is
the obvious half. The other half is that the call is still appended to the
message array as something the assistant said, and every subsequent request in
the session re-sends it. Half of the cost of a malformed call is paid there, and
stage 10 had no guard on that path at all.

**They answer with an instruction.** "send valid JSON", "send it again". More on
why that is a bug later in the chapter; for now, notice that the sentence goes
into the transcript and stays there.

---

## What arrives when a tool call goes wrong

Three sources, and only one of them is the model.

### Truncation, which looks different on every route

The model runs out of output budget in the middle of writing an argument. It is
a simple event, and it produces four distinct client-visible shapes — different
enough that a client written against any one of them mishandles the other three.

| route | envelope says | you receive |
|---|---|---|
| OpenAI, **not** streaming (§A2) | `finish_reason: "length"`, `tool_calls: []` | the gateway's own `<tool_call><function=bash>` markup, in `message.content` |
| OpenAI, **streaming** (§E15) | `finish_reason: "length"` | genuinely partial `arguments` — an unterminated string |
| Anthropic, gateway finished the block (§A3c) | `stop_reason: "tool_use"` — **a lie** | `input` replaced by `{"raw_arguments": "<invalid JSON>"}` |
| Anthropic, cut mid-stream | `stop_reason: "max_tokens"` | partial accumulated `input_json_delta` |

§E15 is the newest of these, and it corrects §A2, which had concluded:

> **No. `tool_calls[].function.arguments` is never returned truncated, because
> on a truncated tool call `tool_calls` is not populated at all.**

That is true of `"stream": false` and false of `"stream": true`. The same
request with `max_tokens: 40` and streaming turned on produces 26 frames, of
which frames 2–21 carry argument fragments and nothing else. Concatenating them
gives exactly this:

```
{"command": "find /srv/app -type f -name \"*.go\" -not -path \"*/vendor/*\" -not -path \"*/testdata
```

An unterminated string. Invalid JSON. No markup anywhere.

The two shapes come from where the gateway's server-side parse sits relative to
the response. The model does not emit JSON on the wire at all; it emits an
XML-ish syntax, `<tool_call><function=bash><parameter=command>…`, which the
gateway parses into protocol-shaped tool calls. Not streaming, that parse runs
once on the finished text, fails on the bisected markup, and falls back to
handing you the raw markup. Streaming, it runs incrementally and has already
forwarded everything it managed to parse before the cut, so there is no fallback
left to fall back to.

**Every real agent streams.** So §A2's takeaway — that you will never have to
handle partial argument JSON on this route — is exactly backwards for the mode
an agent runs in. That is a lesson about method as much as about this endpoint:
a protocol probe run in a mode you do not ship in can produce a conclusion that
is confidently wrong for the mode you do. This shape did not turn up under
`curl`; it turned up under a running agent.

The Anthropic row is the one that matters most, because it is the only row where
**the envelope lies**. `stop_reason` says `"tool_use"`, which means "the model
wants a tool run". The only evidence that anything was cut is the shape of
`input` itself. Stage 01's rule — never execute anything from a
`length`-terminated response — cannot fire here, because the response is not
`length`-terminated.

Truncation that presents as a normal finish has a purely local counterpart,
worth naming because it is easy to build by accident. If a stall watchdog
cancels the reader *before* it rejects, and the I/O layer settles a cancelled
read as a clean end of stream, a stalled stream is reported as a finished one:
the turn is silently truncated rather than raising a retryable error, and the
symptom is a short plausible answer instead of an error. Go's cancellation does
not have this shape — cancelling a request context makes `resp.Body.Read` return
an error and never `io.EOF` — and if the guard fires just as the stream
legitimately completes, `err == nil` wins.

### A schema nobody enforces

§E13 asked the prior question: when a call arrives whole, is it guaranteed to
match the schema the request published? It is not. Three probes, each asking the
model for something the schema forbids:

```
enum ["bash","sh"], asked for "powershell"
  OpenAI     arguments: {"command": "echo hi", "shell": "powershell"}     200
  Anthropic  input:     {"command": "echo hi", "shell": "sh"}             200

additionalProperties:false, asked for an extra timeout_ms
  OpenAI     arguments: {"command": "echo hi", "timeout_ms": "5000"}      200
  Anthropic  input:     {"command": "echo hi"}   (twice — two tool_use blocks)

command declared "string", asked for the array ["echo","hi"]
  OpenAI     arguments: "{\"command\": \"[\\\"echo\\\",\\\"hi\\\"]\"}"
  Anthropic  input:     {"command": "[\"echo\",\"hi\"]"}
```

The first two are plain violations, returned with a 200 and a normal finish
reason. **The schema you send is advisory.** It shapes what the model tends to
produce and it constrains nothing, so if your client does not check it, nothing
does.

The third probe is more interesting, and it is the honest limit of this whole
chapter. Asked for an array where a string was declared, both sides **serialised
the array into the declared type**. The result passes any JSON Schema validator
you care to write — `command` is a string — and it is not a shell command; a
shell handed that string runs a program named `[echo,hi]`. Schema validation
checks the *shape* of an argument and can say nothing about whether the value
means anything.

### Fragments, and three ways of joining them

Arguments do not arrive whole. They arrive in fragments, and §B4 shows the
splits landing mid-token — `" /srv"` and `"/app"` are two separate frames of one
path. Something has to put them back together, and the obvious implementation
appends. Appending is right for exactly one of the three dialects that exist in
the wild:

```
incremental   each fragment is the next few bytes         append
cumulative    each fragment is the whole value so far     replace
re-send       the final fragment repeats the whole value  replace
```

No protocol document names any of them. This endpoint is incremental, so append
works here. A gateway that re-sends the complete arguments in its last chunk
turns append into `{"command":"ls"}{"command":"ls"}`, and the error you get back
is `invalid character '{' after top-level value` — which names a byte offset and
says nothing about the cause. That is what makes this class of bug expensive:
the message is about the symptom and points at the wrong end of the pipe.

The test that separates the dialects without knowing which one you are on is
that **a tool call's arguments are exactly one top-level JSON value** — one
complete object, not a sequence of them. So "the buffer already parses" is a
terminal state, and anything arriving after it is not a continuation.
`mergeArgs` is that rule and little else: if what has accumulated is already
valid JSON, a following fragment replaces it rather than extending it; if the
incoming fragment starts with everything already held, it is cumulative and
replaces; otherwise append.

---

## Why repairing a truncated call is a trap

Every "fix broken LLM JSON" library does the same thing — close the open string,
close the open containers — and it is about twenty lines. Here it is applied to
five real truncated payloads: three recorded verbatim in §A3c, two from a fresh
run.

**All five became valid JSON. None of the five produced the intended command.**

The intent, every time, was *find every `.go` file under `/srv/app` modified in
the last 14 days, excluding vendor and testdata, grep for `TODO(security)`,
sort, write to `/tmp/audit.txt`*. What the repair produced, and what each one
did when run for real in a throwaway tree:

| repaired command | exit | what happened |
|---|---|---|
| `find` | **0** | listed the whole tree, 12 lines |
| `find /srv/app -name "*.go" -mtime -14 -not` | 1 | `find: expected an expression after '-not'` |
| `find /srv/app -name '*.go' -not -path '*/vendor` | 2 | `bash: unexpected EOF while looking for matching '` |
| `… -exec grep -nH 'TODO(security)' {} + \|` | 2 | `bash: syntax error: unexpected end of file` |
| `… -mtime -14 -exec grep -Hn 'TODO(security)'` | 1 | `find: missing argument to '-exec'` |

Four of the five **failed loudly**, and that narrows the argument rather than
weakening it. Bash and `find` are genuinely good at rejecting a half-written
command: truncation usually lands somewhere that produces a syntax error, and a
syntax error is a visible failure the model can read and respond to.

So the danger is narrower than "repair runs garbage", and worse:

> **Repair is dangerous when the truncated prefix is itself a valid, complete,
> and *broader* command.**

Shell commands put their constraints at the end. Exclusions, filters, limits and
`-e` flags are trailing arguments, so truncation removes them first and leaves
the destructive verb intact. The cleanest instance, run for real:

```
intended:  git clean -fdx -e .env -e vendor
repaired:  git clean -fdx

before:  .env present, vendor present
after:   .env GONE,   vendor GONE           exit 0, no error
```

`find` on its own is the same shape wearing a harmless costume: exit 0,
plausible output, and an agent that reports "done". Nothing in the transcript
says the command was not the one the model wrote.

There is a stronger form of the argument with no shell in it at all. Consider a
file-editing tool instead. A truncated edit argument, once its brackets are
closed, is a *valid-looking edit that is missing a chunk* — and silently writing
that into someone's file is worse than any error, because there is no exit code
and no stderr. Bash at least has an opinion about syntax. A file writer does
not.

**So this stage does not repair truncated arguments.** The brace-counting
scanner survives, as `jsonIsOpen`, used to decide *who to blame* rather than
what to run. A payload that is still open at the end was cut off mid-generation;
one that is closed and unparseable was never JSON in the first place. The same
twenty lines, pointed at the opposite question.

---

## The boundary

One function, `checkCall(t Tool, raw string) argCheck`, which every tool call
crosses before it is dispatched **and** before it enters the message array. It
answers with one of three faults, or with the arguments to run.

```
faultCut      generation stopped mid-value — the gateway's raw_arguments
              shape, or a fragment whose JSON is still open
faultNotJSON  closed, and not JSON. Prose, markup, an apology
faultSchema   valid JSON that contradicts the schema we published
```

Three values rather than one, for the same reason stage 09 has three triage
verdicts: they lead to different actions, and collapsing them loses the action.
The pair that matters is `faultCut` versus `faultSchema`, because they disagree
about **whose mistake it was**. A call the model never finished writing is not a
call with bad arguments. Telling the model its JSON was invalid when the truth
is that it ran out of budget spends a round trip on a false diagnosis, and the
model's only available response to a false diagnosis is to rewrite the same
call.

`checkCall` takes the whole `Tool` — the same `Schema` map that went into the
request — rather than a per-tool validator. That is the fix for the first of
stage 10's three problems: the check and the advertisement are now the same
object, so they cannot drift.

### The drift that had already happened

Writing a test that compares the schema and the parser in *both* directions
found a drift immediately. `taskToolDef` declared `description` **required**;
`parseTaskArgs` defaulted it to `"subtask"` when it was missing. The model was
told a field was mandatory and then judged by a rule that did not need it.

Stage 07 had a test for exactly this, and it passed, because it only checked one
direction: *does the schema require everything the parser requires?* It never
asked the converse. The replacement asserts both by construction — a call
carrying exactly the required fields must be accepted, and dropping any one of
them must be rejected — and `description` is no longer declared required.

It looks like a cheap lie with no victim, and it costs in both directions: a
model that believes the label is mandatory invents one for calls where it means
nothing, and a model that correctly omits it would be refused by any validator
stricter than the one stage 07 happened to have.

### Where the boundary is deliberately lenient

`additionalProperties: false` — the schema keyword that says "no properties
beyond the ones declared here" — is honoured by **pruning, not refusing**. An
undeclared property is dropped, the call runs, and a notice says which keys
went.

The arithmetic decides this. §E13 measured that models really do add fields and
that nothing upstream stops them. An undeclared property is by definition one
the tool does not read, so dropping it cannot change what runs — while refusing
costs a full round trip: model writes, harness rejects, model reads the
rejection, model writes again. Paying that to remove a key that was already
going to be ignored is a poor trade. The drop is still reported, because the
alternative is a silent divergence between what the model believes it asked for
and what happened.

The same reasoning is why numbers are checked for being numbers and not for
being integers or within a range. Models emit `5.0` where `5` was declared and
`200` where the maximum is `100`, and a clamp costs nothing where a refusal
costs a turn. **Strictness at a boundary is a latency and cost decision, not
only a correctness one.** The rule this stage settles on: coerce what has
exactly one obvious reading, refuse what does not.

### A lenient parse belongs somewhere, and it is not here

The panel and the compaction summary both need a readable command out of
arguments that may be broken. That is a legitimate use for exactly the lenient
parse this stage otherwise refuses — a viewer showing a mangled command is more
useful than a viewer showing nothing. The way to have both is to make it a
separate function whose name says what it is for, and to make sure its result
can never reach execution. `argsForDisplay` takes whatever it can get and falls
back to the raw bytes.

It also never returns blank, and that is the part people leave out.
`{"raw_arguments":""}` is a real payload, and so is a `command` whose value is
nothing but whitespace; extracting from either gives an empty string, and an
empty string on a panel or in a compaction summary is a tool call that simply
vanished. Falling back to the raw bytes keeps the record that the agent tried
*something*, which is the whole reason a viewer parses leniently at all.

---

## Never let an unvalidated call into the history

This is the half that costs money, and §E14 is the measurement. Six bodies on
the OpenAI route, identical but for `arguments`, each replayed as a
three-message history:

| `arguments` | HTTP |
|---|---|
| `{"command": "echo hi"}` | 200 |
| `""` | **400** |
| `{}` | 200 |
| `{"raw_arguments": "{\"command\": \"find"}` | 200 |
| `{"command": "find /srv/app -name ` | **400** |
| `I will run: echo hi` | **400** |

**The rule is exactly "parseable JSON", and nothing beyond it.** `{}` is
accepted despite `command` being required. An object whose only key is unknown
to the schema is accepted. Only what is not JSON at all is refused — and stage
09 triages a `400` as fatal, correctly, because retrying a body the server
rejected is how a client bug becomes an outage.

Put those two together and you get the sentence that governs this half of the
stage: **one unparseable tool call in the history is a permanently dead
session.** Every subsequent request replays it and gets the same 400. Not the
next call — every call, for the rest of the session.

The Anthropic route takes five of those six — `arguments: ""` has no analogue
there, because `input` is a JSON object rather than a string — and returns 200
for all five, including `{}`, a wrong-typed property, and the gateway's own
`{"raw_arguments": …}` object. That route never compares `input` against
`input_schema` in either direction: not on the way out (§E13) and not on the way
back. The failure is
quieter and it is not better — the model is asked to go on with a conversation
in which it appears to have called a tool with arguments it never wrote, and
nothing reports the divergence.

Two traps ride along with that 400. The first is in its body, verbatim:

```json
{"error":{"param":"","type":"server_error","message":"Error from provider (Console Go): Upstream request failed: [400] Invalid request parameters"}}
```

`error.type` says `server_error` for what is unambiguously a client mistake.
That is the second instance of the §D11 pattern — an error body whose own
classification is wrong — and this time **the HTTP status is the field telling
the truth**. Stage 09 keys its decision on the status and keeps `error.type`
only as evidence; this is the case that proves the ordering was right.

The other trap is that `arguments: ""` is a 400, which matters because the empty
string is the *natural* rendering of a call that takes no arguments. §E15 shows
the very first streamed `tool_calls` delta arriving as `"arguments":""`, so a
stream that breaks between the announcement and the first fragment accumulates
exactly that, and an agent that replays what it accumulated sends it. `{}` is
the rendering that works, so `renderArgs` turns the empty string into `{}` on
the way out. The Anthropic adapter has always done this in `anthropicToolInput`;
stage 10 had the rule on one side only, which is the shape a protocol asymmetry
usually takes — the half you wrote second.

### Two calls with the same id

A gateway can mint the same tool-call id for every call it ever makes. Inside
one turn this is harmless, because nothing reads the id except the matching
result. Across turns the protocol rejects the whole request for a duplicate id,
and the rejection names a *message index* rather than a tool, so the error
points at the wrong place and reads like a bug in your message assembly.

`uniqueIDs` renames collisions, and two properties of it are load-bearing in
ways that are easy to get backwards. The set of seen ids spans the **session**,
not the turn, because the duplicates that matter live in different assistant
messages — a per-turn check finds nothing and the request is rejected anyway.
And renaming happens **before** the result blocks are built, because a call
renamed after its answer already exists is an orphaned result: the same rejected
request with a less helpful message.

---

## Refusing correctly is not enough

Here is the measurement that changed the design after the code already worked.

The boundary was finished, tested, and behaving. Run against the live Anthropic
route at `--max-tokens 110`, with the trace counted:

```
16 model calls · 0 commands · 16 faultCut
```

Every call truncated. Every one correctly classified, correctly refused,
correctly reported. **And the agent ran nothing at all for sixteen consecutive
turns**, stopped in the end only by the turn budget.

The model is not being stubborn. It was told its call had been cut off, and it
cannot see `max_tokens` — so "you were cut off" names a cause it has no way to
act on, and rewording is the only move it has left. It rewrote a command of the
same length, sixteen times.

The class therefore had to change behaviour, or classifying it was pointless. A
turn where *every* call was cut advances a streak; a turn that gets anything
through resets it; three in a row ends the loop, and the message goes to the
**human**, who can change the number:

```
error: 3 turns in a row produced only truncated tool calls. The model cannot
see the output budget, so it will keep re-sending calls of the same length;
raise --max-tokens (currently 110)

3 calls · 0 commands · 3 malformed
```

Sixteen calls down to three. It is a fuse, in the same family as `maxTurns` and
`maxDepth`: not a fix, a bound on what a known-unfixable loop is allowed to
cost. Three rather than two, because two in a row is plausibly the model
shortening its command and getting unlucky, and three is a pattern.

The general shape recurs: a refusal is addressed to somebody. Most of this
stage's refusals go to the model, which can act on them. This one names a
condition only the human can change, so it has to be readdressed.

That run is also worth reading for what it shows about the route. Two of the
three refusals were partial-JSON fragments and the third was the synthetic
`raw_arguments` object — **both Anthropic shapes, in one session**, depending on
where the cut landed relative to the gateway's own parse.

---

## No instructions in a replayed message

A tool result is not a message. It is a permanent addition to the prompt: it
goes into the message array, and every subsequent request re-sends it. An
imperative in there reads as a fresh instruction several turns later, when the
context that made it sensible has scrolled away — so the model re-issues a call
that was already handled, and the older the message gets, the more confidently
it does so.

Stage 10 shipped five:

```
"— send valid JSON"
"the call was probably cut short; send it again"
"— send an actual shell command"
"Retry with a shorter command."
"Do not retry it unchanged."
```

All five are gone. `TestReplayedTextContainsNoInstructions` scans every string
this stage can put into a tool result for imperative openers — "send ", "retry",
"do not ", "you should", "make sure", and the rest. It is a mechanical guard
rather than a review rule because the natural way to write these strings is as
advice; the failure mode is not carelessness, it is helpfulness.

**A statement of fact ages into a statement of fact.** That is the whole rule.

---

## Testing the check is not testing the call

A unit test on a validation function proves the function works. Only a test that
drives the real loop proves the loop calls it. Those are different claims, and
it is easy to have a suite that is thorough about the first and silent about the
second.

The concrete case in this stage: remove `checkCall` from `dispatch` entirely and
the boundary's own tests all still pass — they were never going through
`dispatch`. Worse, the loop's tests pass too, because a call whose arguments
were never checked arrives at the bash branch with no `command` value,
`emptyCommand` rejects it as blank, and a test that only asserts "the call was
refused" cannot tell which guard did the refusing. The check was covered. Its
call site was not.

So the tests are split in two. One file exercises the boundary directly; a
second drives `runTurn` against a scripted provider — no network — and asserts
on the resulting event trace, one assertion per wiring: ids renamed *across*
turns, markup kept out of the history, the fuse firing on the third
all-truncated turn, the request builder never emitting `arguments: ""`.

A configuration struct has the same failure mode. `newChild` — the function that
builds a subagent — copied the provider, the client, the gate and the config,
and did not copy `dl`, the struct holding stage 10's three deadlines. A zero
`deadlines` struct means every clock is off, because `guardBody` and `waitFor`
both read `<= 0` as "no watchdog". So stage 10's entire subject silently did not
apply to subagents: no stall detection, no total-call backstop, while the parent
had both. Nothing failed and nothing said so, and the one child that hangs
forever is exactly the case stage 10 exists to prevent. **A feature that is not
copied into the child is a feature that does not exist there, and the absence
has no symptom until the day it matters.**

---

## What this stage does not fix

- **A schema-valid, semantically wrong argument.** A `command` whose value is
  the string `["echo","hi"]` passes every check here — it is a string, as
  declared — and is not a shell command. Nothing short of running it finds out,
  which is why the permission gate is still the last line.
- **The provider that silently drops a long argument value.** A failure mode
  reported from production elsewhere: the value arrives at the server and does
  not arrive at the model, so what comes back is *valid JSON missing a required
  key*. The model believes it sent the field, retries identically, and the
  transport eats it again — eight consecutive failures observed. No JSON
  validation can see this; detecting it needs identical arguments *and* an
  identical result across turns.
- **Markup leaking on a complete turn.** `stripHarnessMarkup` is gated on
  `StopMaxTokens`, so a provider whose compatibility layer failed to parse a
  *finished* call leaves its markup in place. The gate is deliberate: on a
  truncated turn the text is incomplete by definition, so cutting loses nothing,
  whereas on a complete turn an agent asked to explain this very wire format
  would have its answer silently truncated at the first `<tool_call>` it quoted
  — and this repo's own documentation is full of them. On this endpoint the gate
  opens on a truncated turn and finds nothing to strip, because the streamed
  shape §E15 recorded carries no markup at all; the strip is kept for the
  non-streaming shape and for a model confused enough to write tool-call syntax
  as ordinary prose.
- **Nested object properties in a schema.** No tool here has one, and a schema
  walker that recurses is a schema walker with a cycle bug waiting for it.

---

## Exercises

1. **Watch the fuse fire.** Run `--max-tokens 110` against the Anthropic route
   with a prompt that needs a long command. Then raise it to 4096 and watch the
   same prompt succeed on the first call. The number the error message names is
   the number that fixes it, which is the whole point of addressing it to a
   human.
2. **Turn the boundary off.** Make `checkCall` return the decoded object with no
   fault, whatever it was given, and run the same session. Count what reaches
   the shell.
3. **Add the repair.** Implement the twenty-line brace-closer, run it on the
   `raw_arguments` payloads recorded verbatim in §A3c, and check each result
   against what the model was trying to do. Then try it on a truncated
   `git clean -fdx -e .env`.
4. **Break the accumulator.** Change `mergeArgs` back to unconditional append
   and feed it the re-send dialect. The error you get names a byte offset and
   nothing about the cause, which is what makes this class of bug expensive.
5. **Put an instruction back.** Add "Retry with a shorter command." to a fault
   text and run a long session. It takes a while to see, which is exactly why
   the test is mechanical.
6. **Find the fourth dialect.** Point the agent at a different gateway and log
   every `tool_args_delta`. Three are documented here; there is no reason to
   think that is all of them.

---

## What you can answer now

**Why is a tool call the one model output that must be validated before anything
else happens to it?**
Because it is the only field that crosses into `exec.Command`. Every other field
a model produces is text, and wrong text is a bad answer you read and move on
from. Wrong arguments are a command that runs.

**What does a truncated tool call look like on the wire?**
Four shapes across two routes. OpenAI non-streaming: raw `<tool_call>` markup in
`message.content`, with `tool_calls: []`. OpenAI streaming: genuinely partial
argument JSON. Anthropic: either a synthetic `{"raw_arguments": …}` object or
partial `input_json_delta`, depending on where the cut landed relative to the
gateway's own parse.

**Why does the OpenAI truncation shape depend on whether you streamed?**
Because the gateway parses the model's markup server-side. Not streaming, that
parse runs once on finished text, fails, and falls back to dumping the raw
markup. Streaming, it runs incrementally and has already forwarded what it
parsed, so there is no fallback left. §A2 concluded you would never see partial
argument JSON; §E15 corrects it for the mode every real agent runs in.

**Does either endpoint check a returned tool call against the schema you sent?**
No. §E13 got back an `enum` value the schema forbade and a property banned by
`additionalProperties: false`, both with a 200 and a normal finish reason. The
schema is advisory: it shapes what the model tends to produce and constrains
nothing, so if your client does not validate, nothing does.

**If four of five repaired truncations fail loudly, why not repair them?**
Because the fifth is the one that matters. Shell commands put their constraints
last, so truncation strips the exclusions and leaves the verb: an intended
`git clean -fdx -e .env -e vendor` repairs to `git clean -fdx`, which exits 0
and deletes the two things the command existed to protect.

**Why three fault classes rather than one "invalid"?**
Because they lead to different actions. `faultCut` and `faultSchema` disagree
about whose mistake it was, and telling the model its JSON was invalid when it
actually ran out of budget spends a round trip on a diagnosis it cannot act on.

**What happens if an unparseable tool call reaches the message array?**
On the OpenAI route, §E14 measured a 400 on every subsequent request — and a 400
is correctly fatal, so one unvalidated call is a permanently dead session. On
the Anthropic route everything is accepted, including the gateway's own
truncation shape, so the session degrades quietly instead.

**Why must a zero-argument call be rendered `{}` rather than `""`?**
Because §E14 measured `arguments: ""` as a 400 while `{}` is accepted, and the
empty string is not hypothetical: §E15 shows the first streamed `tool_calls`
delta arriving as `"arguments":""`, so a stream that breaks before the first
fragment accumulates exactly that.

**Why is refusing a truncated call correctly not enough?**
Because the model cannot see `max_tokens`. Told only that it was cut off, it
rewrites a command of the same length — measured at 16 model calls and 0
commands in one session. The fix is a fuse: three consecutive all-truncated
turns end the loop and address the message to the human, who can change the
number.

**Why can a tool result not contain an instruction?**
Because it is not a message, it is a permanent addition to the prompt, re-sent
on every subsequent request. An imperative reads as a fresh instruction several
turns later once its context has scrolled away, so the model re-issues a call
that was already handled. Five such strings from stage 10 were deleted, and a
mechanical test keeps them out.

---

## Questions to think about

These do not have answers in the repo. They are the ones where the answer
depends on what you are building.

1. Undeclared properties are pruned rather than refused, because a round trip
   costs more than a key the tool was never going to read. Describe a tool where
   that arithmetic inverts — where the model's having sent an unknown key means
   it believed something false about the tool, and running the call anyway is
   the expensive option.
2. The three accumulator dialects were found by meeting them. How would you
   write an accumulator that is correct against a fourth dialect you have not
   seen, and what property of the wire would you have to give up assuming to do
   it?
3. This stage's refusals are addressed to two audiences: most to the model, and
   the truncation fuse to the human. Take an agent you have used and find a
   refusal addressed to the wrong one. What is the test for which audience a
   given refusal belongs to?
4. The boundary validates against the tool list the agent is currently
   advertising, which is a function of recursion depth. What breaks when the
   tool list can change mid-session — dynamic registration, a tool server that
   reconnects with a different schema — and what would you have to record
   alongside each call so that a replay could still be validated?
5. Nothing here can distinguish a schema-valid nonsense argument from a good
   one. Where in a system you are building could that judgment live, what could
   it possibly be keyed on, and what would you be willing to pay per call for
   it?

→ Next: [Stage 12 — Echo](12-echo.md)

→ Reference: [Wire notes](wire-notes.md), [Stage 01 — Don't Die](01-dont-die.md), [Stage 09 — Triage](09-triage.md), [Stage 10 — Deadlock](10-deadlock.md)
