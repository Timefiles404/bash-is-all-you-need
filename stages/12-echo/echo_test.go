package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

// echoDir 开一块临时目录，按命令行里的写法交出去。所有平台一律用正斜
// 杠：echo.go 的分词器跟 shell 一样把反斜杠当转义，于是拿反斜杠写的
// Windows 路径，走到文件系统那儿已经是另一串字节了——况且 Git Bash 本
// 来也要正斜杠。
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
// 什么可以进缓存
// ---------------------------------------------------------------------------

// 这张表里的命令不是编出来的。本仓库自己那份 trace 收藏里存了 16 段
// 会话，这些就是其中出现过的所有不同形态。模型手里只有一件工具时到底
// 会敲什么，能拿到的样本就这一份。
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

// 这里每一条都是规则本该做出的拒绝。这份清单就是规格：凡是可能写入、
// 可能拉起第二个程序、可能展开成规则没见过的名字，或者可能读到没有任
// 何参数点名的东西，一律拒掉。
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

// 挤在一起的短 flag，模型天天这么写：`grep -oE`、`grep -noiE`。
//
// 只有每个字母都是这条规则本来就允许的布尔 flag，才收下；所以一串里夹了
// 一个不认识的字母，仍然为那个字母被拒，而结尾是个带值的 flag 的串也是
// 拒掉，不去猜。这个分支之所以存在，是因为一次审计的拒绝清单在同一次会
// 话里点了它三遍——一类漏网和一次意外，长得就是不一样。
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
		// -P 不在 grep 的清单上，所以带着它的那一串照样被拒。
		{"grep -oP pattern notes.md", "unknown flag -oP"},
		// -m 要带值；把它挤进串里就意味着要猜值在哪儿。
		{"grep -om pattern notes.md", "unknown flag -om"},
		// -r 是故意不在的：递归 grep 的见证集合是一整棵树。
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

// 这次拒绝是错的，而且是故意的。
//
// `sed -n '/word/p'` 只读文件，什么都不写。规则拒了它，因为它分不出
// "word" 里的 `w` 和 sed 那个会写文件的 `w` 命令，而要分清就得自己写
// sed 解析器。这个测试摆在这儿，是为了哪天有人来"修"它，得先删掉一条
// 写明这是刻意为之的断言。
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

// sed 脚本是程序，不是路径。它要是混进见证集合，摘要就是 ""——这种见证
// 永远匹配不上——那条目也就永远失效：缓存一次都不命中，还从不说为什么。
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

// 加了引号的 Windows 路径。工作目录是拿反斜杠报给模型的，模型就会这么
// 写——录下来的某段会话里，四条命令有三条长这样。
//
// 双引号里，反斜杠只对 $ ` " \ 和换行起转义作用。见反斜杠就删的分词器
// 会把 `D:\Projects\notes.md` 变成 `D:Projectsnotes.md`，这路径根本不
// 存在，于是它的摘要永远是 ""，见证什么也没看着。没有报错，没有日志，
// 真文件怎么变，那条目都照活不误。
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
	if d := digestOf(paths[0]); d == "" {
		t.Fatalf("the witness %q hashes to nothing, so it can never go stale; the backslashes were "+
			"eaten and the path names no file", paths[0])
	}

	// 真正的转义还得照常管用。
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

// ls 不带参数读的是工作目录，那工作目录就是见证。没有 cwdIsInput，它
// 会因为没点名任何路径被拒；而样本里的 `ls`，从来都是不带参数跑的。
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
// 什么算同一条命令
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

	// 还有那件**不能**改变 key 的事，否则什么都别想复用。
	if keyOf("/bin/bash", "/w", "cat a.md", 8000, []string{"PATH=/usr/bin"}) != base {
		t.Error("two identical calls produced different keys")
	}
	// 环境变量的顺序不携带信息。
	two := []string{"A=1", "B=2"}
	if keyOf("/bin/bash", "/w", "c", 1, two) != keyOf("/bin/bash", "/w", "c", 1, []string{"B=2", "A=1"}) {
		t.Error("the key depends on the order os.Environ() happened to return")
	}
}

// ---------------------------------------------------------------------------
// 失效
// ---------------------------------------------------------------------------

// 重头戏：改写之后文件长度没变。
//
// 这里的 mtime 是直接按住不动的，不是去抢时序，所以测试断言的是那条真
// 命题，而不是概率。这形态也不是假想出来的：在这台机器上，2000 次自然
// 的连续同长度改写里，有 1498 次本来就没人帮忙也从 (size, mtime) 看不
// 出差别——mtime 是按大约半毫秒一档在走的。
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

// 这个自然比例只报不断言：它取决于测试碰巧跑在哪台机器上。
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

// 目录的见证是它一层的列表，所以目录里冒出新文件就会让 `ls` 失效——
// `ls` 能进缓存，全靠这一条。
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

// 只把名字算进去的目录摘要能过上面那个测试，却仍然是错的，因为
// `ls -l` 还要打印大小和日期。下面这个用例把两者分开：没东西出现，没
// 东西消失，只有某个文件变大了。
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

// store() 会拒掉判定为命中的 lookup。
//
// runCommand 在那之前就返回了，所以这道防线拦的是将来的调用方，不是眼
// 下跑着的路径——这恰恰是它需要测试的理由：把它删掉，整套测试里没有别
// 的东西会察觉。它拦下的是见证集合为**空**的条目：什么都没盯着，也就
// 永远不会失效，此后无论磁盘上发生什么，这一整场会话都拿它来应答。
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

	// 命中要是被存了下来，这条目现在就没有见证，改写也够不着它。
	if err := os.WriteFile(filepath.FromSlash(p), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil).verdict; v != cacheStale {
		t.Fatalf("lookup after a rewrite = %s, want stale. Storing a hit replaced a watched entry "+
			"with one that watches nothing", v)
	}
}

