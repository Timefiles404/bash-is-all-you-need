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

// roundTripEvent 把一个事件经由真正的 TraceWriter 写出去，再用真正的
// ReadTrace 读回来，而不是走 json.Marshal/Unmarshal。要点是走一遍会话
// 真正落盘的那条路：一个能扛过直接 marshal、却扛不过 writer 的字段，
// 会通过一个更弱的测试，然后把重放弄坏。
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

// 这个文件测试阶段 09：分类器、退避、信封解析器、重试循环和梯子。
//
// 它几乎没有一处需要网络，而被测的既是代码，也同样是这个设计。决策都
// 住在对 *CallError 的纯函数里，所以那些有意思的情形——一个意思是"模型
// 名不对"的 401，一个意思是"我们自己的 bug"的 500，一个在宕机中途耗尽
// 的预算——都是表里的一行行，不是集成测试的 fixture。真的立起一个
// httptest 服务器的那三个测试，是**传输**本身成为主角的那几个：一个真
// 的 Retry-After 头，一个真的被截断的响应体，一个真的以 text/plain 送
// 来的错误信封。
//
// 下面每一行表格都能追溯到 docs/wire-notes.md 的 §D11/§A3c，或者 RFC
// 9110。不是观测到的行为的那些行，会自己说明。

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
		// 整个这一阶段就是为了这两行才存在的。同样的状态码，同样的
		// 信封形状，相反的决策——§D11。
		{"401 revoked key", CallError{Phase: phaseStatus, Status: 401, Type: "AuthError"}, TriageFatal,
			"a bad key cannot be fixed by waiting or by asking someone else"},
		{"401 wrong model name", CallError{Phase: phaseStatus, Status: 401, Type: "ModelError"}, TriageFallback,
			"observed: a nonexistent model id returns 401 here, not 404"},

		// 一个没有信封可读的 401。判成 Fatal 是安全的读法：没有
		// error.type，就没有任何东西说得出"模型"，而把一个读不出来的
		// 认证失败当成降级来处理，会在一台密钥根本就不存在的机器上
		// 把整个梯子走完。
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

		// 第二个陷阱。可重试，但见下面那个牵绳测试。
		{"500", CallError{Phase: phaseStatus, Status: 500, Type: "error", Message: "Internal server error"}, TriageRetry, ""},
		{"502", CallError{Phase: phaseStatus, Status: 502}, TriageRetry, ""},
		{"503", CallError{Phase: phaseStatus, Status: 503}, TriageRetry, ""},
		{"504", CallError{Phase: phaseStatus, Status: 504}, TriageRetry, ""},

		// 任何没被分类的东西都是 Fatal，这是故意的：一个没被分类的
		// 失败拿去重试，就是把这个失败重演一遍，而发出去的那个事件
		// 就是缺掉的那一种情形被发现的途径。
		{"418", CallError{Phase: phaseStatus, Status: 418}, TriageFatal, ""},
		{"302", CallError{Phase: phaseStatus, Status: 302}, TriageFatal, ""},

		{"build", CallError{Phase: phaseBuild}, TriageFatal, "our own bug; it will not render on the second try either"},
		{"connect", CallError{Phase: phaseConnect}, TriageRetry, "nothing generated, nothing billed: the only free retry"},

		// §A3c 的邻居：一个流内的错误事件。type 说了算，而没有 type
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

