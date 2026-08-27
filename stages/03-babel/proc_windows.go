//go:build windows

// 杀进程树，Windows 版。和阶段 01 一样没改——每个阶段都是独立快照，
// 文件就跟着一起走。
//
// Windows 没有 Unix 意义上的进程组，而且——这一条最容易让人栽跟头——
// 父子关系也不可靠。Windows 进程会记下创建它的那个 PID，但那个 PID 不
// 会被 keep-alive，不会被继承，还会被激进地回收。所以"顺着父链走一遍，把子树
// 杀掉"在 Windows 上不只是有竞态，是错的：被回收的 PID 会让毫不相干的
// 进程看起来像你的孙进程，于是你杀掉的是别人的活儿。
//
// 对的原语是 Job Object：内核里的容器，进程被指派进去，子进程自动继承，
// 整体可以一次终止。Windows 容器背后是同一套机制，TerminateJobObject
// 是原子的也是因为它——内核走的是自己那份成员名单，所以谁也没法在它走
// 名单的中途 fork 出去。
//
// 用的是 golang.org/x/sys/windows，不是手写的 syscall 桩。它由 Go 团队
// 在 Go 项目自己的仓库里维护，所以非标准库的依赖能有多接近"标准库"，
// 它就有多接近；另一条路是自己写 LazyDLL 绑定，那是同一份代码更糟的
// 版本。
package main

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGroup 持有一个 Job Object 句柄。Unix 那版里的"组"不过是个内核
// 早就认识的整数，这里不一样：这是真正的内核对象，带着真正的句柄，
// 你忘了关它就会漏。
type procGroup struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

// newProcGroup 提前把 job object 建好，在进程存在之前。
//
// 要给这个 job 做配置，理由全在那个 limit 标志上：
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 的意思是"这个 job 的最后一个句
// 柄关掉时，把里面还剩的东西全部终止"。这给了我们一张安全网，Unix
// 实现根本没有这东西——Agent 进程退出也好、崩了也好、被任务管理器杀
// 掉也好，内核都会替我们关掉句柄，失控的那棵树跟着一起死。Unix 上这
// 些孤儿会被过继给 init，接着跑。
//
// 有两个后果学生必须吃透：
//
//   - 这个类型上的 Close() 不是被动的清理。它会杀。这就是为什么
//     runBash 里的 `defer g.Close()` 是承重的，也是为什么故意把长命
//     服务扔到后台的那种命令（`nohup npm start &`），在 Linux/macOS
//     上活得过这次工具调用，在 Windows 上**活不过**。这是两个文件之
//     间真实的行为差异，不是疏忽。对 Agent 来说，死掉大概才是更好的
//     默认：不该有东西在 Agent 不知情的情况下活过它那次工具调用。
//   - 不设 JOB_OBJECT_LIMIT_BREAKAWAY_OK（也就是默认）同样是有意的。
//     不设它，子进程要是申请 CREATE_BREAKAWAY_FROM_JOB，会被内核驳
//     回，CreateProcess 直接失败。逃跑默认就是不许，这正是我们要的。
func newProcGroup() (*procGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	// SetInformationJobObject 是裸的内核调用：它收一个指针和一个长度，
	// 让这两者对得上是我们的事。这里传错大小，回来的就是一句
	// ERROR_INVALID_PARAMETER，什么也不告诉你。
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job) // 刚配置失败的这个 job 不能漏掉
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &procGroup{job: job}, nil
}

// attach 在 Windows 上是空操作，而它为什么是空的，是这个文件里最有
// 教益的一件事。
//
// Unix 上 attach 会设 Setpgid，内核就在 fork() 和 exec() 之间把子进
// 程放进它自己的进程组——子进程连一条指令都还没执行。Windows 没有对
// 应的东西：进程只有存在之后才能被分配进 job，而 CreateProcess 交给
// 你的是已经在跑的进程。这要付出什么代价，见 adopt()。
//
// （CreationFlags 字段是故意不动的。CREATE_NEW_PROCESS_GROUP 管的是
// Ctrl+C / Ctrl+Break 往哪儿送，不是圈住进程，在这儿设它只会改变控
// 制台信号怎么送到 shell，对我们杀它没有半点帮助。）
func (g *procGroup) attach(cmd *exec.Cmd) {}

