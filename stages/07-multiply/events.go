// 阶段 02——事件总线。
//
// 这个文件是整个仓库的架构声明：
//
//	Agent 核心**什么都不**打印。它发出事件。你能看到的一切——
//	纯终端输出、JSONL **trace**、重放查看器，以及稍后的 TUI——
//	都是一个订阅者。
//
// 那一个约束为其余项目所需的大部分东西买单。**trace** 文件是
// 一个订阅者，历史记录因此白白到手。重放是通过相同渲染器读回
// 的 **trace**，所以不需要 API 密钥，就能研究一个会话。`--plain`
// 还是 TUI，是订阅者的选择，不是代码的分支。测试针对事件序列
// 进行断言，而不是抓取 stdout。
//
// 值得带走的教训不是"使用事件总线"。而是：可观测性，是你在
// 一开始就选定的一种形状，不是你在末尾才撒上去的日志。阶段
// 00 和 01 把 fmt.Printf 写进了主循环，每一次调用，都是这样一个
// 地方——发生过什么的唯一记录，只是终端上一个转瞬即逝、滚动
// 消失的字符。
package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Kind 识别发生了什么。保持这些稳定：它们被写入 **trace** 文件，
// 对 kind 重命名会无声地破坏重放——改名之前记录的每一个会话
// 都会受影响。
type Kind string

const (
	// 对话形状
	KindUserMessage Kind = "user_message" // 人类说了点什么
	KindTurnStart   Kind = "turn_start"   // 一个模型回合开始
	KindTurnEnd     Kind = "turn_end"     // 模型停止请求工具

	// 模型调用
	KindRequest        Kind = "request"         // 即将发送的确切字节
	KindFirstToken     Kind = "first_token"     // TTFT 在这里到达
	KindTextDelta      Kind = "text_delta"      // 可见的助手文本
	KindReasoningDelta Kind = "reasoning_delta" // 思考，模型流式传输的地方
	KindUsage          Kind = "usage"           // 一次调用的 token 会计
	KindResponseEnd    Kind = "response_end"    // finish_reason 和计时

	// 工具使用
	KindToolCallStart Kind = "tool_call_start" // id + name 到达（一次，早期）
	KindToolArgsDelta Kind = "tool_args_delta" // 原始参数片段
	KindToolCallReady Kind = "tool_call_ready" // 参数完成且已验证
	KindGateVerdict   Kind = "gate_verdict"    // 允许 / 拒绝 / 中止
	KindCommandStart  Kind = "command_start"
	KindCommandEnd    Kind = "command_end"
	KindToolResult    Kind = "tool_result" // 模型会被告知的确切内容

	// 子 Agent 和技能（阶段 07）
	KindSubagentStart Kind = "subagent_start"
	KindSubagentEnd   Kind = "subagent_end"
	KindSkillsIndexed Kind = "skills_indexed" // 有多少技能，以及索引成本是多少

	// 记忆和上下文注入（阶段 05）
	KindMemoryLoaded Kind = "memory_loaded" // 记忆文件被读入系统提示词

	// 压缩（阶段 05）。三个事件，
	// 不是一个，因为它们回答三个不同
	// 问题：它发生了、成本是多少，
	// 以及它破坏了什么。第三个是
	// 每个其他实现都省略的。
	KindCompactStart     Kind = "compact_start"
	KindCompactEnd       Kind = "compact_end"
	KindCacheInvalidated Kind = "cache_invalidated"

	// 其他一切
	KindNotice Kind = "notice" // 用户应该知道的东西，不是错误
	KindError  Kind = "error"
)

// Usage 是一次调用的 token 会计，采用的是唯一不撒谎的那种
// 形态。
//
// 这个结构存在要避免的陷阱：在 Anthropic 风格的协议上，
// `input_tokens` **只是未缓存剩下的那部分**——运行了一小时的
// Agent，可以报告 18 个输入 token，而实际发送了 18,000 个。
// 总计是 Input + CacheWrite + CacheRead，渲染器必须显示拆分，
// 因为这三者的成本差异很大（大约 1x、1.25x 和 0.1x）。
//
// 换成 OpenAI 风格的协议，方向正好相反：prompt_tokens 是完整
// 数字，cached_tokens 嵌套**在**其中。阶段 03 就是这个转换发生
// 的地方；这个结构已经是标准化之后的形式，这也是为什么它没有
// 一个叫"prompt_tokens"的字段。
type Usage struct {
	Input      int `json:"input"`                 // 按全价计费
	CacheWrite int `json:"cache_write,omitempty"` // ~1.25x
	CacheRead  int `json:"cache_read,omitempty"`  // ~0.1x
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning,omitempty"` // Output 的子集，报告时
}

// Prompt 返回发送的所有东西，这是人们问"我的上下文现在有
// 多大"时，心里想的那个数字——也是你没法只靠读取 API 返回的
// 某一个字段，就拿到手的数字。
func (u Usage) Prompt() int { return u.Input + u.CacheWrite + u.CacheRead }

