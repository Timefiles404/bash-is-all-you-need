//go:build windows

// 杀进程树，Windows 版。从阶段 01 没有改变——每个阶段是独立
// 快照，所以文件与它一起旅行。
//
// Windows 在 Unix 意义上没有进程组，并且——这正是最容易让人
// 栽跟头的地方——也没有可靠的父子关系。Windows 进程会记录
// 创建它的那个 PID，但那个 PID 不被保活，不被继承，还被急切
// 回收。所以"走父链、杀子树"这套做法，在 Windows 上不仅是
// 竞速的，更是错的：一个已被回收的 PID，会让一个毫不相干的
// 进程看起来像你的孙进程，你就这样杀掉了别人的工作。
//
// 正确的原语是作业对象：一个内核容器，进程会被分配到其中，
// 子进程自动继承这个容器，整个容器可以作为一个单位被终止。
// 它就是 Windows 容器背后的那个机制，也是 TerminateJobObject
// 能保持原子性的原因——内核遍历的是它自己的成员列表，所以
// 没有东西能在遍历过程中中途分叉逃逸。
//
// 这里用的是 golang.org/x/sys/windows，而不是手写的 syscall
// 存根。它由 Go 团队在 Go 项目自己的仓库里维护，所以作为一个
// 非 stdlib 依赖，它已经算是最接近"stdlib"的那一档了；替代
// 方案是自己写 LazyDLL 绑定，那只是同一段代码的更差版本。
package main

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGroup 拥有一个工作对象句柄。和 Unix 版本不一样——
// 那边的"组"只是一个内核早就知道的整数——这里是一个
// 真实的内核对象，带着一个真实的句柄：忘记关闭它，它
// 就会泄漏。
type procGroup struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

// newProcGroup 会事先就创建好工作对象，在进程存在之前。
//
// 这个限制标志，就是要专门配置这个工作对象的全部理由
// 所在：JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 的意思是
// "当这个工作对象的最后一个句柄关闭时，终止仍在里面的
// 所有东西"。那给了我们一张 Unix 实现根本没有的安全网——
// 如果 Agent 进程死亡、崩溃，或者被任务管理器杀掉，内核
// 会替我们关闭句柄，失控的那整棵树也会跟着一起死掉。而
// 在 Unix 上，孤儿进程只会被过继给 init，然后继续运行。
//
// 有两个后果，学生必须牢记在心：
//
//   - Close() 在这个类型上不是被动的清理，它会杀进程。
//     这就是为什么 runBash 里的 `defer g.Close()` 是承重
//     构件——也是为什么一个故意把长期运行的服务器放到
//     后台的命令（`nohup npm start &`），会在 Linux/macOS
//     上撑过这次工具调用，但在 Windows 上**不**会。这是
//     两个文件之间真实存在的行为差异，不是疏忽。对一个
//     Agent 来说，死掉大概反而是更好的默认行为：不该有
//     什么东西在 Agent 不知情的情况下，活得比它的工具
//     调用还久。
//   - 不设置 JOB_OBJECT_LIMIT_BREAKAWAY_OK（保持默认）
//     同样是故意的。如果不设置它，一个请求
//     CREATE_BREAKAWAY_FROM_JOB 的子进程会被内核拒绝，
//     CreateProcess 直接彻底失败。逃逸在默认情况下就是
//     被禁止的，这正是我们想要的。
func newProcGroup() (*procGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	// SetInformationJobObject 是一次裸内核调用：它接受一个
	// 指针和一个长度，而让两者对得上，是我们自己的责任。
	// 这里如果传错了大小，你得到的就是一个 ERROR_INVALID_PARAMETER
	// ——而它什么都不会告诉你。
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job) // 不要泄漏我们刚配置失败的工作
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &procGroup{job: job}, nil
}

