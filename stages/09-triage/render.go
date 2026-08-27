// 阶段 02——仪表盘。
//
// 这就是个 Subscriber，别的什么都不是。它够不着 Agent，不懂 HTTP，也没
// 有自己的时钟：它打印的每一个数字，都是坐着 Event 来的。这条约束不是
// 为了整洁，它就是功能本身——`replay` 能在不联网的情况下把一段会话复现
// 到毫秒级，靠的正是它；你实跑时看到的和事后读回来的保证是同一样东西，
// 靠的也是它。
//
// 要是你发现自己想在这个文件里用 `time.Now()`，那你想要的那个数字，
// 该待在事件里。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// prices 按每百万 token 计价。零的意思是"不知道"，不知道就印成破折号，
// 不印 $0.00——编出来的零比没有数字更糟，因为人们拿去引用的就是这个数。
type prices struct {
	in, out, cacheRead, cacheWrite float64
}

func (p prices) known() bool {
	return p.in > 0 || p.out > 0 || p.cacheRead > 0 || p.cacheWrite > 0
}

// cost 给一次调用算钱。三种输入费率分开，是因为它们差着一个数量级：
// 缓存读约为基准价的 0.1x，缓存写约 1.25x，所以按 token 数看着贵的会话
// 可能很便宜，反过来也一样。把它们塌成一个"input"数字，是 Agent 成本
// 统计出错最常见的方式。
func (p prices) cost(u Usage) float64 {
	m := func(tok int, rate float64) float64 { return float64(tok) * rate / 1e6 }
	return m(u.Input, p.in) + m(u.CacheWrite, p.cacheWrite) + m(u.CacheRead, p.cacheRead) + m(u.Output, p.out)
}

type renderer struct {
	out    io.Writer
	color  bool
	prices prices
	window int // 模型上下文窗口，给水位线用；0 = 不知道

	// 会话总计。这些数字，没人答得上自己那个 Agent 到底是多少——这个文件
	// 存在，就是因为这个。
	session     Usage
	sessionCost float64
	calls       int
	commands    int

	// showRequest 打开请求检查器：每次调用前把完整的 JSON body 打出来。
	// 默认关着，因为它太大了；模型第一次干出没法解释的事时值得打开——
	// 十次里有九次，答案是 prompt 里根本没有你以为在里面的东西。
	showRequest bool

	// 每次调用的流式状态。
	ttft      int64
	openBlock string // "text" | "reasoning" | ""——正在流的是哪一种
	sawOutput bool

	// 压缩状态。inCompaction 把摘要器流出来的文本改道，送进一条带标记的边
	// 槽里，这样模型*关于*这次会话写的段落，就绝不会被当成它*在*这次会话
	// 里写的段落。
	inCompaction bool
	compactions  int
	saved        int // 压缩从 prompt 里拿掉的 token，累计

	// 线上看到的字节数和 token 数，用来算实时的字符每 token 比值。"没有
	// tokenizer 怎么数 token"，答案全在这里：每个响应都会告诉你，你刚发出
	// 去的那个请求正好是多少 token，而那个请求有多少字节，你自己知道。
	wireBytes  int
	wireTokens int

	// 子 Agent 状态（阶段 07）。
	//
	// 并发逼出来的设计决定，与其让人自己撞见，不如挑明：**只要有子
	// Agent 在跑，这个渲染器就不再显示流式文本。**
	//
	// 两三个子 Agent 同时往外吐 token。线性终端只有一个光标，交错起来就
	// 是一段由三句不同的话拼出来的文字——不只是难看，是实打实的误导，因
	// 为它读起来像是同一个 Agent 在自相矛盾。给每个片段前面加上 agent id
	// 是正确的，也是每个 delta 才四个字符、根本没法读的。
	//
	// 所以朴素渲染器诚实地降级：碰上子 Agent，它显示结构——跑了什么、花
	// 了多少、返回了什么——把散文丢掉。什么都没丢，因为每个 delta 都在
	// trace 里，而阶段 06 的 composer 存在的理由，恰恰就是线性终端对树来
	// 说是错的形状。渲染器显示不了某样东西，就该少显示来把话说明白，绝
	// 不能显示错的。
	subDepth int

	// 重试记账（阶段 09）。
	//
	// 两个计数器，不是一个，而这个拆分是第一次实跑逼出来的修正。
	//
	// retries 是整场会话的总数，永不清零：这是人在最后想看的数，因为"这花了
	// 四分钟"和"这花了四分钟、重试了十一次"是两场不同的会话。
	//
	// billedFailures 每来一帧 usage 就清零，只数那些真的到了模型、因而被收了
	// 钱的尝试——也就是流打开了又死掉的那种。503 不花钱，而为它收费的面板是
	// 在无中生有地造钱。
	//
	// rebilled 和 session 分开放，属于同一类理由：一个是供应商告诉我们的数，
	// 另一个是我们自己推出来的数。把它们并起来，报告里就分不清哪个是量到的、
	// 哪个是推出来的——而估算就是这样变成事实的。
	retries        int
	billedFailures int
	rebilled       Usage
	rebilledCost   float64

	// lastUsage 把最近一条 KindUsage 带来的东西锁存下来。
	//
	// 它存在，是因为真出过一次集成 bug。usage 和响应结束是两条不同的事件，
	// 由同一个组件发出，但不在同一时刻；而这个渲染器的第一版只从
	// KindResponseEnd 上读 usage——结果面板里全是零。渲染器不该关心某个数
	// 是坐哪条事件来的；它该记住最后一次被告知的值，然后用那个值。
	lastUsage Usage
}

