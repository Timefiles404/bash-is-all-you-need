# The runtime layer

A browser-based course that teaches how to build a coding agent by having the
reader assemble one. Thirteen chapters, one per directory under `stages/`. Each
level ships Go source with holes; the learner fills the holes by choosing from a
fixed set of options, then runs the result and sees real output.

Static files. No application server, no bundler, no npm install, ES modules
loaded directly, no framework.

This document is about the layer under the interface: what actually runs, where
it runs, what it costs, and what happens when it does not work. Every number in
it was measured on this machine unless it says otherwise, and the ones that are
estimates say so.

---

## 1. Three problems, three answers

**Running Go.** Programs are compiled at build time by a real `go build`, one
WebAssembly binary per chapter, and the compiler's diagnostics for wrong option
combinations are recorded as transcripts. Nothing compiles in the browser. The
error a learner sees is the error `go build` produced.
*Cost: free editing does not work without an opt-in toolchain, and the interface
must never call a table lookup "compiling".*

**A shell.** `mvdan.cc/sh` — the interpreter stage 08 embeds — compiled to
WebAssembly and run over an in-memory filesystem that satisfies Go's js/wasm
syscall layer. Real parsing, real expansion, real pipelines, real redirection,
the real stage 08 policy in the real exec and open handlers.
*Cost: the external commands are re-implementations, not GNU's, and the
interpreter needs a four-site patch to make pipes work at all.*

**An LLM.** Replay by default, from the JSONL traces the repository already
writes, through the same event vocabulary the live agent uses. Live is opt-in,
direct from the learner's browser to an endpoint they choose, with the key never
leaving their machine except to that endpoint.
*Cost: most hosted providers will refuse the request on CORS, and there is no
honest way around that.*

---

## 2. The shape

```
   ┌──────────────────────── main thread ─────────────────────────┐
   │                                                              │
   │   index.html ──▶ ui/*.js  (another author owns these)        │
   │                     │                                        │
   │                     ▼                                        │
   │              runtime/api.js          Runtime.{build,run,      │
   │              ├── status.js            check,format,shell,fs}  │
   │              ├── llm.js  ──▶ trace.js                         │
   │              └── protocol.js                                  │
   │                     │  postMessage                            │
   └─────────────────────┼────────────────────────────────────────┘
                         │
   ┌─────────────────────▼──────── Web Worker ────────────────────┐
   │                                                              │
   │   runtime/worker.js                                          │
   │     ├── compiler.js   build table  ──▶ levels/*/build-table  │
   │     ├── gohost.js     wasm_exec.js, module cache             │
   │     ├── memfs.js  ────────────┐                              │
   │     └── persist.js  ─▶ IndexedDB                             │
   │                               │ installed as globalThis.fs   │
   │        ┌──────────────────────▼───────────────────────┐      │
   │        │  WebAssembly                                 │      │
   │        │   shell.wasm     mvdan.cc/sh + coreutils      │      │
   │        │                  + stage 08 policy/audit      │      │
   │        │   chNN.wasm      the chapter's own programs   │      │
   │        └──────────────────────────────────────────────┘      │
   └──────────────────────────────────────────────────────────────┘

   build time (offline, never runs in a browser)

     stages/**            web/content/**/levels/*.json
         │                        │
         └────────┬───────────────┘
                  ▼
          web/tools/genlevels     extract → check drift → assemble
                  │                → go build (correct + wrong)
                  ▼
       web/assets/levels/<id>/{level.json, build-table.json}
       web/assets/wasm/{shell.wasm, chNN.wasm, manifest.json}

     web/tools/wasmshell   the shell's own Go source
       + jspipe.py         the four-site patch that makes pipes work
     web/tools/fs-conformance   proves Go's os package works on memfs.js
```

The data flow in one sentence: the interface asks `Runtime` for something, the
worker answers it either from a table written at build time or by running
WebAssembly over a filesystem that lives in the worker, and every answer carries
where it came from.

### Why a worker

Measured: a 2000-iteration shell loop is 198 ms of solid compute. On the main
thread that is dropped frames. A Go program in a tight loop is worse — it never
returns to the JavaScript event loop, so nothing can ask it to stop, and the tab
is gone until the learner closes it.

