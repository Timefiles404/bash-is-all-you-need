// 阶段 03——Anthropic 协议适配器。
//
// Babel 的另一半。本文件和 openai.go 实现同一个接口，被同一个主循环驱动，然后
// 在几乎每件事上都谈不拢：
//
//	             OpenAI                  Anthropic（本文件）
//	系统提示词   messages[0]             顶层 `system` 字段
//	工具结果     一条 role:"tool" 消息   **一条** user 消息里的 tool_result 块
//	工具参数     JSON *字符串*           `input` JSON *对象*
//	工具 schema  嵌在 `function` 里      扁平的 `input_schema`
//	停止原因     finish_reason           stop_reason
//	缓存 token   算在 prompt_tokens 里   在 input_tokens 之外*另计*
//	流的结束     `[DONE]` 哨兵           连接关闭
//
// 这七套词汇，除了本文件和 openai.go，别处一个字都不出现。这就是本阶段的架构主
// 张：供应商的词，到适配器边界为止。
//
// 下面处理的每一处偏离，都在 docs/wire-notes.md 里留了证据。观察到的字节和公开
// 的规范对不上时——在这个端点上，它们在六七个地方各自对不上——听观察的，因为
// 凌晨三点线上跑的是观察到的那套。
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

// anthropicVersion 是规范要求、而这个网关其实并不要求的头（§D11：不带它调用照
// 样成功）。还是发。今天省掉它不花什么代价；等哪天有人把这个 Agent 指向真正的
// api.anthropic.com，拿到一个关于某个头的报错，而代码里根本 grep 不到那个头
// ——那要花掉一个下午。
const anthropicVersion = "2023-06-01"

// 调用方传进来的预算不是正数时，就用 anthropicDefaultMaxTokens。`max_tokens`
// 在这个协议上是必填的，而 §D11 记下了省掉它换来什么：HTTP 400，响应体是
// `{"model":"qwen3.7-plus"}`——没有 `type`，没有 `error`，没有消息。打日志打
// `resp.Error.Message` 的代码，打出来是个空串。在这儿兜个默认值不是客气，而是
// 可诊断的失败和无声的失败之间的差别。
const anthropicDefaultMaxTokens = 4096

// ---------------------------------------------------------------------------
// 供应商
// ---------------------------------------------------------------------------

// anthropicProvider 说 Messages 协议。
//
// 它故意不持有 *http.Client。BuildRequest 只返回请求，ParseStream 只读
// io.Reader，所以这个类型完全不做 I/O。传输层的策略——超时、代理、重定向、连
// 接池——两个协议一模一样，属于调用方；每个适配器各配一个 client，忘记设超时
// 的地方就有两处。副作用是两个适配器都足够纯，纯到能用 strings.Reader 驱动，这
// 也是 anthropic_test.go 不需要网络、不需要 API key 的原因。
type anthropicProvider struct {
	baseURL string
	apiKey  string
	model   string

	// cacheBreakpoints 打开 cache_control 的摆放。它做成开关，纯粹是为了
	// 让这一章能量出差别；真跑 Agent 的时候，没有任何理由把它关掉。
	cacheBreakpoints bool
}

func newAnthropicProvider(baseURL, apiKey, model string) *anthropicProvider {
	return &anthropicProvider{
		// AGENT_BASE_URL 末尾多个斜杠，拼出来就是 "{base}//messages"。有的网关对
		// 此回 404；这个网关回 500，套着那个万能的 "Internal server error" 信封
		// （§D11）——.env 文件里一个字符，换你一小时调试。在这里裁一次，别指望
		// 每份配置都小心。
		baseURL:          strings.TrimRight(baseURL, "/"),
		apiKey:           apiKey,
		model:            model,
		cacheBreakpoints: true,
	}
}

// withCacheBreakpoints 切换要不要钉住前缀。--no-cache 用它产出
// docs/04-the-cache.md 里那组实验的对照臂。
func (p *anthropicProvider) withCacheBreakpoints(on bool) *anthropicProvider {
	p.cacheBreakpoints = on
	return p
}

func (p *anthropicProvider) Protocol() string { return "anthropic" }
func (p *anthropicProvider) Model() string    { return p.model }

