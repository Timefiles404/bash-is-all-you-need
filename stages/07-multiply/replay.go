// 阶段 02——重放。
//
// 一份 **trace**，经由实时 Agent 当初用过的那个相同 Subscriber
// 读回。那就是整个诀窍所在，而它之所以可能，只是因为核心
// 什么都不打印：如果渲染器接收的是**事件**、而不是打印语句，
// 那么一个记录下来的事件，和一个实时发生的事件，就无法
// 区分——重放就只是五十行代码，而不必是 UI 的第二套实现。
//
// 它也是这个仓库里唯一一个，不需要 API 密钥、不需要网络、
// 也不需要花钱，就能运作的部分，这让它成为对"我想研究一次
// 真实的 Agent 会话"这个愿望，唯一诚实的答案——包括你从未
// 为之付费的会话。

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TraceNoticePrefix 标记的，是 ReadTrace **合成**、而非读取
// 出来的事件，这样渲染器（或测试）就能分清，这句话是 **trace**
// 自己说的，还是 Agent 说的。
const TraceNoticePrefix = "[trace] "

// maxReplayGap 限制重放在两个记录事件间等多久。
//
// 一个 **trace** 记录真实的间隙，一个真实的会话，可能包含
// 一个在两次 prompt 之间去吃了午饭的人。忠实地重现一个
// 41 分钟的间隙不是保真，它是挂起：学生看到一个冻结的终端，
// 然后杀掉它。
//
// 选定五秒，是为了让重放要传达的东西原封不动地保留下来
// ——TTFT（0.3–2s）、文本 delta 的节奏（毫秒）、命令的挂钟
// （通常在 5 秒以内）——而超出这个时间的部分，就是一个人
// 正在发呆——这种情况，时间戳本身就能比干等着更好地说明。
// 限制应用在**记录**下来的间隙上，发生在 Speed 缩放它之前，
// 所以 `--speed 2` 仍然会把它减半，刻意设置的 `--speed 0.5`
// 仍然能把它拉伸到十秒。
const maxReplayGap = 5 * time.Second

// ---------------------------------------------------------------------------
// 阅读
// ---------------------------------------------------------------------------

// ReadTrace 加载一份 **trace** 文件。
//
// 错误返回，针对的是完全无法读取的文件。一份最后一行停在
// 对象中间的 **trace，不**属于这些情况之一——它是被杀死
// 的 Agent 的正常形状，这正是你最想看的那个会话。在那里
// 返回错误，会诱使人写出条件反射式的 `if err != nil { fatal }`，
// 并把解释崩溃的那四百个事件一并扔掉。
//
// 所以伤害改为**在事件流里**报告：所有可以恢复的事件都会
// 回来，后面跟着一条合成的 KindNotice，说明哪里出了问题、
// 恢复了多少个事件。随后，重放会把它显示在它自然所属的
// 位置——会话的末尾，也就是学生在读重放时，真正会看到它
// 的地方。
func ReadTrace(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("trace: cannot read %s: %w", path, err)
	}
	defer f.Close()

	// bufio.Reader，不是 bufio.Scanner。Scanner 把单个 token 限制
	// 在 64KB，一旦某一行超出这个上限，就会用 ErrTooLong 让
	// **整个读取失败——而任何 trace** 里最有价值的那一行——
	// 请求体——恰恰正是那种，会在大约第三十回合前后，体积就
	// 长过 64KB 的行。ReadBytes 没有这个限制，它会返回一个没有
	// 尾随 '\n' 的最后一行，并伴随 io.EOF 一起出现，这正是一个
	// 写入方在行的中途死掉的信号。
	r := bufio.NewReaderSize(f, 64*1024)

	var (
		events    []Event
		corrupt   int // 解析失败的完整行：真正的伤害
		truncated int // 没有终止符的最后一行里的字节：普通的崩溃
	)
	for {
		line, rerr := r.ReadBytes('\n')
		atEOF := rerr == io.EOF

		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var e Event
			switch {
			case trimmed[0] != '{':
				// JSONL 行是对象。其他任何东西，都是某个人的日志输出，
				// 碰巧落进了同一个文件里。
				corrupt++

			case json.Unmarshal(trimmed, &e) != nil:
				if atEOF {
					// 无尾随换行，**且**它不解析。写入方会在单次写入中，发出
					// "对象+'\n'"，所以这是一次写入——内核在进程死掉之前，只
					// 提交了一部分。预期，不是损坏。
					truncated = len(trimmed)
				} else {
					// 一个解析失败的完整行，情况不一样：它后面的字节都完好地
					// 保留了下来，所以这是伤害，出现在一份原本完好无损的文件
					// 正中间。跳过它，并把这一点报告出来。
					corrupt++
				}

			default:
				// 注意这里有意**不**检查的东西：e.Kind 从来没有对照 events.go
				// 里的常量做过验证。一份由更新的构建写出的 **trace**，可能
				// 携带着这个二进制从未听说过的 kind，如果拒绝这些 kind，就
				// 意味着以后每一个新增的 kind，都会无声无息地破坏它出现
				// 之后所有记录下来的文件的重放。未知的 kind 一样能加载、能
				// 重放、能到达渲染器，渲染器可以选择把它们原样打印出来。
				// （唯一真正的限制：未知**字段**会在解码时被丢弃——这无害，
				// 因为这里没有任何东西会重新序列化。）
				events = append(events, e)
			}
		}

		if rerr != nil {
			if !atEOF {
				// 一个真实的 I/O 失败。无论如何，都要把已经恢复的部分交
				// 回去；部分证据总好过没有证据。
				return events, fmt.Errorf("trace: reading %s after %d events: %w", path, len(events), rerr)
			}
			break
		}
	}

	if truncated > 0 || corrupt > 0 {
		events = append(events, traceDamageNotice(path, events, truncated, corrupt))
	}
	return events, nil
}

