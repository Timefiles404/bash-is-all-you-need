package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// jsonIsOpen——决定该怪谁的那道判别
// ---------------------------------------------------------------------------

// faultCut 和 faultNotJSON 都表示"这段没解析出来"，但绝不能报成同一件事：
// 一个是模型写到值中间把预算用光了，另一个是模型（或者网关）发来的东西
// 压根就不是 JSON。跟被截断的模型说它的 JSON 不合法，这是误诊；而模型
// 回应误诊的方式，就是把那条同样太长的命令再发一遍。
func TestJSONIsOpenSeparatesTruncationFromGarbage(t *testing.T) {
	open := []string{
		`{"command": "find`,
		`{"command": "find /srv/app -name '*.go' -not -path '*/vendor`,
		`{`,
		`{"command":`,
		`{"a":1,`,
		`[1,2`,
		`{"command":"c:\\`, // 在反斜杠和它转义的字符中间截断
		`{"command": "ls", "shell":`,
		// 只有被截断的值本身，外面没有容器处在未闭合状态。这里唯一的证据
		// 就是那个没闭合的**字符串**——正是这一条证明了字符串跟踪是承重
		// 的，不是括号深度的冗余。
		`"find /srv`,
	}
	for _, s := range open {
		if !jsonIsOpen(s) {
			t.Errorf("jsonIsOpen(%q) = false; this is a truncation and would be reported to the model as "+
				"malformed JSON, which is a diagnosis it cannot act on", s)
		}
	}

	closed := []string{
		`description: survey the docs`,
		`I will run: echo hi`,
		`{]`,
		`{"command":"ls"}`,
		`{}`,
		``,
		`<tool_call>`,
		`{"command":"ls"} trailing words`,
		// 花括号出现在字符串值**里面**。这是完整的；不跟踪字符串的扫描器
		// 会把 `{` 数进去，判成截断——于是本来能执行的命令被当成截断拒掉。
		`{"command":"echo {"}`,
		// 值里面有个**被转义的**引号。忽略转义的扫描器会提前把字符串收掉，
		// 在收尾的引号处又开出新的一串，然后把这次完整的调用报成截断。
		`{"command":"a\""}`,
	}
	for _, s := range closed {
		if jsonIsOpen(s) {
			t.Errorf("jsonIsOpen(%q) = true; a closed payload would be reported as a truncation, so the model "+
				"is told to retry something that will fail identically", s)
		}
	}
}

// 分类必须扛得住字符串里的引号，因为这个 Agent 处理的每个参数都是 shell
// 命令，而 shell 命令里全是引号。§A3c 的载荷条条都带引号。
func TestJSONIsOpenHandlesQuotesInsideStrings(t *testing.T) {
	// 完整的：里层的引号都转义了，值也收尾了。
	if jsonIsOpen(`{"command":"grep -Hn \"TODO(security)\" ."}`) {
		t.Error("a complete call whose value contains escaped quotes was read as truncated")
	}
	// 在转义引号之后被截断——还在字符串里面。
	if !jsonIsOpen(`{"command":"grep -Hn \"TODO`) {
		t.Error("a call truncated inside an escaped-quote value was read as complete")
	}
	// 字符串**里面**的右花括号不能把对象闭合掉。
	if !jsonIsOpen(`{"command":"find . -exec grep x {} +`) {
		t.Error("a brace inside the string value was counted as closing the object")
	}
}

// ---------------------------------------------------------------------------
// checkCall 作用于 bash 工具
// ---------------------------------------------------------------------------