// ---------------------------------------------------------------------------
// 请求的线上格式
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`

	// System 是**顶层字段**，不是消息。这是两个协议之间最显眼的差别，也
	// 是 Provider.BuildRequest 为什么单独给系统提示词留了个参数：中立形
	// 式无论选哪一边，都是把某家厂商的设计偷运进内核。
	//
	// 是 text 块的**数组**，不是阶段 03 用的那个纯字符串。这个改动就是
	// 这一章：块能带 `cache_control`，字符串带不了。§C8 量到的是，这次
	// 升级把每次跑都不一样的 64-token 块命中，变成了稳定的、9,775 个
	// token 的精确前缀命中。
	System []anthropicContent `json:"system,omitempty"`

	Messages []anthropicMessage `json:"messages"`

	// 在这个协议上，Tools 没有 `function` 那层包装。为空时整个字段省掉，而不是发
	// `[]`：存在但为空的 tools 数组和根本不存在，是两个不同的 prompt 前缀，前缀
	// 不同就是缓存未命中。
	Tools []anthropicTool `json:"tools,omitempty"`

	Stream bool `json:"stream"`
}

type anthropicMessage struct {
	Role string `json:"role"`

	// Content 一律是块数组，绝不用规范同样允许的字符串简写。一种形状就是一条代码
	// 路径：简写要自己写 marshaller，而且只要消息里带上工具结果或工具调用，它反
	// 正也得变成数组。
	Content []anthropicContent `json:"content"`
}

// anthropicContent 就是一个内容块。一个 struct，每种块类型配一个 omitempty 字
// 段，而不是接口——理由和 Event 做成扁平 struct（events.go）一样：JSON 在请求
// 检查器里始终读得懂，加一种块类型只是加个字段，不是加个类型再加个
// marshaller。
type anthropicContent struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`

	// Input 是**原始字节**，原样拼进去，一个字节都不碰。
	//
	// 中立的 Block.Args 是一段原始 JSON 字符串（provider.go 讲了为什么），而这个
	// 协议要的是对象。解成 map[string]any 再编回去，得到的对象是等价的，但键的顺
	// 序变了——Go 会给 map 键排序，模型当初是按自己的顺序发的——字节序列一变就
	// 是另一个 prompt 前缀，于是每个重放的回合都缓存未命中。json.RawMessage 是唯
	// 一一种什么都不做就能把字符串变成对象的字段类型。
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`

	// CacheControl 把这个块标成可缓存前缀的末尾。
	//
	// 用指针加 omitempty，这样没标记的块序列化出来的字节，跟阶段 04 之前
	// 一模一样。这件事比看上去要紧：要是加了这个特性，连**没标记**的块的
	// 字节都变了，那打开缓存的动作，恰好会作废它本来要保住的那段前缀。
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl 钉住一段前缀。`ephemeral` 是唯一的 type；5
// 分钟的 TTL 是默认值，在响应里以嵌套的
// `cache_creation.ephemeral_5m_input_tokens` 计数器出现（wire-notes
// §C8）。
type anthropicCacheControl struct {
	Type string `json:"type"`
}

func ephemeral() *anthropicCacheControl { return &anthropicCacheControl{Type: "ephemeral"} }

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// 是 InputSchema，不是 `parameters`，外面也没有 `function` 对象套着。里面的
	// JSON Schema 完全相同，只有信封不一样。
	//
	// 尽管 Go 的 map 遍历顺序是随机的，这里用 map 仍然安全：encoding/json 在
	// marshal 时会给 map 键排序，所以渲出来的 schema 逐次都是同样的字节。这件事
	// 比看上去要紧——tools 块靠在 prompt 前部，落在缓存前缀里面，渲染一不稳定，
	// 每一个请求都要悄悄赔上一次完整的缓存写。
	InputSchema map[string]any `json:"input_schema"`
}

// 工具调用被截断时，这个网关自己会造一个对象出来，anthropicRawArguments 就照它
// 的样子来（§A3c：`input` 被换成
// `{"raw_arguments":"<invalid JSON text>"}`）。见 anthropicToolInput。
type anthropicRawArguments struct {
	RawArguments string `json:"raw_arguments"`
}

// ---------------------------------------------------------------------------
// BuildRequest
// ---------------------------------------------------------------------------

// BuildRequest 把中立形式的对话渲染到这条线上。
//
// 它把 marshal 好的请求体跟请求一起返回，因为调用方要拿它发 KindRequest——那
// 就是请求检查器，也是 trace 里唯一一份"模型到底看到了什么"的记录。从请求上再
// 读回来意味着要把 req.Body 抽干再重建，所以字节直接递出去。
//
// 事件不由这个适配器自己发：BuildRequest 手里没有总线，给它一条，就为了这点方
// 便，把"纯函数、不做 I/O、没有副作用"这条性质弄没了——而正是这条性质让两个适
// 配器都可测。
func (p *anthropicProvider) BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	if len(msgs) == 0 {
		// 网关对这种请求的回答是 400，而且不带错误信封（§D11）。在这里就失败
		// 掉，这里的消息还能说点有用的。
		return nil, nil, fmt.Errorf("anthropic: refusing to send a request with no messages")
	}
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	wireMsgs, err := anthropicMessages(msgs)
	if err != nil {
		return nil, nil, err
	}
	if p.cacheBreakpoints {
		markRollingBreakpoint(wireMsgs)
	}

	body, err := anthropicMarshal(anthropicRequest{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    p.systemBlocks(system),
		Messages:  wireMsgs,
		Tools:     anthropicTools(tools),
		Stream:    true,
	})
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	// Header.Set 在发出时会把这几个头规范成 X-Api-Key / Anthropic-Version。HTTP
	// 字段名不分大小写，所以这既正确又不可见；写下来只是因为拿这段代码对着文档看
	// 的人，会发现线上的大小写不一样。
	//
	// 注意认证方式：是 `x-api-key`，**不是** `Authorization: Bearer`。在这儿发
	// OpenAI 那个头，换来的是 §D11 里那个 AuthError 信封，写着
	// "Missing API key."——读起来像配置出了问题，实际上是把两个协议搞混了。
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")

	return req, body, nil
}

// anthropicMarshal 编码请求体时，把 HTML 转义**关掉**。
//
// 这不是为了好看。json.Marshal 会把 `<`、`>` 和 `&` 转成六个字符的 Unicode 转
// 义（u003c、u003e、u0026，每个前面各带一个反斜杠）——这条规则的存在，是为了
// 让 JSON 文档能贴进 HTML 的 script 标签里而不会提前把它闭合掉。
// 这个 Agent 干的全部活儿就是跑 shell 命令，而 shell 命令基本上就是这三个
// 字符：`2>&1`、`>/tmp/out`、`<<EOF`。转义之后它们语义完全相同，字节完全不
// 同，这意味着：
//
//   - 请求检查器给用户显示的是 `ls \u003e /tmp/out`，以及
//   - 重放的工具调用里只要出现一个重定向，缓存前缀就变了，一点道理都没有。
//
// Encoder.Encode 还会补一个 json.Marshal 不会补的换行；这里把它裁掉，好让
// KindRequest 的字节跟 POST 出去的字节完全一致。
//
// 有一样东西它**保不住**：拼进来的 json.RawMessage 内部那些无意义的空白。
// encoding/json 会把它压紧，所以模型发的 `{"command": "ls"}` 会以
// `{"command":"ls"}` 发出去。而键的**顺序**——真正会毁掉缓存的那部分，也是参
// 数一旦从 map 里过一遍就会被 Go 摧毁的那部分——原封不动地留住了。
func anthropicMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// anthropicTools 渲染工具定义。扁平的：{name, description, input_schema}。
// OpenAI 适配器把同样这三个字段包进 {"type":"function","function":{...}}，差别
// 就这么一点。
func anthropicTools(tools []Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema
		if schema == nil {
			// `input_schema` 是必填的，而且必须描述一个对象。不收参数的工具也得有
			// 这层信封；这里发 `null`，在真实 API 上就是 400。
			schema = map[string]any{"type": "object"}
		}
		out = append(out, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out
}

// anthropicMessages 是本文件最容易搞错的一段转换，所以它的测试也最多。
//
// 中立形式里没有 RoleTool（provider.go 解释了为什么）。工具结果是一个*块*；
// 这个协议回答 N 个工具调用，用的是**一条** user 消息里的 N 个 tool_result
// 块——不是 N 条消息，而 N 条消息正是 OpenAI 适配器发的东西。拆成一条条消息
// 发，会造出连续的 user 回合；在真实 API 上还会换来一个报错，说 tool_use 块
// 没有对应的结果。
//
// 所以工具结果先攒进 `pending`，最后作为一条 user 消息刷出去；这一串由紧接着的
// 下一样东西收尾——包括对话本身的结束，而这恰恰是常见情形：主循环回头去调模型
// 时，`msgs` 末尾正好就是一串新鲜的工具结果。
//
// 两条顺序规则被固定在这里，都是协议要求的：
//
//   - tool_result 块在自己那条 user 消息里排在**最前面**，排在同一回合携带的任
//     何文本之前；
//   - 如果这一串后面紧跟着用户自己的一条消息，两者合并，而不是接连出两
//     个 user 回合。
func anthropicMessages(msgs []Msg) ([]anthropicMessage, error) {
	var (
		out     []anthropicMessage
		pending []anthropicContent // 还没刷出去的 tool_result 块
	)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, anthropicMessage{Role: string(RoleUser), Content: pending})
		pending = nil
	}

	for _, m := range msgs {
		if m.Role == RoleSystem {
			// 要吵，不迁就。系统提示词在这个协议上是顶层字段，
			// Provider.BuildRequest 单独传它就是为了这个；这里出现 system 的 Msg，
			// 说明调用方是按 OpenAI 的路子拼对话的。不声不响把它改标成 "user"，发
			// 出去的 prompt 会有细微的不同，做出来的 Agent 也就细微地更差——这是
			// 最难被察觉的一类 bug。
			return nil, fmt.Errorf("anthropic: a system message in msgs — this protocol takes the system prompt as a top-level field, pass it as BuildRequest's system argument")
		}

		var own []anthropicContent

		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolResult:
				pending = append(pending, anthropicContent{
					Type:      "tool_result",
					ToolUseID: b.ID,
					// Content 是普通字符串。规范在这里也允许块数组（放图片，
					// 或者放 is_error），而这个 Agent 从来只有一句话要说：
					// shell 打了什么。
					Content: b.Text,
				})

			case BlockText:
				// 空的 text 块会被真实 API 拒掉（"text content blocks must be
				// non-empty"），反正空的也没带任何东西。
				if b.Text == "" {
					continue
				}
				own = append(own, anthropicContent{Type: "text", Text: b.Text})

			case BlockToolCall:
				own = append(own, anthropicContent{
					Type:  "tool_use",
					ID:    b.ID,
					Name:  b.Name,
					Input: anthropicToolInput(b.Args),
				})

			case BlockThinking:
				// **故意丢掉**，这是决定，不是漏了。
				//
				// 规范说 thinking 块重放时必须带上模型返回的 `signature`，否
				// 则 API 拒收。而在这个端点上，signature **永远**是空串——
				// 非流式响应里（§A3b）、`signature_delta` 帧里（§B7），无
				// 一例外。没有 signature 可以带回去，重放的 thinking 块就是
				// 一个通不过校验的块。
				//
				// 什么都不发，模型的私有推理就从下一回合的上下文里丢了，这是实打
				// 实的代价。发一个没签名的块，风险是 400 直接掐死会话。thinking
				// 的每个 token 在 trace 里都还在（KindReasoningDelta），所以记录
				// 一点没少——少的只是 prompt。
			}
		}

		if len(own) == 0 {
			// 渲染出来什么都没有的消息，不能变成空的 content 数组：`content: []`
			// 在真实 API 上是 400，而纯思考的 assistant 回合渲染出来正好就是这个。
			continue
		}

		if len(pending) > 0 && m.Role == RoleUser {
			// 合并，而不是刷出去：连着两条 user 消息，是这个协议不喜欢的形状；而
			// tool_result 块按要求必须排在所在消息的最前面。
			merged := make([]anthropicContent, 0, len(pending)+len(own))
			merged = append(merged, pending...)
			merged = append(merged, own...)
			own = merged
			pending = nil
		} else {
			flush()
		}

		out = append(out, anthropicMessage{Role: string(m.Role), Content: own})
	}

	flush()

	if len(out) == 0 {
		return nil, fmt.Errorf("anthropic: every message rendered empty; nothing to send")
	}
	return out, nil
}

// anthropicToolInput 把中立形式里那段原始 JSON 字符串 Args，转成这个协议的
// `input` 对象，字节原样穿过去。
//
// 有意思的是两个边界情形：
//
//   - Args 为空。模型调不带参数的工具时发的就是 ""，而 `input` 是必填的。`{}`
//     是诚实的渲法。
//
//   - Args 不是合法 JSON。§A3c 就是这事会发生的原因：工具调用在 max_tokens 处
//     被截断，回来的 `input` 被换成了
//     `{"raw_arguments":"{\"command\": \"find"}`——真真正正的非法 JSON，字符
//     串中间就断了——而 `stop_reason` 还乐呵呵地写着 "tool_use"。这东西哪天要
//     是转回请求里去，原样拼进去就是个畸形的请求体；而 §D11 记下了这个网关拿
//     到畸形请求体会做什么：HTTP 500，"Internal server error"。客户端的 bug 穿
//     上了服务端故障的衣服，而按 5xx 重试的策略会一直重试下去。
//
//     所以非法的字节被裹进网关自己那套截断形状里。请求体保持合法，证据一字不动
//     地留在字符串里面，模型看到的也是这个端点本来就会产出的结构。
func anthropicToolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(trimmed)) {
		wrapped, err := json.Marshal(anthropicRawArguments{RawArguments: args})
		if err != nil {
			// 只装了一个字符串的 struct，marshal 不可能失败；万一真失败了，空对象
			// 也仍然是个合法请求。
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(wrapped)
	}
	return json.RawMessage(trimmed)
}

// ---------------------------------------------------------------------------
// 流的线上格式
// ---------------------------------------------------------------------------

// anthropicStreamEvent 就是一份 `data:` 载荷。这个协议上的每种事件类型，都解到
// 这同一个 struct 里——另一条路是两趟解码（先读 `type`，再往对应的 struct 里
// unmarshal 一遍），为了省下几个用不上的指针字段，把每一帧的解析开销翻一倍。
//
// 指针是有讲究的：`Delta` 在 content_block_delta（带
// text/thinking/partial_json）和 message_delta（带 stop_reason）上都会出现，而
// nil 就是"这个事件根本没有 delta"和"有，但是空的"之间那道分界。
type anthropicStreamEvent struct {
	Type string `json:"type"`

	// Index 把 content_block_* 事件和它的块绑在一起。并行的工具调用是交错到达
	// 的，所以就靠它把一个调用的参数碎片挡在另一个的缓冲区外面——`index` 在
	// OpenAI 适配器的 tool_calls 数组里干的是同一件事。
	Index int `json:"index"`

	// Message 只出现在 message_start 上。它的 usage **谁都不读**；为什么，看
	// 下面的循环。
	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`

	ContentBlock *anthropicStreamBlock `json:"content_block"`
	Delta        *anthropicStreamDelta `json:"delta"`

	// message_delta 上的 Usage——这条线上唯一可信的 usage。
	Usage *anthropicUsage `json:"usage"`

	// Cost 是夹带在收尾那个 ping 上的非标准键（§B6、§C10）。
	//
	// 类型故意用 RawMessage 而不是 string：§C10 发现它一直是 JSON *字符串*
	// （"0"），可万一哪天来的是个数字，`string` 字段就 unmarshal 不了——而且它
	// 拖下去的是**整帧**，不只是这一个字段。一个可选的非标准键，绝不能有本事把它
	// 周围所有东西的解析一起搞崩。
	Cost json.RawMessage `json:"cost"`

	// Error 出现在 `event: error` 帧上。这个网关上没观察到（§D11 里的错误全是在
	// 流打开之前以 HTTP 状态码到达的），但规范会在响应体中途流出
	// overloaded_error 和 api_error；而中途死掉的流，绝不能被记成正常结束的流。
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStreamBlock struct {
	Type string `json:"type"` // "text", "thinking", "tool_use"

	// ID 和 Name **只在这里**到达，别处一概没有——content_block_start 是唯一给
	// 工具调用命名的事件。漏了它，你手上就是一堆没法归属的参数，和没法用来回答的
	// tool_use_id。
	ID   string `json:"id"`
	Name string `json:"name"`

	// 观察到的每个 content_block_start 上，Text 和 Thinking 都是 ""（§B6、
	// §B7），内容是以 delta 形式到的。照样读——看那个循环。
	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// 这里的 Input 是**空对象**，一直都是（§B6）。真正的参数是以
	// input_json_delta 碎片到的。信这个字段的解析器，每个工具调用都只拿到 `{}`，
	// 什么也执行不了。
	Input json.RawMessage `json:"input"`
}

