# Stage 00 — The Loop

**What you build:** a coding agent that can explore a codebase, run commands, edit
files, and check its own work.

**What it costs you:** one file, 346 lines of Go — 253 once the comment-only
lines are taken out, and this file is heavily commented — with no dependencies
outside the standard library.

That ratio is the first lesson. An agent is not a large piece of software. The
hard parts of this repo are all in the stages that come *after* this one, and
none of them are about making the agent smarter.

---

## Read it in this order

`stages/00-loop/main.go`, three functions:

1. **`main()`** — the loop. Read this first; everything else is a detail it calls.
2. **`callModel()`** — one HTTP POST. There is no SDK here, and there is nothing
   underneath an SDK except this.
3. **`runBash()`** — fourteen lines. Every action the agent is capable of goes through
   them.

## The shape of the loop

```
user types a task
  └─ append {role:"user"} to messages
     └─ LOOP:
        ├─ POST /chat/completions with the whole message array
        ├─ append the assistant reply to messages
        ├─ print [tokens: prompt=… completion=…]
        ├─ any text? → print it, whether or not tools were also asked for
        ├─ no tool_calls? → exit LOOP
        └─ for each tool_call:
             run the command
             append {role:"tool", tool_call_id, content: output}
           → back to the top
```

Two invariants hold this together, and both are load-bearing:

**The message array only grows.** Nothing is ever edited or removed. The
conversation *is* the agent's memory, and every request re-sends all of it.
Stage 05 is the first time we touch this rule, and it will cost us something to
break it.

**Every tool call must get a result.** If the model asks for three commands and
you answer two, the next request is malformed. If a command fails, the failure
*is* the result — see below.

---

## Errors are observations, not exceptions

The single most common beginner mistake in `runBash` is to give it an error
to return, and then return one:

```go
func runBash(shell, command string) (string, error) {   // ← the first wrong move
    out, err := cmd.CombinedOutput()
    if err != nil {
        return "", err                                   // ← and the one it leads to
    }
```

The real signature returns a bare `string`, and that is the decision. There is
nowhere to put an error because a non-zero exit is not one.

A command that exits non-zero has not broken your program. It has produced
information, and the model is the component that should react to it. Look at
what happened in the real run below: `python stats.py` exited 1 with a
`ZeroDivisionError` traceback, we handed the traceback back verbatim, and the
model used it to locate the bug. Had we returned a Go error there, the agent
would have stopped exactly where it became useful.

The same instinct applies all the way up: your job is to report the world
faithfully to the model, not to protect the model from it.

---

## The experiment: watch the bill grow

Set up a scratch directory and run the agent on a bug it has to find by itself.

```sh
go build -o agent ./stages/00-loop
mkdir -p sandbox && cd sandbox
# ... put some broken code here ...
export AGENT_BASE_URL=... AGENT_API_KEY=... AGENT_MODEL=...
../agent
> There is a bug in this directory's code. Find it, fix it, and verify the fix.
```

Here is the token line from an actual run of six turns (find bug → read files →
run → patch → re-read → verify):

| turn | what it did | prompt tokens |
|---:|---|---:|
| 1 | `ls -la` | 429 |
| 2 | `cat README.md; cat stats.py` | 613 |
| 3 | `python stats.py` → traceback | 737 |
| 4 | `sed -i ...` to patch it | 932 |
| 5 | `cat stats.py` | 1079 |
| 6 | `python stats.py` → clean | 1192 |

Now add up the right-hand column: **4982 tokens billed**, for a conversation
whose final size was **1192 tokens**. We paid 4.2× the size of the thing we
built.

This is not a bug in the code. It is what "re-send the whole history every turn"
means, and it is quadratic: a 40-turn session pays for the early turns forty
times. Every serious agent has an answer to it. Ours arrives in **stage 04**,
where the same experiment run twice — once with caching, once with
`--break-cache` — puts a number on the difference.

Keep this table. It is the baseline the rest of the repo is measured against.

---

## What is deliberately missing

Each of these is a stage-01 topic. Try to trigger them before reading the fix —
the failure is more memorable than the patch.

| Missing | How to make it bite |
|---|---|
| Output truncation | Ask it to `find /` or read a large file. Watch the context window fill with noise. |
| Command timeout | Ask it to start a dev server. The agent hangs forever. |
| Process-tree kill | Even with a timeout, a killed shell can leave orphaned children. |
| Permission gate | Nothing stops `rm -rf`. This is why you use a scratch directory. |
| Streaming | You stare at a blank terminal for the whole model turn. |
| `finish_reason` handling | We only branch on "were there tool calls". `length` (hit `max_tokens`) is silently treated as a finished turn. |

The `maxTurns` fuse is the one survival feature that made it in early, because
without it a model stuck in a retry loop will burn your key while you are
reading this file.

---

## Notes from the wire

Things the real API did that the types in `main.go` had to accommodate:

- **`content` comes back as `null`** on a tool-calling turn, not `""`. Go's
  `json.Unmarshal` treats null as a no-op, so a plain `string` field survives it
  — but a language with stricter null handling will crash here.
- **`tool_calls[].function.arguments` is a JSON *string*, not an object.** You
  must parse it. Never string-match it.
