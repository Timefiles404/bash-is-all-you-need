package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// echoDir makes a scratch directory and returns it in the form a command line
// uses. Forward slashes on every platform: the tokenizer in echo.go treats a
// backslash as an escape, exactly as a shell does, so a Windows path written
// with backslashes is a different string by the time it reaches the file
// system — and Git Bash wants forward slashes anyway.
func echoDir(t *testing.T) string {
	t.Helper()
	return filepath.ToSlash(t.TempDir())
}

func echoWrite(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(p)
}

func okResult(ms int) execResult {
	return execResult{ExitCode: 0, Duration: time.Duration(ms) * time.Millisecond}
}

// ---------------------------------------------------------------------------
// What may be cached
// ---------------------------------------------------------------------------

// The commands in this table are not invented. They are the distinct shapes
// that appeared across the sixteen recorded sessions in this repo's own trace
// collection, which is the only sample available of what a model actually
// types when it has one tool.
func TestEligibleAcceptsTheShapesRealSessionsRan(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	for _, cmd := range []string{
		"ls -la",
		"cat notes.md",
		"wc -l notes.md",
		"sed -n '1,150p' notes.md",
		"sed -n '151,300p' notes.md | grep -n '^##' ",
		"head -20 notes.md",
		"tail -n 5 notes.md",
		`cat "notes.md"`,
		"grep -n '^##' notes.md",
	} {
		if _, ok, why := eligible(cmd, dir); !ok {
			t.Errorf("eligible(%q) refused: %s", cmd, why)
		}
	}
}

// Every entry here is a refusal the rule is supposed to make. The list is the
// specification: anything that could write, run a second program, expand into
// a name the rule did not see, or read something no argument names.
func TestEligibleRefusesEverythingItDoesNotUnderstand(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	for _, tc := range []struct{ cmd, contains string }{
		{"cat notes.md > out.txt", "unsupported shell character"},
		{"cat notes.md; rm -rf /", "unsupported shell character"},
		{"cat notes.md && echo ok", "unsupported shell character"},
		{"echo $(whoami)", "unsupported shell character"},
		{"cat `ls`", "unsupported shell character"},
		{"cat $HOME/notes.md", "unsupported shell character"},
		{"cat *.md", "unsupported shell character"},
		{"cat notes.md | tee copy.md", "not a known read-only program: tee"},
		{"curl https://example.com", "not a known read-only program: curl"},
		{"date", "not a known read-only program: date"},
		{"find . -name '*.md'", "not a known read-only program: find"},
		{"sed -i 's/a/b/' notes.md", "unknown flag -i"},
		{"sort -o out.txt notes.md", "unknown flag -o"},
		{"grep -r pattern .", "unknown flag -r"},
		{"cat", "no path named"},
		{"cat 'unterminated", "unterminated single quote"},
	} {
		_, ok, why := eligible(tc.cmd, dir)
		if ok {
			t.Errorf("eligible(%q) accepted it; expected a refusal mentioning %q", tc.cmd, tc.contains)
			continue
		}
		if !strings.Contains(why, tc.contains) {
			t.Errorf("eligible(%q) refused with %q, expected something mentioning %q", tc.cmd, why, tc.contains)
		}
	}
}

