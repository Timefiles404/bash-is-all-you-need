# Stage 12 — Echo

The cheapest tool call is the one you do not make. If the model asks for a
command the agent already ran, and nothing that command read has changed since,
you can hand back the answer instead of starting a process.

That is the whole idea, and it takes about eighty lines to write. This chapter
is mostly about the other two questions, which are harder than they look and
which no amount of code will answer for you: **when are two commands the same
command**, and **is the answer still true**. Then it asks a third one that
almost nobody asks before shipping a cache — **would this have helped?** — and
the answer here turns out to be *barely*, which is the most useful thing in the
chapter.

You will build the cache anyway. Not because the number is good, but because
finding out that a number is bad costs an afternoon, and finding out after you
have shipped costs considerably more.

**Before you start.** You need stage 07's subagents, because the one place this
cache clearly pays is several children reading the same file at once; stage
08's argument that you cannot decide what a shell command does by reading it,
because this chapter needs the same fact pointed the other way; and stage 02's
event trace, because the trace is what lets you evaluate a cache against
sessions that already happened, without an API key and without running
anything.

**What you will build.** A result cache with a content-addressed key, a
whitelist that refuses every command it does not completely understand, a
staleness check made of file hashes rather than timestamps, two independent
size bounds, four counters printed whether or not they are zero, and a
`--cache-audit` mode that replays old traces through a cold cache and tells you
what it would have done. One new file, one new event kind, one flag — off by
default, and the chapter explains why.

---

## Where we are starting from

Stage 11 ends with a dispatch loop that validates a tool call and then runs it.
The bash half is six lines:

```go
func (a *agent) runCommand(ctx context.Context, turn int, callID, command string) string {
    a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: callID, Command: command})
    r := runBash(ctx, a.cfg.shell, command, a.cfg.timeout)
    rendered, truncated := r.render(a.cfg.maxOutput)
    a.bus.Emit(Event{Kind: KindCommandEnd, /* ... */})
    return rendered
}
```

Every command runs. A command the model asked for twenty turns ago runs again
if it asks again. Three subagents that all need to look at the same file open
it three times.

None of that is obviously wrong. Whether it is worth fixing depends on numbers
this chapter is about to go and get.

---

## The question to ask first

The usual way to decide whether to build a cache is to reason about it. The
reasoning is always persuasive: commands repeat, processes are expensive,
therefore a cache. Each step is plausible and the conclusion can still be
worthless, because "commands repeat" is a claim about a workload and nobody
checked.

There is a better way, and this repo has been building toward it since stage
02. **A trace records the exact command of every tool call, in order, with what
it cost.** That is everything a cache decision depends on. So before writing
the cache you can write the evaluator, point it at every session you have ever
recorded, and get the hit rate you would have had — on your own work, with no
API key and nothing running.

That is what `--cache-audit` is:

```sh
./agent12 --cache-audit .work/*.jsonl
```

It reads each trace, pulls out the `command_end` events in order, and pushes
each command through a cold cache. Sixteen recorded sessions from this repo,
107 commands:

```text
trace                      cmds    hit   miss  stale  refus      saved
esc.jsonl                     1      0      0      0      1         0s
m-delegate.jsonl              4      0      3      0      1         0s
m-inline.jsonl                4      0      3      0      1         0s
mem1.jsonl                    3      0      2      0      1         0s
mem2.jsonl                    0      0      0      0      0         0s
p-pipe.jsonl                  1      0      0      0      1         0s
p-steps.jsonl                 1      0      0      0      1         0s
p-strict.jsonl               23      0     21      0      2         0s
s05-compact.jsonl            17      2     15      0      0      217ms
s05-nocompact.jsonl           6      0      6      0      0         0s
s05-roomy.jsonl               6      0      6      0      0         0s
sb.jsonl                      3      0      2      0      1         0s
x-none.jsonl                  8      0      8      0      0         0s
x-roomy.jsonl                 8      0      8      0      0         0s
x-tight.jsonl                11      1     10      0      0       88ms
x-tight2.jsonl               11      1     10      0      0       96ms
TOTAL                       107      4     94      0      9      401ms

hit rate 3.7% of 107 commands · 21.0kB of output not re-read · 401ms of command
time not re-run
```

**Four hits in 107 commands, and 401 milliseconds saved.**

Hold that number. Everything after this is either an argument for why it is so
small, or an argument for building the thing anyway.