In a worker the page stays responsive and the runaway can be killed by
terminating the worker. That is the only stop that always works, and it is not
free: everything the worker held goes with it. The filesystem comes back from
its last snapshot, taken after each completed command, so work done by the
killed program is lost — exactly as it is when stage 01 kills a process tree on
timeout, and worth saying in those words in the interface.

### What is *not* used, and why

**SharedArrayBuffer and `Atomics.wait`.** They would allow a synchronous
filesystem shared between threads, which is the tidy version of this design.
They require the `Cross-Origin-Opener-Policy` and `Cross-Origin-Embedder-Policy`
headers, and a static host that cannot set headers — GitHub Pages, for one —
cannot enable them. Depending on them would mean the site works on some hosts
and not others, with no way for a reader to tell which they are on. So the
filesystem lives in the worker and the two synchronous methods in the runtime
interface are served from a mirror the worker pushes. See §9.

---

## 3. Running Go in a browser

### 3.1 What was actually tried

The first thing to establish was whether the repository's own code compiles for
`GOOS=js GOARCH=wasm` at all. It mostly does not:

| stage | `GOOS=js GOARCH=wasm go build` |
|---|---|
| `00-loop` | **builds**, 11,039,866 bytes |
| `01-dont-die` … `05-live-forever` | fails: `cmd.SysProcAttr.Setpgid undefined` |
| `06-the-composer` … `12-echo` | fails: `tui/term`, `undefined: unix.Termios` |

Both failures are the same shape. `proc_unix.go` and `tui/term/term_unix.go`
carry `//go:build !windows`, so `js/wasm` selects them, and they use process
groups and termios, neither of which exists in a browser. This is not a defect
in the repository — it is what "the platform files are real" means — but it does
mean **the browser build is a port, and the port is a build step.**

`stages/` must not be modified, so the port is an `-overlay`: `go build` takes a
JSON file mapping a source path to a replacement, and the repository stays
untouched. Verified — `stages/01-dont-die` builds for js/wasm through an overlay
that swaps `proc_unix.go` for a linkable stub, and `git status` afterwards is
clean.

### 3.2 The decision

Levels do not compile whole stages. A level's runnable artifact is a small
program made of declarations extracted from the repository's real source plus a
harness — see `web/content/SCHEMA.md` — and those compile for js/wasm with no
overlay at all, because the platform-specific files are not in them.

`web/tools/genlevels` compiles, at build time:

- the **correct** program for every level, into one binary per chapter;
- the **wrong** programs, discarding the binaries and keeping what `go build`
  said.

At run time, `Runtime.build(files)` is a lookup. `compiler.js` reads the
level's build table, matches the learner's selection, and returns either an
artifact id or the recorded diagnostics.

### 3.3 The combinatorial problem, and the containment

A level with 6 holes of 4 options each has 4096 combinations. Compiling all of
them is minutes of CPU per level and, worse, thousands of build-table entries.

So `genlevels` has two modes, and the table records which:

- **`full`** — every combination compiled. Used when the product is under a cap
  (default 256). Every answer is a verbatim transcript.
- **`per-hole`** — the correct combination, plus, for each hole and each wrong
  option, that one option wrong and everything else correct. Σ instead of Π: the
  6×4 level costs 19 builds instead of 4096.

**Is per-hole faithful?** For one wrong choice, exactly — that entry *is* the
verbatim build. For two or more it is a composition, and composition is wrong in
both directions:

- *Under-reports.* Two wrong options can produce an error neither produces
  alone. `declared and not used` is the common one: both wrong branches drop the
  only use of a variable.
- *Over-reports.* Go's type checker stops after ten errors in a file, and some
  errors only appear when an earlier one did not mask them. Concatenating two
  single-fault transcripts can show an error the real build would have
  suppressed.

So a composed answer is labelled `composed: true`, and the interface says
"the first problem in each of the N holes you changed", which is true, rather
than "the compiler said", which would not be. A learner with several holes wrong
is being told *which holes* are wrong, which is the useful part, and is not being
shown a transcript that no build ever produced.