// anthropicStreamDelta 同时罩住两种 delta 形状：content_block_delta 的载荷
// （Type 是 text_delta / thinking_delta / input_json_delta /
// signature_delta），和 message_delta 的载荷（StopReason）。
type anthropicStreamDelta struct {
	Type string `json:"type"`

	Text     string `json:"text"`     // text_delta
	Thinking string `json:"thinking"` // thinking_delta

	// PartialJSON 的碎片**不按 JSON 边界切**。§B6 记下了实测的切法：""、
	// `{"command": "ls`、` -la /srv`、`/app`、`"`、`}`——第一个是空的，第四个在
	// 路径中间就断了，第五个接着往下写。任何时刻单看一个碎片都解析不了，所以它们
	// 被原样拼起来，等流结束之后由调用方解析，且只解析一次。
	PartialJSON string `json:"partial_json"`

	// 这个端点上 Signature 一直是 ""（§B7），signature_delta 帧里也是。那种帧存
	// 在只是为了把形状凑齐，什么都不带，所以没有哪个 thinking 块需要校验或重放。
	Signature string `json:"signature"`

	StopReason   string `json:"stop_reason"`   // message_delta
	StopSequence string `json:"stop_sequence"` // 这个网关上完全不出现
}

// anthropicUsage 是这个协议的 token 账，按这个协议的方向记——而这个方向跟
// OpenAI 那边**正好相反**。
//
// 这里 `input_tokens` **只是**没走缓存的那点余量，缓存计数器是在它之外*另计*的
// （§C8：约 9,800 token 的 prompt，input_tokens 18，cache_read 9,775）。
// OpenAI 那边 `prompt_tokens` 是总数，`cached_tokens` 嵌在**它里面**，所以那个
// 适配器得做减法。同一次缓存命中，两套相反的算术，一个规范化的 struct——这就
// 是要有规范化 struct 的全部理由。
//
// 所以这里的映射是直接照抄，危险跟 OpenAI 那边正好反过来：在这条线上"帮忙"把
// cache_read 从 input 里减掉的适配器，每次热调用报出来的 prompt 都是负数。
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) normalise() Usage {
	return Usage{
		Input:      u.InputTokens,
		CacheWrite: u.CacheCreationInputTokens,
		CacheRead:  u.CacheReadInputTokens,
		Output:     u.OutputTokens,
		// Reasoning 保持 0：这个协议不报思考 token 的小计。思考 token 是真实存在
		// 的，就在 OutputTokens 里面——§A3a 里 max_tokens:10 返回了
		// output_tokens:4403，几乎全是一个 thinking 块——只是根本没有哪个字段说
		// 它有多少。报 0 的意思是"没报"，绝不是"一个都没花"。
	}
}

