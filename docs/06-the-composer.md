# Stage 06 — The Composer

A trace holds two different stories, and every tool you have used shows you only
the first.

```
GOD     what happened.  Every event, with its timings, tokens, exit codes and
        gate verdicts — including the things that were never sent to the model.

MODEL   what the model saw.  Not a reconstruction: the actual bytes, decoded out
        of the request event stage 02 has been recording since it existed.

WIRE    those bytes, unmodified, for when the answer is in the punctuation.
```

Building all three is the point. **The gap between the first two is where agent
bugs live**, and you cannot see a gap with one view.

```sh
go build -o agent ./stages/06-the-composer
./agent --composer session.jsonl        # no key, no network, no provider
```

---

## The line the whole chapter is for

Open the Model view on any call after a compaction:

```
  call 12 of 24   openai · mimo-v2.5 · max_tokens 4096 · 16.4kB
  629 events happened so far · the model can see 11 messages · 0 cache marks · tools: bash
  ⚠ 1 compaction(s) happened before this call: everything below is what SURVIVED, not what happened
```

**629 events happened. The model can see eleven messages.** Before a compaction
those two numbers rise together. After one they part company permanently, and
the divergence never closes.

Every "the agent forgot what I told it" bug is that line. So is every "it keeps
redoing work it already did", and every "it contradicted itself". They are all
the same bug — *the thing you are asking about is not in the model's context* —
and a chat log cannot show it to you, because a chat log renders the God view
and calls it the conversation.

Four more differences the two views expose, all of them normal, all of them
invisible from one side:

- The model reasoned for four hundred tokens and **none of it is in the next
  request**, because thinking is dropped from the history (stage 03).
- The user typed nine words and the model received nine words **plus an
  environment block** it never mentions (stage 05).
- A command printed 40kB and the model was handed 8kB **with a truncation
  marker** (stage 01).
- The `cache breakpoint` markers sit on two specific blocks, and after a
  compaction **the rolling one is somewhere else entirely** (stage 04).

---

## What a TUI actually is

Strip the frameworks away and it is three functions and a `select`:

```
bytes → key           decoding what the terminal sent          keys.go
state + key → state   what that means                          tui.go
state → lines         what it should look like                 views.go
```

```go
for {
    select {
    case chunk := <-in:      // keyboard
    case <-escTimer:         // the Escape ambiguity, below
    case <-t.resize:         // the window changed
    }
    c.draw(t)
}
```

That loop is thirty lines. Everything difficult lives in the three files around
it — the terminal has to be given back, the keyboard speaks a language with an
ambiguity in it, and a column is not a byte. A framework hides all three, which
is fine right up until one of them misbehaves and you have no idea which.

Dependencies for the whole thing: the standard library, plus `golang.org/x/sys`
for three ioctls on Unix and five console calls on Windows. Same as every other
stage.

---

## The contract you take on in raw mode

A TUI needs four things from the terminal, and every one of them is a **global
mutation of a resource you do not own**:

| | why |
|---|---|
| raw mode | keys arrive as bytes; Ctrl-C stops being a signal |
| alternate screen | the user's scrollback is set aside and handed back intact |
| mouse reporting | clicks and the wheel arrive as escape sequences |
| bracketed paste | pasted text arrives wrapped, so it is not executed as keys |

Turning them on is four `printf`s. Turning them off is the entire problem: the
process that turned them on is the only thing in the world that knows how. If it
dies without doing so, the user is left at a shell with no echo, no line
editing, no cursor and broken mouse selection. They will type `reset` if they
know to. Most people close the window.

So this is stage 01's lesson pointed at a different resource. Four exits, and a
real TUI meets all four:

```go
fn returns          the defer runs
fn returns an error the defer runs, and the error prints AFTER the restore —
                    on the user's real screen, not on an alternate screen that
                    is about to be discarded
fn panics           the defer runs, then the panic is re-raised, so the stack
                    trace lands on a terminal that can display it
SIGINT / SIGTERM    the handler restores, resets itself to the default, and
                    re-sends the signal to its own process
```