func TestCheckCallOnTheBashTool(t *testing.T) {
	def := bashToolDef()

	cases := []struct {
		name      string
		raw       string
		wantFault argFault
		wantCmd   string
	}{
		{"a normal call", `{"command":"ls -la"}`, faultNone, "ls -la"},
		{"the §A3c truncation shape", `{"raw_arguments":"{\"command\": \"find"}`, faultCut, ""},
		{"truncated mid-command", `{"command":"go test ./sta`, faultCut, ""},
		{"prose", `I will list the files`, faultNotJSON, ""},
		{"the §A2 markup", `<tool_call>` + "\n" + `<function=bash>`, faultNotJSON, ""},
		{"missing the required field", `{"shell":"bash"}`, faultSchema, ""},
		{"command as a number", `{"command":42}`, faultSchema, ""},
		{"command as an array", `{"command":["echo","hi"]}`, faultSchema, ""},
		{
			// §E13：让端点给出数组时，它返回的就是这个——数组被序列化
			// **进了**声明的类型里。它符合 schema，它又不是 shell 命令，
			// schema 校验的边界就这样写成了测试。
			"the array serialised into a string is accepted",
			`{"command":"[\"echo\",\"hi\"]"}`, faultNone, `["echo","hi"]`,
		},
		{"a zero-argument call is a missing required field", ``, faultSchema, ""},
		{"an explicitly empty object, likewise", `{}`, faultSchema, ""},
	}

	for _, c := range cases {
		got := checkCall(def, c.raw)
		if got.Fault != c.wantFault {
			t.Errorf("%s: fault = %q, want %q (detail %q)", c.name, got.Fault, c.wantFault, got.Detail)
			continue
		}
		if c.wantFault == faultNone && strArg(got, "command") != c.wantCmd {
			t.Errorf("%s: command = %q, want %q", c.name, strArg(got, "command"), c.wantCmd)
		}
	}
}

// 这道边界不能把整个被截断的载荷夹带进历史：detail 会在这次会话余下的
// 每一次请求里重发一遍。要保住的性质是 detail 的大小不随载荷的大小走，
// 这比任何具体上限都更强：clip() 会加省略标记，所以确切的字节数是实现
// 细节，而那份*不依赖*不是。
func TestFaultDetailIsBounded(t *testing.T) {
	frag := "echo alpha bravo charlie delta; "
	short := checkCall(bashToolDef(), `{"command":"`+strings.Repeat(frag, 200))
	long := checkCall(bashToolDef(), `{"command":"`+strings.Repeat(frag, 20000))

	if short.Fault != faultCut || long.Fault != faultCut {
		t.Fatalf("faults = %q / %q, want both %q", short.Fault, long.Fault, faultCut)
	}
	// 不是严格相等：clip 的标记会写明它省掉了多少字节，所以长度是随载荷大
	// 小的**位数**增长的。对数级是有界的，线性不是——这个测试存在，就是
	// 为了抓住线性。
	if grew := len(long.Detail) - len(short.Detail); grew > 8 {
		t.Errorf("detail grew %d bytes when the payload grew 100x (%d -> %d); it goes into the message array "+
			"and is re-sent every turn thereafter, so its size must not track the payload's",
			grew, len(short.Detail), len(long.Detail))
	}
	if len(long.Detail) > 2*maxDetail {
		t.Errorf("detail is %d bytes against a %d-byte budget", len(long.Detail), maxDetail)
	}
}

// ---------------------------------------------------------------------------
// schema 的子集
// ---------------------------------------------------------------------------

// §E13 量过：上游没有任何一环在执行 `enum`。如果这边也不执行，那这个关
// 键字就只是装饰，而请求还在为它付 token。
func TestSchemaEnforcesEnumBecauseNobodyElseDoes(t *testing.T) {
	tool := Tool{
		Name: "runner",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
				"shell":   map[string]any{"type": "string", "enum": []any{"bash", "sh"}},
			},
			"required": []string{"command", "shell"},
		},
	}

	// §E13 从 OpenAI 那条路上拿回来的原样 body。
	got := checkCall(tool, `{"command": "echo hi", "shell": "powershell"}`)
	if got.Fault != faultSchema {
		t.Fatalf("fault = %q, want %q — the endpoint returned this verbatim with a 200 and a normal "+
			"finish_reason, so this check is the only one there is", got.Fault, faultSchema)
	}
	if !strings.Contains(got.Detail, "shell") || !strings.Contains(got.Detail, "powershell") {
		t.Errorf("the detail names neither the field nor the value: %q", got.Detail)
	}

	if got := checkCall(tool, `{"command": "echo hi", "shell": "sh"}`); got.Fault != faultNone {
		t.Errorf("an in-enum value was rejected as %q (%s)", got.Fault, got.Detail)
	}
}