The audit has one limit worth naming: a trace records what the agent ran, not
what the disk did underneath it, so it cannot know whether a file changed
mid-session and would have invalidated an entry. Its hit count is therefore an
upper bound. On this sample that makes no difference — nothing wrote to
anything — but on a session that edits files it would.

---

## What a command actually costs

401 milliseconds is only meaningless if you know what to compare it to. The
same sixteen traces:

| | |
|---|---|
| commands run | 107 |
| total command time | 10,041 ms |
| median command | 92 ms |
| slowest command | 182 ms |
| command output written into contexts | 401,783 B |
| model calls | 173 |
| total model time | 864,374 ms |

**Ten seconds of shell against fourteen minutes of model.** Commands are 1.2%
of the wall clock in these sessions. A cache that eliminated *every single
command* would make them 1.2% faster; the one you can actually build eliminates
3.7% of that 1.2%, which is four hundredths of one percent.

This is not a fact about caching. It is a fact about where an agent's time goes,
and it is worth internalising before optimising anything in a loop like this
one. The tool is bash and the commands are `sed`, `wc`, `cat`, `ls`, `grep`
against files of a few tens of kilobytes. Nothing in that list is slow. The slow
thing is a language model on the other side of a network, and it is slow by
three orders of magnitude.

If your commands are `npm install`, a test suite, or a query against a database
in another region, every number in this chapter changes and the cache becomes
obviously worth it. The instrument is the point: it tells you which of those two
worlds you are in.

---

## Where the four repeats came from

Four hits is a small enough number to look at one at a time, and doing so is
more informative than the rate.

Two of them are in `s05-compact.jsonl`. Laid out in order, with the compaction
events left in:

```text
turn   2   sed -n '1,150p' wire-notes.md
turn   4   sed -n '151,300p' wire-notes.md
turn   6   sed -n '301,450p' wire-notes.md
           << compacted >>
turn  11   sed -n '601,731p' wire-notes.md
           << compacted >>
turn  12   sed -n '1,150p' wire-notes.md        <- repeat of turn 2
turn  13   sed -n '151,300p' wire-notes.md      <- repeat of turn 4
```

The model read the first three hundred lines of a file, the summariser replaced
those turns with a paragraph about them, and the model read the same three
hundred lines again. The other two repeats, in `x-tight.jsonl` and
`x-tight2.jsonl`, have the same shape with a different cause: the user sent a
second message, and the model re-read a file it had read under the first one.

So the repeats an agent produces are not random redundancy that a cache
politely absorbs. **They are the model recovering something it lost**, and the
two ways it loses things are compaction and the passage of turns. That has a
consequence for the design, and it is the one thing in this chapter that is
easy to get wrong in an interesting way.

The tempting optimisation is to answer a repeat with a pointer instead of the
bytes — "you ran this at turn 2, the output is above" — which would save the
context window as well as the process. It cannot work here. The model re-ran the
command *because the earlier result is no longer in front of it*. A pointer to a
message that was compacted away is a pointer to nothing, and it fails exactly in
the case that produced the repeat.

Which leaves a cache that returns the same bytes. That saves the process and
saves nothing at all in the context window: those 5,447 bytes go into the
transcript either way. **A result cache is a wall-clock feature, not a token
feature.** Anyone who tells you otherwise has not looked at where the bytes end
up.

---

## When two commands are the same command

The key is a hash, and everything that can change the answer has to be inside
it.

```go
func keyOf(shell, wd, command string, maxOutput int, env []string) string
```

The command is obvious. The other four are each worth a sentence.

**The working directory**, because `cat notes.md` is a different question in
two directories, and an agent that ever runs a command anywhere but one place
would otherwise serve one directory's answer for another's.

**The shell**, because `bash` and `dash` disagree about enough to matter, and
which one you get is a `findBash()` decision that depends on the machine.

**The output budget**, because what is stored is not what the command printed —
it is the *rendered* result, already truncated to `--max-output` and already
carrying its `[exit 0 · 92ms]` footer. Those are the bytes the model reads. The
same command under a different budget produces different bytes.

**The environment**, because a shell inherits it and programs read it. `sort`
respects `LC_ALL`, `grep` used to respect `GREP_OPTIONS`, and the list is not
enumerable.

Now: three of those four cannot change while the process is running. The agent
never changes directory, `findBash` runs once, and the flags are parsed at
startup. Putting a constant in a cache key looks like superstition.