// PascalCase 那个发现，作为一条性质来测，而不是作为一行来测。
//
// §D11 观测到的是 `ModelError`，而两份协议规范写的都是 snake_case
// （`not_found_error`、`invalid_request_error`）。拿其中任何一种拼法做
// 相等判断，对着文档是对的，对着线上是错的，所以分类器匹配的是子串——
// 而如果有人把它"整理"成一个按精确值分支的 switch，这个测试就会失败。
func TestTriageMatchesModelErrorWhateverItsCasing(t *testing.T) {
	for _, typ := range []string{"ModelError", "model_error", "MODEL_NOT_FOUND", "not_found_model_error"} {
		if got := triageStatus(401, typ); got != TriageFallback {
			t.Errorf("triageStatus(401, %q) = %q, want fallback", typ, got)
		}
	}
	// 还有反面：一个认证失败不能被一个无关的词拽进模型那条分支
	// 里去。
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
		{500, 2}, // 跟他们宕机一样可能是我们配错了（§D11）
		{502, 2},
		{504, 2},
		{503, 0}, // 真正的容量信号：全额尝试次数
		{429, 0},
	}
	for _, c := range cases {
		e := CallError{Phase: phaseStatus, Status: c.status}
		if got := e.leash(); got != c.want {
			t.Errorf("leash(%d) = %d, want %d", c.status, got, c.want)
		}
	}
	// 流断掉不是一个状态码，所以那条照着状态码长出来的规则不能伸
	// 到它身上：正是这一行，挡住了把 leash() 写成不带环节判断的
	// `if Status >= 500`。
	if got := (&CallError{Phase: phaseStream}).leash(); got != 0 {
		t.Errorf("leash(stream) = %d, want 0", got)
	}
	// 同一条规则，放在唯一一种能从上面那一行底下溜过去的组合下面。
	// 这一阶段里没有任何东西会给一个流错误设状态码，所以一个不看
	// 环节的 `if Status >= 500` 看着无害——直到阶段 10 的看门狗开
	// 始把它拿到 200 时的那个状态码一路带着，于是流中途的断裂就悄
	// 悄继承了本来为服务器错误准备的那根短牵绳。
	if got := (&CallError{Phase: phaseStream, Status: 500}).leash(); got != 0 {
		t.Errorf("leash(stream carrying a 500) = %d, want 0 — the leash rule is about statuses, not streams", got)
	}
}

// 压缩拿到的牵绳比会话策略更短，不管会话策略是什么。每一次尝试都按全
// 价重发整份文字记录，而且它这么做的时候，那个需要腾出空间的回合还在
// 等着。
func TestForCompactionShortensTheLeash(t *testing.T) {
	got := retryPolicy{attempts: 9, base: time.Second, max: time.Minute, budget: time.Hour}.forCompaction()
	if got.attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.attempts)
	}
	if got.budget != 5*time.Second {
		t.Errorf("budget = %v, want 5s", got.budget)
	}
	// 一个本来就比这个上限更严的策略会被原样留下：上限是天花板，
	// 不是一个设定项。`--retry 1` 意思是到处都只尝试一次，这里也
	// 一样。
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

		// 就是这一行，让在 CallError 上留着原始响应体这件事站得住脚。
		// 一个完全没有信封的 400：把请求原样回显的 24 个字节。
		{"400 with no envelope (D11)", `{"model":"qwen3.7-plus"}`, "", "", true},

		// 一个 OpenAI 形状的响应体，带一个 `code` 字段，这个网关不发，
		// 但另一个端点会发。`code` 的类型是 `any`，因为在野外它既会以
		// 字符串到达，也会以 null 到达；那里放一个 `string` 字段，会让
		// 整个信封 unmarshal 失败，把 message 也一起丢掉。
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

// 没有信封这种情形，得一路活到人读的那句消息，不只是活到解析器。
// "http 400: "后面拖一条空尾巴，读起来像是 Agent 里的一个 bug；把这
// 个"没有"点出来，才是指向服务器，而响应体就在那一行里。
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
		// 一个已经过去的日期意思是"现在"，不是一次负的 sleep。这里返
		// 回一个带符号的时长，会一路流进 time.Sleep 然后立刻返回，恰
		// 好在服务器正开口要一次退避的时候把退避关掉。
		{"http date in the past", "Thu, 27 Aug 2026 11:59:30 GMT", 0},
		// 解析不出来就忽略，而不是去猜：算出来的退避是一个已知安全的
		// 数字，编出来的那个不是。
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
	full := func() float64 { return 1 } // 抖动区间的上端

	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	for i, w := range want {
		if got := p.wait(i+1, 0, full); got != w {
			t.Errorf("wait(%d) = %v, want %v", i+1, got, w)
		}
	}

	// 在一个跑得很久的会话里，绝不能让这个移位溢出成一个负的或零
	// 的时长。没有 `exp <= 0` 这道守卫，wait(64) 返回 0，循环就彻
	// 底不等了。
	if got := p.wait(64, 0, full); got != p.max {
		t.Errorf("wait(64) = %v, want the cap %v — the shift overflowed", got, p.max)
	}
}

