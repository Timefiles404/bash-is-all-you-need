//go:build windows

package term

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// OwnConsole reports whether this process is the only one attached to its
// console — which on Windows is what "the user double-clicked the exe" looks
// like from the inside, and means the window disappears the moment we return.
func OwnConsole() bool {
	if err := procGetConsoleProcessList.Find(); err != nil {
		return false
	}
	// Eight is plenty: the answer we act on is "exactly one", and any larger
	// count means the same thing whether it is 2 or 200.
	pids := make([]uint32, 8)
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	// Zero is the failure return. An unknown answer must be the answer that
	// changes no behaviour.
	if n == 0 {
		return false
	}
	return n == 1
}
