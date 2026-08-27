# Stage 01 — Don't Die

Stage 00 works right up until it doesn't. This chapter is four specific ways it
dies, and what each one costs to fix.

**Do the reproductions first.** Every fix below is obvious once you've read it,
and none of them are obvious before. Fifteen minutes breaking stage 00 buys you
the intuition that makes stage 01 read as inevitable rather than arbitrary.

```sh
go build -o agent00 ./stages/00-loop
go build -o agent01 ./stages/01-dont-die
mkdir -p sandbox && cd sandbox
```

---

## Death 1 — the command that never returns

**Reproduce** (stage 00):

```
> start a simple http server on port 8000 in the foreground
```

The agent runs `python -m http.server 8000` and never comes back. No timeout, no
Ctrl-C handling worth the name, no way out but killing the terminal.

**The obvious fix, and why it is not enough.** Add a timeout, kill `cmd.Process`
when it fires. Now try:

```
> run: (sleep 300 &) ; echo started
```

The shell exits immediately. `echo started` printed. And the agent still hangs
forever.

This is the part that surprises people. `cmd.Wait()` does not wait for the
process — it waits for the *stdout pipe to close*, and the pipe stays open as
long as any process holds the write end. The backgrounded `sleep` inherited that
handle. So killing only the shell doesn't merely leak an orphan: **it hangs the
very timeout that was supposed to rescue you.**

**The real fix** is to make the shell and everything it spawns killable as a
unit:

- **Unix** — `SysProcAttr{Setpgid: true}` puts the shell in a new process group,
  and `kill(-pgid)` signals the whole group.
- **Windows** — a Job Object; every descendant is assigned to it, and
  `TerminateJobObject` ends all of them at once.

That's `proc_unix.go` and `proc_windows.go`, behind one small interface so
`runBash` never mentions a platform:

```go
g, _ := newProcGroup()
defer g.Close()
g.attach(cmd)          // before Start: process group / job creation flags
cmd.Start()
g.adopt(cmd)           // after Start: assign to the job (Windows)
...
g.kill()               // takes the whole tree down
```

**The lesson that generalises:** when a cleanup path depends on the thing you
are cleaning up, it isn't a cleanup path. Check every "and then we kill it" for
this shape.

**And then apply it to the fix itself.** `g.kill()` is supposed to unblock
`Wait()` — but "supposed to" is the same assumption we just watched fail. So the
reap gets its own five-second deadline, and if that expires the agent reports
`[TIMED OUT and could not be reaped]` and moves on.

Giving up there leaks the `Wait` goroutine, which holds the output buffers, so
the code must also **refuse to read those buffers** — the copying goroutine may
still be writing into them, and reading them anyway is a data race that will
show up as garbled context weeks later. Leaking one goroutine is survivable;
wedging the agent is not; racing on a buffer is the worst of the three because
it fails quietly.

### From a real run

The exact reproduction, through the finished agent, with a five-second timeout:

```
> run exactly this: (sleep 300 &) ; echo started ; sleep 300

  $ (sleep 300 &) ; echo started ; sleep 300
  | started
  | [TIMED OUT after 5.046s — the process tree was killed]
```

Four things to notice, all of them design decisions paying off:

- **It came back.** Eighteen seconds wall-clock for the whole exchange, five of
  them the timeout. Stage 00 would still be running.
- **`started` survived.** Output produced before the kill is still captured and
  still shown. A timeout is not a reason to throw away what you learned.
- **The model was told, in words, what happened** — and its summary correctly
  explained that the backgrounded sleep died with the tree. The status line is
  written for a reader, because it has one.
- **Zero orphans.** `ps -W | grep -c sleep` reads 0 before and 0 after. That is
  the number the whole section exists to produce.

### Why Unix is airtight here and Windows is not

Worth internalising, because it explains the shape of the code:

- **Unix** sets the process group in the gap between `fork()` and `exec()` — the
  child is in its own group **before its first instruction runs**. Nothing can
  escape, because nothing has executed.
- **Windows** cannot assign a Job Object to a process that does not exist yet.
  So `adopt()` runs *after* `Start()`, and there is a microsecond window in
  which a grandchild spawned by the shell's very first act would escape the job.

The airtight Windows fix is `CREATE_SUSPENDED` + `ResumeThread`, which `os/exec`
makes awkward on purpose (a suspended process Go's reaper never resumes would
hang `Wait()`). This repo takes the window and **documents it in the code**
rather than pretending it isn't there. If you are writing a sandbox rather than a
teaching repo, drive `CreateProcess` directly, where the flag and the thread
handle are both in `PROCESS_INFORMATION`.

