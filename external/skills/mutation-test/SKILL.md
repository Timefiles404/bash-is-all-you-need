---
name: mutation-test
description: Verify a Go test suite has teeth by breaking the code on purpose, one change at a time
---

# Mutation testing a suite in this repo

A test suite that cannot fail is worthless, and a green suite is not evidence
that it can. This is the acceptance bar for every test file here.

## The loop

For each mutation:

1. Copy the source file byte for byte to a scratch location outside the repo.
2. Apply exactly one mutation.
3. Run `go vet ./<stage>/code/` **first**. A mutation that does not compile
   proves nothing — it is not a caught mutation, it is an invalid one. Rewrite
   it so it compiles (`if false && x` instead of deleting a block that leaves an
   unused variable).
4. Run `go test ./<stage>/code/`. It must FAIL, and you must record which test
   caught it.
5. Restore from the byte copy and `cmp` to confirm the restore was exact.

A mutation that survives means a missing test. Write it.

## Mutations worth trying on anything

- Invert a boundary: `<=` becomes `<`, `>=` becomes `>`.
- Drop one clause of a compound condition.
- Return the first element where the code returns the last, or vice versa.
- Delete an error check and continue on the happy path.
- Reverse an ordering, or remove a sort.
- Replace a computed value with a plausible constant.
- Skip an element in a loop that should visit all of them.

## Mutations specific to this codebase

- **Accounting**: make `Usage.Input` equal `prompt_tokens` instead of
  `prompt_tokens - cached_tokens`. This is invisible on a cold request, so a
  suite that only tests cold calls will not catch it.
- **Cut points**: drop one of the two checks in `canCutBefore`. Either alone
  still passes on conversations whose cut lands somewhere safe by luck.
- **Byte fidelity**: re-encode `Block.Args` through `map[string]any`. The value
  is equal and the bytes move.
- **Display width**: make a wide rune report 1 column.

## Reporting

Give a table: mutation, whether it compiled, caught or survived, and which test
caught it. Call out any mutation you had to rewrite to make it compile, and any
that only one test catches — that test is now load-bearing and should say so in
a comment.
