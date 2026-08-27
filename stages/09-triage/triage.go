// 阶段 09——分诊。
//
// 全篇只有一个论点：**错误是决策，不是字符串。**
//
// Agent 的模型调用刚失败，它手上正好只有三个动作：等一等，把同样的字节再发
// 一遍；发到别处去；停下来，说清为什么。这个文件里的每一样东西，都是为了把
// 一次失败归到这三个里去。它是个文件而不是一句 `if`，是因为人人上手时都会
// 先立的那两条规则，放到这个仓库开发时对着的那个端点上，两条都错：
//
//	"401 就是密钥不对，所以停。"
//	    在这里，**模型名**写错也回 401，信封形状跟被吊销的密钥一模一样
//	    （§D11）。对其中一种，停下来是对的；对另一种，这一停就把还能用
//	    的会话扔了。
//
//	"5xx 是瞬态的，所以退避重试。"
//	    在这里，请求体畸形回的是 500（§D11）。那是客户端的 bug，而只看
//	    状态码的策略会永远重试下去——重试永远不成功，也永远不停。
//
// 把状态码读得更仔细，这两条都救不回来。要救就得把失败本身带够信息来做判
// 断：它在哪儿断的，供应商管它叫什么，响应体到底写了什么。这就是这个文件和
// `fmt.Errorf` 的区别。
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

// callPhase 说的是失败发生在一次模型调用的哪个环节，也是分类器第一眼要看的
// 东西——因为它回答的是决定其余一切的那个问题：*到底有没有生成过东西？*
//
// 什么都没生成，重试就是免费的。生成了东西，重试就要再花一整份 prompt 的
// 钱，而第一份已经记在账单上了。
//
// 叫它 phase（环节）而不叫 stage，是因为在其他每个文件里 "stage" 都已经指本
// 课程的一章；而在名叫 stages/09-triage 的目录里，给事件加个叫 Stage 的字
// 段，这句话没人能读两遍读出同一个意思。
type callPhase string

const (
	phaseBuild   callPhase = "build"   // 连请求都渲染不出来
	phaseConnect callPhase = "connect" // 完全没有响应：DNS、被拒、TLS、响应头之前就被 reset
	phaseStatus  callPhase = "status"  // 响应到了，但不是 200
	phaseStream  callPhase = "stream"  // 200，然后响应体断了，或者带来了 error 事件
)

// CallError 是一次失败的模型调用，形状是能拿来做决策的那种。
//
// 每个字段都在这儿，是因为下面某条分诊规则要用它，而字符串装不下。看着最多
// 余的那个——挤在 Type 和 Message 旁边的 Body——恰恰是最常派上用场的：在这个
// 网关上，实测到的 400 根本没带错误信封，只有请求本身的一段 24 字节回显
// （`{"model":"qwen3.7-plus"}`，§D11）。那次响应的 Type 和 Message 都是空
// 的，原始响应体是唯一的证据。
type CallError struct {
	Phase   callPhase
	Status  int    // 没有响应时为 0
	Type    string // 供应商给的 error.type，原样保留——绝不归一化，因为它是证据
	Message string
	Body    string // 响应体的前 8 KiB，原样保留

	// RetryAfter 是服务器要我们等的时长，它没要求就是 0。服务器给的数字永远压
	// 过我们自己算出来的退避：这场对话里，只有它知道容量什么时候回来。
	RetryAfter time.Duration

	// Partial 是流断掉那一刻适配器已经攒下的东西。
	//
	// 留着而不是丢掉——这就是阶段 09要来补的那道缝。两个适配器都是故意在
	// 返回非 nil 错误的同时返回非 nil 结果，openai.go 和 anthropic.go 的注释
	// 里都写了；而在此之前的每个阶段，都只是把这个值接住，从没看过一眼。
	//
	// 这里没有任何地方把部分结果喂回模型：它做到一半的计划已经没了，半个工具
	// 调用比没有更糟。留它是为了账。那些 token 是生成过的，生成过的 token 就
	// 要付钱，所以把部分结果随手扔掉的 Agent，就是说不清自己账单的 Agent——而
	// 整个仓库讲的就是这个失败。
	Partial *CallResult

	Err error // 底层的传输错误，这样 errors.Is 还能用
}

