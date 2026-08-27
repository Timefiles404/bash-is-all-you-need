package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// padded 返回长度正好 n 个字符、以 prefix 开头的字符串。
//
// 预算 fixture 需要精确的消息大小：plan() 的算术只有在测试能算出预算边界
// 落在哪里、而不是靠猜的时候，才值得断言。
func padded(prefix string, n int) string {
	if len(prefix) >= n {
		return prefix[:n]
	}
	return prefix + strings.Repeat("x", n-len(prefix))
}

// bashArgs 构造格式正确的 bash 工具调用 payload，长度正好 n 个字符。
func bashArgs(n int) string {
	const head, tail = `{"command":"`, `"}`
	body := n - len(head) - len(tail)
	if body < 1 {
		body = 1
	}
	return head + strings.Repeat("e", body) + tail
}

// convFixture 是每个切点测试都拿来跑的那段对话：三个人类回合；一条
// assistant 消息，它一边说话一边发出两个并行工具调用；一条 user 消息，
// 把两个结果一起装着；最后一句普通回复。
//
// 下标对照表，因为下面每个测试都要拿它来推：
//
//	 0 user      "how big is this repo?"     非法 —— user 回合
//	 1 assistant 文本 + 两个并行调用         合法
//	 2 user      两个工具结果                非法 —— 会留下两个孤立结果
//	 3 assistant 文本 + 一个调用             合法
//	 4 user      一个工具结果                非法 —— 会留下孤立结果
//	 5 assistant 文本                        合法
//	 6 user      "now count the tests"       非法 —— user 回合
//	 7 assistant 一个调用，不带文本          合法
//	 8 user      一个工具结果                非法 —— 会留下孤立结果
//	 9 assistant 文本                        合法
//	10 user      "and the docs?"             非法 —— user 回合
//	11 assistant 文本 + 一个调用             合法
//	12 user      一个工具结果                非法 —— 会留下孤立结果
//	13 assistant 文本                        合法
func convFixture() []Msg {
	return []Msg{
		// 0
		TextMsg(RoleUser, "how big is this repo?"),
		// 1——文本**和**工具调用挤在同一条 assistant 消息里。有些切法天真地
		// 以为"assistant 消息就是文本"，栽的就是这个形状。
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "I'll count the files and the lines at the same time."},
			{Kind: BlockToolCall, ID: "toolu_aa1", Name: "bash", Args: `{"command":"find . -name '*.go' | wc -l"}`},
			{Kind: BlockToolCall, ID: "toolu_bb2", Name: "bash", Args: `{"command":"find . -name '*.go' | xargs wc -l | tail -1"}`},
		}},
		// 2——一条消息回答两个调用；这就是 Anthropic 的形状。
		{Role: RoleUser, Blocks: []Block{
			ToolResultBlock("toolu_aa1", "21\n[exit 0 · 12ms]"),
			ToolResultBlock("toolu_bb2", "  9184 total\n[exit 0 · 31ms]"),
		}},
		// 3
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "21 files, 9184 lines. Checking how much of that is tests."},
			{Kind: BlockToolCall, ID: "toolu_cc3", Name: "bash", Args: `{"command":"wc -l *_test.go | tail -1"}`},
		}},
		// 4
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_cc3", "  3120 total\n[exit 0 · 9ms]")}},
		// 5
		TextMsg(RoleAssistant, "About a third of the repo is tests: 3120 of 9184 lines."),
		// 6
		TextMsg(RoleUser, "now count the tests themselves"),
		// 7——只有工具调用，不带文本。
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: "toolu_dd4", Name: "bash", Args: `{"command":"grep -c '^func Test' *_test.go"}`},
		}},
		// 8
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_dd4", "cache_test.go:8\ntrace_test.go:11\n[exit 0 · 7ms]")}},
		// 9
		TextMsg(RoleAssistant, "19 test functions across two files."),
		// 10
		TextMsg(RoleUser, "and the docs?"),
		// 11
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Counting the prose too."},
			{Kind: BlockToolCall, ID: "toolu_ee5", Name: "bash", Args: `{"command":"wc -w docs/*.md | tail -1"}`},
		}},
		// 12
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_ee5", "  41022 total\n[exit 0 · 5ms]")}},
		// 13
		TextMsg(RoleAssistant, "41k words of docs against 9k lines of code."),
	}
}

