# The level format

A level is a JSON file and a directory of assets, and almost none of it is
written by hand. The rule that shapes everything below:

> **A level's correct answer must reproduce the repository's real source, byte
> for byte, or the level does not build.**

That is the whole reason for the format. A course that transcribes its subject's
code into its own files starts identical and diverges quietly, and six months
later the site is teaching a version of `truncate` that no longer exists. Here,
the correct option is not a copy of the repository's code — it is a *claim about*
the repository's code, checked at build time by extracting the real thing and
comparing. When `stages/02-see-everything/exec.go` changes, either the level's
claim still holds or `genlevels` fails and somebody has to look.

Levels therefore live at `web/content/chNN/levels/*.json`, and their generated
output — the assembled sources, the build table, the compiled artifact — lands
in `web/assets/levels/<levelId>/`, which is entirely machine-written and should
never be edited.

---

## The file

```jsonc
{
  "id": "ch02-l3",              // stable; it names the asset directory
  "chapter": "ch02",
  "stage": "02-see-everything", // the directory under stages/ this teaches
  "title": "Keep the head and the tail",
  "estimateMinutes": 8,

  "source": { ... },            // what is extracted from the repo
  "program": { ... },           // what gets compiled and run
  "holes": [ ... ],             // what the learner fills in
  "expect": { ... },            // what running it should produce
  "hints": [ ... ],
  "reveal": { ... },            // the skip path
  "trace": { ... },             // the recorded session, if the level has one
  "shell": { ... }              // the terminal, if the level has one
}
```

### `source` — where the code comes from

```jsonc
"source": {
  "file": "stages/02-see-everything/exec.go",
  "extract": [
    { "symbol": "truncate" },        // a top-level func, type, var or const
    { "symbol": "sanitize" },
    { "symbol": "ansiRE" }
  ],
  "docComment": "keep"               // "keep" | "drop" | "asProse"
}
```

`genlevels` parses the file with `go/parser` and takes the named declarations
with their doc comments. `symbol` may name a method as `Type.Method`.

`docComment: "asProse"` lifts the declaration's doc comment out of the code and
into the level's prose. This repository's comments carry the teaching — the
reason head+tail truncation exists is a paragraph above `truncate` — and a level
that leaves it in the editor gives the answer away directly above the hole.

### `program` — what actually runs

The extracted declarations are not a program. This says how to make one.

```jsonc
"program": {
  "package": "main",
  "imports": ["fmt", "os", "regexp", "strings", "unicode/utf8"],
  "harness": "harness.go",     // relative to the level's content directory
  "argv": ["3"],               // which variant of the chapter binary to run
  "files": { "input.txt": "fixtures/long-output.txt" }
}
```

`harness.go` is hand-written, small, and is the one place a level author writes
Go. It supplies `main`, feeds fixtures in, and prints results. It is shown to
the learner read-only, in a collapsed pane, because a learner who cannot see the
harness cannot tell whether the output is theirs.

**Imports are checked, not trusted.** `genlevels` type-checks the assembled
program; an import the harness does not use is a build failure, because Go says
so and the level should not ship a program that would not compile.

### `holes` — what the learner fills in

A hole names a span of the extracted source and offers replacements. The span is
addressed by an exact byte match against the extracted text, which is what makes
the drift check work: if the repository's bytes change, the anchor stops matching
and the build fails loudly.

```jsonc
"holes": [
  {
    "id": "split",
    "anchor": "\thead := max * 2 / 3\n\ttail := max - head",
    "prompt": "How much of the output do you keep, and from where?",
    "options": [
      {
        "id": "head-only",
        "text": "\thead := max\n\ttail := 0",
        "why": "The obvious one, and the one almost every harness ships. It is also the one that loses the line you needed: a failing test run puts the summary at the end, and a stack trace puts the actual error there too.",
        "correct": false
      },
      {
        "id": "head-tail",
        "text": "\thead := max * 2 / 3\n\ttail := max - head",
        "why": "Two thirds head, one third tail. The head carries what the command was doing; the tail carries how it ended. The middle is the part a model can most often do without.",
        "correct": true
      },
      {
        "id": "tail-only",
        "text": "\thead := 0\n\ttail := max",
        "why": "Keeps the outcome and throws away the command that produced it. Better than head-only for a test run and worse for anything that fails early.",
        "correct": false
      }
    ]
  }
]
```

Rules the generator enforces:

- Exactly one option per hole has `correct: true`.
- Concatenating every correct option must reproduce the extracted source
  **byte for byte**. This is the drift check, and it is not advisory.
- `why` is required on every option, including the correct one. An option
  without a reason is a guess with a label on it.
- `anchor` must occur exactly once in the extracted text.

### `expect` — what running it should produce

```jsonc
"expect": {
  "stdout": "fixtures/expected-l3.txt",
  "exitCode": 0,
  "match": "exact"        // "exact" | "contains" | "regexp"
}
```

Filled in by `genlevels`, by running the correct program and recording what it
printed. A hand-written expectation is a second implementation of the program
and will disagree with it eventually.