func newRenderer(out io.Writer, color bool, p prices, window int) *renderer {
	return &renderer{out: out, color: color, prices: p, window: window}
}

// ---------------------------------------------------------------------------
// 颜色。故意做得很小：四个语义角色，没有主题系统。
// ---------------------------------------------------------------------------

const (
	cReset = "\x1b[0m"
	cDim   = "\x1b[2m"
	cCmd   = "\x1b[36m" // 青色：Agent 干过的事
	cWarn  = "\x1b[33m"
	cErr   = "\x1b[31m"
	cFull  = "\x1b[31m" // 红色：按全价计费的 token
	cWrite = "\x1b[33m" // 黄色：缓存写，~1.25x
	cRead  = "\x1b[32m" // 绿色：缓存读，~0.1x
)

func (r *renderer) c(code, s string) string {
	if !r.color {
		return s
	}
	return code + s + cReset
}

func (r *renderer) p(format string, args ...any) {
	fmt.Fprintf(r.out, format, args...)
}

// closeBlock 把开着的那个流式区域收尾，不管开的是哪一种。流式意味着
// 文本到达时，换行不会落在可预料的位置，所以版式归渲染器管，不归模型管。
func (r *renderer) closeBlock() {
	if r.openBlock != "" {
		r.p("\n")
		r.openBlock = ""
	}
}

// ---------------------------------------------------------------------------
// Subscriber 的实现。一个 switch；每个 case 都是一次关于屏幕的决定。
// ---------------------------------------------------------------------------

