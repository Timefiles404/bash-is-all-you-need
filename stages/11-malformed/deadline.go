// 阶段 10——死锁：每一次等待都有期限，也有归属。
//
// 阶段 09 把自己的局限写下来了，并且指名这个阶段来修：
//
//	"模型调用没有期限。http.Client{Timeout: 10 * time.Minute} 是唯一
//	 的时钟，而它管的是整个响应体读取：又慢又活着的流，到第十分钟就
//	 在生成中途死掉，而且飞行中的调用没有任何东西能取消。"
//
// 这里面管线那一半——让 context.Context 到达每个会阻塞的调用——枯燥而机
// 械。有意思的那一半是：**对流式响应来说，一个超时的形状就是错的**，这
// 一个数字取什么值都修不好。
//
// # 一个数字为什么做不到
//
// http.Client.Timeout 管 dial、TLS、响应头和整个响应体读取。在流式端点
// 上，响应体读取会一直持续到模型说完，于是这一个数字同时被问了两个不相
// 干的问题：
//
//	健康的生成可以花多久？      → 分钟级，原则上没有上界；
//	                              长答案不是故障
//	**死掉**的连接能装活多久？  → 毫秒级；没有任何理由去等
//
// 设成十分钟，流在第三秒无声死掉，剩下的 597 秒就被它扣作人质。设成十
// 秒，每个长答案都会在句子中途被杀掉——而截断之前生成的东西已经生成了，
// 也已经计过费。阶段 09 那条重复计费的行，就是为了给这个错误标价才存在
// 的。
//
// # 换成三个时钟
//
//	  dial ── TLS ── headers ──┬── byte ── byte ─────── byte ── [DONE]
//	                           │        ↑           ↑
//	  ├──── connect ───────────┤        └── gap ────┘
//	  │                        │
//	  └──────────────── total ─┴─────────────────────────────────────┘
//
//	connect   响应头必须在这段时间内到达。此时还什么都没生成，所以
//	          这是唯一一种重试不花钱的失败。
//	idle      字节之间的**间隔**，不是总时长。只有它能分开慢的流和
//	          死的流。
//	total     整次调用的兜底。不是策略——是防着供应商每个空闲周期只
//	          挤出一个字节、就这么永远拖下去。
//
// 中间那个才是承重的想法。活着的流和挂死的 socket，从外面看一模一样：两
// 边都没有字节到。分开它们的只有一点：其中一个还会再吐出一个字节；而关
// 于这一点，你能拿到的唯一证据就是它已经安静了多久。
//
// # 为什么原因比期限更重要
//
// 三个时钟到期的方式都是取消同一个 context，所以等错误浮上来时它们已经
// 分不出来了——每一个都是 context.Canceled——可它们需要三个不同的裁决。
// 所以每个都**带原因**取消，分诊读的是原因，不是错误。
//
// 第四个原因是漏掉就会成为 bug 的那个：用户按下 Ctrl-C。阶段 09 把失败
// 的调用分成重试 / 降级 / 停，而仅仅被当成"错误"的中断会被重试三次，然
// 后降级到第二个供应商——这就是 Agent 用更卖力地干来回答"停"。见
// triageCause。
package main

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// 从这个程序内部取消一次调用的 context，只有这四个原因。它们是值，不是
// 字符串，因为 triageCause 要对它们 switch，而字符串里打错一个字，会一
// 声不响掉到 default 去。
var (
	// errStalled 的意思是，整个空闲期限里没有字节到达。流是建立起来了的，
	// 所以很可能已经生成了东西，也计了费。
	errStalled = errors.New("the provider stopped sending mid-stream")

	// errCallTimeout 的意思是，调用活过了自己的总预算，而且期间一直在产出
	// 字节。故意跟卡住分开：这一次是活着的。
	errCallTimeout = errors.New("the call ran past its total deadline")

	// errInterrupted 的意思是，有人要它停下来。
	errInterrupted = errors.New("interrupted")
)

// deadlines 就是整套计时策略：三个时长，每个都允许为零，为零就把那个时
// 钟关掉。零是真的设置，不是占位符——docs/wire-notes.md 里的线上探测就
// 是三个全关着做的，因为被中途砍断的探测不算证据。
type deadlines struct {
	connect time.Duration // 响应头必须在这段时间内到达
	idle    time.Duration // 字节之间能容忍的最大间隔
	total   time.Duration // 整次调用的兜底
}

