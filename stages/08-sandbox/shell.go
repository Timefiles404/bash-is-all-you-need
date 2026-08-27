// 阶段 08 —— 级别 3：做 shell。
//
// policy.go 从外面检查一个命令，会输两次，而且两次的原因相同：它看到
// 的是"接下来会发生什么"的描述，而 shell 决定的才是真正发生的事。引用
// 击败了字符串检查。展开击败了解析检查。
//
// 真正管用的做法，是不再当一个置身事外的观察者。嵌入解释器：在引用移
// 除、参数展开、命令替换、算术、波浪展开、globbing 和 `eval` **之后**，
// 它即将执行的每个命令，都会作为一个已经拼好的参数向量到达。没有语法
// 可以再拿来躲在后面，因为语法刚刚才被消耗掉。
//
//	命令：  X=.en; eval "cat \$X"'v'
//	argv：     ["cat", ".env"]
//
// 两个处理器 —— 而两者都要用到，这一点并不显而易见：
//
//	ExecHandler  shell 运行的每个程序，用它的最终 argv
//	OpenHandler  shell 本身打开的每个文件 —— 重定向
//
// `cat < .env` 运行 `cat` **没有参数**。shell 打开文件，交出一个文件描
// 述符。一个只检查 argv 的策略，压根看不到文件名，会在包括这一级在内
// 的每个级别都放它通过。
//
// 然后是诚实的部分，也是这一章剩下的内容：这是一个**策略和可观察性层，
// 不是安全边界。** 它看得到每一个命令，却看不进任何一个的内部。
// `python -c "..."` 是一个 exec，这个 exec 之后，沙箱对任何事都不再有
// 意见。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type sandbox struct {
	root string
	bus  *Bus

	// enforce=false 把这个做成观察者：它报告每个 exec 和每个打开，什么都不
	// 阻挡。这值得单独做成一种模式 —— 这里的价值大部分在于，能看到一个
	// shell 命令真正做了什么，而这份价值并不需要拒绝任何东西。
	enforce bool

	mu      sync.Mutex
	execs   []string // 展开后看到的每个 argv 都要检查，不只是模型写下的那个
	opens   []string // 每个 shell 打开的路径
	blocked []string
}

func newSandbox(root string, bus *Bus, enforce bool) *sandbox {
	return &sandbox{root: root, bus: bus, enforce: enforce}
}

// run 在嵌入的解释器内执行一个命令。
//
// 它返回的 execResult 和 runBash 完全一样，所以 Agent 的其余部分根本分
// 不出区别 —— 这正是 exec.go 从阶段 01 起，就让渲染和运行保持分离的意
// 义所在。
func (s *sandbox) run(command string, timeout time.Duration) execResult {
	started := time.Now()

	file, err := syntax.NewParser().Parse(strings.NewReader(command), "cmd")
	if err != nil {
		// 这里的解析错误，就是一个 **shell** 解析错误，会在任何东西运行之前就
		// 报告出来。这比 bash 的行为更好 —— bash 会一直执行到语法错误那一行，
		// 然后才抱怨。
		return execResult{
			Stderr:   "sandbox: " + err.Error(),
			ExitCode: 2,
			Duration: time.Since(started),
		}
	}

	var stdout, stderr bytes.Buffer
	runner, err := interp.New(
		interp.Dir(s.root),
		interp.StdIO(nil, &stdout, &stderr),
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.ExecHandlers(s.execMiddleware),
		interp.OpenHandler(s.open),
	)
	if err != nil {
		return execResult{Stderr: "sandbox: " + err.Error(), ExitCode: -1, Duration: time.Since(started)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	runErr := runner.Run(ctx, file)
	res := execResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(started),
	}

	// 超时是上下文的，取消上下文杀死默认处理器开始的子进程 —— 所以阶段 01 的
	// 进程组工作仍在这里一层下做它的工作。
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}

	var status interp.ExitStatus
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &status):
		res.ExitCode = int(status)
	default:
		// 一个处理器返回了一个非状态错误 —— 很可能是一个策略拒绝。把文本亮出
		// 来：它是唯一能告诉模型该怎么换种做法的东西。
		res.ExitCode = 1
		if res.Stderr != "" && !strings.HasSuffix(res.Stderr, "\n") {
			res.Stderr += "\n"
		}
		res.Stderr += runErr.Error()
	}
	return res
}