func (r *renderer) OnEvent(e Event) {
	// 深度是下面大部分分支的闸门，而它取自**事件**，不是渲染器自己的状
	// 态。有了并发的子 Agent，"我们此刻在哪个 agent 里面"就不是时间的属
	// 性了：把它当成时间属性来跟踪的渲染器，会把子 Agent 的命令算到碰巧
	// 最后说话的那个子 Agent 头上。
	deep := e.Depth > 0

	switch e.Kind {

	case KindSubagentStart:
		r.closeBlock()
		r.subDepth++
		r.p("\n  %s\n", r.c(cCmd, "╭─ subagent · "+e.ToolName))
		r.p("  %s\n", r.c(cDim, "│ "+oneLineDim(e.Text, 88)))

	case KindSubagentEnd:
		if r.subDepth > 0 {
			r.subDepth--
		}
		u := Usage{}
		if e.Usage != nil {
			u = *e.Usage
		}
		// 构成子 Agent 全部论据的那两个数字，并排打出来：**花掉**多少，
		// **返回**多少。两者之间的落差，就是父 Agent 不必背的上下文。
		r.p("  %s\n", r.c(cCmd, fmt.Sprintf("╰─ %d turns · %d prompt + %d output tokens · %dms → %s returned",
			e.Turn, u.Prompt(), u.Output, e.Millis, humanBytes(e.Bytes))))

	case KindSkillsIndexed:
		r.p("  %s\n", r.c(cDim, fmt.Sprintf("≡ skills: %s · index %s in every request · %s of bodies left on disk",
			e.Text, humanBytes(e.Bytes), humanBytes(e.TokensBefore))))

	case KindUserMessage:
		r.p("\n%s %s\n", r.c(cDim, "you >"), e.Text)

	case KindTurnStart:
		r.ttft, r.sawOutput = 0, false

	case KindRequest:
		r.wireBytes += len(e.Request)
		if r.showRequest {
			var pretty bytes.Buffer
			if json.Indent(&pretty, e.Request, "  │ ", "  ") == nil {
				r.p("\n  %s\n  │ %s\n", r.c(cDim, "┌─ request ─────────"), pretty.String())
			}
			r.p("  %s\n", r.c(cDim, fmt.Sprintf("└─ %s", humanBytes(len(e.Request)))))
		}

	case KindFirstToken:
		r.ttft = e.Millis

	case KindReasoningDelta:
		if deep {
			return // 见 subDepth：线性终端没法把这些交错起来
		}
		// 思考过程要显示出来，压暗，并且做上标记。多数产品的默认是把它藏
		// 起来，在这里那是错的默认：学生看不见模型的推理，就分不清是计划
		// 不好还是工具不好。
		if r.openBlock != "reasoning" {
			r.closeBlock()
			r.p("%s ", r.c(cDim, "\n  ·"))
			r.openBlock = "reasoning"
		}
		r.p("%s", r.c(cDim, e.Text))
		r.sawOutput = true

	case KindTextDelta:
		if deep {
			return
		}
		if r.inCompaction {
			// 摘要是显示出来的，不是藏起来的。读不到的压缩，就是
			// 调不了的压缩；而"Agent 忘了点什么"，几乎总是"摘要
			// 把它丢了"——而这件事，只有亲眼看着摘要滚过去，你
			// 才会知道。
			if r.openBlock != "compact" {
				r.closeBlock()
				r.p("  %s ", r.c(cDim, "\n  ≡"))
				r.openBlock = "compact"
			}
			r.p("%s", r.c(cDim, e.Text))
			return
		}
		if r.openBlock != "text" {
			r.closeBlock()
			r.p("\n")
			r.openBlock = "text"
		}
		r.p("%s", e.Text)
		r.sawOutput = true

	case KindToolCallStart:
		r.closeBlock()

	case KindToolCallReady:
		if deep {
			// 每深一层缩进一格，而且子 Agent 的也照显：被委派的 agent
			// 到底**跑了什么**，是用户最需要看到的东西，也是多数实现
			// 藏在转圈图标后面的东西。
			r.p("  %s%s %s\n", strings.Repeat("│ ", e.Depth),
				r.c(cDim, "$"), r.c(cDim, oneLineDim(e.Command, 84)))
			return
		}
		r.p("\n%s %s\n", r.c(cCmd, "  $"), e.Command)

	case KindGateVerdict:
		if e.Verdict != "allow" {
			r.p("  %s\n", r.c(cWarn, "["+e.Verdict+"] "+e.Text))
		}

	case KindCommandEnd:
		// 只计数，不打印。紧接着的工具结果末尾已经带着退出码和耗时了，因为
		// 那段文本本来就是写给模型看的——给你看一份和模型拿到的不一样的摘要，
		// 正是这个阶段要消灭的那类分歧。
		r.commands++

	case KindToolResult:
		if deep {
			return
		}
		if strings.TrimSpace(e.Text) != "" {
			r.p("%s\n", r.c(cDim, indentLines(e.Text)))
		}

	case KindUsage:
		if e.Usage != nil {
			r.calls++
			r.wireTokens += e.Usage.Prompt()
			r.lastUsage = *e.Usage
			r.session = addUsage(r.session, *e.Usage)
			r.sessionCost += r.prices.cost(*e.Usage)

			// 阶段 09：推算失败的那些尝试花了多少钱。
			//
			// 重试过的调用，每次尝试都各计一次费，而失败的尝试自己说不出
			// 这件事——usage 是在流的*末尾*到的，那些流没有末尾。所以估算
			// 放在这里做，放在这个 prompt 唯一有真实数字的那一刻：最后成
			// 功的那次尝试。
			//
			// 每次失败的尝试，都按成功那次同样的 full/write/read 拆分来计
			// 价，这让它成了**下界**，而不是折中的猜测。冷调用第一次尝试付
			// 的是缓存*写*，重试付的是更便宜的*读*，所以照抄成功那次的拆
			// 分，会把第一次算少了。摘要行上打"≥"就是因为这个。
			//
			// 反正对这个数字来说，下界才是诚实的形状：它在为账单争论时站得
			// 住，而其他 Agent 提供的替代方案是沉默。
			if r.billedFailures > 0 {
				u := *e.Usage
				est := Usage{
					Input:      u.Input * r.billedFailures,
					CacheWrite: u.CacheWrite * r.billedFailures,
					CacheRead:  u.CacheRead * r.billedFailures,
				}
				r.rebilled = addUsage(r.rebilled, est)
				r.rebilledCost += r.prices.cost(est)
			}
			r.billedFailures = 0
		}

	case KindResponseEnd:
		if deep {
			// 子 Agent 的调用照样被上面的 KindUsage 计数、计入会话总
			// 额；被压掉的只是每次调用的面板，因为三块面板交错在一起
			// 就不成其为面板。subagent_end 那行报的是汇总。
			return
		}
		r.closeBlock()
		r.renderPanel(e)

	case KindMemoryLoaded:
		r.p("  %s\n", r.c(cDim, fmt.Sprintf("≡ memory: %s (%s)", e.Path, humanBytes(e.Bytes))))

	case KindCompactStart:
		r.closeBlock()
		r.inCompaction = true
		r.p("\n  %s\n", r.c(cWarn, fmt.Sprintf("≡ compacting: %d messages, ~%d tokens — %s",
			e.MsgsBefore, e.TokensBefore, e.Text)))

	case KindCompactEnd:
		r.closeBlock()
		r.inCompaction = false
		r.compactions++
		r.saved += e.TokensBefore - e.TokensAfter
		pct := 0.0
		if e.TokensBefore > 0 {
			pct = float64(e.TokensBefore-e.TokensAfter) * 100 / float64(e.TokensBefore)
		}
		r.p("  %s\n", r.c(cWarn, fmt.Sprintf("≡ compacted: %d → %d messages · ~%d → ~%d tokens (-%.0f%%) · %dms",
			e.MsgsBefore, e.MsgsAfter, e.TokensBefore, e.TokensAfter, pct, e.Millis)))

	case KindCacheInvalidated:
		// 在它被造成的那一刻打印，不是在它显形的时候。代价落在
		// **下一次**调用上，表现为一条突然变红的条；没有这一行，
		// 那看起来就像退化，不像后果。
		r.p("  %s\n", r.c(cErr, "! "+e.Text))

	// ---- 阶段 09 ---------------------------------------------------------

	case KindProvider:
		// 渲染器重新算价的依据是这个事件，不是启动时的配置——价格之所以要
		// 跟着它走，全部理由就在这里。
		//
		// 两处回报。降级过的会话，不会再拿第一家供应商的价格去算第二家的
		// token——那是一份自信满满的错误成本报告，比一份承认自己不知道的
		// 更糟。另外，在没有 providers.json 的机器上重放 trace，现在也能看
		// 到真钱，因为价钱就在文件里。
		if e.Provider != nil {
			r.prices = prices{in: e.Provider.Prices.In, out: e.Provider.Prices.Out,
				cacheRead: e.Provider.Prices.CacheRead, cacheWrite: e.Provider.Prices.CacheWrite}
			if e.Provider.Window > 0 {
				r.window = e.Provider.Window
			}
			if e.Triage == "" {
				break // 会话开始：横幅里已经写过供应商了
			}
			r.closeBlock()
			r.p("  %s\n", r.c(cWarn, fmt.Sprintf("provider → %s (%s · %s) — %s",
				e.Provider.Name, e.Provider.Protocol, e.Provider.Model, e.Text)))
		}

	case KindCallError:
		// 后面跟着裁决的时候，按警告打印，不按错误打印。多数 call_error 都
		// 被扛过去了，而把扛得过去的失败涂成红色，人就是这样学会忽略红色
		// 的。
		r.closeBlock()
		colour, tag := cWarn, e.Triage
		if e.Triage == string(TriageFatal) {
			colour = cErr
		}
		r.p("  %s\n", r.c(colour, fmt.Sprintf("call failed (attempt %d, %s): %s", e.Attempt, tag, e.Text)))

		// 只有先拿到 200、然后才断掉的失败，才花了钱。
		//
		// 这一行之所以在这里，是因为这个阶段第一次实跑就搞错了。故障注入器
		// 返了两次 503，面板数出两次重试，然后报了一句
		// "re-sent ≥1926 prompt tokens (≥$0.000301)"——比这场会话实际的
		// $0.000276 还多。这是胡说：那些请求在生成之前就被拒了，供应商一分
		// 钱都没为它们计。
		//
		// 被拒的状态和被拒的连接都是免费的。打开了又死掉的流不是：那些 token
		// 已经在上游生成出来、也已经被计了费，而本该说出这件事的 usage 帧永远
		// 没到。正是这个不对称，才是 Phase 要挂在事件上的全部理由。
		if e.Phase == string(phaseStream) {
			r.billedFailures++
		}
		if e.Usage != nil {
			// 断掉的流，但走得够远，把 usage 报出来了。少见，但真发生时
			// 值得单独占一行：那些 token 是要计费的，而面板里再没有别的
			// 地方会提到它们。
			r.p("  %s\n", r.c(cDim, fmt.Sprintf("  └ billed on the failed attempt: %d prompt + %d output",
				e.Usage.Prompt(), e.Usage.Output)))
		}

	case KindRetry:
		// 重试计数器放在这里，因为一个回合花了多少钱要由渲染器来说；而重
		// 试过的回合，花的比面板本来会显示的更多。见 rebilled。
		r.retries++
		r.p("  %s\n", r.c(cDim, fmt.Sprintf("retrying in %dms (attempt %d) — %s", e.Millis, e.Attempt, e.Text)))

	case KindNotice:
		r.p("  %s\n", r.c(cWarn, e.Text))

	case KindError:
		r.closeBlock()
		r.p("  %s\n", r.c(cErr, "error: "+e.Text))
	}
}

