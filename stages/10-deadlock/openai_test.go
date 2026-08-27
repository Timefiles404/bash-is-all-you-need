// OpenAI 协议适配器的测试。
//
// 下面每个帧常量都是从 docs/wire-notes.md 里抄出来的——这些字节是这
// 个端点真的发过来的，不是为了让解析器好看而编出来的。这正是要害：
// 照着规范写出来的 fixture，测的只是你对规范的理解，而这个端点并不
// 符合规范（见 §B4 的第 11 帧和第 13 帧）。哪份 fixture 是重建或者
// 编的，它上面的注释会讲明，并说清为什么。
//
// 从阶段 02 的 sse_test.go 移植过来。fixture 一个字节都没动；变的是
// 被测的那一层——现在是 Provider，不再是自由函数——还有全新的另外半
// 个文件，它测的是阶段 02 一直没给过名字的方向：中立的对话往这条线
// 上发出去。
//
// 命名说明：这里的辅助函数和分帧测试都带 `openai` 前缀，因为
// anthropic.go 的测试和它共用这个包。readSSE 本身是中立的，住在
// sse.go 里；分帧那几个用例留在这儿，是为了让阶段 02 的移植不丢覆盖
// 率，不是因为分帧归这个协议管。
//
// 不联网，不要 API key，没有 `-short` 跳过。整个文件在飞机上就能跑。
package main

