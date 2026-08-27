// 阶段 02——重放。
//
// 把 trace 通过实跑 Agent 用的那同一个 Subscriber 读回去。全部的诀窍就
// 在这儿，而它成立的唯一原因是核心什么都不打印：渲染器收的既然是**事件**
// 而不是 print 语句，那么录下来的事件和实时的事件就分不出差别，重放于是
// 只要五十行，而不是把 UI 再实现一遍。
//
// 它也是这个仓库里唯一不用 API key、不用联网、不用花钱就能跑的部分，
// 所以对"我想研究一段真实的 Agent 会话"这句话，它是老实的答案——包括
// 那些你从来没付过钱的会话。

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

// TraceNoticePrefix 用来标记那些 ReadTrace **合成**出来、而不是读出来的
// 事件，好让渲染器（或者测试）能把"这是 trace 说的"和"这是 Agent 说的"
// 分开。
const TraceNoticePrefix = "[trace] "

// maxReplayGap 给重放在两条录下的事件之间等多久设了上限。
//
// trace 记的是真实的间隔，而真实的会话里有个人在两次 prompt 之间去吃了
// 午饭。把 41 分钟的间隔忠实地重现出来，那不是保真，那是卡死：学生看到
// 终端冻住，就把它杀了。
//
// 选五秒，是为了让重放要传达的一切都原封不动地活下来——TTFT（0.3–2 秒）、
// 文本 delta 的节奏（毫秒级）、命令的挂钟时间（通常不到 5 秒）——而超过
// 五秒的都是人在发呆，那种事时间戳本来就报得比干等更清楚。上限卡的是
// **录下来的**间隔，在 Speed 缩放它之前，所以 `--speed 2` 照样把它减半，
// 而故意写的 `--speed 0.5` 还能把它撑到十秒。
const maxReplayGap = 5 * time.Second

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