func (r *renderer) commandFooter(e Event) string {
	var parts []string
	switch {
	case e.TimedOut:
		parts = append(parts, r.c(cErr, "TIMED OUT"))
	default:
		code := fmt.Sprintf("exit %d", e.ExitCode)
		if e.ExitCode != 0 {
			code = r.c(cWarn, code)
		}
		parts = append(parts, code)
	}
	parts = append(parts, fmt.Sprintf("%dms", e.Millis), humanBytes(e.Bytes))
	if e.Truncated {
		parts = append(parts, r.c(cWarn, "truncated"))
	}
	return r.c(cDim, "  └ "+strings.Join(parts, " · "))
}

// renderPanel 是每次调用的仪表读数，也是别人该读这个仓库的理由。它回答
// 三个普通 Agent 回答不了的问题：
//
//	prompt token 都去哪了？  → 拆成 full / write / read
//	它到底有多快？           → TTFT 和吞吐分开报
//	这一次花了多少钱？       → 这次调用，以及到此为止的会话
func (r *renderer) renderPanel(e Event) {
	u := e.Usage
	if u == nil {
		u = &r.lastUsage // 见 lastUsage：usage 是坐自己那条事件来的
	}
	prompt := u.Prompt()

	label := "  ┌─ call " + fmt.Sprint(r.calls) + " · " + e.FinishReason
	if r.inCompaction {
		// 摘要器是一次真调用，打在真模型上，按真价钱付。凡是把
		// 压缩当成内部细节的实现，都会把这次调用漏在自己的账目
		// 外面，然后解释不了自己的账单。这里给它贴标签、计数，
		// 和其他一切一起记进账本。
		label += " · COMPACTION"
	}
	r.p("\n%s\n", r.c(cDim, label))

	// 第 1 行——prompt token 去哪了。那根条形图才是重点所在：三种颜色，
	// 比例就是你实际被计费的比例。
	bar := r.cacheBar(*u)
	r.p("  %s in %s %s  %s\n",
		r.c(cDim, "│"),
		pad(fmt.Sprint(prompt), 6),
		bar,
		r.c(cDim, fmt.Sprintf("full %d · write %d · read %d%s", u.Input, u.CacheWrite, u.CacheRead, hitRate(*u))))

	// 第 2 行——输出和速度。TTFT 和 tokens/sec 分成两个数，因为它们是分开
	// 坏的：第一个 token 慢，是排队或者 prompt 太长；吞吐慢，那是模型本身。
	speed := ""
	if gen := e.Millis - r.ttft; gen > 0 && u.Output > 0 {
		speed = fmt.Sprintf(" · %.1f tok/s", float64(u.Output)*1000/float64(gen))
	}
	think := ""
	if u.Reasoning > 0 {
		think = fmt.Sprintf(" (think %d)", u.Reasoning)
	}
	r.p("  %s out %s %s\n",
		r.c(cDim, "│"),
		pad(fmt.Sprint(u.Output)+think, 6),
		r.c(cDim, fmt.Sprintf("TTFT %dms · total %dms%s", r.ttft, e.Millis, speed)))

	// 第 3 行——钱，或者一条老实的破折号。
	if r.prices.known() {
		r.p("  %s $%s  %s\n",
			r.c(cDim, "│"),
			pad(fmt.Sprintf("%.6f", r.prices.cost(*u)), 10),
			r.c(cDim, fmt.Sprintf("session $%.6f over %d calls", r.sessionCost, r.calls)))
	} else {
		r.p("  %s %s\n", r.c(cDim, "│"), r.c(cDim, "cost — (no prices configured for this provider; see providers.json)"))
	}

	// 第 4 行——上下文有多满。阶段 05 的压缩什么时候必须触发，就由这个数
	// 说了算。
	ctx := fmt.Sprintf("context %d tokens", prompt)
	if r.window > 0 {
		ctx = fmt.Sprintf("context %d / %d (%.1f%%)", prompt, r.window, float64(prompt)*100/float64(r.window))
	}
	// 这次会话实测的字节每 token。就是这个数让"什么时候该压缩"这件事不再
	// 需要 tokenizer。之所以把它打出来，是因为看着它慢慢稳定下来——读
	// JSON 时 3.1，散文里 4.2——是最快搞明白"为什么固定除数是个糟糕的估
	// 算器、校准过的却挺好"的办法。
	if r.wireTokens > 0 {
		ctx += fmt.Sprintf(" · ≈%.1f B/tok", float64(r.wireBytes)/float64(r.wireTokens))
	}
	r.p("  %s %s\n", r.c(cDim, "└"), r.c(cDim, ctx))
}

