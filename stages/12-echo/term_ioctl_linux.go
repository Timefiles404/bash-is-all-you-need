//go:build !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

// 这个文件为什么存在，见 term_ioctl_bsd.go。这一份是 Linux，以及照抄了它
// 那套编号的平台。
package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