// Bundled short flags, which models write constantly: `grep -oE`, `grep -noiE`.
//
// Accepted only when every letter is a boolean flag the rule already permits,
// so a bundle carrying one unknown letter is still refused for that letter, and
// a bundle ending in a flag that takes a value is refused rather than guessed
// at. This case exists because an audit's refusal tally named it three times in
// one session, which is what a class of misses looks like next to an accident.
func TestBundledShortFlagsAreSplit(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	for _, cmd := range []string{
		"grep -oE pattern notes.md",
		"grep -noiE pattern notes.md",
		"ls -lah",
	} {
		if _, ok, why := eligible(cmd, dir); !ok {
			t.Errorf("eligible(%q) refused: %s", cmd, why)
		}
	}
	for _, tc := range []struct{ cmd, contains string }{
		// -P is not on grep's list, so the bundle carrying it is refused.
		{"grep -oP pattern notes.md", "unknown flag -oP"},
		// -m takes a value; bundling it would mean guessing where the value is.
		{"grep -om pattern notes.md", "unknown flag -om"},
		// -r is absent on purpose: a recursive grep's witness set is a tree.
		{"grep -rn pattern .", "unknown flag -rn"},
	} {
		_, ok, why := eligible(tc.cmd, dir)
		if ok {
			t.Errorf("eligible(%q) accepted it", tc.cmd)
			continue
		}
		if !strings.Contains(why, tc.contains) {
			t.Errorf("eligible(%q) refused with %q, want %q", tc.cmd, why, tc.contains)
		}
	}
}

// This refusal is wrong and is meant to be.
//
// `sed -n '/word/p'` reads a file and writes nothing. The rule refuses it
// because it cannot tell the `w` inside "word" from sed's `w` command, which
// writes a file, and telling them apart means writing a sed parser. The test
// exists so that the day someone "fixes" this, they have to delete an
// assertion that says why it is deliberate.
func TestASedScriptWithTheLetterWIsRefusedOnPurpose(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	if _, ok, _ := eligible("sed -n '/word/p' notes.md", dir); ok {
		t.Error("accepted a sed script containing 'w'. The rule is allowed to be stupid in exactly one " +
			"direction: a false refusal costs one command, a false acceptance writes to the user's disk " +
			"and then serves the write from a cache")
	}
	if _, ok, _ := eligible("sed -n '1,5w out.txt' notes.md", dir); ok {
		t.Error("accepted a sed script that writes a file")
	}
}

// A sed script is a program, not a path. If it landed in the witness set it
// would hash to "" — a witness that can never match — and the entry would be
// permanently stale: a cache that never hits and never says why.
func TestASedScriptIsNotAWitness(t *testing.T) {
	dir := echoDir(t)
	f := echoWrite(t, dir, "notes.md", "hello\n")

	paths, ok, why := eligible("sed -n '1,150p' notes.md", dir)
	if !ok {
		t.Fatalf("refused: %s", why)
	}
	if len(paths) != 1 || filepath.ToSlash(paths[0]) != f {
		t.Fatalf("witnesses = %v, want exactly [%s]", paths, f)
	}
}

