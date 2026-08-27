package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// padded 返回恰好 n 个字符的字符串，
// 以 prefix 开始。
//
// 预算夹具需要精确的消息大小：plan()
// 的算术只有当测试能预测预算边界在哪
// 而不是猜测时才值得断言。
func padded(prefix string, n int) string {
	if len(prefix) >= n {
		return prefix[:n]
	}
	return prefix + strings.Repeat("x", n-len(prefix))
}

// bashArgs 构建恰好 n 个字符的格式良好
// 的 bash 工具调用 payload。
func bashArgs(n int) string {
	const head, tail = `{"command":"`, `"}`
	body := n - len(head) - len(tail)
	if body < 1 {
		body = 1
	}
	return head + strings.Repeat("e", body) + tail
}

// convFixture 是每个切点测试运行的对话：
// 三个人类回合，一条既说话又发出两个并行
// 工具调用的 Assistant 消息，一条单一用户消息
// 携带两个结果，和一个普通回复。
//
// 索引映射，因为下面每个测试都用它来推理：
//
//	 0 user       "how big is this repo?"      ILLEGAL — a user turn
//	 1 assistant   text + two parallel calls   legal
//	 2 user        both tool results           ILLEGAL — would orphan two results
//	 3 assistant   text + one call             legal
//	 4 user        one tool result             ILLEGAL — would orphan a result
//	 5 assistant   text                        legal
//	 6 user       "now count the tests"        ILLEGAL — a user turn
//	 7 assistant   one call, no text           legal
//	 8 user        one tool result             ILLEGAL — would orphan a result
//	 9 assistant   text                        legal
//	10 user       "and the docs?"              ILLEGAL — a user turn
//	11 assistant   text + one call             legal
//	12 user        one tool result             ILLEGAL — would orphan a result
//	13 assistant   text                        legal
func convFixture() []Msg {
	return []Msg{
		// 0
		TextMsg(RoleUser, "how big is this repo?"),
		// 1 — 一条 Assistant 消息中既有文本
		// 又有工具调用，这是天真的"Assistant
		// 消息只是文本"切割器会搞错的形状。
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockText, Text: "I'll count the files and the lines at the same time."},
			{Kind: BlockToolCall, ID: "toolu_aa1", Name: "bash", Args: `{"command":"find . -name '*.go' | wc -l"}`},
			{Kind: BlockToolCall, ID: "toolu_bb2", Name: "bash", Args: `{"command":"find . -name '*.go' | xargs wc -l | tail -1"}`},
		}},
		// 2 — 一条消息回答两个调用；这是
		// Anthropic 的形状。
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
		// 7 — 没有伴随文本的工具调用。
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

// convFixtureToolIDs 是 convFixture
// 中每一个工具调用的 id，供那个
// 断言——这些 id 都不能传到总结
// 程序——的测试使用。
var convFixtureToolIDs = []string{"toolu_aa1", "toolu_bb2", "toolu_cc3", "toolu_dd4", "toolu_ee5"}

// toolTailFixture 以工具结果结束，
// 之后没有任何东西——对话在其命令
// 完成的一瞬间所处的状态，这正是
// 墙壁检查运行和考虑压缩的时候。
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

// budgetFixture 是 plan() 算术的
// 统一对话：每条消息恰好 400 个字符，
// 一旦估算器固定在每 token 4.0 个字符，
// 就恰好 100 个 token。这让测试可以
// 把保留预算对准选定的索引，知道它
// 会在哪落地。
//
// 重复块是 user / assistant-call / tool-result /
// assistant，所以 i%4 为 0 或 2 的索引
// 是非法切点，奇数是合法的。
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

// pinnedCompactor 返回一个压缩器，
// 其估算器已被校准为恰好每 token
// 4 个字符，所以测试可以在纸上做
// 预算算术，而不是追 3.6 冷启动。
func pinnedCompactor(window int, threshold, keepRatio float64) *compactor {
	c := newCompactor(window, threshold, keepRatio)
	c.est.observe(4000, 1000)
	return c
}