It is insurance against the next feature. The day someone persists this cache to
a file, or gives a subagent its own working directory, or makes `--max-output`
adjustable mid-session, those stop being constants — and a key that was only
*accidentally* correct starts quietly returning the wrong answer. The cost of
carrying them now is 11.7 microseconds per lookup, which is measured below and
is nothing.

Sorting the environment before hashing is not decoration either. `os.Environ()`
makes no promise about order, and a key that changes when two unrelated
variables swap places is a cache that never hits and never explains itself.

---

## What the command read

Here is where it gets genuinely hard.

To know whether an answer is still true, you need to know what the command
read. For a tool with a schema — `read_file(path)` — that is a field. For bash
it is undecidable in the general case. `grep -r foo .` reads a whole tree that
no argument names. `cat "$F"` reads a file whose name is in a variable. A
command can read the clock, the network, the process table, or another
program's output.

Stage 08 reached the same wall from the security side and stated it as: *you
cannot secure a shell by reading the command.* The wall is the same one. What
differs is which direction you are allowed to fall off it.

A security blocklist that fails open runs something dangerous. A cache
whitelist that fails closed runs the command — which is what would have happened
without a cache at all. **So the entire eligibility rule is built out of
refusals**, and it is allowed to be stupid in exactly one direction.

### A parser that refuses what it does not understand

`splitPipeline` understands single quotes, double quotes, backslash escapes and
the pipe operator. Everything else is an error:

```go
case '$', '`', ';', '&', '<', '>', '(', ')', '{', '}', '\n', '\r', '#',
     '*', '?', '[', ']', '~', '!':
    return nil, fmt.Errorf("unsupported shell character %q", string(c))
```

Redirection, command substitution, variable expansion, subshells, control
operators and globs all land in that one line. Globs are there with the control
operators for a related reason rather than the same one: `cat *.md` names a set
of files that the *shell* resolves, so the paths this function reports are not
the paths the command reads, and a new file appearing in the directory would
change the answer without changing any recorded witness.

This is not a shell and must not grow into one. If it ever needs `$(...)` to be
useful, the honest fix is stage 08's real parser, not another case in the
switch.

### The unit of the whitelist is not the program

The obvious design is a list of read-only program names. It is wrong for two of
the six programs that would be on it.

`sed` reads, and `sed -i` edits the file in place. `sort` reads, and `sort -o`
writes its output to a file. A name-based whitelist would have let both through
and then cached the result of a write.

So the rule is a program *and* the exact set of flags it may be given, with an
unknown flag being a refusal rather than a shrug:

```go
"sort": {boolFlags: set("-n", "-r", "-u", "-h", "-b", "-f", "-V", "-g"),
         valueFlags: set("-k", "-t", "-S")},
```

`-o` is not on that list, and it does not need to be named as forbidden. It is
refused because it was not permitted, which is the property that makes the rule
maintainable: a flag someone adds to `sort` in 2029 is refused on the day it
ships, with no change here.

Two entries carry more than flags. `grep` may not be given `-r` or `-R`, because
a recursive grep's witness set is a whole tree and a path list cannot hold one
— and a witness set that is quietly incomplete is worse than no cache, because
it produces confident stale answers instead of slow correct ones. `sed` has a
script, and a sed script is a program:

```go
func sedScriptSafe(script string) error {
    if i := strings.IndexAny(script, "wWrRe"); i >= 0 {
        return fmt.Errorf("script contains %q, which could be a file or exec command", script[i])
    }
    return nil
}
```

`w` writes the pattern space to a file, `r` reads one in, `e` executes a shell
command. The check looks at every character rather than at command positions,
because deciding whether a given `w` is a command means writing a sed parser.

### One refusal that is wrong, on purpose

That rule refuses `sed -n '/word/p'`, which reads a file and writes nothing. It
cannot tell the `w` inside "word" from sed's `w` command.

That is the correct trade and it is worth being explicit about why. A false
refusal costs one command: 92 milliseconds, at the median measured above. A
false acceptance writes to the user's disk and then serves the write from a
cache. There is a test asserting this refusal, so that anyone who "fixes" it has
to delete an assertion that explains itself first.

### What the rule actually refused

On the 107 real commands, the audit reports:

```text
refused, by reason:
    4  unsupported shell character "*"
    3  unsupported shell character ">"
    2  unsupported shell character "{"
