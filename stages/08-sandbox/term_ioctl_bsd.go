//go:build darwin || dragonfly || freebsd || netbsd || openbsd

// 就是这两个常量，逼得世上每一个终端库都得有一份这样的文件。
//
// termios 是 POSIX 的，结构体也是可移植的。读写它的那两个 ioctl **号**不
// 是：BSD 管它们叫 TIOCGETA/TIOCSETA，后来的 Linux 叫它们 TCGETS/TCSETS，
// 取值还不一样。没有可移植的名字，所以只能二选一：build tag，或者在
// runtime.GOOS 上做运行时判断——而运行时判断会把 Linux 常量编进 macOS
// 的二进制里，x/sys 在那儿根本没定义过它。
//
// 六行，写两遍，这就是把它做对的全部代价。做错的代价，是 ioctl 偏偏
// 只在一个平台上返回 ENOTTY。
package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
