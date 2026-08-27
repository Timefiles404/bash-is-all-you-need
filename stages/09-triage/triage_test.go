package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// roundTripEvent 拿真正的 TraceWriter 写一个事件，再用真正的 ReadTrace 读回
// 来，而不是走 json.Marshal/Unmarshal。要点是把会话真正落盘的那条路走一遍：
// 某个字段直接 marshal 活得下来、过 writer 却活不下来，那它会通过一个更弱的
// 测试，然后把重放搞坏。
func roundTripEvent(t *testing.T, in Event) Event {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := NewTraceWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	NewBus(tw).Emit(in)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("read %d events back, want 1", len(evs))
	}
	return evs[0]
}

func traceLineFor(t *testing.T, in Event) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 这个文件测阶段 09：分类器、退避、信封解析器、重试循环和梯子。
//
// 其中几乎没有一项需要网络，而被测的既是代码，也是这个设计本身。决策都活在
// 以 *CallError 为输入的纯函数里，所以那些有意思的情况——意思是"模型名不对"
// 的 401、意思是"我们的 bug"的 500、故障中途耗尽的预算——都是表里的一行，不
// 是集成用的 fixture。真的架起 httptest 服务器的那三个测试，考的是*传输*本身：
// 真的 Retry-After 头、真的被截断的响应体、真的以 text/plain 送来的错误信封。
//
// 下面每一行都能追到 docs/wire-notes.md 的 §D11/§A3c 或 RFC 9110。不是实测行
// 为的行，都会自己说明。

// ---------------------------------------------------------------------------
// 分类器
// ---------------------------------------------------------------------------

func TestTriageClassifiesTheObservedFailures(t *testing.T) {
	cases := []struct {
		name string
		err  CallError
		want Triage
		why  string
	}{
		// 整个阶段就是为了这两行而存在的。同样的状态码，同样的信封
		// 形状，相反的决策——§D11。
		{"401 revoked key", CallError{Phase: phaseStatus, Status: 401, Type: "AuthError"}, TriageFatal,
			"a bad key cannot be fixed by waiting or by asking someone else"},
		{"401 wrong model name", CallError{Phase: phaseStatus, Status: 401, Type: "ModelError"}, TriageFallback,
			"observed: a nonexistent model id returns 401 here, not 404"},

		// 一个没有信封可读的 401。判致命是安全的读法：没有 error.type，
		// 就没有任何东西说得出"模型"；而把读不懂的认证失败当成降级，会在
		// 一台根本没有密钥的机器上把整个梯子走完。
		{"401 no envelope", CallError{Phase: phaseStatus, Status: 401}, TriageFatal, ""},
		{"403", CallError{Phase: phaseStatus, Status: 403, Type: "AuthError"}, TriageFatal, ""},
		{"404", CallError{Phase: phaseStatus, Status: 404}, TriageFallback,
			"the route or model is not on this endpoint; another may have it"},

		{"429", CallError{Phase: phaseStatus, Status: 429}, TriageRetry, "not observed on this gateway; RFC behaviour"},
		{"408", CallError{Phase: phaseStatus, Status: 408}, TriageRetry, ""},
		{"409", CallError{Phase: phaseStatus, Status: 409}, TriageRetry, ""},

		{"400", CallError{Phase: phaseStatus, Status: 400}, TriageFatal, "ours; retrying it is how a client bug becomes an outage"},
		{"422", CallError{Phase: phaseStatus, Status: 422}, TriageFatal, ""},
		{"413", CallError{Phase: phaseStatus, Status: 413}, TriageFatal, "the bytes are the problem; only compaction changes them"},

		// 第二个陷阱。可重试，但看下面那个牵绳测试。
		{"500", CallError{Phase: phaseStatus, Status: 500, Type: "error", Message: "Internal server error"}, TriageRetry, ""},
		{"502", CallError{Phase: phaseStatus, Status: 502}, TriageRetry, ""},
		{"503", CallError{Phase: phaseStatus, Status: 503}, TriageRetry, ""},
		{"504", CallError{Phase: phaseStatus, Status: 504}, TriageRetry, ""},

		// 凡是没分类的都判致命，这是故意的：没分类的失败拿去重试，只是把
		// 失败重复一遍，而发出来的事件正是漏掉的情况被发现的途径。
		{"418", CallError{Phase: phaseStatus, Status: 418}, TriageFatal, ""},
		{"302", CallError{Phase: phaseStatus, Status: 302}, TriageFatal, ""},

		{"build", CallError{Phase: phaseBuild}, TriageFatal, "our own bug; it will not render on the second try either"},
		{"connect", CallError{Phase: phaseConnect}, TriageRetry, "nothing generated, nothing billed: the only free retry"},

		// §A3c 的邻居：流内的 error 事件。由 type 决定，而没有 type
		// 就意味着传输死了。
		{"stream broke", CallError{Phase: phaseStream}, TriageRetry, ""},
		{"stream overloaded_error", CallError{Phase: phaseStream, Type: "overloaded_error"}, TriageRetry, ""},
		{"stream api_error", CallError{Phase: phaseStream, Type: "api_error"}, TriageRetry, ""},
		{"stream rate_limit_error", CallError{Phase: phaseStream, Type: "rate_limit_error"}, TriageRetry, ""},
		{"stream invalid_request_error", CallError{Phase: phaseStream, Type: "invalid_request_error"}, TriageFatal,
			"arrived because of what we sent; sending it again produces it again"},
		{"stream authentication_error", CallError{Phase: phaseStream, Type: "authentication_error"}, TriageFatal, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.err.triage(); got != c.want {
				t.Fatalf("triage(%+v) = %q, want %q%s", c.err, got, c.want, hint(c.why))
			}
		})
	}
}

func hint(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}

// PascalCase 那个发现，测的是性质，不是一行。
//
// §D11 实测到的是 `ModelError`，而两份协议规范里写的都是 snake_case
// （`not_found_error`、`invalid_request_error`）。拿任一种拼法做相等判断，对
// 着文档是对的，对着线上是错的，所以分类器按子串匹配——而要是有人把它"整理"
// 成对精确值的 switch，这个测试就会失败。
func TestTriageMatchesModelErrorWhateverItsCasing(t *testing.T) {
	for _, typ := range []string{"ModelError", "model_error", "MODEL_NOT_FOUND", "not_found_model_error"} {
		if got := triageStatus(401, typ); got != TriageFallback {
			t.Errorf("triageStatus(401, %q) = %q, want fallback", typ, got)
		}
	}
	// 再来反面：认证失败绝不能被一个不相干的词拽进模型那条分支里。
	for _, typ := range []string{"AuthError", "authentication_error", "permission_error"} {
		if got := triageStatus(401, typ); got != TriageFatal {
			t.Errorf("triageStatus(401, %q) = %q, want fatal", typ, got)
		}
	}
}