// 全抖动，不是半抖动。要的性质是这次抽样覆盖从零开始的整个区
// 间：当好几个子 Agent 共用一个 client、一个端点时，一套最小等
// 待是 exp/2 的策略会让自己的这些客户端保持同步，每一次尝试都
// 重新撞在一起。
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

	// 服务器知道自己的容量什么时候回来，我们不知道，所以它的
	// 数字胜出——哪怕它比我们的退避更短。
	if got := p.wait(5, 250*time.Millisecond, full); got != 250*time.Millisecond {
		t.Errorf("wait with Retry-After 250ms = %v, want 250ms", got)
	}
	// 但服务器也有权说"一个小时"，而尊重它的 Agent 看起来就是
	// 卡死了。尊重的是这个请求的形状，不是它的长度。
	if got := p.wait(1, time.Hour, full); got != p.max*8 {
		t.Errorf("wait with Retry-After 1h = %v, want the clamp %v", got, p.max*8)
	}
}

// ---------------------------------------------------------------------------
// 循环
// ---------------------------------------------------------------------------

// loopFixture 用一串编排好的结果驱动 retryLoop，并且没有
// 时钟：sleep 只被记录，不真的执行，所以一个 30 秒预算的
// 测试跑起来只要几微秒。
type loopFixture struct {
	t       *testing.T
	rec     *mulRecorder
	bus     *Bus
	slept   []time.Duration
	seen    []Provider // 每次尝试实际跑在哪一级上
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

// oneRung 是上面只有一个供应商的梯子，也就是一个不带
// --fallback 的会话所拿到的东西。
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
	// 是指数的，而且就是这个顺序。一个每次尝试都重置指数的策略
	// 会睡两次 1s，然后通过一个更弱的断言。
	if f.slept[0] != time.Second || f.slept[1] != 2*time.Second {
		t.Errorf("waits = %v, want [1s 2s]", f.slept)
	}
	if got := f.rec.count(KindCallError); got != 2 {
		t.Errorf("call_error events = %d, want 2", got)
	}
	if got := f.rec.count(KindRetry); got != 2 {
		t.Errorf("retry events = %d, want 2", got)
	}
	// 裁决在事件上，不只在日志行里：阶段 18 的指标是从 trace
	// 里读它的。
	for _, e := range f.rec.kind(KindCallError) {
		if e.Triage != string(TriageRetry) {
			t.Errorf("call_error carried triage %q, want retry", e.Triage)
		}
		if e.Status != 503 {
			t.Errorf("call_error carried status %d, want 503", e.Status)
		}
		// 环节必须活过从 CallError 到事件这一趟。没有它，面板就分不
		// 清一个被拒的请求（免费）和一个断掉的流（计费），重复计费
		// 的数字就是编出来的——正是本阶段第一次实跑造出的那个 bug。
		if e.Phase != string(phaseStatus) {
			t.Errorf("call_error carried phase %q, want status", e.Phase)
		}
	}
	// 尝试的编号必须能用：一个重试事件宣布的是它即将做的那次
	// 尝试，不是刚刚失败的那次。
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

// 牵绳，端到端。策略允许五次尝试，只用了两次，因为在这个
// 网关上，一个光秃秃的 500 同样可能是客户端的 bug（§D11）。
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
	// 以及对照，在同一个策略下：503 拿到全部。
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
	// 允许十次尝试，但只有 15s 的等待：前两次等待是 10s 和 20s，
	// 所以停下它的是预算，不是尝试次数。
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
	// 一个普通的 error，不是 *CallError：调用路径里有东西以本
	// 阶段没有建模的方式失败了。返回它而不是重试它，因为一个
	// 没被分类的失败去重试，只是把这个失败重复一遍。
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
	// 而且原因就在那行里，因为"等 3s"和"等 3s，因为服务器要求
	// 3s"会导向不同的调试。
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
	// §D11 那个情形：一个错的模型名以 401 的形式到来，而正确的
	// 动作是换一个端点，不是放弃。
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
	// 价格跟着一起走。没有它们，面板就会一直按主供应商的价格给
	// 备用供应商的 token 计费。
	if e.Provider.Prices.In != 10 {
		t.Errorf("provider event carried prices %+v, want the backup's", e.Provider.Prices)
	}
	if !strings.Contains(e.Text, "ModelError") {
		t.Errorf("provider event text = %q, want the reason in it", e.Text)
	}
}