### Platform caveats you will actually hit

| Caveat | Why it matters |
|---|---|
| **`Close()` is asymmetric** | Windows `KILL_ON_JOB_CLOSE` means closing the handle *kills* the tree — a genuine crash-safety net. Unix has none: if the agent dies, its children live on under init. So `nohup npm start &` survives a tool call on Linux/macOS and does **not** on Windows. |
| **Nested jobs need Win8+** | Some CI runners and container hosts already put every process in a locked-down job; `AssignProcessToJobObject` then fails with `ERROR_ACCESS_DENIED`. `runBash` degrades to a warning — the command still runs, containment is lost — instead of refusing to work. |
| **Zombies look alive** | `kill(pid, 0)` succeeds for an unreaped zombie. Normally init reaps instantly; in a container whose PID 1 doesn't reap, they linger. Poll, don't check once. |
| **PID recycling is *not* a risk here** | `os.Process` holds an open handle from `Start()` to `Wait()`, and Windows will not recycle a PID while a handle is open. |

### A trap that only exists on Windows: `$!` lies

Found while writing the test for this chapter, and worth a paragraph because it
would have silently invalidated the whole thing.

Git Bash is MSYS2, which maintains **its own POSIX PID namespace layered over
Windows PIDs**. `echo $!` prints the MSYS pid, not the Windows one:

```
msys_pid=48908                                <- what $! prints
48908 48907 48905  56176 ... /usr/bin/sleep   <- ps -W: the real WINPID is 56176
```

Hand 48908 to `OpenProcess` and it does not error — it cheerfully queries
whatever unrelated Windows process happens to own that number. A *test* built on
`$!` appears to pass while proving nothing. A *killer* built on it terminates a
bystander.

The translation is available at `/proc/<pid>/winpid`, so the fixture uses
`cat /proc/$p/winpid 2>/dev/null || echo $p` — MSYS2 answers, real Unix has no
such file and falls back to the pid that was already correct.

The general lesson is bigger than Windows: **when your test and your
implementation share an assumption, the test cannot detect that the assumption
is wrong.** Which is why the test suite here also contains a deliberate
demonstration of the failure mode (`TestProcGroupKillingOnlyTheShellLeavesOrphans`),
and why the implementation was mutation-checked — `TerminateJobObject` replaced
with a no-op — to confirm the test actually fails when the code is broken:

```
proc_test.go:209: orphans survived kill(): [18592 36592] — the process tree escaped
--- FAIL: TestProcGroupKillsWholeTree (5.22s)
```

A green test you have never seen fail is not evidence.

---

## Death 2 — the command that prints 40MB

**Reproduce** (stage 00):

```
> how many files are on this machine?
```

The model tries `find / -type f | wc -l` — fine. Now watch it try
`find / -type f` when it wants to see *names*. Stage 00 pushes every byte into
the message array, and it stays there for the rest of the session, re-sent and
re-billed on every subsequent turn.

**Fix: truncate, but not from the front.** Head-only truncation is the reflex
and it is the wrong reflex — the useful part of a failing build is the *last*
twenty lines. Keep both ends, drop the middle, and say how much you dropped:

```
<first 2/3 of the budget>

[... 1481923 bytes elided ...]

<last 1/3 of the budget>
[exit 0 · 3.2s] [output truncated — rerun with a filter such as grep/head/tail]
```

Three details in `truncate()` that matter more than they look:

- **Cut on rune boundaries.** Slicing a byte array mid-character produces
  invalid UTF-8, which some APIs reject outright and others turn into mojibake
  in the model's context.
- **Say the byte count.** "Something was removed" is much less useful to the
  model than "1.4MB was removed" — the latter tells it the command was simply
  the wrong shape.
- **Tell it what to do instead.** The suffix naming `grep`/`head` measurably
  reduces the number of times the model retries the same dump.

**Budget split.** stdout gets ⅔ of the budget, stderr ⅓. A build that fails
prints a little to stdout and a lot to stderr; a listing does the opposite.
Splitting the budget means neither starves the other.

### From a real run

A 275KB log whose *last line* is the only one that matters:

```
> cat big.log and tell me what went wrong at the end

  $ tail -100 big.log
  | [... 1503 bytes elided ...]
  | NFO  worker-008 processed batch 003976 in 396ms
  | 2026-08-27T02:00:00 ERROR worker-042 FATAL: disk quota exceeded, aborting
  | [exit 0 · 161ms] [output truncated — rerun with a filter such as grep/head/tail]
```

