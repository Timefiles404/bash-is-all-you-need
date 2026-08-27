// OpenAI 协议适配器的测试。
//
// 下面的每个帧常数都是从 docs/wire-notes.md
// 复制出来的——这些是这个端点实际发送的
// 字节，不是为了让解析器看起来好看而编造的。
// 这就是全部目的：一个你从规范写出来的
// fixture 测试你对规范的理解，而这个端点
// 与规范不匹配（见 §B4 帧 11 和 13）。如果
// fixture 必须重建或编造，上面的注释会说
// 明这一点以及为什么。
//
// 从 stage 02 的 sse_test.go 移植而来。这些
// fixture 逐字未改；改变的是被测试的表面
// ——一个 Provider 而不是自由函数——以及
// 文件中全新的那部分，它测试 stage 02 从未
// 命名过的方向：中立对话向外到这条线上。
//
// 命名说明：这里的帮助程序和framing测试带有
// `openai` 前缀，因为 anthropic.go 的测试
// 共享这个包。readSSE 本身是中立的，存在于
// sse.go 中；framing 情况被放在这里，以便
// 从 stage 02 移植不会失去覆盖率，而不是
// 因为 framing 是这个协议的事务。
//
// 无网络、无 API 密钥、无 `-short` 跳过。
// 整个文件在飞机上运行。
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

// b4WantCalls 是同一个调用，以 Provider
// 返回的中立形状。
var b4WantCalls = []Block{{
	Kind: BlockToolCall,
	ID:   "call_8d4f0377bc594026a4765cfc",
	Name: "bash",
	Args: b4WantArgs,
}}

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
	openaiParOpener1 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":"call_second","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	openaiParOpener0 = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"bash","arguments":""}}]}}]}`
	openaiParArg1a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"echo "}}]}}]}`
	openaiParArg0a   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":"{\"command\": \"ls"}}]}}]}`
	openaiParArg1b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":1,"id":null,"type":"function","function":{"name":null,"arguments":"two\"}"}}]}}]}`
	openaiParArg0b   = `{"choices":[{"index":0,"finish_reason":null,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":null,"type":"function","function":{"name":null,"arguments":" -la\"}"}}]}}]}`
)

var openaiParallelStream = []string{
	b4RoleOpener,
	openaiParOpener1, openaiParOpener0,
	openaiParArg1a, openaiParArg0a,
	openaiParArg1b, openaiParArg0b,
	b4Finish,
	b4Usage,
	b4Done,
	b4PostDone,
}

