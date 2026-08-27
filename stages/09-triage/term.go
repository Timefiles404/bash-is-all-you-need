// 阶段 06——终端，以及你一旦进入原始模式就要承担的契约。
//
// TUI 需要终端提供四样东西，而每一样都是**对一份你并不拥有的资源的全局改动**：
//
//	原始模式        键以字节而不是行到达，Ctrl-C 不再
//	                  触发信号
//	备用屏          用户的滚回缓冲被安全放到一边，
//	                  事后原样交还
//	鼠标报告       点击和滚轮以转义序列到达
//	括号粘贴       粘贴的文本会被包裹起来送达，
//	                  这样就不会被当成按键读取
//
// 打开它们只需要四个 `printf`。关闭它们才是整个问题所在，因为打开它们的
// 那个进程，是世界上唯一知道该怎么关掉它们的东西——如果它死掉却没有关掉，
// 用户就会被留在一个 shell 里：没有 echo，没有行编辑，没有光标，鼠标选择
// 也坏掉了。知道这个办法的人，会敲一下 `reset`；大多数人则是直接关掉窗口。
//
// 所以这个文件讲的是阶段 01 的道理，只是这次指向了另一份资源。阶段 01 问
// 的是"Agent 死掉时子进程会发生什么"；这里问的是"**终端**会发生什么"，答
// 案必须一样：无论是正常路径、错误路径、恐慌，还是信号，终端都要被恢复。
//
// 由此得出的规则值得单独说一句，因为它会悄悄推翻一个在其他任何地方都正确
// 的习惯：
//
//	**一旦你进入了原始模式，os.Exit 和 log.Fatal 就是 bug。**
//
// 它们会跳过 defer 函数。三层调用之下的一句 `log.Fatalf("bad config")`——
// Go 里最平常不过的一行代码——现在会让用户的终端处于损坏状态，而且它打印
// 的错误信息根本看不见，因为备用屏这时候还开着。
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// 转义序列。这里统一分组，而不是散落各处，为的是让启用和禁用的清单能彼此
// 对照——终端最后落得损坏，最常见的原因就是：某个模式在一个地方被启用，禁
// 用它的代码却在另一处，而那处代码根本没被执行到。
const (
	altScreenOn  = "\x1b[?1049h"
	altScreenOff = "\x1b[?1049l"
	cursorHide   = "\x1b[?25l"
	cursorShow   = "\x1b[?25h"

	// 1000 = 报告按钮按下/释放。1006 = 用 SGR 编码报告它们。
	//
	// 两者都需要，1006 是重要的那个：原始 X10
	// 编码把坐标打包进一个单字节作为 `32 + column`，
	// 在 223 列停止工作。在一个现代宽终端上那不是
	// 边缘情况，它是屏幕的右半部分。
	mouseOn  = "\x1b[?1000h\x1b[?1006h"
	mouseOff = "\x1b[?1006l\x1b[?1000l"

	// 括号粘贴。没有它，粘贴的段落被传送得就像
	// 用户打了每个字符，这在一个键盘驱动的 UI 中
	// 意味着每个字符运行一个命令。
	pasteOn  = "\x1b[?2004h"
	pasteOff = "\x1b[?2004l"

	// 同步输出："在我说完成之前，先别画"。
	// 不认识这个序列的终端会直接忽略它，这就是为什么可以放心无条件发送它。而
	// 认识它的终端，则不会再显示画到一半的帧——也就是当一次重绘卡在两次写入中
	// 间时，那种看得见的画面撕裂。
	syncOn  = "\x1b[?2026h"
	syncOff = "\x1b[?2026l"

	clearLine  = "\x1b[K"
	cursorHome = "\x1b[H"
)

// 终端拥有变异的状态并知道怎样交回它。
type terminal struct {
	in    *os.File
	out   *os.File
	saved *savedState // 平台特定的；见 term_unix.go / term_windows.go

	resize     <-chan struct{}
	stopResize func()

	// mu 守护着 closed 这个标志。Close 可能同时从三个地方被触发——defer、信号
	// 处理器，还有 panic 路径——如果不加保护，其中两个就会同时通过检查，导致
	// 恢复动作跑两次，还会在一个早就不是那个状态的终端上调用 leaveRaw。
	// `go test -race` 能抓到这个问题；人类几乎抓不到，因为这得赶上关闭过程中
	// 恰好来一个信号才会触发。
	mu     sync.Mutex
	closed bool
}

// openTerminal 进入原始模式，并打开 UI 需要的一切。
// 顺序是故意安排的，Close 做的正是反向操作。备用屏排在最前面，这样如果之
// 后有任何一步失败，失败消息会打在用户马上就要拿回的那个屏幕上，而不是打
// 在他们的滚回缓冲区顶部。
func openTerminal(in, out *os.File) (*terminal, error) {
	saved, err := enterRaw(in, out)
	if err != nil {
		return nil, fmt.Errorf("could not enter raw mode: %w (is stdin a terminal?)", err)
	}
	t := &terminal{in: in, out: out, saved: saved}
	io.WriteString(out, altScreenOn+cursorHide+mouseOn+pasteOn)
	t.resize, t.stopResize = watchResize(out)
	return t, nil
}

