// 阶段 06——终端，以及你一进入原始模式就背上的那份约定。
//
// TUI 从终端拿四样东西，而每一样都是**对一份不属于你的资源做全局修
// 改**：
//
//	原始模式      按键以字节而不是整行送来，而 Ctrl-C 也不再
//	              是信号
//	备用屏        用户的回滚缓冲被搁到一边，之后原封不动还回来
//	鼠标上报      点击和滚轮以转义序列的形式送来
//	括号粘贴模式  粘贴的文本带着包装送来，于是不会被当成按键读
//
// 打开它们是四条 `printf`。关掉它们才是全部的难处，因为这世上唯一知道
// 怎么关的，就是当初打开它们的那个进程——它要是没关就死了，用户就被扔
// 在一个 shell 面前：没有回显，没有行编辑，没有光标，鼠标选择也坏了。
// 知道该敲 `reset` 的人会去敲。大多数人直接关窗口。
//
// 所以这个文件就是阶段 01 那一课，换个资源再讲一遍。阶段 01 问的是
// "Agent 死了，子进程会怎么样"。这里问的是**终端**会怎么样，而答案必须
// 一样：正常路径要还，出错路径要还，panic 要还，收到信号也要还。
//
// 由此得出的那条规矩值得单独写一行，因为它会不声不响地废掉一批在别处
// 都对的习惯：
//
//	**一旦进了原始模式，os.Exit 和 log.Fatal 就都是 bug。**
//
// 它们会跳过 defer。三层之下一句 `log.Fatalf("bad config")`——Go 里最平
// 常不过的一行——现在会把用户的终端留在坏掉的状态，而它打出来的那条错
// 误信息你还看不见，因为备用屏还没退。
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

// 转义序列。集中放在这里而不是散开，是为了让开启和关闭两张单子能对着
// 看——终端被留在坏状态，最常见的路子就是某个模式在一处开了，关它的那
// 处却没跑到。
const (
	altScreenOn  = "\x1b[?1049h"
	altScreenOff = "\x1b[?1049l"
	cursorHide   = "\x1b[?25l"
	cursorShow   = "\x1b[?25h"

	// 1000 = 上报按钮的按下/松开。1006 = 用 SGR 编码来报。
	//
	// 两个都得开，而要紧的是 1006：最早的 X10 编码把坐标按 `32 + column`
	// 压进一个字节，到第 223 列就不灵了。在今天的宽终端上这不是边角情况，
	// 那是屏幕的右半边。
	mouseOn  = "\x1b[?1000h\x1b[?1006h"
	mouseOff = "\x1b[?1006l\x1b[?1000l"

	// 括号粘贴模式。没有它，粘进来的一整段就像用户一个字符一个字符敲出来
	// 的一样送到，而在键盘驱动的界面里，这意味着每个字符都执行一条命令。
	pasteOn  = "\x1b[?2004h"
	pasteOff = "\x1b[?2004l"

	// 同步输出："我没说画完之前，别画"。
	//
	// 不认识这段序列的终端会忽略它，所以无条件发出去是安全的。认识它的终
	// 端不再露出画到一半的帧——就是重绘卡在两次写之间时你能看见的撕裂。
	syncOn  = "\x1b[?2026h"
	syncOff = "\x1b[?2026l"

	clearLine  = "\x1b[K"
	cursorHome = "\x1b[H"
)

// terminal 握着那些被改动的状态，也知道怎么还回去。
type terminal struct {
	in    *os.File
	out   *os.File
	saved *savedState // 平台相关；见 term_unix.go / term_windows.go

	resize     <-chan struct{}
	stopResize func()

	// mu 守的是 closed。Close 同时有三条路能进来——defer、信号处理函数、
	// panic 路径——旗标不上锁的话，其中两条会一起溜过那个判断，把终端还
	// 原两遍，还会对早已翻篇的终端调 leaveRaw。`go test -race` 能抓到
	// 这个；人抓不到，因为它得在关停途中正好来一个信号。
	mu     sync.Mutex
	closed bool
}

// openTerminal 进入原始模式，并把界面要用的东西全打开。
//
// 这个顺序是刻意的，Close 走的就是它的倒序。备用屏第一个开，这样后面任
// 何一步失败，那条失败信息都打在用户马上就要拿回去的那块屏幕上，而不是
// 糊在他的回滚缓冲上。
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

