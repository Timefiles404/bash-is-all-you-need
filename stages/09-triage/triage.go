// 阶段 09 — 分诊。
//
// 一个想法：**一个错误是一个决策，不是一个字符串。**
//
// 一个刚刚失败了一次模型调用的 Agent，手上正好只有三个动作。等一下，再把同样的
// 字节发一遍。把它们发到别处去。停下来，说清楚为什么。这个文件里的一切都是为了
// 把一次失败变成这三者之一；而它之所以是一个文件、不是一个 `if`，是因为每个人
// 一开始都会写的那两条规则，在这个仓库所针对的那个端点上都是错的：
//
//	"401 说明密钥是坏的，所以停。"
//	    一个写错的**模型名**在这里返回的是 401，信封
//	    的形状和一把被吊销的密钥一模一样（§D11）。停
//	    下来，对其中一种是正确的，对另一种则是白扔掉
//	    一个能用的会话。
//
//	"5xx 是瞬态的，所以带退避重试。"
//	    一个畸形的请求体在这里返回 500（§D11）。那是
//	    客户端的一个 bug，而一个只认状态码的策略会永
//	    远重试它——这个重试永远不会成功，也永远不会停。
//
// 这两条规则，都不是靠把状态码读得更用力就能修好的。修法是把失败的足够多的部分
// 带上，好用它来做决定：它在哪里坏的、供应商把它叫什么、body 到底说了什么。这
// 就是这个文件和 `fmt.Errorf` 的差别。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 失败的是什么
// ---------------------------------------------------------------------------

// callPhase 是一次模型调用里失败发生在哪个环节，也是分类器
// 第一眼要看的东西——因为它回答了决定其余一切的那个问题：
// **有没有东西被生成出来？**
//
// 什么都没生成，意味着重试是免费的。生成了东西，意味着重试
// 要花掉第二个完整的 prompt，而第一个已经上了账单。
//
// 它叫 phase 而不叫 stage，是因为"stage"在其他每一个文件里都
// 已经表示这门课的一章，而在一个叫 stages/09-triage 的目录里、
// 给一个事件加一个叫 Stage 的字段，就成了一句没人能读两遍读
// 出同一个意思的话。
type callPhase string

const (
	phaseBuild   callPhase = "build"   // 我们连请求都没能渲染出来
	phaseConnect callPhase = "connect" // 完全没有响应：DNS、连接被拒、TLS、响应头之前就被重置
	phaseStatus  callPhase = "status"  // 响应到了，而它不是 200
	phaseStream  callPhase = "stream"  // 200，然后 body 断了、或者带来了一个错误事件
)

// CallError 是一次失败的模型调用，形状是能从里面做出决策的那种。
//
// 每个字段都在这里，是因为下面某条分诊规则需要它，而一个字符
// 串载不动它。看起来多余的那个——Body，紧挨着 Type 和 Message
// ——恰恰是最经常赚回自己位置的那个：在这个网关上，观测到的
// 一个 400 回来时根本没有错误信封，只有 24 字节的请求回声
// （`{"model":"qwen3.7-plus"}`，§D11）。那个响应的 Type 和
// Message 都是空的，原始 body 是唯一存在的证据。
type CallError struct {
	Phase   callPhase
	Status  int    // 没有响应时为 0
	Type    string // 供应商的 error.type，原样照抄——绝不归一化，因为它是证据
	Message string
	Body    string // 响应 body 的前 8 KiB，原样照抄

	// RetryAfter 是服务器要我们等的时长，服务器没要求时为 0。服务器
	// 给的数字永远胜过我们算出来的退避：这场对话里，只有它这一方
	// 知道容量什么时候回来。
	RetryAfter time.Duration

	// Partial 是流断掉时适配器已经攒下来的东西。
	//
	// 留着而不是丢掉，而这就是阶段 09 要来补的那道缝。两个适配器
	// 都故意在返回非 nil 错误的同时返回一个非 nil 的结果——openai.go
	// 和 anthropic.go 的注释里都这么写了——而在此之前的每一个阶段都
	// 接住了那个值，却从来没看过它。
	//
	// 这里没有任何东西会把部分结果喂回模型：它做到一半的那个计划
	// 已经没了，而半个工具调用比没有更糟。它存在是为了那张账。那些
	// token 被生成过，而生成过的 token 是要计费的，所以一个把部分结
	// 果丢在地上的 Agent，就是一个讲不清自己账单的 Agent——而那正是
	// 整个仓库要讲的那个失败。
	Partial *CallResult

	Err error // 底层的传输错误，这样 errors.Is 依然能用
}