Read that carefully, because it is the whole argument for head+tail in one
screen: the model asked for a hundred lines, got truncated anyway, **and the
FATAL line survived** because it was at the tail. Head-only truncation would
have delivered 5KB of routine INFO lines and dropped the single line the user
asked about — and the model would have answered confidently from what it was
given.

Note `NFO  worker-008` on the line above it: that is the tail resuming
mid-line, which is ugly and completely harmless. Do not spend code on making
truncation pretty.

Two more things that run showed:

- The system prompt's *"prefer commands that filter over commands that dump"*
  did real work — the model reached for `tail -100`, never `cat`. Cheap
  instructions beat expensive machinery.
- After the truncation notice, it followed up with `tail -20` **and**
  `grep -c 'error\|fatal'` **in a single assistant message** — two `tool_calls`
  at once. Parallel tool calls are not hypothetical on this provider, which is
  exactly why "every call gets a result, always" is a rule and not a
  nicety.

---

## Death 3 — the model gets cut off

Stage 00 asks one question about each response: *were there tool calls?* That
one question hides an entire class of failure.

**Reproduce**: send a request with a tiny `max_tokens` and a task that needs a
long command. Here is what actually came back, from two protocols on the same
gateway.

**The OpenAI side is the honest failure.** `max_tokens: 24`:

```json
{"finish_reason": "length",
 "message": {"content": null, "tool_calls": null,
             "reasoning_content": "The user wants to search for Go files containing the word \"deadline\" in the"}}
```

Cut off during reasoning, so no tool call was ever emitted. Note the shape of
the lie stage 00 would tell: it sees no tool calls, concludes "the turn is
finished", prints an empty message and waits for you. Nothing crashed. Nothing
was reported. The task simply stopped.

**The Anthropic side is the dangerous one.** `max_tokens: 10`:

```json
{"stop_reason": "tool_use",
 "usage": {"output_tokens": 136},
 "content": [{"type": "tool_use", "name": "bash", "input": {"raw_arguments": ""}}]}
```

Three separate things are wrong in five lines:

1. **`stop_reason` says `tool_use`.** Not `max_tokens`. The envelope claims this
   is a normal, usable tool call.
2. **`max_tokens` was not honoured.** Ten were requested; 136 were generated.
   On this gateway a small `max_tokens` is not a cost cap, and you should not
   plan a budget around it.
3. **`input` is not the schema you published.** The required `command` key is
   absent; a non-spec `raw_arguments` key holds an empty string instead.

### The bug this produces in Go, which does not look like a bug

```go
var args struct{ Command string `json:"command"` }
json.Unmarshal([]byte(`{"raw_arguments":""}`), &args)  // err == nil
args.Command                                           // ""
```

The unmarshal **succeeds**. Go fills absent keys with the zero value, so "the
model omitted a required field" and "the model sent an empty string" are the
same value, and the agent runs an empty command believing it was asked to.

The fix is one character wide:

```go
var args struct{ Command *string `json:"command"` }   // pointer, not value
```

`nil` now means absent, `""` means empty, and both get rejected. That is
`parseBashArgs` in `main.go`, and `render_test.go` feeds it the six payloads
this gateway was actually observed to produce.

**Two rules to take away, both bigger than this bug:**

- **Unmarshalling without an error is not validation.** Validate against the
  schema you published, every time, on every protocol.
- **The envelope is not evidence about its contents.** `stop_reason` is
  generated by the same system that produced the malformed body. When the two
  disagree, the body is the one that will run on your machine.

**Half a shell command is not a safer shell command.** Stage 01 refuses to
execute anything from a `length`-terminated response and tells the model why, in
a tool result, so it can retry with something shorter:

| `finish_reason` | Meaning | What stage 01 does |
|---|---|---|
| `tool_calls` / `tool_use` | Normal tool turn | Execute |
| `stop` / `end_turn` | Model finished talking | End the turn — but if tool calls are present anyway, trust the calls, not the label |
| `length` / `max_tokens` | Cut off mid-generation | Do **not** execute. Answer every pending call with an explanation and let it retry |
| `content_filter` | Provider blocked it | Report and end the turn |
| anything else | New or vendor-specific | Report the literal string and end the turn. Never silently treat an unknown state as success |

That last row is a habit worth keeping: a state machine that maps unknown inputs
to "probably fine" will eventually map a refusal, a quota event, or a new safety
stop to "probably fine".

---

## Death 4 — the command you didn't want

There is nothing in stage 00 between the model and `rm -rf`. The fix is a gate,
and the interesting part is not the prompt — it's what a **denial** is.

