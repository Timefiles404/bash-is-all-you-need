# Stage 11 — Malformed

Every other field a model produces is text. Wrong text is a bad answer, and you
read it and move on. **Arguments are different: they cross into `exec.Command`.**

So a tool call is the one place where "the model said something odd" and "the
machine did something" are the same event, and this chapter is about the
boundary that separates them — what each protocol actually hands you when a call
goes wrong, and which of the obvious repairs is a trap.

The measurements say something I did not expect going in: the dangerous failure
is not the model writing nonsense. It is the *harness* being helpful.

---

## What arrives when a tool call goes wrong

Three sources, and only one of them is the model.

### 1. Truncation, which looks different on every route

The model runs out of output budget mid-argument. Simple event; four distinct
client-visible shapes, and the wire notes had to be corrected to hold them all.

| route | envelope says | you receive |
|---|---|---|
| OpenAI, **not** streaming (§A2) | `finish_reason: "length"`, `tool_calls: []` | the gateway's own `<tool_call><function=bash>` markup, in `message.content` |
| OpenAI, **streaming** (§E15) | `finish_reason: "length"` | genuinely partial `arguments` — an unterminated string |
| Anthropic, gateway finished the block (§A3c) | `stop_reason: "tool_use"` — **a lie** | `input` replaced by `{"raw_arguments": "<invalid JSON>"}` |
| Anthropic, cut mid-stream | `stop_reason: "max_tokens"` | partial accumulated `input_json_delta` |

§E15 is new for this stage and it corrects §A2, which had concluded:

> **No. `tool_calls[].function.arguments` is never returned truncated, because on
> a truncated tool call `tool_calls` is not populated at all.**

That is true of `"stream": false` and false of `"stream": true`. Same request,
`max_tokens: 40`, streaming: 26 frames, of which frames 2–21 carry argument
fragments and nothing else, and concatenating them gives

```
{"command": "find /srv/app -type f -name \"*.go\" -not -path \"*/vendor/*\" -not -path \"*/testdata
```

— unterminated string, invalid JSON, no markup anywhere. The two shapes come from
where the gateway's server-side parse of the model's harness syntax sits relative
to the response: not streaming, the parse runs on finished text, fails, and falls
back to dumping the raw markup; streaming, it runs incrementally and has already
forwarded everything it parsed before the cut. There is no fallback to fall back
to.

**Every real agent streams.** So §A2's takeaway — that you will never have to
handle partial argument JSON on this route — is exactly backwards for the mode an
agent runs in. I would not have found this with `curl`; it turned up because the
agent hit it.

The Anthropic row is the one that matters most, because it is the only row where
**the envelope lies**. `stop_reason` says `"tool_use"`, which means "the model
wants a tool run", and the only evidence that anything was cut is the shape of
`input` itself. Chapter 01's rule — *never execute anything from a
`length`-terminated response* — does not fire here. It cannot. The response is
not `length`-terminated.

### 2. A schema nobody enforces

§E13 asked whether either route validates a returned call against the schema it
was given. It does not. Three probes, each asking the model for something the
schema forbids:

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
produce and constrains nothing, so if your client does not check it, nothing does.

The third is more interesting and it is the honest limit of this whole chapter.
Asked for an array where a string was declared, both sides **serialised the array
into the declared type**. The result passes any JSON Schema validator you care to
write — `command` is a string — and it is not a shell command. Schema validation
checks the *shape* of an argument and can say nothing about whether the value
means anything.

### 3. The accumulator

Arguments arrive in fragments, and §B4 shows the splits landing mid-token. The
obvious implementation appends them, and appending is right for exactly one of the
three dialects that exist in the wild:

```
incremental   each fragment is the next few bytes         append
cumulative    each fragment is the whole value so far     replace
re-send       the final fragment repeats the whole value  replace
```

No protocol document names any of them. This endpoint is incremental, so append
works here; a gateway that re-sends complete arguments in its last chunk turns
append into `{"command":"ls"}{"command":"ls"}`, whose error —
`invalid character '{' after top-level value` — names a byte offset and nothing
about the cause. The test that separates them is that **a tool call's arguments
are exactly one top-level JSON value**, so "the buffer already parses" is a
terminal state and anything after it is not a continuation.

---

## The trap: repairing a truncated call

Every "fix broken LLM JSON" library does the same thing — close the open string,
close the open containers — and it is twenty lines. Here it is applied to five
real truncated payloads: three from §A3c's captures, two from a fresh run.

**All five became valid JSON. None of the five produced the intended command.**

The intent, every time, was *find every `.go` file under `/srv/app` modified in
the last 14 days, excluding vendor and testdata, grep for `TODO(security)`, sort,
write to `/tmp/audit.txt`*. What the repair produced, and what each did when run
for real in a throwaway tree:

