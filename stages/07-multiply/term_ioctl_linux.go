//go:build !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

// 这个文件为什么存在，见 term_ioctl_bsd.go。Linux，以及所有照抄它编号方
// 式的平台。
package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
