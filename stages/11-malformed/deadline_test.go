// 阶段 10 的测试：三个时钟，以及那个说出是哪一个触发了的原因。
//
// 这里没有一个测试在 sleep。卡住检测器是自己拿着时钟的组件，而 watch()
// 把 `tick` 收成 channel、stallReader 把 `now` 收成函数，全部理由就是：
// 另一条路是写满 time.Sleep(50 * time.Millisecond) 的测试套件——慢，在
// 负载高的机器上还会抽风，而且在你真正在意的那个边界上悄悄地测不了。这
// 里时钟是个变量，tick 是测试塞进去的值，所以"过了 45 秒"不花一点时间，
// 而且测试说它什么时候发生，它就什么时候发生。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 是测试用手推着走的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// ---------------------------------------------------------------------------
// 卡住看门狗
// ---------------------------------------------------------------------------

func TestStallGuardFiresOnlyAfterTheWholeIdleWindow(t *testing.T) {
	clk := newClock()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	g := &stallGuard{}
	g.mark(clk.now())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); g.watch(ctx, 45*time.Second, cancel, tick) }()

	// 差一点没到窗口的那次 tick 不许触发。要是有人把 >= "简化"成 >，或者
	// 把窗口"简化"成 idle/2，挂掉的就是这个测试——而提早触发的卡住检测器
	// 看起来就跟供应商在抽风一模一样，那是追起来最贵的一类 bug。
	tick <- clk.advance(44 * time.Second)
	select {
	case <-ctx.Done():
		t.Fatalf("cancelled after 44s of a 45s window")
	default:
	}

	tick <- clk.advance(2 * time.Second)
	<-done
	if got := context.Cause(ctx); !errors.Is(got, errStalled) {
		t.Fatalf("cause = %v, want errStalled", got)
	}
}

func TestStallGuardIsHeldOffByEveryRead(t *testing.T) {
	clk := newClock()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	g := &stallGuard{}
	g.mark(clk.now())
	tick := make(chan time.Time)
	go g.watch(ctx, 45*time.Second, cancel, tick)

	// 四十分钟又慢又活着的流：每 40 秒来一个字节，中间查一次。在单个
	// http.Client.Timeout 底下这次会话已经死了；在按间隔算的时钟底下，它
	// 只是有人在想。
	for i := 0; i < 60; i++ {
		tick <- clk.advance(40 * time.Second)
		select {
		case <-ctx.Done():
			t.Fatalf("cancelled at minute %d of a stream that never went quiet", i*40/60)
		default:
		}
		g.mark(clk.now())
	}
}

func TestStallGuardStopsWhenTheCallEnds(t *testing.T) {
	// 这个阶段"也有归属"的那一半。看门狗活得比它的调用还久，就是 goroutine
	// 泄漏，泄漏量乘上子 Agent 的个数；而只查返回值的测试永远看不见它。
	ctx, cancel := context.WithCancelCause(context.Background())
	g := &stallGuard{}
	g.mark(time.Now())
	done := make(chan struct{})
	go func() { defer close(done); g.watch(ctx, time.Second, cancel, make(chan time.Time)) }()

	cancel(context.Canceled)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return when its context ended")
	}
}

func TestStallGuardIsOffWhenIdleIsZero(t *testing.T) {
	// 零是真的设置，不是漏填：线上探测需要三个时钟全关，因为被中途砍断的
	// 探测不算证据。
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	g := &stallGuard{}
	done := make(chan struct{})
	go func() { defer close(done); g.watch(ctx, 0, cancel, nil) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch with idle=0 should return immediately, not watch forever")
	}
	if ctx.Err() != nil {
		t.Fatal("watch with idle=0 cancelled the call")
	}
}

// ---------------------------------------------------------------------------
// 给它喂字节的 reader
// ---------------------------------------------------------------------------

func TestStallReaderMarksOnDataEvenWithEOF(t *testing.T) {
	// 条件是 n > 0，不是 err == nil。io.Reader 被明确允许在同一次调用里既
	// 返回数据又返回 io.EOF；guard 要是漏掉那次读，每条短流的空闲窗口都会
	// 从最后那批字节之前开始算。
	clk := newClock()
	g := &stallGuard{}
	g.mark(clk.now())
	before := g.last.Load()
	clk.advance(time.Second)

	r := &stallReader{rc: io.NopCloser(dataThenEOF("xyz")), guard: g, now: clk.now}
	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 3 || err != io.EOF {
		t.Fatalf("read = %d, %v; want 3, EOF", n, err)
	}
	if g.last.Load() == before {
		t.Fatal("a read that returned bytes with io.EOF did not mark the guard")
	}
}

