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

	// 子 Agent 和技能（阶段 07）
	KindSubagentStart Kind = "subagent_start"
	KindSubagentEnd   Kind = "subagent_end"
	KindSkillsIndexed Kind = "skills_indexed" // 有多少个技能，以及这份索引花了多少

	// 记忆与上下文注入（阶段 05）
	KindMemoryLoaded Kind = "memory_loaded" // 记忆文件被读进了系统提示词

	// 上下文压缩（阶段 05）。三个事件而不是一个，因为它们回答三个不同的问
	// 题：压缩正在发生、它花了多少、它弄坏了什么。第三个是别人的实现全都
	// 不给的。
	KindCompactStart     Kind = "compact_start"
	KindCompactEnd       Kind = "compact_end"
	KindCacheInvalidated Kind = "cache_invalidated"

	// 失败与恢复（阶段 09）。三种事件，理由和压缩为什么有三种一样：它们回答
	// 的是不同的问题；而其中有一种，别人的实现一律把它压成一行日志——那一
	// 种恰好就是你需要的。
	//
	//	call_error  什么坏了，以及我们对它做了什么决定
	//	retry       正在等、等多久、这个次数是谁的
	//	provider    从这里往后由谁来接调用，价格是多少
	//
	// KindCallError 不是 KindError。KindError 是终局——会话在告诉人它失败
	// 了。call_error 是一次尝试失败，附带一个决定，而它们大多数后面跟着的是
	// 成功。把它们当错误发出去，只会训练人忽略"错误"这个词。
	KindCallError Kind = "call_error"
	KindRetry     Kind = "retry"
	KindProvider  Kind = "provider"

	// 阶段 10。一条流里两帧之间的最大间隔，也就是空闲期限比的那个量。
	//
	// 它是事件，不是 response_end 上的字段，因为**卡住**的流永远不会产出
	// response_end——而卡住那次调用的间隔，恰恰是你最想看的。单独发出来，
	// 这个数字就能活过它所描述的那次失败。
	KindIdleMax Kind = "idle_max"

	// 阶段 11。没能通过校验的工具调用。
	//
	// 跟 KindToolResult 分开，哪怕模型是靠工具结果知道这件事的：两者回答的问
	// 题不一样，而且只有一个扛得过归一化。工具结果记的是模型被告知了什么，这
	// 个记的是真正到的是什么、被哪道检查拦下的。合到一起，trace 就再也分不出
	// "跑了但失败的工具"和"根本算不上调用的调用"——这两种的修法不一样。
	KindToolCallInvalid Kind = "tool_call_invalid"

	// 阶段 12。结果缓存对一条命令做了什么判定。
	//
	// 一种 kind 配四个判定，而不是四种 kind——四个值回答的是同一个问题："这
	// 条命令跑了吗，没跑是为什么"。refused 和 miss 为什么仍然要分开，见
	// cacheVerdict。
	//
	// 真正承重的是它替掉了什么。命中的命令发出的是这个事件，**不发**
	// command_start，也不发 command_end——因为没有命令开始，也没有命令结束。
	// 顺手带个标志位一起发出去只是半行的事，代价是这个仓库以后录下的每一份
	// trace 都在说有个进程跑过，而实际上一个都没有。trace 要么是证据，要么
	// 是摆设。
	KindResultCache Kind = "result_cache"

	// 其余的
	KindNotice Kind = "notice" // 用户该知道的事，不是错误
	KindError  Kind = "error"
)

