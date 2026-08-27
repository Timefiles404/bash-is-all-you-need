//go:build !windows

// 原始模式，Unix 版。
//
// 它和 term_windows.go 配成一对，形状跟阶段 01 的 proc_unix.go /
// proc_windows.go 一样：约定完全相同，机制完全不同，而这个不同不是实现
// 细节——它改变了程序能被告知什么。
//
// 在这边，窗口变了内核会告诉你，靠发 SIGWINCH。在 Windows 上没人告
// 诉你，你得自己去问。就因为这一处不对称，watchResize 返回的是 channel，
// 而不是把信号露出去：channel 既可以由信号处理函数喂，也可以由轮询循环
// 喂，而事件循环分不出自己拿到的是哪一种。
package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// savedState 是我们动手之前终端的那份设置。整套还原约定，就是这个结构体，
// 加上"总把它写回去"的纪律。
type savedState struct {
	termios unix.Termios
}

// enterRaw 把键盘和程序之间的行规程一件件全关掉。
//
// 下面清掉的每一个标志位，都是终端本来在替你提供的一项服务，而 TUI 得
// 自己把它提供出来。之所以一个一个地清，而不是直接赋一个清零的结构体，
// 是因为清零的结构体连那些关乎正确性而不是行为的位也一起清了——字符位
// 宽、校验位——而一台被设成 5 位字符的终端，够你玩一下午。
func enterRaw(in, out *os.File) (*savedState, error) {
	fd := int(in.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	raw := *old

	// 输入：别再做转换。ICRNL 是最多人栽进去的那个——开着它，Enter 键送到
	// 时就是 \n，跟粘贴内容里真正的换行完全分不出来。IXON 是另一个：不清
	// 掉它，Ctrl-S 会冻住终端、Ctrl-Q 再解冻，而用户按 Ctrl-S 本来是想
	// "保存"，结果只会认定程序挂了。
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON

	// 输出：出去的方向也别再做转换。OPOST 开着的话，单独一个 \n 会变成
	// \r\n，于是任何靠列来定位光标的帧都会被悄悄搞坏。
	raw.Oflag &^= unix.OPOST

	// 本地：不回显、不做行缓冲，还有——最要命的那个——不发信号。清掉 ISIG，
	// Ctrl-C 才会以字节 0x03 的形式送来，而不是变成 SIGINT。这正是 TUI 想
	// 要的行为，也正是从这一刻起，程序得自己负责留一条退路：这一行往后，
	// 按键处理里的 bug，就是用户退不出去的程序。
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN

	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8

	// 阻塞到至少来一个字节为止。事件循环是在自己的 goroutine 上读的（见
	// readLoop），所以在那儿阻塞不花什么代价；而且阻塞读比 VTIME 那个以十
	// 分之一秒为单位的计时器好想得多——后者对这个界面需要的约 50ms Escape
	// 消歧来说，也太粗了。
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
//
// $COLUMNS 和 $LINES 确实存在，而且是个陷阱：shell 设它们是给自己用的，
// 窗口一改大小它们立刻就过期了——而你需要它们，恰恰就是这种时候。
func termSize(out *os.File) (int, int, error) {
	ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// 窗口每改一次大小，watchResize 就投一枚 token 出来。
//
// channel 容量为 1，满了就丢，不阻塞。拖窗口边缘时，鼠标每移动一个像素
// 行就来一个 SIGWINCH，而它们说的是同一件事——"现在尺寸不一样了，自己去
// 问是多少"。把它们排成队，只会让界面在拖动停下之后再重画一百帧过期的
// 画面。这里的合并不是优化；这种通知是边沿触发的，而那个值反正得自己去
// 读，对它来说，合并就是正确的语义。
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
