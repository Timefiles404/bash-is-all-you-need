// 阶段 06 —— 一个会话的三个视图，以及为什么这三者注定不一致。
//
// 一个 trace 里有两个不同的故事，大多数工具只会给你看第一个：
//
//	GOD   发生了什么。每个事件，按顺序，带有它的时序、它的 token
//	      计数、它的退出码和它的权限闸裁决。什么都没有隐藏，
//	      包括从不发送给模型的东西。
//
//	MODEL 模型看到了什么。不是重建出来的——而是**实际字节**：从
//	      阶段 02 打从一开始就在录制的请求事件里解码出来的。
//
//	WIRE  那些字节，未修改，用于当答案在标点中的时候。
//
// 构建这三者的原因就是：前两者之间的差距，正是 Agent bug 藏身的地
// 方。而下面每一条，除非你能把两者并排放在一起看，否则都是看不见
// 的：
//
//   - 模型推理了四百个 token，其中没有一个在下一个请求中，因为思考从
//     历史记录中被删除。
//   - 用户输入了九个单词，模型收到九个单词加一个它从未提到的环境块。
//   - 一个工具打印了 40kB，模型只拿到 8kB，外加一个截断标记。
//   - 三十个回合过去了，模型能看到的却只有三条消息，因为其他二十七条
//     被压缩成了一个段落。
//
// 最后这一条，就是这一章排在阶段 05 之后的全部原因。压缩之后，**发
// 生过的历史，和模型手上的历史，成了两个不同的对象**——一个 Agent
// 一旦开始表现古怪，通常就是因为它的模型视角里，已经不再包含你正在
// 问它的那件事。你不能从聊天日志中调试这个。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 解码一个录制的请求
// ---------------------------------------------------------------------------

// wireBlock 是一条内容，无论哪个协议产生了它。
type wireBlock struct {
	Kind   string // "text" | "thinking" | "tool_call" | "tool_result"
	Text   string
	ID     string
	Name   string
	Args   string
	Cached bool // 这个块携带了一个 cache_control 标记
}

type wireMsg struct {
	Role   string
	Blocks []wireBlock
}

// wireView 是一个请求体，解码得足以阅读。
//
// 有意不建立在适配器自己的结构上。这些类型描述了这个 Agent **发送**的
// 东西；这一个必须能读取一条 trace——不管它是由另一个构建版本录制的，
// 是一次手写请求，还是由一个三个版本前就已经删掉了适配器的协议录制的。
// 一个查看器如果只能解析自己编码器生成的东西，就会在你最需要它的时候
// 失灵——也就是在一次改动之后。
type wireView struct {
	Protocol   string
	Model      string
	MaxTokens  int
	System     []wireBlock
	Messages   []wireMsg
	Tools      []string
	Bytes      int
	CacheMarks int
	Err        string
}

// decodeRequest 嗅探协议特征并解码。
//
// 这种嗅探是结构性的，不是靠版本标头：顶级 `system` 键意味着
// Anthropic 形状，它的缺失则意味着 OpenAI 形状。这正是阶段 03 称为
// **分歧 1** 的那个差异，而它之所以是最可靠的判别依据，恰恰是因为
// 这是唯一一件两个协议都无法模仿、又不因此变成对方的事情。
func decodeRequest(raw json.RawMessage) wireView {
	v := wireView{Bytes: len(raw)}
	var probe struct {
		Model     string          `json:"model"`
		MaxTokens int             `json:"max_tokens"`
		System    json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		v.Err = "not JSON: " + err.Error()
		return v
	}
	v.Model, v.MaxTokens = probe.Model, probe.MaxTokens
	if len(probe.System) > 0 {
		v.Protocol = "anthropic"
		viewAnthropicRequest(raw, &v)
	} else {
		v.Protocol = "openai"
		viewOpenAIRequest(raw, &v)
	}
	for _, b := range v.System {
		if b.Cached {
			v.CacheMarks++
		}
	}
	for _, m := range v.Messages {
		for _, b := range m.Blocks {
			if b.Cached {
				v.CacheMarks++
			}
		}
	}
	return v
}

