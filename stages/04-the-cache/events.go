// 阶段 02——事件总线。
//
// 这个文件是整个仓库的架构主张：
//
//	Agent 核心**什么都不打印**。它只发事件。你能看见的一切——
//	朴素的终端输出、JSONL trace、重放查看器，以及后面的 TUI——
//	都是订阅者。
//
// 这一条约束，就把项目后面要用的东西买下了大半。trace 文件是订阅者，
// 历史就这么免费记下了。重放就是把 trace 从同一个渲染器读回去，所以
// 没有 API key 也能研究一段会话。`--plain` 还是 TUI，选的是订阅者，不是
// 把代码分成两份。测试断言的是事件序列，不是去刮 stdout。
//
// 值得带走的教训不是"用事件总线"，而是：可观测性是开头就定下来的形状，
// 不是收尾时撒上去的日志。阶段 00 和 01 把 fmt.Printf 写进了主循环，那里
// 每一次调用都是这么一处：发生过什么，唯一的记录就是终端上的字符，
// 滚过去就没了。
package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Kind 标明发生了什么。这些值要稳住：它们会写进 trace 文件，改个名字，
// 改名之前录下的每一段会话就都重放不了，而且一声不响。
type Kind string

const (
	// 对话的形状
	KindUserMessage Kind = "user_message" // 人说了句话
	KindTurnStart   Kind = "turn_start"   // 一轮模型调用开始
	KindTurnEnd     Kind = "turn_end"     // 模型不再要工具了

	// 模型调用
	KindRequest        Kind = "request"         // 即将发出去的确切字节
	KindFirstToken     Kind = "first_token"     // TTFT 落在这里
	KindTextDelta      Kind = "text_delta"      // 看得见的 assistant 文本
	KindReasoningDelta Kind = "reasoning_delta" // 思考过程，前提是模型会流式发出来
	KindUsage          Kind = "usage"           // 单次调用的 token 账
	KindResponseEnd    Kind = "response_end"    // finish_reason 和各项耗时

	// 工具使用
	KindToolCallStart Kind = "tool_call_start" // id + name 到齐（只此一次，很早）
	KindToolArgsDelta Kind = "tool_args_delta" // 参数的原始碎片
	KindToolCallReady Kind = "tool_call_ready" // 参数收齐并校验通过
	KindGateVerdict   Kind = "gate_verdict"    // 放行 / 拒绝 / 中止
	KindCommandStart  Kind = "command_start"
	KindCommandEnd    Kind = "command_end"
	KindToolResult    Kind = "tool_result" // 告诉模型的东西，一字不差

	// 其余的
	KindNotice Kind = "notice" // 用户该知道的事，不是错误
	KindError  Kind = "error"
)

// Usage 是单次调用的 token 账，而且用的是唯一不撒谎的那种形状。
//
// 这个 struct 是为了躲坑才有的，坑在这儿：Anthropic 那一路的协议里，
// `input_tokens` **只是没命中缓存的那点余量**——跑了一小时的 Agent 可以
// 报出 18 个 input token，实际发出去的是 18,000。总数是
// Input + CacheWrite + CacheRead，而渲染器必须把这三份分开显示，因为
// 它们的价钱差得离谱（大约 1x、1.25x 和 0.1x）。
//
// OpenAI 那一路的协议记账方向正好相反：prompt_tokens 是全量，
// cached_tokens **嵌在它里面**。这个转换在阶段 03；这个 struct 已经是
// 归一化之后的形状，所以它没有叫 "prompt_tokens" 的字段。
type Usage struct {
	Input      int `json:"input"`                 // 按全价计费
	CacheWrite int `json:"cache_write,omitempty"` // ~1.25x
	CacheRead  int `json:"cache_read,omitempty"`  // ~0.1x
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning,omitempty"` // Output 的子集，前提是供应商报了
}

// Prompt 返回发出去的全部。别人问"我现在上下文多大"，说的就是这个数；
// 而这个数，你读 API 返回的任何单个字段都读不出来。
func (u Usage) Prompt() int { return u.Input + u.CacheWrite + u.CacheRead }

// Event 故意做成扁平的单个 struct，而不是接口层次。
//
// sum type 在 Go 里更优雅，放在这里却糟得多：重放要为它写自定义的 JSON
// 反序列化，而且它把数据的形状藏在了 type switch 后面。扁平的好处是，
// trace 里的一行用眼睛就能读，`jq` 不用 schema 就能处理，加个字段只要
// 一行。`omitempty` 让这些行不至于太长。
type Event struct {
	Seq  int       `json:"seq"` // 单调递增；唯一该信的顺序
	T    time.Time `json:"t"`
	Kind Kind      `json:"kind"`

	Turn int `json:"turn,omitempty"` // 当前这条用户消息里的第几轮模型调用

	// Text 装的是这个 kind 讲的那件事：delta 碎片、提示、错误信息、
	// 用户发来的话。
	Text string `json:"text,omitempty"`

	// 工具使用
	ToolID   string `json:"tool_id,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Command  string `json:"command,omitempty"`
	Verdict  string `json:"verdict,omitempty"`

	// 命令的结果
	ExitCode  int  `json:"exit_code,omitempty"`
	TimedOut  bool `json:"timed_out,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
	Bytes     int  `json:"bytes,omitempty"`

	// 模型调用的结果
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`

	// Millis 是这条事件报的时长：first_token 上是 TTFT，command_end 和
	// response_end 上是挂钟时间。
	Millis int64 `json:"ms,omitempty"`

	// Request 是即将发出的完整 JSON body。请求检查器靠它才成立；而当你
	// 想弄明白模型为什么那么干的时候，它是 trace 里最有用的一样东西：
	// 模型究竟看到了什么，只有它记着。
	Request json.RawMessage `json:"request,omitempty"`
}

// Subscriber 收到每一条事件，按顺序。
type Subscriber interface {
	OnEvent(Event)
}

// Bus 把事件扇出给订阅者。
//
// 派发是同步的，而且在锁里，这是故意的：这样顺序是全序的，对每个订阅者
// 都一样，trace 文件和终端就永远不会在"谁先发生"上打架。改成异步总线、
// 每个订阅者一条队列，扩展性会更好，也会让 trace 不再是证据。渲染器如果
// 非慢不可，自己在内部缓冲。
type Bus struct {
	mu   sync.Mutex
	seq  int
	subs []Subscriber
}

func NewBus(subs ...Subscriber) *Bus { return &Bus{subs: subs} }

func (b *Bus) Subscribe(s Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, s)
}

// Emit 给事件盖上戳再送出去。Seq 和 T 在这里赋值，调用方就伪造不了；
// 重放出来的 trace 也就能和实跑一条一条比对。
func (b *Bus) Emit(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if e.T.IsZero() {
		e.T = time.Now()
	}
	for _, s := range b.subs {
		s.OnEvent(e)
	}
}

// 常见形状的小助手，好让 Agent 核心读起来像散文，而不是一堆 struct
// 字面量。

func (b *Bus) Notice(format string, args ...any) {
	b.Emit(Event{Kind: KindNotice, Text: fmt.Sprintf(format, args...)})
}

func (b *Bus) Error(format string, args ...any) {
	b.Emit(Event{Kind: KindError, Text: fmt.Sprintf(format, args...)})
}
