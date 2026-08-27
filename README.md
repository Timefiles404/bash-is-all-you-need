# bash is all you need

**A coding agent with one tool and a glass cockpit.**

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
whatever its author imagined. `docs/` argues both sides where it matters.

---

## Stages

Each stage introduces exactly one idea and ships a complete, runnable snapshot
under `stages/`. Duplication between snapshots is intentional — a readable diff
beats DRY in a teaching repo.

### Phase 1 — the instrument panel (00–08)

| Stage | Idea | Status |
|---|---|---|
| [00 The Loop](docs/00-loop.md) | request → tool call → execute → repeat. One file, no SDK. | ✅ built |
| [01 Don't Die](docs/01-dont-die.md) | truncation, timeouts, process-tree kill, `finish_reason`, permission gate | ✅ built |
| [02 See Everything](docs/02-see-everything.md) | event bus, streaming, full instrumentation, JSONL trace, replay | ✅ built |
| [03 Babel](docs/03-babel.md) | one agent, many protocols: OpenAI + Anthropic behind a neutral core | ✅ built |
| [04 The Cache](docs/04-the-cache.md) | prompt caching as *discipline*, and what it is worth in dollars | ✅ built |
| [05 Live Forever](docs/05-live-forever.md) | compaction, context injection, memory — and what compaction really costs | ✅ built |
| [06 The Composer](docs/06-the-composer.md) | a TUI in the standard library: God view vs Model view of the same session | ✅ built |
| [07 Multiply](docs/07-multiply.md) | subagents by recursion, skills, and what PTC really is | ✅ built |
| [08 Sandbox](docs/08-sandbox.md) *(optional)* | embedded shell interpreter, and why you cannot secure a shell by reading the command | ✅ built |

### Phase 2 — production (09–19)

Phase 1 builds an agent you can *see*. Phase 2 is about the week after you let
someone else use it: the call that fails, the tool that never returns, the JSON
that is not JSON, the note that went stale, the P95 nobody measured. Same rules
— one idea per stage, and no claim without a number behind it.

Phase 2 continues from **stage 07**, not stage 08. Stage 08 is the one place in
the repo that takes a dependency, and it is advertised as optional; carrying it
down the trunk would make it mandatory in practice. It stays a side road —
`diff stages/07-multiply stages/08-sandbox` is the patch, if you want the
sandbox in a later stage.

| Stage | Idea | Status |
|---|---|---|
| [09 Triage](docs/09-triage.md) | an error is a decision, not a string: one taxonomy over two protocols, `Retry-After`, a retry budget, the fallback ladder | ✅ built |
| [10 Deadlock](docs/10-deadlock.md) | the tool that never returns and the stream that stalls: every wait gets a deadline and an owner | ✅ built |
| [11 Malformed](docs/11-malformed.md) | the tool call is not valid JSON — what each protocol hands you, why repairing it is the trap, and one validation boundary | ✅ built |
| 12 Echo | the cheapest tool call is the one you do not make: content-addressed results, an LRU, staleness by `mtime` | 🚧 planned |
| 13 Rewind | the session and the workspace are both state, and both need a rewind — resume from the trace, checkpoint before mutation | 🚧 planned |
| 14 Amnesia | compaction is lossy, so measure the loss: a probe set, a recall number, protected regions | 🚧 planned |
| 15 Rot | a memory needs an expiry and a witness: stale versus wrong, supersede versus contradict, self-evolving skills that disagree | 🚧 planned |
| 16 The Briefing | context sharing is a budget, not a boolean — and the question a subagent is allowed to ask back | 🚧 planned |
| 17 Two Seconds | the P95, not the mean: parallel calls, prompt diet, cache alignment, semantic cache, model tiering | 🚧 planned |
| 18 The Scoreboard | four metrics off the trace, and a bad case that becomes a regression test | 🚧 planned |
| 19 Borrowed Tools | MCP written from scratch over stdio JSON-RPC, and the schema tax measured in tokens | 🚧 planned |

**Appendix: [Wire notes](docs/wire-notes.md)** — what one real gateway actually
sends, probed byte by byte: how each protocol reports a truncated tool call
(badly, and differently), where streaming usage lies, which error you get for an
unknown model (401, not 404), and proof that prompt caching works. Every claim
carries its raw evidence. The teaching material in `docs/` is built on this file
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
  Measured: the worst silence in 14 calls was **5.0s** against a 45s default,
  and finding that took three attempts because the first two measured the wrong
  thing.
- No vendor lock: any OpenAI- or Anthropic-compatible endpoint, including local
  models, configured by URL + key + protocol.

---

## Quickstart

Requires Go 1.24+ and a POSIX shell (on Windows: Git Bash, which ships with Git
for Windows — the agent finds it automatically).

```sh
git clone <this repo> && cd bash-is-all-you-need
cp .env.example .env      # fill in your endpoint, key and model
go build -o agent ./stages/00-loop

mkdir sandbox && cd sandbox    # it runs what the model says. use a scratch dir.
set -a && . ../.env && set +a
../agent --trace session.jsonl
> find the bug in this directory, fix it, and verify the fix
```

Then look at what it did — no key required, and it works on somebody else's
trace just as well as your own:

```sh
go build -o composer ./stages/06-the-composer
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
  which are the three things that chapter is about.
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
