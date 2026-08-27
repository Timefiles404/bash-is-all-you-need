// 阶段 03——OpenAI 协议适配器。
//
// 这个文件中的一切都是一个厂商关于对话如何
// 形成的意见：系统提示词是消息，工具结果是消息，
// 工具参数是嵌套在 JSON 中的 JSON 字符串，工具定义
// 在 `function` 下一层。那些都不是关于语言模型的事实。
// 它们是这个线上的事实，它们被隔离在这里，
// 在 provider.go 中的 Provider 接口后面，
// 所以 agent 主循环永不学到它们中的任何一个。
//
// 解析一半是从阶段 02 的 sse.go 里挖出来的；它曾经
// 紧挨着的 SSE 分帧逻辑，现在住在 sse.go 里，
// 对这些内容一无所知。解析器在这次搬迁中什么都
// 没变，除了它的返回类型——它当初围绕的那些
// 观察行为（§B4 frames 11 和 13、id 锁存、未对齐
// 的参数片段）还是同样的行为，解释它们的注释
// 也还是同样的注释。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 供应商
// ---------------------------------------------------------------------------

// openaiProvider 保持三件事，在说这个协议的
// 端点之间变化。这里没有厂商 SDK，也不需要在任何
// 特定厂商那里开户：一个本地 llama.cpp 服务器、一个网关、
// 以及 OpenAI 本身按 URL 和模型字符串不同。
type openaiProvider struct {
	baseURL string
	apiKey  string
	model   string
}

func newOpenAIProvider(baseURL, apiKey, model string) *openaiProvider {
	return &openaiProvider{
		// 这里也被修剪，因为一个直接在测试中构造的
		// 供应商否则就会 POST 到 `.../v1//chat/completions`
		// ——某些服务器路由它，某些 404，所以 bug 仅在
		// 你没有测试的端点上出现。
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
	}
}

// 编译时证明这个文件遵守 provider.go 的约定。
// 没有它，签名偏差的第一个证据就是 config.go
// 的 switch 内部的一次构建失败，指向错误的文件。
var _ Provider = (*openaiProvider)(nil)

func (p *openaiProvider) Protocol() string { return "openai" }
func (p *openaiProvider) Model() string    { return p.model }

// ---------------------------------------------------------------------------
// 请求：中立对话，呈现成这个厂商的形状
// ---------------------------------------------------------------------------

