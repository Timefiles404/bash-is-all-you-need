package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 这个文件测试第 06 阶段的 TUI 一半：将记录的请求转回可读内容的解码器、
// 将扁平事件流切片为调用的索引、三个视图、按键处理器，以及决定帧是否
// 落在屏幕上或损坏它的两个字符串函数。
//
// 这里没有一处会打开终端。被测试的每一个函数，要么是数据上的纯变换
// （views.go、frameBytes、joinEnds），要么是一次只处理一个按键的状态机
// （composer.handle）——这正是 tui.go 要把"状态 + 键 → 状态"、
// "状态 → 行"、"行 → 字节"这三层彻底分开的全部原因。

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// cspArgs 刻意使用**紧凑** JSON。
//
// OpenAI 适配器将 Block.Args 逐字节地通过作为 JSON 字符串；
// Anthropic 适配器将相同的字节拼接为对象，encoding/json
// 在这个过程中压缩无关的空白。冒号后的空格将
// 是两个线上格式合法不同意的唯一东西，
// 而这个文件是关于其他所有内容。`2>&1` 有意在那里：
// 两个适配器都使用 SetEscapeHTML(false) 编组，
// 所以 shell 重定向必须原样存活而不是作为 >&。
const cspArgs = `{"command":"ls -la /srv/app 2>&1"}`

const (
	cspSystem   = "You are a shell agent. 用中文回答。"
	cspCallID   = "call_ls_1"
	cspUserText = "列出 /srv/app 里的文件"
	cspToolText = "总计 4\ndrwxr-xr-x  2 app app 4096 Aug 27 04:00 .\n[exit 0]"
)

// cspConversation 是一个真实的 Agent 回合：人类提问，模型说点什么
// 并调用一个工具，工具给出回答。两个协议之间的每一处分歧，都会被它
// 演练到——系统提示词放在哪里、什么形态承载工具结果、
// 参数是字符串还是对象。
func cspConversation() []Msg {
	return []Msg{
		TextMsg(RoleUser, cspUserText),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Looking now."},
			{Kind: BlockToolCall, ID: cspCallID, Name: "bash", Args: cspArgs},
		}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock(cspCallID, cspToolText)}},
	}
}

func cspTools() []Tool {
	return []Tool{{
		Name:        "bash",
		Description: "run a shell command",
		Schema:      map[string]any{"type": "object"},
	}}
}

// cspBodies 用的是适配器本身，而不是手写的正文，把 cspConversation
// 呈现到两条线上。手写实例测试的是解码器有没有符合解码器作者自己
// 对协议的理解；而这个测试，测的是解码器能不能处理 Agent
// 实际发出的字节。
func cspBodies(t *testing.T) (openaiBody, anthropicBody json.RawMessage) {
	t.Helper()
	msgs, tools := cspConversation(), cspTools()

	_, ob, err := newOpenAIProvider("https://example.test/v1", "k", "gpt-test").
		BuildRequest(cspSystem, msgs, tools, 1024)
	if err != nil {
		t.Fatalf("openai BuildRequest: %v", err)
	}
	_, ab, err := newAnthropicProvider("https://example.test", "k", "claude-test").
		withCacheBreakpoints(true).
		BuildRequest(cspSystem, msgs, tools, 1024)
	if err != nil {
		t.Fatalf("anthropic BuildRequest: %v", err)
	}
	return ob, ab
}

// cspEvents 是一个综合会话，形状像这个查看器存在的会话：
// 三个模型调用，其中一个是上下文压缩内的总结调用，
// 其中一个在报告使用情况前就已死亡，
// 增量的流式运行所以显示不是事件，没有有效负载的使用情况事件，
// 以及 CJK 在查看器必须测量而不是计数的每个地方。
func cspEvents(t *testing.T) []Event {
	t.Helper()
	oai, anth := cspBodies(t)

	start := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	seq, ms := 0, 0
	ev := func(e Event) Event {
		seq++
		ms += 250
		e.Seq = seq
		e.T = start.Add(time.Duration(ms) * time.Millisecond)
		if e.Turn == 0 {
			e.Turn = 1
		}
		return e
	}

	return []Event{
		ev(Event{Kind: KindUserMessage, Text: cspUserText + "\n(list the files)"}),
		ev(Event{Kind: KindMemoryLoaded, Path: "记忆/AGENT.md", Bytes: 1234}),
		ev(Event{Kind: KindTurnStart}),

		// 调用 1：普通的那个。
		ev(Event{Kind: KindRequest, Request: oai}),
		ev(Event{Kind: KindFirstToken, Millis: 412}),
		ev(Event{Kind: KindTextDelta, Text: "好的"}),
		ev(Event{Kind: KindTextDelta, Text: "，我"}),
		ev(Event{Kind: KindTextDelta, Text: "看一下"}),
		ev(Event{Kind: KindTextDelta, Text: "。"}),
		ev(Event{Kind: KindToolCallStart, ToolID: cspCallID, ToolName: "bash"}),
		ev(Event{Kind: KindToolArgsDelta, Text: `{"command":`}),
		ev(Event{Kind: KindToolArgsDelta, Text: `"ls -la /srv/app 2>&1"}`}),
		ev(Event{Kind: KindToolCallReady, ToolID: cspCallID, Command: "ls -la /srv/app 2>&1"}),
		ev(Event{Kind: KindGateVerdict, Verdict: "allow", Text: "read-only command"}),
		ev(Event{Kind: KindCommandStart, Command: "ls -la /srv/app 2>&1"}),
		ev(Event{Kind: KindCommandEnd, ExitCode: 0, Millis: 12, Bytes: 4096, Truncated: true}),
		ev(Event{Kind: KindToolResult, Text: cspToolText}),
		ev(Event{Kind: KindUsage, Usage: &Usage{Input: 300, CacheWrite: 9000, CacheRead: 0, Output: 26}}),
		ev(Event{Kind: KindResponseEnd, FinishReason: "tool_use", Millis: 900}),

		// 没有有效负载的使用情况事件。godLine 为其返回**零**行，
		// 所以 TUI 中的每个行索引遍历都必须处理占用零行的事件——
		// 这正是 clickAt 会遇到的差一错误。
		ev(Event{Kind: KindUsage}),

		// 调用 2：compact_start 和 compact_end 之间的总结器。
		ev(Event{Kind: KindCompactStart, MsgsBefore: 30, TokensBefore: 40000, Text: "over budget"}),
		ev(Event{Kind: KindRequest, Request: anth, Turn: 2}),
		ev(Event{Kind: KindUsage, Turn: 2, Usage: &Usage{Input: 12000, CacheRead: 8000, Output: 400}}),
		ev(Event{Kind: KindCompactEnd, Turn: 2, MsgsBefore: 30, MsgsAfter: 4,
			TokensBefore: 40000, TokensAfter: 900, Millis: 1500}),
		ev(Event{Kind: KindCacheInvalidated, Turn: 2, Text: "prefix rewritten; 9,775 tokens lost"}),

		// 调用 3：在调用中途崩溃。只有一个请求，别的什么都没有——没有使用情况，
		// 没有 response_end。这正是查看器绝不能丢弃的那个调用。
		ev(Event{Kind: KindRequest, Request: anth, Turn: 3}),
		ev(Event{Kind: KindReasoningDelta, Turn: 3, Text: "the user asked "}),
		ev(Event{Kind: KindReasoningDelta, Turn: 3, Text: "for a listing"}),
		ev(Event{Kind: KindError, Turn: 3, Text: "connection reset by peer"}),
		ev(Event{Kind: KindNotice, Turn: 3, Text: "the trace ends here"}),
		ev(Event{Kind: KindTurnEnd, Turn: 3}),
	}
}

func cspSession(t *testing.T) *session {
	t.Helper()
	return indexSession("traces/composer_test.jsonl", cspEvents(t))
}

func cspComposer(t *testing.T, v viewKind, w, h int) *composer {
	t.Helper()
	c := &composer{path: "traces/composer_test.jsonl", s: cspSession(t), view: v, w: w, h: h}
	c.relayout()
	return c
}

// cspStripCache 清空每个 Cached 标记并报告它清除了多少个。
//
// 标记是两个协议可能合法不同意的**唯一**东西——
// CacheMarks 实际上是它们的和——所以它们被作为数字比较，
// 然后移除，让视图的其余部分被真正比较。
func cspStripCache(v *wireView) int {
	n := 0
	clear := func(bs []wireBlock) {
		for i := range bs {
			if bs[i].Cached {
				n++
				bs[i].Cached = false
			}
		}
	}
	clear(v.System)
	for i := range v.Messages {
		clear(v.Messages[i].Blocks)
	}
	return n
}

// cspWithin 在自己的 goroutine 上运行 fn，如果它没有完成就失败。
//
// 挂起的渲染器，没有堆栈跟踪，也没有谁会去留意的 CPU 尖峰：
// 症状只是一个冻住的用户界面。width_test.go 出于同样的原因这样
// 保护 wrapCols，而下面的每一个视图，最终都会汇入 wrapCols。
func cspWithin(t *testing.T, what string, fn func() []string) []string {
	t.Helper()
	done := make(chan []string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s panicked: %v — a viewer that dies on a hostile window "+
					"size takes the whole session's evidence with it", what, r)
				done <- nil
			}
		}()
		done <- fn()
	}()
	select {
	case got := <-done:
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return within 2s — a rune wider than the whole pane made "+
			"a wrap loop retry the same rune forever", what)
		return nil
	}
}

// ---------------------------------------------------------------------------
// decodeRequest——整个章节所依赖的往返
// ---------------------------------------------------------------------------

