# bash is all you need

**English** · [中文](README_zh.md)

**A coding agent with one tool and a glass cockpit.**

> Every chapter exists in two editions: `<stage>/doc/README.md` (English) and
> `<stage>/doc/README_zh.md` (中文). They are **not** translations of each other
> — each was written separately from the same code. See
> [doc-style.md](doc-style.md) for the form both take.

Plenty of tutorials will teach you to write an agent loop. In 2026 that is an
afternoon's work, and this repo does it in stage 00, in one file, with no
dependencies. Then it spends the remaining stages on the part nobody teaches:

> **watching where every token, every millisecond and every cent actually goes.**

The gap between "I wrote an agent loop" and "I can run an agent in production"
is not intelligence. It is that most people cannot explain their own bill, do
not know why their cache hit rate collapsed, and cannot say what their model
actually saw on turn 30. This repo is built to make all three visible.

The agent has exactly one tool — `bash` — so that the engine stays small enough
to read in one sitting and the instrumentation gets to be the main character.

---

## Why one tool

Three properties, and the third is the one people miss:

- **Composable.** You cannot enumerate every action a user will need, but pipes
  compose the ones you have: `grep -rl foo src | xargs wc -l | sort -n | tail -5`
  is four tool calls collapsed into one round trip, with the intermediate data
  never touching the context window.
- **Discoverable.** The model does not need you to describe the environment.
  `ls`, `--help`, `which` are the discovery mechanism.
- **It inherits an ecosystem.** `ffmpeg`, `jq`, `rg`, `git`, `psql`, `kubectl` —
  forty years of CLI tooling, available immediately. You are not giving the agent
  tools; you are plugging it into every tool that already exists.

The honest caveat, stated up front: **"bash is all you need" is a claim about
sufficiency, not optimality.** Real products ship dedicated read/edit/grep tools
because they buy token efficiency, structured errors, staleness checks, and
permission granularity. Bash is what keeps the agent from being capped at
whatever its author imagined. Each chapter argues both sides where it matters.

---

## Stages

Each stage introduces exactly one idea and ships a complete, runnable snapshot
under its own directory. Duplication between snapshots is intentional — a readable diff
beats DRY in a teaching repo.

Every chapter ends the same way: exercises you can actually run, then the
questions that chapter has made answerable — with short answers, so you can
check yourself — and then a few open questions that have no answer here.

### Phase 1 — the instrument panel (00–08)