// openaiParallelWantCalls 是这一对，按升序
// 索引顺序——线上顺序反向，那是重点。
var openaiParallelWantCalls = []Block{
	{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
	{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
}

// 辅助函数。

// openaiTestProvider 是被测试的 provider。
// 基础 URL 有意地带有尾部斜杠：在测试中
// 手工构建的 provider 必须像 config.go 一样
// 修剪它，否则端点变成 `.../v1//chat/completions`。
func openaiTestProvider() *openaiProvider {
	return newOpenAIProvider("https://opencode.ai/zen/go/v1/", "sk-test-key", "mimo-v2.5")
}

// openaiBody 按照 §B4 显示的这个端点渲染方式
// 呈现负载：`data: <payload>` 然后是一个空行，
// LF 终止（文档用 `cat -A` 显示它，每行末尾
// 是 `$`，没有 `^M` 出现）。
func openaiBody(frames ...string) io.Reader {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return strings.NewReader(b.String())
}

// openaiRecorder 是一个 Subscriber，保留
// 一切，这是演示 Agent 核心为什么发出事件
// 而不是打印的最便宜方式：测试对事件序列
// 做断言，从不接触 stdout。
type openaiRecorder struct{ events []Event }

func (r *openaiRecorder) OnEvent(e Event) { r.events = append(r.events, e) }

func (r *openaiRecorder) kinds() []Kind {
	out := make([]Kind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *openaiRecorder) count(k Kind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (r *openaiRecorder) first(k Kind) (Event, bool) {
	for _, e := range r.events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

// BuildRequest 的解码端。有意地与 openai.go
// 中的结构集分离：用产生字节的同一类型做
// 断言会让错误的 json tag 自己同意并通过。
type openaiWireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiWireMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCallID string               `json:"tool_call_id"`
	ToolCalls  []openaiWireToolCall `json:"tool_calls"`
}

type openaiWireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type openaiWireRequest struct {
	Model         string `json:"model"`
	MaxTokens     int    `json:"max_tokens"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []openaiWireMessage `json:"messages"`
	Tools    []openaiWireTool    `json:"tools"`
}

func decodeOpenAIRequest(t *testing.T, body []byte) openaiWireRequest {
	t.Helper()
	var got openaiWireRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not valid JSON: %v\n%s", err, body)
	}
	return got
}

// roles 是消息角色序列，这是在一个断言中
// 陈述整个对话形状的最便宜方式。
func (r openaiWireRequest) roles() []string {
	out := make([]string, 0, len(r.Messages))
	for _, m := range r.Messages {
		out = append(out, m.Role)
	}
	return out
}

// openaiBashSchema 是 stage 02 发布的工具
// schema，在这里用作一个现实的嵌套值，而不是
// 一个单键桩。
func openaiBashSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}
}

func openaiBashTool() Tool {
	return Tool{
		Name:        "bash",
		Description: "Execute a bash command and return its stdout, stderr and exit code.",
		Schema:      openaiBashSchema(),
	}
}

// ---------------------------------------------------------------------------
// Provider 表面本身。
// ---------------------------------------------------------------------------

func TestOpenAIProviderIdentity(t *testing.T) {
	p := openaiTestProvider()

	// Protocol() 被写入 trace，所以它是一个稳定
	// 的字符串，而不是稍后要美化的显示标签。
	if got := p.Protocol(); got != "openai" {
		t.Errorf("Protocol() got %q, want %q", got, "openai")
	}
	if got := p.Model(); got != "mimo-v2.5" {
		t.Errorf("Model() got %q, want %q", got, "mimo-v2.5")
	}
	if p.baseURL != "https://opencode.ai/zen/go/v1" {
		t.Errorf("trailing slash not trimmed: baseURL is %q", p.baseURL)
	}
}

// ---------------------------------------------------------------------------
// BuildRequest——中立对话向外到这条线上。
// ---------------------------------------------------------------------------

func TestOpenAIBuildRequestEnvelope(t *testing.T) {
	p := openaiTestProvider()

	req, body, err := p.BuildRequest("sys", []Msg{TextMsg(RoleUser, "hi")}, []Tool{openaiBashTool()}, 4096)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	if req.Method != "POST" {
		t.Errorf("method got %q, want POST", req.Method)
	}
	if got, want := req.URL.String(), "https://opencode.ai/zen/go/v1/chat/completions"; got != want {
		t.Errorf("URL\n got %q\nwant %q", got, want)
	}
	for _, h := range []struct{ key, want string }{
		{"Authorization", "Bearer sk-test-key"},
		{"Accept", "text/event-stream"},
		{"Content-Type", "application/json"},
	} {
		if got := req.Header.Get(h.key); got != h.want {
			t.Errorf("header %s got %q, want %q", h.key, got, h.want)
		}
	}

	// 返回的字节必须是请求将发送的字节。调用者
	// 将这些作为 KindRequest 发出，而请求检查器
	// 显示的内容不是线上发出的内容比没有检查器
	// 更糟：它是那些说谎的证据。
	if req.GetBody == nil {
		t.Fatal("request has no GetBody: the body cannot be replayed or compared")
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	sent, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the request body: %v", err)
	}
	if string(sent) != string(body) {
		t.Errorf("returned body is not the body being sent\n sent %s\n got  %s", sent, body)
	}

	got := decodeOpenAIRequest(t, body)
	if got.Model != "mimo-v2.5" {
		t.Errorf("model got %q, want %q", got.Model, "mimo-v2.5")
	}
	if got.MaxTokens != 4096 {
		t.Errorf("max_tokens got %d, want 4096", got.MaxTokens)
	}
	if !got.Stream {
		t.Error("stream is not true: this adapter only knows how to read an SSE body")
	}

	// §B5：在这个网关上可测量地是无操作——有或
	// 没有它都是同样的 13 帧。无论如何都要发送，
	// 因为真实的 OpenAI 端点在没有它的情况下
	// 不会流式传输使用情况，而一个指向其他地方
	// 的那天报告零 token 的 Agent 是这一行
	// 防止的失败。
	if got.StreamOptions == nil {
		t.Fatal("stream_options missing: correct here, wrong on every other OpenAI-compatible endpoint")
	}
	if !got.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage got false, want true")
	}
}

func TestOpenAIBuildRequestSystemPromptIsTheFirstMessage(t *testing.T) {
	p := openaiTestProvider()
	const sys = "You are a coding agent working in a terminal."

	_, body, err := p.BuildRequest(sys, []Msg{TextMsg(RoleUser, "list the repo")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	// **非对称性**。在这个协议上，系统提示词是
	// messages[0]。在 Anthropic 协议上，它是
	// 顶级 `system` 字段，根本不能是消息——这
	// 就是为什么 Provider.BuildRequest 把它当作
	// 自己的参数，而不是让调用者把它推入历史中。
	if want := []string{"system", "user"}; !reflect.DeepEqual(got.roles(), want) {
		t.Fatalf("roles\n got %v\nwant %v", got.roles(), want)
	}
	if got.Messages[0].Content != sys {
		t.Errorf("messages[0].content\n got %q\nwant %q", got.Messages[0].Content, sys)
	}

	// 而它也**不能**作为顶级字段出现：两个都
	// 发送是两个适配器之间的共享结构如何将一个
	// 协议泄漏到另一个协议中。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["system"]; ok {
		t.Error("top-level `system` key present: that is the other protocol's shape")
	}

	// 空系统提示词根本不产生消息，而不是空的
	// 消息，所以没有系统提示词的调用者不会
	// 无声地发送一个计入上下文的空回合。
	_, body, err = p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if want := []string{"user"}; !reflect.DeepEqual(decodeOpenAIRequest(t, body).roles(), want) {
		t.Errorf("empty system prompt still produced a message: %v", decodeOpenAIRequest(t, body).roles())
	}
}

// TestOpenAIBuildRequestToolResultsBecomeSeparateMessages
// 是两个适配器之间的头条差异，表述为一个
// 断言。
//
// **一个中立消息里的三个工具结果，在这里变成三个**
// `role:"tool"` 消息。Anthropic 适配器将相同的输入折叠成
// **一个**用户消息，携带三个 tool_result 块。两种形状都
// 不能是中立的，这就是为什么 provider.go 没有 RoleTool
// 而工具结果是块。
func TestOpenAIBuildRequestToolResultsBecomeSeparateMessages(t *testing.T) {
	p := openaiTestProvider()

	results := Msg{Role: RoleUser, Blocks: []Block{
		ToolResultBlock("call_a", "total 0\n[exit 0 · 4ms]"),
		ToolResultBlock("call_b", "[no output]\n[exit 1 · 2ms]"),
		ToolResultBlock("call_c", "[the user denied this command. Do not retry it unchanged.]"),
	}}

	_, body, err := p.BuildRequest("", []Msg{results}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if want := []string{"tool", "tool", "tool"}; !reflect.DeepEqual(got.roles(), want) {
		t.Fatalf("three results must produce three messages\n got %v\nwant %v", got.roles(), want)
	}

	wantPairs := [][2]string{
		{"call_a", "total 0\n[exit 0 · 4ms]"},
		{"call_b", "[no output]\n[exit 1 · 2ms]"},
		{"call_c", "[the user denied this command. Do not retry it unchanged.]"},
	}
	for i, want := range wantPairs {
		m := got.Messages[i]
		if m.ToolCallID != want[0] {
			t.Errorf("messages[%d].tool_call_id got %q, want %q", i, m.ToolCallID, want[0])
		}
		if m.Content != want[1] {
			t.Errorf("messages[%d].content\n got %q\nwant %q", i, m.Content, want[1])
		}
		// 连接实现通过 id 检查并在这里失败，这是
		// 值得命名的失败模式：模型得到一个三个输出
		// 的 blob，都归属于一个调用。
		if strings.Contains(m.Content, "\x00") {
			t.Errorf("messages[%d].content looks concatenated: %q", i, m.Content)
		}
	}
}

// TestOpenAIBuildRequestFullConversationOrder 固定
// 真实回合的形状：系统、用户请求、Assistant
// 的工具调用、答案、下一个用户消息。工具结果
// 必须落在请求它们的 Assistant 消息**之后**，
// 否则 API 无法将它们配对。
func TestOpenAIBuildRequestFullConversationOrder(t *testing.T) {
	p := openaiTestProvider()

	msgs := []Msg{
		TextMsg(RoleUser, "list /srv/app"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Running both now."},
			{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
			{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
		}},
		{Role: RoleUser, Blocks: []Block{
			ToolResultBlock("call_first", "total 0"),
			ToolResultBlock("call_second", "two"),
		}},
		TextMsg(RoleUser, "thanks"),
	}

	_, body, err := p.BuildRequest("sys", msgs, []Tool{openaiBashTool()}, 4096)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	want := []string{"system", "user", "assistant", "tool", "tool", "user"}
	if !reflect.DeepEqual(got.roles(), want) {
		t.Errorf("conversation shape\n got %v\nwant %v", got.roles(), want)
	}
}

// TestOpenAIBuildRequestAssistantReplayRoundTrip
// 涵盖了重组税：一个流式 Assistant 回合必须
// 回到历史中作为非流式 API 会返回的消息，
// 否则下一个请求有工具调用但没有谁做了
// 它们的记录。
func TestOpenAIBuildRequestAssistantReplayRoundTrip(t *testing.T) {
	p := openaiTestProvider()

	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "Running both now."},
		// 在出口处被丢弃：这个协议没有推理的
		// 入站字段，所以重放它会被忽略或拒绝，
		// 取决于另一端的实现是谁的。
		{Kind: BlockThinking, Text: "The user wants two commands."},
		{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
		{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
	}}

	_, body, err := p.BuildRequest("", []Msg{assistant}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Messages) != 1 {
		t.Fatalf("want one assistant message, got %d: %v", len(got.Messages), got.roles())
	}
	m := got.Messages[0]
	if m.Role != "assistant" {
		t.Errorf("role got %q, want assistant", m.Role)
	}
	if m.Content != "Running both now." {
		t.Errorf("content\n got %q\nwant %q", m.Content, "Running both now.")
	}
	if strings.Contains(m.Content, "The user wants two commands.") {
		t.Error("thinking leaked into content: this protocol has no inbound field for it")
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("want 2 tool_calls, got %d", len(m.ToolCalls))
	}
	for i, want := range []openaiWireToolCall{
		{ID: "call_first", Type: "function"},
		{ID: "call_second", Type: "function"},
	} {
		if m.ToolCalls[i].ID != want.ID {
			t.Errorf("tool_calls[%d].id got %q, want %q", i, m.ToolCalls[i].ID, want.ID)
		}
		if m.ToolCalls[i].Type != "function" {
			t.Errorf("tool_calls[%d].type got %q, want %q", i, m.ToolCalls[i].Type, "function")
		}
		if m.ToolCalls[i].Function.Name != "bash" {
			t.Errorf("tool_calls[%d].function.name got %q, want bash", i, m.ToolCalls[i].Function.Name)
		}
	}
	if got, want := m.ToolCalls[0].Function.Arguments, `{"command": "ls -la"}`; got != want {
		t.Errorf("tool_calls[0].function.arguments\n got %q\nwant %q", got, want)
	}
	if got, want := m.ToolCalls[1].Function.Arguments, `{"command": "echo two"}`; got != want {
		t.Errorf("tool_calls[1].function.arguments\n got %q\nwant %q", got, want)
	}
}

// TestOpenAIBuildRequestArgumentsAreByteIdentical
// 是 Block.Args 是原始字符串而不是已解码
// map 的原因。
//
// 这个协议需要一个包含 JSON 的 JSON 字符串，这是流解析器
// 积累出来的，所以字节直接透传。解码后再重新编码，加上
// Go 随机化的 map 迭代顺序，会在每个回合上重写键的顺序，
// 这会无声地破坏 prompt 缓存依赖的字节前缀匹配（§C9：
// 9,792/9,815 个 token 从缓存提供，全部基于精确前缀）
// ——还会打乱任何格式重要的参数中的空格。
func TestOpenAIBuildRequestArgumentsAreByteIdentical(t *testing.T) {
	p := openaiTestProvider()

	// 有意困难：未排序的键、不规则的间距、
	// 嵌入的引号和嵌入的换行符。这些中的
	// 每一个都在传递中幸存，在重新序列化中死亡。
	const args = `{"zebra":1,  "command": "echo \"a  b\"\nls", "alpha":2}`

	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockToolCall, ID: "call_x", Name: "bash", Args: args},
	}}

	_, body, err := p.BuildRequest("", []Msg{assistant}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Messages) != 1 || len(got.Messages[0].ToolCalls) != 1 {
		t.Fatalf("unexpected shape: %s", body)
	}
	if sent := got.Messages[0].ToolCalls[0].Function.Arguments; sent != args {
		t.Errorf("arguments were rewritten\n got %q\nwant %q", sent, args)
	}

	// 以及 §B4 有效负载本身，这是必须为了让
	// Agent 能够回答自己的工具调用而幸存的那个。
	_, body, err = p.BuildRequest("", []Msg{{Role: RoleAssistant, Blocks: b4WantCalls}}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got = decodeOpenAIRequest(t, body)
	if sent := got.Messages[0].ToolCalls[0].Function.Arguments; sent != b4WantArgs {
		t.Errorf("B4 arguments were rewritten\n got %q\nwant %q", sent, b4WantArgs)
	}
}

func TestOpenAIBuildRequestToolDefinitionNesting(t *testing.T) {
	p := openaiTestProvider()

	_, body, err := p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, []Tool{openaiBashTool()}, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(got.Tools))
	}
	tool := got.Tools[0]
	if tool.Type != "function" {
		t.Errorf(`tools[0].type got %q, want "function"`, tool.Type)
	}
	if tool.Function.Name != "bash" {
		t.Errorf("tools[0].function.name got %q, want bash", tool.Function.Name)
	}
	if tool.Function.Description == "" {
		t.Error("tools[0].function.description is empty: the model is being asked to guess")
	}

	// schema 在 `parameters` 下，低一级。
	// Anthropic 适配器把相同的 map 放在顶级
	// `input_schema` 下的一个未嵌套对象上。
	// 与新构建的 schema 比较也证明在通过
	// 过程中没有任何东西被就地改变。
	want := openaiBashSchema()
	if gotType, _ := tool.Function.Parameters["type"].(string); gotType != "object" {
		t.Errorf("parameters.type got %v, want object", tool.Function.Parameters["type"])
	}
	if _, ok := tool.Function.Parameters["properties"]; !ok {
		t.Errorf("parameters lost its properties: %#v (want the shape of %#v)", tool.Function.Parameters, want)
	}
	if _, ok := tool.Function.Parameters["additionalProperties"]; !ok {
		t.Error("parameters lost additionalProperties")
	}

	// Anthropic 形状必须不出现在工具对象
	// 的任何地方。
	var raw struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"name", "description", "input_schema"} {
		if _, ok := raw.Tools[0][key]; ok {
			t.Errorf("tools[0] has a top-level %q: that is the other protocol's envelope", key)
		}
	}

	// 没有工具意味着没有 `tools` 键，而不是
	// `"tools":null`——有些 OpenAI 兼容服务器
	// 直接拒绝这个。
	_, body, err = p.BuildRequest("", []Msg{TextMsg(RoleUser, "hi")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if strings.Contains(string(body), `"tools"`) {
		t.Errorf("tools key present with no tools: %s", body)
	}
}

// ---------------------------------------------------------------------------
// ParseStream：从头到尾的记录流。
// ---------------------------------------------------------------------------

func TestOpenAIParseStream(t *testing.T) {
	cases := []struct {
		name         string
		frames       []string
		wantText     string
		wantThinking string
		wantCalls    []Block
		wantRawStop  string
		wantStop     StopReason
		wantUsage    Usage
	}{
		{
			// 头号用例：§B4 逐字重现，全部 13 帧。
			name:        "B4 tool call, all thirteen frames",
			frames:      b4ToolCallStream,
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
			wantCalls:   b4WantCalls,
		},
		{
			// §B7：一个 delta 对象上的两个字段必须在两个地方。
			name:         "B7 reasoning and text are kept apart",
			frames:       b7ReasoningStream,
			wantText:     "17 * 23 = 391",
			wantThinking: "Okay, the user is asking for the product of 17 and ",
			wantRawStop:  "stop",
			wantStop:     StopEndTurn,
			wantUsage:    b4WantUsage,
		},
		{
			// 这个使 choices[0] 解析器惊慌失措的帧，
			// 单独的。没有 finish_reason 曾到达，所以
			// 规范化的 stop 是 Unknown——不是 EndTurn，
			// 那会是一个打扮成事实的猜测。
			name:      "usage frame alone, choices is an empty array",
			frames:    []string{b4Usage},
			wantUsage: b4WantUsage,
			wantStop:  StopUnknown,
		},
		{
			// §B4 帧 13，单独来看：空 choices，**并且**带一个未知的顶级键。
			// 正因为它是在哨兵之后到达的，才让人很容易从来没测试过这种
			// 情况。
			name:     "post-DONE cost frame alone",
			frames:   []string{b4Done, b4PostDone},
			wantStop: StopUnknown,
		},
		{
			// 在哨兵之后继续排空，只有在它确实取回了什么东西时，才站得住脚。
			// 把使用情况挪到 [DONE] 后面——这就是正确记账和无声报零之间的
			// 差别所在。
			name:        "frames after the sentinel are still read",
			frames:      []string{b4RoleOpener, b4Finish, b4Done, b4Usage, b4PostDone},
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
		},
		{
			// 并行调用：独立累积，升序索引顺序。
			name:        "two parallel tool calls interleaved",
			frames:      openaiParallelStream,
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
			wantCalls:   openaiParallelWantCalls,
		},
		{
			// 一个什么都没产生的流，也必须干干净净地返回，而不是停在半初始化的状态。
			name:        "role opener and finish only",
			frames:      []string{b4RoleOpener, b4Finish, b4Done},
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
		},
	}

	p := openaiTestProvider()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ParseStream(openaiBody(tc.frames...), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text\n got %q\nwant %q", got.Text, tc.wantText)
			}
			if got.Thinking != tc.wantThinking {
				t.Errorf("Thinking\n got %q\nwant %q", got.Thinking, tc.wantThinking)
			}
			if got.RawStop != tc.wantRawStop {
				t.Errorf("RawStop got %q, want %q", got.RawStop, tc.wantRawStop)
			}
			if got.Stop != tc.wantStop {
				t.Errorf("Stop got %q, want %q", got.Stop, tc.wantStop)
			}
			if got.Usage != tc.wantUsage {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.wantUsage)
			}
			if !reflect.DeepEqual(got.Calls, tc.wantCalls) {
				t.Errorf("Calls\n got %#v\nwant %#v", got.Calls, tc.wantCalls)
			}
		})
	}
}

// TestOpenAIStopNormalisation 检查 CallResult
// 上的两值约定：RawStop 是供应商说的任何东西，
// Stop 是对它的中立阅读，一个从未见过的词
// 变成 StopUnknown 而不是"可能没事"。
func TestOpenAIStopNormalisation(t *testing.T) {
	cases := []struct {
		raw  string
		want StopReason
	}{
		{"tool_calls", StopToolUse},                                     // §B4 帧 10
		{"stop", StopEndTurn},                                           // §C9
		{"length", StopMaxTokens},                                       // §A1，§A2
		{"content_filter", StopFiltered},                                // 未在这里观察到；在别处有文档说明
		{"some_new_thing_the_vendor_shipped_on_a_tuesday", StopUnknown}, // 重要的情况
	}

	p := openaiTestProvider()
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			frame := `{"choices":[{"index":0,"finish_reason":"` + tc.raw + `","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`
			got, err := p.ParseStream(openaiBody(frame, b4Done), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			// 字面值永远不会被规范化掉：§A3c 是信封
			// 说谎的情况，当会话出错时，这两个字段
			// 之间的间隔是仅存的证据。
			if got.RawStop != tc.raw {
				t.Errorf("RawStop got %q, want %q", got.RawStop, tc.raw)
			}
			if got.Stop != tc.want {
				t.Errorf("Stop got %q, want %q", got.Stop, tc.want)
			}
		})
	}
}

// TestOpenAIB4ArgsReassembleIntoValidJSON
// 是永不解析片段的回报。§B4 中的七个
// 片段都不是单独的有效 JSON；连接是，
// 那是仅有的地方允许解析发生。
func TestOpenAIB4ArgsReassembleIntoValidJSON(t *testing.T) {
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got.Calls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.Calls))
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(got.Calls[0].Args), &args); err != nil {
		t.Fatalf("assembled args are not valid JSON: %v (%q)", err, got.Calls[0].Args)
	}
	if args.Command != "ls -la /srv/app" {
		t.Errorf("command got %q, want %q", args.Command, "ls -la /srv/app")
	}
}

// TestOpenAIToolIDSurvivesTheNullChunks
// 是 id-锁定回归，单独陈述，以便失败
// 消息命名实际的疾病。帧 3–9 都携带
// `"id":null`；无防护的赋值留下这个
// 为空，工具调用变得无法回答，因为
// API 在回复中需要那个 id。
func TestOpenAIToolIDSurvivesTheNullChunks(t *testing.T) {
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got.Calls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(got.Calls))
	}
	if got.Calls[0].ID != "call_8d4f0377bc594026a4765cfc" {
		t.Errorf("tool id was lost to a later null chunk: got %q", got.Calls[0].ID)
	}
	if got.Calls[0].Name != "bash" {
		t.Errorf("tool name was lost to a later null chunk: got %q", got.Calls[0].Name)
	}
	if got.Calls[0].Kind != BlockToolCall {
		t.Errorf("call block kind got %q, want %q", got.Calls[0].Kind, BlockToolCall)
	}
}