### 3.4 Where this breaks

It breaks the moment a learner edits off-script. There is nothing to look up,
and `compiler.js` says so in the diagnostics list rather than inventing an
opinion:

> This exact combination was not compiled when the level was built, so there is
> nothing recorded to show you. Load the Go toolchain to compile it here, or
> return the holes to one of the offered options.

### 3.5 The opt-in toolchain, and why it is not promised

The interface for it exists — `Runtime.status().compiler` is `unavailable` until
something loads, and every diagnostic carries `origin` — but the site does not
promise the feature, because the landscape does not currently support the
promise. What I found:

- **The official Go Playground compiles server-side.** There is no in-browser Go
  compiler from the Go project. ([go.dev/wiki/WebAssembly][gowasm])
- **`static-go-playground`** is the reference implementation of the real thing:
  it builds the Go toolchain to WebAssembly, ships a virtual filesystem and a
  modified `wasm_exec.js`, supports a build cache and cross-compilation, and
  needs no server. It is also **archived — read-only since 5 February 2023 — and
  supports Go 1.13 to 1.18.** This repository needs 1.24. No asset sizes are
  stated anywhere in its README; I have not built it.
  ([github.com/Yeicor/static-go-playground][sgp])
- **Yaegi**, a pure-Go Go *interpreter*, avoids the compiler entirely. One
  published build — v0.16.1, a 30-line wrapper, `stdlib.Symbols` loaded,
  `GOOS=wasip1 GOARCH=wasm` — came out at **38 MB**, because loading the stdlib
  symbol table links wrappers for essentially the whole standard library and the
  linker cannot prune them. *Cited, not measured by me.*
  ([blog.lvmbdv.dev][yaegi-post]) Yaegi also does not support cgo, assembly, or
  compiler directives, and reports `%T` differently from compiled Go — which for
  a course whose subject is Go's own behaviour is a fidelity problem on top of a
  size one. ([pkg.go.dev/…/yaegi/interp][yaegi-doc])

Two figures put that in context. This site's entire shell is **1.19 MB brotli**,
and its whole thirteen-chapter binary set is about **9.3 MB**. A 38 MB
interpreter is four times the course, to run the code slower and with different
`%T` output.

So: the toolchain is a Milestone 3 investigation with a real chance of coming
back "no". If it happens it is fetched lazily, only on request, with
subresource-integrity pinning (see §5.3), and the site is complete without it.
If it does not happen, what is lost is free editing off-script, and the honest
message in `compiler.js` is the whole of the fallback.

[gowasm]: https://go.dev/wiki/WebAssembly
[sgp]: https://github.com/Yeicor/static-go-playground
[yaegi-post]: https://blog.lvmbdv.dev/posts/adding-go-to-a-browser-code-runner/
[yaegi-doc]: https://pkg.go.dev/github.com/traefik/yaegi/interp

### 3.6 The word "compiling"

`status.js` closes the vocabulary. Phases are `matching`, `fetching`,
`starting`, `running` on the recorded path and `checking`, `compiling`,
`linking` on the live one, and `PHASE_LABEL` is the only approved English for
each. The three live phases are unreachable unless a toolchain is loaded.

A progress bar that says "Compiling…" for 40 ms of table lookup is a small lie,
and it is exactly the kind this repository has been bitten by. `originNote()`
gives the interface one sentence to put beside a result:

> Recorded from a real `go build` when this level was built. Choosing options
> replays it; it does not re-run the compiler.

---

## 4. A shell in the browser

Stage 08 embeds `mvdan.cc/sh` and drives it through `interp.ExecHandlers` and
`interp.OpenHandler`. The chapter's point is that those are the only places
where the truth about a command is complete. The browser's point is that they
are also the only two places where a browser differs from a machine: there are
no programs to exec, and there is no disk to open.

Both differences land on a seam the lesson already put there. That is why the
shell on the site is the interpreter the chapter is about rather than a mock of
it.

### 4.1 The filesystem

