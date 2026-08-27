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

// 这个文件测的是阶段 07：长成树以后的总线、task 工具的参数解析、
// agent.tools 里的深度保险丝、skills.go，以及 dispatch 的承诺——并发
// 执行照样产出确定的历史。
//
// 这里没有任何代码走网络去调供应商。唯一需要 Provider 的地方——穿过
// dispatch 的那条并发路径——用的是下面的 mulFakeProvider，它靠假的
// RoundTripper 从请求体里把答案取回来。二十行，换来的是全文件唯一一条
// 能区分"结果按下标收集"和"结果按落地顺序收集"的断言。

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// mulBOM 是 UTF-8 的字节序标记，写成 rune 值而不是字符本身：字面的
// U+FEFF 只要不在 Go 源文件的第 0 字节，就是编译错误——parseFrontmatter
// 自己的 cutset 也是绕着这条约束写的。
var mulBOM = string(rune(0xFEFF))

// mulRecorder 把总线送出来的每个事件都收下。
//
// 它自己不需要锁：Bus.Emit 是在 core 的 mutex 底下派发的，OnEvent 因此
// 白得一份串行化。这不是这个测试碰巧如此，而正是下面那个并发测试要
// 钉住的性质——哪天它不成立了，`go test -race` 会先在这里说出来。
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

// mulAgent 造出的 agent 不用联网也能 dispatch 工具调用：调用方挑的
// gate、真的 shell、没有窗口的 compactor（所以永远不会有人去压缩），再加
// 一条挂了 recorder 的总线。
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

// mulShell 返回用来跑命令的 bash，没有就跳过。
func mulShell(t *testing.T) string {
	t.Helper()
	shell, err := findBash()
	if err != nil {
		t.Skipf("no bash on this machine, so dispatch cannot run a real command: %v", err)
	}
	return shell
}

// mulBash 造出格式正确的 bash 工具调用 payload。
func mulBash(command string) string {
	raw, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// mulFakeProvider 不走线上，而是照脚本回答子 Agent 的模型调用。prompt
// 是真的走了一趟往返——BuildRequest 把它写进请求体，假 transport 把请
// 求体原样回吐，ParseStream 再读出来——所以同一个 provider 能给两个并
// 发的子 Agent 两个不同的答案，还能扣住其中一个，等另一个先跑完。
type mulFakeProvider struct {
	mu        sync.Mutex
	completed []string

	reply  func(prompt string) string
	before func(prompt string) // 在这次调用被记为完成之前跑
	after  func(prompt string) // 在之后跑
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

// order 是子 Agent 实际完成的先后顺序。
func (p *mulFakeProvider) order() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.completed...)
}

// mulRoundTrip 是个 http.RoundTripper，把请求体原封不动当 200 返回。
// 不用监听、不占端口，也没有会随机抖动的超时。
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
// 总线，现在它是一棵树
// ---------------------------------------------------------------------------