// TestDecodeRequestReadsBothProtocols 是中心部分。
//
// 第 06 阶段的主张是两个完全不同的线上格式生成**一个**可读视图。
// 那个主张的价值完全取决于它的证据，所以这个测试构建真实对话，
// 将它发送到两个适配器，并通过相同的解码器读回两个正文。
func TestDecodeRequestReadsBothProtocols(t *testing.T) {
	oaiBody, anthBody := cspBodies(t)

	cases := []struct {
		name       string
		body       json.RawMessage
		protocol   string
		model      string
		cacheMarks int
	}{
		{"openai", oaiBody, "openai", "gpt-test", 0},
		{"anthropic", anthBody, "anthropic", "claude-test", 2},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := decodeRequest(c.body)

			if v.Err != "" {
				t.Fatalf("decodeRequest(%s body) = error %q — the viewer cannot read the "+
					"bytes this repo's own adapter just produced\nbody: %s", c.name, v.Err, c.body)
			}
			if v.Protocol != c.protocol {
				t.Errorf("Protocol = %q, want %q — the sniff picked the wrong decoder, so "+
					"every field below it is being read out of the wrong shape",
					v.Protocol, c.protocol)
			}
			if v.Model != c.model || v.MaxTokens != 1024 {
				t.Errorf("Model/MaxTokens = %q/%d, want %q/1024", v.Model, v.MaxTokens, c.model)
			}
			if v.Bytes != len(c.body) {
				t.Errorf("Bytes = %d, want %d — Bytes is what the Model view reports as the "+
					"size of the request", v.Bytes, len(c.body))
			}

			// **分歧 1**：一个协议把系统提示词放在 messages[0] 里，另一个协议
			// 把它放在一个顶层字段里。两者都必须在这里着陆。
			if len(v.System) != 1 || v.System[0].Text != cspSystem {
				t.Fatalf("System = %+v, want one block %q — the system prompt is the largest "+
					"and least visible part of what the model saw; a view that drops it on one "+
					"protocol is a view you cannot compare across protocols", v.System, cspSystem)
			}

			if len(v.Messages) != 3 {
				t.Fatalf("%d messages, want 3 — the conversation was user / assistant+tool_call / "+
					"tool_result, and the count is the number the Model view header prints as "+
					"\"the model can see N messages\"\ngot: %+v", len(v.Messages), v.Messages)
			}
			wantRoles := []string{"user", "assistant", "user"}
			for i, want := range wantRoles {
				if v.Messages[i].Role != want {
					t.Errorf("Messages[%d].Role = %q, want %q — DISAGREEMENT 2 is that one "+
						"protocol answers a tool call with role:\"tool\" and the other with a "+
						"user message; the reader must not be able to tell",
						i, v.Messages[i].Role, want)
				}
			}

			// 工具调用，以及**分歧 4**：一条线上的 JSON 字符串，
			// 另一条线上的 JSON 对象，无论哪种方式都是相同的字节。
			tc := cspFindBlock(v, "tool_call")
			if tc == nil {
				t.Fatalf("no tool_call block in %+v — the single most useful thing in a "+
					"request body is what the model asked to run", v.Messages)
			}
			if tc.Name != "bash" {
				t.Errorf("tool_call Name = %q, want \"bash\"", tc.Name)
			}
			if tc.Args != cspArgs {
				t.Errorf("tool_call Args = %q, want %q — the arguments are what the Model "+
					"view renders as the command; a re-serialised copy is also a different "+
					"cache key", tc.Args, cspArgs)
			}

			tr := cspFindBlock(v, "tool_result")
			if tr == nil {
				t.Fatalf("no tool_result block in %+v", v.Messages)
			}
			if tr.ID != tc.ID {
				t.Errorf("tool_result is addressed to %q but the call was %q — a result "+
					"attached to the wrong id is how a viewer shows you the answer to a "+
					"different question", tr.ID, tc.ID)
			}
			if tr.ID != cspCallID {
				t.Errorf("tool call/result id = %q, want %q", tr.ID, cspCallID)
			}
			if tr.Text != cspToolText {
				t.Errorf("tool_result Text = %q, want %q — the bytes the model was given are "+
					"not the bytes the command printed, and only this view can tell you that",
					tr.Text, cspToolText)
			}

			if v.CacheMarks != c.cacheMarks {
				t.Errorf("CacheMarks = %d, want %d — the breakpoint count is the difference "+
					"between a prefix that is cached and one that is re-read at full price on "+
					"every turn", v.CacheMarks, c.cacheMarks)
			}
			if want := []string{"bash"}; !reflect.DeepEqual(v.Tools, want) {
				t.Errorf("Tools = %v, want %v — DISAGREEMENT 3 is the envelope, not the name",
					v.Tools, want)
			}
		})
	}
}

func cspFindBlock(v wireView, kind string) *wireBlock {
	for i := range v.Messages {
		for j := range v.Messages[i].Blocks {
			if v.Messages[i].Blocks[j].Kind == kind {
				return &v.Messages[i].Blocks[j]
			}
		}
	}
	return nil
}

// TestDecodeRequestGivesBothProtocolsTheSameView 断言的就是这个
// 主张本身，而不是描述它。
//
// Protocol、Model、Bytes 和 CacheMarks 允许不一样——它们是**关于**
// 线上情况的陈述。除此之外的一切都是对话内容，如果两个解码出来的
// 视图在其中任何一点上有分歧，那么模型视角向你展示的，就会是一个
// 随你恰好调用了哪个端点而变的、不一样的会话——这就让它在自己
// 唯一的本职工作上变得毫无用处。
func TestDecodeRequestGivesBothProtocolsTheSameView(t *testing.T) {
	oaiBody, anthBody := cspBodies(t)
	o, a := decodeRequest(oaiBody), decodeRequest(anthBody)

	// 每个块的 Cached 标记**就是** CacheMarks，
	// 所以它们被作为计数比较然后移除。剩下的任何东西都是真正的分歧。
	oCached, aCached := cspStripCache(&o), cspStripCache(&a)
	if oCached != o.CacheMarks || aCached != a.CacheMarks {
		t.Errorf("CacheMarks (%d openai / %d anthropic) does not equal the number of blocks "+
			"actually marked (%d / %d) — the header would report a number the body does not show",
			o.CacheMarks, a.CacheMarks, oCached, aCached)
	}
	if aCached != 2 {
		t.Errorf("the anthropic body has %d cache_control markers, want 2 (the system prefix "+
			"and the rolling conversation breakpoint)", aCached)
	}
	if oCached != 0 {
		t.Errorf("the openai body has %d cache markers, want 0 — that protocol has no such "+
			"field, and inventing one would make the two views agree by lying", oCached)
	}

	o.Protocol, a.Protocol = "", ""
	o.Model, a.Model = "", ""
	o.Bytes, a.Bytes = 0, 0
	o.CacheMarks, a.CacheMarks = 0, 0

	if !reflect.DeepEqual(o, a) {
		t.Fatalf("the two protocols decode to DIFFERENT views, which is the one thing this "+
			"chapter claims cannot happen.\n openai: %s\n anthro: %s",
			cspDump(o), cspDump(a))
	}
}

// cspDump 为一条失败消息渲染出一个 wireView。%+v 用在嵌套的结构体
// 切片上，打印出来的是没法读的一整行；而这个函数打印出来的，
// 是它的形状。
func cspDump(v wireView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  max_tokens=%d tools=%v err=%q", v.MaxTokens, v.Tools, v.Err)
	for _, s := range v.System {
		fmt.Fprintf(&b, "\n  system %s %q", s.Kind, s.Text)
	}
	for i, m := range v.Messages {
		fmt.Fprintf(&b, "\n  [%d] %s", i, m.Role)
		for _, bl := range m.Blocks {
			fmt.Fprintf(&b, "\n      %s id=%q name=%q args=%q text=%q",
				bl.Kind, bl.ID, bl.Name, bl.Args, bl.Text)
		}
	}
	return b.String()
}

// TestDecodeRequestSniffsOnTheTopLevelSystemKey 固定了辨别器。
//
// 诱人的替代方案——"messages[0].role == system 表示 OpenAI"——
// 在两个方向都是错误的，并且无声地错误：
// 它用 Anthropic 解码器读取 OpenAI 正文（其 `content` 是数组，
// 不是字符串）反之亦然，你只看到空视图。
func TestDecodeRequestSniffsOnTheTopLevelSystemKey(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"openai body: system IS messages[0], and there is no top-level system key",
			`{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`,
			"openai",
		},
		{
			"anthropic body: messages[0] is the user, the system prompt is up top",
			`{"model":"m","max_tokens":8,"system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":[{"type":"text","text":"u"}]}]}`,
			"anthropic",
		},
		{
			"anthropic body with no messages at all still sniffs anthropic",
			`{"model":"m","system":[{"type":"text","text":"s"}],"messages":[]}`,
			"anthropic",
		},
		{
			"openai body with no system message still sniffs openai",
			`{"model":"m","messages":[{"role":"user","content":"u"}]}`,
			"openai",
		},
	}
	for _, c := range cases {
		v := decodeRequest(json.RawMessage(c.body))
		if v.Protocol != c.want {
			t.Errorf("decodeRequest(%s) = protocol %q, want %q — %s",
				c.body, v.Protocol, c.want, c.name)
		}
		if v.Err != "" {
			t.Errorf("decodeRequest(%s) = err %q — the wrong decoder ran and choked on a "+
				"field of the wrong type; the view would be blank with no explanation",
				c.body, v.Err)
		}
	}
}