// adopt 把刚起来的进程——以及它接下来创建的每一个进程——分配进 job。
//
// # 残留的竞争，老实交代
//
// CreateProcess 返回，到 AssignProcessToJobObject 生效，中间有段窗
// 口。shell 要是赶在这段窗口里生出个孙子进程，这个孙子**不在** job
// 里，kill() 也碰不到它。窗口只有微秒量级，而且得是那种一上来就先
// 起个后台进程的 shell 才踩得中，所以实际上它不会发作——但"实际上
// 它不会发作"，恰恰就是人们写在自己发布出去的 bug 上面的那句话。
//
// 严丝合缝的修法是 CREATE_SUSPENDED：起进程的时候把主线程挂起，趁
// 着什么都跑不起来的时候把 job 分配好，然后 ResumeThread。什么都没
// 执行过，也就什么都逃不掉。exec.Cmd 让这条路变得别扭，但不是不可
// 能：它暴露了 cmd.Process.Pid，却没暴露主线程的句柄，于是想恢复它
// 就得对机器上所有线程做一次 CreateToolhelp32Snapshot，筛出属于我们
// 这个 PID 的，对活下来的那个 OpenThread，再 ResumeThread——大约六十
// 行 Toolhelp 代码，另外还有个问题要回答：万一进程还没开始跑就不知
// 怎么有了不止一个线程，该怎么办。（os/exec 正是因为这个才故意不支
// 持 CREATE_SUSPENDED；挂起的进程如果 Go 自己的收割逻辑永远不去恢复
// 它，cmd.Wait() 就会挂住。）
//
// 这里选的取舍是：接受这个微秒级的窗口，并且把话说在明处。如果你写
// 的是沙箱而不是教学仓库，那就去写 Toolhelp 那版——或者干脆绕开
// os/exec，直接用 CreateProcess，那里标志和线程句柄都明摆在
// PROCESS_INFORMATION 里。
//
// 有一个竞争是**不**存在的，却常被人以为存在：按 PID 做 OpenProcess
// 看起来可能撞上被回收的 PID。在这里撞不上。从 Start() 到
// Wait()/Release()，os.Process 一直开着指向子进程的句柄，而只要还有
// 任何句柄开着，Windows 就不会回收这个 PID。让我们这次查找安全的，
// 正是 Go 为自己记账而留着的那个句柄。
func (g *procGroup) adopt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("adopt called before Start")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("adopt called after Close")
	}

	// AssignProcessToJobObject 真正检查的访问权限是 PROCESS_SET_QUOTA
	// （job 在正式定义上是个配额与限制的容器），另外还得配上
	// PROCESS_TERMINATE。只要这两个而不是 PROCESS_ALL_ACCESS，在受限
	// token 下也照样能用。
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", cmd.Process.Pid, err)
	}
	// 我们自己这个句柄只在做分配的时候要用。job 会自己留一份对进程的
	// 引用，所以把它关掉，成员关系一点不变。
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		// 这里最典型的失败是 ERROR_ACCESS_DENIED，因为进程已经在某个不许
		// 嵌套的 job 里了。嵌套 job 在 Windows 8 / Server 2012 及以后能用；
		// 再老的版本上，或者在某些把每个进程都塞进受限 job 的 CI runner
		// 和容器宿主里，圈住进程这件事就是在这儿丢的。runBash 把这个错误
		// 当成不致命的，只警告一句，这是诚实的反应：命令照跑，只不过超时
		// 现在只杀得掉 shell 自己。
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// kill 把 job 里的每个进程都终止掉，原子地，由内核那边动手。
//
// 有一点上它严格强于 Unix 版：Unix 的进程组归属是可以改的，进程对自
// 己调一次 setpgid() 就行，所以铁了心的子进程能脱离这个组。job 的成
// 员关系是永久的——一旦分配进去，进程自己没法退出来，别人也没法把它
// 弄出来。
//
// 被终止的进程会报的退出码是 1。这个值是随意挑的；唯一要避开的是
// 259（STILL_ACTIVE），用它会让任何走 GetExitCodeProcess 的代码分不
// 出死掉的进程和还在跑的进程。
//
// 吞掉错误的理由和 Unix 那边一样：走到调 kill() 这一步，剩下的补救
// 办法都是你不会想让 Agent 自动去试的。调两次，或者对着成员早已全部
// 退出的 job 调，都无害——空 job 照样终止得好好的。
func (g *procGroup) kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.job == 0 {
		return
	}
	_ = windows.TerminateJobObject(g.job, 1)
}

// Close 放掉 job 句柄——而因为 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE，
// 这一放同时也会把还在里面的东西全杀掉。
//
// 幂等：句柄是在锁里先清零再关的，所以第二次 Close 什么也不做，跟
// Close 抢跑的 kill() 也不可能用上失效的句柄。后面这点比看上去要
// 紧。Windows 回收句柄值回收得很急，所以对已经关掉的句柄调
// TerminateJobObject 不是无害的错误——那个数字碰巧被谁继承了，它就
// 可能落到谁头上。
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

// processAlive 报告某个 PID 是不是正在跑的进程。它是为 proc_test.go
// 存在的，那边必须证明孙子进程真的没了，而不是相信 kill() 没抱怨就
// 算数。
//
// 注意它**没**用什么：GetExitCodeProcess。那是最顺手的调用，也有个
// 出名的陷阱——它对正在跑的进程报 STILL_ACTIVE（259），可 259 同时
// 也是完全合法的退出码，于是以 259 退出的进程看起来永远活着。
// WaitForSingleObject 没有这种含糊：进程句柄恰好在进程终止的那一刻
// 变成有信号，所以超时给零，一次调用就能拿到毫不含糊的"已经死了 /
// 还在跑"。
//
// 一处 Unix 没有对应物的 Windows 但书：只要系统里任何地方还开着一个
// 指向已死进程的句柄，它的 PID 就不会被回收，对它做 OpenProcess 还是
// 会**成功**。所以"OpenProcess 成功了"不能当作活着的证据——你得接着
// 问句柄有没有信号，而那正是这次 wait 干的事。
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
		// ERROR_INVALID_PARAMETER 表示没这个 PID：死了，而且彻底回收了。
		// ERROR_ACCESS_DENIED 则表示活着但不是我们的——对我们自己起的进
		// 程的子进程来说这不可能，所以在这里把它当成"没了"是安全的，换
		// 成通用工具就是错的。
		return false
	}
	defer windows.CloseHandle(h)

	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	// WAIT_OBJECT_0 => 句柄有信号 => 进程已经终止。
	// WAIT_TIMEOUT  => 没有信号 => 还在跑。
	return event != windows.WAIT_OBJECT_0
}
