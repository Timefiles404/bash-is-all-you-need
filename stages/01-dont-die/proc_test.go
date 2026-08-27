package main

// procGroup 的测试——阶段 01 里必须为真、而不是听起来像真的那部分。
//
// 杀进程树这件事最诱人的写法，是写个测试调一下 kill()，然后断言它
// 没返回错误。这种测试对着什么都不做的实现照样能过，漏孤儿进程的
// Agent 就是这么带着一片绿的 CI 发出去的。所以这些测试只做真正能
// 证明点什么的事：起真的后台进程，拿到它们真的 PID，然后去问操作
// 系统这些 PID 还在不在。
//
// fixture 的形状是刻意的。`bash -c` 里面的 `sleep 300 &` 造出的是
// **孙子进程**：我们的子进程是那个 shell，sleep 是 shell 的子进程。
// 天真的超时就是栽在这层关系上——shell 可以先退出，孙子进程照样跑
// 着，照样把 stdout 管道开着。
//
// 这个文件没有 build tag：它必须在哪儿都能编过。它需要的两件平台
// 相关的事——"这个 PID 还活着吗"和"我说的到底是哪个 PID"——分别由
// processAlive（定义在 proc_unix.go 和 proc_windows.go 里）和下面
// 那段 shell 脚本负责。

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grandchildScript 起两个后台 sleep，各打印一个 PID，然后卡在
// `wait` 上，好让 shell 自己活着，把整棵树撑开。
//
// 那句 `cat /proc/$p/winpid` 不是装饰——它是整个阶段里最难缠的一处
// 可移植性细节。
//
// Windows 上跑的是 Git Bash，也就是 MSYS2，它在 Windows PID 之上另
// 外维护了一套自己的 POSIX PID 命名空间。`$!` 给回来的是 MSYS pid
// （比如 48908），而真正的 Windows 进程 pid 完全是另一个数（比如
// 56176）。把 MSYS pid 交给 OpenProcess 不会失败——它会一声不响地去
// 查那个数字碰巧归了谁，而那是个跟你毫无关系的 Windows 进程。所以
// 只靠 `$!` 写出来的测试，在 Windows 上看着是过了，其实什么都没证
// 明；靠它写出来的 kill，杀的是无辜路人。
//
// MSYS2 把这层换算摆在 /proc/<pid>/winpid。真正的 Unix 上没有这个
// 路径，cat 会失败，`|| echo $p` 就退回到那边本来就对的 pid。一行
// 代码，两个世界，而且出事时是一声响亮的解析错误，不是一句无声的
// 谎。
const grandchildScript = `
sleep 300 & p=$!; cat /proc/$p/winpid 2>/dev/null || echo $p
sleep 300 & p=$!; cat /proc/$p/winpid 2>/dev/null || echo $p
wait
`

// startTree 在 procGroup 底下把 fixture 拉起来，等两个孙子进程都跑
// 起来、都确认活着，才返回。
func startTree(t *testing.T) (*procGroup, *exec.Cmd, []int) {
	t.Helper()

	shell, err := findBash()
	if err != nil {
		// 机器上没有 bash，不等于这台机器有毛病。跳过，才能让这套测试
		// 在光秃秃的 Windows 容器或者最小化的 Linux 镜像上还是诚实的，
		// 而不是为一件代码没干的事报一次红。
		t.Skipf("no POSIX shell on this machine: %v", err)
	}

	g, err := newProcGroup()
	if err != nil {
		t.Fatalf("newProcGroup: %v", err)
	}

	cmd := exec.Command(shell, "-c", grandchildScript)
	cmd.Stdin = nil

	// 用 StdoutPipe 而不是 bytes.Buffer，因为得在命令还在跑的时候把
	// PID 读出来——这条命令自己是不会结束的。
	//
	// Stderr 故意留成 nil（丢弃）：一挂上缓冲区，os/exec 就会起一个
	// 拷贝用的 goroutine，cmd.Wait() 随后要等它，而这个 goroutine 要
	// 等到攥着管道的进程全都没了才会结束。这正是本阶段要讲的那个死
	// 锁，在测试宿主里把它复现出来，无非是把测试挂住。
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		g.Close()
		t.Fatalf("StdoutPipe: %v", err)
	}

	g.attach(cmd) // 必须排在 Start 前面：Unix 上就是靠这句设 Setpgid
	if err := cmd.Start(); err != nil {
		g.Close()
		t.Fatalf("start %s: %v", shell, err)
	}
	if err := g.adopt(cmd); err != nil {
		// runBash 把这个降级成警告，因为有命令在跑总比没有强。测试不能
		// 这么办：圈不圈得住，正是这里要考的。
		g.kill()
		g.Close()
		t.Fatalf("adopt: %v", err)
	}

	// 在 goroutine 里读行，这样 shell 就算一句话不说也卡不死测试——不
	// 带截止时间的阻塞读，正是我们要测的那个 bug。
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

	// 先立基线。没有这一步，下面就算证明了"这些 PID 都死了"，也什么
	// 都没证明——它们可能压根没活过，每一条断言都会空洞地通过。
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

