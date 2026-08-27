// 阶段 02——读取流式响应。
//
// 一个非流式调用给你一个 JSON 对象和一个瞬间：整个答案
// 在你问好几秒后才到达。流式则拿这个，换来一个片段序列，
// 而几乎一切让 Agent 显得"活着"的东西，都来自那个序列——
// 文本随写入而出现，一个 TTFT 数字，一个你能在参数完全
// 到达之前，就在屏幕上叫出名字的工具调用。
//
// 这个文件有意分成两半：
//
//	readSSE           只知道 **SSE**（Server-Sent Events），别的
//	                  什么都不知道。它从未听说过 OpenAI、工具
//	                  调用，或 token。
//	parseOpenAIStream 知道某一个供应商的块 schema，并把它转换
//	                  成这个仓库的事件。
//
// 阶段 03 加入了 Anthropic 协议，这是一套完全不同的块
// schema，却搭载在**相同**的框架之上。它逐字复用前一半，
// 又在后一半旁边，写了第二个解析器。假如这两半原本是
// 同一个函数，这个阶段就会是一次重写，而不是一次新增——
// 这一句话，就是这么拆分的全部理由。
//
// 下面的一切，都是针对 docs/wire-notes.md §B4/§B5/§B7 写的，
// 它记录的是这个端点实际发送的内容，而不是规范上说它应该
// 发送什么。两者不一致时，字节说了算，每一处分歧，都被
// 写进了注释。那些注释，是这份文件里最有价值的几行：每
// 一处都是一次崩溃，或者一个悄无声息出错的数字——都是
// 那种只读 spec 的客户端，会一头撞上去的东西。
//
// 这里有意不处理的：带内错误帧。在这个端点上，一个错误，
// 是一个带 JSON 体的非 200 响应（§D11），永远不是 200 流
// 内部的一个帧，所以根本没有什么好找的。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 第一半：**SSE** 框架。有意做到与协议无关。
// ---------------------------------------------------------------------------

// sseFrame 是一个解码后的 SSE 帧。对于省略了 event: 行的流，
// Name 会是 ""——这就是这个阶段所能看到的每一个帧，因为
// 这个端点的 OpenAI 一侧，只发送 `data:`（§B4：在整个流中，
// `grep -c '^event:'` = 0）。Name 之所以还是存在，是因为阶段
// 03 里的 Anthropic 一侧，确实会用到 `event:` 行，而一个要
// 等到以后才被教会认识它们的读取器，在这之前的这段时间
// 里，本身就是错的。
type sseFrame struct {
	Name string
	Data string
}