// ProviderInfo 是这次调用由谁服务、他家的 token 什么价。
//
// 它挂在 KindProvider 上，因为降级会在会话中途改掉计价的基准。没有它，
// 面板会一直拿第一家供应商的价格去算第二家的 token——一份自信满满的错
// 误成本报告，比一份承认自己不知道的更糟——而重放 trace 的人也没有任何
// 办法查出来。顺带一提，这还是 trace 第一次记下是哪个端点产出了它。
type ProviderInfo struct {
	Name     string      `json:"name"`
	Protocol string      `json:"protocol"`
	Model    string      `json:"model"`
	Window   int         `json:"window,omitempty"`
	Prices   priceConfig `json:"prices,omitempty"`
}

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

	// 这条是哪个 Agent 发的。深度 0 是人正在对话的那个；子 Agent 是深度 1，
	// 以此类推。由 Bus 盖章，不由调用方填，理由跟 Seq 一样：调用方伪造得了
	// 的字段，trace 就没法拿它当证据。
	Depth int    `json:"depth,omitempty"`
	Agent string `json:"agent,omitempty"`

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

	// 压缩的账。记的是前后成对的数，不是差值，因为差值没法告诉你：一次腾出
	// 8,000 token 的压缩，起点是 40,000 还是 9,000——前者是成功，后者
	// 是配错了。
	MsgsBefore   int `json:"msgs_before,omitempty"`
	MsgsAfter    int `json:"msgs_after,omitempty"`
	TokensBefore int `json:"tokens_before,omitempty"`
	TokensAfter  int `json:"tokens_after,omitempty"`

	// 失败记账（阶段 09）。请求根本没拿到响应时 Status 是 0，这跟"响应是 0"
	// 是两件不同的事，也是它没被并进 Text 的原因。
	//
	// ErrType 是供应商 `error.type` 的原文，故意不做归一化：状态码区分不了
	// 写错的模型名和被吊销的密钥时（§D11），靠的就是这个字段，所以一份把它
	// 归一化掉的 trace，等于把自己那次裁决的证据扔了。
	// Phase 是这次调用在哪个环节坏的：build | connect | status | stream。
	//
	// 它不是 Status 旁边的装饰，两者的差别是钱。在 `status` 或 `connect` 上
	// 失败，是模型还没生成任何东西就被拒了，所以这次尝试不花钱。在 `stream`
	// 上失败，那是 200 已经拿到、token 也已经拿到——不管字节有没有到，那些
	// token 都要计费。所以这个字段决定了一次重试有没有花钱，没有它面板就会
	// 算错。Status 顶替不了：连接失败和流中断都带着 Status 0。
	Status  int    `json:"status,omitempty"`
	Phase   string `json:"phase,omitempty"`
	ErrType string `json:"err_type,omitempty"`
	Triage  string `json:"triage,omitempty"`  // 重试 | 降级 | 致命
	Attempt int    `json:"attempt,omitempty"` // 从 1 开始，跨整个梯子累计

	// Provider 说的是从这里往后由谁来接调用。启动时设一次，每次降级再设一
	// 次；价格为什么要跟着它走，见 ProviderInfo。
	Provider *ProviderInfo `json:"provider,omitempty"`

	// Path 点名这个事件涉及的文件（到目前为止都是记忆文件）。
	Path string `json:"path,omitempty"`

	// Fault 是哪道校验拦下了这次工具调用：cut | not_json | schema（阶段 11）。
	// 它跟 Text 并排放，不塞进 Text 里，因为分类是拿来数的，文本是拿来读的；
	// 而面板要靠正则去扒自己的 trace 才数得出东西，那这个面板迟早数错。
	Fault string `json:"fault,omitempty"`

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

// Bus 把事件扇出给各个订阅者。
//
// 分发是同步的，而且在锁里做，这是有意的选择：它让顺序是全序的，而且对每
// 个订阅者都一样，所以 trace 文件和终端永远不会在"谁先发生"上打架。带每订
// 阅者队列的异步总线扩展性更好，也会让 trace 不再是证据。渲染器要是需要
// 慢，那就自己在内部缓冲。
//
// 阶段 07 就是这个选择开始回本的地方，而值得注意的是，这笔钱是提前三章付
// 的。子 Agent 是并发跑的，所以现在有好几个 goroutine 同时在产事件。一把锁
// 加一个计数器，就意味着 trace 仍然是一条单一的全序流——每个事件都有一个
// Seq，精确说明它相对于其他每一个事件、跨每一个 Agent 是什么时候发生的。
// 换成每订阅者一条队列的异步总线，每个订阅者拿到的会是一场并发会话的不同
// 版本，而那恰恰就是没有全序你就没法推理的那种会话。
type busCore struct {
	mu   sync.Mutex
	seq  int
	subs []Subscriber
}

// Bus 是一个 core 上的**视图**：同一个计数器、同一批订阅者，只是给它发出的
// 每样东西都盖上一个深度和一个 Agent 名字。
type Bus struct {
	core  *busCore
	depth int
	agent string
}

func NewBus(subs ...Subscriber) *Bus {
	return &Bus{core: &busCore{subs: subs}}
}

// Fork 返回子 Agent 该往上发事件的那条总线。
//
// 什么都没有复制一份：子 Agent 写进的是跟父 Agent 同一条有序流，所以一个
// trace 文件装着整棵树，`seq` 给它排序。另一条路——一个 Agent 一份 trace
// ——是大多数实现的做法，而它会让你真正想问的那个问题（"子 Agent 在跑的时
// 候，父 Agent 在干什么？"）没法回答，除非你按时间戳把文件合并起来，而那
// 正是时间戳最不擅长的事。
func (b *Bus) Fork(agent string) *Bus {
	return &Bus{core: b.core, depth: b.depth + 1, agent: agent}
}

func (b *Bus) Depth() int { return b.depth }

func (b *Bus) Subscribe(s Subscriber) {
	b.core.mu.Lock()
	defer b.core.mu.Unlock()
	b.core.subs = append(b.core.subs, s)
}

// Emit 给事件盖章，然后送出去。Seq、T、Depth 和 Agent 都在这里赋值，这样调
// 用方就伪造不了，也让重放出来的 trace 能跟一次真实运行逐个事件地对照。
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

// 常见形状的小助手，好让 Agent 核心读起来像散文，而不是一堆 struct
// 字面量。

func (b *Bus) Notice(format string, args ...any) {
	b.Emit(Event{Kind: KindNotice, Text: fmt.Sprintf(format, args...)})
}

func (b *Bus) Error(format string, args ...any) {
	b.Emit(Event{Kind: KindError, Text: fmt.Sprintf(format, args...)})
}
