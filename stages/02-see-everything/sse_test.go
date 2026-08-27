// SSE 读取器和 OpenAI 流解析器的测试。
//
// 下面的每个帧常数都从 docs/wire-notes.md 复制出——这些是这个端点
// 实际发送的字节，不是为了让解析器看起来好而发明的字节。这才是
// 重点：一个照着规范写出来的夹具，测试的只是你对规范的理解，而
// 这个端点本身就不符合规范（见 §B4 帧 11 和 13）。夹具必须被重构
// 或发明的地方，上面的注释会说明这一点，并说明原因。
//
// 没有网络，没有 API 密钥，没有 `-short` 跳过。整个文件在飞机上
// 也能跑。
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
// §B4——完整的 13 帧工具调用流，按顺序。
//
// 产生它的请求：`bash` 工具，tool_choice:"required"，
// reasoning_effort:"none"，prompt "调用 bash 工具一次，命令设置为：
// ls -la /srv/app"。
//
// 帧 1、10、11、12 和 13 在 §B4 中被完整记录，并被逐字复制。
// 帧 2–9 在那里只被记录为 `delta` 对象；它们周围的信封，是根据
// 完整的帧 1 和帧 10 重构出来的。`delta` 对象本身——包括每个
// 显式的 `null`——都是逐字的。
// ---------------------------------------------------------------------------

const (
	// 1. 角色开启。注意 `content` 是""，不是 null，它不携带任何
	//    载荷：这个帧就是为什么 TTFT 不得从第一个接收的帧测量。
	b4RoleOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}`

	// 2. 工具调用开启——**唯一**携带 `id` 和 `function.name` 的块。
	b4ToolOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_8d4f0377bc594026a4765cfc","type":"function","function":{"name":"bash","arguments":""}}]}}]}`

	// 3.–9. 参数片段。`id` 和 `function.name` 现在显式是 null，`index` 保持 0，
	//       `type` 保持"function"——它不是 null，这恰好就是为什么
	//       "键在那里"证明不了什么。
	//
	//       分裂不是 JSON 对齐的：片段 1 在对象中途结束，片段 4 在
	//       路径中途结束（`/srv`），片段 5 把它接上（`/app`）。
	b4Arg1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": "}}]}}]}`
	b4Arg2 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"\""}}]}}]}`
	b4Arg3 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"ls"}}]}}]}`
	b4Arg4 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":" -la /srv"}}]}}]}`
	b4Arg5 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"/app"}}]}}]}`
	b4Arg6 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"\""}}]}}]}`
	b4Arg7 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"}"}}]}}]}`

	// 10. 完成块——空 delta，finish_reason 设置。
	b4Finish = `{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`

	// 11. 使用情况块。`choices` 是一个**空数组**。任何伸手去够 choices[0]
	//     的代码都会在这里当场崩溃——就在每个真实请求的倒数第二帧上。
	//     （§B5：这个帧默认就存在，不需要发送 stream_options——发送了
	//     stream_options 也不会改变什么。）
	b4Usage = `{"id":"...","object":"chat.completion.chunk","created":1787768844,"model":"mimo-v2.5","choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":0}}}`

	// 12. 哨兵。
	b4Done = `[DONE]`

	// 13. 哨兵**之后**的一个帧。每个规范兼容的客户端丢弃它。
	//     `choices` 这里也是空的。
	b4PostDone = `{"choices":[],"cost":"0"}`
)

// b4ToolCallStream 是 §B4 端到端，按记录的顺序。
var b4ToolCallStream = []string{
	b4RoleOpener,
	b4ToolOpener,
	b4Arg1, b4Arg2, b4Arg3, b4Arg4, b4Arg5, b4Arg6, b4Arg7,
	b4Finish,
	b4Usage,
	b4Done,
	b4PostDone,
}

// b4WantArgs 是 §B4 说片段连接到的东西。
const b4WantArgs = `{"command": "ls -la /srv/app"}`

// b4WantUsage 是帧 11 在 sseUsage.normalise 描述的方向反转之后的样子：
// prompt_tokens 506 **包含**了 cached_tokens 192，所以全价 Input 是
// 两者的差，而 Prompt() 最终必须仍然等于 506。
var b4WantUsage = Usage{Input: 314, CacheRead: 192, Output: 26, Reasoning: 0}

// ---------------------------------------------------------------------------
// §B7——推理和文本在同一个 delta 对象上。
//
// 五个 `reasoning_content` delta 和角色开启，是重构信封里逐字照抄
// 的 §B7 `delta` 对象。§B7 记录了这个运行有 44 个推理帧和 1 个
// content 帧，但没有打印出 content 帧，所以这里的两个 `content` 帧
// 被构建成同一种形状——足以证明这两个字段落在两个不同的累积器
// 里，这正是要测试的东西。
// ---------------------------------------------------------------------------

const (
	b7RoleOpener = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}`
	b7Reason1    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":"Okay","tool_calls":null}}]}`
	b7Reason2    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":", the","tool_calls":null}}]}`
	b7Reason3    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":" user is asking for","tool_calls":null}}]}`
	b7Reason4    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":" the product of ","tool_calls":null}}]}`
	b7Reason5    = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":"17 and ","tool_calls":null}}]}`

	// 构建，不是记录——见上面的块注释。
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
// 两个并行工具调用。
//
// **构建，不是记录**：§B4 捕获的是一个单调用流，§D12 只证明了
// `parallel_tool_calls:false` 会被接受但被忽视，所以并行调用是可能
// 到达的，只是没有逐字的实录。块的形状完全照抄 §B4——唯一改变的
// 是 `index`、ids 和片段文本。
//
// 两个故意的扭曲，都是为了让 bug 一定可见，而不是只是可能出现：
//
//   - index 1 在 index 0 **之前**打开，所以按到达顺序返回调用的
//     实现，会每次都失败，而不是只有一半时间失败。
//   - 片段是交错的，所以追加到单一共享缓冲区的实现，会产生明显
//     可见的垃圾，而不是一次不易察觉的混乱。
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