Go's `js/wasm` port has no filesystem. `syscall/fs_js.go` calls out to a
JavaScript global named `fs` whose API is the callback half of Node's `fs`
module, and Go's own `wasm_exec.js` installs a stub where every method answers
`ENOSYS`. That stub is why `os.ReadFile` fails in a browser.

Replace the global and the whole `os` package starts working. Nothing is
patched and nothing is forked: the seam is the one the Go authors put there.
`web/assets/js/runtime/memfs.js` implements it — the exact call list was read
off the Go source tree, not guessed:

```
constants  O_WRONLY O_RDWR O_CREAT O_TRUNC O_APPEND O_EXCL O_DIRECTORY
calls      open close read write fstat stat lstat readdir mkdir rmdir unlink
           rename truncate ftruncate utimes fsync chmod fchmod chown fchown
           lchown link symlink readlink
sync       writeSync            (the runtime's panic path)
also       process.cwd/chdir/getuid/…, path.resolve
```

Two details in `fs_js.go` are load-bearing:

1. `mapJSError` looks an error's `.code` up in a fixed table and **panics** on a
   code it does not know. A wrong code does not surface as a Go error; it takes
   the wasm instance down. Every failure path in `memfs.js` raises a code from
   that table.
2. `fsCall` buffers its result channel before invoking the JS function, so a
   **synchronous** callback is delivered with no trip through the event loop.
   Node's real `fs` never does that; `memfs.js` does, and it removes one
   microtask from every syscall of every `cat`, `ls` and `find`. This is a
   property of Go's internals rather than a documented promise, which is why
   `web/tools/fs-conformance` tests it against the Go we actually build with.

Persistence is a whole-tree snapshot into IndexedDB, debounced 400 ms and
skipped when nothing changed. IndexedDB rather than localStorage because
localStorage stores strings — a file's bytes would become base64, a third larger
— and because it is synchronous, and the thread it would block is the one
running the shell.

### 4.2 exec, and what the commands really are

`mvdan.cc/sh` implements the shell **builtins**, because they are part of the
language: `echo printf cd pwd test [ read eval source trap getopts shopt alias
set unset shift exit return break continue wait command builtin type exec dirs
pushd popd readarray mapfile`.

It does not implement `cat`, because on a real machine `cat` is a separate
program the shell execs — which is exactly the thing a browser does not have.
So the exec handler dispatches to Go implementations in
`web/tools/wasmshell/coreutils.go`:

```
cat ls grep sed find wc head tail mkdir rmdir rm cp mv touch sort uniq tr
cut rev nl tee seq basename dirname env which sleep date help
```

**They are not GNU's**, and the site says so in the shell, where a learner will
read it at the moment it matters:

| command | how it differs |
|---|---|
| `grep` | Go's `regexp` is RE2: no backreferences, no lookaround; `-E` syntax is the default |
| `sed` | `s`, `d`, `p` and line addresses only. No hold space, no `y`, no `a`/`i`/`c` |
| `find` | `-name -path -type -maxdepth -print`. No `-exec` |
| `sort` | bytewise, plus `-n -r -u -k`. No locale collation, ever |
| `ls -l` | a plausible long format, not GNU's column widths |

Anything else — `python`, `git`, `curl` — gets an honest refusal rather than
bash's "command not found", which would suggest that installing something would
help:

```
python3: not available in the browser shell — there are no external programs
here, only the interpreter and the commands `help` lists
```

### 4.3 The pipe problem

This one was found by running the thing, not by reading about it. The first `|`
in the conformance suite failed with:

```
pipe: not implemented on js
```

`interp` calls `os.Pipe()` in four places — the `|` and `|&` operators,
heredocs, here-strings, and `stdinFile` — and Go's js/wasm port returns `ENOSYS`
from `syscall.Pipe`. Unlike the filesystem there is **no JavaScript seam**: pipes
are not routed through a replaceable global. A shell without pipelines cannot
teach anything about an agent whose entire tool is bash, so this had to be
fixed rather than documented as a limitation.

Checked before patching: `mvdan.cc/sh` v3.13.1, the newest release, has the same
four call sites unchanged, and would raise the module's `go` directive to 1.25.0,
which this repository avoids on purpose. There is no version to upgrade to.