func TestLeashIsShortForABare5xxAndFullForA503(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{500, 2}, // 是我们配错的可能性不比对方故障低（§D11）
		{502, 2},
		{504, 2},
		{503, 0}, // 真的容量信号：给全额
		{429, 0},
	}
	for _, c := range cases {
		e := CallError{Phase: phaseStatus, Status: c.status}
		if got := e.leash(); got != c.want {
			t.Errorf("leash(%d) = %d, want %d", c.status, got, c.want)
		}
	}
	// 流断掉不是状态码，所以按状态码成形的规则不能伸到它身上：正是这
	// 一行拦住了把 leash() 写成不查环节的 `if Status >= 500`。
	if got := (&CallError{Phase: phaseStream}).leash(); got != 0 {
		t.Errorf("leash(stream) = %d, want 0", got)
	}
	// 同一条规则，放在唯一能溜过上面那一行的组合下。这个阶段里没有任
	// 何地方会给流错误设状态码，所以不查环节的 `if Status >= 500` 看
	// 着无害——直到阶段 10的看门狗开始把它拿到 200 的那个状态码一并
	// 带上，于是流中途断掉就悄悄继承了本来只给服务器错误的那根短牵绳。
	if got := (&CallError{Phase: phaseStream, Status: 500}).leash(); got != 0 {
		t.Errorf("leash(stream carrying a 500) = %d, want 0 — the leash rule is about statuses, not streams", got)
	}
}

// 不管会话的策略是什么，上下文压缩拿到的牵绳都比它更短。每次尝试都按全价把
// 整份对话记录重发一遍，而且是在那个需要腾地方的回合还等着的时候干的。
func TestForCompactionShortensTheLeash(t *testing.T) {
	got := retryPolicy{attempts: 9, base: time.Second, max: time.Minute, budget: time.Hour}.forCompaction()
	if got.attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.attempts)
	}
	if got.budget != 5*time.Second {
		t.Errorf("budget = %v, want 5s", got.budget)
	}
	// 已经比这个上限更严的策略不去动它：上限是天花板，不是设置项。
	// `--retry 1` 的意思是处处只试一次，这里也一样。
	tight := retryPolicy{attempts: 1, base: time.Second, max: time.Second, budget: time.Second}
	if got := tight.forCompaction(); got.attempts != 1 || got.budget != time.Second {
		t.Errorf("forCompaction raised a tighter policy: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 错误信封
// ---------------------------------------------------------------------------

func TestParseErrorBodyHandlesEveryObservedShape(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantT, wantM  string
		fromWireNotes bool
	}{
		{"anthropic envelope, both protocols (D11)",
			`{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}`,
			"AuthError", "Invalid API key.", true},
		{"model error (D11)",
			`{"type":"error","error":{"type":"ModelError","message":"Model gpt-does-not-exist-9000 is not supported"}}`,
			"ModelError", "Model gpt-does-not-exist-9000 is not supported", true},
		{"500 lowercase type (D11)",
			`{"type":"error","error":{"type":"error","message":"Internal server error"}}`,
			"error", "Internal server error", true},

		// 这一行说明了为什么要在 CallError 上留着原始响应体。一个根
		// 本没有信封的 400：请求的 24 字节回显。
		{"400 with no envelope (D11)", `{"model":"qwen3.7-plus"}`, "", "", true},

		// 带 `code` 字段的 OpenAI 形状响应体，这个网关不发，但别的端
		// 点会发。`code` 的类型是 `any`，因为它在真实世界里既会以字
		// 符串的形式来，也会是 null；那儿要是写成 `string` 字段，整
		// 个信封就会 unmarshal 失败，连 message 也一起丢掉。
		{"openai shape with a code", `{"error":{"message":"nope","type":"invalid_request_error","code":"model_not_found"}}`,
			"invalid_request_error", "nope", false},
		{"openai shape with a null code", `{"error":{"message":"nope","type":"invalid_request_error","code":null}}`,
			"invalid_request_error", "nope", false},
		{"openai shape with a numeric code", `{"error":{"message":"nope","type":"invalid_request_error","code":404}}`,
			"invalid_request_error", "nope", false},

		{"not json at all", `502 Bad Gateway`, "", "", false},
		{"empty", ``, "", "", false},
		{"json but not an object", `[1,2,3]`, "", "", false},
		{"error is a string, not an object", `{"error":"boom"}`, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gt, gm := parseErrorBody([]byte(c.body))
			if gt != c.wantT || gm != c.wantM {
				t.Fatalf("parseErrorBody(%s) = (%q, %q), want (%q, %q)", c.body, gt, gm, c.wantT, c.wantM)
			}
		})
	}
}