func TestFallbackHappensAfterTheRetriesRunOutToo(t *testing.T) {
	// 一个可重试的失败一直失败下去，值得在放弃之前朝梯子看一眼：
	// "供应商挂了"和"这个供应商挂了"是两句不同的话。
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

// 刚刚被降级到的那一级，拿到属于自己的全部尝试额度。把上一
// 级的计数带过来，就意味着一个健康的备用供应商失败一次就被
// 放弃了，只因为已经死掉的主供应商早把额度用光了。
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
	// 会话没法继续下去的原因，是最后一个拒绝它的东西，不是第一
	// 个。在这里报告主供应商的错误，会把人打发去修一个模型名，
	// 而真正的问题是一把被吊销的密钥。
	if !strings.Contains(err.Error(), "backup key is revoked") {
		t.Errorf("error = %q, want the last rung's failure", err)
	}
}

// 两个子 Agent 在同一瞬间撞在同一个死掉的端点上，必须只花掉
// 一级，不是两级。一个只做自增的 advance() 会跳过一个谁都没
// 试过的健康供应商。
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

// advance() 里那道守卫真正挣到自己位置的地方，而且不是上面那
// 个八 goroutine 测试覆盖的情形：那个测试里每个调用方都在同
// 一级上，对它们来说 `cur = from + 1` 本身就是幂等的。
//
// 这个失败需要三个参与者，而且它是一次回退。A 从第 0 级掉下
// 来，落到第 1 级。C 从第 1 级掉下来，落到第 2 级。然后 B——
// 手里还攥着这一切发生之前读到的第 0 级——请求降级，而没有
// 那道守卫，它就会写下 cur = 1，把下一次调用送给一个已经被
// 两个兄弟放弃掉的供应商。这是这个文件里唯一会注意到的测试。
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

	// 落后者。必须回答它"是"——**确实**还有别的地方可以送——同
	// 时又不能让梯子丢掉别人已经挣到的位置。
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
		// 协议是从构建好的供应商上取的，不是从配置字符串上取的，所
		// 以一个协议和模型互相矛盾的级不可能存在。
		if lad.rungs[1].info.Protocol != "anthropic" {
			t.Errorf("rung 1 protocol = %q", lad.rungs[1].info.Protocol)
		}
	})

	t.Run("a duplicate is refused", func(t *testing.T) {
		// 一个把同一个供应商列两遍的梯子，读起来像是多了一层韧性，
		// 实际一点也没给：第二级会因为第一级失败的那个原因而失败，
		// 它买到的只是放弃之前更长的一段等待。
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
		// 每一级都提前构建出来的全部理由。按需构造的降级会按需失败，
		// 而它被需要的那一刻，正是它唯一为之存在的一刻。
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
// 真实传输之上的 modelCall
// ---------------------------------------------------------------------------

func triageProvider(t *testing.T, srv *httptest.Server) Provider {
	t.Helper()
	return newOpenAIProvider(srv.URL, "sk-test", "test-model")
}

// 在这个网关上，信封是以 text/plain 到来的（§D11），这正是
// parseErrorBody 里没有任何东西去看 content type 的原因。一个
// 按它分支的客户端，会把这个端点产生的每一个错误都报成"无法
// 解析的错误"。
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
	// 这个仓库读过的第一个响应头。在阶段 09 之前，就算真来了
	// 一个 Retry-After，Agent 也没法尊重它。
	if ce.RetryAfter != 2*time.Second {
		t.Fatalf("RetryAfter = %v, want 2s", ce.RetryAfter)
	}
}

// 本阶段要来补的那道缝。两个适配器都会在流错误旁边返回一个
// 部分结果，这是故意的，而在此之前的每一个阶段都接住了那个
// 值，然后把它丢掉。
func TestModelCallKeepsThePartialWhenTheStreamBreaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 一个比 body 更长的 Content-Length，于是客户端读 body 时会
		// 以一个 unexpected EOF 失败，而不是干净地结束。一个只是停
		// 下来的流不是错误——sse.go 会在 EOF 时把最后一帧刷出去，
		// 这是故意的——所以这就是复现一个真正断掉的连接的办法。
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
	// 而 trace 会说这个响应是断了，不是结束了：没有
	// response_end，而这个信号适配器从阶段 02 就一直带着，到
	// 阶段 09 才终于被读。
	if got := rec.count(KindResponseEnd); got != 0 {
		t.Errorf("emitted %d response_end events for a stream that broke", got)
	}
}

