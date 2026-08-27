// Stage 05 — memory, and where context is allowed to go.
//
// "Long-term memory" is the phrase that sells vector databases. An agent with a
// shell does not need one, and this file is the whole implementation:
//
//	memory is a file. The agent reads it with `cat` and writes it with `>>`.
//
// That is not a simplification for teaching purposes; it is what the tools you
// use every day do. A file is greppable, diffable, reviewable, versionable, and
// editable by the human whose project it describes — five properties an
// embedding index does not have, in exchange for a similarity search over notes
// that a `grep` would have found.
//
// The harder half of this file is not memory but **placement**: given a piece
// of context, where in the prompt does it go? Stage 04 established that the
// prefix must be byte-stable or the cache dies. This file turns that into a
// rule with two cases, and the rule is the reason the code is shaped the way it
// is.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The placement rule
// ---------------------------------------------------------------------------
//
// Every piece of injected context is one of two things, and the difference is
// not what it contains but **how often its value changes**:
//
//	STABLE for the session   → the system prompt, before the cache breakpoint.
//	                           Written once, cached forever, costs its tokens
//	                           exactly once no matter how long the session runs.
//	                           (memory files, cwd, OS, shell, model limits)
//
//	VOLATILE                 → frozen into a message at the moment that message
//	                           is created, and never recomputed.
//	                           (the clock, git HEAD, the working tree's dirtiness)
//
// The second case is the one people get wrong, and they get it wrong in the
// direction that costs money. The instinct is to keep volatile context *fresh*
// — recompute the timestamp on every request so the model always knows the
// time. That is stage 04's `--break-cache` experiment, and it measured 3.4x.
//
// The resolution is that "fresh" and "in the prefix" are the two things you
// cannot have together, and freshness is the one you can give up almost for
// free: a snapshot taken when the user pressed Enter is accurate for the whole
// turn it belongs to, and it stays in history unchanged afterwards — which is
// exactly what a byte-stable prefix means. The model gets fresh information
// each turn AND the cache survives, because each turn's snapshot is a
// different, permanent line rather than the same line with a moving value.
//
// One sentence, worth more than the rest of the file: **inject once and freeze;
// never recompute what is already in the prefix.**

// memoryFiles are read at startup, in order, and concatenated into the system
// prompt. Both are plain Markdown in the working directory.
//
// The split is by author, not by content:
//
//	AGENTS.md — written by a human, for the agent. Conventions, build commands,
//	            "do not touch generated/", the things a new colleague would be
//	            told on day one. The agent should not edit it.
//	MEMORY.md — written by the agent, for its future self. Discoveries that
//	            cost tool calls to make.
//
// Keeping them apart means a human can review what the agent decided to
// remember without wading through their own instructions, and can delete a bad
// memory with an editor. An agent that writes into the human's file eventually
// argues with it.
var memoryFiles = []string{"AGENTS.md", "MEMORY.md"}

const memoryFileForWriting = "MEMORY.md"

// loadMemory reads whichever memory files exist and returns them as one block,
// plus the list of files found.
//
// Note what it does NOT do: watch the files, re-read them per turn, or notice
// that the agent just appended to MEMORY.md. That is deliberate and it is a
// cache decision, not an oversight — memory sits in the system prompt, so
// re-reading it mid-session would rewrite the prefix and invalidate everything.
// A note written now takes effect next session. Trading a turn of latency for a
// session of cache hits is the right side of that trade, and it is worth
// knowing you made it.
func loadMemory(dir string, bus *Bus) (string, []string) {
	var b strings.Builder
	var found []string
	for _, name := range memoryFiles {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		found = append(found, name)
		fmt.Fprintf(&b, "<memory file=%q>\n%s\n</memory>\n\n", name, strings.TrimSpace(string(raw)))
		if bus != nil {
			bus.Emit(Event{Kind: KindMemoryLoaded, Path: path, Bytes: len(raw)})
		}
	}
	return b.String(), found
}

