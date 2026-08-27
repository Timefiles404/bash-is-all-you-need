// 阶段 03——OpenAI 协议适配器。
//
// 这个文件里的一切，都是某一家厂商对"对话该长什么样"的看法：系统提示词是一
// 条消息，工具结果也是一条消息，工具参数是嵌在 JSON 里的 JSON 字符串，工具
// 定义还要往下钻一层放在 `function` 底下。这些没有一条是关于语言模型的事
// 实。它们是关于这条线的事实，所以被隔离在这里，挡在 provider.go 的
// Provider 接口后面，Agent 主循环一条都学不到。
//
// 解析这一半是从阶段 02 的 sse.go 里切出来的；原先跟它挨着的 SSE 分帧现在住
// 在 sse.go 里，那个文件对这些事一无所知。搬家过程中解析器只改了返回类型，
// 别的什么都没动——它当初围着写的那些实测行为（§B4 的第 11 和 13 帧、id 的
// 锁存、参数片段的不对齐）还是同样的行为，解释它们的注释也还是同样的注释。
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

// 说这个协议的端点之间，只有三样东西不同，openaiProvider 就装着这三样。这里
// 没有厂商 SDK，也不需要在谁那儿开户：本地的 llama.cpp 服务、网关、还有
// OpenAI 自己，差别就在 URL 和模型串上。
type openaiProvider struct {
	baseURL string
	apiKey  string
	model   string
}

func newOpenAIProvider(baseURL, apiKey, model string) *openaiProvider {
	return &openaiProvider{
		// 这里跟 config.go 里都要裁一次，否则测试里直接构造出来的供应商
		// 会往 `.../v1//chat/completions` POST——有的服务器认这个路由，
		// 有的回 404，于是这个 bug 只在你没拿来测的那个端点上冒头。
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
	}
}

// 在编译期证明这个文件守着 provider.go。没有它，签名漂移的第一个证据会是
// config.go 那个 switch 里的构建失败——指向的是错的文件。
var _ Provider = (*openaiProvider)(nil)

func (p *openaiProvider) Protocol() string { return "openai" }
func (p *openaiProvider) Model() string    { return p.model }

// ---------------------------------------------------------------------------
// 请求：把中立的对话渲染成这家厂商要的形状
// ---------------------------------------------------------------------------

