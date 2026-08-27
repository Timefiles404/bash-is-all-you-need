// 阶段 02——读流式响应。
//
// 非流式调用给你的是一个 JSON 对象、一个瞬间：你问完几秒之后，整个答案
// 一次性砸下来。流式把这个换成一串碎片，而 Agent 之所以显得是活的，几乎
// 全部出自这串碎片——文字边写边出、有 time-to-first-token 这个数、工具调
// 用在参数还没传完时就能在屏幕上被叫出名字。
//
// 这个文件故意切成两半：
//
//	readSSE           只懂 Server-Sent Events，此外一无所知。它没听说过
//	                  OpenAI，没听说过工具调用，也没听说过 token。
//	parseOpenAIStream 懂某一家供应商的 chunk 结构，把它转成这个仓库里
//	                  的事件。
//
// 阶段 03 加进 Anthropic 协议，那是一套完全不同的 chunk 结构，跑在*同一
// 套*分帧上。它把前一半原样复用，在后一半旁边再写第二个解析器。这两半要
// 是写成一个函数，那个阶段就不是新增而是重写——一句话，这就是拆开的全部
// 理由。
//
// 底下的一切都是照着 docs/wire-notes.md §B4/§B5/§B7 写的，那份文档记的是
// 这个端点实际发了什么，而不是规范说它该发什么。两边打架时字节说了算，每
// 一处冲突都有注释。那些注释是这个文件里最值钱的行：每一条对应的，都是照
// 规范写的客户端一头撞上去的崩溃，或者悄无声息就错掉的数字。
//
// 这里故意不处理带内错误帧。在这个端点上，错误是带 JSON body 的非 200 响
// 应（§D11），从来不是 200 流里的某一帧，所以没什么可找的。
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
// 前一半：SSE 分帧。故意做成与协议无关。
// ---------------------------------------------------------------------------

// sseFrame 是解出来的一帧 SSE。流里不带 event: 行时 Name 是 ""——本阶段会
// 见到的每一帧都是这样，因为这个端点的 OpenAI 那边只发 `data:`（§B4：整
// 条流上 `grep -c '^event:'` = 0）。Name 还是留着，因为阶段 03 的
// Anthropic 那边确实用 `event:` 行；等到那时再来教读取器认它，中间这段时
// 间它就是错的。
type sseFrame struct {
	Name string
	Data string
}

