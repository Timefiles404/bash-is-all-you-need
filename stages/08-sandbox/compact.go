// Stage 05——压缩。
//
// 上下文窗口是一面墙，一个跑得够久的 Agent 迟早会撞上它。
// 压缩，就是接下来发生的事：扔掉大部分历史记录，换成一段
// 摘要，然后继续跑。
//
// 这个想法一句话就能说完。真正费劲的全在细节里，而这个
// 文件，讲的就是这些细节：
//
//   - 不是随便什么地方都能切。在一次工具调用和它的结果之间
//     切一刀，下一个请求就会格式错误——而这个 API 错误要
//     等到几个回合之后才会冒出来，它的栈跟踪指向的是请求
//     构建器，而不是当初那一刀。
//   - 不数 token，就不知道该在哪里切；而数 token 需要一个
//     tokenizer，这个仓库偏偏没有这个依赖。所以它是拿 API
//     已经报出来的数字，去校准出一个估算值。
//   - 摘要本身也是一次模型调用，要花 token、也要花时间；
//     一个把这一点藏起来的实现，就是在会话成本上撒谎。
//   - 压缩会重写 prompt 前缀，把 stage 04 花了一整章功夫
//     才赚到的缓存条目全部摧毁。这不是免费的，是一笔交易，
//     而唯一能知道这笔交易划不划算的办法，是把两边都测出来。
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 计 token 而不需要 tokenizer
// ---------------------------------------------------------------------------

// estimator 通过对照 API 已经报告的 token 计数做校准，把
// 字符转换成 token。
//
// 常见的建议是"自己带一个 tokenizer"。但对于决定**何时
// 压缩**这件事来说，那是用错了工具：tokenizer 是个不小的
// 依赖，还得按模型区分，它在工具 schema 和消息信封的框架
// 开销上算出来的数字，和服务器本身对不上，而且它告诉你的
// 东西，没有一样不是你能免费拿到的——因为每一个响应本身，
// 就已经带着你刚发的那个 prompt 的精确 token 数。
//
// 所以：发一个 prompt，记下它有多少字符、服务器说它算多少
// token，你就有了一个比率。每次调用都这么记，这个比率就会
// 收敛到这场对话里散文、代码和 JSON 的真实混合比例。
//
// 微妙的地方，也是这套办法能成立的原因：**这个估算不需要
// 准，只需要稳。** 它唯一的用途，是回答"我们是不是快撞墙
// 了"，而它自己也是拿同一套字符计数校准出来的，回头又被
// 拿来做同一种换算。像 JSON 信封开销、工具 schema、系统
// 提示词这些系统性偏差，都被吸收进了这个比率里，而不会
// 累积成误差。真正会搞砸它的，是测一样东西、估算另一样。
type estimator struct {
	ratio float64 // 每 token 的字符数
	obs   int
}

// 3.6 是英语散文、代码和 JSON 混合场景下合理的冷启动值；
// 纯英语更接近 4.0，密集 JSON 更接近 2.5。它只对第一次
// 调用要紧，往后就交给测量接管。
func newEstimator() *estimator { return &estimator{ratio: 3.6} }

// observe 记录一次真实的测量：发送的字符数，计费的 token 数。
func (e *estimator) observe(chars, tokens int) {
	if chars <= 0 || tokens <= 0 {
		return
	}
	r := float64(chars) / float64(tokens)
	// 一个落在这个范围之外的比率，说明这两个数字测的不是同一个
	// 请求——最可能的情况是：一个用量事件，对应的是一次我们从
	// 没记下字符数的调用。丢掉这个数据点，好过让一个坏样本把
	// 估算值拖到一个要花十次调用才能爬回来的地方。
	if r < 1.0 || r > 20.0 {
		return
	}
	if e.obs == 0 {
		e.ratio = r
	} else {
		// 指数移动平均，权重向历史倾斜。比率会缓慢而真实地漂移
		// ——一段对话如果一开始是散文，后来变成在读 JSON 文件，
		// 这种变化是真实发生的——所以比率应该跟着变，但不应该
		// 因为一个反常的回合就猛地一冲。
		e.ratio = 0.75*e.ratio + 0.25*r
	}
	e.obs++
}