// cacheBar 把 prompt 的切分画成二十格。
//
// 三个数字排成表也读得懂；条形图则是**扫一眼就看见**，而你想察觉的东西
// 是回合之间比例的变化。绿色突然没了，说明有什么东西把你的缓存作废了
// ——你要在它发生的那个回合就看见，不是月底在账单上看见。
func (r *renderer) cacheBar(u Usage) string {
	const width = 20
	total := u.Prompt()
	if total == 0 {
		return r.c(cDim, strings.Repeat("·", width))
	}
	cells := func(n int) int {
		if n == 0 {
			return 0
		}
		c := n * width / total
		if c == 0 {
			c = 1 // 非零的分量，绝不许画成什么都没有
		}
		return c
	}
	full, write, read := cells(u.Input), cells(u.CacheWrite), cells(u.CacheRead)
	for full+write+read > width && full > 0 {
		full--
	}
	pad := width - full - write - read

	// 三种不同的**字形**，不只是三种颜色。这根条形图要扛得住 `| grep`、
	// 扛得住存成文件、扛得住 CI 日志、扛得住色盲的读者——人们看 Agent 输
	// 出，实际上就是这么看的。只在彩色终端里管用的图，恰好会在有人想给
	// 你看问题的时候是一片空白。
	return r.c(cFull, strings.Repeat("█", full)) +
		r.c(cWrite, strings.Repeat("▓", write)) +
		r.c(cRead, strings.Repeat("░", read)) +
		strings.Repeat(" ", max(0, pad))
}