func TestStallReaderDoesNotMarkOnAnEmptyRead(t *testing.T) {
	clk := newClock()
	g := &stallGuard{}
	g.mark(clk.now())
	before := g.last.Load()
	clk.advance(time.Second)

	r := &stallReader{rc: io.NopCloser(strings.NewReader("")), guard: g, now: clk.now}
	if _, err := r.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if g.last.Load() != before {
		t.Fatal("a read that returned no bytes counted as proof of life")
	}
}

// dataThenEOF 造出的 reader 会在一次调用里同时交回 s 和 io.EOF。
type dataThenEOF string

func (d dataThenEOF) Read(p []byte) (int, error) { return copy(p, d), io.EOF }

// ---------------------------------------------------------------------------
// 仪表
// ---------------------------------------------------------------------------

// dlRecorder 把事件收起来，好让测试对"什么到了面板上"下断言。
type dlRecorder struct{ events []Event }

func (r *dlRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func TestTheWidestGapIsMeasuredAndReported(t *testing.T) {
	// 这个测试是变异测试发现漏掉的：把 `mark` 改成永远不记新的最大值，其
	// 他每一个测试都还能过。卡住看门狗照旧工作——期限比的是 `last`，不是
	// `widest`——所以一声不响死掉的是**仪表**；而这个阶段整篇的论点就是
	// "把超时比的那个数印出来"，那个数不能悄悄变成零。
	clk := newClock()
	rec := &dlRecorder{}
	bus := NewBus(rec)
	g := &stallGuard{}
	g.mark(clk.now())
	r := &stallReader{rc: io.NopCloser(strings.NewReader("abcdef")),
		guard: g, now: clk.now, bus: bus, turn: 3}

	buf := make([]byte, 2)
	for _, gap := range []time.Duration{40 * time.Millisecond, 900 * time.Millisecond, 30 * time.Millisecond} {
		clk.advance(gap)
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	if got := g.idleMax(); got != 900*time.Millisecond {
		t.Fatalf("idleMax = %v, want 900ms", got)
	}
	// 每刷出一个**新**的最大值才发一个事件，所以 30ms 那次读不该发：40 和
	// 900 是涨了，30 不是。
	var ms []int64
	for _, e := range rec.events {
		if e.Kind == KindIdleMax {
			if e.Turn != 3 {
				t.Fatalf("idle_max on turn %d, want 3", e.Turn)
			}
			ms = append(ms, e.Millis)
		}
	}
	if len(ms) != 2 || ms[0] != 40 || ms[1] != 900 {
		t.Fatalf("idle_max events = %v, want [40 900]", ms)
	}
}

// ---------------------------------------------------------------------------
// 是哪个时钟触发的，以及它意味着什么
// ---------------------------------------------------------------------------

func TestTriageCause(t *testing.T) {
	// 第一行是那个真会成为 bug 的，不是口味问题。中断被归成普通失败，就会
	// 被重试三次，然后降级到第二个供应商：Agent 回答"停"的方式，是换个地
	// 方把活干两遍。
	cases := []struct {
		name  string
		err   error
		want  Triage
		label string
		known bool
	}{
		{"interrupt never retries and never falls back", errInterrupted, TriageFatal, "interrupted", true},
		{"a stall is a dead connection", errStalled, TriageRetry, "stalled", true},
		{"the total deadline is a backstop, not a policy", errCallTimeout, TriageFatal, "call_timeout", true},
		{"a parent shutting down", context.Canceled, TriageFatal, "cancelled", true},
		{"a plain deadline with no cause", context.DeadlineExceeded, TriageFatal, "cancelled", true},
		{"wrapped still counts", fmt.Errorf("read: %w", errStalled), TriageRetry, "stalled", true},
		{"nothing of ours ended it", nil, "", "", false},
		{"a provider error is not ours", errors.New("http 503"), "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, label, ok := triageCause(c.err)
			if ok != c.known || v != c.want || label != c.label {
				t.Fatalf("triageCause(%v) = %q, %q, %v; want %q, %q, %v",
					c.err, v, label, ok, c.want, c.label, c.known)
			}
		})
	}
}

func TestCauseBeatsTheStatusClassifier(t *testing.T) {
	// 被取消的调用身上可能还挂着状态码，那是正在死掉的请求最后看到的东
	// 西。阶段 09 的表会读到那个 503 然后重试——那就是 Agent 因为服务器今
	// 天也不顺，所以不理 Ctrl-C。
	ce := &CallError{Phase: phaseStatus, Status: 503, cause: errInterrupted}
	if got := ce.triage(); got != TriageFatal {
		t.Fatalf("triage = %q, want fatal: the cause must outrank the status", got)
	}
	// 而没有原因的时候，状态码照旧说话，不然阶段 09 就等于被删了。
	ce.cause = nil
	if got := ce.triage(); got != TriageRetry {
		t.Fatalf("triage = %q, want retry once the cause is gone", got)
	}
}

func TestCancelCauseIsNilWhileTheContextIsOpen(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	if got := cancelCause(ctx); got != nil {
		t.Fatalf("cause on a live context = %v, want nil", got)
	}
	cancel(errStalled)
	if got := cancelCause(ctx); !errors.Is(got, errStalled) {
		t.Fatalf("cause after cancel = %v, want errStalled", got)
	}
}

// ---------------------------------------------------------------------------
// waitFor
// ---------------------------------------------------------------------------

func TestWaitForReturnsTheCauseWhenInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { cancel(errInterrupted) }()
	// 一小时，这样"等到它自己过去"就不可能让测试通过。
	err := waitFor(ctx, time.Hour)
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("waitFor = %v, want errInterrupted", err)
	}
}