// 没有信封这种情况，要一路活到人读的那条消息里，不是只活到解析器。
// "http 400: " 后面空着，读起来像 Agent 自己的 bug；把这份缺失说出来才是指
// 向服务器，而响应体就摆在那行里。
func TestErrorNamesAMissingEnvelopeAndKeepsTheBody(t *testing.T) {
	e := &CallError{Phase: phaseStatus, Status: 400, Body: `{"model":"qwen3.7-plus"}`}
	got := e.Error()
	for _, want := range []string{"400", "no error envelope", `{"model":"qwen3.7-plus"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, missing %q", got, want)
		}
	}
}

func TestUnwrapKeepsErrorsIsWorking(t *testing.T) {
	e := &CallError{Phase: phaseConnect, Err: io.ErrUnexpectedEOF, Message: "boom"}
	if !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatal("errors.Is could not see through CallError — Unwrap is missing or wrong")
	}
	ce, ok := asCallError(fmt.Errorf("wrapped: %w", e))
	if !ok || ce.Phase != phaseConnect {
		t.Fatalf("asCallError could not find a CallError through fmt.Errorf: %v %v", ce, ok)
	}
}

// ---------------------------------------------------------------------------
// Retry-After
// ---------------------------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	hdr := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return h
	}
	cases := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"absent", "", 0},
		{"delta seconds", "7", 7 * time.Second},
		{"delta seconds with spaces", "  7  ", 7 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"http date in the future", "Thu, 27 Aug 2026 12:00:30 GMT", 30 * time.Second},
		// 已经过去的日期意思是"现在"，不是睡一个负数。这里返回一个带符
		// 号的时长，会一路流进 time.Sleep 然后立刻返回，等于正好在服务
		// 器要求退避的时候把退避关掉。
		{"http date in the past", "Thu, 27 Aug 2026 11:59:30 GMT", 0},
		// 解析不了就忽略，不去猜：算出来的退避是个已知安全的数，编出来的不是。
		{"garbage", "soon please", 0},
		{"float", "7.5", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRetryAfter(hdr(c.val), now); got != c.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", c.val, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 退避
// ---------------------------------------------------------------------------

func TestWaitGrowsExponentiallyAndIsCapped(t *testing.T) {
	p := retryPolicy{base: 500 * time.Millisecond, max: 2 * time.Second}
	full := func() float64 { return 1 } // 抖动区间的顶端

	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	for i, w := range want {
		if got := p.wait(i+1, 0, full); got != w {
			t.Errorf("wait(%d) = %v, want %v", i+1, got, w)
		}
	}

	// 长时间跑的会话里，不能让移位溢出成负的或零的时长。没有 `exp <= 0`
	// 那道守卫，wait(64) 会返回 0，循环就彻底不等了。
	if got := p.wait(64, 0, full); got != p.max {
		t.Errorf("wait(64) = %v, want the cap %v — the shift overflowed", got, p.max)
	}
}

// 全抖动，不是半抖动。要保的性质是：取值从零起覆盖整个区间。好几个子 Agent
// 共用一个客户端、一个端点时，最小等待是 exp/2 的策略会把自己的客户端一直保
// 持同步，每次尝试都重新撞在一起。
func TestWaitUsesFullJitter(t *testing.T) {
	p := retryPolicy{base: time.Second, max: time.Minute}
	if got := p.wait(3, 0, func() float64 { return 0 }); got != 0 {
		t.Errorf("wait with rnd=0 = %v, want 0 — the jitter does not reach the bottom of the interval", got)
	}
	if got := p.wait(3, 0, func() float64 { return 0.5 }); got != 2*time.Second {
		t.Errorf("wait with rnd=0.5 = %v, want 2s (half of the 4s interval)", got)
	}
}

func TestRetryAfterBeatsTheComputedBackoffButNotTheClamp(t *testing.T) {
	p := retryPolicy{base: 500 * time.Millisecond, max: 2 * time.Second}
	full := func() float64 { return 1 }

	// 服务器知道自己的容量什么时候回来，我们不知道，所以它的数字赢——
	// 就算它比我们的退避还短。
	if got := p.wait(5, 250*time.Millisecond, full); got != 250*time.Millisecond {
		t.Errorf("wait with Retry-After 250ms = %v, want 250ms", got)
	}
	// 但服务器也有权说"一个小时"，而照办的 Agent 看起来就是卡死了。请
	// 求的形式照办，长度不照办。
	if got := p.wait(1, time.Hour, full); got != p.max*8 {
		t.Errorf("wait with Retry-After 1h = %v, want the clamp %v", got, p.max*8)
	}
}

// ---------------------------------------------------------------------------
// 循环
// ---------------------------------------------------------------------------

// loopFixture 用一串排好的结果去驱动 retryLoop，不带时钟：sleep 只被记下
// 来，不真去睡，所以测一个 30 秒的预算，跑起来只要几微秒。
type loopFixture struct {
	t       *testing.T
	rec     *mulRecorder
	bus     *Bus
	slept   []time.Duration
	seen    []Provider // 每次尝试实际是对着哪一级跑的
	scripts []error    // nil 表示成功
	n       int
}

func newLoopFixture(t *testing.T, script ...error) *loopFixture {
	rec := &mulRecorder{}
	return &loopFixture{t: t, rec: rec, bus: NewBus(rec), scripts: script}
}

func (f *loopFixture) run(pol retryPolicy, lad *ladder) (*CallResult, error) {
	return retryLoop(f.bus, 1, pol, lad,
		func(d time.Duration) { f.slept = append(f.slept, d) },
		func() float64 { return 1 }, // 抖动区间的顶端：确定的
		func(p Provider) (*CallResult, error) {
			f.seen = append(f.seen, p)
			i := f.n
			f.n++
			if i >= len(f.scripts) || f.scripts[i] == nil {
				return &CallResult{Text: "ok", Usage: Usage{Input: 100, Output: 5}}, nil
			}
			return nil, f.scripts[i]
		})
}

// oneRung 是只有一家供应商的梯子，也就是没给 --fallback 的会话手里那种。
func oneRung(name string) *ladder {
	return newLadder(rung{p: nil, info: ProviderInfo{Name: name}})
}

func TestRetryLoopRetriesUntilItWorks(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 503}
	f := newLoopFixture(t, boom, boom, nil)
	pol := retryPolicy{attempts: 3, base: time.Second, max: time.Minute, budget: time.Minute}

	res, err := f.run(pol, oneRung("primary"))
	if err != nil {
		t.Fatalf("want success on the third attempt, got %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("res.Text = %q", res.Text)
	}
	if len(f.slept) != 2 {
		t.Fatalf("slept %v, want two waits", f.slept)
	}
	// 指数式的，而且就是这个顺序。每次尝试都把指数重置的策略，会睡两次
	// 1s，然后通过一个更弱的断言。
	if f.slept[0] != time.Second || f.slept[1] != 2*time.Second {
		t.Errorf("waits = %v, want [1s 2s]", f.slept)
	}
	if got := f.rec.count(KindCallError); got != 2 {
		t.Errorf("call_error events = %d, want 2", got)
	}
	if got := f.rec.count(KindRetry); got != 2 {
		t.Errorf("retry events = %d, want 2", got)
	}
	// 裁决是落在事件上的，不只在日志行里：阶段 18的指标会从 trace 里
	// 把它读出来。
	for _, e := range f.rec.kind(KindCallError) {
		if e.Triage != string(TriageRetry) {
			t.Errorf("call_error carried triage %q, want retry", e.Triage)
		}
		if e.Status != 503 {
			t.Errorf("call_error carried status %d, want 503", e.Status)
		}
		// 环节必须能活着从 CallError 走到事件上。没有它，面板就分不清
		// 被拒的请求（免费）和断掉的流（计费），重复计费那个数字就是
		// 编的——这正是这个阶段第一次实跑时出的那个 bug。
		if e.Phase != string(phaseStatus) {
			t.Errorf("call_error carried phase %q, want status", e.Phase)
		}
	}
	// 尝试的编号得能用：重试事件公布的是它马上要做的那次尝试，
	// 不是刚失败的那次。
	if a := f.rec.kind(KindRetry)[0].Attempt; a != 2 {
		t.Errorf("first retry announced attempt %d, want 2", a)
	}
}

func TestRetryLoopStopsImmediatelyOnAFatalVerdict(t *testing.T) {
	f := newLoopFixture(t, &CallError{Phase: phaseStatus, Status: 400, Type: "invalid_request_error"}, nil)
	pol := retryPolicy{attempts: 5, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, oneRung("primary")); err == nil {
		t.Fatal("a 400 must not be retried into a success")
	}
	if f.n != 1 {
		t.Fatalf("made %d attempts, want exactly 1", f.n)
	}
	if len(f.slept) != 0 {
		t.Fatalf("slept %v on a fatal verdict", f.slept)
	}
	if got := f.rec.count(KindRetry); got != 0 {
		t.Errorf("emitted %d retry events on a fatal verdict", got)
	}
}

// 牵绳，端到端。策略允许五次尝试，实际只用了两次，因为在这个网关上，光秃秃
// 的 500 同样可能是客户端的 bug（§D11）。
func TestRetryLoopHonoursTheShortLeashOnABare500(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 500, Type: "error", Message: "Internal server error"}
	f := newLoopFixture(t, boom, boom, boom, boom, boom, nil)
	pol := retryPolicy{attempts: 5, base: time.Millisecond, max: time.Second, budget: time.Minute}

	_, err := f.run(pol, oneRung("primary"))
	if err == nil {
		t.Fatal("want failure after the leash runs out")
	}
	if f.n != 2 {
		t.Fatalf("made %d attempts, want 2 (the leash), policy allowed %d", f.n, pol.attempts)
	}
	if !strings.Contains(err.Error(), "2 attempts") {
		t.Errorf("error = %q, want it to name the attempt count", err)
	}
	// 再看对照，同一套策略下：503 拿全额。
	boom503 := &CallError{Phase: phaseStatus, Status: 503}
	g := newLoopFixture(t, boom503, boom503, boom503, boom503, boom503, nil)
	if _, err := g.run(pol, oneRung("primary")); err == nil {
		t.Fatal("want failure after five attempts")
	}
	if g.n != 5 {
		t.Fatalf("503 made %d attempts, want the policy's 5", g.n)
	}
}

func TestRetryLoopStopsWhenTheBudgetRunsOut(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 503}
	f := newLoopFixture(t, boom, boom, boom, boom, boom, boom, nil)
	// 允许十次尝试，但只允许等 15s：头两次等待是 10s 和 20s，所以拦住它
	// 的是预算，不是尝试次数。
	pol := retryPolicy{attempts: 10, base: 10 * time.Second, max: time.Minute, budget: 15 * time.Second}

	_, err := f.run(pol, oneRung("primary"))
	if err == nil {
		t.Fatal("want failure when the budget is exhausted")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error = %q, want it to name the budget — it is the number a user will want to change", err)
	}
	if f.n != 2 {
		t.Fatalf("made %d attempts, want 2 before the budget stopped it", f.n)
	}
	total := time.Duration(0)
	for _, d := range f.slept {
		total += d
	}
	if total > pol.budget {
		t.Errorf("slept %v in total, over the %v budget", total, pol.budget)
	}
}

func TestRetryLoopDoesNotRetryAnUnclassifiedError(t *testing.T) {
	// 一个普通的 error，不是 *CallError：调用路径上有东西以这个阶段没
	// 有建模的方式失败了。返回而不是重试，因为没分类的失败拿去重试，只
	// 是把失败重复一遍。
	f := newLoopFixture(t, errors.New("something else entirely"), nil)
	pol := retryPolicy{attempts: 5, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, oneRung("primary")); err == nil {
		t.Fatal("want the unclassified error returned")
	}
	if f.n != 1 {
		t.Fatalf("made %d attempts on an unclassified error, want 1", f.n)
	}
	if got := f.rec.count(KindCallError); got != 0 {
		t.Errorf("emitted %d call_error events for an error it could not classify", got)
	}
}

func TestRetryLoopHonoursRetryAfterFromTheServer(t *testing.T) {
	boom := &CallError{Phase: phaseStatus, Status: 429, RetryAfter: 3 * time.Second}
	f := newLoopFixture(t, boom, nil)
	pol := retryPolicy{attempts: 3, base: 50 * time.Millisecond, max: time.Minute, budget: time.Minute}

	if _, err := f.run(pol, oneRung("primary")); err != nil {
		t.Fatalf("want success on the second attempt: %v", err)
	}
	if len(f.slept) != 1 || f.slept[0] != 3*time.Second {
		t.Fatalf("slept %v, want [3s] from the server's header rather than the 50ms backoff", f.slept)
	}
	// 而原因就写在那行里，因为"等 3s"和"等 3s，因为服务器要求等 3s"会
	// 导向不同的排查。
	if txt := f.rec.kind(KindRetry)[0].Text; !strings.Contains(txt, "the server asked for") {
		t.Errorf("retry text = %q, want it to attribute the delay", txt)
	}
}

// ---------------------------------------------------------------------------
// 梯子
// ---------------------------------------------------------------------------

func twoRungs() *ladder {
	return newLadder(
		rung{info: ProviderInfo{Name: "primary", Prices: priceConfig{In: 1, Out: 4}}},
		rung{info: ProviderInfo{Name: "backup", Prices: priceConfig{In: 10, Out: 40}}},
	)
}

func TestFallbackMovesToTheNextRungAndSaysSo(t *testing.T) {
	// §D11 那个情况：模型名写错是以 401 的形式来的，而正确的动作是换个
	// 端点，不是放弃。
	f := newLoopFixture(t, &CallError{Phase: phaseStatus, Status: 401, Type: "ModelError"}, nil)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}
	lad := twoRungs()

	if _, err := f.run(pol, lad); err != nil {
		t.Fatalf("want success on the backup rung: %v", err)
	}
	if len(f.slept) != 0 {
		t.Errorf("slept %v before falling back — a fallback is not a wait", f.slept)
	}
	evs := f.rec.kind(KindProvider)
	if len(evs) != 1 {
		t.Fatalf("provider events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e.Provider == nil || e.Provider.Name != "backup" {
		t.Fatalf("provider event named %v, want backup", e.Provider)
	}
	if e.Triage != string(TriageFallback) {
		t.Errorf("provider event triage = %q, want fallback — a session-start event carries none", e.Triage)
	}
	// 价格是跟着一起走的。没有它们，面板就会一直拿主供应商的费率去算备
	// 用那家的 token。
	if e.Provider.Prices.In != 10 {
		t.Errorf("provider event carried prices %+v, want the backup's", e.Provider.Prices)
	}
	if !strings.Contains(e.Text, "ModelError") {
		t.Errorf("provider event text = %q, want the reason in it", e.Text)
	}
}

func TestFallbackHappensAfterTheRetriesRunOutToo(t *testing.T) {
	// 可重试的失败一直失败，放弃之前值得往梯子上看一眼："供应商挂了"和
	// "这家供应商挂了"是两句不同的话。
	boom := &CallError{Phase: phaseConnect, Message: "connection refused"}
	f := newLoopFixture(t, boom, boom, boom, nil)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, twoRungs()); err != nil {
		t.Fatalf("want success on the backup rung after the retries: %v", err)
	}
	if f.n != 4 {
		t.Fatalf("made %d attempts, want 3 on the primary and 1 on the backup", f.n)
	}
	if got := f.rec.count(KindProvider); got != 1 {
		t.Errorf("provider events = %d, want 1", got)
	}
}

// 刚刚降级到的那一级，拿到的是属于它自己的全额尝试次数。把上一级的计数带过
// 来，就意味着一个健康的备用供应商失败一次就被丢掉——只因为死掉的主供应商已
// 经把预算用光了。
func TestFallbackGivesTheNewRungItsOwnAttempts(t *testing.T) {
	modelErr := &CallError{Phase: phaseStatus, Status: 401, Type: "ModelError"}
	busy := &CallError{Phase: phaseStatus, Status: 503}
	f := newLoopFixture(t, modelErr, busy, busy, nil)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}

	if _, err := f.run(pol, twoRungs()); err != nil {
		t.Fatalf("want success: the backup was allowed one fallback and then three attempts of its own: %v", err)
	}
	if f.n != 4 {
		t.Fatalf("made %d attempts, want 4 (1 on the primary, 3 on the backup)", f.n)
	}
}

func TestTheLastRungsErrorIsTheOneReported(t *testing.T) {
	f := newLoopFixture(t,
		&CallError{Phase: phaseStatus, Status: 401, Type: "ModelError", Message: "primary has no such model"},
		&CallError{Phase: phaseStatus, Status: 401, Type: "AuthError", Message: "backup key is revoked"},
	)
	pol := retryPolicy{attempts: 3, base: time.Millisecond, max: time.Second, budget: time.Minute}

	_, err := f.run(pol, twoRungs())
	if err == nil {
		t.Fatal("want failure once the ladder is exhausted")
	}
	// 会话没法继续的原因，是最后拒了它的那个，不是最先的那个。这里报主
	// 供应商的错误，会把人打发去改模型名，而真正的问题是密钥被吊销了。
	if !strings.Contains(err.Error(), "backup key is revoked") {
		t.Errorf("error = %q, want the last rung's failure", err)
	}
}

// 两个子 Agent 在同一瞬间对着同一个挂掉的端点失败，代价必须是一级，不是两
// 级。advance() 要是只做个自增，就会跳过一家谁都没试过的健康供应商。
func TestConcurrentFallbacksBurnOneRung(t *testing.T) {
	lad := newLadder(
		rung{info: ProviderInfo{Name: "a"}},
		rung{info: ProviderInfo{Name: "b"}},
		rung{info: ProviderInfo{Name: "c"}},
	)
	at, _, _ := lad.pos()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !lad.advance(at) {
				t.Error("advance reported nowhere to go with two rungs left")
			}
		}()
	}
	wg.Wait()

	_, _, info := lad.pos()
	if info.Name != "b" {
		t.Fatalf("landed on %q after eight concurrent fallbacks from rung 0, want b", info.Name)
	}
}

// advance() 里那道守卫真正派上用场的地方——而且不是上面那个八 goroutine 测试
// 覆盖的情形：那个测试里每个调用方都在同一级上，对它们来说 `cur = from + 1`
// 本身就是幂等的。
//
// 这个故障需要三个参与者，而且它是一次回退。A 从第 0 级掉下来，落在第 1 级。
// C 从第 1 级掉下来，落在第 2 级。然后 B——手里还攥着这一切发生之前读到的第
// 0 级——来要求降级，而没有守卫，它会写下 cur = 1，把下一次调用发给两个兄弟
// 都已经放弃的供应商。这是文件里唯一会察觉到这件事的测试。
func TestAdvanceNeverMovesTheLadderBackwards(t *testing.T) {
	lad := newLadder(
		rung{info: ProviderInfo{Name: "a"}},
		rung{info: ProviderInfo{Name: "b"}},
		rung{info: ProviderInfo{Name: "c"}},
	)
	if !lad.advance(0) {
		t.Fatal("advance off rung 0 failed")
	}
	if !lad.advance(1) {
		t.Fatal("advance off rung 1 failed")
	}
	if _, _, info := lad.pos(); info.Name != "c" {
		t.Fatalf("after two steps the ladder is on %q, want c", info.Name)
	}

	// 落后者。必须告诉它可以——**确实**还有别处可以发——同时梯子不能丢掉
	// 别人挣来的进展。
	if !lad.advance(0) {
		t.Error("a straggler still on rung 0 was told there is nowhere to go")
	}
	if _, _, info := lad.pos(); info.Name != "c" {
		t.Fatalf("the ladder rewound to %q; a straggler's stale rung must not undo a sibling's fallback", info.Name)
	}
}

func TestAdvanceReportsFalseAtTheEndOfTheLadder(t *testing.T) {
	lad := oneRung("only")
	if lad.advance(0) {
		t.Fatal("advance said yes on a one-rung ladder")
	}
	var nilLadder *ladder
	if nilLadder.advance(0) {
		t.Fatal("advance said yes on a nil ladder")
	}
	if _, p, _ := nilLadder.pos(); p != nil {
		t.Fatal("pos on a nil ladder returned a provider")
	}
}

// ---------------------------------------------------------------------------
// buildLadder
// ---------------------------------------------------------------------------

func TestBuildLadderValidatesEveryRungAtStartup(t *testing.T) {
	t.Setenv("TRIAGE_KEY_A", "sk-a")
	t.Setenv("TRIAGE_KEY_B", "sk-b")
	pf := &providersFile{
		Default: "a",
		Providers: map[string]providerConfig{
			"a": {Protocol: "openai", BaseURL: "https://a.example", APIKeyEnv: "TRIAGE_KEY_A", Model: "m-a", Prices: priceConfig{In: 1}},
			"b": {Protocol: "anthropic", BaseURL: "https://b.example", APIKeyEnv: "TRIAGE_KEY_B", Model: "m-b", Prices: priceConfig{In: 2}},
			"c": {Protocol: "openai", BaseURL: "https://c.example", APIKeyEnv: "TRIAGE_KEY_MISSING", Model: "m-c"},
		},
	}
	primary, err := pf.Providers["a"].build(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("primary alone", func(t *testing.T) {
		lad, err := buildLadder(pf, "a", pf.Providers["a"], primary, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(lad.rungs) != 1 {
			t.Fatalf("rungs = %d, want 1", len(lad.rungs))
		}
		if lad.rungs[0].info.Name != "a" || lad.rungs[0].info.Model != "m-a" {
			t.Fatalf("rung 0 = %+v", lad.rungs[0].info)
		}
	})

	t.Run("with a fallback", func(t *testing.T) {
		lad, err := buildLadder(pf, "a", pf.Providers["a"], primary, " b ", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(lad.rungs) != 2 || lad.rungs[1].info.Name != "b" {
			t.Fatalf("rungs = %+v", lad.rungs)
		}
		// Protocol 是从构建好的供应商上取的，不是从配置字符串上取的，所
		// 以协议和模型对不上的那一级根本不可能存在。
		if lad.rungs[1].info.Protocol != "anthropic" {
			t.Errorf("rung 1 protocol = %q", lad.rungs[1].info.Protocol)
		}
	})

	t.Run("a duplicate is refused", func(t *testing.T) {
		// 同一家供应商在梯子上列两遍，读起来像多了一层韧性，实际一点都没
		// 多：第二级失败的原因跟第一级一样，它买到的只有放弃之前更长的等待。
		if _, err := buildLadder(pf, "a", pf.Providers["a"], primary, "b,b", true); err == nil {
			t.Fatal("want an error for a repeated fallback")
		}
		if _, err := buildLadder(pf, "a", pf.Providers["a"], primary, "a", true); err == nil {
			t.Fatal("want an error when the fallback is the primary")
		}
	})

	t.Run("an unknown name fails at startup", func(t *testing.T) {
		_, err := buildLadder(pf, "a", pf.Providers["a"], primary, "nope", true)
		if err == nil {
			t.Fatal("want an error for an unknown provider name")
		}
	})

	t.Run("a missing key fails at startup, not during the outage", func(t *testing.T) {
		// 这就是每一级都要提前构建的全部理由。按需构建的降级，就是按需
		// 失败的降级，而它被需要的那一刻，正是它唯一存在的意义。
		_, err := buildLadder(pf, "a", pf.Providers["a"], primary, "c", true)
		if err == nil {
			t.Fatal("want an error when a fallback's key env var is empty")
		}
		if !strings.Contains(err.Error(), "c") {
			t.Errorf("error = %q, want it to name the rung", err)
		}
	})

	t.Run("empty entries are skipped", func(t *testing.T) {
		lad, err := buildLadder(pf, "a", pf.Providers["a"], primary, "b,,", true)
		if err != nil || len(lad.rungs) != 2 {
			t.Fatalf("rungs=%v err=%v", lad, err)
		}
	})
}

// ---------------------------------------------------------------------------
// modelCall 走真实传输
// ---------------------------------------------------------------------------

func triageProvider(t *testing.T, srv *httptest.Server) Provider {
	t.Helper()
	return newOpenAIProvider(srv.URL, "sk-test", "test-model")
}

// 在这个网关上，信封是以 text/plain 送来的（§D11），这正是 parseErrorBody 里
// 没有任何地方去看 content type 的原因。照它分支的客户端，会把这个端点产生的
// 每一个错误都报成"解析不了的错误"。
func TestModelCallClassifiesAnErrorServedAsTextPlain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
		w.WriteHeader(401)
		io.WriteString(w, `{"type":"error","error":{"type":"ModelError","message":"Model nope is not supported"}}`)
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	_, err := modelCall(triageProvider(t, srv), srv.Client(), NewBus(rec), 1, "sys",
		[]Msg{TextMsg(RoleUser, "hi")}, nil, 64)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *CallError", err, err)
	}
	if ce.Phase != phaseStatus || ce.Status != 401 || ce.Type != "ModelError" {
		t.Fatalf("CallError = %+v", ce)
	}
	if ce.triage() != TriageFallback {
		t.Errorf("triage = %q, want fallback", ce.triage())
	}
}

func TestModelCallReadsRetryAfterOffTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	_, err := modelCall(triageProvider(t, srv), srv.Client(), NewBus(rec), 1, "sys",
		[]Msg{TextMsg(RoleUser, "hi")}, nil, 64)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v, want a *CallError", err)
	}
	// 这是本仓库读过的第一个响应头。阶段 09之前，就算真来了一个
	// Retry-After，Agent 也没法照办。
	if ce.RetryAfter != 2*time.Second {
		t.Fatalf("RetryAfter = %v, want 2s", ce.RetryAfter)
	}
}

// 这个阶段要来补的那道缝。两个适配器都是故意在返回流错误的同时返回一份部分
// 结果，而在此之前的每个阶段，都只是把那个值接住，然后丢掉。
func TestModelCallKeepsThePartialWhenTheStreamBreaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Content-Length 比响应体长，于是客户端读响应体时会以 unexpected
		// EOF 失败，而不是干净地结束。仅仅停下来的流不算错误——sse.go 会
		// 在 EOF 时把最后一帧冲出去，这是故意的——所以真正断掉的连接只能
		// 这样复现。
		w.Header().Set("Content-Length", "4096")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answ\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	res, err := modelCall(triageProvider(t, srv), srv.Client(), NewBus(rec), 1, "sys",
		[]Msg{TextMsg(RoleUser, "hi")}, nil, 64)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *CallError", err, err)
	}
	if ce.Phase != phaseStream {
		t.Fatalf("Stage = %q, want stream", ce.Phase)
	}
	if ce.Partial == nil {
		t.Fatal("Partial is nil: the accumulated text was dropped, which is the bug stage 09 fixed")
	}
	if !strings.Contains(ce.Partial.Text, "partial answ") {
		t.Errorf("Partial.Text = %q, want the text that did arrive", ce.Partial.Text)
	}
	if res == nil || res.Text != ce.Partial.Text {
		t.Errorf("the returned result and the partial disagree: %v vs %q", res, ce.Partial.Text)
	}
	if ce.triage() != TriageRetry {
		t.Errorf("triage = %q, want retry", ce.triage())
	}
	// 而 trace 说的是这次响应断了，不是结束了：没有 response_end——那个
	// 信号适配器从阶段 02就一直带着，到阶段 09才终于有人读。
	if got := rec.count(KindResponseEnd); got != 0 {
		t.Errorf("emitted %d response_end events for a stream that broke", got)
	}
}

// 流失败的另一种方式，走的是 modelCall 的另一条分支：供应商在响应体中途发来
// 一个 `error` **事件**。anthropic.go 会把它变成 *CallError，带着供应商自己的
// error.type，而 modelCall 必须对它做*增补*而不是包一层——让那个 type 留在分
// 类器能拿到的地方，同时把部分结果挂上去。
//
// 这个网关上没实测到过（§D11 里的错误全是在流打开之前以 HTTP 状态码来的），
// 但它写在规范里，而且供应商在响应中途降级时，`overloaded_error` 走的就是这
// 条路。
func TestModelCallEnrichesAnInStreamErrorWithThePartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"m","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"halfway through a "}}`,
			`{"type":"error","error":{"type":"overloaded_error","message":"capacity"}}`,
		} {
			io.WriteString(w, "data: "+frame+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	p := newAnthropicProvider(srv.URL, "sk-test", "test-model")
	_, err := modelCall(p, srv.Client(), NewBus(rec), 1, "sys", []Msg{TextMsg(RoleUser, "hi")}, nil, 64)

	ce, ok := asCallError(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *CallError", err, err)
	}
	if ce.Type != "overloaded_error" {
		t.Fatalf("Type = %q, want the provider's own error.type — wrapping instead of enriching loses it", ce.Type)
	}
	if ce.triage() != TriageRetry {
		t.Errorf("triage = %q, want retry: overloaded_error is the canonical retryable condition", ce.triage())
	}
	if ce.Partial == nil {
		t.Fatal("Partial is nil on the enrichment branch: the text that did arrive was dropped")
	}
	if !strings.Contains(ce.Partial.Text, "halfway through a ") {
		t.Errorf("Partial.Text = %q, want the text that arrived before the error event", ce.Partial.Text)
	}
	if got := rec.count(KindResponseEnd); got != 0 {
		t.Errorf("emitted %d response_end events for a stream that carried an error", got)
	}
}

// 重试的调用必须把请求重新建一遍。*http.Request 的 body 在第一次 Do 之后就是
// 一个读完了的 reader，所以复用请求对象的循环，第二次尝试会送出零个字节——重
// 试里的 bug，长得跟服务器的 bug 一模一样。
func TestRetriedCallsSendTheWholeBodyEveryTime(t *testing.T) {
	var mu sync.Mutex
	var lengths []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lengths = append(lengths, len(body))
		n := len(lengths)
		mu.Unlock()

		if n < 3 {
			w.WriteHeader(503)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	rec := &mulRecorder{}
	bus := NewBus(rec)
	p := triageProvider(t, srv)
	pol := retryPolicy{attempts: 3, base: time.Nanosecond, max: time.Millisecond, budget: time.Second}

	res, err := retryLoop(bus, 1, pol, newLadder(rung{p: p}),
		func(time.Duration) {}, func() float64 { return 1 },
		func(pr Provider) (*CallResult, error) {
			return modelCall(pr, srv.Client(), bus, 1, "sys", []Msg{TextMsg(RoleUser, "hi")}, nil, 64)
		})
	if err != nil {
		t.Fatalf("want success on the third attempt: %v", err)
	}
	if res.Text != "hi" {
		t.Errorf("res.Text = %q", res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lengths) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(lengths))
	}
	for i, n := range lengths {
		if n != lengths[0] || n == 0 {
			t.Errorf("request %d had a body of %d bytes, want %d — the retry re-sent a consumed reader",
				i+1, n, lengths[0])
		}
	}
}

// ---------------------------------------------------------------------------
// 面板
// ---------------------------------------------------------------------------

// 降级会在会话中途改掉计价基准，面板必须跟上。拿主供应商的费率去算备用那家
// 的 token，出来的报告是自信地错着，而这比一份承认自己不知道的报告更糟。
func TestThePanelRepricesOnFallback(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)

	usage := func(n int) Event {
		return Event{Kind: KindUsage, Usage: &Usage{Input: n, Output: 0}}
	}
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{
		Name: "primary", Window: 1000, Prices: priceConfig{In: 1}}}) // $1 / 1M
	r.OnEvent(usage(1_000_000)) // $1.00
	r.OnEvent(Event{Kind: KindProvider, Triage: string(TriageFallback), Text: "fell back",
		Provider: &ProviderInfo{Name: "backup", Window: 2000, Prices: priceConfig{In: 10}}}) // $10 / 1M
	r.OnEvent(usage(1_000_000)) // $10.00

	if got := r.sessionCost; got != 11 {
		t.Fatalf("sessionCost = %v, want 11 (1 at the primary's rate + 10 at the backup's)", got)
	}
	// 窗口也一起走：上下文水位线是相对某个数的占比，而那个数刚刚变了。
	if r.window != 2000 {
		t.Errorf("window = %d, want the backup's 2000", r.window)
	}
	if !strings.Contains(out.String(), "backup") {
		t.Errorf("the fallback was not announced:\n%s", out.String())
	}
}

