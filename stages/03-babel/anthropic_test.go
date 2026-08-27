// Anthropic 协议适配器的测试。
//
// 下面每一份流式 fixture 都出自 docs/wire-notes.md §B6 和 §B7——是这个端点
// 真的发过的字节，不是为了让解析器显得正确而编出来的字节。这些 fixture 存
// 在的全部理由就是这个区别：照规范写出来的 fixture，测的是你对规范的理解；
// 而这个端点至少在四处跟规范相左，每一处都关系到这个文件（流外面的 ping、
// 没有 [DONE]、message_start 里报错的 usage，以及漏进可见文本的 `</think>`
// 标签）。
//
// 有些帧的信封是重建出来的——wire notes 里有些事件只记成裸 `delta` 对象，
// 有些只在事件序列里留了个名字——凡是这种，上面的注释都说了。里面的值一律
// 照抄。
//
// 不联网，不要 API key，没有 `-short` 跳过。整个文件在飞机上就能跑。
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

// 适配器必须满足 provider.go 里的契约。在这里断言一句，签名一旦漂移就变成
// 测试构建的编译错误——而且错误就报在旁边，那几个测试正好讲清楚了每个方法
// 欠调用方什么。
var _ Provider = (*anthropicProvider)(nil)

// ---------------------------------------------------------------------------
// 帧构造器。
//
// wire notes 里，参数片段是裸值，delta 是裸 `delta` 对象，所以这几个 helper
// 的活儿就是给照抄来的值重新套上信封。字符串转义交给 json.Marshal——网关当
// 初生成这些字节走的就是这条路；在 Go 字面量里手写 `\"`，正是 fixture 跟线
// 上对不上的开端。
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
// §B6 / §B7 的 fixture。
// ---------------------------------------------------------------------------

const (
	// **照抄** §B6。注意它**缺**了什么：没有 stop_reason，没有缓存计数；另外它
	// 给出的 input_tokens，紧接着的那份 usage 报告就推翻了。
	b6MessageStart = `{"type":"message_start","message":{"id":"msg_e3f9307e-2dc9-41f0-a70e-cca934593aa0","type":"message","role":"assistant","model":"qwen3.7-plus","content":[],"usage":{"input_tokens":56,"output_tokens":0}}}`

	// **照抄** §B6——tool_use 的宣告帧。`input` 是空对象，id 和 name 只在这里
	// 出现，别的帧里一个都没有。
	b6ToolStart0 = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ff07c814f3f34014aa526469","name":"bash","input":{}}}`

	// **照抄** §B6——同一次请求，这里的 usage 跟 message_start 对不上；而且只
	// 有这一帧带 stop_reason 和缓存计数。
	b6MessageDelta = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":291,"output_tokens":63,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

	// **照抄** §B6——收尾那个 ping，多带了 `cost` 这个键。
	b6PingWithCost = `{"type":"ping","cost":"0"}`

	// 形状是**重建**的：§B6 的事件序列里列了 `ping` 和 `message_stop`，但只印出
	// 了收尾那个 ping 的 body。
	b6Ping        = `{"type":"ping"}`
	b6MessageStop = `{"type":"message_stop"}`

	// **照抄** §B7——thinking 块、它那条（永远是空的）signature delta，以及它
	// 关掉之后在**下一个** index 上打开的 text 块。
	b7ThinkingStart   = `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`
	b7ThinkingDelta   = `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let"}}`
	b7SignatureDelta  = `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`
	b7BlockStop0      = `{"type":"content_block_stop","index":0}`
	b7TextStart1      = `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`
	b7TextDeltaFirst  = `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"To calculate"}}`
	b7TextDeltaSecond = `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" 17 ×"}}`
)

// b6ArgFragments 就是 §B6 记下的那六个 `partial_json` 值，按原顺序。
//
// 第一片是**空串**，第三片断在路径中间（`/srv`），第四片接着往下写
// （`/app`）。任何一片单拿出来都不是能解析的 JSON——所以适配器只管把裸字节
// 接起来，从不去看里面是什么。
var b6ArgFragments = []string{
	``,
	`{"command": "ls`,
	` -la /srv`,
	`/app`,
	`"`,
	`}`,
}