func (e *estimator) tokens(chars int) int {
	if e.ratio <= 0 {
		return chars / 4
	}
	return int(float64(chars)/e.ratio + 0.5)
}

// msgChars 计数一条消息贡献给 prompt 的字符。
//
// 它计入文本、工具调用参数和工具结果——所有会被重新发送的
// 东西——并忽略结构性 JSON。思维块之所以被计入，是因为这个
// 仓库会在发送前把它们丢弃——如果这里计入、那里却不计入，
// 这正是那种会带偏校准的不对称。但情况并非如此：见
// runTurn，thinking 从不进入历史。
func msgChars(m Msg) int {
	n := 0
	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText, BlockToolResult:
			n += len(b.Text)
		case BlockToolCall:
			n += len(b.Name) + len(b.Args)
		}
	}
	return n
}

func convChars(msgs []Msg) int {
	n := 0
	for _, m := range msgs {
		n += msgChars(m)
	}
	return n
}

// ---------------------------------------------------------------------------
// 允许切割的地方
// ---------------------------------------------------------------------------

// canCutBefore 报告是否 `[summary] + msgs[i:]` 是 API 会接受
// 的一个对话。
//
// 两个条件，第一个是人人都会犯的 bug：
//
//  1. **msgs[i] 必须不包含工具结果。** 它匹配的工具调用住在
//     msgs[i-1]，压缩马上就要把它删掉。一个调用已经不在了
//     的工具结果，就是孤立工具结果，两个协议都会拒绝它——
//     OpenAI 报 "messages with role 'tool' must be a response
//     to a preceding message with tool_calls"，Anthropic 报
//     一个意外的 `tool_use_id`。失败会在*下一个*请求上才
//     冒出来，所以栈跟踪指向的是请求构建器，而真正的错误
//     其实在压缩器里，隔着一百行。
//
//  2. **msgs[i] 必须是一条 Assistant 消息。** 摘要是作为用户
//     消息注入的，所以如果切割点在另一条用户消息前面，就会
//     产生连续两条用户消息。有些端点会合并它们，有些会拒绝；
//     会合并的那些，彼此的合并方式也不一样。
//
// 两个条件，可以折叠成一条很容易记在脑子里的规则：**只能
// 紧挨着一个 Assistant 回合之前切割对话。** Assistant 消息
// 从不携带工具结果，所以条件 2 能推出条件 1——但两个条件
// 还是分开检查，因为迟早有一天会有人加入一种新的块类型，
// 到了那天，条件 2 就推不出条件 1 了，而如果只写了一个
// 合并后的单一检查，它还会继续糊里糊涂地返回真。
func canCutBefore(msgs []Msg, i int) bool {
	if i <= 0 || i >= len(msgs) {
		return false
	}
	for _, b := range msgs[i].Blocks {
		if b.Kind == BlockToolResult {
			return false
		}
	}
	return msgs[i].Role == RoleAssistant
}

// safeCut 返回的是不早于 want 的最小合法切割索引，找不到
// 就返回 -1。
//
// 它*向前*搜索——朝着丢弃更多的方向——这是有意为之。压缩被
// 触发，是因为窗口已经快满了，所以绝不能出现的失败模式，
// 就是释放的比预期要少，结果立刻又要再压缩一次。向后搜索
// 虽然能保留更多最近的上下文，但有时会一点也释放不出来。
func safeCut(msgs []Msg, want int) int {
	if want < 1 {
		want = 1
	}
	for i := want; i < len(msgs); i++ {
		if canCutBefore(msgs, i) {
			return i
		}
	}
	return -1
}

