# Stage 12 · part 2: the key, the whitelist, and the witnesses

[← back to stage 12](README.md) · previous: [1 · the audit](1-audit.md) · next: [3 · off by default](3-off.md)

> `(size, mtime)` is the standard way to tell whether a file changed. Measured on
> this machine: **1498 of 2000 same-length rewrites were invisible to it**, and
> the bytes differed every time.

---

## The problem

Two questions, and a wrong answer to either one serves stale data forever.

**What counts as the same command?** `cat notes.md` run twice is the same
command — unless the working directory changed, or the shell is different, or the
output cap moved, or an environment variable the command reads has a new value.

**Did what it read change?** This is the harder one, because the honest answer
requires knowing what the command read, and all you have is a string.

And the failure mode is silent in both directions. A cache that never hits
behaves identically to one that works — correct results, no errors, no log line,
tests green. A cache that hits when it should not returns yesterday's answer with
today's confidence.

---

## The idea

Refuse whatever you cannot account for, at three separate gates.

![The path one command takes through the cache](images/cache.svg)

| gate | question | on doubt |
|---|---|---|
| the tokenizer | do I understand every character? | refuse |
| the whitelist | is this program-plus-flags a pure reader? | refuse |
| the witnesses | is every path it named byte-identical? | miss |

Over-refusing costs one command being run that need not have been. Letting one
through writes to a user's disk and then serves the write from a cache.

---

## Building it

### Step 1: put four "cannot change now" quantities in the key

```go
func keyOf(shell, wd, command string, maxOutput int, env []string) string {
	sorted := append([]string(nil), env...)
	sort.Strings(sorted)
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00%d\x00", shell, wd, maxOutput)
	for _, e := range sorted {
		fmt.Fprintf(h, "%s\x00", e)
	}
	fmt.Fprintf(h, "\x00%s", command)
	return hex.EncodeToString(h.Sum(nil))
}
```

Four details in eleven lines.

**`sort.Strings`** — `os.Environ()` makes no promise about order, so an unsorted
environment hash is a key that changes when two unrelated variables swap places.
The cache would go cold at random.

**The whole environment**, not a selected subset. `LANG` changes `sort` order,
`TZ` changes `date`, `PATH` changes which program runs. Selecting is a guess; the
whole thing is a fact.

**`\x00` separators** so that `("a", "bc")` and `("ab", "c")` do not hash the
same.

**`v1`** at the front, so that changing this function invalidates every entry
rather than mixing two schemes.

### Step 2: the tokenizer understands four things; everything else is an error

```go
case '$', '`', ';', '&', '<', '>', '(', ')', '{', '}', '\n', '\r', '#', '*', '?', '[', ']', '~', '!':
	return nil, fmt.Errorf("unsupported shell character %q", string(c))
```

Single quotes, double quotes, backslash escapes and `|`. That is the whole
grammar it accepts.

Stage 08 is the argument for this being a list of refusals rather than a parser:
`$X` has no value at this point, a glob names a set the shell resolves later, and
`$(…)` is a program. None of them can be witnessed.

Backslashes need care, because inside double quotes only some escapes are
escapes:

```go
case '\\':
	if i+1 >= len(command) {
		return nil, fmt.Errorf("trailing backslash")
	}
	if next := command[i+1]; next == '$' || next == '`' || next == '"' || next == '\\' || next == '\n' {
		i++
	}
	cur.WriteByte(command[i])
	continue
```

### Step 3: the whitelist's unit is not the program, it is program plus flags

```go
"sort": {boolFlags: set("-n", "-r", "-u", "-h", "-b", "-f", "-V", "-g"),
	valueFlags: set("-k", "-t", "-S")},