// remember appends a note to the agent's memory file.
//
// This exists for the `/remember` command, and it is worth being clear about
// why there is a Go function here at all when the agent could run
// `echo ... >> MEMORY.md` itself. It could, and the system prompt tells it so.
// But leaving memory entirely to the model's discretion means it happens
// roughly never: models do not spontaneously decide to write notes, because
// nothing in the current turn rewards it. Every agent that actually accumulates
// useful memory has an explicit trigger — a command, an end-of-session hook, a
// prompt that asks. The mechanism being trivially simple does not make the
// policy question go away.
func remember(dir, note string) error {
	path := filepath.Join(dir, memoryFileForWriting)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Datestamped, because a memory whose age you cannot tell is a memory you
	// cannot decide to delete. Six months of undated notes is a file nobody
	// prunes and everybody stops reading.
	_, err = fmt.Fprintf(f, "\n- (%s) %s\n", time.Now().Format("2006-01-02"), strings.TrimSpace(note))
	return err
}

// ---------------------------------------------------------------------------
// Stable context: goes in the system prompt
// ---------------------------------------------------------------------------

// stableContext describes things that cannot change while the process runs.
//
// The cwd is here rather than in the volatile block on purpose: the agent's
// shell is not persistent (each command is a fresh process, stage 00), so `cd`
// inside a command cannot move it. The one thing that would make this wrong is
// giving the agent a persistent shell — at which point cwd becomes volatile and
// has to move to the other block. Worth noticing how a change in the execution
// model propagates straight into the cache layout.
func stableContext(shell, cwd string) string {
	return fmt.Sprintf(`<environment>
os: %s/%s
shell: %s
working directory: %s
</environment>`, runtime.GOOS, runtime.GOARCH, shell, cwd)
}

// ---------------------------------------------------------------------------
// Volatile context: frozen into one message, once
// ---------------------------------------------------------------------------

// volatileContext takes a snapshot of the things that move.
//
// It runs git, which costs a process. That is affordable once per user turn and
// would not be affordable once per request — another reason the snapshot is
// attached to the user's message rather than rebuilt at request time. The cheap
// design and the cache-correct design happen to be the same design here, which
// is usually a sign the boundary is in the right place.
func volatileContext(shell string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<now>%s</now>\n", now.Format("2006-01-02 15:04:05 -0700"))

	// One command, one process, everything we want. The `|| true` matters: this
	// runs in directories that are not repositories, and a context probe that
	// reports failure as content teaches the model that its environment is
	// broken.
	const gitProbe = `git rev-parse --abbrev-ref HEAD 2>/dev/null && ` +
		`git status --porcelain 2>/dev/null | wc -l && ` +
		`git log -1 --format=%s 2>/dev/null || true`
	// context.Background, not the session's: this runs once at startup to
	// build the system prompt, before there is a session to cancel.
	r := runBash(context.Background(), shell, gitProbe, 3*time.Second)
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if r.ExitCode == 0 && len(lines) >= 2 && lines[0] != "" {
		subject := ""
		if len(lines) >= 3 {
			subject = strings.TrimSpace(lines[2])
		}
		fmt.Fprintf(&b, "<git branch=%q dirty=%q>%s</git>\n",
			strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), subject)
	}
	return strings.TrimSpace(b.String())
}

// userTurn builds the message for one thing the human typed, with the volatile
// snapshot frozen alongside it.
//
// Two blocks rather than one concatenated string, because stage 06 renders them
// differently: the God view shows exactly what was injected, and the Model view
// shows the message as the model received it. Merging them here would make that
// distinction unrecoverable — and "what did the model actually see" is a
// question you can only answer if you never threw the answer away.
func userTurn(text, volatile string) Msg {
	m := Msg{Role: RoleUser}
	if volatile != "" {
		m.Blocks = append(m.Blocks, Block{Kind: BlockText, Text: volatile + "\n\n"})
	}
	m.Blocks = append(m.Blocks, Block{Kind: BlockText, Text: text})
	return m
}
