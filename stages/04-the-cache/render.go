// 阶段 02——仪器面板。
//
// 这是一个 Subscriber，仅此而已。它访问不到 Agent，不懂 HTTP，
// 也没有属于自己的时钟：它打印的每个数字，都在一个事件中
// 到达。那个约束不是整洁，它是功能——它正是为什么 `replay`
// 能够不联网，就把一次会话精确重现到毫秒级，以及为什么你
// 当场看到的东西，和你事后读回的东西，能保证是同一样东西。
//
// 如果你发现自己在这个文件里想用 `time.Now()`，那说明你想要
// 的那个数字，其实应该放在某个事件里。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// 价格是每百万 token。零意味着"未知"，未知打印为破折号而不是 $0.00——
// 一个编造的零比无数字更差，因为它是人们引用的数字。
type prices struct {
	in, out, cacheRead, cacheWrite float64
}

func (p prices) known() bool {
	return p.in > 0 || p.out > 0 || p.cacheRead > 0 || p.cacheWrite > 0
}

// cost 为一次调用计价。三个输入速率是分开的，因为它们相差
// 一个数量级：缓存读大约是基础速率的 0.1x，缓存写大约 1.25x，
// 所以一个按 token 数量看起来很贵的会话，实际上可能很便宜，
// 反之亦然。把它们压缩成一个"输入"数字，是 Agent 成本报告
// 出错最常见的一种方式。
func (p prices) cost(u Usage) float64 {
	m := func(tok int, rate float64) float64 { return float64(tok) * rate / 1e6 }
	return m(u.Input, p.in) + m(u.CacheWrite, p.cacheWrite) + m(u.CacheRead, p.cacheRead) + m(u.Output, p.out)
}

type renderer struct {
	out    io.Writer
	color  bool
	prices prices
	window int // 模型上下文窗口，用于水位线；0 = 未知

	// 会话总计。这些是问起自己的 Agent 时，没有人答得上来的
	// 那些数字——这正是这个文件存在的原因。
	session     Usage
	sessionCost float64
	calls       int
	commands    int

	// showRequest 打开请求检查器：完整 JSON 体，在每个调用前
	// 打印。它默认关闭，因为体积巨大；但模型第一次做出无法解释
	// 的事情时，就值得打开看看——十之八九，答案是 prompt 里
	// 根本没有你以为它有的东西。
	showRequest bool

	// 每次调用的流式状态。
	ttft      int64
	openBlock string // "text" | "reasoning" | "" — 我们正处在哪种流式的中途
	sawOutput bool

	// lastUsage 锁定的，是最近一次 KindUsage 携带的内容。
	//
	// 它的存在，是因为一个真实发生过的集成 bug。Usage 和响应的
	// 结束是两个不同的事件，由相同组件发出，但不在同一时刻；
	// 第一版的这个渲染器，只从 KindResponseEnd 上读取 usage——
	// 结果就是一整块全是零的面板。渲染器不应该关心某个数字是
	// 搭着哪个事件来的；它应该记住自己被告知的最后一个值，并
	// 使用那个值。
	lastUsage Usage
}

func newRenderer(out io.Writer, color bool, p prices, window int) *renderer {
	return &renderer{out: out, color: color, prices: p, window: window}
}

// ---------------------------------------------------------------------------
// 颜色。有意很小：四个语义角色，无主题系统。
// ---------------------------------------------------------------------------