// anthropicBlockAccum 是一个内容块在传输途中的状态，以流里的 `index` 为键。
type anthropicBlockAccum struct {
	index int
	kind  string // "tool_use", "text", "thinking"
	id    string
	name  string
	args  strings.Builder
}

// anthropicHarnessResidue 判断一段 text delta 是不是网关漏出来的 `</think>` 标
// 签，而不是模型真想说的话。
//
// **这个决定**只说一遍，也只在一处执行：残留从用户可见的文本里**丢掉**，并
// **报出**一条 notice。不是悄悄吞掉，也不是渲染出来。
//
// 处理的是什么：这个网关的 thinking 抽取有时候会失败，闭合标签就漏进了真正的
// `text` 内容块。§A3b 在非流式下抓到过
// （`{"type":"text","text":"\n</think>\n\n"}`），§B6 在流式下抓到同样的东西，
// 位置是内容块 index 1。这不是模型的输出，是宿主从接缝里漏了出来。
//
// 渲染出来，用户的答案前面就顶着一个 `</think>`。一声不响地丢掉，trace 里就会
// 显示一段从未到达的文本，而且没人会知道这个网关是坏的。notice 两件事都办了：
// 终端保持干净，JSONL 里留着证据，还附上线上记录的出处。
//
// 这个判断故意做得**很窄**：整段 delta trim 完之后必须恰好就是那个标签。要是按
// 子串来（`strings.ReplaceAll(text, "</think>", "")`），模型正在解释 think 标
// 签怎么用的时候，那段话就被悄悄毁掉了——而这本来就是会去问 coding Agent 的东
// 西。为了替供应商擦垃圾而悄悄弄坏真实输出，比漏过一个野标签更糟。标签要是拆在
// 两段 delta 里，也照样能从这里溜过去；§B6 显示它是整个到的，而为了防一个从没
// 观察到的情形，把每段 text delta 都缓起来看它是不是半个标签，会给每个响应的每
// 个 token 都加上延迟。
func anthropicHarnessResidue(s string) bool {
	switch strings.TrimSpace(s) {
	case "</think>", "<think>":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// ParseStream
// ---------------------------------------------------------------------------

// ParseStream 吃掉一份 Anthropic 协议的 SSE 响应体，边到边发事件，最后返回拼
// 装好的结果。
//
// 它发出的事件种类跟 OpenAI 适配器一样，顺序一样，含义一样——正是这一点让每个
// 渲染器、trace 写入器和重放都彻底看不见协议。订阅者分不出自己正在画的这条流是
// 哪家供应商产的，而这就是目的。
//
// `started` 是**请求**发出去的时刻，不是这个函数被调用的时刻。TTFT 是往返的属
// 性；从响应头到达的那一刻起算，恰好把你想看的那段延迟藏掉了。
//
// 流中途失败时，这里把部分结果**和**错误一起返回。一条在完整工具调用之后才死
// 掉的流，跟一条什么都没产出的流，是两回事；而调用方只有拿到已经到达的东西，
// 才分得出来。
func (p *anthropicProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	res := &CallResult{}

	// emit 给每个事件盖上回合号，这样没有哪个调用点能忘掉它；同时容忍 bus 为
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
		thinking  strings.Builder
		blocks    = map[int]*anthropicBlockAccum{}
		firstSeen bool
	)

	// markFirstToken 只触发一次，在模型真实输出的第一个字节上。
	//
	// `ping` 明确不算，而在这个协议上这不是假设：§B6 记下了 ping 在
	// message_start **之前**就到了，所以从第一帧起算的 TTFT，量到的是一次
	// keep-alive。message_start 也不算，它不带内容。工具调用的结构算：tool_use
	// 的 content_block_start 意味着模型已经决定要调哪个工具——thinking 也算，在推
	// 理模型上它确实是最先生成出来的东西。
	markFirstToken := func() {
		if firstSeen {
			return
		}
		firstSeen = true
		res.TTFT = time.Since(started)
		emit(Event{Kind: KindFirstToken, Millis: res.TTFT.Milliseconds()})
	}

	// 可见文本**只走** addText 这一条路，所以 `</think>` 那个决定（见
	// anthropicHarnessResidue）只在一处做。有两个调用点能到这儿——
	// content_block_start，观察到的流里它从没带过文本，但规范说它会带；以及
	// text_delta，文本全是它带的——而同一个过滤器抄成两份，就是两个迟早会走
	// 偏的过滤器。
	addText := func(s string) {
		if s == "" {
			return
		}
		// 标记放在残留检查**之前**：字节确实到了，而 TTFT 量的是这一趟往返，不是
		// 对回来的东西下判断。
		markFirstToken()
		if anthropicHarnessResidue(s) {
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("dropped gateway harness residue from visible text: %q (docs/wire-notes.md §A3b, §B6)", s)})
			return
		}
		text.WriteString(s)
		emit(Event{Kind: KindTextDelta, Text: s})
	}

	// addThinking 自己走一条路，分开就是重点：§B7 警告过，把每个内容块都当文本
	// 处理的代码，会把模型的私有推理渲染给用户看。Kind 不同，意味着每个订阅者自
	// 己决定怎么办。
	addThinking := func(s string) {
		if s == "" {
			return
		}
		markFirstToken()
		thinking.WriteString(s)
		emit(Event{Kind: KindReasoningDelta, Text: s})
	}

	// blockAt 按 index 返回累加器；要是某个块的 content_block_start 从没见过，
	// delta 却来了，就当场建一个。这本该不可能发生；真发生了，把碎片留住也胜过因
	// 为丢了一帧就扔掉整个工具调用。
	blockAt := func(index int) *anthropicBlockAccum {
		b := blocks[index]
		if b == nil {
			b = &anthropicBlockAccum{index: index}
			blocks[index] = b
		}
		return b
	}

	err := readSSE(r, func(f sseFrame) error {
		payload := strings.TrimSpace(f.Data)
		if payload == "" {
			return nil
		}

		var ev anthropicStreamEvent
		if jerr := json.Unmarshal([]byte(payload), &ev); jerr != nil {
			// 回合里已经产出了合法的工具调用，一帧畸形不能把它整个毁掉。把这一帧报
			// 成一条 notice——在 trace 里看得见，在主循环里活得下去——然后继续
			// 走。在这里返回错误，是看起来更整齐、实际上更糟的那个选择。
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("skipped an SSE frame that was not JSON: %v (%.120s)", jerr, payload)})
			return nil
		}

		// 有两处说明这个事件叫什么：`event:` 行，和载荷自己的 `type`。以载荷为
		// 准，因为遇到会把分帧规范化掉的代理，活下来的是它；反过来的情形，
		// `event:` 行是兜底。观察到的每一帧里两者都一致，而它们哪天不一致，那天
		// 值得活下来。
		kind := ev.Type
		if kind == "" {
			kind = f.Name
		}

		switch kind {
		case "ping":
			// §B6：ping 把整条流夹在中间——message_start 之前一个，message_stop
			// 之后一个——此外也会作为普通 keep-alive 出现。任何位置都容忍，
			// 也一概不计。
			//
			// 收尾那个 ping 还是 `cost` 的藏身处，这就是这个解析器读过
			// message_stop 之后还继续读、而不是在那儿返回的原因（跟
			// sseDoneSentinel 在 OpenAI 那边给的理由一样：抽干不要钱，提前停手会丢
			// 数据，也丢掉那条 keep-alive 连接）。
			if len(ev.Cost) > 0 {
				if c := strings.Trim(string(ev.Cost), `"`); c != "" && c != "0" {
					// §C10 在这里只见过 "0"。非零的数字，会是这个端点头一次真的
					// 发出成本信号，所以它进 trace，而不是掉地上。
					emit(Event{Kind: KindNotice, Text: fmt.Sprintf("gateway reported cost %s on the trailing ping", c)})
				}
			}

		case "message_start":
			// **故意忽略**——包括、而且尤其是它的 usage。
			//
			// §B6 抓到过：**同一个请求**，message_start 报 input_tokens:56，
			// message_delta 报 input_tokens:291。同样 prompt 的非流式调用，跟 291
			// 一致。规范说 message_start 才是权威；在这个端点上它就是错的，而且它
			// 也从来不带缓存计数器，所以读它的解析器把 input 少报 5 倍，还永远报出
			// 零缓存命中率。
			//
			// message_delta 万一没来，也不会退回去读它。缺一个数字看得见，也追得下
			// 去；一个貌似合理的错数字，会进成本看板，然后就一直待在那儿。

		case "content_block_start":
			if ev.ContentBlock == nil {
				return nil
			}
			b := blockAt(ev.Index)
			b.kind = ev.ContentBlock.Type

			// 把 id/name 锁存下来：只有这个事件里有它们。
			if ev.ContentBlock.ID != "" {
				b.id = ev.ContentBlock.ID
			}
			if ev.ContentBlock.Name != "" {
				b.name = ev.ContentBlock.Name
			}

			switch b.kind {
			case "tool_use":
				// 调用一旦能认出来就宣布出去。这个事件上的 `input` 是 `{}`
				// （§B6），这里刻意不读它：参数在碎片里。
				markFirstToken()
				emit(Event{Kind: KindToolCallStart, ToolID: b.id, ToolName: b.name})

			case "text":
				// 每次观察到的都是 ""（§B6、§B7）。照样读，不假定它是空的
				// ——为了跟测试样本对齐而丢掉模型输出，网关一变，就会有一整
				// 段话不见了。
				addText(ev.ContentBlock.Text)

			case "thinking":
				addThinking(ev.ContentBlock.Thinking)
			}

		case "content_block_delta":
			if ev.Delta == nil {
				return nil
			}

			switch ev.Delta.Type {
			case "text_delta":
				addText(ev.Delta.Text)

			case "thinking_delta":
				addThinking(ev.Delta.Thinking)

			case "input_json_delta":
				b := blockAt(ev.Index)
				if b.kind == "" {
					b.kind = "tool_use"
				}
				// §B6：**第一个**碎片是空串。它什么都没带，所以既不算 token，也
				// 不占 trace 里的一行。
				if ev.Delta.PartialJSON == "" {
					return nil
				}
				markFirstToken()
				b.args.WriteString(ev.Delta.PartialJSON)
				emit(Event{
					Kind:     KindToolArgsDelta,
					ToolID:   b.id,
					ToolName: b.name,
					Text:     ev.Delta.PartialJSON,
				})

			case "signature_delta":
				// §B7：会发，永远是空的，没什么可带回去的。显式忽略，而不是掉进
				// default 分支，这样它不会在每个 thinking 块上都生成一条 notice
				// ——也让下一个读代码的人知道，这事想过了。

			default:
				emit(Event{Kind: KindNotice, Text: fmt.Sprintf("unknown content_block_delta type %q at index %d", ev.Delta.Type, ev.Index)})
			}

		case "content_block_stop":
			// 没什么要做的。这个块的内容已经攒好了，而它关掉的 index 之后可能在更
			// 靠后的 index 上为另一个块重新打开。工具参数只解析一次，由调用方在整
			// 条流结束之后解析——碎片边界不是 JSON 边界（§B6），这个事件也不是。

		case "message_delta":
			// **这条流上唯一可信的一帧**。停止原因和每一个 usage 数字——包括别处
			// 根本不出现的缓存计数器——都出自这里，别处都没有。
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				// 是锁存，不是赋值：否则第二条 stop_reason 为 null 的
				// message_delta，会把那个真正有用的值擦掉。
				res.RawStop = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				res.Usage = ev.Usage.normalise()
				// 发出去的是**副本**。递出 &res.Usage，等于让事件别名到某个字段
				// 上，而调用方随后还能往那个字段里写；懒序列化的订阅者，记下的就
				// 会是它后来变成的样子。
				sent := res.Usage
				emit(Event{Kind: KindUsage, Usage: &sent})
			}

		case "message_stop":
			// **不是**停止读取的理由。§B6 记下了它后面还有一个 ping，带着
			// `cost`；而这个协议上根本没有 `[DONE]` 哨兵——流是在连接关闭时结束
			// 的，readSSE 把它报成 EOF。在这里返回，会丢下一个还剩着字节的响应体，
			// 这也让 HTTP 传输层不再把连接还回池里：接下来整个会话，每个回合都要重
			// 做一次 TLS 握手，什么都换不回来。

		case "error":
			// 这个网关上没观察到（§D11 的错误全是在流打开之前以 HTTP 状态到达
			// 的），但规范会在响应体中途流出 overloaded_error 和 api_error。返回错
			// 误会让 readSSE 停下，然后这个函数的尾部返回部分结果，**不带**
			// KindResponseEnd。
			if ev.Error != nil {
				return fmt.Errorf("anthropic: stream error: %s: %s", ev.Error.Type, ev.Error.Message)
			}
			return fmt.Errorf("anthropic: stream error with no error object: %.200s", payload)

		default:
			// 新出现的事件类型是信息，不是失败。发一条 notice，它就落进 trace 里，
			// 有人能读到；一声不响地忽略掉，协议改了也能一个月没人发现。
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("ignored unknown stream event %q", kind)})
		}

		return nil
	})

	res.Text = text.String()
	res.Thinking = thinking.String()

	// 工具调用按**块 index 升序**排，不是按到达顺序。§B6 那条两个调用的流，把
	// tool_use 放在 index 0 和 2，中间夹一个 text 块；而 Go 是故意把 map 遍历顺
	// 序随机化的，所以少了这次排序，并行工具调用的顺序就逐次不同——这种 bug 一
	// 周复现一次，然后被算到模型头上。
	var indices []int
	for i, b := range blocks {
		if b.kind == "tool_use" {
			indices = append(indices, i)
		}
	}
	sort.Ints(indices)
	for _, i := range indices {
		b := blocks[i]
		res.Calls = append(res.Calls, Block{
			Kind: BlockToolCall,
			ID:   b.id,
			Name: b.name,
			Args: b.args.String(),
		})
	}

	// RawStop 留着线上那个原字符串，Stop 留规范化之后的，两者之间的落差就是证据
	// （provider.go 讲了为什么）。§A3c 是这个协议上要在意它的具体理由：在
	// max_tokens 处被截断的工具调用，来的时候 stop_reason 是 "tool_use"，`input`
	// 却没法用，所以 RawStop 永远不能是调用方唯一检查的东西。
	//
	// normaliseStop 无条件跑，"" 也跑：没等到 message_delta 就结束的流会映射成
	// StopUnknown，Agent 主循环会把它报出来，而不是继续往下走。把 Stop 留成空
	// 串，等于凭空造出第四种状态，而没有哪个 switch 处理得了它。
	res.Stop = normaliseStop(res.RawStop)

	if err != nil {
		// 不发 KindResponseEnd：响应不是结束了，是断了。发一个出去，等于对
		// 每个订阅者说一句干干净净的谎，而 trace 本该是证据。
		return res, err
	}

	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.RawStop, // 线上那个原字符串，不是规范化之后的
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}