func viewAnthropicRequest(raw json.RawMessage, v *wireView) {
	var body struct {
		System   []anthropicContent `json:"system"`
		Messages []struct {
			Role    string             `json:"role"`
			Content []anthropicContent `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		v.Err = err.Error()
		return
	}
	conv := func(c anthropicContent) wireBlock {
		b := wireBlock{Text: c.Text, ID: c.ID, Name: c.Name, Cached: c.CacheControl != nil}
		switch c.Type {
		case "tool_use":
			b.Kind, b.Args = "tool_call", string(c.Input)
		case "tool_result":
			b.Kind, b.ID, b.Text = "tool_result", c.ToolUseID, c.Content
		case "thinking":
			b.Kind = "thinking"
		default:
			b.Kind = "text"
		}
		return b
	}
	for _, c := range body.System {
		v.System = append(v.System, conv(c))
	}
	for _, m := range body.Messages {
		wm := wireMsg{Role: m.Role}
		for _, c := range m.Content {
			wm.Blocks = append(wm.Blocks, conv(c))
		}
		v.Messages = append(v.Messages, wm)
	}
	for _, t := range body.Tools {
		v.Tools = append(v.Tools, t.Name)
	}
}

func viewOpenAIRequest(raw json.RawMessage, v *wireView) {
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		v.Err = err.Error()
		return
	}
	for _, m := range body.Messages {
		// 这个协议上，系统提示词就是 messages[0]。把它提到
		// System 字段里，是模型视角能用同一个函数渲染两种协议的原因——
		// 这也是一个关于线上情况的、小小的、诚实的谎言，这就是为什么线上视
		// 角就在隔壁。
		if m.Role == "system" && len(v.Messages) == 0 {
			v.System = append(v.System, wireBlock{Kind: "text", Text: m.Content})
			continue
		}
		wm := wireMsg{Role: m.Role}
		if m.Role == "tool" {
			wm.Role = "user"
			wm.Blocks = append(wm.Blocks, wireBlock{Kind: "tool_result", ID: m.ToolCallID, Text: m.Content})
		} else if m.Content != "" {
			wm.Blocks = append(wm.Blocks, wireBlock{Kind: "text", Text: m.Content})
		}
		for _, c := range m.ToolCalls {
			wm.Blocks = append(wm.Blocks, wireBlock{
				Kind: "tool_call", ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments,
			})
		}
		v.Messages = append(v.Messages, wm)
	}
	for _, t := range body.Tools {
		v.Tools = append(v.Tools, t.Function.Name)
	}
}

// ---------------------------------------------------------------------------
// 会话索引
// ---------------------------------------------------------------------------

// call 是一次模型调用：包括启动它的那个请求，以及会话在下一次调用
// 之前所做的一切。
type call struct {
	Seq     int
	Turn    int
	At      time.Time
	Request json.RawMessage
	Events  []Event // 这个调用在事件流里的那一段，包括请求

	Usage      *Usage
	Compaction bool // 这个调用是汇总者，不是 Agent
}

type session struct {
	Path   string
	Events []Event
	Calls  []call
	Start  time.Time

	// Display 是把增量的连续段合并之后得到的 Events。一个流式响应是一
	// 千个四字符的 text_delta 事件，一个每个事件渲染一行的查看器，就是
	// 一个没人能滚动的查看器。折叠这个动作，**只在这里**做一次，上帝视
	// 角的每一部分——渲染、点击、滚动位置——读取的都是这个切片，因为如
	// 果一个行索引对渲染器来说是一个意思，对点击处理程序来说又是另一个
	// 意思，那这种 bug 就只会在有人真的用鼠标点的时候才会冒出来。
	Display []Event

	Total       Usage
	Compactions int
}

// indexSession 把扁平的事件流切成一个个调用。
//
// 每个调用都始于一个 KindRequest，这是唯一能保证每次调用都会有的事
// 件——哪怕一次调用在它的第一个 token 之前就已经死掉，它也仍然会有
// 这个事件。把索引锚定在失败路径也会产生的东西上，不然你的查看器偏
// 偏会在最值得看的那些会话上，变成一片空白。
func indexSession(path string, events []Event) *session {
	s := &session{Path: path, Events: events}
	if len(events) > 0 {
		s.Start = events[0].T
	}
	inCompaction := false
	for i, e := range events {
		switch e.Kind {
		case KindCompactStart:
			inCompaction = true
		case KindCompactEnd:
			inCompaction = false
			s.Compactions++
		case KindRequest:
			s.Calls = append(s.Calls, call{
				Seq: e.Seq, Turn: e.Turn, At: e.T, Request: e.Request,
				Compaction: inCompaction,
			})
		case KindUsage:
			if e.Usage != nil {
				s.Total = addUsage(s.Total, *e.Usage)
				if n := len(s.Calls); n > 0 {
					s.Calls[n-1].Usage = e.Usage
				}
			}
		}
		if n := len(s.Calls); n > 0 {
			s.Calls[n-1].Events = append(s.Calls[n-1].Events, events[i])
		}
	}
	s.Display = collapseDeltas(events)
	return s
}

// collapseDeltas 把每一段连续的、同类型流式增量，合并成一个事件：它的
// Text 是拼接后的文本，Bytes 是一共到达了多少。
//
// 合并后的事件，保留的是这一段连续增量里**第一个**的 Seq。这个选择对
// 点击处理程序很重要：点击一个折叠后的行，选中的应该是这段连续增量开
// 始的那次调用；一个跨越边界的连续段——这种情况发生在一次响应结束、
// 下一次请求开始的地方——不然就会把你跳到下一个调用去。
func collapseDeltas(events []Event) []Event {
	streaming := func(k Kind) bool {
		return k == KindTextDelta || k == KindReasoningDelta || k == KindToolArgsDelta
	}
	var out []Event
	for i := 0; i < len(events); i++ {
		e := events[i]
		if !streaming(e.Kind) {
			out = append(out, e)
			continue
		}
		var b strings.Builder
		n := 0
		for ; i < len(events) && events[i].Kind == e.Kind; i++ {
			b.WriteString(events[i].Text)
			n++
		}
		i--
		e.Text = b.String()
		e.Bytes = n
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// 渲染。每个视图返回普通行；TUI 做滚动。
// ---------------------------------------------------------------------------

const (
	sDim  = "\x1b[2m"
	sBold = "\x1b[1m"
	sOff  = "\x1b[0m"
	sUser = "\x1b[36m"
	sAsst = "\x1b[32m"
	sWarn = "\x1b[33m"
	sBad  = "\x1b[31m"
	sSys  = "\x1b[35m"
	sSel  = "\x1b[7m" // 选定行的反色
)

func dim(s string) string  { return sDim + s + sOff }
func bold(s string) string { return sBold + s + sOff }

// godView 渲染整个事件流。
func (s *session) godView(w int, selSeq int) ([]string, int) {
	var out []string
	selLine := 0
	for _, e := range s.Display {
		if e.Seq == selSeq {
			selLine = len(out)
		}
		out = append(out, s.godLine(e, w)...)
	}
	return out, selLine
}

func (s *session) godLine(e Event, w int) []string {
	off := e.T.Sub(s.Start).Seconds()
	head := fmt.Sprintf("%5d %7.2fs ", e.Seq, off)

	// 深度沟。这就是一旦 Agent 开始嵌套，上帝视角的**用途**所在：终端渲染
	// 器不得不放弃交错显示并发的子 Agent（看 render.go），而一个可滚动列表
	// 不必，因为它不需要争抢同一个光标。普通输出丢弃的一切，都在这里。
	gutter := strings.Repeat("│ ", e.Depth)

	line := func(style, kind, rest string) []string {
		return []string{dim(head) + dim(gutter) + style + fmt.Sprintf("%-16s", kind) + sOff + " " + rest}
	}

	switch e.Kind {
	case KindUserMessage:
		return line(sUser, "user", bold(oneLine(e.Text, w-32)))
	case KindTurnStart:
		return line(sDim, "turn_start", dim(fmt.Sprintf("turn %d", e.Turn)))
	case KindToolCallStart:
		// 没有专属 case 的话，这里会被渲染成空白：payload 在 ToolID 和 ToolName
		// 里，不在 Text 里。它赚得一行，是因为它和 tool_call_ready 之间的间隙，
		// 正是参数流式传输的地方 —— 这里间隙一长，就说明模型是真的花了时间在写
		// 这一个命令。
		return line(sDim, "tool_call_start", dim(e.ToolName+"  "+e.ToolID))

	case KindTurnEnd:
		return line(sDim, "turn_end", dim("turn "+fmt.Sprint(e.Turn)))

	case KindRequest:
		v := decodeRequest(e.Request)
		return line(sBold, "request", dim(fmt.Sprintf("%s · %d messages · %d cache marks · %s",
			v.Protocol, len(v.Messages), v.CacheMarks, humanBytes(len(e.Request)))))
	case KindFirstToken:
		return line(sDim, "first_token", dim(fmt.Sprintf("TTFT %dms", e.Millis)))
	case KindTextDelta, KindReasoningDelta, KindToolArgsDelta:
		// 一个折叠后的连续段。两个数字都会显示——有多少帧抵达，以及它们总
		// 共带了多少文本——因为这两者的比例，就是这个流本身的形状；如果某
		// 个供应商突然改成每个 token 发一次增量、而不是每个块发一次，这种
		// 变化只有在这里才看得出来，其他任何地方都看不出。
		st := sDim
		if e.Kind == KindTextDelta {
			st = ""
		}
		tag := fmt.Sprintf("%s ×%d", e.Kind, e.Bytes)
		return line(st, tag, dim(oneLine(e.Text, w-32)))
	case KindToolCallReady:
		return line(sAsst, "tool_call", "$ "+oneLine(e.Command, w-34))
	case KindCommandStart:
		// 这里没有专属的 case，于是落到了 default 分支，而 default 打印的是
		// e.Text——但 command_start 的有效负载是放在 e.Command 里的，所以这
		// 一行渲染出来是空的。一个悄悄留下空行的查看器，比干脆省略这个事件
		// 的查看器更糟：看上去像是发生了什么事，却没有任何描述。
		return line(sDim, "command_start", dim(oneLine(e.Command, w-34)))

	case KindGateVerdict:
		st := sDim
		if e.Verdict != "allow" {
			st = sWarn
		}
		why := ""
		if e.Text != "" {
			why = " " + dim(e.Text)
		}
		return line(st, "gate", e.Verdict+why)
	case KindCommandEnd:
		st := sDim
		if e.ExitCode != 0 || e.TimedOut {
			st = sBad
		}
		extra := ""
		if e.Truncated {
			extra = sWarn + " TRUNCATED" + sOff
		}
		return line(st, "command_end", dim(fmt.Sprintf("exit %d · %dms · %s", e.ExitCode, e.Millis, humanBytes(e.Bytes)))+extra)
	case KindToolResult:
		return line(sDim, "tool_result", dim(fmt.Sprintf("%s to model", humanBytes(len(e.Text)))))
	case KindUsage:
		if e.Usage == nil {
			return nil
		}
		u := *e.Usage
		return line(sBold, "usage", dim(fmt.Sprintf("prompt %d (full %d · write %d · read %d) · out %d",
			u.Prompt(), u.Input, u.CacheWrite, u.CacheRead, u.Output)))
	case KindResponseEnd:
		return line(sDim, "response_end", dim(fmt.Sprintf("%s · %dms", e.FinishReason, e.Millis)))
	case KindCompactStart:
		return line(sWarn, "COMPACT_START", sWarn+fmt.Sprintf("%d messages, ~%d tokens — %s", e.MsgsBefore, e.TokensBefore, e.Text)+sOff)
	case KindCompactEnd:
		return line(sWarn, "COMPACT_END", sWarn+fmt.Sprintf("%d → %d messages · ~%d → ~%d tokens · %dms",
			e.MsgsBefore, e.MsgsAfter, e.TokensBefore, e.TokensAfter, e.Millis)+sOff)
	case KindCacheInvalidated:
		return line(sBad, "cache_lost", sBad+oneLine(e.Text, w-34)+sOff)
	case KindSandboxExec:
		// 让一个 shell 命令变得可读的这一行：shell 实际运行的每个程序各占一
		// 行，都在展开之后。一个 pipeline 有其中好几行；一个循环有很多行。这
		// 么大的量，就是终端渲染器把它们藏起来、而 composer 不藏的原因。
		return line(sAsst, "sandbox exec", dim(oneLine(e.Command, w-40)))

	case KindSandboxOpen:
		return line(sDim, "sandbox open", dim(e.Path))

	case KindSandboxBlock:
		return line(sBad, "SANDBOX BLOCK", sBad+oneLine(e.Text, w-40)+sOff)

	case KindSubagentStart:
		return line(sUser, "SUBAGENT →", bold(e.ToolName)+"  "+dim(oneLine(e.Text, w-40)))

	case KindSubagentEnd:
		u := Usage{}
		if e.Usage != nil {
			u = *e.Usage
		}
		// 把"花费"放在"返回"旁边，是因为这个比率正是子 Agent 存在的原因，在
		// trace 里的其他地方都看不到。
		return line(sUser, "SUBAGENT ←", dim(fmt.Sprintf("%s · %d turns · %d prompt + %d out · %dms → %s returned",
			e.ToolName, e.Turn, u.Prompt(), u.Output, e.Millis, humanBytes(e.Bytes))))

	case KindSkillsIndexed:
		return line(sSys, "skills", dim(fmt.Sprintf("%s · index %s per request · %s of bodies on disk",
			e.Text, humanBytes(e.Bytes), humanBytes(e.TokensBefore))))

	case KindMemoryLoaded:
		return line(sSys, "memory", dim(fmt.Sprintf("%s (%s)", e.Path, humanBytes(e.Bytes))))
	case KindNotice:
		return line(sWarn, "notice", e.Text)
	case KindError:
		return line(sBad, "error", sBad+e.Text+sOff)
	}
	return line(sDim, string(e.Kind), dim(oneLine(e.Text, w-32)))
}

// modelView 渲染模型在某一次调用上看到的东西。
//
// 标头是整章的重点：它把"迄今为止的事件数"和"模型能看到的消息
// 数"并排放在同一行上。压缩之前，这些数字会一起上升；而压缩过后，
// 它们就永远分道扬镳了，这个差距，就是一场长 Agent 会话里最有用的
// 一个数字。
func (s *session) modelView(idx, w int) []string {
	if idx < 0 || idx >= len(s.Calls) {
		return []string{dim("  no calls in this trace")}
	}
	c := s.Calls[idx]
	v := decodeRequest(c.Request)

	eventsSoFar := 0
	for _, e := range s.Events {
		if e.Seq <= c.Seq {
			eventsSoFar++
		}
	}
	compactionsBefore := 0
	for _, e := range s.Events {
		if e.Seq < c.Seq && e.Kind == KindCompactEnd {
			compactionsBefore++
		}
	}

	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }

	title := fmt.Sprintf("call %d of %d", idx+1, len(s.Calls))
	if c.Compaction {
		title += sWarn + "  [the summarising call, not the agent]" + sOff
	}
	add("  %s   %s", bold(title), dim(fmt.Sprintf("%s · %s · max_tokens %d · %s",
		v.Protocol, v.Model, v.MaxTokens, humanBytes(v.Bytes))))
	add("  %s", dim(fmt.Sprintf("%d events happened so far · the model can see %d messages · %d cache marks · tools: %s",
		eventsSoFar, len(v.Messages), v.CacheMarks, strings.Join(v.Tools, ","))))
	if compactionsBefore > 0 {
		add("  %s", sWarn+fmt.Sprintf("⚠ %d compaction(s) happened before this call: everything below is what SURVIVED, not what happened", compactionsBefore)+sOff)
	}
	if v.Err != "" {
		add("  %s", sBad+"could not decode this request: "+v.Err+sOff)
		return out
	}
	add("")

	if len(v.System) > 0 {
		n := 0
		for _, b := range v.System {
			n += len(b.Text)
		}
		add("  %s %s", sSys+bold("SYSTEM")+sOff, dim(fmt.Sprintf("%d chars", n)))
		for _, b := range v.System {
			out = append(out, blockLines(b, w, "  │ ")...)
			if b.Cached {
				add("  %s", sWarn+"└──◀ cache breakpoint"+sOff)
			}
		}
		add("")
	}

	for i, m := range v.Messages {
		style := sUser
		if m.Role == "assistant" {
			style = sAsst
		}
		add("  %s %s", style+bold(fmt.Sprintf("[%d] %s", i+1, m.Role))+sOff,
			dim(fmt.Sprintf("%d blocks", len(m.Blocks))))
		for _, b := range m.Blocks {
			out = append(out, blockLines(b, w, "  │ ")...)
			if b.Cached {
				add("  %s", sWarn+"└──◀ cache breakpoint (rolling)"+sOff)
			}
		}
		add("")
	}
	return out
}

// blockLines 渲染一个内容块，按窗口宽度换行。
func blockLines(b wireBlock, w int, prefix string) []string {
	var out []string
	body := func(s string) {
		for _, l := range wrapCols(strings.TrimRight(s, "\n"), max(20, w-len(prefix)-2)) {
			out = append(out, dim(prefix)+l)
		}
	}
	switch b.Kind {
	case "tool_call":
		cmd, err := parseBashArgs(b.Args)
		if err != nil {
			cmd = b.Args
		}
		out = append(out, dim(prefix)+sAsst+"→ "+b.Name+sOff+"  "+dim(b.ID))
		body("$ " + cmd)
	case "tool_result":
		out = append(out, dim(prefix)+sUser+"← result"+sOff+"  "+dim(b.ID))
		body(b.Text)
	case "thinking":
		out = append(out, dim(prefix)+dim("· thinking"))
		body(b.Text)
	default:
		body(b.Text)
	}
	return out
}

// wireLines 格式化打印原始请求体。
func (s *session) wireView(idx, w int) []string {
	if idx < 0 || idx >= len(s.Calls) {
		return []string{dim("  no calls in this trace")}
	}
	c := s.Calls[idx]
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, c.Request, "", "  "); err != nil {
		return []string{sBad + "not valid JSON: " + err.Error() + sOff, string(c.Request)}
	}
	out := []string{
		"  " + bold(fmt.Sprintf("call %d of %d", idx+1, len(s.Calls))) + "   " +
			dim(fmt.Sprintf("%s on the wire, exactly as sent", humanBytes(len(c.Request)))),
		"",
	}
	for _, l := range strings.Split(pretty.String(), "\n") {
		// 换行，而不是截断：在这个视图里，你最常想读到的，就是挤在一行里
		// 的 30kB 系统提示词；一个在窗口边缘把它截断的查看器，就是一个把
		// 答案藏起来的查看器。
		out = append(out, wrapCols("  "+l, max(20, w-2))...)
	}
	return out
}

// oneLine 展平空白，好让一个多行的值，能塞进上帝视角的一行里。换
// 行符会变成 ⏎ 而不是直接消失，因为"这个字符串里有换行符"这件事本
// 身，往往就是 bug 所在。
func oneLine(s string, w int) string {
	s = strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", dim("⏎ "))
	if w < 8 {
		w = 8
	}
	return truncCols(s, w)
}
