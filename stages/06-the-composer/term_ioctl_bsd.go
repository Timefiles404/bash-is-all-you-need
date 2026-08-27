//go:build darwin || dragonfly || freebsd || netbsd || openbsd

// 正是这两个常数，逼着现存的每一个终端库，都得有像这个文件一样的东西。
// termios 是 POSIX 标准，这个 struct 本身是可移植的。但负责读写它的那两
// 个 ioctl **数字**就不是了：BSD 把它们叫做 TIOCGETA/TIOCSETA，而后来才
// 出现的 Linux，把它们叫做 TCGETS/TCSETS，用的还是不同的值。没有一个可移
// 植的名字，所以只能靠构建标签，或者在 runtime.GOOS 上做运行时判断——而运
// 行时判断会把一个 Linux 的常数编译进 macOS 二进制里，可 x/sys 在 macOS
// 上根本没有定义这个常数。
// 六行，写两遍，就是把这件事做对的全部代价。做错的代价，则是某一个平台上，
// 偏偏就有个 ioctl 会返回 ENOTTY。
package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