// 如果 budgetFixture 偏离了预算
// 算术所依赖的"每条消息 400 字符"
// 这一假设，requireUniform 就会
// 让测试失败。
func requireUniform(t *testing.T, msgs []Msg) {
	t.Helper()
	for i, m := range msgs {
		if got := msgChars(m); got != 400 {
			t.Fatalf("budgetFixture message %d is %d characters, not 400 — every budget number in this test is now wrong", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 切点不变量
// ---------------------------------------------------------------------------

// 整个 compact.go 存在的意义，就是
// 守护这一条不变量：如果 canCutBefore
// 说允许切割，切割产生的对话就必须
// 是 API 会接受的对话。之所以在每个
// 索引上断言，而不是挑一个位置断言，
// 是因为这个守卫防的 bug 恰恰是
// "索引 5 处的切割合法，却在索引 9
// 孤立了一个工具结果"——单索引测试
// 永远看不到这种情况。
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

	// 没有这一条，面对一个到处都只
	// 返回 false 的 canCutBefore，整个
	// 测试也会虚假地"通过"——这同样
	// 会让压缩变得不可能，而且是
	// 悄无声息地不可能。
	if legal < 4 {
		t.Fatalf("only %d of %d indices are cuttable; the fixture no longer exercises the invariant", legal, len(msgs))
	}
}

// 上面测试的镜像：拒绝切割的两个原因，
// 每个在夹具中存在且被命名。一个从不
// 拒绝任何东西的 canCutBefore 只有
// 从不被询问时才会通过不变量测试。
func TestCutPointsAreRejectedForTheRightReason(t *testing.T) {
	msgs := convFixture()

	// 索引 2 回答消息 1 中发出的两个
	// 并行调用。在这里切割删除调用
	// 并保留答案。
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

	// 索引 6 是第二个人类回合。总结
	// 作为用户消息注入，所以在这里
	// 切割把两条用户消息放在一起。
	const userTurnIdx = 6
	if msgs[userTurnIdx].Role != RoleUser {
		t.Fatalf("fixture drift: message %d was supposed to be a user turn", userTurnIdx)
	}
	if canCutBefore(msgs, userTurnIdx) {
		t.Errorf("cutting before message %d is allowed, but it is a user message and the summary is also a user message — "+
			"the request would carry two user turns in a row, which some endpoints reject and others silently merge", userTurnIdx)
	}
}

// canCutBefore 检查工具结果**和**
// 角色，工具结果检查之所以不冗余，
// 原因就在这种情况：中立的 Msg
// 类型让任何块都能出现在任何角色
// 里，所以某天新适配器把结果放进
// Assistant 消息，一个只看角色的
// 检查还是会返回 true，放出一个
// 孤立结果。
func TestCanCutBeforeRejectsAToolResultWhateverRoleCarriesIt(t *testing.T) {
	msgs := []Msg{
		TextMsg(RoleUser, "go"),
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolCall, ID: "t1", Name: "bash", Args: `{"command":"ls"}`},
		}},
		// Assistant 消息携带结果。不常见，
		// 且正是角色检查看不到的形状。
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

// safeCut 向前搜索——朝向删除更多——
// 因为压缩在窗口几乎满时运行。向后
// 搜索释放的比要求的少，Agent 在
// 下一个调用时再次撞到墙。
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

// 尾部情况：当从 `want` 起往后全都
// 是工具结果时，就没有合法的切割
// 点，safeCut 必须明说这一点，而
// 不是勉强返回一个相对没那么糟的
// 索引。在这里返回非法索引比拒绝
// 压缩更糟，因为拒绝的代价是一次
// 迟缓的回合，而切错的代价是一个
// 格式错误的请求。
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

// 工具仍在运行时对话所处的状态：
// 最后一条 Assistant 消息发出了调用，
// 没有任何东西回答过它。天真的"每个
// 调用必须有结果"检查标记这个并让
// Agent 拒绝压缩，恰好在它最需要压缩
// 的时候，在工具循环顶部。
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

// 孤立：一个调用被切割走的结果。
// 这是压缩在切割错误位置时引起的失败，
// 并在下一个请求中出现。
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

// 孤立的镜像，更阴险的那个：调用
// 幸存但其答案被丢弃，所以模型相信
// 它发出的命令没有产生任何东西。
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

// 空消息：一个没有文本也没有工具
// 调用的模型。附加它会产生长度
// 为零的内容数组，Anthropic 协议
// 在**下一个**请求而不是这个拒绝。
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

// 估算器必须确定会话的真实每 token
// 字符数，因为每个压缩决定都依赖它。
// 一个从不收敛的估算器压缩太早
// （白白烧掉 cache）或太晚
// （撞到墙）。
func TestEstimatorConverges(t *testing.T) {
	e := newEstimator()

	// 真实比率 3.0 加上 ±8% 的回合间
	// 噪声，这大约是会话在散文和 JSON
	// 之间移动时看起来的样子。
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

// 理智范围存在是因为使用事件和字符
// 计数可能不匹配——某个使用事件
// 对应的调用，其字符从未被测量过。
// 这样一个样本，需要十个好调用
// 才能爬出来。
func TestEstimatorRejectsImpossibleSamples(t *testing.T) {
	e := newEstimator()
	e.observe(3000, 1000) // 真实样本：比率 3.0
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

// 3.6 是猜测。第一次真实测量是
// 证据。把两者平均，会让这个猜测
// 多活一打回合——这已经是一个
// 短会话的大半——而且这一切
// 悄无声息，因为比率看起来
// 依然可信。
func TestEstimatorFirstObservationReplacesTheColdStart(t *testing.T) {
	e := newEstimator()
	if e.ratio != 3.6 {
		t.Fatalf("cold start is %.4f, not 3.6", e.ratio)
	}
	e.observe(2500, 1000) // JSON 繁重会话：每 token 2.5 个字符
	if e.ratio != 2.5 {
		t.Errorf("after one real measurement of 2.5 the ratio is %.4f — the 3.6 cold-start guess was blended in "+
			"rather than replaced, so the first turns of every session estimate against a number nobody measured", e.ratio)
	}
	if e.obs != 1 {
		t.Errorf("observation count is %d after one sample", e.obs)
	}
}

// 章节做出的声明，在测试中。
//
// 估计不必**准确；它必须一致**。
// 下面的供应商按 chars/2.9 计费，
// 外加一笔 Agent 从不看到明细的
// 固定 700 token 信封费——这正是
// 厂商化分词器会搞错的那种系统
// 偏差。因为估算器校准所用的，
// 正是它后来被要求换算的同一个量
// （convChars + baseChars），偏差
// 被吸收进了比率，而不会累积成
// 误差。
func TestEstimatorIsConsistentWithTheProviderItCalibratesAgainst(t *testing.T) {
	// "服务器"：Agent 无法访问的分词器。
	tokenize := func(chars int) int { return int(float64(chars)/2.9) + 700 }

	c := newCompactor(200_000, 0.8, 0.3)
	const baseChars = 12_000 // 系统提示词和工具模式

	var msgs []Msg
	msgs = calibrationTurn(msgs, "t00", 4000)
	msgs = calibrationTurn(msgs, "t01", 4000)

	// 真实循环的十个回合：发送、计费、校准。
	for i := range 10 {
		if i > 0 {
			msgs = calibrationTurn(msgs, fmt.Sprintf("t%02d", i+1), 4000)
		}
		sent := convChars(msgs) + baseChars
		c.est.observe(sent, tokenize(sent))
	}

	// 下一个回合到达。这是墙检查所
	// 基于的预测，在询问服务器之前
	// 取得。
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

// calibrationTurn 附加一个回合的历史——
// 一个问题、一个工具调用、其结果和
// 回复——总共恰好 `chars` 个字符，
// 如 msgChars 计算它们，所以上面的
// 校准算术是准确的。
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

// due 回答"我们靠近墙吗"，它必须
// 依据即将发送的 prompt 的估计来
// 回答，而不是依据最后一次使用
// 报告——那份报告永远慢了一回合。
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

// 压缩这么短的对话会用什么都没有的总结
// 替换真实内容，并花费一个模型调用去做。
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

// 下限。当最新消息单独比整个保留
// 预算都大时，切割历史也释放不出
// 什么要紧的东西——问题是用户必须
// 处理的输出大小，不是对话的长度。
// 说"压缩得更狠"在这里会把读者
// 引向错误的方向。
func TestPlanRefusesWhenTheNewestMessageIsBiggerThanTheBudget(t *testing.T) {
	msgs := budgetFixture(5)
	requireUniform(t, msgs)

	// 10,000 token 窗口的 0.005 是
	// 50 token 预算；每条消息 100。
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

// 从外部看，safeCut 的全部意义就是：
// 预算边界落在哪里就是哪里，plan
// 必须在返回之前，把它挪到一个
// 合法切点。
//
// 算术：20 条消息各 100 token，
// 400 token 预算，所以向后走
// 恰好装下最新的四条，在索引 16
// 停止——用户回合，非法切割位置。
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

// 遍历每个似乎可信的保留预算。
// plan 返回的任何切割方案都必须
// 合法，必须留下一个还有内容的
// 对话，而且绝不能留下少于两条
// 消息——一条消息加一份总结，
// 会让模型回答起来，就像用户刚刚
// 输入的正是这份总结。
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

// 从中间往外切，不是从头开始切。
// 构建日志把错误放在末尾，堆栈
// 跟踪把原因放在末尾，一个只
// 保留头部的 `clip` 能通过每一个
// 天真的长度断言，却把答案扔掉了。
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

	// 跨越多字节 rune 的字节切割不能
	// panic。结果里可以有替换字符；
	// 但不能在压缩中途把进程拖垮。
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

// 总结程序读文字记录，不是对话。
// 工具调用必须作为运行的命令到达，
// 因为 `{"command":"ls -la /srv"}` 在
// JSON 标点上花费 token，读起来像
// 数据结构，而不是动作。
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

// 小模型发出破损工具调用——截断的
// JSON，缺失 `command` 字段。文字记录
// 是发生了什么的最后记录，所以无法
// 解析的调用必须仍然作为某物出现
// 而不是消失。
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

// 这份文字记录，会被送进一次没有
// 定义工具、也没有历史的调用。
// 在其中出现的 id 是总结可以引用
// 的 id，而总结又是幸存对话里的
// 一条用户消息——那个 id 在这里
// 指代什么都没有。
func TestFlattenLeaksNoToolCallIDs(t *testing.T) {
	got := flatten(convFixture(), 4000)
	for _, id := range convFixtureToolIDs {
		if strings.Contains(got, id) {
			t.Errorf("tool call id %q reached the summariser; anything it writes about that id survives into a conversation "+
				"where the call no longer exists", id)
		}
	}
	// 理智检查：文字记录不为空，所以
	// 上面的循环是在一个确实有内容
	// 的地方，什么也没找到。
	if len(got) < 200 {
		t.Fatalf("the flattened transcript is only %d characters; it cannot be a faithful rendering of a 14-message conversation", len(got))
	}
}