// oaiMessage 是 `messages` 中的一个条目。
//
// 这些类型上的 `oai` 前缀不是装饰。anthropic.go 在
// 相同包中声明相同概念，一个裸 `message` 类型就意味着
// 两个适配器抢占同一个名字——这正是这整个阶段所要
// 防止的那类错误，只是发生在文件这一层。
type oaiMessage struct {
	Role string `json:"role"`

	// 当空时 Content 被省略而不是作为 null 发送。
	// 那是阶段 02 的发送行为，故意保留：一条除了
	// 工具调用之外什么都没有的助手消息是没有 content
	// 的，这个端点也接受 content 缺失。它唯一无法
	// 表示的形状，是一个**故意的**空工具结果——在实践
	// 中 exec.go 总是追加一个 `[exit N]` 页脚，所以
	// 空结果永远到不了这里。
	Content string `json:"content,omitempty"`

	// ToolCalls 仅在被重放的助手消息上设置。
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`

	// ToolCallID 仅在 `role:"tool"` 消息上设置，
	// 它是这个协议上的整个寻址机制：结果命名调用。
	// 在流解析器中丢失 id，答案无处可去。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`

		// Arguments 是一个包含 JSON 的 JSON **字符串**——
		// 标准 OpenAI 双编码（§A2 在响应端显示它逐字）。
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

type oaiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	Messages  []oaiMessage `json:"messages"`
	Tools     []oaiToolDef `json:"tools,omitempty"`
	Stream    bool         `json:"stream"`

	// 没有这个标记，真正的 OpenAI 端点是不会流式
	// 返回 usage 的。这个 repo 开发时所针对的网关
	// 无论有没有这个标记都会发送 usage——见
	// docs/wire-notes.md §B5，那里这个标记**可测量地**
	// 是个空操作：相同的 13 个 frame、相同位置、
	// 相同字段，有它没它都一样。还是要发送它：
	// 这不费什么成本，不发的代价是——某天有人把
	// Agent 指向另一个供应商时，它会报告零 token。
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// BuildRequest 将中立对话呈现到这个线上。
//
// 它返回 marshal 后的 body 以及请求，因为调用方
// 以 KindRequest 发出它，而请求检查器只有在显示
// 实际发送的字节时才算诚实——不是同一个结构体的
// 重新 marshal，那可能会不一样。
//
// 四个翻译在这个路径上发生——三个下面以及
// 第四个在 assistantMessage——每一个都是
// 两个协议不同的地方。它们在它们发生的地方
// 被指出，而不是在顶部列成清单，因为这些差异
// 本身就是这一章要讲的内容。
func (p *openaiProvider) BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	out := make([]oaiMessage, 0, len(msgs)+1)

	// **不同意 1**——系统提示词住在哪里。
	//
	// 这里它只是另一条消息，数组中首先，角色"系统"。
	// 在 Anthropic 协议上它是顶级 `system` 字段，
	// 根本不能是消息。这不对称是为什么
	// Provider.BuildRequest 把系统提示词作为它自己的
	// 参数：两个位置都不能是中立的，所以中立形式
	// 拒绝选择。
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: system})
	}

	for _, m := range msgs {
		if m.Role == RoleAssistant {
			out = append(out, p.assistantMessage(m))
			continue
		}

		// **不同意 2**——工具结果如何被寻址。
		//
		// 每个结果变成它**自己的**消息，`role:"tool"`，
		// 命名它回答的调用。三个结果，三条消息。
		// Anthropic 协议把同样这三个折叠进**一个**用户
		// 消息里的 tool_result 块中，谁把顺序弄反了，
		// 两边的 API 都会报错。
		//
		// 这正是为什么 provider.go 没有 RoleTool：
		// 选择任何一种形状作为中立形式，都会把某个
		// 厂商的设计走私进核心里，所以工具结果是一个
		// **块**，由适配器决定用什么消息携带它。
		sawText := false
		var text strings.Builder
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolResult:
				out = append(out, oaiMessage{
					Role:       "tool",
					ToolCallID: b.ID,
					Content:    b.Text,
				})
			case BlockText:
				sawText = true
				text.WriteString(b.Text)
			}
			// BlockThinking 在发送出去的路上被丢弃。这个协议上
			// 没有入站字段——`reasoning_content` 是
			// 仅响应——所以重放它要么会被忽略，要么会被拒绝，
			// 这取决于谁的实现在远端。
		}
		if sawText {
			out = append(out, oaiMessage{Role: string(m.Role), Content: text.String()})
		}
	}

	// **不同意 3**——工具定义信封。
	//
	// 这里 schema 被埋在 `{"type":"function","function":{...}}`
	// 下，schema 键被称为 `parameters`。Anthropic
	// 协议把 name/description 放在顶层，把 schema 叫作
	// `input_schema`。中立的 Tool 结构体两种信封都不携带，
	// 这也是唯一一张工具表能同时服务两边的原因。
	var defs []oaiToolDef
	for _, t := range tools {
		var d oaiToolDef
		d.Type = "function"
		d.Function.Name = t.Name
		d.Function.Description = t.Description
		d.Function.Parameters = t.Schema
		defs = append(defs, d)
	}

	// 用 HTML 转义**关闭**编码，匹配 anthropic.go。
	//
	// Go 的 json.Marshal 把 <、> 和 & 转义成
	// \u003c、\u003e 和 \u0026——这是一个对 shell
	// Agent 实打实敌意的浏览器安全默认设置，因为
	// 那三个字符正是 `2>&1`、`>/tmp/out` 和
	// `<<EOF`。一条真实命令会变成：
	//
	//	{"command":"grep -rn 'x' . 2\u003e\u00261 | head -5 \u003e/tmp/out"}
	//
	// 服务器会解码它，所以模型无论哪种方式读到的
	// 都是同一个字符串。这四行仍然值得保留，理由
	// 有两个。请求检查器的意义就是向你展示你实际
	// 发送了什么，而转义后的内容是不可读的。另外，
	// 转义是否会改变供应商的缓存键，取决于它是对
	// 原始字节还是解码后内容做哈希——我们不知道
	// 答案，这正是选择保持一致而不是去猜的理由。
	//
	// 一致性才是真正的论据：两个适配器为同一段
	// 对话发出不同的字节，是一个疣，而本章讲的
	// 正是把恰恰这一类差异给规范化掉。
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(oaiRequest{
		Model:         p.model,
		MaxTokens:     maxTokens,
		Messages:      out,
		Tools:         defs,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
	}); err != nil {
		return nil, nil, err
	}
	// Encoder.Encode 追加 Marshal 不做的换行。
	// 对服务器无害，但它会显示在检查器和每个 trace 中。
	body := bytes.TrimRight(buf.Bytes(), "\n")

	req, err := http.NewRequest("POST", p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	return req, body, nil
}