import (
	"context"
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

// b4WantCalls 是同一次调用，换成 Provider 返回的中立形状。
var b4WantCalls = []Block{{
	Kind: BlockToolCall,
	ID:   "call_8d4f0377bc594026a4765cfc",
	Name: "bash",
	Args: b4WantArgs,
}}

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

// openaiParallelWantCalls 是这一对调用按 index 升序排好的样子——和线
// 上顺序正好相反，这正是要点。
var openaiParallelWantCalls = []Block{
	{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
	{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
}

// ---------------------------------------------------------------------------
// 辅助函数。
// ---------------------------------------------------------------------------

// openaiTestProvider 就是被测的那个 Provider。base URL 末尾那个斜杠
// 是故意留的：测试里手工搭出来的 Provider 也得像 config.go 那样把它
// 削掉，不然端点会变成 `.../v1//chat/completions`。
func openaiTestProvider() *openaiProvider {
	return newOpenAIProvider("https://opencode.ai/zen/go/v1/", "sk-test-key", "mimo-v2.5")
}

// openaiBody 渲染 payload 的方式，就是 §B4 里这个端点渲染它们的方
// 式：`data: <payload>` 后跟一个空行，以 LF 结尾（文档里是用
// `cat -A` 打出来的，每行都以 `$` 收尾，看不见 `^M`）。
func openaiBody(frames ...string) io.Reader {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return strings.NewReader(b.String())
}

// openaiRecorder 是个什么都留着的 Subscriber，也是能想到的最省事的
// 演示，说明 Agent 内核为什么发事件而不是直接打印：测试断言的是事件
// 序列，全程没碰过 stdout。
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

// BuildRequest 的解码这一侧。这里的 struct 是特意跟 openai.go 里那
// 套分开写的：拿产出这些字节的同一批类型去断言，写错的 json tag 会
// 自己跟自己对上，然后通过。
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

// roles 是消息的 role 序列，用一条断言说清整段对话的形状，这是最省
// 事的办法。
func (r openaiWireRequest) roles() []string {
	out := make([]string, 0, len(r.Messages))
	for _, m := range r.Messages {
		out = append(out, m.Role)
	}
	return out
}

// openaiBashSchema 就是阶段 02 交付的那份工具 schema，在这儿当真实
// 的嵌套值用，而不是只有一个键的占位桩。
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
// Provider 接口本身。
// ---------------------------------------------------------------------------

func TestOpenAIProviderIdentity(t *testing.T) {
	p := openaiTestProvider()

	// Protocol() 会写进 trace，所以它是个稳定的字符串，不是留着以后美化
	// 的显示标签。
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
// BuildRequest——中立的对话往这条线上发出去。
// ---------------------------------------------------------------------------

func TestOpenAIBuildRequestEnvelope(t *testing.T) {
	p := openaiTestProvider()

	req, body, err := p.BuildRequest(context.Background(), "sys", []Msg{TextMsg(RoleUser, "hi")}, []Tool{openaiBashTool()}, 4096)
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

	// 返回的字节必须就是请求要发出去的字节。调用方把它们作为 KindRequest
	// 发出去，而请求查看器要是显示了线上实际内容以外的东西，那还不如没
	// 有查看器：它是会撒谎的证据。
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

	// §B5：在这个网关上实测是空操作——带不带它都是同样的 13 帧。照发不
	// 误，因为真正的 OpenAI 端点没有它就不会流式发 usage，而 Agent 哪天
	// 被指到别处去、报出零 token，正是这一行挡下来的故障。
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

	_, body, err := p.BuildRequest(context.Background(), sys, []Msg{TextMsg(RoleUser, "list the repo")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	// **不对称就在这儿**。在这个协议上，系统提示词是 messages[0]。在
	// Anthropic 协议上，它是顶层的 `system` 字段，根本不可能是消息——所
	// 以 Provider.BuildRequest 才单独给它留了个参数，而不是让调用方把它
	// 塞进历史里。
	if want := []string{"system", "user"}; !reflect.DeepEqual(got.roles(), want) {
		t.Fatalf("roles\n got %v\nwant %v", got.roles(), want)
	}
	if got.Messages[0].Content != sys {
		t.Errorf("messages[0].content\n got %q\nwant %q", got.Messages[0].Content, sys)
	}

	// 而且它**不能**同时又作为顶层字段出现：两边都发，就是两个适配器共
	// 用一份 struct 时，一边的协议漏进另一边的方式。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["system"]; ok {
		t.Error("top-level `system` key present: that is the other protocol's shape")
	}

	// 系统提示词为空时，一条消息都不生成，而不是生成一条空的；这样没有
	// 系统提示词的调用方，就不会一声不响地发出一个占着上下文的空回合。
	_, body, err = p.BuildRequest(context.Background(), "", []Msg{TextMsg(RoleUser, "hi")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if want := []string{"user"}; !reflect.DeepEqual(decodeOpenAIRequest(t, body).roles(), want) {
		t.Errorf("empty system prompt still produced a message: %v", decodeOpenAIRequest(t, body).roles())
	}
}

// TestOpenAIBuildRequestToolResultsBecomeSeparateMessages 是两个适
// 配器之间最显眼的差别，写成了一条断言。
//
// **一条**中立消息里的三个工具结果，到这儿变成**三条**
// `role:"tool"` 消息。Anthropic 适配器拿同样的输入，塌成**一条**
// user 消息，里面装三个 tool_result 块。哪种形状都当不了中立的那
// 个，所以 provider.go 里没有 RoleTool，工具结果是块。
func TestOpenAIBuildRequestToolResultsBecomeSeparateMessages(t *testing.T) {
	p := openaiTestProvider()

	results := Msg{Role: RoleUser, Blocks: []Block{
		ToolResultBlock("call_a", "total 0\n[exit 0 · 4ms]"),
		ToolResultBlock("call_b", "[no output]\n[exit 1 · 2ms]"),
		ToolResultBlock("call_c", "[the user denied this command. Do not retry it unchanged.]"),
	}}

	_, body, err := p.BuildRequest(context.Background(), "", []Msg{results}, nil, 100)
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
		// 把结果拼起来的实现能过 id 那一关，却栽在这里；这个故障模式值得
		// 点名：模型收到的是三份输出黏成的一坨，还都算在同一次调用头上。
		if strings.Contains(m.Content, "\x00") {
			t.Errorf("messages[%d].content looks concatenated: %q", i, m.Content)
		}
	}
}

// TestOpenAIBuildRequestFullConversationOrder 钉住真实回合的形状：
// system、用户的请求、assistant 的工具调用、答案、下一条用户消息。
// 工具结果必须落在提出它们的那条 assistant 消息**之后**，否则 API
// 配不上对。
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

	_, body, err := p.BuildRequest(context.Background(), "sys", msgs, []Tool{openaiBashTool()}, 4096)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	got := decodeOpenAIRequest(t, body)

	want := []string{"system", "user", "assistant", "tool", "tool", "user"}
	if !reflect.DeepEqual(got.roles(), want) {
		t.Errorf("conversation shape\n got %v\nwant %v", got.roles(), want)
	}
}

// TestOpenAIBuildRequestAssistantReplayRoundTrip 讲的是重组这笔开
// 销：流式收到的 assistant 回合，得以"非流式时 API 会返回的那条消
// 息"的样子回到历史里，否则下一个请求里的工具调用，就查不到是谁
// 发起的。
func TestOpenAIBuildRequestAssistantReplayRoundTrip(t *testing.T) {
	p := openaiTestProvider()

	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "Running both now."},
		// 出去的路上被丢掉：这个协议没有接收 reasoning 的入向字段，回放它
		// 要么被忽略要么被拒，看对面是谁家的实现。
		{Kind: BlockThinking, Text: "The user wants two commands."},
		{Kind: BlockToolCall, ID: "call_first", Name: "bash", Args: `{"command": "ls -la"}`},
		{Kind: BlockToolCall, ID: "call_second", Name: "bash", Args: `{"command": "echo two"}`},
	}}

	_, body, err := p.BuildRequest(context.Background(), "", []Msg{assistant}, nil, 100)
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

// TestOpenAIBuildRequestArgumentsAreByteIdentical 就是 Block.Args
// 为什么是原始字符串、而不是解码后的 map 的原因。
//
// 这个协议要的是装着 JSON 的 JSON 字符串，而流解析器攒出来的正是这
// 个，所以字节原样穿过去。一旦解码再编码，Go 随机化的 map 遍历顺序
// 每个回合都会把键序重排一遍，prompt 缓存赖以生效的字节前缀匹配就
// 这么悄悄没了（§C9：9,815 个 token 里有 9,792 个是从缓存拿的，全
// 都拴在一段一模一样的前缀上）——顺带还会搅烂那些参数里的空白，而它
// 们的格式本身是有意义的。
func TestOpenAIBuildRequestArgumentsAreByteIdentical(t *testing.T) {
	p := openaiTestProvider()

	// 故意做得别扭：键没排序、空格不规整、里面嵌了引号、还嵌了换行。这
	// 些东西原样穿过去都活得下来，重新序列化一遍就全死了。
	const args = `{"zebra":1,  "command": "echo \"a  b\"\nls", "alpha":2}`

	assistant := Msg{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockToolCall, ID: "call_x", Name: "bash", Args: args},
	}}

	_, body, err := p.BuildRequest(context.Background(), "", []Msg{assistant}, nil, 100)
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

	// 还有 §B4 那份 payload 本身——它得活下来，Agent 才答得上自己发出的
	// 工具调用。
	_, body, err = p.BuildRequest(context.Background(), "", []Msg{{Role: RoleAssistant, Blocks: b4WantCalls}}, nil, 100)
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

	_, body, err := p.BuildRequest(context.Background(), "", []Msg{TextMsg(RoleUser, "hi")}, []Tool{openaiBashTool()}, 100)
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

	// schema 放在 `parameters` 底下，往下一层。Anthropic 适配器把同一份
	// map 挂在顶层的 `input_schema` 上，对象不嵌套。跟现搭出来的 schema
	// 对比，顺便也证明了它一路上没被原地改过。
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

	// Anthropic 那套形状不能出现在工具对象的任何地方。
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

	// 没有工具就没有 `tools` 键，而不是 `"tools":null`——有些 OpenAI 兼
	// 容的服务器会直接拒掉后者。
	_, body, err = p.BuildRequest(context.Background(), "", []Msg{TextMsg(RoleUser, "hi")}, nil, 100)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if strings.Contains(string(body), `"tools"`) {
		t.Errorf("tools key present with no tools: %s", body)
	}
}

// ---------------------------------------------------------------------------
// ParseStream：录下来的那些流，从头跑到尾。
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
			// 头号用例：§B4 逐字，13 帧全上。
			name:        "B4 tool call, all thirteen frames",
			frames:      b4ToolCallStream,
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
			wantCalls:   b4WantCalls,
		},
		{
			// §B7：同一个 delta 对象上的两个字段必须落到两个地方。
			name:         "B7 reasoning and text are kept apart",
			frames:       b7ReasoningStream,
			wantText:     "17 * 23 = 391",
			wantThinking: "Okay, the user is asking for the product of 17 and ",
			wantRawStop:  "stop",
			wantStop:     StopEndTurn,
			wantUsage:    b4WantUsage,
		},
		{
			// 单独放这一帧，它能把伸手拿 choices[0] 的解析器 panic 掉。
			// finish_reason 一直没来，所以归一化后的 stop 是 Unknown——不是
			// EndTurn，那是把猜测打扮成事实。
			name:      "usage frame alone, choices is an empty array",
			frames:    []string{b4Usage},
			wantUsage: b4WantUsage,
			wantStop:  StopUnknown,
		},
		{
			// §B4 第 13 帧，单独上：choices 是空的，**而且**顶层还有个不
			// 认识的 key。它是在哨兵之后才到的，这就是这种情况容易一直
			// 没测过的原因。
			name:     "post-DONE cost frame alone",
			frames:   []string{b4Done, b4PostDone},
			wantStop: StopUnknown,
		},
		{
			// 读过哨兵这件事，只有真能捡到东西才站得住脚。把 usage 挪到
			// [DONE] 后面，这就是账算得对和悄无声息记成零之间的区别。
			name:        "frames after the sentinel are still read",
			frames:      []string{b4RoleOpener, b4Finish, b4Done, b4Usage, b4PostDone},
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
		},
		{
			// 并行调用：各攒各的，按 index 升序回来。
			name:        "two parallel tool calls interleaved",
			frames:      openaiParallelStream,
			wantRawStop: "tool_calls",
			wantStop:    StopToolUse,
			wantUsage:   b4WantUsage,
			wantCalls:   openaiParallelWantCalls,
		},
		{
			// 一条什么都没产出的流，回来的时候也必须是干净的，不能是初
			// 始化到一半的样子。
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

// TestOpenAIStopNormalisation 检查 CallResult 上那份两值契约：
// RawStop 是供应商说什么就是什么，Stop 是对它的中立读法，而谁都没见
// 过的词会变成 StopUnknown，不是"八成没事"。
func TestOpenAIStopNormalisation(t *testing.T) {
	cases := []struct {
		raw  string
		want StopReason
	}{
		{"tool_calls", StopToolUse},                                     // §B4 第 10 帧
		{"stop", StopEndTurn},                                           // §C9
		{"length", StopMaxTokens},                                       // §A1、§A2
		{"content_filter", StopFiltered},                                // 这里没观测到；别处有记录
		{"some_new_thing_the_vendor_shipped_on_a_tuesday", StopUnknown}, // 真正要紧的那个用例
	}

	p := openaiTestProvider()
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			frame := `{"choices":[{"index":0,"finish_reason":"` + tc.raw + `","delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null}}]}`
			got, err := p.ParseStream(openaiBody(frame, b4Done), NewBus(), 1, time.Now())
			if err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			// 字面值永远不会被归一化抹掉：§A3c 就是信封撒谎的例子，会话出岔子
			// 的时候，这两个字段之间的差距是剩下的唯一证据。
			if got.RawStop != tc.raw {
				t.Errorf("RawStop got %q, want %q", got.RawStop, tc.raw)
			}
			if got.Stop != tc.want {
				t.Errorf("Stop got %q, want %q", got.Stop, tc.want)
			}
		})
	}
}