That last one is deliberate rather than `os.Exit(130)`. A process killed by
SIGTERM should *report* that it was killed by SIGTERM — its parent may be a
shell, a supervisor, or a test harness that distinguishes signal death from a
non-zero exit. Clean up without lying about how you died.

And one rule that quietly invalidates a habit which is correct everywhere else:

> **Once you have entered raw mode, `os.Exit` and `log.Fatal` are bugs.**

They skip deferred functions. A `log.Fatalf("bad config")` three layers down —
the most ordinary line in Go — now leaves the terminal broken *and* prints its
message onto an alternate screen the user will never see.

---

## The Escape key is genuinely ambiguous

A lone `\x1b` at the end of the input buffer is either the Escape key, or the
first byte of a sequence still arriving. **No decoder can tell from the bytes.**

So the decoder does not try:

```go
decodeKey(buf)        // lone ESC → ok=false: "I need more bytes"
decodeKeyFinal(buf)   // called after a timeout produced nothing → keyEsc
```

The policy — how long to wait — belongs to the event loop, which has a clock,
and not to the decoder, which does not and should not:

```go
if len(buf) > 0 {
    escTimer = time.After(50 * time.Millisecond)
} else {
    escTimer = nil            // a nil channel blocks forever = disarmed
}
```

That one line arms and disarms the timer, and it is the whole mechanism.

Two things worth taking away. **This is why Escape feels very slightly late in
every terminal application you have ever used** — including vim, and it is not a
bug in any of them. And **the decoder is testable precisely because it has no
clock**: a function that decided the timeout itself could only be tested by
waiting.

The same discipline covers the rest of the input language. Arrows arrive as
`\x1b[A` *or* `\x1bOA` depending on whether the terminal is in application
cursor mode — a decoder that only knows the first works until someone runs it
inside `tmux`. Home and End arrive in **eight** different forms. A bracketed
paste that is cut in half must report "incomplete", not deliver half a paste.
Mouse coordinates use SGR encoding because the legacy one packs the column into
`32 + n` and cannot name a column past 223 — which on a wide terminal is not an
edge case, it is the right-hand half of the screen.

---

## A column is not a byte, and it is not a rune either

```go
len("你好世界")                    // 12   bytes
utf8.RuneCountInString("你好世界")  //  4   runes
dispWidth("你好世界")               //  8   columns   ← the only one a terminal cares about
```

`%-20s` aligns on bytes. Point it at a table of filenames and the first Chinese
one shears the whole column. Then:

- combining marks are **0** columns — `"é"` is 3 bytes, 2 runes, 1 column
- fullwidth forms are **2** — `"ＡＢ"` is 4 columns
- ANSI escapes are **0**, so `dispWidth("\x1b[31mred\x1b[0m")` is 3

Three consequences the code has to handle, each of which is a corrupted frame if
you get it wrong:

**Truncation must not split a wide character.** With one column left and a
2-column rune next, stop *before* it and pad the orphan column with a space. Half
a CJK glyph is not a rendering artefact, it is a byte sequence the terminal
cannot interpret.

**Truncation must not split an escape sequence** — and if an SGR was still open
at the cut, the result has to close it. Otherwise the colour leaks into
everything drawn after it, for the rest of the session.

**A line that overflows by one column wraps**, which pushes every line below it
down by one and corrupts the entire frame. One cosmetic mistake, one broken
screen. That is why `frameBytes` calls `truncCols` and not `s[:w]`.