// 阶段 07 是第一次有两个 goroutine 同时发事件，而整套 trace 设计压在
// 一句话上：Seq 仍然是这棵树上的全序——每个事件恰好编号一次，按编号
// 顺序送达，跨越每个 agent。Seq 要是在锁外面盖的，两个子 Agent 就可能
// 拿到同一个号，或者小号在大号之后才送到——而一份说两件事同时发生的
// trace，没法证明其中哪件引起了另一件。
//
// 用 -race 跑，还能直接抓到那个没做同步的计数器。
func TestBusSeqIsATotalOrderAcrossConcurrentForks(t *testing.T) {
	const (
		emitters = 8
		perAgent = 50
		total    = emitters * perAgent
	)

	rec := &mulRecorder{}
	root := NewBus(rec)

	// 一个 root 加七个子 Agent，全部在开始之前就 fork 好，这样 goroutine
	// 从第一个事件起就在同一个计数器上抢。
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
			start.Wait() // 一次全部放出来，把争抢拉到最大
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

	// 一条断言同时盖住四条性质：送达顺序等于编号顺序，编号从 1 排到 N，
	// 一个不缺，一个不重。
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

// Fork 盖上树坐标，而 root 是深度 0、没有名字。Fork 要是忘了加一，每个
// 子 Agent 的事件都会自称是父 Agent 的，于是子 Agent 的 trace 存在的
// 唯一意义——是哪个 agent 跑了这条命令——就没法回答了。
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

// Emit 上面的注释说，Seq、Depth 和 Agent 是在那里赋值的，"这样没有调用
// 方能伪造它们"。这个测试让那句话真正承重：调用方能设的字段，trace 就
// 无法拿它当证据；而最可能一不小心设上的调用方，就是被重放的 Event 又
// 发了一遍。
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

// 父 Agent 和子 Agent 是同一个 core 上的两个视图，所以从任一边加进来的
// 订阅者都能看见全部。正是因为这样，main() 只要把 trace 写入器挂到 root
// 总线上一次，就能捕到每个子 Agent——也正是因为这样，会话中途挂上的
// 渲染器不会只看见半棵树。
func TestSubscribersAreSharedBetweenAParentBusAndItsChildren(t *testing.T) {
	root := NewBus()
	child := root.Fork("child#1")

	// 从子 Agent 那边加进来的，必须看见父 Agent 的事件。
	viaChild := &mulRecorder{}
	child.Subscribe(viaChild)
	root.Emit(Event{Kind: KindNotice, Text: "from the root"})
	if viaChild.count(KindNotice) != 1 {
		t.Errorf("a subscriber added on the child bus saw %d of the root's events; the two buses do not share a core, "+
			"so the trace file attached to one of them holds half a session", viaChild.count(KindNotice))
	}

	// 从父 Agent 那边加进来的，必须看见子 Agent 的事件。
	viaRoot := &mulRecorder{}
	root.Subscribe(viaRoot)
	child.Emit(Event{Kind: KindNotice, Text: "from the child"})
	if viaRoot.count(KindNotice) != 1 {
		t.Errorf("a subscriber added on the root bus saw %d of the child's events; every subagent's work would be "+
			"missing from the trace the user actually opens", viaRoot.count(KindNotice))
	}
	// 先加进来的那个也看见了，可见订阅没有把列表替换掉。
	if viaChild.count(KindNotice) != 2 {
		t.Errorf("the earlier subscriber saw %d events after a second one was added; Subscribe is overwriting "+
			"rather than appending", viaChild.count(KindNotice))
	}
}

// Fork 共享订阅者列表，不是复制。复制是最容易想到的写法，而一旦有了
// 子 Agent，父 Agent 的每个事件都会被送两遍——trace 里每个事件两行，
// composer 据此算出的每一项总数都翻倍。
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

// parseTaskArgs 就是 task 工具的 parseBashArgs，活儿也一样：把能干净
// unmarshal 出来、却没带任务的 payload 拒掉。指针字段就是全部机制——
// 值类型的 string 会让 json.Unmarshal 在 `{}` 上成功，于是截断的工具调
// 用变成带着空 prompt 启动的子 Agent，那正是阶段 01 的 bug，外加一次
// 网络调用。
//
// 错误文本是要断言的，因为它不是写给我们看的：它作为工具结果返回给
// 模型，而模型只能靠它判断该改发什么。
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
			// 标签缺了只是门面问题——它就是用户在转圈图标旁边看到的
			// 那行字。为它把整次调用判死，等于模型少写了个形容词，就
			// 得把本来完好的任务扔掉。
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
			// prompt 本身原样传过去，不做 trim：子 Agent 的任务就是模型
			// 写的那些字，空白也算。
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
			// docs/wire-notes.md：工具调用被截断时，网关真的会发这个。它
			// 是合法 JSON，里面没有任务。
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

// 给模型看的 schema，必须和解析器实际强制的东西对得上。要是 `prompt`
// 在 schema 里不再是必填，守规矩的模型就会开始不发它，而那些调用每一
// 次都会拿回上面那句错误——一趟往返，烧在我们自己二进制里的分歧上。
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
// agent.tools——深度保险丝
// ---------------------------------------------------------------------------

// 保险丝：到了深度上限，`task` 是从列表里**摘掉**，不是在调用的时候
// 拒掉。拒掉要花一趟往返，还要为永远用不上它的每次请求付一份工具定
// 义的 token；更糟的是，这是条随意的规矩，而模型对付随意的规矩，办法
// 就是换个说法反复试，直到有一次过了。
//
// 所以断言必须落在返回的 slice 上。只查"这次调用被拒了"的测试，恰好
// 能在这个设计所拒绝的那种实现上通过。
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

// 阶段 04：工具定义是缓存 prompt 前缀的一部分，而前缀是逐字节比的。
// tools() 要是第二次调用时把同样的两个工具换个顺序返回，每次请求的缓
// 存都会失效——价格涨十倍，而唯一露头的地方就是账单。
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
// 技能
// ---------------------------------------------------------------------------

// mulSkillDoc 造出带 frontmatter、正文约 n 字节的 SKILL.md。
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

// mulSkillsRoot 把 skills/ 树写进新的 t.TempDir，返回可以交给 loadSkills
// 的 root。值为 "" 时只建目录，里面不放 SKILL.md。
//
// 用 t.TempDir，绝不用仓库自己的 skills/——去读真目录的测试，是过还是
// 挂，取决于上周有人往里加了什么。
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

// 项目里没有 skills/ 是常态，而 loadSkills 每次启动都会被调用，那时什
// 么都还没配上。安静地返回 nil 是唯一能接受的行为；这里一 panic，
// Agent 一启动就死，对每个从没要过技能的用户都一样。
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

// Path 就是模型敲在 `cat` 后面的东西。在 Windows 上 filepath.Join 产出
// 的是反斜杠，而 `cat skills\deploy\SKILL.md` 在 bash 里读到的是转义，
// 不是路径——技能于是打不开，一声不响，而且只在唯一那个用 Mac 测的人
// 永远看不到的平台上。
//
// 这里排序的理由和工具顺序一样：索引就在缓存前缀里，而目录顺序不是
// 任何文件系统给过的承诺。
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

// 没有 description 的技能是隐形的：模型能看到的只有索引，所以索引里
// 没名字的那行，永远不会被选中。留着它，就是为一件用不上的东西永远
// 付前缀 token。
func TestLoadSkillsSkipsASkillWithNoDescription(t *testing.T) {
	root := mulSkillsRoot(t, map[string]string{
		"good":         mulSkillDoc("good", "this one can be chosen", 200),
		"nodesc":       "---\nname: nodesc\n---\n\nA body nobody will ever ask for.\n",
		"nofront":      "Just a Markdown file with no frontmatter at all.\n",
		"emptydesc":    "---\nname: emptydesc\ndescription:   \n---\n\nbody\n",
		"notaskilldir": "", // 里面没有 SKILL.md 的目录
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

// 目录名是兜底，这样写技能的人可以只写两行 frontmatter，不用写三行。
// 换成把技能整个丢掉，就等于拿文件系统本来就答得出的字段去罚它。
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

// 散落在 skills/ 里的文件不是技能——README、.gitkeep、编辑器备份。
// 把其中任何一个当成技能目录，会让 loadSkills 在完全正常的目录树上
// 出错。
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

// parseFrontmatter 是二十行代码，而不是一份 YAML 依赖，所以它到底懂
// 到哪一步，得原原本本写下来。它不懂的东西全部产出 ""，也就是技能从
// 索引里消失——静默失败，唯一的症状是模型从来不用你写的那个技能。
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
			// 作者开了这个块，却从来没关上。去猜它在哪结束，会把整篇
			// 文档塞进 description。
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
			// description 里出现冒号并不稀奇，谁想写"先做 x：再做 y"都得
			// 这么写。见冒号就切，会把句子在有用的那半截断。
			name:     "a colon inside the value keeps the whole tail",
			in:       "---\nname: deploy\ndescription: build the image: then push it and tag the release\n---\n",
			wantName: "deploy",
			wantDesc: "build the image: then push it and tag the release",
		},
		{
			// 用记事本写的技能。BOM 坐在栅栏前面，HasPrefix("---") 不成
			// 立，技能就无声无息地消失了，任何地方都不报错——而且正是
			// 在这个仓库开发所用的平台上。
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

// 零个技能必须产出空串，不是空块。底下什么都没有的 `<skills>` 头，会
// 进到每次请求的缓存前缀里，在每个没有 skills 目录的项目、每次会话的
// 全程都在，而且告诉模型：有份列表你该去查。
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

// 索引就是模型和磁盘上技能之间的全部接口：一条它能 cat 的路径，加一
// 句话说明值不值得看。两样都得原文进去，那三条指令也一样，每一条都
// 是因为某种具体的出错方式才存在。
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

	// 比的是把空白压平之后的文本，所以重新折行是允许的，删掉一条指令
	// 不行。
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

// 渐进披露的全部论据就是一道算术：名称和描述在每次请求里都要花一点，
// 永远花下去；正文花得多，但只在被读的时候才花。skillsCost 要是在现
// 实的目录树上都显不出这道差距，它打印的数字就不能给它本该支撑的设
// 计当证据。
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
// dispatch——顺序保证
// ---------------------------------------------------------------------------

// dispatch 的契约：每次调用一个结果块，按模型发出的顺序排，每个都带
// 着它所回答的那次调用的 id。
//
// 这里每一条承重的地方都在**下一次**请求，不是这一次。少一个结果，
// 就有一个 tool_use_id 没人答，请求会被拒；配错一对，模型就把
// `git log` 的输出当成 `ls` 的答案读下去，一声不响。那是阶段 05 的
// bug，而症状永远指向请求构造器，不指向到底是谁把块弄丢了。
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

	// 比的是内容，不只是 id：按下标整体错位、但 id 映射还对得上的洗牌，
	// 一样是错的，而抓住它的正是这里。
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

// 模型编出来的工具名是能挽回的——前提是结果里说清楚错的是哪个名字。
// 光说一句"没这个工具"，发了三次调用的模型就无从知道该停用哪一次。
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

// 参数解析不出来的 task 调用，必须以工具结果的形式回来，不能变成带着
// 空 prompt 启动的子 Agent，也不能变成被丢掉的块。权限闸也不该问：根
// 本没有什么可批准的。
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

// 被拒的调用照样要有结果块。诱惑在于跳过它——命令都没跑，有什么好报
// 的呢——而后果是下一次请求里有个 tool_use_id 没人答，挂掉的是整段对
// 话，而不是用户拒掉的那一条命令。
func TestDispatchAnswersDeniedCallsToo(t *testing.T) {
	// available:false 让 ask() 不需要终端就给出拒绝。
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

// abort 之后，剩下的调用不再执行——但它们仍然被回答。要紧的是去数块，
// 而不是信那个 flag：flag 只说循环停了，只有块数才说得出这段对话事后
// 还发得出去。
func TestDispatchStillAnswersEveryCallAfterAnAbort(t *testing.T) {
	// 已经处在 EOF 的输入流会让 ask() 返回 abort。
	g := &gate{available: true, in: bufio.NewScanner(strings.NewReader("")), out: io.Discard}
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
	// 只问了一次：abort 必须把后面的都短路掉，而不是四次追问用户是不是
	// 真这个意思。
	if n := rec.count(KindGateVerdict); n != 1 {
		t.Errorf("the gate produced %d verdicts after an abort on the first call, want 1", n)
	}
}

// 真并发下的顺序保证，也正是这个函数存在的全部理由：两个子 Agent 同
// 时跑，第二个先结束，而历史读起来必须仍然是模型问的那个顺序。
//
// 结果要是按落地顺序收集，同一次会话重放两遍会产出两份不同的消息数
// 组、两份不同的 prompt 前缀，以及——照阶段 04 的说法——永远命不中的
// 缓存。并发可以改变事情花多长时间，不可以改变对话说了什么。
func TestDispatchKeepsSubagentResultsInTheModelsOrder(t *testing.T) {
	betaDone := make(chan struct{})
	p := &mulFakeProvider{
		reply: func(prompt string) string { return "report for " + prompt },
		before: func(prompt string) {
			// 扣住 alpha，等 beta 跑完，这样每一次运行的完成顺序都
			// 和调用顺序相反，也不用 sleep 去随机抖动。
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

	// 前置条件。没有它，把两个子 Agent 按调用顺序挨个跑完的 dispatch 也
	// 能让这个测试通过，而那恰恰是唯一不可能把顺序搞错的实现。
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

	// 子 Agent 的事件落在了父 Agent 的流里，带着树坐标。这是端到端地查
	// newChild 是 fork 了总线，而不是新造了一条。
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
// spawn 和 newChild
// ---------------------------------------------------------------------------

// newChild 就是"什么共享、什么不共享"这个决定写成了代码，两半的失败
// 方式各不相同：共享 compactor，子 Agent 的估算器就是拿父 Agent 的流量校
// 准的；**不**共享权限闸，子 Agent 就会跑没人批准过的命令。
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

// 什么都没返回的子 Agent 必须把这件事说出来。父 Agent 没有对话记录可
// 查，也没法追问一句，于是空串会被当成结论读——"我看过了，没有东
// 西"——父 Agent 于是信心十足地往下走，撑着它的是个撞上回合上限的子
// Agent。
//
// subTurns 预算为零会让 runTurn 在调用供应商之前就返回，所以这一条是
// 在离网络八丈远的地方演练那道防线。
func TestSpawnReportsAnEmptyChildAsAFailureRatherThanAnEmptyResult(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, "")
	a.cfg.subTurns = 0 // 子 Agent 在第一次模型调用之前就停住

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

	// 子 Agent 自己的事件就在父 Agent 的流里，往下一层。
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
// lastAssistantText——子 Agent 的返回值
// ---------------------------------------------------------------------------

// 这个函数**就是**子 Agent 的返回值，而每一种错答案都是对父 Agent 说
// 的一句具体的谎：取第一条而不是最后一条消息，是过期的答案；空串是
// "没什么可报的"；只有工具调用的消息，是本该给结论的地方一片沉默。
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
		// 空的那几种情况是特意断言的，不是顺手：spawn 会把 "" 变成
		// 明说的失败字符串，而那条分支能一直走得到，前提是这个函数
		// 真的返回 ""。
		if c.want == "" && got != "" {
			t.Errorf("%s: a conversation with no assistant text returned %q. spawn's guard only fires on the empty "+
				"string, so this value is handed to the parent as the subagent's finding", c.name, got)
		}
	}
}

// 权限闸的提问和 trace 给子 Agent 任务显示的就是 firstLine：第一行，
// 外加一个标记说明后面还有。把标记吞掉，截断的 prompt 就看着像完整
// 的了，而那正是权限提问绝对不能做的一件事。
func TestFirstLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"list every TODO", "list every TODO"},
		{"  list every TODO  ", "list every TODO"},
		{"list every TODO\nand say which file it is in", "list every TODO …"},
		{"  list every TODO  \nmore", "list every TODO …"},
		// 前导空白在切之前就 trim 掉了，所以这就是仅有的一行，不加
		// 省略号。之前的行为返回的是 " …"——省略号前面什么都没有，
		// 而这一行正是要人来授权的那行。
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

// 权限提问必须点名它问的是哪条命令。
//
// 阶段 07 之前不需要：在严格串行的先打印再发问的循环里，"跑吗？"只
// 可能指上面那一行。并发的子 Agent 把这个保证拿走了——命令文本经渲染
// 器、在总线锁下到达终端，问题经权限闸、在它自己的锁下到达，两者之
// 间没有任何顺序。用户看到两条命令，然后看到一句光秃秃的"跑吗？"，
// 是可能批错的。
//
// 这就是防它回归的那道守卫，而且它是安全测试，不是门面测试。
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
			in:        bufio.NewScanner(strings.NewReader("n\n")),
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
		// `a` 设的是**共享**权限闸上的 always，所以提问必须把这点说
		// 出来。
		if !strings.Contains(got, "every agent") {
			t.Errorf("the prompt does not say that `a` applies to every agent, but it does:\n%s\n"+
				"One subagent's \"allow all\" disarms the gate for the parent and every sibling.", got)
		}
	}
}

// yolo 和终端不可用这两种情况，都不能打印出任何提问。
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
