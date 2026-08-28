// Stage 07 — subagents, which are the same loop called again.
//
// There is no framework here and no orchestration layer. A subagent is:
//
//	a fresh []Msg, a different system prompt, the same provider, the same
//	tools — and it returns TEXT to its caller, not its transcript.
//
// That last clause is the entire product. Everything a subagent does — every
// tool call, every 40kB of command output, every wrong turn it took and backed
// out of — happens in a message array that is thrown away when it finishes. The
// parent's context grows by the length of the report and by nothing else.
//
// So the thing to be clear about, because it is the opposite of what people
// assume:
//
//	**A subagent does not save tokens. It saves CONTEXT.**
//
// It usually costs *more* total tokens than doing the work inline — the child
// re-reads a system prompt, re-establishes what it is doing, and re-discovers
// things the parent already knew. What it buys is that the parent's window does
// not fill up, which is the resource that actually runs out. Stage 05 measured
// what happens when it does.
//
// The second thing worth noticing is what is NOT new. The parent already had a
// loop; the child is that loop. The parent already had a bus; the child forks
// it. The parent already had a compactor and a gate; the child shares them.
// Roughly a hundred lines of this file are the feature, and most of those are
// the fuses.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// The subagent's system prompt.
//
// The third paragraph is the one that matters, and it is the mechanism
// explained to the model rather than hidden from it. A subagent that does not
// know its transcript is discarded writes a summary of its process ("I looked
// at several files and found some things"), because that is what a chat turn
// normally is. Told plainly that its final message is the only thing that
// survives, it writes a report.
const subagentSystem = `You are a subagent. Another agent has delegated one task to you and is waiting.

You have the same shell it has, and the same working directory. Do the task.

Everything you do here is discarded when you finish EXCEPT your final message.
Your caller will never see your commands, your reasoning, or your tool output —
only the last thing you say. So your final message has to stand alone:

- Give exact paths, exact command lines, exact identifiers, exact error text.
  Your caller cannot re-run anything to check, and cannot ask you a follow-up.
- State what you could NOT determine, and why. A gap you name is useful; a gap
  you paper over sends your caller down a wrong path with confidence.
- Report findings, not process. "src/auth/token.go:88 hardcodes a 3600s TTL" is
  worth a line; "I searched the repo and looked at several files" is worth none.

You cannot ask the user anything: there is no user here. If the task is
ambiguous, choose the most useful reading, do it, and say which reading you
chose.`

func taskToolDef() Tool {
	return Tool{
		Name: "task",
		// The description is written for the *economics*, because that is the
		// decision the model has to make. A tool description that says what a
		// tool does tells the model nothing about when to reach for it.
		Description: "Delegate a self-contained piece of work to a subagent with its own context window. " +
			"The subagent has the same shell and returns only a final written report; its commands and " +
			"output never enter your context. Use this for work that will read a lot and conclude a little — " +
			"searching a large codebase, investigating a failure, surveying files. Do not use it for a single " +
			"command, and do not use it for work whose intermediate output you need to see.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A three-to-six word label for this task, shown to the user.",
				},
				"prompt": map[string]any{
					"type": "string",
					"description": "The full task. The subagent shares your working directory but none of " +
						"your conversation, so state everything it needs: what to look for, what you already " +
						"know, and exactly what to report back.",
				},
			},
			"required":             []string{"description", "prompt"},
			"additionalProperties": false,
		},
	}
}

// parseTaskArgs mirrors parseBashArgs, including the pointer fields.
//
// Stage 01 learned this the hard way: a value-typed string field makes
// json.Unmarshal succeed on a payload that never contained the key, so a
// truncated tool call becomes an empty task rather than an error. Two required
// fields here, so two pointers.
func parseTaskArgs(raw string) (description, prompt string, err error) {
	var args struct {
		Description *string `json:"description"`
		Prompt      *string `json:"prompt"`
	}
	if e := json.Unmarshal([]byte(raw), &args); e != nil {
		return "", "", fmt.Errorf("tool call arguments were not valid JSON: %v", e)
	}
	if args.Prompt == nil || strings.TrimSpace(*args.Prompt) == "" {
		return "", "", fmt.Errorf(`the "prompt" field was missing or empty; a subagent with no task cannot do anything`)
	}
	d := "subtask"
	if args.Description != nil && strings.TrimSpace(*args.Description) != "" {
		d = strings.TrimSpace(*args.Description)
	}
	return d, *args.Prompt, nil
}

// ---------------------------------------------------------------------------
// Spawning
// ---------------------------------------------------------------------------