// ---------------------------------------------------------------------------
// 阶段 04——断点摆在哪儿，为什么摆那儿。
//
// 渲染出来的 prompt 依次是 `tools`、`system`、`messages`，就这个顺
// 序，而缓存匹配的是**前缀**：cache_control 标记的意思是"到这里为止
// 的东西是一段可复用的前缀"。两个后果立刻跟着来，纪律全在这里：
//
//   - 标记只有在它**之前**的一切下次还逐字节一样时，才帮得上忙。
//   - 前面某个字节一变，它后面的每个标记就全作废，所以内容按从稳定
//     到易变排序，比标记本身还重要。
//
// 每个请求最多允许四个标记。这个适配器摆了两个，也正是 Agent 真正需
// 要的两个：
//
//	tools ─────────┐
//	system ────────┴─▶ [1] 整场会话都冻住
//	messages
//	  turn 1 …
//	  turn N ──────────▶ [2] 滚动：一直到最新回合为止的全部
//
// 标记 1 从第二个请求起就把自己赚回来了。标记 2 才是 Agent 里要紧的
// 那个：每个回合都会重发整段对话，没有它，每个回合都按全价把整段历
// 史再读一遍——就是阶段 00 量到的 3.7 倍重发比。
// ---------------------------------------------------------------------------