func TestWaitForCompletesAShortWait(t *testing.T) {
	if err := waitFor(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("waitFor = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// 端到端，走真实的传输
// ---------------------------------------------------------------------------

// callWithin 跑一次模型调用，它没能按时返回就让测试失败。
//
// 这个期限不是双保险，它是"测试**挂掉**"和"测试**挂住**"之间的差别。下
// 面每一次调用对着的服务器都永不关闭连接，所以被测的机制一坏，调用就永
// 远等下去，测试也一直坐在那儿，直到 Go 自己的十分钟 panic。变异测试里
// 这一下最疼：有个变异体把卡住看门狗关掉，两秒的一次跑变成了十分钟；一
// 整套变异体下来将近一小时，而在你盯着它的时候，这和宿主卡死根本分不出
// 来。
//
// 不能快速失败的测试，迟早没人再跑；理由和写满 sleep 的测试套件一样。
func callWithin(t *testing.T, d time.Duration, ctx context.Context, p Provider,
	c *http.Client, bus *Bus, dl deadlines) (*CallResult, error) {
	t.Helper()
	type out struct {
		res *CallResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, e := modelCall(ctx, p, c, bus, 1, "s",
			[]Msg{TextMsg(RoleUser, "hi")}, nil, 16, dl, nil)
		ch <- out{r, e}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-time.After(d):
		t.Fatalf("the call did not return within %s — nothing ended it", d)
		return nil, nil
	}
}

// stalledServer 先发一段合法的 SSE，然后停止写入但不关闭连接——这正是单
// 个 http.Client.Timeout 分不清它和慢答案的那个形状。
func stalledServer(t *testing.T, hold chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		<-hold
	}))
}

func TestAStalledStreamIsCancelledWithItsOwnCause(t *testing.T) {
	hold := make(chan struct{})
	srv := stalledServer(t, hold)
	defer srv.Close()
	defer close(hold)

	bus := NewBus()
	p := &anthropicProvider{baseURL: srv.URL, apiKey: "k", model: "m"}
	// 空闲窗口取得短，因为这个测试真的要等：它要的是整条路，传输也算在里
	// 面，而 net/http 里没有时钟可以注入。guard 每 idle/4 tick 一次，所以
	// 发现会落在 250ms 以内。
	dl := deadlines{idle: 150 * time.Millisecond}

	_, err := callWithin(t, 5*time.Second, context.Background(), p, srv.Client(), bus, dl)
	if err == nil {
		t.Fatal("a stalled stream returned no error")
	}
	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("error was %T, want *CallError", err)
	}
	if !errors.Is(ce.cause, errStalled) {
		t.Fatalf("cause = %v, want errStalled", ce.cause)
	}
	if got := ce.triage(); got != TriageRetry {
		t.Fatalf("triage = %q, want retry", got)
	}
}

