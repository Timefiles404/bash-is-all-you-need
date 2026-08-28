package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file tests stage 07: the bus once it became a tree, the task tool's
// argument parser, the depth fuse in agent.tools, skills.go, and dispatch's
// promise that concurrent execution still produces a deterministic history.
//
// Nothing here calls a provider over the network. The one place a Provider is
// needed — the concurrent path through dispatch — uses mulFakeProvider below,
// which answers out of the request body through a fake RoundTripper. That is
// twenty lines and it buys the only assertion in the file that can distinguish
// "results are collected by index" from "results are collected as they land".

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// mulBOM is a UTF-8 byte order mark, spelled as a rune value rather than as the
// character itself: a literal U+FEFF anywhere but byte zero of a Go source file
// is a compile error, which is the same constraint parseFrontmatter's own cutset
// is written around.
var mulBOM = string(rune(0xFEFF))

// mulRecorder collects every event a bus delivers.
//
// It needs no lock of its own: Bus.Emit dispatches under the core mutex, so
// OnEvent is serialised for free. That is not an accident of this test, it is
// the property the concurrency test below exists to pin — and if it ever stops
// being true, `go test -race` will say so here first.
type mulRecorder struct{ events []Event }

func (r *mulRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func (r *mulRecorder) kind(k Kind) []Event {
	var out []Event
	for _, e := range r.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func (r *mulRecorder) count(k Kind) int { return len(r.kind(k)) }

// mulAgent builds an agent that can dispatch tool calls without a network: a
// gate the caller chose, a real shell, a compactor with no window (so nothing
// ever tries to compact), and a bus with a recorder on it.
func mulAgent(g *gate, shell string) (*agent, *mulRecorder) {
	rec := &mulRecorder{}
	bus := NewBus(rec)
	return &agent{
		g:   g,
		bus: bus,
		cfg: config{
			shell:     shell,
			timeout:   20 * time.Second,
			maxOutput: 8192,
			maxTurns:  8,
			subTurns:  2,
		},
		comp:     newCompactor(0, 0, 0),
		pol:      defaultRetryPolicy(),
		system:   func() string { return "you are a test harness" },
		stable:   "\n\n<env>test</env>",
		maxDepth: 2,
	}, rec
}

// mulShell returns a bash to run commands with, or skips.
func mulShell(t *testing.T) string {
	t.Helper()
	shell, err := findBash()
	if err != nil {
		t.Skipf("no bash on this machine, so dispatch cannot run a real command: %v", err)
	}
	return shell
}

// mulBash builds a well-formed bash tool-call payload.
func mulBash(command string) string {
	raw, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// mulFakeProvider answers a subagent's model call from a script instead of over
// the wire. The prompt makes the round trip — BuildRequest writes it into the
// body, the fake transport echoes the body back, ParseStream reads it — so one
// provider can give two concurrent children two different answers, and can hold
// one of them until the other has finished.
type mulFakeProvider struct {
	mu        sync.Mutex
	completed []string

	reply  func(prompt string) string
	before func(prompt string) // runs before this call is recorded as complete
	after  func(prompt string) // runs after
}

var _ Provider = (*mulFakeProvider)(nil)

func (p *mulFakeProvider) Protocol() string { return "fake" }
func (p *mulFakeProvider) Model() string    { return "fake-model" }

func (p *mulFakeProvider) BuildRequest(ctx context.Context, system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	body, err := json.Marshal(struct {
		Prompt string `json:"prompt"`
	}{msgs[len(msgs)-1].Text()})
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://subagent.invalid/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	return req, body, nil
}

func (p *mulFakeProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if p.before != nil {
		p.before(in.Prompt)
	}
	p.mu.Lock()
	p.completed = append(p.completed, in.Prompt)
	p.mu.Unlock()
	if p.after != nil {
		p.after(in.Prompt)
	}
	return &CallResult{
		Text:    p.reply(in.Prompt),
		Stop:    StopEndTurn,
		RawStop: "end_turn",
		Usage:   Usage{Input: 900, Output: 40},
	}, nil
}

// order is the sequence in which the children actually finished.
func (p *mulFakeProvider) order() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.completed...)
}

// mulRoundTrip is an http.RoundTripper that hands the request body straight
// back as a 200. No listener, no port, no timeout to flake on.
type mulRoundTrip struct{}

func (mulRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
		body = b
	}
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    r,
	}, nil
}

// ---------------------------------------------------------------------------
// The bus, now that it is a tree
// ---------------------------------------------------------------------------

// Stage 07 is the first time two goroutines emit at once, and the claim the
// whole trace design rests on is that Seq is still a total order over the tree:
// every event numbered exactly once, delivered in numbered order, across every
// agent. If Seq is stamped outside the lock, two children can be handed the
// same number, or a lower number can be delivered after a higher one — and a
// trace that says two things happened at the same moment is not evidence about
// which of them caused the other.
//
// Run under -race this also catches the unsynchronised counter directly.
func TestBusSeqIsATotalOrderAcrossConcurrentForks(t *testing.T) {
	const (
		emitters = 8
		perAgent = 50
		total    = emitters * perAgent
	)

	rec := &mulRecorder{}
	root := NewBus(rec)

	// One root plus seven children, all forked before anything starts, so the
	// goroutines contend on the same counter from the first event.
	buses := []*Bus{root}
	for i := 1; i < emitters; i++ {
		buses = append(buses, root.Fork(fmt.Sprintf("child#%d", i)))
	}

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i, b := range buses {
		done.Add(1)
		go func(i int, b *Bus) {
			defer done.Done()
			start.Wait() // release them all at once, to maximise contention
			for j := 0; j < perAgent; j++ {
				b.Emit(Event{Kind: KindNotice, Text: fmt.Sprintf("agent %d event %d", i, j)})
			}
		}(i, b)
	}
	start.Done()
	done.Wait()

	if len(rec.events) != total {
		t.Fatalf("the bus delivered %d events for %d emitted; events were lost or duplicated on the way out, "+
			"so the trace file is not a record of the session", len(rec.events), total)
	}

	// One assertion covers all four properties at once: delivery order equals
	// numbering order, the numbers run 1..N, none is missing, none repeats.
	seen := map[int]int{}
	for i, e := range rec.events {
		seen[e.Seq]++
		if e.Seq == i+1 {
			continue
		}
		if seen[e.Seq] > 1 {
			t.Fatalf("event %d of %d carries Seq %d, which was already used: two goroutines were handed the same "+
				"sequence number, so nothing downstream can order them and `jq 'select(.seq==%d)'` returns two different events",
				i, total, e.Seq, e.Seq)
		}
		t.Fatalf("event %d of %d carries Seq %d, not %d: the stream was delivered out of numbered order, "+
			"so a replay of this trace shows a different session from the one that ran",
			i, total, e.Seq, i+1)
	}
}

// Fork stamps the tree coordinates, and the root is depth 0 with no name. If
// Fork forgot to increment, every subagent's events would claim to be the
// parent's, and the one question a subagent trace exists to answer — which
// agent ran this command — is unanswerable.
func TestForkStampsDepthAndAgentAndTheRootDoesNot(t *testing.T) {
	rec := &mulRecorder{}
	root := NewBus(rec)
	child := root.Fork("survey docs#1")
	grand := child.Fork("grep for TODOs#2")

	root.Emit(Event{Kind: KindNotice, Text: "root"})
	child.Emit(Event{Kind: KindNotice, Text: "child"})
	grand.Emit(Event{Kind: KindNotice, Text: "grandchild"})

	if root.Depth() != 0 || child.Depth() != 1 || grand.Depth() != 2 {
		t.Fatalf("Depth() reports %d/%d/%d for root/child/grandchild; the depth fuse in agent.tools is driven by this "+
			"number, so a wrong one either removes the task tool from an agent that should have it or leaves it on forever",
			root.Depth(), child.Depth(), grand.Depth())
	}

	if len(rec.events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(rec.events))
	}
	for i, want := range []struct {
		depth int
		agent string
	}{
		{0, ""},
		{1, "survey docs#1"},
		{2, "grep for TODOs#2"},
	} {
		got := rec.events[i]
		if got.Depth != want.depth {
			t.Errorf("the %s event carries Depth %d, want %d — a trace with the wrong depth cannot be indented, "+
				"filtered by agent, or read as a tree at all", got.Text, got.Depth, want.depth)
		}
		if got.Agent != want.agent {
			t.Errorf("the %s event carries Agent %q, want %q — this is the only label that says which subagent "+
				"emitted it, and %q is what a reader would have to guess from", got.Text, got.Agent, want.agent, got.Agent)
		}
	}
}

