//go:build !windows

// 杀进程树，Unix 版。从阶段 01 没有改变——每个阶段是独立
// 快照，所以文件与它一起旅行。
//
// 这解决的问题：`bash -c "npm start &"` 会立即退出，但后台化
// 的 npm 仍然保持运行，**并且**保持 stdout 管道打开。只杀死
// shell 的话，孙进程还会照样活着，cmd.Wait() 也会卡在一个
// 永远不会关闭的管道上。参见 main.go 中的 runBash。
//
// Unix 从 v7 以来就有了答案：进程组。每个进程属于一个，孩子
// 继承它，如果你给它一个负 PID，kill(2) 会给整个组发信号。
// 这样一来，整个作业只靠一个整数就能被杀掉，我们也永远不必
// 走进程树——这是好事，因为遍历一棵进程树，本质上就是竞速
// 的（一个进程可以在你枚举它的瞬间和你杀死它的瞬间之间
// 分叉）。
package main

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

// procGroup 是"shell 和它生成的所有东西"的句柄。
//
// 在 Unix 上，那个句柄只是一个数字，所以这个结构体看起来
// 几乎是空的。Windows 那边的文件则携带着一个真实的内核
// 对象；共享这套 API 的关键在于，main.go 根本看不出其中
// 的差别。
type procGroup struct {
	// runBash 在一个 goroutine 上调用 adopt()，并在超时分支里
	// 调用 kill()——目前这两者刚好是同一个 goroutine，但把一个
	// 杀死开关的可靠性建立在"目前"这个前提上，可不是什么好事；
	// Windows 那边的实现确实需要一把锁，来阻止 kill() 使用一个
	// Close() 已经释放掉的句柄。两个文件都守着同样的纪律，这样
	// 谁都不会慢慢漂移、变成不安全的那一个。
	mu sync.Mutex

	// 进程组 ID，同时也是 shell 的 PID。它在 adopt() 时就被
	// 缓存下来，而不是等到 kill() 时才从 cmd.Process 读取，
	// 这样 kill() 就永远不会跟那个守在 cmd.Wait() 里的
	// goroutine，并发地去碰 exec.Cmd 的状态。
	pgid int
}

// newProcGroup 在 Unix 上什么都不分配：这个组由内核创建，
// 是 fork() 的一个副作用，所以事先没有什么要设置的，也
// 没有什么能失败的。错误返回的存在，是为了 Windows 那边
// 的实现——在那里，创建工作对象是一次真正的系统调用，
// 真的可能会失败。
func newProcGroup() (*procGroup, error) {
	return &procGroup{}, nil
}

// attach 必须在 cmd.Start() **之前**调用。
//
// Setpgid 告诉 Go 运行时在子进程里调用 setpgid(0, 0)，就在
// fork() 和 exec() 之间的那个窗口期。正是这个时序，让 Unix
// 做到了万无一失，而 Windows 做不到：子进程在它的 shell
// 代码第一条指令运行之前，就已经在自己的组里了，所以根本
// 不存在这样一个空档——让孙进程被生成到我们的组里，而
// 不是它自己的组里。
//
// Pgid 被留在零，意味着"让子进程成为一个全新进程组的组长，
// 组 ID 等于它自己的 PID"。非零的 Pgid 会转而加入一个已有
// 的组——这里绝不能这么做，否则 kill(-pgid) 连 Agent 自己
// 也会一起干掉。
func (g *procGroup) attach(cmd *exec.Cmd) {
	// 保留呼叫者已设置的任何属性；我们仅要这一个位。
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// adopt 在 cmd.Start() 之后立刻被调用。在 Unix 上，内核已经
// 把活干完了，剩下的就只是记住我们拥有的是哪个组。
//
// 值得搞清楚，为什么这一步在这里是空操作，换到 Windows
// 上却是一次系统调用：Unix 允许父进程在子进程**存在之前**
// 就为它声明好进程组，而 Windows 那边的等价做法（分配一个
// 工作对象）只能对一个已经在运行的进程去做。那个单一的
// 差别，就是 Windows 实现会有竞争、而这边不会的全部原因。
func (g *procGroup) adopt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		// 跟 Windows 实现给出的是同一种错误，这样一来，把调用
		// 顺序搞错的呼叫者，会在自己开发时用的那个平台上就发现
		// 问题，而不是等到部署的平台上才发现。
		return fmt.Errorf("adopt called before Start")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pgid = cmd.Process.Pid
	return nil
}

