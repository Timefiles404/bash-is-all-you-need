//go:build !windows

// 杀进程树，Unix 版。
//
// 它解决的问题：`bash -c "npm start &"` 立刻就退了，可被扔到后台的
// npm 还在跑，**而且**还开着 stdout 管道。只杀 shell，孙子进程照样
// 活着，cmd.Wait() 也就卡在一根永远不会关的管道上。见 main.go 里的
// runBash。
//
// Unix 从 v7 起就有答案了：进程组。每个进程都属于某个组，子进程继
// 承它，而 kill(2) 只要拿到一个负 PID，就会给整个组发信号。于是整
// 份活儿靠一个整数就能杀掉，我们也永远不用去走进程树——这是好事，
// 因为走树天生就有竞争：从你把进程枚举出来，到你杀它，中间它可以
// fork。
package main

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

// procGroup 是"这个 shell 和它生出来的一切"的句柄。
//
// Unix 上这个句柄就是个数字，所以这个结构体看着几乎是空的。Windows
// 那个文件里装的是真正的内核对象；共用同一套 API 的意义就在于，
// main.go 分不出这两者的差别。
type procGroup struct {
	// runBash 在一个 goroutine 上调 adopt()，又在超时分支里调 kill()，
	// 今天这两处是同一个 goroutine——但"今天"这种东西不该拿来当急停开
	// 关的地基，而且 Windows 那边是真的需要一把锁，拦住 kill() 去用
	// Close() 已经放掉的句柄。两个文件守同样的规矩，谁也别悄悄漂成不
	// 安全的那个。
	mu sync.Mutex

	// 进程组 ID，也就是 shell 的 PID。在 adopt() 的时候缓存下来，而不
	// 是等 kill() 的时候去读 cmd.Process，这样 kill() 就永远不会跟坐在
	// cmd.Wait() 里的那个 goroutine 同时去碰 exec.Cmd 的状态。
	pgid int
}

// newProcGroup 在 Unix 上什么也不分配：组是内核在 fork() 的副作用里
// 建的，所以既没有什么要提前搭好，也没有什么会失败。那个 error 返回
// 值是给 Windows 实现留的——在那边，创建 job object 是一次真的系统调
// 用，是真的会失败。
func newProcGroup() (*procGroup, error) {
	return &procGroup{}, nil
}

// attach 必须在 cmd.Start() **之前**调用。
//
// Setpgid 是在告诉 Go runtime：在子进程里、在 fork() 和 exec() 之间
// 那个窗口里，调一次 setpgid(0, 0)。Unix 严丝合缝而 Windows 不行，
// 差的就是这个时机：shell 代码的第一条指令还没跑，子进程就已经在自
// 己的组里了，于是根本没有哪个间隙，能让孙子进程落进我们的组，而不
// 是它自己的组。
//
// Pgid 留在零，意思是"让这个子进程去当组长，开一个全新的组，组 ID
// 就等于它的 PID"。非零的 Pgid 会去加入已有的组——这里千万别那么干，
// 否则 kill(-pgid) 会把 Agent 一起带走。
func (g *procGroup) attach(cmd *exec.Cmd) {
	// 调用方已经设过的属性都留着，我们要的只是这一位。
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// adopt 紧跟在 cmd.Start() 后面调用。Unix 上内核已经把活干完了，剩
// 下的只是记住我们拥有的是哪个组。
//
// 值得弄明白它为什么在这儿是空操作、在 Windows 上却是一次系统调用：
// Unix 允许父进程**在子进程存在之前**就声明它的组，而 Windows 的对
// 应做法（往 job object 里分配）只能对已经在跑的进程做。就这一处差
// 别，就是 Windows 那版有竞争、这版没有的全部原因。
func (g *procGroup) adopt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		// 和 Windows 实现给的是同一个错误，这样调用方把顺序搞反了，是在
		// 自己开发用的平台上发现，而不是在部署的那台上。
		return fmt.Errorf("adopt called before Start")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pgid = cmd.Process.Pid
	return nil
}

// kill 用 SIGKILL 给整个组发信号。
//
// 负 PID 就是全部的窍门：kill(-N, sig) 的意思是"把 sig 送给 N 组里
// 的每个进程"。shell 死的时候被过继给 init 的那些后代，仍然在这个组
// 里，所以照样会死——组的归属随 fork 继承，组长死了它也还在，而这正
// 是杀孤儿的东西需要的性质。
//
// 用 SIGKILL 不用 SIGTERM，因为 shell 命令无视 SIGTERM 不是什么假想
// 的事（任何带"优雅退出"处理函数又挂住的程序都算），而这次调用是
// Agent 最后的手段，走到这一步说明超时已经到了。更宽容的设计是先发
// SIGTERM，等一秒，再 SIGKILL；这个改法讲得通，但前提是你先有办法
// 放弃这次等待，否则你就把自己正想逃开的那个挂死又请回来了。
//
// 错误是故意吞掉的。实际会出现的只有 ESRCH（"那个组里什么都不剩了"
// ——命令自己跑完的话，这是好事）和 EPERM。这两种都没有什么有用的补
// 救，而 kill() 的文档也写明了：对已经死掉的进程调用它是安全的。
func (g *procGroup) kill() {
	g.mu.Lock()
	pgid := g.pgid
	g.mu.Unlock()

	if pgid <= 0 {
		// adopt() 没跑过，或者进程压根没起来。杀 0 号组的意思是"我自己
		// 这个组"——也就是 Agent 会自杀。拒绝。
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// Close 释放资源。Unix 上没有资源可释放。
//
// 唯一值得一提的是 Close **不**做什么：它不杀任何东西。Windows 上
// job object 被配置成最后一个句柄关闭时就把成员全杀掉，所以那边的
// `defer g.Close()` 同时还是张安全网——万一 Agent 自己崩了，也不会
// 漏出一棵失控的树。Unix 没有对应的东西——Agent 一死，它的子进程就
// 挂在 init 底下接着跑。这个不对称是真实存在的，也正因为如此，
// runBash 才显式地调 kill()，而不是指望 Close。
func (g *procGroup) Close() error {
	return nil
}

// processAlive 报告某个 PID 眼下存不存在。用它的是 proc_test.go，那
// 边必须证明孙子进程真的没了，而不是相信 kill() 没抱怨就算数。
//
// 信号 0 是标准的存在性探针：内核会走一遍权限检查和存在性检查，然后
// 什么也不投递。error 为 nil 就说明 PID 还在。
//
// 有一处学生该知道的但书：僵尸进程也会让它返回 true——就是那种已经
// 死了、但退出状态还没人收走的进程。杀掉进程组之后，孤儿会被过继给
// init（PID 1），正常系统上 init 会立刻把它们收掉。而在某些容器里，
// PID 1 是个不收尸的应用程序，它们就能以僵尸的样子一直挂着。那是容
// 器配置的 bug，不是这里的 bug，但它会让这个测试变得不稳，所以测试
// 用的是轮询，而不是查一次。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}