// 会话开始时的 provider 事件不能当成降级来报：横幅上已经写了供应商的名字，
// 而每次干净启动都来一行"provider →"，人就是这样学会跳过这一块的。
func TestSessionStartProviderEventIsSilent(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "primary", Prices: priceConfig{In: 3}}})

	if out.String() != "" {
		t.Errorf("session start printed %q, want nothing", out.String())
	}
	if r.prices.in != 3 {
		t.Errorf("prices were not adopted: %+v", r.prices)
	}
}

// 别的 Agent 都不报的那个数：失败的尝试花了多少钱。
func TestTheSummaryReportsWhatRetriesReBilled(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "p", Prices: priceConfig{In: 1_000_000}}})

	// 两个打开了又死掉的流，然后是成功的那次尝试，它终于报出了真实的
	// prompt 数字。估算就在那一刻做，因为那是这份 prompt 唯一存在真实数
	// 字的时刻。
	//
	// 环节在这里要紧，不只是为了整齐：见下面那个配套测试，以及 render.go
	// 里那条注释——它讲的是把这件事搞错了的那次实跑。
	r.OnEvent(Event{Kind: KindCallError, Attempt: 1, Phase: string(phaseStream), Triage: string(TriageRetry), Text: "stream broke"})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 2, Millis: 500})
	r.OnEvent(Event{Kind: KindCallError, Attempt: 2, Phase: string(phaseStream), Triage: string(TriageRetry), Text: "stream broke"})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 3, Millis: 1000})
	r.OnEvent(Event{Kind: KindUsage, Usage: &Usage{Input: 10, CacheRead: 90, Output: 5}})

	if got := r.rebilled.Prompt(); got != 200 {
		t.Fatalf("rebilled = %d, want 200 (two failed attempts at the successful attempt's 100-token prompt)", got)
	}
	// 拆分是保住的，没有塌成全价 input：正因如此，这才是一个站得住的下
	// 界，不是一个吓人的数字。
	if r.rebilled.Input != 20 || r.rebilled.CacheRead != 180 {
		t.Errorf("rebilled split = %+v, want the successful attempt's shape doubled", r.rebilled)
	}
	// 按调用计的那个乘数会归零，所以下一次干净的调用不会被再收一遍。会话
	// 的重试计数不归零：它是个总数。
	if r.billedFailures != 0 {
		t.Errorf("billedFailures = %d after the usage frame, want 0", r.billedFailures)
	}
	if r.retries != 2 {
		t.Errorf("retries = %d, want the session total 2", r.retries)
	}

	out.Reset()
	r.SessionSummary(100)
	s := out.String()
	if !strings.Contains(s, "2 retries") {
		t.Errorf("summary does not report the retry count:\n%s", s)
	}
	if !strings.Contains(s, "200 prompt tokens") {
		t.Errorf("summary does not report the re-billed tokens:\n%s", s)
	}
	// 报的是一个界，因为冷调用第一次尝试付的是缓存写，重试时付的是更便宜
	// 的缓存读，所以照抄成功那次尝试的拆分，会把第一次算少了。
	if !strings.Contains(s, "\u2265") {
		t.Errorf("summary states the estimate as exact:\n%s", s)
	}
}