```

`sort` reads. `sort -o out.txt` writes. `sed` reads; `sed -i` edits in place.
**A name-based whitelist is wrong for 2 of the 11 programs that would be on it**,
and it is wrong in the direction that destroys files.

Note how `-o` is excluded: by **omission**, not by naming. A whitelist that lists
the dangerous flags has to be complete to be safe; one that lists the safe flags
is safe by default and merely incomplete.

And models write bundled short flags, so:

```go
for _, c := range a[1:] {
	if c > 127 || !r.boolFlags["-"+string(c)] {
		return false
	}
}
```

Splitting a bundle is eight lines and is allowed only when **every** letter is a
boolean flag already on the list. An audit elsewhere found three refusals reading
`grep: unknown flag -oE`, `-oP`, `-noiE` — one class, not three accidents.

### Step 4: a sed script is a program, not a path

```go
func sedScriptSafe(script string) error {
	if i := strings.IndexAny(script, "wWrRe"); i >= 0 {
		return fmt.Errorf("script contains %q, which could be a file or exec command", script[i])
	}
	return nil
}
```

`w` writes the pattern space to a file. `r` reads one in. `e` executes a shell
command. `W` and `R` are variants. So `sed -n '1,150p' f` is a reader and
`sed -n 'w out.txt' f` is not, and the difference is inside a quoted argument.

This rule is deliberately stupid in one direction, and there is a test that pins
the stupidity:

```go
if _, ok, _ := eligible("sed -n '/word/p' notes.md", dir); ok {
	t.Error("accepted a sed script containing 'w'. The rule is allowed to be stupid in exactly one " +
		"direction: a false refusal costs one command, a false acceptance writes to the user's disk " +
		"and then serves the write from a cache")
}
```

`/word/p` is refused because "word" contains a `w`. That is a false refusal, it
is asserted on purpose, and the test message says why.

### Step 5: the witness cannot be (size, mtime)

```go
b, err := os.ReadFile(path)
if err != nil {
	return ""
}
sum := sha256.Sum256(b)
return "f:" + hex.EncodeToString(sum[:])
```

Content hash, and the measurement in the next section is the reason.

Two properties `(size, mtime)` does not have. A same-length rewrite changes no
size, and modification-time resolution on this machine is about **half a
millisecond** — not the 100 ns NTFS stores. And a modification time **can be
set**: `os.Chtimes` is one call, `touch -r` is one command, and editors that
preserve mtimes are not exotic. A witness any program can forge is not a witness.

A directory gets hashed as a one-level listing:

```go
fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00", n, sub.Size(), sub.Mode(), sub.ModTime().UnixNano())
```

One level, which is exactly why `ls -R` is not on the flag list. Recursive
anything cannot be witnessed by a bounded set — the witness for `grep -r` is a
whole tree.

### Step 6: hash twice, and store nothing if they disagree

```go
ws := digestAll(witnessPaths(look.before))
if len(ws) != len(look.before) {
	return
}
for i := range ws {
	if ws[i] != look.before[i] {
		return // it changed under the command; this text describes nothing
	}
}
```

Hash before the command *and* after, and only store if they match.

Each half alone fails differently. **Only after**: a torn read gets stored
against the digest the file ended up with, and is then served forever. **Only
before**: the entry is stale the moment it is created — safe, and silently
useless.

And the other precondition:

```go
if r.ExitCode != 0 || r.TimedOut || r.Cancelled || r.Unreaped {
	return
}
```

Nothing that failed gets stored. A non-zero exit is data the model needs (stage
00), and it is data whose cause is usually the thing about to be fixed.

### Step 7: the assertion that passed for four months on Windows

```go
if runtime.GOOS == "windows" {
	if d := digestOf(paths[0]); d == "" {
		t.Fatalf("the witness %q hashes to nothing, so it can never go stale", paths[0])
	}
}
```

`digestOf` returns `""` when it cannot read the file. An unreadable witness
hashes to the empty string — and the empty string compares equal to itself, so
the entry never goes stale and the cache serves it forever.

On Windows a `stat` is not a free glance at an inode, it opens a handle, and
there are more ways for that to fail. The test exists because a witness that
cannot fail is not a witness either.

### Putting it together

```go
func (rc *resultCache) lookup(shell, wd, command string, maxOutput int, env []string) cacheLookup {
	paths, ok, why := eligible(command, wd)
	if !ok {
		return cacheLookup{verdict: cacheRefused, reason: why}
	}
	key := keyOf(shell, wd, command, maxOutput, env)
	// ...
	e, have := rc.entries[key]
	if !have {
		return cacheLookup{key: key, verdict: cacheMiss, before: digestAll(paths)}
	}
	stored := e.witnesses
	rc.mu.Unlock()

	changed := ""
	for _, w := range stored {
		if d := digestOf(w.Path); d != w.Digest {
			changed = w.Path
			break
		}
	}
	// ...
	if changed != "" {
		return cacheLookup{key: key, verdict: cacheStale, reason: changed, before: digestAll(paths)}
	}
	return cacheLookup{key: key, text: e.text, verdict: cacheHit, millis: e.millis}
}
```

Note `rc.mu.Unlock()` before the hashing loop. Witness checking reads files, and
holding a mutex across disk I/O would serialise four concurrent subagents behind
whichever one is reading the largest file.

Four verdicts, not two: `refused`, `miss`, `stale`, `hit`. `stale` is separated
from `miss` because they mean different things to whoever is reading the panel —
a miss says "not seen before", a stale says "seen, and the world moved".

---

## Run it

```sh
go test ./12-echo/code/ -run 'TestKey|TestEligible|TestWitness|TestSed' -v
```

Then the property tests behind the measurements below:

```sh
go test ./12-echo/code/ -run TestMtime -v
go test ./12-echo/code/ -run TestSameLengthRewrite -v
```

And by hand, which is more convincing:

```sh
cd sandbox
printf 'route2:x\n' > f.txt
../agent --yolo --cache -p "cat f.txt, then cat f.txt again"
```

**What to watch for:** the second `cat` is a hit. Now do it with a rewrite in
between — `printf 'route3:y\n' > f.txt` — and it is `stale`, not a hit, and the
panel names the path that changed.

---

## Measured

### What `(size, mtime)` cannot see

```text
P2 same-length rewrite, back to back
   1498/2000 rewrites were invisible to (size, mtime)
   the bytes differ every time: "route2:x" -> "route3:y"