| Stage | Idea | Chapter |
|---|---|---|
| [00 The Loop](00-loop/) | request → tool call → execute → repeat. One file, no SDK. | [en](00-loop/doc/README.md) · [zh](00-loop/doc/README_zh.md) |
| [01 Don't Die](01-dont-die/) | truncation, timeouts, process-tree kill, `finish_reason`, permission gate | [en](01-dont-die/doc/README.md) · [zh](01-dont-die/doc/README_zh.md) |
| [02 See Everything](02-see-everything/) | event bus, streaming, full instrumentation, JSONL trace, replay | [en](02-see-everything/doc/README.md) · [zh](02-see-everything/doc/README_zh.md) |
| [03 Babel](03-babel/) | one agent, many protocols: OpenAI + Anthropic behind a neutral core | [en](03-babel/doc/README.md) · [zh](03-babel/doc/README_zh.md) |
| [04 The Cache](04-the-cache/) | prompt caching as *discipline*, and what it is worth in dollars | [en](04-the-cache/doc/README.md) · [zh](04-the-cache/doc/README_zh.md) |
| [05 Live Forever](05-live-forever/) | compaction, context injection, memory — and what compaction really costs | [en](05-live-forever/doc/README.md) · [zh](05-live-forever/doc/README_zh.md) |
| [06 The Composer](06-the-composer/) | a TUI in the standard library: God view vs Model view of the same session | [en](06-the-composer/doc/README.md) · [zh](06-the-composer/doc/README_zh.md) |
| [07 Multiply](07-multiply/) | subagents by recursion, skills, and what PTC really is | [en](07-multiply/doc/README.md) · [zh](07-multiply/doc/README_zh.md) |
| [08 Sandbox](08-sandbox/) *(optional)* | embedded shell interpreter, and why you cannot secure a shell by reading the command | [en](08-sandbox/doc/README.md) · [zh](08-sandbox/doc/README_zh.md) |

### Phase 2 — production (09–19)

Phase 1 builds an agent you can *see*. Phase 2 is about the week after you let
someone else use it: the call that fails, the tool that never returns, the JSON
that is not JSON, the note that went stale, the P95 nobody measured. Same rules
— one idea per stage, and no claim without a number behind it.

Phase 2 continues from **stage 07**, not stage 08. Stage 08 is the one place in
the repo that takes a dependency, and it is advertised as optional; carrying it
down the trunk would make it mandatory in practice. It stays a side road —
`diff 07-multiply/code 08-sandbox/code` is the patch, if you want the
sandbox in a later stage.

| Stage | Idea | Chapter |
|---|---|---|
| [09 Triage](09-triage/) | an error is a decision, not a string: one taxonomy over two protocols, `Retry-After`, a retry budget, the fallback ladder | [en](09-triage/doc/README.md) · [zh](09-triage/doc/README_zh.md) |
| [10 Deadlock](10-deadlock/) | the tool that never returns and the stream that stalls: every wait gets a deadline and an owner | [en](10-deadlock/doc/README.md) · [zh](10-deadlock/doc/README_zh.md) |
| [11 Malformed](11-malformed/) | the tool call is not valid JSON — what each protocol hands you, why repairing it is the trap, and one validation boundary | [en](11-malformed/doc/README.md) · [zh](11-malformed/doc/README_zh.md) |
| [12 Echo](12-echo/) | the cheapest tool call is the one you do not make — and an audit, on traces you already have, of what that is worth before you build it | [en](12-echo/doc/README.md) · [zh](12-echo/doc/README_zh.md) |
| 13 Rewind | the session and the workspace are both state, and both need a rewind — resume from the trace, checkpoint before mutation | 🚧 planned |
| 14 Amnesia | compaction is lossy, so measure the loss: a probe set, a recall number, protected regions | 🚧 planned |
| 15 Rot | a memory needs an expiry and a witness: stale versus wrong, supersede versus contradict, self-evolving skills that disagree | 🚧 planned |
| 16 The Briefing | context sharing is a budget, not a boolean — and the question a subagent is allowed to ask back | 🚧 planned |
| 17 Two Seconds | the P95, not the mean: parallel calls, prompt diet, cache alignment, semantic cache, model tiering | 🚧 planned |
| 18 The Scoreboard | four metrics off the trace, and a bad case that becomes a regression test | 🚧 planned |
| 19 Borrowed Tools | MCP written from scratch over stdio JSON-RPC, and the schema tax measured in tokens | 🚧 planned |

**Appendix: [Wire notes](external/wire-notes.md)** — what one real gateway actually
sends, probed byte by byte: how each protocol reports a truncated tool call
(badly, and differently), where streaming usage lies, which error you get for an
unknown model (401, not 404), and proof that prompt caching works. Every claim
carries its raw evidence. The teaching material in every chapter is built on this file
rather than on protocol documentation, because the two do not agree.

## What you get by the end

- Per-turn token accounting that distinguishes **full-price / cache-write /
  cache-read**, with a running cost ledger in your own currency.
- TTFT and tokens-per-second on every model call; wall-clock on every command.
- A **request inspector** — one keystroke dumps the exact bytes about to be sent.
- A JSONL trace of every session, and `replay` to step through one **without an
  API key** (which is also how you can study a session you never paid for).
- A conversation view that shows compaction as a first-class event: what was
  summarized, why, and what it cost you in tokens *and* in cache invalidation —
  measured at **+25% in full-price tokens on identical work**, which is why
  stage 05 argues compaction is a survival mechanism and not an optimisation.
- A three-view TUI over any trace — **what happened**, **what the model saw**,
  and **the raw bytes** — written on the standard library, because the
  interesting parts of a terminal UI (raw-mode restoration, the Escape
  ambiguity, display width vs byte length) are exactly what a framework hides.
- Long-term memory that is a file the agent appends to with `>>`, and a rule for
  where injected context may live so that knowing the time does not cost you
  your cache.
- Subagents that are the same loop called again, running concurrently into a
  single ordered trace — with the measurement that matters: **20% more tokens
  for a parent context 9.6x smaller.** A subagent does not save tokens, it saves
  context, and knowing which one you are short of is the whole decision.
- Skills that are a directory and one paragraph, with the index cost printed so
  you can see the tax you are paying on every request forever.
- An embedded shell that sees every command *after* expansion, and a measured
  table of the fourteen ways a regexp and a parser both lose.
- A failure taxonomy where two protocols' errors become one of three decisions —
  retry, fall back, stop — grounded in the recorded bytes that make the two
  obvious rules wrong: a nonexistent model returns **401**, and a malformed body
  returns **500**. Plus the number nobody reports, because the API cannot be
  asked for it: **what the failed attempts cost**.
- Three clocks on a streamed call instead of one, because `http.Client.Timeout`
  covers the body read and so cannot tell a slow answer from a dead socket — and
  the widest silence each stream actually had, **printed on the panel** next to
  TTFT, so the timeout is a measured margin rather than a number someone liked.
  Measured: the worst silence in 14 calls was **5.0s** against a 45s default —
  and the chapter shows why the two obvious ways to measure that number both
  report something else.
- A result cache for tool calls that ships **switched off**, because the chapter
  that builds it also measures it: replayed against sixteen recorded sessions it
  would have hit **4 times in 107 commands** and saved **401 ms** — against
  864 s of model time in those same sessions. The audit runs on traces you
  already have, with no API key, which is the point: you can find out what a
  cache is worth to *your* workload before writing one. Where it does pay is
  measured too — four agents reading one file at once, 21% hits — and so is the
  cache that was quietly doing the real work all along, at **83.9%**.
- No vendor lock: any OpenAI- or Anthropic-compatible endpoint, including local
  models, configured by URL + key + protocol.

---

## Quickstart

Requires Go 1.24+ and a POSIX shell (on Windows: Git Bash, which ships with Git
for Windows — the agent finds it automatically).

```sh
git clone <this repo> && cd bash-is-all-you-need
cp .env.example .env      # fill in your endpoint, key and model
go build -o agent ./00-loop/code

mkdir sandbox && cd sandbox    # it runs what the model says. use a scratch dir.
set -a && . ../.env && set +a
../agent --trace session.jsonl
> find the bug in this directory, fix it, and verify the fix
```

That is stage 00: a prompt, a loop, and nothing else. **From stage 06 on the
same binary opens an interactive shell instead** — a scrollback pane, a
bordered prompt with the provider, the model, how full the context is and the
running bill on the line under it, Escape to interrupt a turn, Ctrl-O to fold
the instrument panels away and bring them back, and slash commands:

```sh
go build -o agent ./12-echo/code
cd sandbox && ../agent
```

`/help` lists them, `/keys` is the keyboard, `/status` prints everything the
session is currently configured to do. It starts even with nothing configured:
`/provider-url`, `/provider-protocol`, `/provider-model`, `/provider-apikey`
and `/provider-window` set an endpoint up and save it outside the repo, which is
what makes the binary usable when it was started by double-clicking it rather
than from a shell that had sourced `.env`. `/open <dir>` moves the agent to
another directory.

Nothing that worked before it stopped working:

```sh
../agent -p "explain what this directory is"    # one turn, no UI, then exit
echo "same thing" | ../agent                    # a pipe, as in stage 00
../agent --no-tui                               # the plain prompt of the chapters
```

The shell lives in `external/tui/` and **no chapter explains it**, deliberately — see
the note in [AGENTS.md](AGENTS.md) on the one package that is allowed to exist
outside the stage folders.

Then look at what it did — no key required, and it works on somebody else's
trace just as well as your own:

```sh
go build -o composer ./06-the-composer/code
./composer --composer session.jsonl                  # TUI: g / m / w switch views
./composer --composer-dump session.jsonl --view model --call 12   # the same, greppable
```

Any OpenAI-compatible endpoint works — OpenRouter, DeepSeek, Kimi, GLM, or a
local Ollama / vLLM / LM Studio. Stage 03 adds the Anthropic protocol alongside
it.

---

## Non-goals

Stated so the boundaries are visible, since knowing where a teaching project
stops is part of the teaching:

- **Not a Claude Code replacement.** Use Claude Code. This explains one.
- **No plan mode.** Each doc notes the layer where you would add it. MCP and
  multi-model routing were non-goals through stage 08 and arrive in phase 2 —
  stage 19 and stage 17 — each with its bill attached rather than as a feature
  bullet.
- **No agent framework, and no TUI framework.** No LangChain, no vector
  database, no orchestration layer, no vendor SDK, no Bubble Tea. Stages 00-07
  and every phase 2 stage are the standard library plus `golang.org/x/sys` — for
  Windows Job Objects in stage 01 and terminal control in stage 06 — and that is
  all. Stage 08 is the
  single exception: it embeds `mvdan.cc/sh/v3`, and its chapter is largely an
  argument about when a dependency earns its place, with a measured account of
  what that one cost (it moved the Go floor twice before being pinned back).
  Stage 06 is where the no-framework rule stops being an aesthetic: a TUI
  framework hides raw-mode
  restoration, the Escape-key ambiguity, and display width versus byte length,
  which are the three things that chapter is about. The interactive shell in
  `external/tui/` is held to the same rule — it is the standard library and `x/sys`, and
  it is a package rather than a stage file precisely because it is *not* part of
  the course.
- **Not a benchmark chaser.** If you want SWE-bench numbers from a minimal
  agent, see `mini-swe-agent` below.

---

## Related work

This is a crowded field and the honest framing is that the *loop* has been
taught well many times. What is missing elsewhere is the instrumentation.

| Project | Shape | What it does not cover |
|---|---|---|
| [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) | Python, 17 progressive lessons, same slogan | single-provider; no token/cost/cache instrumentation; no TUI; no replay |
| [SWE-agent/mini-swe-agent](https://github.com/SWE-agent/mini-swe-agent) | ~100 lines Python; the purest bash-only agent — it does not even use the tool-calling API, the model just replies with a command | providers via litellm as a black box; not a progressive course; no cost/cache instrumentation |
| [ghuntley/how-to-build-a-coding-agent](https://ghuntley.com/agent/) | Go, 6-step workshop | multi-tool route; no instrumentation, TUI, or replay |
| [decodingai course](https://github.com/decodingai-magazine/building-a-coding-agent-from-scratch-course) | Python, 8 articles + 4 videos, Modal sandbox | tracing outsourced to a SaaS rather than built |
| [owenthereal/build-your-own-coding-agent](https://github.com/owenthereal/build-your-own-coding-agent) | Python, ~700 lines, no SDK — closest in spirit | no instrumentation, multi-protocol, or TUI |

Worth knowing about `mini-swe-agent` specifically: it demonstrates a form even
more radical than one tool — *zero* tools, with commands parsed out of plain
model output. If you think this repo is minimal, that one is the floor.

## License

MIT.