// 更正。服务器拒掉的请求从来没被生成过，所以从来没被计费，而把它算进钱里，
// 就把这个面板上唯一诚实的那个数变成了一个恐怖故事。
//
// 这个测试存在，是因为阶段 09第一次实跑时，对一个真实成本是 $0.000276 的
// 会话，打出了"re-sent ≥1926 prompt tokens (≥$0.000301)"——而那之前注入的两个
// 503 根本没花一分钱。
func TestARefusedRequestIsNotReBilled(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "p", Prices: priceConfig{In: 1_000_000}}})

	// 在生成之前就被拒了：status 环节的一个 503，和一次被拒的连接。两次
	// 重试，零 token。
	r.OnEvent(Event{Kind: KindCallError, Attempt: 1, Phase: string(phaseStatus), Status: 503, Triage: string(TriageRetry)})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 2, Millis: 500})
	r.OnEvent(Event{Kind: KindCallError, Attempt: 2, Phase: string(phaseConnect), Triage: string(TriageRetry)})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 3, Millis: 1000})
	r.OnEvent(Event{Kind: KindUsage, Usage: &Usage{Input: 963, Output: 41}})

	if r.rebilled.Prompt() != 0 {
		t.Fatalf("rebilled = %d after two refused requests, want 0 — nothing was generated, so nothing was billed",
			r.rebilled.Prompt())
	}
	if r.rebilledCost != 0 {
		t.Fatalf("rebilledCost = %v, want 0", r.rebilledCost)
	}
	// 重试确实发生了，会话也确实丢了那些时间。没有发生的是计费。
	out.Reset()
	r.SessionSummary(963)
	if strings.Contains(out.String(), "re-sent") {
		t.Errorf("summary invented a re-bill for refused requests:\n%s", out.String())
	}
}

