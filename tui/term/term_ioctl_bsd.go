//go:build darwin || dragonfly || freebsd || netbsd || openbsd

// termios is POSIX and the struct is portable; the ioctl *numbers* that read and
// write it are not, and there is no portable name. A runtime switch on
// runtime.GOOS would compile a Linux constant into a macOS binary, where x/sys
// does not define it — so it has to be a build tag.
package term

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