// traceDamageNotice 构建出一个合成事件，告诉读者这份文件
// 缺失了什么。它借用最后一个真实事件的时间戳和回合数，
// 这样一个按时间顺序排列的渲染器，才会把它放在它该在的
// 位置，而不是放到纪元起点。
func traceDamageNotice(path string, events []Event, truncated, corrupt int) Event {
	e := Event{Kind: KindNotice}
	if n := len(events); n > 0 {
		e.Seq = events[n-1].Seq + 1
		e.T = events[n-1].T
		e.Turn = events[n-1].Turn
	} else {
		e.Seq = 1
		e.T = time.Now()
	}

	var parts []string
	if truncated > 0 {
		parts = append(parts, fmt.Sprintf("ends in a %d-byte partial line (the agent was killed mid-write)", truncated))
	}
	if corrupt > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable line(s) skipped", corrupt))
	}
	e.Text = fmt.Sprintf("%s%s %s — %d event(s) recovered",
		TraceNoticePrefix, filepath.Base(path), strings.Join(parts, "; "), len(events))
	return e
}

// ---------------------------------------------------------------------------
// 总结
// ---------------------------------------------------------------------------

// TraceSummary 是在重放开始前显示的、可以一眼看完的表头。
type TraceSummary struct {
	Events     int
	Turns      int
	Commands   int
	Duration   time.Duration
	TotalUsage Usage // 对所有 KindUsage 事件求和
	Errors     int
}

