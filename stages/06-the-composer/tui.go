// 阶段 06——composer：事件循环，以及 TUI 到底是什么东西。
//
// 把框架剥掉，终端界面就是三个函数加一个 select：
//
//	字节 → 键          解码终端送来的东西        （keys.go）
//	状态 + 键 → 状态    这意味着什么              （本文件）
//	状态 → 行          它该长什么样              （views.go）
//
// 下面这个循环三十行。让 TUI 变难的东西全在它周围那三个文件里：终端要还
// 回去（term.go），键盘说的那门语言里带着歧义（keys.go），而一列不等于
// 一字节（width.go）。框架把这三样全藏起来，这挺好，直到其中一样出岔子，
// 而你完全不知道是哪一样。
//
// 它还是有意做成**读者**而不是聊天窗口。trace 才是事实来源——阶段 02 把这
// 件事坐实了——所以 composer 不用 key、不用网络、也不用模型就能跑，无论会
// 话是几周前录的，还是此刻正在另一个终端里跑着（按 `r` 重读文件）。你在
// 这儿能看到的一切，换成不是你跑的会话，同样能看到。
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type viewKind int

const (
	viewGod viewKind = iota
	viewModel
	viewWire
)

func (v viewKind) String() string {
	return [...]string{"GOD", "MODEL", "WIRE"}[v]
}

type composer struct {
	path string
	s    *session

	view viewKind
	call int // 选中的调用序号
	top  int // 正文里最先可见的那一行

	w, h  int
	lines []string // 当前视图，渲染好的
	help  bool
	note  string // 只显示一次的状态消息
}

// escTimeout 是主循环在认定落单的 ESC 是 Escape 键、而不是某段序列的开头
// 之前，愿意等多久。
//
// keys.go 里讲了解码器为什么不能自己下这个判断。这个数字是一条策略：定短
// 了，慢一点的 ssh 链路会把方向键变成 Escape；定长了，Escape 按着就像坏
// 了。50ms 是绝大多数终端程序最后收敛到的值，也是为什么在 vim 里按
// Escape 从来都感觉略微晚了一点。
const escTimeout = 50 * time.Millisecond

func runComposer(path string) error {
	events, err := ReadTrace(path)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("%s contains no events", path)
	}
	c := &composer{path: path, s: indexSession(path, events)}

	return withTerminal(os.Stdin, os.Stdout, func(t *terminal) error {
		c.w, c.h = t.Size()
		c.relayout()
		c.draw(t)

		in := readLoop(t.in)
		var buf []byte
		var escTimer <-chan time.Time

		for {
			select {
			case chunk, ok := <-in:
				if !ok {
					return nil // stdin 关了
				}
				buf = append(buf, chunk...)
				// 把每一个**完整**的键都排干。剩下的是前缀，而对前缀
				// 唯一正确的反应就是等更多字节。
				for len(buf) > 0 {
					k, n, ok := decodeKey(buf)
					if !ok {
						break
					}
					buf = buf[n:]
					if !c.handle(k) {
						return nil
					}
				}

			case <-escTimer:
				// 等待结束了：缓冲区里有什么就只有什么。decodeKey 不肯
				// 猜的那个落单 ESC，由 decodeKeyFinal 来裁定。
				for len(buf) > 0 {
					k, n, ok := decodeKeyFinal(buf)
					if !ok {
						break
					}
					buf = buf[n:]
					if !c.handle(k) {
						return nil
					}
				}

			case <-t.resize:
				// 去问尺寸，别指望通知里带着它。channel 说的是"它变
				// 了"，而等我们去看的时候，它可能又变了一次——通知不带
				// 任何载荷，正是为了这一点。
				c.w, c.h = t.Size()
				c.relayout()
			}

			// nil 的 channel 会永远阻塞，所以这一行就把 Escape 计时器
			// 装上了、也卸下了。缓冲区里还有字节，说明有个前缀没裁
			// 定；缓冲区空了，就说明没什么在等着。
			if len(buf) > 0 {
				escTimer = time.After(escTimeout)
			} else {
				escTimer = nil
			}
			c.draw(t)
		}
	})
}

// bodyHeight 是可滚动的区域：除掉那两行界面框架之外的全部。
func (c *composer) bodyHeight() int { return max(1, c.h-3) }

func (c *composer) relayout() {
	switch c.view {
	case viewGod:
		c.lines, _ = c.s.godView(c.w, 0)
	case viewModel:
		c.lines = c.s.modelView(c.call, c.w)
	case viewWire:
		c.lines = c.s.wireView(c.call, c.w)
	}
	c.clamp()
}