// validConversation 返回消息列表中第一个
// 结构性问题的描述；如果格式良好，
// 则返回 ""。
//
// 这个检查刻意独立于 canCutBefore，
// 而不是其重述。canCutBefore 说的是允许
// 切割的位置；这说的是结果是否真的可发送，
// 源自协议规则而不是切割逻辑。如果压缩器
// 有误，从相同假设写出的检查会与之一致。
// 从另一端写出的就不会——这就是拥有它的
// 全部价值。
func validConversation(msgs []Msg) string {
	open := map[string]bool{}     // 等待结果的工具调用
	answered := map[string]bool{} // 已看到结果的 id
	for i, m := range msgs {
		if len(m.Blocks) == 0 {
			return fmt.Sprintf("message %d (%s) has no content blocks; the Anthropic protocol rejects an empty content array", i, m.Role)
		}
		if i > 0 && msgs[i-1].Role == m.Role {
			return fmt.Sprintf("messages %d and %d are both %s; roles must alternate", i-1, i, m.Role)
		}
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockToolCall:
				open[b.ID] = true
			case BlockToolResult:
				if !open[b.ID] {
					return fmt.Sprintf("message %d answers tool call %q, which no earlier message made — the call was cut away and its result left behind", i, b.ID)
				}
				delete(open, b.ID)
				answered[b.ID] = true
			}
		}
	}
	// **最终**消息中的未答复工具调用是合法的：
	// 这是工具仍在运行时对话所处的状态。
	// 在其他地方，这是孤立结果 bug 的镜像，
	// 它让模型相信，自己发出的命令悄无声息地
	// 什么都没有产生。
	for i, m := range msgs[:max(0, len(msgs)-1)] {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolCall && !answered[b.ID] {
				return fmt.Sprintf("tool call %q in message %d is never answered", b.ID, i)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 压缩器
// ---------------------------------------------------------------------------

type compactor struct {
	window    int     // 模型的上下文窗口，以 token 计
	threshold float64 // 当估计的 prompt 超过此比例时压缩
	keepRatio float64 // 之后保留的窗口比例
	maxTokens int     // 用于总结调用的 max_tokens
	est       *estimator
	count     int // 此会话已压缩的次数
}

func newCompactor(window int, threshold, keepRatio float64) *compactor {
	return &compactor{
		window:    window,
		threshold: threshold,
		keepRatio: keepRatio,
		maxTokens: 2048,
		est:       newEstimator(),
	}
}

// due 报告估计的下一个 prompt
// 是否越过阈值。
//
// 注意这里需要什么：**估计**，
// 而非最后报告的使用量。使用最后
// 使用量是显而易见的实现，但晚了
// 一个回合——填满窗口的工具结果已
// 在历史中，此时还没有构建会报告
// 它的调用。估算器的全部意义在于
// 在支付之前回答问题。
func (c *compactor) due(estimated int) bool {
	if c.window <= 0 || c.threshold <= 0 {
		return false
	}
	return float64(estimated) >= c.threshold*float64(c.window)
}

// estimate 将对话及其固定开销
// 转换成 token。
func (c *compactor) estimate(msgs []Msg, baseChars int) int {
	return c.est.tokens(convChars(msgs) + baseChars)
}

// plan 选择切割索引，或返回 -1
// 并附带原因。
//
// 预算是要保留的**历史** token 数。
// 消息从最新向后遍历，累积，直到
// 预算用完；该索引是我们想保留的最早
// 位置，然后 safeCut 将其向前移动到
// 合法边界。
func (c *compactor) plan(msgs []Msg, baseChars int) (int, string) {
	if len(msgs) < 4 {
		return -1, "nothing to compact: the conversation is only " + fmt.Sprint(len(msgs)) + " messages"
	}
	budget := int(c.keepRatio * float64(c.window))
	if budget <= 0 {
		return -1, "keep budget is zero"
	}

	// 从最新消息向后遍历。`want` 最终是
	// 仍落在预算内的最老消息的索引。
	kept, want := c.est.tokens(baseChars), len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		t := c.est.tokens(msgChars(msgs[i]))
		if kept+t > budget {
			break
		}
		kept += t
		want = i
	}

	// 下限。如果预算不足以超过最新消息，
	// 就没有什么有用的事可做：一个总结
	// 加一条消息不是压缩。
	//
	// 两种不同的事情会让你到这里，它们
	// 需要两种不同的修复，第一版代码为两者
	// 都打印了第一条消息。这比无消息更糟——
	// 一个指向错误 flag 的错误会把读者送去
	// 修改从来不是问题的设置，当它没能解决
	// 问题时，他们会断定诊断是对的，情况
	// 已经无可救药。错误消息是关于因果关系
	// 的主张；搞错了你就不仅没有帮助，反而
	// 是误导。
	if want >= len(msgs)-1 {
		newest := c.est.tokens(msgChars(msgs[len(msgs)-1]))
		if newest > budget {
			return -1, fmt.Sprintf("cannot compact: the newest message alone is ~%d tokens against a keep budget of %d — lower --max-output or use a command that filters", newest, budget)
		}
		return -1, fmt.Sprintf("cannot compact: a keep budget of %d tokens has room for only the newest message (~%d) — raise --keep or --window", budget, newest)
	}

	cut := safeCut(msgs, want)
	if cut < 0 {
		return -1, "no legal cut point: every message from here on is a tool result or a user turn"
	}
	if cut >= len(msgs)-1 {
		return -1, "the only legal cut point would discard the whole conversation"
	}
	return cut, ""
}

// ---------------------------------------------------------------------------
// 总结
// ---------------------------------------------------------------------------

// 给总结调用的指令。
//
// 第 2 点中的选择标准值得窃取：**保留那些
// 需要靠工具调用才能重新找到的东西。**
// 这是经济测试，不是语义测试，模型应用
// 起来远比"保留重要的东西"容易得多。
// 花费三次 grep 才能找到的文件路径值得
// 一行；模型自己的叙述段落一文不值，
// 因为重新生成它的成本为零。
const summarySystem = `You are compacting a coding-agent session transcript so the agent can continue in a smaller context window. You are not continuing the session and you are not answering the user.

Write a summary that preserves, under these headings:

1. GOAL — what the user asked for, in their words where possible, including anything they explicitly corrected, refused, or ruled out.
2. FACTS — everything discovered about this environment that would cost tool calls to rediscover: exact file paths, directory layouts, command output that mattered, version numbers, error messages verbatim, what was tried and failed.
3. DECISIONS — choices made and the reason for each, so they are not relitigated.
4. STATE — what the transcript shows was done, and what it shows was still outstanding at the point it ends.

Rules:
- You are reading only the EARLIER part of the session. More recent messages are being kept verbatim and will appear immediately after your summary, and you cannot see them. So never write that something was "never done", "not started" or "still outstanding" as a statement about the session — it may have happened in the part you cannot see. Say "as of the end of this transcript".
- Keep identifiers, paths, flags and error text EXACT. Never paraphrase a filename or a command.
- Drop narration, restatements, apologies, and anything the agent said about what it was about to do.
- If something is uncertain, say it is uncertain rather than resolving it.
- Prefer a longer FACTS section and a shorter everything else.
- Output the summary only. No preamble, no tool calls.`

// flatten 将对话中注定消亡的部分
// 呈现为文字记录。
//
// 另一种方式——将实际消息数组传递给
// 总结调用——看起来更忠实，但表现更差：
// 给定一个对话，模型会继续它。它再次
// 回答最后一个问题，或发出下一个工具调用。
// flatten 将任务从"交谈"改为"读此文档"，
// 这正是我们实际想要的，它还有另外两个
// 好处：它让长工具输出在被支付之前被截断，
// 它使总结程序调用不携带任何工具定义，
// 所以工具调用不只是不被鼓励，而是彻底
// 不可能。
func flatten(msgs []Msg, maxBlock int) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case BlockText:
				fmt.Fprintf(&b, "[%s]\n%s\n\n", m.Role, clip(blk.Text, maxBlock))
			case BlockToolCall:
				cmd, err := parseBashArgs(blk.Args)
				if err != nil {
					cmd = blk.Args
				}
				fmt.Fprintf(&b, "[%s ran] %s\n", m.Role, clip(cmd, 400))
			case BlockToolResult:
				fmt.Fprintf(&b, "[output]\n%s\n\n", clip(blk.Text, maxBlock))
			}
		}
	}
	return b.String()
}