```

Nine refusals, and **not one of them is about the program**. Every command the
model ran in sixteen sessions was `sed`, `wc`, `cat`, `ls`, `find` or `grep` —
six programs, all readers, all on the list. What the rule actually spends its
refusals on is shell syntax: four globs, three `2>&1` redirections, and two
`find ... -exec ... {} +`.

That is a useful thing to learn from a refusal tally, and it is why the audit
prints one. A hit rate is a verdict; a refusal reason is a to-do list. It names
the rule that would have to grow, and lets you decide whether growing it would
be worth anything. Here it would buy four commands out of 107, so it is not
worth anything, and the rule stays small.

One caveat on that sample: those sessions were given reading tasks, so a
whitelist of readers covering 100% of them is not the general figure. A session
that edits and builds would look nothing like this.

---

## Is the answer still true?

A witness is a path the answer depends on, together with the digest that path
had when the command ran. On lookup, re-digest and compare.

The interesting question is what a digest should be. The cheap answer — the one
almost every implementation reaches for — is the pair `(size, modification
time)`. It is two fields off a `stat` and it does not read the file.

### Size and modification time cannot see a rewrite

The failure is a same-length rewrite inside one clock tick. Write `route2:x`,
then write `route3:y`. Same length, different bytes. If both writes land inside
one tick of whatever resolution the filesystem records, the two stats are
identical.

That sounds like a race you would have to be unlucky to lose. On this machine
you have to be lucky to win it. Two thousand back-to-back trials:

```text
P2 same-length rewrite, back to back
   1498/2000 rewrites were invisible to (size, mtime)
   the bytes differ every time: "route2:x" -> "route3:y"
```

**Three quarters of them.** The reason is in the probe above it:

```text
P1 mtime granularity
   1897 writes in 300ms produced 555 distinct mtimes
   smallest gap between two distinct mtimes: 501200 ns (501.2µs)
   median gap: 527000 ns (527µs)
   writes that landed on an mtime a previous write already had: 1342
