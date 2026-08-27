// 阶段 05——上下文压缩。
//
// 上下文窗口是一堵墙，跑得够久的 Agent 总会撞上去。上下文压缩就是撞
// 上之后发生的事：把大部分记录扔掉，换成一份摘要，接着跑。
//
// 这个想法一句话就说完了。它一切昂贵的地方都在细节里，而这个文件就
// 是那些细节：
//
//   - 不是哪儿都能切。在工具调用和它的结果之间下刀，下一个请求就是
//     畸形的——几个回合之后报出 API 错误，而栈回溯指的是请求构造
//     器，不是那一刀。
//   - 不数 token 就不知道什么时候该切，而数 token 需要 tokenizer，
//     这个依赖仓库里没有。所以它拿 API 已经报出来的数字，去校准
//     估计值。
//   - 摘要本身就是一次模型调用。它要花 token，也要花秒数，把它藏起
//     来的实现是在会话成本上撒谎。
//   - 上下文压缩会重写 prompt 前缀，阶段 04 花一整章挣来的缓存条
//     目，会被它全部摧毁。它不是免费的。它是一笔交易，而想知道这笔
//     交易划不划算，只能把两边都量出来。
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 没有 tokenizer，怎么数 token
// ---------------------------------------------------------------------------

// estimator 把字符数换算成 token 数，办法是拿 API 已经报出来的 token
// 计数来校准。
//
// 常见的建议是"把 tokenizer 打包进来"。要决定**什么时候压缩**，那是
// 把工具用错了：tokenizer 是个大依赖，还分模型；它和服务器在工具
// schema、消息信封的框架开销上谈不拢；而且它告诉你的东西，你本来就
// 能白拿——因为每一条响应里，都已经带着你刚发出去那个 prompt 的精确
// token 数。
//
// 所以：发一个 prompt，记下它有多少字符、服务器说它是多少 token，你
// 就有了比值。每次调用都做一遍，这个比值就收敛到这段对话里散文、代
// 码和 JSON 的真实配比上。
//
// 微妙的地方，也是这招之所以能成立的原因：**这个估计不需要准，它需
// 要的是一致。**它只被用来回答"我们是不是快撞墙了"，而它拿来校准的
// 字符数，跟后来要它换算的字符数是同一套。系统性的偏差——JSON 信封
// 开销、工具 schema、系统提示词——被吸收进比值里，而不是当成误差攒
// 起来。真正会毁掉它的，是量的是一样东西、估的是另一样。
type estimator struct {
	ratio float64 // 每个 token 多少字符
	obs   int
}

// 3.6 对英文散文、代码和 JSON 混在一起的情况来说是个合理的冷启动
// 值；纯英文更接近 4.0，密集的 JSON 更接近 2.5。它只在第一次调用时
// 有影响，之后就交给实测了。
func newEstimator() *estimator { return &estimator{ratio: 3.6} }