Honestly stated, because pretending otherwise is worse than the limitation:
`width.go` measures ZWJ emoji sequences too wide. `👨‍👩‍👧‍👦` measures 8 and draws 2.
Fixing it properly needs grapheme-cluster segmentation (UAX #29), which is a
real dependency, and the symptom without it — one user, ragged borders, a week
later — is otherwise completely inscrutable.

---

## Two platforms, and one asymmetry that changes the design

The same shape as stage 01's `proc_unix.go` / `proc_windows.go`: identical
contract, entirely different mechanism.

| | Unix | Windows |
|---|---|---|
| settings | one `termios` struct | two console-mode bit fields (in and out) |
| raw mode | clear `ICANON`, `ECHO`, `ISIG`, `OPOST`, … | clear `ENABLE_LINE_INPUT`, `ENABLE_ECHO_INPUT`, `ENABLE_PROCESSED_INPUT` |
| ANSI | assumed | opt-in on **both** handles |
| size | `TIOCGWINSZ` | `GetConsoleScreenBufferInfo`, **window** rect not buffer |
| resize | `SIGWINCH` | **nothing tells you** |

**There is no SIGWINCH on Windows**, so `watchResize` polls at 4Hz. That is not
a shortcut; it is what is left after choosing the VT path. The Win32 way is to
read `WINDOW_BUFFER_SIZE_EVENT` records off the console input queue — but
`ENABLE_VIRTUAL_TERMINAL_INPUT` is exactly what turns that queue into a byte
stream, and having asked for bytes you no longer get records. The trade is: a
syscall every 250ms forever, in exchange for one key decoder instead of two.

Both implementations return the same `<-chan struct{}`, capacity 1, dropping
rather than blocking, so the event loop cannot tell which one it got. Coalescing
is not an optimisation — dragging a window edge produces a notification per
pixel row and every one of them means the same thing, "the size is different
now, go and ask".

Three more that cost an afternoon each if you meet them cold:

- **`ENABLE_QUICK_EDIT_MODE` is on by default** and makes the mouse select text
  instead of reaching your program. Clearing it requires also setting
  `ENABLE_EXTENDED_FLAGS` in the same call — without that, the console ignores
  you, silently. "My TUI gets no mouse events on Windows" is usually this.
- **`ENABLE_VIRTUAL_TERMINAL_PROCESSING`** on the *output* handle is what makes
  escape sequences be interpreted rather than printed. One API call, and the most
  common "my Go TUI is broken on Windows" report.
- **`TCGETS` vs `TIOCGETA`.** termios is POSIX and the struct is portable; the
  ioctl numbers that read and write it are not. Linux and BSD chose different
  names and different values, there is no portable spelling, and that is why
  every terminal library in existence has a six-line file with a build tag on it.
  This one has two: `term_ioctl_linux.go` and `term_ioctl_bsd.go`.

---

## Drawing without flicker

Two things `frameBytes` deliberately does not do.

**It never clears the screen.** A `\x1b[2J` before each frame is the classic
cause of flicker, because for one refresh the terminal genuinely has nothing on
it. Instead: home the cursor, and erase each line as you rewrite it
(`\x1b[K`), so every cell is either overwritten or explicitly cleared and no
frame is ever blank.

**It never writes line by line.** One buffer, one `Write`, wrapped in
synchronised-output markers (`\x1b[?2026h` … `\x1b[?2026l`) which tell modern
terminals not to paint until the frame is complete. Terminals that do not know
the sequence ignore it, which is why it is safe to send unconditionally.

Streaming deltas get collapsed once, in `indexSession`, and every part of the
God view reads the collapsed slice:

```
  389   32.40s reasoning_delta ×11  The user wants me to continue compacting the transcript…
  400   32.98s text_delta ×165      1. GOAL⏎ The user instructed the agent to read `wire-notes.md`…
```

A streamed response is a thousand four-character events, and one row per event
is a view nobody can scroll. Collapsing happens in exactly one place because a
line index that means one thing to the renderer and another to the click handler
is a bug that only appears when someone uses the mouse. Both numbers are shown —
frames and characters — because their ratio is the shape of the stream, and a
provider that switches to one delta per token is visible here and nowhere else.

---

## A TUI you can grep

```sh
./agent --composer-dump session.jsonl --view model --call 12 --width 96
```

This is not a debug hatch. A TUI is a dead end for anything you want to diff,
grep, paste into an issue, or assert on in CI — and *"what did the model see on
call 12"* is exactly the kind of question whose answer you want to pipe:

```sh
# what changed in the model's view across a compaction?
diff <(agent --composer-dump t.jsonl --view model --call 11) \
     <(agent --composer-dump t.jsonl --view model --call 12)
```

It cost eight lines, because rendering and drawing were already separate
functions — `views.go` turns a session into `[]string`, `term.go` paints
`[]string`. That is also how the TUI is tested: **a UI whose output can only be
produced by pressing a key is a UI without tests.**

---

## From a real run

The God view around a compaction, from the session in stage 05:

```
  379   31.28s usage            prompt 5258 (full 138 · write 0 · read 5120) · out 47
  380   31.33s response_end     tool_calls · 1946ms
  381   31.33s tool_call        $ sed -n '91,180p' wire-notes.md
  382   31.33s gate             allow
  383   31.33s command_start    sed -n '91,180p' wire-notes.md
  384   31.41s command_end      exit 0 · 82ms · 5.3kB TRUNCATED
  385   31.41s tool_result      5.3kB to model
  386   31.41s COMPACT_START    15 messages, ~7714 tokens — summarising messages 0–10, keeping 4
  387   31.41s request          openai · 1 messages · 0 cache marks · 11.6kB
  388   32.40s first_token      TTFT 991ms
  389   32.40s reasoning_delta ×11 The user wants me to continue compacting the transcript. Let me look at 
  400   32.98s text_delta ×165  1. GOAL⏎ The user instructed the agent to read `wire-notes.md` in eight 
  565   38.34s usage            prompt 3310 (full 3310 · write 0 · read 0) · out 506
  566   38.39s response_end     stop · 6975ms
  567   38.39s COMPACT_END      15 → 5 messages · ~7714 → ~3556 tokens · 6976ms
  568   38.39s cache_lost       the prompt prefix was rewritten — every cache entry from before this p
  569   38.39s turn_start       turn 2
  570   38.39s request          openai · 5 messages · 0 cache marks · 10.2kB
```

Everything stage 05 argued is on that screen. The summarising call is a real
call (`prompt 3310 · out 506`) and it is entirely full price (`read 0`). The
request before the compaction carried 15 messages and 5,258 prompt tokens; the
one after carries 5 messages and 10.2kB. `TRUNCATED` on the `command_end` row says the model was
given less than the command produced.

Press `m` on any of those rows and you get the messages that request contained.
Press `w` and you get the bytes. That is the whole tool.

### The Wire view was not telling the truth either

`WIRE` promises "those bytes, unmodified". Building the view is what proved it
false — and the cause is a bug stage 03 already documented, sitting in a third
place nobody had looked.

`json.Marshal` escapes `<`, `>` and `&`, and `encoding/json` applies that
**inside a `json.RawMessage` too** while compacting it. `Event.Request` is a
RawMessage holding exactly what the adapter posted, and both adapters go out of
their way to encode with `SetEscapeHTML(false)` precisely because a shell
agent's requests are mostly `2>&1`, `>/tmp/out` and `<<EOF`. The trace writer
then used plain `json.Marshal`, and undid all of it one layer later:

```
posted:  {"command":"ls 2>&1 <in"}
traced:  {"command":"ls 2\u003e\u00261 \u003cin"}
```

Nothing errors. Every consumer that decodes it gets the right string back. What
breaks is the *claim*: `events.go` calls `Request` "the exact bytes about to be
sent", and after a round trip through the file it is not. All 24 recorded
requests in the session above carried the escapes.

The fix is one encoder. The lesson is that **a defence applied at one layer has
to be applied at every layer that re-encodes the same bytes** — and that the
value of writing "byte for byte" on a view is that somebody eventually checks.

### A note on what it is not

The composer reads a trace; it is not a chat window. That is not a compromise,
it is the payoff of stage 02's decision to make the trace the source of truth:

- it needs **no key, no network and no provider**, so you can read a session on
  a machine that has never been configured
- it works on a session recorded **weeks ago**, or on one running in another
  terminal right now — `r` re-reads the file, and the trace is appended live, so
  a second terminal is a live monitor with no IPC at all
- it is **deterministic**, which is why it can be tested

That first bullet was a lie for three stages, and finding out how is a better
lesson than the bullet. Stage 03 introduced a providers file and put the
config resolution above the replay branch, taking its `os.Exit(1)` with it.
Every machine it was tested on had the environment variables set, so resolution
succeeded and nothing looked wrong. On a machine with a trace file and nothing
else — the machine the feature is *for* — `--replay` printed "no provider
configured" and quit.

The fix is three lines: carry the resolution error instead of raising it, and
check it at the one place that needs a live provider. **A config error should be
fatal to the code that depends on the config and to nothing else** — and a
feature whose selling point is "works without X" needs a test that runs without
X, or the claim decays into documentation.

Wiring it to a live session in-process is one line — `bus.Subscribe(tui)` — for
the same reason the JSONL writer and the plain renderer were one line each.

### The other terminal UI in this directory

From this stage on the directory also holds a `shell.go`, and the repo holds a
`tui/` package. That is a second terminal UI, and it is not this one.

The composer is a reader: it opens a file and shows you what happened in a
session. The shell is a front end for the agent itself — a pane the panel
prints into, a bordered prompt with the provider and the running bill on the
line beneath it, a line editor, Escape to interrupt a turn, Ctrl-O to fold the
instruments away and bring them back, and slash commands for the things you
would otherwise restart the process to change.

It exists because running the agent and reading this chapter want different
programs. The UI in this chapter has to be small enough to hold in your head,
because that is the whole of its value; every feature added to it costs the
chapter something. Somebody using the agent to poke at stage 09's error triage
wants the opposite — a window that does not close when a config value is
missing, a key that stops a turn that has gone wrong, and a way to set an
endpoint without editing a file. Those are not lessons. They are the difference
between a repo you read and a repo you can work in.

So no chapter explains the shell, including this one. It is a tool and it is
allowed to be boring. Nothing here was deleted to make room for it: `tui/term`
is a copy of `term.go`, `keys.go` and `width.go` with the essays taken out, not
a replacement for them, and a behaviour change in one has to be mirrored in
the other or the paragraphs above stop being true. If you want to know what a
terminal UI is, read the four files in this directory. If you want to use the
agent, run it with no arguments.

---

## Exercises

1. **Open a trace from stage 04** and step through the calls in the Model view
   watching the `cache breakpoint` markers move. That is the rolling breakpoint,
   drawn.
2. **Find a divergence.** Pick any call, read its God events and its Model
   messages, and list everything present in one and absent from the other. There
   will be more than you expect.
3. **Break the terminal contract on purpose.** Put a `log.Fatal` inside the
   event loop, run it, and see what your shell is like afterwards. Then put it
   back.
4. **Set the Escape timeout to 1ms** and use the arrow keys over a slow ssh
   link. Then set it to 500ms and press Escape.
5. **Delete `truncCols` from `frameBytes`** and replace it with `s[:w]`. Open a
   trace whose working directory has a CJK name and watch one column of overflow
   destroy the entire frame.
6. **Add a diff view.** Two call indices, and the messages that changed between
   them. Everything you need is already in `wireView`; the interesting part is
   deciding what "changed" means when the whole prefix was rewritten.
7. **Subscribe it live.** `bus.Subscribe` the composer and run the agent inside
   it. The work is not the plumbing; it is deciding what a UI does when new
   events arrive while the user is scrolled somewhere else.

---

## What you can answer now

**Why can a chat log not show you the most common agent bug?**
Because a chat log renders the God view and calls it the conversation. Nearly
every "it forgot what I told it" report is the same fault — the thing you are
asking about is not in the model's context — and seeing that means comparing
what happened against what was sent. In the run above, 629 events had happened
and the model could see eleven messages.

**Why is `os.Exit` a bug once the program has entered raw mode?**
Because it skips deferred functions, and the deferred function is the only thing
in the world that knows how to give the terminal back. The user is left with no
echo, no line editing and no cursor, and the message explaining why was printed
onto an alternate screen that is already gone. `log.Fatal` is the same bug
wearing the most ordinary line in Go.

**Why does the signal handler re-send the signal to itself instead of calling
`os.Exit(130)`?**
Because a process killed by SIGTERM should report that it was killed by SIGTERM.
Its parent may be a shell, a supervisor or a test harness that distinguishes
signal death from a non-zero exit code. Restore the terminal, reset the handler
to the default, then die honestly.

**Why does the key decoder refuse to decide what a lone Escape byte means?**
Because it cannot: a trailing `\x1b` is either the Escape key or the first byte
of a sequence still arriving, and nothing in the bytes tells them apart. Only
elapsed time does, so the decoder says "I need more bytes" and the event loop,
which owns a clock, applies the 50ms policy. That is why Escape feels slightly
late in every terminal application you have used, and why the decoder is
testable at all — a function that timed itself could only be tested by waiting.

**Why does the renderer measure columns rather than bytes or runes?**
Because the terminal is laid out in columns and neither of the others predicts
them: `"你好世界"` is 12 bytes, 4 runes and 8 columns. Combining marks take
zero columns, fullwidth forms take two, and ANSI escapes take none, so `%-20s`
— which aligns on bytes — shears any column holding text it did not expect.

**Why is truncating with `s[:w]` a corrupted frame rather than a cosmetic
mistake?**
Because it can cut a wide character in half, leaving the terminal a byte
sequence it cannot interpret, and it can cut an escape sequence in half, leaking
an unclosed colour into everything drawn after it. Worse, a line that ends up
one column too wide wraps, which pushes every line below it down by one and
destroys the whole frame. That is why `frameBytes` calls `truncCols`.

**Why does Windows poll for the window size instead of waiting to be told?**
Because there is no SIGWINCH, and the Win32 alternative is unavailable by
construction: a resize arrives as a record on the console input queue, and
`ENABLE_VIRTUAL_TERMINAL_INPUT` — the flag that lets one key decoder serve both
platforms — is exactly what turns that queue into a byte stream. The trade is a
syscall every 250ms forever against a second decoder to write and maintain.

**Why did the Wire view's promise of unmodified bytes turn out to be false?**
Because `encoding/json` escapes `<`, `>` and `&` inside a `json.RawMessage` too,
so the trace writer's plain `json.Marshal` undid the `SetEscapeHTML(false)` both
adapters had gone out of their way to use. Nothing errored and every consumer
decoded the right string back; what broke was the claim, on all 24 recorded
requests. A defence applied at one layer has to be applied at every layer that
re-encodes the same bytes.

**Why was `--replay` broken on exactly the machines it was built for?**
Because stage 03 put provider resolution, and its `os.Exit(1)`, above the replay
branch. Every machine it was tested on had the environment variables set, so
nothing ever looked wrong; on a machine with a trace file and nothing else it
printed "no provider configured" and quit. A config error should be fatal to the
code that depends on the config and to nothing else, and a feature that
advertises "works without X" needs a test that runs without X.

**Why does a TUI need a dump mode?**
Because a TUI is a dead end for anything you want to diff, grep, paste into an
issue or assert on in CI, and "what did the model see on call 12" is exactly the
kind of question whose answer you want to pipe. It cost eight lines here only
because rendering and drawing were already separate functions. A UI whose output
can only be produced by pressing a key is a UI without tests.

---

## Questions to think about

These do not have answers in the repo. They are the ones where the answer
depends on what you are building.

1. The Model view shows what was sent, not what the model attended to. If a
   session goes wrong and the right information was in the context, what
   evidence could separate "it never had it" from "it had it and ignored it",
   and where would that evidence have to come from?

2. Every view here is assembled from events that something chose to emit, so a
   bug in code that emits nothing is invisible. How would you notice a missing
   event, and what would a test for a trace's completeness actually assert?

3. This session produced 629 events. Work out what breaks first at sixty
   thousand, and whether the answer is a filter, an index, a different storage
   format, or a different idea of what one session is.

4. Three views was the answer here: what happened, what the model saw, and the
   bytes. Is there a fourth — a story the trace holds that none of the three
   tells? Decide what it would show, and which of the existing three it would be
   competing with for the reader's attention.

5. The Wire bug was found because a view made a promise that somebody eventually
   checked. Which claims in your own agent are enforced only by prose, and what
   would it take to make one of them fail a test instead of failing a reader?

→ Next: [Stage 07 — Multiply](07-multiply.md)

→ Reference: [Stage 02 — See Everything](02-see-everything.md), [Stage 05 — Live Forever](05-live-forever.md)