```

The modification time this machine hands back moves in steps of about half a
millisecond, so 1,342 of 1,897 writes — 70.7% — were stamped with a time some
earlier write already had. That is not the 100-nanosecond resolution NTFS
stores; it is the rate at which the timestamp the file system writes is actually
updated, and it is a property of the running system rather than of the disk.

On a FAT or exFAT volume it is far coarser. A shipped agent that snapshots a
workspace refuses to trust any modification time within **two seconds** of its
own index save for exactly this reason, and the constant in its source is
commented as being about FAT granularity, not clock skew.

There is a second reason not to build on timestamps, independent of resolution:
a timestamp can be set. `os.Chtimes` is one call, `touch -r` is one command, and
an editor that preserves modification times is not exotic. A witness that can be
forged by any program on the machine is not a witness.

### What the correct check costs

The alternative is to read the file and hash it. The usual objection is that
this is orders of magnitude more expensive, so here is the measurement, with
both halves taken in the same round so that whatever the machine is doing to the
clock affects both:

```text
file                                  bytes        stat   read+hash   ratio
go.mod                                  149      16.9µs      34.3µs    2.0x
docs/00-loop.md                       11801      16.9µs        40µs    2.4x
docs/wire-notes.md                    49616      17.8µs      68.1µs    3.8x
```

**Two to four times, not a hundred.** On Windows a `stat` is not a free glance
at an inode — it opens a handle — so the cheap check is not as cheap as the
mental model says, and the correct one is 68 microseconds on a 50-kilobyte
file. Against a command whose median is 92 milliseconds, that is 0.07%.

So the digest is the content hash. The decision took one measurement and it was
not close.

(Both figures are warm-cache, and on Linux a `stat` is a great deal cheaper than
17µs, so the ratio there is worse. It is still a comparison between microseconds
and a command measured in tens of milliseconds.)

### A directory is a witness too

`ls` with no argument reads the working directory, which is the one command in
the whole sample that is always run with no path. Its witness is the directory,
and a directory's digest is a hash of its one-level listing — names, sizes,
modes and modification times, which is roughly what `ls -l` prints and therefore
roughly what an `ls` result depends on.

One level, not a tree. That is why `ls -R` is not on the flag list.

### Hashing before and after, and why one of them is not enough

This part was arrived at by a test failing, and the failure is more instructive
than the fix.

The first version hashed the witnesses once, after the command had run, and
stored the result against those digests. Consider a file that is being written
while the command reads it. The command produces a torn result — half the old
file, half the new — and that result is then stored against the digest the file
*ended up* with. The next lookup finds a matching witness and serves a result
that never corresponded to any state of the file. The cache is now confidently
wrong, and stays wrong until something touches the file again.

The obvious correction is to hash before the read instead. That can never serve
a wrong answer, and it introduces a different failure: a file that changed
between the lookup and the command is stored against a digest it no longer has,
so the entry is stale the moment it is created and every future lookup re-runs
the command. Safe, and silently useless — which, as the next section argues, is
the failure mode a cache is worst at telling you about.

Both, then. Hash on the way in, hash again on the way out, and store nothing if
they disagree:

```go
ws := digestAll(witnessPaths(look.before))
for i := range ws {
    if ws[i] != look.before[i] {
        return // it changed under the command; this text describes nothing
    }
}
```

One extra hash — 68 microseconds — and it is the only version that is neither
wrong nor pointless.

---

## What must never be stored

**A non-zero exit.** An exit code is an outcome, not an answer, and the outcomes
that repeat are exactly the ones you least want frozen: a permission blip, a
file being written as it was read, a disk that filled for a minute. Cache one
and the agent is stuck with it until the bytes underneath happen to change.

There is a field report of the opposite choice, and it is not a mistake — it is
a deliberate trade. A code index cached parse *failures* keyed on the content
hash, so that a file which failed to parse was never retried. Fixing the parser
re-indexed nothing, because every file still hashed to the same bytes that had
failed. They chose termination over correctness and wrote down that they had.
Here the trade goes the other way, because a shell command that failed once is
usually a command that will succeed next time.

**A timeout, a cancellation, or an unreaped process tree**, for the same reason
and more strongly. None of those texts describes what the command does; they
describe what happened to it once. Stage 10's `[TIMED OUT after 30s]` is a
sentence about a moment.

### Two bounds, because they run out at different times

The cache is capped by entry count *and* by total bytes.

One bound leaves the other unguarded, and the two are exhausted by different
sessions. A session that runs `wc -l` over four hundred files fills the entry
count with forty-byte answers and never approaches a byte budget. A session that
reads four large files fills the byte budget with four entries and never
approaches a count. An unbounded result cache in a long session is a memory leak
with good intentions.

The general rule the two caps come from: **cap any cache whose key space grows
with the input.** A cache keyed on something bounded — one entry per workspace,
one per provider — does not need it and is clearer without it.

---

## The cache bug with no symptom

Everything above is about being wrong. This section is about the failure that is
harder to find, because a cache has a second way to fail and it produces no
evidence at all.

A shipped agent cached the result of a `git log --name-only` behind a
15-second time-to-live. Its caller refetched every 30 seconds. Every entry had
always expired by the time it was asked for, so the hit rate was exactly zero,
and every open document paid the full 0.3-second command every single time.

Nothing was wrong. No incorrect answers, no errors, no log line, no alert. A
cache that never hits behaves identically to a cache that works — the results
are correct, the code runs, the tests pass. It ran that way for months.

That is the argument against a TTL as the *primary* mechanism, and this cache
does not have one by default. Staleness here is decided by content, which is a
fact rather than a guess about how long a fact stays true.

A TTL is still available as a backstop, and there is a real reason for it: the
witness set is a **lower bound** on what a command read. It holds the paths
named on the command line, and a command can depend on things no path names. A
TTL bounds how long a wrong answer can survive that gap. It is off by default
because on this workload the gap is small, and because a badly chosen one costs
you everything while looking like it costs nothing.

The defence against the silent version is not clever. It is a counter, printed:

```text
  result cache: 0 hits / 12 lookups (0 refused · 0 stale) · 0B not re-read · 0s not re-run
