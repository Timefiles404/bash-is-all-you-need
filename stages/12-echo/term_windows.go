//go:build windows

// 原始模式，Windows 版。
//
// 约定和 term_unix.go 一样，除此之外几乎没有共同点。有三处差别改的是设
// 计，而不只是代码：
//
//  1. 没有 termios。有的是两份**控制台模式**，输入句柄一份、输出句柄一
//     份，而且它们是位标志，不是结构体——所以"还原"就是记住两个 uint32。
//
//  2. ANSI 两头都要显式打开。得告诉输入句柄把按键编码成转义序列
//     （ENABLE_VIRTUAL_TERMINAL_INPUT），也得告诉输出句柄去解释它们
//     （ENABLE_VIRTUAL_TERMINAL_PROCESSING）。少了第二条，TUI 就会把自
//     己的转义码当文本打出来——这是"我的 Go TUI 在 Windows 上坏了"这类
//     报告里最常见的一种，而它只差一次 API 调用。
//
//  3. **没有 SIGWINCH。** 窗口被改了大小，没有任何东西会告诉你。Win32
//     的路子是从控制台输入队列里读 WINDOW_BUFFER_SIZE_EVENT 记录——可
//     ENABLE_VIRTUAL_TERMINAL_INPUT 干的正是把那个队列变成字节流，既然
//     要了字节，就再也拿不到记录了。所以这个文件靠轮询。这不是走捷径，
//     这是选了 VT 这条路之后你手上剩下的东西，而 VT 这条路，才是让键盘
//     的行为跟别的平台一致的那条。
package main

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

type savedState struct {
	in, out uint32
}

func enterRaw(in, out *os.File) (*savedState, error) {
	inH := windows.Handle(in.Fd())
	outH := windows.Handle(out.Fd())

	var oldIn, oldOut uint32
	if err := windows.GetConsoleMode(inH, &oldIn); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(outH, &oldOut); err != nil {
		return nil, err
	}

	// 输入：把行规程剥掉，加上 VT 编码。
	//
	// ENABLE_PROCESSED_INPUT 就是 Windows 上的 ISIG——开着它，Ctrl-C 会触发
	// 一个控制台控制事件，而不是以字节的形式送来。
	newIn := oldIn
	newIn &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT
	newIn |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT

	// Quick Edit 是谁也没想到的那个。它默认开着，它让鼠标去选文本而不是把
	// 事件送到应用里，而要关掉它，必须在同一次调用里设上
	// ENABLE_EXTENDED_FLAGS——不带它就去清 QUICK_EDIT，控制台会无视你，一声
	// 不响。鼠标界面"在 Windows 上收不到点击"，通常就是这个，没别的。
	newIn |= windows.ENABLE_EXTENDED_FLAGS
	newIn &^= windows.ENABLE_QUICK_EDIT_MODE

	if err := windows.SetConsoleMode(inH, newIn); err != nil {
		return nil, err
	}

	// 输出：解释 ANSI，并且到最后一列时别再自动折行。
	//
	// DISABLE_NEWLINE_AUTO_RETURN 关乎的是右下角那个格子。往那儿写一个字符，
	// 正常情况下会把屏幕往上滚一行，而在备用屏上，这意味着界面刚画好的那一
	// 帧整个往上错位。
	newOut := oldOut | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING |
		windows.ENABLE_PROCESSED_OUTPUT | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(outH, newOut); err != nil {
		// 输入句柄已经改过了。返回之前先把它放回去，否则这里一失败，用户就
		// 落得既没有回显也没有解释——而整份文件存在的意义，正是为了挡住这个
		// 结局。
		windows.SetConsoleMode(inH, oldIn)
		return nil, err
	}
	return &savedState{in: oldIn, out: oldOut}, nil
}

func leaveRaw(in, out *os.File, s *savedState) error {
	if s == nil {
		return nil
	}
	err := windows.SetConsoleMode(windows.Handle(in.Fd()), s.in)
	if err2 := windows.SetConsoleMode(windows.Handle(out.Fd()), s.out); err == nil {
		err = err2
	}
	return err
}

// termSize 读的是控制台屏幕缓冲区信息。
//
// 注意它取的是**窗口**矩形，不是缓冲区的 Size。这两者在 Windows 上会不一
// 样，而在 Unix 上从来不会：控制台可以在 80x25 的窗口后面挂着 9,000 行的
// 回滚缓冲，而 Size 描述的是缓冲区。量了缓冲区，界面就会把 9,000 行画进
// 一个 25 行的坑里。
func termSize(out *os.File) (int, int, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(out.Fd()), &info); err != nil {
		return 0, 0, err
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	h := int(info.Window.Bottom-info.Window.Top) + 1
	return w, h, nil
}

// watchResize 靠轮询，因为没人会来告诉我们。
//
// 一秒四次：快到让改大小感觉是即时的，慢到界面闲着时测不出它在跑。跟上
// 一次看到的尺寸做比较，是这东西能像 Unix 那边一样保持边沿触发的原因，
// 于是事件循环分不出这两者——而共用同一个签名，图的就是这个。
//
// 老实说，这个差别是：在 Unix 上，改大小之前不花任何代价；在这儿，它每
// 250ms 就要花一次系统调用，永远如此。这是走 VT 输入这条路的价钱，而且
// 值得付，因为另一条路是单独写一个 Windows 按键解码器，再在用户最不可
// 能诊断出问题的那个平台上，收获另一批 bug。
func watchResize(out *os.File) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		lastW, lastH, _ := termSize(out)
		for {
			select {
			case <-t.C:
				w, h, err := termSize(out)
				if err != nil || (w == lastW && h == lastH) {
					continue
				}
				lastW, lastH = w, h
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return ch, func() { close(done) }
}
