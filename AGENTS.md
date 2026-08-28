# AGENTS.md

Conventions for this repository. Stage 05's agent reads this file at startup and
puts it in its system prompt, so this is both documentation and a live
demonstration of the feature.

## Layout

```
00-loop/            a stage. Thirteen of these, 00 through 12.
  code/             the complete program at that point in the course
  doc/              the chapter, and the diagrams it references
external/           everything the course does not teach
  tui/              the interactive shell the stage binaries launch
  web/              the browser course
  skills/           skills for the agent working on this repo
  wire-notes.md     what one real gateway actually sends, byte by byte
```

Each stage directory is a **complete, standalone snapshot** of the agent at one
point in the course. Duplication between them is deliberate: a reader should be
able to `diff 04-the-cache/code 05-live-forever/code` and see exactly one idea
arrive.

**Do not refactor shared code into a common package.** That is the one change
that would break the thing this repo is for.

`code/` holds the tests too, next to what they test. Go requires it — a package
is a directory — and splitting them would mean turning every stage into a
library plus a thin `main`, which is a bigger change to the teaching material
than the tidiness is worth.

## What goes in external/

The test that lets anything live outside a stage, and it is what stops the next
thing from moving there:

> **A chapter explains it, or it is not in a stage.**

`external/tui/` is the interactive shell — a scrollback pane that folds its own
detail away, a bordered composer with a status row under it, a line editor, a
Markdown renderer, slash commands, a settings file. No chapter explains any of
it, and none should. It is there so that *using* the repo does not require the
lesson to grow: stage 06 builds a terminal UI out of three functions and a
select, and that chapter's whole value is that you can hold all of it in your
head, so it must stay small. Somebody debugging stage 09's triage wants the
opposite — a window that does not close when a config value is missing, a key
that stops a runaway turn, and a way to fix an endpoint without editing a file.

Two different programs, so two different places. Nothing was deleted from a
stage to make room for it, and `NN-name/code/shell.go` — the file that wires one
up — is duplicated per stage like everything else.

`external/web/` is a browser course that teaches the same thirteen stages by
having the reader assemble them. It is here rather than in a repository of its
own for one mechanical reason: `external/web/tools/genlevels` extracts every
line of every level out of the stage directories and refuses to build if a byte
has moved, so a lesson cannot quietly go on describing code that changed. That
check needs to see both trees at once, which means one repository. Run it with
`python external/web/tools/build.py --check`; it takes seconds, needs no
compiler, and CI runs it on anything that touches the repo.

The rule stays absolute for anything a chapter teaches. If you are about to move
something a chapter walks through into a package because it appears in seven
stages: that duplication is the feature.

## The course

Two halves. **Phase 1 (00–08) is the instrument panel; phase 2 (09 onward) is
what fails in production.** Phase 2 *branches* from **stage 07**, not stage 08:
stage 08 is the only stage with a dependency and is advertised as optional, so
carrying it down the trunk would make it mandatory.

That is a rule about where phase 2 starts, not about every stage in it. Stage 09
was copied from 07; stage 10 from 09, and so on, because the property that
matters just as much is that `diff NN/code NN+1/code` shows **one** idea.
Copying from 07 every time would put every earlier phase 2 idea in that diff
too. So: `cp -r <previous>/code NN-name/code`, then add the one idea — where
`<previous>` is stage 07 for stage 09, and the preceding stage after that.

## Rules

- **No dependencies** beyond the standard library and `golang.org/x/sys`, which
  is pinned at v0.41.0 because v0.42+ declares `go 1.25.0`. No SDKs, no TUI
  framework, no JSON library, no test framework.
  Stage 08 is the single exception (`mvdan.cc/sh/v3`, pinned at v3.12.0 with
  `golang.org/x/term` at v0.33.0 to keep the module floor at go 1.24.0). Before
  adding anything, read the new `go.mod` — a dependency's `go` directive is part
  of its cost, and it is the part nothing announces.
- **Every teaching claim rests on `external/wire-notes.md`**, which records what
  one real gateway actually sends, byte by byte. Where the protocol
  documentation and the observed behaviour disagree, the observation wins and
  the disagreement gets written down.
- **Comments explain *why*, and name the failure they prevent.** They never
  restate the code. Match the density of `04-the-cache/code/render.go`.
  Comments are written in English, in every branch and every language edition.
- **A chapter reports what it measured**, including when the measurement
  undercuts the chapter's thesis. Stage 04 found that no cache markers beat
  markers on a short session; stage 05 found that compaction costs more than it
  saves. Both say so.
- **Tests are accepted only after mutation testing.** Break the code on purpose,
  one change at a time, and confirm a test fails for each. A mutation that
  survives means a missing test. A mutation that fails to *compile* proves
  nothing — check that the test binary built before believing a "caught".

## How a chapter is written

The rule the documentation is held to, because it is the one it failed before:

> **A reader meets a problem before they meet a solution, and every step is
> something they could have arrived at themselves.**

Not: here is the finished system, now let me take it apart for you. The
finished system is what the reader ends up holding, never what they are handed.
The full form is in [doc-style.md](doc-style.md).

## Commands

```sh
go build -o agent ./06-the-composer/code   # or any stage
go test ./...                              # every stage, and external/tui
gofmt -l ./*/code/ external/tui/           # must be empty
go vet ./...                               # must be clean
GOOS=linux go build -o /dev/null ./06-the-composer/code   # platform files are real
GOOS=darwin go build -o /dev/null ./06-the-composer/code
python external/web/tools/build.py --check                # the drift check
```

## Running the agent

It executes what the model says. Use `sandbox/` (gitignored) as the working
directory, never the repo root. Credentials come from `.env`, which is
gitignored and never committed:

```sh
set -a && . ./.env && set +a
```

From stage 06 on, running the binary with no arguments opens the interactive
shell in `external/tui/`; `--no-tui` gives the plain line prompt the chapters
describe, `-p "prompt"` runs one turn and exits, and a piped stdin still behaves
exactly as it did in stage 00. When there is no `.env` — which is what happens
if you start the binary by double-clicking it — the shell starts anyway and
`/provider-url`, `/provider-protocol`, `/provider-model` and `/provider-apikey`
configure it and save outside the repo. `/help` lists the rest.
