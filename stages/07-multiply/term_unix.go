//go:build !windows

// 原始模式，Unix 版本。
// 这个文件与 term_windows.go 的配对方式，跟阶段 01 的 proc_unix.go /
// proc_windows.go 是同一个形状：契约相同，机制完全不同，而这个差异不是什
// 么实现细节——它改变的是程序能被告知什么。
// 这里内核会通过发送 SIGWINCH 来告诉你窗口变了。在 Windows 上什么都不会
// 主动告诉你，你必须自己去问。正是这一处不对称，才让 watchResize 返回一
// 个通道，而不是直接暴露一个信号：通道既可以由信号处理器喂数据，也可以由
// 轮询循环喂数据，事件循环没法分辨自己收到的究竟是哪一种。
package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// savedState 就是终端设置在我们碰它之前的样子。整个恢复契约，就是这个
// struct，再加上"必须始终把它写回去"这一条纪律。
type savedState struct {
	termios unix.Termios
}

// enterRaw 把键盘和程序之间的每一条线纪律都关掉。
// 下面清除的每一个标志，对应的都是一项原本由终端提供、现在必须由 TUI 自
// 己来提供的服务；之所以一次只清一个、而不是直接赋值一个零值 struct，是
// 因为零值 struct 会连那些不只影响行为、还关系到正确性的标志一起清掉——比
// 如字符位数、奇偶校验——而一个被设成 5-bit 字符的终端，就是一个有趣的下
// 午。
func enterRaw(in, out *os.File) (*savedState, error) {
	fd := int(in.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	raw := *old

	// 输入：停止翻译。ICRNL 是最让人栽跟头的那个——开着它，Enter 键会以 \n 的
	// 形式到达，没法把它和粘贴内容里的字面换行符区分开。IXON 是另一个：不清
	// 除它，Ctrl-S 会冻结终端，Ctrl-Q 才能解冻——一个按下 Ctrl-S、指望它能"保
	// 存"的用户，只会得出结论：程序卡死了。
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON

	// 输出：往外走的方向也停止翻译。OPOST 打开时，单独的 \n 会变成 \r\n，悄
	// 悄破坏任何按列定位光标的帧。
	raw.Oflag &^= unix.OPOST

	// 本地：没有 echo，没有行缓冲，以及——最要命的一条——没有信号。正是清除
	// ISIG，才让 Ctrl-C 以字节 0x03 的形式到达，而不是作为 SIGINT。这是 TUI
	// 想要的行为，而这也是程序自己开始要对"给用户留一条出路"负责的那一刻：这
	// 一行之后，键处理器里但凡有一个 bug，就会变成一个用户没法退出的程序。
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN

	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8

	// 阻塞，直到至少收到一个字节为止。事件循环是在它自己的 goroutine 里读取
	// 的（见 readLoop），所以阻塞在那里不花任何代价；而且比起 VTIME 那种以十
	// 分之一秒为单位的计时器，阻塞读取也远远更容易推理——何况那种精度，对于这
	// 个 UI 需要的 ~50ms Escape 消歧来说也太粗了。
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return nil, err
	}
	return &savedState{termios: *old}, nil
}

func leaveRaw(in, out *os.File, s *savedState) error {
	if s == nil {
		return nil
	}
	t := s.termios
	return unix.IoctlSetTermios(int(in.Fd()), ioctlSetTermios, &t)
}

// termSize 问的是内核，不是环境变量。
// $COLUMNS 和 $LINES 确实存在，但那是个陷阱：这两个变量是 shell 为自己设
// 置的，而恰好在窗口大小改变的那一刻——也就是你最需要读它们的时候——它们的
// 值早就过期了。
func termSize(out *os.File) (int, int, error) {
	ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// watchResize 每次窗口改变大小时传送一个令牌。
//
// 通道容量为 1，满了就丢弃而不是阻塞。拖动窗口边缘时，每移动一个像素行就
// 会产生一个 SIGWINCH，但它们的意思都一样——"大小现在不一样了，去问问现在
// 是多少"。把这些通知排队，只会让 UI 在拖拽停止之后，把上百帧过时的画面
// 重新画一遍。这里的合并不是什么优化手段，而是边沿触发通知本该有的正确语
// 义——反正这个值你迟早都要去读一次。
func watchResize(out *os.File) (<-chan struct{}, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGWINCH)
	ch := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sigs:
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return ch, func() {
		signal.Stop(sigs)
		close(done)
	}
}
