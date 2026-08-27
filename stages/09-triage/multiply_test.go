package main

import (
	"bufio"
	"bytes"
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

// 这个文件测试阶段 07：变成一棵树之后的总线、任务工具的参数解析器、
// agent.tools 中的深度保险丝、skills.go，以及 dispatch 的承诺——并发
// 执行仍然会产生一段确定性的历史。
//
// 这里没有任何代码会通过网络调用供应商。唯一需要用到 Provider 的
// 地方——也就是 dispatch 里的并发路径——用的是下面的 mulFakeProvider，
// 它借着一个假的 RoundTripper，直接从请求体里回答。这就是 20 行代码，
// 换来了这个文件里唯一能区分"结果是按索引收集的"还是"结果是落地
// 先后收集的"的那条断言。

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// mulBOM 是一个 UTF-8 字节顺序标记，这里特意拼成 rune 值，而不是直接
// 写字符本身：在一份 Go 源文件里，字面量 U+FEFF 只要不是出现在第
// 0 字节，就是一个编译错误——parseFrontmatter 自己的 cutset，也正是
// 绕着这同一条限制写的。
var mulBOM = string(rune(0xFEFF))

// mulRecorder 收集总线送出的每一个事件。
//
// 它不需要自己的锁：Bus.Emit 是在核心 mutex 下分发的，所以 OnEvent
// 不用自己费劲，就已经是串行的了。这不是这个测试碰巧如此，而是下面
// 那个并发测试存在的意义，就是为了把这一点钉死——要是这一点哪天不再
// 成立，`go test -race` 会第一个在这里报出来。
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

// mulAgent 建立一个不用联网也能分发工具调用的 Agent：调用者选定的
// 门、一个真正的 shell、一个没有窗口的压缩器（所以什么都不会触发
// 压缩），以及一个挂了记录器的总线。
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

// mulShell 返回一个 bash 来运行命令，或跳过。
func mulShell(t *testing.T) string {
	t.Helper()
	shell, err := findBash()
	if err != nil {
		t.Skipf("no bash on this machine, so dispatch cannot run a real command: %v", err)
	}
	return shell
}

// mulBash 建立一个格式良好的 bash 工具调用有效载荷。
func mulBash(command string) string {
	raw, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// mulFakeProvider 从脚本而不是通过线上回答子 Agent 的模型调用。
// prompt 做往返——BuildRequest 把它写进 body，假传输回显 body，
// ParseStream 读它——所以一个供应商可以给两个并发的子 Agent 两个不同
// 的答案，也可以握住其中之一，直到另一个完成。
type mulFakeProvider struct {
	mu        sync.Mutex
	completed []string

	reply  func(prompt string) string
	before func(prompt string) // 在这个调用被记录为完成之前运行
	after  func(prompt string) // 随后运行
}

var _ Provider = (*mulFakeProvider)(nil)

func (p *mulFakeProvider) Protocol() string { return "fake" }
func (p *mulFakeProvider) Model() string    { return "fake-model" }

func (p *mulFakeProvider) BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	body, err := json.Marshal(struct {
		Prompt string `json:"prompt"`
	}{msgs[len(msgs)-1].Text()})
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://subagent.invalid/v1/messages", bytes.NewReader(body))
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

// order 是子 Agent 实际完成的顺序。
func (p *mulFakeProvider) order() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.completed...)
}

// mulRoundTrip 是一个 http.RoundTripper，它把请求 body 直接原样交回，
// 当作一个 200 返回。没有监听器、没有端口，也没有超时会让测试变得
// 时好时坏。
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