The fix is `io.Pipe` — pure Go, works everywhere. The four sites want "a reader
and a writer joined end to end"; `os.Pipe` is there because a real pipeline may
hand the read end to a child process, and a browser has no child processes.

Applying it is where it gets awkward. `go build -overlay` refuses:

```
go: overlay contains a replacement for …/mvdan.cc/sh/v3@v3.12.0/interp/api.go.
Files beneath GOMODCACHE must not be replaced.
```

So the shell's module runs `go mod vendor` and patches the vendor tree, which is
ordinary files. `web/tools/wasmshell/jspipe.py` does it, textually, with an
asserted occurrence count per substitution: an upstream change makes the build
fail loudly instead of producing a half-patched interpreter that compiles and
then deadlocks. Eight substitutions across three files; `go mod vendor` costs
9.2 MB of source in the build tree, which is generated and need not be
committed.

The patch changes one observable thing. `io.Pipe` is synchronous where `os.Pipe`
has a 64 KB kernel buffer, so a pipeline whose reader never reads blocks at the
first byte instead of the 65537th. Under js/wasm goroutines are cooperatively
scheduled on one thread and a blocked write yields, so it is not a deadlock — and
a shell that hangs hangs either way. It also loses `read`'s deadline-based
cancellation when stdin is a pipe, which is noted in the patch.

### 4.4 Two bugs the port forced into the open

Both are in the "browser has no operator" family and both would have shipped:

**A command with no input never ends.** The interpreter leaves
`HandlerCtx.Stdin` as a nil interface when the shell has no input, so `cat` with
no arguments dereferences nil, panics, kills the wasm instance, and leaves the
page waiting on a promise that never settles. A terminal answers this with
Ctrl-D; a browser has nobody to press it. `cmdenv.in()` returns EOF instead.

**A panic is not a crash the page can see.** Any panic on the goroutine running
a command is an unsettled promise and, worse, a dead instance — every later
command fails with "Go program has already exited". The exec goroutine now
recovers, says the failure is a bug in the site's shell rather than in the
learner's command, and keeps the session alive.

### 4.5 The policy and the audit log

`web/tools/wasmshell/policy.go` carries stage 08's three inspectors — string,
AST, argv — as three settings of one object, so a learner can switch between
them and re-run the same bypass. The audit log keeps every argv, every path the
shell opened, and every refusal with its reason, as ordered lists rather than
counters, because the interface has to answer "which", not "how many".

The conformance suite asserts the chapter's central claim rather than describing
it: `X=.en; eval 'cat ${X}v'` — the bypass corpus's own string, from
`stages/08-sandbox/bypass_test.go` — is **blocked at argv** and **allowed at
AST**, because the value does not exist until `eval` runs.

It also caught a mistake in its own first draft that is worth recording: the
suite wrote `.env` with the policy already enforcing, the open handler correctly
blocked the redirect, and every later assertion was testing an empty file.

### 4.6 What is proven

`node web/tools/fs-conformance/run.mjs` — 50 checks, all passing, against the
real `shell.wasm`:

- the shell language (loops, functions, arithmetic, substitution, `&&`/`||`,
  `printf`, exit codes);
- the filesystem through Go's `os` (`>`, `>>`, `mkdir -p`, `cd` persisting
  across commands, `[ -f ]`, error text for a missing file);
- globbing, which needs `readdir` to be right;
- pipelines of three re-implemented commands over a file the shell wrote;
- a 300-iteration append loop, which exercises the growth path in `memfs`;
- the stage 08 lesson, all three levels, including `cat < .env` being caught by
  the open handler and not by argv;
- host-side reads and writes of the same tree, and a snapshot round-trip.

---

## 5. An LLM from a browser

### 5.1 Replay, the default

The repository writes JSONL event traces and reads them back through the same
`Subscriber` the live agent used. Because the agent core prints nothing, a
recorded event is indistinguishable from a live one, and replay is fifty lines
rather than a second implementation of the interface.