// TestDecodeRequestSurvivesRubbish。trace 是崩溃留下的证据，
// 所以查看器读到的，是这个仓库里任何编码器都不会生成的正文：
// 写了一半的行、手改过的文件、三个版本前的旧构建。这些情况
// 一个都不能引发恐慌，而且都必须在屏幕上说清楚出了什么问题，
// 而不是甩出一个堆栈跟踪。
func TestDecodeRequestSurvivesRubbish(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		wantMsg int
	}{
		{"not JSON at all", "not json", true, 0},
		{"empty object", "{}", false, 0},
		{"valid JSON, no messages", `{"model":"m","max_tokens":4,"messages":[]}`, false, 0},
		{"valid JSON, null messages", `{"model":"m","messages":null}`, false, 0},
		{"anthropic shape, no messages", `{"system":[{"type":"text","text":"s"}],"messages":[]}`, false, 0},
		{"a JSON array where an object belongs", `[1,2,3]`, true, 0},
		{"truncated mid-string", `{"model":"m","messages":[{"role":"user","content":"abc`, true, 0},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("decodeRequest(%q) panicked: %v — %s. A viewer that dies on a "+
						"damaged trace is a viewer that dies on exactly the traces worth "+
						"opening", c.body, r, c.name)
				}
			}()
			v := decodeRequest(json.RawMessage(c.body))
			if (v.Err != "") != c.wantErr {
				t.Errorf("decodeRequest(%q).Err = %q, want an error: %v — %s",
					c.body, v.Err, c.wantErr, c.name)
			}
			if len(v.Messages) != c.wantMsg {
				t.Errorf("decodeRequest(%q) produced %d messages, want %d — %s",
					c.body, len(v.Messages), c.wantMsg, c.name)
			}
			if v.Bytes != len(c.body) {
				t.Errorf("decodeRequest(%q).Bytes = %d, want %d — the size is known before "+
					"the parse and must survive it failing", c.body, v.Bytes, len(c.body))
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// collapseDeltas
// ---------------------------------------------------------------------------

// TestCollapseDeltasMergesARun。一次流式回复，是一千个四字符事件；
// 一个逐行渲染、一行一个事件的查看器，是没人能滚动得动的查看器。
func TestCollapseDeltasMergesARun(t *testing.T) {
	const n = 200
	var events []Event
	var want strings.Builder
	for i := 0; i < n; i++ {
		frag := fmt.Sprintf("f%d ", i)
		want.WriteString(frag)
		events = append(events, Event{Seq: i + 1, Kind: KindTextDelta, Text: frag})
	}

	got := collapseDeltas(events)
	if len(got) != 1 {
		t.Fatalf("collapseDeltas(%d text deltas) produced %d events, want 1 — the God view "+
			"renders one row per event", n, len(got))
	}
	if got[0].Text != want.String() {
		t.Errorf("collapsed Text is %d chars, want %d — the merged row is the only place the "+
			"streamed reply is readable, so losing a fragment loses the reply",
			len(got[0].Text), want.Len())
	}
	if got[0].Bytes != n {
		t.Errorf("collapsed Bytes = %d, want %d — Bytes becomes the ×N frame count in the God "+
			"view, and its ratio to the text length is how you spot a provider that switched "+
			"to one delta per token", got[0].Bytes, n)
	}
	if got[0].Kind != KindTextDelta {
		t.Errorf("collapsed Kind = %q, want %q", got[0].Kind, KindTextDelta)
	}
}

// TestCollapseDeltasKeepsTheSeqOfTheFirstDelta，让 views.go 里的
// 那条注释变得真正**举足轻重**。它说的是：合并后的事件，保留的是
// **第一个** seq，原因就是单击处理器；这两层意思，在这里都被断言到了。
func TestCollapseDeltasKeepsTheSeqOfTheFirstDelta(t *testing.T) {
	got := collapseDeltas([]Event{
		{Seq: 10, Kind: KindTextDelta, Text: "a"},
		{Seq: 11, Kind: KindTextDelta, Text: "b"},
		{Seq: 12, Kind: KindTextDelta, Text: "c"},
	})
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Seq != 10 {
		t.Errorf("collapsed Seq = %d, want 10 (the FIRST delta in the run) — the seq is what "+
			"the click handler resolves to a call, so taking the last one moves the selection "+
			"forward past the call that produced the text", got[0].Seq)
	}
}

// TestClickOnACollapsedRunSelectsTheCallItStartedIn 是后果。
//
// Seq 是文件中行的标签，不是关于它们之前的行的承诺：trace.go
// 打开文件 O_APPEND 就是为了两个写入者可以共享它，并且恢复的会话
// 扩展了一个它没有写的 trace。所以一个请求事件的 seq，完全可能
// 落在前一段增量运行的跨度内——这时"取第一个还是取最后一个"
// 就不再只是个细节，而是变成了"你到底在看哪一个调用"这个问题本身。
func TestClickOnACollapsedRunSelectsTheCallItStartedIn(t *testing.T) {
	body := json.RawMessage(`{"model":"m","messages":[]}`)
	s := indexSession("x", []Event{
		{Seq: 1, Kind: KindRequest, Request: body},
		{Seq: 2, Kind: KindTextDelta, Text: "he"},
		{Seq: 3, Kind: KindTextDelta, Text: "llo"},
		{Seq: 4, Kind: KindTextDelta, Text: "!"},
		{Seq: 4, Kind: KindRequest, Request: body},
	})
	if len(s.Calls) != 2 {
		t.Fatalf("indexSession produced %d calls, want 2", len(s.Calls))
	}
	if len(s.Display) != 3 {
		t.Fatalf("Display has %d events, want 3 (request, one collapsed run, request)", len(s.Display))
	}

	c := &composer{path: "x", s: s, view: viewGod, w: 80, h: 24, call: 1}
	c.relayout()
	// clickAt 接受的是一个屏幕行号：第 1 行是标题，第 2 行是分隔线，
	// 所以第 3 行才是**第一条**正文行（也就是请求），第 4 行是第二条——
	// 也就是折叠后的增量运行。
	c.clickAt(4)

	if c.call != 0 {
		t.Errorf("clicking the collapsed run selected call %d, want 0 — the run was produced "+
			"BY call 1, and selecting the next call means pressing m shows you a request that "+
			"had not been sent when that text was streamed", c.call+1)
	}
}

// TestCollapseDeltasBoundaries，覆盖的是"不算一次运行"的所有情况。
func TestCollapseDeltasBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		in    []Event
		kinds []Kind
		texts []string
		bytes []int
	}{
		{
			name:  "empty input",
			in:    nil,
			kinds: nil,
		},
		{
			name: "a non-delta between two runs keeps them apart",
			in: []Event{
				{Seq: 1, Kind: KindTextDelta, Text: "a"},
				{Seq: 2, Kind: KindTextDelta, Text: "b"},
				{Seq: 3, Kind: KindToolCallReady, Command: "ls"},
				{Seq: 4, Kind: KindTextDelta, Text: "c"},
				{Seq: 5, Kind: KindTextDelta, Text: "d"},
			},
			kinds: []Kind{KindTextDelta, KindToolCallReady, KindTextDelta},
			texts: []string{"ab", "", "cd"},
			bytes: []int{2, 0, 2},
		},
		{
			name: "adjacent runs of DIFFERENT kinds do not merge",
			in: []Event{
				{Seq: 1, Kind: KindReasoningDelta, Text: "think"},
				{Seq: 2, Kind: KindReasoningDelta, Text: "ing"},
				{Seq: 3, Kind: KindTextDelta, Text: "say"},
				{Seq: 4, Kind: KindTextDelta, Text: "ing"},
				{Seq: 5, Kind: KindToolArgsDelta, Text: `{"a":`},
				{Seq: 6, Kind: KindToolArgsDelta, Text: `1}`},
			},
			kinds: []Kind{KindReasoningDelta, KindTextDelta, KindToolArgsDelta},
			texts: []string{"thinking", "saying", `{"a":1}`},
			bytes: []int{2, 2, 2},
		},
		{
			name: "a lone delta is still collapsed, and counts as one frame",
			in: []Event{
				{Seq: 1, Kind: KindTextDelta, Text: "x"},
			},
			kinds: []Kind{KindTextDelta},
			texts: []string{"x"},
			bytes: []int{1},
		},
		{
			name: "no deltas at all: everything passes through",
			in: []Event{
				{Seq: 1, Kind: KindUserMessage, Text: "hi"},
				{Seq: 2, Kind: KindRequest},
				{Seq: 3, Kind: KindResponseEnd, FinishReason: "stop"},
			},
			kinds: []Kind{KindUserMessage, KindRequest, KindResponseEnd},
			texts: []string{"hi", "", ""},
			bytes: []int{0, 0, 0},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collapseDeltas(c.in)
			if len(got) != len(c.kinds) {
				t.Fatalf("collapseDeltas produced %d events %v, want %d %v",
					len(got), cspKinds(got), len(c.kinds), c.kinds)
			}
			for i := range got {
				if got[i].Kind != c.kinds[i] {
					t.Errorf("event %d is %q, want %q — collapsing must not reorder or "+
						"re-label the stream; the God view IS the order", i, got[i].Kind, c.kinds[i])
				}
				if got[i].Text != c.texts[i] {
					t.Errorf("event %d Text = %q, want %q", i, got[i].Text, c.texts[i])
				}
				if got[i].Bytes != c.bytes[i] {
					t.Errorf("event %d Bytes = %d, want %d", i, got[i].Bytes, c.bytes[i])
				}
			}
		})
	}
}