// execMiddleware 包装解释器的默认 exec 处理器。
//
// 这里的 `args` 是完成的参数向量。shell 原本要对源文本做的所有事，此
// 刻都已经做完了 —— 这正是为什么这里是策略唯一能立足的地方。
func (s *sandbox) execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		joined := strings.Join(args, " ")

		s.mu.Lock()
		s.execs = append(s.execs, joined)
		s.mu.Unlock()

		// 对**每个** exec 发出，包括模型从不写的那些 —— pipeline、循环、别名
		// 或 `eval` 生产出来的那些。这就是这一章"每进程拦截"的那一半，读它的
		// trace，是搞清楚一个 shell 命令到底做了什么的最快方式。
		s.bus.Emit(Event{Kind: KindSandboxExec, Command: joined})

		if r := s.checkArgv(args); r != nil {
			s.mu.Lock()
			s.blocked = append(s.blocked, joined)
			s.mu.Unlock()
			s.bus.Emit(Event{Kind: KindSandboxBlock, Command: joined, Text: r.Error()})
			if s.enforce {
				return r
			}
		}
		return next(ctx, args)
	}
}

// shell 本身打开的每个文件 —— 也就是重定向 —— 都会触发一次 open 调用。
//
// 由 shell 运行的**程序**打开的文件，不会经过这里 —— 解释器对另一个进
// 程的 syscall 没有可见性，而想得到这种可见性，就得靠 ptrace、
// seccomp-bpf，或者一个文件系统命名空间，这也是这一章最后落脚的、操作
// 系统级别的答案。
func (s *sandbox) open(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	s.mu.Lock()
	s.opens = append(s.opens, path)
	s.mu.Unlock()
	s.bus.Emit(Event{Kind: KindSandboxOpen, Path: path})

	if isSecretPath(path) {
		r := &refusal{Level: "sandbox/open", What: path, Why: "a redirect targets " + secretName}
		s.mu.Lock()
		s.blocked = append(s.blocked, "< "+path)
		s.mu.Unlock()
		s.bus.Emit(Event{Kind: KindSandboxBlock, Command: "redirect " + path, Text: r.Error()})
		if s.enforce {
			return nil, r
		}
	}
	return interp.DefaultOpenHandler()(ctx, path, flag, perm)
}

// checkArgv 是策略，应用到一个完成的参数向量。
func (s *sandbox) checkArgv(args []string) *refusal {
	for _, a := range args[1:] {
		if isSecretPath(a) {
			return &refusal{Level: "sandbox/exec", What: a,
				Why: "an argument resolves to " + secretName + " after expansion"}
		}
	}

	// shell-in-a-shell 情况，一个诚实的半措施。
	//
	// `sh -c 'cat .env'` 是一个 exec，它的 argv 里，装着一整个新程序，就塞
	// 在一个字符串里。拒绝把脚本交给一个嵌套 shell，意味着沙箱无法被轻易
	// 绕过，也让 Agent 付出了一项真实能力的代价。它**不**做的是泛化：perl、
	// python、awk、ruby、node、`find -exec`、`git -c core.pager=` 和
	// `make`，也都能拿一个程序当参数，列举它们，就是级别 1 已经输掉的那个
	// denylist 游戏。
	//
	// 它之所以在这里，是因为它确实有点用；之所以被说成一个半措施，是因为
	// 假装它不止于此，正是沙箱骗得信任的方式。
	if len(args) >= 3 {
		switch args[0] {
		case "sh", "bash", "dash", "zsh", "ksh":
			if args[1] == "-c" {
				return &refusal{Level: "sandbox/exec", What: strings.Join(args, " "),
					Why: "a nested shell would run outside this sandbox's view"}
			}
		}
	}
	return nil
}

// report 汇总沙箱观察到的一切，供一次会话结束时使用。
func (s *sandbox) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("sandbox: %d commands executed · %d files opened by the shell · %d blocked",
		len(s.execs), len(s.opens), len(s.blocked))
}
