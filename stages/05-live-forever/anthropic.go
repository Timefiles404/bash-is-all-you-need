// 阶段 03——Anthropic 协议适配器。
//
// Babel 的另一半。这个文件和 openai.go 实现同一个接口，
// 由同一个循环驱动，几乎在所有事上都不一致：
//
//	                 OpenAI                    Anthropic（这个文件）
//	system prompt    messages[0]               一个顶层 `system` 字段
//	tool results     一个 role:"tool" message   ONE user message 里的 tool_result 块
//	tool arguments   一个 JSON *string*        一个 `input` JSON *object*
//	tool schema      嵌套在 `function` 下      平的，`input_schema`
//	stop reason      finish_reason             stop_reason
//	cached tokens    在 prompt_tokens 里       *额外的* input_tokens
//	stream end       一个 `[DONE]` 哨兵        连接关闭
//
// 这七个词汇表都不出现在这个文件和 openai.go 之外的任何地方。
// 这就是这个阶段的架构主张：供应商的词在适配器边界处停止。
//
// 下面处理的每一个偏差，都在 docs/wire-notes.md 里有据可查。
// 在观察到的字节和发布的规范不一致的地方——而且在这个端点它们在
// 半打独立地方不一致——观察赢了，因为观察就是在凌晨 3 点会在线上
// 出现的东西。
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

// anthropicVersion 是规范要求的一个请求头，这个网关实际并不要求它
// （§D11：不带它调用也会成功）。还是照发不误。省掉它，今天成本是零；
// 但将来有一天，有人把这个 Agent 指向真实的 api.anthropic.com，
// 遇到一个关于某个请求头的报错——而代码里根本没有这个请求头可以
// grep——那一天就要搭上一个下午。
const anthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens 在调用者传非正数预算时用。`max_tokens`
// 在这个协议上是强制的，§D11 记录了省掉它买到的确切东西：HTTP 400
// 加上 body `{"model":"qwen3.7-plus"}`——没有 `type`，没有 `error`，
// 没有消息。记录 `resp.Error.Message` 的代码记录空字符串。在这里
// 默认不是礼貌，而是可诊断失败和无声失败之间的区别。
const anthropicDefaultMaxTokens = 4096

// ---------------------------------------------------------------------------
// 供应商
// ---------------------------------------------------------------------------

// anthropicProvider 讲的是 Messages 协议。
//
// 它故意不持有 *http.Client。BuildRequest 返回一个请求，ParseStream
// 读一个 io.Reader，所以这个类型根本不执行任何 I/O。传输策略——超时、
// 代理、重定向、连接池——对两个协议完全相同，属于调用者；每个适配器
// 各自一个 client，就是两个可能忘记设置超时的地方。副作用是两个
// 适配器都纯净到足以从 strings.Reader 驱动，这就是为什么
// anthropic_test.go 不需要网络也不需要 API 密钥。
type anthropicProvider struct {
	baseURL string
	apiKey  string
	model   string

	// cacheBreakpoints 打开 cache_control 放置。
	// 它作为开关存在，只是为了这一章可以
	// 测量差异；没有理由运行一个把它关掉的 Agent。
	cacheBreakpoints bool
}

func newAnthropicProvider(baseURL, apiKey, model string) *anthropicProvider {
	return &anthropicProvider{
		// AGENT_BASE_URL 里的末尾斜杠会渲染成"{base}//messages"。
		// 有些网关 404 它；这一个用泛型"Internal server error"封装答 500（§D11）——
		// 由.env 文件里一个字符引起的一小时调试。修剪一次，在这里，
		// 而不是让每个配置都小心。
		baseURL:          strings.TrimRight(baseURL, "/"),
		apiKey:           apiKey,
		model:            model,
		cacheBreakpoints: true,
	}
}

// withCacheBreakpoints 切换前缀固定。--no-cache 用它来生成
// docs/04-the-cache.md 那个实验里的对照组。
func (p *anthropicProvider) withCacheBreakpoints(on bool) *anthropicProvider {
	p.cacheBreakpoints = on
	return p
}

func (p *anthropicProvider) Protocol() string { return "anthropic" }
func (p *anthropicProvider) Model() string    { return p.model }

