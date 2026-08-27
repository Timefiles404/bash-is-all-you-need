// Anthropic 协议适配器的测试。
//
// 以下每个流 fixture 都从 docs/wire-notes.md §B6 和 §B7 构建——
// 这个端点实际发送的字节，不是为了让解析器看起来正确而发明的字节。
// 这个区别正是为什么这些 fixture 在这里：从规范编写的 fixture
// 测试的是你对规范的理解，而这个端点在至少四个地方与规范矛盾，
// 这四个地方对这个文件很重要（流外的 ping、没有 [DONE]、
// message_start 中的使用报告错误，以及 `</think>` 标签泄漏到可见文本）。
//
// 某些 frame 的信封必须重建——线上笔记将某些事件记录为
// 裸 `delta` 对象或作为序列列表中的名称——上面的注释会说明。
// 内部的值总是逐字记录的。
//
// 没有网络、没有 API 密钥、没有 `-short` 跳过。
// 整个文件可以在飞机上运行。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// 适配器必须满足 provider.go 中的契约。在这里断言它意味着
// 签名偏差是测试构建中的编译错误，就在解释每个方法
// 对调用方的义务的测试旁边。
var _ Provider = (*anthropicProvider)(nil)

// ---------------------------------------------------------------------------
// Frame 构建器。
//
// 线上笔记将参数片段打印为裸值，deltas 打印为裸 `delta` 对象，
// 所以这些辅助函数围绕逐字值重建信封。json.Marshal 进行字符串转义，
// 这正是网关最初产生字节的方式——在 Go 字面值中手写 `\"`
// 是 fixture 停止匹配线上内容的方式。
// ---------------------------------------------------------------------------

func anthQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func anthArgsDelta(index int, partial string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, anthQuote(partial))
}

func anthTextDelta(index int, text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, anthQuote(text))
}

func anthThinkingDelta(index int, thinking string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`, index, anthQuote(thinking))
}

func anthToolStart(index int, id, name string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, index, anthQuote(id), anthQuote(name))
}

func anthBlockStop(index int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
}

func anthMessageDelta(stopReason string, input, output, cacheWrite, cacheRead int) string {
	return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s},"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}`,
		anthQuote(stopReason), input, output, cacheWrite, cacheRead)
}

// ---------------------------------------------------------------------------
// §B6 / §B7 fixtures。
// ---------------------------------------------------------------------------

const (
	// **逐字 §B6。注意里面没有**什么：没有 stop_reason、
	// 没有缓存计数器，以及一个 input_tokens 数字，
	// 这个数字与下一个使用报告矛盾。
	b6MessageStart = `{"type":"message_start","message":{"id":"msg_e3f9307e-2dc9-41f0-a70e-cca934593aa0","type":"message","role":"assistant","model":"qwen3.7-plus","content":[],"usage":{"input_tokens":56,"output_tokens":0}}}`

	// **逐字** §B6——tool_use 公告。`input` 是一个空对象，
	// id/name 出现在这里，也不在其他 frame 中。
	b6ToolStart0 = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ff07c814f3f34014aa526469","name":"bash","input":{}}}`

	// **逐字** §B6——使用报告与 message_start 关于同一请求不一致，
	// 也是唯一携带 stop_reason 或缓存计数器的 frame。
	b6MessageDelta = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":291,"output_tokens":63,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

	// **逐字** §B6——尾部 ping，携带额外键 `cost`。
	b6PingWithCost = `{"type":"ping","cost":"0"}`

	// **重建**形状：§B6 在事件序列中列出 `ping` 和 `message_stop`，
	// 但仅为尾部 ping 打印 body。
	b6Ping        = `{"type":"ping"}`
	b6MessageStop = `{"type":"message_stop"}`

	// **逐字** §B7——一个思考块、它的（总是空的）签名 delta，
	// 以及在它关闭后在**下一个**索引处打开的文本块。
	b7ThinkingStart   = `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`
	b7ThinkingDelta   = `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let"}}`
	b7SignatureDelta  = `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`
	b7BlockStop0      = `{"type":"content_block_stop","index":0}`
	b7TextStart1      = `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`
	b7TextDeltaFirst  = `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"To calculate"}}`
	b7TextDeltaSecond = `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" 17 ×"}}`
)

// b6ArgFragments 是 §B6 记录的六个 `partial_json` 值，按顺序。
//
// 第一个是**空字符串**，片段三在路径中间结束（`/srv`），
// 片段四继续它（`/app`）。在任何时刻，片段都不是可解析的 JSON，
// 这就是为什么适配器连接原始字节，永远不检查它们。
var b6ArgFragments = []string{
	``,
	`{"command": "ls`,
	` -la /srv`,
	`/app`,
	`"`,
	`}`,
}

// b6WantArgs 是 §B6 说这些片段连接成什么。
const b6WantArgs = `{"command": "ls -la /srv/app"}`

// b6LeakText 是网关的 `</think>` 泄漏，**逐字** §B6（以及 §A3b，
// 它在非流式响应中捕获了相同的字符串）。一个换行符、裸闭合标签、
// 两个换行符——整个用户可见的文本块，不包含模型输出。
const b6LeakText = "\n</think>\n\n"

// b6FullStream 是 §B6 的两个工具调用流端到端：
//
//	ping message_start
//	content_block_start content_block_delta x6 content_block_stop  (索引 0, tool_use)
//	content_block_start content_block_delta   content_block_stop   (索引 1, 文本)
//	content_block_start content_block_delta x6 content_block_stop  (索引 2, tool_use)
//	message_delta message_stop ping
//
// 索引 2 块是**构造的**——§B6 记录其位置、类型和 delta
// 计数，但没有记录它的 id 或参数——它故意被赋予不同的命令，
// 所以一个在块之间共享一个缓冲区的解析器会产生可见的垃圾，
// 而不是微妙的混淆。
func b6FullStream() []string {
	frames := []string{
		b6Ping, // message_start **之前**。规范说这不可能发生。
		b6MessageStart,
		b6ToolStart0,
	}
	for _, frag := range b6ArgFragments {
		frames = append(frames, anthArgsDelta(0, frag))
	}
	frames = append(frames,
		anthBlockStop(0),

		// 索引 1：`</think>` 泄漏，作为它自己的文本内容块。
		b7TextStart1,
		anthTextDelta(1, b6LeakText),
		anthBlockStop(1),

		// 索引 2：第二个工具调用。
		anthToolStart(2, "toolu_5ae0ccdc34f44d30a2217c5e", "bash"),
	)
	for _, frag := range []string{``, `{"command": "wc`, ` -l /srv`, `/app/main`, `.go"`, `}`} {
		frames = append(frames, anthArgsDelta(2, frag))
	}
	frames = append(frames,
		anthBlockStop(2),
		b6MessageDelta,
		b6MessageStop,
		b6PingWithCost, // message_stop **之后**，携带 `cost`。
	)
	return frames
}