// b6WantArgs 是 §B6 说这些片段拼起来该有的样子。
const b6WantArgs = `{"command": "ls -la /srv/app"}`

// b6LeakText 是网关漏出来的 `</think>`，**照抄** §B6（§A3b 在非流式响应里也
// 逮到了一模一样的串）。先是换行，然后是光秃秃的闭合标签，再两个换行——整
// 整一个用户可见的文本块，里面没有半点模型输出。
const b6LeakText = "\n</think>\n\n"

// b6FullStream 是 §B6 那条两次工具调用的流，从头到尾：
//
//	ping message_start
//	content_block_start content_block_delta x6 content_block_stop  (index 0, tool_use)
//	content_block_start content_block_delta   content_block_stop   (index 1, text)
//	content_block_start content_block_delta x6 content_block_stop  (index 2, tool_use)
//	message_delta message_stop ping
//
// index 2 那个块是**构造**的——§B6 记了它的位置、类型和 delta 条数，但没记
// id 和参数——而且故意给了它另一条命令：解析器要是几个块共用一个缓冲区，出
// 来的就是一眼能看见的垃圾，而不是不易察觉的串味。
func b6FullStream() []string {
	frames := []string{
		b6Ping, // 在 message_start **之前**。规范说这不可能发生。
		b6MessageStart,
		b6ToolStart0,
	}
	for _, frag := range b6ArgFragments {
		frames = append(frames, anthArgsDelta(0, frag))
	}
	frames = append(frames,
		anthBlockStop(0),

		// index 1：漏出来的 `</think>`，自成一个 text 内容块。
		b7TextStart1,
		anthTextDelta(1, b6LeakText),
		anthBlockStop(1),

		// index 2：第二次工具调用。
		anthToolStart(2, "toolu_5ae0ccdc34f44d30a2217c5e", "bash"),
	)
	for _, frag := range []string{``, `{"command": "wc`, ` -l /srv`, `/app/main`, `.go"`, `}`} {
		frames = append(frames, anthArgsDelta(2, frag))
	}
	frames = append(frames,
		anthBlockStop(2),
		b6MessageDelta,
		b6MessageStop,
		b6PingWithCost, // 在 message_stop **之后**，带着 `cost`。
	)
	return frames
}

// ---------------------------------------------------------------------------
// 测试宿主。
// ---------------------------------------------------------------------------

// anthSSE 按 §B6 描述的这个端点的渲染器式来铺帧：`event: <name>`、
// `data: <payload>`、空行。事件名取自 payload 自己的 `type`——网关就是这么
// 拼的。
//
// 最后一帧后面**不带**空行，因为这条流真的就是这么结束的：没有 `[DONE]`，
// 没有终止符，连接一关就完了。读取器要是只在遇到空行时才派发，每次响应的最
// 后一帧都会被无声丢掉——而在这个协议上，最后几帧恰恰是 message_delta 和那
// 个报 cost 的 ping。
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

// anthRecorder 把每个事件都留着。这是对"Agent 核心为什么发事件而不是直接打
// 印"最省事的一次演示：这些测试断言的是事件序列，从头到尾不碰 stdout。
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