// The comment on Emit says Seq, Depth and Agent are assigned there "so no
// caller can forge them". This makes that claim load-bearing: a field a caller
// can set is a field a trace cannot be evidence about, and the caller most
// likely to set one by accident is a replayed Event being re-emitted.
func TestEmitOverwritesAnyForgedSeqDepthOrAgent(t *testing.T) {
	rec := &mulRecorder{}
	child := NewBus(rec).Fork("real child#1")

	child.Emit(Event{
		Kind:  KindNotice,
		Text:  "forged",
		Seq:   9999,
		Depth: 77,
		Agent: "impostor",
	})

	if len(rec.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(rec.events))
	}
	e := rec.events[0]
	if e.Seq != 1 {
		t.Errorf("the caller's Seq %d survived (bus said %d): a trace whose ordering can be written by the code "+
			"being traced orders nothing", 9999, e.Seq)
	}
	if e.Depth != 1 {
		t.Errorf("the caller's Depth 77 survived as %d: any agent could claim to be any other agent in the trace", e.Depth)
	}
	if e.Agent != "real child#1" {
		t.Errorf("the caller's Agent %q survived as %q: attribution in the trace is now whatever the emitter felt like", "impostor", e.Agent)
	}
}

// Parent and child are two views onto one core, so a subscriber added through
// either sees everything. This is what lets main() attach the trace writer to
// the root bus once and still capture every subagent — and what stops a
// renderer attached mid-session from seeing only half the tree.
func TestSubscribersAreSharedBetweenAParentBusAndItsChildren(t *testing.T) {
	root := NewBus()
	child := root.Fork("child#1")

	// Added through the child, must see the parent's events.
	viaChild := &mulRecorder{}
	child.Subscribe(viaChild)
	root.Emit(Event{Kind: KindNotice, Text: "from the root"})
	if viaChild.count(KindNotice) != 1 {
		t.Errorf("a subscriber added on the child bus saw %d of the root's events; the two buses do not share a core, "+
			"so the trace file attached to one of them holds half a session", viaChild.count(KindNotice))
	}

	// Added through the parent, must see the child's events.
	viaRoot := &mulRecorder{}
	root.Subscribe(viaRoot)
	child.Emit(Event{Kind: KindNotice, Text: "from the child"})
	if viaRoot.count(KindNotice) != 1 {
		t.Errorf("a subscriber added on the root bus saw %d of the child's events; every subagent's work would be "+
			"missing from the trace the user actually opens", viaRoot.count(KindNotice))
	}
	// And the one added earlier saw it too, so subscribing did not replace the list.
	if viaChild.count(KindNotice) != 2 {
		t.Errorf("the earlier subscriber saw %d events after a second one was added; Subscribe is overwriting "+
			"rather than appending", viaChild.count(KindNotice))
	}
}

// Fork shares the subscriber list, it does not copy it. A copy is the obvious
// implementation and it delivers every parent event twice once a child exists —
// two lines per event in the trace, doubled token counts in every total the
// composer computes from it.
func TestForkDoesNotDuplicateSubscribers(t *testing.T) {
	rec := &mulRecorder{}
	root := NewBus(rec)
	child := root.Fork("child#1")
	grand := child.Fork("grandchild#2")

	root.Emit(Event{Kind: KindNotice, Text: "a"})
	child.Emit(Event{Kind: KindNotice, Text: "b"})
	grand.Emit(Event{Kind: KindNotice, Text: "c"})

	if len(rec.events) != 3 {
		t.Fatalf("one subscriber received %d events for 3 emitted; forking copied the subscriber list, so every "+
			"event is recorded once per living agent", len(rec.events))
	}
}

// ---------------------------------------------------------------------------
// parseTaskArgs
// ---------------------------------------------------------------------------

// parseTaskArgs is parseBashArgs for the task tool, and it has the same job:
// reject a payload that unmarshalled cleanly but does not contain a task. The
// pointer field is the whole mechanism — a value-typed string makes
// json.Unmarshal succeed on `{}` and turns a truncated tool call into a
// subagent launched with an empty prompt, which is stage 01's bug with a
// network call attached.
//
// The error text is asserted because it is not for us: it is returned to the
// model as the tool result, and it is the model's only clue about what to send
// instead.
func TestParseTaskArgs(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantDesc    string
		wantPrompt  string
		wantErr     bool
		errMentions string
	}{
		{
			name:       "both fields present",
			raw:        `{"description":"survey the docs","prompt":"List every file under docs/ and summarise each in one line."}`,
			wantDesc:   "survey the docs",
			wantPrompt: "List every file under docs/ and summarise each in one line.",
		},
		{
			name:        "no prompt key at all — the pointer case",
			raw:         `{"description":"survey the docs"}`,
			wantErr:     true,
			errMentions: "prompt",
		},
		{
			name:        "empty object",
			raw:         `{}`,
			wantErr:     true,
			errMentions: "prompt",
		},
		{
			name:        "prompt present but empty",
			raw:         `{"description":"survey the docs","prompt":""}`,
			wantErr:     true,
			errMentions: "prompt",
		},
		{
			name:        "prompt present but whitespace",
			raw:         `{"description":"survey the docs","prompt":"   \n\t  "}`,
			wantErr:     true,
			errMentions: "prompt",
		},
		{
			name:        "prompt explicitly null",
			raw:         `{"description":"survey the docs","prompt":null}`,
			wantErr:     true,
			errMentions: "prompt",
		},
		{
			// A missing label is cosmetic — it is what the user sees next to a
			// spinner. Failing the call over it would throw away a perfectly
			// good task because the model skipped an adjective.
			name:       "description missing defaults instead of failing",
			raw:        `{"prompt":"Find every TODO in the repo."}`,
			wantDesc:   "subtask",
			wantPrompt: "Find every TODO in the repo.",
		},
		{
			name:       "description empty defaults",
			raw:        `{"description":"","prompt":"Find every TODO in the repo."}`,
			wantDesc:   "subtask",
			wantPrompt: "Find every TODO in the repo.",
		},
		{
			name:       "description whitespace defaults",
			raw:        `{"description":"  \t ","prompt":"Find every TODO in the repo."}`,
			wantDesc:   "subtask",
			wantPrompt: "Find every TODO in the repo.",
		},
		{
			name:       "description is trimmed",
			raw:        `{"description":"  survey the docs  ","prompt":"go"}`,
			wantDesc:   "survey the docs",
			wantPrompt: "go",
		},
		{
			// The prompt itself is passed through untrimmed: the subagent's
			// task is whatever the model wrote, whitespace included.
			name:       "a prompt with surrounding whitespace is kept verbatim",
			raw:        `{"description":"d","prompt":"  go read main.go  "}`,
			wantDesc:   "d",
			wantPrompt: "  go read main.go  ",
		},
		{
			name:        "not JSON at all",
			raw:         `description: survey the docs`,
			wantErr:     true,
			errMentions: "JSON",
		},
		{
			name:        "truncated mid-string — the shape a max_tokens cutoff produces",
			raw:         `{"description":"survey the docs","prompt":"List every file under d`,
			wantErr:     true,
			errMentions: "JSON",
		},
		{
			// docs/wire-notes.md: the gateway really does send this when a tool
			// call is cut short. It is valid JSON and it contains no task.
			name:        "the observed {\"raw_arguments\":\"\"} payload",
			raw:         `{"raw_arguments":""}`,
			wantErr:     true,
			errMentions: "prompt",
		},
		{
			name:        "empty string arguments",
			raw:         ``,
			wantErr:     true,
			errMentions: "JSON",
		},
		{
			name:        "a JSON array where an object belongs",
			raw:         `["survey the docs","go"]`,
			wantErr:     true,
			errMentions: "JSON",
		},
		{
			name:       "unknown extra keys are ignored, not rejected",
			raw:        `{"description":"d","prompt":"go","model":"opus","budget":3}`,
			wantDesc:   "d",
			wantPrompt: "go",
		},
	}

	for _, c := range cases {
		desc, prompt, err := parseTaskArgs(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: parseTaskArgs(%s) was accepted, returning description %q and prompt %q — "+
					"a subagent is now being launched with a task nobody wrote", c.name, c.raw, desc, prompt)
				continue
			}
			if !strings.Contains(err.Error(), c.errMentions) {
				t.Errorf("%s: the error does not mention %q: %q\n"+
					"this text is the tool result the model reads; if it does not name the field, the model's only "+
					"option is to guess and retry", c.name, c.errMentions, err.Error())
			}
			if desc != "" || prompt != "" {
				t.Errorf("%s: a rejected call still returned description %q / prompt %q; a caller that ignores the "+
					"error spawns on that", c.name, desc, prompt)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: parseTaskArgs(%s) failed: %v — a well-formed delegation was refused", c.name, c.raw, err)
			continue
		}
		if desc != c.wantDesc {
			t.Errorf("%s: description = %q, want %q", c.name, desc, c.wantDesc)
		}
		if prompt != c.wantPrompt {
			t.Errorf("%s: prompt = %q, want %q — this string is the entire brief the subagent gets", c.name, prompt, c.wantPrompt)
		}
	}
}