// ReadTrace 加载一份 trace 文件。
//
// 返回 error 是留给根本读不了的文件的。trace 的最后一行停在对象中间，
// **不**属于这种情况——那是 Agent 被杀掉之后的正常样子，而那恰恰是你最
// 想看的那次会话。在那里返回 error，等于招来 `if err != nil { fatal }`
// 这个条件反射，把解释崩溃的那四百条事件全扔了。
//
// 所以损坏是**改在事件流里**报的：所有还能救回来的都还回去，后面跟一条
// 合成的 KindNotice，说清楚哪里不对、救回了多少条事件。重放于是把它摆在
// 它本来该在的位置，会话的末尾——读重放的学生在那里真的会看见它。
func ReadTrace(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("trace: cannot read %s: %w", path, err)
	}
	defer f.Close()

	// 用 bufio.Reader，不用 bufio.Scanner。Scanner 把单个 token 卡在 64KB，
	// 碰到第一行超限就用 ErrTooLong 让**整次**读取失败——而任何 trace 里最
	// 值钱的那一行，request body，恰恰就是在第三十个回合前后涨过 64KB 的那
	// 一行。ReadBytes 没有上限，而且它会把末尾没有 '\n' 的最后一行连同
	// io.EOF 一起返回，那正是"写入方写到一半死了"的信号。
	r := bufio.NewReaderSize(f, 64*1024)

	var (
		events    []Event
		corrupt   int // 解析不了的完整行：真正的损坏
		truncated int // 末行没写完的字节数：寻常的崩溃
	)
	for {
		line, rerr := r.ReadBytes('\n')
		atEOF := rerr == io.EOF

		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var e Event
			switch {
			case trimmed[0] != '{':
				// JSONL 的行都是对象。不是对象的，就是谁的日志
				// 输出跑到同一个文件里来了。
				corrupt++

			case json.Unmarshal(trimmed, &e) != nil:
				if atEOF {
					// 末尾没有换行，**而且**解析不了。写入方是把
					// object+'\n' 一次写出去的，所以这次写内核只提交
					// 了一部分，进程就死了。是预期之内，不是损坏。
					truncated = len(trimmed)
				} else {
					// 完整的一行却解析不了，那是另一回事：它后面的
					// 字节都还在，所以这是原本完好的文件中间烂了一块。
					// 跳过它，并且把这事说出来。
					corrupt++
				}

			default:
				// 注意这里故意**不**检查什么：e.Kind 从不拿 events.go
				// 里的常量去校验。更新的构建写出来的 trace，会带着这个
				// 二进制程序从没听说过的 kind；把它们拒掉，就等于以后
				// 每出一种新 kind，都会悄没声地让此后录下的每个文件
				// 都重放不了。不认识的 kind 照样加载、照样重放、照样
				// 送到渲染器手里，渲染器爱怎么原样打印就怎么打印。
				// （唯一真正的限制：不认识的**字段**在解码时会被丢掉
				// ——只要这里没有谁重新序列化，就无害。）
				events = append(events, e)
			}
		}

		if rerr != nil {
			if !atEOF {
				// 真正的 I/O 失败。救回来的照样交回去；有一部分证据，
				// 也好过一点没有。
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

// traceDamageNotice 造出那条合成事件，告诉读者这个文件缺了什么。它借用
// 最后一条真事件的时间戳和回合数，这样按时间排序的渲染器会把它放在它
// 该在的地方，而不是放到纪元零点。
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
// 汇总
// ---------------------------------------------------------------------------

// TraceSummary 是重放开始前显示的表头，一眼看完。
type TraceSummary struct {
	Events     int
	Turns      int
	Commands   int
	Duration   time.Duration
	TotalUsage Usage // 把所有 KindUsage 事件加起来
	Errors     int

	// 阶段 09。故意和 Errors 分开数：重试之后成功了的 call_error，不是会话
	// 挨到的错误，而是会话吸收掉的失败。把两者并在一起，会让每一场健壮的会
	// 话看上去都像坏了；而没人信的表头，就是没人读的表头。
	CallErrors int
	Retries    int
	Fallbacks  int
}

// Summarize 把整段会话压成最值得先看的六个数。
func Summarize(events []Event) TraceSummary {
	var s TraceSummary
	s.Events = len(events)

	var first, last time.Time
	for _, e := range events {
		switch e.Kind {
		case KindTurnStart:
			// 在**开头**计数，不在结尾。值得读的 trace，恰恰是停在回合
			// 中间的那些：会话在第 12 回合被杀，它做了十二个回合；数
			// turn_end 会报十一个，而你打开这个文件要看的，正好是被
			// 藏掉的那一个。
			//
			// 也不是"e.Turn 有多少个不同的值"：每来一条用户消息 Turn
			// 就从 1 重来（见 events.go），所以只要会话超过一次 prompt，
			// 按不同值去数就数少了。
			s.Turns++

		case KindCommandStart:
			s.Commands++ // 同样的道理：没返回的那条命令也算

		case KindError:
			s.Errors++

		case KindCallError:
			s.CallErrors++

		case KindRetry:
			s.Retries++

		case KindProvider:
			// 会话开始那个事件不带分诊裁决，只有降级才带。要是把每个
			// provider 事件都数进去，那么有记录以来的每一场干净会话，都
			// 会被报成发生过一次降级。
			if e.Triage != "" {
				s.Fallbacks++
			}

		case KindUsage:
			// 是按 kind 卡的，不只是按 Usage != nil 卡，这样以后要是有别
			// 的事件顺带捎上一份 usage 快照，也不会悄没声地把总数翻倍。
			if e.Usage != nil {
				// 每个字段分开求和，prompt 总数事后再从它们推出来。
				// 把"input tokens"加起来，正是 Usage 那段文档注释警
				// 告的那个 bug：命中缓存的回合报 Input 18，实际发出
				// 去的是 18,000，所以只用 Input 堆出来的总数会差三个
				// 数量级——而且这数字看着够合理，压根没人回头核对。
				s.TotalUsage = addUsage(s.TotalUsage, *e.Usage)
			}
		}

		// 取最小最大，不取首尾两个元素：万一有条事件的 T 是零值
		// （手工造的，或者来自将来某个不写它的写入方），时长就会
		// 变成负的 55 年。
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

// 别人问这段会话发出去了多少，问的就是 PromptTokens。
//
// 它是 Prompt()，绝不是 Input。见 events.go 里 Usage 的文档注释：
// Input 只是没命中缓存的那点余量，把它当成总数来读，是 Agent 自报的
// token 账变成谎话最常见的方式。
func (s TraceSummary) PromptTokens() int { return s.TotalUsage.Prompt() }

// String 渲染表头。两行，不上色：学生把 replay 管进文件的时候，它得能读。
func (s TraceSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "trace · %s · %s · %s · %s",
		tracePlural(s.Events, "event"), tracePlural(s.Turns, "turn"),
		tracePlural(s.Commands, "command"), traceDur(s.Duration))
	if s.Errors > 0 {
		fmt.Fprintf(&b, " · %s", tracePlural(s.Errors, "error"))
	}
	if s.CallErrors > 0 {
		fmt.Fprintf(&b, " · %s", tracePlural(s.CallErrors, "failed call"))
	}
	if s.Retries > 0 {
		// 这里是写死的，没走 tracePlural——那个函数会加个 "s"。表头上出现
		// "2 retrys"，恰好就是那类细节：读者从此不再相信印在它旁边的数字。
		word := "retries"
		if s.Retries == 1 {
			word = "retry"
		}
		fmt.Fprintf(&b, " · %d %s", s.Retries, word)
	}
	if s.Fallbacks > 0 {
		fmt.Fprintf(&b, " · %s", tracePlural(s.Fallbacks, "fallback"))
	}
	if s.PromptTokens() > 0 || s.TotalUsage.Output > 0 {
		// 永远给出切分。只报一句 "prompt tokens: 18231"，就藏住了这些
		// token 到底按三种价钱里的哪一种计费，而三者差着 10x 以上。
		fmt.Fprintf(&b, "\ntokens · prompt %d (full %d · write %d · read %d) · output %d",
			s.PromptTokens(), s.TotalUsage.Input, s.TotalUsage.CacheWrite,
			s.TotalUsage.CacheRead, s.TotalUsage.Output)
	}
	return b.String()
}

// tracePlural 不让表头写出 "1 commands"。为一个字符操这么多心，是有点
// 傻；可这个仓库宣传说不用 API key 就能用的功能只有这一个，而这是学生
// 在里面看到的第一行。
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
	Speed  float64          // 0 = 瞬间放完，1 = 原本的挂钟节奏，2 = 双倍速
	Step   bool             // 每条事件之前等一次 Enter
	Filter func(Event) bool // nil = 全都要
}

// Replay 把录下的事件喂给 Subscriber，就像它们此刻正在发生。
//
// 它故意不做的一件事，是给 Event.T 重新盖时间戳。"就像此刻正在发生"
// 说的是节奏，不是撒谎：录下的时间戳就是证据，而显示 TTFT 或者命令
// 挂钟时间的渲染器，读的正是它们。Replay 控制的是 OnEvent
// **什么时候**被调用，从不控制调用时传的是什么——测试能拿重放的一次
// 运行和实跑的一次逐条对照，靠的也是这一点。
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

	// 就算开了过滤，表头汇总的也是**整份** trace。因为有了"这段会话调了
	// 47 次模型，你现在看的是其中 3 次"这句话，才不会有人把过滤后的视图
	// 当成整段会话。
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
		// 只在循环外面建一次。每条事件都新建一次 bufio.Reader，它就会
		// 预读进自己的 buffer，把第一行之后的东西全扔掉，一声不响地吃
		// 掉用户接下来的按键。
		stepIn = bufio.NewReader(in)
	}

	var prev time.Time
	for i, e := range shown {
		switch {
		case opts.Step:
			// Step 压过 Speed。先等人，再把录下的间隔也睡一遍，那不过是
			// 换个更慢的法子等同一个人。
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
				// 间隔为负是可能的，也不是该在这里修的 bug：两条事件
				// 可以共用一个时间戳，两个进程合并出来的 trace 也可能
				// 往回走。夹到零，不管时钟干了什么，重放都只往前走。
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

// readStep 正好吃掉一行，并报告重放该不该继续。
//
// 正好一行：有 4,000 条文本 delta 的 trace，要是一次 Enter 会被读成两步，
// 就没法用了；要是一步得按两次 Enter，就没法读了。
func readStep(r *bufio.Reader) (bool, error) {
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	if err == io.EOF && strings.TrimSpace(line) == "" {
		// Ctrl-D，或者脚本的输入用完了。把"用户关掉了输入"读成"停下来"，
		// 是老实的读法；没人看着还把剩下的放完，那就太意外了。
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