// spawn runs one subagent to completion and returns its report.
//
// Note what is shared and what is not. Shared: the provider, the HTTP client,
// the gate, the shell config, and the bus core — so the child's permission
// prompts reach the same human and the child's events land in the same ordered
// trace. Not shared: the message array, the system prompt, the compactor, and
// the turn budget. The split is exactly "state the parent must not lose" versus
// "state the child must not inherit".
func (a *agent) spawn(ctx context.Context, callID, description, prompt string) (string, Usage, error) {
	started := time.Now()
	agentID := fmt.Sprintf("%s#%d", description, a.nextChild())

	a.bus.Emit(Event{
		Kind: KindSubagentStart, ToolID: callID,
		ToolName: description, Text: prompt,
	})

	child := a.newChild(agentID, func() string { return subagentSystem + para + a.stable })

	msgs := []Msg{TextMsg(RoleUser, prompt)}
	msgs = child.runTurn(ctx, msgs)

	report := lastAssistantText(msgs)
	if strings.TrimSpace(report) == "" {
		// A subagent that returns nothing is worse than an error, because the
		// parent will treat the empty string as a finding. Say so out loud.
		report = "[the subagent produced no final report — it may have hit its turn limit or been cut off. Treat this as a failure, not as an empty result.]"
	}

	a.bus.Emit(Event{
		Kind: KindSubagentEnd, ToolID: callID, ToolName: description,
		Text: report, Bytes: len(report), Millis: time.Since(started).Milliseconds(),
		Usage: &child.spent, Turn: child.turnsUsed,
	})
	return report, child.spent, nil
}

func (a *agent) nextChild() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.children++
	return a.children
}

// newChild builds a subagent that shares what must be shared and inherits
// nothing that must not be.
//
// Shared: the provider, the HTTP client, the gate, the shell config, and the
// bus core — so the child's permission prompts reach the same human and its
// events land in the same ordered trace. Not shared: the message array, the
// system prompt, the compactor, and the turn budget.
//
// It is written out field by field rather than as `child := *a`, which is
// shorter and which `go vet` correctly refuses: agent holds a sync.Mutex, and
// copying a struct that contains one gives the copy a mutex that is already in
// whatever state the original's was. The explicit form is also the honest one —
// every line of it is a decision about what a subagent is.
func (a *agent) newChild(agentID string, system func() string) *agent {
	child := &agent{
		// The ladder is shared by pointer, not copied, and that is a decision
		// rather than an accident of syntax. "This endpoint is refusing calls"
		// is a fact about the endpoint, so a child that discovers it should not
		// have to teach its parent, and a parent that already fell back should
		// not send its children back to the dead rung. It is why ladder has a
		// mutex: several children can fail against the same provider at once.
		lad: a.lad, pol: a.pol, httpc: a.httpc, g: a.g, cfg: a.cfg,
		bus:       a.bus.Fork(agentID),
		memoryDir: a.memoryDir,
		stable:    a.stable,
		depth:     a.depth + 1,
		maxDepth:  a.maxDepth,
		system:    system,

		// A fresh compactor, because the child's conversation is a different
		// conversation. Sharing one would mean the child's estimator was
		// calibrated on the parent's traffic — usually close enough, and
		// "usually close enough" is how a shared mutable object becomes a bug
		// six months later.
		comp: newCompactor(a.comp.window, a.comp.threshold, a.comp.keepRatio),
	}
	child.comp.est.ratio = a.comp.est.ratio // one free hint, then it calibrates

	// The child's own turn budget, smaller than the parent's by default: a
	// subagent that needs thirty rounds was given a task that should have been
	// three subagents, and the fuse is the only thing that will tell you.
	child.cfg.maxTurns = a.cfg.subTurns
	return child
}

