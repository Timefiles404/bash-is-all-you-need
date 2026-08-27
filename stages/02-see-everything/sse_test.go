// SSE 读取器和 OpenAI 流解析器的测试。
//
// 下面每一个帧常量都是从 docs/wire-notes.md 里抄出来的——这些是这个端点
// 真的发过的字节，不是为了让解析器好看而编出来的字节。这正是全部要点：你
// 照着规范写的 fixture，测的只是你对规范的理解，而这个端点跟规范对不上
// （见 §B4 第 11、13 帧）。哪个 fixture 是重建的或是编的，它上面的注释会
// 说清楚，也会说为什么。
//
// 不联网，不用 API key，没有 `-short` 跳过。整个文件在飞机上就能跑。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// ---------------------------------------------------------------------------
// §B4——完整的 13 帧工具调用流，按原顺序。
//
// 产生它的请求：`bash` 工具，tool_choice:"required"，
// reasoning_effort:"none"，prompt 是 "Call the bash tool once with command set
// to: ls -la /srv/app"。
//
// 第 1、10、11、12、13 帧在 §B4 里是整帧记下的，这里逐字照抄。第 2–9 帧在
// 那儿只记了 `delta` 对象，外面那层信封是拿完整的第 1 帧和第 10 帧重建出来
// 的。`delta` 对象本身——包括每一个显式的 `null`——是逐字的。
// ---------------------------------------------------------------------------

const (
	// 1. role 开场帧。注意 `content` 是 ""，不是 null，而且它一点载荷都没
	//    带：正是这一帧决定了 TTFT 不能从收到的第一帧算起。
	b4RoleOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}`

	// 2. 工具调用开场帧——**唯一**带 `id` 和 `function.name` 的 chunk。
	b4ToolOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_8d4f0377bc594026a4765cfc","type":"function","function":{"name":"bash","arguments":""}}]}}]}`

	// 3.–9. 参数碎片。`id` 和 `function.name` 现在是显式的 null，`index`
	//       仍是 0，`type` 仍是 "function"——它没有被置 null，这正是"key
	//       在那儿"什么也证明不了的原因。
	//
	//       切法不按 JSON 边界：第 1 个碎片停在对象中间，第 4 个停在路径
	//       中间（`/srv`），第 5 个把它接上（`/app`）。
	b4Arg1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": "}}]}}]}`
	b4Arg2 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"\""}}]}}]}`
	b4Arg3 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"ls"}}]}}]}`
	b4Arg4 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":" -la /srv"}}]}}]}`
	b4Arg5 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"/app"}}]}}]}`
	b4Arg6 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"\""}}]}}]}`
	b4Arg7 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"}"}}]}}]}`

	// 10. finish chunk——delta 为空，finish_reason 有值。
	b4Finish = `{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`

	// 11. usage chunk。`choices` 是**空数组**。任何伸手去拿 choices[0] 的
	//     代码都会在这里 panic，就在每个真实请求的倒数第二帧上。（§B5：
	//     这一帧默认就有，不用发 stream_options——而且发了 stream_options
	//     也没有任何变化。）
	b4Usage = `{"id":"...","object":"chat.completion.chunk","created":1787768844,"model":"mimo-v2.5","choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}`

	// 12. 哨兵。
	b4Done = `[DONE]`

	// 13. 哨兵**之后**的一帧。每个守规范的客户端都会把它丢掉。这里的
	//     `choices` 也是空的。
	b4PostDone = `{"choices":[],"cost":"0"}`
)

// b4ToolCallStream 是 §B4 从头到尾，按记录下来的顺序。
var b4ToolCallStream = []string{
	b4RoleOpener,
	b4ToolOpener,
	b4Arg1, b4Arg2, b4Arg3, b4Arg4, b4Arg5, b4Arg6, b4Arg7,
	b4Finish,
	b4Usage,
	b4Done,
	b4PostDone,
}

// b4WantArgs 是 §B4 说这些碎片拼起来应该得到的东西。
const b4WantArgs = `{"command": "ls -la /srv/app"}`

// b4WantUsage 是第 11 帧经过 sseUsage.normalise 上讲的那次方向反转之后的
// 样子：prompt_tokens 506 **包含**着 cached_tokens 192，所以全价的 Input
// 是两者之差，而 Prompt() 必须还原回 506。
var b4WantUsage = Usage{Input: 314, CacheRead: 192, Output: 26, Reasoning: 0}

// ---------------------------------------------------------------------------
// §B7——reasoning 和文本落在同一个 delta 对象上。
//
// 那五条 `reasoning_content` delta 和 role 开场帧，是 §B7 里逐字的 `delta`
// 对象，装在重建出来的信封里。§B7 记着这次运行有 44 帧 reasoning、1 帧
// content，但没把那帧 content 印出来，所以这里的两帧 `content` 是照着一模
// 一样的形状造的——足够证明这两个字段会落进两个不同的累加器，而这正是要
// 测的东西。
// ---------------------------------------------------------------------------

const (
	b7RoleOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}`
	b7Reason1    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":"Okay","tool_calls":null}}]}`
	b7Reason2    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":", the","tool_calls":null}}]}`
	b7Reason3    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":" user is asking for","tool_calls":null}}]}`
	b7Reason4    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":" the product of ","tool_calls":null}}]}`
	b7Reason5    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":"17 and ","tool_calls":null}}]}`

	// 是造出来的，不是记录下来的——见上面那段块注释。
	b7Text1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":"17 * 23 = ","reasoning_content":null,"tool_calls":null}}]}`
	b7Text2 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":"391","reasoning_content":null,"tool_calls":null}}]}`

	b7Finish = `{"choices":[{"index":0,"finish_reason":"stop","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`
)

