// Stage 12 — not running the command again.
//
// A word of warning before anything else: this repo now has two unrelated
// things called a cache, and confusing them will cost you an afternoon.
//
//	stage 04's cache  is the PROVIDER's prompt cache. It lives on their side,
//	                  it is billed, and hitRate() in render.go measures it.
//	stage 12's cache  is ours. It lives in this process, it stores the text a
//	                  command produced, and nothing about it reaches the wire.
//
// The idea is one sentence: if the model asks for a command we already ran, and
// nothing that command read has changed since, hand back the answer instead of
// running it again.
//
// Every hard part is in the second half of that sentence. "We already ran it"
// needs a definition of when two commands are the same command. "Nothing it
// read has changed" needs to know what it read, and the tool is bash, so in
// general you cannot know. Stage 08 made the same discovery from the security
// side: you cannot decide what a shell command will do by reading it. The
// difference is which way the two are allowed to fail. A blocklist that fails
// open runs something dangerous. A whitelist that fails closed runs the command
// — which is what would have happened anyway. So this file is built entirely
// out of refusals, and the price of that is measured in 12-echo/doc/.
package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The verdict
// ---------------------------------------------------------------------------

// cacheVerdict is what happened when we looked a command up.
//
// Four values, one event kind — unlike compaction and triage, which each got
// three kinds. The test is whether the values answer the same question. These
// do: "was this command run, and if not why not". The precedent is
// KindGateVerdict, which carries allow/deny/abort the same way.
//
// refused and miss are separate on purpose, and the split is the whole reason
// this type exists rather than a bool. A miss says the cache could have helped
// and did not have the answer yet; ten misses in a row is a cold cache warming
// up. A refusal says the cache will never help with this command no matter how
// often it is run, and a session that is all refusals means the eligibility
// rules are too narrow for the work being done — a completely different
// problem, invisible if you count both as "not a hit".
type cacheVerdict string

const (
	cacheHit     cacheVerdict = "hit"
	cacheMiss    cacheVerdict = "miss"
	cacheStale   cacheVerdict = "stale"   // we had it; something it read changed
	cacheRefused cacheVerdict = "refused" // not eligible, and never will be
)

// ---------------------------------------------------------------------------
// Witnesses
// ---------------------------------------------------------------------------

// A witness is a path whose contents the cached answer depends on, together
// with the digest that path had when the command ran.
//
// Digest is the content hash for a file and the hash of a one-level listing for
// a directory — names, sizes, modes and mtimes, which is roughly what `ls -l`
// prints and therefore roughly what an `ls` result depends on.
//
// It is NOT (size, mtime), and that is a measurement rather than a preference.
// On this machine, two back-to-back writes of "route2:x" and "route3:y" — same
// length, different bytes — were indistinguishable through (size, mtime) in
// 1498 of 2000 trials, because the mtime this filesystem hands back moves in
// steps of about half a millisecond and a rewrite lands inside one step. The
// content hash costs 2 to 4 times what a stat costs (17µs against 34–68µs on
// files from 149 B to 50 KB), which buys correctness for a rounding error on a
// command whose median is 92 ms.
type witness struct {
	Path   string
	Digest string
}

