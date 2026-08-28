---
name: new-stage
description: Add a stage to this course — the snapshot rules, the chapter shape, and what a chapter has to measure
---

# Adding a stage

Each `NN-name/` at the repo root is one stage: `code/` is a **complete,
standalone snapshot** of the agent at that point in the course, and `doc/` is the
chapter about it. A reader should be able to `diff NN/code NN+1/code` and see
exactly one idea arrive.

## Rules that are not negotiable

- **Copy the previous stage, do not import it.** Duplication between snapshots is
  the feature. Never factor shared code into a common package.
- **No dependencies** beyond the standard library and `golang.org/x/sys`. Stage
  08 is the single exception, and its chapter is largely about why that one
  earns its place.
- **One idea per stage.** If the diff introduces two, it is two stages.

## The chapter

**Read `doc-style.md` before writing a word.** It is the form, and it is not
optional — the previous set of chapters was deleted for ignoring it. The one
sentence version:

> A reader meets a problem before they meet a solution, and every step is
> something they could have arrived at themselves.

On top of the form, a stage in this repo owes the reader:

- At least one **measurement**, with the arms stated and the confounds named. If
  the measurement undercuts the chapter's thesis, say so — stage 04 found that
  no cache markers beat markers on a short session, and stage 05 found that
  compaction costs more than it saves. Both say so plainly.
- At least one **failure found while writing it**. These are the most valuable
  paragraphs in the repo. Look for them rather than writing a clean narrative.
- Output you actually captured. Invented examples undercut the entire premise.

Chinese first, at `NN-name/doc/README_zh.md`. The English edition is written
separately from the same code, never translated from the Chinese.

## Verification before commit

```sh
gofmt -l ./*/code/ external/tui/         # empty
go vet ./...                             # clean
go test -race ./...                      # green
GOOS=linux go build -o /dev/null ./NN-name/code    # the platform files are real
GOOS=darwin go build -o /dev/null ./NN-name/code
python external/tools/quotecheck.py      # the chapter quotes real code
grep -rnE 'sk-[A-Za-z0-9]{20,}' --exclude-dir=.git .   # no keys, ever
```

Run the agent only inside `sandbox/` — it executes what the model says.

## Comments

Match the density of `04-the-cache/code/render.go`. Comments explain *why* and
name the failure they prevent; they never restate the code. They are written in
English regardless of which language edition of the chapter you are writing.