var b7ReasoningStream = []string{
	b7RoleOpener,
	b7Reason1, b7Reason2, b7Reason3, b7Reason4, b7Reason5,
	b7Text1, b7Text2,
	b7Finish,
	b4Usage,
	b4Done,
}

// ---------------------------------------------------------------------------
// 两个并行的工具调用。
//
// **造出来的**，不是记录下来的：§B4 抓到的是单次调用的流，而 §D12 只确认
// 了 `parallel_tool_calls:false` 会被接受并被忽略，所以并行调用是够得着
// 的，但没有逐字的抓包。chunk 的形状是从 §B4 原样抄的——只改了 `index`、
// 那些 id，还有碎片文本。
//
// 两处刻意的扭曲，都是为了让 bug 显形，而不是让它有可能显形：
//
//   - index 1 比 index 0 **先**开场，这样按到达顺序返回调用的实现是每次都
//     挂，而不是一半的时候挂。
//   - 碎片是交错的，这样往同一个共享缓冲里追加的实现会产出一眼看得见的垃
//     圾，而不是一处不易察觉的串味。
// ---------------------------------------------------------------------------

const (
	parOpener1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_second","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	parOpener0 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	parArg1a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"echo "}}]}}]}`
	parArg0a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"ls"}}]}}]}`
	parArg1b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"two\"}"}}]}}]}`
	parArg0b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":" -la\"}"}}]}}]}`
)

var parallelToolCallStream = []string{
	b4RoleOpener,
	parOpener1, parOpener0,
	parArg1a, parArg0a,
	parArg1b, parArg0b,
	b4Finish,
	b4Usage,
	b4Done,
	b4PostDone,
}

// ---------------------------------------------------------------------------
// 辅助函数。
// ---------------------------------------------------------------------------

// sseBody 照着 §B4 里这个端点的渲染样子来渲染载荷：`data: <payload>` 后面
// 跟一个空行，用 LF 结尾（那份文档是用 `cat -A` 展示的，每行都以 `$` 收
// 尾，看不到 `^M`）。
func sseBody(frames ...string) io.Reader {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return strings.NewReader(b.String())
}

// sseRecorder 是个什么都留着的 Subscriber，这是对"Agent 核心为什么发事件
// 而不是直接打印"最省事的一次演示：测试断言的是事件序列，全程不碰 stdout。
type sseRecorder struct{ events []Event }