`trace.js` is the JavaScript half of the same bargain. It reads the format
`stages/02-see-everything/trace.go` writes and makes the same two decisions
`ReadTrace` makes, for the same reasons: a final line that stops mid-object is
the normal shape of a killed agent and comes back as a synthetic notice, not an
error; a complete line that does not parse is real damage and is counted
separately. `maxReplayGap` is five seconds here too — everything replay exists to
convey is below it, and everything above it is a person being idle.

**The trace subset a level needs** is in `SCHEMA.md`; the core is
`user_message turn_start request first_token text_delta reasoning_delta
tool_call_start tool_args_delta tool_call_ready gate_verdict command_start
command_end tool_result usage response_end turn_end notice error`, and stage 08
adds `sandbox_exec sandbox_open sandbox_block`. A level names its trace, an
optional `focus` range of `seq` values, and a `kinds` filter. The header always
summarises the *whole* trace, so a filtered view cannot be mistaken for the
session.

Binding is checked at build time: the trace must parse, its `seq` range must
cover the level's `focus`, and it must come from the stage the level names. A
trace from another stage, replayed under this level's commentary, would be
describing a session that did not happen.

### 5.2 Live, opt-in

Four fields, the same four as `/provider-url`, `/provider-protocol`,
`/provider-model` and `/provider-apikey`: endpoint, protocol (`openai` |
`anthropic`), model, key. Stage 03's claim is that a local Ollama and a frontier
API are the same four fields, and it holds here.

### 5.3 Threat model

The repository's own config file never stores a key. `api_key_env` names an
environment variable, and the comment says why: *a config file gets committed
eventually, every one of them does, and the only reliable defence is for the
secret to have nowhere to sit in the file at all.*

A browser has no environment. The key has to sit somewhere, and every option is
worse:

| where | survives | who can read it |
|---|---|---|
| memory (default) | nothing | script on this origin |
| `sessionStorage` (opt-in) | reload | script on this origin |
| `localStorage` (opt-in) | everything, including being forgotten | script on this origin, ever; extensions with storage access; anyone on a shared machine |

What is never done, in any mode: **the key does not go to this site — there is no
server — and it does not go through any proxy, ours or anyone else's.** It is
sent by the learner's browser directly to the endpoint the learner typed. A
design that proxied a key to make CORS work would be asking a learner to hand a
credential to a third party to solve a browser configuration problem, and no
amount of convenience is worth teaching that.

Assets:
- the API key, in one of the three places above;
- the conversation, which goes to the chosen endpoint;
- the filesystem in IndexedDB, which is local and unencrypted.

Adversaries:
- **Cross-site script on this origin.** The whole defence is a strict CSP, no
  third-party scripts, and no `eval` of remote content — with one exception,
  named because it matters: `gohost.js` evaluates the *vendored* `wasm_exec.js`,
  which is same-origin, shipped with the site, and version-pinned. A CDN-loaded
  toolchain (§3.5) must be subresource-integrity pinned or it becomes a second
  exception and a real hole.
- **A malicious endpoint.** The learner typed the URL. Model output is rendered
  as text and never as HTML, and never executed. What it *can* do is write shell
  commands the learner runs — which is the subject of the course, and which is
  what the sandbox panel is for.
- **A shared machine.** Answered only by the memory default and by saying what
  `localStorage` means in the words above.

Not in the model: the wasm sandbox itself. The learner's own code runs in their
own tab, over a filesystem that exists only in that tab.

### 5.4 CORS, stated plainly

A browser will not let this page read a response from an endpoint that does not
send `Access-Control-Allow-Origin`, and the major hosted providers do not,
because browser-side API keys are what they are discouraging.

Live mode works with a local model server started with CORS enabled (Ollama,
llama.cpp, vLLM, LM Studio), or a gateway the learner runs. It does not work by
typing a frontier vendor's URL into it.

`LLM.preflight()` reports this before a key is spent finding out, and it does
not guess: a CORS refusal and a dead endpoint are **the same opaque `TypeError`**
from JavaScript, with no status and no body, so the message names both.

---

## 6. Build time versus browser time

