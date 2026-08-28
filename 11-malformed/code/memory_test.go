package main

import (
	"context"
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

// memRecorder collects what loadMemory told the bus. Memory is loaded once, at
// startup, into a part of the prompt nobody sees again — so the event is the
// only evidence the user has that it happened at all.
type memRecorder struct{ events []Event }

func (r *memRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

// memWrite drops a file into dir. Every filesystem test in this file works in
// t.TempDir(); nothing here may go near the repository's own AGENTS.md or
// MEMORY.md, which are real files a human maintains.
func memWrite(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// memWhen is a fixed instant, so the <now> assertions compare against a literal
// rather than against a re-derived format string.
var memWhen = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const memWhenLine = "<now>2026-01-02 03:04:05 +0000</now>"

// ---------------------------------------------------------------------------
// loadMemory
// ---------------------------------------------------------------------------

// A fresh directory has no memory files, and that is the normal case. Returning
// anything other than an empty string here puts stray bytes at the front of the
// system prompt of every session that has never used the feature.
func TestLoadMemoryOnAnEmptyDirectory(t *testing.T) {
	got, found := loadMemory(t.TempDir(), nil)
	if got != "" {
		t.Errorf("a directory with no memory files produced %q, which would be prepended to the system prompt of every session", got)
	}
	if len(found) != 0 {
		t.Errorf("reported %v as loaded from a directory containing neither file", found)
	}
}

// Both files, in the documented order, each in its own tagged block.
//
// The order is part of the contract: AGENTS.md is the human's instructions and
// MEMORY.md is the agent's own notes, and when the two disagree the later block
// is the one the model weighs more heavily. Reversing them silently lets the
// agent's guesses override what its operator wrote.
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

// An empty file is what a `touch AGENTS.md` leaves behind, and a whitespace-only
// one is what an editor leaves after the last note is deleted. Either one, if
// injected, spends prompt bytes and tells the model there is a convention file
// with nothing in it.
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

// The event is the user's only view of what went into the prompt prefix, and a
// nil bus is the startup path before the renderer is attached.
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

	// Must not panic: main.go loads memory before any subscriber exists.
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

// The whole value of a memory file is that it accumulates. Opening it for
// writing without O_APPEND destroys every earlier note, and nothing reports it:
// the command succeeds, the new note is there, and the session that notices is
// the one three weeks later that finds the file has exactly one line in it.
func TestRememberAppendsRatherThanOverwrites(t *testing.T) {
	dir := t.TempDir()

	// Pre-existing content a human wrote by hand, which the agent must not eat.
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

// A memory whose age you cannot tell is a memory you cannot decide to delete.
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

// Two blocks, and the human's text is the LAST one.
//
// The ordering is load-bearing rather than cosmetic: stage 06 renders the two
// blocks differently — the God view shows the injected snapshot, the Model view
// shows the message as the model received it — and the model reads the final
// block as the instruction. Putting the snapshot last makes the user's question
// context for a timestamp.
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

// The messages userTurn builds go straight into the history the compactor cuts,
// so whatever shape it produces has to be a shape validConversation accepts.
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
// Context blocks
// ---------------------------------------------------------------------------

// The clock is the one thing that must be in every snapshot, and the git probe
// is the one thing that must never be required. This runs the probe against a
// shell that does not exist, which is what a machine without bash looks like:
// the snapshot must still carry <now>, and it must not report the failure as
// content — a probe that says "git: not found" teaches the model that its
// environment is broken.
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

// The same guarantee with a real shell, in a directory that is not a
// repository — the case the `|| true` in the probe exists for. Skipped rather
// than failed where there is no bash, because the agent has to work there too.
func TestVolatileContextOmitsGitOutsideARepository(t *testing.T) {
	shell, err := findBash()
	if err != nil {
		t.Skipf("no bash on this machine, so the git probe cannot be exercised: %v", err)
	}
	dir := t.TempDir()
	t.Chdir(dir)

	// If the temp directory happens to live inside somebody's repository, this
	// test has nothing to say.
	if r := runBash(context.Background(), shell, "git rev-parse --abbrev-ref HEAD 2>/dev/null", 10*time.Second); r.ExitCode == 0 && strings.TrimSpace(r.Stdout) != "" {
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

// The positive half: inside a repository the snapshot has to carry the branch,
// the dirty count and the subject of HEAD, because those are the three things
// the agent otherwise burns a tool call to discover on every turn.
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
	if r := runBash(context.Background(), shell, setup, 60*time.Second); r.ExitCode != 0 {
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

// stableContext goes in the system prompt, before the cache breakpoint. Two
// calls in one process must produce identical bytes, or the prefix moves and
// stage 04's cache work is undone.
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