// convFixtureToolIDs 是 convFixture 里所有的工具调用 id。有个测试要断言：
// 这些 id 一个都不许流到摘要器。
var convFixtureToolIDs = []string{"toolu_aa1", "toolu_bb2", "toolu_cc3", "toolu_dd4", "toolu_ee5"}

// toolTailFixture 以工具结果收尾，后面什么都没有——命令刚跑完的那一
// 瞬间，对话就是这个样子；而撞墙检查、要不要压缩，都恰好在这一刻发生。
func toolTailFixture() []Msg {
	return []Msg{
		TextMsg(RoleUser, "run the suite and the vet pass"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Both, in parallel."},
			{Kind: BlockToolCall, ID: "toolu_tail1", Name: "bash", Args: `{"command":"go test ./..."}`},
			{Kind: BlockToolCall, ID: "toolu_tail2", Name: "bash", Args: `{"command":"go vet ./..."}`},
		}},
		{Role: RoleUser, Blocks: []Block{
			ToolResultBlock("toolu_tail1", "ok  bash-is-all-you-need  0.412s\n[exit 0]"),
			ToolResultBlock("toolu_tail2", "[exit 0]"),
		}},
	}
}

// budgetFixture 是给 plan() 的算术准备的一段整齐对话：每条消息正好 400
// 个字符；把估算器钉在 4.0 字符每 token 之后，那就正好是 100 个 token。
// 于是测试可以把保留预算瞄准某个下标，并且知道它会落在哪儿。
//
// 重复单元是 user / assistant 调用 / 工具结果 / assistant，所以 i%4 为 0
// 或 2 的下标是非法切点，奇数下标是合法的。
func budgetFixture(blocks int) []Msg {
	var msgs []Msg
	for i := range blocks {
		id := fmt.Sprintf("toolu_b%02d", i)
		msgs = append(msgs,
			TextMsg(RoleUser, padded(fmt.Sprintf("step %d: ", i), 400)),
			Msg{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockToolCall, ID: id, Name: "bash", Args: bashArgs(396)},
			}},
			Msg{Role: RoleUser, Blocks: []Block{ToolResultBlock(id, padded(fmt.Sprintf("output %d: ", i), 400))}},
			TextMsg(RoleAssistant, padded(fmt.Sprintf("done %d: ", i), 400)),
		)
	}
	return msgs
}

// pinnedCompactor 返回的 compactor，估算器已经校准到正好 4 字符每 token，
// 这样测试就能在纸上把预算算清楚，不用去追 3.6 的冷启动值。
func pinnedCompactor(window int, threshold, keepRatio float64) *compactor {
	c := newCompactor(window, threshold, keepRatio)
	c.est.observe(4000, 1000)
	return c
}