// Close 把所有东西还原，而且可以放心调用多次。
//
// 幂等在这里不是装饰。Close 会从 defer 里被调、从信号处理函数里被调、从
// panic 路径里被调，运气差的时候三处一起调；第二次还原会在早已翻篇的终
// 端上重新点亮光标，那是肉眼可见的抽搐，而对着已关闭的 fd 再来一次
// ioctl，只会换来一条永远没人会读的错误。
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
	// 严格按开启的倒序来。
	io.WriteString(t.out, pasteOff+mouseOff+cursorShow+altScreenOff)
	return leaveRaw(t.in, t.out, t.saved)
}

// Size 返回当前的宽和高，以字符格为单位，并给一个能用的兜底值。
//
// 兜底选 80x24，是因为尺寸猜小了，界面顶多挤一点；猜大了，界面每行都会
// 折行，整帧就烂了。非猜不可的时候，往失败还能收拾的那个方向猜。
func (t *terminal) Size() (int, int) {
	w, h, err := termSize(t.out)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// Frame 写出一整帧。
//
// 有两件事它是有意不做的。它不清屏：每帧之前来一发 `\x1b[2J` 是闪烁的经
// 典成因，因为有那么一个刷新周期，终端上真的什么都没有。它改成把光标归
// 位，边重写边逐行擦，于是每个格子要么被覆盖、要么被明确清掉，任何一帧
// 都不会是空的。
//
// 它也不逐行写。一个缓冲区，一次系统调用——外面套上同步输出的标记——这
// 就是"重绘"和"看得见地从上往下扫一遍屏幕"之间的差别。
func (t *terminal) Frame(lines []string, w, h int) {
	io.WriteString(t.out, frameBytes(lines, w, h))
}

// frameBytes 拼出一整帧。从 Frame 里拆出来，是为了不用终端也能测——真正
// 会出错的地方就是那些转义序列。
func frameBytes(lines []string, w, h int) string {
	var b strings.Builder
	b.Grow(w*h + 64)
	b.WriteString(syncOn)
	b.WriteString(cursorHome)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			// 用 truncCols，不要切片：一行多出一列就会折行，把它下面
			// 每一行都往下推一格，于是外观上的小毛病变成整帧损坏。
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

// readLoop 把 stdin 泵进 channel。
//
// 用 goroutine 而不是读超时（Unix 上的 VMIN/VTIME，Windows 上的重叠读），
// 是因为事件循环得同时等三样东西——输入、改大小、还有 Escape 计时器——而
// 在 channel 上 `select` 是这件事唯一在两个平台上长得一样的形态。这里这
// 次阻塞读，正是别处那个 select 得以成立的前提。
//
// 程序退出时它是有意泄漏的：没有可移植的办法去打断 stdin 上的阻塞读，而
// 进程退出时有个 goroutine 停在系统调用里，不花什么代价。假装不是这样，
// 就得写一个假的取消——读还攥着 fd，它就先返回了。
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
// 还原的约定
// ---------------------------------------------------------------------------

// withTerminal 在终端布置好之后跑 fn，并且不管 fn 怎么结束都把终端还回
// 去。
//
// 四种出口，真正的 TUI 四种都会遇上：
//
//	fn 正常返回      defer 会跑
//	fn 返回错误      defer 会跑，而且错误是在还原**之后**才打的
//	                 ——打在用户真正的屏幕上，在那儿他能看见、也
//	                 能复制，而不是打在一块马上就要被丢掉的备用
//	                 屏上
//	fn panic         defer 会跑，然后 panic 被重新抛出，于是栈回溯
//	                 落在一块能显示它的终端上
//	SIGINT / SIGTERM 处理函数先还原，再把这个信号重新发给自己
//
// 最后一种要的是重新抛信号，不是 os.Exit(130)。被 SIGTERM 杀掉的进程，就
// 该**报告**自己是被 SIGTERM 杀的——它的父进程可能是 shell、是某个
// supervisor、也可能是一套测试宿主，要区分"死于信号"和"非零退出"。把处
// 理函数重置回默认、再给自己补发一次那个信号，就是既清理干净、又不谎报
// 死因的做法。
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
		// 要是重新抛信号没生效（在 Windows 上就不可能生效，那里这些信号是合
		// 成出来的，不是真投递的），就退回到用退出码——但只能在终端已经还原
		// 之后。
		os.Exit(130)
	}()

	defer func() {
		signal.Stop(sigs)
		close(sigs)
		t.Close()
		if r := recover(); r != nil {
			// 还原之后再 panic 一次。栈回溯指到的是这一行，而不是原
			// 来那行，这个代价是实打实的；但另一条路是正确的栈回溯
			// 打在一块转眼就没的备用屏上，那更糟。
			panic(r)
		}
	}()

	return fn(t)
}