// 流失败的另一种方式，它走的是 modelCall 的另一个分支：供应商
// 在 body 中途发来一个 `error` **事件**。anthropic.go 把它变成
// 一个带着供应商自己的 error.type 的 *CallError，而 modelCall
// 必须**增补**它，而不是包装它——让这个类型对分类器保持可达，
// 并把部分结果挂到它上面。
//
// 在这个网关上没有观测到（§D11 的错误全都在流打开之前以一个
// HTTP 状态码到来），但规范里有它，而且当一个供应商在响应中
// 途劣化时，`overloaded_error` 走的就是这条路。
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

// 一次被重试的调用必须重建自己的请求。*http.Request 的 body
// 在第一次 Do 之后就是一个已经被消耗掉的 reader，所以一个复
// 用请求对象的循环，会在第 2 次尝试上发出零字节——一个长得
// 和服务器 bug 一模一样的重试 bug。
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

// 一次降级会在会话中途改掉计价的基准，而面板必须跟上。按主
// 供应商的价格给备用供应商的 token 计费，会产出一份自信地错
// 着的报告，而那比一份承认自己不知道的报告更糟。
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
	// 窗口也跟着走：上下文水位线是一个刚刚变掉的数字的比例。
	if r.window != 2000 {
		t.Errorf("window = %d, want the backup's 2000", r.window)
	}
	if !strings.Contains(out.String(), "backup") {
		t.Errorf("the fallback was not announced:\n%s", out.String())
	}
}

// 会话开始时的 provider 事件不能当成一次降级来宣布：横幅已经
// 点过供应商的名字了，而每一次干净的启动都印一行
// "provider →"，正是人们学会跳过这一整块的原因。
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