// readSSE 会对每一帧都调用 fn，直到流结束为止。它必须
// 处理：只有 `data:` 行的帧、带 `event:` + `data:` 的帧、多行
// 数据、空行分隔、CRLF，以及以 ':' 开头的注释行。如果 fn
// 返回一个非 nil 的错误，就会停止扫描，并把那个错误返回。
//
// 注意它**不**做的事：它完全不知道 `[DONE]` 是什么意思。
// 一个哨兵，是载荷协议的属性，不是框架的属性——把这个
// 知识硬塞进这一层里，正是你最终会没法复用这个读取器的
// 原因。
//
// 实现里有三个细节，每一个都值得算作一个 bug：
//
//  1. bufio.Reader，不是 bufio.Scanner。Scanner 默认会拒绝
//     超过 64KB 的 token，并在最糟糕的时刻，把这一点报告
//     为错误——一个在单个 delta 里被原样回显回来的大型
//     工具结果，正是会触发这个问题的那种帧，而这种情况，
//     只会在生产环境里才发生。
//
//  2. 流的最后一行，会在 EOF 被处理**之前**，先被处理掉。
//     ReadString 会把它设法读到的字节，连同 io.EOF 一起
//     交回来，所以一个没有以空行收尾就关闭连接的服务器，
//     它的最后一帧，仍然会好好地待在 `line` 变量里。如果
//     你先检查错误，就会无声无息地丢掉每一个这种流的
//     最后一帧——而这一帧，通常正是携带着 usage 的那一个。
//
//  3. 行尾会被逐个剥离（先 `\n`，再 `\r`），而不是用一个
//     cutset 一起处理，所以那些确实是以一个回车符结尾的
//     合法数据，会把它保留下来。一个单独的 CR 终止符——
//     SSE spec 允许、没有人会发出、§B4 里也没有出现——
//     超出范围；在这里，也是观察赢过 spec——就像这份文件
//     里其他所有地方一样。
func readSSE(r io.Reader, fn func(sseFrame) error) error {
	br := bufio.NewReader(r)

	var (
		name    string
		data    []string // 每个 `data:` 行一项；分发时用 "\n" 连接
		sawData bool     // 是否有**任何**数据行到达，不是是否非空
	)

	// 分发会交付目前为止构建好的帧，并重置缓冲区。
	//
	// 规范说没有数据行的帧不是事件，这里就是这个规则：它让连续的
	// 空白行和裸 keep-alive 注释不产生代价，而不是引发一阵空帧。
	// 有一个数据行恰好为空的帧**确实**会分发，这是有意越过规范一步
	// ——这是调试工具，可见的空帧比无声丢弃的帧教得更多。
	dispatch := func() error {
		if !sawData {
			name = ""
			return nil
		}
		f := sseFrame{Name: name, Data: strings.Join(data, "\n")}
		name, data, sawData = "", data[:0], false
		return fn(f)
	}

	for {
		line, err := br.ReadString('\n')

		if line != "" {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")

			switch {
			case line == "":
				// 空行：帧结束。
				if derr := dispatch(); derr != nil {
					return derr
				}

			case strings.HasPrefix(line, ":"):
				// 注释。代理和网关把这个当 keep-alive 发送，这样空闲连接就不会
				// 在生成过程中被回收。它们什么都不带，也不能终止当前帧——注意
				// 这种情况得在下面的字段拆分之前测试，否则 `: ping` 会解析成一个
				// 名字为空的字段。

			default:
				// `field: value`，其中只有**第一个**冒号分隔，值的单个前导空格被
				// 剥离。两个都很重要：这里的每个载荷都是 JSON，所以值里全是冒号，
				// 空格规则错误会把每个消息的每个字节移位一位。
				field, value := line, ""
				if i := strings.IndexByte(line, ':'); i >= 0 {
					field, value = line[:i], line[i+1:]
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "event":
					name = value
				case "data":
					data = append(data, value)
					sawData = true
				}
				// `id:` 和 `retry:` 是规范字段，用于重新连接到断开的流。两个都
				// 没有出现在 §B4，这个端点不提供恢复半生成完成的功能，所以它们
				// 被忽视而不是半支持。
			}
		}

		if err != nil {
			if err == io.EOF {
				// 流结束了。任何还在缓冲的东西是一个真实的帧，只是没有得到它
				// 的终止空行——Anthropic 一侧（§B6）正是这样结束的，关闭连接
				// 时根本没有哨兵。
				return dispatch()
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 下半部分：OpenAI 块模式。
// ---------------------------------------------------------------------------

// sseDoneSentinel 是 OpenAI 协议用来说"就这么多"的帧。
//
// **决策：我们跳过它并继续排空到 EOF**。它不是这里的停止信号。
//
// §B4 帧 13 是一个真实的帧，在哨兵**之后**到达：
// `{"choices":[],"cost":"0"}`。每个规范兼容的客户端在 `[DONE]` 处停止
// 读取并丢弃它。有三个理由不这样做：
//
//   - 正确性。成本帧是这个端点试图给我们的数据。
//   - 连接卫生。放弃还有字节在其中的响应体意味着 HTTP 传输不能
//     把连接返回到 keep-alive 池；你每回合支付一次新 TLS 握手，
//     永远不会注意到为什么。
//   - 健壮性。如果使用量曾经在哨兵之后移动——在一个已经在那里放
//     `cost` 的端点，那不是疯狂的假设——一个停止得早的客户端报告
//     零 token 且充满信心地错了。
//
// 排空什么都不花：服务器之后立即关闭流。
const sseDoneSentinel = "[DONE]"

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

// streamToolCall 是跨许多块组装的一个工具调用。
type streamToolCall struct {
	ID   string
	Name string
	Args string // 连接的原始 JSON 字符串；**这里不解析**
}

// streamResult 是一个流式模型调用产生的东西。
type streamResult struct {
	Text         string
	Reasoning    string
	ToolCalls    []streamToolCall // 按升序索引顺序
	FinishReason string
	Usage        Usage
	TTFT         time.Duration // 如果什么都没流式过则为零
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

// parseOpenAIStream 消耗一个 OpenAI 协议 SSE 体，在事件到达时向
// bus 发出事件，并返回组装的结果。
//
// `started` 是请求出去时，不是这个函数被调用时——TTFT 是往返的
// 属性，从响应头到达的时刻测量隐藏了你试图看到的整个延迟。
//
// 在中流 I/O 失败时这返回部分结果**和**错误，这是有意打破常规
// 的 `return nil, err`。一个在完整工具调用后死掉的流与一个什么都
// 没产生的不同，调用者只有在被交给到达的东西时才能区分。调用者
// 必须仍然检查错误——一个没有 finish_reason 的部分结果是截断，
// 阶段 01 是一个关于截断未被注意时会发生什么的整章。
func parseOpenAIStream(r io.Reader, bus *Bus, turn int, started time.Time) (*streamResult, error) {
	res := &streamResult{}

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

			// 以同样的方式锁定，出于同样的原因：除了完成块之外，处处都是
			// null，一个无防卫的赋值会在那之后的帧上把它擦掉。
			if ch.FinishReason != "" {
				res.FinishReason = ch.FinishReason
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
	res.Reasoning = reasoning.String()

	// 升序索引顺序，不是到达顺序。Go 里 map 的迭代顺序是故意随机化的，
	// 所以不做这次排序，顺序就会一次运行一个样——一种一周重现一次的
	// bug，还会被怪到模型头上。没有工具调用时留 nil，所以仅文本结果
	// 等于零值 streamResult。
	if len(calls) > 0 {
		ordered := make([]*sseToolAccum, 0, len(calls))
		for _, a := range calls {
			ordered = append(ordered, a)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })

		res.ToolCalls = make([]streamToolCall, 0, len(ordered))
		for _, a := range ordered {
			res.ToolCalls = append(res.ToolCalls, streamToolCall{
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

	// 如果流结束时没有带上 finish reason，FinishReason 在这里就是
	// 空字符串——这个协议报告截断的方式，就是干脆不提。把这个空
	// 字符串原样传下去，能让调用者看到这一点，而不是凭空发明一个
	// 从未发生过的"stop"。
	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.FinishReason,
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}