func (e *CallError) Error() string {
	switch e.Phase {
	case phaseBuild:
		return fmt.Sprintf("could not build the request: %v", e.Err)
	case phaseConnect:
		return fmt.Sprintf("no response from the provider: %v", e.Err)
	case phaseStatus:
		// 信封缺失这种情况会得到一条看得出不一样的消息，而不是一条空
		// 的。"http 400: "冒号后面什么都没有，读起来像是 Agent 自己的
		// bug；把这处缺失点出来，才是指向服务器。
		if e.Type == "" && e.Message == "" {
			return fmt.Sprintf("http %d with no error envelope: %.200s", e.Status, strings.TrimSpace(e.Body))
		}
		return fmt.Sprintf("http %d %s: %s", e.Status, e.Type, e.Message)
	case phaseStream:
		if e.Type != "" {
			return fmt.Sprintf("the provider sent %s mid-stream: %s", e.Type, e.Message)
		}
		return fmt.Sprintf("the stream broke: %s", e.Message)
	}
	return e.Message
}

func (e *CallError) Unwrap() error { return e.Err }

// asCallError 就是把 target 声明好了的 errors.As，否则那三行的
// 固定舞步会出现在每一个调用点上。
func asCallError(err error) (*CallError, bool) {
	var ce *CallError
	ok := errors.As(err, &ce)
	return ce, ok
}

// ---------------------------------------------------------------------------
// 决策
// ---------------------------------------------------------------------------

// Triage 是对一次失败该做什么。三个取值，因为 Agent 只有三种
// 动作——而按动作、而不是按失败来命名它们，是整个文件的要点。
// `ErrRateLimited` 告诉你发生了什么；`TriageRetry` 告诉你该做
// 什么，而后者才是调用者唯一需要的东西。
type Triage string

const (
	TriageRetry    Triage = "retry"    // 同样的字节，晚一点
	TriageFallback Triage = "fallback" // 同样的字节，换个地方
	TriageFatal    Triage = "fatal"    // 停下，并说清为什么
)

// triage 把一次失败映射到一个决策。
//
// 请对着 docs/wire-notes.md 的 §D11 读这一段。这里几乎每一行的
// 存在，都是因为记录下来的字节和那条显而易见的规则相矛盾。
func (e *CallError) triage() Triage {
	switch e.Phase {
	case phaseBuild:
		// 我们自己的 bug。一个我们渲染不出来的请求，第二次尝试也一样
		// 渲染不出来，而重试它烧掉的，是这次会话后面真出故障时要用的
		// 预算。
		return TriageFatal

	case phaseConnect:
		// 没有响应意味着什么都没生成、什么都没计费，所以这是唯一一类
		// 重试真正免费的失败。DNS、连接被拒、TLS 握手、响应头之前的
		// RST——它们都值得再试一次，而在尝试次数用完之前，它们都不值
		// 得换一个供应商。
		return TriageRetry

	case phaseStream:
		// 在线上断掉的流，和带来了一个错误**事件**的流，今天是同一个
		// Go 错误，但绝不能是同一个决策。
		//
		// Type == "" 是传输那种情况：连接在 body 中途死了。值得再试一
		// 次。
		//
		// 有名字的 type 是供应商在响应中途故意发出来的，其中只有两个
		// 的意思是"再问一次"。其余的到来是因为我们发出去的东西，再发
		// 一次会产生同一个事件——这是 5xx 陷阱在 phaseStream 上的孪生
		// 兄弟。
		if e.Type == "" {
			return TriageRetry
		}
		switch e.Type {
		case "overloaded_error", "api_error", "rate_limit_error", "timeout_error":
			return TriageRetry
		}
		return TriageFatal

	case phaseStatus:
		return triageStatus(e.Status, e.Type)
	}
	return TriageFatal
}