// attach 在 Windows 上是一个空操作，而这背后的原因，是
// 这个文件里最有启发性的部分。
//
// 在 Unix 上，attach 会设置 Setpgid，内核会在 fork() 和
// exec() 之间——也就是子进程执行第一条指令之前——把子
// 进程放进它自己的进程组。Windows 没有对应的机制：一个
// 进程只有在它已经存在之后，才能被分配到工作对象里，而
// CreateProcess 给你的，是一个已经在运行的进程。这付出了
// 什么代价，见 adopt()。
//
// （CreationFlags 字段是故意保持原样不动的。
// CREATE_NEW_PROCESS_GROUP 关乎的是 Ctrl+C / Ctrl+Break
// 的路由，跟包含机制无关；在这里设置它，只会改变控制台
// 信号到达 shell 的方式，却没法帮我们杀掉它。）
func (g *procGroup) attach(cmd *exec.Cmd) {}

// adopt 会把刚启动的这个进程——以及它接下来创建的每一个
// 进程——都分配进这个工作对象。
//
// # 剩余的竞争条件，坦诚交代
//
// 在 CreateProcess 返回、到 AssignProcessToJobObject 生效
// 之间，有一个窗口期。如果 shell 设法在那个窗口内生成了
// 一个孙进程，这个孙进程就**不**在工作对象里，kill() 也
// 碰不到它。这个窗口只有几微秒宽，只有一个把启动后台
// 进程当成自己第一件事来做的 shell，才可能踩中它，所以
// 实际上这种情况不会被触发——但"实际上不会被触发"这句
// 话，正是人们在自己发布出去的 bug 头上，最爱写的那句话。
//
// 彻底的修复办法是 CREATE_SUSPENDED：启动进程时让它的
// 主线程保持挂起，在什么都还没法运行的时候把它分配进
// 工作对象，然后再 ResumeThread。什么都逃不掉，因为什么
// 都还没执行。exec.Cmd 让这件事变得别扭，而不是完全做
// 不到：它暴露了 cmd.Process.Pid，却不暴露主线程的句柄，
// 所以要恢复运行，就得对机器上所有线程做一次
// CreateToolhelp32Snapshot，筛选出属于我们这个 PID 的
// 线程，对幸存下来的那个调用 OpenThread，再
// ResumeThread——大概六十行 Toolhelp 代码，再加上：
// 如果这个进程在开始前不知怎么就有了不止一个线程，那又
// 该怎么办的问题。（正是因为这个原因，os/exec 才故意不
// 支持 CREATE_SUSPENDED：一个被挂起、而 Go 自己的收割者
// 又永远不会去恢复的进程，会让 cmd.Wait() 直接挂起。）
//
// 这里做出的取舍是：接受这个微秒级的窗口，并把这一点
// 大大方方地说出来。如果你写的是一个沙箱、而不是一个
// 教学仓库，就该去写 Toolhelp 版本——或者干脆直接用
// CreateProcess 代替 os/exec，那个标志位和线程句柄，在
// PROCESS_INFORMATION 里都现成地摆在那儿。
//
// 有一种竞争其实**不**存在，却常被以为存在：按 PID 调用
// OpenProcess，看起来可能会碰上一个被回收的 PID。但在
// 这里不会。os.Process 会一直持有一个指向子进程的打开
// 句柄，从 Start() 开始，直到 Wait()/Release() 为止，
// 而只要还有任何一个句柄指向它、并保持打开，Windows
// 就不会回收这个 PID。Go 为自己簿记而保留的这个句柄，
// 正是让我们的查找得以安全的原因。
func (g *procGroup) adopt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("adopt called before Start")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("adopt called after Close")
	}

	// PROCESS_SET_QUOTA 是 AssignProcessToJobObject 实际会
	// 检查的那个访问权限（严格来说，工作对象是一种配额与
	// 限制的容器）；此外还需要 PROCESS_TERMINATE。只精确地
	// 申请这两项权限、而不是 PROCESS_ALL_ACCESS，才能让这套
	// 机制在一个受限令牌下也照样能用。
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", cmd.Process.Pid, err)
	}
	// 我们自己持有的这个句柄，只是分配这一步本身需要用到。
	// 工作对象自己保留着一份对这个进程的引用，所以关掉我们
	// 这个句柄，不会对成员归属造成任何影响。
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		// 这里典型的失败是 ERROR_ACCESS_DENIED，因为这个进程
		// 已经处于一个禁止嵌套的工作对象里。嵌套工作对象只在
		// Windows 8 / Server 2012 及更新版本上才能用；在更旧的
		// 系统上，或是在某些会把每个进程都塞进一个锁死的工作
		// 对象里的 CI 运行环境和容器宿主上，包含性就会在这里
		// 失效。runBash 会把这个错误当成非致命错误处理，并发出
		// 警告，这是诚实的做法：命令依然会运行，只是这时候超时
		// 机制就只能杀掉 shell 本身了。
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// kill 会从内核那一侧，原子地终止这个工作对象里的每
// 一个进程。
//
// 在某一点上，这严格强于 Unix 版本：在 Unix 上，进程组
// 成员关系可以被一个对自己调用 setpgid() 的进程改变，
// 所以一个存心要离开的子进程是可以脱离这个组的。工作
// 对象的成员关系是永久性的——一旦分配，进程自己没法
// 退出，别人也办不到。
//
// 被终止的进程，报告的会是退出代码 1。这个值是任意选的；
// 唯一要避开的是 259（STILL_ACTIVE），那会让任何用
// GetExitCodeProcess 检查的代码，都没法把一个已经死掉
// 的进程和一个还在运行的进程区分开。
//
// 错误被吞掉的原因，跟 Unix 上一样：等到你要调用 kill()
// 的这个时候，剩下能做的补救，都是些你不会希望 Agent
// 自动去尝试的事情。调用它两次，或者对一个成员已经全部
// 退出的工作对象调用它，都没有害处——一个空的工作对象，
// 照样能正常终止。
func (g *procGroup) kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.job == 0 {
		return
	}
	_ = windows.TerminateJobObject(g.job, 1)
}

