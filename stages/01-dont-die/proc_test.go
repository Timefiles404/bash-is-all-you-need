package main

// procGroup 的测试——阶段 01 里必须为真、而不是看起来
// 可信就行的那部分。
//
// 用进程树杀死时，诱惑在于：写一个调用 kill() 的测试，
// 然后断言它返回时没有错误。那个测试，对一个什么都不做
// 的实现也能通过——这正是孤儿泄漏的 Agent 能顶着绿色
// CI 一起发布出去的原因。所以这些测试做的是唯一真正能
// 证明点什么的事：它们启动真实的后台进程，取得它们
// 真实的 PID，然后问操作系统那些 PID 是否仍然存在。
//
// fixture 的形状是故意的。`sleep 300 &` 在 `bash -c`
// 里面创建一个**孙进程**：我们的子进程是 shell，sleep 是
// shell 的子进程。那正是会让天真的超时机制失效的那种
// 关系——shell 可以退出，孙进程却继续运行，还占着
// stdout 管道不放。
//
// 这个文件没有 build tag：它必须到处都能编译。它需要的
// 两个平台特定的东西——"这个 PID 还活着吗？"和"我说的
// 到底是哪个 PID？"——由 processAlive 处理（定义在
// proc_unix.go 和 proc_windows.go 中）和下面的 shell
// 脚本一起搞定。

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grandchildScript 启动两个后台 sleep，为每个打印
// 一个 PID，然后在 `wait` 中阻塞，所以 shell 自己活着
// 并保持这棵树打开。
//
// `cat /proc/$p/winpid` 不是装饰——它是这整个
// 阶段中最恶劣的可移植性细节。
//
// 在 Windows 上我们运行 Git Bash，它是 MSYS2，它在
// Windows PID 上面维护它自己的 POSIX PID 命名空间。
// `$!` 返回 MSYS pid（比如 48908）而实际 Windows
// 进程有个完全不同的 pid（比如 56176）。把 MSYS pid
// 交给 OpenProcess 不会失败——它无声地查询恰巧拥有
// 那个数字的任何不相关 Windows 进程。一个仅在 `$!`
// 上建立的测试会因此看起来在 Windows 上通过，同时
// 什么都不证明，一个建立在它上面的杀死会谋杀旁观者。
//
// MSYS2 在 /proc/<pid>/winpid 暴露翻译。在真实
// Unix 那个路径不存在，cat 失败，`|| echo $p`
// 回落到那里已经正确的 pid。一行，两个世界，
// 失败模式是响亮的解析错误而不是无声的谎言。
const grandchildScript = `
sleep 300 & p=$!; cat /proc/$p/winpid 2>/dev/null || echo $p
sleep 300 & p=$!; cat /proc/$p/winpid 2>/dev/null || echo $p
wait
`

// startTree 在一个 procGroup 下启动 fixture，
// 并一旦两个孙进程运行并确认活着就返回。
func startTree(t *testing.T) (*procGroup, *exec.Cmd, []int) {
	t.Helper()

	shell, err := findBash()
	if err != nil {
		// 一台没有 bash 的机器，不是一台失败的机器。跳过测试
		// 能让这个套件在一个裸 Windows 容器或最小化的 Linux
		// 镜像上保持诚实，而不是为了代码压根没做过的事，
		// 去报告一次构建失败。
		t.Skipf("no POSIX shell on this machine: %v", err)
	}

	g, err := newProcGroup()
	if err != nil {
		t.Fatalf("newProcGroup: %v", err)
	}

	cmd := exec.Command(shell, "-c", grandchildScript)
	cmd.Stdin = nil

	// StdoutPipe 而不是 bytes.Buffer，因为我们必须在命令
	// 仍在运行时读 PID——这个命令永远不会自己结束。
	//
	// Stderr 被故意留成 nil（丢弃掉）：附加一个缓冲会让
	// os/exec 生成一个复制用的 goroutine，cmd.Wait() 之后
	// 会等它，而只有当每个还占着这根管道的进程都消失了，
	// 那个 goroutine 才会结束。那正是这一整个阶段想要说清楚
	// 的那个死锁，在测试宿主里面重现它，只会把测试本身
	// 也一起挂起。
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		g.Close()
		t.Fatalf("StdoutPipe: %v", err)
	}

	g.attach(cmd) // 必须在 Start 前：在 Unix 这是设置 Setpgid 的
	if err := cmd.Start(); err != nil {
		g.Close()
		t.Fatalf("start %s: %v", shell, err)
	}
	if err := g.adopt(cmd); err != nil {
		// runBash 把这个降级成一个警告，因为一个能运行的命令，
		// 比没有命令要好。但一个测试不能这样：包含性才是
		// 这里的主题。
		g.kill()
		g.Close()
		t.Fatalf("adopt: %v", err)
	}

	// 在一个 goroutine 上读取输出行，这样一个永不开口的
	// shell 就不能把测试卡住——一个没有期限的阻塞读取，
	// 正是我们正在测试的那同一个 bug。
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

	// 建立基线。没有这一步，一个证明"这些 PID 都死了"的
	// 测试，什么都证明不了——它们可能压根就没活过，下面的
	// 每一条断言，都会没有意义地"通过"。
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