func (c *composer) clamp() {
	maxTop := max(0, len(c.lines)-c.bodyHeight())
	c.top = min(max(0, c.top), maxTop)
}

// handle 处理一个键。返回 false 就退出。
func (c *composer) handle(k key) bool {
	c.note = ""
	switch k.Kind {
	case keyCtrlC, keyCtrlD:
		return false
	case keyEsc:
		if c.help {
			c.help = false
			return true
		}
		return false

	case keyUp:
		c.top--
	case keyDown:
		c.top++
	case keyPageUp:
		c.top -= c.bodyHeight()
	case keyPageDown:
		c.top += c.bodyHeight()
	case keyHome:
		c.top = 0
	case keyEnd:
		c.top = len(c.lines)

	case keyMouse:
		switch k.Mouse.Button {
		case 64: // 滚轮上滚
			c.top -= 3
		case 65: // 滚轮下滚
			c.top += 3
		case 0:
			if k.Mouse.Press {
				c.clickAt(k.Mouse.Y)
			}
		}

	case keyRune:
		switch k.Rune {
		case 'q':
			return false
		case 'g', '1':
			c.setView(viewGod)
		case 'm', '2':
			c.setView(viewModel)
		case 'w', '3':
			c.setView(viewWire)
		case 'j':
			c.top++
		case 'k':
			c.top--
		case ' ':
			c.top += c.bodyHeight()
		case 'n', ']':
			c.selectCall(c.call + 1)
		case 'p', '[':
			c.selectCall(c.call - 1)
		case 'r':
			c.reload()
		case '?':
			c.help = !c.help
		}

	case keyTab:
		c.setView((c.view + 1) % 3)
	}
	c.relayout()
	return true
}

func (c *composer) setView(v viewKind) {
	if v == c.view {
		return
	}
	c.view = v
	c.top = 0
}

func (c *composer) selectCall(i int) {
	if i < 0 || i >= len(c.s.Calls) {
		return
	}
	c.call = i
	c.top = 0
	if c.view == viewGod {
		// 上帝视角里，在调用之间移动是滚到那次调用去，而不是换掉显示的内
		// 容——上帝视角里根本没有"当前"调用这个概念，硬装成有，就会让 n/p 在
		// 不同视角下代表两件不同的事。
		if _, ln := c.s.godView(c.w, c.s.Calls[i].Seq); ln > 0 {
			c.top = max(0, ln-2)
		}
	}
}

// clickAt 在上帝视角里把屏幕上的一行映射到一次调用。
//
// 鼠标值得接进来，理由就在这里：在两千行的事件流里，"让我看看出问题那一
// 刻模型看到了什么"是一次点击，换成任何别的输入方式都是一次搜索。
func (c *composer) clickAt(row int) {
	if c.view != viewGod {
		return
	}
	// draw() 把表头放在屏幕第 1 行、分隔线放在第 2 行，所以正文第一行是第
	// 3 行，正文第 i 行就是第 3+i 行。这里原来写的是 `row - 2`，选中的是点
	// 击那行的下一行——这类 bug 看起来像鼠标不准，而不像算错了数，所以它能
	// 活很久：没人会为了核对，对着同一个像素点两次。
	idx := c.top + row - 3
	if idx < 0 || idx >= len(c.lines) {
		return
	}
	// 沿着事件流走到那一行对应的事件，再找出它属于哪次调用。
	seq := 0
	n := 0
	for _, e := range c.s.Display {
		ls := len(c.s.godLine(e, c.w))
		if n+ls > idx {
			seq = e.Seq
			break
		}
		n += ls
	}
	for i := len(c.s.Calls) - 1; i >= 0; i-- {
		if c.s.Calls[i].Seq <= seq {
			c.call = i
			c.note = fmt.Sprintf("selected call %d — press m for what the model saw", i+1)
			return
		}
	}
}

// reload 从磁盘重读 trace。
//
// 这才是重点：Agent 在跑的时候 trace 一直在被追加，所以这一下就让
// composer 变成了另一个终端里的实时监视器，一行 IPC 代码都不用写。阶段
// 02 的订阅者模型自己都不知道，它早就把这笔账付过了——文件就是接口。
func (c *composer) reload() {
	events, err := ReadTrace(c.path)
	if err != nil {
		c.note = sBad + "reload failed: " + err.Error() + sOff
		return
	}
	before := len(c.s.Events)
	c.s = indexSession(c.path, events)
	c.note = fmt.Sprintf("reloaded: %d events (+%d)", len(events), len(events)-before)
	if c.call >= len(c.s.Calls) {
		c.call = max(0, len(c.s.Calls)-1)
	}
}