// ---------------------------------------------------------------------------
// 宿主。
// ---------------------------------------------------------------------------

// anthSSE 以 §B6 描述这个端点呈现它们的方式呈现 frames：
// `event: <name>`、`data: <payload>`、空行。event 名称取自
// payload 自己的 `type`，这就是网关如何构建它的方式。
//
// body 在最后一个 frame 处结束**没有**尾部空行，因为这是
// 这个流的实际结束方式：没有 `[DONE]`、没有终止符、
// 只是连接关闭。一个只在空行上分发的读取器会无声地
// 丢弃每个响应的最后一个 frame——在这个协议上，最后的 frame
// 是 message_delta 和成本 ping。
func anthSSE(frames ...string) io.Reader {
	var b strings.Builder
	for i, f := range frames {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n", anthEventName(f), f)
		if i < len(frames)-1 {
			b.WriteString("\n")
		}
	}
	return strings.NewReader(b.String())
}

func anthEventName(payload string) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return "unknown"
	}
	return probe.Type
}

// anthRecorder 保留每个事件，这是展示为什么 agent 核心
// 发出事件而不是打印的最便宜方式：这些测试对事件序列断言，
// 从不接触 stdout。
type anthRecorder struct{ events []Event }

func (r *anthRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func (r *anthRecorder) kinds() []Kind {
	out := make([]Kind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *anthRecorder) count(k Kind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (r *anthRecorder) first(k Kind) (Event, bool) {
	for _, e := range r.events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

func (r *anthRecorder) textsOf(k Kind) []string {
	var out []string
	for _, e := range r.events {
		if e.Kind == k {
			out = append(out, e.Text)
		}
	}
	return out
}

const anthTestTurn = 7

func anthProvider() *anthropicProvider {
	return newAnthropicProvider("https://opencode.ai/zen/go/v1", "sk-test", "qwen3.7-plus")
}

// anthParse 在 frame 列表上运行适配器，交回测试可能想
// 断言的一切。
func anthParse(t *testing.T, frames []string) (*CallResult, *anthRecorder, error) {
	t.Helper()
	rec := &anthRecorder{}
	res, err := anthProvider().ParseStream(anthSSE(frames...), NewBus(rec), anthTestTurn, time.Now())
	if res == nil {
		t.Fatal("ParseStream returned a nil result; it must always return what it assembled")
	}
	return res, rec, err
}

// ---------------------------------------------------------------------------
// 完整的 §B6 流。
// ---------------------------------------------------------------------------

func TestAnthropicFullB6Stream(t *testing.T) {
	res, rec, err := anthParse(t, b6FullStream())
	if err != nil {
		t.Fatalf("the recorded stream must parse cleanly: %v", err)
	}

	// message_start 前的一个 ping 和 message_stop 后的一个 ping，
	// 都不是一个 token、一条消息，也不是停止读取的理由。
	if got := rec.count(KindFirstToken); got != 1 {
		t.Errorf("KindFirstToken emitted %d times, want exactly 1 (a ping is not a token)", got)
	}

	if len(res.Calls) != 2 {
		t.Fatalf("got %d tool calls, want 2 (indices 0 and 2, with a text block between them)", len(res.Calls))
	}
	if res.Calls[0].ID != "toolu_ff07c814f3f34014aa526469" || res.Calls[0].Name != "bash" {
		t.Errorf("first call id/name = %q/%q, want the values from content_block_start", res.Calls[0].ID, res.Calls[0].Name)
	}
	if res.Calls[0].Args != b6WantArgs {
		t.Errorf("first call args = %q, want %q", res.Calls[0].Args, b6WantArgs)
	}
	if res.Calls[0].Kind != BlockToolCall {
		t.Errorf("call block kind = %q, want %q", res.Calls[0].Kind, BlockToolCall)
	}
	if want := `{"command": "wc -l /srv/app/main.go"}`; res.Calls[1].Args != want {
		t.Errorf("second call args = %q, want %q — fragments leaked between content blocks", res.Calls[1].Args, want)
	}

	// 这个流中唯一的文本块是 `</think>` 泄漏，所以根本
	// 没有可见文本。
	if res.Text != "" {
		t.Errorf("Text = %q, want empty: the only text block was gateway residue", res.Text)
	}
	if res.Thinking != "" {
		t.Errorf("Thinking = %q, want empty: this stream has no thinking block", res.Thinking)
	}

	if res.RawStop != "tool_use" {
		t.Errorf("RawStop = %q, want the literal wire string %q", res.RawStop, "tool_use")
	}
	if res.Stop != StopToolUse {
		t.Errorf("Stop = %q, want %q", res.Stop, StopToolUse)
	}

	want := Usage{Input: 291, Output: 63}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v (message_delta, not message_start)", res.Usage, want)
	}

	end, ok := rec.first(KindResponseEnd)
	if !ok {
		t.Fatal("no KindResponseEnd on a stream that ended cleanly")
	}
	if end.FinishReason != "tool_use" {
		t.Errorf("KindResponseEnd.FinishReason = %q, want the raw wire string", end.FinishReason)
	}

	// 每个事件都携带回合，所以多回合会话的 trace
	// 可以按回合重新切分开。
	for _, e := range rec.events {
		if e.Turn != anthTestTurn {
			t.Fatalf("event %s carries turn %d, want %d", e.Kind, e.Turn, anthTestTurn)
		}
	}
}

// ---------------------------------------------------------------------------
// 工具参数。
// ---------------------------------------------------------------------------

func TestAnthropicToolArgsReassembly(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   []Block
	}{
		{
			// 观察到的片段，逐字 §B6，包括空的第一个
			// 以及在中间分割路径的两个。
			name: "observed fragments reassemble",
			frames: func() []string {
				f := []string{b6Ping, b6MessageStart, b6ToolStart0}
				for _, frag := range b6ArgFragments {
					f = append(f, anthArgsDelta(0, frag))
				}
				return append(f, anthBlockStop(0), b6MessageDelta, b6MessageStop, b6PingWithCost)
			}(),
			want: []Block{{
				Kind: BlockToolCall,
				ID:   "toolu_ff07c814f3f34014aa526469",
				Name: "bash",
				Args: b6WantArgs,
			}},
		},
		{
			// **构造的**。两个块在任何一个关闭前打开，
			// 它们的片段交错，更高索引的块**先**打开——
			// 所以一个累积到一个缓冲区中，或按到达顺序
			// 返回调用的实现，每次运行都失败，而不是一半时间。
			name: "interleaved blocks stay separate and come back index-ordered",
			frames: []string{
				b6Ping, b6MessageStart,
				anthToolStart(2, "toolu_second", "bash"),
				anthToolStart(0, "toolu_first", "bash"),
				anthArgsDelta(2, `{"command": "echo `),
				anthArgsDelta(0, `{"command": "ls`),
				anthArgsDelta(2, `two"}`),
				anthArgsDelta(0, ` -la"}`),
				anthBlockStop(2), anthBlockStop(0),
				b6MessageDelta, b6MessageStop,
			},
			want: []Block{
				{Kind: BlockToolCall, ID: "toolu_first", Name: "bash", Args: `{"command": "ls -la"}`},
				{Kind: BlockToolCall, ID: "toolu_second", Name: "bash", Args: `{"command": "echo two"}`},
			},
		},
		{
			// 一个工具调用，其参数从未到达。id 和 name
			// 仍然必须幸存：没有 id 就没有 tool_use_id 来回答，
			// 回合根本无法关闭。
			name: "announced but empty stays announced",
			frames: []string{
				b6Ping, b6MessageStart,
				anthToolStart(0, "toolu_empty", "bash"),
				anthArgsDelta(0, ``),
				anthBlockStop(0),
				b6MessageDelta, b6MessageStop,
			},
			want: []Block{{Kind: BlockToolCall, ID: "toolu_empty", Name: "bash", Args: ""}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, rec, err := anthParse(t, tc.frames)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(res.Calls, tc.want) {
				t.Errorf("calls =\n  %+v\nwant\n  %+v", res.Calls, tc.want)
			}

			// 空的第一个片段不应该变成 trace 行：一个
			// 不携带任何字符的参数 delta 在请求检查器中是噪音，
			// 在下游的每个渲染器中也是。
			for _, txt := range rec.textsOf(KindToolArgsDelta) {
				if txt == "" {
					t.Error("emitted a KindToolArgsDelta with empty text; the first observed fragment is \"\" and carries nothing")
				}
			}

			// 每个公告都必须为其调用命名。
			for _, e := range rec.events {
				if e.Kind == KindToolCallStart && (e.ToolID == "" || e.ToolName == "") {
					t.Errorf("KindToolCallStart missing id or name: %+v", e)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 使用。这个文件中价值最高的测试。
// ---------------------------------------------------------------------------

func TestAnthropicUsageComesFromMessageDeltaNotMessageStart(t *testing.T) {
	cases := []struct {
		name       string
		frames     []string
		want       Usage
		wantPrompt int
		wantRaw    string
		wantStop   StopReason
	}{
		{
			// §B6，不一致的对：message_start 说 input_tokens 56，
			// message_delta 说 291，对于同一请求。带有相同
			// prompt 的非流式调用同意了 291。如果这个断言
			// 曾经读取 56，适配器就在信任规范称为权威的 frame，
			// 而这个端点弄错了。
			name:       "message_start says 56, message_delta says 291, 291 wins",
			frames:     []string{b6Ping, b6MessageStart, b6ToolStart0, anthArgsDelta(0, `{}`), anthBlockStop(0), b6MessageDelta, b6MessageStop, b6PingWithCost},
			want:       Usage{Input: 291, Output: 63},
			wantPrompt: 291,
			wantRaw:    "tool_use",
			wantStop:   StopToolUse,
		},
		{
			// 一个热缓存调用，在线验证：input=18、cache_creation=0、
			// cache_read=17967。这个协议的 input_tokens **仅**是
			// 未缓存的剩余部分，所以它直接映射，上下文大小是
			// **总和**——17,985——一个单个线上字段不会报告的数字。
			// （§C8 在较小的手册上测量了相同的形状：input 18、
			// cache_read 9,775。）
			//
			// 一个复制 OpenAI 方向并从 input 减去 cache_read
			// 的适配器会报告 -17,949。
			name:       "warm cache: input is only the uncached remainder",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "ACK"), anthBlockStop(1), anthMessageDelta("end_turn", 18, 249, 0, 17967), b6MessageStop},
			want:       Usage{Input: 18, CacheRead: 17967, Output: 249},
			wantPrompt: 17985,
			wantRaw:    "end_turn",
			wantStop:   StopEndTurn,
		},
		{
			// 针对冷缓存的第一个调用写入前缀。CacheWrite
			// 是它自己的字段，因为它按约 1.25 倍计费，而不是 0.1 倍。
			name:       "cold cache: creation tokens land in CacheWrite",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "ACK"), anthBlockStop(1), anthMessageDelta("end_turn", 18, 249, 9775, 0), b6MessageStop},
			want:       Usage{Input: 18, CacheWrite: 9775, Output: 249},
			wantPrompt: 9793,
			wantRaw:    "end_turn",
			wantStop:   StopEndTurn,
		},
		{
			// 流在 message_delta 前死亡。**没有**回退到
			// message_start 的数字：缺少的数字可以看到并追踪，
			// 一个看似正确的会出现在成本仪表板上。
			// 并且没有 stop_reason，回合是 StopUnknown——
			// 不是"可能没问题"。
			name:       "no message_delta: no usage, and an unknown stop",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "half a sen")},
			want:       Usage{},
			wantPrompt: 0,
			wantRaw:    "",
			wantStop:   StopUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, rec, err := anthParse(t, tc.frames)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Usage != tc.want {
				t.Errorf("Usage = %+v, want %+v", res.Usage, tc.want)
			}
			if got := res.Usage.Prompt(); got != tc.wantPrompt {
				t.Errorf("Usage.Prompt() = %d, want %d — the context size is Input+CacheWrite+CacheRead", got, tc.wantPrompt)
			}
			if res.RawStop != tc.wantRaw {
				t.Errorf("RawStop = %q, want %q", res.RawStop, tc.wantRaw)
			}
			if res.Stop != tc.wantStop {
				t.Errorf("Stop = %q, want %q", res.Stop, tc.wantStop)
			}

			// KindUsage 事件必须携带相同的规范化数字，
			// 而且什么都不报告时根本不能发出。
			ev, ok := rec.first(KindUsage)
			if tc.want == (Usage{}) {
				if ok {
					t.Errorf("emitted KindUsage %+v when the stream reported none", ev.Usage)
				}
				return
			}
			if !ok {
				t.Fatal("no KindUsage event")
			}
			if ev.Usage == nil || *ev.Usage != tc.want {
				t.Errorf("KindUsage carried %+v, want %+v", ev.Usage, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 思考。
// ---------------------------------------------------------------------------

func TestAnthropicThinkingAndTextStaySeparate(t *testing.T) {
	// §B7 逐字：索引 0 处有一个思考块及其自己的 delta 类型，
	// 在索引 1 处的文本块打开之前关闭。假设索引 0 是文本的代码
	// 会将模型的私有推理呈现给用户。
	frames := []string{
		b6Ping, b6MessageStart,
		b7ThinkingStart,
		b7ThinkingDelta,
		anthThinkingDelta(0, " me multiply that."),
		b7SignatureDelta,
		b7BlockStop0,
		b7TextStart1,
		b7TextDeltaFirst,
		b7TextDeltaSecond,
		anthBlockStop(1),
		anthMessageDelta("end_turn", 291, 63, 0, 0),
		b6MessageStop, b6PingWithCost,
	}

	res, rec, err := anthParse(t, frames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := "Let me multiply that."; res.Thinking != want {
		t.Errorf("Thinking = %q, want %q", res.Thinking, want)
	}
	if want := "To calculate 17 ×"; res.Text != want {
		t.Errorf("Text = %q, want %q — thinking must never reach the visible text", res.Text, want)
	}

	if got := rec.count(KindReasoningDelta); got != 2 {
		t.Errorf("KindReasoningDelta count = %d, want 2", got)
	}
	if got := rec.count(KindTextDelta); got != 2 {
		t.Errorf("KindTextDelta count = %d, want 2", got)
	}

	// signature_delta 由这个网关发出，总是携带 ""（§B7）。
	// 它不能产生任何类型的事件：它既不是文本也不是通知，
	// 也没有签名要来回往返。
	for _, txt := range append(rec.textsOf(KindTextDelta), rec.textsOf(KindReasoningDelta)...) {
		if txt == "" {
			t.Error("an empty delta reached the bus; signature_delta must be ignored, not forwarded")
		}
	}
	if got := rec.count(KindNotice); got != 0 {
		t.Errorf("got %d notices, want 0 — signature_delta is expected, not unknown", got)
	}

	// 第一个 token 是第一个**思考** token，
	// 不是第一个可见字符。在推理模型上，那是诚实的测量：
	// 它是模型首先生产的东西。
	if got := rec.count(KindFirstToken); got != 1 {
		t.Fatalf("KindFirstToken count = %d, want 1", got)
	}
	if rec.kinds()[0] != KindFirstToken || rec.kinds()[1] != KindReasoningDelta {
		t.Errorf("first two events were %v, want first_token then reasoning_delta", rec.kinds()[:2])
	}
}

// ---------------------------------------------------------------------------
// `</think>` 泄漏。§B6 偏差 4。
// ---------------------------------------------------------------------------

// **受测试的决定**：残基从用户可见文本中丢弃，
// 并报告为通知。不呈现（它不是模型的输出），
// 不无声吞咽（trace 必须保留网关泄漏其自己的宿主标记的证据）。
func TestAnthropicThinkTagLeak(t *testing.T) {
	cases := []struct {
		name        string
		deltas      []string
		wantText    string
		wantNotices int
	}{
		{
			// **逐字** §B6 / §A3b：整个文本内容块，
			// 其整个内容是泄漏的闭合标签。
			name:        "the observed leak is dropped and reported",
			deltas:      []string{b6LeakText},
			wantText:    "",
			wantNotices: 1,
		},
		{
			name:        "the leak in front of real text does not eat the text",
			deltas:      []string{b6LeakText, "The answer is 391."},
			wantText:    "The answer is 391.",
			wantNotices: 1,
		},
		{
			// 尚未观察到打开标签泄漏，但这是相同的失败，
			// 相同的修复。
			name:        "a bare opening tag is residue too",
			deltas:      []string{"<think>", "hello"},
			wantText:    "hello",
			wantNotices: 1,
		},
		{
			// **这个规则存在以避免的假阳性**。一个模型解释思考标签——
			// 这是要求 coding Agent 做的完全合理的事情——
			// 必须完全原封不动地通过。悄悄破坏真实输出来整理厂商垃圾
			// 是两个失败中更糟糕的一个，所以规则是
			// "整个 delta 是标签"，而不是"delta 包含它"。
			name:        "a tag inside a sentence is the model talking, not residue",
			deltas:      []string{"Close the block with </think> and continue."},
			wantText:    "Close the block with </think> and continue.",
			wantNotices: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := []string{b6Ping, b6MessageStart, b7TextStart1}
			for _, d := range tc.deltas {
				frames = append(frames, anthTextDelta(1, d))
			}
			frames = append(frames, anthBlockStop(1), anthMessageDelta("end_turn", 291, 63, 0, 0), b6MessageStop, b6PingWithCost)

			res, rec, err := anthParse(t, frames)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", res.Text, tc.wantText)
			}
			if got := rec.count(KindNotice); got != tc.wantNotices {
				t.Errorf("notice count = %d, want %d (notices: %q)", got, tc.wantNotices, rec.textsOf(KindNotice))
			}
			// 丢弃意味着丢弃：任何渲染器都绝不能看到这个标签。
			for _, txt := range rec.textsOf(KindTextDelta) {
				if strings.TrimSpace(txt) == "</think>" || strings.TrimSpace(txt) == "<think>" {
					t.Errorf("residue %q was forwarded as visible text", txt)
				}
			}
			if tc.wantNotices > 0 {
				n, _ := rec.first(KindNotice)
				if !strings.Contains(n.Text, "harness residue") {
					t.Errorf("notice text = %q, want it to say what was dropped and why", n.Text)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 事件序列。
// ---------------------------------------------------------------------------

func TestAnthropicEventSequence(t *testing.T) {
	// 一个流，其中包含每种 payload 类型，所以下面的断言是
	// 这个适配器欠渲染器的完整契约：相同的类型、
	// 相同的顺序、相同的含义，来自一个不共享其任何词汇的线上。
	frames := []string{
		b6Ping,         // 不是一个 token
		b6MessageStart, // 不是 token，它的使用是谎言
		b7ThinkingStart,
		b7ThinkingDelta,
		b7SignatureDelta, // 总是空的；什么都不产生
		b7BlockStop0,
		b7TextStart1,
		b7TextDeltaFirst,
		anthBlockStop(1),
		anthToolStart(2, "toolu_x", "bash"),
		anthArgsDelta(2, ``), // 空的第一个片段什么都不产生
		anthArgsDelta(2, `{"command": "ls"}`),
		anthBlockStop(2),
		b6MessageDelta,
		b6MessageStop,
		b6PingWithCost, // message_stop 之后，仍然不是 token
	}

	_, rec, err := anthParse(t, frames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []Kind{
		KindFirstToken,     // 由思考 delta 发起，而不是由 ping
		KindReasoningDelta, //
		KindTextDelta,      //
		KindToolCallStart,  // id + name，一次
		KindToolArgsDelta,  // 一个片段，跳过空的那个
		KindUsage,          // message_delta
		KindResponseEnd,    // 最后
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("event kinds =\n  %v\nwant\n  %v", got, want)
	}
	if got := rec.count(KindFirstToken); got != 1 {
		t.Errorf("KindFirstToken count = %d, want exactly 1", got)
	}
	if ft, _ := rec.first(KindFirstToken); ft.Millis < 0 {
		t.Errorf("KindFirstToken.Millis = %d, want a TTFT measurement", ft.Millis)
	}
}

// ---------------------------------------------------------------------------
// 帧边界情况和可生存的损坏。
// ---------------------------------------------------------------------------

func TestAnthropicStreamTolerance(t *testing.T) {
	t.Run("pings anywhere, and no [DONE] anywhere", func(t *testing.T) {
		frames := []string{
			b6Ping, b6Ping, // message_start 前
			b6MessageStart,
			b7TextStart1,
			b6Ping, // 一个普通的中流 keepalive
			anthTextDelta(1, "hello"),
			anthBlockStop(1),
			anthMessageDelta("end_turn", 291, 63, 0, 0),
			b6MessageStop,
			b6PingWithCost, b6Ping, // message_stop 后
		}
		res, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text != "hello" {
			t.Errorf("Text = %q, want %q", res.Text, "hello")
		}
		if got := rec.count(KindFirstToken); got != 1 {
			t.Errorf("KindFirstToken count = %d, want 1 — five pings are not five tokens", got)
		}
		if got := rec.count(KindNotice); got != 0 {
			t.Errorf("pings produced %d notices, want 0: %q", got, rec.textsOf(KindNotice))
		}
		// 尾部 ping 在 message_stop **后**到达，所以一个
		// 在那里返回的解析器永远不会看到 `cost`，
		// 会在套接字中留下字节，停止连接返回到 keep-alive 池。
		if got := rec.count(KindUsage); got != 1 {
			t.Errorf("KindUsage count = %d, want 1", got)
		}
	})

	t.Run("a non-zero cost on the trailing ping is reported", func(t *testing.T) {
		// §C10 只见过字符串"0"。一个真实数字会是这个端点
		// 发出的第一个成本信号，所以它进入 trace。
		frames := []string{
			b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "hi"), anthBlockStop(1),
			anthMessageDelta("end_turn", 1, 1, 0, 0), b6MessageStop,
			`{"type":"ping","cost":"0.0042"}`,
		}
		_, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		n, ok := rec.first(KindNotice)
		if !ok || !strings.Contains(n.Text, "0.0042") {
			t.Errorf("notices = %q, want one reporting the cost", rec.textsOf(KindNotice))
		}
	})

	t.Run("a numeric cost does not break the frame", func(t *testing.T) {
		// `cost` 键被类型化为 json.RawMessage，正是为了
		// JSON 类型的改变不能把整个 frame 带下来。
		frames := []string{
			b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "hi"), anthBlockStop(1),
			anthMessageDelta("end_turn", 1, 1, 0, 0), b6MessageStop,
			`{"type":"ping","cost":0}`,
		}
		_, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, n := range rec.textsOf(KindNotice) {
			if strings.Contains(n, "not JSON") {
				t.Errorf("a numeric cost broke the frame decode: %q", n)
			}
		}
	})

	t.Run("a malformed frame is survivable, not fatal", func(t *testing.T) {
		frames := []string{
			b6Ping, b6MessageStart,
			b6ToolStart0,
			anthArgsDelta(0, `{"command": "ls -la /srv/app"}`),
			`{"type":"content_block_delta",` + `truncated`,
			anthBlockStop(0),
			b6MessageDelta, b6MessageStop,
		}
		res, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("one bad frame must not fail the turn: %v", err)
		}
		if len(res.Calls) != 1 || res.Calls[0].Args != b6WantArgs {
			t.Errorf("the complete tool call was lost to a neighbouring bad frame: %+v", res.Calls)
		}
		n, ok := rec.first(KindNotice)
		if !ok || !strings.Contains(n.Text, "not JSON") {
			t.Errorf("notices = %q, want one naming the unparseable frame", rec.textsOf(KindNotice))
		}
	})

	t.Run("an unknown event type is noticed, not fatal", func(t *testing.T) {
		frames := []string{
			b6Ping, b6MessageStart,
			`{"type":"message_thermal_throttle","index":0}`,
			b7TextStart1, anthTextDelta(1, "hi"), anthBlockStop(1),
			anthMessageDelta("end_turn", 1, 1, 0, 0), b6MessageStop,
		}
		res, rec, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text != "hi" {
			t.Errorf("Text = %q, want %q", res.Text, "hi")
		}
		n, ok := rec.first(KindNotice)
		if !ok || !strings.Contains(n.Text, "message_thermal_throttle") {
			t.Errorf("notices = %q, want one naming the unknown event", rec.textsOf(KindNotice))
		}
	})

	t.Run("an unknown stop reason is not 'probably fine'", func(t *testing.T) {
		frames := []string{
			b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "..."), anthBlockStop(1),
			anthMessageDelta("model_context_window_exceeded", 291, 63, 0, 0), b6MessageStop,
		}
		res, _, err := anthParse(t, frames)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RawStop != "model_context_window_exceeded" {
			t.Errorf("RawStop = %q, want the literal string preserved for the trace", res.RawStop)
		}
		if res.Stop != StopUnknown {
			t.Errorf("Stop = %q, want %q — mapping unknown reasons to end_turn is how a refusal becomes a shrug", res.Stop, StopUnknown)
		}
	})

	t.Run("a nil bus is tolerated", func(t *testing.T) {
		res, err := anthProvider().ParseStream(anthSSE(b6FullStream()...), nil, 0, time.Now())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(res.Calls) != 2 {
			t.Errorf("got %d calls with a nil bus, want 2 — the parser must work as a pure function", len(res.Calls))
		}
	})
}

// ---------------------------------------------------------------------------
// 中流故障。
// ---------------------------------------------------------------------------

func TestAnthropicMidStreamFailureKeepsPartialAndSkipsResponseEnd(t *testing.T) {
	t.Run("the connection dies", func(t *testing.T) {
		// 直到一个完整工具调用的所有东西到达，然后套接字
		// 破裂。调用方需要部分结果来区分"在完整工具调用后死亡"
		// 和"什么都没产生"。
		var b strings.Builder
		for _, f := range []string{b6Ping, b6MessageStart, b6ToolStart0, anthArgsDelta(0, b6WantArgs), anthBlockStop(0)} {
			fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", anthEventName(f), f)
		}
		body := io.MultiReader(strings.NewReader(b.String()), iotest.ErrReader(errors.New("connection reset by peer")))

		rec := &anthRecorder{}
		res, err := anthProvider().ParseStream(body, NewBus(rec), anthTestTurn, time.Now())
		if err == nil {
			t.Fatal("a broken connection must be reported as an error")
		}
		if res == nil || len(res.Calls) != 1 || res.Calls[0].Args != b6WantArgs {
			t.Fatalf("the partial result was lost: %+v", res)
		}
		if rec.count(KindResponseEnd) != 0 {
			t.Error("emitted KindResponseEnd for a response that never ended — the trace must never record a clean ending that did not happen")
		}
		if res.RawStop != "" || res.Stop != StopUnknown {
			t.Errorf("RawStop/Stop = %q/%q, want empty and %q", res.RawStop, res.Stop, StopUnknown)
		}
	})

	t.Run("an error event mid-stream", func(t *testing.T) {
		// **构造的**：§D11 的错误都作为 HTTP 状态在流打开前到达，
		// 所以这个形状在这个网关上未被观察到。规范在正文中间流
		// overloaded_error，而一个在中间死亡的流不应该被记录为
		// 一个完成的流。
		frames := []string{
			b6Ping, b6MessageStart, b6ToolStart0,
			anthArgsDelta(0, b6WantArgs), anthBlockStop(0),
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		}
		res, rec, err := anthParse(t, frames)
		if err == nil {
			t.Fatal("an error event must be reported as an error")
		}
		if !strings.Contains(err.Error(), "overloaded_error") {
			t.Errorf("error = %v, want it to carry the provider's own type", err)
		}
		if len(res.Calls) != 1 {
			t.Errorf("the partial result was lost: %+v", res)
		}
		if rec.count(KindResponseEnd) != 0 {
			t.Error("emitted KindResponseEnd after a stream error")
		}
	})
}

// ---------------------------------------------------------------------------
// BuildRequest。
// ---------------------------------------------------------------------------

// anthWireBody 镜像 BuildRequest 应该已经产生的东西。
// 它从 anthropic.go 中的结构分开写出来是有意的：
// 一个用编码它的相同类型解码的测试不能捕获错误的 json 标签，
// 因为双方共享这个错误。
type anthWireBody struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	System    string `json:"system"`
	Stream    bool   `json:"stream"`
	Messages  []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   string          `json:"content"`
		} `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema map[string]any  `json:"input_schema"`
		Function    json.RawMessage `json:"function"` // 绝不能出现
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"tools"`
}

func anthDecodeBody(t *testing.T, body []byte) anthWireBody {
	t.Helper()
	var got anthWireBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the request body is not valid JSON: %v\n%s", err, body)
	}
	return got
}

func anthBashTool() Tool {
	return Tool{
		Name:        "bash",
		Description: "Execute a bash command.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	}
}

func TestAnthropicBuildRequestEnvelope(t *testing.T) {
	p := anthProvider()
	req, body, err := p.BuildRequest("you are a shell", []Msg{TextMsg(RoleUser, "hello")}, []Tool{anthBashTool()}, 700)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	if p.Protocol() != "anthropic" {
		t.Errorf("Protocol() = %q, want %q", p.Protocol(), "anthropic")
	}
	if p.Model() != "qwen3.7-plus" {
		t.Errorf("Model() = %q", p.Model())
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if want := "https://opencode.ai/zen/go/v1/messages"; req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL.String(), want)
	}

	// x-api-key，**不是** Authorization: Bearer。
	// 在这里发送其他协议的 header 会产生"Missing API key."，
	// 这看起来像一个配置问题。
	for _, h := range []struct{ key, want string }{
		{"x-api-key", "sk-test"},
		{"anthropic-version", "2023-06-01"},
		{"content-type", "application/json"},
		{"accept", "text/event-stream"},
	} {
		if got := req.Header.Get(h.key); got != h.want {
			t.Errorf("header %s = %q, want %q", h.key, got, h.want)
		}
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("an Authorization header leaked in from the other protocol")
	}

	// 返回的字节必须是线上的字节：调用方以 KindRequest 发出它们，
	// 而请求检查器显示发送以外的东西比没有检查器更糟。
	sent, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reading the request body: %v", err)
	}
	if string(sent) != string(body) {
		t.Errorf("the returned body and the request body differ:\n returned %s\n sent     %s", body, sent)
	}

	got := anthDecodeBody(t, body)
	if !got.Stream {
		t.Error(`"stream": true is missing; this adapter only knows how to read an SSE body`)
	}
	if got.Model != "qwen3.7-plus" {
		t.Errorf("model = %q", got.Model)
	}
	if got.MaxTokens != 700 {
		t.Errorf("max_tokens = %d, want 700", got.MaxTokens)
	}

	// **不对称**。系统提示词在这里是顶级字段；OpenAI 适配器
	// 让它成为 messages[0]。两个形状都不能是中立的那个。
	if got.System != "you are a shell" {
		t.Errorf("system = %q, want it at the top level", got.System)
	}
	for _, m := range got.Messages {
		if m.Role == "system" {
			t.Error("the system prompt was sent as a message; on this protocol it is a top-level field")
		}
	}

	// 工具是扁平的：{name、description、input_schema}。
	// 没有 `function` 嵌套，没有 `parameters`。
	if len(got.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(got.Tools))
	}
	tool := got.Tools[0]
	if tool.Name != "bash" || tool.Description != "Execute a bash command." {
		t.Errorf("tool = %+v", tool)
	}
	if tool.InputSchema == nil {
		t.Error("input_schema is missing — this protocol does not call it `parameters`")
	}
	if tool.InputSchema["type"] != "object" {
		t.Errorf("input_schema.type = %v, want object", tool.InputSchema["type"])
	}
	if len(tool.Function) != 0 || len(tool.Parameters) != 0 {
		t.Errorf("the OpenAI tool envelope leaked in: function=%s parameters=%s", tool.Function, tool.Parameters)
	}

	// max_tokens 在这里是强制的，§D11 展示了省略它的代价：
	// 一个 400，其 body 是 `{"model":"qwen3.7-plus"}`，
	// 根本没有错误信封。非正预算获得默认值而不是谜团。
	_, defaulted, err := p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 0)
	if err != nil {
		t.Fatalf("BuildRequest with no budget: %v", err)
	}
	if d := anthDecodeBody(t, defaulted); d.MaxTokens != anthropicDefaultMaxTokens {
		t.Errorf("max_tokens = %d with a zero budget, want the default %d", d.MaxTokens, anthropicDefaultMaxTokens)
	}
	if d := anthDecodeBody(t, defaulted); d.System != "" || len(d.Tools) != 0 {
		t.Error("an empty system prompt or tool list must be omitted, not sent empty: a different prefix is a cache miss")
	}
}

// TestAnthropicBuildRequestCollapsesToolResults 是这个文件存在的
// 原因。三个工具结果变成**一个**用户消息，有三个
// tool_result 块，不管调用方如何安排它们。OpenAI 适配器每个
// 结果发出一条消息；把这个弄反是 anthropic.go 中最可能的 bug。
func TestAnthropicBuildRequestCollapsesToolResults(t *testing.T) {
	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "Running three checks."},
		{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: `{"command":"ls /a"}`},
		{Kind: BlockToolCall, ID: "toolu_2", Name: "bash", Args: `{"command":"ls /b"}`},
		{Kind: BlockToolCall, ID: "toolu_3", Name: "bash", Args: `{"command":"ls /c"}`},
	}}

	cases := []struct {
		name string
		msgs []Msg
	}{
		{
			// 一个为每个已完成的工具追加一条消息的循环，会自然地
			// 构建出这种安排——也就是 OpenAI 协议想要的那种。
			name: "one neutral message per result",
			msgs: []Msg{
				TextMsg(RoleUser, "check three paths"),
				assistant,
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", "a\n")}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_2", "b\n")}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_3", "c\n")}},
			},
		},
		{
			name: "one neutral message holding all three",
			msgs: []Msg{
				TextMsg(RoleUser, "check three paths"),
				assistant,
				{Role: RoleUser, Blocks: []Block{
					ToolResultBlock("toolu_1", "a\n"),
					ToolResultBlock("toolu_2", "b\n"),
					ToolResultBlock("toolu_3", "c\n"),
				}},
			},
		},
		{
			name: "two then one",
			msgs: []Msg{
				TextMsg(RoleUser, "check three paths"),
				assistant,
				{Role: RoleUser, Blocks: []Block{
					ToolResultBlock("toolu_1", "a\n"),
					ToolResultBlock("toolu_2", "b\n"),
				}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_3", "c\n")}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, body, err := anthProvider().BuildRequest("sys", tc.msgs, []Tool{anthBashTool()}, 700)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			got := anthDecodeBody(t, body)

			// user(文本) → assistant(文本 + 3 tool_use) → user(3 tool_result)。
			// 这里四条或六条消息意味着结果没有折叠。
			if len(got.Messages) != 3 {
				var shape []string
				for _, m := range got.Messages {
					shape = append(shape, fmt.Sprintf("%s(%d blocks)", m.Role, len(m.Content)))
				}
				t.Fatalf("got %d messages %v, want 3 — consecutive tool results must collapse into ONE user message", len(got.Messages), shape)
			}

			results := got.Messages[2]
			if results.Role != "user" {
				t.Errorf("tool results were sent as role %q, want user — this protocol has no tool role", results.Role)
			}
			if len(results.Content) != 3 {
				t.Fatalf("got %d blocks in the results message, want 3", len(results.Content))
			}
			for i, want := range []struct{ id, content string }{
				{"toolu_1", "a\n"}, {"toolu_2", "b\n"}, {"toolu_3", "c\n"},
			} {
				b := results.Content[i]
				if b.Type != "tool_result" {
					t.Errorf("block %d type = %q, want tool_result", i, b.Type)
				}
				if b.ToolUseID != want.id {
					t.Errorf("block %d tool_use_id = %q, want %q (order must match the calls)", i, b.ToolUseID, want.id)
				}
				if b.Content != want.content {
					t.Errorf("block %d content = %q, want %q", i, b.Content, want.content)
				}
				if b.ID != "" {
					t.Errorf("block %d carries an `id` field; tool results are addressed by tool_use_id", i)
				}
			}

			// 助手回合重放为一个内容数组：首先是文本，
			// 然后是三个 tool_use 块，按顺序。
			asst := got.Messages[1]
			if asst.Role != "assistant" || len(asst.Content) != 4 {
				t.Fatalf("assistant message = %s with %d blocks, want assistant with 4", asst.Role, len(asst.Content))
			}
			if asst.Content[0].Type != "text" || asst.Content[0].Text != "Running three checks." {
				t.Errorf("assistant block 0 = %+v, want the text block", asst.Content[0])
			}
			for i, id := range []string{"toolu_1", "toolu_2", "toolu_3"} {
				b := asst.Content[i+1]
				if b.Type != "tool_use" || b.ID != id || b.Name != "bash" {
					t.Errorf("assistant block %d = %+v, want tool_use %s", i+1, b, id)
				}
				if len(b.Input) == 0 {
					t.Errorf("assistant block %d has no `input`; the field is required even when empty", i+1)
				}
			}
		})
	}
}