// clip 从**中间**缩短字符串，
// 保留两端。
//
// 头部截断是条件反射，对命令输出来说是错的。
// 构建日志在末尾放错误；堆栈跟踪在末尾放原因；
// diff 在任何地方放有趣的块。保留前 60% 和后 40%
// 保留了命令在说什么和它的结论，失去了重复的
// 中间部分，那是长的部分。
func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := max * 6 / 10
	tail := max - head
	return s[:head] + fmt.Sprintf("\n… [%d characters omitted] …\n", len(s)-max) + s[len(s)-tail:]
}

// summaryMsg 将总结包装在将替换
// 历史的消息中。
//
// 这是一条用户消息，被标记了。标记很重要：
// 没有它，模型会把过去式文本墙视为
// 用户刚刚输入的东西，然后回答它。有了它，
// 模型会把它视为简报材料——这就是它的本质。
func summaryMsg(text string) Msg {
	return TextMsg(RoleUser, "<session-summary>\nThe earlier part of this session was compacted to fit the context window. This is the summary of what happened; treat it as established fact, not as a new request.\n\n"+
		strings.TrimSpace(text)+"\n</session-summary>")
}

// run 执行一次压缩：总结 msgs[:cut]，
// 返回新历史。
//
// 这产生的每个数字都上总线。压缩如果
// 不报告自身成本，Agent 最终就会收到
// 一张没人能说清楚的账单：总结调用是
// 一次真实的模型调用，用的是真实模型，
// 按真实费率计费，在每个把压缩当作
// 内部细节处理的实现里，这笔账都是
// 不可见的。
func (c *compactor) run(p Provider, httpc *http.Client, bus *Bus, msgs []Msg, cut int, baseChars int) ([]Msg, error) {
	before := c.estimate(msgs, baseChars)
	bus.Emit(Event{
		Kind: KindCompactStart, MsgsBefore: len(msgs), TokensBefore: before,
		Text: fmt.Sprintf("summarising messages 0–%d, keeping %d", cut-1, len(msgs)-cut),
	})

	transcript := flatten(msgs[:cut], 4000)

	// 没有工具。见 flatten：总结程序
	// 不是 Agent，一定不能表现得像一个。
	req, body, err := p.BuildRequest(summarySystem,
		[]Msg{TextMsg(RoleUser, "Transcript to compact:\n\n"+transcript)},
		nil, c.maxTokens)
	if err != nil {
		return msgs, err
	}
	bus.Emit(Event{Kind: KindRequest, Request: body})

	started := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		return msgs, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return msgs, fmt.Errorf("compaction call failed: http %d", resp.StatusCode)
	}
	res, err := p.ParseStream(resp.Body, bus, 0, started)
	if err != nil {
		return msgs, err
	}
	if strings.TrimSpace(res.Text) == "" {
		// 拒绝继续是对的选择。把历史换成
		// 一片空白的压缩，不会吵吵嚷嚷地
		// 失败——Agent 只是把一切都忘掉，
		// 还继续显得信心十足。
		return msgs, fmt.Errorf("the summarising call returned no text (stop: %s)", res.RawStop)
	}

	out := append([]Msg{summaryMsg(res.Text)}, msgs[cut:]...)
	c.count++

	after := c.estimate(out, baseChars)
	bus.Emit(Event{
		Kind: KindCompactEnd, Text: res.Text,
		MsgsBefore: len(msgs), MsgsAfter: len(out),
		TokensBefore: before, TokensAfter: after,
		Millis: time.Since(started).Milliseconds(),
	})

	// 此账单在**下一次**调用时到期，
	// 不是这一次，并作为看起来像衰退的
	// 数字到达。要在它发生的那一刻就
	// 说清楚，而不是等它冒出来了才说。
	bus.Emit(Event{
		Kind:         KindCacheInvalidated,
		TokensBefore: before,
		Text:         "the prompt prefix was rewritten — every cache entry from before this point is now unreachable, and the next call is a full-price miss",
	})
	return out, nil
}