// digestOf hashes a path the way its readers see it.
//
// A missing path is not an error: it returns "" and no error, so that a witness
// which disappears compares unequal to the digest recorded when the file
// existed. Returning an error here would make "the file was deleted" a failure
// of the cache rather than a fact about the world, and the caller would have to
// treat two different things the same way.
func digestOf(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		ents, err := os.ReadDir(path)
		if err != nil {
			return ""
		}
		h := sha256.New()
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		sort.Strings(names) // ReadDir already sorts; do not rely on it staying so
		for _, n := range names {
			sub, err := os.Lstat(filepath.Join(path, n))
			if err != nil {
				fmt.Fprintf(h, "%s\x00?\x00", n)
				continue
			}
			fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00", n, sub.Size(), sub.Mode(), sub.ModTime().UnixNano())
		}
		return "d:" + hex.EncodeToString(h.Sum(nil))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "f:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// The cache
// ---------------------------------------------------------------------------

type echoEntry struct {
	key       string
	command   string
	text      string // exactly the bytes the model was told the first time
	witnesses []witness
	millis    int64 // what the command cost when it really ran
	stored    time.Time
	el        *list.Element
}

// resultCache is an LRU over command results, shared by an agent and all of its
// subagents.
//
// Shared by pointer, deliberately, and it is the one place a shared cache
// clearly pays: stage 07 fans several children out at once over the same
// working tree, and three children that each open the same file are three
// identical commands separated by microseconds. A per-agent cache would miss
// every one of them.
//
// Bounded twice — by entry count and by total bytes — because the two run out
// at different times. A session that runs `wc -l` over four hundred files fills
// the entry count with 40-byte answers; a session that reads four large files
// fills the byte budget with four entries. One bound leaves the other unguarded,
// and an unbounded result cache in a long session is a memory leak with good
// intentions.
type resultCache struct {
	mu      sync.Mutex
	entries map[string]*echoEntry
	order   *list.List // front = most recently used
	bytes   int

	maxEntries int
	maxBytes   int

	// ttl is a backstop, off by default, and the comment matters more than the
	// field.
	//
	// The witness set is a LOWER BOUND on what a command read: it holds the
	// paths named on the command line, and a command can depend on things no
	// path names. A TTL bounds how long a wrong answer can survive that gap.
	//
	// What it must never be is the primary mechanism, and there is a field
	// report for why. An agent cached a `git log` behind a 15-second TTL while
	// its caller refetched every 30 seconds; every entry had always expired by
	// the time it was asked for, so the hit rate was exactly zero — for months,
	// with no wrong answers, no errors and nothing in any log. A cache that
	// never hits is indistinguishable from a cache that works unless you count
	// the hits, which is why this file counts them and prints the count.
	ttl time.Duration

	stats cacheStats
}

type cacheStats struct {
	Lookups     int
	Hits        int
	Stale       int
	Refused     int
	Expired     int
	Stored      int
	Evicted     int
	BytesServed int
	SavedMillis int64
}

func newResultCache(maxEntries, maxBytes int, ttl time.Duration) *resultCache {
	return &resultCache{
		entries:    map[string]*echoEntry{},
		order:      list.New(),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		ttl:        ttl,
	}
}

// keyOf is the definition of "the same command".
//
// Everything that can change the answer goes in, including four values that
// cannot actually change while this process runs: the shell, the working
// directory, the output budget and the environment. Putting a constant in a key
// looks like superstition. It is insurance against the next feature: the day
// someone persists this cache to a file, or gives a subagent its own working
// directory, those four stop being constants, and a key that was only
// accidentally correct starts returning one directory's answer for another's.
//
// maxOutput is in there because the stored text is the RENDERED result, already
// truncated to fit. The same command under a different --max-output produces
// different bytes, and they are the bytes the model reads.
//
// What is deliberately not in the key: the time, the turn number, the agent
// that asked. If those mattered the entry would not be reusable at all.
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

// cacheLookup is one answer, in the shape the caller needs to both act on it
// and describe it in the trace.
//
// key is here because the caller needs it again on the way back to store the
// result, and recomputing it would mean hashing the environment twice per
// command for no reason.
//
// millis is what this command cost on the day it really ran, and it is the only
// honest way to say what a hit saved. The alternative — timing the lookup and
// calling the difference a saving — measures nothing, because the command that
// was not run has no duration to subtract from.
//
// before holds the witness digests taken BEFORE the command runs, and it is the
// half of this struct that took a failing test to arrive at. See store.
type cacheLookup struct {
	key     string
	text    string
	verdict cacheVerdict
	reason  string
	millis  int64
	before  []witness
}

// lookup answers three questions at once: may we cache this at all, do we have
// it, and is what we have still true.
func (rc *resultCache) lookup(shell, wd, command string, maxOutput int, env []string) cacheLookup {
	if rc == nil {
		return cacheLookup{verdict: cacheRefused, reason: "cache disabled"}
	}
	paths, ok, why := eligible(command, wd)
	if !ok {
		rc.mu.Lock()
		rc.stats.Lookups++
		rc.stats.Refused++
		rc.mu.Unlock()
		return cacheLookup{verdict: cacheRefused, reason: why}
	}

	key := keyOf(shell, wd, command, maxOutput, env)

	rc.mu.Lock()
	rc.stats.Lookups++
	e, have := rc.entries[key]
	if !have {
		rc.mu.Unlock()
		return cacheLookup{key: key, verdict: cacheMiss, before: digestAll(paths)}
	}
	if rc.ttl > 0 && time.Since(e.stored) > rc.ttl {
		rc.dropLocked(e)
		rc.stats.Expired++
		rc.mu.Unlock()
		return cacheLookup{key: key, verdict: cacheMiss, reason: "expired", before: digestAll(paths)}
	}
	stored := e.witnesses
	rc.mu.Unlock()

	// Hashing happens outside the lock. It touches the disk, and holding a
	// mutex across a filesystem call in a cache that several subagents share is
	// how a cache becomes slower than the thing it replaced.
	//
	// The race this opens is real and benign: another goroutine may evict this
	// entry while we are hashing. The recheck below is what makes that safe,
	// and its failure mode is one wasted lookup rather than a wrong answer.
	changed := ""
	for _, w := range stored {
		if d := digestOf(w.Path); d != w.Digest {
			changed = w.Path
			break
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, have = rc.entries[key]
	if !have {
		return cacheLookup{key: key, verdict: cacheMiss, before: digestAll(paths)}
	}
	if changed != "" {
		rc.dropLocked(e)
		rc.stats.Stale++
		return cacheLookup{key: key, verdict: cacheStale, reason: changed, before: digestAll(paths)}
	}
	rc.order.MoveToFront(e.el)
	rc.stats.Hits++
	rc.stats.BytesServed += len(e.text)
	rc.stats.SavedMillis += e.millis
	return cacheLookup{key: key, text: e.text, verdict: cacheHit, millis: e.millis}
}

func witnessPaths(ws []witness) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Path)
	}
	return out
}

