// 阶段 06——一场会话的三种视角，以及它们为什么必须互相打架。
//
// 一份 trace 里装着两个不同的故事，而大多数工具只让你看见第一个：
//
//	上帝视角  发生了什么。每一个事件，按顺序，带上它的时间、token 计数、
//	          退出码和权限闸裁决。什么都不藏，包括那些从来没有发给模型
//	          的东西。
//
//	模型视角  模型看见了什么。不是重建出来的——是**真正的字节**，从阶段
//	          02 起就一直在记录的 request 事件里解出来。
//
//	线上视角  同一批字节，原封不动，留给答案藏在标点里的时候。
//
// 三个都要做，是因为前两个之间的落差正是 Agent 的 bug 藏身的地方。下面每一
// 条，不把两边并排放在一起就看不见：
//
//   - 模型推理了四百个 token，一个字都没进下一次请求，因为 thinking 在历史
//     里被丢掉了。
//   - 用户敲了九个词，模型收到的是九个词外加一整块它从没提过的环境信息。
//   - 工具打印了 40kB，模型拿到的是 8kB 外加一个截断标记。
//   - 发生了三十个回合，模型只能看见三条消息，因为另外二十七个被压缩成了
//     一段话。
//
// 最后这条，就是本章排在阶段 05 后面的全部理由。压缩之后，**真实发生过的
// 历史和模型手里的历史，是两个不同的东西**，而 Agent 开始表现古怪，通常就
// 是因为它的模型视角里已经没有你正在问的那个东西了。这种事，看聊天记录是
// 查不出来的。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 解码一份记录下来的请求
// ---------------------------------------------------------------------------

// wireBlock 是一块内容，不管它出自哪个协议。
type wireBlock struct {
	Kind   string // "text" | "thinking" | "tool_call" | "tool_result"
	Text   string
	ID     string
	Name   string
	Args   string
	Cached bool // 这块内容带了 cache_control 标记
}

type wireMsg struct {
	Role   string
	Blocks []wireBlock
}

// wireView 是一份请求体，解到能读为止。
//
// 故意不搭在几个适配器自己的结构体上。那些类型描述的是这个 Agent **发出
// 去**的东西；而这里要能读另一个构建版本录下的 trace，能读手写的请求，还
// 能读适配器三个版本前就被删掉的协议。查看器要是只认自己的编码器吐出来的
// 东西，那它恰好会在你需要它的时候罢工——也就是改动之后。
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

// decodeRequest 嗅出协议，然后解码。
//
// 嗅探靠的是结构，不是版本号：顶层有 `system` 键就是 Anthropic 那套形状，
// 没有就是 OpenAI 那套。这正是阶段 03 里叫做**分歧 1** 的那处差别，而它之
// 所以是最可靠的判别依据，恰恰因为它是两个协议谁都没法模仿、一模仿就变成
// 对方的那一件事。
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
		// 在这个协议上，系统提示词就是 messages[0]。把它提到 System 字段
		// 里，模型视角才能用一个函数渲染两种协议——这是对线上格式撒的一个
		// 小小的、诚实的谎，隔壁的线上视角就是为它而存在的。
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

// call 是一次模型调用：发起它的那个请求，加上到下一次调用之前会话做的
// 一切。
type call struct {
	Seq     int
	Turn    int
	At      time.Time
	Request json.RawMessage
	Events  []Event // 这次调用在事件流里占的那一段，含 request

	Usage      *Usage
	Compaction bool // 这次调用是那个做总结的，不是 Agent 本身
}

type session struct {
	Path   string
	Events []Event
	Calls  []call
	Start  time.Time

	// Display 就是把连续的 delta 合并之后的 Events。一次流式响应是一千个
	// text_delta 事件、每个四个字符，而查看器要是一个事件渲染一行，就没人
	// 滚得动了。合并**只做一次**，就在这里；上帝视角的每一处——渲染、点
	// 击、滚动位置——读的都是这个切片，因为同一个行号在渲染器眼里是一回
	// 事、在点击处理那里是另一回事，这种 bug 只有等人动鼠标的时候才冒出来。
	Display []Event

	Total       Usage
	Compactions int
}

// indexSession 把扁平的事件流切成一次次调用。
//
// 每次调用都从一个 KindRequest 开始，那是唯一一个保证存在的事件——调用就
// 算在第一个 token 之前就死了，它也有。索引要锚在失败路径同样会产出的东西
// 上，否则你的查看器偏偏就在最值得看的那些会话上一片空白。
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