// ---------------------------------------------------------------------------
// 请求线上格式
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`

	// System 是一个**顶级字段**，不是一条消息。这是两个协议之间
	// 最可见的差异，以及为什么 Provider.BuildRequest 把系统
	// 提示词当作自己的参数的原因：中立形式无法选择任一形状而
	// 不把一个供应商的设计走私到核心。
	//
	// 文本块的**数组**，不是 stage 03 使用的纯字符串。那个改变
	// 是这一章：一个块可以携带 `cache_control`，字符串无法。
	// §C8 测量了升级把一个运行到运行可变的 64-token-块命中
	// 变成一个稳定的精确前缀命中 9,775 token。
	System []anthropicContent `json:"system,omitempty"`

	Messages []anthropicMessage `json:"messages"`

	// 工具在这个协议上不携带 `function` 包装。为空时完全省略，而不是作为
	// `[]` 发送，因为一个存在但为空的 tool array，和完全没有的 tool array，
	// 是两个不同的 prompt 前缀——而不同的前缀，就是一次缓存未命中。
	Tools []anthropicTool `json:"tools,omitempty"`

	Stream bool `json:"stream"`
}

type anthropicMessage struct {
	Role string `json:"role"`

	// Content 总是块的数组，从来不是规范也允许的那种字符串简写。一个形状
	// 对应一条代码路径：简写需要一个自定义 marshaller，而且只要有一条
	// message 携带工具结果或工具调用，它反正也得变成数组。
	Content []anthropicContent `json:"content"`
}

// anthropicContent 是一个 content 块。用一个 struct，每种块类型对应一个
// omitempty 字段，而不是用接口——出于和 Event 是一个扁平 struct
// （events.go）相同的理由：JSON 在请求检查器里保持可读，新增一种
// 块类型只是加一个字段，而不是加一个类型再加一个 marshaller。
type anthropicContent struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`

	// Input 是**原始字节**，原封不动地拼接过去。
	//
	// 中立的 Block.Args 是一个原始 JSON 字符串（provider.go 说明为什么）；
	// 这个协议想要一个对象。解码到 map[string]any 再重新编码，会产生
	// 一个等价对象，但键的顺序不同——Go 会排序 map 键，而模型是按自己的
	// 顺序发出它们的——字节序列一变，prompt 前缀就跟着变，这在每次重放
	// 的回合上都是一次缓存未命中。json.RawMessage 是唯一能什么都不做、
	// 就把字符串变成对象的字段类型。
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`

	// CacheControl 把这个块标记为一段可缓存前缀的结束。
	//
	// 这是一个带 omitempty 的指针，这样一个未标记的块，序列化
	// 出来的字节就和它在 stage 04 之前完全一样。这一点比看起来
	// 更重要：如果加上这个特性改变了每个*未标记*块的字节，打开
	// 缓存反而会让它本该保留的那段前缀失效。
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl 固定一个前缀。
// `ephemeral` 是唯一的类型；5 分钟 TTL
// 是默认的，在响应中显示为嵌套的
// `cache_creation.ephemeral_5m_input_tokens`
// 计数器（wire-notes §C8）。
type anthropicCacheControl struct {
	Type string `json:"type"`
}

func ephemeral() *anthropicCacheControl { return &anthropicCacheControl{Type: "ephemeral"} }

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// InputSchema，不是 `parameters`，周围没有 `function` 对象。里面的 JSON
	// Schema 是完全相同的；只有包装不同。
	//
	// 尽管 Go 的随机化 map 迭代，map 在这里是安全的：encoding/json 在排列时
	// 排序 map 键，所以渲染的模式从运行到运行是字节稳定。这一点比看上去更要
	// 紧——tool 块坐在 prompt 前面附近，在缓存前缀内，所以一个不稳定的渲染会
	// 无声地在每个单一请求上花费一个完整的缓存写。
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicRawArguments 镜像了这个网关自己在工具调用被截断时
// 产生的合成对象（§A3c：`input` 被替换成
// `{"raw_arguments":"<invalid JSON text>"}`）。
// 看 anthropicToolInput。
type anthropicRawArguments struct {
	RawArguments string `json:"raw_arguments"`
}

// ---------------------------------------------------------------------------
// BuildRequest
// ---------------------------------------------------------------------------

// BuildRequest 把中立对话渲染到这条线上。
//
// 它把排列好的 body 和请求一起返回，因为调用者会把它作为 KindRequest
// 发出——这是请求检查器，也是 trace 里唯一记录"模型实际看到了什么"的
// 地方。把它读回请求会意味着排空 req.Body 并重建它，所以字节直接交付。
//
// 这个适配器不自己发出事件：BuildRequest 没有 bus，而给它一个 bus，
// 会让"纯函数，无 I/O，无副作用"这个让两个适配器都可测试的属性，
// 为了一点方便就消失掉。
func (p *anthropicProvider) BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error) {
	if len(msgs) == 0 {
		// 网关对这个的答案是一个没有错误封装的 400（§D11）。在这里失败，
		// 这样消息至少能说点有用的东西。
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

	// Header.Set 会把这些规范化成 X-Api-Key / Anthropic-Version 再发出去。
	// HTTP 字段名是大小写不敏感的，所以这样做是正确的、也是无形的；
	// 这里提一句，只是因为读者拿这个和文档对比时，会在线上看到不同的大小写。
	//
	// 注意认证方案：`x-api-key`，不是 `Authorization: Bearer`。
	// 在这里发送 OpenAI 的请求头，会产生 §D11 里的 AuthError 封装，
	// 带着"Missing API key."——读起来像一个配置问题，实际上是协议混淆。
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")

	return req, body, nil
}

// anthropicMarshal 编码 body，其中 HTML 转义被**关闭**。
//
// 这不是为了好看。json.Marshal 会把 `<`、`>` 和 `&` 转义成它们的
// 六字符 Unicode 转义（u003c、u003e、u0026，每个后面跟一个反斜杠）——
// 这条规则存在，是为了让 JSON 文档能贴进 HTML 的 script 标签里
// 而不会提前把它闭合。这个 Agent 的整个工作就是运行 shell 命令，
// 而 shell 命令大多数就是那三个字符：`2>&1`、`>/tmp/out`、`<<EOF`。
// 转义之后，它们在语义上完全相同、字节上却不同，这意味着：
//
//   - 请求检查器给用户看到的是 `ls > /tmp/out`，而且
//   - 重放的工具调用里一出现重定向，缓存前缀就跟着改变——毫无道理。
//
// Encoder.Encode 还会追加一个 json.Marshal 不加的换行；这个换行会被
// 修剪掉，所以 KindRequest 字节正好就是被 POSTed 的字节。
//
// 有一样东西这里**不会**保留：拼接进来的 json.RawMessage 内部那些
// 无意义的空白。encoding/json 会把它压紧，所以模型给出的
// `{"command": "ls"}` 会被发送成 `{"command":"ls"}`。**键的顺序**——
// 真正会打破缓存的部分，也是如果 args 被折腾一圈进出 map、Go 就会
// 打乱的部分——却完完整整地保留了下来。
func anthropicMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// anthropicTools 渲染工具定义。是平的：{name, description, input_schema}。
// OpenAI 适配器把同样这三个字段包进 {"type":"function","function":{...}}
// 里，这就是全部区别。
func anthropicTools(tools []Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema
		if schema == nil {
			// `input_schema` 是必需的，并且必须描述一个对象。一个不取参数的
			// 工具仍然需要这个封装，这里发送 `null`，在真实 API 上是一个 400。
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

// anthropicMessages 是这个文件最可能弄错的翻译，所以它是最多
// 测试的那个。
//
// 中立形式没有 RoleTool（provider.go 说明为什么）。工具结果是
// 一个*块*，这个协议用 ONE user message 里的 N tool_result 块
// 答 N 工具调用——不是 N messages，这是 OpenAI 适配器发出的。
// 把它们作为独立消息发送，会产生连续的用户回合，而且在真实 API 上，
// 会触发一个关于 tool_use 块没有匹配结果的错误。
//
// 所以工具结果积累到 `pending` 并被冲成一个单一 user message，
// 而运行被下一个来的东西关闭——包括对话的末尾，那是常见情况：
// 当循环回到模型时 `msgs` 里最后的东西正是新工具结果的一次运行。
//
// 两个顺序规则被烤入，两个都被协议要求：
//
//   - tool_result 块**首先**出现在它们的 user message 里，在同一回合
//     携带的任何文本之前；
//   - 如果运行紧接着被它自己的一个用户消息跟踪，两个合并而不是
//     产生两个连续 user 回合。
func anthropicMessages(msgs []Msg) ([]anthropicMessage, error) {
	var (
		out     []anthropicMessage
		pending []anthropicContent // 还没被冲的 tool_result 块
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
			// 大声，不宽松。系统提示词是这个协议上的顶层字段，
			// Provider.BuildRequest 正是因为这个理由才把它单独传递；
			// 一个系统 Msg 在这里意思是调用者以 OpenAI 的方式建立了对话。
			// 无声地把它重新标记成"user"，会发出一个微妙不同的 prompt，
			// 产生一个微妙更糟的 Agent——这是最难被发现的一类 bug。
			return nil, fmt.Errorf("anthropic: a system message in msgs — this protocol takes the system prompt as a top-level field, pass it as BuildRequest's system argument")
		}

		var own []anthropicContent

		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolResult:
				pending = append(pending, anthropicContent{
					Type:      "tool_result",
					ToolUseID: b.ID,
					// Content 是一个纯字符串。规范这里也允许一个块数组（为了图像，
					// 或为了 is_error），而这个 Agent 从来就只有一件事要说：
					// shell 打印了什么。
					Content: b.Text,
				})

			case BlockText:
				// 空文本块被真实 API 拒绝（"text content blocks must be non-empty"），
				// 而一个空块无论如何不携带任何东西。
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
				// **故意删除**，这是一个决定，不是疏忽。
				//
				// 规范说一个思考块必须用模型返回的 `signature` 重放，否则 API
				// 会拒绝它。在这个端点上 signature 总是空字符串——在非流响应里
				// （§A3b），在 `signature_delta` 帧里（§B7），处处如此。没有
				// signature 可以往返，所以一个被重放的思考块，就是一个无法通过
				// 验证的块。
				//
				// 什么都不发送，会让模型的私密推理从下一回合的上下文里消失，
				// 这是一个真实的代价。发送一个未签名的块，冒的风险是一个会杀死
				// 整个对话的 400。trace 里仍然留着每一个思考 token
				// （KindReasoningDelta），所以记录里什么都没丢——只是 prompt 里丢了。
			}
		}

		if len(own) == 0 {
			// 一个渲染成什么都没有的消息，一定不能变成一个空 content
			// 数组：`content: []` 在真实 API 上是 400，而一个纯思考
			// 的 assistant 回合，渲染出来的正好就是这个。
			continue
		}

		if len(pending) > 0 && m.Role == RoleUser {
			// 合并而不是冲掉：连续两条 user 消息，是这个协议不喜欢的形状，
			// 而 tool_result 块必须首先出现在携带它们的消息里。
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

// anthropicToolInput 把中立的原始 JSON 字符串 Args 转换成这个
// 协议的 `input` 对象，原封不动地传递字节。
//
// 两个边界情况是有趣的部分：
//
//   - 空 Args。一个调用零参数工具的模型发送""，而 `input` 是
//     必需的。`{}` 是诚实的渲染。
//
//   - 不是有效 JSON 的 Args。§A3c 是这可能发生的原因：
//     一个工具调用在 max_tokens 处被截断，回来的 `input` 被替换成
//     `{"raw_arguments":"{\"command\": \"find"}`——真正无效的
//     JSON，在字符串中途未终止——而 `stop_reason` 仍然高兴地
//     说着"tool_use"。如果这东西哪天往返回了一个请求里，原样
//     拼接它就会产生一个畸形 body，而 §D11 记录了这个网关对畸形
//     body 做什么：HTTP 500，"Internal server error"。一个客户端
//     bug 穿着服务器故障的衣服，而一个绑定在 5xx 上的重试策略
//     会永远重试下去。
//
//     所以无效字节被包进网关自己的截断形状里。body 保持有效，
//     证据原封不动地活在字符串里，而模型看到的是这个端点本就
//     会产生的结构。
func anthropicToolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(trimmed)) {
		wrapped, err := json.Marshal(anthropicRawArguments{RawArguments: args})
		if err != nil {
			// 给一个只含一个字符串字段的 struct 编码，不可能失败；如果它不知怎样
			// 还是失败了，一个空对象仍然是有效请求。
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(wrapped)
	}
	return json.RawMessage(trimmed)
}

// ---------------------------------------------------------------------------
// 流线上格式
// ---------------------------------------------------------------------------

// anthropicStreamEvent 是一个 `data:` 有效载荷。这个协议上的每个
// 事件类型都解码到这同一个 struct——另一种做法是两遍解码（先读
// `type`，再解析进正确的 struct），那会让每一帧的解析成本翻倍，
// 只为省下几个用不到的指针字段。
//
// 指针很重要：`Delta` 既出现在 content_block_delta 上（携带
// text/thinking/partial_json），也出现在 message_delta 上（携带
// stop_reason），而 nil，就是让"这个事件根本没有 delta"和"它的
// delta 是空的"两者保持可区分的办法。
type anthropicStreamEvent struct {
	Type string `json:"type"`

	// Index 把一个 content_block_* 事件绑到它的块。并行的工具调用会
	// 交错到达，所以这是唯一能让一次调用的参数片段不会混进另一次
	// 调用缓冲区的东西——和 `index` 在 OpenAI 适配器的 tool_calls
	// 数组里扮演的角色一样。
	Index int `json:"index"`

	// Message 只出现在 message_start 上。它的 Usage **没有任何东西读取**；
	// 原因见下面的循环。
	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`

	ContentBlock *anthropicStreamBlock `json:"content_block"`
	Delta        *anthropicStreamDelta `json:"delta"`

	// Usage 在 message_delta 上——这条线上唯一可信的 Usage。
	Usage *anthropicUsage `json:"usage"`

	// Cost 是一个偷运到尾部 ping 上的非标准键（§B6，§C10）。
	//
	// 故意把类型定成 RawMessage，而不是字符串：§C10 发现它总是
	// 一个 JSON *string*（"0"），而如果它哪天以数字形式到达，一个
	// `string` 字段就会解析失败——拖垮**整个**帧，而不只是这个字段。
	// 一个可选的非标准键，绝不能有能力打破它周围一切的解析。
	Cost json.RawMessage `json:"cost"`

	// Error 在 `event: error` 帧上出现。在这个网关上未观察到过（§D11
	// 的错误都在流打开之前以 HTTP 状态码的形式到达），但规范里
	// overloaded_error 和 api_error 是会在流的中途出现的，而一个中途
	// 死掉的流，绝不能被记录成一个正常结束的流。
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStreamBlock struct {
	Type string `json:"type"` // "text"、"thinking"、"tool_use"

	// ID 和 Name **只在这里**到达，别处没有——content_block_start
	// 是唯一会给工具调用命名的事件。错过它，你手上就会有一堆无法
	// 归属的参数，和一个没法拿来回应的 tool_use_id。
	ID   string `json:"id"`
	Name string `json:"name"`

	// Text 和 Thinking 在每个观察的 content_block_start 上是""
	// （§B6，§B7）；content 作为 delta 到达。无论如何读——看循环。
	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// Input 在这里永远是**空对象**（§B6）。真正的参数是以
	// input_json_delta 片段的形式到达的。一个信任这个字段的解析器，
	// 对每个工具调用得到的都是 `{}`，什么都不会执行。
	Input json.RawMessage `json:"input"`
}