const (
	cReset = "\x1b[0m"
	cDim   = "\x1b[2m"
	cCmd   = "\x1b[36m" // 青色：Agent 做的东西
	cWarn  = "\x1b[33m"
	cErr   = "\x1b[31m"
	cFull  = "\x1b[31m" // 红色：全价计费的 token
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

// closeBlock 结束任何打开的流式区域。流式意味着文本会在
// 可预测的位置到达，却没有换行符，所以由渲染器——而不是
// 模型——来决定版面。
func (r *renderer) closeBlock() {
	if r.openBlock != "" {
		r.p("\n")
		r.openBlock = ""
	}
}

// ---------------------------------------------------------------------------
// Subscriber 实现。一个 switch；每个分支都是一个屏幕决定。
// ---------------------------------------------------------------------------

func (r *renderer) OnEvent(e Event) {
	switch e.Kind {

	case KindUserMessage:
		r.p("\n%s %s\n", r.c(cDim, "you >"), e.Text)

	case KindTurnStart:
		r.ttft, r.sawOutput = 0, false

	case KindRequest:
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
		// 思考会被显示出来——调暗、并加上标记。隐藏它是大多数产品
		// 的默认做法，但在这里，这是错误的默认选择：看不到模型推理
		// 过程的学生，分不清究竟是计划出了问题，还是工具出了问题。
		if r.openBlock != "reasoning" {
			r.closeBlock()
			r.p("%s ", r.c(cDim, "\n  ·"))
			r.openBlock = "reasoning"
		}
		r.p("%s", r.c(cDim, e.Text))
		r.sawOutput = true

	case KindTextDelta:
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
		r.p("\n%s %s\n", r.c(cCmd, "  $"), e.Command)

	case KindGateVerdict:
		if e.Verdict != "allow" {
			r.p("  %s\n", r.c(cWarn, "["+e.Verdict+"] "+e.Text))
		}

	case KindCommandEnd:
		// 计数，不打印。下面的工具结果已经以退出码和持续时间结束，
		// 因为那段文本是为模型写的——如果你看到的总结和模型拿到的
		// 不一样，那正是这个阶段存在的目的所要消灭的那种偏差。
		r.commands++

	case KindToolResult:
		if strings.TrimSpace(e.Text) != "" {
			r.p("%s\n", r.c(cDim, indentLines(e.Text)))
		}

	case KindUsage:
		if e.Usage != nil {
			r.calls++
			r.lastUsage = *e.Usage
			r.session = addUsage(r.session, *e.Usage)
			r.sessionCost += r.prices.cost(*e.Usage)
		}

	case KindResponseEnd:
		r.closeBlock()
		r.renderPanel(e)

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

// renderPanel 是每次调用的仪器读数，也是任何人都该读这个
// 仓库的理由。它回答了三个普通 Agent 回答不了的问题：
//
//	prompt token 去哪了？ → 完整 / 写 / 读拆分
//	速度有多快，真的？ → TTFT 与吞吐分离
//	那花了多少钱？  → 这个调用，以及会话到目前为止
func (r *renderer) renderPanel(e Event) {
	u := e.Usage
	if u == nil {
		u = &r.lastUsage // 参见 lastUsage：usage 搭的是自己事件的车
	}
	prompt := u.Prompt()

	r.p("\n%s\n", r.c(cDim, "  ┌─ call "+fmt.Sprint(r.calls)+" · "+e.FinishReason))

	// 行 1——prompt token 去哪了。柱状图才是关键：三种颜色，
	// 按你实际被计费的比例排布。
	bar := r.cacheBar(*u)
	r.p("  %s in %s %s  %s\n",
		r.c(cDim, "│"),
		pad(fmt.Sprint(prompt), 6),
		bar,
		r.c(cDim, fmt.Sprintf("full %d · write %d · read %d%s", u.Input, u.CacheWrite, u.CacheRead, hitRate(*u))))

	// 行 2——输出和速度。TTFT 和 token/秒是两个独立的数字，
	// 因为它们会各自失灵：第一个 token 慢，可能是排队或者
	// prompt 太长；吞吐慢，则是模型本身的问题。
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

	// 行 3——钱，或一个诚实的破折号。
	if r.prices.known() {
		r.p("  %s $%s  %s\n",
			r.c(cDim, "│"),
			pad(fmt.Sprintf("%.6f", r.prices.cost(*u)), 10),
			r.c(cDim, fmt.Sprintf("session $%.6f over %d calls", r.sessionCost, r.calls)))
	} else {
		r.p("  %s %s\n", r.c(cDim, "│"), r.c(cDim, "cost — (no prices configured for this provider; see providers.json)"))
	}

	// 行 4——上下文有多满。这个数字决定了阶段 05 的压缩
	// 什么时候必须触发。
	ctx := r.c(cDim, fmt.Sprintf("context %d tokens", prompt))
	if r.window > 0 {
		ctx = r.c(cDim, fmt.Sprintf("context %d / %d (%.1f%%)", prompt, r.window, float64(prompt)*100/float64(r.window)))
	}
	r.p("  %s %s\n", r.c(cDim, "└"), ctx)
}

// cacheBar 把 prompt 拆分画成二十个格子。
//
// 三个数字的表格是可读的；柱状图**是可扫一眼的**，你想注意
// 到的，是比例在回合之间的变化。当绿色突然消失，说明某个
// 东西让你的缓存失效了——你想在它发生的那个回合就看到，
// 而不是等到月底账单里才看到。
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
			c = 1 // 永远不要让非零分量渲染成空白
		}
		return c
	}
	full, write, read := cells(u.Input), cells(u.CacheWrite), cells(u.CacheRead)
	for full+write+read > width && full > 0 {
		full--
	}
	pad := width - full - write - read

	// 三个不同的**符号**，不仅仅三种颜色。bar 必须活下来
	// `| grep`、一个文件、一个 CI log 和一个色盲读者——这些
	// 才是人们实际查看 Agent 输出的方式。一个只在彩色终端里
	// 才能用的图表，就是一个恰好在有人想用它向你展示问题时，
	// 是空白的图表。
	return r.c(cFull, strings.Repeat("█", full)) +
		r.c(cWrite, strings.Repeat("▓", write)) +
		r.c(cRead, strings.Repeat("░", read)) +
		strings.Repeat(" ", max(0, pad))
}