// TestOpenAIParallelToolCallsComeBackInIndexOrder
// 多次运行相同的流，因为 Go 有意随机化
// map 迭代顺序。一次通过会在大约一半时间
// 捕获缺少的排序——一个每两个 commit
// 失败一次的测试比没有测试更糟，因为它
// 教人们重新运行 CI。二十次通过把虚假
// 通过的概率降到约百万分之一。
func TestOpenAIParallelToolCallsComeBackInIndexOrder(t *testing.T) {
	p := openaiTestProvider()
	for i := 0; i < 20; i++ {
		got, err := p.ParseStream(openaiBody(openaiParallelStream...), NewBus(), 1, time.Now())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if !reflect.DeepEqual(got.Calls, openaiParallelWantCalls) {
			t.Fatalf("pass %d: out of index order\n got %#v\nwant %#v", i, got.Calls, openaiParallelWantCalls)
		}
	}
}

// TestOpenAIStreamedCallsReplayUnchanged
// 关闭了循环：ParseStream 生产的东西必须
// 直接回到 BuildRequest 作为 Assistant
// 消息。这是 Agent 循环在每个回合执行的
// 往返，也是这里仅有的同时运用适配器两半
// 的测试。
func TestOpenAIStreamedCallsReplayUnchanged(t *testing.T) {
	p := openaiTestProvider()

	res, err := p.ParseStream(openaiBody(openaiParallelStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}

	assistant := Msg{Role: RoleAssistant, Blocks: append([]Block{{Kind: BlockText, Text: res.Text}}, res.Calls...)}
	_, body, err := p.BuildRequest("", []Msg{assistant}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	if len(got.Messages) != 1 || len(got.Messages[0].ToolCalls) != 2 {
		t.Fatalf("round trip lost the calls: %s", body)
	}
	for i, want := range openaiParallelWantCalls {
		tc := got.Messages[0].ToolCalls[i]
		if tc.ID != want.ID || tc.Function.Name != want.Name || tc.Function.Arguments != want.Args {
			t.Errorf("tool_calls[%d]\n got %+v\nwant %+v", i, tc, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 使用情况规范化——方向反转。
// ---------------------------------------------------------------------------

func TestOpenAIUsageNormalisation(t *testing.T) {
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
			// 冷请求：没有缓存，所以 Input 是整个 prompt。
			// 这是复制 prompt_tokens 直接通过看起来
			// 完美的情况，正是为什么它通过审查。
			name: "no cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 506, Output: 26},
		},
		{
			// §C9 自己的数字：9,792/9,815 个 token 从
			// 缓存提供。不做减法直接复制 prompt_tokens，
			// Prompt() 就会对一个 9,815-token 的 prompt
			// 报告 19,607——这个误差正好是缓存命中的
			// 大小，所以缓存效果越好，它反而越大。
			name: "C9 warm call, a 99.8% cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 23, CacheRead: 9792, Output: 2},
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

	p := openaiTestProvider()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ParseStream(openaiBody(tc.in, b4Done), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			if got.Usage != tc.want {
				t.Errorf("Usage\n got %+v\nwant %+v", got.Usage, tc.want)
			}
		})
	}
}

// TestOpenAIUsagePromptRoundTrips 陈述
// 使得可检查的不变式，无需再次做减法：
// 不管怎么分割，Prompt() 必须等于
// 端点报告的 prompt_tokens。
//
// 这是断言，一旦有人把 normalise()
// "简化"为 Input = prompt_tokens，
// 它就变红，而且恰好是缓存命中的大小——
// 这就是为什么它被陈述为不变式而不是
// 另一份算术副本。
func TestOpenAIUsagePromptRoundTrips(t *testing.T) {
	p := openaiTestProvider()

	got, err := p.ParseStream(openaiBody(b4Usage), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.Usage.Prompt() != 506 {
		t.Errorf("Prompt() got %d, want 506 (the prompt_tokens on the wire)", got.Usage.Prompt())
	}
	if got.Usage.CacheWrite != 0 {
		t.Errorf("CacheWrite got %d, want 0: this protocol reports no write figure", got.Usage.CacheWrite)
	}

	// 同一不变式在 §C9 的温调用上，其中对与错
	// 之间的间隔是 9,792 token 而不是 192。
	warm := `{"choices":[],"usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}}`
	got, err = p.ParseStream(openaiBody(warm), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.Usage.Prompt() != 9815 {
		t.Errorf("Prompt() got %d, want 9815 (§C9's prompt_tokens)", got.Usage.Prompt())
	}
}

// ---------------------------------------------------------------------------
// 事件流。这些是事件总线存在的测试。
// ---------------------------------------------------------------------------

func TestOpenAIEventSequenceForB4ToolCall(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(rec), 7, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
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

	// response_end 仍然携带**供应商的**字面说法，不是规范化后
	// 的：一个渲染器如果显示的是"end_turn"，而线上实际说的是
	// "tool_calls"，这样的渲染器就没法用来调试线上情况。
	if end, ok := rec.first(KindResponseEnd); !ok {
		t.Error("no response_end")
	} else if end.FinishReason != "tool_calls" {
		t.Errorf("response_end finish reason got %q, want %q", end.FinishReason, "tool_calls")
	}
}

func TestOpenAIEventSequenceForB7Reasoning(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(b7ReasoningStream...), NewBus(rec), 2, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
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

func TestOpenAIParallelToolCallEventsAreRoutableByID(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(openaiParallelStream...), NewBus(rec), 1, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
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

func TestOpenAITTFTMeasuresFromTheRequest(t *testing.T) {
	// 假装请求是 1.5 秒前发出的。把 `started` 的时间往回调，就是不用
	// sleep 也能断言这一点的办法：TTFT 是从调用者提供的某个时刻算起
	// 的一段时长，所以测试可以自己选定这个时刻。
	started := time.Now().Add(-1500 * time.Millisecond)

	rec := &openaiRecorder{}
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(rec), 1, started)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
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

func TestOpenAITTFTIsZeroWhenNothingStreamed(t *testing.T) {
	// 角色开启携带 `content: ""`，完成块携带一个空 delta。两者都不算
	// 输出，所以没有第一个 token 可供计时——对一个什么都没产生的
	// 响应，报告一个看似合理的 TTFT，比干脆不报告还要糟糕。
	rec := &openaiRecorder{}
	got, err := openaiTestProvider().ParseStream(openaiBody(b4RoleOpener, b4Finish, b4Usage, b4Done), NewBus(rec), 1, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
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

func TestOpenAIMalformedFrameIsSurvivedAndReported(t *testing.T) {
	// 中间一帧坏帧，不能让我们损失一个已经完成的工具调用。它也不能
	// 悄无声息地就这么过去——发一条通知，把它记进 trace，这样以后
	// 就能找到它。
	frames := append([]string{}, b4ToolCallStream[:2]...)
	frames = append(frames, `{"choices":[{"delta":`) // 截断的 JSON
	frames = append(frames, b4ToolCallStream[2:]...)

	rec := &openaiRecorder{}
	got, err := openaiTestProvider().ParseStream(openaiBody(frames...), NewBus(rec), 1, time.Now())
	if err != nil {
		t.Fatalf("a single bad frame should not fail the stream: %v", err)
	}
	if len(got.Calls) != 1 || got.Calls[0].Args != b4WantArgs {
		t.Errorf("tool call damaged by the bad frame: %#v", got.Calls)
	}
	if n := rec.count(KindNotice); n != 1 {
		t.Errorf("bad frame produced %d notices, want 1", n)
	}
}

func TestOpenAITruncatedStreamReturnsPartialResultAndError(t *testing.T) {
	boom := errors.New("connection reset by peer")

	// 一个完整的工具调用，接着套接字在完成块到达之前就死掉了。
	// 这是阶段 01 截断教训的流式版本：危险的不是这个错误本身，而是
	// 调用者忽视了它，把一个没有 finish_reason 的结果发出去，就好像
	// 模型是故意停下来的一样。
	good := openaiBody(b4ToolCallStream[:9]...)
	rec := &openaiRecorder{}

	got, err := openaiTestProvider().ParseStream(io.MultiReader(good, iotest.ErrReader(boom)), NewBus(rec), 1, time.Now())
	if !errors.Is(err, boom) {
		t.Fatalf("want the read error back, got %v", err)
	}
	if got == nil {
		t.Fatal("partial result was thrown away: the caller cannot tell a dead stream from an empty one")
	}
	if len(got.Calls) != 1 || got.Calls[0].Args != b4WantArgs {
		t.Errorf("partial result lost the completed tool call: %#v", got.Calls)
	}
	if got.RawStop != "" {
		t.Errorf("RawStop got %q, want empty: the stream never sent one", got.RawStop)
	}
	if n := rec.count(KindResponseEnd); n != 0 {
		t.Errorf("response_end emitted %d times for a broken stream, want 0", n)
	}
}

func TestOpenAINilBusIsTolerated(t *testing.T) {
	// 不带订阅者进行解析，就是测试或批处理工具在不搭建总线的
	// 情况下使用这个函数的方式。
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), nil, 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got.Calls) != 1 || got.Calls[0].Args != b4WantArgs {
		t.Errorf("nil bus changed the result: %#v", got.Calls)
	}
}

// ---------------------------------------------------------------------------
// Framing，通过 sse.go 中的中立读取器。
//
// readSSE 不是这个协议的代码，这些不是
// 这个协议的规则——但情况随着从 stage 02
// 的移植而来，存在的覆盖率击败了别人要
// 写的覆盖率。用 `openai` 前缀命名，以便
// 未来的 sse_test.go 可以声称中立的名字，
// 无需冲突。
// ---------------------------------------------------------------------------

func TestOpenAIReadSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []sseFrame
	}{
		{
			// 这个协议实际发送的形状：`data:` 及其他
			// 什么都没有。
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
			// 另一个适配器需要的（§B6）。这条线上
			// 没有任何东西产生它。
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
			// readSSE 对哨兵一无所知。决定
			// [DONE] 的含义是这个文件的工作，这是
			// 什么保持读取器对一个根本没有哨兵的协议
			// 可重用的（§B6）。
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

func TestOpenAIReadSSEStopsOnCallbackError(t *testing.T) {
	boom := errors.New("subscriber gave up")

	seen := 0
	err := readSSE(openaiBody("a", "b", "c"), func(sseFrame) error {
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

func TestOpenAIReadSSEHandlesLinesOverScannerLimit(t *testing.T) {
	// bufio.Scanner 到 64KB 就会失败，报 ErrTooLong。这么大的单个 delta
	// 不是假设出来的：这就是一次大文件 `cat`，经工具结果回显后，在
	// 传出去的路上会呈现的样子。
	huge := strings.Repeat("x", 200*1024)

	var got []sseFrame
	if err := readSSE(openaiBody(huge), func(f sseFrame) error {
		got = append(got, f)
		return nil
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 1 || got[0].Data != huge {
		t.Fatalf("large frame not reassembled: %d frames, %d bytes", len(got), len(got[0].Data))
	}
}