// anthropicStreamDelta 覆盖两个 delta 形状：content_block_delta
// 有效载荷（Type 是 text_delta / thinking_delta / input_json_delta /
// signature_delta）以及 message_delta 有效载荷（StopReason）。
type anthropicStreamDelta struct {
	Type string `json:"type"`

	Text     string `json:"text"`     // text_delta
	Thinking string `json:"thinking"` // thinking_delta

	// PartialJSON 片段**不是** JSON-aligned。§B6 记录了观察到的拆分：
	// ""、`{"command": "ls`、` -la /srv`、`/app`、`"`、`}`——第一个是
	// 空的，第四个在路径中途结束，第五个接着把它续上。没有任何一个
	// 片段能在当时被解析，所以它们被原样拼接起来，由调用者在流结束后
	// 统一解析恰好一次。
	PartialJSON string `json:"partial_json"`

	// Signature 在这个端点上总是""（§B7），包括在 signature_delta
	// 帧里。这个帧的存在只是为了满足形状，不携带任何东西，所以没有
	// 思考块需要验证或重放。
	Signature string `json:"signature"`

	StopReason   string `json:"stop_reason"`   // message_delta
	StopSequence string `json:"stop_sequence"` // 在这个网关上完全不存在
}

// anthropicUsage 是这个协议的 token 记账，按这个协议自己的方向来——
// **正好和 OpenAI 相反**。
//
// 这里 `input_tokens` **只是**未缓存的余数，缓存计数器是*额外*加上去的
// （§C8：input_tokens 18，cache_read 9,775，对应一个 ~9,800-token
// 的 prompt）。在 OpenAI 那边，`prompt_tokens` 是完整总数，而
// `cached_tokens` **嵌套在它里面**，所以那个适配器得做减法。同样一次
// 缓存命中，两套相反的算术，一个正常化 struct——这就是要有一个
// 正常化 struct 的全部理由。
//
// 所以这里的映射是直接拷贝，而危险正好和 OpenAI 那边相反：一个
// "乐于助人"、从 input 里减去 cache_read 的适配器，会在这条线上的
// 每次温暖调用中，报告出一个负数的 prompt。
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
		// Reasoning 停在 0：这个协议报告没有思考-token 小计。
		// 思考 token 是真的并且在 OutputTokens 里——§A3a 显示
		// max_tokens:10 返回 output_tokens:4403，几乎全部是一个
		// 思考块——只是没有字段说有多少。
		// 报告 0 意思是"未报告"，从不"无花费"。
	}
}