func cspKinds(events []Event) []Kind {
	out := make([]Kind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// TestCollapseDeltasLeavesNonDeltasByteIdentical。凡是不属于增量的
// 内容，都必须原样从另一头出来——字段相同，顺序相同。要是某次折叠
// 悄悄把 command_end 上的 Bytes 清零了，那就是在无声地篡改
// 上帝视角打印出来的数字。
func TestCollapseDeltasLeavesNonDeltasUntouched(t *testing.T) {
	in := []Event{
		{Seq: 1, Kind: KindCommandEnd, ExitCode: 2, Millis: 40, Bytes: 4096, Truncated: true},
		{Seq: 2, Kind: KindTextDelta, Text: "a"},
		{Seq: 3, Kind: KindUsage, Usage: &Usage{Input: 1, Output: 2}},
		{Seq: 4, Kind: KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	}
	want := []Event{in[0], in[2], in[3]}

	got := collapseDeltas(in)
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4", len(got))
	}
	pass := []Event{got[0], got[2], got[3]}
	if !reflect.DeepEqual(pass, want) {
		t.Errorf("non-delta events were modified.\n got: %+v\nwant: %+v", pass, want)
	}
}

// ---------------------------------------------------------------------------
// indexSession
// ---------------------------------------------------------------------------

// TestIndexSessionAnchorsCallsOnTheRequest，包括那个已经死掉的调用。
//
// 失败路径是这里唯一要紧的路径：一个在拿到第一个 token 之前就崩溃的
// 调用，仍然会发出一个请求；而一个把锚点定在 usage 或 response_end
// 上的查看器，恰恰会在你打开它最想看的那些会话上，变成一片空白。
func TestIndexSessionAnchorsCallsOnTheRequest(t *testing.T) {
	s := cspSession(t)

	if len(s.Calls) != 3 {
		t.Fatalf("indexSession found %d calls, want 3 (ordinary, summariser, crashed) — "+
			"kinds in order: %v", len(s.Calls), cspKinds(s.Events))
	}
	if s.Calls[2].Usage != nil {
		t.Errorf("call 3 has usage %+v, want nil — it is the crashed call and the fixture "+
			"gives it none", s.Calls[2].Usage)
	}
	if len(s.Calls[2].Request) == 0 {
		t.Errorf("call 3 has no request body — the request is the one event a dead call is " +
			"guaranteed to have produced, and it is what the Model view renders")
	}
	for i, c := range s.Calls {
		if len(c.Events) == 0 || c.Events[0].Kind != KindRequest {
			t.Errorf("call %d starts with %v, want the slice to begin at its own KindRequest",
				i+1, cspKinds(c.Events))
		}
		if c.Seq != c.Events[0].Seq {
			t.Errorf("call %d has Seq %d but its first event is Seq %d — selectCall and "+
				"clickAt both look the call up by this number", i+1, c.Seq, c.Events[0].Seq)
		}
	}
}

// TestIndexSessionAttachesUsageAndTotals。
func TestIndexSessionAttachesUsageAndTotals(t *testing.T) {
	s := cspSession(t)

	if s.Calls[0].Usage == nil || s.Calls[0].Usage.CacheWrite != 9000 {
		t.Fatalf("call 1 usage = %+v, want the 9000-token cache write the fixture emitted "+
			"right after it", s.Calls[0].Usage)
	}
	if s.Calls[1].Usage == nil || s.Calls[1].Usage.CacheRead != 8000 {
		t.Fatalf("call 2 usage = %+v, want the summariser's own accounting — usage attaches "+
			"to the call it followed, not to the newest one", s.Calls[1].Usage)
	}

	want := Usage{Input: 12300, CacheWrite: 9000, CacheRead: 8000, Output: 426}
	if s.Total != want {
		t.Errorf("Total = %+v, want %+v — Total is what a cost report is computed from, and "+
			"input_tokens alone under-reports a cached session by an order of magnitude",
			s.Total, want)
	}
	if got := s.Total.Prompt(); got != 29300 {
		t.Errorf("Total.Prompt() = %d, want 29300", got)
	}
}

// TestIndexSessionMarksTheCompactingCall。
//
// 总结调用不是 Agent：其请求包含被丢弃的对话，不是 Agent 正在进行的
// 对话。混淆两者会使模型视角向你显示已被删除的历史记录。
func TestIndexSessionMarksTheCompactingCall(t *testing.T) {
	s := cspSession(t)

	if s.Compactions != 1 {
		t.Errorf("Compactions = %d, want 1 — the count comes from compact_END, because a "+
			"compaction that started and never finished did not compact anything", s.Compactions)
	}
	want := []bool{false, true, false}
	for i, w := range want {
		if s.Calls[i].Compaction != w {
			t.Errorf("call %d Compaction = %v, want %v — call 1 is before compact_start, "+
				"call 2 is between the two markers, call 3 is after compact_end and is the "+
				"agent again", i+1, s.Calls[i].Compaction, w)
		}
	}
}

// TestIndexSessionKeepsEventsBeforeTheFirstRequest。
// 用户的消息、加载的记忆文件、回合标记——
// 所有这些都在任何调用存在之前发生，所有这些都属于上帝视角。
func TestIndexSessionKeepsEventsBeforeTheFirstRequest(t *testing.T) {
	events := cspEvents(t)
	s := indexSession("x", events)

	if len(s.Events) != len(events) {
		t.Fatalf("Events has %d entries, want all %d — the God view renders this slice and "+
			"nothing else", len(s.Events), len(events))
	}
	if s.Events[0].Kind != KindUserMessage {
		t.Errorf("Events[0] is %q, want %q — the prologue must not be trimmed to the first "+
			"call", s.Events[0].Kind, KindUserMessage)
	}
	if s.Start != events[0].T {
		t.Errorf("Start = %v, want %v — every God-view offset is measured from it", s.Start, events[0].T)
	}

	// 它们不属于任何调用，这是同一陈述的另一半。
	inCalls := 0
	for _, c := range s.Calls {
		inCalls += len(c.Events)
	}
	if inCalls >= len(events) {
		t.Errorf("%d of %d events were assigned to a call — the three events before the first "+
			"request belong to none", inCalls, len(events))
	}
}

// TestIndexSessionOnAnEmptyStream。`r` 重新加载另一个进程仍在创建的
// trace，所以零事件读是寻常的日子。
func TestIndexSessionOnAnEmptyStream(t *testing.T) {
	s := indexSession("x", nil)
	if s == nil {
		t.Fatal("indexSession(nil) returned nil — the caller dereferences it immediately")
	}
	if len(s.Calls) != 0 || len(s.Display) != 0 || s.Compactions != 0 {
		t.Errorf("indexSession(nil) = %+v, want an empty index", s)
	}
	if !s.Start.IsZero() {
		t.Errorf("Start = %v, want the zero time", s.Start)
	}
}

// ---------------------------------------------------------------------------
// 视图，在没人设计的窗口大小处
// ---------------------------------------------------------------------------

// TestViewsSurviveHostileWidths。
//
// 宽度 1 不是开玩笑：它是一个被拖到边缘的分割窗格，也是会把一个
// 天真的换行逻辑直接挂死的场景——因为一个 CJK 字形占两列，永远
// 放不下——于是"清空这一行，重试这个 rune"就会永远重试下去。
// 宽度 400 是一个超宽终端，算术会朝相反的方向出错。
func TestViewsSurviveHostileWidths(t *testing.T) {
	s := cspSession(t)
	widths := []int{1, 5, 20, 100, 400}

	views := []struct {
		name string
		fn   func(w int) []string
	}{
		{"godView", func(w int) []string { lines, _ := s.godView(w, 0); return lines }},
		{"modelView(0)", func(w int) []string { return s.modelView(0, w) }},
		{"modelView(1)", func(w int) []string { return s.modelView(1, w) }},
		{"wireView(0)", func(w int) []string { return s.wireView(0, w) }},
		{"wireView(2)", func(w int) []string { return s.wireView(2, w) }},
	}

	for _, v := range views {
		for _, w := range widths {
			what := fmt.Sprintf("%s at width %d", v.name, w)
			lines := cspWithin(t, what, func() []string { return v.fn(w) })
			if len(lines) == 0 {
				t.Errorf("%s returned no lines at all — the pane would be blank with no "+
					"indication that anything was there", what)
				continue
			}
			for i, l := range lines {
				n := dispWidth(l)
				if n < 0 {
					t.Errorf("%s line %d = %q measures %d columns — a negative width means "+
						"the escape scanner ran off the end of the string", what, i, l, n)
				}
				if n > len(l) {
					t.Errorf("%s line %d = %q measures %d columns from %d bytes — no string "+
						"can be wider than its own byte count", what, i, l, n, len(l))
				}
				if !utf8.ValidString(l) {
					t.Errorf("%s line %d = %q is not valid UTF-8 — a cut landed inside a "+
						"multi-byte rune and half a character is on its way to the terminal",
						what, i, l)
				}
			}
		}
	}
}

// TestViewsAtWidthOneWithCJK 隔离了挂起。
//
// 实例携带的每个字符串——用户的消息、记忆路径、
// 工具输出——都是中文，所以在宽度 1 处，每个视图中的每个
// 换行决定都是不可满足的那个。
func TestViewsAtWidthOneWithCJK(t *testing.T) {
	s := cspSession(t)
	if !strings.Contains(cspUserText, "列") {
		t.Fatal("the fixture lost its CJK; this test then proves nothing")
	}
	cspWithin(t, "godView at width 1", func() []string { l, _ := s.godView(1, 0); return l })
	cspWithin(t, "modelView at width 1", func() []string { return s.modelView(0, 1) })
	cspWithin(t, "wireView at width 1", func() []string { return s.wireView(0, 1) })
}

// TestModelAndWireViewsOutOfRange。`n` 在列表的末尾，
// 一个缩短 trace 的重新加载，一个由人类键入的 --call 标记：
// 索引从三个方向超出范围，重新绘制内的切片索引恐慌
// 会把终端拖下去。
func TestModelAndWireViewsOutOfRange(t *testing.T) {
	body := json.RawMessage(`{"model":"m","messages":[]}`)
	s := indexSession("x", []Event{
		{Seq: 1, Kind: KindRequest, Request: body},
		{Seq: 2, Kind: KindRequest, Request: body},
	})
	if len(s.Calls) != 2 {
		t.Fatalf("fixture has %d calls, want 2", len(s.Calls))
	}

	for _, idx := range []int{-1, -999, 2, 999} {
		for _, v := range []struct {
			name string
			fn   func(int, int) []string
		}{
			{"modelView", s.modelView},
			{"wireView", s.wireView},
		} {
			lines := cspWithin(t, fmt.Sprintf("%s(%d)", v.name, idx), func() []string {
				return v.fn(idx, 80)
			})
			if len(lines) == 0 {
				t.Errorf("%s(%d, 80) returned no lines — an out-of-range call must produce a "+
					"message, not an empty pane the user reads as a hung program", v.name, idx)
				continue
			}
			if !strings.Contains(lines[0], "no calls") {
				t.Errorf("%s(%d, 80)[0] = %q, want something that says there is nothing to "+
					"show", v.name, idx, lines[0])
			}
		}
	}

	// 范围内仍然工作，所以保护不是简单的始终打开。
	for idx := 0; idx < 2; idx++ {
		if got := s.modelView(idx, 80); strings.Contains(got[0], "no calls") {
			t.Errorf("modelView(%d, 80) reported no calls, but call %d exists", idx, idx+1)
		}
	}
}

// TestModelViewHeaderShowsTheDivergence。标题就是这一章的核心：
// 那些发生在模型能看到的消息旁边的事件，以及两者一旦分道扬镳，
// 随之而来的警告。
func TestModelViewHeaderShowsTheDivergence(t *testing.T) {
	s := cspSession(t)

	first := strings.Join(s.modelView(0, 120), "\n")
	if !strings.Contains(first, "events happened so far") || !strings.Contains(first, "the model can see") {
		t.Errorf("modelView(0) header does not put both counts on one line:\n%s", first)
	}
	if strings.Contains(first, "compaction(s) happened before this call") {
		t.Errorf("modelView(0) warns about a compaction that had not happened yet:\n%s", first)
	}

	second := strings.Join(s.modelView(1, 120), "\n")
	if !strings.Contains(second, "the summarising call") {
		t.Errorf("modelView(1) does not say it is the summarising call — its request holds "+
			"the history being thrown away, not the history the agent has:\n%s", second)
	}

	third := strings.Join(s.modelView(2, 120), "\n")
	if !strings.Contains(third, "compaction(s) happened before this call") {
		t.Errorf("modelView(2) does not warn that a compaction preceded it. Everything it "+
			"shows is what SURVIVED, and a reader who does not know that will conclude the "+
			"agent forgot something it was never told:\n%s", third)
	}
}

// TestWireViewShowsTheRawBody。线上视角之所以存在，是因为模型视角
// 说了一个诚实的小谎（它把 OpenAI 的 system 消息提到了 System 里），
// 所以这一个视角必须做到逐字节忠实。
func TestWireViewShowsTheRawBody(t *testing.T) {
	s := cspSession(t)
	out := strings.Join(s.wireView(0, 200), "\n")

	if !strings.Contains(out, "on the wire, exactly as sent") {
		t.Errorf("wireView(0) lost its header:\n%s", out)
	}
	if !strings.Contains(out, `"role"`) || !strings.Contains(out, "2>&1") {
		t.Errorf("wireView(0) does not contain the raw JSON keys or the shell redirect — "+
			"when the answer is in the punctuation, this is the only view that has it:\n%s", out)
	}

	// 根本不是 JSON 的正文仍然必须被显示，不被吞没。
	bad := indexSession("x", []Event{{Seq: 1, Kind: KindRequest, Request: json.RawMessage("not json")}})
	lines := bad.wireView(0, 80)
	if len(lines) < 2 || !strings.Contains(lines[0], "not valid JSON") {
		t.Errorf("wireView on an unparseable body = %q, want an error line followed by the "+
			"bytes themselves — a trace records what was sent, including the malformed thing "+
			"you are trying to explain", lines)
	}
}

// TestOneLineNeverSwallowsANewline。"这个字符串里有换行符"，
// 常常就是那个 bug，所以它们会变成一个可见的标记，而不是直接
// 消失不见。
func TestOneLineNeverSwallowsANewline(t *testing.T) {
	cases := []struct {
		name string
		s    string
		w    int
	}{
		{"plain", "a\nb", 40},
		{"trailing newlines are trimmed, interior ones are not", "a\nb\n\n", 40},
		{"absurdly narrow", "a\nb", 0},
		{"negative width", "a\nb", -30},
		{"CJK at width zero", "你好\n世界", 0},
	}
	for _, c := range cases {
		got := oneLine(c.s, c.w)
		if strings.Contains(c.s, "\n") && !strings.Contains(got, "⏎") && dispWidth(got) >= 3 {
			t.Errorf("oneLine(%q, %d) = %q — the newline disappeared instead of becoming a "+
				"marker; %s", c.s, c.w, got, c.name)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("oneLine(%q, %d) = %q still contains a literal newline, which will push "+
				"every God-view row below it down one line", c.s, c.w, got)
		}
		if w := dispWidth(got); w > max(8, c.w) {
			t.Errorf("oneLine(%q, %d) is %d columns, want at most %d", c.s, c.w, w, max(8, c.w))
		}
	}
}

// ---------------------------------------------------------------------------
// composer.handle——状态机，无头
// ---------------------------------------------------------------------------

// TestHandleQuitKeys。三条出路，其他一切都必须留在里面。
// 意外退出的键会失去读者在两千行 trace 中的滚动位置；
// 不退出的退出键让他们被困在备用屏中。
func TestHandleQuitKeys(t *testing.T) {
	quits := []struct {
		name string
		k    key
	}{
		{"q", key{Kind: keyRune, Rune: 'q'}},
		{"Ctrl-C", key{Kind: keyCtrlC}},
		{"Ctrl-D", key{Kind: keyCtrlD}},
		{"Escape with no help open", key{Kind: keyEsc}},
	}
	for _, c := range quits {
		c := c
		t.Run("quits/"+c.name, func(t *testing.T) {
			cmp := cspComposer(t, viewGod, 80, 24)
			if cmp.handle(c.k) {
				t.Errorf("handle(%s) returned true — the loop keeps running and the user "+
					"cannot get out", c.name)
			}
		})
	}

	stays := []struct {
		name string
		k    key
	}{
		{"Enter", key{Kind: keyEnter}},
		{"Tab", key{Kind: keyTab}},
		{"Shift-Tab", key{Kind: keyShiftTab}},
		{"Backspace", key{Kind: keyBackspace}},
		{"Delete", key{Kind: keyDelete}},
		{"Ctrl-L", key{Kind: keyCtrlL}},
		{"Up", key{Kind: keyUp}},
		{"Down", key{Kind: keyDown}},
		{"Left", key{Kind: keyLeft}},
		{"Right", key{Kind: keyRight}},
		{"Home", key{Kind: keyHome}},
		{"End", key{Kind: keyEnd}},
		{"PageUp", key{Kind: keyPageUp}},
		{"PageDown", key{Kind: keyPageDown}},
		{"paste", key{Kind: keyPaste, Text: "pasted"}},
		{"an unknown but well-formed sequence", key{Kind: keyUnknown, Raw: "\x1b[99~"}},
		{"a wheel event", key{Kind: keyMouse, Mouse: mouseEvent{Button: 64, Y: 5}}},
		{"a left click", key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, Y: 5, Press: true}}},
		{"a right click", key{Kind: keyMouse, Mouse: mouseEvent{Button: 2, Y: 5, Press: true}}},
		{"g", key{Kind: keyRune, Rune: 'g'}},
		{"m", key{Kind: keyRune, Rune: 'm'}},
		{"w", key{Kind: keyRune, Rune: 'w'}},
		{"1", key{Kind: keyRune, Rune: '1'}},
		{"2", key{Kind: keyRune, Rune: '2'}},
		{"3", key{Kind: keyRune, Rune: '3'}},
		{"j", key{Kind: keyRune, Rune: 'j'}},
		{"k", key{Kind: keyRune, Rune: 'k'}},
		{"space", key{Kind: keyRune, Rune: ' '}},
		{"n", key{Kind: keyRune, Rune: 'n'}},
		{"p", key{Kind: keyRune, Rune: 'p'}},
		{"]", key{Kind: keyRune, Rune: ']'}},
		{"[", key{Kind: keyRune, Rune: '['}},
		{"r", key{Kind: keyRune, Rune: 'r'}},
		{"?", key{Kind: keyRune, Rune: '?'}},
		{"an unbound letter", key{Kind: keyRune, Rune: 'x'}},
		{"an unbound capital", key{Kind: keyRune, Rune: 'Q'}},
		{"Ctrl-A", key{Kind: keyRune, Rune: 'a', Ctrl: true}},
		{"a CJK rune somebody typed by accident", key{Kind: keyRune, Rune: '你'}},
	}
	for _, c := range stays {
		c := c
		t.Run("stays/"+c.name, func(t *testing.T) {
			cmp := cspComposer(t, viewGod, 80, 24)
			if !cmp.handle(c.k) {
				t.Errorf("handle(%s) returned false — this key quit the program", c.name)
			}
		})
	}
}