func TestAnthropicBuildRequestMessageShapes(t *testing.T) {
	t.Run("a following user turn merges into the tool_result message", func(t *testing.T) {
		// 连续两条用户消息是这个协议不喜欢的形状，
		// tool_result 块需要首先出现在携带它们的消息中。
		msgs := []Msg{
			TextMsg(RoleUser, "go"),
			{Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: `{"command":"ls"}`}}},
			{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", "out")}},
			TextMsg(RoleUser, "now explain it"),
		}
		_, body, err := anthProvider().BuildRequest("sys", msgs, nil, 700)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		got := anthDecodeBody(t, body)
		if len(got.Messages) != 3 {
			t.Fatalf("got %d messages, want 3 (user, assistant, user)", len(got.Messages))
		}
		last := got.Messages[2]
		if len(last.Content) != 2 || last.Content[0].Type != "tool_result" || last.Content[1].Type != "text" {
			t.Errorf("last message content = %+v, want tool_result then text", last.Content)
		}
	})

	t.Run("thinking blocks are dropped, and a thinking-only turn vanishes", func(t *testing.T) {
		// §B7/§A3b：签名在这个端点上**总是**空的，
		// 所以重放的思考块不能验证。丢弃它会丢失来自下一回合
		// 上下文的推理；发送它未签名冒着 400 的风险，
		// 这会杀死会话。trace 无论哪种方式都保留每个思考 token。
		msgs := []Msg{
			TextMsg(RoleUser, "go"),
			{Role: RoleAssistant, Blocks: []Block{{Kind: BlockThinking, Text: "long private plan"}}},
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockThinking, Text: "more planning"},
				{Kind: BlockText, Text: "Here is the answer."},
			}},
		}
		_, body, err := anthProvider().BuildRequest("sys", msgs, nil, 700)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		if strings.Contains(string(body), "thinking") {
			t.Errorf("a thinking block reached the wire: %s", body)
		}
		got := anthDecodeBody(t, body)
		if len(got.Messages) != 2 {
			t.Fatalf("got %d messages, want 2 — a message that renders to nothing must be skipped, not sent as content:[]", len(got.Messages))
		}
		if len(got.Messages[1].Content) != 1 || got.Messages[1].Content[0].Text != "Here is the answer." {
			t.Errorf("assistant content = %+v", got.Messages[1].Content)
		}
	})

	t.Run("a system message in msgs is a caller bug, reported loudly", func(t *testing.T) {
		// 将其重新标记为"user"会发送略有不同的 prompt，
		// 并产生略有较差的 Agent——最难注意到的 bug 类别。
		_, _, err := anthProvider().BuildRequest("sys", []Msg{TextMsg(RoleSystem, "you are a shell"), TextMsg(RoleUser, "hi")}, nil, 700)
		if err == nil {
			t.Fatal("want an error for a system message in msgs")
		}
		if !strings.Contains(err.Error(), "top-level") {
			t.Errorf("error = %v, want it to say where the system prompt belongs", err)
		}
	})

	t.Run("no messages at all is refused before the network", func(t *testing.T) {
		// §D11：这个网关对此的答案是一个 400，
		// 其 body 是 `{"model":"qwen3.7-plus"}`
		// ——没有类型、没有错误、没有东西可以记录。
		if _, _, err := anthProvider().BuildRequest("sys", nil, nil, 700); err == nil {
			t.Fatal("want an error for an empty conversation")
		}
	})

	t.Run("a tool with no schema still gets an input_schema", func(t *testing.T) {
		_, body, err := anthProvider().BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, []Tool{{Name: "now", Description: "the time"}}, 700)
		if err != nil {
			t.Fatalf("BuildRequest: %v", err)
		}
		got := anthDecodeBody(t, body)
		if len(got.Tools) != 1 || got.Tools[0].InputSchema["type"] != "object" {
			t.Errorf("tools = %+v, want an object input_schema even with no properties", got.Tools)
		}
	})
}