func defaultDeadlines() deadlines {
	// 空闲的默认值不是猜的；见 docs/10-deadlock.md，那里在真实会话上测了
	// 相邻 SSE 帧之间的间隔，默认值定得远高于观测到的最宽那个。
	return deadlines{
		connect: 30 * time.Second,
		idle:    45 * time.Second,
		total:   15 * time.Minute,
	}
}

// stallGuard 盯着两次读之间的间隔，间隔宽过限度就取消。
//
// # 为什么用时间戳加 ticker，而不是 timer.Reset
//
// 显然的实现是每次读都 Reset 一个 time.Timer。它也是错的，而且错得只在
// 负载下才现形：Reset 和它想阻止的那次触发之间有竞争。定时器到期了，它
// 的函数已经排进队列等着跑，而本该拦住它的那次 Reset 晚到了一微秒。于
// 是调用被判成卡住，尽管字节确实来了。
//
// 窗口很小，而这恰恰是它糟糕的地方——它只在繁忙的流上偶尔响一次，看起
// 来就像供应商在抽风。
//
// 比较时间戳不会输掉这场竞争。读的那一侧是**先**写下时间，看门狗才去读
// 它，所以已经到达的字节，下一次检查一定看得见。代价是发现得晚，最多晚
// 一个 tick，而 tick 是故意取空闲窗口的四分之一：卡住会在 idle 到
// idle*1.25 之间的某处被发现，绝不会早于 idle。
type stallGuard struct {
	last atomic.Int64 // 最近一个字节的 UnixNano

	// widest 是这次调用真正见过的最大间隔，单位纳秒。
	//
	// 超时如果从不把它比的那个量显示出来，那它就只是某人猜的数字。空闲期
	// 限比的就是这个量，所以面板把它印在 TTFT 旁边，读的人能看到自己的余
	// 量，而不是信着默认值——阶段 04 讲缓存标记、阶段 09 讲重试，讲的都是
	// 同一条理由。
	widest atomic.Int64
}

// mark 记下一次字节到达，并报告它有没有刷出新的最大间隔。
//
// 那个 bool 就是这个数字到达面板的路。等 ParseStream 返回之后再发最大间
// 隔是行不通的：面板上每次调用的那一块是由 KindResponseEnd 画出来的，而
// 适配器是从它正在读的那条流**里面**发出这个事件的，所以之后再送的事件
// 会晚一次调用，印到错的那个框上。每刷新一次最大值就报一次，等响应结束
// 时渲染器手里已经是最终值了。
func (g *stallGuard) mark(now time.Time) bool {
	n := now.UnixNano()
	prev := g.last.Swap(n)
	if prev == 0 {
		return false
	}
	gap := n - prev
	if gap <= g.widest.Load() {
		return false
	}
	// 没有写成循环里的 compare-and-swap：只有读的那个 goroutine 会调
	// mark，所以写者恰好只有一个。用 atomic 是为了看门狗和面板，它们要读。
	g.widest.Store(gap)
	return true
}

// idleMax 把见过的最大间隔当成 duration 报出来。
func (g *stallGuard) idleMax() time.Duration { return time.Duration(g.widest.Load()) }

// watch 让 guard 一直跑，直到 ctx 结束，或者间隔超过 idle。
//
// now 和 tick 是注入进来的，理由和阶段 09 注入 sleep、rnd 一样：组件一
// 旦自己拿着时钟，不 sleep 就没法测；而写满 sleep 的测试套件，迟早没人
// 再跑。这里整个卡住检测器，都是靠往 channel 里塞值来跑的。
func (g *stallGuard) watch(ctx context.Context, idle time.Duration,
	cancel context.CancelCauseFunc, tick <-chan time.Time) {
	if idle <= 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-tick:
			if !ok {
				return
			}
			if now.Sub(time.Unix(0, g.last.Load())) >= idle {
				cancel(errStalled)
				return
			}
		}
	}
}

// stallReader 在每一次读出了字节的读之后，给 guard 打一次 mark。
//
// 条件是 n > 0，不是 err == nil：一次读可以同时返回数据和 io.EOF，而那
// 次读跟别的读一样是活着的证据。
type stallReader struct {
	rc    io.ReadCloser
	guard *stallGuard
	now   func() time.Time

	// bus 和 turn 是这个测量值离开本文件的通道。渲染器没有时钟——这是阶段
	// 02 的规矩，也是重放能把一次会话复现到毫秒的原因——所以它要显示的时
	// 长必须装在事件里到达，由拿着秒表的那一方测出来。在这里，那就是程序
	// 里唯一碰到 socket 的对象。
	bus  *Bus
	turn int
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 && s.guard.mark(s.now()) && s.bus != nil {
		s.bus.Emit(Event{Kind: KindIdleMax, Turn: s.turn,
			Millis: s.guard.idleMax().Milliseconds()})
	}
	return n, err
}