// 预算算术立在"每条消息 400 个字符"这个假设上；budgetFixture 一旦飘离
// 这个假设，requireUniform 就让测试失败。
func requireUniform(t *testing.T, msgs []Msg) {
	t.Helper()
	for i, m := range msgs {
		if got := msgChars(m); got != 400 {
			t.Fatalf("budgetFixture message %d is %d characters, not 400 — every budget number in this test is now wrong", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 切点不变式
// ---------------------------------------------------------------------------

// 整个 compact.go 存在就是为了守住这条不变式：canCutBefore 说某处能切，
// 那么切完得到的对话，API 就必须收。断言在每个下标上都做一遍，而不是只
// 挑一个，因为要防的 bug 长这样：在下标 5 切是合法的，却在下标 9 留下
// 孤立工具结果——只测单个下标永远看不见它。
func TestEveryLegalCutProducesASendableConversation(t *testing.T) {
	msgs := convFixture()

	legal := 0
	for i := -1; i <= len(msgs)+1; i++ {
		if i < 0 || i > len(msgs) {
			if canCutBefore(msgs, i) {
				t.Errorf("canCutBefore allowed out-of-range index %d; the slice that follows would panic", i)
			}
			continue
		}
		if !canCutBefore(msgs, i) {
			continue
		}
		legal++
		out := append([]Msg{summaryMsg("s")}, msgs[i:]...)
		if why := validConversation(out); why != "" {
			t.Errorf("cutting before message %d is allowed but produces an unsendable conversation: %s\n"+
				"the API rejects this on the NEXT request, so the error will point at the request builder, not at the compactor", i, why)
		}
	}

	// 没有这一段，只要 canCutBefore 到处都返回 false，整个测试就会空转着
	// 通过——而那同时也让压缩彻底不可能，一声不响。
	if legal < 4 {
		t.Fatalf("only %d of %d indices are cuttable; the fixture no longer exercises the invariant", legal, len(msgs))
	}
}

// 上面那个测试的镜像：拒绝切点的两个理由，fixture 里都有，而且都点了名。
// canCutBefore 要是什么都不拒绝，那它能通过不变式测试只是因为从来没人
// 问过它。
func TestCutPointsAreRejectedForTheRightReason(t *testing.T) {
	msgs := convFixture()

	// 下标 2 回答的是消息 1 里发出的那两个并行调用。在这里切，等于删掉
	// 调用、留下回答。
	const orphan = 2
	hasResult := false
	for _, b := range msgs[orphan].Blocks {
		if b.Kind == BlockToolResult {
			hasResult = true
		}
	}
	if !hasResult {
		t.Fatalf("fixture drift: message %d was supposed to carry tool results", orphan)
	}
	if canCutBefore(msgs, orphan) {
		t.Errorf("cutting before message %d is allowed, but that message answers the tool calls in message %d — "+
			"the results would be orphaned and the provider rejects an unmatched tool_use_id", orphan, orphan-1)
	}

	// 下标 6 是第二个人类回合。摘要是以 user 消息的身份注入的，所以在这里
	// 切，会让两条 user 消息背靠背挨在一起。
	const userTurnIdx = 6
	if msgs[userTurnIdx].Role != RoleUser {
		t.Fatalf("fixture drift: message %d was supposed to be a user turn", userTurnIdx)
	}
	if canCutBefore(msgs, userTurnIdx) {
		t.Errorf("cutting before message %d is allowed, but it is a user message and the summary is also a user message — "+
			"the request would carry two user turns in a row, which some endpoints reject and others silently merge", userTurnIdx)
	}
}

// canCutBefore 既查工具结果*也*查 role。工具结果那一道为什么不是多余
// 的？就为了这种情况：中立的 Msg 类型允许任何 block 待在任何 role 下，
// 所以哪天新适配器把结果放进 assistant 消息，只看 role 的检查照旧返回
// true，孤立结果就这么发出去了。
func TestCanCutBeforeRejectsAToolResultWhateverRoleCarriesIt(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: "t1", Name: "bash", Args: `{"command":"ls"}`},
		}},
		// 一条 assistant 消息里装着结果。少见，而这恰好是只看 role 的检查
		// 看不见的形状。
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolResult, ID: "t1", Text: "a\nb\n[exit 0]"},
		}},
		TextMsg(RoleUser, "thanks"),
	}
	if canCutBefore(msgs, 2) {
		t.Error("a message holding a tool result was accepted as a cut point because its role was assistant; " +
			"the matching call is in the message about to be deleted, so the next request carries an orphan")
	}
}

// safeCut 往前搜——朝着多丢一些的方向——因为压缩是在窗口快满的时候
// 跑的。往后搜腾出来的空间比要求的少，Agent 下一次调用就又撞上墙。
func TestSafeCutNeverSearchesBackward(t *testing.T) {
	msgs := convFixture()
	moved := 0
	for k := -2; k <= len(msgs)+2; k++ {
		got := safeCut(msgs, k)
		if got < 0 {
			continue
		}
		if got < k {
			t.Errorf("safeCut(msgs, %d) = %d: it searched backward and kept more history than the caller asked to free, "+
				"so this compaction frees less than intended and the next call hits the wall again", k, got)
		}
		if !canCutBefore(msgs, got) {
			t.Errorf("safeCut(msgs, %d) = %d, which is not a legal cut point", k, got)
		}
		if got > k && k >= 1 {
			moved++
		}
	}
	if moved == 0 {
		t.Fatal("safeCut never had to move a requested index forward; the fixture no longer exercises the search")
	}
}