func (e *CallError) Error() string {
	switch e.Phase {
	case phaseBuild:
		return fmt.Sprintf("could not build the request: %v", e.Err)
	case phaseConnect:
		return fmt.Sprintf("no response from the provider: %v", e.Err)
	case phaseStatus:
		// 信封缺失这种情况给的是一条明显不同的消息，而不是空的。
		// "http 400: " 冒号后面什么都没有，读起来像 Agent 自己的
		// bug；把这份缺失说出来，才是指向服务器。
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

// asCallError 就是把目标变量声明好的 errors.As，不然每个调用点都要跳一遍那
// 三行舞。
func asCallError(err error) (*CallError, bool) {
	var ce *CallError
	ok := errors.As(err, &ce)
	return ce, ok
}

// ---------------------------------------------------------------------------
// 决策
// ---------------------------------------------------------------------------

// Triage 是对一次失败该怎么办。三个取值，因为 Agent 只有三个动作——而按动作
// 命名、不按失败命名，就是整个文件的要点。`ErrRateLimited` 告诉你发生了什
// 么；`TriageRetry` 告诉你该做什么，而调用方需要的只有后者。
type Triage string

const (
	TriageRetry    Triage = "retry"    // 同样的字节，晚点再发
	TriageFallback Triage = "fallback" // 同样的字节，发到别处
	TriageFatal    Triage = "fatal"    // 停下来，说清为什么
)

// triage 把一次失败映射成一个决策。
//
// 读它的时候请对着 docs/wire-notes.md 的 §D11 一起读。这里几乎每一行的存在
// 理由，都是记录下来的字节推翻了那条显然的规则。
func (e *CallError) triage() Triage {
	switch e.Phase {
	case phaseBuild:
		// 我们自己的 bug。渲染不出来的请求，第二次尝试照样渲染不出
		// 来，而重试它烧掉的预算，正是会话后面真出故障时要用的。
		return TriageFatal

	case phaseConnect:
		// 没有响应就意味着什么都没生成、什么都没计费，所以这是唯一
		// 一类重试真正免费的失败。DNS、连接被拒、TLS 握手、响应头
		// 之前的 RST——它们都值得再试一次；而在尝试次数用完之前，它
		// 们都不值得换供应商。
		return TriageRetry

	case phaseStream:
		// 线上断掉的流，和带来 error *事件* 的流，今天在 Go 里是
		// 同一个错误，但决策绝不能相同。
		//
		// Type == "" 是传输那一种：连接在响应体中途死了。值得再试一次。
		//
		// 有名字的 type 是供应商在响应中途故意发出来的，其中只有下面
		// 这四个的意思是"再问一次"。其余那些是因为我们发过去的东西才来
		// 的，再发一遍还会产生同一个事件——这是 5xx 陷阱在
		// phaseStream 上的孪生兄弟。
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

// triageStatus 单独拆出来，因为意外都在这一段里，而表驱动测试想直接调它。
func triageStatus(status int, typ string) Triage {
	t := strings.ToLower(typ)

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// §D11，推动整个文件成立的那个发现：在这个网关上，不存在的
		// model id 回的是 **401** 而不是 404，信封和被吊销的密钥一
		// 模一样，都是 `{"type":"error","error":{...}}`。状态码分不
		// 开它们，`error.type` 分得开——`ModelError` 对 `AuthError`
		// ——而这两者需要相反的决策，因为一个是死掉的会话，另一个是
		// 指错了模型名的、还活着的会话。
		//
		// 按子串匹配，这是故意的。实测到的值是 PascalCase
		// （`ModelError`），而两份协议规范写的都是 snake_case
		// （`not_found_error`、`invalid_request_error`），所以拿任一
		// 种拼法做相等判断，对着文档是对的，对着线上是错的。
		if strings.Contains(t, "model") {
			return TriageFallback
		}
		return TriageFatal

	case status == http.StatusNotFound:
		// 路由或模型不在这个端点上。别的端点可能有；等下去也不会
		// 让它在这儿出现。
		return TriageFallback

	case status == http.StatusTooManyRequests:
		return TriageRetry

	case status == http.StatusRequestTimeout, status == http.StatusConflict:
		return TriageRetry

	case status == http.StatusRequestEntityTooLarge:
		// 问题就是这堆字节。等待和换供应商都改不了它们；能改的是上
		// 下文压缩，而那是阶段 05的活，不是这里的。这里判致命，
		// 是为了让消息传到那个能把它变小的人手里。
		return TriageFatal

	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		// 我们的问题。服务器拒了的参数你还去重试，客户端的 bug 就是
		// 这样变成一次故障的。
		return TriageFatal

	case status >= 500:
		// 会重试，但牵绳是这里所有情况里最短的——见 leash()。
		//
		// 原因是 §D11：在这个网关上，请求体畸形回的是 **500**，把
		// OpenAI 形状的请求体 POST 到 Anthropic 路由上也一样回 500。
		// 两者都是客户端的 bug，穿着服务器的状态码。"5xx = 瞬态"会
		// 一直重试它们，直到预算耗尽，每个回合如此，永远如此。
		return TriageRetry
	}

	// 上面谁都没认领的状态码。判致命而不是重试，因为没分类的失败拿去重试，
	// 只是把失败重复一遍——而这里发出的事件，正是漏掉的情况被发现的途径。
	return TriageFatal
}

// leash 给一类失败值得的尝试次数设上限，0 表示"策略给的全额"。
//
// 一条规则，一个理由。503 是真的容量信号，给它全部额度。光秃秃的 500 总共
// 只给两次尝试，因为在这个端点上，它是*我们*配错的可能性至少不比对方故障低
// （§D11），而两次尝试足够熬过一次抽风，却远远不够把一个永久性错误藏在重试
// 循环后面。
func (e *CallError) leash() int {
	if e.Phase == phaseStatus && e.Status >= 500 && e.Status != http.StatusServiceUnavailable {
		return 2
	}
	return 0
}

// ---------------------------------------------------------------------------
// 等多久
// ---------------------------------------------------------------------------

// retryPolicy 就是重试的全部配置，它是四个数；人们会加的第五个——"一直重试
// 到成功"——正是瞬态失败变成账单的方式。
type retryPolicy struct {
	attempts int           // 每个供应商的总尝试次数，含第一次
	base     time.Duration // 第一次退避
	max      time.Duration // 单次等待的上限
	budget   time.Duration // 一次调用里所有等待加起来的上限
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{attempts: 3, base: 500 * time.Millisecond, max: 8 * time.Second, budget: 30 * time.Second}
}

// wait 返回第 n 次尝试之前要睡多久（n 从 1 数起，所以第一次等待是 wait(1)，
// 发生在第一次失败之后）。
//
// 用的是全抖动——在 [0, exp) 上均匀取——而不是更常见的
// `exp/2 + rand(exp/2)`。这个差别在这里格外要紧：阶段 07会派出子 Agent，
// 它们共用一个 http.Client、一个端点，所以供应商一打嗝，同一毫秒里就有好几
// 个调用一起失败。半抖动会让这些调用继续挤在一堆，每次尝试都重新撞在一起；
// 全抖动把它们摊开到整个区间上。会把自己的客户端同步起来的重试策略，就是一
// 台压力发生器，对准的还是那个已经撑不住的服务。
//
// after 是服务器给的 Retry-After。它在场就直接赢，因为这个函数里只有它来自
// 知道容量何时恢复的那一方——但它仍然被调用方的预算夹住，毕竟服务器也有权说
// "一个小时"。
func (p retryPolicy) wait(n int, after time.Duration, rnd func() float64) time.Duration {
	if after > 0 {
		if after > p.max*8 {
			// 服务器要的时长，可能比我们愿意让一个回合挂着的时间更长。
			// 请求的形式要尊重，长度不必：不然 Agent 会看起来卡死一小时。
			return p.max * 8
		}
		return after
	}
	exp := p.base << (n - 1)
	if exp > p.max || exp <= 0 { // <= 0 用来接住移位溢出
		exp = p.max
	}
	return time.Duration(rnd() * float64(exp))
}

// parseRetryAfter 按 RFC 9110 允许的两种形式读这个头：delta-seconds 和
// HTTP-date。
//
// 这段是照 RFC 写的，不是照观测写的，本章也如实说了：docs/wire-notes.md 里
// 那个网关从来没发过 429，所以本仓库的证据里根本没有记录到过 Retry-After。
// 测试是拿本地服务器来练它的。这比本文件其余部分的立足点要弱，而把这份弱点
// 说出来，好过暗示存在一次并不存在的测量。
//
// now 是参数，因为日期形式是相对它算的；而测试若定不住"现在"，日期形式就完
// 全没法测。
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
		// 过去的日期意思是"现在"，不是"睡负数"。
		return 0
	}
	// 解析不了。宁可忽略也不去猜：退回到算出来的退避是个已知安全的数，
	// 从一个畸形的头里编出来的不是。
	return 0
}

// ---------------------------------------------------------------------------
// 错误信封
// ---------------------------------------------------------------------------

// errEnvelope 用一个结构体覆盖两种形状，这之所以做得到，全靠 §D11 的一个发
// 现：这个网关连 OpenAI 路由也返回 **Anthropic** 的信封。嵌在里面的
// `error` 对象是两边一致的那部分，所以读的就是那部分。
type errEnvelope struct {
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    any    `json:"code"` // OpenAI 的；有时是字符串，有时是 null——所以用 `any`
	} `json:"error"`
}