// triageStatus 被拆出来，是因为意外都在这一部分里，而一个表驱
// 动的测试想直接调用它。
func triageStatus(status int, typ string) Triage {
	t := strings.ToLower(typ)

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// §D11，促成整个文件的那个发现：在这个网关上，一个不存在的
		// model id 返回的是 **401**，不是 404，而且带着和被吊销的密钥
		// 一样的 `{"type":"error","error":{...}}` 信封。状态码分不开它
		// 们。`error.type` 分得开——`ModelError` 对 `AuthError`——而
		// 这两者需要相反的决策，因为一个是死掉的会话，另一个是指错了
		// 模型名的、还能工作的会话。
		//
		// 故意用子串来匹配。观测到的值是 PascalCase（`ModelError`），
		// 而两份协议规范说的都是 snake_case（`not_found_error`、
		// `invalid_request_error`），所以拿任一种拼写去做相等判断，都会
		// 是对着文档正确、对着线上错误。
		if strings.Contains(t, "model") {
			return TriageFallback
		}
		return TriageFatal

	case status == http.StatusNotFound:
		// 这条路由或这个模型不在这个端点上。别的端点可能有它；等待不
		// 会让它出现在这里。
		return TriageFallback

	case status == http.StatusTooManyRequests:
		return TriageRetry

	case status == http.StatusRequestTimeout, status == http.StatusConflict:
		return TriageRetry

	case status == http.StatusRequestEntityTooLarge:
		// 问题就在这些字节上。等待和换一个供应商都改不了它们；上下文
		// 压缩能改，而那是阶段 05 的活，不是这里的。这里判 Fatal，是
		// 为了让这条消息送到能把它变小的那个人手上。
		return TriageFatal

	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		// 我们的。重试一个服务器已经拒绝的参数，正是一个客户端 bug 变
		// 成一次故障的方式。
		return TriageFatal

	case status >= 500:
		// 会重试，但牵绳是这里所有情况里最短的——见 leash()。
		//
		// 原因是 §D11：在这个网关上，一个格式错误的请求 body 返回的是
		// **500**，把一个 OpenAI 形状的 body POST 到 Anthropic 那条路
		// 由上，也返回 500。两者都是穿着服务器状态码的客户端 bug。
		// "5xx = 瞬态"会一直重试它们，直到预算死掉，每个回合都如此，
		// 永远。
		return TriageRetry
	}

	// 一个上面谁都没认领的状态码。判 Fatal 而不是重试，因为一个没
	// 被分类的失败去重试，只是把这个失败重复一遍——而这里发出的
	// 那个事件，正是缺失的那种情况被找出来的途径。
	return TriageFatal
}

// leash 给一类失败值得的尝试次数设上限，0 表示"策略给的全部额
// 度"。
//
// 一条规则，一个理由。503 是真正的容量信号，给它全部额度。光秃
// 秃的 500 总共只给两次尝试，因为在这个端点上，它是**我们**配错
// 了的可能性，至少不低于是他们出故障（§D11），而两次尝试足够
// 撑过一次一闪而过的小故障，同时又远远不够把一个永久性的错误
// 藏在重试循环后面。
func (e *CallError) leash() int {
	if e.Phase == phaseStatus && e.Status >= 500 && e.Status != http.StatusServiceUnavailable {
		return 2
	}
	return 0
}

// ---------------------------------------------------------------------------
// 等多久
// ---------------------------------------------------------------------------