// ---------------------------------------------------------------------------
// 什么不会被存下来
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
// 两道上限
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

	// 另一道上限，这回把第一道敞开。两道上限并存，是因为它们见底的时机
	// 不同：四百条 40 字节的答案先撑爆条数，四条大的先撑爆字节数。
	byBytes := newResultCache(1000, 100, 0)
	for i := 0; i < 6; i++ {
		store(byBytes, i, strings.Repeat("x", 40))
	}
	if got := byBytes.snapshot().Evicted; got == 0 {
		t.Error("byte-capped cache evicted nothing after storing 240 bytes into a 100-byte budget")
	}
}

// ---------------------------------------------------------------------------
// 没有症状的缓存 bug
// ---------------------------------------------------------------------------

// TTL 比两次相同调用之间的间隔还短，那么每一条目被问到的时候都早已过
// 期。命中率是准准的零，而且哪儿都不报问题：没有错答案，没有报错，没
// 有日志。有个上线的 Agent 拿 15 秒的 TTL 去配 30 秒一次的重取，这么
// 跑了好几个月，每一次都照付 0.3 秒的 git 命令。
//
// 这里的比例照搬，单位换成毫秒，于是测试花掉五分之一秒，不是三分钟。
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
	// 关键就在这儿。上面这一切，按每一项信号看都是运转正常的缓存，只有
	// 一项例外，而那一项是得有人去打印出来的计数器。
	if st.Hits == 0 && st.Stored > 0 {
		t.Logf("%d lookups, %d stores, %d hits, and not one error anywhere", st.Lookups, st.Stored, st.Hits)
	}
}

// 不设 TTL，失效就由内容说了算，同样的序列除了第一次之外次次命中。这
// 是上面那个测试的对照组：两者的差别只在一个字段。
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
// 拒绝不等于未命中
// ---------------------------------------------------------------------------

// 十次未命中，说明缓存还凉着。十次拒绝，说明这套负载不管跑多久缓存都
// 帮不上忙。把两者一并记成"没命中"，这个区别就被盖住了，而它们的修法
// 并不一样。
func TestRefusalsAreCountedApartFromMisses(t *testing.T) {
	dir := echoDir(t)
	echoWrite(t, dir, "notes.md", "hello\n")

	rc := newResultCache(64, 1<<20, 0)
	rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)       // 未命中
	rc.lookup("/bin/bash", dir, "cat notes.md > out", 8000, nil) // 拒绝
	rc.lookup("/bin/bash", dir, "curl http://x", 8000, nil)      // 拒绝

	st := rc.snapshot()
	if st.Lookups != 3 {
		t.Fatalf("lookups = %d, want 3", st.Lookups)
	}
	if st.Refused != 2 {
		t.Errorf("refused = %d, want 2", st.Refused)
	}
}

// ---------------------------------------------------------------------------
// 从循环内部看
// ---------------------------------------------------------------------------

// 上面测的全是缓存本身。没有哪一条能证明循环真去问过它——这是阶段 11
// 的教训：那里有个变异体删掉了一次调用，却在覆盖率满格的测试套件下活
// 了下来。

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

	// 两次告诉模型的必须是同一件事。拿删节版答案来应答的缓存是另一种功
	// 能，故障模式也另算。
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

// 命中并没有跑命令，那 trace 里就不能说跑了。下游每一处计数——面板、
// 回放头、以后谁再写的东西——读的都是 command_start 和 command_end，
// 而报出根本不存在的进程的 trace，算不上证据。
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

// 缓存是在权限门之后问的，不是之前。命中照样是往模型眼前送字节；权限
// 系统要是因为某条命令批过一次就不再问，会话跑得越久，它就越松。
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

// 缓存按指针共享给子 Agent。见 newChild：结果缓存明摆着划算的场合就是
// 这一种，而阶段 10 就因为那个结构体字面量里漏了个字段，整个功能没了。
func TestAChildSharesItsParentsResultCache(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")
	a.echo = newResultCache(8, 1<<20, 0)

	child := a.newChild("kid", func() string { return "sys" })
	if child.echo != a.echo {
		t.Fatal("the child got a different result cache; three children reading the same file would " +
			"miss on every one of them")
	}
}

// 不开缓存，这个 Agent 就跟阶段 11 分毫不差。每个方法都容得下 nil 接
// 收者，为的是让关掉缓存那条路只是一处分支，而不是 runCommand 的第二
// 份实现。
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

// 两条相同的命令之间文件被改写了，从循环里看就必须得出两个不同的答
// 案——而且隔着整整两个用户回合，刚被压缩过的模型正是在那里把同一个问
// 题又问了一遍。
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

// 上面那个测试最初要查的规则，单独拎出来。
//
// 命令读文件读到一半文件变了，得出的结果描述的是这文件从未处在过的状
// 态。要是拿文件最终的摘要把它存下来，下一次 lookup 就会匹配上并拿它
// 应答——缓存自信地错着，直到有人再动这文件为止。store() 会把见证再算
// 一遍摘要，两次对不上就什么都不留。
func TestAFileThatChangedUnderTheCommandIsNotStored(t *testing.T) {
	dir := echoDir(t)
	p := echoWrite(t, dir, "notes.md", "before\n")

	rc := newResultCache(16, 1<<20, 0)
	look := rc.lookup("/bin/bash", dir, "cat notes.md", 8000, nil)
	if len(look.before) != 1 {
		t.Fatalf("lookup carried %d witness digests, want 1: without them store cannot compare", len(look.before))
	}

	// 顶替一次在命令读取期间跑起来的写入。
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
