//go:build windows

// Process-tree killing, Windows edition. Unchanged from stage 01 — each stage is
// a standalone snapshot, so the file travels with it.
//
// Windows has no process groups in the Unix sense and — this is the part that
// catches people — no reliable parent/child relationship either. A Windows
// process records the PID that created it, but that PID is not kept alive, is
// not inherited, and is recycled aggressively. So "walk the parent links and
// kill the subtree" is not merely racy on Windows, it is wrong: a PID that got
// recycled can make an unrelated process look like your grandchild, and you will
// kill somebody else's work.
//
// The correct primitive is a Job Object: a kernel container that processes are
// assigned to, that children inherit automatically, and that can be terminated
// as a unit. It is the same mechanism behind Windows containers and the reason
// TerminateJobObject is atomic — the kernel walks its own membership list, so
// nothing can fork its way out mid-walk.
//
// golang.org/x/sys/windows is used rather than hand-written syscall stubs. It is
// maintained by the Go team in the Go project's own repository, so it is about
// as close to "stdlib" as a non-stdlib dependency gets; the alternative is
// writing your own LazyDLL bindings, which is a worse version of the same code.
package main

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGroup owns one Job Object handle. Unlike the Unix version — where the
// "group" is just an integer the kernel already knows about — this is a real
// kernel object with a real handle that leaks if you forget to close it.
type procGroup struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

// newProcGroup creates the job object up front, before the process exists.
//
// The limit flag is the whole reason to configure the job at all:
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE means "when the last handle to this job
// closes, terminate everything still inside it". That gives us a safety net the
// Unix implementation simply does not have — if the agent process dies, crashes,
// or is killed with Task Manager, the kernel closes our handles for us and the
// runaway tree dies with it. On Unix the orphans would be re-parented to init
// and keep running.
//
// Two consequences a student must internalise:
//
//   - Close() on this type is not a passive cleanup. It kills. That is why
//     runBash's `defer g.Close()` is load-bearing, and why a command that
//     deliberately backgrounds a long-lived server (`nohup npm start &`) will
//     survive the tool call on Linux/macOS and NOT survive it on Windows. This
//     is a genuine behavioural difference between the two files, not an
//     oversight. For an agent, dying is arguably the better default: nothing
//     should outlive its tool call without the agent knowing.
//   - Not setting JOB_OBJECT_LIMIT_BREAKAWAY_OK (the default) is also deliberate.
//     Without it, a child that asks for CREATE_BREAKAWAY_FROM_JOB is refused by
//     the kernel and CreateProcess fails outright. Escape is denied by default,
//     which is what we want.
func newProcGroup() (*procGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	// SetInformationJobObject is a raw kernel call: it takes a pointer and a
	// length, and it is on us to make them agree. Passing the wrong size here is
	// how you get an ERROR_INVALID_PARAMETER that tells you nothing.
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job) // do not leak the job we just failed to configure
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &procGroup{job: job}, nil
}

// attach is a no-op on Windows, and the reason why is the most instructive thing
// in this file.
//
// On Unix, attach sets Setpgid, and the kernel puts the child in its own process
// group between fork() and exec() — before the child has executed a single
// instruction. Windows offers no equivalent: a process can only be assigned to a
// job once it exists, and CreateProcess gives you a process that is already
// running. See adopt() for what that costs us.
//
// (The CreationFlags field is left alone on purpose. CREATE_NEW_PROCESS_GROUP is
// about Ctrl+C/Ctrl+Break routing, not about containment, and setting it here
// would change how console signals reach the shell without helping us kill it.)
func (g *procGroup) attach(cmd *exec.Cmd) {}