| build time (offline) | browser time |
|---|---|
| extract declarations from `stages/**` | look a selection up in the build table |
| check every hole's anchor still matches | fetch and instantiate a chapter binary |
| check correct options reproduce the repo byte for byte | run it over the in-memory filesystem |
| `go build` the correct program per chapter | run the shell |
| `go build` the wrong programs, keep the diagnostics | replay a trace |
| run the correct program, record its output as `expect` | (opt-in) fetch a toolchain and really compile |
| `go mod vendor` + patch `mvdan.cc/sh`, build `shell.wasm` | |
| compute chapter diffs from the real files | |

Nothing in the left column runs in a browser, and nothing in the right column
needs a server.

---

## 7. Budgets

Measured on this machine: Go 1.26.3, Node 24.15.0 (V8), Windows 11. `gzip` is
`gzip -9`; `brotli` is quality 11 via Node's zlib. Browser figures where noted
are V8 under Node and are an **estimate** for a browser, which runs the same
engine but not the same host.

### Sizes

| artifact | raw | gzip | brotli |
|---|---:|---:|---:|
| Go js/wasm floor (`fmt` hello world) | 2,632,683 | 757,822 | 572,007 |
| …with `-ldflags="-s -w"` | 2,582,098 | 740,948 | — |
| a realistic level program (`regexp fmt os strings utf8`) | 3,339,537 | 947,677 | 707,033 |
| the same with **five** level variants in one binary | 3,355,858 | 951,516 | 713,748 |
| `shell.wasm` (mvdan.cc/sh + coreutils + policy), as shipped with `-s -w` | 5,892,396 | 1,605,756 | 1,189,562 |
| `stages/00-loop` whole agent, with `net/http` | 11,039,866 | 2,912,846 | 2,140,752 |
| `wasm_exec.js` (vendored, uncompressed) | 16,992 | — | — |

Two of those decide the design.

**The marginal cost of a level is nothing.** Four extra level variants added
16,321 bytes raw and 6,715 brotli to a binary. The fixed cost is the Go runtime
and the standard library, and it is paid once. So: **one binary per chapter, not
per level.** Thirteen chapters at ~715 KB brotli is about **9.3 MB** for the whole
course, fetched one chapter at a time.

**`net/http` costs 1.93 MB gzipped.** Measured directly: the same program with
and without a `http.Post` call was 3,915,485 → 11,441,956 raw, 1,078,328 →
3,008,167 gzip. That is `crypto/tls` and it is the single largest lever in the
whole design — and it is exactly what the replay-first decision removes. A level
that needs the agent's model call replays a trace instead of linking a client.

### Load and run

| | measured |
|---|---:|
| `WebAssembly.compile(shell.wasm)` | 19 ms |
| instantiate + shell ready | 21 ms |
| cold start to a usable prompt | **43 ms** |
| `echo hi` | 0.13 ms |
| `echo x > f; cat f \| wc -l` | 1.14 ms |
| 2000-iteration append loop | 198 ms |
| `grep -c` over 2000 lines | 2.9 ms |

The compile figure is why `gohost.js` caches `WebAssembly.Module`: re-running a
program a learner is iterating on costs the instantiate, not the compile.

### Targets

| | target | where it stands |
|---|---|---|
| first paint, no runtime | < 200 ms | UI's; the runtime loads nothing until asked |
| terminal usable | < 1.5 s on a warm cache | 1.19 MB brotli + 43 ms — network-bound |
| level runnable | < 2 s | ~715 KB brotli per chapter, cached after the first level |
| whole course | < 12 MB | ~9.3 MB of chapter binaries + 1.21 MB shell |
| a level's non-wasm assets | < 200 KB | JSON and a trace; traces in `.work/` run 10 KB–2.8 MB, so large ones need trimming to their `focus` range at build time |

---

## 8. What breaks

