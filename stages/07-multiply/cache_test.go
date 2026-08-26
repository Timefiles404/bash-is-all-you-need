package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The conversation used by every test below: two turns, a tool call and its
// result, so there is a realistic "last block of the last message" to pin.
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

// The rolling breakpoint is the one that matters in an agent: it must sit on
// the newest turn, so each request reads the prefix the previous one wrote.
func TestCacheBreakpointRollsToTheNewestTurn(t *testing.T) {
	got := cacheBuild(t, true)
	last := got.Messages[len(got.Messages)-1]
	if last.Content[len(last.Content)-1].CacheControl == nil {
		t.Error("the last block of the last message is unmarked — the conversation prefix is never pinned")
	}

	// Every earlier block must stay unmarked. A marker on a block that is not
	// the newest stops moving with the conversation, so it caches less of it
	// every turn — and it burns one of the four available slots forever.
	for mi, m := range got.Messages[:len(got.Messages)-1] {
		for bi, b := range m.Content {
			if b.CacheControl != nil {
				t.Errorf("message %d block %d is marked; only the newest turn should be", mi, bi)
			}
		}
	}
}

func TestCacheBreakpointCountStaysUnderTheLimit(t *testing.T) {
	// The protocol allows four markers per request. This adapter places two and
	// leaves two free — a fan-out agent needs an intermediate marker to stay
	// inside the 20-block lookback window (see markRollingBreakpoint).
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

// The invariant that makes the feature safe to add.
//
// If switching caching on changed the bytes of an *unmarked* block, then
// enabling it would invalidate the very prefix it was meant to preserve — and
// the first request after the upgrade would silently pay full price. So the
// only permitted difference between the two bodies is the cache_control keys
// themselves.
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

// Regression guard for the thing this whole chapter is about.
//
// Tool arguments are spliced through as raw bytes precisely so that a replayed
// turn produces the same prefix it did last time. If anything in the request
// path ever decodes and re-encodes them, Go will sort the map keys, the bytes
// will move, and every cached turn after that point becomes a miss — with no
// error, and no symptom except the bill.
func TestToolArgumentKeyOrderSurvives(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{
			// Deliberately NOT alphabetical: "zeta" before "alpha".
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

// Prompt() is the number a cache-hit rate must be computed against. Reading
// Input alone under-reports true input by ~500x on a warm call.
func TestCacheHitRateUsesPromptNotInput(t *testing.T) {
	// The shape observed in wire-notes §C8 on a warm call.
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

// Sanity: the fixture body is valid JSON with cache_control included, so a
// marked request is still something a server will accept.
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