// assistantMessage 重建的是 API 在非流式情况下
// 本该返回的那条消息，因为这正是必须写回历史
// 记录的东西。重新组装是为流式付出的税，忘记
// 这一点，就是流式 Agent 会"丢失"工具调用的原因。
func (p *openaiProvider) assistantMessage(m Msg) oaiMessage {
	am := oaiMessage{Role: "assistant", Content: m.Text()}
	for _, b := range m.Blocks {
		if b.Kind != BlockToolCall {
			continue
		}
		var call oaiToolCall
		call.ID, call.Type = b.ID, "function"
		call.Function.Name = b.Name

		// **不同意 4**——`arguments` 的类型，以及
		// Block.Args 为何是一个原始字符串。
		//
		// 这个协议想要一个包含 JSON 的 JSON 字符串，
		// 这正是流解析器累积出来的东西，所以字节
		// 原封不动地直接通过。Anthropic 端则想要
		// 相同的数据作为 JSON **对象**，必须对它们
		// 做 unmarshal。
		//
		// 把中立形式存成解码后的 map，会让这一端
		// 每个回合都要重新序列化，而 Go 故意把 map
		// 的迭代顺序随机化——于是同一个工具调用
		// 每次都会产生不同的字节，击穿字节级的 prompt
		// 缓存（§C9：9,815 个 token 里有 9,792 个
		// 是从缓存提供的，全都依赖精确的前缀匹配），
		// 还会破坏任何格式化方式很重要的参数值。
		call.Function.Arguments = b.Args

		am.ToolCalls = append(am.ToolCalls, call)
	}
	return am
}

// ---------------------------------------------------------------------------
// 响应：流式块 schema
// ---------------------------------------------------------------------------