// A quoted Windows path, which is what a model writes when the working
// directory was reported to it with backslashes — three of the four commands in
// one recorded session look exactly like this.
//
// Inside double quotes a backslash escapes only $ ` " \ and a newline. A
// tokenizer that strips every backslash turns `D:\Projects\notes.md` into
// `D:Projectsnotes.md`, which never exists, so its digest is "" for ever and
// the witness watches nothing. Nothing fails, nothing is logged, and the entry
// survives any change to the real file.
func TestABackslashInDoubleQuotesIsNotAlwaysAnEscape(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")
	win := strings.ReplaceAll(filepath.Join(dir, "notes.md"), "/", `\`)

	paths, ok, why := eligible(`cat "`+win+`"`, dir)
	if !ok {
		t.Fatalf("refused: %s", why)
	}
	if len(paths) != 1 {
		t.Fatalf("witnesses = %v, want one path", paths)
	}

	// The claim, and it holds everywhere: inside double quotes a backslash
	// escapes only $ ` " \ and a newline, so every one of these survives into
	// the argument. That is shell grammar, not a property of this machine.
	//
	// Counted rather than compared, because the two platforms disagree about
	// what happens next and neither disagreement is the point. On Windows the
	// argument is already absolute and arrives unchanged; on Linux `\tmp\…` is a
	// relative name, so it gets joined onto the working directory first. Either
	// way a tokenizer that ate the backslashes would show up here as a smaller
	// number.
	if got, want := strings.Count(paths[0], `\`), strings.Count(win, `\`); got != want {
		t.Fatalf("the witness %q has %d backslashes, expected %d from %q — some were eaten",
			paths[0], got, want, win)
	}

	// Whether that argument then names a file is a property of this machine,
	// and only on Windows does it. Asserting the digest everywhere is what this
	// test used to do, and it passed for four months because it was only ever
	// run on Windows: on Linux the same string is one filename with backslashes
	// in it, naming nothing, and the digest is correctly empty.
	if runtime.GOOS == "windows" {
		if d := digestOf(paths[0]); d == "" {
			t.Fatalf("the witness %q hashes to nothing, so it can never go stale", paths[0])
		}
	}

	// And an escape that really is one still works.
	if _, ok, _ := eligible(`cat "a\"b.md"`, dir); !ok {
		t.Error(`refused cat "a\"b.md", where the backslash escapes a quote`)
	}
}

func TestEveryStageOfAPipelineContributesWitnesses(t *testing.T) {
	dir := echoDir(t)
	a := echoWrite(t, dir, "a.md", "x\n")
	b := echoWrite(t, dir, "b.md", "y\n")

	paths, ok, why := eligible("cat a.md | grep -n x b.md", dir)
	if !ok {
		t.Fatalf("refused: %s", why)
	}
	got := map[string]bool{}
	for _, p := range paths {
		got[filepath.ToSlash(p)] = true
	}
	if !got[a] || !got[b] {
		t.Fatalf("witnesses = %v, want both %s and %s", paths, a, b)
	}
}

// ls with no argument reads the working directory, so the working directory is
// the witness. Without cwdIsInput it would be refused for naming no path, and
// `ls` is the one command in the sample that is always run with no argument.
func TestLsWithNoArgumentWitnessesTheWorkingDirectory(t *testing.T) {
	dir := echoDir(t)
	paths, ok, why := eligible("ls -la", dir)
	if !ok {
		t.Fatalf("refused: %s", why)
	}
	if len(paths) != 1 || filepath.ToSlash(paths[0]) != filepath.ToSlash(filepath.Clean(dir)) {
		t.Fatalf("witnesses = %v, want [%s]", paths, dir)
	}
}

// ---------------------------------------------------------------------------
// What counts as the same command
// ---------------------------------------------------------------------------

func TestTheKeySeparatesThingsThatChangeTheAnswer(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	base := keyOf("/bin/bash", "/w", "cat a.md", 8000, env)

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"a different working directory", keyOf("/bin/bash", "/other", "cat a.md", 8000, env)},
		{"a different shell", keyOf("/bin/sh", "/w", "cat a.md", 8000, env)},
		{"a different output budget", keyOf("/bin/bash", "/w", "cat a.md", 4000, env)},
		{"a different environment", keyOf("/bin/bash", "/w", "cat a.md", 8000, []string{"PATH=/bin"})},
		{"a different command", keyOf("/bin/bash", "/w", "cat b.md", 8000, env)},
	} {
		if tc.key == base {
			t.Errorf("%s produced the same key", tc.name)
		}
	}

	// And the one thing that must NOT change it, or nothing is ever reusable.
	if keyOf("/bin/bash", "/w", "cat a.md", 8000, []string{"PATH=/usr/bin"}) != base {
		t.Error("two identical calls produced different keys")
	}
	// Environment order is not information.
	two := []string{"A=1", "B=2"}
	if keyOf("/bin/bash", "/w", "c", 1, two) != keyOf("/bin/bash", "/w", "c", 1, []string{"B=2", "A=1"}) {
		t.Error("the key depends on the order os.Environ() happened to return")
	}
}

// ---------------------------------------------------------------------------
// Staleness
// ---------------------------------------------------------------------------

// The headline: a rewrite that keeps the file the same length.
//
// The mtime is forced identical here rather than raced for, so the test asserts
// the real claim instead of a probability. It is not a hypothetical shape. On
// this machine 1498 of 2000 natural back-to-back same-length rewrites were
// already indistinguishable through (size, mtime) without any help, because the
// mtime moves in steps of about half a millisecond.
func TestASameLengthRewriteWithTheSameMtimeIsStillCaught(t *testing.T) {
	dir := echoDir(t)
	p := echoWrite(t, dir, "route.conf", "route2:x")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	rc := newResultCache(16, 1<<20, 0)
	cmd := "cat route.conf"
	look := rc.lookup("/bin/bash", dir, cmd, 8000, nil)
	if look.verdict != cacheMiss {
		t.Fatalf("cold lookup = %s, want miss", look.verdict)
	}
	rc.store(look, cmd, "route2:x\n[exit 0 · 1ms]", okResult(90))

	if err := os.WriteFile(p, []byte("route3:y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the premise of this test did not hold: (size, mtime) went from (%d, %v) to (%d, %v)",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}

	look = rc.lookup("/bin/bash", dir, cmd, 8000, nil)
	if look.verdict != cacheStale {
		t.Fatalf("lookup after a same-length rewrite = %s, want stale.\nThe file now holds different bytes "+
			"and every cheap witness — size, mtime, both together — says it does not", look.verdict)
	}
	if !strings.HasSuffix(filepath.ToSlash(look.reason), "route.conf") {
		t.Errorf("stale reason = %q, want the path that changed", look.reason)
	}
}

// The natural rate, reported rather than asserted, because it is a property of
// the machine the test happens to run on.
func TestReportTheNaturalSameLengthCollisionRate(t *testing.T) {
	dir := echoDir(t)
	p := filepath.Join(dir, "race.conf")
	blind, trials := 0, 300
	for i := 0; i < trials; i++ {
		os.WriteFile(p, []byte("route2:x"), 0o644)
		a, _ := os.Stat(p)
		os.WriteFile(p, []byte("route3:y"), 0o644)
		b, _ := os.Stat(p)
		if a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) {
			blind++
		}
	}
	t.Logf("(size, mtime) could not see %d of %d same-length rewrites on this machine", blind, trials)
}

func TestADeletedWitnessIsStale(t *testing.T) {
	dir := echoDir(t)
	p := echoWrite(t, dir, "notes.md", "hello\n")

	rc := newResultCache(16, 1<<20, 0)
	look := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
	rc.store(look, "cat notes.md", "hello", okResult(90))

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if v := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil).verdict; v != cacheStale {
		t.Fatalf("lookup after deleting the witness = %s, want stale", v)
	}
}

// A directory's witness is its one-level listing, so a file appearing in it
// invalidates an `ls` — which is the only reason `ls` is cacheable at all.
func TestANewFileInvalidatesAnLs(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "a.md", "x")

	rc := newResultCache(16, 1<<20, 0)
	look := rc.lookup("/bin/bash", dir, "ls -la", 8000, nil)
	rc.store(look, "ls -la", "a.md", okResult(90))

	if v := rc.lookup("/bin/bash", dir, "ls -la", 8000, nil).verdict; v != cacheHit {
		t.Fatalf("second lookup with nothing changed = %s, want hit", v)
	}
	echoWrite(t, dir, "b.md", "y")
	if v := rc.lookup("/bin/bash", dir, "ls -la", 8000, nil).verdict; v != cacheStale {
		t.Fatalf("lookup after a new file appeared = %s, want stale", v)
	}
}

// A directory digest that hashed only the names would pass the test above and
// still be wrong, because `ls -l` prints sizes and dates. This is the case that
// separates the two: nothing appears, nothing disappears, one file grows.
func TestAFileGrowingInsideTheDirectoryInvalidatesAnLs(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "a.md", "x")

	rc := newResultCache(16, 1<<20, 0)
	look := rc.lookup("/bin/bash", dir, "ls -la", 8000, nil)
	rc.store(look, "ls -la", "a.md", okResult(90))

	echoWrite(t, dir, "a.md", "xxxxxxxxxxxxxxxx")
	if v := rc.lookup("/bin/bash", dir, "ls -la", 8000, nil).verdict; v != cacheStale {
		t.Fatalf("lookup after a file in the directory changed size = %s, want stale.\n"+
			"The listing has the same names and `ls -l` would print a different number", v)
	}
}

// store() refuses a lookup whose verdict was a hit.
//
// runCommand returns before it could ever happen, so this is a guard against a
// future caller rather than a live path — which is exactly why it needs a test:
// nothing else in the suite would notice it being deleted. What it prevents is
// an entry with an EMPTY witness set, one that can never go stale because it is
// not watching anything, and which would then be served for the rest of the
// session no matter what happened on disk.
func TestStoringAHitWouldCreateAWitnessLessEntry(t *testing.T) {
	dir := echoDir(t)
	p := echoWrite(t, dir, "notes.md", "first\n")

	rc := newResultCache(16, 1<<20, 0)
	miss := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
	rc.store(miss, "cat notes.md", "first", okResult(90))

	hit := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
	if hit.verdict != cacheHit {
		t.Fatalf("second lookup = %s, want hit", hit.verdict)
	}
	rc.store(hit, "cat notes.md", "first", okResult(90))

	// If the hit was stored, the entry now has no witnesses and a rewrite
	// cannot reach it.
	if err := os.WriteFile(filepath.FromSlash(p), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil).verdict; v != cacheStale {
		t.Fatalf("lookup after a rewrite = %s, want stale. Storing a hit replaced a watched entry "+
			"with one that watches nothing", v)
	}
}

// ---------------------------------------------------------------------------
// What is not stored
// ---------------------------------------------------------------------------

func TestOutcomesAreNotStored(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	for _, tc := range []struct {
		name string
		r    execResult
	}{
		{"a non-zero exit", execResult{ExitCode: 1}},
		{"a timeout", execResult{ExitCode: -1, TimedOut: true}},
		{"a cancellation", execResult{ExitCode: -1, Cancelled: true}},
		{"an unreaped process tree", execResult{ExitCode: -1, Unreaped: true}},
	} {
		rc := newResultCache(16, 1<<20, 0)
		look := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
		rc.store(look, "cat notes.md", "whatever it printed", tc.r)

		if v := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil).verdict; v == cacheHit {
			t.Errorf("%s was stored and then served. An exit code is an outcome, not an answer, and the "+
				"outcomes that repeat are the ones you least want frozen", tc.name)
		}
	}
}

// ---------------------------------------------------------------------------
// The bounds
// ---------------------------------------------------------------------------

func TestEvictionIsByEntryCountAndAlsoByBytes(t *testing.T) {
	dir := echoDir(t)
	for i := 0; i < 6; i++ {
		echoWrite(t, dir, fmt.Sprintf("f%d.md", i), "x")
	}
	store := func(rc *resultCache, i int, text string) {
		cmd := fmt.Sprintf("cat f%d.md", i)
		look := rc.lookup("/bin/bash", dir, cmd, 8000, nil)
		rc.store(look, cmd, text, okResult(1))
	}

	byCount := newResultCache(3, 1<<20, 0)
	for i := 0; i < 6; i++ {
		store(byCount, i, "tiny")
	}
	if got := byCount.snapshot().Evicted; got != 3 {
		t.Errorf("entry-capped cache evicted %d, want 3", got)
	}
	if v := byCount.lookup("/bin/bash", dir, "cat f0.md", 8000, nil).verdict; v == cacheHit {
		t.Error("the least recently used entry survived an entry-count eviction")
	}
	if v := byCount.lookup("/bin/bash", dir, "cat f5.md", 8000, nil).verdict; v != cacheHit {
		t.Error("the most recently used entry was evicted first; the list is the wrong way round")
	}

	// The other bound, with the first one wide open. Two caps because they run
	// out at different times: four hundred 40-byte answers exhaust the count,
	// four large ones exhaust the bytes.
	byBytes := newResultCache(1000, 100, 0)
	for i := 0; i < 6; i++ {
		store(byBytes, i, strings.Repeat("x", 40))
	}
	if got := byBytes.snapshot().Evicted; got == 0 {
		t.Error("byte-capped cache evicted nothing after storing 240 bytes into a 100-byte budget")
	}
}

// ---------------------------------------------------------------------------
// The cache bug with no symptom
// ---------------------------------------------------------------------------

// A TTL shorter than the interval between two identical calls means every entry
// has always expired by the time it is asked for. The hit rate is exactly zero,
// and nothing anywhere reports a problem: no wrong answers, no errors, no log
// line. A shipped agent ran a 15-second TTL against a 30-second refetch for
// months and paid a 0.3-second git command every single time.
//
// The proportions here are the same and the units are milliseconds, so the test
// takes a fifth of a second rather than three minutes.
func TestATTLShorterThanTheGapNeverHits(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	rc := newResultCache(64, 1<<20, 15*time.Millisecond)
	for i := 0; i < 5; i++ {
		look := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
		if look.verdict == cacheHit {
			t.Fatalf("round %d hit; this test is meant to demonstrate that it cannot", i)
		}
		rc.store(look, "cat notes.md", "hello", okResult(300))
		time.Sleep(30 * time.Millisecond)
	}

	st := rc.snapshot()
	if st.Hits != 0 {
		t.Fatalf("hits = %d, want 0", st.Hits)
	}
	if st.Expired != 4 {
		t.Errorf("expired = %d, want 4 — one for every lookup after the first", st.Expired)
	}
	if st.Stored != 5 {
		t.Errorf("stored = %d, want 5: the cache did all of the work and none of the saving", st.Stored)
	}
	// The whole point. Everything above is a functioning cache by every signal
	// except one, and the one is a counter somebody has to print.
	if st.Hits == 0 && st.Stored > 0 {
		t.Logf("%d lookups, %d stores, %d hits, and not one error anywhere", st.Lookups, st.Stored, st.Hits)
	}
}

// With no TTL, staleness is decided by content, and the same sequence hits
// every time after the first. This is the control arm for the test above: the
// difference between them is one field.
func TestWithoutATTLTheSameSequenceHits(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	rc := newResultCache(64, 1<<20, 0)
	for i := 0; i < 5; i++ {
		look := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
		if i > 0 && look.verdict != cacheHit {
			t.Fatalf("round %d = %s, want hit", i, look.verdict)
		}
		rc.store(look, "cat notes.md", "hello", okResult(300))
		time.Sleep(30 * time.Millisecond)
	}
	if st := rc.snapshot(); st.Hits != 4 {
		t.Fatalf("hits = %d, want 4", st.Hits)
	}
}

// ---------------------------------------------------------------------------
// Refused is not missed
// ---------------------------------------------------------------------------

// Ten misses is a cold cache. Ten refusals is a cache that will never help with
// this workload no matter how long it runs. Counting both as "not a hit" hides
// the difference, and they have different fixes.
func TestRefusalsAreCountedApartFromMisses(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	rc := newResultCache(64, 1<<20, 0)
	rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)       // miss
	rc.lookup("/bin/bash", dir, "cat notes.md > out", 8000, nil) // refused
	rc.lookup("/bin/bash", dir, "curl http://x", 8000, nil)      // refused

	st := rc.snapshot()
	if st.Lookups != 3 {
		t.Fatalf("lookups = %d, want 3", st.Lookups)
	}
	if st.Refused != 2 {
		t.Errorf("refused = %d, want 2", st.Refused)
	}
}

// ---------------------------------------------------------------------------
// From inside the loop
// ---------------------------------------------------------------------------

// Everything above tests the cache. None of it proves the loop asks it
// anything — which is stage 11's lesson, learned there from a mutant that
// deleted a call and survived a fully covered test suite.

func echoAgent(t *testing.T, dir string, script ...*CallResult) (*agent, *mulRecorder, *scriptProvider) {
	t.Helper()
	a, rec, p := scriptAgent(t, script...)
	a.cfg.wd = dir
	a.echo = newResultCache(64, 1<<20, 0)
	return a, rec, p
}

func TestTheLoopServesARepeatedCommandFromTheCache(t *testing.T) {
	dir := echoDir(t)
	f := echoWrite(t, dir, "notes.md", "the file contents\n")
	cmd := "cat " + f

	a, rec, _ := echoAgent(t, dir,
		callResult(StopToolUse, "tool_use", "", toolCall("c1", "bash", mulBash(cmd))),
		callResult(StopToolUse, "tool_use", "", toolCall("c2", "bash", mulBash(cmd))),
		callResult(StopEndTurn, "end_turn", "done"),
	)
	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if got := rec.count(KindCommandStart); got != 1 {
		t.Fatalf("command_start count = %d, want 1: the second call was run instead of being served", got)
	}
	hits := 0
	for _, e := range rec.kind(KindResultCache) {
		if e.Verdict == string(cacheHit) {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("cache hits = %d, want 1", hits)
	}

	// The model must be told the same thing both times. A cache that serves an
	// abbreviated answer is a different feature with a different failure mode.
	var results []string
	for _, e := range rec.kind(KindToolResult) {
		results = append(results, e.Text)
	}
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(results))
	}
	if results[0] != results[1] {
		t.Errorf("the cached result differs from the one the command produced:\n first: %q\nsecond: %q",
			results[0], results[1])
	}
	if !strings.Contains(results[1], "the file contents") {
		t.Errorf("second result = %q, want the file's contents", results[1])
	}
}

// A hit did not run a command, so the trace must not say one ran. Every count
// downstream — the panel, the replay header, anything anyone writes later —
// reads command_start and command_end, and a trace that reports processes that
// never existed is not evidence.
func TestAHitEmitsNoCommandEvents(t *testing.T) {
	dir := echoDir(t)
	f := echoWrite(t, dir, "notes.md", "x\n")
	cmd := "cat " + f

	a, rec, _ := echoAgent(t, dir,
		callResult(StopToolUse, "tool_use", "", toolCall("c1", "bash", mulBash(cmd))),
		callResult(StopToolUse, "tool_use", "", toolCall("c2", "bash", mulBash(cmd))),
		callResult(StopEndTurn, "end_turn", "done"),
	)
	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if got := rec.count(KindCommandEnd); got != 1 {
		t.Errorf("command_end count = %d, want 1", got)
	}
	if got := Summarize(rec.events).Commands; got != 1 {
		t.Errorf("the replay header reports %d commands, want 1", got)
	}
	if got := Summarize(rec.events).CacheHits; got != 1 {
		t.Errorf("the replay header reports %d cache hits, want 1", got)
	}
}

// The cache is consulted after the gate, not before. A hit is still bytes
// arriving in front of the model, and a permission system that stops asking
// about a command because it was approved once gets weaker the longer a session
// runs.
func TestACachedCommandStillGoesThroughTheGate(t *testing.T) {
	dir := echoDir(t)
	f := echoWrite(t, dir, "notes.md", "x\n")
	cmd := "cat " + f

	a, rec, _ := echoAgent(t, dir,
		callResult(StopToolUse, "tool_use", "", toolCall("c1", "bash", mulBash(cmd))),
		callResult(StopToolUse, "tool_use", "", toolCall("c2", "bash", mulBash(cmd))),
		callResult(StopEndTurn, "end_turn", "done"),
	)
	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if got := rec.count(KindGateVerdict); got != 2 {
		t.Fatalf("gate verdicts = %d, want 2: the second command was served without being asked about", got)
	}
}

// The cache is shared with children by pointer. See newChild: this is the one
// case where a result cache clearly pays, and stage 10 lost a whole feature by
// leaving a field out of that struct literal.
func TestAChildSharesItsParentsResultCache(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")
	a.echo = newResultCache(8, 1<<20, 0)

	child := a.newChild("kid", func() string { return "sys" })
	if child.echo != a.echo {
		t.Fatal("the child got a different result cache; three children reading the same file would " +
			"miss on every one of them")
	}
}

// With no cache the agent is stage 11 exactly. Every method tolerates a nil
// receiver so that the disabled path is one branch rather than a second
// implementation of runCommand.
func TestWithNoCacheEveryCommandRuns(t *testing.T) {
	dir := echoDir(t)
	f := echoWrite(t, dir, "notes.md", "x\n")
	cmd := "cat " + f

	a, rec, _ := scriptAgent(t,
		callResult(StopToolUse, "tool_use", "", toolCall("c1", "bash", mulBash(cmd))),
		callResult(StopToolUse, "tool_use", "", toolCall("c2", "bash", mulBash(cmd))),
		callResult(StopEndTurn, "end_turn", "done"),
	)
	a.cfg.wd = dir
	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if got := rec.count(KindCommandStart); got != 2 {
		t.Errorf("command_start count = %d, want 2", got)
	}
	if got := rec.count(KindResultCache); got != 0 {
		t.Errorf("result_cache events = %d with the cache off, want 0", got)
	}
}

// A file rewritten between two identical commands must produce two different
// answers, from inside the loop — two whole user turns apart, which is where a
// model that has just been compacted asks the same question again.
func TestTheLoopRerunsACommandWhoseFileChanged(t *testing.T) {
	dir := echoDir(t)
	f := echoWrite(t, dir, "notes.md", "first\n")
	cmd := "cat " + f

	a, rec, _ := echoAgent(t, dir,
		callResult(StopToolUse, "tool_use", "", toolCall("c1", "bash", mulBash(cmd))),
		callResult(StopEndTurn, "end_turn", "one"),
		callResult(StopToolUse, "tool_use", "", toolCall("c2", "bash", mulBash(cmd))),
		callResult(StopEndTurn, "end_turn", "two"),
	)
	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "read it")})
	echoWrite(t, dir, "notes.md", "second\n")
	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "read it again")})

	results := rec.kind(KindToolResult)
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(results))
	}
	if !strings.Contains(results[1].Text, "second") {
		t.Errorf("the second result is %q; a rewritten file was served from the cache", results[1].Text)
	}
	if got := rec.count(KindCommandStart); got != 2 {
		t.Errorf("command_start count = %d, want 2: the stale entry was served instead of re-run", got)
	}
}

// The rule that the test above was originally written to check, isolated.
//
// A command whose file changes while it is being read produces a result that
// describes no state the file was ever in. Storing it against the digest the
// file ended up with would make the next lookup match and serve it — the cache
// confidently wrong until something touches the file again. store() hashes the
// witnesses a second time and keeps nothing when the two disagree.
func TestAFileThatChangedUnderTheCommandIsNotStored(t *testing.T) {
	dir := echoDir(t)
	p := echoWrite(t, dir, "notes.md", "before\n")

	rc := newResultCache(16, 1<<20, 0)
	look := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
	if len(look.before) != 1 {
		t.Fatalf("lookup carried %d witness digests, want 1: without them store cannot compare", len(look.before))
	}

	// Stand in for a writer that ran while the command was reading.
	if err := os.WriteFile(filepath.FromSlash(p), []byte("during\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc.store(look, "cat notes.md", "a torn read", okResult(90))

	if st := rc.snapshot(); st.Stored != 0 {
		t.Fatalf("stored = %d, want 0", st.Stored)
	}
	if v := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil).verdict; v == cacheHit {
		t.Error("a result read from a file that was changing underneath it was stored and then served")
	}
}
