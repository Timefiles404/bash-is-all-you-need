package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 阶段 11 那道边界的测试在 toolcall_test.go 里。这个文件测的是它**有没有被
// 调用**——在对的地方、对的时机、对着对的值。
//
// 这个文件是变异测试逼出来的。toolcall.go 里每个检查都被覆盖到了，可是把
// dispatch 里那句调用删掉的变异体活了下来，因为下游另一道兜底恰好挡住了
// 同一份载荷。单测证明的是函数没问题；只有跑整个主循环的测试，才能证明
// 主循环真的用了它。

// ---------------------------------------------------------------------------
// 照着剧本走的供应商
// ---------------------------------------------------------------------------

// scriptProvider 按顺序返回事先搭好的 CallResult，每次模型调用给一个，并记
// 下自己伺候了多少次。它完全不看对话内容：这些测试关心的是主循环拿到回
// 复之后做什么，不是什么东西引出了这个回复。
type scriptProvider struct {
	mu     sync.Mutex
	script []*CallResult
	served int
}

var _ Provider = (*scriptProvider)(nil)

func (p *scriptProvider) Protocol() string { return "script" }
func (p *scriptProvider) Model() string    { return "script-model" }

func (p *scriptProvider) BuildRequest(ctx context.Context, system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	body := []byte(`{}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://script.invalid/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	return req, body, nil
}

func (p *scriptProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	io.Copy(io.Discard, r)
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.served
	p.served++
	if i >= len(p.script) {
		// 跑过头，说明主循环发起的调用比测试预期的多，这本身就是失败。
		// 这里选择结束回合而不是 panic，好让断言把次数报出来。
		return &CallResult{Text: "script exhausted", Stop: StopEndTurn, RawStop: "end_turn"}, nil
	}
	return p.script[i], nil
}

func (p *scriptProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.served
}

// scriptAgent 把 Agent 接到剧本上。shell 是真的，因为这里有些测试需要命令
// 真的跑起来。
func scriptAgent(t *testing.T, script ...*CallResult) (*agent, *mulRecorder, *scriptProvider) {
	t.Helper()
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))
	p := &scriptProvider{script: script}
	a.lad = newLadder(rung{p: p})
	a.httpc = &http.Client{Transport: mulRoundTrip{}}
	return a, rec, p
}

func toolCall(id, name, args string) Block {
	return Block{Kind: BlockToolCall, ID: id, Name: name, Args: args}
}

func callResult(stop StopReason, raw, text string, calls ...Block) *CallResult {
	return &CallResult{Text: text, Stop: stop, RawStop: raw, Calls: calls,
		Usage: Usage{Input: 100, Output: 10}}
}

// ---------------------------------------------------------------------------
// 身份，从主循环内部看
// ---------------------------------------------------------------------------

// 真正要命的重复出现在**不同的** assistant 消息里，所以只能靠跑不止一个回
// 合来测。网关给它发出的每次调用都用同一个 id，就是这种情况；协议会为此
// 把整个请求拒掉，点名的是消息下标，不是工具。
func TestRunTurnRenamesDuplicateIDsAcrossTurns(t *testing.T) {
	a, _, _ := scriptAgent(t,
		callResult(StopToolUse, "tool_use", "", toolCall("call_go_0", "bash", mulBash("echo s11-first"))),
		callResult(StopToolUse, "tool_use", "", toolCall("call_go_0", "bash", mulBash("echo s11-second"))),
		callResult(StopEndTurn, "end_turn", "done"),
	)

	msgs := a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	var ids []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolCall {
				ids = append(ids, b.ID)
			}
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 tool calls in the history, got %d (%v)", len(ids), ids)
	}
	if ids[0] == ids[1] {
		t.Fatalf("both tool calls carry the id %q; the next request is rejected for a duplicate tool_use id, "+
			"and the rejection names a message index rather than the tool", ids[0])
	}

	// 结果必须跟着它应答的那次调用一起挪。调用在应答已经存在之后才改名，
	// 留下的是孤立工具结果：同样是被拒的请求，报错还更没用。
	answered := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolResult {
				answered[b.ID] = true
			}
		}
	}
	for _, id := range ids {
		if !answered[id] {
			t.Errorf("tool call %q has no result addressed to it; every call in an assistant message must be "+
				"answered or the request is rejected", id)
		}
	}
}

// ---------------------------------------------------------------------------
// 宿主标记泄漏，从主循环内部看
// ---------------------------------------------------------------------------

// §A2 的形状：被截断的回合，正文是网关自己内部的工具调用语法。留着它要
// 付两遍代价——人看到的是网关的内部东西，却像是 assistant 说出来的；而历
// 史还在教模型：在这儿把这套语法当散文吐出来是正常的。
func TestRunTurnKeepsLeakedMarkupOutOfTheHistory(t *testing.T) {
	leak := "Looking at that now.\n\n<tool_call>\n<function=bash>\n<parameter=command>find /srv"
	a, rec, _ := scriptAgent(t, callResult(StopMaxTokens, "length", leak))

	msgs := a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind != BlockText {
				continue
			}
			for _, marker := range harnessMarkers {
				if strings.Contains(b.Text, marker) {
					t.Errorf("the history contains the gateway's %q markup:\n  %q", marker, b.Text)
				}
			}
		}
	}

	// 模型在宿主标记开始之前确实说了的那些话，是模型在跟用户讲话，要留下来。
	var kept string
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			kept = m.Text()
		}
	}
	if !strings.Contains(kept, "Looking at that now.") {
		t.Errorf("the real text before the markup was discarded too: %q", kept)
	}

	var reported bool
	for _, e := range rec.events {
		if e.Kind == KindToolCallInvalid && strings.Contains(e.Text, "markup") {
			reported = true
		}
	}
	if !reported {
		t.Error("the leak was stripped without being recorded; a repair nobody can see is a repair nobody " +
			"can measure the rate of")
	}
}

// 闸口是 StopMaxTokens，这道闸的代价是：正常文本里提到这套标记，会被原
// 样放过。这是故意的——这个仓库自己的文档里就引了 `<tool_call>`——所以它
// 配一个测试，不只是一句注释。
func TestRunTurnLeavesMarkupAloneOnACompleteTurn(t *testing.T) {
	quoted := "The gateway emits <tool_call>\n<function=bash> when it truncates."
	a, _, _ := scriptAgent(t, callResult(StopEndTurn, "end_turn", quoted))

	msgs := a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "explain")})

	var kept string
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			kept = m.Text()
		}
	}
	if kept != quoted {
		t.Errorf("a complete turn that quoted the markup was truncated:\n  got  %q\n  want %q", kept, quoted)
	}
}

// ---------------------------------------------------------------------------
// 截断保险丝
// ---------------------------------------------------------------------------

// 拒得对还不够。对着真端点在 --max-tokens 110 下量过：十六次模型调用，零
// 条命令，每次调用都被截断。模型看不到 max_tokens，所以"你被截断了"点出
// 的原因它根本无从下手，于是它永远在重写同样长度的命令。
func TestCutStreakEndsTheLoop(t *testing.T) {
	cut := mulBash("echo hi")[:12] // 写到值中间被截断
	var script []*CallResult
	for i := 0; i < 8; i++ {
		script = append(script, callResult(StopToolUse, "tool_use", "",
			toolCall("call_"+string(rune('a'+i)), "bash", cut)))
	}
	a, rec, p := scriptAgent(t, script...)

	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if p.calls() != maxCutStreak {
		t.Errorf("the loop made %d model calls before stopping, want %d. Without the fuse it runs to the turn "+
			"budget (%d), which is what the live session did", p.calls(), maxCutStreak, a.cfg.maxTurns)
	}
	var errored bool
	for _, e := range rec.events {
		if e.Kind == KindError && strings.Contains(e.Text, "truncated") {
			errored = true
			if !strings.Contains(e.Text, "max-tokens") {
				t.Errorf("the error does not name the knob that fixes it: %q", e.Text)
			}
		}
	}
	if !errored {
		t.Error("the loop stopped without telling the human why; the model cannot fix this and the human can")
	}
}

// 只要有回合放过去了东西，这串连续截断就归零，否则偶尔被截断一次的会话
// 迟早会莫名其妙地死掉。
func TestCutStreakResetsOnAProductiveTurn(t *testing.T) {
	cut := mulBash("echo hi")[:12]
	good := mulBash("echo s11-productive")

	// 截断、截断、**成功**、截断、截断，然后结束。五次调用，从没连着三次截断。
	a, rec, p := scriptAgent(t,
		callResult(StopToolUse, "tool_use", "", toolCall("c1", "bash", cut)),
		callResult(StopToolUse, "tool_use", "", toolCall("c2", "bash", cut)),
		callResult(StopToolUse, "tool_use", "", toolCall("c3", "bash", good)),
		callResult(StopToolUse, "tool_use", "", toolCall("c4", "bash", cut)),
		callResult(StopToolUse, "tool_use", "", toolCall("c5", "bash", cut)),
		callResult(StopEndTurn, "end_turn", "done"),
	)

	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if p.calls() != 6 {
		t.Errorf("the loop made %d model calls, want 6; the fuse fired on a session that was making progress",
			p.calls())
	}
	for _, e := range rec.events {
		if e.Kind == KindError && strings.Contains(e.Text, "truncated") {
			t.Fatalf("the fuse fired despite a productive turn in between: %q", e.Text)
		}
	}
}

// 几次好调用里夹着一次坏的，不是这根保险丝要管的形态：那个回合模型是干
// 成了活的。只有**每一次**调用都被截断的回合才算数。
func TestCutStreakIgnoresATurnThatGotSomethingThrough(t *testing.T) {
	cut := mulBash("echo hi")[:12]
	good := mulBash("echo s11-mixed")

	mixed := func(n string) *CallResult {
		return callResult(StopToolUse, "tool_use", "",
			toolCall(n+"a", "bash", cut), toolCall(n+"b", "bash", good))
	}
	a, rec, p := scriptAgent(t, mixed("t1"), mixed("t2"), mixed("t3"), mixed("t4"),
		callResult(StopEndTurn, "end_turn", "done"))

	a.runTurn(context.Background(), []Msg{TextMsg(RoleUser, "go")})

	if p.calls() != 5 {
		t.Errorf("the loop made %d model calls, want 5; a turn that ran a command counted as a cut turn",
			p.calls())
	}
	for _, e := range rec.events {
		if e.Kind == KindError && strings.Contains(e.Text, "truncated") {
			t.Fatalf("the fuse fired on turns that each ran a command: %q", e.Text)
		}
	}
}

// ---------------------------------------------------------------------------
// dispatch 负责分类，进 trace 的就是这个分类
// ---------------------------------------------------------------------------

// 分类决定了告诉谁、怎么告诉，所以记错分类的拒绝，后面就会被错误地处
// 理——而且只要断言写的只有"它被拒了"，这件事根本看不见。
func TestDispatchRecordsTheRightFaultClassPerCall(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	want := []struct {
		id, args string
		fault    argFault
	}{
		{"c1", `{"raw_arguments":"{\"command\": \"find"}`, faultCut},
		{"c2", `{"command":"go test ./sta`, faultCut},
		{"c3", `I will list the files`, faultNotJSON},
		{"c4", `{}`, faultSchema},
		{"c5", `{"command":42}`, faultSchema},
	}
	var calls []Block
	for _, w := range want {
		calls = append(calls, toolCall(w.id, "bash", w.args))
	}
	a.dispatch(context.Background(), 1, calls)

	got := map[string]string{}
	for _, e := range rec.events {
		if e.Kind == KindToolCallInvalid {
			got[e.ToolID] = e.Fault
		}
	}
	for _, w := range want {
		if got[w.id] != string(w.fault) {
			t.Errorf("%s (%s): recorded fault %q, want %q", w.id, w.args, got[w.id], w.fault)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d refusals recorded for %d invalid calls", len(got), len(want))
	}
}

// ---------------------------------------------------------------------------
// OpenAI 请求这一侧
// ---------------------------------------------------------------------------

// §E14：在这条路上 `arguments: ""` 是 HTTP 400，而 400 是致命的，所以历史
// 里有一次零参数的工具调用，会话就结束了。renderArgs 的单测证明函数
// 没问题；这个测试证明 BuildRequest 真的调了它。
func TestOpenAIRequestNeverSendsEmptyArguments(t *testing.T) {
	p := newOpenAIProvider("http://x.invalid/v1", "k", "m")
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{toolCall("call_1", "clock", "")}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("call_1", "12:00")}},
	}
	_, body, err := p.BuildRequest(context.Background(), "sys", msgs, []Tool{bashToolDef()}, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if bytes.Contains(body, []byte(`"arguments":""`)) {
		t.Errorf("the request carries an empty arguments string, which this route answers with a 400 — "+
			"and a 400 is fatal, so the session is over:\n%s", body)
	}
	if !bytes.Contains(body, []byte(`"arguments":"{}"`)) {
		t.Errorf("a zero-argument call was not rendered as {}:\n%s", body)
	}
}