// 辅助函数。

// sseBody 渲染载荷的方式，和 §B4 里这个端点渲染它们的方式一样：
// `data: <payload>`，然后一个空行，以 LF 结尾（文档用 `cat -A`
// 显示过，每行结尾是 `$`，没有出现 `^M`）。
func sseBody(frames ...string) io.Reader {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return strings.NewReader(b.String())
}

// sseRecorder 是一个保留所有东西的 Subscriber，这是演示"Agent 核心
// 为什么发出事件而不是直接打印"成本最低的方式：测试只对事件序列
// 做断言，从不碰 stdout。
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

// readSSE：只管分帧。

func TestReadSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []sseFrame
	}{
		{
			// 阶段实际遇到的形状：只有 `data:`，没有别的。
			name: "openai style, data lines only",
			in:   "data: a\n\ndata: b\n\n",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "b"}},
		},
		{
			// 这一点在这个端点上没有被观察到——它发送的是裸 LF。但 SSE 的
			// 规范建立在 CRLF 之上，路径中的任何代理都可能重写行尾，所以一个
			// 只处理 LF 的解析器，会在每个载荷末尾留下一个多余的 \r，导致
			// JSON 解码失败。
			name: "CRLF line endings",
			in:   "data: a\r\n\r\ndata: b\r\n\r\n",
			want: []sseFrame{{Name: "", Data: "a"}, {Name: "", Data: "b"}},
		},
		{
			// 阶段 03 需要的东西（§B6）。这个阶段本身不产生它。
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
			// Keep-alive。它们不能终止正在进行的帧，也不能产生自己的帧。
			name: "comment lines are ignored",
			in:   ": keep-alive\ndata: a\n: mid-frame comment\ndata: b\n\n: trailing\n\n",
			want: []sseFrame{{Name: "", Data: "a\nb"}},
		},
		{
			// 这里要防的 bug 是无声的：流的最后一帧，往往就是携带使用情况的那一帧。
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
			// 这条线上的每个载荷都是 JSON，所以按最后一个冒号拆分（或者按所有冒号拆
			// 分），都会破坏每一帧。
			name: "only the first colon separates field from value",
			in:   "data: {\"model\":\"mimo-v2.5\",\"t\":\"12:34:56\"}\n\n",
			want: []sseFrame{{Name: "", Data: `{"model":"mimo-v2.5","t":"12:34:56"}`}},
		},
		{
			// 规范字段，用于恢复一个掉线的流。故意被忽视，但不能被误认成数据。
			name: "id and retry fields are ignored, not treated as data",
			in:   "id: 42\nretry: 3000\ndata: a\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			// 根据规范，重要的是：事件类型缓冲区必须重置，否则名字
			// 泄漏到下一帧。
			name: "a frame with no data line is not dispatched and does not leak its name",
			in:   "event: ping\n\ndata: a\n\n",
			want: []sseFrame{{Name: "", Data: "a"}},
		},
		{
			// readSSE 对哨兵一无所知。决定 [DONE] 是什么意思，是载荷解析器的
			// 工作，正是这一点，让这一半代码对一个根本没有哨兵的协议
			// （§B6）也能保持可重用。
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
	// bufio.Scanner 到 64KB 就会失败，报 ErrTooLong。这么大的单个 delta
	// 不是假设出来的：这就是一次大文件 `cat`，经工具结果回显后，在
	// 传出去的路上会呈现的样子。
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
// parseOpenAIStream：记录的流，端到端。
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
			// 头号用例：§B4 逐字重现，全部 13 帧。
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
			// §B7：一个 delta 对象上的两个字段必须在两个地方。
			name:          "B7 reasoning and text are kept apart",
			frames:        b7ReasoningStream,
			wantText:      "17 * 23 = 391",
			wantReasoning: "Okay, the user is asking for the product of 17 and ",
			wantFinish:    "stop",
			wantUsage:     b4WantUsage,
		},
		{
			// 让 choices[0] 解析器崩溃的帧，独自。
			name:      "usage frame alone, choices is an empty array",
			frames:    []string{b4Usage},
			wantUsage: b4WantUsage,
		},
		{
			// §B4 帧 13，单独来看：空 choices，**并且**带一个未知的顶级键。
			// 正因为它是在哨兵之后到达的，才让人很容易从来没测试过这种
			// 情况。
			name:   "post-DONE cost frame alone",
			frames: []string{b4Done, b4PostDone},
		},
		{
			// 在哨兵之后继续排空，只有在它确实取回了什么东西时，才站得住脚。
			// 把使用情况挪到 [DONE] 后面——这就是正确记账和无声报零之间的
			// 差别所在。
			name:       "frames after the sentinel are still read",
			frames:     []string{b4RoleOpener, b4Finish, b4Done, b4Usage, b4PostDone},
			wantFinish: "tool_calls",
			wantUsage:  b4WantUsage,
		},
		{
			// 并行调用：独立累积，升序索引顺序。
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
			// 一个什么都没产生的流，也必须干干净净地返回，而不是停在半初始化的状态。
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

// TestB4ArgsReassembleIntoValidJSON 是"从不解析单个片段"这件事的
// 回报。§B4 里的七个片段没有一个单独是合法 JSON；拼接后的结果才
// 是，而这也是唯一允许解析发生的地方。
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

// TestToolIDSurvivesTheNullChunks 是 id 锁定的回归测试，单独列出来，
// 好让失败信息指向真正的病根。帧 3–9 都携带 `"id":null`；不加防范
// 的赋值会让它留空，工具调用就变得无法回答，因为 API 要求回复里
// 带回那个 id。
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

// TestParallelToolCallsComeBackInIndexOrder 把同一个流跑了很多遍，
// 因为 Go 故意把 map 的迭代顺序随机化了。只跑一次，大约有一半
// 的机会能捕捉到缺失的排序——一个每两次提交里就有一次失败的
// 测试，比根本没有测试还糟，因为它教会大家的是重新跑一遍 CI。
// 跑二十次，能把误判通过的概率压到大约百万分之一。
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
// 使用情况规范化——方向反转。
// ---------------------------------------------------------------------------

func TestUsageNormalisation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Usage
	}{
		{
			// §B4 帧 11，逐字照录。506 是**完整**的 prompt，里面包含了 192
			// 个缓存 token，所以全价 Input 是 314，Prompt() 必须仍然算出 506。
			name: "B4 frame 11 without stream_options",
			in:   b4Usage,
			want: Usage{Input: 314, CacheRead: 192, Output: 26, Reasoning: 0},
		},
		{
			// §B5：同一个请求 **WITH** stream_options:{include_usage:true}。
			// 参数是 no-op；只有 cached_tokens 不同，它不同因为缓存状态
			// 在运行间变化。
			name: "B5 frame 11 with stream_options, a no-op",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":448},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 58, CacheRead: 448, Output: 26},
		},
		{
			// 冷请求：什么都没缓存，所以 Input 是整个 prompt。
			name: "no cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 506, Output: 26},
		},
		{
			// 推理是 completion_tokens 的一个**子集**，不是外加的东西。
			name: "a thinking model reports reasoning inside completion",
			in:   `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":900,"total_tokens":1000,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":850}}}`,
			want: Usage{Input: 60, CacheRead: 40, Output: 900, Reasoning: 850},
		},
		{
			// 细节对象整个是 null。这个端点的每个字段都可能是 null，所以
			// 解析器得扛得住——结果是零，不是崩溃，也不是负数。
			name: "null detail objects",
			in:   `{"choices":[],"usage":{"prompt_tokens":80,"completion_tokens":9,"total_tokens":89,"prompt_tokens_details":null,"completion_tokens_details":null}}`,
			want: Usage{Input: 80, Output: 9},
		},
		{
			// 防卫性写法：缓存比 prompt 还多，这在算术上是不可能的，但导出
			// 一个负的 token 计数，会连累 Prompt() 和下游每一个成本估计。
			// 限制住，然后继续。
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

// TestUsagePromptRoundTrips 陈述的这个不变量，让人不用再做一次
// 减法，就能检验这个反转对不对：无论怎么拆分，Prompt() 都必须
// 等于端点报告的 prompt_tokens。
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
// 事件流。这些是事件总线存在的测试。
// ---------------------------------------------------------------------------

func TestEventSequenceForB4ToolCall(t *testing.T) {
	rec := &sseRecorder{}
	if _, err := parseOpenAIStream(sseBody(b4ToolCallStream...), NewBus(rec), 7, time.Now()); err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}

	want := []Kind{
		KindFirstToken,    // 帧 2：工具调用开启是第一个真实载荷
		KindToolCallStart, // 同一帧，一旦 id 和 name 被锁定
		// 帧 3-9。开启端的 `"arguments":""` 什么都不产生，这就是为什么
		// 这里是七个，而不是八个。
		KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta,
		KindToolArgsDelta, KindToolArgsDelta, KindToolArgsDelta,
		KindUsage,       // 帧 11，空 choices 的
		KindResponseEnd, // 在排空帧 12 和 13 之后
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("event kinds\n got %v\nwant %v", got, want)
	}

	if n := rec.count(KindFirstToken); n != 1 {
		t.Errorf("KindFirstToken emitted %d times, want exactly 1", n)
	}

	// 帧 1 携带 `content: ""`，这不是 token。如果 TTFT 是从它测量的，
	// first_token 就会在模型生成任何东西之前到达，数字会奉承每一个
	// 请求。
	if start, ok := rec.first(KindToolCallStart); !ok {
		t.Error("no tool_call_start")
	} else if start.ToolID != "call_8d4f0377bc594026a4765cfc" || start.ToolName != "bash" {
		t.Errorf("tool_call_start got id=%q name=%q", start.ToolID, start.ToolName)
	}

	// 每个事件都携带着回合，这样 trace 就能按往返拆分，不用重新
	// 推导任何东西。
	for _, e := range rec.events {
		if e.Turn != 7 {
			t.Fatalf("event %s has turn %d, want 7", e.Kind, e.Turn)
		}
	}

	// 使用情况事件得携带**规范化的**结构，不是线上数字。
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
		KindFirstToken, // 第一个推理 delta，不是角色开启
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

	// 渲染器仅凭 kind 就能把思考和说话区分开，所以一旦推理片段当作
	// 文本 delta 泄露出去，就等于把模型的私密草稿纸打印给用户看。
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

	// 两个启动，按到达顺序——这个排序只适用于返回的结果，不适用于
	// 实时事件流；实时事件流必须保持线上顺序，这样渲染器才能按事情
	// 发生的样子实时显示出来。
	if n := rec.count(KindToolCallStart); n != 2 {
		t.Fatalf("want 2 tool_call_start events, got %d", n)
	}

	// 每个 args delta 得命名它的调用，否则一个有两个调用打开的
	// 渲染器不能说一个片段属于哪个框。
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
	// 假装请求是 1.5 秒前发出的。把 `started` 的时间往回调，就是不用
	// sleep 也能断言这一点的办法：TTFT 是从调用者提供的某个时刻算起
	// 的一段时长，所以测试可以自己选定这个时刻。
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
	// 角色开启携带 `content: ""`，完成块携带一个空 delta。两者都不算
	// 输出，所以没有第一个 token 可供计时——对一个什么都没产生的
	// 响应，报告一个看似合理的 TTFT，比干脆不报告还要糟糕。
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
// 破坏的流。
// ---------------------------------------------------------------------------

func TestMalformedFrameIsSurvivedAndReported(t *testing.T) {
	// 中间一帧坏帧，不能让我们损失一个已经完成的工具调用。它也不能
	// 悄无声息地就这么过去——发一条通知，把它记进 trace，这样以后
	// 就能找到它。
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

	// 一个完整的工具调用，接着套接字在完成块到达之前就死掉了。
	// 这是阶段 01 截断教训的流式版本：危险的不是这个错误本身，而是
	// 调用者忽视了它，把一个没有 finish_reason 的结果发出去，就好像
	// 模型是故意停下来的一样。
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
	// 不带订阅者进行解析，就是测试或批处理工具在不搭建总线的
	// 情况下使用这个函数的方式。
	got, err := parseOpenAIStream(sseBody(b4ToolCallStream...), nil, 1, time.Now())
	if err != nil {
		t.Fatalf("parseOpenAIStream: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Args != b4WantArgs {
		t.Errorf("nil bus changed the result: %#v", got.ToolCalls)
	}
}