```

Every other conditional line in this panel is hidden when it is zero, on the
principle that "0 retries" on every clean session trains people to stop reading
the line. This one is the exception, and the exception is the point. A session
with no retries is a session where nothing went wrong. A cache with no hits
looks *exactly* like a cache that is working, and the only thing that
distinguishes them is a number somebody prints.

The same reasoning explains why `refused` and `miss` are counted apart.
Ten misses in a row is a cold cache warming up, and it will get better. Ten
refusals is a cache that will never help with this workload no matter how long
it runs, and it will not. Fold them into "not a hit" and you cannot tell which
of those two sessions you are looking at, and they have different fixes.

---

## A hit is not a command

A command served from the cache emits a `result_cache` event and does **not**
emit `command_start` or `command_end`.

It would have been half a line of work to emit them anyway with a flag set, and
several things downstream would have been simpler for it: the panel's command
counter, the replay header, anything anyone writes later against a trace. It is
worth being clear about what that half-line would have bought — a trace, in
every session this agent ever records, that says a process ran when none did.

A trace is evidence or it is decoration. It cannot be both, and the moment it
starts describing things that did not happen it is only the second one.

The visible consequence is that a session of ten tool calls with four hits
honestly reports six commands, which looks like four calls went missing. So the
replay summary carries the hits as their own number:

```text
trace · 214 events · 8 turns · 6 commands · 4 cached · 1m12s
```

Two numbers instead of one, because there are two facts and no single number
can carry both.

---

## Where it does pay

None of the three live sessions run for this chapter produced a single hit from
compaction, which is worth reporting because it was the case the audit pointed
at. A session with a window tight enough to compact five times read a file in
twelve sequential chunks and never re-read one: the summariser recorded which
ranges were done, and the model believed it. **A compactor that summarises well
removes the repeats a cache was going to absorb.** That is not a disappointing
result, it is a better fix than caching, and it belongs to stage 14.

The case that does pay is the one stage 07 built. Four agents — a parent and
three children — asked to read the same file at the same time:

```text
  38 calls · 44 commands
  prompt tokens billed: 499172  (full 80484 · write 0 · read 418688)
  result cache: 12 hits / 56 lookups (2 refused · 0 stale) · 63.8kB not re-read · 1.107s not re-run