// 阶段 07 是两个 goroutine 第一次同时发出事件，而整个 trace 设计所
// 依赖的论断，是 Seq 仍然是树上的全序：每个事件恰好编号一次，按编号
// 顺序送达，跨越每一个 Agent。如果 Seq 是在锁外盖上的，两个子 Agent
// 可能会拿到同一个数字，或者一个更小的数字会排在一个更大的数字后面
// 才送到——一个说两件事同时发生的 trace，没法证明是哪一个导致了另
// 一个。
//
// 在 -race 下运行，也能直接捕获这个不同步的计数器。
func TestBusSeqIsATotalOrderAcrossConcurrentForks(t *testing.T) {
	const (
		emitters = 8
		perAgent = 50
		total    = emitters * perAgent
	)

	rec := &mulRecorder{}
	root := NewBus(rec)

	// 一个根加七个子 Agent，全部在任何东西开始之前就分叉完毕，所以从
	// 第一个事件开始，goroutine 们就在争抢同一个计数器。
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
			start.Wait() // 一次释放全部，最大化竞争
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

	// 一个断言一次覆盖全部四个属性：传递顺序等于编号顺序，数字运行 1..N，
	// 没有缺失，没有重复。
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

// Fork 会盖上树坐标，根节点的深度是 0，没有名字。如果 Fork 忘记递增，
// 每个子 Agent 的事件就会都自称是父 Agent 的事件，而子 Agent trace
// 存在的意义——回答"是哪个 Agent 运行了这条命令"——也就没法回答了。
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

// Emit 上的注释说，Seq、Depth 和 Agent 都是在这里赋值的，"这样任何
// 调用者都伪造不了它们"。这就让那句话成了一句承重的断言：一个调用者
// 自己就能设置的字段，正是 trace 没法拿来当证据的字段；而最可能在
// 不经意间设置到这种字段的调用者，就是把一个重放出来的 Event 又重新
// 发出去的那种情况。
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

// 父 Agent 和子 Agent，是对着同一个核心的两个视图，所以不管从哪一个
// 挂上去的订阅者，都能看到全部事件。这就是为什么 main() 只需要把
// trace writer 挂到根总线上一次，就依然能捕获每一个子 Agent——也是
// 为什么一个在会话中途才挂上去的渲染器，不会只看到半棵树。
func TestSubscribersAreSharedBetweenAParentBusAndItsChildren(t *testing.T) {
	root := NewBus()
	child := root.Fork("child#1")

	// 通过子 Agent 添加的，必须看到父 Agent 的事件。
	viaChild := &mulRecorder{}
	child.Subscribe(viaChild)
	root.Emit(Event{Kind: KindNotice, Text: "from the root"})
	if viaChild.count(KindNotice) != 1 {
		t.Errorf("a subscriber added on the child bus saw %d of the root's events; the two buses do not share a core, "+
			"so the trace file attached to one of them holds half a session", viaChild.count(KindNotice))
	}

	// 通过父 Agent 添加的，必须看到子 Agent 的事件。
	viaRoot := &mulRecorder{}
	root.Subscribe(viaRoot)
	child.Emit(Event{Kind: KindNotice, Text: "from the child"})
	if viaRoot.count(KindNotice) != 1 {
		t.Errorf("a subscriber added on the root bus saw %d of the child's events; every subagent's work would be "+
			"missing from the trace the user actually opens", viaRoot.count(KindNotice))
	}
	// 而早前添加的那个，也照样看到了它，说明订阅并没有把列表替换掉。
	if viaChild.count(KindNotice) != 2 {
		t.Errorf("the earlier subscriber saw %d events after a second one was added; Subscribe is overwriting "+
			"rather than appending", viaChild.count(KindNotice))
	}
}

// Fork 共享的是订阅者列表本身，而不是复制一份。复制是最直接会想到的
// 实现方式，但只要一有子 Agent 存在，它就会把每一个父 Agent 事件都
// 发送两次——trace 里每个事件变成两行，composer 从中算出的每一项
// 合计里，token 计数也都跟着翻了倍。
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

// parseTaskArgs 是任务工具的 parseBashArgs，它有相同工作：拒绝一个
// 整齐解组但不包含任务的有效载荷。指针字段是整个机制——值类型字符串
// 使 json.Unmarshal 在 `{}` 上成功，并把截断的工具调用变成用空
// prompt 启动的子 Agent，这是阶段 01 的 bug 加网络调用。
//
// 错误文本被断言，因为它不是给我们：它作为工具结果返回给模型，它是
// 模型关于改为发送什么的唯一线索。
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
			// 缺失标签只是个外观问题——它是用户在加载动画旁看到的东西。只因为
			// 模型漏写了一个形容词就让调用失败，会白白扔掉一个好端端的任务。
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
			// prompt 本身会原样传下去，不做任何修剪：子 Agent 的任务就是模型写下
			// 的任何内容，空白字符也算在内。
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
			// docs/wire-notes.md：网关在工具调用被截断时，真的会发出这样的东西。
			// 它是合法的 JSON，只是不包含任务。
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

// 展示给模型看的 schema，必须和解析器实际执行的规则一致。如果 schema
// 里不再要求 `prompt`，一个循规蹈矩的模型就会开始省略它，那些调用就会
// 一个接一个地变成上面那种错误——白白烧掉一次往返，只因为我们自己的
// 二进制内部出现了分歧。
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
// agent.tools — 深度保险丝
// ---------------------------------------------------------------------------

// 保险丝：到了深度限制，`task` 会被**移除**出工具列表，而不是在调用时
// 才被拒绝。一次拒绝要付出的代价，是一次往返，外加那些永远用不上这个
// 工具的请求里，白白耗费掉的工具定义 token；更糟的是，这是一条武断的
// 规则，而模型对付武断规则的办法，就是不断换着说法，直到蒙混过关为止。
//
// 所以断言必须落在返回的切片上。一个只检查"调用被拒绝"的测试，就算
// 换成这个设计原本要否决的那种实现，也一样能通过。
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

// 阶段 04：工具定义是缓存 prompt 前缀的一部分，前缀会被逐字节比较。
// 如果 tools() 在第二次调用时返回了同样两个工具，却换了个顺序，就会
// 让每一个请求的缓存都失效——一次十倍的价格上涨，除了账单上的数字，
// 什么迹象都不会显现。
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

// mulSkillDoc 建立一个带前言、正文大约 n 字节的 SKILL.md。
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

// mulSkillsRoot 把一个 skills/ 树写进一个全新的 t.TempDir，返回这个
// 根目录，用来传给 loadSkills。"" 的值会创建一个没有 SKILL.md 的目录。
//
// 用 t.TempDir，绝不用 repo 自己的 skills/——一个去读真实目录的测试，
// 能不能通过，就要看上周有人往里面加了什么。
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

// 没有 skills/ 的项目是常规情况，loadSkills 在每次启动、在任何东西被
// 配置之前，都会被调用。安静地返回 nil 是唯一可接受的行为；在这里
// panic，会在启动时就杀死 Agent，殃及每一个根本没要求过 skills 功能
// 的用户。
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

// 路径是模型在 `cat` 后面输入的东西。在 Windows 上，filepath.Join
// 产生的是反斜杠，`cat skills\deploy\SKILL.md` 在 bash 里读到的是
// 转义，不是路径——技能会无声无息地打不开，而且恰恰是在那个所有用
// Mac 做测试的人永远看不到的平台上。
//
// 这里要排序，原因和工具顺序必须固定是同一个：索引坐在缓存前缀里，
// 而目录顺序不是任何文件系统会做出的承诺。
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

// 没有描述的技能是隐形的：索引是模型唯一能看到的东西，所以其中缺了
// 描述的那一行，就永远不会被选中。留着它，就等于要为一个永远用不上
// 的东西，一直付出前缀 token 的代价。
func TestLoadSkillsSkipsASkillWithNoDescription(t *testing.T) {
	root := mulSkillsRoot(t, map[string]string{
		"good":         mulSkillDoc("good", "this one can be chosen", 200),
		"nodesc":       "---\nname: nodesc\n---\n\nA body nobody will ever ask for.\n",
		"nofront":      "Just a Markdown file with no frontmatter at all.\n",
		"emptydesc":    "---\nname: emptydesc\ndescription:   \n---\n\nbody\n",
		"notaskilldir": "", // 一个没有 SKILL.md 的目录
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

// 目录名充当后备，所以技能作者可以只写两行前言，而不是三行。换成
// 直接丢弃这个技能，等于是在惩罚一个文件系统早就替你回答过的字段
// 缺失。
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

// 散落在 skills/ 里的文件不算技能——比如 README、.gitkeep、编辑器
// 备份文件。要是把其中一个当成技能目录来处理，就会让 loadSkills 在
// 一棵再普通不过的目录树上失败。
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

// parseFrontmatter 用 20 行代码代替了一个 YAML 依赖，所以它到底能
// 理解到什么程度，边界必须写清楚。所有它不理解的内容都会产生 ""，
// 这意味着技能会从索引中消失——一次无声的失败，唯一的症状就是模型
// 永远不会用到你写的这个技能。
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
			// 作者打开了这个块，却一直没有关闭它。去猜它在哪里结束，只会把整份
			// 文档都塞进描述里。
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
			// 描述里出现冒号不是什么稀罕事；就跟谁写"做 x：然后 y"时会用的冒号
			// 一样平常。见冒号就切，会把句子恰好从最有用的那一半处截断。
			name:     "a colon inside the value keeps the whole tail",
			in:       "---\nname: deploy\ndescription: build the image: then push it and tag the release\n---\n",
			wantName: "deploy",
			wantDesc: "build the image: then push it and tag the release",
		},
		{
			// 一个用记事本写成的技能。BOM 坐在栅栏前面，HasPrefix("---") 失败，
			// 技能就这样消失了，哪里都不会报错——而且还是在这个 repo 自己开发
			// 所用的那个平台上。
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

// 零技能必须产生空字符串，而不是一个空块。一个下面什么都没有的
// `<skills>` 头，会被塞进每一个请求的缓存前缀里——在每一个没有
// skills 目录的项目中，整个会话期间都是如此——并且等于在告诉模型：
// 这里有一份你应该去查阅的列表。
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

// 索引是模型和磁盘上技能之间的整个接口：一条它可以拿去 cat 的路径，
// 一句话说清楚值不值得为它费心。这两样必须逐字都在，那三条指令也
// 一样必须都在——每一条的存在，都是因为某种具体的出错方式。
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

	// 比对时用的是空白折叠后的文本，所以重新折行 prompt 是允许的，但
	// 删掉一条指令不行。
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

// 渐进披露的整个论证，说到底是一笔算术账：名称和描述这两项，每次
// 请求都要花一点点，而且永远都要花；正文的开销大得多，但只有在被
// 读取时才要花。如果 skillsCost 没法在一棵贴近现实的树上，把这个
// 差距显示出来，那么它打印出的数字，就没法证明它存在的意义——也就
// 是证明这个设计本身是合理的。
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
// dispatch — 排序保证
// ---------------------------------------------------------------------------

// dispatch 的契约是：每次调用对应一个结果块，按模型发出调用的顺序
// 排列，每个块都带着它所回答的那个调用 id。
//
// 这里的每一处，分量都压在**下一个**请求上，而不是这一个。少一个
// 结果，就是一个没有答复的 tool_use_id，请求会被拒绝；一对错了序的
// 结果，就意味着模型会悄悄把 `git log` 的输出当成 `ls` 的答案来读。
// 那是阶段 05 的 bug，而症状指向的，永远是请求构建的地方，而不是
// 无论是什么弄丢了那个块。
func TestDispatchAnswersEveryCallOnceInOrderAndByID(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	calls := []Block{
		{Kind: BlockToolCall, ID: "call_1", Name: "bash", Args: mulBash("echo mul-alpha")},
		{Kind: BlockToolCall, ID: "call_2", Name: "frobnicate", Args: `{"target":"x"}`},
		{Kind: BlockToolCall, ID: "call_3", Name: "bash", Args: mulBash("echo mul-beta")},
		{Kind: BlockToolCall, ID: "call_4", Name: "search_files", Args: `{}`},
		{Kind: BlockToolCall, ID: "call_5", Name: "bash", Args: mulBash("echo mul-gamma")},
	}

	results, stopped := a.dispatch(1, calls)
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

	// 内容也要对，不能只对 id：一次按索引打乱、却保留了 id 映射的洗牌，
	// 仍然是错的，而这正是这里要抓住的东西。
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

// 模型自己编造的工具名是可以纠正的——但前提是结果得说清楚，错的是
// 哪个名字。光是一句 "Unknown tool"，会让那个发出三次调用的模型没法
// 知道该停用哪一个。
func TestDispatchNamesTheToolItDoesNotHave(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")

	results, stopped := a.dispatch(1, []Block{
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

// 一个参数解析不了的任务调用，必须以工具结果的形式返回——而不是被
// 启动成一个 prompt 为空的子 Agent，也不是被悄悄丢掉的块。这道门也
// 不该被问起：根本没有什么好批准的。
func TestDispatchTurnsMalformedTaskArgsIntoAResultInsteadOfSpawning(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, "")

	results, stopped := a.dispatch(1, []Block{
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

// 一个被拒绝的调用，仍然会得到一个结果块。诱惑是干脆跳过它——命令
// 根本没运行，还有什么好报告的呢——但后果是下一个请求里出现一个
// 没人答复的 tool_use_id，这会搞砸整个对话，而不只是用户拒绝的那
// 一条命令。
func TestDispatchAnswersDeniedCallsToo(t *testing.T) {
	// available:false 让 ask() 不需要终端就能直接拒绝。
	a, _ := mulAgent(&gate{available: false}, "")

	calls := []Block{
		{Kind: BlockToolCall, ID: "call_1", Name: "bash", Args: mulBash("rm -rf /")},
		{Kind: BlockToolCall, ID: "call_2", Name: "bash", Args: mulBash("echo two")},
		{Kind: BlockToolCall, ID: "call_3", Name: "task", Args: `{"description":"survey","prompt":"look around"}`},
	}
	results, stopped := a.dispatch(1, calls)

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

// 中止之后，剩下的调用不会再执行——但仍然会得到回答。重点在于清点
// 这些块，而不是相信那个标志位：标志位只能说明循环已经停止，唯有
// 清点数量才能说明，对话在那之后是否还能继续发送。
func TestDispatchStillAnswersEveryCallAfterAnAbort(t *testing.T) {
	// 一个已经 EOF 的输入流使 ask() 返回中止。
	g := &gate{available: true, in: bufio.NewScanner(strings.NewReader("")), out: io.Discard}
	a, rec := mulAgent(g, "")

	calls := []Block{
		{Kind: BlockToolCall, ID: "call_1", Name: "bash", Args: mulBash("echo one")},
		{Kind: BlockToolCall, ID: "call_2", Name: "bash", Args: mulBash("echo two")},
		{Kind: BlockToolCall, ID: "call_3", Name: "task", Args: `{"description":"survey","prompt":"look around"}`},
		{Kind: BlockToolCall, ID: "call_4", Name: "made_up", Args: `{}`},
	}
	results, stopped := a.dispatch(1, calls)

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
	// 恰好只问了一个问题：中止必须让剩下的问题都短路掉，而不是把用户
	// 问上四遍，来确认他们是不是真是这个意思。
	if n := rec.count(KindGateVerdict); n != 1 {
		t.Errorf("the gate produced %d verdicts after an abort on the first call, want 1", n)
	}
}

// 真并发下的排序保证，是这个函数存在的全部理由：两个子 Agent 同时
// 运行，第二个先完成，历史却必须仍然按模型提问的顺序来读。
//
// 如果结果是按落地的先后顺序收集的，同一个会话重放两次就会产生两个
// 不同的 message 数组、两个不同的 prompt 前缀，以及——按阶段 04 的
// 说法——一个永远不会命中的缓存。并发可以改变事情要花多久，但不能
// 改变对话里说了什么。
func TestDispatchKeepsSubagentResultsInTheModelsOrder(t *testing.T) {
	betaDone := make(chan struct{})
	p := &mulFakeProvider{
		reply: func(prompt string) string { return "report for " + prompt },
		before: func(prompt string) {
			// 按住 alpha，直到 beta 完成，这样一来，每次运行完成的顺序都会和
			// 调用顺序相反，也没有 sleep 会让测试变得时好时坏。
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
	results, stopped := a.dispatch(1, calls)
	if stopped {
		t.Fatal("dispatch reported the session stopped with a --yolo gate")
	}

	// 前提条件。如果没有这一条，就算换成一个按调用顺序把两个子 Agent
	// 一前一后串行跑完的 dispatch，这个测试也照样能通过——而这偏偏是
	// 唯一一种不可能把顺序搞错的实现。
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

	// 子 Agent 的事件，落在了父 Agent 的流里，并且盖上了树坐标。这是
	// 一个端到端检查，用来确认 newChild 分叉的是总线本身，而不是另起
	// 一个新的。
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

// newChild 把"什么被共享、什么不被共享"这个决定写成了代码，两半
// 各有各的出错方式：共享压缩器，能让子 Agent 的估算器用父 Agent 的
// 流量来校准；**不**共享门，则意味着子 Agent 会跑起没人批准过的
// 命令。
func TestNewChildSharesTheGateAndForksEverythingElse(t *testing.T) {
	parent, _ := mulAgent(&gate{yolo: true}, "")
	parent.cfg.maxTurns = 40
	parent.cfg.subTurns = 6
	parent.comp = newCompactor(200_000, 0.8, 0.3)
	parent.comp.est.observe(4000, 1000) // 比例 4.0

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

// 什么都不返回的子 Agent，必须明说这一点。父 Agent 没有笔录可查，
// 也没办法追问，于是空字符串会被当成一个结论来读——"我看过了，
// 什么都没有"——父 Agent 就会带着这份信心继续往下走，而这份信心
// 的全部根基，不过是一个撞上了回合上限的子 Agent。
//
// 把 subTurns 预算设成零，会让 runTurn 在它还没调用供应商之前就
// 返回，所以这就测到了这道防线，而且全程没有一丁点网络牵扯在里头。
func TestSpawnReportsAnEmptyChildAsAFailureRatherThanAnEmptyResult(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, "")
	a.cfg.subTurns = 0 // 子 Agent 在它第一次模型调用之前就停止

	report, _, err := a.spawn("call_1", "probe", "look at everything")
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

	// 子 Agent 自己的事件，就在父 Agent 的流里，只是深了一层。
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

// lastAssistantText — 子 Agent 的返回值

// 这个函数**是**子 Agent 的返回值，每一个错误的答案，都是对父 Agent
// 撒的一个具体的谎：取第一条 message 而不是最后一条，得到的是一个
// 过时的答案；空字符串意味着"无事可报"；一条只有工具调用的 message，
// 则是本该有结论的地方，却只剩一片沉默。
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
		// 对空情况的断言是故意为之，不是顺带的：spawn 把 "" 转成一个明确的
		// 失败字符串，而那个分支只有在这个函数真的会返回 "" 时，才够得到、
		// 走得进去。
		if c.want == "" && got != "" {
			t.Errorf("%s: a conversation with no assistant text returned %q. spawn's guard only fires on the empty "+
				"string, so this value is handed to the parent as the subagent's finding", c.name, got)
		}
	}
}

// firstLine 是门 prompt 和 trace 为子 Agent 的任务所显示的内容：第
// 一行，外加一个说明还有更多的标记。吞掉这个标记，会让一个被截断的
// prompt 看起来完整，而这恰恰是权限 prompt 绝不能做的一件事。
func TestFirstLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"list every TODO", "list every TODO"},
		{"  list every TODO  ", "list every TODO"},
		{"list every TODO\nand say which file it is in", "list every TODO …"},
		{"  list every TODO  \nmore", "list every TODO …"},
		// 裁切之前，会先修剪掉前导空格，所以这就是唯一存在的一行，它也就
		// 得不到省略号。旧的行为会返回 " …"——省略号前面什么都没有，而
		// 这一行，恰恰是人类被问要不要批准的那一行。
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

// 权限 prompt 必须点明自己问的是哪条命令。
//
// 直到阶段 07 之前都不需要这样：在严格顺序的 print-then-ask 循环下，
// "run?" 只能指它上面的那一行。并发子 Agent 移除了那个保证——命令
// 文本经由渲染器、在总线锁下到达终端，问题则经由门、在它自己的锁下
// 到达终端，两者之间没有任何排序。一个眼前先后出现了两条命令、随后
// 只看到一句光秃秃的 "run?" 的用户，可能会批准错一条。
//
// 这就是针对这一点的回归防线，它是一项安全测试，不是什么门面测试。
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
		// `a` 会在**共享**门上设置 `always`，所以 prompt 必须把这一点说清楚。
		if !strings.Contains(got, "every agent") {
			t.Errorf("the prompt does not say that `a` applies to every agent, but it does:\n%s\n"+
				"One subagent's \"allow all\" disarms the gate for the parent and every sibling.", got)
		}
	}
}

// yolo 和无可用终端必须根本不打印问题。
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