// 尾部这种情况：从 `want` 往后全是工具结果，那就没有合法切点，safeCut
// 必须直说，而不是返回一个"最不糟"的下标。这里返回非法下标，比拒绝
// 压缩更糟：拒绝只赔上一个慢回合，切错赔上的是一条格式非法的请求。
func TestSafeCutReturnsMinusOneWhenTheTailIsAllToolResults(t *testing.T) {
	msgs := toolTailFixture()
	if why := validConversation(msgs); why != "" {
		t.Fatalf("fixture drift: the tail fixture is not a valid conversation to begin with: %s", why)
	}
	if got := safeCut(msgs, len(msgs)-1); got != -1 {
		t.Errorf("safeCut returned %d for a conversation whose tail is nothing but tool results; "+
			"there is no assistant message to cut before, so the only correct answer is -1", got)
	}
}

// ---------------------------------------------------------------------------
// validConversation
// ---------------------------------------------------------------------------

func TestValidConversationAcceptsAWellFormedConversation(t *testing.T) {
	if why := validConversation(convFixture()); why != "" {
		t.Errorf("a well-formed conversation was rejected: %s — every legal cut in this file would now be reported as a bug", why)
	}
}

// 工具还在跑的时候，对话就是这个状态：最后一条 assistant 消息发出了调
// 用，还没有任何东西回答它。天真的"每个调用都必须有结果"检查会把这里
// 判成问题，于是 Agent 恰好在最需要压缩的时刻拒绝压缩——也就是工具
// 循环的开头。
func TestValidConversationAcceptsAnInFlightToolCall(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "list /srv"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Looking."},
			{Kind: BlockToolCall, ID: "toolu_inflight", Name: "bash", Args: `{"command":"ls -la /srv"}`},
		}},
	}
	if why := validConversation(msgs); why != "" {
		t.Errorf("an unanswered tool call in the FINAL message was reported as a problem: %s\n"+
			"that is the normal state between issuing a command and running it, not an error", why)
	}
}

// 孤立结果：调用被切掉了，结果还留着。压缩切错地方造成的故障就是这个，
// 而它要到下一次请求才浮出来。
func TestValidConversationRejectsAnOrphanToolResult(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "carry on"),
		TextMsg(RoleAssistant, "sure"),
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_gone", "  9184 total\n[exit 0]")}},
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("a tool result with no matching call was accepted; the provider will reject the request with an unexpected tool_use_id " +
			"and nothing in this codebase will have warned about it first")
	}
	if !strings.Contains(why, "toolu_gone") {
		t.Errorf("the complaint does not name the offending call id, so it cannot be found in a transcript: %s", why)
	}
}

// 孤立结果的镜像，也是更阴险的那一种：调用活下来了，它的回答被丢掉了，
// 于是模型以为自己发出去的命令什么都没产出。
func TestValidConversationRejectsAnUnansweredToolCall(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "count them"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: "toolu_lost", Name: "bash", Args: `{"command":"wc -l *.go"}`},
		}},
		TextMsg(RoleUser, "actually never mind"),
		TextMsg(RoleAssistant, "ok"),
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("a tool call that is never answered, and is not the final message, was accepted")
	}
	if !strings.Contains(why, "toolu_lost") {
		t.Errorf("the complaint does not name the unanswered call: %s", why)
	}
}

func TestValidConversationRejectsConsecutiveSameRole(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "one thing"),
		TextMsg(RoleUser, "and another"),
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("two user messages in a row were accepted; this is what injecting the summary before a user turn produces, " +
			"and endpoints disagree about whether to merge or reject it")
	}
	if !strings.Contains(why, "alternate") {
		t.Errorf("the complaint does not say what the rule is: %s", why)
	}
}

// 空消息：模型既不返回文本也不返回工具调用。把它追加进去，得到的是长度
// 为零的 content 数组，而 Anthropic 协议要到**下一次**请求才拒绝它，
// 不是这一次。
func TestValidConversationRejectsAnEmptyMessage(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "hello"),
		{Role: RoleAssistant},
	}
	why := validConversation(msgs)
	if why == "" {
		t.Fatal("a message with zero content blocks was accepted; the request fails one turn later, " +
			"with a traceback pointing at the request builder instead of at whatever appended the empty message")
	}
	if !strings.Contains(why, "1") {
		t.Errorf("the complaint does not say which message is empty: %s", why)
	}
}

// ---------------------------------------------------------------------------
// 估算器
// ---------------------------------------------------------------------------

