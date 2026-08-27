// Stage 03——Babel。
//
// 两个协议，一个 Agent。这个文件就是 Agent 说的语言；适配器
// （openai.go、anthropic.go）在线上把它翻译过去。
//
// 让它成立的规则是：**Agent 循环里永远不能出现供应商自己的
// 措辞。** 没有 `tool_calls`、没有 `stop_reason`、没有
// `input_tokens`。一旦有一个泄漏进 main.go，第二个协议就
// 不再是适配器了，会变成一个 `if` 语句，然后是一百个 `if`
// 语句。
//
// 适配器需要调和的东西，不是表面文章。两个协议在这些事情上
// 意见不合：系统提示词放哪里、工具结果怎么寻址、工具参数是
// 字符串还是对象、stop 原因叫什么——以及最费钱的一点，token
// 计费按哪个方向算。docs/03-babel.md 里的表格列出了全部
// 这些，附带观察到的证据。
package main

import (
	"io"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// 中立对话
// ---------------------------------------------------------------------------

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// 注意什么缺失：没有 RoleTool。
//
// OpenAI 协议用它自己的 `role:"tool"` 消息
// 回答一个工具调用，每次调用一个。Anthropic
// 协议用一个**用户**消息内的 `tool_result`
// 块来回答。选择任一作为中立形式会把一个
// 供应商的设计走私到核心，所以中立形式
// 既没有：一个工具结果是一个*块*，每个
// 适配器决定什么消息形状携带它。那个选择
// 就是这个文件使用块的全部原因。

type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockThinking   BlockKind = "thinking"
	BlockToolCall   BlockKind = "tool_call"
	BlockToolResult BlockKind = "tool_result"
)

type Block struct {
	Kind BlockKind

	// Text 携带 text、thinking 或 tool_result
	// 块的内容。
	Text string

	// ID 是工具调用的 id——在 BlockToolCall
	// 上设置，在 BlockToolResult 上说它
	// 回答哪个调用。
	ID   string
	Name string // 工具名字，在 BlockToolCall 上

	// Args 是工具调用的参数作为原始 JSON 字符串。
	//
	// 一个字符串，不是已解码的 map，那是有意的。
	// 一个协议将参数作为 JSON 字符串发送，另一个
	// 作为 JSON 对象；唯一可以通过两者往返而
	// 无需重新序列化的形式是原始字节。重新序列化
	// 也会破坏字节级 prompt 缓存，因为 Go 的
	// map 迭代顺序不稳定。
	Args string
}

type Msg struct {
	Role   Role
	Blocks []Block
}

// 便利构造函数，所以 Agent 循环读起来
// 像散文。

func TextMsg(role Role, text string) Msg {
	return Msg{Role: role, Blocks: []Block{{Kind: BlockText, Text: text}}}
}

func ToolResultBlock(callID, content string) Block {
	return Block{Kind: BlockToolResult, ID: callID, Text: content}
}

// Text 返回连接的文本块，忽略 thinking
// 和工具。
func (m Msg) Text() string {
	var s string
	for _, b := range m.Blocks {
		if b.Kind == BlockText {
			s += b.Text
		}
	}
	return s
}

// ToolCalls 按顺序返回工具调用块。
func (m Msg) ToolCalls() []Block {
	var out []Block
	for _, b := range m.Blocks {
		if b.Kind == BlockToolCall {
			out = append(out, b)
		}
	}
	return out
}

// Tool 是中立形式的工具定义。每个适配器
// 把它呈现到自己的 schema 信封中——一个
// 在 `function` 下嵌套它，另一个不。
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ---------------------------------------------------------------------------
// 中立响应
// ---------------------------------------------------------------------------

// StopReason 是为什么生成停止了，规范化后。
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"   // 模型完成了说话
	StopToolUse   StopReason = "tool_use"   // 它想要工具运行
	StopMaxTokens StopReason = "max_tokens" // 被截断了
	StopFiltered  StopReason = "filtered"   // 供应商阻止了它
	StopUnknown   StopReason = "unknown"    // 一个我们从未见过的字符串
)

// CallResult 是一个模型调用，用 Agent
// 循环理解的形状。
type CallResult struct {
	Text     string
	Thinking string
	Calls    []Block // BlockToolCall，按模型发出它们的顺序
	Usage    Usage
	TTFT     time.Duration

	Stop StopReason

	// RawStop 是供应商的字面字符串，和规范化后的值一起保留，
	// 并写入 trace 中。
	//
	// 这不是冗余。在这个仓库据以开发的网关上，一个在 max_tokens
	// 处截断的工具调用会带着 stop_reason "tool_use" 和一个
	// 不可用的 body 回来（docs/wire-notes.md §A3c）——信封在
	// 说谎。当会话出错时，规范化值告诉你 Agent 当时相信什么，
	// RawStop 告诉你它当时被告知什么，两者之间的落差就是 bug。
	// 永不能把你仅有的证据给规范化掉。
	RawStop string
}

// ---------------------------------------------------------------------------
// 接口
// ---------------------------------------------------------------------------

// Provider 是一个协议。两个实现，每个
// 约 350 行，Agent 循环无法区分它们。
type Provider interface {
	// Protocol 标明线上格式（"openai"、"anthropic"），
	// 用于显示，也用于 trace。
	Protocol() string

	// Model 是被调用的模型 id。
	Model() string

	// BuildRequest 把对话呈现为 HTTP 请求。
	// 系统提示词被单独传递，因为协议对它
	// 属于哪里不同——一个上的顶级字段，另一个
	// 的第一条消息——而那个分歧不能到达调用者。
	BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error)

	// ParseStream 消费一个 SSE body，当它们
	// 到达时发出事件，并返回组装的结果。
	// 两个实现都使用 sse.go 中的 readSSE：
	// framing 被共享，有效负载不共享。
	ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error)
}

// normaliseStop 把供应商的字面 stop
// 字符串映射到中立集上。
//
// 未知字符串映射到 StopUnknown 而不是
// StopEndTurn，Agent 循环报告它们而不是
// 继续。一个状态机，把任何未识别的
// 映射到"可能没事"最终会把拒绝、配额事件
// 或新的安全 stop 映射到"可能没事"。
func normaliseStop(raw string) StopReason {
	switch raw {
	case "stop", "end_turn":
		return StopEndTurn
	case "tool_calls", "tool_use":
		return StopToolUse
	case "length", "max_tokens":
		return StopMaxTokens
	case "content_filter", "refusal":
		return StopFiltered
	default:
		return StopUnknown
	}
}