// oaiMessage 是 `messages` 里的一项。
//
// 这些类型上的 `oai` 前缀不是装饰。anthropic.go 在同一个包里声明了同样的概
// 念，光叫 `message` 就意味着两个适配器要抢同一个名字——而那正是这整个阶段
// 要防的那个错误，只不过发生在文件层面。
type oaiMessage struct {
	Role string `json:"role"`

	// Content 为空时是省略掉，不是发 null。这是阶段 02 上线时的行为，故意保
	// 留：只包含工具调用的 assistant 消息本来就没有 content，而这个端点接受它
	// 缺席。它唯一表达不了的形状，是*故意*留空的工具结果——实际上 exec.go 总会
	// 补一行 `[exit N]` 收尾，所以空的结果根本到不了这里。
	Content string `json:"content,omitempty"`

	// 只有回放回去的 assistant 消息才会设 ToolCalls。
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`

	// 只有 `role:"tool"` 的消息才设 ToolCallID，而它就是这个协议全部的寻址机
	// 制：结果报出它答的是哪个调用。流解析器里把 id 弄丢，答案就无处可去。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`

		// Arguments 是装着 JSON 的 JSON *字符串*——OpenAI 标准的双层编
		// 码（§A2 在响应那一侧原样记了一份）。
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

	// 不带这个，真正的 OpenAI 端点不会把 usage 流出来。这个仓库开发时对着的那
	// 个网关带不带都会发 usage——见 docs/wire-notes.md §B5，那里这个 flag 是
	// *可以量出来*的空操作：加与不加，都是 13 帧、同样的位置、同样的字段。还是
	// 照发不误：它一分钱不花，而不发的下场是——哪天有人把它指向另一个供应商，
	// 这个 Agent 就报出零 token。
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// BuildRequest 把中立的对话渲染到这条线上。
//
// 它连同请求一起返回 marshal 好的 body，是因为调用方要拿它发 KindRequest；
// 而请求检查器只有把真正发出去的字节摆给你看，才算诚实——不是把同一个结构
// 体再 marshal 一遍，那两次可能不一样。
//
// 这条路径上发生了四次翻译——三次在下面，第四次在 assistantMessage 里——每
// 一次都是两个协议意见不合的地方。它们被标在各自发生的位置，而不是在开头列
// 成一张表，因为这些分歧就是这一章本身。
func (p *openaiProvider) BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	out := make([]oaiMessage, 0, len(msgs)+1)

	// **分歧 1**——系统提示词住在哪儿。
	//
	// 在这边它就是普通的一条消息，排在数组第一个，role 是 "system"。在
	// Anthropic 协议上它是顶层的 `system` 字段，根本当不成消息。正是这份不对
	// 称，让 Provider.BuildRequest 把系统提示词单列成一个参数：两种放法都当不了
	// 中立的那个，所以中立形式干脆拒绝选。
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: system})
	}

	for _, m := range msgs {
		if m.Role == RoleAssistant {
			out = append(out, p.assistantMessage(m))
			continue
		}

		// **分歧 2**——工具结果怎么寻址。
		//
		// 每份结果都变成**自己独立的**一条消息，`role:"tool"`，并报出它
		// 答的是哪个调用。三份结果，三条消息。Anthropic 协议则把同样这
		// 三份收进**一条** user 消息里的 tool_result 块，两边做反了都是
		// API 报错。
		//
		// provider.go 里没有 RoleTool，原因正在于此：挑哪一种形状当中立
		// 形式，都等于把某一家厂商的设计偷渡进核心，所以工具结果是一个
		// *块*，由适配器决定用什么消息来装它。
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
			// BlockThinking 出去的路上就丢掉了。这个协议没有对应的入
			// 站字段——`reasoning_content` 只在响应里有——所以把它回
			// 放回去，要么被忽略要么被拒，看对面是谁家的实现。
		}
		if sawText {
			out = append(out, oaiMessage{Role: string(m.Role), Content: text.String()})
		}
	}

	// **分歧 3**——工具定义的信封。
	//
	// 在这边，schema 被埋在 `{"type":"function","function":{...}}` 底下，装
	// schema 的键叫 `parameters`。Anthropic 协议把 name/description 放在顶层，
	// schema 那个键叫 `input_schema`。中立的 Tool 结构体两种信封都不带——一张工
	// 具表能同时喂饱两边，全靠这一点。
	var defs []oaiToolDef
	for _, t := range tools {
		var d oaiToolDef
		d.Type = "function"
		d.Function.Name = t.Name
		d.Function.Description = t.Description
		d.Function.Parameters = t.Schema
		defs = append(defs, d)
	}

	// 编码时把 HTML 转义**关掉**，跟 anthropic.go 保持一致。
	//
	// Go 的 json.Marshal 会把 <、> 和 & 转成 \u003c、\u003e 和 \u0026——这是为
	// 浏览器安全定的默认值，对 shell Agent 却是彻头彻尾的敌意：那三个字符在这儿
	// 就是 `2>&1`、`>/tmp/out` 和 `<<EOF`。一条真实的命令会变成：
	//
	//	{"command":"grep -rn 'x' . 2\u003e\u00261 | head -5 \u003e/tmp/out"}
	//
	// 服务端会解码，所以模型读到的字符串两种写法都一样。但这四行仍然值：请求检
	// 查器本来是要给你看你发了什么，而上面那行不是人能读的。还有，这样转义会不
	// 会挪动供应商的缓存键，取决于它哈希的是裸字节还是解码后的内容——这一点我
	// 们不知道，而不知道正是保持一致的理由，不是拍脑袋猜的理由。
	//
	// 真正的论据是一致性：同一段对话，两个适配器发出不同的字节，这在专讲"把这类
	// 差异归一化掉"的章节里就是个疙瘩。
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
	// Encoder.Encode 会在末尾加一个换行，Marshal 不加。对服务端无害，但它会出现
	// 在检查器里，也会出现在每一份 trace 里。
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