// 估算器必须收敛到这次会话真实的字符每 token，因为每个压缩决定都是拿它
// 算出来的。估算器要是永远不收敛，压缩就会太早（白烧一遍缓存）或者太晚
// （直接撞墙）。
func TestEstimatorConverges(t *testing.T) {
	e := newEstimator()

	// 真实比值 3.0，回合之间带 ±8% 的噪声——一段会话在散文和 JSON 之间
	// 来回走的时候，大致就是这个样子。
	for i := range 20 {
		noise := 1.08
		if i%2 == 1 {
			noise = 0.92
		}
		tokens := 1000 + i*137
		chars := int(3.0 * noise * float64(tokens))
		e.observe(chars, tokens)
	}

	if math.Abs(e.ratio-3.0)/3.0 > 0.05 {
		t.Errorf("after 20 samples of a 3.0 conversation the ratio is %.4f, more than 5%% away — "+
			"compaction decisions are being made against a number that never learned this session", e.ratio)
	}
	if e.obs != 20 {
		t.Errorf("observation count is %d after 20 valid samples", e.obs)
	}
}

// 合理区间之所以存在，是因为 usage 事件和字符计数可能对不上号——比如
// usage 事件来了，可它对应那次调用的字符数从来没量过。这样一个样本，要
// 十次好调用才爬得回来。
func TestEstimatorRejectsImpossibleSamples(t *testing.T) {
	e := newEstimator()
	e.observe(3000, 1000) // 真样本：比值 3.0
	ratio, obs := e.ratio, e.obs

	for _, bad := range []struct {
		name          string
		chars, tokens int
	}{
		{"0.5 characters per token — fewer characters than tokens", 1000, 2000},
		{"50 characters per token — an order of magnitude too high", 50000, 1000},
		{"no characters", 0, 1000},
		{"no tokens", 3000, 0},
		{"negative characters", -3000, 1000},
	} {
		e.observe(bad.chars, bad.tokens)
		if e.ratio != ratio || e.obs != obs {
			t.Errorf("%s moved the estimate to %.4f (obs %d); one mismatched usage event now poisons every "+
				"compaction decision for the next ten turns", bad.name, e.ratio, e.obs)
			e.ratio, e.obs = ratio, obs
		}
	}
}

// 3.6 是猜的。第一次真实测量是证据。把两者平均一下，猜的那个数就能活过
// 十几个回合——短会话的大半程——而且它活得隐形，因为比值看上去
// 依然合理。
func TestEstimatorFirstObservationReplacesTheColdStart(t *testing.T) {
	e := newEstimator()
	if e.ratio != 3.6 {
		t.Fatalf("cold start is %.4f, not 3.6", e.ratio)
	}
	e.observe(2500, 1000) // JSON 很重的会话：2.5 字符每 token
	if e.ratio != 2.5 {
		t.Errorf("after one real measurement of 2.5 the ratio is %.4f — the 3.6 cold-start guess was blended in "+
			"rather than replaced, so the first turns of every session estimate against a number nobody measured", e.ratio)
	}
	if e.obs != 1 {
		t.Errorf("observation count is %d after one sample", e.obs)
	}
}

// 把这一章的论断拿来测。
//
// 估算不需要*准*，需要的是*一致*。下面那个供应商按 chars/2.9 收费，外
// 加 700 token 的固定信封开销，而这笔开销 Agent 永远看不到明细——
// 这正是自带一份 tokenizer 会算错的那种系统性偏差。而估算器校准用的量，
// 就是它后来被要求换算的那个量（convChars + baseChars），所以偏差被吸
// 进了比值里，而不是当成误差一路累积。
func TestEstimatorIsConsistentWithTheProviderItCalibratesAgainst(t *testing.T) {
	// "服务端"：Agent 拿不到的 tokenizer。
	tokenize := func(chars int) int { return int(float64(chars)/2.9) + 700 }

	c := newCompactor(200_000, 0.8, 0.3)
	const baseChars = 12_000 // 系统提示词加上工具 schema

	var msgs []Msg
	msgs = calibrationTurn(msgs, "t00", 4000)
	msgs = calibrationTurn(msgs, "t01", 4000)

	// 真实主循环跑十个回合：发出去，被计费，做校准。
	for i := range 10 {
		if i > 0 {
			msgs = calibrationTurn(msgs, fmt.Sprintf("t%02d", i+1), 4000)
		}
		sent := convChars(msgs) + baseChars
		c.est.observe(sent, tokenize(sent))
	}

	// 下一个回合到了。撞墙检查靠的就是这个预测，而这个预测是在问服务端之前
	// 就取好的。
	msgs = calibrationTurn(msgs, "t99", 4000)
	got := c.estimate(msgs, baseChars)
	want := tokenize(convChars(msgs) + baseChars)

	off := math.Abs(float64(got-want)) / float64(want)
	if off > 0.10 {
		t.Errorf("the estimator predicted %d tokens for a prompt the provider billed at %d — %.1f%% out.\n"+
			"The estimate is only ever used to answer 'are we near the wall yet', and at this error the answer "+
			"is wrong by more than a turn's worth of tool output.", got, want, off*100)
	}
}