// TestOpenAIB4ArgsReassembleIntoValidJSON 是"绝不解析碎片"这条规矩
// 的回报。§B4 里那七块，单拿出来没有一块是合法 JSON；拼起来才是，而
// 那里是唯一允许发生解析的地方。
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

// TestOpenAIToolIDSurvivesTheNullChunks 是 id 锁存的回归测试，单独
// 拎出来写，这样失败信息点的是真正的病名。第 3 到第 9 帧都带着
// `"id":null`；没设防的赋值会把它清空，工具调用也就没法回答了——API
// 要求回复里把那个 id 带回去。
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

// TestOpenAIParallelToolCallsComeBackInIndexOrder 把同一条流跑很多
// 遍，因为 Go 是故意把 map 遍历顺序随机化的。只跑一遍，漏掉排序大概
// 只有一半概率会被抓到——每两次提交就红一次的测试比没有测试还糟，它
// 教会大家重跑 CI。跑二十遍，假通过的概率落到百万分之一左右。
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

// TestOpenAIStreamedCallsReplayUnchanged 把这个环闭上：ParseStream
// 产出的东西，得原封不动地当成 assistant 消息再穿过 BuildRequest。
// 这就是主循环每个回合都在做的往返，也是这里唯一同时把适配器两半都
// 跑起来的测试。
func TestOpenAIStreamedCallsReplayUnchanged(t *testing.T) {
	p := openaiTestProvider()

	res, err := p.ParseStream(openaiBody(openaiParallelStream...), NewBus(), 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}

	assistant := Msg{Role: RoleAssistant, Blocks: append([]Block{{Kind: BlockText, Text: res.Text}}, res.Calls...)}
	_, body, err := p.BuildRequest(context.Background(), "", []Msg{assistant}, nil, 100)
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
// Usage 归一化——把方向反过来。
// ---------------------------------------------------------------------------

func TestOpenAIUsageNormalisation(t *testing.T) {
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
			// 冷请求：什么都没缓存，所以 Input 就是整个 prompt。把
			// prompt_tokens 直接抄过来，在这种情形下看着完美无缺——它就是这么
			// 活着通过评审的。
			name: "no cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":506,"completion_tokens":26,"total_tokens":532,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 506, Output: 26},
		},
		{
			// §C9 自己的数字：9,815 个 token 里 9,792 个来自缓存。不做减法直
			// 接抄 prompt_tokens，prompt 明明是 9,815 个 token，Prompt() 报出
			// 来的是 19,607——误差正好是缓存命中的大小，所以缓存越好使，误差
			// 越大。
			name: "C9 warm call, a 99.8% cache hit",
			in:   `{"choices":[],"usage":{"prompt_tokens":9815,"completion_tokens":2,"total_tokens":9817,"prompt_tokens_details":{"cached_tokens":9792},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: Usage{Input: 23, CacheRead: 9792, Output: 2},
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

// TestOpenAIUsagePromptRoundTrips 立了个不变式，让方向反转这件事不
// 必再算一遍减法就能查：不管怎么拆，Prompt() 都得等于端点报的
// prompt_tokens。
//
// 谁要是把 normalise() "简化"成 Input = prompt_tokens，这条断言当场
// 变红，而且红出来的差额正好是缓存命中的大小——所以它写成不变式，而
// 不是把那套算术再抄一遍。
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

	// 同一条不变式，用在 §C9 的热调用上——那里对和错之间差的是 9,792 个
	// token，不是 192 个。
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
// 事件流。事件总线之所以存在，就是为了这几个测试。
// ---------------------------------------------------------------------------

func TestOpenAIEventSequenceForB4ToolCall(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), NewBus(rec), 7, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
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

	// response_end 带的仍然是**供应商**的原话，不是归一化之后的：线上明
	// 明写着 "tool_calls"，渲染器却显示 "end_turn"，这种渲染器没法拿来
	// 调线上的问题。
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

func TestOpenAIParallelToolCallEventsAreRoutableByID(t *testing.T) {
	rec := &openaiRecorder{}
	if _, err := openaiTestProvider().ParseStream(openaiBody(openaiParallelStream...), NewBus(rec), 1, time.Now()); err != nil {
		t.Fatalf("ParseStream: %v", err)
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

func TestOpenAITTFTMeasuresFromTheRequest(t *testing.T) {
	// 假装请求是 1.5 秒前发出去的。把 `started` 往前挪，就是这条断言不用
	// sleep 的办法：TTFT 是从调用方给的那个时刻起算的时长，所以测试可以自己
	// 挑这个时刻。
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
	// role 开场帧带的是 `content: ""`，finish chunk 带的是空 delta。两者都不
	// 是输出，所以没有第一个 token 可计时——而响应什么都没产出，却给它报一
	// 个看着挺像样的 TTFT，那比什么都不报更糟。
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
// 坏掉的流。
// ---------------------------------------------------------------------------

func TestOpenAIMalformedFrameIsSurvivedAndReported(t *testing.T) {
	// 中间来一帧坏帧，不能让我们赔上一次已经完成的工具调用。它也不能就这么
	// 无声无息地过去——发条 notice，它就进了 trace，以后还找得到。
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

	// 一次完整的工具调用，然后 socket 在 finish chunk 之前断了。这是阶段 01
	// 那堂截断课的流式版本：危险的不是错误本身，而是调用方无视它，把一个没
	// 有 finish_reason 的结果当成模型是主动停下的，就这么交出去。
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
	// 不带订阅者地解析，就是测试、或者某个批处理工具用它的方式——不用先支起
	// 一条总线。
	got, err := openaiTestProvider().ParseStream(openaiBody(b4ToolCallStream...), nil, 1, time.Now())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got.Calls) != 1 || got.Calls[0].Args != b4WantArgs {
		t.Errorf("nil bus changed the result: %#v", got.Calls)
	}
}

// ---------------------------------------------------------------------------
// 分帧，走的是 sse.go 里那个中立的 reader。
//
// readSSE 不是这个协议的代码，这些规则也不是这个协议的规则——但用例
// 是跟着阶段 02 的移植一起过来的，而已经存在的覆盖率，胜过"该由别人
// 来写"的覆盖率。名字带 `openai` 前缀，是为了让将来的 sse_test.go
// 能占下那些中立的名字而不撞车。
// ---------------------------------------------------------------------------

func TestOpenAIReadSSEFraming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []sseFrame
	}{
		{
			// 这个协议实际发出来的形状：只有 `data:`，别的都没有。
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
			// 另一个适配器需要的东西（§B6）。这条线上没有任何东西会产生它。
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
			// readSSE 完全不懂哨兵这回事。[DONE] 是什么意思，由这个文件说了
			// 算；正是这一点让这个 reader 还能给根本没有哨兵的协议复用（§B6）。
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
	// bufio.Scanner 会在 64KB 处以 ErrTooLong 挂掉这条。这么大的单条 delta
	// 不是假想：一次 `cat` 大文件，经工具结果回显出去，出去的时候就长这
	// 样。
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