```

**Twelve hits in fifty-six lookups: 21%.** Grouped by which agent asked:

```text
List every JSON field name#1   sed -n '1,300p' wire-notes.md      5449B   99ms
List every HTTP status code#3  sed -n '1,300p' wire-notes.md      5449B   99ms
List every HTTP status code#3  sed -n '301,600p' wire-notes.md    5447B   83ms
List every HTTP status code#3  sed -n '601,952p' wire-notes.md    5448B   90ms
List all section headings#2    sed -n '301,600p' wire-notes.md    5447B   83ms
List all section headings#2    sed -n '601,952p' wire-notes.md    5448B   90ms
...
```

Three ranges, four readers, and whoever arrived second got the answer free. This
is why the cache is shared with children by pointer rather than being created
per agent — and sharing it is a decision, not a default. Stage 10 lost an entire
feature to the opposite mistake: `dl` was simply not listed in the child's
struct literal, so subagents ran with every deadline switched off, and nothing
failed and nothing said so.

The test for what to share is whose fact it is. "This endpoint is refusing
calls" is a fact about the endpoint, so the provider ladder is shared. "This
file contains these bytes" is a fact about the working tree, which the parent
and every child are looking at simultaneously, so the cache is shared. "This
conversation is 12,000 tokens long" is a fact about one conversation, so the
compactor is not.

---

## What that was worth, exactly

The fan-out session is the best case this cache has, so its numbers are the
ceiling rather than the average:

| | |
|---|---|
| session wall clock | 4 m 23 s |
| model time | 360,656 ms |
| command time actually spent | 3,789 ms |
| command time not spent, thanks to the cache | 1,107 ms |

The cache removed 12 of 56 commands and 22.6% of the command time. That is
**0.3% of the session.**

The same task was run again with the cache off, and the comparison is worth
showing precisely because it proves nothing. That run did 47 commands to the
cached run's 44, made three fewer model calls, produced 2,300 more output
tokens, hit a 500 that cost it a retry, and finished in 4 m 50 s against
4 m 22 s. Two runs of a nondeterministic agent cannot measure a 1.1-second
difference; the noise between them is two orders of magnitude larger than the
effect. **The number that is a measurement is the one from inside a single
run** — twelve commands did not happen, and the cache says so itself. When the
effect you are looking for is smaller than the variance of the thing you are
measuring, an A/B comparison is theatre, and an internal counter is evidence.

And in the same summary, from the same run, is this:

```text
prompt tokens billed: 499172  (full 80484 · write 0 · read 418688)
```

**83.9% of the prompt tokens were served from a cache.** Not this one — stage
04's, the provider's prompt cache, which was already there, which needed no
whitelist, no witness set and no tokenizer, and which is doing more than two
hundred times as much work as the feature this chapter builds.

That is the real lesson of stage 12 and it is not "do not cache". It is that
**the expensive repetition in an agent is on the wire, not in the shell**, and
the instinct that reaches for a result cache is usually pointing at the wrong
one of the two. The eighty lines are still worth writing, because the way you
find that out is by building the instrument and reading it — and because on a
workload where commands take thirty seconds instead of ninety milliseconds, the
same eighty lines change the answer completely.

### What the cache itself costs

Cheap enough not to be part of the decision, and measured rather than assumed:

| | |
|---|---|
| `eligible` — tokenize and check a piped command | 759 ns |
| `keyOf` — hash command, cwd, shell, budget and whole environment | 11.7 µs |
| a complete miss on a 50 KB file, including both witness hashes | 73.7 µs |

73.7 microseconds against a 92-millisecond median command is 0.08%. The
overhead is not why this cache does not pay.

---

## What this stage does not do

- **Globs.** Four of the nine refusals are `ls *.md` and `wc -l src/*.go`.
  Supporting them means expanding the pattern yourself, witnessing the directory
  *and* every matched file, and getting the shell's matching rules right.
  Worth four commands out of 107 here; possibly worth much more on a workload
  that lists directories constantly.
- **A cache that survives the process.** Everything here lives in memory and
  dies with the session, which is why the shell, the working directory and the
  environment are constants — and why they are in the key anyway. Persisting it
  means the witness set has to survive too, and a stale entry can now outlive
  the machine state that justified it.
- **Anything the model can see.** A hit is invisible to the model: it gets the
  same bytes with the same footer. Telling it "this was cached" would be one
  more sentence in the permanent transcript, and stage 11 spent a section on why
  that is a cost rather than a courtesy.
- **Caching a model call.** Two prompts that differ by a timestamp are not the
  same prompt, and the provider's own cache already handles the case where they
  share a prefix. Stage 17 is where prompt shape and cache alignment get their
  own chapter.
- **Deduplicating identical results.** Four agents each got their own 5,449-byte
  copy of the same text in their own context window. The cache saved the read,
  not the tokens. That is a different mechanism with a different failure mode.
- **A Windows visibility guard.** A shipped agent sleeps 200 ms after a child
  process exits before snapshotting a workspace, on the grounds that the child's
  writes may not be visible yet. Two hundred trials on this machine needed zero
  retries, so the guard is not here — a delay that costs 200 ms per command to
  prevent something you cannot reproduce is a bad trade until you can.

---

## Exercises

1. **Audit your own traces.** Run `--cache-audit` over every trace you have
   from earlier stages. If your hit rate is also under 5%, you have just
   avoided building this in a project where it matters. If it is 30%, you have
   learned something about your workload that no amount of reasoning would have
   told you.
2. **Make the witness lie.** Cache `cat notes.md`, then use `touch -r` or
   `os.Chtimes` to give the file its old modification time after editing it.
   The content hash catches it. Now switch `digestOf` to return
   `fmt.Sprintf("%d:%d", size, mtimeNs)` and watch the same sequence serve the
   old bytes.
3. **Give it a bad TTL.** Run with `--cache-ttl 2s` on a session whose repeats
   are further apart than that. Everything works, the results are right, and the
   hit rate is zero — which you can only see because the summary prints it.
   Then find the smallest TTL that still hits.
4. **Delete a bound.** Set `--cache-entries 0` and run something that lists many
   files. Watch the process size. Then set `--cache-bytes 0` instead and read a
   few large files. The two caps catch different sessions, which is why removing
   either one is not half a problem.
5. **Break the double hash.** Make `store` skip the before/after comparison,
   then run a command against a file that a background loop is rewriting. Look
   for a served result that matches no version of the file.
6. **Widen the rule.** Add glob support: expand the pattern, witness the
   directory and every match. Run the audit again and see what four commands
   were worth.
7. **Find the workload where this wins.** Point the agent at a repository large
   enough that `grep -n` over it takes a second or more, give it a task that
   searches repeatedly, and compare the audit's saved-time figure with the 401
   milliseconds measured here.

---

## What you can answer now

**Why is a result cache worth so little in an agent like this one?**
Because commands are not where the time goes. Across sixteen recorded sessions,
107 commands took 10,041 ms and 173 model calls took 864,374 ms — a ratio of
about 86 to 1. Eliminating every command would save 1.2% of the wall clock, and
the achievable hit rate was 3.7%, worth 401 ms. The picture changes entirely if
your commands are slow ones.

**How do you find out whether a cache will help before you build it?**
Replay the commands from traces you already have through a cold cache and count.
`--cache-audit` does exactly that: no API key, no model, no running anything.
Its hit count is an upper bound, because a trace does not record what changed on
disk underneath the session.

**Why do agents re-run commands at all?**
In this sample, never at random. All four repeats followed either a compaction
or a new user message — the model recovering something it had lost. That is why
answering a repeat with "you already ran this, see turn 2" cannot work: the
result it points at is exactly what is no longer there.

**Does a result cache save tokens?**
No. A hit returns the same bytes, so the transcript grows by the same amount as
if the command had run. It saves the process and the wall clock. In the fan-out
session, four agents each received their own 5,449-byte copy of the same text
even with the cache hitting.

**What belongs in the cache key?**
Everything that can change the answer: the command, the working directory, the
shell, the output budget (because what is stored is the rendered, truncated
result) and the environment, sorted so that ordering is not information. Three
of those cannot change in a single process today, and they are in the key
anyway, because a key that is only accidentally correct breaks silently on the
day someone adds a feature.

**Why can't you just check whether a command is read-only?**
For the same reason stage 08 could not decide whether one was dangerous: the
command is a program, and reading a program does not tell you what it does. The
difference is the direction of failure. A cache is allowed to fail closed, so
the rule refuses everything it does not completely understand — every glob,
redirection, substitution and subshell, plus any program or flag not explicitly
listed. The whitelist is over flags rather than over program names because
`sed` reads while `sed -i` writes and `sort` reads while `sort -o` writes: a
name-based list would have been wrong for two of the six programs on it. On 107
real commands the rule refused nine, all for shell syntax and none for the
program.

**How do you tell whether a cached answer is still true?**
By hashing the contents of every file the command named, twice. Not `(size,
mtime)`: on this machine 1,498 of 2,000 same-length rewrites were invisible to
that pair, since the timestamp moves in steps of about half a millisecond and
70.7% of writes landed on a stamp an earlier write already had — and a
timestamp can be set by any program on the machine anyway. Reading and hashing
costs 2.0× to 3.8× a `stat`, 17 µs against 34–68 µs, which is 0.07% of a 92 ms
command. Twice, because hashing only *after* the command stores a torn read
against the digest the file ended up with and then serves it, while hashing only
*before* stores an entry that is stale the moment it is created — safe and
silently useless.

**What is the cache failure that produces no evidence?**
A cache that never hits. There are no wrong answers, no errors and no log lines
— it is indistinguishable from one that works. A shipped agent ran a 15-second
TTL against a 30-second refetch for months at a 0% hit rate. The defence is to
count hits and print the count even when it is zero, which is the one line on
this panel that is shown when it is nothing.

**Why doesn't a cache hit emit `command_start` and `command_end`?**
Because no command started and none ended, and a trace that records processes
that never existed is not evidence. The cost is that a session of ten calls with
four hits reports six commands, so the hits are carried as their own number in
the replay header rather than folded into the command count.

**Which cache in this agent is actually doing the work?**
The provider's prompt cache from stage 04. In the same fan-out session where the
result cache saved 1.1 seconds of 361 seconds — 0.3% — 418,688 of 499,172 prompt
tokens were served from the prompt cache, or 83.9%. The expensive repetition in
an agent is on the wire, not in the shell.

---

## Questions to think about

1. The witness set is a lower bound: it holds the paths named on the command
   line, and a command can depend on things no path names. What would you have
   to observe — and at what cost — to turn it into an upper bound instead, and
   which of the tools your agent runs would still defeat it?
2. A hit returns the same bytes, so the context window grows exactly as much as
   if the command had run. Design a mechanism that saves the tokens as well, and
   then decide what it must do on the turn after a compaction, when the model
   asks again precisely because it can no longer see the earlier copy.
3. This cache is shared between a parent and its children because the working
   tree is one fact they all observe. What happens to that argument the day a
   subagent gets its own working directory, its own container, or its own
   machine — and what would have to be in the key for the sharing to stay
   correct rather than merely to keep working?
4. The refusal tally is a to-do list, and here it said globs were worth four
   commands out of 107. Under what workload would that tally justify a real
   shell parser, and what would you have to measure to know you had crossed
   that line rather than guessing you had?
5. Two features in this repo are called a cache and they have nothing to do with
   each other. Find another pair of names in a system you work on that collide
   like that, and work out whether the confusion has already cost anyone a
   wrong number — the test is whether the two are ever printed on the same
   line.

→ Next: Stage 13 — Rewind

→ Reference: [Stage 04 — The Cache](04-the-cache.md), [Stage 07 — Multiply](07-multiply.md), [Stage 08 — Sandbox](08-sandbox.md), [Stage 10 — Deadlock](10-deadlock.md)