// SessionSummary 打印总计。重要的那一行，是最后一行：被
// 计费的 token 数，相对于产生这些 token 的对话规模。阶段 00
// 的文档记录过，在没有缓存的情况下，这个比例是 4.2x；这就是
// 你观察它变化的地方。
func (r *renderer) SessionSummary(finalPrompt int) {
	r.p("\n%s\n", r.c(cDim, "  ── session ──────────────────────"))
	r.p("  %d calls · %d commands\n", r.calls, r.commands)
	r.p("  prompt tokens billed: %d  (full %d · write %d · read %d)\n",
		r.session.Prompt(), r.session.Input, r.session.CacheWrite, r.session.CacheRead)
	r.p("  output tokens: %d\n", r.session.Output)
	if r.prices.known() {
		r.p("  cost: $%.6f\n", r.sessionCost)
	}
	if finalPrompt > 0 {
		r.p("  %s\n", r.c(cDim, fmt.Sprintf("re-send ratio: %.1fx (billed %d for a final context of %d)",
			float64(r.session.Prompt())/float64(finalPrompt), r.session.Prompt(), finalPrompt)))
	}
}

// ---------------------------------------------------------------------------

// hitRate 报告的是，prompt 里有多大比例是从缓存提供的。
//
// 分母是 Prompt()，永远不是 Input。在一个温调用上，Input
// 只是没被缓存的那部分剩余——在一次实测的运行里，是 18 个
// token 对一个 17,985-token 的 prompt——所以拿它做分母，
// 会报出 99.9% 的命中率，不管缓存到底有没有在起作用。得拿
// 你实际被计费的总数来算这个比率，不然你搭出来的就是一个
// 看不出回归的仪表盘。
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

func indentLines(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  │ " + strings.ReplaceAll(s, "\n", "\n  │ ")
}

// colorEnabled 报告是否发出 ANSI。尊重 NO_COLOR（一个实际
// 存在的跨工具约定），永远不要给管道上色——一份被输入到
// `less` 或文件里的 **trace**，应该是纯文本。
func colorEnabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