// TestEscapeIsTwoKeys。一个按键绑定，两种行为，两者之间的分支，
// 是一个没人会去看的字段：帮助面板打开时，Escape 必须关闭帮助；
// 只有在帮助没打开的时候，它才会真正离开。
func TestEscapeIsTwoKeys(t *testing.T) {
	cmp := cspComposer(t, viewGod, 80, 24)

	if !cmp.handle(key{Kind: keyRune, Rune: '?'}) || !cmp.help {
		t.Fatalf("? did not open the help (help=%v)", cmp.help)
	}
	if !cmp.handle(key{Kind: keyEsc}) {
		t.Fatal("Escape with the help open quit the program — the user pressed it to close a " +
			"panel and lost their session instead")
	}
	if cmp.help {
		t.Error("Escape did not close the help, so the key did nothing visible at all")
	}
	if cmp.handle(key{Kind: keyEsc}) {
		t.Error("Escape with no help open did not quit — the second press has to mean what " +
			"the first one would have")
	}
}

// TestScrollingStaysInBounds，反复锤打这套夹紧机制。
//
// top 是 c.lines 上的一个索引，绘制循环读取的是 c.lines[c.top+i]。
// 它可能持有的每一个值，都必须落在 [0, len(lines)-bodyHeight] 这个
// 区间内，而且必须在**每一个**按键之后都成立，不只是等尘埃落定了
// 才成立——因为帧，就是在这些按键之间被画出来的。
func TestScrollingStaysInBounds(t *testing.T) {
	for _, v := range []viewKind{viewGod, viewModel, viewWire} {
		v := v
		t.Run(v.String(), func(t *testing.T) {
			cmp := cspComposer(t, v, 80, 10)
			if len(cmp.lines) <= cmp.bodyHeight() {
				t.Fatalf("the %s fixture renders %d lines into a body of %d; this test needs "+
					"more content than fits or it proves nothing", v, len(cmp.lines), cmp.bodyHeight())
			}
			check := func(after string) {
				t.Helper()
				maxTop := len(cmp.lines) - cmp.bodyHeight()
				if cmp.top < 0 || cmp.top > maxTop {
					t.Fatalf("after %s: top = %d, want it inside [0, %d]. draw() reads "+
						"lines[top+i], so anything outside that range is either blank rows at "+
						"the top of the pane or an index the renderer only survives because it "+
						"bounds-checks a second time", after, cmp.top, maxTop)
				}
			}

			seq := []struct {
				name string
				k    key
				n    int
			}{
				{"PageDown", key{Kind: keyPageDown}, 100},
				{"PageUp", key{Kind: keyPageUp}, 100},
				{"Down", key{Kind: keyDown}, 100},
				{"Up", key{Kind: keyUp}, 100},
				{"space", key{Kind: keyRune, Rune: ' '}, 50},
				{"k", key{Kind: keyRune, Rune: 'k'}, 200},
				{"End", key{Kind: keyEnd}, 3},
				{"j", key{Kind: keyRune, Rune: 'j'}, 50},
				{"Home", key{Kind: keyHome}, 3},
				{"wheel down", key{Kind: keyMouse, Mouse: mouseEvent{Button: 65}}, 100},
				{"wheel up", key{Kind: keyMouse, Mouse: mouseEvent{Button: 64}}, 100},
			}
			for _, step := range seq {
				for i := 0; i < step.n; i++ {
					if !cmp.handle(step.k) {
						t.Fatalf("%s quit the program", step.name)
					}
					check(fmt.Sprintf("%s #%d", step.name, i+1))
				}
			}

			// 并且两端实际上是可以到达的，所以这套夹紧机制，并不是简简单单
			// 把 top 钉死在零上。
			for i := 0; i < 100; i++ {
				cmp.handle(key{Kind: keyPageDown})
			}
			if cmp.top != len(cmp.lines)-cmp.bodyHeight() {
				t.Errorf("100 PageDowns left top at %d, want %d — the last page of the trace "+
					"is unreachable", cmp.top, len(cmp.lines)-cmp.bodyHeight())
			}
			for i := 0; i < 100; i++ {
				cmp.handle(key{Kind: keyPageUp})
			}
			if cmp.top != 0 {
				t.Errorf("100 PageUps left top at %d, want 0", cmp.top)
			}
		})
	}
}