// retryPolicy 是重试的全部配置，而它是四个数字，因为人们会加上
// 的第五个——"一直重试到成功为止"——正是一次瞬态失败变成一
// 张账单的方式。
type retryPolicy struct {
	attempts int           // 每个供应商的总尝试次数，包含第一次
	base     time.Duration // 第一次退避
	max      time.Duration // 任何单次等待的上限
	budget   time.Duration // 一次调用里所有等待加起来的上限
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{attempts: 3, base: 500 * time.Millisecond, max: 8 * time.Second, budget: 30 * time.Second}
}

// wait 返回第 n 次尝试之前该睡多久（n 从 1 数起，所以第一次等待
// 是 wait(1)，发生在第一次失败之后）。
//
// 用全抖动——从 [0, exp) 里均匀取一个数——而不是更常见的
// `exp/2 + rand(exp/2)`。这个差别在这里格外要紧：阶段 07 派
// 出的子 Agent 共用一个 http.Client 和一个端点，所以供应商打一
// 个嗝，就有好几个调用在同一毫秒里失败。半抖动会让这些调用保
// 持成一簇，它们每次尝试都会再撞一次；全抖动把它们摊到整个区
// 间上。一个把自己的客户端同步起来的重试策略，就是一台对准了
// 已经吃不消的那个服务的压力发生器。
//
// after 是服务器的 Retry-After。它存在时就直接取胜，因为它是这
// 个函数里唯一一个来自"知道容量什么时候回来"的人的数字——但
// 它仍然被调用者的预算夹住，因为服务器也有权说"一个小时"。
func (p retryPolicy) wait(n int, after time.Duration, rnd func() float64) time.Duration {
	if after > 0 {
		if after > p.max*8 {
			// 服务器要求的时间，可能比我们愿意让一个回合挂在那里的时间更
			// 长。尊重这个要求的形状，但不尊重它的长度：另一种选择是一个
			// 看起来卡死了一小时的 Agent。
			return p.max * 8
		}
		return after
	}
	exp := p.base << (n - 1)
	if exp > p.max || exp <= 0 { // <= 0 是为了抓住位移溢出
		exp = p.max
	}
	return time.Duration(rnd() * float64(exp))
}

// parseRetryAfter 按 RFC 9110 允许的两种形式读这个响应头：
// delta-seconds 和 HTTP-date。
//
// 这是照着 RFC 写的，不是照着观测写的，而这一章也这么说了：
// docs/wire-notes.md 里那个网关根本没发过 429，所以这个仓库的
// 证据里，任何地方都没有记录到过 Retry-After。测试是拿一个本地
// 服务器来跑它的。这比这个文件其余部分的立足点要弱，而把这处
// 弱点说出来，好过暗示一次并不存在的测量。
//
// now 是一个参数，因为日期形式是相对它而言的，而一个没法把
// "现在"钉死的测试，根本测不了日期形式。
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
		// 一个过去的日期意思是"现在"，不是"负数的睡眠"。
		return 0
	}
	// 解析不了。选择忽略而不是去猜：退回到算出来的退避是一个已知
	// 安全的数字，而从一个格式错误的头里编一个出来不是。
	return 0
}

// ---------------------------------------------------------------------------
// 错误信封
// ---------------------------------------------------------------------------

// errEnvelope 用一个 struct 覆盖两种形状，而这只有靠 §D11 里的
// 一个发现才成为可能：这个网关连 OpenAI 那条路由也返回
// **Anthropic** 的信封。嵌套的那个 `error` 对象是两边一致的部
// 分，所以这里读的就是那一部分。
type errEnvelope struct {
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    any    `json:"code"` // OpenAI 的；有时是字符串，有时是 null——所以用 `any`
	} `json:"error"`
}