// anthropicBlockAccum 是一个 content 块的飞行中状态，以流的
// `index` 为键。
type anthropicBlockAccum struct {
	index int
	kind  string // "tool_use"、"text"、"thinking"
	id    string
	name  string
	args  strings.Builder
}

// anthropicHarnessResidue 报告一个文本 delta 是否是网关泄露的
// `</think>` 标签，而不是模型想说的什么。
//
// **决定**，只声明一次，并在一处强制执行：残留会被从用户可见文本里
// **删掉，并报告**为一条通知。不是无声吞掉，也不是渲染出来。
//
// 这里要处理的是：这个网关的思考提取有时会失败，导致结束标签漏到
// 一个真正的 `text` content 块里。§A3b 在非流式下抓到过它
// （`{"type":"text","text":"\n</think>\n\n"}`），§B6 在流式下抓到了
// 同样的东西，出现在 content 块索引 1 处。这不是模型的输出；是宿主
// 从接缝里泄漏出来的东西。
//
// 渲染它，就会把 `</think>` 摆在用户答案的前面。一声不吭地删掉它，
// 就意味着 trace 里会出现从未真正抵达过的文本，而且没人会知道这个
// 网关是坏的。一个通知同时做了两件事：终端保持干净，而 JSONL
// 保留着证据，并指向对应的 wire note。
//
// 测试故意收得很**窄**：整个 delta 去掉首尾空白后，必须刚好就是
// 这个标签。一个子字符串规则（`strings.ReplaceAll(text, "</think>", "")`）
// 会无声地弄乱一个模型对 think 标签如何工作的解释——这对一个编码
// Agent 来说是真实会被问到的东西——为了清理供应商产生的垃圾，
// 却悄悄腐蚀了真实的输出，这比放过一个偶尔出现的杂散标签，是更糟
// 的失败。一个跨两个 delta 拆开的标签，也会从这套检测里溜过去；
// §B6 显示它是完整到达的，而为了防着它可能是半个标签就缓冲每一个
// 文本 delta，会给每次响应的每一个 token 都添上延迟，去捕捉一种
// 从未被观察到的情况。
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