// SessionSummary 打印总计。要紧的是最后一行：计费的 token 数，对上产出
// 这些 token 的那段对话有多大。阶段 00 的文档记下这个比值在不开缓存时
// 是 4.2x；在这里你可以看着它动。
func (r *renderer) SessionSummary(finalPrompt int) {
	r.p("\n%s\n", r.c(cDim, "  ── session ──────────────────────"))
	r.p("  %d calls · %d commands\n", r.calls, r.commands)
	r.p("  prompt tokens billed: %d  (full %d · write %d · read %d)\n",
		r.session.Prompt(), r.session.Input, r.session.CacheWrite, r.session.CacheRead)
	r.p("  output tokens: %d\n", r.session.Output)
	if r.prices.known() {
		r.p("  cost: $%.6f\n", r.sessionCost)
	}
	if r.compactions > 0 {
		r.p("  compactions: %d · ~%d tokens removed from the prompt\n", r.compactions, r.saved)
	}
	// 只有真发生过才打印。每场干净的会话都印一行"retries: 0"，只会教会人不
	// 再读这个块。
	if r.retries > 0 {
		word := "retries"
		if r.retries == 1 {
			word = "retry"
		}
		r.p("  %d %s\n", r.retries, word)
	}
	if r.rebilled.Prompt() > 0 {
		line := fmt.Sprintf("retried attempts re-sent ≥%d prompt tokens", r.rebilled.Prompt())
		if r.prices.known() {
			line += fmt.Sprintf(" (≥$%.6f)", r.rebilledCost)
		}
		r.p("  %s\n", r.c(cWarn, line))
	}
	if finalPrompt > 0 {
		r.p("  %s\n", r.c(cDim, fmt.Sprintf("re-send ratio: %.1fx (billed %d for a final context of %d)",
			float64(r.session.Prompt())/float64(finalPrompt), r.session.Prompt(), finalPrompt)))
	}
}