func TestACleanSessionSaysNothingAboutRetries(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{in: 1}, 0)
	r.OnEvent(Event{Kind: KindUsage, Usage: &Usage{Input: 100, Output: 5}})
	r.SessionSummary(100)
	if strings.Contains(out.String(), "retried") {
		t.Errorf("a clean session mentioned retries:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// trace
// ---------------------------------------------------------------------------

// Kind 字符串和 json tag 是写进 trace 文件里的，所以改一个名字，就会一声不响
// 地把改名之前记录的每一个会话的重放搞坏（events.go 里写了）。这里把第 09 阶
// 段新加的东西按名字钉住。
func TestStage09EventFieldsSurviveTheTrace(t *testing.T) {
	in := Event{
		Kind: KindCallError, Turn: 3, Status: 429, Phase: string(phaseStatus), ErrType: "rate_limit_error",
		Triage: string(TriageRetry), Attempt: 2, Millis: 1500,
		Provider: &ProviderInfo{Name: "backup", Protocol: "anthropic", Model: "m", Window: 2048,
			Prices: priceConfig{In: 1, Out: 2, CacheRead: 0.1, CacheWrite: 1.25}},
	}
	out := roundTripEvent(t, in)

	if out.Kind != KindCallError || out.Status != 429 || out.Phase != "status" || out.ErrType != "rate_limit_error" {
		t.Fatalf("round trip lost the failure fields: %+v", out)
	}
	if out.Triage != string(TriageRetry) || out.Attempt != 2 {
		t.Fatalf("round trip lost the decision fields: %+v", out)
	}
	if out.Provider == nil {
		t.Fatal("round trip lost the provider")
	}
	if out.Provider.Name != "backup" || out.Provider.Window != 2048 || out.Provider.Prices.CacheWrite != 1.25 {
		t.Fatalf("round trip mangled the provider: %+v", *out.Provider)
	}
	// 线上的名字，一个个写出来。在 Go 里改个字段名是看不见的；在 Go 里改个
	// json tag，会把每一份归档的 trace 都搞坏，而会察觉这件事的就是这条断言。
	raw := traceLineFor(t, in)
	for _, key := range []string{`"kind":"call_error"`, `"status":429`, `"phase":"status"`, `"err_type":`, `"triage":`, `"attempt":`, `"provider":`} {
		if !strings.Contains(raw, key) {
			t.Errorf("trace line is missing %s:\n%s", key, raw)
		}
	}
}

// 熬过去的失败，不是会话吃到的错误。把这两个计数器揉成一个，会让每个稳健的
// 会话都看起来像坏了；而没人信的头部，就是没人读的头部。
func TestSummarizeCountsFailuresApartFromErrors(t *testing.T) {
	s := Summarize([]Event{
		{Seq: 1, Kind: KindProvider, Provider: &ProviderInfo{Name: "primary"}}, // 会话开始：不是降级
		{Seq: 2, Kind: KindCallError, Status: 503, Triage: string(TriageRetry)},
		{Seq: 3, Kind: KindRetry, Millis: 500},
		{Seq: 4, Kind: KindCallError, Status: 401, Triage: string(TriageFallback)},
		{Seq: 5, Kind: KindProvider, Triage: string(TriageFallback), Provider: &ProviderInfo{Name: "backup"}},
		{Seq: 6, Kind: KindRetry, Millis: 900},
		{Seq: 7, Kind: KindError, Text: "the session gave up"},
	})

	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1 — only the terminal one", s.Errors)
	}
	if s.CallErrors != 2 {
		t.Errorf("CallErrors = %d, want 2", s.CallErrors)
	}
	if s.Retries != 2 {
		t.Errorf("Retries = %d, want 2", s.Retries)
	}
	if s.Fallbacks != 1 {
		t.Errorf("Fallbacks = %d, want 1 — the session-start provider event is not a fallback", s.Fallbacks)
	}

	head := s.String()
	for _, want := range []string{"1 error", "2 failed calls", "2 retries", "1 fallback"} {
		if !strings.Contains(head, want) {
			t.Errorf("header is missing %q:\n%s", want, head)
		}
	}
}

// 空事件必须短。阶段 09加的字段每一个都是 omitempty，少一个 tag，就会在每
// 一份 trace 的每一行上多出六个零——这正是 events.go 要求加它们的原因。
func TestStage09FieldsAreOmittedWhenEmpty(t *testing.T) {
	raw := traceLineFor(t, Event{Kind: KindTurnStart, Turn: 1})
	for _, key := range []string{"status", "phase", "err_type", "triage", "attempt", "provider"} {
		if strings.Contains(raw, key) {
			t.Errorf("a turn_start line carries %q:\n%s", key, raw)
		}
	}
}
