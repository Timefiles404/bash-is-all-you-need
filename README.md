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

| Stage | Idea | Status |
|---|---|---|
| [00 The Loop](docs/00-loop.md) | request → tool call → execute → repeat. One file, no SDK. | ✅ built |
| 01 Don't Die | truncation, timeouts, process-tree kill, `finish_reason`, permission gate | planned |
| 02 See Everything | event bus, streaming, full instrumentation, JSONL trace, replay | planned |
| 03 Babel | one agent, many protocols: OpenAI + Anthropic behind a neutral core | planned |
| 04 The Cache | prompt caching as *discipline*, and what it is worth in dollars | planned |
| 05 Live Forever | compaction, context injection, file-based memory | planned |
| 06 The Composer | TUI: God view vs Model view of the conversation | planned |
| 07 Multiply | subagents by recursion, skills, and what PTC really is | planned |
| 08 Sandbox *(optional)* | embedded shell interpreter, per-process interception | planned |

## What you get by the end

- Per-turn token accounting that distinguishes **full-price / cache-write /
  cache-read**, with a running cost ledger in your own currency.
- TTFT and tokens-per-second on every model call; wall-clock on every command.
- A **request inspector** — one keystroke dumps the exact bytes about to be sent.
- A JSONL trace of every session, and `replay` to step through one **without an
  API key** (which is also how you can study a session you never paid for).
- A conversation view that shows compaction as a first-class event: what was
  summarized, why, and what it cost you in tokens *and* in cache invalidation.
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
../agent
> find the bug in this directory, fix it, and verify the fix
```

Any OpenAI-compatible endpoint works — OpenRouter, DeepSeek, Kimi, GLM, or a
local Ollama / vLLM / LM Studio. Stage 03 adds the Anthropic protocol alongside
it.

---

## Non-goals

Stated so the boundaries are visible, since knowing where a teaching project
stops is part of the teaching:

- **Not a Claude Code replacement.** Use Claude Code. This explains one.
- **No MCP, no plan mode, no multi-model routing.** Each doc notes the layer
  where you would add them.
- **No agent framework.** No LangChain, no vector database, no orchestration
  layer. Standard library plus a shell.
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