// systemBlocks 渲染系统提示词，并把它钉成可缓存前缀。
//
// 因为 `tools` 渲染在 `system` 前面，最后那个 system 块上只要有一个
// 标记，就把**两者都**缓存了。这就是工具列表必须确定的全部原因：把某
// 个工具换个位置，就改掉了位置零上的字节，于是一切作废，这个标记也
// 跟着废。
func (p *anthropicProvider) systemBlocks(system string) []anthropicContent {
	if system == "" {
		return nil
	}
	b := anthropicContent{Type: "text", Text: system}
	if p.cacheBreakpoints {
		b.CacheControl = ephemeral()
	}
	return []anthropicContent{b}
}

// markRollingBreakpoint 钉住到目前为止的对话，办法是给最后一条消息
// 的最后一个内容块打标记。
//
// 为什么是**最后一条**消息的**最后一个**块，而不是固定位置：每个回
// 合都往后追加，标记跟着一起走，于是回合 N 读到的是回合 N-1 写下的
// 前缀。停在固定偏移上的标记不会随对话一起长，每个回合能缓存的部分
// 只会越来越少。
//
// 20 块回溯是这里的陷阱。断点往回找有限个内容块，看有没有现成的条
// 目；而 Agent 一个回合里要是并发点起一大堆工具，一口气加的块就可能
// 比这还多——之后下一个标记就悄没声地什么都找不到，你付全价，没有报
// 错，也没有警告。一个回合一个工具，离窗口边界还远得很；扇出型的
// Agent 需要一个中间标记，四个槽位里还空着两个，就是为了这个。
func markRollingBreakpoint(msgs []anthropicMessage) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	if len(last.Content) == 0 {
		return
	}
	last.Content[len(last.Content)-1].CacheControl = ephemeral()
}
