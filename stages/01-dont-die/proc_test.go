package main

// Tests for procGroup — the part of stage 01 that has to be true, not plausible.
//
// The temptation with process-tree killing is to write a test that calls kill()
// and asserts it returned no error. That test passes against an implementation
// that does nothing at all, which is how orphan-leaking agents get shipped with
// green CI. So these tests do the only thing that actually proves anything: they
// start real background processes, learn their real PIDs, and then ask the
// operating system whether those PIDs still exist.
//
// The shape of the fixture is deliberate. `sleep 300 &` inside `bash -c` creates
// a GRANDCHILD: our child is the shell, and the sleep is the shell's child. That
// is the exact relationship that breaks naive timeouts — the shell can exit
// while the grandchild keeps running and keeps the stdout pipe open.
//
// This file carries no build tag: it must compile everywhere. The two
// platform-specific things it needs — "is this PID alive?" and "which PID am I
// even talking about?" — are handled by processAlive (defined in proc_unix.go
// and proc_windows.go) and by the shell script below.

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grandchildScript starts two background sleeps, prints a PID for each, then
// blocks in `wait` so the shell itself stays alive and holds the tree open.
//
// The `cat /proc/$p/winpid` is not decoration — it is the single nastiest
// portability detail in this whole stage.
//
// On Windows we run Git Bash, which is MSYS2, which maintains its own POSIX PID
// namespace layered on top of Windows PIDs. `$!` returns the MSYS pid (e.g.
// 48908) and the actual Windows process has a completely different pid (e.g.
// 56176). Handing the MSYS pid to OpenProcess does not fail — it silently
// queries whichever unrelated Windows process happens to own that number. A test
// built on `$!` alone would therefore appear to pass on Windows while proving
// nothing, and a kill built on it would murder a bystander.
//
// MSYS2 exposes the translation at /proc/<pid>/winpid. On a real Unix that path
// does not exist, cat fails, and `|| echo $p` falls back to the pid that is
// already correct there. One line, both worlds, and the failure mode is a loud
// parse error rather than a quiet lie.
const grandchildScript = `
sleep 300 & p=$!; cat /proc/$p/winpid 2>/dev/null || echo $p
sleep 300 & p=$!; cat /proc/$p/winpid 2>/dev/null || echo $p
wait
`

// startTree launches the fixture under a procGroup and returns once both
// grandchildren are running and confirmed alive.
func startTree(t *testing.T) (*procGroup, *exec.Cmd, []int) {
	t.Helper()

	shell, err := findBash()
	if err != nil {
		// A machine without bash is not a failing machine. Skipping keeps this
		// suite honest on a bare Windows container or a minimal Linux image,
		// instead of reporting a red build for something the code did not do.
		t.Skipf("no POSIX shell on this machine: %v", err)
	}

	g, err := newProcGroup()
	if err != nil {
		t.Fatalf("newProcGroup: %v", err)
	}

	cmd := exec.Command(shell, "-c", grandchildScript)
	cmd.Stdin = nil

	// StdoutPipe rather than a bytes.Buffer, because we must read the PIDs while
	// the command is still running — this command never finishes on its own.
	//
	// Stderr is left nil (discarded) on purpose: attaching a buffer would make
	// os/exec spawn a copying goroutine that cmd.Wait() then waits on, and that
	// goroutine only finishes when every process holding the pipe is gone. That
	// is precisely the deadlock this stage exists to explain, and reproducing it
	// inside the test harness would just hang the test.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		g.Close()
		t.Fatalf("StdoutPipe: %v", err)
	}

	g.attach(cmd) // must precede Start: on Unix this is what sets Setpgid
	if err := cmd.Start(); err != nil {
		g.Close()
		t.Fatalf("start %s: %v", shell, err)
	}
	if err := g.adopt(cmd); err != nil {
		// runBash downgrades this to a warning because a running command is
		// better than no command. A test must not: containment is the subject.
		g.kill()
		g.Close()
		t.Fatalf("adopt: %v", err)
	}

	// Read lines on a goroutine so a shell that never speaks cannot wedge the
	// test — a blocking read with no deadline is the same bug we are testing for.
	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	var pids []int
	deadline := time.After(20 * time.Second)
	for len(pids) < 2 {
		select {
		case line, ok := <-lines:
			if !ok {
				g.kill()
				g.Close()
				t.Fatalf("shell closed stdout after only %d pids", len(pids))
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				g.kill()
				g.Close()
				t.Fatalf("unparseable pid line %q: %v", line, err)
			}
			pids = append(pids, pid)
		case <-deadline:
			g.kill()
			g.Close()
			t.Fatalf("timed out waiting for grandchild pids (got %d)", len(pids))
		}
	}

	// Establish the baseline. Without this, a test that proves "the PIDs are
	// dead" proves nothing — they might never have been alive, and every
	// assertion below would pass vacuously.
	for _, pid := range pids {
		if !processAlive(pid) {
			g.kill()
			g.Close()
			t.Fatalf("grandchild %d was never alive; the fixture is broken", pid)
		}
	}
	t.Logf("shell pid=%d, live grandchildren=%v", cmd.Process.Pid, pids)
	return g, cmd, pids
}