// stillAlive 一直轮询，直到每个 pid 都没了、或者预算用完，然后把剩
// 下的返回。用轮询而不是查一次，是因为进程的死在哪儿都是异步的：
// SIGKILL 只是把进程标成"可以去死了"，而 TerminateJobObject 在内核
// 还没把成员拆完的时候就返回了。
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

// reap 把 shell 收走，免得测试进程在 Unix 上攒一堆僵尸；它自己也带
// 截止时间，理由还是那个：树没杀干净的时候，挂住的正是 Wait 这次调
// 用。
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

// cleanup 是这个文件对它所在机器的承诺：这里没有哪个测试会漏出进
// 程，包括那些整个目的就是制造漏出进程的测试。
//
// 顺序有讲究。kill() 两个平台都管用；Close() 只在 Windows 上会杀
// （靠 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE），而它在那儿做的也不过
// 是放掉句柄。先杀，Unix 那条路才也被盖住。
func cleanup(g *procGroup, cmd *exec.Cmd) {
	g.kill()
	g.Close()
	reap(cmd)
}

// TestProcGroupKillsWholeTree 就是阶段 01 立下的断言：一次 kill()
// 调用，这条命令造出来的进程一个不剩。
func TestProcGroupKillsWholeTree(t *testing.T) {
	g, cmd, pids := startTree(t)
	defer cleanup(g, cmd)

	g.kill()

	if alive := stillAlive(pids, 5*time.Second); len(alive) != 0 {
		t.Fatalf("orphans survived kill(): %v — the process tree escaped", alive)
	}
	t.Logf("all grandchildren %v are gone after kill()", pids)
}

// TestProcGroupKillingOnlyTheShellLeavesOrphans 是对照实验，两个里
// 头它更值钱。
//
// 它只改了一处——用 cmd.Process.Kill() 换掉 g.kill()——然后断言
// **相反**的结果。往后要是有人把 procGroup 掏空成
// cmd.Process.Kill() 的一层壳，上面那个测试照样是绿的，理由是"进程
// 反正是死了"；这个测试会变红，因为这两条路本来就该表现不同。
//
// 注意两个测试里圈住的方式一模一样：进程组 / job object 的搭法完全
// 相同。差别只在这次 kill 瞄的是哪个句柄。这就是全部的教训——操作
// 系统把工具递给你了，还伸手去够 cmd.Process，就是那个错。
func TestProcGroupKillingOnlyTheShellLeavesOrphans(t *testing.T) {
	g, cmd, pids := startTree(t)
	// 注册在断言前面，这样测试半途失败，活下来的那些也照样被清掉。
	// 演示进程泄漏的测试，自己不能真把进程漏出去。
	defer cleanup(g, cmd)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the shell: %v", err)
	}

	// 给这个天真的 kill 充分的机会。孙子进程要是真会跟父进程一起死，
	// 2 秒绰绰有余；而 Unix 上它们只会被过继给 init，Windows 上更是
	// 压根没有东西把它们和 shell 连在一起。
	if alive := stillAlive(pids, 2*time.Second); len(alive) != len(pids) {
		t.Fatalf("expected all %d grandchildren to survive killing only the shell, but only %v did — "+
			"if this platform really does cascade the kill, this test needs rethinking, not deleting",
			len(pids), alive)
	}
	t.Logf("as expected, grandchildren %v outlived the shell (pid %d) — this is the orphan bug",
		pids, cmd.Process.Pid)

	// 现在证明同一批幸存者是可以通过进程组杀掉的——这既是修法，也是
	// 善后。
	g.kill()
	if alive := stillAlive(pids, 5*time.Second); len(alive) != 0 {
		t.Fatalf("group kill failed to collect the orphans: %v", alive)
	}
	t.Logf("group kill collected the orphans %v", pids)
}

// TestProcGroupIdempotentKillAndClose 把 runBash 依赖的那份契约钉死。
//
// runBash 超时时调 kill()，又在 defer 里调 Close()，所以超时的时候
// 两个都会跑，命令跑得快的时候只跑 Close()。两个都不许 panic，进程
// 已经死了的时候两个也都不许干出吓人的事——落到 Windows 上就是：
// Close() 不能把句柄关第二遍，那个句柄号内核可能已经发给别人了。
func TestProcGroupIdempotentKillAndClose(t *testing.T) {
	g, err := newProcGroup()
	if err != nil {
		t.Fatalf("newProcGroup: %v", err)
	}

	// 什么都还没起就调 kill()。Unix 上这才是危险的那种：pgid 还是零，
	// 而 kill(-0, SIGKILL) 的意思是"给我自己的进程组发信号"——测试程序
	// 会把自己杀掉。哪天这一行开始把整轮跑都拖下水、把测试套件搞挂了，
	// 那说明退化的就是那道防护。
	g.kill()

	if err := g.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
	g.kill() // Close 之后：必须什么都不做，而不是 use-after-free
}