// TestSwitchingViewsResetsScrollButOnlyWhenTheViewChanges。
//
// 上帝视角的第 400 行和模型视角的第 400 行彼此无关，
// 所以开关必须从顶部开始。在已经处于上帝视角时按 `g`
// 不是开关，并且为此丢弃读者的位置是那种小背叛，
func TestSwitchingViewsResetsScrollButOnlyWhenTheViewChanges(t *testing.T) {
	cmp := cspComposer(t, viewGod, 80, 10)
	for i := 0; i < 4; i++ {
		cmp.handle(key{Kind: keyDown})
	}
	if cmp.top == 0 {
		t.Fatalf("could not scroll the God view off the top (lines=%d body=%d)",
			len(cmp.lines), cmp.bodyHeight())
	}
	was := cmp.top

	cmp.handle(key{Kind: keyRune, Rune: 'g'})
	if cmp.view != viewGod || cmp.top != was {
		t.Errorf("pressing g in the God view moved top from %d to %d — the view did not "+
			"change, so neither should the scroll position", was, cmp.top)
	}
	cmp.handle(key{Kind: keyRune, Rune: '1'})
	if cmp.top != was {
		t.Errorf("pressing 1 in the God view moved top from %d to %d", was, cmp.top)
	}

	cmp.handle(key{Kind: keyRune, Rune: 'm'})
	if cmp.view != viewModel {
		t.Fatalf("m selected view %v, want MODEL", cmp.view)
	}
	if cmp.top != 0 {
		t.Errorf("switching to MODEL left top at %d, want 0 — the reader is now looking at "+
			"the middle of a different document for no reason they can see", cmp.top)
	}

	// Tab 循环，循环是像任何其他一样的开关。
	order := []viewKind{viewWire, viewGod, viewModel}
	for _, want := range order {
		cmp.handle(key{Kind: keyDown})
		cmp.handle(key{Kind: keyTab})
		if cmp.view != want {
			t.Fatalf("Tab selected %v, want %v", cmp.view, want)
		}
		if cmp.top != 0 {
			t.Errorf("Tab into %v left top at %d, want 0", want, cmp.top)
		}
	}
}

// TestNextAndPreviousCallClampRatherThanWrap。
//
// 对读者来说，绕回开头是错误的行为：`n` 在会话末尾一直按下去，
// 应该停在末尾，而不是无声地传送回调用 1，给你看一个二十分钟前的
// 请求，看着还挺像那么回事。
func TestNextAndPreviousCallClampRatherThanWrap(t *testing.T) {
	cmp := cspComposer(t, viewModel, 80, 24)
	last := len(cmp.s.Calls) - 1
	if last < 1 {
		t.Fatalf("the fixture has %d calls; this test needs at least two", last+1)
	}

	for _, k := range []key{{Kind: keyRune, Rune: 'p'}, {Kind: keyRune, Rune: '['}} {
		cmp.call = 0
		for i := 0; i < 5; i++ {
			cmp.handle(k)
		}
		if cmp.call != 0 {
			t.Errorf("pressing %q at call 1 moved to call %d — it must stay put, not wrap to "+
				"the end", k.Rune, cmp.call+1)
		}
	}
	for _, k := range []key{{Kind: keyRune, Rune: 'n'}, {Kind: keyRune, Rune: ']'}} {
		cmp.call = last
		for i := 0; i < 5; i++ {
			cmp.handle(k)
		}
		if cmp.call != last {
			t.Errorf("pressing %q at the last call moved to call %d — it must stay put, not "+
				"wrap to the start", k.Rune, cmp.call+1)
		}
	}

	// 并且它在有地方去时确实移动。
	cmp.call = 0
	cmp.handle(key{Kind: keyRune, Rune: 'n'})
	if cmp.call != 1 {
		t.Errorf("n at call 1 of %d selected call %d, want 2 — the clamp is not a freeze",
			last+1, cmp.call+1)
	}
}

// TestWheelScrollsThree。数字是惯例，不是偏好：一个只移动一行的
// 轮槽，会让人觉得不对劲；一个直接翻页的，又会让你找不着自己的位置。
func TestWheelScrollsThree(t *testing.T) {
	cmp := cspComposer(t, viewGod, 80, 10)
	cmp.top = 10
	cmp.clamp()
	start := cmp.top

	cmp.handle(key{Kind: keyMouse, Mouse: mouseEvent{Button: 65, X: 4, Y: 4, Press: true}})
	if cmp.top != start+3 {
		t.Errorf("wheel-down moved top from %d to %d, want %d", start, cmp.top, start+3)
	}
	cmp.handle(key{Kind: keyMouse, Mouse: mouseEvent{Button: 64, X: 4, Y: 4, Press: true}})
	if cmp.top != start {
		t.Errorf("wheel-up moved top to %d, want %d — up and down must be the same distance "+
			"or the view drifts as you rock the wheel", cmp.top, start)
	}
}

// TestClickSelectsACallOnlyInTheGodView。
//
// 上帝视角是唯一一种"一行就代表一个事件"的视角；在模型视角和线上
// 视角里，一行只是一次请求里被换行截断的一小段，把它映射到某次
// 调用上，就会在读者阅读的当口，把选中项从他们眼皮底下换掉。
func TestClickSelectsACallOnlyInTheGodView(t *testing.T) {
	click := key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 10, Y: 6, Press: true}}

	for _, v := range []viewKind{viewModel, viewWire} {
		cmp := cspComposer(t, v, 80, 24)
		cmp.call = 1
		if !cmp.handle(click) {
			t.Fatalf("a click in the %v view quit the program", v)
		}
		if cmp.call != 1 {
			t.Errorf("clicking row 6 of the %v view changed the selected call from 2 to %d — "+
				"a row there is a wrapped fragment of one request, not an event", v, cmp.call+1)
		}
		if cmp.note != "" {
			t.Errorf("clicking in the %v view set the status line to %q", v, cmp.note)
		}
	}

	god := cspComposer(t, viewGod, 80, 24)
	god.call = 0
	// 向崩溃调用的请求滚动并单击其行。
	god.top = 0
	target := -1
	line := 0
	for _, e := range god.s.Display {
		if e.Kind == KindRequest && e.Seq == god.s.Calls[2].Seq {
			target = line
			break
		}
		line += len(god.s.godLine(e, god.w))
	}
	if target < 0 {
		t.Fatal("could not find the third call's request row in the God view")
	}
	if !god.handle(key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 1, Y: target + 3, Press: true}}) {
		t.Fatal("a click in the God view quit the program")
	}
	if god.call != 2 {
		t.Errorf("clicking the third call's request row selected call %d, want 3 — this is "+
			"the whole reason the mouse is wired up: in a two-thousand line stream, \"show me "+
			"what the model saw here\" has to be a click", god.call+1)
	}
	if god.note == "" {
		t.Error("a click that changed the selection said nothing in the status line, so the " +
			"reader has no idea it worked")
	}

	// 低于最后一行的单击超出范围，必须被忽略，
	// 不被夹到最后一个调用。
	god.call = 0
	god.note = ""
	if !god.handle(key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 1, Y: 9999, Press: true}}) {
		t.Fatal("a click past the end quit the program")
	}
	if god.call != 0 {
		t.Errorf("clicking row 9999 selected call %d, want no change", god.call+1)
	}

	// 释放不是按。
	god.call = 0
	god.handle(key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 1, Y: target + 2, Press: false}})
	if god.call != 0 {
		t.Errorf("a button RELEASE selected call %d — every click would then fire twice",
			god.call+1)
	}
}

// TestRelayoutFollowsTheSelectedCall。换一个调用，就必须换掉模型
// 视角和线上视角所呈现的内容，不然 n/p 就只是摆设。
func TestRelayoutFollowsTheSelectedCall(t *testing.T) {
	cmp := cspComposer(t, viewModel, 100, 24)
	first := strings.Join(cmp.lines, "\n")
	cmp.handle(key{Kind: keyRune, Rune: 'n'})
	second := strings.Join(cmp.lines, "\n")

	if first == second {
		t.Error("n re-rendered the same lines — the Model view is not following the selected call")
	}
	if !strings.Contains(second, "call 2 of") {
		t.Errorf("after n the Model view does not say it is on call 2:\n%s", second)
	}
}

// ---------------------------------------------------------------------------
// frameBytes
// ---------------------------------------------------------------------------