// stillAlive polls until every pid is gone or the budget runs out, and returns
// whatever is left. Polling rather than checking once, because process death is
// asynchronous everywhere: SIGKILL only marks a process runnable-to-die, and
// TerminateJobObject returns before the kernel has finished tearing members down.
func stillAlive(pids []int, within time.Duration) []int {
	deadline := time.Now().Add(within)
	for {
		var alive []int
		for _, pid := range pids {
			if processAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reap collects the shell so the test process does not accumulate zombies on
// Unix, with its own deadline for the usual reason: Wait is exactly the call
// that hangs when a tree was killed incompletely.
func reap(cmd *exec.Cmd) {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// cleanup is the promise this file makes to the machine it runs on: no test in
// here leaks a process, including the tests whose entire point is to create
// leaked processes.
//
// Order matters. kill() works on both platforms; Close() only kills on Windows
// (via JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) and merely frees the handle there.
// Killing first means the Unix path is covered too.
func cleanup(g *procGroup, cmd *exec.Cmd) {
	g.kill()
	g.Close()
	reap(cmd)
}

// TestProcGroupKillsWholeTree is the claim stage 01 makes: one kill() call, and
// every process the command created is gone.
func TestProcGroupKillsWholeTree(t *testing.T) {
	g, cmd, pids := startTree(t)
	defer cleanup(g, cmd)

	g.kill()

	if alive := stillAlive(pids, 5*time.Second); len(alive) != 0 {
		t.Fatalf("orphans survived kill(): %v — the process tree escaped", alive)
	}
	t.Logf("all grandchildren %v are gone after kill()", pids)
}

// TestProcGroupKillingOnlyTheShellLeavesOrphans is the control experiment, and
// it is the more valuable of the two.
//
// It changes exactly one thing — cmd.Process.Kill() instead of g.kill() — and
// asserts the OPPOSITE outcome. If someone later guts procGroup into a wrapper
// around cmd.Process.Kill(), the test above would still be green on the theory
// that "the processes died somehow"; this one goes red, because the two paths
// are supposed to behave differently.
//
// Note that the containment is identical in both tests: the process group / job
// object is set up exactly the same way. The difference is only in which handle
// the kill is aimed at. That is the whole lesson — the OS gives you the tool,
// and reaching for cmd.Process anyway is the mistake.
func TestProcGroupKillingOnlyTheShellLeavesOrphans(t *testing.T) {
	g, cmd, pids := startTree(t)
	// Registered before the assertions so the survivors are cleaned up even if
	// the test fails midway. A test that demonstrates leaking processes must not
	// actually leak them.
	defer cleanup(g, cmd)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the shell: %v", err)
	}

	// Give the naive kill every chance to work. If the grandchildren were going
	// to die with their parent, 2s is ample; on Unix they are instead re-parented
	// to init, and on Windows nothing links them to the shell at all.
	if alive := stillAlive(pids, 2*time.Second); len(alive) != len(pids) {
		t.Fatalf("expected all %d grandchildren to survive killing only the shell, but only %v did — "+
			"if this platform really does cascade the kill, this test needs rethinking, not deleting",
			len(pids), alive)
	}
	t.Logf("as expected, grandchildren %v outlived the shell (pid %d) — this is the orphan bug",
		pids, cmd.Process.Pid)

	// Now prove the same survivors are killable through the group, which is both
	// the fix and the cleanup.
	g.kill()
	if alive := stillAlive(pids, 5*time.Second); len(alive) != 0 {
		t.Fatalf("group kill failed to collect the orphans: %v", alive)
	}
	t.Logf("group kill collected the orphans %v", pids)
}

// TestProcGroupIdempotentKillAndClose pins down the contract runBash relies on.
//
// runBash calls kill() on timeout and Close() from a defer, so on a timeout both
// run, and on a fast command only Close() does. Neither may panic, and neither
// may do anything alarming when the process is already dead — which on Windows
// means Close() must not double-close a handle whose number the kernel has
// already handed to somebody else.
func TestProcGroupIdempotentKillAndClose(t *testing.T) {
	g, err := newProcGroup()
	if err != nil {
		t.Fatalf("newProcGroup: %v", err)
	}

	// kill() before anything was ever started. On Unix this is the dangerous
	// case: the pgid is still zero, and kill(-0, SIGKILL) means "signal my own
	// process group" — the test binary would kill itself. If this line ever
	// starts failing the suite by taking the whole run down with it, that guard
	// is what regressed.
	g.kill()

	if err := g.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
	g.kill() // after Close: must be a no-op, not a use-after-free
}