// stillAlive 会一直轮询，直到每个 pid 都消失，或者预算
// 耗尽，然后返回剩下的那些。用轮询而不是只检查一次，
// 是因为进程的死亡在任何地方都是异步的：SIGKILL 只是
// 把一个进程标记为"可以被调度去死"，而 TerminateJobObject
// 会在内核真正把所有成员都清理干净之前，就先返回了。
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

// reap 会回收掉 shell，这样测试进程就不会在 Unix 上
// 积累僵尸；出于通常的那个原因，它也有自己的期限：
// Wait 正是那个会在一棵树没被杀干净时，整个挂起的调用。
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

// cleanup 是这个文件对它运行所在的机器做出的承诺：
// 这里没有任何测试会泄漏进程，包括那些整个意义就是要
// 制造出泄漏进程的测试。
//
// 顺序很重要。kill() 在两个平台上都能用；Close() 只在
// Windows 上杀（通过 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE），
// 在那里它做的其实只是释放句柄。先执行杀，就意味着连
// Unix 的路径也一并覆盖到了。
func cleanup(g *procGroup, cmd *exec.Cmd) {
	g.kill()
	g.Close()
	reap(cmd)
}

// TestProcGroupKillsWholeTree 是阶段 01 做出的声言：
// 一个 kill() 调用，每个命令创建的进程都消失。
func TestProcGroupKillsWholeTree(t *testing.T) {
	g, cmd, pids := startTree(t)
	defer cleanup(g, cmd)

	g.kill()

	if alive := stillAlive(pids, 5*time.Second); len(alive) != 0 {
		t.Fatalf("orphans survived kill(): %v — the process tree escaped", alive)
	}
	t.Logf("all grandchildren %v are gone after kill()", pids)
}

// TestProcGroupKillingOnlyTheShellLeavesOrphans 是
// 对照实验，它是两者中更有价值的那一个。
//
// 它只改变了一件事——用 cmd.Process.Kill() 代替
// g.kill()——并断言**相反**的结果。如果有人之后
// 把 procGroup 掏空成一个套在 cmd.Process.Kill() 外面
// 的包装，上面那个测试理论上仍然会是绿色的——按
// "进程反正是死了"这套说法；这一个则会变红，因为
// 两条路径本该表现不同。
//
// 注意，两个测试里的包含机制是完全相同的：进程组 /
// 工作对象的设置方式一模一样。差别只在于，这次杀，
// 瞄准的是哪个句柄。那就是全部的教训——操作系统把
// 工具给了你，你却还是伸手去够 cmd.Process，这就是
// 错误所在。
func TestProcGroupKillingOnlyTheShellLeavesOrphans(t *testing.T) {
	g, cmd, pids := startTree(t)
	// 注册在断言之前，这样一来，就算测试中途失败，幸存者
	// 也能被清理掉。一个用来演示"进程会泄漏"的测试，但
	// 不能真的把它们泄漏出去。
	defer cleanup(g, cmd)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the shell: %v", err)
	}

	// 给这次天真的杀，每一个奏效的机会。如果孙进程本来会
	// 跟着它们的父进程一起死，2s 充分；在 Unix 上，它们
	// 其实会被过继给 init，在 Windows 上，则根本没有什么
	// 把它们和 shell 连在一起。
	if alive := stillAlive(pids, 2*time.Second); len(alive) != len(pids) {
		t.Fatalf("expected all %d grandchildren to survive killing only the shell, but only %v did — "+
			"if this platform really does cascade the kill, this test needs rethinking, not deleting",
			len(pids), alive)
	}
	t.Logf("as expected, grandchildren %v outlived the shell (pid %d) — this is the orphan bug",
		pids, cmd.Process.Pid)

	// 现在证明，同一批幸存者能通过这个组被杀死——这既是
	// 修复，也是清理。
	g.kill()
	if alive := stillAlive(pids, 5*time.Second); len(alive) != 0 {
		t.Fatalf("group kill failed to collect the orphans: %v", alive)
	}
	t.Logf("group kill collected the orphans %v", pids)
}

// TestProcGroupIdempotentKillAndClose 锁定了 runBash
// 所依赖的契约。
//
// runBash 会在超时时调用 kill()，并从 defer 里调用
// Close()，所以超时的情况下两者都会运行，命令很快
// 结束时则只有 Close() 会运行。两者都不能 panic，
// 也都不能在进程已经死掉时做出什么吓人的事——这在
// Windows 上意味着，Close() 不能对一个编号已经被内核
// 转交给别人的句柄重复关闭。
func TestProcGroupIdempotentKillAndClose(t *testing.T) {
	g, err := newProcGroup()
	if err != nil {
		t.Fatalf("newProcGroup: %v", err)
	}

	// kill() 在什么都不曾启动前。在 Unix 这是危险的情况：
	// pgid 仍是零，kill(-0, SIGKILL) 意味着"向我自己的
	// 进程组发信号"——测试用的这个二进制会把自己杀掉。
	// 如果这一行有一天开始失败、把整个测试流程一起拖垮，
	// 那就说明发生回归的正是这道防线。
	g.kill()

	if err := g.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
	g.kill() // 在 Close 后：必须是无操作，不能是释放后用
}
