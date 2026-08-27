// 阶段 03——Babel。
//
// 两个协议，一个 Agent。这个文件是 Agent 说的语言；适配器
// （openai.go、anthropic.go）负责在线上翻译它。
//
// 让它成立的那条规矩：**主循环里绝不能出现任何厂商的词。**不许有
// `tool_calls`，不许有 `stop_reason`，不许有 `input_tokens`。只要漏
// 一个进 main.go，第二个协议就不再是适配器，而是一条 `if`，接着就是
// 一百条 `if`。
//
// 适配器要调和的东西不是表面功夫。两个协议在这些事上谈不拢：系统提
// 示词放哪儿、工具结果怎么寻址、工具参数是字符串还是对象、停止原因
// 叫什么，还有最烧钱的一条——token 记账往哪个方向走。
// docs/03-babel.md 里的表格把它们连同观测到的证据全列了出来。
package main

import (
	"io"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// 中立的对话
// ---------------------------------------------------------------------------

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// 注意这里少了什么：没有 RoleTool。
//
// OpenAI 协议回答工具调用，用的是自己的 `role:"tool"` 消息，一次调
// 用一条。Anthropic 协议的回答，是塞在一条 **user** 消息里的
// `tool_result` 块。选哪一边当中立形式，都是把某家厂商的设计偷运进
// 内核，所以中立形式两个都不选：工具结果是**块**，至于用什么形状的
// 消息装它，各个适配器自己定。这个选择，就是这个文件为什么要用块的
// 全部原因。

type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockThinking   BlockKind = "thinking"
	BlockToolCall   BlockKind = "tool_call"
	BlockToolResult BlockKind = "tool_result"
)

type Block struct {
	Kind BlockKind

	// Text 装的是 text、thinking 或者 tool_result 块的内容。
	Text string

	// ID 是工具调用的 id——在 BlockToolCall 上设，在 BlockToolResult 上
	// 也设，用来说明它回答的是哪次调用。
	ID   string
	Name string // 工具名，在 BlockToolCall 上

	// Args 是工具调用的参数，以原始 JSON 字符串的形式存着。
	//
	// 是字符串，不是解码后的 map，这是故意的。一个协议把参数当 JSON 字
	// 符串发，另一个当 JSON 对象发；能在两边都往返一圈还不用重新序列化
	// 的形式，只有原始字节。重新序列化还会破坏字节级的 prompt 缓存，因
	// 为 Go 的 map 遍历顺序不稳定。
	Args string
}

type Msg struct {
	Role   Role
	Blocks []Block
}

// 几个便利构造函数，让主循环读起来像散文。

func TextMsg(role Role, text string) Msg {
	return Msg{Role: role, Blocks: []Block{{Kind: BlockText, Text: text}}}
}

func ToolResultBlock(callID, content string) Block {
	return Block{Kind: BlockToolResult, ID: callID, Text: content}
}

// Text 返回拼好的 text 块，忽略 thinking 和工具。
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

// Tool 是中立形式的工具定义。各个适配器把它渲染进自家的 schema 信
// 封：一个嵌在 `function` 底下，另一个不嵌。
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ---------------------------------------------------------------------------
// 中立的响应
// ---------------------------------------------------------------------------

// StopReason 是生成为什么停下来，归一化之后的。
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"   // 模型话说完了
	StopToolUse   StopReason = "tool_use"   // 它要跑工具
	StopMaxTokens StopReason = "max_tokens" // 被截断了
	StopFiltered  StopReason = "filtered"   // 供应商拦下了
	StopUnknown   StopReason = "unknown"    // 没见过的字符串
)

// CallResult 是一次模型调用，形状是主循环认得的那种。
type CallResult struct {
	Text     string
	Thinking string
	Calls    []Block // BlockToolCall，按模型发出它们的顺序
	Usage    Usage
	TTFT     time.Duration

	Stop StopReason

	// RawStop 是供应商给的字面字符串，和归一化后的值一起留着，并写进
	// trace。
	//
	// 这不是冗余。在这个仓库对着开发的那个网关上，工具调用被 max_tokens
	// 截断，回来的 stop_reason 是 "tool_use"，body 根本没法用
	// （docs/wire-notes.md §A3c）——信封在撒谎。会话出岔子的时候，归一化
	// 后的值告诉你 Agent 信了什么，RawStop 告诉你别人跟它说了什么，两者
	// 之间的差距就是那个 bug。永远不要把你唯一的证据归一化掉。
	RawStop string
}

// ---------------------------------------------------------------------------
// 接口
// ---------------------------------------------------------------------------

// Provider 就是一个协议。两份实现，各约 350 行，主循环分不出谁是谁。
type Provider interface {
	// Protocol 给线上格式起名（"openai"、"anthropic"），用于显示，也用
	// 于 trace。
	Protocol() string

	// Model 是被调用的模型 id。
	Model() string

	// BuildRequest 把一段对话渲染成 HTTP 请求。系统提示词单独传，因为两
	// 个协议对它该待在哪儿谈不拢——一个说是顶层字段，另一个说是第一条消
	// 息——而这种分歧不能捅到调用方那儿去。
	BuildRequest(system string, msgs []Msg, tools []Tool, maxTokens int) (*http.Request, []byte, error)

	// ParseStream 吃掉 SSE body，事件到一个发一个，最后返回组装好的
	// 结果。两份实现都用 sse.go 里的 readSSE：分帧是共用的，payload
	// 不是。
	ParseStream(r io.Reader, bus *Bus, turn int, started time.Time) (*CallResult, error)
}

// normaliseStop 把供应商给的字面 stop 字符串映射到中立那一套上。
//
// 不认识的字符串映射成 StopUnknown，不是 StopEndTurn；主循环会把它
// 们报出来，而不是接着往下走。状态机只要把认不出来的东西一律映射成
// "八成没事"，早晚会把一次拒绝、一次配额事件、或者一种新的安全停止
// 也映射成"八成没事"。
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