// parseErrorBody 抽出两份协议一致的那部分，并且在它们毫无一致之处时也活得下
// 来。
//
// 有三种实测到的形状要从这里过（§D11）：
//
//	{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}
//	{"type":"error","error":{"type":"error","message":"Internal server error"}}
//	{"model":"qwen3.7-plus"}                       <- 一个 400，根本没有信封
//
// 第三种就是这里返回两个空串而不是返回错误的原因。信封缺失不是一桩要上报的
// 解析失败，它是关于这次响应的一个事实，而调用方本来就为这种情况留着原始响
// 应体。在这里返回错误，就等于让 Agent 报"解析不了这个错误"，而不是把错误报
// 出来。
//
// 顺带一提，它是以 `Content-Type: text/plain;charset=UTF-8` 送来的，这也正是
// 这个函数里没有任何地方去看 content type 的原因。
func parseErrorBody(raw []byte) (typ, msg string) {
	var env errEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error == nil {
		return "", ""
	}
	return env.Error.Type, env.Error.Message
}

// ---------------------------------------------------------------------------
// 改发到哪里
// ---------------------------------------------------------------------------

// rung 是降级梯子上的一级——一家供应商，以及跟着它的身份和价格。
type rung struct {
	p    Provider
	info ProviderInfo
}