// sseChunk 是 OpenAI 协议上的一个 `data:` 载荷。
//
// 关于这些结构体，最重要的一件事是：在这个端点，每个字段都被显式
// 地发出为 `null` 而不是省略（§B4）。Go 的解码器把 `null` 转成字符串
// 的零值、切片的 nil 和结构体的 no-op——默默无声，没有错误。那正是
// 我们想要的，也正是陷阱：这里"键在场"根本说不了什么。测试值。
// 下面的每个检查都测试一个值。
type sseChunk struct {
	// Choices 在 usage 帧（§B4 帧 11）和 DONE 之后的 cost 帧
	// （帧 13）上是**空的**。这里是这个文件最可能藏 bug 的地方：
	// `chunk.Choices[0]` 读起来毫无问题，能通过每一个 happy path
	// 测试，然后在每个真实请求的倒数第二帧上以 index-out-of-range
	// panic。下面那个循环用的是 `range`，那就是修法。
	Choices []sseChoice `json:"choices"`

	// Usage 是指针，所以"不存在/null"和"存在但全是零"保持可区分。
	// 零 token 响应是一种合法的、可以正当报告的情况。
	Usage *sseUsage `json:"usage"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	FinishReason string   `json:"finish_reason"` // 除最后一个以外每个块都是 null
	Delta        sseDelta `json:"delta"`
}

// sseDelta 是增量载荷。注意推理**不是**这个协议上的独立事件或块
// 类型——它搭在同一个对象里的相邻字段上，只通过两个中哪个
// 非 null 来区分（§B7）。在那里记录的运行中，44 帧携带了 reasoning_content
// 和 1 帧携带了 content。
type sseDelta struct {
	Role             string             `json:"role"`              // 开启端是"assistant"，之后是 null
	Content          string             `json:"content"`           // 开启端是""，大多数块上是 null
	ReasoningContent string             `json:"reasoning_content"` // §B7：思考在这里到达
	ToolCalls        []sseToolCallDelta `json:"tool_calls"`
}

type sseToolCallDelta struct {
	// Index 是助手消息的 tool_calls 数组中的位置，它是把片段和它所属的
	// 调用绑在一起的**唯一**依据。并行工具调用会交错它们的片段；按别的
	// 什么累积，就会把一个调用的参数拼接进另一个调用里。
	Index int `json:"index"`

	// ID 和 Function.Name 在**恰好一个**块中到达，在它之后的每个块中都
	// 是 null（§B4 帧 2 对比帧 3–9）。第一眼就锁定它们。
	ID       string `json:"id"`
	Type     string `json:"type"` // 始终是"function"；不是 null，也不是信号
	Function struct {
		Name string `json:"name"`

		// Arguments 片段**不是** JSON 对齐的。§B4 观察了分裂
		// `{"command": ` / `"` / `ls` / ` -la /srv` / `/app` / `"` / `}`——
		// 中间 token 和中间路径。片段在任何时点上都不是可解析的 JSON，
		// 所以这被累积为原始字符串，在流结束后恰好被调用者解析一次。
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// sseUsage 是 OpenAI 的 token 记账，在 OpenAI 的方向。
type sseUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// normalise 转换成这个仓库的 Usage，转换是方向反转，不是重命名。
//
// 线上（§B4 帧 11）：
//
//	"prompt_tokens": 506, "prompt_tokens_details": {"cached_tokens": 192}
//
// 506 是**完整的 prompt。192 个缓存 token 被计入其中**。
//
// 这个仓库的 Usage.Input 意思是"按满价计费"（见 events.go），所以
// 缓存部分得**出来**：
//
//	Input = 506 - 192 = 314   CacheRead = 192   →   Prompt() = 506 ✓
//
// 把字段不变地复制过来，Usage.Prompt() 对一个 506 token 的 prompt
// 报告 698。错误恰好是缓存命中的大小，所以它在冷首次请求上是零：
// 测试时看起来完美，你的缓存工作得越好它变得越差。那就是这个是
// 函数而不是结构体标签的全部原因。
//
// Anthropic 一侧再反转一次——那里 `input_tokens` 只是未缓存的
// 余量，所以它直接映射到 Input，没有减法。两个协议，相反的约定，
// 一个规范化的结构体，这正是要有一个规范化结构体的理由所在。
func (u sseUsage) normalise() Usage {
	cached := u.PromptTokensDetails.CachedTokens

	// 限制而不是信任。负的 Input 会传播到 Prompt() 和任何建立在它上
	// 的成本估计；如果端点哪天报告比 prompt token 更多的缓存 token，
	// 丢掉这个差值，好过导出一个负的 token 计数。
	input := u.PromptTokens - cached
	if input < 0 {
		input = 0
	}

	return Usage{
		Input:     input,
		CacheRead: cached,
		Output:    u.CompletionTokens,
		// 是 Output 的一个子集，不是外加的东西——§B4 报告这里是 0，因为那个运行使
		// 用了 reasoning_effort:"none"。
		Reasoning: u.CompletionTokensDetails.ReasoningTokens,
		// CacheWrite 一直是 0：这个协议的缓存是隐式的，线上根本没有"写入"
		// 这个数字。它是零，不是因为什么都没缓存，而是因为这个概念不上报。
	}
}

// sseToolAccum 是一个工具调用的飞行中状态。它不是返回的形状因为
// 它持有两个调用者永远不该看到的东西：构建器和启动事件是否已经
// 出去了。
type sseToolAccum struct {
	index     int
	id        string
	name      string
	args      strings.Builder
	announced bool // 已为这个索引发出 KindToolCallStart
}

// ParseStream 消耗 OpenAI 协议 SSE body，
// 在事件到达时发出它们到 bus，并在中立形状返回
// 已组装结果。
//
// `started` 是请求出去时，不是这个函数被调用时
// ——TTFT 是往返的属性，从响应 header 到达
// 的时刻测量隐藏你试图看的整个延迟。
//
// 遇到流式传输中途的 I/O 故障时，这个函数会
// 返回部分结果**和**错误——这是故意打破通常的
// `return nil, err` 惯例。一个在完整工具调用之后
// 中断的流，和一个什么都没产生的流，是两种不同
// 的情况，调用方只有在拿到了确实到达的内容时，
// 才能把两者区分开。调用方仍然必须检查错误——
// 没有 finish_reason 的部分结果就是一次截断，
// 阶段 01 整整一章讲的就是截断如果没被发现
// 会发生什么。
func (p *openaiProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	res := &CallResult{}

	// emit 在每个事件上戳上回合号，所以没有调用点能忘记它，并且
	// 容忍 nil bus，好让解析器可以当纯函数使用。
	emit := func(e Event) {
		if bus == nil {
			return
		}
		e.Turn = turn
		bus.Emit(e)
	}

	var (
		text      strings.Builder
		reasoning strings.Builder
		calls     = map[int]*sseToolAccum{}
		firstSeen bool
	)

	// markFirstToken 只触发一次，在模型真正输出的第一个字节上。
	//
	// role 开启帧（§B4 帧 1）刻意不算：它带的是 `content: ""`，没有任何
	// 载荷。把它算进去，TTFT 就变成了 time-to-first-byte —— 而在一个开口
	// 之前先想四秒的模型上，那是个好看却毫无意义的数字。文本、推理和工具
	// 调用结构都算，尤其是推理：在思考型模型上，它确实是最先生成的东西。
	markFirstToken := func() {
		if firstSeen {
			return
		}
		firstSeen = true
		res.TTFT = time.Since(started)
		emit(Event{Kind: KindFirstToken, Millis: res.TTFT.Milliseconds()})
	}

	err := readSSE(r, func(f sseFrame) error {
		payload := strings.TrimSpace(f.Data)
		if payload == "" {
			return nil
		}
		if payload == sseDoneSentinel {
			// 跳过它，继续读。见 sseDoneSentinel 了解为什么。
			return nil
		}

		var c sseChunk
		if jerr := json.Unmarshal([]byte(payload), &c); jerr != nil {
			// 一个格式错误的帧不应该摧毁一个已经产生有效工具调用的回合。
			// 作为通知呈现它——在 trace 中可见，在主循环中存活——并继续。
			// 在这里返回错误，是那个看起来更整洁、实际更差的选择。
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("skipped an SSE frame that was not JSON: %v (%.120s)", jerr, payload)})
			return nil
		}

		// range 遍历 Choices，永远不是 Choices[0]。在使用情况帧和
		// post-DONE 成本帧上，这个数组是空的，循环体根本不会执行。
		// 那一个词，决定了这个文件是能正常工作，还是会在每个请求的
		// 倒数第二帧上崩溃。
		//
		// (`n > 1` 会把几个补全交错进一个结果里。这个 Agent 从不要求它，
		// 要正确支持它，就意味着每个累积器都得同时以选择索引和工具
		// 索引为键。)
		for _, ch := range c.Choices {
			d := ch.Delta

			if d.Content != "" {
				markFirstToken()
				text.WriteString(d.Content)
				emit(Event{Kind: KindTextDelta, Text: d.Content})
			}

			if d.ReasoningContent != "" {
				markFirstToken()
				reasoning.WriteString(d.ReasoningContent)
				emit(Event{Kind: KindReasoningDelta, Text: d.ReasoningContent})
			}

			for _, tc := range d.ToolCalls {
				markFirstToken()

				acc := calls[tc.Index]
				if acc == nil {
					acc = &sseToolAccum{index: tc.Index}
					calls[tc.Index] = acc
				}

				// **闩。** 只在传入值非空时赋值。§B4 帧 3–9 携带
				// `"id":null,"function":{"name":null}`，
				// 一个不加防范的 `acc.id = tc.ID` 会在紧接着的下一块里把 id 清空——
				// 留下一个参数完整、却无法被回复的工具调用，因为 API 在回复中
				// 要求的 tool_call_id 没了。
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}

				// 宣布一次，一旦这个调用可识别。在这个端点 id 和 name 在一个块
				// 中一起到达，所以实践中事件总是两个都携带；"任一非空"的权限闸
				// 意味着把它们拆开的协议仍然得到宣布而不是沉默。
				if !acc.announced && (acc.id != "" || acc.name != "") {
					acc.announced = true
					emit(Event{Kind: KindToolCallStart, ToolID: acc.id, ToolName: acc.name})
				}

				// 开启端携带 `"arguments":""`，所以这个空值检查会让一个没有意义
				// 的零长度 delta 进不了 trace。片段是照原样追加的，从不检查内容
				// ——见 sseToolCallDelta。
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
					emit(Event{
						Kind:     KindToolArgsDelta,
						ToolID:   acc.id,
						ToolName: acc.name,
						Text:     tc.Function.Arguments,
					})
				}
			}

			// 用同样的方式被锁定，原因也一样：除了完成
			// 块之外到处都是 null，无防护的赋值会在
			// 后续的帧上擦除它。
			//
			// 存储的是字面值。在这里规范化意味着要
			// 在两个地方规范化（这个分支和下面的后备方案），
			// 而第二个总是会腐烂的那个。
			if ch.FinishReason != "" {
				res.RawStop = ch.FinishReason
			}
		}

		if c.Usage != nil {
			u := c.Usage.normalise()
			res.Usage = u

			// 发出**复制**。交出 &res.Usage 会把事件别名到调用者仍然可以写
			// 到的一个字段，一个懒序列化的订阅者（trace 写入者不是；TUI 之后
			// 可能）会记录它之后变成的任何东西。
			sent := u
			emit(Event{Kind: KindUsage, Usage: &sent})
		}

		return nil
	})

	res.Text = text.String()
	res.Thinking = reasoning.String()

	// RawStop 保留供应商的字面说法；Stop 保留
	// 规范化后的说法。两个都要，不能选一个：
	// 关于 CallResult.RawStop 的情况见 §A3c，
	// 在那种情况下信封在说谎，两者之间的间隔
	// 是仅存的证据。
	//
	// 无条件地完成，包括当 RawStop 是 "" 的
	// 时候——一个没有 finish_reason 的流规范化成
	// StopUnknown，这是 Agent 循环报告的东西，
	// 而不是零值 StopReason，那是一个没有任何
	// case 语句的值。
	res.Stop = normaliseStop(res.RawStop)

	// 升序索引顺序，不是到达顺序。Go 中的 map
	// 迭代被有意随机化，所以没有这个排序，顺序
	// 就会因运行而异——这是那种一周出现一次、
	// 被甩锅给模型的 bug。当没有工具调用时设为
	// nil，所以纯文本结果与零值 CallResult 相等。
	if len(calls) > 0 {
		ordered := make([]*sseToolAccum, 0, len(calls))
		for _, a := range calls {
			ordered = append(ordered, a)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })

		res.Calls = make([]Block, 0, len(ordered))
		for _, a := range ordered {
			res.Calls = append(res.Calls, Block{
				Kind: BlockToolCall,
				ID:   a.id,
				Name: a.name,
				Args: a.args.String(),
			})
		}
	}

	if err != nil {
		// 没有 KindResponseEnd：响应没有结束，它破了。发出这样一个事件，
		// 等于向每个订阅者撒了一个干净利落的谎，trace 应该是证据。
		return res, err
	}

	// 如果流在没有 finish_reason 的情况下结束，
	// RawStop 在这里是 ""——这个协议通过根本不
	// 提及它来报告的截断。把空字符串原封不动地
	// 传出去，而不是编造一个从未发生过的"stop"，
	// 让调用者可以看到这个情况；Stop 保持为
	// StopUnknown，这是 Agent 循环报告的东西，
	// 而不是当作清洁的完成处理。
	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.RawStop,
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}