func TestAnthropicBuildRequestArgumentBytes(t *testing.T) {
	// Block.Args 是字符串而 Input 是 json.RawMessage 的原因：
	// 这些字节必须到达线上不变。通过 map[string]any 往返
	// 会对字段排序——Go 在 marshal 上对 map 键排序，
	// 模型按自己的顺序发出它们——而不同的字节序列是
	// 不同的 prompt 前缀，这是对每次重放回合的缓存未命中。
	cases := []struct {
		name string
		args string
		want string // body 必须包含的确切子字符串
	}{
		{
			// 有意**不**按字母顺序的键，以及一个充满
			// encoding/json 会默认转义的字符的命令：
			// `>` 和 `&` 变成 u003e/u0026，除非 HTML 转义关闭，
			// 这会破坏请求检查器中的每个重定向。
			name: "key order and shell metacharacters survive byte for byte",
			args: `{"z_last":"first","command":"grep -rn 'TODO' . 2>&1 > /tmp/o"}`,
			want: `"input":{"z_last":"first","command":"grep -rn 'TODO' . 2>&1 > /tmp/o"}`,
		},
		{
			// encoding/json 在拼接 RawMessage 时规范化的
			// **唯一**东西：token 之间无关紧要的空格。键顺序
			// ——真正破坏缓存的部分——未被触碰。记录在这里
			// 所以行为是一个决定，而不是一个惊喜。
			name: "insignificant whitespace is compacted, order is not",
			args: `{"command": "ls -la /srv/app"}`,
			want: `"input":{"command":"ls -la /srv/app"}`,
		},
		{
			// 一个调用零参数工具的模型。`input` 是必需的。
			name: "empty args become an empty object",
			args: ``,
			want: `"input":{}`,
		},
		{
			// §A3c：在 max_tokens 处截断的工具调用返回时
			// `input` 被替换为 `{"raw_arguments":"<invalid JSON>"}` 而
			// stop_reason 仍然说"tool_use"。如果那曾经往返到
			// 请求中，原始拼接会产生格式错误的 body——§D11 展示了
			// 这个网关用 500 回答格式错误的 body，一个依据 5xx
			// 判断的重试策略会永远重试。所以无效字节被包裹在网关自己的
			// 截断形状中：body 保持有效，证据完全存活在里面。
			name: "invalid arguments are wrapped, not spliced",
			args: `{"command": "find`,
			want: `"input":{"raw_arguments":"{\"command\": \"find"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []Msg{
				TextMsg(RoleUser, "go"),
				{Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: tc.args}}},
				{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", "done")}},
			}
			_, body, err := anthProvider().BuildRequest("sys", msgs, []Tool{anthBashTool()}, 700)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body does not contain\n  %s\ngot\n  %s", tc.want, body)
			}
			if !json.Valid(body) {
				t.Errorf("the body is not valid JSON — this gateway answers a malformed body with a 500: %s", body)
			}
		})
	}
}

func TestAnthropicBuildRequestKeepsToolResultTextIntact(t *testing.T) {
	// 命令输出是整个循环中最少被清理的东西：它是
	// shell 打印的任何东西。它必须逐字到达模型的 `content`，
	// 包括 encoding/json 本来会转义掉的尖括号和 & 符号。
	out := "total 4\ndrwxr-xr-x 2 root root <dir> & more\nexit 0\n"
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: `{"command":"ls"}`}}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", out)}},
	}
	_, body, err := anthProvider().BuildRequest("sys", msgs, nil, 700)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if strings.Contains(string(body), "u003c") {
		t.Errorf("tool output was HTML-escaped on the way out: %s", body)
	}
	got := anthDecodeBody(t, body)
	last := got.Messages[len(got.Messages)-1]
	if last.Content[0].Content != out {
		t.Errorf("tool_result content = %q, want %q", last.Content[0].Content, out)
	}
}

func TestAnthropicBaseURLTrailingSlash(t *testing.T) {
	// 一个 .env 文件中的一个字符；§D11 展示了这个网关
	// 用不透明的 500 回答生成的双斜杠。
	p := newAnthropicProvider("https://opencode.ai/zen/go/v1/", "k", "m")
	req, _, err := p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 10)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if want := "https://opencode.ai/zen/go/v1/messages"; req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL.String(), want)
	}
}