// observe 记下一次真实测量：发出去多少字符，计费多少 token。
func (e *estimator) observe(chars, tokens int) {
	if chars <= 0 || tokens <= 0 {
		return
	}
	r := float64(chars) / float64(tokens)
	// 比值落在这个区间外面，说明这两个数根本不是在量同一个请求——多半
	// 是：某次调用的字符数我们压根没取，它的 usage 事件却到了。把它丢
	// 掉，好过让一个坏样本把估计值拽到要十次调用才爬得回来的地方。
	if r < 1.0 || r > 20.0 {
		return
	}
	if e.obs == 0 {
		e.ratio = r
	} else {
		// 指数移动平均，权重偏向历史。这个比值会缓慢地、真实地漂移——一场
		// 从散文开始、后来去读 JSON 文件的会话，确实是变了——所以它该跟着
		// 走，但不该因为一个不寻常的回合就猛地一晃。
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

// msgChars 数的是一条消息给 prompt 贡献了多少字符。
//
// 它数文本、工具调用的参数和工具结果——凡是会被重发的都数——结构性的
// JSON 不算。思考块不算，因为这个仓库在发送前就把它们丢了：这边数、那
// 边不发，正是会把标定毒掉的那种不对称。见 runTurn：思考从不进历史。
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
// 哪些地方允许下刀
// ---------------------------------------------------------------------------

// canCutBefore 判断 `[summary] + msgs[i:]` 是不是 API 会接受的对话。
//
// 两个条件，第一个就是人人都会写出去的那个 bug：
//
//  1. **msgs[i] 里不能有工具结果。**跟它配对的工具调用在
//     msgs[i-1] 里，而上下文压缩马上就要删掉那一条。调用没了的工
//     具结果就是孤立工具结果，两个协议都会拒——OpenAI 报的是
//     "messages with role 'tool' must be a response to a preceding
//     message with tool_calls"，Anthropic 报的是没料到的
//     `tool_use_id`。故障浮出水面是在**下一个**请求上，所以回溯指
//     向请求构造器，而真正的错在一百行开外的 compactor 里。
//
//  2. **msgs[i] 必须是 assistant 消息。**摘要是作为 user 消息注入
//     的，所以在另一条 user 消息前面切，会切出连着两条 user 消
//     息。有些端点会把它们合并，有些直接拒，而合并的那些，各合
//     各的。
//
// 两个条件合起来，就是一条脑子里放得下的规矩：**对话只能在紧挨着
// assistant 回合之前的位置切开。**assistant 消息从不带工具结果，所
// 以条件 2 蕴含条件 1——但还是分开检查，因为哪天有人加了一种新的块
// 类型，这个蕴含关系当天就不成立了，而合并成一个检查会继续返回
// true。
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

// safeCut 返回 want 或其之后最小的合法切点下标，没有就返回 -1。
//
// 它是**往前**找的——朝着多丢一点的方向——这是故意的。上下文压缩之所
// 以被触发，就是因为窗口快满了，所以绝不能出现的失败模式是：腾出来
// 的比打算腾的少，然后马上又得压缩一次。往回找会保住更近的上下文，
// 但有时候什么都腾不出来。
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

// validConversation 返回消息列表里第一个结构性问题的描述，没问题就
// 返回 ""。
//
// 这是故意写成独立的检查，而不是把 canCutBefore 重述一遍。
// canCutBefore 说的是哪里允许切；这个说的是切完的结果到底发不发得
// 出去，依据是协议的规则，不是切的那套逻辑。compactor 要是哪天错了，检
// 查只要是从同一套假设出发写的，就会跟着它一起错。从另一头写的不
// 会——这正是留着它的全部价值。
func validConversation(msgs []Msg) string {
	open := map[string]bool{}     // 还在等结果的工具调用
	answered := map[string]bool{} // 已经见到结果的那些 id
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
	// **最后一条**消息里有没被回答的调用是合法的：工具还在跑的时候，对
	// 话本来就是这个状态。出现在别的地方，它就是孤立工具结果那个 bug 的
	// 镜像，会让模型以为自己发出去的命令一声不响地什么都没产出。
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
// compactor
// ---------------------------------------------------------------------------

type compactor struct {
	window    int     // 模型的上下文窗口，按 token 算
	threshold float64 // 估算出的 prompt 超过窗口的这个比例就压缩
	keepRatio float64 // 压缩完要原样留下的窗口比例
	maxTokens int     // 摘要那次调用的 max_tokens
	est       *estimator
	count     int // 这场会话压缩过几次
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

// due 判断估算出来的下一个 prompt 有没有越过阈值。
//
// 注意它收的是什么：**估计值**，不是上一次报回来的 usage。用上一次
// 的 usage 是最顺手的实现，而它晚了整整一个回合——等到本该由它报数的
// 那次调用开始构造时，撑爆窗口的那条工具结果早就在历史里了。估算器
// 存在的全部意义，就是在花钱去问之前先把这个问题答了。
func (c *compactor) due(estimated int) bool {
	if c.window <= 0 || c.threshold <= 0 {
		return false
	}
	return float64(estimated) >= c.threshold*float64(c.window)
}

// estimate 把一段对话加上它的固定开销换算成 token。
func (c *compactor) estimate(msgs []Msg, baseChars int) int {
	return c.est.tokens(convChars(msgs) + baseChars)
}

// plan 挑出切点下标，挑不出来就返回 -1 加一条原因。
//
// 预算是要留下多少 token 的**历史**。消息从最新的往回走，一路累加，
// 直到预算花完；那个下标就是我们最早想保留的位置，然后 safeCut 把它
// 往前推到合法的边界上。
func (c *compactor) plan(msgs []Msg, baseChars int) (int, string) {
	if len(msgs) < 4 {
		return -1, "nothing to compact: the conversation is only " + fmt.Sprint(len(msgs)) + " messages"
	}
	budget := int(c.keepRatio * float64(c.window))
	if budget <= 0 {
		return -1, "keep budget is zero"
	}

	// 从最新的消息往回走。`want` 最后停在预算还装得下的、最老的那条消息
	// 的下标上。
	kept, want := c.est.tokens(baseChars), len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		t := c.est.tokens(msgChars(msgs[i]))
		if kept+t > budget {
			break
		}
		kept += t
		want = i
	}

	// 地板。如果预算连最新那条消息都伸不过去，就没什么有用的事可做了：
	// 一份摘要加一条消息不叫压缩。
	//
	// 有两种不同的原因会把你带到这儿，它们要两种不同的修法，而这段代码
	// 的第一版对两种情况打印的是同一条消息。那比不打印还糟——报错点错
	// 了 flag，读者就跑去改设置，而那个设置从来不是问题所在；改完不管
	// 用，他们就断定诊断是对的、情况没救了。报错信息是在主张因果；主张
	// 错了，你不是没帮上忙，你是在误导。
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
// 摘要
// ---------------------------------------------------------------------------

// 给摘要那次调用的指令。
//
// 第 2 点里的挑选标准值得偷走：**留下那些重新发现要花工具调用的东
// 西。**这是经济上的判据，不是语义上的，而且模型执行起来，比执行
// "留下重要的东西"容易得多。找了三次 grep 才找到的文件路径值一行；
// 模型自己那一段叙述一文不值，因为重新生成它不花钱。
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

// flatten 把注定要被丢掉的那部分对话渲染成一份记录。
//
// 另一条路——把真正的消息数组交给摘要调用——看着更忠实，实际表现更
// 差：给模型一段对话，模型就会接着往下说。它会把最后那个问题再答一
// 遍，或者发起下一次工具调用。摊平之后，任务从"对话"变成了"读这份
// 文档"，那才是我们真正要的；而且它还多带来两个好处：长的工具输出
// 可以在付钱之前就截断；摘要调用可以完全不带工具定义，于是工具调用
// 不只是不受鼓励，而是根本不可能。
func flatten(msgs []Msg, maxBlock int) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case BlockText:
				fmt.Fprintf(&b, "[%s]\n%s\n\n", m.Role, clip(blk.Text, maxBlock))
			case BlockToolCall:
				fmt.Fprintf(&b, "[%s ran] %s\n", m.Role,
					clip(argsForDisplay(blk.Args), 400))
			case BlockToolResult:
				fmt.Fprintf(&b, "[output]\n%s\n\n", clip(blk.Text, maxBlock))
			}
		}
	}
	return b.String()
}