| repaired command | exit | what happened |
|---|---|---|
| `find` | **0** | listed the whole tree, 12 lines |
| `find /srv/app -name "*.go" -mtime -14 -not` | 1 | `find: expected an expression after '-not'` |
| `find /srv/app -name '*.go' -not -path '*/vendor` | 2 | `bash: unexpected EOF while looking for matching '` |
| `… -exec grep -nH 'TODO(security)' {} + \|` | 2 | `bash: syntax error: unexpected end of file` |
| `… -mtime -14 -exec grep -Hn 'TODO(security)'` | 1 | `find: missing argument to '-exec'` |

Four of the five **failed loudly**, which undercuts the version of this argument
I expected to be making. Bash and `find` are good at rejecting a half-written
command; the truncation usually lands somewhere that produces a syntax error, and
a syntax error is a visible failure the model can read.

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

`find` alone is the same shape wearing a harmless costume: exit 0, plausible
output, and an agent that reports "done". Nothing in the transcript says the
command was not the one the model wrote.

The independent version of this argument is sharper than mine. A shipped agent I
read while planning this stage refuses truncation repair for a reason that has no
shell in it at all: a truncated file-edit argument, once bracket-closed, is a
*valid-looking edit that is missing a chunk*, and silently writing that into the
user's file is worse than any error. Bash at least has an opinion about
syntax. A file writer does not.

**So this stage does not repair truncated arguments.** The brace-counting scanner
is still here — as `jsonIsOpen`, used to decide *who to blame* rather than what to
run. A payload that is still open at the end was cut off; one that is closed and
unparseable was never JSON. Same twenty lines, opposite purpose.

---

## The boundary

One function, `checkCall(t Tool, raw string) argCheck`, that every tool call
crosses before it is dispatched **and** before it enters the message array.

```go
faultCut      generation stopped mid-value — the gateway's raw_arguments
              shape, or a fragment whose JSON is still open
faultNotJSON  closed, and not JSON. Prose, markup, an apology
faultSchema   valid JSON that contradicts the schema we published
```

Three values rather than one, for the reason stage 09 has three triage verdicts:
they lead to different actions. The pair that matters is `faultCut` versus
`faultSchema`, because they disagree about **whose mistake it was**. A call the
model never finished writing is not a call with bad arguments, and telling the
model its JSON was invalid when the truth is that it ran out of budget spends a
round trip on a false diagnosis.

It takes the whole `Tool` — the same `Schema` map that went into the request — so
the check and the advertisement cannot drift. Stage 10 had a `parseBashArgs` and
a `parseTaskArgs`, each re-stating in Go what `bashToolDef` and `taskToolDef` had
already said in JSON, and nothing made them agree.

### The drift that had already happened

Writing the bidirectional test found one immediately. `taskToolDef` declared
`description` **required**; `parseTaskArgs` defaulted it to `"subtask"` when it
was missing. The model was told a field was mandatory and then judged by a rule
that did not need it.

Stage 07 had a test for exactly this, and it passed, because it only checked one
direction: *does the schema require everything the parser requires?* It never
asked the converse. The replacement asserts both, by construction — a call with
exactly the required fields must be accepted, and dropping any one of them must be
rejected — and `description` is no longer declared required.

### Where the boundary is deliberately lenient

`additionalProperties: false` is honoured by **pruning, not refusing**.

The arithmetic decides this. §E13 measured that models really do add fields, and
that nothing upstream stops them. An undeclared property is by definition one the
tool does not read, so dropping it cannot change what runs — while refusing costs
a full round trip: model writes, harness rejects, model reads the rejection, model
writes again. Paying that to remove a key that was already going to be ignored is
a poor trade. The drop is reported as a notice, so it is visible without being
permanent.

The same reasoning is why numbers are checked for being numbers and not for being
integers or in range. Models emit `5.0` where `5` was declared and `200` where the
maximum is `100`, and a clamp costs nothing where a refusal costs a turn.
**Strictness at the boundary is a latency and cost decision, not only a
correctness one.** The rule this stage settles on: coerce what has exactly one
obvious reading, refuse what does not.

---

## Never let an unvalidated call into the history

This is the half that costs money, and §E14 is the measurement. Six bodies on the
OpenAI route, identical but for `arguments`, each replayed as a three-message
history:

| `arguments` | HTTP |
|---|---|
| `{"command": "echo hi"}` | 200 |
| `""` | **400** |
| `{}` | 200 |
| `{"raw_arguments": "{\"command\": \"find"}` | 200 |
| `{"command": "find /srv/app -name ` | **400** |
| `I will run: echo hi` | **400** |

