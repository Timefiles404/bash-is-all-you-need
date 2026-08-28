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

Two things live outside `stages/`, and the test that let them exist is worth
stating because it is what stops the third:

> **A chapter explains it, or it is not in a stage.**

`tui/` is the interactive shell — a scrollback pane that folds its own detail
away, a bordered composer with a status row under it, a line editor, a Markdown
renderer, slash commands, a settings file. No chapter explains any of it, and
none should. It is there so that *using* the repo does not require the lesson
to grow: stage 06 builds a terminal UI out of three functions and a select, and
that chapter's whole value is that you can hold all of it in your head, so it
must stay small. Somebody debugging stage 09's triage wants the opposite — a
window that does not close when a config value is missing, a key that stops a
runaway turn, and a way to fix an endpoint without editing a file.

Two different programs, so two different places. Nothing was deleted from
`stages/` to make room for it, and `stages/NN/shell.go` — the file that wires
one up — is duplicated per stage like everything else.

`web/` is the second: a browser course that teaches the same thirteen stages by
having the reader assemble them. It is here rather than in a repository of its
own for one reason, and it is a mechanical one. `web/tools/genlevels` extracts
every line of every level from `stages/` and refuses to build if a byte has
moved — so a lesson cannot quietly go on describing code that changed. That
check needs to see both trees at once, which means one repository. Run it with
`python web/tools/build.py --check`; it takes seconds and needs no compiler, and
CI runs it on anything that touches `stages/`.

Unlike `tui/`, `web/` is **main-only**. It is bilingual inside itself — every
string it shows has a `zh` and an `en` form — so mirroring it onto `zh-cn` would
make two copies of one program that drift, with no chapter's correctness to
catch it. The Go it teaches is still mirrored; only the site is not.

The rule remains absolute for anything a chapter teaches. If you are about to
move something that a chapter walks through into a package because it appears in
seven stages: that duplication is the feature.

The course has two halves. **Phase 1 (00–08) is the instrument panel; phase 2
(09 onward) is what fails in production.** Phase 2 *branches* from **stage 07**,
not stage 08: stage 08 is the only stage in the repo with a dependency and is
advertised as optional, so carrying it down the trunk would make it mandatory.

That is a rule about where phase 2 starts, not about every stage in it. Stage 09
was copied from 07; stage 10 is copied from 09, and so on, because the property
that matters just as much is that `diff stages/NN stages/NN+1` shows **one**
idea. Copying from 07 every time would put every earlier phase 2 idea into that
diff as well. So: `cp -r stages/<previous> stages/NN-name`, and then add the one
idea — where `<previous>` is stage 07 for stage 09 and the preceding stage after
that.

## Rules

- **No dependencies** beyond the standard library and `golang.org/x/sys`, which
  is pinned at v0.41.0 because v0.42+ declares `go 1.25.0`. No SDKs, no TUI
  framework, no JSON library, no test framework.
  Stage 08 is the single exception (`mvdan.cc/sh/v3`, pinned at v3.12.0 with
  `golang.org/x/term` at v0.33.0 to keep the module floor at go 1.24.0). Before
  adding anything, read the new `go.mod` — a dependency's `go` directive is part
  of its cost, and it is the part nothing announces.
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
go test ./...                          # all stages, and tui/
gofmt -l stages/ tui/                  # must be empty
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

From stage 06 on, running the binary with no arguments opens the interactive
shell in `tui/`; `--no-tui` gives the plain line prompt the chapters describe,
`-p "prompt"` runs one turn and exits, and a piped stdin still behaves exactly
as it did in stage 00. When there is no `.env` — which is what happens if you
start the binary by double-clicking it — the shell starts anyway and
`/provider-url`, `/provider-protocol`, `/provider-model` and `/provider-apikey`
configure it and save outside the repo. `/help` lists the rest.