// anthParse 拿适配器跑一遍帧列表，把测试可能想断言的东西全都递回来。
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

	// message_start 前面一个 ping，message_stop 后面一个 ping。两个都不是
	// token，不是消息，也不是停止读取的理由。
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

	// 这条流里唯一的 text 块就是漏出来的 `</think>`，所以根本没有可见文本。
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

	// 每个事件都带着回合号，所以多回合会话的 trace 事后还能重新切开。
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
			// 实测到的片段，照抄 §B6，包括空的第一片，以及把路
			// 径从中间劈开的那两片。
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
			// **构造**的。两个块都开了才有一个关，片段交叉着来，
			// 而且 index 大的**先**开——所以，往同一个缓冲区里
			// 攒的实现，或者按到达顺序返回调用的实现，每跑必挂，
			// 不是跑两次挂一次。
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
			// 参数始终没到的工具调用。id 和 name 还是得留住：没
			// 有 id 就没有 tool_use_id 可以拿来回话，这个回合根
			// 本收不了尾。
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

			// 空的第一片不能变成一行 trace：一条字符都不带的参数
			// delta，在请求检查器里是噪音，在下游每个渲染器里也
			// 是噪音。
			for _, txt := range rec.textsOf(KindToolArgsDelta) {
				if txt == "" {
					t.Error("emitted a KindToolArgsDelta with empty text; the first observed fragment is \"\" and carries nothing")
				}
			}

			// 每次宣告都必须报出自己是哪个调用。
			for _, e := range rec.events {
				if e.Kind == KindToolCallStart && (e.ToolID == "" || e.ToolName == "") {
					t.Errorf("KindToolCallStart missing id or name: %+v", e)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Usage。这个文件里最有价值的测试。
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
			// §B6 里对不上的那一对：同一次请求，message_start 说
			// input_tokens 是 56，message_delta 说是 291。同样
			// prompt 的非流式调用站在 291 这边。这条断言哪天读到
			// 56，就说明适配器信了那一帧——规范说它权威，这个端
			// 点却把它搞错了。
			name:       "message_start says 56, message_delta says 291, 291 wins",
			frames:     []string{b6Ping, b6MessageStart, b6ToolStart0, anthArgsDelta(0, `{}`), anthBlockStop(0), b6MessageDelta, b6MessageStop, b6PingWithCost},
			want:       Usage{Input: 291, Output: 63},
			wantPrompt: 291,
			wantRaw:    "tool_use",
			wantStop:   StopToolUse,
		},
		{
			// 缓存热的一次调用，线上验过：input=18、
			// cache_creation=0、cache_read=17967。这个协议的
			// input_tokens **只**是没命中缓存的那部分余量，所以
			// 直接一一对应搬过来就行；上下文大小是它们的
			// **和**——17,985——线上没有任何单独字段报这个数。
			// （§C8 在更小的一本手册上量到同样的形状：input 18、
			// cache_read 9,775。）
			//
			// 适配器要是照搬 OpenAI 那个方向、拿 input 减掉
			// cache_read，在这儿会报出 -17,949。
			name:       "warm cache: input is only the uncached remainder",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "ACK"), anthBlockStop(1), anthMessageDelta("end_turn", 18, 249, 0, 17967), b6MessageStop},
			want:       Usage{Input: 18, CacheRead: 17967, Output: 249},
			wantPrompt: 17985,
			wantRaw:    "end_turn",
			wantStop:   StopEndTurn,
		},
		{
			// 缓存是冷的时候，第一次调用会把前缀写进去。
			// CacheWrite 单列一个字段，是因为它按约 1.25 倍计
			// 费，不是 0.1 倍。
			name:       "cold cache: creation tokens land in CacheWrite",
			frames:     []string{b6Ping, b6MessageStart, b7TextStart1, anthTextDelta(1, "ACK"), anthBlockStop(1), anthMessageDelta("end_turn", 18, 249, 9775, 0), b6MessageStop},
			want:       Usage{Input: 18, CacheWrite: 9775, Output: 249},
			wantPrompt: 9793,
			wantRaw:    "end_turn",
			wantStop:   StopEndTurn,
		},
		{
			// 流在 message_delta 之前就断了。这里**不**回退到
			// message_start 给的数：数字缺了看得见、追得到，而看
			// 着挺像样的错数字会一路进到成本看板里。而且完全没有
			// stop_reason，这个回合就是 StopUnknown——不是"大概
			// 没事"。
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

			// KindUsage 事件必须带着同样的归一化数字；什么都没报
			// 的时候，这个事件根本就不该发出来。
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
// Thinking。
// ---------------------------------------------------------------------------

func TestAnthropicThinkingAndTextStaySeparate(t *testing.T) {
	// 照抄 §B7：index 0 上是 thinking 块，有自己的 delta 类型，在 index 1 的
	// text 块打开之前就关掉了。代码要是默认 index 0 就是文本，就会把模型私下的
	// 推理渲染给用户看。
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

	// 这个网关会发 signature_delta，而且里面永远是 ""（§B7）。它不该产生任何
	// 事件：它既不是文本也不是提示，而且根本没有签名可以往返带回去。
	for _, txt := range append(rec.textsOf(KindTextDelta), rec.textsOf(KindReasoningDelta)...) {
		if txt == "" {
			t.Error("an empty delta reached the bus; signature_delta must be ignored, not forwarded")
		}
	}
	if got := rec.count(KindNotice); got != 0 {
		t.Errorf("got %d notices, want 0 — signature_delta is expected, not unknown", got)
	}

	// 第一个 token 是第一个**思考** token，不是第一个可见字符。在推理模型上，
	// 这才是诚实的度量：它确实是模型最先产出的东西。
	if got := rec.count(KindFirstToken); got != 1 {
		t.Fatalf("KindFirstToken count = %d, want 1", got)
	}
	if rec.kinds()[0] != KindFirstToken || rec.kinds()[1] != KindReasoningDelta {
		t.Errorf("first two events were %v, want first_token then reasoning_delta", rec.kinds()[:2])
	}
}

// ---------------------------------------------------------------------------
// `</think>` 泄漏。§B6 的第 4 处偏离。
// ---------------------------------------------------------------------------

// **受测的决定**：残留从用户可见文本里丢掉，改成一条提示报出来。不渲染（它
// 不是模型的输出），也不悄悄吞掉（trace 得留住证据，证明网关在漏自己宿主的
// 标记）。
func TestAnthropicThinkTagLeak(t *testing.T) {
	cases := []struct {
		name        string
		deltas      []string
		wantText    string
		wantNotices int
	}{
		{
			// **照抄** §B6 / §A3b：整整一个 text 内容块，内容从
			// 头到尾就是漏出来的那个闭合标签。
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
			// 开标签还没见过漏出来，但那是同一个故障，也是同一个
			// 修法。
			name:        "a bare opening tag is residue too",
			deltas:      []string{"<think>", "hello"},
			wantText:    "hello",
			wantNotices: 1,
		},
		{
			// **这条规则就是为了躲开这个误判**。模型在讲 think 标
			// 签怎么用——问 coding Agent 这个再正常不过——必须原
			// 封不动地传出去。为了扫干净供应商的垃圾而悄悄改坏真
			// 正的输出，是两种故障里更糟的那种，所以规则写的是
			// "整条 delta 就是那个标签"，不是"delta 里含有它"。
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
			// 丢掉就是丢掉：任何渲染器都不许看见这个标签。
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
	// 一条流里塞进了每一种 payload 类型，所以下面那条断言就是这个适配器欠渲染
	// 器的全部契约：跟 OpenAI 适配器同样的 kind、同样的顺序、同样的含义——而线
	// 上这套词汇跟那边毫无交集。
	frames := []string{
		b6Ping,         // 不是 token
		b6MessageStart, // 不是 token，而且它报的 usage 是假的
		b7ThinkingStart,
		b7ThinkingDelta,
		b7SignatureDelta, // 永远是空的；什么都不产生
		b7BlockStop0,
		b7TextStart1,
		b7TextDeltaFirst,
		anthBlockStop(1),
		anthToolStart(2, "toolu_x", "bash"),
		anthArgsDelta(2, ``), // 空的第一片什么都不产生
		anthArgsDelta(2, `{"command": "ls"}`),
		anthBlockStop(2),
		b6MessageDelta,
		b6MessageStop,
		b6PingWithCost, // 在 message_stop 之后，照样不是 token
	}

	_, rec, err := anthParse(t, frames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []Kind{
		KindFirstToken,     // 由 thinking delta 触发，不是 ping
		KindReasoningDelta, //
		KindTextDelta,      //
		KindToolCallStart,  // id + name，只此一次
		KindToolArgsDelta,  // 一个片段，空的那个跳过了
		KindUsage,          // message_delta
		KindResponseEnd,    // 最后一个
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
// 分帧的边角情况，以及能扛过去的损坏。
// ---------------------------------------------------------------------------

func TestAnthropicStreamTolerance(t *testing.T) {
	t.Run("pings anywhere, and no [DONE] anywhere", func(t *testing.T) {
		frames := []string{
			b6Ping, b6Ping, // 在 message_start 之前
			b6MessageStart,
			b7TextStart1,
			b6Ping, // 流中间一次普通的 keep-alive
			anthTextDelta(1, "hello"),
			anthBlockStop(1),
			anthMessageDelta("end_turn", 291, 63, 0, 0),
			b6MessageStop,
			b6PingWithCost, b6Ping, // 在 message_stop 之后
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
		// 收尾那个 ping 是在 message_stop **之后**才到的，所以在
		// 那儿就返回的解析器永远看不到 `cost`，还会在 socket 里
		// 剩下字节——连接因此回不了 keep-alive 池。
		if got := rec.count(KindUsage); got != 1 {
			t.Errorf("KindUsage count = %d, want 1", got)
		}
	})

	t.Run("a non-zero cost on the trailing ping is reported", func(t *testing.T) {
		// §C10 见到的一直只有字符串 "0"。真出来一个数字，那就是这
		// 个端点发过的第一个成本信号，所以得进 trace。
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
		// `cost` 这个键的类型定成 json.RawMessage，就是为了它的
		// JSON 类型一变，不至于把整帧一起拖垮。
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
// 流中途失败。
// ---------------------------------------------------------------------------

func TestAnthropicMidStreamFailureKeepsPartialAndSkipsResponseEnd(t *testing.T) {
	t.Run("the connection dies", func(t *testing.T) {
		// 到一次完整工具调用为止的东西都到了，然后 socket 断了。调
		// 用方要靠这份残缺结果，才能把"完整工具调用之后才死"和"什
		// 么都没产出"分开。
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
		// **构造**的：§D11 里的错误全是在流打开之前以 HTTP 状态码
		// 到达的，所以这个形状在这个网关上没实测到过。规范说
		// overloaded_error 会在 body 中间流出来；而半路死掉的流，
		// 绝不能被记成跑完的流。
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

// anthWireBody 照着 BuildRequest 本该产出的东西写了一份。它故意不复用
// anthropic.go 里的结构体：用编码时的同一个类型去解码，是抓不到写错的 json
// tag 的——两头一起错，谁也发现不了谁。
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
		Function    json.RawMessage `json:"function"` // 绝不该出现
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

	// 是 x-api-key，**不是** Authorization: Bearer。在这儿发另一个协议的头，换
	// 回来的是 "Missing API key."，读上去像是配置出了问题。
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

	// 返回的字节必须就是线上的字节：调用方拿它发 KindRequest，而请求检查器给你
	// 看的东西一旦跟实际发出去的不一样，那还不如没有检查器。
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

	// **不对称就在这里**。系统提示词在这边是顶层字段；OpenAI 适配器把它塞进
	// messages[0]。两种形状都当不了中立的那个。
	if got.System != "you are a shell" {
		t.Errorf("system = %q, want it at the top level", got.System)
	}
	for _, m := range got.Messages {
		if m.Role == "system" {
			t.Error("the system prompt was sent as a message; on this protocol it is a top-level field")
		}
	}

	// 工具定义是平的：{name, description, input_schema}。没有 `function` 那层
	// 嵌套，也没有 `parameters`。
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

	// max_tokens 在这边是必填的，§D11 展示了漏掉它的代价：400，body 就是
	// `{"model":"qwen3.7-plus"}`，连错误信封都没有。预算不是正数就给个默认值，
	// 省得变成一桩悬案。
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

// TestAnthropicBuildRequestCollapsesToolResults 就是这个文件存在的理由。三
// 份工具结果会合成**一条** user 消息，里面装三个 tool_result 块——不管调用
// 方当初怎么摆的。OpenAI 适配器是一份结果一条消息；把这件事做反，是
// anthropic.go 里最可能出现的 bug。
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
			// 主循环每做完一个工具就追加一条消息，自然就摆成这
			// 样——也正是 OpenAI 协议想要的摆法。
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

			// user(text) → assistant(text + 3 tool_use) → user(3 tool_result)。
			// 这里出现四条或六条消息，就说明结果没合并。
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

			// assistant 那一轮回放成一个 content 数组：先是文本，
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
		// 连着两条 user 消息，是这个协议不喜欢的形状；而且
		// tool_result 块必须排在承载它们那条消息的最前面。
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
		// §B7/§A3b：这个端点上的签名**永远**是空的，所以回放的
		// thinking 块通不过校验。丢掉它，下一回合的上下文就少了
		// 推理；不带签名发出去，则可能吃个 400 把整个会话干掉。
		// 反正无论走哪条路，trace 里每个 thinking token 都留着。
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
		// 把它改标成 "user"，发出去的 prompt 就会有微妙的不同，
		// 做出来的 Agent 也就微妙地差一点——这是最难被察觉的一
		// 类 bug。
		_, _, err := anthProvider().BuildRequest("sys", []Msg{TextMsg(RoleSystem, "you are a shell"), TextMsg(RoleUser, "hi")}, nil, 700)
		if err == nil {
			t.Fatal("want an error for a system message in msgs")
		}
		if !strings.Contains(err.Error(), "top-level") {
			t.Errorf("error = %v, want it to say where the system prompt belongs", err)
		}
	})

	t.Run("no messages at all is refused before the network", func(t *testing.T) {
		// §D11：网关对这种情况的回答是 400，body 就是
		// `{"model":"qwen3.7-plus"}`——没有 type，没有 error，没
		// 有任何可记的东西。
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
	// Block.Args 是字符串、Input 是 json.RawMessage，原因就在这里：这些字节必
	// 须原样上线。让它们在 map[string]any 里过一遍，键就被排序了——Go 在
	// marshal 时会给 map 的键排序，而模型是按自己的顺序吐出来的——字节序列一
	// 变，prompt 前缀就变了，每个回放的回合都是一次缓存未命中。
	cases := []struct {
		name string
		args string
		want string // body 里必须原样含有的子串
	}{
		{
			// 键的顺序故意**不**按字母排；命令里也塞满了
			// encoding/json 默认会转义的字符：不关掉 HTML 转义，
			// `>` 和 `&` 就变成 u003e/u0026，请求检查器里每一条重
			// 定向都会被写坏。
			name: "key order and shell metacharacters survive byte for byte",
			args: `{"z_last":"first","command":"grep -rn 'TODO' . 2>&1 > /tmp/o"}`,
			want: `"input":{"z_last":"first","command":"grep -rn 'TODO' . 2>&1 > /tmp/o"}`,
		},
		{
			// 拼接 RawMessage 时，encoding/json **只**动一样东西：
			// token 之间无意义的空白。键的顺序——真正会毁掉缓存的
			// 那部分——一动没动。记在这里，是让这个行为成为一个决
			// 定，而不是一次意外。
			name: "insignificant whitespace is compacted, order is not",
			args: `{"command": "ls -la /srv/app"}`,
			want: `"input":{"command":"ls -la /srv/app"}`,
		},
		{
			// 模型调用了不带参数的工具。`input` 是必填的。
			name: "empty args become an empty object",
			args: ``,
			want: `"input":{}`,
		},
		{
			// §A3c：工具调用在 max_tokens 处被截断，回来的 `input`
			// 会被换成 `{"raw_arguments":"<invalid JSON>"}`，而
			// stop_reason 还写着 "tool_use"。这东西一旦又回到请求
			// 里，裸着拼进去就会产出畸形的 body——而 §D11 显示这个
			// 网关拿 500 回答畸形 body，按 5xx 来重试的策略会永远重
			// 试下去。所以无效字节要用网关自己那套截断形状包起来：
			// body 保持合法，证据原封不动地活在里面。
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
	// 命令输出是整个主循环里最没被处理过的东西：shell 打了什么就是什么。它必须
	// 一个字节不差地变成模型看到的 `content`，包括那些 encoding/json 本来会转
	// 义掉的尖括号和 & 号。
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
	// .env 文件里的一个字符；§D11 显示，由此产生的双斜杠会让这个网关回一个不知
	// 所云的 500。
	p := newAnthropicProvider("https://opencode.ai/zen/go/v1/", "k", "m")
	req, _, err := p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 10)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if want := "https://opencode.ai/zen/go/v1/messages"; req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL.String(), want)
	}
}