**The rule is exactly "parseable JSON", and nothing beyond it.** `{}` is accepted
despite `command` being required. An object whose only key is unknown to the
schema is accepted. Only what is not JSON at all is refused — and stage 09 triages
a 400 as fatal, correctly, because retrying an argument the server rejected is how
a client bug becomes an outage.

Put those together: **one unparseable tool call in the history is a permanently
dead session.** Every subsequent request replays it and gets the same 400.

The same probe on the Anthropic route returns 200 for all five bodies, including
`{}`, a wrong-typed property, and the gateway's own `{"raw_arguments": …}`. It
never compares `input` to `input_schema` in either direction. That failure is
quieter and not better: the model is asked to continue a conversation in which it
appears to have called a tool with arguments it never wrote.

Two traps ride along in the 400's body:

```json
{"error":{"param":"","type":"server_error","message":"Error from provider (Console Go): Upstream request failed: [400] Invalid request parameters"}}
```

`error.type` says `server_error` for what is unambiguously a client mistake — the
second instance of the §D11 pattern, and this time **the HTTP status is the field
telling the truth**. Stage 09 keys its decision on status and keeps `error.type`
only as evidence; this is the case that proves the ordering was right.

And `arguments: ""` being a 400 matters because the empty string is the *natural*
rendering of a zero-argument call. §E15 shows the first streamed `tool_calls`
delta arriving as `"arguments":""`, so a stream that breaks between the
announcement and the first fragment accumulates exactly that. `{}` is the
rendering that works, and `renderArgs` is now the symmetric half of
`anthropicToolInput`, which stage 10 had on one side only.

### Identity

A gateway can mint the same tool-call id for every call it makes. Inside one turn
nothing reads the id but the matching result, so it works; across turns the
protocol rejects the request for a duplicate id, and the rejection names a
*message index* rather than the tool, so the error points at the wrong place.

`uniqueIDs` renames collisions, and two properties of it were bugs on the way
here. The seen-set spans the **session**, not the turn, because the duplicates
that matter live in different assistant messages — a per-turn check finds nothing
and the request is rejected anyway. And renaming happens **before** the result
blocks are built, because a call renamed after its answer exists is an orphaned
result: the same rejected request with a less helpful message.

---

## Refusing correctly is not enough

Here is the measurement that changed the design after the code already worked.

The boundary was finished, tested, and behaving. Run against the live Anthropic
route at `--max-tokens 110`, with the trace counted:

```
16 model calls · 0 commands · 16 faultCut
```

Every call truncated. Every one correctly classified, correctly refused, correctly
reported. **And the agent ran nothing at all for sixteen consecutive turns**,
stopped in the end only by the turn budget.

The model is not being stubborn. It was told its call was cut off, and it cannot
see `max_tokens` — so "you were cut off" names a cause it has no way to act on,
and rewording is the only move it has. It rewrote a command of the same length,
sixteen times.

So the class had to change behaviour, or classifying it was pointless. A turn
where *every* call was cut advances a streak; a turn that gets anything through
resets it; three in a row ends the loop and the message goes to the **human**, who
can change the number:

```
error: 3 turns in a row produced only truncated tool calls. The model cannot
see the output budget, so it will keep re-sending calls of the same length;
raise --max-tokens (currently 110)

3 calls · 0 commands · 3 malformed
```

Sixteen calls to three. It is a fuse, in the family of `maxTurns` and `maxDepth`:
not a fix, a bound on what a known-unfixable loop is allowed to cost.

That run is also worth reading for what it shows about the route. Two of the three
refusals were partial-JSON fragments and the third was the synthetic
`raw_arguments` object — **both Anthropic shapes, in one session**, depending on
where the cut landed relative to the gateway's own parse.

---

## No instructions in a replayed message

A rule that arrived from reading someone else's code, and it made this stage
delete four of its own strings.

A tool result is not a message. It is a permanent addition to the prompt: it goes
into the message array, and every subsequent request re-sends it. An imperative in
there reads as a fresh instruction several turns later, when the context that made
it sensible has scrolled away — so the model re-issues a call that was already
handled, and the older the message gets, the more confidently it does so.

Stage 10 shipped four:

```
"— send valid JSON"
"the call was probably cut short; send it again"
"Retry with a shorter command."
"Do not retry it unchanged."
```

All four are gone, and `TestReplayedTextContainsNoInstructions` scans every string
this stage can put in a tool result for imperative openers. It is a mechanical
guard because the natural way to write these strings is as advice.

**A statement of fact ages into a statement of fact.** That is the whole rule.

---

## What is in the diff