func (r *sseRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func (r *sseRecorder) kinds() []Kind {
	out := make([]Kind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *sseRecorder) count(k Kind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (r *sseRecorder) first(k Kind) (Event, bool) {
	for _, e := range r.events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

// ---------------------------------------------------------------------------
// readSSE：只管分帧。
// ---------------------------------------------------------------------------

func TestReadSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []sseFrame
	}{
		{
			// 本阶段真正会碰上的形态：只有 `data:`，别的都没有。
			name: "openai style, data lines only",
			in:   "data: a\n\ndata: b\n\n",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "b"}},
		},
		{
			// 这个端点上没观测到，它发的是裸 LF——但 SSE 规范是按 CRLF
			// 定的，路径上任何一个代理都可能改写行尾，于是只认 LF 的解
			// 析器会在每份载荷末尾留下一个多余的 \r，JSON 就解不开了。
			name: "CRLF line endings",
			in:   "data: a\r\n\r\ndata: b\r\n\r\n",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "b"}},
		},
		{
			// 阶段 03 需要的形态（§B6）。本阶段里没有东西会产生它。
			name: "anthropic style, event plus data",
			in:   "event: content_block_delta\ndata: {\"type\":\"text_delta\"}\n\n",
			want: []sseFrame{{Name: "content_block_delta", Data: `{"type":"text_delta"}`}},
		},
		{
			name: "multi-line data is joined with newlines",
			in:   "data: line one\ndata: line two\ndata: line three\n\n",
			want: []sseFrame{{Name: "", Data: "line one\nline two\nline three"}},
		},
		{
			// keep-alive。它们不能终结正在进行的那一帧，也不能自己产出
			// 一帧来。
			name: "comment lines are ignored",
			in:   ": keep-alive\ndata: a\n: mid-frame comment\ndata: b\n\n: trailing\n\n",
			want: []sseFrame{{Name: "", Data: "a\nb"}},
		},
		{
			// 这条抓的 bug 是无声的：一条流的最后一帧，通常就是带 usage
			// 的那帧。
			name: "no trailing blank line at EOF",
			in:   "data: a\n\ndata: last",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "last"}},
		},
		{
			name: "trailing newline but no blank line at EOF",
			in:   "data: last\n",
			want: []sseFrame{{Name: "", Data: "last"}},
		},
		{
			name: "runs of blank lines produce no frames",
			in:   "\n\n\ndata: a\n\n\n\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			name: "no space after the colon",
			in:   "data:tight\n\n",
			want: []sseFrame{{Name: "", Data: "tight"}},
		},
		{
			name: "exactly one leading space is stripped",
			in:   "data:  two spaces\n\n",
			want: []sseFrame{{Name: "", Data: " two spaces"}},
		},
		{
			// 这条线上每份载荷都是 JSON，所以按最后一个冒号切（或者见冒
			// 号就切）会把每一帧都毁掉。
			name: "only the first colon separates field from value",
			in:   "data: {\"model\":\"mimo-v2.5\",\"t\":\"12:34:56\"}\n\n",
			want: []sseFrame{{Name: "", Data: `{"model":"mimo-v2.5","t":"12:34:56"}`}},
		},
		{
			// 规范里用来恢复断流的字段。这里故意忽略，但绝不能把它们错
			// 当成 data。
			name: "id and retry fields are ignored, not treated as data",
			in:   "id: 42\nretry: 3000\ndata: a\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			// 照规范办，而且这条有分量：事件类型那个缓冲必须清掉，否则
			// 名字会漏到下一帧上。
			name: "a frame with no data line is not dispatched and does not leak its name",
			in:   "event: ping\n\ndata: a\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			// readSSE 完全不知道哨兵这回事。[DONE] 是什么意思，由载荷解
			// 析器去定，正是这一点让前一半还能复用到根本没有哨兵的协议
			// （§B6）上。
			name: "the DONE sentinel is just another frame down here",
			in:   "data: [DONE]\n\ndata: {\"choices\":[],\"cost\":\"0\"}\n\n",
			want: []sseFrame{{Name: "", Data: "[DONE]"}, {Name: "", Data: `{"choices":[],"cost":"0"}`}},
		},
		{
			name: "empty stream",
			in:   "",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []sseFrame
			if err := readSSE(strings.NewReader(tc.in), func(f sseFrame) error {
				got = append(got, f)
				return nil
			}); err != nil {
				t.Fatalf("readSSE: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("frames:\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestReadSSEStopsOnCallbackError(t *testing.T) {
	boom := errors.New("subscriber gave up")

	seen := 0
	err := readSSE(sseBody("a", "b", "c"), func(sseFrame) error {
		seen++
		if seen == 2 {
			return boom
		}
		return nil
	})

	if !errors.Is(err, boom) {
		t.Fatalf("want the callback's error back, got %v", err)
	}
	if seen != 2 {
		t.Errorf("scan continued past the error: fn ran %d times, want 2", seen)
	}
}

func TestReadSSEHandlesLinesOverScannerLimit(t *testing.T) {
	// bufio.Scanner 会在 64KB 处以 ErrTooLong 挂掉这条。这么大的单条 delta
	// 不是假想：一次 `cat` 大文件，经工具结果回显出去，出去的时候就长这
	// 样。
	huge := strings.Repeat("x", 200*1024)

	var got []sseFrame
	if err := readSSE(sseBody(huge), func(f sseFrame) error {
		got = append(got, f)
		return nil
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 1 || got[0].Data != huge {
		t.Fatalf("large frame not reassembled: %d frames, %d bytes", len(got), len(got[0].Data))
	}
}

// ---------------------------------------------------------------------------
// parseOpenAIStream：记录下来的那些流，从头跑到尾。
// ---------------------------------------------------------------------------

func TestParseOpenAIStream(t *testing.T) {
	cases := []struct {
		name          string
		frames        []string
		wantText      string
		wantReasoning string
		wantTools     []streamToolCall
		wantFinish    string
		wantUsage     Usage
	}{
		{
			// 头号用例：§B4 逐字，13 帧全上。
			name:       "B4 tool call, all thirteen frames",
			frames:     b4ToolCallStream,
			wantFinish: "tool_calls",
			wantUsage:  b4WantUsage,
			wantTools: []streamToolCall{{
				ID:   "call_8d4f0377bc594026a4765cfc",
				Name: "bash",
				Args: b4WantArgs,
			}},
		},
		{
			// §B7：同一个 delta 对象上的两个字段必须落到两个地方。
			name:          "B7 reasoning and text are kept apart",
			frames:        b7ReasoningStream,
			wantText:      "17 * 23 = 391",
			wantReasoning: "Okay, the user is asking for the product of 17 and ",
			wantFinish:    "stop",
			wantUsage:     b4WantUsage,
		},
		{
			// 那帧能让 choices[0] 式解析器 panic 的帧，单独上。
			name:      "usage frame alone, choices is an empty array",
			frames:    []string{b4Usage},
			wantUsage: b4WantUsage,
		},
		{
			// §B4 第 13 帧，单独上：choices 是空的，**而且**顶层还有个不
			// 认识的 key。它是在哨兵之后才到的，这就是这种情况容易一直
			// 没测过的原因。
			name:   "post-DONE cost frame alone",
			frames: []string{b4Done, b4PostDone},
		},
		{
			// 读过哨兵这件事，只有真能捡到东西才站得住脚。把 usage 挪到
			// [DONE] 后面，这就是账算得对和悄无声息记成零之间的区别。
			name:       "frames after the sentinel are still read",
			frames:     []string{b4RoleOpener, b4Finish, b4Done, b4Usage, b4PostDone},
			wantFinish: "tool_calls",
			wantUsage:  b4WantUsage,
		},
		{
			// 并行调用：各攒各的，按 index 升序回来。
			name:       "two parallel tool calls interleaved",
			frames:     parallelToolCallStream,
			wantFinish: "tool_calls",
			wantUsage:  b4WantUsage,
			wantTools: []streamToolCall{
				{ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
				{ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
			},
		},
		{
			// 一条什么都没产出的流，回来的时候也必须是干净的，不能是初
			// 始化到一半的样子。
			name:       "role opener and finish only",
			frames:     []string{b4RoleOpener, b4Finish, b4Done},
			wantFinish: "tool_calls",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOpenAIStream(sseBody(tc.frames...), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("parseOpenAIStream: %v", err)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text\n got %q\nwant %q", got.Text, tc.wantText)
			}
			if got.Reasoning != tc.wantReasoning {
				t.Errorf("Reasoning\n got %q\nwant %q", got.Reasoning, tc.wantReasoning)
			}
			if got.FinishReason != tc.wantFinish {
				t.Errorf("FinishReason got %q, want %q", got.FinishReason, tc.wantFinish)
			}
			if got.Usage != tc.wantUsage {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.wantUsage)
			}
			if !reflect.DeepEqual(got.ToolCalls, tc.wantTools) {
				t.Errorf("ToolCalls\n got %#v\nwant %#v", got.ToolCalls, tc.wantTools)
			}
		})
	}
}

// TestB4ArgsReassembleIntoValidJSON 是"从不解析碎片"这条规矩换来的回报。
// §B4 里那七段，单拎出来没有一段是合法 JSON；拼起来才是，而那也是唯一允
// 许做解析的地方。
func TestB4ArgsReassembleIntoValidJSON(t *testing.T) {
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.ToolCalls))
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(got.ToolCalls[0].Args), &args); err != nil {
		t.Fatalf("assembled args are not valid JSON: %v (%q)", err, got.ToolCalls[0].Args)
	}
	if args.Command != "ls -la /srv/app" {
		t.Errorf("command got %q, want %q", args.Command, "ls -la /srv/app")
	}
}

// TestToolIDSurvivesTheNullChunks 是 id 锁存的回归测试，单独立出来，好让
// 失败信息直接点出病名。第 3–9 帧全都带着 `"id":null`；不加防护的赋值会把
// 它留空，这次工具调用就没法回答了，因为 API 要求回复里把那个 id 带回去。
func TestToolIDSurvivesTheNullChunks(t *testing.T) {
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].ID != "call_8d4f0377bc594026a4765cfc" {
		t.Errorf("tool id was lost to a later null chunk: got %q", got.ToolCalls[0].ID)
	}
	if got.ToolCalls[0].Name != "bash" {
		t.Errorf("tool name was lost to a later null chunk: got %q", got.ToolCalls[0].Name)
	}
}

// TestParallelToolCallsComeBackInIndexOrder 把同一条流跑很多遍，因为 Go 是
// 故意把 map 遍历顺序随机化的。只跑一遍，大概只有一半的概率能抓到漏掉的排
// 序——两次提交挂一次的测试比没有测试更糟，因为它教会大家重跑 CI。跑二十
// 遍，误判为通过的概率大约是百万分之一。
func TestParallelToolCallsComeBackInIndexOrder(t *testing.T) {
	want := []streamToolCall{
		{ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
		{ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
	}

	for i := 0; i < 20; i++ {
		got, err := parseOpenAIStream(sseBody(parallelToolCallStream...), NewBus(), 1, time.Now())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if !reflect.DeepEqual(got.ToolCalls, want) {
			t.Fatalf("pass %d: out of index order\n got %#v\nwant %#v", i, got.ToolCalls, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Usage 归一化——把方向反过来。
// ---------------------------------------------------------------------------

func TestUsageNormalisation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Usage
	}{
		{
			// §B4 第 11 帧，逐字。506 是**整个** prompt，里面含着 192 个缓
			// 存 token，所以全价的 Input 是 314，而 Prompt() 必须还原回
			// 506。
			name: "B4 frame 11 without stream_options",
			in:   b4Usage,
			want: Usage{Input: 314, CacheRead: 192, Output: 26, Reasoning: 0},
		},
		{
			// §B5：同一个请求，**带上** stream_options:{include_usage:true}。
			// 这个参数不起任何作用；只有 cached_tokens 不一样，而它不一样
			// 是因为缓存状态本来就会随运行变化。
			name: "B5 frame 11 with stream_options, a no-op",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":448},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 58, CacheRead: 448, Output: 26},
		},
		{
			// 冷请求：什么都没缓存，所以 Input 就是整个 prompt。
			name: "no cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 506, Output: 26},
		},
		{
			// Reasoning 是 completion_tokens 的**子集**，不是外加的。
			name: "a thinking model reports reasoning inside completion",
			in:   `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":900,"total_tokens":1000,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":850}}}`,
			want: Usage{Input: 60, CacheRead: 40, Output: 900, Reasoning: 850},
		},
		{
			// detail 对象直接给成 null。这个端点上每个字段都可能是 null，
			// 解析器就得活下来——给零，不是崩溃，也不是负数。
			name: "null detail objects",
			in:   `{"choices":[],"usage":{"prompt_tokens":80,"completion_tokens":9,"total_tokens":89,"prompt_tokens_details":null,"completion_tokens_details":null}}`,
			want: Usage{Input: 80, Output: 9},
		},
		{
			// 防御性的：缓存比 prompt 还多，算术上不可能，但对外抛一个负
			// 的 token 数会毒害 Prompt()，也毒害下游每一处成本估算。钳住，
			// 往下走。
			name: "cached exceeds prompt, clamped rather than negative",
			in:   `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":99}}}`,
			want: Usage{Input: 0, CacheRead: 99, Output: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOpenAIStream(sseBody(tc.in, b4Done), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("parseOpenAIStream: %v", err)
			}
			if got.Usage != tc.want {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.want)
			}
		})
	}
}

// TestUsagePromptRoundTrips 说的是那条不变量，有了它就不用再算一遍减法也
// 能检查这次反转：不管怎么拆，Prompt() 都必须等于端点报出来的
// prompt_tokens。
func TestUsagePromptRoundTrips(t *testing.T) {
	got, err := parseOpenAIStream(sseBody(b4Usage), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if got.Usage.Prompt() != 506 {
		t.Errorf("Prompt() got %d, want 506 (the prompt_tokens on the wire)", got.Usage.Prompt())
	}
	if got.Usage.CacheWrite != 0 {
		t.Errorf("CacheWrite got %d, want 0: this protocol reports no write figure", got.Usage.CacheWrite)
	}
}

// ---------------------------------------------------------------------------
// 事件流。事件总线之所以存在，就是为了这几个测试。
// ---------------------------------------------------------------------------

func TestEventSequenceForB4ToolCall(t *testing.T) {
	rec := &sseRecorder{}
	if _, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(rec), 7, time.Now()); err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}

	want := []Kind{
		KindFirstToken,    // 第 2 帧：工具调用开场帧才是第一份真载荷
		KindToolCallStart, // 同一帧，id 和 name 锁住之后
		// 第 3-9 帧。开场帧那个 `"arguments":""` 什么都不产生，所以这里是
		// 七条，不是八条。
		KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta,
		KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta,
		KindUsage,       // 第 11 帧，choices 为空的那帧
		KindResponseEnd, // 把第 12、13 帧读完之后
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("event kinds\n got %v\nwant %v", got, want)
	}

	if n := rec.count(KindFirstToken); n != 1 {
		t.Errorf("KindFirstToken emitted %d times, want exactly 1", n)
	}

	// 第 1 帧带的是 `content: ""`，那不是 token。要是 TTFT 从它算起，
	// first_token 会落在模型什么都还没生成的时候，这个数字就会给每个请求往
	// 脸上贴金。
	if start, ok := rec.first(KindToolCallStart); !ok {
		t.Error("no tool_call_start")
	} else if start.ToolID != "call_8d4f0377bc594026a4765cfc" || start.ToolName != "bash" {
		t.Errorf("tool_call_start got id=%q name=%q", start.ToolID, start.ToolName)
	}

	// 每个事件都带着回合号，这样 trace 可以直接按回合切开，不用再去推导什
	// 么。
	for _, e := range rec.events {
		if e.Turn != 7 {
			t.Fatalf("event %s has turn %d, want 7", e.Kind, e.Turn)
		}
	}

	// usage 事件带的必须是**归一化后**的 struct，不是线上那些数。
	if u, ok := rec.first(KindUsage); !ok {
		t.Error("no usage event")
	} else if u.Usage == nil {
		t.Error("usage event has a nil Usage")
	} else if *u.Usage != b4WantUsage {
		t.Errorf("usage event\n got %+v\nwant %+v", *u.Usage, b4WantUsage)
	}

	if end, ok := rec.first(KindResponseEnd); !ok {
		t.Error("no response_end")
	} else if end.FinishReason != "tool_calls" {
		t.Errorf("response_end finish reason got %q, want %q", end.FinishReason, "tool_calls")
	}
}

func TestEventSequenceForB7Reasoning(t *testing.T) {
	rec := &sseRecorder{}
	if _, err := parseOpenAIStream(sseBody(b7ReasoningStream...), NewBus(rec), 2, time.Now()); err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}

	want := []Kind{
		KindFirstToken, // 第一条 reasoning delta，不是 role 开场帧
		KindReasoningDelta, KindReasoningDelta, KindReasoningDelta, KindReasoningDelta, KindReasoningDelta,
		KindTextDelta, KindTextDelta,
		KindUsage,
		KindResponseEnd,
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("event kinds\n got %v\nwant %v", got, want)
	}
	if n := rec.count(KindFirstToken); n != 1 {
		t.Errorf("KindFirstToken emitted %d times, want exactly 1", n)
	}

	// 渲染器只靠 kind 来区分"在想"和"在说"，所以一段 reasoning 要是当成文本
	// delta 漏出去，就等于把模型的私人草稿纸打给用户看。
	if e, ok := rec.first(KindReasoningDelta); !ok {
		t.Error("no reasoning_delta")
	} else if e.Text != "Okay" {
		t.Errorf("first reasoning delta got %q, want %q", e.Text, "Okay")
	}
	if e, ok := rec.first(KindTextDelta); !ok {
		t.Error("no text_delta")
	} else if e.Text != "17 * 23 = " {
		t.Errorf("first text delta got %q, want %q", e.Text, "17 * 23 = ")
	}
}

func TestParallelToolCallEventsAreRoutableByID(t *testing.T) {
	rec := &sseRecorder{}
	if _, err := parseOpenAIStream(sseBody(parallelToolCallStream...), NewBus(rec), 1, time.Now()); err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}

	// 两个 start，按到达顺序——排序是对返回的结果做的，不是对实时的事件流
	// 做的；事件流必须保持线上顺序，渲染器才能边发生边显示。
	if n := rec.count(KindToolCallStart); n != 2 {
		t.Fatalf("want 2 tool_call_start events, got %d", n)
	}

	// 每条 args delta 都得报出自己属于哪次调用，否则同时开着两个调用的渲染
	// 器分不出某个碎片该进哪个框。
	byID := map[string]string{}
	for _, e := range rec.events {
		if e.Kind == KindToolArgsDelta {
			if e.ToolID == "" {
				t.Fatal("tool_args_delta with no tool id: fragments are unroutable")
			}
			byID[e.ToolID] += e.Text
		}
	}
	want := map[string]string{
		"call_first":  `{"command": "ls -la"}`,
		"call_second": `{"command": "echo two"}`,
	}
	if !reflect.DeepEqual(byID, want) {
		t.Errorf("args reassembled from events\n got %#v\nwant %#v", byID, want)
	}
}

// ---------------------------------------------------------------------------
// TTFT。
// ---------------------------------------------------------------------------

func TestTTFTMeasuresFromTheRequest(t *testing.T) {
	// 假装请求是 1.5 秒前发出去的。把 `started` 往前挪，就是这条断言不用
	// sleep 的办法：TTFT 是从调用方给的那个时刻起算的时长，所以测试可以自己
	// 挑这个时刻。
	started := time.Now().Add(-1500 * time.Millisecond)

	rec := &sseRecorder{}
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(rec), 1, started)
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}

	if got.TTFT < 1400*time.Millisecond || got.TTFT > 3*time.Second {
		t.Errorf("TTFT got %v, want about 1.5s", got.TTFT)
	}
	e, ok := rec.first(KindFirstToken)
	if !ok {
		t.Fatal("no first_token event")
	}
	if e.Millis < 1400 || e.Millis > 3000 {
		t.Errorf("first_token Millis got %d, want about 1500", e.Millis)
	}
	if e.Millis != got.TTFT.Milliseconds() {
		t.Errorf("first_token Millis %d disagrees with result TTFT %d", e.Millis, got.TTFT.Milliseconds())
	}
}

