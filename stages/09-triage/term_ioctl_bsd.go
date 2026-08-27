//go:build darwin || dragonfly || freebsd || netbsd || openbsd

// The two constants that force every terminal library in existence to have a
// file like this one.
//
// termios is POSIX and the struct is portable. The ioctl *numbers* that read
// and write it are not: BSD called them TIOCGETA/TIOCSETA and Linux, arriving
// later, called them TCGETS/TCSETS with different values. There is no portable
// name, so the choice is a build tag or a runtime switch on runtime.GOOS — and
// a runtime switch would compile a Linux constant into a macOS binary, where
// x/sys does not even define it.
//
// Six lines, twice, is the whole cost of getting this right. The cost of
// getting it wrong is an ioctl that returns ENOTTY on exactly one platform.
package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