// calibrationTurn 往历史里追加一个回合——一个问题、一次工具调用、它的
// 结果、一句回复——按 msgChars 的数法，总共正好 `chars` 个字符，所以
// 上面那段校准算术是精确的。
func calibrationTurn(msgs []Msg, id string, chars int) []Msg {
	q := chars / 4
	return append(msgs,
		TextMsg(RoleUser, padded("ask "+id+" ", q)),
		Msg{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: id, Name: "bash", Args: bashArgs(q - len("bash"))},
		}},
		Msg{Role: RoleUser, Blocks: []Block{ToolResultBlock(id, padded("out "+id+" ", q))}},
		TextMsg(RoleAssistant, padded("done "+id+" ", chars-3*q)),
	)
}

// due 回答的是"快撞墙了吗"。它必须拿即将发出的那个 prompt 的估算来回答，
// 不能拿上一次的 usage 报告——那份报告永远晚一个回合。
func TestDueFiresOnTheThreshold(t *testing.T) {
	c := newCompactor(100_000, 0.75, 0.3)
	for _, tc := range []struct {
		est  int
		want bool
	}{
		{0, false},
		{74_999, false},
		{75_000, true},
		{90_000, true},
	} {
		if got := c.due(tc.est); got != tc.want {
			t.Errorf("due(%d) = %v with a 100k window at 75%%; the agent will compact %s", tc.est, got,
				map[bool]string{true: "when it did not need to, burning the cache for nothing", false: "too late, after the wall"}[got])
		}
	}
	if (&compactor{window: 0, threshold: 0.75}).due(1 << 20) {
		t.Error("due fired with no configured window; an unconfigured compactor should never compact, not always compact")
	}
}

// ---------------------------------------------------------------------------
// plan
// ---------------------------------------------------------------------------

// 对话这么短还压缩，等于用一份什么都没有的摘要替掉真实内容，还要为此花
// 掉一次模型调用。
func TestPlanRefusesAShortConversation(t *testing.T) {
	c := pinnedCompactor(10_000, 0.8, 0.3)
	cut, why := c.plan(convFixture()[:3], 0)
	if cut != -1 {
		t.Fatalf("plan chose cut %d on a 3-message conversation", cut)
	}
	if why == "" {
		t.Error("plan refused silently; the caller has nothing to report and the user sees a turn that just did not compact")
	}
	if !strings.Contains(why, "3") {
		t.Errorf("the reason does not say how short the conversation is: %q", why)
	}
}

// 下限。最新那一条消息本身就比整个保留预算还大的时候，切历史腾不出任何
// 有意义的空间——问题在于用户要处理的那份输出太大，不在于对话太长。
// 这时候说"再压紧一点"，只会把读者引向错的方向。
func TestPlanRefusesWhenTheNewestMessageIsBiggerThanTheBudget(t *testing.T) {
	msgs := budgetFixture(5)
	requireUniform(t, msgs)

	// 10,000 token 窗口的 0.005 是 50 token 的预算；而每条消息是 100。
	c := pinnedCompactor(10_000, 0.8, 0.005)
	cut, why := c.plan(msgs, 0)
	if cut != -1 {
		t.Fatalf("plan chose cut %d when the newest message alone does not fit the keep budget", cut)
	}
	if !strings.Contains(why, "--max-output") {
		t.Errorf("the refusal does not point at the output limit: %q\n"+
			"this is an output-size problem, and a reason that does not say so sends the reader looking at the compactor", why)
	}
}