// assistantMessage 把 API 在非流式下本会返回的那条消息重建出来，因为要放回
// 历史里的就是它。重新拼装是流式的税，忘了交这份税，就是流式 Agent"弄丢"自
// 己工具调用的原因。
func (p *openaiProvider) assistantMessage(m Msg) oaiMessage {
	am := oaiMessage{Role: "assistant", Content: m.Text()}
	for _, b := range m.Blocks {
		if b.Kind != BlockToolCall {
			continue
		}
		var call oaiToolCall
		call.ID, call.Type = b.ID, "function"
		call.Function.Name = b.Name

		// **分歧 4**——`arguments` 的类型，以及 Block.Args 为什么是裸
		// 字符串。
		//
		// 这个协议要的是装着 JSON 的 JSON 字符串，而流解析器攒下来的正
		// 是这个，所以字节原样穿过去，什么都不动。Anthropic 那边要同样
		// 的数据以 JSON *对象*的形式出现，得先 unmarshal 一遍。
		//
		// 中立形式要是存成解码后的 map，这边每个回合都得重新序列化一
		// 次；而 Go 是故意把 map 的迭代顺序随机化的——于是同一次工具调
		// 用每次产出的字节都不一样，按字节比对的 prompt 缓存就废了
		// （§C9：9,815 个 token 里有 9,792 个是从缓存里取的，全靠前缀完
		// 全一致），格式本身有意义的参数值也会被改坏。
		call.Function.Arguments = b.Args

		am.ToolCalls = append(am.ToolCalls, call)
	}
	return am
}

// ---------------------------------------------------------------------------
// 响应：流式 chunk 的结构
// ---------------------------------------------------------------------------

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

// ParseStream 吃下一段 OpenAI 协议的 SSE body，事件一到就往 bus 上发，最后
// 把拼好的结果以中立形状返回。
//
// `started` 是请求发出去的时刻，不是这个函数被调用的时刻——TTFT 是往返的属
// 性，从响应头到达的那一刻起算，会把你本来想看的那段延迟整个藏起来。
//
// 流中途 I/O 出错时，这里把残缺结果**和**错误一起返回，这是对常见的
// `return nil, err` 的一次有意背离。一条在完整工具调用之后才死掉的流，和一
// 条什么都没产出的流，是两回事；只有把已经到手的东西交给调用方，它才分得
// 清。调用方仍然必须检查错误——没有 finish_reason 的残缺结果就是截断，而阶
// 段 01 整整一章讲的就是截断没被发现会怎样。
func (p *openaiProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	res := &CallResult{}

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

			// 同样的锁存，同样的理由：除了收尾那个 chunk，别处全是
			// null，不加判断直接赋值，后面的帧就会把它抹掉。
			//
			// 存下来的是原字符串。在这里归一化，就意味着要在两个地
			// 方归一化（这一支和下面那个兜底），而烂掉的永远是第二
			// 个。
			if ch.FinishReason != "" {
				res.RawStop = ch.FinishReason
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
	res.Thinking = reasoning.String()

	// RawStop 留供应商的原话，Stop 留归一化之后的那个。两个都要，不是二选一：
	// 见 CallResult.RawStop 里那个案例（§A3c）——信封在说谎，两者之间的落差是
	// 仅存的证据。
	//
	// 这一步无条件执行，RawStop 是 "" 时也执行——没带 finish_reason 就结束的流
	// 会归一化成 StopUnknown，Agent 主循环会把它报出来；而不是归成 StopReason
	// 的零值，那个值在这个仓库里没有任何 switch 为它写过分支。
	res.Stop = normaliseStop(res.RawStop)

	// 按 index 升序，不按到达顺序。Go 的 map 迭代是故意随机化的，不排这一下，顺
	// 序每跑一次变一次——这种 bug 一周复现一回，最后赖到模型头上。没有工具调用
	// 时就留 nil，这样纯文本的结果跟零值的 CallResult 比较起来相等。
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
		// 不发 KindResponseEnd：响应不是结束了，是断了。发一个出去，等于对
		// 每个订阅者说一句干干净净的谎，而 trace 本该是证据。
		return res, err
	}

	// 流要是没带 finish_reason 就结束了，这里的 RawStop 就是 ""——这个协议报告
	// 截断的方式，就是压根不提。把空串原样传下去，调用方就还看得见这件事，好过
	// 凭空造出一个从没发生过的 "stop"；Stop 保持 StopUnknown，Agent 主循环会把
	// 它报出来，而不是当成干净的收尾。
	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.RawStop,
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}
