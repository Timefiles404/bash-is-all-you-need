package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 下面每个测试用的都是这段对话：两个回合，一次工具调用加它的结果，
// 这样才有像样的"最后一条消息的最后一个块"可以钉。
func cacheFixture() []Msg {
	return []Msg{
		TextMsg(RoleUser, "count the go files"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "I'll look."},
			{Kind: BlockToolCall, ID: "toolu_1", Name: "bash", Args: `{"command":"ls *.go | wc -l"}`},
		}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_1", "3\n[exit 0]")}},
	}
}

func cacheBuild(t *testing.T, on bool) anthWireBody {
	t.Helper()
	p := newAnthropicProvider("https://example.test", "k", "m").withCacheBreakpoints(on)
	_, body, err := p.BuildRequest("you are a shell", cacheFixture(), []Tool{{Name: "bash", Schema: map[string]any{"type": "object"}}}, 512)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	return anthDecodeBody(t, body)
}

func TestCacheBreakpointOnSystemBlock(t *testing.T) {
	got := cacheBuild(t, true)
	if len(got.System) != 1 {
		t.Fatalf("system blocks = %d, want 1", len(got.System))
	}
	if got.System[0].CacheControl == nil {
		t.Fatal("the system block carries no cache_control — tools and system are re-read at full price on every request")
	}
	if got.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("cache_control type = %q, want ephemeral", got.System[0].CacheControl.Type)
	}
}

// 滚动断点才是 Agent 里要紧的那个：它必须落在最新的回合上，这样每个
// 请求读到的都是上一个请求写下的前缀。
func TestCacheBreakpointRollsToTheNewestTurn(t *testing.T) {
	got := cacheBuild(t, true)
	last := got.Messages[len(got.Messages)-1]
	if last.Content[len(last.Content)-1].CacheControl == nil {
		t.Error("the last block of the last message is unmarked — the conversation prefix is never pinned")
	}

	// 更早的每个块都必须保持没有标记。标记打在不是最新的块上，就不再跟
	// 着对话往前走，于是每个回合能缓存的部分越来越少——而且它把四个可用
	// 槽位里的一个永久烧掉了。
	for mi, m := range got.Messages[:len(got.Messages)-1] {
		for bi, b := range m.Content {
			if b.CacheControl != nil {
				t.Errorf("message %d block %d is marked; only the newest turn should be", mi, bi)
			}
		}
	}
}

func TestCacheBreakpointCountStaysUnderTheLimit(t *testing.T) {
	// 协议允许每个请求四个标记。这个适配器摆了两个，留两个空着——扇出型
	// 的 Agent 需要一个中间标记，才待得住 20 块的回溯窗口（见
	// markRollingBreakpoint）。
	got := cacheBuild(t, true)
	n := 0
	for _, b := range got.System {
		if b.CacheControl != nil {
			n++
		}
	}
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				n++
			}
		}
	}
	if n != 2 {
		t.Errorf("breakpoints = %d, want exactly 2 (system, newest turn)", n)
	}
	if n > 4 {
		t.Fatal("over the protocol limit of 4")
	}
}

func TestNoCacheOmitsEveryBreakpoint(t *testing.T) {
	got := cacheBuild(t, false)
	if len(got.System) != 1 || got.System[0].CacheControl != nil {
		t.Error("--no-cache still marked the system block")
	}
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				t.Error("--no-cache still marked a message block")
			}
		}
	}
}

// 让这个特性可以放心加进来的不变式。
//
// 要是打开缓存会改掉**没标记**的块的字节，那启用它就会作废它本来要
// 保住的那段前缀——升级之后的第一个请求会一声不响地付全价。所以两份
// body 之间唯一允许存在的差别，就是 cache_control 这几个键本身。
func TestEnablingCachingChangesNothingElse(t *testing.T) {
	p := newAnthropicProvider("https://example.test", "k", "m")
	tools := []Tool{{Name: "bash", Schema: map[string]any{"type": "object"}}}

	_, on, err := p.withCacheBreakpoints(true).BuildRequest("sys", cacheFixture(), tools, 512)
	if err != nil {
		t.Fatal(err)
	}
	q := newAnthropicProvider("https://example.test", "k", "m")
	_, off, err := q.withCacheBreakpoints(false).BuildRequest("sys", cacheFixture(), tools, 512)
	if err != nil {
		t.Fatal(err)
	}

	strip := func(b []byte) string {
		s := string(b)
		s = strings.ReplaceAll(s, `,"cache_control":{"type":"ephemeral"}`, "")
		s = strings.ReplaceAll(s, `"cache_control":{"type":"ephemeral"},`, "")
		return s
	}
	if strip(on) != string(off) {
		t.Errorf("enabling caching changed bytes other than cache_control:\n with: %s\nwithout: %s", strip(on), off)
	}
}

// 整章讲的就是这件事，这里是它的回归防线。
//
// 工具参数以原始字节拼接穿过，正是为了让重放的回合产出跟上次一样的
// 前缀。请求路径上只要有哪一步把它们解码再编码，Go 就会把 map 的键
// 排序，字节就挪位了，那之后每个缓存过的回合都变成未命中——没有报
// 错，除了账单之外没有任何症状。
func TestToolArgumentKeyOrderSurvives(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{
			// 故意**不**按字母序："zeta" 排在 "alpha" 前面。
			{Kind: BlockToolCall, ID: "t1", Name: "bash", Args: `{"zeta":1,"alpha":2}`},
		}},
	}
	p := newAnthropicProvider("https://example.test", "k", "m")
	_, body, err := p.BuildRequest("sys", msgs, nil, 512)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"zeta"`) {
		t.Fatal("arguments missing entirely")
	}
	zeta := strings.Index(string(body), `"zeta"`)
	alpha := strings.Index(string(body), `"alpha"`)
	if zeta > alpha {
		t.Errorf("tool arguments were re-serialised: keys came back sorted, so the prompt prefix moved and every cached turn is now a miss")
	}
}

// 缓存命中率必须拿 Prompt() 来算。热调用的时候只看 Input，会把真实
// 输入少报约 500 倍。
func TestCacheHitRateUsesPromptNotInput(t *testing.T) {
	// wire-notes §C8 在一次热调用上观测到的形状。
	u := Usage{Input: 18, CacheRead: 17967}
	if u.Prompt() != 17985 {
		t.Fatalf("Prompt() = %d, want 17985", u.Prompt())
	}
	rate := float64(u.CacheRead) / float64(u.Prompt())
	if rate < 0.99 {
		t.Errorf("hit rate = %.3f, want ~0.999", rate)
	}
	naive := float64(u.CacheRead) / float64(u.Input+u.CacheRead)
	if naive < 0.99 {
		t.Errorf("the wire-notes formula should agree here: %.3f", naive)
	}
}

// 兜底检查：带上 cache_control 之后，fixture 的 body 仍然是合法
// JSON，所以打了标记的请求，服务器还是收得下。
func TestMarkedBodyIsValidJSON(t *testing.T) {
	p := newAnthropicProvider("https://example.test", "k", "m")
	_, body, err := p.BuildRequest("sys", cacheFixture(), nil, 512)
	if err != nil {
		t.Fatal(err)
	}
	var any map[string]any
	if err := json.Unmarshal(body, &any); err != nil {
		t.Fatalf("marked body is not valid JSON: %v\n%s", err, body)
	}
}