// ParseStream 消耗一个 Anthropic 协议的 SSE body，事件一到达就
// 发出，并返回组装好的结果。
//
// 它发出的事件种类和 OpenAI 适配器一样，顺序一样，含义也一样——
// 这就是让每一个渲染器、trace 写入器和重放都能对协议完全无感的
// 原因。一个订户看不出自己在读取的这个流是哪个供应商产生的，
// 而这正是要点所在。
//
// `started` 指的是**请求**发出的那一刻，不是这个函数被调用的那一刻。
// TTFT 是一个往返属性；从响应头到达的那一刻开始测量，恰恰会把你
// 想看的那部分延迟藏起来。
//
// 在一次流中途的故障上，这个函数会**同时**返回部分结果和错误。
// 一个在完整工具调用之后才断掉的流，和一个什么都没产出的流，
// 是两种不同的情况，调用者只有拿到了确实到达的那部分内容，
// 才能把两者区分开。
func (p *anthropicProvider) ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error) {
	res := &CallResult{}

	// emit 会在每个事件上盖上回合戳，这样就没有调用点能忘记它；
	// 同时容忍一个 nil bus，好让这个解析器可以被当作纯函数使用。
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

	// markFirstToken 只开火一次，在真实模型输出的第一个字节上。
	//
	// `ping` 明确不计数，而在这个协议上，这不只是理论上的可能：
	// §B6 记录到一个 ping 在 message_start **之前**到达，所以如果
	// TTFT 是从第一帧开始测量的，测到的其实是一次 keepalive。
	// message_start 也不计数，它不携带任何内容。工具调用的结构会
	// 计数——一个 tool_use 的 content_block_start，就是模型已经
	// 决定好要调用哪个工具——思考同样计数，这在一个推理模型上，
	// 是真正意义上第一个被生成的东西。
	markFirstToken := func() {
		if firstSeen {
			return
		}
		firstSeen = true
		res.TTFT = time.Since(started)
		emit(Event{Kind: KindFirstToken, Millis: res.TTFT.Milliseconds()})
	}

	// addText 是可见文本会经过的**唯一**路径，所以 `</think>` 的决定
	// （见 anthropicHarnessResidue）只在这一个地方做出。两个调用点会
	// 走到这里——content_block_start，在任何观察到的流里都没携带过
	// 文本，但规范上说它应该携带——以及 text_delta，携带了全部文本——
	// 而一份过滤逻辑如果抄成两份，迟早会彼此走样。
	addText := func(s string) {
		if s == "" {
			return
		}
		// **在残留检查之前**标记：字节确实到达了，TTFT 衡量的是往返
		// 本身，不是对"回来的是什么"的判断。
		markFirstToken()
		if anthropicHarnessResidue(s) {
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("dropped gateway harness residue from visible text: %q (docs/wire-notes.md §A3b, §B6)", s)})
			return
		}
		text.WriteString(s)
		emit(Event{Kind: KindTextDelta, Text: s})
	}

	// addThinking 是它自己的一条路径，分离开来就是要点：§B7 警告过，
	// 把每个 content 块都当文本处理的代码，会把模型的私密推理原样
	// 渲染给用户看。不同的 Kind 意味着由每个订户自己来决定。
	addThinking := func(s string) {
		if s == "" {
			return
		}
		markFirstToken()
		thinking.WriteString(s)
		emit(Event{Kind: KindReasoningDelta, Text: s})
	}

	// blockAt 返回某个索引的累积器；如果一个 delta 到达时，它所属的块，
	// 其 content_block_start 从未出现过，就顺手创建一个。这按理不该
	// 发生；但真发生了的话，留着这些片段，好过因为丢了一帧，就把
	// 整个工具调用扔掉。
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
			// 一个畸形帧，绝不能摧毁一个已经产生了有效工具调用的回合。把它
			// 作为通知呈现出来——在 trace 里可见，在循环里挺得过去——然后
			// 继续。在这里返回错误，是那种看起来更干净、实际却更糟的选择。
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("skipped an SSE frame that was not JSON: %v (%.120s)", jerr, payload)})
			return nil
		}

		// 两个来源为事件命名：`event:` 行，和有效载荷自己的 `type`。
		// 有效载荷胜出，因为就算遇到一个会重新规范化分帧方式的代理，
		// 它也能活下来；`event:` 行是反过来那种情况下的兜底。它们在
		// 每一个观察到的帧里都是一致的，而它们不一致的那一天，是值得
		// 好好挺过去的一天。
		kind := ev.Type
		if kind == "" {
			kind = f.Name
		}

		switch kind {
		case "ping":
			// §B6：ping 把整个流夹在两头——一个在 message_start 前，一个在
			// message_stop 后——此外也会作为普通 keepalive 出现。出现在任何
			// 位置都可以容忍，且不计入任何计数。
			//
			// 尾部也是 `cost` 藏身的地方，这就是这个解析器在 message_stop
			// 之后仍然继续读、而不是当场返回的原因（这和 sseDoneSentinel 在
			// OpenAI 那边讲的是同一个道理：排空是免费的，而早停既丢数据，
			// 也丢掉那个保活连接）。
			if len(ev.Cost) > 0 {
				if c := strings.Trim(string(ev.Cost), `"`); c != "" && c != "0" {
					// §C10 在这里只见过"0"。一个非零的数值，会是这个端点第一次发出
					// 真实的成本信号，所以它会被记进 trace，而不是被扔掉。
					emit(Event{Kind: KindNotice, Text: fmt.Sprintf("gateway reported cost %s on the trailing ping", c)})
				}
			}

		case "message_start":
			// **故意忽略**——包括，尤其，它的 Usage。
			//
			// §B6 捕捉到 message_start 报告 input_tokens:56，而 message_delta
			// 对**同一个请求**报告的是 input_tokens:291。用相同 prompt 发起的
			// 非流式调用，结果和 291 一致。规范说 message_start 才是权威来源；
			// 在这个端点上，它就是错的，而且它也从不携带缓存计数器，所以一个
			// 读取它的解析器，会把 input 少报 5 倍，并且永远报出零缓存命中率。
			//
			// 如果 message_delta 一直不来，也不会退回去用它兜底。一个缺失的
			// 数字，看得见，也能追查；一个看似合理却错误的数字，会溜进成本
			// 仪表盘，然后就赖在那里不走了。

		case "content_block_start":
			if ev.ContentBlock == nil {
				return nil
			}
			b := blockAt(ev.Index)
			b.kind = ev.ContentBlock.Type

			// 锁定 id/name：这是它们唯一出现的地方。
			if ev.ContentBlock.ID != "" {
				b.id = ev.ContentBlock.ID
			}
			if ev.ContentBlock.Name != "" {
				b.name = ev.ContentBlock.Name
			}

			switch b.kind {
			case "tool_use":
				// 一旦这次调用可以辨认，就立即公布。这个事件上的 `input` 是
				// `{}`（§B6），故意不读它：参数活在那些片段里。
				markFirstToken()
				emit(Event{Kind: KindToolCallStart, ToolID: b.id, ToolName: b.name})

			case "text":
				// 每次观察到的都是""（§B6，§B7）。还是照样读，而不是假设它是
				// 空的——为了匹配一个 fixture 就丢弃模型输出，就是网关一旦发生
				// 变化，一整段内容凭空消失的原因。
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
				// §B6：**第一个**片段是空字符串。它什么都不携带，所以既不是
				// token，也不是 trace 行。
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
				// §B7：会发出，但总是空的，没什么可往返的。明确地忽略掉，而不是
				// 靠掉进默认分支来忽略——这样它就不会在每一个思考块上都生成一条
				// 通知，也好让下一个读者知道，这种情况是被考虑过的。

			default:
				emit(Event{Kind: KindNotice, Text: fmt.Sprintf("unknown content_block_delta type %q at index %d", ev.Delta.Type, ev.Index)})
			}

		case "content_block_stop":
			// 什么也不用做。块的内容已经累积好了，而它关闭的这个索引，之后
			// 可能会在另一个索引上，为一个不同的块重新打开。工具参数由调用者
			// 在整个流结束后统一解析，正好一次——一个片段边界不是一个 JSON
			// 边界（§B6），这个事件也不是。

		case "message_delta":
			// **这条流上唯一可信的帧。** stop reason 和每一个 Usage 数字——
			// 包括缓存计数器，它们在别的地方根本不出现——都来自这里，
			// 再没有别处。
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				// 锁定，而不是每次赋值：不然的话，第二个带着 null stop_reason
				// 的 message_delta，会把真正重要的那个值给擦掉。
				res.RawStop = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				res.Usage = ev.Usage.normalise()
				// 发出一个**副本**。把 &res.Usage 直接交出去，等于让这个事件和
				// 一个调用者仍然能写入的字段共用同一块内存；而一个懒序列化的
				// 订户，记录下来的就会是这块内存后来变成的样子，不管那是什么。
				sent := res.Usage
				emit(Event{Kind: KindUsage, Usage: &sent})
			}

		case "message_stop":
			// **不是**停止读取的理由。§B6 记录到它之后还有一个 ping，携带着
			// `cost`，而这个协议上根本没有 `[DONE]` 哨兵——流会在连接关闭时
			// 结束，readSSE 把这个报告为 EOF。在这里返回，就是放弃一个里面
			// 明明还有字节的 body，也会让 HTTP 传输层没法把这个连接归还给
			// 连接池：接下来整个会话里，每个回合都要重新做一次 TLS 握手，
			// 却什么都换不来。

		case "error":
			// 在这个网关上未观察到过（§D11 的错误都以 HTTP 状态的形式，在
			// 流打开前到达），但规范里，overloaded_error 和 api_error 是会在
			// 主体中途出现的。返回一个错误会让 readSSE 停下来，这个函数的
			// 尾部随后会返回部分结果，但**不带** KindResponseEnd。
			if ev.Error != nil {
				return fmt.Errorf("anthropic: stream error: %s: %s", ev.Error.Type, ev.Error.Message)
			}
			return fmt.Errorf("anthropic: stream error with no error object: %.200s", payload)

		default:
			// 一个新的事件类型，是信息，不是失败。注意到它，就能把它放进
			// trace，让人读到；悄悄忽略它，就是一次协议变更能被无声无息地
			// 漏掉一整个月的原因。
			emit(Event{Kind: KindNotice, Text: fmt.Sprintf("ignored unknown stream event %q", kind)})
		}

		return nil
	})

	res.Text = text.String()
	res.Thinking = thinking.String()

	// 工具调用按**上升块索引**顺序排列，而非到达顺序。
	// §B6 的两个调用流在索引 0 和 2 处放置 tool_use，
	// 中间有一个文本块，Go 刻意随机化 map 迭代顺序，
	// 所以不排序的话，并行工具调用的顺序会因运行而异——
	// 这种 bug 一周出现一次，常被归咎于模型问题。
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

	// RawStop 保留字面线上字符串，Stop 保留规范化的字符串，
	// 两者之间的差异是证据（provider.go 解释原因）。
	// §A3c 是这个协议特别关心这点的原因：工具调用在
	// max_tokens 处被截断时到达的 stop_reason 是
	// "tool_use"，`input` 不可用，所以 RawStop 不能是
	// 调用方唯一检查的东西。
	//
	// normaliseStop 无条件运行，即使是在 ""：一个
	// 以没有 message_delta 的方式结束的流映射到
	// StopUnknown，agent 主循环会报告它而不是继续。
	// 如果 Stop 留作空字符串，就会发明第四种状态，
	// 没有 switch 处理它。
	res.Stop = normaliseStop(res.RawStop)

	if err != nil {
		// 没有 KindResponseEnd：响应没有结束，它破了。发出这样一个事件，
		// 等于向每个订阅者撒了一个干净利落的谎，trace 应该是证据。
		return res, err
	}

	emit(Event{
		Kind:         KindResponseEnd,
		FinishReason: res.RawStop, // 字面线上字符串，不是规范化的那个
		Millis:       time.Since(started).Milliseconds(),
	})

	return res, nil
}

