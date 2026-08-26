# Stage 00 — The Loop

**What you build:** a coding agent that can explore a codebase, run commands, edit
files, and check its own work.

**What it costs you:** one file, ~200 lines of Go, no dependencies outside the
standard library.

That ratio is the first lesson. An agent is not a large piece of software. The
hard parts of this repo are all in the stages that come *after* this one, and
none of them are about making the agent smarter.

---

## Read it in this order

`stages/00-loop/main.go`, three functions:

1. **`main()`** — the loop. Read this first; everything else is a detail it calls.
2. **`callModel()`** — one HTTP POST. There is no SDK here, and there is nothing
   underneath an SDK except this.
3. **`runBash()`** — ten lines. Every action the agent is capable of goes through
   them.

## The shape of the loop

```
user types a task
  └─ append {role:"user"} to messages
     └─ LOOP:
        ├─ POST /chat/completions with the whole message array
        ├─ append the assistant reply to messages
        ├─ no tool_calls?  → print the text, exit LOOP
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

The single most common beginner mistake in `runBash` is this:

```go
out, err := cmd.CombinedOutput()
if err != nil {
    return err          // ← wrong
}
```

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
| 2 | `cat README.md && cat stats.py` | 613 |
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
3. **Change `maxTokens` to 100.** Now `finish_reason` comes back as `length` and
   the agent silently truncates mid-thought. This is the bug stage 01 fixes.
4. **Point it at a different model.** Any OpenAI-compatible endpoint works,
   including a local Ollama. Small models produce malformed tool calls — that is
   not your code failing, and it is worth seeing early.

→ Next: [Stage 01 — Don't Die](01-dont-die.md)
