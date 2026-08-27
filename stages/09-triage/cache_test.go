package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 每个下面测试使用的对话：两个回合，
// 一个工具调用和它的结果，所以有一个现实的
// "最后一条消息的最后块"来固定。
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

// 滚动 breakpoint 是在 Agent 中重要的：
// 它必须在最新回合上，所以每个请求读取
// 前一个写的前缀。
func TestCacheBreakpointRollsToTheNewestTurn(t *testing.T) {
	got := cacheBuild(t, true)
	last := got.Messages[len(got.Messages)-1]
	if last.Content[len(last.Content)-1].CacheControl == nil {
		t.Error("the last block of the last message is unmarked — the conversation prefix is never pinned")
	}

	// 每个更早的块必须保持未标记。一个标记
	// 在一个不是最新的块上停止随对话移动，
	// 所以它缓存每个回合的更少——并且
	// 它永远烧掉四个可用槽中的一个。
	for mi, m := range got.Messages[:len(got.Messages)-1] {
		for bi, b := range m.Content {
			if b.CacheControl != nil {
				t.Errorf("message %d block %d is marked; only the newest turn should be", mi, bi)
			}
		}
	}
}

func TestCacheBreakpointCountStaysUnderTheLimit(t *testing.T) {
	// 协议允许每个请求四个标记。这个适配器
	// 放置两个并留下两个空闲——一个扇出 Agent
	// 需要一个中间标记来保持在 20-块回看
	// 窗口内（见 markRollingBreakpoint）。
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

// 使这个特性能安全添加的不变式。
//
// 如果打开缓存改变了一个*未标记*块的字节，那么启用它反而会
// 让它本该保留的那段前缀失效——升级后的第一个请求就会在
// 悄无声息中支付全价。所以两份 body 之间唯一被允许的差异，
// 就是 cache_control 键本身。
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

// 针对这一整章都在讲的那件事，设下的回归防护。
//
// 工具参数被原样当作原始字节透传，就是为了让一个重放的回合，
// 产生和它上次一样的前缀。如果请求路径里的任何环节把它们
// 解码后又重新编码一遍，Go 就会把 map 的键排序，字节就会
// 移位，从那之后每个本该命中缓存的回合都会变成一次缺失
// ——不会报错，唯一的症状就是账单。
func TestToolArgumentKeyOrderSurvives(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{
			// 有意**不**按字母顺序："zeta"在"alpha"前。
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

// Prompt() 是缓存命中率必须对其计算的
// 数字。单独读取 Input 在一个温调用上
// 低报真实输入 ~500x。
func TestCacheHitRateUsesPromptNotInput(t *testing.T) {
	// 在 wire-notes §C8 的一个温调用上观察到
	// 的形状。
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

// 健全性检查：fixture body 是包括
// cache_control 的有效 JSON，所以一个
// 标记的请求仍然是服务器会接受的东西。
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