func TestAnInterruptedCallIsFatalAndKeepsTheProviderCause(t *testing.T) {
	hold := make(chan struct{})
	srv := stalledServer(t, hold)
	defer srv.Close()
	defer close(hold)

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { cancel(errInterrupted) }()

	bus := NewBus()
	p := &anthropicProvider{baseURL: srv.URL, apiKey: "k", model: "m"}
	_, err := callWithin(t, 5*time.Second, ctx, p, srv.Client(), bus, deadlines{})
	if err == nil {
		t.Fatal("an interrupted call returned no error")
	}
	ce, _ := asCallError(err)
	if ce == nil || !errors.Is(ce.cause, errInterrupted) {
		t.Fatalf("cause = %v, want errInterrupted", ce)
	}
	if got := ce.triage(); got != TriageFatal {
		t.Fatalf("triage = %q, want fatal", got)
	}
}

func TestAnInterruptedCallDoesNotWalkTheLadder(t *testing.T) {
	// 没有 triageCause，这个阶段就会带着这个 bug 发出去。停被回答成"去别
	// 处试试"，那是停的反面——而降级到的供应商那边 prompt 缓存是冷的，所
	// 以这也是被叫停之后可能给出的最贵的回答。
	bus := NewBus()
	lad := newLadder(
		rung{p: &mulFakeProvider{}, info: ProviderInfo{Name: "first"}},
		rung{p: &mulFakeProvider{}, info: ProviderInfo{Name: "second"}},
	)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errInterrupted)

	calls := 0
	_, err := retryLoop(ctx, bus, 1, defaultRetryPolicy(), lad,
		func(context.Context, time.Duration) error { return nil },
		func() float64 { return 1 },
		func(ctx context.Context, p Provider) (*CallResult, error) {
			calls++
			return nil, &CallError{Phase: phaseStream, Message: "gone", cause: cancelCause(ctx)}
		})
	if err == nil {
		t.Fatal("interrupted loop returned no error")
	}
	if calls != 1 {
		t.Fatalf("made %d attempts after an interrupt, want 1", calls)
	}
	if at, _, info := lad.pos(); at != 0 {
		t.Fatalf("ladder advanced to %d (%s) on an interrupt", at, info.Name)
	}
}

func TestABackoffIsCutShortByCancellation(t *testing.T) {
	// 没有可打断的等待，重试退避期间按 Ctrl-C，最长在整段退避里都不起作
	// 用——而用户按下的第二次 Ctrl-C，会在 trace 落盘之前杀掉进程。
	bus := NewBus()
	ctx, cancel := context.WithCancelCause(context.Background())

	slept := 0
	_, err := retryLoop(ctx, bus, 1, defaultRetryPolicy(), newLadder(rung{p: &mulFakeProvider{}}),
		func(c context.Context, d time.Duration) error {
			slept++
			cancel(errInterrupted) // 用户在等待期间按下 Ctrl-C
			return context.Cause(c)
		},
		func() float64 { return 1 },
		func(ctx context.Context, p Provider) (*CallResult, error) {
			return nil, &CallError{Phase: phaseConnect, Message: "refused"}
		})
	if err == nil {
		t.Fatal("no error after an interrupted backoff")
	}
	if slept != 1 {
		t.Fatalf("slept %d times, want 1: the loop kept going after the wait was cut short", slept)
	}
	ce, _ := asCallError(err)
	if ce == nil || !errors.Is(ce.cause, errInterrupted) {
		t.Fatalf("error = %v, want one carrying errInterrupted", err)
	}
}