// lastAssistantText is the child's return value: the final thing it said.
func lastAssistantText(msgs []Msg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			if t := msgs[i].Text(); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// The tool table, which is a function of depth
// ---------------------------------------------------------------------------

// tools returns what this agent may call.
//
// At the depth limit the `task` tool is **removed**, not refused. That is a
// deliberate difference and it is worth the paragraph:
//
// A runtime refusal costs a full round trip — the model writes a tool call, the
// harness rejects it, the model reads the rejection and tries something else —
// and it costs the tokens of the tool definition on every request that will
// never be able to use it. Worse, it is a rule the model can see is arbitrary,
// and models argue with arbitrary rules by rephrasing.
//
// A tool that is not in the list is not a rule. There is nothing to argue with
// and nothing to work around, and the model plans within the tools it has,
// which is what you wanted.
func (a *agent) tools() []Tool {
	if a.depth >= a.maxDepth {
		return []Tool{bashToolDef()}
	}
	return []Tool{bashToolDef(), taskToolDef()}
}

// ---------------------------------------------------------------------------
// Running a turn's tool calls, some of them at once
// ---------------------------------------------------------------------------

// dispatch executes every tool call in one assistant turn and returns the
// results **in the order the model emitted them**.
//
// Subagent calls run concurrently; everything else runs in sequence. The
// ordering guarantee is the part to notice: execution is concurrent, the
// history is deterministic. If results were appended as they completed, the
// same session replayed twice would produce two different message arrays, two
// different prompt prefixes, and — per stage 04 — a cache that never hits.
// Concurrency is allowed to change how long things take. It is not allowed to
// change what the conversation says.
func (a *agent) dispatch(ctx context.Context, turn int, calls []Block) ([]Block, bool) {
	results := make([]Block, len(calls))
	texts := make([]string, len(calls))
	stopped := false

	// Pass 1: the sequential work, and the gate decisions for everything.
	//
	// Every permission question is asked HERE, on one goroutine, before any
	// concurrency starts. A gate prompt written from two goroutines at once
	// produces two half-questions on one line and reads a single answer for
	// both, which is a security bug wearing a UI bug's clothes.
	type pending struct {
		i           int
		description string
		prompt      string
	}
	var async []pending

	for i, c := range calls {
		if stopped {
			texts[i] = "[not executed: the session was stopped.]"
			continue
		}
		switch c.Name {
		case "task":
			description, prompt, err := parseTaskArgs(c.Args)
			if err != nil {
				texts[i] = fmt.Sprintf("[%v]", err)
				continue
			}
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name,
				Command: description + ": " + firstLine(prompt)})
			v, why := a.g.ask("subagent — " + description)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				texts[i] = "[the user denied this subagent. Do not retry it unchanged.]"
			case abort:
				stopped = true
				texts[i] = "[the user stopped the session.]"
			default:
				async = append(async, pending{i, description, prompt})
			}

		case "bash":
			command, err := parseBashArgs(c.Args)
			if err != nil {
				texts[i] = fmt.Sprintf("[%v]", err)
				continue
			}
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name, Command: command})
			v, why := a.g.ask(command)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				texts[i] = "[the user denied this command. Do not retry it unchanged.]"
			case abort:
				stopped = true
				texts[i] = "[the user stopped the session.]"
			default:
				texts[i] = a.runCommand(ctx, turn, c.ID, command)
			}

		default:
			// A tool name the model invented. It happens, and the answer is to
			// say so precisely rather than to fail the turn: the model can
			// recover from "there is no such tool" and cannot recover from a
			// dropped result.
			texts[i] = fmt.Sprintf("[there is no tool called %q. The tools available to you are listed in this request.]", c.Name)
		}
	}

	// Pass 2: the subagents, all at once.
	if len(async) > 0 {
		var wg sync.WaitGroup
		for _, p := range async {
			wg.Add(1)
			go func(p pending) {
				defer wg.Done()
				report, _, err := a.spawn(ctx, calls[p.i].ID, p.description, p.prompt)
				if err != nil {
					texts[p.i] = fmt.Sprintf("[the subagent failed: %v]", err)
					return
				}
				texts[p.i] = report
			}(p)
		}
		wg.Wait()
	}

	for i, c := range calls {
		results[i] = a.emitResult(turn, c.ID, texts[i])
	}
	return results, stopped
}

// runCommand is the bash half of dispatch, unchanged from stage 06 except that
// it returns the rendered result instead of appending it.
func (a *agent) runCommand(ctx context.Context, turn int, callID, command string) string {
	a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: callID, Command: command})
	r := runBash(ctx, a.cfg.shell, command, a.cfg.timeout)
	rendered, truncated := r.render(a.cfg.maxOutput)
	a.bus.Emit(Event{
		Kind: KindCommandEnd, Turn: turn, ToolID: callID, Command: command,
		ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
		Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
	})
	return rendered
}

// firstLine is what the user reads when approving a subagent, so it has two
// jobs: show something, and never read as complete when it is not.
//
// Leading whitespace is trimmed BEFORE the cut rather than after. A prompt
// beginning with a newline — which is most of them, when a model writes a
// multi-paragraph task — used to produce the string " …": an ellipsis with
// nothing in front of it, on the line a human is being asked to authorise.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}