// kill 用 SIGKILL 向整个组发信号。
//
// 负 PID 就是整个技巧所在：kill(-N, sig) 的意思是"把 sig
// 发给组 N 里的每一个进程"。那些在 shell 死掉时被过继给
// init 的子孙进程，依然留在这个组里，所以照样会被杀死——
// 组成员关系是通过 fork 继承下来的，组长死了这层归属关系
// 也不会消失，而这正是一个"孤儿杀手"需要的特性。
//
// 选 SIGKILL 而不是 SIGTERM，是因为"shell 命令会无视
// SIGTERM"根本不是纸上谈兵（随便一个带"优雅关机"逻辑、
// 结果却卡死的程序就是例子），而且这一步本来就是 Agent
// 的最后手段，是超时到期之后才会走到的。更宽容一点的
// 设计会先发 SIGTERM，等一秒，再发 SIGKILL；这么改是
// 合理的，但前提是你得有办法半途放弃这次等待——不然就
// 等于重新引入了那个你本来想逃开的挂起。
//
// 错误被故意吞掉。实践中唯一会出现的是 ESRCH（"那个组里
// 什么都不剩了"——如果命令是自己结束的，这就是理想情况）
// 和 EPERM。这两种情况都没有什么有用的补救办法，而且
// kill() 的文档写明了：对一个已经死掉的进程调用它是安全的。
func (g *procGroup) kill() {
	g.mu.Lock()
	pgid := g.pgid
	g.mu.Unlock()

	if pgid <= 0 {
		// adopt() 根本没运行过，或者进程压根没启动。杀死组 0 就
		// 意味着"我自己的组"——也就是说 Agent 会把自己杀掉。
		// 拒绝执行。
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// Close 释放资源。在 Unix 上，没有资源可释放。
//
// 唯一值得注意的是 Close **不**做什么：它不会杀掉任何
// 东西。在 Windows 上，工作对象被配置成：当最后一个句柄
// 关闭时，就杀掉它的所有成员，所以那边的 `defer g.Close()`
// 也顺带成了一张安全网——就算 Agent 自己崩溃了，也不会
// 泄漏一整棵失控的进程树。Unix 没有对应的机制——如果
// Agent 死掉，它的子进程会继续在 init 下运行。这种不
// 对称是真实存在的，这也是为什么 runBash 会明确地调用
// kill()，而不是依赖 Close。
func (g *procGroup) Close() error {
	return nil
}

// processAlive 报告一个 PID 当前是否存在。它被 proc_test.go
// 使用，proc_test.go 必须证明孙进程真的消失了，而不是
// 轻信 kill() 老老实实地返回、没有报错。
//
// 信号 0 是标准的存在性探针：内核会执行它的权限检查和
// 存在性检查，然后不递送任何东西。一个 nil 错误就意味着
// 这个 PID 还存在。
//
// 有个警告学生应该知道：遇到僵尸进程时，它也同样会返回
// true——僵尸进程是指已经死掉、但退出状态还没人收集的
// 进程。在我们杀掉一个进程组之后，孤儿进程会被过继给
// init（PID 1），在任何正常系统上，init 都会很快把它们
// 回收掉。但如果某个容器的 PID 1 是一个不会回收子进程的
// 应用，它们就可能无限期地以僵尸进程的样子留在那里。那
// 是容器配置的 bug，不是这里的 bug，但它会让这个测试变得
// 不稳定，所以测试用轮询、而不是只检查一次。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}