```

**~75% in that run.** Five consecutive reruns of the same 2000 trials: 1440,
1442, 1449, 1456, 1457 — about 72%. One taken while the machine was busy: 1087,
54%.

So the headline figure is not quotable to three digits, and saying so is part of
the result: the number moves with how fast the process is allowed to run.

### Why: the modification time steps in half-milliseconds

```text
P1 mtime granularity
   1897 writes in 300ms produced 555 distinct mtimes
   smallest gap between two distinct mtimes: 501200 ns (501.2µs)
   median gap: 527000 ns (527µs)
   writes that landed on an mtime a previous write already had: 1342
```

**1342 of 1897 = 70.7%** of writes landed on a timestamp a previous write already
had.

Steps of about half a millisecond — **not** NTFS's stored 100 ns resolution.
This is the rate at which the filesystem actually updates the stamp, which is a
property of the running system rather than of the format.

### What the content hash costs

Both halves taken in the same round, warm cache:

```text
file                                  bytes        stat   read+hash   ratio
go.mod                                  149      16.9µs      34.3µs    2.0x
docs/00-loop.md                       11801      16.9µs        40µs    2.4x
docs/wire-notes.md                    49616      17.8µs      68.1µs    3.8x
```

**2× to 4×, not 100×.** And the number that matters is not the ratio: 68 µs on a
50 kB file against a 92 ms median command is **0.07%**.

On Linux a `stat` is much cheaper than 17 µs, so the ratio there is worse — and
still microseconds against tens of milliseconds.

The whole overhead, measured:

| | |
|---|---|
| `eligible` — tokenize and check a piped command | 759 ns |
| `keyOf` — hash command, cwd, shell, budget, environment | 11.7 µs |
| a complete miss on a 50 KB file, both witness hashes | **73.7 µs** |

73.7 µs against a 92 ms median command: **0.08%**. The correct witness is
affordable, which removes the only argument for the cheap one.

---

## Next

The mechanism is correct. [Part 3](3-off.md) wires it into the loop — where the
lookup goes relative to the permission gate, what a hit must *not* emit, and why
the panel prints a line full of zeros.
