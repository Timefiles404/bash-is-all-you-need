// 阶段 06 —— composer：事件循环，以及 TUI 的本质。
//
// 脱掉框架，一个终端 UI 就是三个函数和一个 select：
//
//	bytes → key       解码终端发来的内容             (keys.go)
//	state + key → state   这意味着什么                  (this file)
//	state → lines     它应该看起来如何              (views.go)
//
// 下面的循环有三十行。让 TUI 困难的所有东西都在它周围的三个文件中：终端
// 必须还回去 (term.go)、键盘说的是一种有歧义的语言 (keys.go)，一列不等
// 于一个字节 (width.go)。框架把这三件事都藏了起来，这样很好，直到其中
// 一个行为不当，你却不知道是哪一个。
//
// 它也是有意为之的一个**阅读器**，而不是聊天窗口。trace 才是事实来
// 源——阶段 02 已经确保了这一点——所以 composer 可以在没有按键、没有
// 网络、没有模型的情况下工作，处理一个几周前录制的会话，或者一个此刻正
// 在另一个终端里运行的会话 (`r` 重新读取文件)。你在这里能看到的一切，
// 对一个你没有运行过的会话，你也一样能看到。
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
	call int // 选中的调用索引
	top  int // 第一个可见的正文行

	w, h  int
	lines []string // 当前视图，已渲染
	help  bool
	note  string // 一次性状态消息
}

// escTimeout 是循环在决定单独的 ESC 是 Escape 键、而不是某个序列的开始
// 之前，要等待多长时间。
//
// keys.go 解释了为什么解码器不能自己做这个判断。这个数字是一种策略：太
// 短了，慢的 ssh 链接会把方向键变成 Escape；太长了，Escape 又会感觉像
// 是坏掉了。50ms 是大多数终端应用最终都收敛到的值，这也是为什么在 vim
// 里按 Escape，一直感觉慢了那么一点点。
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
					return nil // stdin 关闭
				}
				buf = append(buf, chunk...)
				// 消耗每一个**完整的**按键。剩下的是一个前缀，唯一正确的响应是等待更多字节。
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
				// 等待结束了：缓冲区中的任何东西就是全部。
				// decodeKeyFinal 解决了 decodeKey 拒绝猜测的单独 ESC。
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
				// 询问大小；不要相信通知能承载它。
				// 通道说"它改变了"，当我们查看时它可能已经改变了——这正是为什么通知
				// 不承载任何有效负载。
				c.w, c.h = t.Size()
				c.relayout()
			}

			// nil 通道永远阻塞，所以这一行武装和解除了
			// Escape 计时器。缓冲区中的字节意味着一个未解决的
			// 前缀；空缓冲区意味着没有待处理的东西。
			if len(buf) > 0 {
				escTimer = time.After(escTimeout)
			} else {
				escTimer = nil
			}
			c.draw(t)
		}
	})
}

// bodyHeight 是可滚动区域：除了两个边框行以外的所有东西。
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

// handle 应用一个按键。返回 false 退出。
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
		case 64: // 向上滚轮
			c.top -= 3
		case 65: // 向下滚轮
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
		// 在上帝视角中，在调用之间移动会滚动到调用而不是
		// 改变显示的内容——上帝视角没有"当前"调用的概念，假装
		// 有它会使 n/p 根据你所在的视图意味着两个不同的东西。
		if _, ln := c.s.godView(c.w, c.s.Calls[i].Seq); ln > 0 {
			c.top = max(0, ln-2)
		}
	}
}

// clickAt 将屏幕行映射到上帝视角中的调用。
//
// 这是鼠标值得连接的原因：在两千行事件流中，"向我展示模型在这出错时看到的"
// 是一次点击，任何其他输入机制都是搜索。
func (c *composer) clickAt(row int) {
	if c.view != viewGod {
		return
	}
	// draw() 在屏幕第 1 行放标头，第 2 行放分隔线，所以第一个正文行是第 3
	// 行，正文行 i 是第 3+i 行。这里以前是 `row - 2`，选中的是被点击那一行
	// 的下一行——这种 bug 看起来像是鼠标不精确，而不像是算术出了错，这也是
	// 为什么它能存活这么久：没人会两次点击同一个像素去检查。
	idx := c.top + row - 3
	if idx < 0 || idx >= len(c.lines) {
		return
	}
	// 沿着事件流走到那一行的事件，然后找到它的调用。
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

// reload 从磁盘重新读取 trace。
//
// 整个重点在于：Agent 运行的时候，trace 会不断被追加内容，这让 composer
// 不需要一行 IPC，就能在第二个终端里充当实时监视器。阶段 02 的订阅者模
// 型早就替这一点付过钱了，只是它自己并不知道——文件本身就是接口。
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
// 绘图
// ---------------------------------------------------------------------------

func (c *composer) draw(t *terminal) {
	body := c.bodyHeight()
	out := make([]string, 0, c.h)

	// 标头。
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

// joinEnds 在左边放置 left，在右边放置 right。
//
// 填充用 dispWidth 计算，而不是 len。这些字符串里，每一个都包含 ANSI
// 转义序列，其中一半还可能包含带 CJK 目录名的路径；如果按字节测量，右
// 边就会落在屏幕中间附近的某处。
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

// dumpComposer 将一个视图渲染到一个写入器，不涉及任何终端。
//
// 它之所以存在，是因为渲染函数从一开始就不需要终端——views.go 把一个会
// 话变成 []string，term.go 画出 []string——一旦这种分离真正成立，无头
// 模式就是免费的。这也是 TUI 的测试方式：一个只能靠按键才能产生输出的
// UI，就是一个没有测试的 UI。
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
