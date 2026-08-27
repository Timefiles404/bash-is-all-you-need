package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 辅助代码
// ---------------------------------------------------------------------------

// traceRecorder 是每个测试拿来断言的那个 Subscriber：事件总线的意义就在于，
// "用户看到了什么"是份能直接比对的列表，不是一段还得你去抠的字符串。
type traceRecorder struct{ events []Event }

func (r *traceRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

// traceSameEvent 按文件格式真正承诺的方式去比两个事件。
//
// 这里对整个 struct 用 reflect.DeepEqual 是错的，而且错在哪儿值得知道：
// time.Now() 带着单调时钟读数和本地的 *time.Location，从 JSON 里解回来的时间戳
// 两者都没有。这两个值指的是同一个瞬间，却永远不会深度相等。瞬间用 Equal 比，
// 其余的按结构比。
func traceSameEvent(a, b Event) bool {
	if !a.T.Equal(b.T) {
		return false
	}
	a.T, b.T = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// traceSample 是一段真实的会话：用户消息、流式回复、跑了条命令的工具调用、带缓
// 存的 token 账，以及一处错误。Seq 和 T 故意留成零值——它们由 Bus 盖上，在测
// 试里自己伪造，测的就是这个测试本身了。
func traceSample() []Event {
	return []Event{
		{Kind: KindUserMessage, Text: "how big is this repo?"},
		{Kind: KindTurnStart, Turn: 1},
		{Kind: KindRequest, Turn: 1, Request: json.RawMessage(
			`{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"how big is this repo?"}]}`)},
		{Kind: KindFirstToken, Turn: 1, Millis: 812},
		{Kind: KindReasoningDelta, Turn: 1, Text: "cheapest check is wc -l"},
		{Kind: KindTextDelta, Turn: 1, Text: "Let me count the lines."},
		{Kind: KindToolCallStart, Turn: 1, ToolID: "call_01", ToolName: "bash"},
		{Kind: KindToolArgsDelta, Turn: 1, ToolID: "call_01", Text: `{"command":"find . -na`},
		{Kind: KindToolCallReady, Turn: 1, ToolID: "call_01", Command: `find . -name '*.go' | xargs wc -l`},
		{Kind: KindGateVerdict, Turn: 1, ToolID: "call_01", Verdict: "allow"},
		{Kind: KindCommandStart, Turn: 1, ToolID: "call_01", Command: `find . -name '*.go' | xargs wc -l`},
		{Kind: KindCommandEnd, Turn: 1, ToolID: "call_01", ExitCode: 0, Millis: 143, Bytes: 2048},
		{Kind: KindToolResult, Turn: 1, ToolID: "call_01", Text: "  1204 total\n[exit 0 · 143ms]", Bytes: 30},
		// 整个仓库存在的意义就是把这个形状露出来：18 个 token 按全价计费，
		// 17,967 个从缓存读。任何把"18 个 input token"当成这次调用规模的东西，
		// 都差了一千倍。
		{Kind: KindUsage, Turn: 1, Usage: &Usage{Input: 18, CacheRead: 17967, Output: 214, Reasoning: 96}},
		{Kind: KindResponseEnd, Turn: 1, FinishReason: "tool_calls", Millis: 2210},
		{Kind: KindTurnEnd, Turn: 1},
		{Kind: KindTurnStart, Turn: 2},
		{Kind: KindRequest, Turn: 2, Request: json.RawMessage(`{"model":"claude-opus-5","messages":[]}`)},
		{Kind: KindUsage, Turn: 2, Usage: &Usage{Input: 512, CacheWrite: 4096, Output: 88}},
		{Kind: KindNotice, Turn: 2, Text: "context is 22% full"},
		{Kind: KindError, Turn: 2, Text: "http 529: overloaded"},
		{Kind: KindResponseEnd, Turn: 2, FinishReason: "stop", Millis: 1180},
		{Kind: KindTurnEnd, Turn: 2},
	}
}

// traceRecordSession 把 traceSample 经一条真实的 Bus 发出去，Bus 上挂着
// TraceWriter；返回文件路径，以及总线原原本本投递出去的东西。
func traceRecordSession(t *testing.T) (string, []Event) {
	t.Helper()

	// 这里的子目录还不存在：真实的 trace 写到 traces/<date>/ 下面，所以
	// NewTraceWriter 自己建父目录是契约的一部分，不是顺手给的方便。
	path := filepath.Join(t.TempDir(), "traces", "session.jsonl")
	w, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	if w.Path() != path {
		t.Errorf("Path() = %q, want %q", w.Path(), path)
	}

	rec := &traceRecorder{}
	bus := NewBus(w, rec)
	for _, e := range traceSample() {
		bus.Emit(e)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, rec.events
}

// ---------------------------------------------------------------------------
// trace 文件
// ---------------------------------------------------------------------------

func TestTraceRoundTrip(t *testing.T) {
	path, want := traceRecordSession(t)

	got, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d events, wrote %d", len(got), len(want))
	}

	for i := range want {
		if got[i].Seq != i+1 {
			t.Errorf("event %d: Seq = %d, want %d (the file must preserve bus order)", i, got[i].Seq, i+1)
		}
		if !traceSameEvent(got[i], want[i]) {
			t.Errorf("event %d (%s) did not survive the round trip:\n got %+v\nwant %+v",
				i, want[i].Kind, got[i], want[i])
		}
	}

	// 抽查那些草率的 schema 会悄悄丢掉的字段：指针类型的 Usage，以及原
	// 始的请求体。
	usage := got[13]
	if usage.Kind != KindUsage || usage.Usage == nil {
		t.Fatalf("event 13 = %+v, want a usage event with a Usage payload", usage)
	}
	if usage.Usage.Input != 18 || usage.Usage.CacheRead != 17967 || usage.Usage.Reasoning != 96 {
		t.Errorf("Usage = %+v, want {Input:18 CacheRead:17967 Output:214 Reasoning:96}", *usage.Usage)
	}

	// Request 是 json.RawMessage，所以字节相等才是真正的断言——而它能成立，只因
	// 为 traceSample 里的请求体紧凑，且不含 <、> 和 &，这三个字符 encoding/json
	// 在输出时会转义。谁把抓下来的请求体重新缩进、或者重新 marshal 一遍，它就不
	// 再是"发出去了什么"的记录。
	wantBody := traceSample()[2].Request
	if !bytes.Equal(got[2].Request, wantBody) {
		t.Errorf("request body changed:\n got %s\nwant %s", got[2].Request, wantBody)
	}
}

func TestTraceTruncatedFinalLine(t *testing.T) {
	path, want := traceRecordSession(t)

	// 把最后一行砍掉一半：SIGKILL 落在 write(2) 和缓冲区末尾之间，留下的就
	// 是这个样子。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) != len(want) {
		t.Fatalf("file has %d lines, want %d", len(lines), len(want))
	}
	last := lines[len(lines)-1]
	maimed := append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n')
	maimed = append(maimed, last[:len(last)/2]...) // 没有结尾换行符，这就是破绽
	if err := os.WriteFile(path, maimed, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace on a truncated trace must not fail: %v", err)
	}

	// 伤口之前的全部内容，加上一条合成出来的 notice 说明伤在哪。
	recovered := len(want) - 1
	if len(got) != recovered+1 {
		t.Fatalf("got %d events, want %d recovered + 1 notice", len(got), recovered)
	}
	for i := 0; i < recovered; i++ {
		if !traceSameEvent(got[i], want[i]) {
			t.Errorf("event %d was damaged by the recovery:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}

	notice := got[len(got)-1]
	if notice.Kind != KindNotice {
		t.Fatalf("last event is %s, want a %s explaining the truncation", notice.Kind, KindNotice)
	}
	if !strings.HasPrefix(notice.Text, TraceNoticePrefix) {
		t.Errorf("notice %q must carry %q so a renderer can tell it from an agent notice",
			notice.Text, TraceNoticePrefix)
	}
	// "把情况报出来"的意思是说清活下来了多少，说在人会读的那段文字里——而不是塞
	// 进错误里，调用方拿到错误很可能直接 fatal。
	for _, substr := range []string{"partial line", "22 event(s) recovered"} {
		if !strings.Contains(notice.Text, substr) {
			t.Errorf("notice %q does not mention %q", notice.Text, substr)
		}
	}
	if notice.Seq != want[recovered-1].Seq+1 {
		t.Errorf("notice Seq = %d, want %d (it must continue the sequence)", notice.Seq, want[recovered-1].Seq+1)
	}
}

func TestTraceUnknownKindStillLoads(t *testing.T) {
	// 手写的，不是录出来的：这份文件出自 Agent *以后*某个版本，里面的 kind 和
	// 字段，当前这个二进制根本没听说过。读取端要是拿自己的常量去校验 kind，那
	// 么下一个特性上线之后录的每份 trace 都重放不了——而这跟耐久文件格式存在
	// 的意义正好相反。
	path := filepath.Join(t.TempDir(), "future.jsonl")
	body := strings.Join([]string{
		`{"seq":1,"t":"2026-08-27T09:15:00.123456789Z","kind":"user_message","text":"hi"}`,
		`{"seq":2,"t":"2026-08-27T09:15:01Z","kind":"subagent_spawn","text":"reviewer","depth":2,"budget":{"usd":0.5}}`,
		`{"seq":3,"t":"2026-08-27T09:15:02Z","kind":"turn_end","turn":1}`,
		``, // 多出来的空行，这不算损坏
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (unknown kinds must load, blank lines must not count)", len(got))
	}
	if got[1].Kind != "subagent_spawn" {
		t.Errorf("Kind = %q, want the unknown kind preserved verbatim", got[1].Kind)
	}
	if got[1].Text != "reviewer" {
		t.Errorf("Text = %q — known fields must survive alongside unknown ones", got[1].Text)
	}
	if got[0].T.UTC().Nanosecond() != 123456789 {
		t.Errorf("timestamp lost precision: %s", got[0].T.UTC().Format(time.RFC3339Nano))
	}
}

func TestTraceWriterDegradesInsteadOfDying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doomed.jsonl")
	w, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	var warnings []string
	w.warn = func(format string, args ...any) { warnings = append(warnings, format) }

	w.OnEvent(Event{Seq: 1, T: time.Now(), Kind: KindUserMessage, Text: "recorded fine"})

	// 把文件从写入方脚底下抽掉：磁盘满了、卷被卸载了、运维对着 trace 目
	// 录跑了 `rm`。
	if err := w.f.Close(); err != nil {
		t.Fatalf("closing the underlying file: %v", err)
	}
	for i := 0; i < 50; i++ {
		w.OnEvent(Event{Seq: i + 2, Kind: KindTextDelta, Text: "into the void"})
	}

	if len(warnings) != 1 {
		t.Errorf("got %d warnings, want exactly 1 — a broken trace must be reported once, not 50 times", len(warnings))
	}
	if w.dropped != 50 {
		t.Errorf("dropped = %d, want 50", w.dropped)
	}
	closeErr := w.Close()
	if closeErr == nil || !strings.Contains(closeErr.Error(), "50 event(s)") {
		t.Errorf("Close() = %v, want an error naming the 50 lost events", closeErr)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil (main defers it, a signal handler may also call it)", err)
	}

	// 而已经写进去的事件仍然读得出来：降级保住了文件的有效性，没有留下半行没
	// 写完的东西。
	got, err := ReadTrace(path)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadTrace = %d events, %v; want the 1 event written before the failure", len(got), err)
	}
}

// ---------------------------------------------------------------------------
// Summarize
// ---------------------------------------------------------------------------

func TestSummarizeUsesPromptNotInput(t *testing.T) {
	_, events := traceRecordSession(t)
	s := Summarize(events)

	if s.Events != len(events) {
		t.Errorf("Events = %d, want %d", s.Events, len(events))
	}
	if s.Turns != 2 {
		t.Errorf("Turns = %d, want 2", s.Turns)
	}
	if s.Commands != 1 {
		t.Errorf("Commands = %d, want 1", s.Commands)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1", s.Errors)
	}

	// 两个 usage 事件分别是 {18, cache_read 17967, out 214} 和
	// {512, cache_write 4096, out 88}：真实的形状——没走缓存的那点余量，只占实
	// 际发出去的 2%。
	want := Usage{Input: 530, CacheWrite: 4096, CacheRead: 17967, Output: 302, Reasoning: 96}
	if s.TotalUsage != want {
		t.Errorf("TotalUsage = %+v, want %+v", s.TotalUsage, want)
	}

	// 这条断言就是整个 struct 存在的理由。把 Input 加起来是 530——这数看着像
	// token，排序起来也像 token，可它错了 22,063 个。
	if s.PromptTokens() != 22593 {
		t.Errorf("PromptTokens() = %d, want 22593 (Input + CacheWrite + CacheRead)", s.PromptTokens())
	}
	if s.PromptTokens() == s.TotalUsage.Input {
		t.Fatalf("PromptTokens() must not be the sum of Input alone")
	}

	// 学生在重放之前读到的那行头，必须把这笔账拆开显示，否则便宜的 token 和贵的
	// token 长得一模一样。
	header := s.String()
	for _, substr := range []string{"prompt 22593", "full 530", "write 4096", "read 17967", "output 302", "1 error"} {
		if !strings.Contains(header, substr) {
			t.Errorf("header %q is missing %q", header, substr)
		}
	}
}

func TestSummarizeEmptyAndClockSafety(t *testing.T) {
	if s := Summarize(nil); s.Events != 0 || s.Duration != 0 || s.PromptTokens() != 0 {
		t.Errorf("Summarize(nil) = %+v, want a zero summary", s)
	}

	// 有个事件干脆没有时间戳（手写的，或者未来版本写的）。Duration 不能因此变成
	// 从零时刻算起的 55 年。
	base := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	s := Summarize([]Event{
		{Seq: 1, Kind: KindNotice}, // T 是零值
		{Seq: 2, T: base, Kind: KindTurnStart},
		{Seq: 3, T: base.Add(90 * time.Second), Kind: KindTurnEnd},
	})
	if s.Duration != 90*time.Second {
		t.Errorf("Duration = %s, want 1m30s", s.Duration)
	}
	if got := traceDur(s.Duration); got != "1m30s" {
		t.Errorf("traceDur = %q, want %q", got, "1m30s")
	}
}

// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------

func TestReplayDeliversEveryEventInOrder(t *testing.T) {
	path, want := traceRecordSession(t)
	events, err := ReadTrace(path)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}

	rec := &traceRecorder{}
	var out bytes.Buffer
	if err := Replay(events, rec, ReplayOpts{Speed: 0}, nil, &out); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(rec.events) != len(want) {
		t.Fatalf("replayed %d events, want %d", len(rec.events), len(want))
	}
	for i := range want {
		if !traceSameEvent(rec.events[i], want[i]) {
			// Replay 不能重新盖 T：录下来的那个时钟才是证据。
			t.Fatalf("replayed event %d differs from the recorded one:\n got %+v\nwant %+v",
				i, rec.events[i], want[i])
		}
	}
	if !strings.Contains(out.String(), "trace · 23 events · 2 turns · 1 command") {
		t.Errorf("replay header missing or wrong:\n%s", out.String())
	}
}

func TestReplayFilterShowsOnlyMatchingEvents(t *testing.T) {
	_, events := traceRecordSession(t)

	rec := &traceRecorder{}
	var out bytes.Buffer
	opts := ReplayOpts{
		Speed:  0,
		Filter: func(e Event) bool { return e.Kind == KindCommandStart || e.Kind == KindCommandEnd },
	}
	if err := Replay(events, rec, opts, nil, &out); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(rec.events) != 2 {
		t.Fatalf("delivered %d events, want 2", len(rec.events))
	}
	if rec.events[0].Kind != KindCommandStart || rec.events[1].Kind != KindCommandEnd {
		t.Errorf("got kinds %s,%s; want command_start,command_end", rec.events[0].Kind, rec.events[1].Kind)
	}
	// 头部照旧描述整段会话，这样过滤后的视图永远不会被当成会话本身。
	if !strings.Contains(out.String(), "23 events") || !strings.Contains(out.String(), "showing 2 of 23") {
		t.Errorf("filtered header should summarise the whole trace and say what is hidden:\n%s", out.String())
	}
}

// traceLineFeeder 每次 Read 只给一行，并且计数，这样测试断言的就是 Replay 实际
// *吃掉*了多少输入，而不是喂给它多少。循环里现建的 bufio.Reader 会预读，把用户
// 接下来的按键一并吞掉；这个 bug 就是这么抓出来的。
type traceLineFeeder struct {
	lines []string
	n     int
}

func (f *traceLineFeeder) Read(p []byte) (int, error) {
	if f.n >= len(f.lines) {
		return 0, io.EOF
	}
	line := f.lines[f.n]
	if len(p) < len(line) {
		return 0, io.ErrShortBuffer // 只有这个辅助函数自己出 bug 才走得到
	}
	f.n++
	return copy(p, line), nil
}

func TestReplayStepConsumesOneLinePerEvent(t *testing.T) {
	_, events := traceRecordSession(t)
	events = events[:5]

	feeder := &traceLineFeeder{lines: []string{"\n", "\n", "\n", "\n", "\n", "\n"}} // 多备一行
	rec := &traceRecorder{}
	var out bytes.Buffer
	if err := Replay(events, rec, ReplayOpts{Step: true}, feeder, &out); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(rec.events) != 5 {
		t.Fatalf("delivered %d events, want 5", len(rec.events))
	}
	if feeder.n != 5 {
		t.Errorf("consumed %d lines for 5 events, want exactly 5", feeder.n)
	}
	for _, prompt := range []string{"[1/5 user_message] ", "[5/5 reasoning_delta] "} {
		if !strings.Contains(out.String(), prompt) {
			t.Errorf("step prompt %q missing from:\n%s", prompt, out.String())
		}
	}

	// 输入读完就是 Ctrl-D，而 Ctrl-D 会停下重放，不会一声不响地把剩下
	// 的自己放完。
	short := &traceLineFeeder{lines: []string{"\n", "\n"}}
	rec2 := &traceRecorder{}
	var out2 bytes.Buffer
	if err := Replay(events, rec2, ReplayOpts{Step: true}, short, &out2); err != nil {
		t.Fatalf("Replay after EOF: %v", err)
	}
	if len(rec2.events) != 2 {
		t.Errorf("delivered %d events after 2 lines of input, want 2", len(rec2.events))
	}
	if !strings.Contains(out2.String(), "[replay stopped after 2 of 5 events]") {
		t.Errorf("replay should say why it stopped:\n%s", out2.String())
	}

	// 而 "q" 直接退出，剩下的一行都不吃。
	quit := &traceLineFeeder{lines: []string{"\n", "q\n", "\n", "\n"}}
	rec3 := &traceRecorder{}
	if err := Replay(events, rec3, ReplayOpts{Step: true}, quit, io.Discard); err != nil {
		t.Fatalf("Replay with quit: %v", err)
	}
	if len(rec3.events) != 1 || quit.n != 2 {
		t.Errorf("q delivered %d events after %d lines, want 1 event after 2 lines", len(rec3.events), quit.n)
	}
}

func TestReplayClampsAbsurdGaps(t *testing.T) {
	// 这段会话里，用户在两个事件之间去吃了顿饭。照挂钟速度重放又不设上限，这里要
	// 睡 41 分钟。
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Seq: 1, T: base, Kind: KindTurnEnd},
		{Seq: 2, T: base.Add(41 * time.Minute), Kind: KindUserMessage, Text: "back"},
		{Seq: 3, T: base.Add(41*time.Minute + 30*time.Millisecond), Kind: KindTurnStart},
	}

	rec := &traceRecorder{}
	started := time.Now()
	// Speed 缩放的是*已经封顶*之后的间隙，所以速度调大，连最坏情况也一起缩掉：
	// 5s / 1000 = 5ms。
	if err := Replay(events, rec, ReplayOpts{Speed: 1000}, nil, io.Discard); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	elapsed := time.Since(started)

	if len(rec.events) != 3 {
		t.Fatalf("delivered %d events, want 3", len(rec.events))
	}
	if elapsed > 2*time.Second {
		t.Errorf("replay took %s — the %s gap cap is not being applied before Speed scales it",
			elapsed, maxReplayGap)
	}
}

func TestReplayRejectsNilSubscriber(t *testing.T) {
	if err := Replay(nil, nil, ReplayOpts{}, nil, io.Discard); err == nil {
		t.Error("Replay with no subscriber should fail loudly: there is nowhere for the events to go")
	}
}