// cspFrameRows 检查帧的外层结构，返回的是已经剥掉了尾部擦除序列的
// h 行正文。
func cspFrameRows(t *testing.T, lines []string, w, h int) []string {
	t.Helper()
	got := frameBytes(lines, w, h)

	if n := strings.Count(got, syncOn); n != 1 {
		t.Fatalf("the frame contains %d synchronised-output BEGIN markers, want 1 — without "+
			"exactly one wrapping the whole frame the terminal is free to paint a half-drawn "+
			"screen", n)
	}
	if n := strings.Count(got, syncOff); n != 1 {
		t.Fatalf("the frame contains %d synchronised-output END markers, want 1 — an unclosed "+
			"one leaves the terminal holding the next frame too", n)
	}
	if n := strings.Count(got, cursorHome); n != 1 {
		t.Fatalf("the frame homes the cursor %d times, want 1 — a frame that homes twice "+
			"overwrites its own first rows", n)
	}
	if !strings.HasPrefix(got, syncOn+cursorHome) {
		t.Fatalf("the frame does not begin with BEGIN-SYNC then cursor-home: %q", cspHead(got))
	}
	if !strings.HasSuffix(got, syncOff) {
		t.Fatalf("the frame does not end with END-SYNC: %q", cspTail(got))
	}

	body := strings.TrimSuffix(strings.TrimPrefix(got, syncOn+cursorHome), syncOff)
	rows := strings.Split(body, "\r\n")
	if len(rows) != h {
		t.Fatalf("the frame has %d rows and %d line separators, want %d of each. One row too "+
			"many and the terminal scrolls, which pushes the whole UI up by a line on every "+
			"single repaint", len(rows), len(rows)-1, h)
	}
	if n := strings.Count(got, clearLine); n != h {
		t.Fatalf("the frame erases %d lines, want %d — a row that is not erased still shows "+
			"the tail of whatever the PREVIOUS frame put there", n, h)
	}
	for i, r := range rows {
		if !strings.HasSuffix(r, clearLine) {
			t.Fatalf("row %d = %q does not end with the erase-line sequence", i, r)
		}
		rows[i] = strings.TrimSuffix(r, clearLine)
	}
	return rows
}

func cspHead(s string) string {
	if len(s) > 24 {
		return s[:24]
	}
	return s
}

func cspTail(s string) string {
	if len(s) > 24 {
		return s[len(s)-24:]
	}
	return s
}

func TestFrameBytesShape(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		w, h  int
	}{
		{"exactly enough lines", []string{"a", "b", "c"}, 10, 3},
		{"one row", []string{"only"}, 10, 1},
		{"fewer lines than rows", []string{"only"}, 20, 5},
		{"no lines at all", nil, 20, 4},
		{"more lines than rows", []string{"a", "b", "c", "d", "e"}, 20, 2},
		{"a tall thin window", []string{"a", "b"}, 1, 40},
		{"an ultrawide window", []string{"a"}, 400, 2},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rows := cspFrameRows(t, c.lines, c.w, c.h)
			for i, r := range rows {
				if n := dispWidth(r); n > c.w {
					t.Errorf("row %d = %q is %d columns in a %d-column window. One column of "+
						"overflow wraps, which pushes every row below it down by one and turns "+
						"a cosmetic bug into a corrupted frame", i, r, n, c.w)
				}
				if i >= len(c.lines) && r != "" {
					t.Errorf("row %d = %q, want empty — there were only %d lines, and the "+
						"tail must be erased rather than left showing the previous frame",
						i, r, len(c.lines))
				}
			}
		})
	}
}

// TestFrameBytesTruncatesInColumnsNotBytes 是损坏整个帧的 bug，
// 以及 term.go 调用 truncCols 而不是切片的原因。
func TestFrameBytesTruncatesInColumnsNotBytes(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		w     int
		wantW int
	}{
		{"plain ASCII overflow", strings.Repeat("x", 200), 10, 10},
		{"CJK, even boundary", "你好世界你好世界", 8, 8},
		// 3 列无法容纳两个宽字形，所以一个适合，孤立列是空格。
		// 字节切片会将 你 切成两半。
		{"CJK, odd boundary", "你好世界", 3, 3},
		{"CJK, nothing fits", "你好", 1, 1},
		{"mixed", "ab你好cd", 4, 4},
		{"coloured overflow", "\x1b[31m" + strings.Repeat("y", 50) + "\x1b[0m", 12, 12},
		{"a wide status line", "总计 4 · exit 0 · 4096B · TRUNCATED", 12, 12},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rows := cspFrameRows(t, []string{c.line}, c.w, 1)
			got := rows[0]

			if n := dispWidth(got); n != c.wantW {
				t.Errorf("frameBytes(%q, %d, 1) row = %q, %d columns, want exactly %d. A row "+
					"one column short shears the frame as badly as one column too long",
					c.line, c.w, got, n, c.wantW)
			}
			if !utf8.ValidString(got) {
				t.Errorf("frameBytes(%q, %d, 1) row = %q is not valid UTF-8 — the cut landed "+
					"inside a multi-byte rune and half a character went to the terminal",
					c.line, c.w, got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("frameBytes(%q, %d, 1) row = %q contains U+FFFD", c.line, c.w, got)
			}
			if got != truncCols(c.line, c.w) {
				t.Errorf("frameBytes row = %q but truncCols(%q, %d) = %q — the frame builder "+
					"is not using the column-aware truncator", got, c.line, c.w, truncCols(c.line, c.w))
			}
			// 反例，拼出来：字节切片会产生什么。
			if len(c.line) >= c.w {
				if byteCut := c.line[:c.w]; byteCut == got && dispWidth(byteCut) != c.wantW {
					t.Errorf("the row equals the BYTE slice %q, which is %d columns",
						byteCut, dispWidth(byteCut))
				}
			}
		})
	}
}

// TestFrameBytesLineCount 单独固定行算术，因为这里的差一错误，
// 截图里看不出来，终端上却扎眼得很：每次重绘，帧都会往下滚一行。
func TestFrameBytesLineCount(t *testing.T) {
	for _, h := range []int{1, 2, 3, 24, 60} {
		got := frameBytes([]string{"a", "b", "c"}, 10, h)
		if n := strings.Count(got, "\r\n"); n != h-1 {
			t.Errorf("frameBytes(..., h=%d) contains %d line separators, want %d — h rows "+
				"need h-1 of them, and the h'th newline is what scrolls the terminal",
				h, n, h-1)
		}
		if n := strings.Count(got, clearLine); n != h {
			t.Errorf("frameBytes(..., h=%d) erases %d lines, want %d", h, n, h)
		}
	}
}

// TestFrameBytesRedrawsOverThePreviousFrame。帧永远不会清屏
// （这就是经典的闪烁成因），所以每个格子都必须要么被重新写过，
// 要么被明确擦除。具体来说：一个长帧后面跟着一个短帧时，
// 不能在屏幕上留下长帧多出来的尾巴。
func TestFrameBytesRedrawsOverThePreviousFrame(t *testing.T) {
	rows := cspFrameRows(t, []string{"the previous frame was taller"}, 30, 6)
	for i := 1; i < len(rows); i++ {
		if rows[i] != "" {
			t.Fatalf("row %d = %q, want empty", i, rows[i])
		}
	}
	// 每个空行依然带着它的擦除序列，这一点由 cspFrameRows 检查；
	// 这里断言的是，帧不会抄提前停止这条近道。
	full := frameBytes([]string{"one"}, 30, 6)
	if strings.Count(full, clearLine) != 6 {
		t.Errorf("a 6-row frame with one line of content erases %d rows, want 6 — the other "+
			"five would still show the previous frame", strings.Count(full, clearLine))
	}
}

// ---------------------------------------------------------------------------
// joinEnds
// ---------------------------------------------------------------------------

// TestJoinEnds。标题的两个部分和页脚的两个部分都通过这个。
// 以字节而不是列测量，右侧落在屏幕中间的某处——而标题里的路径，
// 正是最可能带有 CJK 目录名的那个字符串。
func TestJoinEnds(t *testing.T) {
	cases := []struct {
		name  string
		left  string
		right string
		w     int
		want  string
	}{
		{"plain", "L", "R", 5, "L   R"},
		{"exactly one space of gap", "abc", "de", 6, "abc de"},
		{
			"both sides coloured: the escapes cost no columns",
			"\x1b[1mcomposer\x1b[0m", "\x1b[2m[GOD]\x1b[0m", 20,
			"\x1b[1mcomposer\x1b[0m       \x1b[2m[GOD]\x1b[0m",
		},
		{
			// 记忆 是四列、两个 rune；其余部分是九列，所以左侧宽 13、长 17
			// 字节。如果改用 len() 来填充，间隙就会变成零，整个事情就会退回去
			// 靠"切"来解决。
			"a CJK path on the left is measured in columns",
			"记忆/AGENT.md", "42%", 20,
			"记忆/AGENT.md    42%",
		},
		{
			"CJK on both sides",
			"你好", "世界", 10,
			"你好  世界",
		},
	}
	for _, c := range cases {
		got := joinEnds(c.left, c.right, c.w)
		if got != c.want {
			t.Errorf("joinEnds(%q, %q, %d) = %q, want %q — %s",
				c.left, c.right, c.w, got, c.want, c.name)
		}
		if n := dispWidth(got); n != c.w {
			t.Errorf("joinEnds(%q, %q, %d) is %d columns, want exactly %d — %s. This is the "+
				"header row; a column too many wraps it and shifts the entire frame down one",
				c.left, c.right, c.w, n, c.w, c.name)
		}
		if !strings.HasSuffix(got, c.right) {
			t.Errorf("joinEnds(%q, %q, %d) = %q does not end with the right-hand string, so "+
				"it is not at the right edge", c.left, c.right, c.w, got)
		}
	}
}