// Summarize 把一整个会话，浓缩成最值得先看的六个数字。
func Summarize(events []Event) TraceSummary {
	var s TraceSummary
	s.Events = len(events)

	var first, last time.Time
	for _, e := range events {
		switch e.Kind {
		case KindTurnStart:
			// 在**开始时计数，不是结束时。值得读的 trace** 是在回合
			// 中间停的那些：一个在第 12 回合中途被杀死的会话，实际做了
			// 十二个回合，而统计 turn_end 只会报告十一，还会把你打开
			// 这份文件正是想看的那一个回合，藏起来。
			//
			// 也不是"不同 e.Turn 值的数量"：Turn 在每一条用户消息处都会
			// 重新从 1 开始（参见 events.go），所以，对任何长度超过一条
			// prompt 的会话，用不同值计数都会偏少。
			s.Turns++

		case KindCommandStart:
			s.Commands++ // 同样的道理：永远没有返回的命令，也算数

		case KindError:
			s.Errors++

		case KindUsage:
			// 以 kind 作为判断依据，而不只是看 Usage != nil，这样一来，
			// 未来某个事件如果在携带别的东西的同时，也搭带了一份
			// usage 快照，也不会无声无息地把总数算重复。
			if e.Usage != nil {
				// 每个字段各自单独求和，prompt 的总数，是之后再从它们
				// 派生出来的。对"输入 token"求和，正是 Usage 文档注释警告
				// 过的那个 bug：一个缓存回合报告输入 18，而实际发送了
				// 18,000，所以一个只靠 Input 构建出来的总数，会偏差达三个
				// 数量级——而且这个数字看起来又足够合理，以至于从来没有
				// 人回头去复查它。
				s.TotalUsage = addUsage(s.TotalUsage, *e.Usage)
			}
		}

		// 最小/最大值，而不是第一个和最后一个元素：一个 T 为零
		// 的事件（手动构建的，或者来自某个将来会省略它的写入方），
		// 否则就会让持续时间变成一个 55 年的负数。
		if !e.T.IsZero() {
			if first.IsZero() || e.T.Before(first) {
				first = e.T
			}
			if e.T.After(last) {
				last = e.T
			}
		}
	}
	if !first.IsZero() {
		s.Duration = last.Sub(first)
	}
	return s
}

// PromptTokens 是人们问"这个会话到底发送了多少"时，真正
// 想问的那个数字。
//
// 它是 Prompt()，从不是 Input。参见 events.go 中 Usage 的文档
// 注释：Input 只是未缓存剩下的那部分，把它当成总数来读，
// 是 Agent 自身的 token 会计变成一句谎话的最常见单一原因。
func (s TraceSummary) PromptTokens() int { return s.TotalUsage.Prompt() }

// String 渲染出表头。两行，不带颜色：它必须在学生把重放
// 的输出用管道接进文件时，依然可读。
func (s TraceSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "trace · %s · %s · %s · %s",
		tracePlural(s.Events, "event"), tracePlural(s.Turns, "turn"),
		tracePlural(s.Commands, "command"), traceDur(s.Duration))
	if s.Errors > 0 {
		fmt.Fprintf(&b, " · %s", tracePlural(s.Errors, "error"))
	}
	if s.PromptTokens() > 0 || s.TotalUsage.Output > 0 {
		// 拆分，总是。一个"prompt token: 18231"这样的数字，会
		// 掩盖这些 token 到底是按三种价格中的哪一种计费的，而这
		// 三者之间的差距超过十倍。
		fmt.Fprintf(&b, "\ntokens · prompt %d (full %d · write %d · read %d) · output %d",
			s.PromptTokens(), s.TotalUsage.Input, s.TotalUsage.CacheWrite,
			s.TotalUsage.CacheRead, s.TotalUsage.Output)
	}
	return b.String()
}

// tracePlural 不让表头说出"1 commands"这种话。为了一个
// 字符，付出这么多心思，多少有点可笑，但它也是学生在这个
// 仓库唯一宣传为无需 API 密钥就能用的那个功能里，看到的
// 第一行。
func tracePlural(n int, singular string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %ss", n, singular)
}

func traceDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d >= time.Minute:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

// ---------------------------------------------------------------------------
// 重放
// ---------------------------------------------------------------------------

type ReplayOpts struct {
	Speed  float64          // 0 = 立即，1 = 原始挂钟计时，2 = 双速
	Step   bool             // 在每个事件前等待回车
	Filter func(Event) bool // nil = 一切
}