// 没有别的 Agent 会报的那个数字：失败的尝试花了多少钱。
func TestTheSummaryReportsWhatRetriesReBilled(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "p", Prices: priceConfig{In: 1_000_000}}})

	// 两个打开了又死掉的流，然后是成功的那次尝试，它终于报出了
	// 一个真实的 prompt 数字。估算就在那一刻做出，因为那是这个
	// prompt 的真实数字唯一存在的一刻。
	//
	// 环节在这里是要紧的，不只是为了整洁：见下面那个配套测试，
	// 以及 render.go 里关于那次把这件事搞错了的实跑的注释。
	r.OnEvent(Event{Kind: KindCallError, Attempt: 1, Phase: string(phaseStream), Triage: string(TriageRetry), Text: "stream broke"})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 2, Millis: 500})
	r.OnEvent(Event{Kind: KindCallError, Attempt: 2, Phase: string(phaseStream), Triage: string(TriageRetry), Text: "stream broke"})
	r.OnEvent(Event{Kind: KindRetry, Attempt: 3, Millis: 1000})
	r.OnEvent(Event{Kind: KindUsage, Usage: &Usage{Input: 10, CacheRead: 90, Output: 5}})

	if got := r.rebilled.Prompt(); got != 200 {
		t.Fatalf("rebilled = %d, want 200 (two failed attempts at the successful attempt's 100-token prompt)", got)
	}
	// 那个拆分被保住了，没有被压成全价的 input：正是这一点让它
	// 成为一个站得住的下界，而不是一个吓人的数字。
	if r.rebilled.Input != 20 || r.rebilled.CacheRead != 180 {
		t.Errorf("rebilled split = %+v, want the successful attempt's shape doubled", r.rebilled)
	}
	// 每次调用的那个倍数会重置，所以下一次干净的调用不会再被收
	// 一遍钱。会话的重试计数不重置：它是一个总数。
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
	// 报成一个界，因为一次冷调用在第一次尝试上付的是缓存写，在
	// 重试上付的是更便宜的缓存读，所以照抄成功那次尝试的拆分，
	// 会把第一次算少了。
	if !strings.Contains(s, "\u2265") {
		t.Errorf("summary states the estimate as exact:\n%s", s)
	}
}

// 这处更正。一个被服务器拒掉的请求从来没有被生成过，所以从
// 来没有被计费，而为它收钱，会把这块面板里唯一诚实的那个数
// 字变成一个恐怖故事。
//
// 这个测试存在，是因为阶段 09 第一次实跑时，为一个真实成本
// 是 $0.000276 的会话印出了"re-sent ≥1926 prompt tokens
// (≥$0.000301)"——而在那之前注入的两个 503 一分钱都没花。
func TestARefusedRequestIsNotReBilled(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, prices{}, 0)
	r.OnEvent(Event{Kind: KindProvider, Provider: &ProviderInfo{Name: "p", Prices: priceConfig{In: 1_000_000}}})

	// 在生成之前就被拒掉：一个在 status 环节的 503，和一个被拒的
	// 连接。两次重试，零 token。
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
	// 重试确实发生了，会话也确实丢掉了那段时间。没有发生的是一
	// 笔收费。
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

// Kind 字符串和 json tag 会被写进 trace 文件，所以改掉一个
// 名字，就会静默地破坏改名之前记录的每一个会话的重放
// （events.go 这么说了）。这里把阶段 09 新增的那些按名字
// 钉住。
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
	// 线上的那些名字，一个个拼出来。在 Go 里改一个字段名是看不见
	// 的；在 Go 里改一个 json tag 会破坏每一份归档的 trace，而这
	// 就是会注意到的那条断言。
	raw := traceLineFor(t, in)
	for _, key := range []string{`"kind":"call_error"`, `"status":429`, `"phase":"status"`, `"err_type":`, `"triage":`, `"attempt":`, `"provider":`} {
		if !strings.Contains(raw, key) {
			t.Errorf("trace line is missing %s:\n%s", key, raw)
		}
	}
}

// 一个被扛过去的失败，不是这次会话遭受的一个错误。把两个计
// 数器折在一起，会让每一个健壮的会话看起来都是坏的，而一个
// 没人相信的表头，就是一个没人读的表头。
func TestSummarizeCountsFailuresApartFromErrors(t *testing.T) {
	s := Summarize([]Event{
		{Seq: 1, Kind: KindProvider, Provider: &ProviderInfo{Name: "primary"}}, // 会话开始：不是一次降级
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

// 一个空事件必须保持短。阶段 09 的每一个字段都是 omitempty，
// 少一个 tag 就会在每一份 trace 的每一行上都放上六个零——这
// 就是 events.go 要求它们的原因。
func TestStage09FieldsAreOmittedWhenEmpty(t *testing.T) {
	raw := traceLineFor(t, Event{Kind: KindTurnStart, Turn: 1})
	for _, key := range []string{"status", "phase", "err_type", "triage", "attempt", "provider"} {
		if strings.Contains(raw, key) {
			t.Errorf("a turn_start line carries %q:\n%s", key, raw)
		}
	}
}