// ladder 是一个会话可以用的供应商有序列表：配置的那个在最前，其余的只有靠
// TriageFallback 这个裁决才到得了。
//
// 它故意不是负载均衡器，也故意不是熔断器。这里没有任何东西在分摊流量，顺序
// 也从不改变。两个后果，与其让人自己发现，不如直接说出来：
//
//   - 它从不往回爬。会话一旦掉到第 1 级就留在那儿，因为"主供应商是不是好
//     了"这个问题，不真花一次调用去问是答不出来的。所以降级是你留着的降级，
//     这也正是每次切换都要发 KindProvider 的原因——会话被便宜模型悄悄服务了
//     一小时，就是这个设计为了简单而换来的失败，而它只有在看得见的时候才活
//     得下来。
//
//   - 每一级有自己的价格和自己的上下文窗口。面板是按事件重新计价的，不是按
//     启动时的配置，因为另一条路是：成本报告悄悄拿第一家供应商的费率去算第
//     二家的 token。
//
// 那把互斥锁不是装饰。阶段 07的子 Agent 是并发跑的，而且是按指针共用这个
// 梯子——这是故意的，因为"端点挂了"是端点的属性，不是哪个 Agent 先发现的属
// 性，所以父 Agent 不该重新发现一遍子 Agent 已经付过账的失败。共用就意味着
// 两个子 Agent 可能在同一微秒里调用 advance()。
type ladder struct {
	mu    sync.Mutex
	rungs []rung
	cur   int
}