// Close 会恢复一切，而且可以安全地被多次调用。
// 等幂性在这里不是什么装饰品。Close 会从 defer、信号处理器和 panic 路径
// 被调用，运气不好的时候，这三处会同时调用它：如果在一个早就已经翻篇的终
// 端上，第二次恢复又重新启用了光标，那就是一次看得见的画面故障；而对一个
// 已经关闭的 fd 发起第二次 ioctl，则会产生一个没有人会去看的错误。
func (t *terminal) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	if t.stopResize != nil {
		t.stopResize()
	}
	// 启用顺序的准确反向。
	io.WriteString(t.out, pasteOff+mouseOff+cursorShow+altScreenOff)
	return leaveRaw(t.in, t.out, t.saved)
}

// Size 返回当前的宽度和高度（单位是单元格），并带有一个可用的回退值。
//
// 80x24 就是这个回退值：偏小的错误尺寸，产生的只是一个局促的 UI；而偏大
// 的错误尺寸，会让每一行都换行，把整个帧弄花。需要猜测时，要往失败后果能
// 挽回的那个方向猜。
func (t *terminal) Size() (int, int) {
	w, h, err := termSize(t.out)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// Frame 写一整帧。
//
// 它故意不做两件事。它不清屏：在每一帧之前发一个 `\x1b[2J`，是造成闪烁的
// 经典原因，因为刷新一次，终端上真正需要变的其实没几处。相反它把光标归位，
// 并在重写每一行时把这一行擦除，所以每个单元格要么被覆盖，要么被明确清除——
// 任何一帧都不会是空白的。
//
// 并且它不逐行写。一个缓冲区，一个系统调用——包裹在同步输出标记里——这就是
// 重绘和一次看得见的从上到下扫屏之间的区别。
func (t *terminal) Frame(lines []string, w, h int) {
	io.WriteString(t.out, frameBytes(lines, w, h))
}

// frameBytes 构建一个完整的帧。从 Frame 里拆分出来，这样不需要终端也能测
// 试它——转义序列正是最容易出错的部分。
func frameBytes(lines []string, w, h int) string {
	var b strings.Builder
	b.Grow(w*h + 64)
	b.WriteString(syncOn)
	b.WriteString(cursorHome)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			// truncCols，而不是切片：溢出一列的行会自动换行，把下面的每一行都往下推
			// 一格，把一个无关痛痒的外观小 bug，变成一帧被毁掉的画面。
			b.WriteString(truncCols(lines[i], w))
		}
		b.WriteString(clearLine)
		if i < h-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString(syncOff)
	return b.String()
}

// ---------------------------------------------------------------------------
// 输入
// ---------------------------------------------------------------------------

// readLoop 把标准输入泵进一个通道。
// 这里用 goroutine 而不是读取超时（Unix 上的 VMIN/VTIME，或 Windows 上的
// 重叠读取），是因为事件循环必须同时等待三件事——输入、窗口大小变化和
// Escape 计时器——而通道上的 `select` 是唯一一种在两个平台上都一样的等待
// 方式。这里的阻塞读取，正是让别处的 select 得以成立的关键。
// 程序退出时它会故意泄漏：没有跨平台的办法能中断 stdin 上的阻塞读取，而
// 一个卡在系统调用里的 goroutine，在进程退出时不会有任何代价。假装并非如
// 此，就得造一个假的取消——它会在读取操作明明还占着 fd 的时候提前返回。
func readLoop(in *os.File) <-chan []byte {
	ch := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				ch <- b
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// ---------------------------------------------------------------------------
// 恢复契约
// ---------------------------------------------------------------------------

// withTerminal 负责在终端设置好的状态下运行 fn，并且无论 fn 怎么结束，都
// 会恢复终端。
//
// 四种退出方式，一个像样的 TUI 都要应付：
//
//	fn 正常返回        defer 运行
//	fn 返回一个错误    defer 运行，错误在恢复**之后**才打印
//	                    ——打在用户真正的屏幕上，他们能
//	                    看见、能复制，而不是打在即将
//	                    被丢弃的备用屏上
//	fn 发生 panic      defer 运行，然后 panic 被重新
//	                    触发，堆栈跟踪就落在了一个
//	                    能显示它的终端上
//	SIGINT / SIGTERM   处理器负责恢复终端，再把
//	                    信号重新发出去
//
// 最后一种要靠重新发出信号，而不是 os.Exit(130)。一个被 SIGTERM 杀死的进
// 程应该**报告**自己是被 SIGTERM 杀死的——它的父进程可能是一个 shell、一
// 个监督进程，也可能是一个会区分信号致死与非零退出码的测试宿主。把处理器
// 重置为默认，再给自己重新发一次信号——这样才能收拾干净，同时不必在死因上
// 撒谎。
func withTerminal(in, out *os.File, fn func(*terminal) error) (err error) {
	t, err := openTerminal(in, out)
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s, ok := <-sigs
		if !ok {
			return
		}
		t.Close()
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		if p, e := os.FindProcess(os.Getpid()); e == nil {
			_ = p.Signal(s)
		}
		// 如果重新发出信号没有成功（在 Windows 上它必然不会成功，因为这些信号是
		// 合成出来的，而不是真正传送的），就回退到用退出码——但必须等终端已经恢复
		// 之后才这么做。
		os.Exit(130)
	}()

	defer func() {
		signal.Stop(sigs)
		close(sigs)
		t.Close()
		if r := recover(); r != nil {
			// 在恢复终端之后，重新触发 panic。堆栈跟踪标注的是这一行，而不是最初
			// panic 发生的那一行，这是实打实的代价；但另一种做法——把正确的堆栈跟踪打
			// 印在一块转眼就会消失的备用屏上——更糟。
			panic(r)
		}
	}()

	return fn(t)
}
