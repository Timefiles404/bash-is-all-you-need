//go:build !windows

// Process-tree killing, Unix edition.
//
// The problem this solves: `bash -c "npm start &"` exits immediately, but the
// backgrounded npm keeps running *and* keeps the stdout pipe open. Killing only
// the shell leaves the grandchild alive and leaves cmd.Wait() blocked on a pipe
// that will never close. See runBash in main.go.
//
// Unix has had the answer since v7: a process group. Every process belongs to
// one, children inherit it, and kill(2) will signal an entire group if you pass
// it a negative PID. So the whole job becomes killable through a single integer,
// and we never have to walk a process tree — which is good, because walking one
// is inherently racy (a process can fork between the moment you enumerate it and
// the moment you kill it).
package main

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

// procGroup is a handle on "the shell and everything it spawned".
//
// On Unix that handle is just a number, so this struct looks almost empty. The
// Windows file carries a real kernel object; the point of the shared API is that
// main.go cannot tell the difference.
type procGroup struct {
	// runBash calls adopt() on one goroutine and kill() from the timeout branch,
	// which is the same goroutine today — but "today" is a bad thing to build a
	// kill switch on, and the Windows implementation genuinely needs a lock to
	// stop kill() using a handle Close() already released. Same discipline in
	// both files, so neither can drift into being the unsafe one.
	mu sync.Mutex

	// The process-group ID, which is also the shell's PID. Cached at adopt()
	// rather than read from cmd.Process at kill() time, so that kill() never
	// touches exec.Cmd state concurrently with the goroutine sitting in
	// cmd.Wait().
	pgid int
}

// newProcGroup allocates nothing on Unix: the group is created by the kernel as
// a side effect of fork(), so there is nothing to set up in advance and nothing
// that can fail. The error return exists for the Windows implementation, where
// creating the job object is a real syscall that really can fail.
func newProcGroup() (*procGroup, error) {
	return &procGroup{}, nil
}

// attach must be called BEFORE cmd.Start().
//
// Setpgid tells the Go runtime to call setpgid(0, 0) in the child, in the
// window between fork() and exec(). That timing is what makes Unix airtight and
// Windows not: the child is already in its own group before its first
// instruction of shell code runs, so there is no interval in which a grandchild
// could be spawned into our group instead of its own.
//
// Pgid is left at zero, which means "make the child the leader of a brand new
// group whose ID equals its PID". A non-zero Pgid would join an existing group
// instead — never do that here, or kill(-pgid) would take out the agent too.
func (g *procGroup) attach(cmd *exec.Cmd) {
	// Preserve any attributes a caller already set; we only want this one bit.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// adopt is called immediately after cmd.Start(). On Unix the kernel has already
// done the work, so all that is left is to remember which group we own.
//
// It is worth understanding why this is a no-op here and a syscall on Windows:
// Unix lets a parent declare a child's group *before the child exists*, whereas
// the Windows equivalent (assigning a job object) can only be done to a process
// that is already running. That single difference is the entire reason the
// Windows implementation has a race and this one does not.
func (g *procGroup) adopt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		// Same error as the Windows implementation gives, so a caller that gets
		// the ordering wrong finds out on whichever platform they develop on
		// rather than the one they deploy to.
		return fmt.Errorf("adopt called before Start")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pgid = cmd.Process.Pid
	return nil
}

// kill signals the whole group with SIGKILL.
//
// The negative PID is the entire trick: kill(-N, sig) means "deliver sig to
// every process in group N". Descendants that were re-parented to init when the
// shell died are still in the group, so they still die — group membership is
// inherited through fork and survives the death of the leader, which is exactly
// the property an orphan-killer needs.
//
// SIGKILL, not SIGTERM, because a shell command that ignores SIGTERM is not a
// hypothetical (any program with a "graceful shutdown" handler that hangs) and
// this call is the agent's last resort, already reached after the timeout
// expired. A more forgiving design sends SIGTERM, waits a second, then SIGKILL;
// that is a reasonable change to make, but only once you have a way to abandon
// the wait, or you have re-introduced the hang you were escaping.
//
// Errors are deliberately swallowed. The only ones that occur in practice are
// ESRCH ("nothing left in that group" — the happy case if the command finished
// on its own) and EPERM. There is no useful recovery for either, and kill() is
// documented as safe to call on an already-dead process.
func (g *procGroup) kill() {
	g.mu.Lock()
	pgid := g.pgid
	g.mu.Unlock()

	if pgid <= 0 {
		// adopt() never ran, or the process never started. Killing group 0 would
		// mean "my own group" — i.e. the agent would kill itself. Refuse.
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// Close releases resources. There are none on Unix.
//
// The one thing worth noting is what Close does NOT do: it does not kill
// anything. On Windows the job object is configured to kill its members when the
// last handle closes, so `defer g.Close()` there is also a safety net against
// leaking a runaway tree if the agent itself crashes. Unix has no equivalent —
// if the agent dies, its children keep running under init. That asymmetry is
// real, and it is why runBash calls kill() explicitly instead of relying on
// Close.
func (g *procGroup) Close() error {
	return nil
}

// processAlive reports whether a PID currently exists. It is used by
// proc_test.go, which has to prove that grandchildren are gone rather than trust
// that kill() returned without complaint.
//
// Signal 0 is the standard existence probe: the kernel runs its permission and
// existence checks and then delivers nothing. A nil error means the PID is
// there.
//
// Caveat a student should know: this also returns true for a zombie — a process
// that has died but whose exit status nobody has collected yet. After we kill a
// process group, the orphans are re-parented to init (PID 1), which reaps them
// promptly on any normal system. Inside a container whose PID 1 is an
// application that does not reap, they can stay visible as zombies indefinitely.
// That is a container-configuration bug, not a bug here, but it will make this
// test flake, so the test polls rather than checking once.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}