func newLadder(rungs ...rung) *ladder { return &ladder{rungs: rungs} }

// pos 在一把锁里返回当前的级号和这一级上的所有东西，因为调用方要是分两次去
// 取供应商和级号，就可能对着两个不同的级动手——而级号正是它后面要传给
// advance 的东西。
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

// advance 从 `from` 这一级迈开，无处可去时返回 false。
//
// 它取的是调用方所在的级，而不是自己去读当前级；由此多出来的那两行，就是全
// 部的并发故事。阶段 07的子 Agent 是并发打同一个端点的，所以一家供应商挂
// 了会一次弄失败好几个调用，而它们每一个都想降级。
//
// 对两个在同一级失败的兄弟来说，`cur = from + 1` 本来就是幂等的：两个写的是
// 同一个值。真正需要那道守卫的情形有三个参与者，而且它是一次*回退*，不是多
// 迈了一步。A 在第 0 级失败，走到第 1 级；C 在第 1 级失败，走到第 2 级；然后
// B——手里还攥着这一切发生之前读到的第 0 级——来要求降级。没有守卫，它会写下
// cur = 1，把下一次调用发给两个兄弟都已经放弃的供应商。有守卫，B 会被告知
// *可以*，同时什么都不挪动；而这才是它真正在问的那个问题的诚实答案："还有
// 别处可发吗？"有，梯子早就在那儿了。
func (l *ladder) advance(from int) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur > from {
		return true // 已经有兄弟把我们挪走了；这算成功，不算迈一步
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

// modelCall 执行一次模型调用的一次尝试：渲染请求、发出去、解析流，并给任何
// 出岔子的东西分类。
//
// 一个函数，两个调用方——Agent 的回合，和阶段 05的摘要器。在这个阶段之前
// 它们是两份拷贝，而且已经漂开了：上下文压缩那份不读响应体，所以压缩失败时
// 只报一个 `http 500`，别的什么都没有——那正是唯一没法调试的东西。共用代码其
// 实不是重点。共用*分类表*才是，因为两条路径分类不一致的失败，你没法为它写
// 出一条策略。
//
// 它每次尝试都重新构建请求，而不是复用一个，这不是随手为之：*http.Request 的
// body 在第一次 Do 之后就是一个读完了的 reader，所以重发同一个请求对象的重
// 试，会送出零个字节、拿回一个 400——重试里的 bug，长得跟服务器的 bug 一模一
// 样。
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
			// 这是本仓库读过的第一个响应头。在此之前，就算真来了
			// 一个 Retry-After，Agent 也没法照办，因为它从不去看。
			RetryAfter: parseRetryAfter(resp.Header, time.Now()),
		}
	}

	res, err := p.ParseStream(resp.Body, bus, turn, started)
	if err != nil {
		// 两个适配器都在返回非 nil 错误的同时返回非 nil 结果，这是
		// 故意的，也都写了注释；而在此之前的每个阶段，都只是把那个
		// 值接住，从没读过。到这里它成了 CallError.Partial——为什么
		// 留它的理由是账，见那个字段。
		//
		// 流内的 error **事件**本来就是以 *CallError 的形式从
		// anthropic.go 过来的，带着供应商自己的 error.type。对它做
		// 增补而不是包一层，那个 type 才留在分类器能拿到的地方；而
		// 这就是熬过一次容量抽风和干脆放弃之间的差别。
		if ce, ok := asCallError(err); ok {
			ce.Partial = res
			return res, ce
		}
		return res, &CallError{Phase: phaseStream, Message: err.Error(), Err: err, Partial: res}
	}
	return res, nil
}

// forCompaction 是同一套策略，牵绳短一些。
//
// 上下文压缩本来就有一条安全的失败路：Agent 不压缩继续跑，并在总线上说出
// 来。所以早点放弃代价很小，而死命重试代价极大——每次尝试都按全价把整份对话
// 记录重发一遍，而且是在那个需要腾地方的回合还等着的时候干的。两次尝试、五
// 秒，不管会话的策略是什么。
func (p retryPolicy) forCompaction() retryPolicy {
	if p.attempts > 2 {
		p.attempts = 2
	}
	if p.budget > 5*time.Second {
		p.budget = 5 * time.Second
	}
	return p
}