// `integer` 必须能匹配 JSON number，因为检查跑起来的时候，json.Unmarshal
// 早就把整数和浮点一起塌成 float64 了——这个区别观察不到，拒了它就等于
// 拒掉任何工具声明过的每一个整数参数。
//
// 这里也钉住了那份刻意的宽松：5.0 和超出范围的值都放过，因为拒绝要付一
// 整个往返，而工具那边加道 clamp 就能不花代价把这事修好。
func TestIntegerPropertiesAcceptNumbers(t *testing.T) {
	tool := Tool{Name: "reader", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		},
		"required": []string{"limit"},
	}}

	for _, raw := range []string{`{"limit":20}`, `{"limit":5.0}`, `{"limit":9999}`, `{"limit":-1}`} {
		if got := checkCall(tool, raw); got.Fault != faultNone {
			t.Errorf("checkCall(%s) = %q (%s); an integer declaration that rejects numbers rejects every "+
				"integer argument, because json.Unmarshal has already made them all float64",
				raw, got.Fault, got.Detail)
		}
	}
	// 字符串仍然是字符串。
	if got := checkCall(tool, `{"limit":"20"}`); got.Fault != faultSchema {
		t.Errorf(`checkCall({"limit":"20"}) = %q, want %q`, got.Fault, faultSchema)
	}
}

// `additionalProperties: false` 是靠剪掉来兑现的，不是靠拒绝。§E13 量到过
// 模型在明令禁止的 schema 下加了 `timeout_ms`，值还是字符串 "5000"；工具
// 读不到它，剪掉它不改变任何跑起来的东西，而拒绝要付一次往返。
func TestUndeclaredPropertiesArePrunedNotRefused(t *testing.T) {
	strict := Tool{
		Name: "runner",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"command": map[string]any{"type": "string"}},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
	got := checkCall(strict, `{"command": "echo hi", "timeout_ms": "5000"}`)
	if got.Fault != faultNone {
		t.Fatalf("fault = %q (%s); refusing costs a whole round trip to remove a key that was already "+
			"going to be ignored", got.Fault, got.Detail)
	}
	if _, still := got.Args["timeout_ms"]; still {
		t.Error("the undeclared key survived into Args; the schema said this field does not exist here, " +
			"and leaving it in makes the declaration decoration")
	}
	if len(got.Dropped) != 1 || got.Dropped[0] != "timeout_ms" {
		t.Errorf("Dropped = %v, want [timeout_ms]; an unreported drop is a silent divergence between what "+
			"the model asked for and what happened", got.Dropped)
	}

	// 什么都没说的 schema 沿用 JSON Schema 的默认值，把它们收下。
	lax := Tool{
		Name: "runner",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	}
	got = checkCall(lax, `{"command": "echo hi", "timeout_ms": "5000"}`)
	if len(got.Dropped) != 0 {
		t.Errorf("Dropped = %v on a schema that never forbade extras; the behaviour must follow the "+
			"declaration, not a house preference", got.Dropped)
	}
	if _, ok := got.Args["timeout_ms"]; !ok {
		t.Error("the extra key was removed anyway")
	}
}

// `required` 从 Go 里搭出来的 Tool 过来是 []string，从走过一趟 JSON 往返的
// Tool 过来是 []any——回放的 trace、配置文件都算。只认一种形态的校验器，
// 每个测试都能过，然后在回放里挂掉。
func TestRequiredNamesAcceptsBothEncodings(t *testing.T) {
	for _, req := range []any{
		[]string{"command"},
		[]any{"command"},
	} {
		tool := Tool{Name: "runner", Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   req,
		}}
		if got := checkCall(tool, `{}`); got.Fault != faultSchema {
			t.Errorf("required as %T: a call missing the required field was accepted", req)
		}
		if got := checkCall(tool, `{"command":"ls"}`); got.Fault != faultNone {
			t.Errorf("required as %T: a valid call was rejected as %q", req, got.Fault)
		}
	}
}