// adopt assigns the freshly started process — and therefore every process it
// goes on to create — to the job.
//
// # The residual race, stated honestly
//
// There is a window between CreateProcess returning and AssignProcessToJobObject
// taking effect. If the shell manages to spawn a grandchild inside that window,
// the grandchild is NOT in the job, and kill() will not touch it. The window is
// microseconds and it takes a shell that starts a background process as its very
// first act to hit it, so in practice this does not fire — but "in practice this
// does not fire" is exactly the sentence people write above the bug they ship.
//
// The airtight fix is CREATE_SUSPENDED: start the process with its main thread
// suspended, assign the job while nothing can possibly run, then ResumeThread.
// Nothing escapes because nothing has executed. exec.Cmd makes this awkward
// rather than impossible: it exposes cmd.Process.Pid but not the main thread's
// handle, so resuming means taking a CreateToolhelp32Snapshot of all threads on
// the machine, filtering for ones owned by our PID, OpenThread on the survivor
// and ResumeThread — roughly sixty lines of Toolhelp code, plus the question of
// what to do if the process somehow has more than one thread before it starts.
// (os/exec deliberately does not support CREATE_SUSPENDED for this reason; a
// suspended process that Go's own reaper never resumes would hang cmd.Wait().)
//
// The trade taken here: accept the microsecond window, and say so out loud. If
// you are writing a sandbox rather than a teaching repo, write the Toolhelp
// version — or use CreateProcess directly instead of os/exec, where the flag and
// the thread handle are both right there in PROCESS_INFORMATION.
//
// One race that is NOT present, and often assumed to be: OpenProcess by PID
// looks like it could hit a recycled PID. It cannot, here. os.Process holds an
// open handle to the child from Start() until Wait()/Release(), and Windows will
// not recycle a PID while any handle to it remains open. The handle Go keeps for
// its own bookkeeping is what makes our lookup safe.
func (g *procGroup) adopt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("adopt called before Start")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("adopt called after Close")
	}

	// PROCESS_SET_QUOTA is the access right AssignProcessToJobObject actually
	// checks (a job is, formally, a quota-and-limit container); PROCESS_TERMINATE
	// is required alongside it. Asking for exactly these two rather than
	// PROCESS_ALL_ACCESS keeps this working under a restricted token.
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", cmd.Process.Pid, err)
	}
	// Our own handle is only needed for the assignment itself. The job keeps its
	// own reference to the process, so closing this changes nothing about
	// membership.
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		// The classic failure here is ERROR_ACCESS_DENIED because the process is
		// already in a job that forbids nesting. Nested jobs work on Windows 8 /
		// Server 2012 and later; on anything older, or under some CI runners and
		// container hosts that put every process in a locked-down job, this is
		// where containment is lost. runBash treats the error as non-fatal and
		// warns, which is the honest response: the command still runs, but the
		// timeout can now only kill the shell itself.
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// kill terminates every process in the job, atomically, from the kernel's side.
//
// This is strictly stronger than the Unix version in one respect: process-group
// membership on Unix can be changed by a process that calls setpgid() on itself,
// so a determined child can leave the group. Job membership is permanent — once
// assigned, a process cannot remove itself, and neither can anyone else.
//
// Exit code 1 is what the terminated processes will report. It is arbitrary; the
// only value to avoid is 259 (STILL_ACTIVE), which would make a dead process
// indistinguishable from a running one to any code using GetExitCodeProcess.
//
// Errors are swallowed for the same reason as on Unix: by the time you are
// calling kill(), the only recoveries left are things you would not want an
// agent to attempt automatically. Calling it twice, or on a job whose members
// have all already exited, is harmless — an empty job terminates fine.
func (g *procGroup) kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.job == 0 {
		return
	}
	_ = windows.TerminateJobObject(g.job, 1)
}

// Close releases the job handle — which, because of
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, also kills anything still inside it.
//
// Idempotent: the handle is zeroed under the lock before closing, so a second
// Close is a no-op and a kill() racing with Close cannot use a stale handle.
// That last point matters more than it looks. Windows recycles handle values
// eagerly, so calling TerminateJobObject on a closed handle is not a harmless
// error — it can land on whatever object happened to inherit that number.
func (g *procGroup) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	job := g.job
	g.job = 0
	if job == 0 {
		return nil
	}
	return windows.CloseHandle(job)
}

// processAlive reports whether a PID is a currently-running process. It exists
// for proc_test.go, which has to prove the grandchildren are gone rather than
// trust that kill() returned without complaint.
//
// Note what this does NOT use: GetExitCodeProcess. That is the obvious call and
// it has a famous trap — it reports STILL_ACTIVE (259) for a running process,
// but 259 is also a perfectly legal exit code, so a process that exits with 259
// looks eternally alive. WaitForSingleObject has no such ambiguity: a process
// handle becomes signalled exactly when the process terminates, so a zero
// timeout gives an unambiguous "already dead / still running" in one call.
//
// Windows caveat with no Unix equivalent: while any handle to a dead process
// remains open anywhere on the system, its PID is not recycled and OpenProcess
// on it still SUCCEEDS. So "OpenProcess worked" is not evidence of life — you
// must go on to ask whether the handle is signalled, which is precisely what the
// wait does.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		// ERROR_INVALID_PARAMETER means no such PID: dead and fully reclaimed.
		// ERROR_ACCESS_DENIED would mean alive but not ours — impossible for the
		// children of a process we started, so treating it as "gone" is safe here
		// and would be wrong in a general-purpose utility.
		return false
	}
	defer windows.CloseHandle(h)

	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	// WAIT_OBJECT_0 => the handle is signalled => the process has terminated.
	// WAIT_TIMEOUT  => not signalled => still running.
	return event != windows.WAIT_OBJECT_0
}