// buildLadder 用解析好的主供应商和 --fallback 把梯子搭起来。
//
// 每一级都在启动时、第一个请求之前构建好，密钥也检查过。这就是它是个函数、
// 而不是失败当口才去懒查的全部理由：按需搭出来的降级，就是按需失败的降级，
// 而它被需要的那一刻，正是它唯一存在的意义。--fallback 里打错一个字，代价应
// 该是启动报错，不是故障期间一个死掉的会话。
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
			// 拒掉，而不是跳过。同一家供应商在梯子上列两遍，读起来像多了一层
			// 韧性，实际一点都没多——第二级失败的原因跟第一级一模一样，它买到
			// 的只有会话放弃之前更长的等待。
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

// retryLoop 在策略下跑一次模型调用，也是这个 Agent 里唯一按分诊裁决行事的地
// 方。
//
// 它收的是闭包而不是请求，只有一个理由：**上下文压缩那次调用也是模型调用**，
// 而它正是每个 Agent 都会忘的那一个。阶段 05的摘要器自己发 POST，在这个阶
// 段之前，它有自己的一套错误处理——另一套的一半，还少了响应体。现在两个调用
// 方从同一份代码里拿到同样的决策。
//
// 两个调用方不一样的旋钮是梯子和策略，而差别就写在参数里：上下文压缩传的梯
// 子是 nil，因为把整个会话的供应商换掉，只是摘要打了个嗝的副作用，那不叫恢
// 复，那叫惊喜。上下文压缩本来就有一条安全的失败路——不压缩继续跑——所以它要
// 的是短牵绳，不留后患。
//
// sleep 是注入进来的，这样测试能拿真实的等待跑真实的循环，而且完全不花时间。
// 这个函数里再没有别的地方读时钟：预算是按它决定等的时长累加着记的，不是按
// 截止时刻，这意味着只要 rnd 是确定的，整件事就是确定的。
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
			// 这个阶段没有建模的失败。返回，不重试：没分类的失败
			// 拿去重试，只是把失败重复一遍，而诚实的做法是把我们
			// 不懂的东西摆到台面上。
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
			// 部分结果自己那份账，前提是流跑得够远、真攒下了一点。
			// 它几乎总是空的——usage 在流的末尾才到，而这个流没有末
			// 尾——这件事本身就是 docs/09-triage.md 里的那个发现：断
			// 掉的流的账单，是真实的，同时又是观测不到的。
			Usage: partialUsage(ce),
		})

		switch v {
		case TriageFatal:
			return res, err

		case TriageFallback:
			if !lad.advance(at) {
				// 无处可去了。调用方看到的错误是最后一家供应商的，这是对的：
				// 它才是会话没法继续的原因。
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
				// 这一级的尝试次数用完了。可重试的失败重试到没了，放弃之前值得往
				// 梯子上看一眼："供应商挂了"和"这家供应商挂了"是两句不同的话。
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
				// 预算算的是挂钟，不是尝试次数，因为人
				// 注意到的不是"它试了四次"，而是"它在那儿
				// 坐了两分钟"。把预算的名字报出来是故意的：
				// 那正是他们想改的那个数。
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

// retryWhy 是打在一次等待旁边的那行原因。它点明延迟的来源，因为"等 4s"和
// "等 4s，因为服务器要求等 4s"会导向不同的排查。
func retryWhy(ce *CallError) string {
	if ce.RetryAfter > 0 {
		return fmt.Sprintf("%s · the server asked for %s", ce.Error(), ce.RetryAfter)
	}
	return ce.Error()
}

// partialUsage 报告一次断掉的尝试算清了多少，算不清就返回 nil。
//
// 返回 nil 而不是零值 Usage：零会打成 "0 tokens"，读起来像"这没花钱"——而留
// 着部分结果的全部意义，就在于这恰恰是花费非零且未知的那种情况。
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