// 从外面看 safeCut 的全部意义：预算边界爱落在哪儿就落在哪儿，而 plan
// 必须先把它挪到合法切点上，再返回。
//
// 算术是这样：20 条消息、每条 100 token，预算 400 token，所以往回走正
// 好装得下最新的四条，边界停在下标 16——那是个 user 回合，切不得。
// 答案必须是 17。
func TestPlanCutsAtALegalBoundaryInsideTheBudget(t *testing.T) {
	msgs := budgetFixture(5)
	requireUniform(t, msgs)
	const budget = 400 // 0.04 × 10,000

	if canCutBefore(msgs, 16) {
		t.Fatalf("fixture drift: index 16 was supposed to be an illegal cut point, so this test no longer exercises safeCut")
	}

	c := pinnedCompactor(10_000, 0.8, 0.04)
	cut, why := c.plan(msgs, 0)
	if cut < 0 {
		t.Fatalf("plan refused a 20-message conversation that has room to cut: %s", why)
	}
	if !canCutBefore(msgs, cut) {
		t.Fatalf("plan returned index %d, which is not a legal cut point — the budget boundary was used raw instead of "+
			"being moved forward, and the next request carries an orphaned tool result", cut)
	}
	if cut <= 16 {
		t.Errorf("plan returned %d; the budget boundary is 16 and safeCut must move forward from there, never back", cut)
	}

	kept := c.est.tokens(convChars(msgs[cut:]))
	if kept > budget {
		t.Errorf("the kept tail is ~%d tokens against a keep budget of %d; compaction freed less than it promised", kept, budget)
	}
	if why := validConversation(append([]Msg{summaryMsg("s")}, msgs[cut:]...)); why != "" {
		t.Errorf("the planned compaction produces an unsendable conversation: %s", why)
	}
}

// 把所有说得过去的保留预算扫一遍。plan 返回的切点必须合法，必须留下一段
// 里面还有东西的对话，而且绝不能只剩不到两条消息——一条消息加一份
// 摘要，模型会当成用户刚把这份摘要打进来，然后回答它。
func TestPlanAlwaysLeavesASendableTail(t *testing.T) {
	msgs := budgetFixture(6)
	requireUniform(t, msgs)

	cuts, refusals := 0, 0
	for k := 1; k <= 60; k++ {
		c := pinnedCompactor(10_000, 0.8, float64(k)/100)
		cut, why := c.plan(msgs, 0)
		if cut < 0 {
			refusals++
			if why == "" {
				t.Errorf("keepRatio %.2f: plan refused with no reason", float64(k)/100)
			}
			continue
		}
		cuts++
		if !canCutBefore(msgs, cut) {
			t.Errorf("keepRatio %.2f: plan returned illegal cut %d", float64(k)/100, cut)
		}
		if len(msgs)-cut < 2 {
			t.Errorf("keepRatio %.2f: plan returned cut %d, leaving %d message(s) after the summary — "+
				"the model reads a lone summary as a fresh request from the user and answers it", float64(k)/100, cut, len(msgs)-cut)
		}
		if why := validConversation(append([]Msg{summaryMsg("s")}, msgs[cut:]...)); why != "" {
			t.Errorf("keepRatio %.2f: cut %d produces an unsendable conversation: %s", float64(k)/100, cut, why)
		}
	}
	if cuts == 0 || refusals == 0 {
		t.Fatalf("the sweep produced %d cuts and %d refusals; it needs both to be meaningful", cuts, refusals)
	}
}

// ---------------------------------------------------------------------------
// clip
// ---------------------------------------------------------------------------

// 掏中间，不是留开头。构建日志把错误放在末尾，堆栈把原因放在末尾；只
// 留头部的 `clip`，能通过一切天真的长度断言，同时把答案扔了。
func TestClipKeepsBothEnds(t *testing.T) {
	s := "HEAD" + strings.Repeat("m", 992) + "TAIL"
	if len(s) != 1000 {
		t.Fatalf("fixture is %d characters, not 1000", len(s))
	}

	got := clip(s, 200)
	if !strings.HasPrefix(got, "HEAD") {
		t.Errorf("the beginning of the string is gone; whatever the command was announcing was dropped:\n%q", got)
	}
	if !strings.HasSuffix(got, "TAIL") {
		t.Errorf("the END of the string is gone. This is head truncation, and it discards exactly the part that matters — "+
			"the error message, the last line of the diff, the conclusion:\n%q", got)
	}
	if !strings.Contains(got, "omitted") || !strings.Contains(got, "800") {
		t.Errorf("the clip does not say that 800 characters were dropped, so the model reads the two halves as contiguous:\n%q", got)
	}
}

func TestClipIsANoOpAndNeverPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		max  int
	}{
		{"shorter than the limit", "still small", 100},
		{"exactly the limit", "0123456789", 10},
		{"zero limit means no limit", "leave me alone", 0},
		{"negative limit means no limit", "leave me alone", -5},
	} {
		if got := clip(tc.s, tc.max); got != tc.s {
			t.Errorf("%s: clip rewrote a string it had no reason to touch: %q", tc.name, got)
		}
	}

	// 按字节切，切到多字节 rune 中间，不能 panic。结果里可以出现替换字符，
	// 但不能在压缩中途把进程搞挂。
	s := strings.Repeat("日本語のログ出力", 300)
	got := clip(s, 100)
	if len(got) >= len(s) {
		t.Errorf("a %d-byte multi-byte string clipped to 100 came back %d bytes", len(s), len(got))
	}
	if !strings.Contains(got, "omitted") {
		t.Errorf("the multi-byte clip lost its omission marker: %q", got)
	}
}

// ---------------------------------------------------------------------------
// flatten
// ---------------------------------------------------------------------------

// 摘要器读的是一份文字记录，不是一场对话。工具调用送到它面前时得是那条
// 真正跑过的命令，因为 `{"command":"ls -la /srv"}` 既把 token 花在 JSON
// 标点上，读起来像的是数据结构，不是动作。
func TestFlattenRendersTheCommandNotTheJSON(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "list /srv"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "Looking."},
			{Kind: BlockToolCall, ID: "toolu_f1", Name: "bash", Args: `{"command":"ls -la /srv/app"}`},
		}},
		{Role: RoleUser, Blocks: []Block{ToolResultBlock("toolu_f1", "total 0\n[exit 0]")}},
	}

	got := flatten(msgs, 4000)
	if !strings.Contains(got, "ls -la /srv/app") {
		t.Errorf("the command the agent ran is missing from the transcript:\n%s", got)
	}
	if strings.Contains(got, `{"command"`) {
		t.Errorf("the raw JSON arguments were pasted in instead of the command:\n%s", got)
	}
	if !strings.Contains(got, "Looking.") || !strings.Contains(got, "total 0") {
		t.Errorf("text or command output went missing from the transcript:\n%s", got)
	}
}

// 小模型会发出坏掉的工具调用——JSON 被截断，`command` 字段丢了。文字
// 记录是"到底发生了什么"的最后一份存档，所以解析不了的调用也得以某种
// 形式出现，不能就这么消失。
func TestFlattenFallsBackToRawArgsWhenTheJSONIsBroken(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"truncated mid-string", `{"command":"go test ./sta`},
		{"valid JSON, wrong shape", `{"raw_arguments":""}`},
		{"empty command", `{"command":"  "}`},
	} {
		msgs := []Msg{
			TextMsg(RoleUser, "run it"),
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockToolCall, ID: "toolu_broken", Name: "bash", Args: tc.args},
			}},
		}
		got := flatten(msgs, 4000)
		if !strings.Contains(got, tc.args) {
			t.Errorf("%s: the unparseable call left no trace in the transcript, so the summary cannot record that the "+
				"agent tried something and it did not work:\n%s", tc.name, got)
		}
	}
}

// 这份文字记录发给的是一次没有工具定义、没有历史的调用。里面出现过的 id
// 就是摘要可以引用的 id，而摘要在活下来的那段对话里是一条 user 消息——
// 在那里，那个 id 什么都指不到。
func TestFlattenLeaksNoToolCallIDs(t *testing.T) {
	got := flatten(convFixture(), 4000)
	for _, id := range convFixtureToolIDs {
		if strings.Contains(got, id) {
			t.Errorf("tool call id %q reached the summariser; anything it writes about that id survives into a conversation "+
				"where the call no longer exists", id)
		}
	}
	// 兜个底：这份文字记录不是空的，所以上面那个循环确实有东西可以在里面
	// "什么也没找到"。
	if len(got) < 200 {
		t.Fatalf("the flattened transcript is only %d characters; it cannot be a faithful rendering of a 14-message conversation", len(got))
	}
}