// readSSE 对每一帧调用 fn，直到流结束。它必须应付：只有 `data:` 行的帧、
// `event:` + `data:` 的帧、多行 data、空行分隔、CRLF，以及以 ':' 开头的注
// 释行。fn 返回非 nil 的 error 会中止扫描，并把那个 error 原样返回。
//
// 注意它*不*做什么：它根本不知道 `[DONE]` 是什么意思。哨兵是载荷协议的属
// 性，不是分帧的属性，把这份知识往下压到这里，最后就是读取器复用不了。
//
// 实现里有三个细节，每一个都对得起一个 bug：
//
//  1. 用 bufio.Reader，不用 bufio.Scanner。Scanner 默认拒收超过 64KB 的
//     token，而且是在最糟的时刻把这事报成错误——某个大工具结果在一条
//     delta 里回显出来，正是踩中它的那一帧，而且这种事只会在生产环境发
//     生。
//
//  2. 流的最后一行是在处理 EOF *之前*就先处理掉的。ReadString 会把已经
//     读到的字节连同 io.EOF 一起交回来，所以服务端不带结尾空行就关闭
//     时，最后一帧还躺在 `line` 里。先判错误，你就会悄悄丢掉每一条这种
//     流的最后一帧——而那通常就是带 usage 的那帧。
//
//  3. 行尾是一次剥一个（先 `\n`，再 `\r`），不是拿 cutset 一把切掉，所以
//     正当地以回车结尾的数据能把回车留住。单独一个 CR 作终止符——SSE 规
//     范允许，没人真发，§B4 里也没有——不在处理范围内；这里跟这个文件其
//     他地方一样，观测赢过规范。
func readSSE(r io.Reader, fn func(sseFrame) error) error {
	br := bufio.NewReader(r)

	var (
		name    string
		data    []string // 每个 `data:` 行一项；dispatch 时用 "\n" 拼起来
		sawData bool     // 有没有来过*任何* data 行，而不是它是否非空
	)

	// dispatch 把攒到此刻的这一帧交出去，然后清空缓冲。
	//
	// 规范说没有 data 行的帧不算事件，这里就照这条办：一连串空行、光秃秃的
	// keep-alive 注释于是一点代价都不花，而不是抖出一串空帧。带一条恰好为空
	// 的 data 行的帧*是*会 dispatch 的，这是有意越过规范一步——这是个调试工
	// 具，看得见的空帧比被悄悄丢掉的空帧教得更多。
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
				// 空行：一帧到此结束。
				if derr := dispatch(); derr != nil {
					return derr
				}

			case strings.HasPrefix(line, ":"):
				// 注释行。代理和网关拿它当 keep-alive 发，免得空闲连接在
				// 生成到一半时被回收。它们什么都不带，也不能结束当前这一
				// 帧——另外注意，这个 case 必须放在下面的字段切分之前判，
				// 否则 `: ping` 会被解析成一个名字为空的字段。

			default:
				// `field: value`，只有**第一个**冒号算分隔，值开头恰好剥掉
				// 一个空格。两条都要紧：这里每份载荷都是 JSON，值里全是冒
				// 号；空格规则搞错，每条消息的每个字节都会错开一位。
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
				// `id:` 和 `retry:` 是规范里用来重连断流的字段。§B4 里这两
				// 个都没出现，而这个端点也不提供"续上生成到一半的补全"这
				// 回事，所以直接忽略，而不是支持一半。
			}
		}

		if err != nil {
			if err == io.EOF {
				// 流结束了。缓冲里还剩下的是一帧真帧，只是没等到它那行终
				// 止空行——Anthropic 那边（§B6）正是这么收尾的：连接一关，
				// 连哨兵都没有。
				return dispatch()
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 后一半：OpenAI 的 chunk 结构。
// ---------------------------------------------------------------------------

// sseDoneSentinel 是 OpenAI 协议用来说"就这些了"的那一帧。
//
// **决定**：跳过它，**继续读到 EOF**。它在这里不是停止信号。
//
// §B4 的第 13 帧是一帧真帧，而且到得比哨兵*还晚*：
// `{"choices":[],"cost":"0"}`。每个守规范的客户端读到 `[DONE]` 就停，把它
// 扔了。不当这种客户端，有三个理由：
//
//   - 正确性。cost 帧是这个端点想给我们的数据。
//   - 连接卫生。body 里还有字节就把响应丢开，HTTP transport 就没法把连接
//     还回 keep-alive 池；于是你每个回合都付一次全新的 TLS 握手，而且一直
//     不知道为什么。
//   - 健壮性。万一哪天 usage 挪到哨兵后面——在一个已经把 `cost` 放在那儿
//     的端点上，这算不上什么离谱的假设——提前收手的客户端会报出零 token，
//     而且报得理直气壮。
//
// 读到底不花什么代价：服务端紧接着就把流关了。
const sseDoneSentinel = "[DONE]"

// sseChunk 是 OpenAI 协议上的一份 `data:` 载荷。
//
// 关于这几个 struct，最要紧的一件事：这个端点上每个字段都是显式发出
// `null`，而不是省略掉（§B4）。Go 的解码器会把 `null` 变成 string 的零
// 值、slice 的 nil，对 struct 则什么都不做——安安静静，不报错。这正是我
// 们要的，也正是陷阱所在："这个 key 在"在这里什么都说明不了。要判的是
// 值。下面每一处检查判的都是值。
type sseChunk struct {
	// usage 帧（§B4 第 11 帧）和 DONE 之后的 cost 帧（第 13 帧）上，Choices
	// 是**空的**。这个文件最可能出 bug 的地方就在这儿：
	// `chunk.Choices[0]` 读起来没毛病，顺利路径上的测试全都过，然后在每个真
	// 实请求的倒数第二帧上以 index-out-of-range panic。下面那个循环用的是
	// `range`，这就是修法。
	Choices []sseChoice `json:"choices"`

	// Usage 用指针，是为了让"缺席/null"和"在，但全是零"这两种情况分得开。零
	// token 的响应是件正当的、该报出来的事。
	Usage *sseUsage `json:"usage"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	FinishReason string   `json:"finish_reason"` // 除最后一个 chunk 外都是 null
	Delta        sseDelta `json:"delta"`
}

// sseDelta 是增量载荷。注意在这个协议里，reasoning **不是**单独的事件或
// 块类型——它就搭在同一个对象里，是个兄弟字段，区分它俩全靠哪个非 null
// （§B7）。那儿记下的那次运行里，44 帧带 reasoning_content，1 帧带
// content。
type sseDelta struct {
	Role             string             `json:"role"`              // 开场帧上是 "assistant"，之后是 null
	Content          string             `json:"content"`           // 开场帧上是 ""，多数 chunk 上是 null
	ReasoningContent string             `json:"reasoning_content"` // §B7：思考从这里来
	ToolCalls        []sseToolCallDelta `json:"tool_calls"`
}

type sseToolCallDelta struct {
	// Index 是这条 assistant 消息的 tool_calls 数组里的位置，也是**唯一**能
	// 把一个碎片和它所属的调用绑起来的东西。并行的工具调用会把各自的碎片交
	// 错着发；按别的东西攒，就会把一个调用的参数接到另一个的身上。
	Index int `json:"index"`

	// ID 和 Function.Name 只在**一个** chunk 里到，在它之后的每个 chunk 上都
	// 是 null（§B4 第 2 帧对比第 3–9 帧）。第一次见到就锁住。
	ID       string `json:"id"`
	Type     string `json:"type"` // 全程都是 "function"；不会置 null，也不是信号
	Function struct {
		Name string `json:"name"`

		// Arguments 的碎片**不**按 JSON 边界切。§B4 观测到的切法是
		// `{"command": ` / `"` / `ls` / ` -la /srv` / `/app` / `"` / `}`——
		// 词切一半，路径也切一半。没有哪个时刻碎片是能解析的 JSON，所以这
		// 里当裸字符串攒着，等流结束之后由调用方解析，只解析这一次。
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// sseUsage 是 OpenAI 的 token 账，按 OpenAI 自己的方向记的。
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

// normalise 转成本仓库的 Usage，这个转换是把方向反过来，不是改个名。
//
// 线上（§B4 第 11 帧）：
//
//	"prompt_tokens": 506, "prompt_tokens_details": {"cached_tokens": 192}
//
// 506 是**整个** prompt。那 192 个缓存 token 是算在**它里面**的。
//
// 本仓库的 Usage.Input 意思是"按全价计费的那部分"（见 events.go），所以缓
// 存那部分得**减出去**：
//
//	Input = 506 - 192 = 314   CacheRead = 192   →   Prompt() = 506 ✓
//
// 字段照抄不动，Usage.Prompt() 就会把 506 token 的 prompt 报成 698。误差恰
// 好等于缓存命中的大小，所以头一次冷请求时它是零：你测的时候它看着完美无
// 缺，而你的缓存做得越好它错得越狠。这就是这里为什么是个函数，而不是
// struct tag。
//
// Anthropic 那边又反了过来——在那儿 `input_tokens` 只是没命中缓存的余量，
// 所以直接映射到 Input，什么都不用减。两个协议，相反的约定，一个归一化的
// struct——这正是要有这个归一化 struct 的理由。
func (u sseUsage) normalise() Usage {
	cached := u.PromptTokensDetails.CachedTokens

	// 钳住，别信它。负的 Input 会一路传进 Prompt()，也传进任何拿它算出来的成
	// 本估算；万一这个端点报出的缓存 token 比 prompt token 还多，把这点出入吞
	// 掉，也好过对外抛一个负的 token 数。
	input := u.PromptTokens - cached
	if input < 0 {
		input = 0
	}

	return Usage{
		Input:     input,
		CacheRead: cached,
		Output:    u.CompletionTokens,
		// 它是 Output 的子集，不是外加的——§B4 这里报 0，是因为那次运行用
		// 的是 reasoning_effort:"none"。
		Reasoning: u.CompletionTokensDetails.ReasoningTokens,
		// CacheWrite 保持 0：这个协议的缓存是隐式的，线上根本没有缓存写这
		// 个数。它是零，不是因为什么都没缓存，是因为这个概念压根不上报。
	}
}

// streamToolCall 是跨很多个 chunk 拼起来的一次工具调用。
type streamToolCall struct {
	ID   string
	Name string
	Args string // 拼接起来的裸 JSON 字符串；这里**不**解析
}

// streamResult 是一次流式模型调用产出的东西。
type streamResult struct {
	Text         string
	Reasoning    string
	ToolCalls    []streamToolCall // 按 index 升序
	FinishReason string
	Usage        Usage
	TTFT         time.Duration // 什么都没流出来的话就是零
}

// sseToolAccum 是一次工具调用在途中的状态。它不是对外返回的那个形状，因为
// 它拿着两样调用方绝不该看见的东西：builder，以及 start 事件是不是已经发
// 出去了。
type sseToolAccum struct {
	index     int
	id        string
	name      string
	args      strings.Builder
	announced bool // 这个 index 的 KindToolCallStart 已经发过了
}

// parseOpenAIStream 吃一份 OpenAI 协议的 SSE body，事件到一个就往 bus 上发
// 一个，最后返回拼好的结果。
//
// `started` 是请求发出去的时刻，不是这个函数被调用的时刻——TTFT 是往返的
// 属性，从响应头到达那一刻起算，会把你本来想看的那段延迟整个藏起来。
//
// 流中途 I/O 失败时，这里把部分结果**和**错误一起返回，是有意不走通常那套
// `return nil, err`。一条在完整工具调用之后才断掉的流，和一条什么都没产出
// 的流，是两回事；而调用方只有拿到已经到手的东西才分得清这两者。调用方仍
// 然必须检查错误——没有 finish_reason 的部分结果就是截断，而阶段 01 整整
// 一章讲的就是截断没被发现会怎么样。
func parseOpenAIStream(r io.Reader, bus *Bus, turn int, started time.Time) (*streamResult, error) {
	res := &streamResult{}

	// emit 在每个事件上都盖上回合号，这样哪个调用点都忘不掉；它也容忍 bus 为
	// nil，好让这个解析器能当纯函数用。
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
	// role 开场帧（§B4 第 1 帧）故意不算数：它带的是 `content: ""`，一点载荷
	// 都没有。把它算进去，TTFT 就变成了 time-to-first-byte——碰上先想四秒再
	// 开口的模型，这个数好看得很，却什么也说明不了。文本、reasoning、工具调
	// 用结构都算数——尤其是 reasoning，在会思考的模型上它确实是最先生成出来
	// 的东西。
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
			// 跳过它，接着往下读。理由见 sseDoneSentinel。
			return nil
		}

		var c sseChunk
		if jerr := json.Unmarshal([]byte(payload), &c); jerr != nil {
			// 回合已经产出了有效的工具调用，不该被一帧坏帧毁掉。把它
			// 以 notice 的形式露出来——在 trace 里看得见，在主循环里活得
			// 下去——然后接着走。在这里返回错误，看着更利落，实际更糟。
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("skipped an SSE frame that was not JSON: %v (%.120s)", jerr, payload)})
			return nil
		}

		// 对 Choices 用 range，绝不写 Choices[0]。usage 帧和 DONE 之后的 cost
		// 帧上，这个数组是空的，循环体干脆就不执行。就这一个词，决定了这个文
		// 件是能跑，还是在每个请求的倒数第二帧上 panic。
		//
		// （`n > 1` 会把好几个 completion 交错进同一个结果。这个 Agent 从不
		// 这么要，而要正经支持它，每个累加器就得同时按 choice index 和 tool
		// index 做键。）
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

				// **锁存点**。只在进来的值非空时才赋值。§B4 第 3–9 帧带
				// 的是 `"id":null,"function":{"name":null}`，直白写一句
				// `acc.id = tc.ID`，下一个 chunk 就会把 id 抹平——于是你
				// 有一次参数完整却没法回答的工具调用，因为 API 要求回复
				// 里带回的那个 tool_call_id 没了。
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}

				// 只宣告一次，这次调用刚能被认出来就宣告。这个端点上 id
				// 和 name 是同一个 chunk 里一起到的，所以实际上事件总是
				// 两个都带着；用"两者之一非空"来开闸，是为了让某个把它
				// 俩拆开发的协议照样能得到一次宣告，而不是一片沉默。
				if !acc.announced && (acc.id != "" || acc.name != "") {
					acc.announced = true
					emit(Event{Kind: KindToolCallStart, ToolID: acc.id, ToolName: acc.name})
				}

				// 开场帧带的是 `"arguments":""`，这个空值判断挡住的是一
				// 条毫无意义的零长度 delta，别让它进 trace。碎片是原样
				// 追加的，从不检查——见 sseToolCallDelta。
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

			// 同样地锁存，同样的理由：除了 finish chunk，别处全是 null，
			// 不加防护的赋值会在它后面的那些帧上把它抹掉。
			if ch.FinishReason != "" {
				res.FinishReason = ch.FinishReason
			}
		}

		if c.Usage != nil {
			u := c.Usage.normalise()
			res.Usage = u

			// 发**副本**。把 &res.Usage 交出去，等于让事件和调用方还能写
			// 的那个字段共用同一份；而某个惰性序列化的订阅者（trace 写入
			// 器不是，后面的 TUI 可能是）记下来的，就会是它后来变成的样子。
			sent := u
			emit(Event{Kind: KindUsage, Usage: &sent})
		}

		return nil
	})

	res.Text = text.String()
	res.Reasoning = reasoning.String()

	// 按 index 升序，不是按到达顺序。Go 的 map 遍历是故意随机化的，没有这次
	// 排序，顺序就会一次运行一个样——这种 bug 一周复现一次，然后被赖到模型
	// 头上。没有工具调用时留 nil，这样纯文本的结果跟零值 streamResult 比起来
	// 是相等的。
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
		// 不发 KindResponseEnd：响应不是结束了，是断了。发一个出去，等于对
		// 每个订阅者说一句干干净净的谎，而 trace 本该是证据。
		return res, err
	}

	// 流结束时没给出 finish reason 的话，这里的 FinishReason 就是 ""——这种
	// 截断，这个协议就是靠只字不提来"报告"的。把空字符串原样传下去，调用方
	// 就还看得见它，而不是凭空编出一个根本没发生过的 "stop"。
	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.FinishReason,
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}