func TestTTFTIsZeroWhenNothingStreamed(t *testing.T) {
	// role 开场帧带的是 `content: ""`，finish chunk 带的是空 delta。两者都不
	// 是输出，所以没有第一个 token 可计时——而响应什么都没产出，却给它报一
	// 个看着挺像样的 TTFT，那比什么都不报更糟。
	rec := &sseRecorder{}
	got, err := parseOpenAIStream(sseBody(b4RoleOpener, b4Finish, b4Usage, b4Done), NewBus(rec), 1, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if got.TTFT != 0 {
		t.Errorf("TTFT got %v, want 0", got.TTFT)
	}
	if n := rec.count(KindFirstToken); n != 0 {
		t.Errorf("first_token emitted %d times for a stream with no output, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 坏掉的流。
// ---------------------------------------------------------------------------

func TestMalformedFrameIsSurvivedAndReported(t *testing.T) {
	// 中间来一帧坏帧，不能让我们赔上一次已经完成的工具调用。它也不能就这么
	// 无声无息地过去——发条 notice，它就进了 trace，以后还找得到。
	frames := append([]string{}, b4ToolCallStream[:2]...)
	frames = append(frames, `{"choices":[{"delta":`) // 截断的 JSON
	frames = append(frames, b4ToolCallStream[2:]...)

	rec := &sseRecorder{}
	got, err := parseOpenAIStream(sseBody(frames...), NewBus(rec), 1, time.Now())
	if err != nil {
		t.Fatalf("a single bad frame should not fail the stream: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Args != b4WantArgs {
		t.Errorf("tool call damaged by the bad frame: %#v", got.ToolCalls)
	}
	if n := rec.count(KindNotice); n != 1 {
		t.Errorf("bad frame produced %d notices, want 1", n)
	}
}

func TestTruncatedStreamReturnsPartialResultAndError(t *testing.T) {
	boom := errors.New("connection reset by peer")

	// 一次完整的工具调用，然后 socket 在 finish chunk 之前断了。这是阶段 01
	// 那堂截断课的流式版本：危险的不是错误本身，而是调用方无视它，把一个没
	// 有 finish_reason 的结果当成模型是主动停下的，就这么交出去。
	good := sseBody(b4ToolCallStream[:9]...)
	rec := &sseRecorder{}

	got, err := parseOpenAIStream(io.MultiReader(good, iotest.ErrReader(boom)), NewBus(rec), 1, time.Now())
	if !errors.Is(err, boom) {
		t.Fatalf("want the read error back, got %v", err)
	}
	if got == nil {
		t.Fatal("partial result was thrown away: the caller cannot tell a dead stream from an empty one")
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Args != b4WantArgs {
		t.Errorf("partial result lost the completed tool call: %#v", got.ToolCalls)
	}
	if got.FinishReason != "" {
		t.Errorf("FinishReason got %q, want empty: the stream never sent one", got.FinishReason)
	}
	if n := rec.count(KindResponseEnd); n != 0 {
		t.Errorf("response_end emitted %d times for a broken stream, want 0", n)
	}
}

func TestNilBusIsTolerated(t *testing.T) {
	// 不带订阅者地解析，就是测试、或者某个批处理工具用它的方式——不用先支起
	// 一条总线。
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), nil, 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Args != b4WantArgs {
		t.Errorf("nil bus changed the result: %#v", got.ToolCalls)
	}
}