// parseErrorBody 把两个协议一致的东西抽出来，而且在它们什么都
// 不一致的情况下也活得下来。
//
// 有三种观测到的形状要从这里过（§D11）：
//
//	{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}
//	{"type":"error","error":{"type":"error","message":"Internal server error"}}
//	{"model":"qwen3.7-plus"}                       <- 一个 400，根本没有信封
//
// 第三种就是这里返回两个空字符串、而不是返回一个错误的原因。
// 信封缺失不是一次要上报的解析失败，它是关于这个响应的一个事
// 实，而调用者本来就正是为这种情况留着原始 body。在这里返回一
// 个错误，会让 Agent 报"解析不了这个错误"，而不是把这个错误报
// 出来。
//
// 顺带一提，它是以 `Content-Type: text/plain;charset=UTF-8` 送
// 来的，这也是这个函数里没有任何一处去看 content type 的原因。
func parseErrorBody(raw []byte) (typ, msg string) {
	var env errEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error == nil {
		return "", ""
	}
	return env.Error.Type, env.Error.Message
}

// ---------------------------------------------------------------------------
// 改发到哪里去
// ---------------------------------------------------------------------------

// rung 是降级梯子上的一个供应商，连带它配套的身份和价格。
type rung struct {
	p    Provider
	info ProviderInfo
}

// ladder 是一次会话可以使用的供应商的有序列表：配置里那个排在
// 最前，其余的只有靠一个 TriageFallback 裁决才够得着。
//
// 故意不是负载均衡器，也故意不是熔断器。这里没有任何东西会分
// 摊流量，顺序也从不改变。有两个后果，值得说出来，而不是留给
// 人去发现：
//
// 它从不往回爬。一次会话一旦掉到第 1 级，就一直待在那里，
//
//	因为"主供应商是不是又健康了"这个问题，不花一次真实调用
//	去问就答不出来。所以一次降级是一次你得一直带着的降级，
//	而这正是为什么每一次切换都要发出 KindProvider——一次会
//	话被悄悄地用更便宜的模型服务了一个小时，就是这个设计为
//	了简单而换来的那个失败，它只有在可见时才活得下去。
//
// 每一级都有自己的价格，也有自己的上下文窗口。面板是按事
//
//	件重新计价的，不是按启动时的配置，因为另一种做法是一份
//	成本报告，悄悄地把第二个供应商的 token 按第一个供应商的
//	价格计费。
//
// 那个 mutex 不是装饰。阶段 07 的子 Agent 是并发跑的，而且按
// 指针共用这个 ladder——这是故意的，因为"这个端点挂了"是端点
// 的属性，不是哪个 Agent 先注意到它的属性，所以父 Agent 不该去
// 重新发现一个它的子 Agent 已经付过代价的失败。共用它意味着两
// 个子 Agent 可以在同一微秒里调用 advance()。
type ladder struct {
	mu    sync.Mutex
	rungs []rung
	cur   int
}

func newLadder(rungs ...rung) *ladder { return &ladder{rungs: rungs} }

// pos 在同一把锁里返回当前的级号和这一级上的一切，因为分开去取供应商
// 和级号的调用方，可能会作用在两个不同的级上——而这个级号正是它稍后
// 要传给 advance 的那个东西。
func (l *ladder) pos() (int, Provider, ProviderInfo) {
	if l == nil {
		return 0, nil, ProviderInfo{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.rungs) == 0 {
		return 0, nil, ProviderInfo{}
	}
	return l.cur, l.rungs[l.cur].p, l.rungs[l.cur].info
}

// advance 从 `from` 这一级迈出去，无处可去时报 false。
//
// 它拿的是调用方的那一级，而不是自己去读，由此多出来的那两行就是全部
// 的并发故事。阶段 07 的子 Agent 并发地打同一个端点，所以一个死掉的
// 供应商会一次让好几个调用失败，而它们每一个都想降级。
//
// 把 `cur = from + 1` 写下去，对两个在同一级上失败的兄弟来说本来就是
// 幂等的：两边写的是同一个值。真正需要这道守卫的情形有三个参与者，而
// 且它是一次**回退**，不是走了两步。A 在第 0 级失败，挪到 1；C 在 1 上
// 失败，挪到 2；然后 B——它手里还攥着这一切发生之前读到的第 0 级——来问
// 能不能降级。没有这道守卫，它会写下 cur = 1，把下一次调用送去一个两
// 个兄弟都已经放弃了的供应商。有了它，B 被告知**可以**，同时什么都没
// 挪动，而这才是它真正在问的那个问题的诚实答案："还有别的地方能把这
// 个送过去吗？"有，而梯子早就在那儿了。
func (l *ladder) advance(from int) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur > from {
		return true // 已经有兄弟替我们挪过了；这是一次成功，不是一步
	}
	if from+1 >= len(l.rungs) {
		return false
	}
	l.cur = from + 1
	return true
}