```
toolcall.go        the boundary: the taxonomy, checkCall, the schema subset,
                   uniqueIDs, mergeArgs, renderArgs, stripHarnessMarkup
toolcall_test.go   31 tests, none of which need a network
exec.go            parseBashArgs deleted; emptyCommand is the residue
subagent.go        parseTaskArgs deleted; `description` no longer declared
                   required; dispatch validates before the switch
main.go            uniqueIDs and the markup strip before the message is built;
                   the cut fuse; --max-tokens
openai.go          mergeArgs in the accumulator; renderArgs on the way out
events.go          KindToolCallInvalid, and a Fault field
render.go          the fault line, and a session malformed count
```

### Two corrections to stage 10 that this stage surfaced

**Subagents ran with every deadline switched off.** `newChild` never copied
`dl`, so a child got a zero `deadlines` struct — and `guardBody` and `waitFor`
both treat `<= 0` as "no watchdog". Stage 10's entire subject did not apply to
subagents, silently, and nothing failed to say so. The one child that hangs
forever is exactly the case stage 10 exists to prevent, and it was the case still
exposed to it.

**`argsForDisplay` is the lenient parse, quarantined.** The panel and the
compaction summary need a readable command from arguments that may be broken —
that is the *right* place for a lenient parse, and a viewer that shows a mangled
command is more useful than one that shows nothing. It is a separate function
with a name that says so, and its result must never reach execution. It also
never returns blank: `{"raw_arguments":""}` extracts to an empty string, and a
tool call that vanishes from the transcript is the loss of the last record that
the agent tried something.

### One thing checked and found already correct

A shipped agent I read hit a bug worth naming: their stall watchdog **cancelled
the reader before rejecting**, and cancelling settles the pending read as a clean
end-of-stream — so a stalled stream was reported as a finished one, silently
truncating the turn instead of raising a retryable error. The failure presents as
a short, plausible answer rather than an error.

Stage 10 does not reproduce it, and the reason is worth knowing rather than
assuming: cancelling a Go request context makes `resp.Body.Read` return an error,
never `io.EOF`; `readSSE` propagates any non-EOF error; and both decoders return
`res, err` while deliberately not emitting `KindResponseEnd`. If the guard fires
as the stream legitimately completes, `err == nil` wins and the cause is ignored —
the correct resolution of the same race, reached from the other side.

---

## What this stage does not fix

- **A schema-valid, semantically wrong argument.** `{"command": "[\"echo\",\"hi\"]"}`
  passes every check here and is not a shell command. Nothing short of running it
  finds out, which is why the permission gate is still the last line.
- **The provider that silently drops a long argument value.** A failure mode
  reported by a shipped agent: the value arrives at the server and does not arrive
  at the model, so what comes back is *valid JSON missing a required key*. The
  model believes it sent the field, retries identically, and the transport eats it
  again — eight consecutive failures observed. No JSON validation can see this;
  detecting it needs identical arguments *and* an identical result across turns.
- **Markup leaking on a complete turn.** `stripHarnessMarkup` is gated on
  `StopMaxTokens`, so a provider whose compatibility layer failed to parse a
  finished call leaves its markup in place. The gate is deliberate: on a truncated
  turn the text is incomplete by definition, so cutting loses nothing, whereas on a
  complete turn an agent asked to explain this very wire format would have its
  answer silently truncated at the first `<tool_call>` it quoted — and this repo's
  own documentation is full of them.
- **Nested object properties in a schema.** No tool here has one, and a schema
  walker that recurses is a schema walker with a cycle bug waiting for it.

---

## Try it yourself

1. **Watch the fuse fire.** `--max-tokens 110` against the Anthropic route with a
   prompt that needs a long command. Then raise it to 4096 and watch the same
   prompt succeed on the first call. The number the error message names is the
   number that fixes it, which is the whole point of sending it to the human.
2. **Turn the boundary off.** Make `checkCall` always return `argCheck{Args: obj}`
   and run the same session. Count what reaches the shell.
3. **Add the repair.** Implement the twenty-line brace-closer, run it on the
   `raw_arguments` payloads recorded verbatim in §A3c, and check each result against
   what the model was trying to do. Then try it on `git clean -fdx -e .env`.
4. **Break the accumulator.** Change `mergeArgs` back to unconditional append and
   feed it the re-send dialect from `toolcall_test.go`. The error you get names a
   byte offset and nothing about the cause — which is what makes this class of bug
   expensive.
5. **Put an instruction back.** Add "Retry with a shorter command." to a fault
   text and run a long session. It takes a while to see, which is exactly why the
   test is mechanical.
6. **Find the fourth dialect.** Point the agent at a different gateway and log
   every `tool_args_delta`. Three are documented here; there is no reason to think
   that is all of them.

→ Next: Stage 12 — Echo

→ Reference: [Wire notes](wire-notes.md), [Stage 01 — Don't Die](01-dont-die.md), [Stage 09 — Triage](09-triage.md), [Stage 10 — Deadlock](10-deadlock.md)
