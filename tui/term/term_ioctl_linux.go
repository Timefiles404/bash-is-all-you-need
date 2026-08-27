//go:build !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

// See term_ioctl_bsd.go for why this file exists. Linux, and the platforms that
// copied its numbering.
package term

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