### `hints` — in order, revealed one at a time

```jsonc
"hints": [
  "What does a Go test print last?",
  "Look at what `[... N bytes elided ...]` implies about where the cut is.",
  "docs/01-dont-die.md explains why head-only truncation loses the line that mattered."
]
```

### `reveal` — the skip path

Every level can be skipped, and skipping shows the diff rather than the answer
in isolation, because the diff is what the repository is organised around: `diff
stages/NN stages/NN+1` shows one idea arriving.

```jsonc
"reveal": {
  "steps": [
    { "hole": "split", "note": "The repository's answer, and the paragraph above it says why." }
  ],
  "diff": { "from": "stages/01-dont-die/exec.go", "to": "stages/02-see-everything/exec.go" },
  "readMore": "docs/01-dont-die.md#truncation"
}
```

`diff` is computed at build time from the real files; the level stores only the
two paths.

### `trace` — the recorded session

```jsonc
"trace": {
  "file": "session.jsonl",
  "kinds": ["user_message", "turn_start", "request", "first_token",
            "text_delta", "tool_call_ready", "command_start", "command_end",
            "tool_result", "usage", "response_end", "turn_end"],
  "focus": { "fromSeq": 41, "toSeq": 96 },
  "speed": 1
}
```

`kinds` is a filter, not a schema: a trace carries whatever the agent emitted,
and the level chooses what to show. `focus` narrows to a range of `seq` values —
a chapter about cache markers wants two turns out of forty, and the header still
reports the whole session so a filtered view is not mistaken for the session.

`genlevels` checks that the trace's `seq` range covers `focus`, that the file
parses, and that it was produced by the stage the level names. A trace from
another stage replayed under this level's commentary would be describing a
session that did not happen.

### `shell` — the terminal

```jsonc
"shell": {
  "cwd": "/work",
  "seed": { "notes.txt": "fixtures/notes.txt", ".env": "KEY=not-a-real-key\n" },
  "policy": { "level": "argv", "enforce": true, "secret": ".env" },
  "suggest": ["ls -la", "grep -c pear fruit", "cat < .env"]
}
```

`policy` drives the stage 08 lesson: the three inspectors are three settings of
one object, and a learner switches between them and re-runs the same bypass.

---

## Generating a level from the repository

`web/tools/genlevels` does this, in order, and stops at the first failure:

1. **Extract.** Parse `source.file` with `go/parser`; take the declarations
   named in `extract`, with doc comments, in source order. Record their exact
   bytes.
2. **Check the anchors.** Every hole's `anchor` must appear exactly once in the
   extracted text. A missing anchor means the repository moved and the level is
   stale.
3. **Check the drift.** Substitute every correct option into its anchor. The
   result must equal the extracted text exactly. It will, unless an option's
   `text` was edited without re-checking, which is the mistake this catches.
4. **Assemble.** Extracted source + harness + package clause + imports.
5. **Type-check the correct program** with `go/types`. A level that ships a
   program the compiler rejects is a level nobody can pass.
6. **Compile.** `GOOS=js GOARCH=wasm go build`, into the chapter's binary. All of
   a chapter's levels compile into **one** binary with a variant switch, because
   the fixed cost of a Go wasm binary is ~2.6 MB and the marginal cost of another
   level's code is a few kilobytes — measured at 16 KB raw for four extra
   variants. See ARCHITECTURE.md.
7. **Compile the wrong ones**, and record what `go build` said. Full product when
   it is under the cap, per-hole otherwise; the mode goes in the build table so
   the runtime knows whether an answer is a transcript or a composition.
8. **Run the correct program** and record stdout, stderr and the exit code as
   `expect`.
9. **Emit** `web/assets/levels/<id>/{level.json, build-table.json}` and the
   chapter binary, plus a copy of the trace.

Steps 2, 3 and 5 are why this format exists. They are the difference between a
course that is about a repository and a course that used to be.

---

## A complete worked example

`web/content/ch02/levels/ch02-l3.json`:

```json
{
  "id": "ch02-l3",
  "chapter": "ch02",
  "stage": "02-see-everything",
  "title": "Keep the head and the tail",
  "estimateMinutes": 8,

  "source": {
    "file": "stages/02-see-everything/exec.go",
    "extract": [{ "symbol": "truncate" }],
    "docComment": "asProse"
  },

  "program": {
    "package": "main",
    "imports": ["fmt", "os", "unicode/utf8"],
    "harness": "harness.go",
    "argv": ["3"],
    "files": { "input.txt": "fixtures/go-test-output.txt" }
  },

  "holes": [
    {
      "id": "floor",
      "anchor": "\tif max < 256 {\n\t\tmax = 256\n\t}",
      "prompt": "What happens when the caller asks for a very small budget?",
      "options": [
        {
          "id": "none",
          "text": "",
          "why": "No floor. A caller that passes 12 gets eight bytes of head and four of tail, which is not an excerpt of anything. Worse, the elision marker is longer than the output it replaced.",
          "correct": false
        },
        {
          "id": "floor-256",
          "text": "\tif max < 256 {\n\t\tmax = 256\n\t}",
          "why": "A floor, so that the result is always big enough to be worth reading. 256 is not magic; it is 'more than the elision marker and enough for a line or two either side'.",
          "correct": true
        }
      ]
    },
    {
      "id": "split",
      "anchor": "\thead := max * 2 / 3\n\ttail := max - head",
      "prompt": "How much of the output do you keep, and from where?",
      "options": [
        {
          "id": "head-only",
          "text": "\thead := max\n\ttail := 0",
          "why": "The obvious one, and the one almost every harness ships. It is also the one that loses the line you needed: a failing test run puts the summary at the end, and a panic puts the error there too.",
          "correct": false
        },
        {
          "id": "head-tail",
          "text": "\thead := max * 2 / 3\n\ttail := max - head",
          "why": "Two thirds head, one third tail. The head carries what the command was doing; the tail carries how it ended. The middle is the part a model can most often do without.",
          "correct": true
        },
        {
          "id": "tail-only",
          "text": "\thead := 0\n\ttail := max",
          "why": "Keeps the outcome and throws away the command that produced it. Better than head-only for a test run, worse for anything that fails early.",
          "correct": false
        }
      ]
    },
    {
      "id": "runes",
      "anchor": "\tfor head > 0 && !utf8.RuneStart(s[head]) {\n\t\thead--\n\t}",
      "prompt": "The cut lands in the middle of a multi-byte character. Then what?",
      "options": [
        {
          "id": "ignore",
          "text": "",
          "why": "Cut anyway. The output ends in half a rune, which prints as a replacement character — and if the model is asked to quote it back, the broken byte travels into the next request.",
          "correct": false
        },
        {
          "id": "back-up",
          "text": "\tfor head > 0 && !utf8.RuneStart(s[head]) {\n\t\thead--\n\t}",
          "why": "Walk back to a rune boundary. Costs at most three bytes and means the excerpt is always valid UTF-8, which everything downstream assumes.",
          "correct": true
        }
      ]
    }
  ],

  "expect": { "stdout": "fixtures/expected-l3.txt", "exitCode": 0, "match": "exact" },

  "hints": [
    "Run it with head-only selected and read the last line of the output you get.",
    "What does `go test` print last, and where would it be after a head-only cut?",
    "docs/01-dont-die.md has the paragraph this function's comment is short for."
  ],

  "reveal": {
    "steps": [
      { "hole": "floor", "note": "The floor comes first because the other two only make sense above it." },
      { "hole": "split", "note": "The repository's answer, and the reason is the comment above the function." },
      { "hole": "runes", "note": "The detail that only shows up when someone's build output is not ASCII." }
    ],
    "diff": { "from": "stages/01-dont-die/main.go", "to": "stages/02-see-everything/exec.go" },
    "readMore": "docs/01-dont-die.md"
  },

  "trace": {
    "file": "session.jsonl",
    "kinds": ["command_start", "command_end", "tool_result", "notice"],
    "focus": { "fromSeq": 118, "toSeq": 140 },
    "speed": 1
  },

  "quiz": null
}
```

`web/content/ch02/levels/harness.go`:

```go
// Shown to the learner, read-only. Everything above this line is theirs.
package main

import (
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile("/work/input.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, cut := truncate(string(data), 512)
	fmt.Println(out)
	fmt.Printf("\n--- %d bytes in, %d out, truncated=%v ---\n", len(data), len(out), cut)
}
```

---

## The chapter quiz

One per chapter, at `web/content/chNN/quiz.json`. Its job is to catch the reader
who filled every hole by elimination.

```jsonc
{
  "chapter": "ch02",
  "passMark": 3,                 // of 4
  "questions": [
    {
      "id": "q1",
      "kind": "choice",          // "choice" | "multi" | "order" | "predict"
      "prompt": "The trace file is written with no bufio.Writer. Why?",
      "options": [
        { "id": "a", "text": "Buffered writes are slower for small records.", "correct": false,
          "why": "They are faster, which is the point of the trade being interesting." },
        { "id": "b", "text": "A 64KB buffer loses every event in it when the agent is killed — the moment the trace existed to explain.", "correct": true,
          "why": "One write(2) per event costs microseconds against model calls measured in hundreds of milliseconds. The trade is not close." },
        { "id": "c", "text": "bufio is not safe for concurrent use.", "correct": false,
          "why": "True and irrelevant: the writer holds its own mutex either way." }
      ],
      "source": "stages/02-see-everything/trace.go"
    },
    {
      "id": "q2",
      "kind": "predict",
      "prompt": "A trace's last line stops mid-object. What does ReadTrace return?",
      "answer": "everything recoverable, plus a synthetic notice",
      "accept": ["notice", "synthetic", "recovered"],
      "why": "Returning an error would invite `if err != nil { fatal }` and throw away the four hundred events that explain the crash."
    }
  ]
}
```

`source` on a question is checked the same way a level's is: the file must exist.
A quiz question about code that has been deleted is a question with no answer.