func digestAll(paths []string) []witness {
	ws := make([]witness, 0, len(paths))
	for _, p := range paths {
		ws = append(ws, witness{Path: p, Digest: digestOf(p)})
	}
	return ws
}

// store keeps a result, and refuses far more often than it keeps.
//
// The refusals are the interesting half:
//
//   - A non-zero exit is not stored. An exit code is an outcome, not an answer,
//     and the outcomes that repeat are the ones you least want frozen: a
//     permission blip, a file being written as it was read, a disk that filled
//     for a minute. There is a field report of the opposite choice — an index
//     that cached parse FAILURES keyed on the content hash, so fixing the
//     parser re-indexed nothing, because every file still hashed to the same
//     bytes that had failed. That was a deliberate trade of correctness for
//     termination; this is the other side of it.
//   - A timeout, a cancellation or an unreaped process is not stored, for the
//     same reason and more strongly. None of those texts describes what the
//     command does; they describe what happened to it once.
//   - An empty key means lookup refused the command, so there is nothing to
//     store it under.
//   - A file that changed while the command was reading it is not stored, and
//     this is the one that had to be found rather than reasoned out. See below.
//
// The witnesses are hashed twice: once in lookup, before the command runs, and
// again here, after it has. Both are necessary, and neither on its own is
// enough.
//
// Take only the digest from AFTER the read. A file that changes mid-read gives
// a torn result, and that result is then stored against the digest the file
// ended up with — so the next lookup finds a matching witness and serves a
// result that never corresponded to any state of the file. The cache is now
// confidently wrong, and stays wrong until the file changes again.
//
// Take only the digest from BEFORE the read and nothing can be served wrongly,
// but a file that changed between the lookup and the command is stored against
// a digest it no longer has, so the entry is stale the moment it is created and
// every future lookup re-runs the command. Safe, and silently useless.
//
// Comparing the two costs one extra hash — 68µs against a 92 ms median command
// — and refusing to store when they disagree is the only version that is
// neither wrong nor pointless.
func (rc *resultCache) store(look cacheLookup, command, text string, r execResult) {
	if rc == nil || look.key == "" {
		return
	}
	// A hit carries no `before` digests, because on a hit nothing runs and
	// there is nothing to compare. Storing against it would write an entry with
	// an EMPTY witness set — one that can never go stale, because it is not
	// watching anything. runCommand returns before it gets here, so this guard
	// is unreachable today; it is here because the next caller will not know
	// that, and the failure it prevents is silent and permanent.
	if look.verdict == cacheHit {
		return
	}
	if r.ExitCode != 0 || r.TimedOut || r.Cancelled || r.Unreaped {
		return
	}
	ws := digestAll(witnessPaths(look.before))
	if len(ws) != len(look.before) {
		return
	}
	for i := range ws {
		if ws[i] != look.before[i] {
			return // it changed under the command; this text describes nothing
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if old, ok := rc.entries[look.key]; ok {
		rc.dropLocked(old)
	}
	key := look.key
	e := &echoEntry{
		key: key, command: command, text: text, witnesses: ws,
		millis: r.Duration.Milliseconds(), stored: time.Now(),
	}
	e.el = rc.order.PushFront(e)
	rc.entries[key] = e
	rc.bytes += len(text)
	rc.stats.Stored++

	for (rc.maxEntries > 0 && rc.order.Len() > rc.maxEntries) ||
		(rc.maxBytes > 0 && rc.bytes > rc.maxBytes) {
		back := rc.order.Back()
		if back == nil {
			break
		}
		rc.dropLocked(back.Value.(*echoEntry))
		rc.stats.Evicted++
	}
}

func (rc *resultCache) dropLocked(e *echoEntry) {
	rc.order.Remove(e.el)
	delete(rc.entries, e.key)
	rc.bytes -= len(e.text)
}

func (rc *resultCache) snapshot() cacheStats {
	if rc == nil {
		return cacheStats{}
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.stats
}

// ---------------------------------------------------------------------------
// Auditing a cache against sessions that already happened
// ---------------------------------------------------------------------------

// cacheAudit answers the only question worth asking before switching a cache
// on: would it have helped?
//
// It needs no API key and no model. Every trace this repo can produce records
// the exact command of every tool call, in order, with what it cost — which is
// all a cache decision depends on. Replaying those commands through a cold
// cache gives the hit rate you would have had, on your own work rather than on
// somebody's benchmark.
//
// The one thing the trace does not hold is the text each command printed, only
// its length, so the stored bodies here are filler of the right size. That
// affects the byte accounting not at all and the hit accounting not at all: a
// key is built from the command, never from the result.
//
// What it cannot model is a file changing mid-session, because a trace records
// what the agent ran and not what the disk did underneath it. The number it
// reports is therefore an upper bound on hits, and the chapter says so.
func cacheAudit(paths []string, wd string, out io.Writer) error {
	fmt.Fprintf(out, "%-24s %6s %6s %6s %6s %6s %10s\n",
		"trace", "cmds", "hit", "miss", "stale", "refus", "saved")
	var tot cacheStats
	var totCmds int
	// Why the refusals happened, tallied across every trace. This is the half
	// of the report you act on: a hit rate is a verdict, and a refusal reason
	// is a to-do list — it names the rule that would have to grow, and lets you
	// see whether growing it would be worth anything.
	reasons := map[string]int{}
	for _, p := range paths {
		events, err := ReadTrace(p)
		if err != nil {
			return err
		}
		rc := newResultCache(1<<20, 1<<30, 0)
		n := 0
		for _, e := range events {
			if e.Kind != KindCommandEnd {
				continue
			}
			n++
			look := rc.lookup("audit", wd, e.Command, 8000, nil)
			if look.verdict == cacheHit {
				continue
			}
			if look.verdict == cacheRefused {
				reasons[look.reason]++
			}
			rc.store(look, e.Command, strings.Repeat("x", e.Bytes),
				execResult{ExitCode: e.ExitCode, TimedOut: e.TimedOut,
					Duration: time.Duration(e.Millis) * time.Millisecond})
		}
		s := rc.snapshot()
		fmt.Fprintf(out, "%-24s %6d %6d %6d %6d %6d %10v\n", filepath.Base(p), n,
			s.Hits, s.Lookups-s.Hits-s.Stale-s.Refused, s.Stale, s.Refused,
			time.Duration(s.SavedMillis)*time.Millisecond)
		totCmds += n
		tot.Lookups += s.Lookups
		tot.Hits += s.Hits
		tot.Stale += s.Stale
		tot.Refused += s.Refused
		tot.BytesServed += s.BytesServed
		tot.SavedMillis += s.SavedMillis
	}
	fmt.Fprintf(out, "%-24s %6d %6d %6d %6d %6d %10v\n", "TOTAL", totCmds,
		tot.Hits, tot.Lookups-tot.Hits-tot.Stale-tot.Refused, tot.Stale, tot.Refused,
		time.Duration(tot.SavedMillis)*time.Millisecond)
	if tot.Lookups > 0 {
		fmt.Fprintf(out, "\nhit rate %.1f%% of %d commands · %s of output not re-read · %v of command time not re-run\n",
			float64(tot.Hits)*100/float64(tot.Lookups), totCmds,
			humanBytes(tot.BytesServed), time.Duration(tot.SavedMillis)*time.Millisecond)
	}
	if len(reasons) > 0 {
		keys := make([]string, 0, len(reasons))
		for k := range reasons {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if reasons[keys[i]] != reasons[keys[j]] {
				return reasons[keys[i]] > reasons[keys[j]]
			}
			return keys[i] < keys[j]
		})
		fmt.Fprintf(out, "\nrefused, by reason:\n")
		for _, k := range keys {
			fmt.Fprintf(out, "  %3d  %s\n", reasons[k], k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Eligibility
// ---------------------------------------------------------------------------

// eligible decides whether a command may be cached, and if so which paths its
// answer depends on.
//
// It is a parser that refuses everything it does not completely understand.
// That is the opposite shape from the regexp in stage 08, which tried to spot
// the dangerous forms and let the rest through; a rule of that shape has to be
// right about every command that exists, and this one only has to be right
// about the ones it accepts.
//
// The consequences are deliberately visible. `sed -n '/word/p'` is refused,
// because the rule cannot tell the `w` inside "word" from sed's `w` command,
// which writes a file. A false refusal costs one command — 92 ms, at the median
// measured across sixteen real sessions. A false acceptance writes to the
// user's disk and then serves the write from a cache. The rule is allowed to be
// stupid in exactly one direction.
func eligible(command, wd string) (paths []string, ok bool, why string) {
	stages, err := splitPipeline(command)
	if err != nil {
		return nil, false, err.Error()
	}
	if len(stages) == 0 {
		return nil, false, "empty command"
	}
	seen := map[string]bool{}
	for i, argv := range stages {
		rule, known := readers[argv[0]]
		if !known {
			return nil, false, "not a known read-only program: " + argv[0]
		}
		args, err := rule.check(argv[1:])
		if err != nil {
			return nil, false, argv[0] + ": " + err.Error()
		}
		if i == 0 && len(args) == 0 && !rule.cwdIsInput {
			// The head of a pipeline with no file argument reads standard
			// input, which runBash sets to nothing. That is deterministic, but
			// only because of a decision made in another file, and a cache that
			// depends on a detail like that is one refactor from being wrong.
			return nil, false, argv[0] + ": no path named"
		}
		if len(args) == 0 && rule.cwdIsInput {
			args = []string{"."}
		}
		for _, a := range args {
			p := a
			if !filepath.IsAbs(p) {
				p = filepath.Join(wd, p)
			}
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths, true, ""
}

// readerRule is one program and everything it is allowed to be told.
//
// The unit of the whitelist is not the program. `sed` reads and `sed -i`
// writes; `sort` reads and `sort -o` writes. A list of program names would have
// been wrong for two of the six programs on it.
type readerRule struct {
	// flags that take no value, and flags that swallow the next argument.
	boolFlags  map[string]bool
	valueFlags map[string]bool

	// cwdIsInput marks a program whose input with no argument is the working
	// directory rather than standard input. `ls` is the only one here.
	cwdIsInput bool

	// scriptArgs is how many leading non-flag arguments are a program rather
	// than a path — 1 for sed and grep, 0 for everything else. Getting this
	// wrong would put a sed script in the witness set as if it were a file, and
	// the witness would hash to "" forever, which reads as permanently stale:
	// a cache that never hits and never says why.
	scriptArgs int

	// scriptSafe screens those arguments. Nil means no screening is needed.
	scriptSafe func(string) error
}

var readers = map[string]readerRule{
	"cat": {boolFlags: set("-n", "-b", "-s", "-A", "-e", "-t", "-v", "-E", "-T")},
	"head": {boolFlags: set("-q", "-v", "-z"),
		valueFlags: set("-n", "-c")},
	"tail": {boolFlags: set("-q", "-v", "-z"),
		valueFlags: set("-n", "-c")},
	"wc":  {boolFlags: set("-l", "-w", "-c", "-m", "-L")},
	"nl":  {boolFlags: set("-p"), valueFlags: set("-b", "-w", "-s", "-v", "-i")},
	"cut": {boolFlags: set("-s", "-n"), valueFlags: set("-d", "-f", "-b", "-c", "--output-delimiter")},
	"ls":  {boolFlags: set("-l", "-a", "-A", "-h", "-t", "-r", "-S", "-1", "-la", "-al", "-lh", "-lha", "-alh", "-ltr", "-F", "-d"), cwdIsInput: true},
	// sort -o writes a file, so -o is not on the list and an unknown flag is a
	// refusal rather than a shrug.
	"sort": {boolFlags: set("-n", "-r", "-u", "-h", "-b", "-f", "-V", "-g"),
		valueFlags: set("-k", "-t", "-S")},
	"uniq": {boolFlags: set("-c", "-d", "-u", "-i"), valueFlags: set("-f", "-s", "-w")},

	"sed": {
		boolFlags:  set("-n", "-E", "-r"),
		valueFlags: set("-e"),
		scriptArgs: 1,
		scriptSafe: sedScriptSafe,
	},
	"grep": {
		// -r and -R are absent on purpose. Their witness set is a whole tree,
		// which is not something a path list can hold, and a witness set that
		// is quietly incomplete is worse than no cache: it produces confident
		// stale answers instead of slow correct ones.
		boolFlags:  set("-n", "-i", "-c", "-v", "-E", "-F", "-l", "-L", "-h", "-H", "-o", "-w", "-x", "-s", "-a", "-q"),
		valueFlags: set("-m", "-A", "-B", "-C", "-e"),
		scriptArgs: 1,
		scriptSafe: nil,
	},
}

// check validates flags and returns the arguments that are paths.
func (r readerRule) check(args []string) ([]string, error) {
	var paths []string
	scriptsLeft := r.scriptArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, rest := range args[i+1:] {
				paths = append(paths, rest)
			}
			return paths, nil
		case strings.HasPrefix(a, "-") && a != "-":
			name := a
			if eq := strings.IndexByte(a, '='); eq > 0 {
				name = a[:eq]
			}
			if r.valueFlags[name] {
				if name == a { // value is the next argument
					if i+1 >= len(args) {
						return nil, fmt.Errorf("flag %s has no value", a)
					}
					if name == "-e" && r.scriptSafe != nil {
						if err := r.scriptSafe(args[i+1]); err != nil {
							return nil, err
						}
						scriptsLeft = 0
					}
					i++
				}
				continue
			}
			if r.boolFlags[name] && name == a {
				continue
			}
			// A numeric shorthand: head -20, tail -5. Not a path, and not a
			// flag this rule has to know about individually.
			if isNumericFlag(a) && (r.valueFlags["-n"] || r.valueFlags["-c"]) {
				continue
			}
			// Bundled short flags: `grep -oE` is -o and -E, and models write
			// them constantly. Accepted only when EVERY letter is a boolean
			// flag this rule already permits, so `grep -oP` is still refused
			// for the -P, and a bundle ending in a flag that takes a value is
			// refused rather than guessed at.
			//
			// This case is here because a refusal tally asked for it. Three
			// commands in one recorded session were refused as `unknown flag
			// -oE`, `-oP` and `-noiE`, which named a whole class rather than
			// three accidents — and that is the difference between a reason
			// worth acting on and one worth living with.
			if bundleOK(a, r) {
				continue
			}
			return nil, fmt.Errorf("unknown flag %s", a)
		case scriptsLeft > 0:
			scriptsLeft--
			if r.scriptSafe != nil {
				if err := r.scriptSafe(a); err != nil {
					return nil, err
				}
			}
		default:
			paths = append(paths, a)
		}
	}
	return paths, nil
}

// bundleOK reports whether `-abc` is three boolean flags this rule permits.
func bundleOK(a string, r readerRule) bool {
	if len(a) < 3 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for _, c := range a[1:] {
		if c > 127 || !r.boolFlags["-"+string(c)] {
			return false
		}
	}
	return true
}

func isNumericFlag(a string) bool {
	if len(a) < 2 || a[0] != '-' {
		return false
	}
	for _, c := range a[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// sedScriptSafe refuses any script containing a letter that could name a sed
// command which touches a file or a process.
//
//	w  write the pattern space to a file          W  write the first line
//	r  read a file in                             R  read one line of a file
//	e  execute a shell command (GNU extension)
//
// It looks at every character, not at command positions, so `/word/p` and
// `s/read/x/` are refused along with `1,5w out.txt`. That is the cost of not
// writing a sed parser to decide whether a cache may skip a 92 ms command, and
// it is the right way round: the refusal is measured in milliseconds and the
// alternative is measured in the user's files.
func sedScriptSafe(script string) error {
	if i := strings.IndexAny(script, "wWrRe"); i >= 0 {
		return fmt.Errorf("script contains %q, which could be a file or exec command", script[i])
	}
	return nil
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// ---------------------------------------------------------------------------
// The tokenizer
// ---------------------------------------------------------------------------

// splitPipeline turns a command into pipeline stages of arguments, or an error
// naming the first thing it did not understand.
//
// It understands single quotes, double quotes, backslash escapes and the pipe
// operator, and nothing else. Every other shell construct is an error, which
// means every construct that could redirect output, run a second command,
// substitute a command's output, expand a variable or open a subshell is an
// error. That list is short only because the accepted grammar is small.
//
// This is not a shell and must not grow into one. If it ever needs to
// understand `$(...)` to be useful, the honest fix is stage 08's real parser,
// not another case in this switch.
func splitPipeline(command string) ([][]string, error) {
	var stages [][]string
	var argv []string
	var cur strings.Builder
	quoted := false // this argument has been started, even if it is empty

	flush := func() {
		if cur.Len() > 0 || quoted {
			argv = append(argv, cur.String())
			cur.Reset()
			quoted = false
		}
	}
	endStage := func() error {
		flush()
		if len(argv) == 0 {
			return fmt.Errorf("empty pipeline stage")
		}
		stages = append(stages, argv)
		argv = nil
		return nil
	}

	for i := 0; i < len(command); i++ {
		c := command[i]
		switch c {
		case '\'':
			quoted = true
			j := strings.IndexByte(command[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(command[i+1 : i+1+j])
			i += j + 1
		case '"':
			quoted = true
			// A double-quoted string is only safe while it contains none of the
			// two characters that make it a program rather than a literal.
			//
			// The backslash rule is not the same as the one outside quotes, and
			// getting it wrong is how this function silently produced paths
			// that do not exist. Inside double quotes bash treats a backslash
			// as an escape only before $ ` " \ and a newline; before anything
			// else it is an ordinary character. So `cat "D:\Projects\x.md"` is
			// a Windows path, and a tokenizer that strips every backslash turns
			// it into `D:Projectsx.md` — a path that hashes to nothing, for
			// ever, no matter what happens to the real file. Nothing fails. The
			// witness is simply not watching anything.
			for i++; i < len(command) && command[i] != '"'; i++ {
				switch command[i] {
				case '$', '`':
					return nil, fmt.Errorf("substitution inside double quotes")
				case '\\':
					if i+1 >= len(command) {
						return nil, fmt.Errorf("trailing backslash")
					}
					if next := command[i+1]; next == '$' || next == '`' || next == '"' || next == '\\' || next == '\n' {
						i++
					}
					cur.WriteByte(command[i])
					continue
				}
				cur.WriteByte(command[i])
			}
			if i >= len(command) {
				return nil, fmt.Errorf("unterminated double quote")
			}
		case '\\':
			if i+1 >= len(command) {
				return nil, fmt.Errorf("trailing backslash")
			}
			i++
			quoted = true
			cur.WriteByte(command[i])
		case ' ', '\t':
			flush()
		case '|':
			if i+1 < len(command) && command[i+1] == '|' {
				return nil, fmt.Errorf("|| is a control operator")
			}
			if err := endStage(); err != nil {
				return nil, err
			}
		case '$', '`', ';', '&', '<', '>', '(', ')', '{', '}', '\n', '\r', '#', '*', '?', '[', ']', '~', '!':
			// Globs are in this list with the control operators, and for a
			// related reason: `cat *.md` names a set of files that the shell
			// resolves, so the paths this function returns would not be the
			// paths the command reads, and a new file appearing in the
			// directory would change the answer without changing any witness.
			return nil, fmt.Errorf("unsupported shell character %q", string(c))
		default:
			cur.WriteByte(c)
		}
	}
	if err := endStage(); err != nil {
		return nil, err
	}
	return stages, nil
}
