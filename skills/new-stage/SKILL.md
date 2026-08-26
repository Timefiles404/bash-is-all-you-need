---
name: new-stage
description: Add a stage to this course — the snapshot rules, the chapter shape, and what a chapter has to measure
---

# Adding a stage

Each `stages/NN-name/` is a **complete, standalone snapshot** of the agent at one
point in the course. A reader should be able to `diff stages/NN stages/NN+1` and
see exactly one idea arrive.

## Rules that are not negotiable

- **Copy the previous stage, do not import it.** Duplication between snapshots is
  the feature. Never factor shared code into a common package.
- **No dependencies** beyond the standard library and `golang.org/x/sys`. Stage
  08 is the single exception, and its chapter is largely about why that one
  earns its place.
- **One idea per stage.** If the diff introduces two, it is two stages.

## The chapter

`docs/NN-name.md`, and it needs all of these:

- The idea in the first three paragraphs, including what it costs.
- A **"From a real run"** section with output you actually captured. Invented
  examples undercut the entire premise of the repo.
- At least one **measurement**, with the arms stated and the confounds named. If
  the measurement undercuts the chapter's thesis, say so — stage 04 found that
  no cache markers beat markers on a short session, and stage 05 found that
  compaction costs more than it saves. Both say so plainly.
- At least one **failure found while writing it**. These are the most valuable
  paragraphs in the repo. Look for them rather than writing a clean narrative.
- Exercises that break something on purpose.

## Verification before commit

```sh
gofmt -l stages/                        # empty
go vet ./...                            # clean
go test -race ./...                     # green
GOOS=linux go build ./stages/...        # the platform files are real
GOOS=darwin go build ./stages/...
grep -rnE 'sk-[A-Za-z0-9]{20,}' --exclude-dir=.git .   # no keys, ever
```

Run the agent only inside `sandbox/` — it executes what the model says.

## Comments

Match the density of `stages/04-the-cache/render.go`. Comments explain *why* and
name the failure they prevent; they never restate the code.