// ---------------------------------------------------------------------------
// Stage 04——breakpoint 去哪里，以及为什么在那里。
//
// 呈现的 prompt 依次是 `tools`、`system`、`messages`，缓存是
// **前缀**匹配：一个 cache_control 标记的意思是"到这里为止
// 的一切，都是可重用的前缀"。两个结果立即随之而来，它们就是
// 全部的原则：
//
//   - 一个标记只有在它**之前**的内容下次仍然逐字节相同时才
//     有用。
//   - 一个早期发生变化的字节，会让它之后的每个标记都失效，
//     所以内容从稳定到易变的排列顺序，比标记本身更重要。
//
// 每个请求最多允许四个标记。这个适配器放了两个，这是一个
// Agent 实际需要的数量：
//
//	tools ─────────┐
//	system ────────┴─▶ [1] 为整个会话冻结
//	messages
//	  turn 1 …
//	  turn N ──────────▶ [2] 滚动：到最新回合的一切
//
// 标记 1 在第一次请求之后的每次请求上都能自己收回成本。标记
// 2 才是在 Agent 里真正要紧的那个：每个回合都要重发整段
// 对话，所以没有它，每个回合都得以全价重读一遍完整历史——
// 这就是 stage 00 里测到的 3.7 倍重发比率。
// ---------------------------------------------------------------------------

