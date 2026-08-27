package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 辅助代码
// ---------------------------------------------------------------------------

// memRecorder 收集 loadMemory 往总线上说的话。记忆只在启动时加载一次，
// 进的是 prompt 里之后再没人看见的那一块——所以这个事件是用户手上
// 唯一的证据，证明这件事真的发生过。
type memRecorder struct{ events []Event }

func (r *memRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

// memWrite 往 dir 里丢一个文件。这个文件里每个碰文件系统的测试都在
// t.TempDir() 里干活；这里的任何东西都不许靠近仓库自己的 AGENTS.md 和
// MEMORY.md——那是真人在维护的真文件。
func memWrite(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// memWhen 是固定的时刻，这样 <now> 的断言比的是一个字面量，而不是又推
// 导一遍格式串。
var memWhen = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const memWhenLine = "<now>2026-01-02 03:04:05 +0000</now>"

// ---------------------------------------------------------------------------
// loadMemory
// ---------------------------------------------------------------------------

// 新目录里没有记忆文件，这才是常态。这里只要返回的不是空字符串，那些从
// 没用过这个功能的会话，系统提示词的最前面就都会多出几个野字节。
func TestLoadMemoryOnAnEmptyDirectory(t *testing.T) {
	got, found := loadMemory(t.TempDir(), nil)
	if got != "" {
		t.Errorf("a directory with no memory files produced %q, which would be prepended to the system prompt of every session", got)
	}
	if len(found) != 0 {
		t.Errorf("reported %v as loaded from a directory containing neither file", found)
	}
}

// 两个文件都在，按文档里的顺序，各自裹在自己的标签块里。
//
// 顺序是契约的一部分：AGENTS.md 是人的指令，MEMORY.md 是 Agent 自己的
// 笔记；两边冲突时，模型更看重靠后的那一块。把它们倒过来，就悄无声息
// 地让 Agent 的猜测盖过了操作者写下的东西。
func TestLoadMemoryReturnsBothFilesInTheDocumentedOrder(t *testing.T) {
	dir := t.TempDir()
	memWrite(t, dir, "AGENTS.md", "# Conventions\n\nDo not touch generated/.\n")
	memWrite(t, dir, "MEMORY.md", "\n- (2026-08-01) the build script lives in tools/build.sh\n")

	got, found := loadMemory(dir, nil)

	agents := strings.Index(got, `<memory file="AGENTS.md">`)
	memory := strings.Index(got, `<memory file="MEMORY.md">`)
	if agents < 0 {
		t.Fatalf("AGENTS.md is not wrapped in its own <memory file=...> block:\n%s", got)
	}
	if memory < 0 {
		t.Fatalf("MEMORY.md is not wrapped in its own <memory file=...> block:\n%s", got)
	}
	if agents > memory {
		t.Error("MEMORY.md was placed before AGENTS.md; the agent's own notes now sit closer to the end of the system prompt " +
			"than the human's instructions, which is the wrong way round when they contradict each other")
	}
	if !strings.Contains(got, "Do not touch generated/.") || !strings.Contains(got, "tools/build.sh") {
		t.Errorf("a file was tagged but its contents did not make it in:\n%s", got)
	}
	if strings.Count(got, "</memory>") != 2 {
		t.Errorf("expected two closed <memory> blocks, got:\n%s", got)
	}
	if len(found) != 2 || found[0] != "AGENTS.md" || found[1] != "MEMORY.md" {
		t.Errorf("found = %v; the caller reports this list to the user, so it has to match what was actually injected", found)
	}
}

// 空文件是 `touch AGENTS.md` 留下的东西，只有空白的文件是编辑器在最后
// 一条笔记被删掉之后留下的。这两种只要注入进去，都在花 prompt 的字节，
// 还告诉模型：有一份约定文件，里面什么都没有。
func TestLoadMemorySkipsEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	memWrite(t, dir, "AGENTS.md", "   \n\t\n\n")
	memWrite(t, dir, "MEMORY.md", "- (2026-08-01) something real\n")

	got, found := loadMemory(dir, nil)
	if strings.Contains(got, "AGENTS.md") {
		t.Errorf("a whitespace-only AGENTS.md was injected anyway:\n%s", got)
	}
	if len(found) != 1 || found[0] != "MEMORY.md" {
		t.Errorf("found = %v, want just MEMORY.md", found)
	}
	if !strings.Contains(got, "something real") {
		t.Errorf("the non-empty file was dropped along with the empty one:\n%s", got)
	}
}

// prompt 前缀里进了什么，用户唯一的窗口就是这个事件；而 nil 总线对应的
// 是渲染器还没接上的那段启动路径。
func TestLoadMemoryEmitsOneEventPerFileAndToleratesANilBus(t *testing.T) {
	dir := t.TempDir()
	memWrite(t, dir, "AGENTS.md", "conventions")
	memWrite(t, dir, "MEMORY.md", "notes")

	rec := &memRecorder{}
	loadMemory(dir, NewBus(rec))

	var loaded []string
	for _, e := range rec.events {
		if e.Kind == KindMemoryLoaded {
			loaded = append(loaded, e.Path)
		}
	}
	if len(loaded) != 2 {
		t.Fatalf("%d memory_loaded events for two files; the user cannot tell what was injected", len(loaded))
	}
	if filepath.Base(loaded[0]) != "AGENTS.md" || filepath.Base(loaded[1]) != "MEMORY.md" {
		t.Errorf("events name %v; they must carry the full path so the file can be opened from the trace", loaded)
	}

	// 不能 panic：main.go 加载记忆的时候，一个订阅者都还不存在。
	if _, found := loadMemory(dir, nil); len(found) != 2 {
		t.Errorf("loading with a nil bus found %v", found)
	}
}

// ---------------------------------------------------------------------------
// remember
// ---------------------------------------------------------------------------

func TestRememberCreatesTheFile(t *testing.T) {
	dir := t.TempDir()
	if err := remember(dir, "the test suite needs AGENT_BASH set"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, memoryFileForWriting))
	if err != nil {
		t.Fatalf("remember reported success but wrote no file: %v", err)
	}
	if !strings.Contains(string(raw), "the test suite needs AGENT_BASH set") {
		t.Errorf("the note is not in the file:\n%s", raw)
	}
}