func (s *stallReader) Close() error { return s.rc.Close() }

// guardBody 把响应体包一层，让流在被读的同时就被盯着；它返回的 stop 函
// 数，调用方必须 defer 掉。
//
// goroutine 的归属就在这里，它随 context 一起结束——这就是这个阶段标题
// 里"也有归属"的那一半。没人去停的看门狗是一处泄漏，泄漏量随调用次数
// 增长，而子 Agent 树一来就是几百次。
// 它故意**不**接受注入的时钟，尽管 stallGuard 和 stallReader 两个都接
// 受。下面那个 ticker 是真的，所以假的 `now` 会让读的那一侧按一条时间
// 线盖戳，而看门狗对着另一条时间线比，于是完全健康的流会在第一个 tick
// 上就被判卡住。零件是在假时钟上做单元测试的；而这个函数，也就是真正
// 盯着实际 socket 的那个，从构造上就跑在真时钟上。
func guardBody(ctx context.Context, rc io.ReadCloser, idle time.Duration,
	cancel context.CancelCauseFunc, bus *Bus, turn int) (io.ReadCloser, *stallGuard, func()) {
	if idle <= 0 {
		// 照样包一层，这是故意的：时钟关了就没有看门狗，但最大间隔还是值得
		// 测。把期限关掉，不该顺手把那个能告诉你该设多少的仪表也关掉。
		g := &stallGuard{}
		g.mark(time.Now())
		return &stallReader{rc: rc, guard: g, now: time.Now, bus: bus, turn: turn}, g, func() {}
	}
	g := &stallGuard{}
	g.mark(time.Now())

	// 取窗口的四分之一：够密，发现的时刻不会比期限晚太多；也够稀，45 秒
	// 的空闲窗口一分钟只醒四次，不是空转。
	t := time.NewTicker(idle / 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.watch(ctx, idle, cancel, t.C)
	}()
	stop := func() {
		t.Stop()
		cancel(context.Canceled) // 就算响应体永远不 EOF 也能结束 watch()
		<-done
	}
	return &stallReader{rc: rc, guard: g, now: time.Now, bus: bus, turn: turn}, g, stop
}

// waitFor 是一次能被取消提前打断的 sleep。
//
// time.Sleep 打不断，所以建在它上面的重试退避，最长会把整段等待里的
// Ctrl-C 全吞掉。按阶段 09 的默认值那是八秒：程序已经被叫停了，还要再
// 走八秒。用户读到的是卡死，回应它的是第二次 Ctrl-C——而第二次会在
// trace 落盘之前杀掉进程。
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// triageCause 把一次被取消的调用变成阶段 09 那三个裁决之一。
//
// 它跑在普通分类器**之前**，因为错误一旦走过 http 包，这些就全都是同一
// 个 context.Canceled，失败的形状已经没了。还知道是哪个时钟触发的，只
// 剩 context.Cause。
//
// 四个答案，以及每一个为什么不是另外三个：
//
//	interrupted  fatal，而且是既不许重试**也**不许降级的那一个。
//	             人说了停。Agent 拿"去试第二个供应商"来回答这句
//	             话，就是把停止按钮变成了扇出。
//	stalled      retry。它和阶段 09 已经在重试的那些传输失败没有
//	             任何区别；它是一条死连接，花了整个空闲窗口才证
//	             明自己死了。
//	call timeout fatal。这一次是活着的，只是太慢，所以同一个请求
//	             再发一次还会一样慢——而每次尝试都要为截断之前生
//	             成的每个 token 付钱。会被重试的兜底不是兜底。
//	parent gone  fatal。结束的那个 context 不是这次调用自己的；是
//	             回合，或者整个程序，正在它周围关停。
func triageCause(err error) (Triage, string, bool) {
	switch {
	case errors.Is(err, errInterrupted):
		return TriageFatal, "interrupted", true
	case errors.Is(err, errStalled):
		return TriageRetry, "stalled", true
	case errors.Is(err, errCallTimeout):
		return TriageFatal, "call_timeout", true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return TriageFatal, "cancelled", true
	}
	return "", "", false
}