// ---------------------------------------------------------------------------
// 一次尝试
// ---------------------------------------------------------------------------

// modelCall 完成一次模型调用的一次尝试：渲染请求、把它发出去、解析流，
// 并对出错的任何东西做分类。
//
// 一个函数，两个调用方——Agent 的回合，和阶段 05 的摘要器。在这一阶段
// 之前它们是两份拷贝，而拷贝已经漂移了：压缩那一份不读响应体，所以一次
// 失败的压缩只报 `http 500`，别的什么都没有，而那是唯一没法调试的东西。
// 共用代码其实不是重点。共用**分类表**才是，因为一个被两条路径分成不同
// 类的失败，是一个你没法为它写出一套策略的失败。
//
// 它每次尝试都重建请求，而不是复用一个，这不是无所谓的细节：一个
// *http.Request 的 body 在第一次 Do 之后就是一个已经被读完的 reader，
// 所以一次重发同一个请求对象的重试会发出零字节，然后收回一个 400——一
// 个长得跟服务器 bug 一模一样的重试 bug。
func modelCall(p Provider, httpc *http.Client, bus *Bus, turn int,
	system string, msgs []Msg, tools []Tool, maxTokens int) (*CallResult, error) {

	req, body, err := p.BuildRequest(system, msgs, tools, maxTokens)
	if err != nil {
		return nil, &CallError{Phase: phaseBuild, Err: err, Message: err.Error()}
	}
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	started := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, &CallError{Phase: phaseConnect, Err: err, Message: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		typ, msg := parseErrorBody(raw)
		return nil, &CallError{
			Phase: phaseStatus, Status: resp.StatusCode,
			Type: typ, Message: msg, Body: strings.TrimSpace(string(raw)),
			// 这个仓库读到的第一个响应头。在此之前，就算真来了一个
			// Retry-After，Agent 也没法照办，因为它从来没看过。
			RetryAfter: parseRetryAfter(resp.Header, time.Now()),
		}
	}

	res, err := p.ParseStream(resp.Body, bus, turn, started)
	if err != nil {
		// 两个适配器都在返回非 nil 错误的同时返回一个非 nil 的
		// result，这是故意的，而且有注释这么说，而这一阶段之前的每
		// 个阶段都把那个值绑上却从来不读。在这里它成了
		// CallError.Partial——为什么账单才是留住它的理由，见那个字段。
		//
		// 一个流内的错误**事件**，本来就已经是从 anthropic.go 来的一
		// 个 *CallError，带着供应商自己的 error.type。给它补上信息而
		// 不是把它包起来，能让分类器仍然拿得到那个 type，而这正是熬
		// 过一次短暂的容量波动和干脆放弃之间的区别。
		if ce, ok := asCallError(err); ok {
			ce.Partial = res
			return res, ce
		}
		return res, &CallError{Phase: phaseStream, Message: err.Error(), Err: err, Partial: res}
	}
	return res, nil
}

// forCompaction 是同一套策略，只是牵绳更短。
//
// 压缩本来就有一个安全的失败方式：Agent 不压缩地继续，并在总线上说出
// 来。所以早点放弃代价很小，而硬着头皮重试代价极大——每一次尝试都按全
// 价重发整份文字记录，而且它这么做的时候，那个需要腾出空间的回合还在
// 等着。两次尝试、五秒钟，不管会话的策略是什么。
func (p retryPolicy) forCompaction() retryPolicy {
	if p.attempts > 2 {
		p.attempts = 2
	}
	if p.budget > 5*time.Second {
		p.budget = 5 * time.Second
	}
	return p
}

