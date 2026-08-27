//go:build !windows

package term

// OwnConsole reports whether this process is the only one attached to its
// console. On Unix the process that owns the terminal is the shell that launched
// us, so nothing vanishes when we return; the answer is always false.
func OwnConsole() bool { return false }