```go
case deny:
    msgs = append(msgs, toolResult(call.ID,
        "[the user denied this command. Do not retry it unchanged.]"))
    continue
```

A denial is **data, not an error**. It goes back as a tool result, the turn
continues, and the model gets to propose something narrower. Treating refusal as
a fatal error kills the agent at the exact moment a human was engaged enough to
be watching — which is the worst possible moment to lose the thread.

Modes: `y` once, `a` for the session, `n` deny-and-continue, `q` stop. `--yolo`
skips the gate entirely. If stdin is a pipe there is nobody to ask, so the gate
detects that up front and says so instead of silently denying everything.

### From a real run

Piping a task in with no `--yolo`, so every command is refused:

```
> list the files here
  $ ls -la
  [denied: no terminal to ask on — rerun with --yolo to allow commands]
  $ ls
  [denied: no terminal to ask on — rerun with --yolo to allow commands]

It looks like both `ls -la` and `ls` were denied. Could you let me know which
command or approach you'd prefer me to use to list the files?
```

That is the behaviour the design is buying. The agent got refused, **narrowed
its proposal** (`ls -la` → `ls`), got refused again, and then asked a sensible
question — all inside the same turn, because a denial was a tool result rather
than a fatal error. Return `error` from that path instead and you get a stack
trace and a dead session.

### The honest argument against "bash is all you need"

Look at what the gate is able to show you: a command string. That's all it has.

A dedicated `write_file` tool could render a **diff**. A dedicated `send_email`
tool could show you the **recipient**. A dedicated `edit` tool could refuse a
write when the file changed since the model last read it — an invariant bash
cannot express at all. And read-only tools like `grep` could be marked
parallel-safe, while `bash -c "..."` has the same opaque shape whether it is
`grep` or `git push`, so the harness must serialise everything.

This is the real trade. One tool buys breadth and buys it cheaply. Dedicated
tools buy the harness the ability to **gate, render, audit, and parallelise** —
and you pay for that breadth every time you want to ask the user a good
question. The rest of this repo stays on one tool because the instrumentation is
the subject; a product would promote three or four actions and keep bash as the
escape hatch.

---

## The sanitising trio

Command output is not text yet. Three separate problems that all present as
"weird characters":

| Problem | Symptom | Fix |
|---|---|---|
| ANSI escapes | `[0;32m` litter in context; wasted tokens | strip with a regex |
| CRLF | invisible `\r` on every Windows line | normalise to `\n` |
| Invalid UTF-8 | a native program writing in the local code page (GBK on a Chinese Windows, Shift-JIS on a Japanese one) | replace invalid bytes with U+FFFD |

The third one is worth dwelling on if you're on a non-English Windows: the bytes
are not corrupt, they're *correct in a different encoding*. Replacing them makes
the failure **visible** rather than silent — the model sees `����` and knows
something went wrong, instead of confidently reasoning about mojibake. Real
transcoding is `golang.org/x/text/encoding`, deliberately not a dependency here;
the `chcp 65001` / `PYTHONIOENCODING=utf-8` route fixes it at the source.

---

## What did it cost?

The whole chapter is about four failures, and the code that fixes them is
smaller than the code that explains them. That ratio is normal for harness work
and it is why harness work is undervalued: none of this makes the agent smarter,
and all of it is the difference between a demo and a tool.

Still missing after this chapter, on purpose:

- You still stare at a blank terminal for the whole model turn (**stage 02**).
- You still can't see the token bill in any useful form (**stage 02**, then
  **04**).
- There's still no record of what happened (**stage 02**).
- The history still grows forever (**stage 05**).

---

## Exercises

1. **Reproduce the pipe hang.** Time out only `cmd.Process`, not the tree, then
   run `(sleep 300 &) ; echo hi`. Watch `cmd.Wait()` block anyway. This is the
   single most valuable ten minutes in the chapter.
2. **Truncate from the head only** and give it a failing build. Notice that the
   error message — the only part that mattered — is exactly what got dropped.
3. **Delete the "do not retry it unchanged" sentence** from the denial text and
   deny something. Watch how many times the model proposes the identical
   command. Tool-result wording is prompt engineering.
4. **Set `--timeout 1s`** and ask for something slow. Confirm the model reads the
   `[TIMED OUT]` line and adapts rather than repeating itself.
5. **Run it with stdin piped** and no `--yolo`. Confirm you get a clear message
   rather than a silent wall of denials.

→ Next: [Stage 02 — See Everything](02-see-everything.md)

→ Reference: [Wire notes](wire-notes.md) — the observed behaviour every claim in this chapter rests on