// ---------------------------------------------------------------------------

// hitRate 报的是 prompt 里有多大比例是从缓存拿的。
//
// 分母是 Prompt()，绝不是 Input。热调用时 Input 只剩没缓存的那点余
// 数——一次实测里，prompt 有 17,985 个 token，Input 只有 18 个——拿它
// 去除，不管缓存有没有在起作用，报出来的命中率都是 99.9%。要拿你
// 真正被计费的总数去算比率，不然你搭出来的仪表盘根本显示不出退化。
func hitRate(u Usage) string {
	total := u.Prompt()
	if total == 0 || u.CacheRead == 0 {
		return ""
	}
	return fmt.Sprintf("  %.0f%% cached", float64(u.CacheRead)*100/float64(total))
}

func addUsage(a, b Usage) Usage {
	return Usage{
		Input:      a.Input + b.Input,
		CacheWrite: a.CacheWrite + b.CacheWrite,
		CacheRead:  a.CacheRead + b.CacheRead,
		Output:     a.Output + b.Output,
		Reasoning:  a.Reasoning + b.Reasoning,
	}
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// oneLineDim 把值压平到一行沟槽上，并且把删掉的换行标出来，而不是不
// 声不响地接在一起——"这里原来是多行"往往正是你需要知道的事。
func oneLineDim(s string, w int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	if len(s) > w {
		s = s[:w-1] + "…"
	}
	return s
}

func indentLines(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  │ " + strings.ReplaceAll(s, "\n", "\n  │ ")
}

// colorEnabled 判断要不要输出 ANSI。尊重 NO_COLOR（这是跨工具真在用的
// 约定），并且绝不给管道上色——trace 管进 `less` 或者管进文件，都该是
// 纯文本。
func colorEnabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