// Close 释放作业句柄——因为 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE，
// 也会杀死其中剩余的任何东西。
//
// 幂等：句柄会在持锁的情况下先清零，再关闭，所以第二次 Close
// 是空操作，kill() 与 Close 竞速时无法使用失效句柄。
// 这一点比看起来的要重要。Windows 急切地回收句柄值，
// 所以在已关闭的句柄上调用 TerminateJobObject 不是无害错误——
// 它可能会落在恰好继承了该数字的任何对象上。
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

// processAlive 报告一个 PID 是否是当前正在运行的进程。
// 它为 proc_test.go 存在，后者必须证明孙进程已离开，
// 而不是相信 kill() 返回时没有抱怨。
//
// 注意这里**不**使用的东西：GetExitCodeProcess。这是显而易见的调用，
// 有一个著名的陷阱——它为运行中的进程报告 STILL_ACTIVE（259），
// 但 259 也是一个完全合法的退出码，所以以 259 退出的进程看起来
// 永远活着。WaitForSingleObject 没有这样的歧义：进程句柄恰好在
// 进程终止的那一刻变得有信号，所以只需一次零超时调用，就能
// 给出明确的"已死/仍在运行"结果。
//
// Windows 专属的注意事项，Unix 没有对应物：只要系统里任何地方
// 还留着一个指向死进程的句柄没关闭，它的 PID 就不会被回收，
// OpenProcess 用在它上面依然**成功**。所以"OpenProcess 工作"
// 不是活跃的证据——你必须继续问句柄是否有信号，这正是 wait
// 做的。
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
		// ERROR_INVALID_PARAMETER 表示没有这样的 PID：已死且完全回收。
		// ERROR_ACCESS_DENIED 会表示活着但不属于我们——
		// 对于我们启动的进程的孩子来说不可能，所以把它当作"消失"
		// 在这里是安全的，但在通用工具中会是错的。
		return false
	}
	defer windows.CloseHandle(h)

	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	// WAIT_OBJECT_0 => 句柄有信号 => 进程已终止。
	// WAIT_TIMEOUT  => 无信号 => 仍在运行。
	return event != windows.WAIT_OBJECT_0
}