| failure | symptom | fallback |
|---|---|---|
| no `WebAssembly` | shell and run unavailable | prose, diffs, quizzes, and replayed sessions all still work; `status()` says `unavailable` and the interface says why |
| no ES module workers (older Safari) | worker fails to construct | same; this is the one that costs the most and the only fix is a bundled classic-worker build, which is a build step the site currently does not have |
| `wasm_exec.js` / binary version mismatch | a crash inside `runtime.wasmExit`, or a silent hang | refused up front: `manifest.json` records the toolchain, `assertVersion` compares, and the error names the cause. This check exists because the failure it prevents is undiagnosable from the symptom |
| IndexedDB blocked (private window, quota) | files do not survive a reload | the session works; a `fault` event says persistence is off, once |
| CDN unreachable | no opt-in toolchain, no web fonts | everything else works: all runtime assets are same-origin. The learner loses free editing and gets system fonts |
| `mvdan.cc/sh` changes upstream | — | `jspipe.py` fails the build with the count that no longer matches |
| a stage's source changes | — | `genlevels` fails: the hole's anchor no longer matches, or the correct options no longer reproduce the file |
| runaway program | tab would freeze | the worker is terminated and rebooted; the filesystem returns to its last snapshot and the interface says so |
| a Go panic in a command | promise never settles, instance dead | recovered on the exec goroutine, reported as a bug in the site's shell |
| `Runtime.shell.cwd()` read mid-command | one command stale | it is a mirror; correct between commands, which is when a prompt is drawn |

The firewalled learner — no CDN of any kind — gets: all reading, all diffs, all
quizzes, all replayed sessions, the full shell, and every level's run. They lose
the opt-in Go toolchain and any CDN font. That is the whole list, because
everything else is same-origin static files.

---

## 9. Two places the agreed interface is awkward

Implemented as agreed, because a silent divergence costs more than a wrong
signature. Both are noted here and in the code.

**`Runtime.shell.cwd()` is synchronous** and the shell is in a worker. It is
served from a mirror the worker pushes on every `fs.changed`. Correct between
commands; up to one command stale if read mid-flight. The fix is an async
`cwd()`.

**`Runtime.fs.list()` is synchronous** while `read` and `write` are async. Same
mirror. It is affordable only because a level's tree is tens of entries — if a
level ever seeds thousands of files, this becomes a real cost and the method has
to change.

One addition rather than a change: **every diagnostic and every build result
carries `origin`**, and build results carry `composed`. Without them the
interface cannot keep the promise in §3.6, and it seemed better to add a field
than to leave the interface unable to tell the truth.

---

## 10. Delivery

### Milestone 1 — one chapter, end to end

Worth showing when a reader can open chapter 2, fill three holes, press Run, see
real output, open a terminal, and replay a real session. Concretely:

- `shell.wasm` built and served, with `manifest.json` and the version check
  — **done and measured**;
- `memfs.js` + `persist.js` — **done**, 50/50 conformance;
- `api.js` / `worker.js` / `protocol.js` / `status.js` — **done**, against the
  agreed interface;
- `compiler.js` reading a build table — **done**; the table is written by
  `genlevels`, which is **specified, not built**;
- `genlevels`: extract → anchor check → drift check → assemble → build → record.
  This is the remaining build-time work and it is the critical path;
- one chapter's levels authored in the `SCHEMA.md` format, with one trace
  trimmed to its focus range;
- `trace.js` replay wired to the transcript view — **done** on the runtime side.

### Milestone 2 — the course

Thirteen chapters generated, chapter binaries deduplicated, quizzes, the skip and
reveal path fed by real `git diff` output, and a size budget enforced in CI by
the same script that builds the assets.

### Milestone 3 — off-script

Live LLM mode with the preflight and the storage choice; the opt-in Go toolchain
if and only if a candidate measures well enough to promise; `gofmt` as
`go/format` compiled to wasm, which is the cheapest of the three because it needs
`go/parser` and `go/printer` and not the compiler.

### The riskiest thing

Not the shell, which is measured and passing. It is `genlevels`, and specifically
the **drift check**: the claim that a level's correct options reproduce the
repository's source byte for byte is the only thing standing between this course
and the usual fate of a course about a codebase. Until that check runs in CI on
every commit to `stages/`, the site is one refactor away from teaching something
that is no longer true. Building it and wiring it into CI is what retires the
risk, and it is Milestone 1's critical path for that reason and not for schedule.