// 累加器必须把线上的三种方言调和起来，不能闷头拼接。mergeArgs 的单测
// 证明函数没问题；这个测试用真的 SSE 帧把重发方言喂进去，证明流式解析
// 器确实用了它。
func TestOpenAIStreamHandlesTheReSendDialect(t *testing.T) {
	frame := func(args string) string {
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "bash", "arguments": args},
					}},
				},
			}},
		})
		return "data: " + string(payload) + "\n\n"
	}
	stream := frame("") + frame(`{"comm`) + frame(`and":"ls"`) + frame(`}`) +
		// 整个值又来一遍，重发型网关就是这么给流式收尾的
		frame(`{"command":"ls"}`) +
		`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	p := newOpenAIProvider("http://x.invalid/v1", "k", "m")
	res, err := p.ParseStream(strings.NewReader(stream), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(res.Calls))
	}
	got := res.Calls[0].Args
	if !json.Valid([]byte(got)) {
		t.Fatalf("the accumulated arguments do not parse: %q\nBlind appending produces `{...}{...}`, whose "+
			"error names a byte offset and nothing about the cause", got)
	}
	if got != `{"command":"ls"}` {
		t.Errorf("arguments = %q, want %q", got, `{"command":"ls"}`)
	}
}

// ---------------------------------------------------------------------------
// 阶段 10 的那处订正
// ---------------------------------------------------------------------------

// newChild 从来没拷过 `dl`，于是子 Agent 拿到的是全零的 deadlines 结构体——
// 而 guardBody 和 waitFor 都把 <= 0 当成"没有看门狗"。阶段 10 整章讲的东西
// 对子 Agent 根本不生效，而且没有任何测试挂掉来说这件事。
func TestChildInheritsItsParentsDeadlines(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")
	a.dl = deadlines{connect: 7 * time.Second, idle: 11 * time.Second, total: 13 * time.Minute}

	child := a.newChild("kid", func() string { return "sys" })

	if child.dl != a.dl {
		t.Errorf("child deadlines = %+v, parent = %+v.\nA zero deadlines struct means every clock is off, so "+
			"the child runs with no stall detection and no total-call backstop — and the one child that hangs "+
			"forever is exactly what stage 10 exists to prevent", child.dl, a.dl)
	}
}

// id 集合**不**共享：子 Agent 有自己的消息数组，它的 id 只需要在这个数组
// 里唯一；共享一张 map 的话，并发的子 Agent 每次工具调用都要抢同一张表。
func TestChildGetsItsOwnIDSet(t *testing.T) {
	a, _ := mulAgent(&gate{yolo: true}, "")
	a.seenIDs["call_parent"] = true

	child := a.newChild("kid", func() string { return "sys" })
	if child.seenIDs == nil {
		t.Fatal("the child has no id set; uniqueIDs would write into a nil map and panic")
	}
	if child.seenIDs["call_parent"] {
		t.Error("the child inherited the parent's ids")
	}
	child.seenIDs["call_child"] = true
	if a.seenIDs["call_child"] {
		t.Error("the child's ids leaked into the parent's set; the two maps are shared")
	}
}