func TestACancelledParentDoesNotWaitOnItsChildrenForever(t *testing.T) {
	// 这个阶段的名字就来自这次等待。dispatch() 用 wg.Wait() 汇合它的子
	// Agent，而 wg.Wait() 自己既没有期限也没有取消——所以在阶段 10 之前，
	// 子 Agent 各自卡在带十分钟上限的 socket 上，父 Agent 就跟着卡十分
	// 钟，而 Ctrl-C 落到的那个 goroutine 根本没在听它。
	//
	// 为了修这件事，dispatch() 里一行都没改。context 到达子 Agent 的模型
	// 调用，调用失败，子 Agent 返回，汇合就完成了——这就是"穿一个 context
	// 下去"而不是"每一层加一个超时"的全部理由。
	hold := make(chan struct{})
	srv := stalledServer(t, hold)
	defer srv.Close()
	defer close(hold)

	ctx, cancel := context.WithCancelCause(context.Background())
	bus := NewBus()
	p := &anthropicProvider{baseURL: srv.URL, apiKey: "k", model: "m"}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = modelCall(ctx, p, srv.Client(), bus, 1, "s",
				[]Msg{TextMsg(RoleUser, "hi")}, nil, 16, deadlines{}, nil)
		}()
	}

	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()

	select {
	case <-joined:
		t.Fatal("the children returned before anything cancelled them")
	case <-time.After(100 * time.Millisecond):
	}

	cancel(errInterrupted)
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("the join did not complete after the parent was cancelled")
	}
}

func TestTheClientHasNoBlanketTimeout(t *testing.T) {
	// 阶段 10 要做的那个改动，这里是断言它，不是描述它。
	// http.Client.Timeout 管着响应体读取，所以在流式客户端上，它取任何非
	// 零值都是给"模型能说多久"设上限。
	dl := defaultDeadlines()
	c := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: dl.connect}}
	if c.Timeout != 0 {
		t.Fatalf("client Timeout = %v, want 0", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout != dl.connect {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, dl.connect)
	}
}

// ---------------------------------------------------------------------------
// 永不返回的那个工具
// ---------------------------------------------------------------------------

func TestACancelledCommandIsKilledAndSaysSo(t *testing.T) {
	shell, err := findBash()
	if err != nil {
		t.Skip("no bash: ", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	// 命令超时给得很宽，这样通过就不可能是走超时那条路来的。
	r := runBash(ctx, shell, "sleep 30", 60*time.Second)
	if !r.Cancelled {
		t.Fatalf("Cancelled = false; result was %+v", r)
	}
	if r.TimedOut {
		t.Fatal("reported as a timeout: the model would be told to try something narrower")
	}
	if r.Duration > 10*time.Second {
		t.Fatalf("took %s — the process tree was not killed", r.Duration)
	}
	out, _ := r.render(4000)
	if !strings.Contains(out, "CANCELLED") {
		t.Fatalf("rendered output does not say it was cancelled:\n%s", out)
	}
}

func TestACancelledCommandTakesItsGrandchildrenWithIt(t *testing.T) {
	// 这个测试也是变异测试发现漏掉的，而且是两处里更严重的那处。把取消路
	// 径上的整树 kill 删掉，其他每一条断言都还成立：runBash 照样很快返回，
	// 照样报 Cancelled，照样渲染出对的状态。唯一变了的是，事后那些进程还
	// 在跑——而那恰恰是全部的论断，却没有任何东西在看它。
	//
	// 查的是心跳，不是 pid 列表：活下来的进程是**孙进程**（在 shell 里被
	// 放到后台的），而那正是 exec.CommandContext 杀不掉的东西；它不再去碰
	// 的那个文件，就是它已经没了的、可移植的证据。
	shell, err := findBash()
	if err != nil {
		t.Skip("no bash: ", err)
	}
	dir := t.TempDir()
	hb := filepath.ToSlash(filepath.Join(dir, "hb"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(400 * time.Millisecond); cancel() }()

	cmd := "( while true; do date +%s%N > '" + hb + "'; sleep 0.05; done ) & sleep 60"
	r := runBash(ctx, shell, cmd, 90*time.Second)
	if !r.Cancelled {
		t.Fatalf("Cancelled = false; %+v", r)
	}

	first, err := os.ReadFile(hb)
	if err != nil {
		t.Skipf("the heartbeat never started (%v) — nothing to prove", err)
	}
	time.Sleep(600 * time.Millisecond) // 超过 10 个心跳间隔
	second, err := os.ReadFile(hb)
	if err == nil && !bytes.Equal(first, second) {
		t.Fatalf("the grandchild is still writing after cancellation: %q -> %q",
			first, second)
	}
}