// ---------------------------------------------------------------------------
// 绘制
// ---------------------------------------------------------------------------

func (c *composer) draw(t *terminal) {
	body := c.bodyHeight()
	out := make([]string, 0, c.h)

	// 表头。
	left := fmt.Sprintf(" %s  %s", bold("composer"), c.path)
	right := fmt.Sprintf("%d events · %d calls · %d compactions  [%s] ",
		len(c.s.Events), len(c.s.Calls), c.s.Compactions, bold(c.view.String()))
	out = append(out, joinEnds(left, right, c.w))
	out = append(out, dim(strings.Repeat("─", c.w)))

	if c.help {
		out = append(out, helpLines()...)
		for len(out) < c.h {
			out = append(out, "")
		}
		t.Frame(out, c.w, c.h)
		return
	}

	for i := 0; i < body; i++ {
		if c.top+i < len(c.lines) {
			out = append(out, c.lines[c.top+i])
		} else {
			out = append(out, "")
		}
	}

	// 页脚。
	pos := "top"
	if len(c.lines) > body {
		pos = fmt.Sprintf("%d%%", min(100, (c.top+body)*100/len(c.lines)))
	}
	status := c.note
	if status == "" {
		status = dim("g god · m model · w wire · n/p call · ↑↓ scroll · r reload · ? keys · q quit")
	}
	out = append(out, joinEnds(" "+status,
		dim(fmt.Sprintf("call %d/%d · %s ", c.call+1, max(1, len(c.s.Calls)), pos)), c.w))

	t.Frame(out, c.w, c.h)
}

// joinEnds 把 left 顶到左边缘，把 right 顶到右边缘。
//
// 中间的填充是用 dispWidth 算的，不是 len。这些字符串里每一个都含 ANSI
// 转义，其中一半还可能含着带 CJK 目录名的路径；按字节量出来，右边那一截
// 会落在屏幕中间附近某个地方。
func joinEnds(left, right string, w int) string {
	gap := w - dispWidth(left) - dispWidth(right)
	if gap < 1 {
		return truncCols(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func helpLines() []string {
	return []string{
		"",
		"  " + bold("views"),
		"    g / 1     " + dim("GOD    — every event that happened, including what was never sent"),
		"    m / 2     " + dim("MODEL  — the messages the model actually received on this call"),
		"    w / 3     " + dim("WIRE   — the raw request body, byte for byte"),
		"    Tab       " + dim("cycle"),
		"",
		"  " + bold("moving"),
		"    ↑ ↓ j k   " + dim("scroll one line          PgUp/PgDn/Space   scroll a page"),
		"    Home End  " + dim("jump to top / bottom     wheel             scroll three"),
		"    n / p     " + dim("next / previous model call"),
		"    click     " + dim("(GOD view) select the call containing that line"),
		"",
		"  " + bold("other"),
		"    r         " + dim("re-read the trace from disk — works on a session still running"),
		"    ? Esc     " + dim("this help"),
		"    q Ctrl-C  " + dim("quit"),
		"",
		"  " + dim("The point of three views is that they DISAGREE. What happened, what the"),
		"  " + dim("model saw, and what went on the wire are three different things, and every"),
		"  " + dim("gap between them is a bug you cannot find in a chat log."),
	}
}

// dumpComposer 把一个视图渲染到 writer 上，全程不牵扯终端。
//
// 它之所以存在，是因为那些渲染函数本来就从没需要过终端——views.go 把会话
// 变成 []string，term.go 把 []string 画出来——这个分离一旦是真的，无头模式
// 就是白送的。TUI 也正是靠它来测的：只有按键才能产出输出的界面，就是没有
// 测试的界面。
func dumpComposer(path, view string, call, width int, w io.Writer) error {
	events, err := ReadTrace(path)
	if err != nil {
		return err
	}
	s := indexSession(path, events)
	idx := call - 1
	var lines []string
	switch view {
	case "god":
		lines, _ = s.godView(width, 0)
	case "model":
		lines = s.modelView(idx, width)
	case "wire":
		lines = s.wireView(idx, width)
	default:
		return fmt.Errorf("unknown view %q (want god, model or wire)", view)
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	return nil
}
