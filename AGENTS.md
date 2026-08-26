# AGENTS.md

Conventions for this repository. Stage 05's agent reads this file at startup and
puts it in its system prompt, so this is both documentation and a live
demonstration of the feature — see [docs/05-live-forever.md](docs/05-live-forever.md).

## What this repo is

A progressive course. Each directory under `stages/` is a **complete, standalone
snapshot** of the agent at one point in the course. Duplication between them is
deliberate: a reader should be able to `diff stages/04-the-cache
stages/05-live-forever` and see exactly one idea arrive.

**Do not refactor shared code into a common package.** That is the one change
that would break the thing this repo is for.

## Rules

- **No dependencies** beyond the standard library and `golang.org/x/sys`, which
  is pinned at v0.41.0 because v0.42+ declares `go 1.25.0`. No SDKs, no TUI
  framework, no JSON library, no test framework.
- **Every teaching claim rests on `docs/wire-notes.md`**, which records what one
  real gateway actually sends, byte by byte. Where the protocol documentation
  and the observed behaviour disagree, the observation wins and the disagreement
  gets written down.
- **Comments explain *why*, and name the failure they prevent.** They never
  restate the code. Match the density of `stages/04-the-cache/render.go`.
- **A chapter reports what it measured**, including when the measurement
  undercuts the chapter's thesis. Stage 04 found that no cache markers beat
  markers on a short session; stage 05 found that compaction costs more than it
  saves. Both say so.
- **Tests are accepted only after mutation testing.** Break the code on purpose,
  one change at a time, and confirm a test fails for each. A mutation that
  survives means a missing test. A mutation that fails to *compile* proves
  nothing — check that the test binary built before believing a "caught".

## Commands

```sh
go build ./stages/06-the-composer      # or any stage
go test ./...                          # all stages
gofmt -l stages/                       # must be empty
go vet ./...                           # must be clean
GOOS=linux go build ./stages/06-the-composer    # the platform files are real
GOOS=darwin go build ./stages/06-the-composer
```

## Running the agent

It executes what the model says. Use `sandbox/` (gitignored) as the working
directory, never the repo root. Credentials come from `.env`, which is
gitignored and never committed:

```sh
set -a && . ./.env && set +a
```