- **`reasoning_content` appears on models that think.** We drop it in this stage;
  stage 02 renders it, and stage 03 shows how it maps onto the Anthropic
  protocol's `thinking` blocks.

---

## Exercises

1. **Break the tool_call pairing.** Skip appending one `role:"tool"` message and
   read the error the API returns. This teaches you more about the protocol than
   the docs do.
2. **Delete the system prompt.** Watch how the agent's behaviour degrades — it
   starts asking *you* to run commands. Most of "agent quality" lives in that
   string.
3. **Change the `MaxTokens: 4096` in `callModel` to 100.** Now `finish_reason` comes back as `length` and
   the agent silently truncates mid-thought. This is the bug stage 01 fixes.
4. **Point it at a different model.** Any OpenAI-compatible endpoint works,
   including a local Ollama. Small models produce malformed tool calls — that is
   not your code failing, and it is worth seeing early.

---

## What you can answer now

**Why is a working coding agent only a few hundred lines?**
Because the loop is genuinely small: send the conversation, run whatever
commands come back, append the results, repeat. There is no SDK underneath —
`callModel()` is one HTTP POST — and one tool covers exploring, editing and
verifying. The hard parts of this repo are all in the stages after this one, and
none of them are about making the agent smarter.

**Why does a command that exits non-zero not return an error?**
Because a failed command has not broken your program, it has produced
information, and the model is the component that should react to it. In the run
in this chapter `python stats.py` exited 1 with a `ZeroDivisionError` traceback,
the traceback went back verbatim, and the model used it to locate the bug.
Returning a Go error there would have stopped the agent at the exact moment it
became useful.

**Why must every tool call get a result?**
Because the protocol pairs them: if the model asks for three commands and you
answer two, the next request is malformed. This holds for commands that failed
as much as for ones that worked, because the failure is the result. Exercise 1
exists to make you break the pairing on purpose and read what the API says
about it.

**Why is the message array only allowed to grow?**
Because the conversation is the agent's entire memory. Nothing is edited or
removed, so every request re-sends all of it, and that array is the only place
the agent knows what it has already seen and done. Stage 05 is the first time
this rule is broken, and breaking it costs something.

**Where did 4982 billed tokens come from in a session whose conversation ended
at 1192?**
From the prompt column of the six turns, added together. Each turn re-sends
everything before it, so the early turns are paid for again on every later turn
— 4.2× the size of the thing that was built. The final conversation is what you
have at the end; the sum of the column is what you were billed for.

**Why does that cost grow faster than the conversation does?**
Because the number of re-sends grows with the number of turns: a forty-turn
session pays for its first turn forty times. Six turns at 4.2× is mild, and the
ratio keeps climbing for as long as the session does. Every serious agent has an
answer to this, and this repo's arrives in stage 04.

**Why is `maxTurns` the one survival feature that made it in this early?**
Because without it a model stuck in a retry loop keeps calling the API for as
long as you leave the process running. Truncation, a timeout and a permission
gate each protect you from one bad command; `maxTurns` protects you from an
agent that is working exactly as written and going nowhere, which is the failure
most likely to find you while you are still reading the code.

**Why must `tool_calls[].function.arguments` be parsed rather than matched as
text?**
Because the field is a JSON *string* whose contents are themselves JSON, so the
value you want sits behind a second layer of encoding, with its own escaping.
Unmarshalling it a second time gives you a typed struct. Matching the raw string
gives you something that works on the examples you happened to try and depends
on how the model happened to format its arguments.

**Why does `content` coming back as `null` not break anything here?**
Because on a tool-calling turn the API sends `null` rather than `""`, and Go's
`json.Unmarshal` treats null as a no-op, leaving a plain `string` field at its
zero value. That is luck rather than design, and it is written down for that
reason: a language with stricter null handling crashes on exactly that field.

**Why does a response cut off at `max_tokens` look like a finished turn?**
Because the loop asks a response only one question — were there tool calls — and
a response truncated mid-thought usually has none. `finish_reason` is never
read, so `length` is indistinguishable from a model that had finished talking.
That is the bug stage 01 fixes.

---

## Questions to think about

These have no answer in the repo. They are the decisions that go differently
depending on what you are building.

1. The turn ends when the model stops asking for tools, which makes "finished"
   and "gave up" the same event. What else could end a turn, and what would the
   agent have to look at to tell those two apart?

2. Most of what makes this agent behave well lives in the system prompt rather
   than in the code. For some behaviour you wanted — say, always running the
   tests before claiming a fix works — how would you decide whether it belongs
   in that string or in the harness, and how would you find out you had chosen
   wrong?

3. `maxTurns` fuses the number of turns, but the thing you are spending is
   tokens, and one turn can cost anything. What would you fuse instead, and
   where would that fuse have to sit to stop a run before the money is gone
   rather than after?

4. The history only grows, and stages 04 and 05 answer that in two different
   ways. Before reading either: what would you drop first from a fifty-turn
   history, and how would you discover that you had dropped something the model
   still needed?

5. Nothing here restricts what the agent may run, and the advice is to use a
   scratch directory — which does not survive contact with real work. What would
   you actually restrict, and would you enforce it in the prompt, in the tool,
   or in the process the tool runs?

→ Next: [Stage 01 — Don't Die](01-dont-die.md)