// Go 里 map 的遍历顺序是随机的，不排序地走一遍，每跑一次点到的字段都不
// 一样——写出来的 bug 报告没人复现得了。
func TestSchemaViolationIsDeterministic(t *testing.T) {
	tool := Tool{Name: "runner", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"b": map[string]any{"type": "string"},
			"c": map[string]any{"type": "string"},
			"d": map[string]any{"type": "string"},
		},
	}}
	raw := `{"d":1,"c":2,"b":3,"a":4}`
	first := checkCall(tool, raw).Detail
	for i := 0; i < 40; i++ {
		if got := checkCall(tool, raw).Detail; got != first {
			t.Fatalf("detail differs between runs: %q then %q", first, got)
		}
	}
	if !strings.Contains(first, `"a"`) {
		t.Errorf("the first violation reported is %q; sorted order should reach \"a\" first", first)
	}
}

// ---------------------------------------------------------------------------
// 关于重发文本的那条规矩
// ---------------------------------------------------------------------------

// 这个文件放进工具结果里的每一句话，都是往 prompt 里永久加了一笔。里面
// 只要有祈使句，几个回合之后——让它讲得通的那段上下文早已滚出去了——就
// 会被读成一条崭新的指令，于是模型把一次早就处理完的调用又发了一遍。
//
// 这是给那条规矩加的机械看守。值得机械地守，是因为这些字符串写着写着，
// 最自然的样子就是建议。
func TestReplayedTextContainsNoInstructions(t *testing.T) {
	imperatives := []string{
		"send ", "retry", "try again", "do not ", "don't ", "please ",
		"you should", "you must", "make sure", "instead, ", "next time",
	}

	var texts []string
	for _, f := range []argFault{faultCut, faultNotJSON, faultSchema} {
		texts = append(texts, faultText(bashToolDef(), argCheck{Fault: f, Detail: "the required \"command\" field is absent"}))
	}
	// dispatch 自己产出的那些字符串。抄在这里，是因为测试不跑 shell 就够不
	// 着它们。
	texts = append(texts,
		"[not executed: the session was stopped]",
		"[not executed: the command was an empty string]",
		"[not executed: the prompt was blank, so there was no task to delegate]",
		"[not executed: the reply was cut off at max_tokens]",
		"[the user denied this subagent]",
		"[the user denied this command]",
		"[the user stopped the session]",
	)

	for _, txt := range texts {
		low := strings.ToLower(txt)
		for _, imp := range imperatives {
			if strings.Contains(low, imp) {
				t.Errorf("a replayed tool result contains the instruction %q:\n  %s\n"+
					"this string is re-sent on every subsequent request; an instruction in it will be "+
					"obeyed later, out of context", imp, txt)
			}
		}
	}
}

// 故障文本还是得点出是哪个工具，否则一回合里有好几次调用时，就会出来好
// 几条一模一样的拒绝，模型分不清被拒的到底是哪一次。
func TestFaultTextNamesTheTool(t *testing.T) {
	for _, f := range []argFault{faultCut, faultNotJSON} {
		txt := faultText(bashToolDef(), argCheck{Fault: f, Detail: "x"})
		if !strings.Contains(txt, "bash") {
			t.Errorf("fault %q produced %q, which does not name the tool", f, txt)
		}
	}
}

// 截断消息不能把那段碎片原样引回去。§A3c 的载荷是好几百字节的 shell 命
// 令，模型自己写的，引回去就得永远重发下去。
func TestCutTextDoesNotQuoteTheFragment(t *testing.T) {
	frag := "find /srv/app -name '*.go' -not -path '*/vendor"
	txt := faultText(bashToolDef(), argCheck{Fault: faultCut, Detail: frag})
	if strings.Contains(txt, "vendor") {
		t.Errorf("the truncated fragment was echoed into the history:\n  %s", txt)
	}
}

// ---------------------------------------------------------------------------
// uniqueIDs
// ---------------------------------------------------------------------------

