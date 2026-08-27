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
		// 只计数，不打印。紧接着的工具结果末尾已经带着退出码和耗时了，因为
		// 那段文本本来就是写给模型看的——给你看一份和模型拿到的不一样的摘要，
		// 正是这个阶段要消灭的那类分歧。
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

	r.p("\n%s\n", r.c(cDim, "  ┌─ call "+fmt.Sprint(r.calls)+" · "+e.FinishReason))

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
	ctx := r.c(cDim, fmt.Sprintf("context %d tokens", prompt))
	if r.window > 0 {
		ctx = r.c(cDim, fmt.Sprintf("context %d / %d (%.1f%%)", prompt, r.window, float64(prompt)*100/float64(r.window)))
	}
	r.p("  %s %s\n", r.c(cDim, "└"), ctx)
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