// 记忆文件的全部价值就在于它会累积。不带 O_APPEND 打开写，之前的每一条
// 笔记全毁，而且没人报告这件事：命令成功了，新笔记在那儿；发现问题的会
// 话是三周后的那一次——它打开文件，发现里面正好只有一行。
func TestRememberAppendsRatherThanOverwrites(t *testing.T) {
	dir := t.TempDir()

	// 人手写在里面的既有内容，Agent 不许吃掉。
	memWrite(t, dir, memoryFileForWriting, "# Memory\n\n- (2026-01-01) hand-written line\n")

	if err := remember(dir, "first note"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := remember(dir, "second note"); err != nil {
		t.Fatalf("remember: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, memoryFileForWriting))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{"hand-written line", "first note", "second note"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is gone: the file was truncated instead of appended to, so every note older than the newest one was destroyed\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "first note"), strings.Index(got, "second note"); i >= 0 && j >= 0 && i > j {
		t.Errorf("notes are out of chronological order; a memory file is read top to bottom\n%s", got)
	}
}

// 看不出年纪的记忆，你没法决定要不要删。
func TestRememberDatestamps(t *testing.T) {
	dir := t.TempDir()
	if err := remember(dir, "no date on this one?"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, memoryFileForWriting))
	stamp := time.Now().Format("2006-01-02")
	if !strings.Contains(string(raw), stamp) {
		t.Errorf("the note carries no date, so six months from now nobody can tell which lines are stale:\n%s", raw)
	}
	if !strings.HasPrefix(strings.TrimLeft(string(raw), "\n"), "- (") {
		t.Errorf("the note is not a Markdown list item, so it does not merge cleanly with a hand-edited file:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// userTurn
// ---------------------------------------------------------------------------

// 两个 block，人类的文本是**最后**那一个。
//
// 这个顺序是承重的，不是装饰：阶段 06 渲染这两个 block 的方式不一样——
// 上帝视角显示注入的快照，模型视角显示模型收到的那条消息——而模型把
// 最后一个 block 当成指令来读。把快照放在最后，用户的问题就成了时间戳
// 的上下文。
func TestUserTurnPutsTheSnapshotFirstAndTheHumanLast(t *testing.T) {
	const text = "what changed since yesterday?"
	m := userTurn(text, memWhenLine)

	if m.Role != RoleUser {
		t.Errorf("role is %q", m.Role)
	}
	if len(m.Blocks) != 2 {
		t.Fatalf("%d blocks, want exactly 2 — merging the snapshot into the text makes 'what did the model actually see' "+
			"unanswerable, because the two halves can no longer be told apart", len(m.Blocks))
	}
	if !strings.Contains(m.Blocks[0].Text, "<now>") {
		t.Errorf("block 0 is not the volatile snapshot: %q", m.Blocks[0].Text)
	}
	if m.Blocks[1].Text != text {
		t.Errorf("block 1 is %q, not the user's text — the snapshot was appended after the question, so the model reads a "+
			"timestamp as the thing it was asked to act on", m.Blocks[1].Text)
	}
	for i, b := range m.Blocks {
		if b.Kind != BlockText {
			t.Errorf("block %d is %q, not text", i, b.Kind)
		}
	}
}

func TestUserTurnWithoutASnapshotIsASingleBlock(t *testing.T) {
	m := userTurn("hello", "")
	if len(m.Blocks) != 1 {
		t.Fatalf("%d blocks with no snapshot, want 1 — an empty snapshot block spends prompt bytes on nothing "+
			"and shows up in the God view as an injection that never happened", len(m.Blocks))
	}
	if m.Blocks[0].Text != "hello" {
		t.Errorf("block 0 is %q, not the user's text", m.Blocks[0].Text)
	}
}

// userTurn 造出来的消息会直接进入历史，而历史正是 compactor 要切的东
// 西；所以它造出什么形状，validConversation 就必须收得下那个形状。
func TestUserTurnSurvivesValidConversation(t *testing.T) {
	msgs := []Msg{
		userTurn("how big is this repo?", memWhenLine),
		TextMsg(RoleAssistant, "21 files."),
		userTurn("and the tests?", memWhenLine),
		TextMsg(RoleAssistant, "19 of them."),
	}
	if why := validConversation(msgs); why != "" {
		t.Errorf("a conversation built out of userTurn is not sendable: %s", why)
	}
	if why := validConversation(append([]Msg{summaryMsg("s")}, msgs[1:]...)); why != "" {
		t.Errorf("compacting a userTurn conversation produces an unsendable result: %s", why)
	}
}

// ---------------------------------------------------------------------------
// 上下文块
// ---------------------------------------------------------------------------

// 时钟是每张快照里都必须有的东西，git 探针是永远不许成为必需品的东西。
// 这里拿一个不存在的 shell 去跑探针——没装 bash 的机器就是这个样子：
// 快照里还是得有 <now>，而且不能把失败当成内容报上来——探针要是说
// "git: not found"，就等于告诉模型：它的环境是坏的。
func TestVolatileContextAlwaysHasANowLineAndNeverReportsAFailedProbe(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-a-shell")
	got := volatileContext(missing, memWhen)

	if !strings.Contains(got, memWhenLine) {
		t.Errorf("the snapshot has no usable <now> line when the shell is unavailable:\n%q", got)
	}
	if strings.Contains(got, "<git") {
		t.Errorf("a git block was emitted although the probe could not even start:\n%q", got)
	}
	if strings.Contains(strings.ToLower(got), "not found") || strings.Contains(strings.ToLower(got), "error") {
		t.Errorf("the probe's failure was injected into the prompt as content:\n%q", got)
	}
}

// 同样的保证，换成真 shell，在一个不是仓库的目录里——探针里那个
// `|| true` 就是为这种情况准备的。没有 bash 的地方跳过而不是失败，因为
// Agent 在那里也得能干活。
func TestVolatileContextOmitsGitOutsideARepository(t *testing.T) {
	shell, err := findBash()
	if err != nil {
		t.Skipf("no bash on this machine, so the git probe cannot be exercised: %v", err)
	}
	dir := t.TempDir()
	t.Chdir(dir)

	// 临时目录要是刚好落在谁的仓库里面，这个测试就没什么可说的了。
	if r := runBash(shell, "git rev-parse --abbrev-ref HEAD 2>/dev/null", 10*time.Second); r.ExitCode == 0 && strings.TrimSpace(r.Stdout) != "" {
		t.Skip("the temp directory is itself inside a git repository")
	}

	got := volatileContext(shell, memWhen)
	if !strings.Contains(got, memWhenLine) {
		t.Errorf("no <now> line outside a repository:\n%q", got)
	}
	if strings.Contains(got, "<git") {
		t.Errorf("a git block was emitted outside a repository, so every turn tells the model about a branch that does not exist:\n%q", got)
	}
}

// 正面那一半：在仓库里面，快照必须带上分支、脏文件数和 HEAD 的标题，因
// 为不然的话，Agent 每个回合都要烧一次工具调用去查这三样东西。
func TestVolatileContextReportsGitInsideARepository(t *testing.T) {
	shell, err := findBash()
	if err != nil {
		t.Skipf("no bash on this machine: %v", err)
	}
	dir := t.TempDir()
	t.Chdir(dir)

	setup := `git init -q . && ` +
		`git config user.email agent@example.test && git config user.name agent && ` +
		`git config commit.gpgsign false && ` +
		`echo one > a.txt && git add a.txt && git commit -q -m "the first commit" && ` +
		`echo two > b.txt`
	if r := runBash(shell, setup, 60*time.Second); r.ExitCode != 0 {
		t.Skipf("could not build a scratch repository here (no git?): exit %d %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}

	got := volatileContext(shell, memWhen)
	if !strings.Contains(got, "<git branch=") {
		t.Fatalf("no git block inside a real repository; the probe's output was not recognised:\n%q", got)
	}
	if !strings.Contains(got, `dirty="1"`) {
		t.Errorf("the dirty count is wrong for a tree with exactly one untracked file:\n%q", got)
	}
	if !strings.Contains(got, "the first commit") {
		t.Errorf("the subject of HEAD is missing, so the model cannot tell what the last commit was about:\n%q", got)
	}
}

// stableContext 进的是系统提示词，在缓存断点之前。同一个进程里调用两
// 次，必须产出完全一样的字节，否则前缀就动了，阶段 04 的缓存功夫就全
// 白费了。
func TestStableContextIsByteStable(t *testing.T) {
	a := stableContext("/usr/bin/bash", "/srv/app")
	b := stableContext("/usr/bin/bash", "/srv/app")
	if a != b {
		t.Errorf("two calls disagreed:\n%q\n%q\nanything that varies here rewrites the cached prefix on every request", a, b)
	}
	for _, want := range []string{runtime.GOOS, runtime.GOARCH, "/usr/bin/bash", "/srv/app"} {
		if !strings.Contains(a, want) {
			t.Errorf("%q is missing from the environment block:\n%s", want, a)
		}
	}
	if strings.Contains(a, "<now>") {
		t.Error("a timestamp is in the STABLE block; it changes every turn, so it rewrites the system prompt " +
			"and invalidates the cache on every single request")
	}
}
