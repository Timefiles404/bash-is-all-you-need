// 阶段 08——第 3 级：成为 shell。
//
// policy.go 从外面检查命令，输了两次，两次都输在同一件事上：它看的是"将
// 要发生什么"的一份描述，而 shell 那边正在决定实际发生什么。引号干掉了字
// 符串检查，展开干掉了解析检查。
//
// 真正管用的一步是：别再当外部观察者。把解释器嵌进来，它每要执行一条命
// 令，到手的就是一个成品参数向量——在去引号、参数展开、命令替换、算术、
// 波浪号展开、通配和 `eval` **之后**。已经没有语法可以躲在后面了，因为语
// 法刚刚就是被吃掉的那个东西。
//
//	命令:     X=.en; eval "cat \$X"'v'
//	argv:     ["cat", ".env"]
//
// 两个 handler，而两个都需要，是不那么显然的那部分：
//
//	ExecHandler  shell 跑的每个程序，连它最终的 argv
//	OpenHandler  **shell** 自己打开的每个文件——重定向
//
// `cat < .env` 跑 `cat` 时**一个参数都没有**。文件由 shell 打开，交过来的
// 是文件描述符。只检查 argv 的策略根本看不到那个文件名，而且它在每一级都
// 会放这条命令过去，包括这一级。
//
// 然后是诚实的那部分，也就是本章剩下的内容：这是**一层策略与可观测层，不
// 是安全边界。** 它看得见每一条命令，却看不进任何一条里面。
// `python -c "..."` 是一次 exec，那次 exec 之后，沙箱对任何事情都不再有意
// 见了。
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

	// enforce=false 把它变成观察者：报告每一次 exec、每一次 open，什么都不
	// 拦。这值得单独做成一种模式——这里大半的价值就在于看清一条 shell 命令
	// 究竟做了什么，而这份价值并不需要拒绝任何东西。
	enforce bool

	mu      sync.Mutex
	execs   []string // 看到的每个 argv，展开之后的
	opens   []string // shell 打开过的每个路径
	blocked []string
}

func newSandbox(root string, bus *Bus, enforce bool) *sandbox {
	return &sandbox{root: root, bus: bus, enforce: enforce}
}

// run 在嵌入的解释器里执行一条命令。
//
// 它返回和 runBash 一样的 execResult，所以 Agent 的其余部分分辨不出差别
// ——这正是从阶段 01 起就把 exec.go 的"跑命令"和"渲染结果"分开的意义。
func (s *sandbox) run(command string, timeout time.Duration) execResult {
	started := time.Now()

	file, err := syntax.NewParser().Parse(strings.NewReader(command), "cmd")
	if err != nil {
		// 这里的解析失败是 *shell* 的解析失败，在任何东西跑起来之前就报
		// 出来了。这比 bash 的行为好：bash 会把语法错误之前的全部执行掉，
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

	// 超时用的是 context 的超时，取消 context 会杀掉默认 handler 起的子进
	// 程——所以阶段 01 那套进程组的功夫，在这里、在下面一层，仍然在干活。
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
		// 某个 handler 返回了不是退出状态的错误——十有八九是策略拒绝。把
		// 文本抛出来：只有它才会告诉模型该换成怎么做。
		res.ExitCode = 1
		if res.Stderr != "" && !strings.HasSuffix(res.Stderr, "\n") {
			res.Stderr += "\n"
		}
		res.Stderr += runErr.Error()
	}
	return res
}

// execMiddleware 包住解释器默认的 exec handler。
//
// 这里的 `args` 就是成品参数向量。shell 打算对源文本做的一切都已经做完
// 了，这正是为什么只有站在这里，策略才站得住。
func (s *sandbox) execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		joined := strings.Join(args, " ")

		s.mu.Lock()
		s.execs = append(s.execs, joined)
		s.mu.Unlock()

		// **每一次** exec 都会发出事件，包括模型根本没写过的那些——管道、
		// 循环、别名或 `eval` 产出来的那些。这是本章"逐进程拦截"的那一半；
		// 读它的 trace，是搞清一条 shell 命令究竟做了什么的最快办法。
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

// shell 自己打开的每个文件都会调到 open：也就是重定向。
//
// shell 跑起来的那些*程序*打开的文件不走这里——解释器看不见另一个进程的
// 系统调用，要看见就得上 ptrace、seccomp-bpf 或者文件系统 namespace，那
// 就是本章结尾落到的操作系统层答案。
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

// checkArgv 就是策略本身，作用在成品参数向量上。
func (s *sandbox) checkArgv(args []string) *refusal {
	for _, a := range args[1:] {
		if isSecretPath(a) {
			return &refusal{Level: "sandbox/exec", What: a,
				Why: "an argument resolves to " + secretName + " after expansion"}
		}
	}

	// shell 里再套一层 shell 的情形，还有诚实的半截措施。
	//
	// `sh -c 'cat .env'` 是一次 exec，它的 argv 里用字符串装着一整个新程
	// 序。不让嵌套 shell 拿到脚本，意味着这个沙箱没法被轻轻松松绕开，代价
	// 是 Agent 真丢掉了一项能力。它**不**做的事情是推广开去：perl、python、
	// awk、ruby、node、`find -exec`、`git -c core.pager=` 和 `make` 也都能
	// 把程序当参数收，而一个个列举它们，就是第 1 级早就输掉的那场黑名单游
	// 戏。
	//
	// 它留在这里，是因为它确实值点什么；把它写成半截措施，是因为不这么写，
	// 沙箱就会被人当真信了。
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

// report 把沙箱观察到的东西汇总一句，给会话结束时用。
func (s *sandbox) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("sandbox: %d commands executed · %d files opened by the shell · %d blocked",
		len(s.execs), len(s.opens), len(s.blocked))
}