// 网关可能给它发出的每一次调用都用同一个 id。回合之内，除了配对的那条
// 结果，没别的东西读这个 id，所以它照样能用；跨回合就不行了：协议会因
// 为 `tool_use` id 重复把整个请求拒掉，而拒绝里点的是消息下标，不是工具。
func TestUniqueIDsRenamesCollisionsAcrossTurns(t *testing.T) {
	seen := map[string]bool{}

	turn1 := []Block{{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"}}
	if n := uniqueIDs(turn1, seen); n != 0 {
		t.Fatalf("renamed %d ids on the first turn; there was nothing to collide with", n)
	}
	if turn1[0].ID != "call_go_0" {
		t.Errorf("the first use of an id was renamed to %q; only collisions should move", turn1[0].ID)
	}

	// 同一个 id 又来了，这回在**另一条** assistant 消息里。按回合做的检查在
	// 这里什么都看不见——seen 要横跨整个会话，正是为了这个。
	turn2 := []Block{{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"}}
	if n := uniqueIDs(turn2, seen); n != 1 {
		t.Fatalf("renamed %d ids, want 1", n)
	}
	if turn2[0].ID == "call_go_0" {
		t.Error("the duplicate id survived; the next request is rejected for a duplicate tool_use id")
	}

	// 再来第三次，好确认改名不是在两个值之间来回翻。
	turn3 := []Block{{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"}}
	uniqueIDs(turn3, seen)
	if turn3[0].ID == turn2[0].ID || turn3[0].ID == "call_go_0" {
		t.Errorf("third occurrence became %q, colliding again with %q", turn3[0].ID, turn2[0].ID)
	}
}

func TestUniqueIDsHandlesCollisionsWithinOneTurn(t *testing.T) {
	seen := map[string]bool{}
	calls := []Block{
		{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"},
		{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"},
		{Kind: BlockToolCall, ID: "call_go_0", Name: "bash"},
	}
	if n := uniqueIDs(calls, seen); n != 2 {
		t.Fatalf("renamed %d of 3 identical ids, want 2", n)
	}
	ids := map[string]bool{}
	for _, c := range calls {
		if ids[c.ID] {
			t.Fatalf("duplicate id %q survived within one turn: %v", c.ID, calls)
		}
		ids[c.ID] = true
	}
}

// 结果是靠 id 找到自己那次调用的，所以没有 id 的调用就没法被应答——而没
// 被应答的调用，就是一次被拒的请求。
func TestUniqueIDsFillsInAMissingID(t *testing.T) {
	calls := []Block{{Kind: BlockToolCall, ID: "", Name: "bash"}}
	uniqueIDs(calls, map[string]bool{})
	if calls[0].ID == "" {
		t.Error("a call with no id was left without one; its result has nothing to address")
	}
}

// 网关会按 64 校验 `call_id` 的长度。改名改超了，等于拿一次 id 重复的拒绝
// 换一次 id 过长的拒绝。
func TestUniqueIDsStaysInsideTheLengthLimit(t *testing.T) {
	// 63 个字符：比上限少一个，所以**任何**后缀都会把改名顶出上限。底子短
	// 一点就给 `_2` 留出了位置，上限永远碰不到——那种测试能过，却根本没碰
	// 它名字里写的那件事。
	long := "call_" + strings.Repeat("x", 58)
	if len(long) != 63 {
		t.Fatalf("fixture is %d chars, not 63", len(long))
	}
	seen := map[string]bool{long: true}
	calls := []Block{{Kind: BlockToolCall, ID: long, Name: "bash"}}
	uniqueIDs(calls, seen)
	if len(calls[0].ID) > 64 {
		t.Errorf("renamed id is %d chars: %q", len(calls[0].ID), calls[0].ID)
	}
	if calls[0].ID == long {
		t.Error("the collision was not resolved")
	}
}

// 只有工具调用的 id 值得去重，文本块压根没有 id——硬写一个上去，就是往
// 线上塞了个协议根本没地方放的字段。
func TestUniqueIDsIgnoresNonToolCallBlocks(t *testing.T) {
	blocks := []Block{
		{Kind: BlockText, Text: "hello"},
		{Kind: BlockThinking, Text: "hmm"},
	}
	seen := map[string]bool{}
	if n := uniqueIDs(blocks, seen); n != 0 {
		t.Errorf("renamed %d ids among blocks that have none", n)
	}
	if len(seen) != 0 {
		t.Errorf("non-call blocks contributed %d ids to the seen set", len(seen))
	}
}

// ---------------------------------------------------------------------------
// stripHarnessMarkup——§A2，原样
// ---------------------------------------------------------------------------

// 这些就是工具调用被截断时，OpenAI 那条路返回的 `message.content` 原值，
// 在三个 max_tokens 取值下各抓了一次。三次的 finish_reason 都是 "length"，
// tool_calls 都是空数组。
func TestStripHarnessMarkupOnTheObservedPayloads(t *testing.T) {
	observed := []string{
		"<tool_call>\n<function=bash>\n<parameter=",
		"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name \"*.go\" -not -path \"*/",
		"<tool_call>\n<function=bash>\n<parameter=command>find /srv/app -type f -name \"*.go\" -not -path \"*/vendor/*\" -not -path \"*/testdata/*\" -mtime -14 -exec grep -Hn \"TODO(security)\" {} +",
		"<tool_call>\n<function=b",
	}
	for _, s := range observed {
		clean, found := stripHarnessMarkup(s)
		if !found {
			t.Errorf("the gateway's own markup was not recognised: %q", s)
		}
		if clean != "" {
			t.Errorf("stripping %q left %q; the markup is the whole content here", s, clean)
		}
	}
}

// 被截断的回合，在宿主标记开始之前可能真说了点什么，那一段是模型在跟用户
// 讲话。
func TestStripHarnessMarkupKeepsRealTextBeforeIt(t *testing.T) {
	clean, found := stripHarnessMarkup("Let me look at that.\n\n<tool_call>\n<function=bash>")
	if !found {
		t.Fatal("markup not found")
	}
	if clean != "Let me look at that." {
		t.Errorf("clean = %q, want %q", clean, "Let me look at that.")
	}
}

// 不含宿主标记的文本必须一个字节不差地回来，否则每个正常回合都会被悄悄改写。
func TestStripHarnessMarkupLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"Here is the answer.",
		"A function=bash mention with no angle brackets.",
		"Trailing whitespace matters   ",
	} {
		clean, found := stripHarnessMarkup(s)
		if found {
			t.Errorf("markup reported in %q", s)
		}
		if clean != s {
			t.Errorf("ordinary text was rewritten: %q -> %q", s, clean)
		}
	}
}

// ---------------------------------------------------------------------------
// mergeArgs——三种方言
// ---------------------------------------------------------------------------

func TestMergeArgsIncrementalDialect(t *testing.T) {
	// §B4 观测到的切法：碎片不按 JSON 的边界对齐。
	frags := []string{`{"comm`, `and":"ec`, `ho hi"`, `}`}
	got := ""
	for _, f := range frags {
		got = mergeArgs(got, f)
	}
	if got != `{"command":"echo hi"}` {
		t.Errorf("got %q; the incremental dialect is what this endpoint actually sends", got)
	}
}

// 网关要是在最后一个 chunk 里把完整参数再发一遍，闷头拼接就会拼出
// `{...}{...}`，报出来的错只说字节偏移，对成因只字不提。
func TestMergeArgsReSendDialect(t *testing.T) {
	got := ""
	for _, f := range []string{`{"comm`, `and":"ls"`, `}`, `{"command":"ls"}`} {
		got = mergeArgs(got, f)
	}
	if got != `{"command":"ls"}` {
		t.Errorf("got %q, want a single top-level value; a tool call's arguments are exactly one", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("the accumulated arguments do not parse: %q", got)
	}
}

func TestMergeArgsCumulativeDialect(t *testing.T) {
	got := ""
	for _, f := range []string{`{"command"`, `{"command":"l`, `{"command":"ls"}`} {
		got = mergeArgs(got, f)
	}
	if got != `{"command":"ls"}` {
		t.Errorf("got %q; each fragment was a superset of the last", got)
	}
}

// 完整值后面又跟来半截，那是重发本身也被截断了。该留的是完整的那个——为
// 了那截碎片把它丢掉，会把一次本来能用的调用变成截断。
func TestMergeArgsKeepsTheCompleteValueOverATrailingFragment(t *testing.T) {
	got := mergeArgs(`{"command":"ls"}`, `{"comm`)
	if got != `{"command":"ls"}` {
		t.Errorf("got %q, want the complete value", got)
	}
}

func TestMergeArgsEmptyFragmentsAreIgnored(t *testing.T) {
	if got := mergeArgs(`{"a":1}`, ""); got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
	if got := mergeArgs("", `{"a":1}`); got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
	if got := mergeArgs("", ""); got != "" {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// renderArgs——§E14 的那个 400
// ---------------------------------------------------------------------------

// 在 OpenAI 那条路上，`arguments: ""` 是 HTTP 400，而 400 是致命的，所以
// 历史里只要有一次零参数的工具调用，会话就完了。`{}` 是收的。这是
// anthropicToolInput 的对称另一半，阶段 10 只做了一边。
func TestRenderArgsNeverEmitsTheEmptyString(t *testing.T) {
	for _, in := range []string{"", " ", "\t\n"} {
		if got := renderArgs(in); got != "{}" {
			t.Errorf("renderArgs(%q) = %q, want {} — the empty string is a 400 that ends the session", in, got)
		}
	}
	// 别的都逐字节透传：重新序列化会打断字节级的 prompt 缓存，因为 Go 把
	// map 的遍历顺序随机化了。
	for _, in := range []string{`{"command":"ls"}`, `{ "command" : "ls" }`, `not json`} {
		if got := renderArgs(in); got != in {
			t.Errorf("renderArgs(%q) = %q; the bytes must pass through unchanged", in, got)
		}
	}
}

// 两个协议对零参数调用必须口径一致，各按各的形状来：这边是字符串 `{}`，
// 那边是对象 `{}`。
func TestBothProtocolsRenderAZeroArgumentCallAsAnEmptyObject(t *testing.T) {
	if got := renderArgs(""); got != "{}" {
		t.Errorf("openai side: %q", got)
	}
	if got := string(anthropicToolInput("")); got != "{}" {
		t.Errorf("anthropic side: %q", got)
	}
}

// ---------------------------------------------------------------------------
// argsForDisplay——宽松解析，关在这里
// ---------------------------------------------------------------------------

// 工具调用绝不能从面板或者压缩摘要里消失掉：文字记录是"Agent 试过、没成"
// 这件事最后的凭据。
func TestArgsForDisplayNeverReturnsBlank(t *testing.T) {
	for _, in := range []string{
		`{"raw_arguments":""}`,
		`{"command":"  "}`,
		`{"command":""}`,
		`{"command":"go test ./sta`,
		`{}`,
		`nonsense`,
	} {
		got := argsForDisplay(in)
		if strings.TrimSpace(got) == "" {
			t.Errorf("argsForDisplay(%q) = %q; the call disappears from the transcript entirely", in, got)
		}
	}
}

func TestArgsForDisplayPrefersTheCommand(t *testing.T) {
	if got := argsForDisplay(`{"command":"ls -la"}`); got != "ls -la" {
		t.Errorf("got %q", got)
	}
	if got := argsForDisplay(`{"raw_arguments":"{\"command\": \"find"}`); got != `{"command": "find` {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// dispatch，穿过这道边界
// ---------------------------------------------------------------------------

// 协议要求 assistant 消息里的每一次工具调用都得到应答。在 dispatch 前面加
// 一道校验边界，恰恰就是那种会开始漏掉结果的改动——被拒的调用照样要它那
// 条应答。
func TestDispatchAnswersEveryCallEvenWhenAllOfThemAreInvalid(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	calls := []Block{
		{Kind: BlockToolCall, ID: "c1", Name: "bash", Args: `{"raw_arguments":"{\"command\": \"find"}`},
		{Kind: BlockToolCall, ID: "c2", Name: "bash", Args: `{"command":"go test ./sta`},
		{Kind: BlockToolCall, ID: "c3", Name: "bash", Args: `I will list the files`},
		{Kind: BlockToolCall, ID: "c4", Name: "bash", Args: `{}`},
		{Kind: BlockToolCall, ID: "c5", Name: "bash", Args: `{"command":"  "}`},
		{Kind: BlockToolCall, ID: "c6", Name: "nosuchtool", Args: `{}`},
	}

	results, disp := a.dispatch(context.Background(), 1, calls)
	if disp.stop {
		t.Fatal("dispatch reported the session stopped")
	}
	if len(results) != len(calls) {
		t.Fatalf("%d results for %d calls; the provider rejects a request whose tool calls are not all answered",
			len(results), len(calls))
	}
	for i, r := range results {
		if r.ID != calls[i].ID {
			t.Errorf("result %d answers %q, not %q", i, r.ID, calls[i].ID)
		}
		if strings.TrimSpace(r.Text) == "" {
			t.Errorf("result %d (%s) is empty; the model is told nothing at all happened", i, r.ID)
		}
		if !strings.Contains(r.Text, "not executed") && !strings.Contains(r.Text, "no tool called") {
			t.Errorf("result %d (%s) does not say the call was refused: %q", i, r.ID, r.Text)
		}
	}

	// 六个里五个是参数故障；第六个是未知工具，那是另一回事，不算参数故障。
	var invalid int
	for _, e := range rec.events {
		if e.Kind == KindToolCallInvalid {
			invalid++
			if e.Fault == "" {
				t.Error("a tool_call_invalid event carries no fault class; the class is what you count")
			}
		}
	}
	if invalid != 5 {
		t.Errorf("%d tool_call_invalid events for 5 invalid calls; a rejection missing from the trace is a "+
			"rejection nobody can measure the rate of", invalid)
	}
}

// 被拒的调用不能碰到 shell。就是这条断言让这道边界成其为边界，而不是一
// 份报告。
func TestDispatchRunsNothingForARefusedCall(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	// 把 §A3c 那次截断"补齐"之后的样子。边界要是放它过去，这就会跑一条不
	// 带任何参数的 `find`，把整棵树列出来。
	calls := []Block{
		{Kind: BlockToolCall, ID: "c1", Name: "bash", Args: `{"raw_arguments":"{\"command\": \"find"}`},
	}
	a.dispatch(context.Background(), 1, calls)

	for _, e := range rec.events {
		switch e.Kind {
		case KindCommandStart, KindCommandEnd, KindToolCallReady:
			t.Errorf("a refused call produced %q — it reached the shell", e.Kind)
		case KindGateVerdict:
			t.Error("a refused call reached the permission gate; the human is being asked about a command " +
				"the model never finished writing")
		}
	}
}

// 未声明的键被剪掉，走的是 notice，不是工具结果：模型要了个工具没有的东
// 西，这值得让人看见，但不值得往后每一次请求都背着这段历史。
func TestDispatchReportsADroppedArgumentAsANoticeOnly(t *testing.T) {
	a, rec := mulAgent(&gate{yolo: true}, mulShell(t))

	calls := []Block{
		{Kind: BlockToolCall, ID: "c1", Name: "bash",
			Args: `{"command":"echo s11-drop","timeout_ms":5000}`},
	}
	results, _ := a.dispatch(context.Background(), 1, calls)

	if !strings.Contains(results[0].Text, "s11-drop") {
		t.Errorf("the command did not run despite being valid: %q", results[0].Text)
	}
	if strings.Contains(results[0].Text, "timeout_ms") {
		t.Errorf("the dropped key was reported in the tool result, which is replayed forever: %q", results[0].Text)
	}
	var noticed bool
	for _, e := range rec.events {
		if e.Kind == KindNotice && strings.Contains(e.Text, "timeout_ms") {
			noticed = true
		}
	}
	if !noticed {
		t.Error("the dropped argument was not reported anywhere; a silent drop is a divergence between what " +
			"the model asked for and what happened")
	}
}