// Replay 把已经记录下来的事件，喂给 Subscriber，就好像它们
// 是现在正在发生的一样。
//
// 它故意不做的一件事，是重新给 Event.T 盖时间戳。"就好像
// 它们是现在发生的"，说的是节奏，不是撒谎：记录下来的
// 时间戳才是证据，一个显示 TTFT 或命令挂钟耗时的渲染器，
// 读的正是它们。重放控制的是 OnEvent **何时**被调用，从来
// 不控制调用它时传的是什么内容——这也正是为什么测试能够
// 把一次重放运行，和一次实时运行，逐个事件地拿来比较。
func Replay(events []Event, sub Subscriber, opts ReplayOpts, in io.Reader, out io.Writer) error {
	if sub == nil {
		return fmt.Errorf("replay: no subscriber to replay into")
	}
	if out == nil {
		out = io.Discard
	}

	shown := events
	if opts.Filter != nil {
		shown = nil
		for _, e := range events {
			if opts.Filter(e) {
				shown = append(shown, e)
			}
		}
	}

	// 表头总结的是**整个 trace**，即便某个过滤器正开着，
	// 因为"这个会话一共发起了 47 次模型调用，而你正在看的只是
	// 其中 3 次"这句话，正是防止一个被过滤过的视图，被误认成
	// 整个会话的那个上下文信息。
	fmt.Fprintln(out, Summarize(events))
	fmt.Fprintf(out, "replay · %s", replayMode(opts))
	if len(shown) != len(events) {
		fmt.Fprintf(out, " · showing %d of %d events", len(shown), len(events))
	}
	fmt.Fprint(out, "\n\n")

	var stepIn *bufio.Reader
	if opts.Step {
		if in == nil {
			in = strings.NewReader("")
		}
		// 构建一次，在循环外。如果每个事件都用一个全新的
		// bufio.Reader，就会预读进它自己的缓冲区，把第一行之后的
		// 所有内容都扔掉，无声无息地吃掉用户接下来敲的按键。
		stepIn = bufio.NewReader(in)
	}

	var prev time.Time
	for i, e := range shown {
		switch {
		case opts.Step:
			// Step 赢过 Speed。等一个人类，然后又把记录下来的间隙睡
			// 一遍，只是用更慢的方式，等同一个人类罢了。
			fmt.Fprintf(out, "[%d/%d %s] ", i+1, len(shown), e.Kind)
			cont, err := readStep(stepIn)
			if err != nil {
				return fmt.Errorf("replay: reading step input: %w", err)
			}
			if !cont {
				fmt.Fprintf(out, "\n[replay stopped after %d of %d events]\n", i, len(shown))
				return nil
			}

		case opts.Speed > 0 && !prev.IsZero():
			gap := e.T.Sub(prev)
			if gap > maxReplayGap {
				gap = maxReplayGap
			}
			if gap > 0 {
				// 负间隙是可能的，且不是一个需要在这里修复的 bug：两个
				// 事件能共享一个时间戳，一份从两个进程合并而成的 **trace**，
				// 时间戳可能会倒退。把它钳位在零，能让重放不管时钟做了
				// 什么，都继续向前推进。
				time.Sleep(time.Duration(float64(gap) / opts.Speed))
			}
		}

		if !e.T.IsZero() {
			prev = e.T
		}
		sub.OnEvent(e)
	}
	return nil
}

// readStep 消费正好一行，并报告重放是否应该继续。
//
// 正好一行：如果单次回车会被读成两步，一份有 4,000 个文本
// delta 的 **trace** 就没法用；如果读完一步要按两次回车，
// 它就没法读。
func readStep(r *bufio.Reader) (bool, error) {
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	if err == io.EOF && strings.TrimSpace(line) == "" {
		// Ctrl-D，或者一个输入用完了的脚本。停止，是对"用户关闭
		// 了输入"这件事，最诚实的解读；在无人看管的情况下，播放
		// 剩下的内容，则会是一个意外。
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "q", "quit", "exit":
		return false, nil
	default:
		return true, nil
	}
}

func replayMode(opts ReplayOpts) string {
	if opts.Step {
		return "step (Enter = next, q = quit)"
	}
	if opts.Speed <= 0 {
		return "instant"
	}
	return fmt.Sprintf("%gx speed (gaps capped at %s)", opts.Speed, maxReplayGap)
}