// Event 有意是一个平面结构，而不是接口层次。
//
// 求和类型在 Go 中会更优雅，在这里会差得多：它需要自定义 JSON
// 解组来重放，还会把数据的形状藏到一个类型开关背后。平面
// 意味着一行 **trace** 肉眼可读，`jq` 不用 schema 就能处理它，
// 添加字段是一行。`omitempty` 使行保持简短。
type Event struct {
	Seq  int       `json:"seq"` // 单调递增；你唯一应该信任的顺序
	T    time.Time `json:"t"`
	Kind Kind      `json:"kind"`

	Turn int `json:"turn,omitempty"` // 当前用户消息内的哪个模型回合

	// 哪个 Agent 发出这个。深度 0 是与人类交谈的那个；子 Agent 是深度 1，
	// 以此类推。由总线标记，而不是由调用者，理由与 Seq 相同：调用者能伪造
	// 的字段是 trace 无法作为证据的字段。
	Depth int    `json:"depth,omitempty"`
	Agent string `json:"agent,omitempty"`

	// Text 携带的，是这个 kind 具体关于什么：可能是一个 delta
	// 片段、一条通知、一条错误消息，或者用户的消息。
	Text string `json:"text,omitempty"`

	// 工具使用
	ToolID   string `json:"tool_id,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Command  string `json:"command,omitempty"`
	Verdict  string `json:"verdict,omitempty"`

	// 命令结果
	ExitCode  int  `json:"exit_code,omitempty"`
	TimedOut  bool `json:"timed_out,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
	Bytes     int  `json:"bytes,omitempty"`

	// 模型调用结果
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`

	// 压缩计费。前/后对而不是增量，
	// 因为增量无法告诉你一个释放了
	// 8,000 token 的压缩从 40,000
	// 还是从 9,000 开始——这些分别
	// 是成功和配置错误。
	MsgsBefore   int `json:"msgs_before,omitempty"`
	MsgsAfter    int `json:"msgs_after,omitempty"`
	TokensBefore int `json:"tokens_before,omitempty"`
	TokensAfter  int `json:"tokens_after,omitempty"`

	// Path 为此事件相关的文件命名
	// （到目前为止是记忆文件）。
	Path string `json:"path,omitempty"`

	// Millis 是这个事件报告的持续时间：first_token 上是 TTFT，
	// command_end 和 response_end 上是挂钟时间。
	Millis int64 `json:"ms,omitempty"`

	// Request 是即将发送的完整 JSON 体。它使请求检查器成为可能，
	// 当你试图弄清为什么模型做了某事时，它是 **trace** 中最有用的
	// 一样东西：它是模型实际看到的唯一记录。
	Request json.RawMessage `json:"request,omitempty"`
}

// Subscriber 按顺序接收每个事件。
type Subscriber interface {
	OnEvent(Event)
}

// 总线将事件扇出到订阅者。
//
// 分发是同步的，并且在锁的保护下进行，这是一个深思熟虑的选择：它让顺序
// 对每个订阅者来说，都是同一个全序，所以 trace 文件和终端永远不会对"什么
// 先发生"产生分歧。一个给每个订阅者单开队列的异步总线，伸缩性会更好，
// 但也会让 trace 不再能充当证据。真需要放慢速度的渲染器，应该自己在内部
// 做缓冲。
//
// 阶段 07 正是这个选择开始回本的地方，而且值得注意的是，这笔投入其实
// 提前三章就已经付出了。子 Agent 并发运行，所以现在有好几个 goroutine
// 在同时产生事件。一把锁、一个计数器，就让 trace 依然是唯一一条全序的
// 事件流——每个事件都有一个 Seq，精确标出它相对于所有其他事件、跨越
// 所有 Agent，是在什么时刻发生的。一个给每个订阅者单开队列的异步总线，
// 会让每个订阅者看到关于这个并发会话的不同版本——而这恰恰就是那种，
// 没有一条全序流就没法推理清楚的会话。
type busCore struct {
	mu   sync.Mutex
	seq  int
	subs []Subscriber
}

// 总线是对核心的一个**视图**：相同的计数器和相同的订阅者，深度和 Agent
// 名称盖在它发出的所有东西上。
type Bus struct {
	core  *busCore
	depth int
	agent string
}

func NewBus(subs ...Subscriber) *Bus {
	return &Bus{core: &busCore{subs: subs}}
}

// Fork 返回子 Agent 应该发到的那条总线。
//
// 没有任何重复：子 Agent 写入的是和父 Agent 同一条有序流，所以一个
// trace 文件就装得下整棵树，`seq` 给它排好顺序。另一种做法——每个
// Agent 一份 trace——是大多数实现的选择，但它会让你真正想问的那个
// 问题（"子 Agent 运行期间，父 Agent 在做什么？"）变得没法回答，
// 除非按时间戳合并文件——而按时间戳合并，恰恰是时间戳最不擅长的事。
func (b *Bus) Fork(agent string) *Bus {
	return &Bus{core: b.core, depth: b.depth + 1, agent: agent}
}

func (b *Bus) Depth() int { return b.depth }

func (b *Bus) Subscribe(s Subscriber) {
	b.core.mu.Lock()
	defer b.core.mu.Unlock()
	b.core.subs = append(b.core.subs, s)
}

// Emit 给事件盖上戳记，然后把它发出去。Seq、T、Depth 和 Agent 都是在
// 这里赋值的，这样任何调用者都伪造不了它们，重放的 trace 也才能和一次
// 真实运行逐个事件地比对。
func (b *Bus) Emit(e Event) {
	b.core.mu.Lock()
	defer b.core.mu.Unlock()
	b.core.seq++
	e.Seq = b.core.seq
	if e.T.IsZero() {
		e.T = time.Now()
	}
	e.Depth, e.Agent = b.depth, b.agent
	for _, s := range b.core.subs {
		s.OnEvent(e)
	}
}

// 常见形状的辅助函数，所以 Agent 核心读起来像散文而不像
// 结构字面量。

func (b *Bus) Notice(format string, args ...any) {
	b.Emit(Event{Kind: KindNotice, Text: fmt.Sprintf(format, args...)})
}

func (b *Bus) Error(format string, args ...any) {
	b.Emit(Event{Kind: KindError, Text: fmt.Sprintf(format, args...)})
}
