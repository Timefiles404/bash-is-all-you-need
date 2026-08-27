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
// 辅助函数
// ---------------------------------------------------------------------------

// memRecorder 收集的是 loadMemory
// 告诉总线的内容。记忆只在启动
// 时装载一次，进入的是提示词里
// 一个没有人会再看的部分——所以
// 这个事件，是用户手上唯一能
// 证明这件事真的发生过的证据。
type memRecorder struct{ events []Event }

func (r *memRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

// memWrite 把一个文件放进 dir。
// 这个文件里的每一个文件系统
// 测试，都在 t.TempDir() 里运行；
// 这里的任何测试都不能碰到
// 仓库自己的 AGENTS.md 或
// MEMORY.md——那些是人类会亲自
// 维护的真实文件。
func memWrite(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// memWhen 是一个固定的时刻，
// 所以 <now> 断言比对的是一个
// 字面量，而不是重新生成的
// 格式化字符串。
var memWhen = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const memWhenLine = "<now>2026-01-02 03:04:05 +0000</now>"

// ---------------------------------------------------------------------------
// loadMemory
// ---------------------------------------------------------------------------

// 新目录没有记忆文件，这是常见
// 情况。如果这里返回的不是空
// 字符串，就会把流浪字节，塞进
// 每一个从未用过这个功能的会话
// 的系统提示词最前面。
func TestLoadMemoryOnAnEmptyDirectory(t *testing.T) {
	got, found := loadMemory(t.TempDir(), nil)
	if got != "" {
		t.Errorf("a directory with no memory files produced %q, which would be prepended to the system prompt of every session", got)
	}
	if len(found) != 0 {
		t.Errorf("reported %v as loaded from a directory containing neither file", found)
	}
}

// 两个文件，按记录顺序，每个都
// 在自己的标记块里。
//
// 顺序本身就是约定的一部分：
// AGENTS.md 是人类的指令，
// MEMORY.md 是 Agent 自己的笔记，
// 两者内容有冲突时，模型会更
// 看重后面那个块。如果默默把
// 顺序倒过来，就等于让 Agent
// 的猜测，压过了操作者写下的
// 东西。
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

// 一个空文件，是 `touch AGENTS.md`
// 留下的东西；一个只剩空白的
// 文件，则是编辑器在最后一条
// 笔记被删除之后留下的东西。
// 这两种情况，只要被注入进去，
// 都会白白花掉 prompt 的字节，
// 还会让模型以为：这里有一个
// 约定文件，但里面什么都没有。
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

// 这个事件，是用户唯一能看到
// "提示词前缀里进了什么"的
// 窗口；nil bus 对应的，是渲染器
// 还没接上之前，启动阶段会走
// 的那条路径。
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

	// 必须不 panic：main.go 会在
	// 任何订阅者存在之前，先装载
	// 记忆。
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

// 记忆文件的全部价值，就在于
// 它会不断累积。不带 O_APPEND
// 打开它来写，会摧毁掉之前
// 所有的笔记，而且没有任何
// 报错：命令执行成功，新笔记
// 也确实写进去了，真正注意到
// 问题的，是三周后的那次
// 会话——那时候才发现，文件里
// 恰好只剩一行。
func TestRememberAppendsRatherThanOverwrites(t *testing.T) {
	dir := t.TempDir()

	// 人类手写下的既有内容，Agent
	// 绝不能吃掉。
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

// 一条你判断不出年龄的记忆，
// 就是一条你没法决定删不删的
// 记忆。
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

// 两块，人类的文本是**最后的**
// 一个。
//
// 顺序是承重的，不是装饰性的：
// 阶段 06 对这两块的呈现方式
// 不同——上帝视角显示的是被
// 注入的快照，模型视角显示的
// 是模型实际收到的那条消息——
// 而模型会把最后一块当作指令
// 来读。如果把快照放在最后，
// 用户的问题就会反过来，变成
// 时间戳的背景信息。
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

// userTurn 构建的消息直接进入 compactor 截断的历史记录，
// 所以它生成的任何形态都必须是 validConversation 接受的形态。
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

// 时钟是每个快照必须包含的唯一东西，git 探针是必须永远不被需要的唯一东西。
// 这在不存在的 shell 上运行探针——这就是没有 bash
// 的机器的样子：快照仍然必须携带 <now>，
// 不得将失败报告为内容——说"git: not found"的探针会教导模型其环境被破坏了。
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

// 与真实 shell 相同的保证，在不是仓库的目录中——
// 这就是探针中 `|| true` 存在的情况。没有 bash 时跳过而不是失败，
// 因为 Agent 也必须在那里工作。
func TestVolatileContextOmitsGitOutsideARepository(t *testing.T) {
	shell, err := findBash()
	if err != nil {
		t.Skipf("no bash on this machine, so the git probe cannot be exercised: %v", err)
	}
	dir := t.TempDir()
	t.Chdir(dir)

	// 如果临时目录碰巧位于某人的仓库内，这个测试就什么都说不了。
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

// 积极的一面：在仓库内部，快照必须带上分支、脏计数和 HEAD 的主题，
// 因为这三样东西，要是没有它们，Agent 就得每个回合都烧一次工具调用去问。
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

// stableContext 进入系统提示词，在缓存断点之前。
// 同一进程中的两个调用必须生成相同的字节，否则前缀会移动，
// 第 04 阶段的缓存工作就会被撤销。
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