// The schema the model is shown has to agree with what the parser enforces. If
// `prompt` stopped being required in the schema, a well-behaved model would
// start omitting it and every one of those calls would come back as the error
// above — a round trip burnt on a disagreement inside our own binary.
func TestTaskToolSchemaRequiresWhatTheParserRequires(t *testing.T) {
	def := taskToolDef()
	if def.Name != "task" {
		t.Fatalf("the task tool is named %q; dispatch switches on the literal \"task\", so it would fall through "+
			"to the unknown-tool branch", def.Name)
	}
	req, ok := def.Schema["required"].([]string)
	if !ok {
		t.Fatalf("the schema's `required` is %T, not []string; the adapters marshal it as-is", def.Schema["required"])
	}
	want := map[string]bool{"description": true, "prompt": true}
	for _, r := range req {
		delete(want, r)
	}
	if len(want) > 0 {
		t.Errorf("the schema does not mark %v as required, but parseTaskArgs rejects a call without a prompt — "+
			"the model is being told one thing and judged by another", want)
	}
}

// ---------------------------------------------------------------------------
// agent.tools — the depth fuse
// ---------------------------------------------------------------------------

// The fuse: at the depth limit `task` is REMOVED from the list, not refused at
// call time. A refusal costs a round trip and the tokens of a tool definition
// on every request that can never use it; worse, it is an arbitrary rule, and
// models argue with arbitrary rules by rephrasing until one gets through.
//
// So the assertion has to be on the returned slice. A test that only checked
// "the call was rejected" would pass against exactly the implementation this
// design rejects.
func TestToolsRemovesTaskAtTheDepthLimit(t *testing.T) {
	names := func(ts []Tool) []string {
		var out []string
		for _, tl := range ts {
			out = append(out, tl.Name)
		}
		return out
	}

	cases := []struct {
		name     string
		depth    int
		maxDepth int
		want     []string
	}{
		{"the agent the human talks to", 0, 2, []string{"bash", "task"}},
		{"one level down, still allowed to delegate", 1, 2, []string{"bash", "task"}},
		{"at the limit", 2, 2, []string{"bash"}},
		{"past the limit, if depth ever overshoots", 3, 2, []string{"bash"}},
		{"maxDepth 1 stops the root's children delegating", 1, 1, []string{"bash"}},
		{"maxDepth 0 removes task from the root itself", 0, 0, []string{"bash"}},
	}

	for _, c := range cases {
		a := &agent{depth: c.depth, maxDepth: c.maxDepth}
		got := names(a.tools())
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s (depth %d, maxDepth %d): tools() = %v, want %v",
				c.name, c.depth, c.maxDepth, got, c.want)
		}
		if c.depth >= c.maxDepth {
			for _, n := range got {
				if n == "task" {
					t.Errorf("%s: the task tool is still in the list at depth %d of %d. Leaving it in and refusing "+
						"the call costs a full round trip plus the schema's tokens on every request, and gives the "+
						"model a rule to argue with instead of a tool set to plan within",
						c.name, c.depth, c.maxDepth)
				}
			}
		}
	}
}

// Stage 04: the tool definitions are part of the cached prompt prefix, and the
// prefix is compared byte for byte. A tools() that returned the same two tools
// in a different order on the second call would invalidate the cache on every
// request — a tenfold price increase that shows up as nothing but a bill.
func TestToolOrderIsStableAcrossCalls(t *testing.T) {
	a := &agent{depth: 0, maxDepth: 4}
	var first string
	for i := 0; i < 5; i++ {
		var b strings.Builder
		for _, tl := range a.tools() {
			fmt.Fprintf(&b, "%s|%s|", tl.Name, tl.Description)
		}
		if i == 0 {
			first = b.String()
			continue
		}
		if b.String() != first {
			t.Fatalf("call %d returned a different tool list from call 0; the tool block sits in the cached prefix, "+
				"so every request after the first would be a cache miss and nothing would report it", i)
		}
	}
	if first == "" {
		t.Fatal("tools() returned nothing at depth 0; the agent has no shell")
	}
}

// ---------------------------------------------------------------------------
// skills
// ---------------------------------------------------------------------------

// mulSkillDoc builds a SKILL.md with frontmatter and a body of about n bytes.
func mulSkillDoc(name, description string, n int) string {
	head := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n"
	const line = "Run the command, read the exit code, and stop if it is not zero.\n"
	var b strings.Builder
	b.WriteString(head)
	for b.Len() < n {
		b.WriteString(line)
	}
	return b.String()
}

// mulSkillsRoot writes a skills/ tree into a fresh t.TempDir and returns the
// root to pass to loadSkills. A value of "" creates the directory with no
// SKILL.md in it.
//
// t.TempDir, never the repo's own skills/ — a test that reads the real one
// passes or fails depending on what someone added to it last week.
func mulSkillsRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		dir := filepath.Join(root, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatalf("write SKILL.md in %s: %v", dir, err)
		}
	}
	return root
}

// A project with no skills/ is the normal case, and loadSkills is called on
// every startup before anything has been configured. Returning nil quietly is
// the only acceptable behaviour; a panic here kills the agent at launch for
// every user who never asked for skills.
func TestLoadSkillsWithoutASkillsDirectory(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("loadSkills panicked on a directory with no skills/: %v — the agent now refuses to start "+
				"in any project that has not opted in", r)
		}
	}()
	if got := loadSkills(t.TempDir()); got != nil {
		t.Errorf("loadSkills returned %d skills for a directory with no skills/ in it", len(got))
	}
	if got := loadSkills(filepath.Join(t.TempDir(), "does-not-exist")); got != nil {
		t.Errorf("loadSkills returned %d skills for a root that does not exist", len(got))
	}
}