// TestJoinEndsWhenItDoesNotFit。狭窄窗口是分割窗格的常见情况，
// 唯一安全的答案是切割。填充到负间隙会恐慌；
// 让它溢出会将标题换到第二行并将整个正文推下来。
func TestJoinEndsWhenItDoesNotFit(t *testing.T) {
	cases := []struct {
		name  string
		left  string
		right string
		w     int
	}{
		{"no room for even one space", "abc", "def", 6},
		{"right alone is wider than the window", "a", "bbbbbbbbbb", 5},
		{"both far too wide", strings.Repeat("a", 40), strings.Repeat("b", 40), 5},
		{"CJK that does not fit", "你好世界", "你好世界", 6},
		{"CJK cut on a wide boundary", "你好世界", "世界", 3},
		{"zero width", "abc", "def", 0},
		{"coloured and too wide", "\x1b[31m" + strings.Repeat("a", 20) + "\x1b[0m", "\x1b[2mxx\x1b[0m", 8},
	}
	for _, c := range cases {
		got := joinEnds(c.left, c.right, c.w)
		if n := dispWidth(got); n > c.w {
			t.Errorf("joinEnds(%q, %q, %d) = %q is %d columns — %s. It overflows the window "+
				"and the terminal wraps it onto a second row",
				c.left, c.right, c.w, got, n, c.name)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("joinEnds(%q, %q, %d) = %q contains a newline", c.left, c.right, c.w, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("joinEnds(%q, %q, %d) = %q is not valid UTF-8", c.left, c.right, c.w, got)
		}
	}
}

// TestDrawnChromeFitsTheWindow 把真正的页眉和页脚，一起送进
// frameBytes，送到那些足以把它们弄坏的尺寸上去。
func TestDrawnChromeFitsTheWindow(t *testing.T) {
	s := cspSession(t)
	for _, w := range []int{1, 3, 10, 40, 200} {
		left := fmt.Sprintf(" %s  %s", bold("composer"), "记忆/traces/2026-08-27/session-3.jsonl")
		right := fmt.Sprintf("%d events · %d calls · %d compactions  [%s] ",
			len(s.Events), len(s.Calls), s.Compactions, bold("GOD"))
		header := joinEnds(left, right, w)
		if n := dispWidth(header); n > w {
			t.Errorf("the header is %d columns in a %d-column window: %q", n, w, header)
		}
		rows := cspFrameRows(t, []string{header, dim(strings.Repeat("─", w))}, w, 2)
		for i, r := range rows {
			if n := dispWidth(r); n > w {
				t.Errorf("chrome row %d is %d columns at width %d: %q", i, n, w, r)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// dumpComposer——无头模式，端到端
// ---------------------------------------------------------------------------

// cspWriteTrace 通过真实的 TraceWriter 记录实例，所以这个测试
// 读到的文件，生成方式跟一次真实会话产生文件的方式一模一样。
func cspWriteTrace(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "2026-08-27", "session-1.jsonl")
	w, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter(%s): %v", path, err)
	}
	for _, e := range cspEvents(t) {
		w.OnEvent(e)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the trace: %v", err)
	}
	return path
}

// TestDumpComposerRendersEveryView。
//
// 一个只能靠按键才能产生输出的用户界面，就是一个没有测试的用户
// 界面，所以 composer 会渲染到一个完全不涉及终端的 io.Writer。
// 这就是那条路：从磁盘上的一个文件，到缓冲区里的字节。
func TestDumpComposerRendersEveryView(t *testing.T) {
	path := cspWriteTrace(t)

	cases := []struct {
		view string
		call int
		want string
	}{
		{"god", 1, "request"},
		{"god", 1, "COMPACT_END"},
		{"god", 1, cspUserText},
		{"model", 1, "call 1 of 3"},
		{"model", 1, cspSystem},
		{"model", 2, "the summarising call"},
		{"model", 3, "compaction(s) happened before this call"},
		{"wire", 1, "on the wire, exactly as sent"},
		{"wire", 1, `"max_tokens"`},
		{"wire", 3, `"cache_control"`},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := dumpComposer(path, c.view, c.call, 120, &buf); err != nil {
			t.Fatalf("dumpComposer(%s, call %d): %v", c.view, c.call, err)
		}
		if buf.Len() == 0 {
			t.Errorf("dumpComposer(%s, call %d) wrote nothing", c.view, c.call)
			continue
		}
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("dumpComposer(%s, call %d) does not contain %q — this is the output a "+
				"reader pipes into less or pastes into a bug report\n%s",
				c.view, c.call, c.want, buf.String())
		}
	}
}

// TestDumpComposerRejectsBadInput。两个失败都到达普通终端上的人，
// 所以两个都必须是错误而不是恐慌或空文件。
func TestDumpComposerRejectsBadInput(t *testing.T) {
	path := cspWriteTrace(t)

	var buf bytes.Buffer
	err := dumpComposer(path, "raw", 1, 80, &buf)
	if err == nil {
		t.Errorf("dumpComposer(view=%q) returned no error — a typo in the flag would print "+
			"nothing and exit 0, which reads as an empty trace", "raw")
	} else if !strings.Contains(err.Error(), "raw") {
		t.Errorf("dumpComposer(view=%q) said %q — the message must name what was typed", "raw", err)
	}
	if buf.Len() != 0 {
		t.Errorf("dumpComposer with an unknown view still wrote %d bytes", buf.Len())
	}

	missing := filepath.Join(t.TempDir(), "no-such-session.jsonl")
	buf.Reset()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("dumpComposer on a missing file panicked: %v — a mistyped path is "+
					"the commonest thing a human does with this command", r)
			}
		}()
		if err := dumpComposer(missing, "god", 1, 80, &buf); err == nil {
			t.Errorf("dumpComposer(%s) returned no error for a file that does not exist", missing)
		}
	}()

	// 超出范围的调用不是错误：视图在屏幕上说出来，
	// 这对于用 n 分页通过的读者来说是更有用的行为。
	buf.Reset()
	if err := dumpComposer(path, "model", 99, 80, &buf); err != nil {
		t.Errorf("dumpComposer(model, call 99) = %v, want the \"no calls\" line instead", err)
	}
	if !strings.Contains(buf.String(), "no calls") {
		t.Errorf("dumpComposer(model, call 99) wrote %q, want a message saying there is no "+
			"such call", buf.String())
	}
}

// cspCanonArgs 把每个 tool_call 的 Args 都重写成一种规范编码。
//
// 它的存在，是因为这个文件**之外**有一个缺陷，而这个缺陷值得在
// 读者会被它绊倒的地方挑明。TraceWriter 用普通的 json.Marshal
// 记录事件，这会把 `<`、`>` 和 `&` 做 HTML 转义——包括在装着请求
// 正文的那个 json.RawMessage 内部，而两个适配器都是特意用
// SetEscapeHTML(false) 编码的。字符串本身没事（解码回来还是对的），
// 但 Anthropic 视图是把工具调用的参数当成 `input` 的**原始**字节
// 来读的，所以记录下来的 `2>&1`，读出来还是 `2>&1`。trace 因此在
// 字节上，和被 POST 出去的内容并不一致，而 Wire 视角——它唯一的
// 承诺就是"逐字节"——显示的是转义之后的样子。
//
// 这里做规范化，是为了让这个测试始终只关心它要测的那个往返，
// 而不是那个 bug；它没有把 bug 藏起来——等 trace.go 不再转义的
// 那天，这个测试还会照样通过，不用改。
func cspCanonArgs(v *wireView) {
	for i := range v.Messages {
		for j := range v.Messages[i].Blocks {
			b := &v.Messages[i].Blocks[j]
			if b.Kind != "tool_call" || b.Args == "" {
				continue
			}
			var parsed any
			if json.Unmarshal([]byte(b.Args), &parsed) != nil {
				continue
			}
			if canon, err := json.Marshal(parsed); err == nil {
				b.Args = string(canon)
			}
		}
	}
}

// TestDumpComposerRoundTripsThroughTheTraceFile。文件就是接口——
// 这是 reload() 和整个无头模式赖以成立的主张——所以从里面读出来的，
// 必须是当初写进去的那个会话。
func TestDumpComposerRoundTripsThroughTheTraceFile(t *testing.T) {
	path := cspWriteTrace(t)

	events, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace(%s): %v", path, err)
	}
	from := indexSession(path, events)
	direct := cspSession(t)

	if len(from.Calls) != len(direct.Calls) {
		t.Errorf("the trace read back has %d calls, the in-memory session has %d — something "+
			"did not survive the file", len(from.Calls), len(direct.Calls))
	}
	if from.Compactions != direct.Compactions {
		t.Errorf("compactions: %d from the file, %d in memory", from.Compactions, direct.Compactions)
	}
	if from.Total != direct.Total {
		t.Errorf("token totals: %+v from the file, %+v in memory", from.Total, direct.Total)
	}
	for i := range from.Calls {
		a := decodeRequest(from.Calls[i].Request)
		b := decodeRequest(direct.Calls[i].Request)
		cspCanonArgs(&a)
		cspCanonArgs(&b)
		a.Bytes, b.Bytes = 0, 0 // 见 cspCanonArgs：转义改变长度
		if !reflect.DeepEqual(a, b) {
			t.Errorf("call %d decodes differently after a round trip through the trace file.\n"+
				"file: %s\nmem:  %s", i+1, cspDump(a), cspDump(b))
		}
	}
}

// TestReloadPicksUpNewEvents。正是 `r`，让 composer 在第二个终端里
// 变成一个实时监控器，全程不需要任何 IPC：文件就是接口。
func TestReloadPicksUpNewEvents(t *testing.T) {
	path := cspWriteTrace(t)
	events, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}

	cmp := &composer{path: path, s: indexSession(path, events), view: viewGod, w: 100, h: 24}
	cmp.relayout()
	before := len(cmp.s.Events)
	cmp.call = len(cmp.s.Calls) - 1

	w, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("reopening the trace: %v", err)
	}
	w.OnEvent(Event{Seq: 1000, Kind: KindNotice, Text: "appended while the viewer was open"})
	if err := w.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	if !cmp.handle(key{Kind: keyRune, Rune: 'r'}) {
		t.Fatal("r quit the program")
	}
	if len(cmp.s.Events) != before+1 {
		t.Errorf("after r the session has %d events, want %d — a viewer that cannot re-read "+
			"a growing trace is a viewer you have to restart to watch a run", len(cmp.s.Events), before+1)
	}
	if !strings.Contains(cmp.note, "+1") {
		t.Errorf("the status line says %q, want it to report how many events arrived", cmp.note)
	}

	// 发现更少调用的重新加载，不能让选择悬空。
	cmp.path = filepath.Join(t.TempDir(), "empty.jsonl")
	if w, err := NewTraceWriter(cmp.path); err == nil {
		w.OnEvent(Event{Seq: 1, Kind: KindNotice, Text: "no calls here"})
		w.Close()
	}
	cmp.handle(key{Kind: keyRune, Rune: 'r'})
	if cmp.call != 0 {
		t.Errorf("after reloading a trace with no calls, call = %d, want 0 — modelView would "+
			"index past the end of a slice that just got shorter", cmp.call)
	}
	if !cmp.handle(key{Kind: keyRune, Rune: 'm'}) {
		t.Fatal("m quit the program after a shrinking reload")
	}

	// 而如果重新加载遇到读不出来的东西，也是照实报告，而不是直接崩掉。
	cmp.path = filepath.Join(t.TempDir(), "gone.jsonl")
	if !cmp.handle(key{Kind: keyRune, Rune: 'r'}) {
		t.Fatal("r quit the program when the trace had vanished")
	}
	if !strings.Contains(cmp.note, "reload failed") {
		t.Errorf("reloading a missing file left the status line at %q", cmp.note)
	}
}