// collapseDeltas 把每一串同类型的流式 delta 合成一个事件，Text 是拼起来的
// 文本，Bytes 是到了多少条。
//
// 合并出来的事件保留这一串里**第一个** delta 的 Seq。这个选择对点击处理很
// 要紧：点一行合并后的行，该选中的是这一串开始时所在的那次调用；而横跨边
// 界的一串——响应结束、下一个请求开始的时候就会发生——否则会把你往前甩一
// 次调用。
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
// 渲染。每个视角都只返回纯粹的行；滚动交给 TUI。
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
	sSel  = "\x1b[7m" // 选中行用反显
)

func dim(s string) string  { return sDim + s + sOff }
func bold(s string) string { return sBold + s + sOff }

// godView 渲染整条事件流。
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
	line := func(style, kind, rest string) []string {
		return []string{dim(head) + style + fmt.Sprintf("%-16s", kind) + sOff + " " + rest}
	}

	switch e.Kind {
	case KindUserMessage:
		return line(sUser, "user", bold(oneLine(e.Text, w-32)))
	case KindTurnStart:
		return []string{dim(head) + dim(fmt.Sprintf("%-16s turn %d", "turn_start", e.Turn))}
	case KindRequest:
		v := decodeRequest(e.Request)
		return line(sBold, "request", dim(fmt.Sprintf("%s · %d messages · %d cache marks · %s",
			v.Protocol, len(v.Messages), v.CacheMarks, humanBytes(len(e.Request)))))
	case KindFirstToken:
		return line(sDim, "first_token", dim(fmt.Sprintf("TTFT %dms", e.Millis)))
	case KindTextDelta, KindReasoningDelta, KindToolArgsDelta:
		// 合并后的一串 delta。两个数都要显示——来了多少帧，它们带了多少
		// 文本——因为两者的比值就是这条流的形状；供应商哪天突然改成一个
		// token 一条 delta、而不是一块一条，只有在这里看得见。
		st := sDim
		if e.Kind == KindTextDelta {
			st = ""
		}
		tag := fmt.Sprintf("%s ×%d", e.Kind, e.Bytes)
		return []string{dim(head) + st + fmt.Sprintf("%-16s", tag) + sOff + " " + dim(oneLine(e.Text, w-32))}
	case KindToolCallReady:
		return line(sAsst, "tool_call", "$ "+oneLine(e.Command, w-34))
	case KindCommandStart:
		// 没有自己的 case，它就掉到 default 里去了，而 default 打的是
		// e.Text——可 command_start 的内容装在 e.Command 里，于是这行渲
		// 染出来是空的。查看器里有一行不声不响地空着，比干脆不显示这个
		// 事件更糟：看上去像是有什么事发生了，却没有任何说明。
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
	case KindMemoryLoaded:
		return line(sSys, "memory", dim(fmt.Sprintf("%s (%s)", e.Path, humanBytes(e.Bytes))))
	case KindNotice:
		return line(sWarn, "notice", e.Text)
	case KindError:
		return line(sBad, "error", sBad+e.Text+sOff)
	}
	return line(sDim, string(e.Kind), dim(oneLine(e.Text, w-32)))
}

// modelView 渲染模型在某一次调用里看见的东西。
//
// 那行头部就是整章的要害：它把"到目前为止发生了多少事件"和"模型能看见多少
// 条消息"摆在同一行上。压缩之前，这两个数一起涨。压缩之后，它们就永久地分
// 道扬镳了，而这个背离，是一场很长的 Agent 会话里最有用的那个数。
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

// blockLines 渲染一个内容块，按窗口宽度折行。
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

// wireLines 把原始请求体漂亮地打出来。
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
		// 折行，不是截断：挤在一行里的 30kB 系统提示词，正是在这个视角下
		// 最常想读的东西；查看器要是在窗口边上把它切掉，那就是在藏答案。
		out = append(out, wrapCols("  "+l, max(20, w-2))...)
	}
	return out
}

// oneLine 把空白压平，好让多行的值塞进上帝视角的一行里。换行变成 ⏎ 而不是
// 消失，因为"这个字符串里有换行"本身经常就是那个 bug。
func oneLine(s string, w int) string {
	s = strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", dim("⏎ "))
	if w < 8 {
		w = 8
	}
	return truncCols(s, w)
}