// Path is what the model types after `cat`. On Windows filepath.Join produces
// backslashes, and `cat skills\deploy\SKILL.md` inside bash reads the escapes,
// not the path — the skill silently cannot be opened, on the one platform where
// nobody testing on a Mac will see it.
//
// The sort is here for the same reason the tool order is: the index sits in the
// cached prefix, and directory order is not a promise any filesystem makes.
func TestLoadSkillsSortsAndPublishesSlashSeparatedPaths(t *testing.T) {
	root := mulSkillsRoot(t, map[string]string{
		"zebra":  mulSkillDoc("zebra", "the last one alphabetically", 300),
		"deploy": mulSkillDoc("deploy", "ship a build to staging", 300),
		"mango":  mulSkillDoc("mango", "something in the middle", 300),
	})

	got := loadSkills(root)
	if len(got) != 3 {
		t.Fatalf("loadSkills found %d skills, want 3", len(got))
	}

	wantOrder := []string{"deploy", "mango", "zebra"}
	for i, s := range got {
		if s.Name != wantOrder[i] {
			t.Errorf("skill %d is %q, want %q — the index is part of the cached prompt prefix, and a list whose "+
				"order follows the filesystem changes between machines and between runs", i, s.Name, wantOrder[i])
		}
		if strings.Contains(s.Path, `\`) {
			t.Errorf("skill %q has Path %q, which contains a backslash. The model runs `cat %s` in bash, where "+
				"backslashes are escapes: the skill body cannot be read at all, and only on Windows",
				s.Name, s.Path, s.Path)
		}
		if want := "skills/" + s.Name + "/SKILL.md"; s.Path != want {
			t.Errorf("skill %q has Path %q, want %q — it must be relative to the working directory the model shares",
				s.Name, s.Path, want)
		}
		onDisk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(s.Path)))
		if err != nil {
			t.Fatalf("the path loadSkills published does not resolve to a file: %v", err)
		}
		if s.BodyBytes != len(onDisk) {
			t.Errorf("skill %q reports BodyBytes %d for a %d-byte file; the progressive-disclosure accounting is "+
				"reporting a cost nobody pays", s.Name, s.BodyBytes, len(onDisk))
		}
	}
}

// A skill with no description is invisible: the index is the only thing the
// model ever sees, so a nameless line in it is a line that will never be
// chosen. Keeping it means paying prefix tokens forever for something that
// cannot be used.
func TestLoadSkillsSkipsASkillWithNoDescription(t *testing.T) {
	root := mulSkillsRoot(t, map[string]string{
		"good":         mulSkillDoc("good", "this one can be chosen", 200),
		"nodesc":       "---\nname: nodesc\n---\n\nA body nobody will ever ask for.\n",
		"nofront":      "Just a Markdown file with no frontmatter at all.\n",
		"emptydesc":    "---\nname: emptydesc\ndescription:   \n---\n\nbody\n",
		"notaskilldir": "", // a directory with no SKILL.md in it
	})

	got := loadSkills(root)
	if len(got) != 1 {
		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		t.Fatalf("loadSkills kept %v; only the skill with a description belongs in the index, because a line with "+
			"no description is prompt overhead the model can never act on", names)
	}
	if got[0].Name != "good" {
		t.Errorf("the surviving skill is %q, want \"good\"", got[0].Name)
	}
}

// The directory name is the fallback, so a skill author can write two lines of
// frontmatter instead of three. Dropping the skill instead would punish the
// omission of a field the filesystem already answers.
func TestLoadSkillsFallsBackToTheDirectoryNameWhenNameIsMissing(t *testing.T) {
	root := mulSkillsRoot(t, map[string]string{
		"release-notes": "---\ndescription: draft the release notes from the git log\n---\n\nSteps here.\n",
	})
	got := loadSkills(root)
	if len(got) != 1 {
		t.Fatalf("loadSkills found %d skills, want 1 — a skill with a description but no explicit name was dropped", len(got))
	}
	if got[0].Name != "release-notes" {
		t.Errorf("Name = %q, want the directory name \"release-notes\"; the sort order and the index label both "+
			"come from this field, so an empty one puts a blank line at the top of the list", got[0].Name)
	}
}

// Files loose in skills/ are not skills — a README, a .gitkeep, an editor
// backup. Treating one as a skill directory would make loadSkills fail on a
// perfectly ordinary tree.
func TestLoadSkillsIgnoresLooseFilesInTheSkillsDirectory(t *testing.T) {
	root := mulSkillsRoot(t, map[string]string{
		"deploy": mulSkillDoc("deploy", "ship a build to staging", 200),
	})
	if err := os.WriteFile(filepath.Join(root, "skills", "README.md"), []byte("# skills\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if got := loadSkills(root); len(got) != 1 {
		t.Errorf("loadSkills found %d skills next to a README.md, want 1", len(got))
	}
}

// parseFrontmatter is twenty lines instead of a YAML dependency, so the exact
// edge of what it understands has to be written down. Everything it does not
// understand yields "", which means the skill disappears from the index — a
// silent failure whose only symptom is that the model never uses the skill you
// wrote.
func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantName string
		wantDesc string
	}{
		{
			name:     "the ordinary case",
			in:       "---\nname: deploy\ndescription: ship a build to staging\n---\n\nbody\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name: "no frontmatter at all",
			in:   "# deploy\n\nJust a Markdown file.\n",
		},
		{
			// The author started the block and never closed it. Guessing where
			// it ends would put the whole document in the description.
			name: "unterminated frontmatter",
			in:   "---\nname: deploy\ndescription: ship a build to staging\n\nbody with no closing fence\n",
		},
		{
			name: "empty input",
			in:   "",
		},
		{
			name:     "double-quoted values",
			in:       "---\nname: \"deploy\"\ndescription: \"ship a build to staging\"\n---\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name:     "single-quoted values",
			in:       "---\nname: 'deploy'\ndescription: 'ship a build to staging'\n---\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			// A colon in a description is not exotic; it is how anyone writes
			// "do x: then y". Splitting on every colon truncates the sentence
			// at the useful half.
			name:     "a colon inside the value keeps the whole tail",
			in:       "---\nname: deploy\ndescription: build the image: then push it and tag the release\n---\n",
			wantName: "deploy",
			wantDesc: "build the image: then push it and tag the release",
		},
		{
			// A skill authored in Notepad. The BOM sits in front of the fence,
			// HasPrefix("---") fails, and the skill vanishes with no error
			// anywhere — on the platform this repo is developed on.
			name:     "a leading UTF-8 BOM",
			in:       mulBOM + "---\nname: deploy\ndescription: ship a build to staging\n---\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name:     "CRLF line endings",
			in:       "---\r\nname: deploy\r\ndescription: ship a build to staging\r\n---\r\n\r\nbody\r\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name:     "a BOM and CRLF together, which is what Windows actually writes",
			in:       mulBOM + "---\r\nname: deploy\r\ndescription: ship a build to staging\r\n---\r\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name:     "leading blank lines before the fence",
			in:       "\n\n---\nname: deploy\ndescription: ship a build to staging\n---\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name:     "keys we do not know are ignored, not fatal",
			in:       "---\nversion: 3\nname: deploy\nallowed-tools: bash\ndescription: ship a build to staging\n---\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name:     "a line with no colon is skipped",
			in:       "---\nname: deploy\njust some prose\ndescription: ship a build to staging\n---\n",
			wantName: "deploy",
			wantDesc: "ship a build to staging",
		},
		{
			name:     "an empty frontmatter block yields nothing",
			in:       "---\n---\n\nbody\n",
			wantName: "",
			wantDesc: "",
		},
		{
			name:     "description only",
			in:       "---\ndescription: ship a build to staging\n---\n",
			wantName: "",
			wantDesc: "ship a build to staging",
		},
	}

	for _, c := range cases {
		name, desc := parseFrontmatter(c.in)
		if name != c.wantName {
			t.Errorf("%s: name = %q, want %q (input %q)", c.name, name, c.wantName, c.in)
		}
		if desc != c.wantDesc {
			t.Errorf("%s: description = %q, want %q (input %q)\n"+
				"a wrong or empty description here removes the skill from the index, and nothing prints when that "+
				"happens — the only symptom is a skill the model never uses", c.name, desc, c.wantDesc, c.in)
		}
	}
}

// Zero skills must produce the empty string, not an empty block. An
// `<skills>` header with nothing under it goes into the cached prefix of every
// request for the life of every session in every project that has no skills
// directory, and tells the model there is a list it should be consulting.
func TestSkillsPromptIsExactlyEmptyForZeroSkills(t *testing.T) {
	for _, in := range [][]skill{nil, {}} {
		if got := skillsPrompt(in); got != "" {
			t.Errorf("skillsPrompt(%v) = %q, want \"\" — every request in every project without skills now carries "+
				"an empty section, and the model is being told to consult a list that does not exist", in, got)
		}
	}
	idx, bodies := skillsCost(nil)
	if idx != 0 || bodies != 0 {
		t.Errorf("skillsCost(nil) = (%d, %d), want (0, 0)", idx, bodies)
	}
}

// The index is the entire interface between the model and the skills on disk:
// a path it can cat and a sentence that says whether to bother. Both have to be
// in there verbatim, and so do the three instructions, each of which exists
// because of a specific way this goes wrong.
func TestSkillsPromptCarriesEveryPathDescriptionAndInstruction(t *testing.T) {
	skills := []skill{
		{Name: "deploy", Description: "ship a build to staging and watch the rollout", Path: "skills/deploy/SKILL.md", BodyBytes: 3000},
		{Name: "triage", Description: "work through a failing CI run from the top", Path: "skills/triage/SKILL.md", BodyBytes: 4000},
		{Name: "release-notes", Description: "draft release notes from the git log", Path: "skills/release-notes/SKILL.md", BodyBytes: 2000},
	}
	got := skillsPrompt(skills)

	for _, s := range skills {
		if !strings.Contains(got, s.Path) {
			t.Errorf("the index does not contain %q — the model has no path to cat, so the body is unreachable "+
				"however well it is written", s.Path)
		}
		if !strings.Contains(got, s.Description) {
			t.Errorf("the index does not contain the description of %q — the description is the ONLY basis on which "+
				"the model decides whether to open the skill", s.Name)
		}
	}

	// Compared against whitespace-collapsed text, so re-wrapping the prompt is
	// allowed but deleting an instruction is not.
	flat := strings.Join(strings.Fields(got), " ")
	for _, want := range []struct {
		phrase string
		why    string
	}{
		{"read it first with `cat`", "without it the model acts on the one-line description, which was written to be selectable, not to be sufficient"},
		{"Read at most one before acting", "without it a model given five plausible skills reads all five, turning a token saving into a token cost plus five round trips"},
		{"If none clearly applies, ignore this list", "without it the list reads as a menu the model has to order from, and it will find one that nearly fits"},
	} {
		if !strings.Contains(flat, want.phrase) {
			t.Errorf("the index is missing the instruction %q: %s", want.phrase, want.why)
		}
	}

	if !strings.Contains(got, "<skills>") || !strings.Contains(got, "</skills>") {
		t.Error("the index is not delimited; the model cannot tell where the skill list ends and the rest of the system prompt begins")
	}
}

// The whole argument for progressive disclosure is an arithmetic one: names and
// descriptions cost a little in every request forever, bodies cost a lot but
// only when read. If skillsCost cannot show that gap on a realistic tree, the
// number it prints is not evidence for the design it exists to justify.
func TestSkillsCostShowsBodiesDwarfingTheIndex(t *testing.T) {
	root := mulSkillsRoot(t, map[string]string{
		"deploy":        mulSkillDoc("deploy", "ship a build to staging and watch the rollout", 3000),
		"triage":        mulSkillDoc("triage", "work through a failing CI run from the top", 4000),
		"release-notes": mulSkillDoc("release-notes", "draft release notes from the git log", 2500),
	})
	skills := loadSkills(root)
	if len(skills) != 3 {
		t.Fatalf("fixture drift: loadSkills found %d skills, want 3", len(skills))
	}

	idx, bodies := skillsCost(skills)
	if idx != len(skillsPrompt(skills)) {
		t.Errorf("indexBytes = %d but the rendered index is %d bytes; the number printed at startup is not the "+
			"number being sent", idx, len(skillsPrompt(skills)))
	}
	if idx <= 0 {
		t.Fatalf("indexBytes = %d for three skills; the permanent overhead is being reported as free", idx)
	}

	var wantBodies int
	for _, s := range skills {
		wantBodies += s.BodyBytes
	}
	if bodies != wantBodies {
		t.Errorf("bodyBytes = %d, want %d (the sum of the files on disk)", bodies, wantBodies)
	}
	if bodies <= 5*idx {
		t.Errorf("the bodies (%d bytes) are only %.1fx the index (%d bytes) on a realistic tree. That ratio IS the "+
			"argument for keeping bodies on disk; if it is near 1, indexing costs as much as loading and the design "+
			"buys nothing", bodies, float64(bodies)/float64(idx), idx)
	}
}

// ---------------------------------------------------------------------------
// dispatch — the ordering guarantee
// ---------------------------------------------------------------------------

// dispatch's contract: one result block per call, in the order the model
// emitted them, each carrying the id of the call it answers.
//
// Every part of that is load-bearing on the NEXT request rather than this one.
// A missing result is an unanswered tool_use_id and the request is rejected; a
// misordered pair means the model reads the output of `git log` as the answer
// to `ls`, silently. That is stage 05's bug, and the symptom always points at
// the request builder rather than at whatever dropped the block.
func TestDispatchAnswersEveryCallOnceInOrderAndByID(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	calls := []Block{
		{Kind: BlockToolCall, ID: "call_1", Name: "bash", Args: mulBash("echo mul-alpha")},
		{Kind: BlockToolCall, ID: "call_2", Name: "frobnicate", Args: `{"target":"x"}`},
		{Kind: BlockToolCall, ID: "call_3", Name: "bash", Args: mulBash("echo mul-beta")},
		{Kind: BlockToolCall, ID: "call_4", Name: "search_files", Args: `{}`},
		{Kind: BlockToolCall, ID: "call_5", Name: "bash", Args: mulBash("echo mul-gamma")},
	}

	results, stopped := a.dispatch(context.Background(), 1, calls)
	if stopped {
		t.Fatal("dispatch reported the session stopped with a --yolo gate")
	}
	if len(results) != len(calls) {
		t.Fatalf("dispatch returned %d results for %d calls; the provider rejects a request whose tool calls are not "+
			"all answered, one turn later and with an error naming the request builder", len(results), len(calls))
	}

	for i, r := range results {
		if r.Kind != BlockToolResult {
			t.Errorf("result %d has Kind %q, not a tool result; a zero block in this slot is what a skipped result "+
				"looks like on the wire", i, r.Kind)
		}
		if r.ID != calls[i].ID {
			t.Errorf("result %d answers %q but sits where the answer to %q belongs. The model reads results "+
				"positionally as well as by id, so this hands it one command's output as another's", i, r.ID, calls[i].ID)
		}
		if strings.TrimSpace(r.Text) == "" {
			t.Errorf("result %d (%s) is empty; the model is told its command produced nothing at all", i, r.ID)
		}
	}

	// Content, not just ids: a per-index shuffle that preserved the id mapping
	// would still be wrong, and this is what catches it.
	for i, want := range map[int]string{0: "mul-alpha", 2: "mul-beta", 4: "mul-gamma"} {
		if !strings.Contains(results[i].Text, want) {
			t.Errorf("result %d does not contain %q; it says %q instead", i, want, results[i].Text)
		}
	}
	if strings.Contains(results[0].Text, "mul-beta") || strings.Contains(results[4].Text, "mul-alpha") {
		t.Error("two bash results have been crossed; each shell command's output must land in its own block")
	}

	if got := rec.count(KindToolResult); got != len(calls) {
		t.Errorf("%d tool_result events were emitted for %d calls; the trace and the conversation now disagree "+
			"about what the model was told", got, len(calls))
	}
}

// A tool name the model invented is recoverable — but only if the result says
// which name was wrong. "Unknown tool" alone leaves a model that emitted three
// calls with no way to tell which of them to stop using.
func TestDispatchNamesTheToolItDoesNotHave(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")

	results, stopped := a.dispatch(context.Background(), 1, []Block{
		{Kind: BlockToolCall, ID: "call_x", Name: "read_file", Args: `{"path":"main.go"}`},
	})
	if stopped {
		t.Fatal("an unknown tool name stopped the session; it is a recoverable mistake, not an abort")
	}
	if len(results) != 1 {
		t.Fatalf("dispatch returned %d results for an unknown tool, want 1 — dropping the block makes the NEXT "+
			"request malformed, which is a much worse failure than the one the model made", len(results))
	}
	if !strings.Contains(results[0].Text, "read_file") {
		t.Errorf("the result does not name the tool that does not exist: %q\n"+
			"the model has to know which of its calls to abandon, and this text is the only place it is told",
			results[0].Text)
	}
	if results[0].ID != "call_x" {
		t.Errorf("the result answers %q, not the call that was made", results[0].ID)
	}
}

// A task call whose arguments do not parse must come back as a tool result, not
// as a subagent launched with an empty prompt and not as a dropped block. The
// gate must not be asked either: there is nothing to approve.
func TestDispatchTurnsMalformedTaskArgsIntoAResultInsteadOfSpawning(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, "")

	results, stopped := a.dispatch(context.Background(), 1, []Block{
		{Kind: BlockToolCall, ID: "call_bad", Name: "task", Args: `{"description":"survey the docs"}`},
	})
	if stopped {
		t.Fatal("bad task arguments stopped the session")
	}
	if len(results) != 1 || results[0].ID != "call_bad" {
		t.Fatalf("dispatch returned %d results for one malformed task call", len(results))
	}
	if !strings.Contains(results[0].Text, "prompt") {
		t.Errorf("the result does not say which field was missing: %q — the model's next attempt is a guess", results[0].Text)
	}
	if n := rec.count(KindSubagentStart); n != 0 {
		t.Errorf("%d subagents were started from a call with no prompt; a child with an empty task burns a whole "+
			"context window discovering it has nothing to do", n)
	}
	if n := rec.count(KindGateVerdict); n != 0 {
		t.Errorf("the gate was asked %d times about a call that could not be parsed; the user is being shown a "+
			"permission prompt for work that was never going to run", n)
	}
	if n := a.children; n != 0 {
		t.Errorf("the child counter advanced to %d without a child being spawned", n)
	}
}

// A denied call still gets a result block. The temptation is to skip it — the
// command did not run, so what is there to report — and the consequence is an
// unanswered tool_use_id on the next request, which fails the whole
// conversation rather than the one command the user declined.
func TestDispatchAnswersDeniedCallsToo(t *testing.T) {
	// available:false makes ask() deny without needing a terminal.
	a, _ := mulAgent(&gate{available: false}, "")

	calls := []Block{
		{Kind: BlockToolCall, ID: "call_1", Name: "bash", Args: mulBash("rm -rf /")},
		{Kind: BlockToolCall, ID: "call_2", Name: "bash", Args: mulBash("echo two")},
		{Kind: BlockToolCall, ID: "call_3", Name: "task", Args: `{"description":"survey","prompt":"look around"}`},
	}
	results, stopped := a.dispatch(context.Background(), 1, calls)

	if stopped {
		t.Error("a denial stopped the session; deny refuses one call, abort ends the turn, and conflating them " +
			"throws away the rest of the model's work")
	}
	if len(results) != len(calls) {
		t.Fatalf("dispatch returned %d results for %d denied calls; every one of them still needs an answer or the "+
			"next request is rejected for an unanswered tool call", len(results), len(calls))
	}
	for i, r := range results {
		if r.Kind != BlockToolResult || r.ID != calls[i].ID {
			t.Errorf("result %d is %q/%q, want a tool result answering %q", i, r.Kind, r.ID, calls[i].ID)
		}
		if !strings.Contains(r.Text, "denied") {
			t.Errorf("result %d does not tell the model it was denied: %q — without that it reads the block as "+
				"command output and continues on a false premise", i, r.Text)
		}
	}
}

// After an abort, the remaining calls are not executed — but they are still
// answered. Counting the blocks rather than trusting the flag is the point: the
// flag says the loop stopped, and only the count says the conversation is still
// sendable afterwards.
func TestDispatchStillAnswersEveryCallAfterAnAbort(t *testing.T) {
	// An input stream that is already at EOF makes ask() return abort.
	g := &gate{available: true, read: lineReader(bufio.NewScanner(strings.NewReader(""))), out: io.Discard}
	a, rec := mulAgent(g, "")

	calls := []Block{
		{Kind: BlockToolCall, ID: "call_1", Name: "bash", Args: mulBash("echo one")},
		{Kind: BlockToolCall, ID: "call_2", Name: "bash", Args: mulBash("echo two")},
		{Kind: BlockToolCall, ID: "call_3", Name: "task", Args: `{"description":"survey","prompt":"look around"}`},
		{Kind: BlockToolCall, ID: "call_4", Name: "made_up", Args: `{}`},
	}
	results, stopped := a.dispatch(context.Background(), 1, calls)

	if !stopped {
		t.Fatal("dispatch did not report the abort; the turn loop will call the model again after the user asked it to stop")
	}
	if len(results) != len(calls) {
		t.Fatalf("dispatch returned %d results after an abort on call 1 of %d. The conversation is appended to the "+
			"history either way, so an unanswered call means the session cannot even be resumed — the user's stop "+
			"has corrupted the transcript", len(results), len(calls))
	}
	for i, r := range results {
		if r.ID != calls[i].ID || r.Kind != BlockToolResult {
			t.Errorf("result %d is %q/%q, want a tool result answering %q", i, r.Kind, r.ID, calls[i].ID)
		}
	}
	for i := 1; i < len(results); i++ {
		if !strings.Contains(results[i].Text, "not executed") {
			t.Errorf("result %d after the abort says %q; it must say the command was not executed, or the model "+
				"treats an empty answer as an empty result", i, results[i].Text)
		}
	}
	if n := rec.count(KindSubagentStart); n != 0 {
		t.Errorf("%d subagents were started after the user stopped the session", n)
	}
	// Exactly one question was asked: the abort must short-circuit the rest,
	// not ask the user four times whether they meant it.
	if n := rec.count(KindGateVerdict); n != 1 {
		t.Errorf("the gate produced %d verdicts after an abort on the first call, want 1", n)
	}
}

// The ordering guarantee under real concurrency, which is the reason this
// function exists at all: two subagents run at the same time, the second
// finishes first, and the history must still read in the order the model asked.
//
// If results were collected as they landed, the same session replayed twice
// would produce two different message arrays, two different prompt prefixes,
// and — per stage 04 — a cache that never hits. Concurrency is allowed to
// change how long things take. It is not allowed to change what the
// conversation says.
func TestDispatchKeepsSubagentResultsInTheModelsOrder(t *testing.T) {
	betaDone := make(chan struct{})
	p := &mulFakeProvider{
		reply: func(prompt string) string { return "report for " + prompt },
		before: func(prompt string) {
			// Hold alpha until beta has finished, so completion order is the
			// reverse of call order every run, with no sleeps to flake on.
			if prompt == "alpha" {
				select {
				case <-betaDone:
				case <-time.After(3 * time.Second):
				}
			}
		},
		after: func(prompt string) {
			if prompt == "beta" {
				close(betaDone)
			}
		},
	}

	a, rec := mulAgent(&gate{yolo: true}, "")
	a.lad = newLadder(rung{p: p})
	a.httpc = &http.Client{Transport: mulRoundTrip{}}

	calls := []Block{
		{Kind: BlockToolCall, ID: "call_alpha", Name: "task", Args: `{"description":"alpha","prompt":"alpha"}`},
		{Kind: BlockToolCall, ID: "call_beta", Name: "task", Args: `{"description":"beta","prompt":"beta"}`},
	}
	results, stopped := a.dispatch(context.Background(), 1, calls)
	if stopped {
		t.Fatal("dispatch reported the session stopped with a --yolo gate")
	}

	// The precondition. Without it this test would pass against a dispatch that
	// ran the two children one after another in call order, which is the one
	// implementation that cannot get the ordering wrong.
	if order := p.order(); len(order) != 2 || order[0] != "beta" || order[1] != "alpha" {
		t.Fatalf("the subagents completed in the order %v, want [beta alpha]. Either they were not run "+
			"concurrently — in which case this test proves nothing about the ordering guarantee — or the fixture "+
			"has drifted", order)
	}

	if len(results) != 2 {
		t.Fatalf("dispatch returned %d results for 2 task calls", len(results))
	}
	for i, want := range []struct{ id, text string }{
		{"call_alpha", "report for alpha"},
		{"call_beta", "report for beta"},
	} {
		if results[i].ID != want.id {
			t.Errorf("result %d answers %q, want %q — the results were collected in completion order, so the "+
				"transcript now depends on which subagent happened to finish first and no two runs of the same "+
				"session produce the same prompt prefix", i, results[i].ID, want.id)
		}
		if results[i].Text != want.text {
			t.Errorf("result %d carries %q, want %q — the reports have been swapped between the calls they answer",
				i, results[i].Text, want.text)
		}
	}

	// The child's events landed in the parent's stream, stamped with the tree
	// coordinates. This is the end-to-end check that newChild forks the bus
	// rather than making a new one.
	if n := rec.count(KindSubagentStart); n != 2 {
		t.Errorf("%d subagent_start events, want 2", n)
	}
	deep := 0
	for _, e := range rec.events {
		if e.Depth == 1 && e.Agent != "" {
			deep++
		}
	}
	if deep == 0 {
		t.Error("no event in the parent's trace carries Depth 1 and an agent name: the children emitted into a bus " +
			"of their own, so one trace file no longer holds the whole tree and the question 'what was the parent " +
			"doing while the child ran' cannot be answered")
	}
}

// ---------------------------------------------------------------------------
// spawn and newChild
// ---------------------------------------------------------------------------

// newChild is the "what is shared and what is not" decision written out as
// code, and each half fails differently: sharing the compactor calibrates the
// child's estimator on the parent's traffic, and NOT sharing the gate means a
// subagent runs commands nobody approved.
func TestNewChildSharesTheGateAndForksEverythingElse(t *testing.T) {
	parent, _ := mulAgent(&gate{yolo: true}, "")
	parent.cfg.maxTurns = 40
	parent.cfg.subTurns = 6
	parent.comp = newCompactor(200_000, 0.8, 0.3)
	parent.comp.est.observe(4000, 1000) // ratio 4.0

	child := parent.newChild("survey docs#1", func() string { return "child system" })

	if child.g != parent.g {
		t.Error("the child got its own gate; a subagent's permission prompts must reach the same human, or the " +
			"agent has a second, unsupervised way to run commands")
	}
	if child.depth != parent.depth+1 {
		t.Errorf("child depth = %d, parent depth = %d; the depth fuse never fires and subagents recurse until "+
			"something else runs out", child.depth, parent.depth)
	}
	if child.bus.Depth() != parent.bus.Depth()+1 {
		t.Errorf("the child's bus is at depth %d, the parent's at %d; every event the child emits would be "+
			"attributed to the parent", child.bus.Depth(), parent.bus.Depth())
	}
	if child.maxDepth != parent.maxDepth {
		t.Errorf("child maxDepth = %d, want the parent's %d; the limit has to be absolute, not per-agent", child.maxDepth, parent.maxDepth)
	}
	if child.comp == parent.comp {
		t.Error("the child shares the parent's compactor. Two conversations calibrating one estimator is a shared " +
			"mutable object, and the symptom is a compaction that fires at the wrong size in whichever agent " +
			"happened to write to it last")
	}
	if child.comp.est.ratio != parent.comp.est.ratio {
		t.Errorf("the child's estimator starts at %.2f instead of inheriting the parent's measured %.2f; it pays "+
			"for the 3.6 cold start all over again", child.comp.est.ratio, parent.comp.est.ratio)
	}
	if child.cfg.maxTurns != parent.cfg.subTurns {
		t.Errorf("the child's turn budget is %d, want the configured subTurns %d. A subagent that needs thirty "+
			"rounds was given a task that should have been three subagents, and this fuse is the only thing that "+
			"will say so", child.cfg.maxTurns, parent.cfg.subTurns)
	}
	if child.stable != parent.stable {
		t.Error("the child's stable context differs from the parent's; they then share no cache prefix and every " +
			"subagent pays full price for the environment block")
	}
}

// A subagent that returns nothing must say so. The parent has no transcript to
// inspect and no way to ask a follow-up, so an empty string is read as a
// finding — "I looked and there was nothing" — and the parent proceeds
// confidently on the strength of a child that hit its turn limit.
//
// A subTurns budget of zero makes runTurn return before it ever calls a
// provider, so this exercises the guard with no network anywhere near it.
func TestSpawnReportsAnEmptyChildAsAFailureRatherThanAnEmptyResult(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, "")
	a.cfg.subTurns = 0 // the child stops before its first model call

	report, _, err := a.spawn(context.Background(), "call_1", "probe", "look at everything")
	if err != nil {
		t.Fatalf("spawn returned an error: %v", err)
	}
	if strings.TrimSpace(report) == "" {
		t.Fatal("spawn returned an empty report. The parent cannot tell that apart from a subagent that looked and " +
			"found nothing, so it continues as though the question were answered")
	}
	if !strings.Contains(report, "no final report") {
		t.Errorf("the report does not say the subagent produced nothing: %q", report)
	}

	if n := rec.count(KindSubagentStart); n != 1 {
		t.Errorf("%d subagent_start events, want 1 — the trace has to show the delegation even when it fails", n)
	}
	ends := rec.kind(KindSubagentEnd)
	if len(ends) != 1 {
		t.Fatalf("%d subagent_end events, want 1", len(ends))
	}
	if ends[0].Text != report {
		t.Errorf("the subagent_end event records %q but the parent was handed %q; the trace is not evidence about "+
			"what the parent actually read", ends[0].Text, report)
	}
	if ends[0].Bytes != len(report) {
		t.Errorf("subagent_end reports %d bytes for a %d-byte report", ends[0].Bytes, len(report))
	}
	if ends[0].ToolID != "call_1" {
		t.Errorf("subagent_end carries tool id %q, want \"call_1\"; nothing can join the start and end of this "+
			"delegation in the trace", ends[0].ToolID)
	}

	// The child's own events are in the parent's stream, one level down.
	found := false
	for _, e := range rec.events {
		if e.Depth == 1 && e.Agent == "probe#1" {
			found = true
		}
	}
	if !found {
		t.Error("nothing in the trace carries Depth 1 and the agent id \"probe#1\"; the child's events are either " +
			"missing or attributed to the parent")
	}
}

// ---------------------------------------------------------------------------
// lastAssistantText — the child's return value
// ---------------------------------------------------------------------------

// This function IS the subagent's return value, and every wrong answer is a
// specific lie told to the parent: the first message instead of the last is a
// stale answer, an empty string is "nothing to report", and a tool-call-only
// message is silence where a conclusion belongs.
func TestLastAssistantText(t *testing.T) {
	callBlock := func(id, cmd string) Block {
		return Block{Kind: BlockToolCall, ID: id, Name: "bash", Args: mulBash(cmd)}
	}

	cases := []struct {
		name string
		msgs []Msg
		want string
	}{
		{
			name: "skips the trailing tool-result message",
			msgs: []Msg{
				TextMsg(RoleUser, "count the go files"),
				TextMsg(RoleAssistant, "There are 21 Go files under stages/."),
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("t1", "21\n[exit 0]")}},
			},
			want: "There are 21 Go files under stages/.",
		},
		{
			name: "the LAST assistant text, not the first",
			msgs: []Msg{
				TextMsg(RoleUser, "count the go files"),
				TextMsg(RoleAssistant, "Let me look."),
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("t1", "21\n[exit 0]")}},
				TextMsg(RoleAssistant, "21 Go files, 9184 lines."),
			},
			want: "21 Go files, 9184 lines.",
		},
		{
			name: "skips an assistant message that is only tool calls",
			msgs: []Msg{
				TextMsg(RoleUser, "count them"),
				TextMsg(RoleAssistant, "21 Go files, 9184 lines."),
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("t1", "ok")}},
				{Role: RoleAssistant, Blocks: []Block{callBlock("t2", "wc -l *.go")}},
			},
			want: "21 Go files, 9184 lines.",
		},
		{
			name: "skips an assistant message whose text is only whitespace",
			msgs: []Msg{
				TextMsg(RoleUser, "count them"),
				TextMsg(RoleAssistant, "21 Go files, 9184 lines."),
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("t1", "ok")}},
				TextMsg(RoleAssistant, "   \n\t "),
			},
			want: "21 Go files, 9184 lines.",
		},
		{
			name: "text alongside a tool call in the final message still counts",
			msgs: []Msg{
				TextMsg(RoleUser, "count them"),
				{Role: RoleAssistant, Blocks: []Block{
					{Kind: BlockText, Text: "21 Go files. Checking the tests too."},
					callBlock("t1", "wc -l *_test.go"),
				}},
			},
			want: "21 Go files. Checking the tests too.",
		},
		{
			name: "an empty conversation",
			msgs: nil,
			want: "",
		},
		{
			name: "no assistant message at all",
			msgs: []Msg{TextMsg(RoleUser, "count them")},
			want: "",
		},
		{
			name: "assistant messages exist but none of them said anything",
			msgs: []Msg{
				TextMsg(RoleUser, "count them"),
				{Role: RoleAssistant, Blocks: []Block{callBlock("t1", "wc -l *.go")}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("t1", "9184\n[exit 0]")}},
			},
			want: "",
		},
	}

	for _, c := range cases {
		got := lastAssistantText(c.msgs)
		if got != c.want {
			t.Errorf("%s: lastAssistantText = %q, want %q", c.name, got, c.want)
		}
		// The empty cases are asserted deliberately rather than incidentally:
		// spawn turns "" into an explicit failure string, and that branch stays
		// reachable only while this function really does return "".
		if c.want == "" && got != "" {
			t.Errorf("%s: a conversation with no assistant text returned %q. spawn's guard only fires on the empty "+
				"string, so this value is handed to the parent as the subagent's finding", c.name, got)
		}
	}
}

// firstLine is what the gate prompt and the trace show for a subagent's task:
// the first line, with a marker saying there is more. Swallowing the marker
// makes a truncated prompt look complete, which is the one thing a permission
// prompt must never do.
func TestFirstLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"list every TODO", "list every TODO"},
		{"  list every TODO  ", "list every TODO"},
		{"list every TODO\nand say which file it is in", "list every TODO …"},
		{"  list every TODO  \nmore", "list every TODO …"},
		// Leading whitespace is trimmed before the cut, so this is the only
		// line there is and it gets no ellipsis. The previous behaviour
		// returned " …" — an ellipsis with nothing in front of it, on the line
		// a human is being asked to authorise.
		{"\nsecond line", "second line"},
		{"\n\n  first real line\nand more", "first real line …"},
		{"", ""},
		{"one\ntwo\nthree", "one …"},
	}
	for _, c := range cases {
		if got := firstLine(c.in); got != c.want {
			t.Errorf("firstLine(%q) = %q, want %q — the user approves a subagent on the strength of this line, so "+
				"it must not read as complete when it is not", c.in, got, c.want)
		}
	}
}

// The permission prompt must name the command it is asking about.
//
// Until stage 07 it did not need to: under a strictly sequential
// print-then-ask loop, "run?" can only refer to the line above it. Concurrent
// subagents removed that guarantee — the command text reaches the terminal via
// the renderer under the bus lock, and the question via the gate under its own
// lock, with no ordering between them. A user shown two commands and then one
// bare "run?" can authorise the wrong one.
//
// This is the regression guard for that, and it is a security test rather than
// a cosmetic one.
func TestGateQuestionNamesItsCommand(t *testing.T) {
	for _, command := range []string{
		"rm -rf /tmp/build",
		"echo hello",
		"grep -rn 'x' . 2>&1 | head -5",
	} {
		var out bytes.Buffer
		g := &gate{
			available: true,
			out:       &out,
			read:      lineReader(bufio.NewScanner(strings.NewReader("n\n"))),
		}
		if v, _ := g.ask(command); v != deny {
			t.Fatalf("answering n gave verdict %q, want deny", v)
		}
		got := out.String()
		if !strings.Contains(got, command) {
			t.Errorf("the prompt for %q did not contain the command:\n%s\n"+
				"A question that does not name its subject can be answered for a different "+
				"command entirely once subagents print concurrently.", command, got)
		}
		for _, want := range []string{"y", "n", "a", "q"} {
			if !strings.Contains(got, want) {
				t.Errorf("the prompt no longer offers %q:\n%s", want, got)
			}
		}
		// `a` sets always on the SHARED gate, so the prompt has to say so.
		if !strings.Contains(got, "every agent") {
			t.Errorf("the prompt does not say that `a` applies to every agent, but it does:\n%s\n"+
				"One subagent's \"allow all\" disarms the gate for the parent and every sibling.", got)
		}
	}
}

// yolo and an unavailable terminal must not print a question at all.
func TestGateDoesNotAskWhenItCannot(t *testing.T) {
	var out bytes.Buffer
	g := &gate{yolo: true, out: &out}
	if v, _ := g.ask("rm -rf /"); v != allow {
		t.Error("--yolo did not allow")
	}
	if out.Len() != 0 {
		t.Errorf("--yolo printed a prompt nobody will answer: %q", out.String())
	}

	out.Reset()
	g = &gate{available: false, out: &out}
	v, why := g.ask("rm -rf /")
	if v != deny {
		t.Errorf("with no terminal the verdict was %q, want deny — a gate that cannot ask must not allow", v)
	}
	if why == "" {
		t.Error("the refusal gave no reason, so the model cannot tell it apart from a user saying no")
	}
}