// systemBlocks 呈现系统提示词，把它固定
// 作为可缓存前缀。
//
// 因为 `tools` 在 `system` 之前呈现，最后
// 一个 system 块上的一个标记缓存**两个**。
// 那是工具列表必须是确定的全部原因：
// 重新排序一个工具改变位置零处的字节，
// 使一切失效，包括这个标记。
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

// markRollingBreakpoint 通过标记最后一条消息里的最后一个
// 内容块，固定住目前为止的对话。
//
// 为什么是**最后一条消息里的最后**一个块，而不是一个
// 固定位置：每个回合都会追加内容，标记也跟着移动，所以回合
// N 读到的，是回合 N-1 写下的前缀。一个钉死在固定偏移量上的
// 标记，会停止随对话一起增长，每个回合能缓存到的部分也会
// 越来越少。
//
// 这里的陷阱是 20 块回看。一个 breakpoint 会向后搜索有限
// 数量的内容块，寻找一个已有的条目；而一个触发了许多并行
// 工具的 Agent 回合，可以一口气添加比这更多的块——一旦发生
// 这种情况，下一个标记就会悄无声息地什么也找不到，你会在
// 没有报错、没有警告的情况下，付出全价。一个工具一个回合，
// 远远落在窗口范围之内；但一个会扇出的 Agent 需要一个中间
// 标记，这正是四个槽位里还空着两个的原因。
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
