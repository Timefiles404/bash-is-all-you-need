//go:build windows

// 原始模式，Windows 版本。
// 契约和 term_unix.go 一样，但除此之外几乎没有别的共同点了。三处差异改变
// 的是设计本身，而不只是代码：
//  1. 没有 termios。这里有两个**控制台模式**，一个给输入句柄，一个给输出
//     句柄，而且它们是位标志，不是 struct——所以"恢复"意味着要记住两个
//     uint32。
//  2. ANSI 在两端都是选择性开启的。输入句柄必须被告知把按键编码成转义序
//     列（ENABLE_VIRTUAL_TERMINAL_INPUT），输出句柄必须被告知去解释这些
//     转义序列（ENABLE_VIRTUAL_TERMINAL_PROCESSING）。少了后者，TUI 就会
//     把自己的转义码当文本打印出来——这是"我的 Go TUI 在 Windows 上坏了"
//     这类反馈里最常见的一种，而修复它只需要一次 API 调用。
//  3. **没有 SIGWINCH。** 没有什么会主动告诉你窗口大小变了。Win32 的做法
//     是从控制台输入队列里读取 WINDOW_BUFFER_SIZE_EVENT 记录——但正是
//     ENABLE_VIRTUAL_TERMINAL_INPUT 把这个队列变成了字节流，一旦你选择接
//     收字节，就不会再收到记录。所以这个文件采用轮询。这不是图省事的捷径；
//     这是选择了 VT 路径之后唯一剩下的办法，而 VT 路径正是让键盘在
//     Windows 上表现得和其他平台一样的那条路径。
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

	// 输入：去掉线纪律，加上 VT 编码。
	// ENABLE_PROCESSED_INPUT 是 Windows 版的 ISIG——开着它，Ctrl-C 会触发一个
	// 控制台控制事件，而不是作为字节到达。
	newIn := oldIn
	newIn &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT
	newIn |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT

	// Quick Edit 是没有人会想到的那一个。它默认是开着的，会让鼠标去选取文本，
	// 而不是把点击传给应用程序；要关掉它，得在同一次调用里设置
	// ENABLE_EXTENDED_FLAGS——不带上这个标志就去清除 QUICK_EDIT，控制台会悄悄
	// 地无视你。一个"在 Windows 上收不到点击"的鼠标 UI，通常就是踩了这个坑，
	// 没有别的原因。
	newIn |= windows.ENABLE_EXTENDED_FLAGS
	newIn &^= windows.ENABLE_QUICK_EDIT_MODE

	if err := windows.SetConsoleMode(inH, newIn); err != nil {
		return nil, err
	}

	// 输出：解释 ANSI，并且在最后一列不再自动换行。
	// DISABLE_NEWLINE_AUTO_RETURN 真正要解决的，是右下角那个单元格：在那里写
	// 一个字符，正常情况下会让屏幕向上滚动一行，而在备用屏上，这意味着 UI 刚
	// 画好的那一帧会向上滑出原位。
	newOut := oldOut | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING |
		windows.ENABLE_PROCESSED_OUTPUT | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(outH, newOut); err != nil {
		// 输入句柄这时已经被改掉了。一定要在返回之前把它改回来——不然这里一旦失败，
		// 用户就会被留在一个既没有 echo、也没有任何解释的处境里，而这恰恰是这整
		// 个文件存在的目的所要阻止的结果。
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

// termSize 读取控制台屏幕缓冲区信息。
// 注意它用的是**窗口**矩形，不是缓冲区的 Size。这两者在 Windows 上是不同
// 的，而这种区别在 Unix 上从来不会出现：一个控制台可以有一个 9,000 行的
// 滚回缓冲，藏在一个 80x25 的窗口后面，Size 描述的是缓冲区。去量缓冲区的
// 大小，UI 就会想把 9,000 行画进一个只有 25 行的窟窿里。
func termSize(out *os.File) (int, int, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(out.Fd()), &info); err != nil {
		return 0, 0, err
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	h := int(info.Window.Bottom-info.Window.Top) + 1
	return w, h, nil
}

// watchResize 轮询，因为没有其他办法通知我们。
//
// 一秒四次：快得足以让调整大小感觉即时，慢得足以让空闲的 UI 不会显著运行。
// 靠的是和上一次观察到的尺寸做比较，这个事件才能像它的 Unix 对应物一样保
// 持边缘触发，所以事件循环分不出两者——这正是共享签名的意义所在。
//
// 诚实地说说这个差异：在 Unix 上调整大小的代价是零，直到它发生；这里的
// 代价是每 250ms 一次系统调用，永远如此。这是 VT 输入路径的代价，值得
// 付出，因为另一种选择是一个独立的 Windows 按键解码器，以及这个平台上
// 一套不同的 bug——而这恰恰是用户最不可能自己诊断出问题的平台。
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