// buildLadder 用解析好的主供应商和 --fallback 拼出梯子。
//
// 每一级都在启动时构造出来、密钥也在启动时查过，都在第一个请求之前。
// 这就是它是一个函数、而不是在失败当口做一次懒查找的全部理由：按需搭
// 出来的降级，就是按需失败的降级，而它被需要的那一刻，是它唯一为之存
// 在的那一刻。--fallback 里打错一个字，代价应该是一个启动错误，而不是
// 宕机期间一个死掉的会话。
func buildLadder(pf *providersFile, name string, pcfg providerConfig, p Provider, fallback string, cacheBreakpoints bool) (*ladder, error) {
	describe := func(n string, c providerConfig, pr Provider) ProviderInfo {
		return ProviderInfo{Name: n, Protocol: pr.Protocol(), Model: pr.Model(), Window: c.Window, Prices: c.Prices}
	}
	rungs := []rung{{p: p, info: describe(name, pcfg, p)}}
	seen := map[string]bool{name: true}

	for _, n := range strings.Split(fallback, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if seen[n] {
			// 是拒绝，不是跳过。一个把同一个供应商列两遍的梯子，读起来
			// 像多了一层韧性，实际什么都没给——第二级失败的原因跟第一级
			// 一模一样，它买到的唯一东西，是会话放弃之前更长的等待。
			return nil, fmt.Errorf("--fallback lists %q more than once (or it is already the primary)", n)
		}
		seen[n] = true

		c, resolved, err := pf.resolve(n)
		if err != nil {
			return nil, err
		}
		pr, err := c.build(cacheBreakpoints)
		if err != nil {
			return nil, fmt.Errorf("fallback %q: %w", n, err)
		}
		rungs = append(rungs, rung{p: pr, info: describe(resolved, c, pr)})
	}
	return newLadder(rungs...), nil
}

// ---------------------------------------------------------------------------
// 循环
// ---------------------------------------------------------------------------