// clip 从**中间**把字符串缩短，两头都留着。
//
// 只留开头是本能反应，而对命令输出来说是错的。构建日志把错误放在末
// 尾；栈回溯把起因放在末尾；diff 把有意思的那块放在任何地方。留前
// 60% 和后 40%，保住的是这条命令宣告了什么、又得出了什么结论，丢掉
// 的是重复的中段——而那正是它长的原因。
func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := max * 6 / 10
	tail := max - head
	return s[:head] + fmt.Sprintf("\n… [%d characters omitted] …\n", len(s)-max) + s[len(s)-tail:]
}

// summaryMsg 把摘要包进那条将要取代历史的消息里。
//
// 它是一条 user 消息，而且带了标签。标签很要紧：没有它，模型会把一
// 大片过去时的文字当成用户刚打进来的东西，然后去回答它。有了它，模
// 型会把它当成简报材料——它本来就是。
func summaryMsg(text string) Msg {
	return TextMsg(RoleUser, "<session-summary>\nThe earlier part of this session was compacted to fit the context window. This is the summary of what happened; treat it as established fact, not as a new request.\n\n"+
		strings.TrimSpace(text)+"\n</session-summary>")
}

// run 执行一次上下文压缩：给 msgs[:cut] 做摘要，返回新的历史。
//
// 它产出的每个数字都会上总线。Agent 最后会拿到一张没人对得上的账
// 单，原因就是上下文压缩不报自己的成本：摘要那次调用是真的模型调
// 用，用的是真的模型，按真的价钱计费，而凡是把上下文压缩当成内部细
// 节的实现，它在里面都是隐形的。
func (c *compactor) run(ctx context.Context, p Provider, pol retryPolicy, httpc *http.Client, bus *Bus, msgs []Msg, cut int, baseChars int, dl deadlines) ([]Msg, error) {
	before := c.estimate(msgs, baseChars)
	bus.Emit(Event{
		Kind: KindCompactStart, MsgsBefore: len(msgs), TokensBefore: before,
		Text: fmt.Sprintf("summarising messages 0–%d, keeping %d", cut-1, len(msgs)-cut),
	})

	transcript := flatten(msgs[:cut], 4000)
	started := time.Now()

	// 做摘要的那次调用，走的是和其他每一次模型调用相同的分类、相同的重试循
	// 环。阶段 09 就是它不再各自留一份更差的拷贝的地方——旧的那份根本不读
	// 响应体，所以压缩失败时只能报个 `http 500`，别的什么都说不出。
	//
	// 梯子只有一级，这是故意的：这次调用可以重试，但不许降级。因为摘要打了
	// 个嗝就把整个会话搬到另一家供应商去，那不是恢复，那是惊吓——而且用户
	// 看到的是"compacting"，价格却在他脚底下换了。
	//
	// 也不给工具。见 flatten：摘要器不是 Agent，也绝不能有像 Agent 那样行动
	// 的能力。
	res, err := retryLoop(ctx, bus, 0, pol.forCompaction(), newLadder(rung{p: p}), nil, nil,
		func(ctx context.Context, pr Provider) (*CallResult, error) {
			return modelCall(ctx, pr, httpc, bus, 0, summarySystem,
				[]Msg{TextMsg(RoleUser, "Transcript to compact:\n\n"+transcript)},
				nil, c.maxTokens, dl, nil)
		})
	if err != nil {
		return msgs, err
	}
	if strings.TrimSpace(res.Text) == "" {
		// 拒绝往下走才是对的。上下文压缩要是把历史换成一片空白，它不会大
		// 声失败——Agent 只是把什么都忘了，然后接着一副胸有成竹的样子
		// 说下去。
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

	// 这笔账单要到**下一次**调用才结，不是这一次，而且它到账时看上去像
	// 一次退化。在它被造成的那一刻就说出来，别等它冒头才说。
	bus.Emit(Event{
		Kind:         KindCacheInvalidated,
		TokensBefore: before,
		Text:         "the prompt prefix was rewritten — every cache entry from before this point is now unreachable, and the next call is a full-price miss",
	})
	return out, nil
}