// retryLoop 在策略之下跑一次模型调用，也是这个 Agent 里唯一根据分诊裁
// 决采取行动的地方。
//
// 它接受一个闭包而不是一个请求，只有一个理由：**压缩调用也是一次模型
// 调用**，而它是每个 Agent 都会忘掉的那一个。阶段 05 的摘要器自己发
// POST，在这一阶段之前它有自己的一套错误处理——是另一套的一半，还少了
// 响应体。现在两个调用方从同一份代码里拿到同样的决策。
//
// 两个调用方不一样的那两个旋钮是梯子和策略，而区别就是参数本身：压缩
// 传的是 nil 梯子，因为把整个会话的供应商换掉、当作一次摘要小故障的副
// 作用，不是恢复，是惊吓。压缩本来就有一个安全的失败方式——不压缩地继
// 续——所以它要的是短牵绳，和不留下持久后果。
//
// sleep 是注入进来的，这样测试可以用真实的等待跑真实的循环，却一点时
// 间都不花。这个函数里没有别的东西读时钟：预算是按它决定要等的时长累
// 加追踪的，不是一个截止时刻，这意味着只要 rnd 是确定的，整件事就是确
// 定的。
func retryLoop(
	bus *Bus, turn int, pol retryPolicy, lad *ladder,
	sleep func(time.Duration), rnd func() float64,
	do func(Provider) (*CallResult, error),
) (*CallResult, error) {
	if rnd == nil {
		rnd = rand.Float64
	}
	var waited time.Duration
	attempt, perRung := 0, 0

	for {
		attempt++
		perRung++
		at, p, _ := lad.pos()
		res, err := do(p)
		if err == nil {
			return res, nil
		}

		ce, ok := asCallError(err)
		if !ok {
			// 一个这一阶段没有建模的失败。是返回，不是重试：一个没被
			// 分类的失败拿去重试，就是把这个失败重演一遍，诚实的做法
			// 是把我们不理解的那个东西摆到明面上。
			return res, err
		}

		v := ce.triage()
		bus.Emit(Event{
			Kind:    KindCallError,
			Turn:    turn,
			Status:  ce.Status,
			Phase:   string(ce.Phase),
			ErrType: ce.Type,
			Triage:  string(v),
			Attempt: attempt,
			Text:    ce.Error(),
			// 部分结果自己的账目，前提是流走得够远、真攒下了一点。它几
			// 乎总是空的——usage 在流的末尾才到，而这条流没有结束——而这
			// 本身就是 docs/09-triage.md 里的那个发现：一条断掉的流的账
			// 单，既是真的，又是观测不到的。
			Usage: partialUsage(ce),
		})

		switch v {
		case TriageFatal:
			return res, err

		case TriageFallback:
			if !lad.advance(at) {
				// 无处可去了。调用方看到的错误是最后一个供应商的，
				// 而这是对的那一个：它才是会话没法继续的原因。
				return res, err
			}
			_, _, info := lad.pos()
			bus.Emit(Event{
				Kind: KindProvider, Turn: turn, Triage: string(TriageFallback),
				Provider: &info,
				Text:     fmt.Sprintf("fell back after %s", ce.Error()),
			})
			perRung = 0
			continue

		case TriageRetry:
			limit := pol.attempts
			if l := ce.leash(); l > 0 && l < limit {
				limit = l
			}
			if perRung >= limit {
				// 这一级上的尝试用完了。一个可重试的失败把重试用光
				// 之后，值得在放弃前朝梯子看一眼："供应商挂了"和"这
				// 个供应商挂了"是两句不同的话。
				if lad.advance(at) {
					_, _, info := lad.pos()
					bus.Emit(Event{
						Kind: KindProvider, Turn: turn, Triage: string(TriageFallback),
						Provider: &info,
						Text:     fmt.Sprintf("fell back after %d attempts: %s", perRung, ce.Error()),
					})
					perRung = 0
					continue
				}
				return res, fmt.Errorf("%d attempts: %w", perRung, err)
			}

			w := pol.wait(perRung, ce.RetryAfter, rnd)
			if waited+w > pol.budget {
				// 预算算的是挂钟时间，不是尝试次数，因为人注意到的
				// 不是"它试了四次"，而是"它在那儿坐了两分钟"。把预算
				// 按名字报出来是有意的：那是他们会想改的那个数字。
				return res, fmt.Errorf("retry budget %s exhausted after %d attempts: %w", pol.budget, perRung, err)
			}
			waited += w
			bus.Emit(Event{
				Kind: KindRetry, Turn: turn, Attempt: attempt + 1,
				Millis: w.Milliseconds(), Status: ce.Status, ErrType: ce.Type,
				Text: retryWhy(ce),
			})
			sleep(w)
			continue
		}
	}
}

// retryWhy 是打在一次等待旁边的那一行理由。它点明延迟的来源，因为"等
// 4s"和"等 4s，因为服务器要求等 4s"会导向不同的排查。
func retryWhy(ce *CallError) string {
	if ce.RetryAfter > 0 {
		return fmt.Sprintf("%s · the server asked for %s", ce.Error(), ce.RetryAfter)
	}
	return ce.Error()
}

// partialUsage 报告一次断掉的尝试好歹算清了多少，或者返回 nil。
//
// 是 nil 而不是一个零值 Usage：零会打印成"0 tokens"，读起来像"这没花
// 钱"——而留住部分结果的全部意义就在于，这正是成本非零且未知的那一种
// 情况。
func partialUsage(ce *CallError) *Usage {
	if ce.Partial == nil {
		return nil
	}
	if ce.Partial.Usage == (Usage{}) {
		return nil
	}
	u := ce.Partial.Usage
	return &u
}
