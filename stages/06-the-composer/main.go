// 阶段 06——The Composer：阶段 05，外加一个办法，去看清它干了什么。
//
// 三种时间尺度，任何 Agent 三样都要：
//
//	一次请求之内  —— messages 数组。阶段 00–04。
//	一次会话之内  —— 上下文压缩，数组撑不进窗口的时候。
//	跨会话        —— 一个文件。机制全部就在这儿。
//
// 跟阶段 04 的 diff 很小，落在三个地方：系统提示词现在带上了记忆和稳定
// 的环境信息（memory.go），每个用户回合会在旁边冻一份易变的快照，工具
// 循环的开头会查一下对话是不是快撞墙了（compact.go）。
//
// 有一处结构上的改动值得一提：那些长命的部件——供应商、总线、权限闸、
// 配置、compactor——挪到了 `agent` 结构体上。阶段 04 的 runTurn 收八个
// 参数，阶段 05 还要再加三个。这里的接收者不是什么抽象，它就是同一堆值
// 换了个短名字。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"bash-is-all-you-need/tui"
	"bash-is-all-you-need/tui/settings"
)

const basePrompt = `You are a coding agent working in a terminal on the user's machine.

You have exactly one tool: bash. Every action you take is a shell command, so
reach for ordinary Unix tools (ls, cat, grep, find, sed, git) instead of asking
the user to do things for you. Chain commands with pipes when that saves a round
trip.

The shell is not persistent: each call runs in a fresh process, so cd and
environment variables do not survive between calls. Write POSIX-compatible
commands — the shell may be bash 3.2.

Commands are killed after a timeout, so never run anything that waits forever:
no dev servers in the foreground, no interactive prompts. Output is truncated
past a size limit, so prefer commands that filter (grep, head, wc) over
commands that dump.

The user may deny a command. If that happens, do not retry it unchanged —
either find another way or ask.

When the task is done, reply with a short plain-text summary and no tool call.`

// memoryPrompt 就是长期记忆这个功能的全部。
//
// 没有工具，没有存储，没有 embedding，没有检索这一步：一个文件，加一句
// 话告诉模型，它可以用手头已有的工具往里追加。最后那一行才是决定这份
// 文件半年后还值不值得读的地方——"记你学到了什么，不是你干了什么"，
// 就是知识库和日记的分界线。
const memoryPrompt = `

Durable notes live in ` + memoryFileForWriting + ` in the working directory. If that file
exists, its contents are already in your context above.

When you learn something about this project that would cost you tool calls to
rediscover in a future session — a build command, where something lives, a
gotcha, a decision the user made — append it:

  printf '\n- <one short factual line>\n' >> ` + memoryFileForWriting + `

Record what you learned, not what you did. Notes written now take effect in your
next session, not this one.`

func bashToolDef() Tool {
	return Tool{
		Name:        "bash",
		Description: "Execute a bash command and return its stdout, stderr and exit code.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute.",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

type config struct {
	shell     string
	timeout   time.Duration
	maxOutput int
	maxTurns  int
	yolo      bool
}

// ---------------------------------------------------------------------------
// 权限闸。除了改成通过总线上报，从阶段 01 起就没变过。
// ---------------------------------------------------------------------------

type gate struct {
	yolo, always bool
	available    bool
	out          io.Writer

	// read 是答案的来路。
	//
	// 一个函数，而不是从阶段 01 一直放到 tui/ 里那个交互式外壳出现之前的
	// *bufio.Scanner。那个外壳把终端切进原始模式，并且占着 stdin；同一个描述
	// 符上再挂一个 Scanner，会把用户正在敲的那一行里的按键抢走，两个读者各拿
	// 到半个答案。所以读法由外面递进来；而递进来一个 nil，跟
	// `available: false` 是同一种情况——没有地方可问。
	read func() (string, bool)
}

type verdict string

const (
	allow verdict = "allow"
	deny  verdict = "deny"
	abort verdict = "abort"
)

func (g *gate) ask(command string) (verdict, string) {
	if g.yolo || g.always {
		return allow, ""
	}
	if !g.available || g.read == nil {
		return deny, "no terminal to ask on — rerun with --yolo to allow commands"
	}
	fmt.Fprintf(g.out, "  run? [y / n / a = all / q = stop] ")
	line, ok := g.read()
	if !ok {
		return abort, "input closed"
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return allow, ""
	case "a", "all":
		g.always = true
		return allow, ""
	case "q", "quit":
		return abort, "the user stopped the session"
	default:
		return deny, "the user denied this command"
	}
}

// lineReader 是朴素行提示符下权限闸用的那个读法：从用户敲自己消息用的同一个
// Scanner 上读一行。
func lineReader(in *bufio.Scanner) func() (string, bool) {
	return func() (string, bool) {
		if !in.Scan() {
			return "", false
		}
		return in.Text(), true
	}
}

// ---------------------------------------------------------------------------

// agent 装着活满整个会话的那些东西。
type agent struct {
	p     Provider
	httpc *http.Client
	g     *gate
	bus   *Bus
	cfg   config
	comp  *compactor

	// system 是函数而不是字符串，原因在阶段 04 的 --break-cache 实验：启动
	// 时算一次的值是恒定前缀，只有每次请求都重算的值才会作废缓存。留着这
	// 一层间接，才能把这个差别表达出来。
	system func() string

	// out 是这个文件自己往外写的去处——只有下面那些斜杠命令，别无其他。朴素行
	// 提示符下是 stdout，交互式外壳下是外壳的输出区。它存在，是因为在备用屏里
	// 光秃秃一句 fmt.Println 会落到帧的底下，把版面弄坏，而且永远不会被看见。
	out io.Writer

	memoryDir  string
	lastPrompt int
}

func main() {
	var (
		providerName = flag.String("provider", "", "provider name from the providers file")
		providersAt  = flag.String("providers", "providers.json", "path to the providers file")
		listProv     = flag.Bool("list-providers", false, "list configured providers and exit")
		tracePath    = flag.String("trace", "", "write a JSONL event trace to this file")
		replayPath   = flag.String("replay", "", "replay a trace instead of running the agent")
		speed        = flag.Float64("speed", 1, "replay speed: 0 = instant, 1 = original timing")
		step         = flag.Bool("step", false, "replay: wait for Enter before each event")
		showReq      = flag.Bool("show-request", false, "print the full request body before each call")

		// 阶段 06。composer 是个**读者**，所以它只要一个路径，别的什么都不
		// 需要——不要 key，不要供应商，不要网络。这不是限制，这是阶段 02 当
		// 初决定让 trace 当事实来源、而不是当调试日志的回报。
		composerAt = flag.String("composer", "", "open the TUI on a trace file instead of running the agent")

		// 同样这几个视图，只是打印出来而不是画出来。
		//
		// 这不是调试暗门。凡是你想 diff、想 grep、想贴进 issue、想在 CI 里
		// 核对的东西，TUI 都是死路；而"第 12 次调用时模型看到了什么"，正是
		// 那种你希望能把答案接进管道的问题。渲染和绘制原本就是两个函数
		// （views.go 返回行，term.go 负责画），所以这件事只花了八行——不让
		// UI 占住数据，回报就在这儿。
		dumpAt   = flag.String("composer-dump", "", "print one composer view for a trace and exit")
		dumpView = flag.String("view", "model", "composer-dump: god | model | wire")
		dumpCall = flag.Int("call", 1, "composer-dump: which model call (1-based)")
		dumpW    = flag.Int("width", 100, "composer-dump: render width in columns")

		noCache    = flag.Bool("no-cache", false, "omit cache_control breakpoints (stage 04 control arm)")
		breakCache = flag.Bool("break-cache", false, "put a fresh timestamp in the system prompt on every request (stage 04)")

		// 阶段 05。
		compactAt = flag.Float64("compact-at", 0.70, "compact when the estimated prompt passes this fraction of the window")
		keepAt    = flag.Float64("keep", 0.30, "fraction of the window to leave in place after compacting")
		noCompact = flag.Bool("no-compact", false, "never compact — ride the window until the API refuses (control arm)")
		window    = flag.Int("window", 0, "override the provider's context window, in tokens")
		noMemory  = flag.Bool("no-memory", false, "do not read AGENTS.md / MEMORY.md")

		// 交互式外壳，在 tui/ 里。不是课程的一部分；见 shell.go。
		printOnly  = flag.String("p", "", "run one prompt without a UI, print the reply, and exit")
		noTUI      = flag.Bool("no-tui", false, "use the plain line prompt instead of the interactive shell")
		settingsAt = flag.String("settings", "", "path to the saved settings file; empty means the one under your user config directory")
	)
	cfg := config{}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds per user message")
	flag.BoolVar(&cfg.yolo, "yolo", false, "run every command without asking")
	flag.Parse()

	// 放在最前面：composer 从来不需要供应商，让它先等着供应商，就等于
	// 没配 key 的机器上读不了 trace——而你想读 trace 的机器，多半正是这
	// 种。
	if *dumpAt != "" {
		if err := dumpComposer(*dumpAt, *dumpView, *dumpCall, *dumpW, os.Stdout); err != nil {
			tui.Die(err)
		}
		return
	}
	if *composerAt != "" {
		if err := runComposer(*composerAt); err != nil {
			tui.Die(err)
		}
		return
	}

	// 存下来的设置，在任何东西去看环境之前就先读进环境里。
	//
	// 它们输给任何已经设过的值，而这条规矩正是让 `.env`、CI 和 `set -a` 的行为
	// 跟这个文件出现之前一模一样的东西——见 settings.ExportMissing。一份解析不
	// 了的文件会被报出来，然后就彻底不碰它：设置类命令宁可把自己关掉，也不去
	// 赌覆盖掉那里头某处的一个 key。
	store, storeErr := settings.Load(*settingsAt)
	if storeErr != nil {
		// 留到后面报，不在这里报。在出错的当场打印，那条消息会在备用屏盖上来
		// 的前一瞬间出现在屏幕上——于是外壳底下设置类命令没了，而唯一的解释显
		// 示过一下，随即被盖住。
		store = nil
	} else {
		store.ExportMissing()
	}

	pf, err := loadProviders(*providersAt)
	if err != nil {
		tui.Die(err)
	}
	if *listProv {
		for name, p := range pf.Providers {
			mark := " "
			if name == pf.Default {
				mark = "*"
			}
			fmt.Printf(" %s %-16s %-10s %s\n", mark, name, p.Protocol, p.Model)
		}
		return
	}

	// resolveErr 在这里故意**不**致命。
	//
	// 重放不需要 key、不需要 shell、不需要网络，也不需要供应商——这是阶段 02
	// 许下的承诺，写在 README 里；而从阶段 03 开始，直到这行代码写下之前，它一
	// 直是假的：resolve() 挪到了重放分支的上面，把自己那句 os.Exit(1) 也一起带
	// 了上去。在环境变量都设好的机器上（作者测过的每台机器都是这样），什么毛病
	// 也看不出来。在只有 trace 文件、别的什么都没有的机器上——而这个功能存在的
	// 意义正是为了这种机器——`--replay` 打出来的是 "no provider configured"。
	//
	// 所以错误是被带着走的，不是当场抛出来；到下面真正需要供应商的那一条路径上
	// 再检查。配置出错，该致命的只有依赖这份配置的代码，别的一概不该。
	pcfg, pname, resolveErr := pf.resolve(*providerName)
	if *window > 0 {
		pcfg.Window = *window
	}

	view := newRenderer(os.Stdout, colorEnabled(os.Stdout),
		prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
			cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite},
		pcfg.Window)
	view.showRequest = *showReq

	if *replayPath != "" {
		events, err := ReadTrace(*replayPath)
		if err != nil {
			tui.Die(err)
		}
		if err := Replay(events, view, ReplayOpts{Speed: *speed, Step: *step}, os.Stdin, os.Stdout); err != nil {
			tui.Die(err)
		}
		return
	}

	// 供应商能建就建——而**建不起来不致命**，只要有一个界面能把它修好。
	//
	// 这是交互式外壳唯一动过的启动行为，也是"一闪就没"这个修法的全部。从文件
	// 管理器里打开的二进制没有环境：没有 AGENT_BASE_URL，没有 key，
	// `set -a && . ./.env` 会放进去的东西一样都没有。外壳之前的每一版程序都是
	// 往 stderr 打一行然后退出——而在 Windows 上，发给它的那个控制台几微秒后就
	// 被销毁了，于是消息既正确又没人读得到，报上来的 bug 是"它就闪一下"。
	//
	// 所以失败被带到界面那边：界面照样起来，说清少了什么，再把能修好它的命令
	// 摆出来。没有界面的时候——管道喂进来的 stdin、-p、--no-tui——它仍然致命，
	// 因为没有人在那儿修。
	shellMode := useShell(*noTUI, *printOnly)

	var provider Provider
	provErr := resolveErr
	if provErr == nil {
		p, err := pcfg.build(!*noCache)
		if err != nil {
			provErr = err
		} else {
			provider = p
		}
	}
	if provider == nil && !shellMode {
		tui.Die(provErr)
	}

	shell, err := findBash()
	if err != nil {
		// 在每种模式下都致命，外壳里也一样，而这样的东西极少。没有 shell 就意
		// 味着这个 Agent 唯一那件工具不存在，而没有任何一条斜杠命令能装一个出
		// 来。
		tui.Die(err)
	}
	cfg.shell = shell

	bus := NewBus(view)

	// trace 蹲在一个开关后面，这样 /trace 才能在会话中途把它挪走。至于为什么不
	// 是给总线加一个 Unsubscribe，见 shell.go 里的 traceSink。
	traces := &traceSink{}
	bus.Subscribe(traces)
	if *tracePath != "" {
		if err := traces.open(*tracePath); err != nil {
			tui.Die(err)
		}
	}
	defer traces.close()

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}

	wd, _ := os.Getwd()

	// 外壳底下这句由横幅来带；stderr 留给那些没有横幅可带的路径。
	if storeErr != nil && !shellMode {
		fmt.Fprintf(os.Stderr, "note: %v\nnote: the settings commands are off until that file is fixed or deleted\n", storeErr)
	}

	sh := &shellSession{
		storeErr: storeErr,
		pf:       pf, view: view, bus: bus, store: store, trace: traces,
		pname: pname, pcfg: pcfg, wd: wd,
		opts: shellOpts{
			provider: *providerName, cacheBP: !*noCache,
			window: *window, noMemory: *noMemory,
			breakCache: *breakCache,
		},
	}

	// ---- 系统提示词 -----------------------------------------------------
	//
	// 它里面的一切在整个会话里都是稳定的，这才让它有资格待在缓存断点之前；会
	// 动的东西一律进消息流——见 memory.go 的放置规则。
	//
	// 在 tui/ 里那个外壳出现之前，它就是在这里当场拼好，只拼一次。现在这套装
	// 配是个函数，只为一个理由：/open 会换掉工作目录，而记忆文件是从那个目录
	// 里读的。依赖这个目录的东西一共三样，挪两样比一样都不挪更糟。
	sys := sh.assemble(shell, wd)
	if *breakCache && !shellMode {
		fmt.Println("--break-cache: a fresh timestamp goes into the system prompt on every request")
	}

	comp := newCompactor(pcfg.Window, *compactAt, *keepAt)
	if *noCompact {
		comp.threshold = 0
	}
	if pcfg.Window <= 0 && !*noCompact && provider != nil && !shellMode {
		fmt.Println("note: this provider has no `window` configured, so compaction can never fire. Set it, or pass --window.")
	}

	a := &agent{
		p: provider, httpc: &http.Client{Timeout: 10 * time.Minute},
		g:   &gate{yolo: cfg.yolo, read: lineReader(stdin), available: interactive, out: os.Stdout},
		bus: bus, cfg: cfg, comp: comp, system: sys, memoryDir: wd,
		out: os.Stdout,
	}
	sh.a = a

	// -p：一条提示词，一份仪表盘，没有界面，退出。
	//
	// 把非交互的契约写成一个 flag，而不是听天由命地看 stdin 恰好是不是管道。外
	// 壳能做的每件事，这里都能通过 flag 做到；它唯一做不到的是问，所以权限闸被
	// 明确关掉，需要授权的命令会带着理由被拒——而不是挂在一个没人看的终端上。
	if *printOnly != "" {
		a.g.available = false
		bus.Emit(Event{Kind: KindUserMessage, Text: *printOnly})
		msgs := a.runTurn([]Msg{userTurn(*printOnly, volatileContext(shell, time.Now()))})
		fmt.Println()
		fmt.Println(lastAssistantText(msgs))
		view.SessionSummary(a.lastPrompt)
		return
	}

	if shellMode {
		if err := sh.run(context.Background()); err == nil {
			return
		} else {
			// 外壳拿不下这个终端。不致命：掉下去走朴素行提示符，反正 --no-tui
			// 给的也是这个。一个因为画不出状态栏就不肯跑的工具，比一个什么都
			// 不画的工具更糟。
			fmt.Fprintf(os.Stderr, "the interactive shell could not start (%v); using the plain prompt\n", err)
			if provider == nil {
				tui.Die(provErr)
			}
		}
	}

	fmt.Printf("stage 06 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, pcfg.Protocol, pcfg.Model, wd)

	var msgs []Msg
	for {
		fmt.Print("\n> ")
		if !stdin.Scan() {
			break
		}
		line := strings.TrimSpace(stdin.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		if handled, next := a.command(line, msgs); handled {
			msgs = next
			continue
		}

		bus.Emit(Event{Kind: KindUserMessage, Text: line})
		// 易变快照在**这里**取，只取一次，然后冻进消息里。它永不重算——
		// 会话知道现在几点，缓存却还活着，全靠这一点。
		msgs = append(msgs, userTurn(line, volatileContext(shell, time.Now())))
		msgs = a.runTurn(msgs)
	}
	view.SessionSummary(a.lastPrompt)
}

// useShell 决定要不要画界面。
//
// 有四条路会落到朴素行提示符上，而每一条都是某个人真实的处境：--no-tui 给的
// 是想要各章讲的那个循环的读者，-p 给的是脚本，管道喂进来的 stdin 则因为
// `echo hi | agent` 从阶段 00 起就一直能用，而一个全屏界面会把每个这么干的
// 脚本弄坏，还有 TERM=dumb——一个终端说自己干不了这个，那是实话。
//
// stdin 之外也查了 stdout。终端还接着、却把输出重定向进文件，就是这么拿到一
// 份满是转义序列的日志，外加一块在你的 shell 上面反复重画的屏幕。
func useShell(noTUI bool, printOnly string) bool {
	if noTUI || printOnly != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	fo, err := os.Stdout.Stat()
	return err == nil && fo.Mode()&os.ModeCharDevice != 0
}

// command 处理斜杠命令。它们是为 docs/05-live-forever.md 里的实验准备
// 的：压缩只在窗口快满时才触发，这很难演示，更难测试。
func (a *agent) command(line string, msgs []Msg) (bool, []Msg) {
	switch {
	case line == "/help":
		fmt.Fprintln(a.out, "  /compact          compact the conversation now")
		fmt.Fprintln(a.out, "  /remember <note>  append a line to "+memoryFileForWriting)
		fmt.Fprintln(a.out, "  /context          show what the conversation currently costs")
		return true, msgs

	case line == "/compact":
		base := len(a.system()) + toolChars()
		cut, why := a.comp.plan(msgs, base)
		if cut < 0 {
			a.bus.Notice("%s", why)
			return true, msgs
		}
		out, err := a.comp.run(a.p, a.httpc, a.bus, msgs, cut, base)
		if err != nil {
			a.bus.Error("compaction failed: %v — the conversation is unchanged", err)
		}
		return true, out

	case line == "/context":
		base := len(a.system()) + toolChars()
		fmt.Fprintf(a.out, "  %d messages · %d chars of history + %d chars of system/tools\n",
			len(msgs), convChars(msgs), base)
		fmt.Fprintf(a.out, "  estimated prompt: ~%d tokens at %.2f chars/token (%d calibration samples)\n",
			a.comp.estimate(msgs, base), a.comp.est.ratio, a.comp.est.obs)
		if a.lastPrompt > 0 {
			fmt.Fprintf(a.out, "  last call actually billed: %d prompt tokens\n", a.lastPrompt)
		}
		if problem := validConversation(msgs); problem != "" {
			fmt.Fprintf(a.out, "  MALFORMED: %s\n", problem)
		}
		return true, msgs

	case strings.HasPrefix(line, "/remember "):
		note := strings.TrimSpace(strings.TrimPrefix(line, "/remember "))
		if err := remember(a.memoryDir, note); err != nil {
			a.bus.Error("could not write memory: %v", err)
			return true, msgs
		}
		a.bus.Notice("noted in %s — it takes effect next session, not this one", memoryFileForWriting)
		return true, msgs
	}
	return false, msgs
}

// toolChars 是工具定义的字符开销。工具定义是每个 prompt 的一部分，而估
// 算器本来看不见它们。
func toolChars() int {
	n := 0
	for _, t := range []Tool{bashToolDef()} {
		n += len(t.Name) + len(t.Description) + 200 // schema 大概就这么大
	}
	return n
}

// call 做一次模型调用。
func (a *agent) call(turn int, msgs []Msg) (*CallResult, error) {
	req, body, err := a.p.BuildRequest(a.system(), msgs, []Tool{bashToolDef()}, 4096)
	if err != nil {
		return nil, err
	}
	a.bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	started := time.Now()
	resp, err := a.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return a.p.ParseStream(resp.Body, a.bus, turn, started)
}

func (a *agent) runTurn(msgs []Msg) []Msg {
	for turn := 1; ; turn++ {
		if turn > a.cfg.maxTurns {
			a.bus.Notice("stopped: hit the %d-turn limit", a.cfg.maxTurns)
			return msgs
		}

		// ---- 撞墙检查 -------------------------------------------------
		//
		// 它放在**这里**，工具循环的开头，不是用户循环的开头。填满上下文
		// 窗口的不是对话，是一个回合内部的工具输出：一条 `find /` 就能顶上
		// 一个多小时的聊天。只在用户消息之间检查，意味着墙是在回合中途
		// 撞上的——而那正是唯一没有优雅退路的地方。
		base := len(a.system()) + toolChars()
		if est := a.comp.estimate(msgs, base); a.comp.due(est) {
			cut, why := a.comp.plan(msgs, base)
			if cut < 0 {
				a.bus.Notice("%s", why)
			} else if out, err := a.comp.run(a.p, a.httpc, a.bus, msgs, cut, base); err != nil {
				a.bus.Error("compaction failed: %v — continuing uncompacted", err)
			} else {
				msgs = out
			}
		}

		a.bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		sentChars := convChars(msgs) + base
		res, err := a.call(turn, msgs)
		if err != nil {
			a.bus.Error("%v", err)
			return msgs
		}
		a.lastPrompt = res.Usage.Prompt()
		// 校准。Agent 能在不自带 tokenizer 的情况下决定何时压缩，全靠这一
		// 点：服务端刚刚告诉了我们，发出去的那些字符最后变成了多少 token。
		a.comp.est.observe(sentChars, res.Usage.Prompt())

		am := Msg{Role: RoleAssistant}
		if res.Text != "" {
			am.Blocks = append(am.Blocks, Block{Kind: BlockText, Text: res.Text})
		}
		am.Blocks = append(am.Blocks, res.Calls...)

		// 模型可以什么都不返回——没有文本，没有工具调用——把它追加进去，
		// 就得到一条 content 数组为空的消息，而 Anthropic 协议要到*下一次*
		// 请求才拒绝它。阶段 04 就潜伏着这个问题；是 compact.go 里的
		// validConversation() 把它挖出来的。
		if len(am.Blocks) == 0 {
			a.bus.Notice("the model returned an empty response (wire: %q) — not adding it to the history", res.RawStop)
			return msgs
		}
		msgs = append(msgs, am)

		switch res.Stop {
		case StopMaxTokens:
			a.bus.Notice("the model was cut off at max_tokens (wire: %q)", res.RawStop)
			if len(res.Calls) == 0 {
				return msgs
			}
			msgs = append(msgs, a.resultsMsg(turn, res.Calls,
				func(Block) string {
					return "[not executed: your reply was cut off at max_tokens. Retry with a shorter command.]"
				}))
			continue

		case StopFiltered:
			a.bus.Notice("the provider filtered this response (wire: %q)", res.RawStop)
			return msgs

		case StopUnknown, "":
			a.bus.Notice("unknown stop reason %q — treating the turn as finished", res.RawStop)
			return msgs
		}

		if len(res.Calls) == 0 {
			a.bus.Emit(Event{Kind: KindTurnEnd, Turn: turn})
			return msgs
		}

		results := Msg{Role: RoleUser}
		stop := false
		for _, c := range res.Calls {
			if stop {
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, "[not executed: the session was stopped.]"))
				continue
			}
			command, err := parseBashArgs(c.Args)
			if err != nil {
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, fmt.Sprintf("[%v]", err)))
				continue
			}
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name, Command: command})

			v, why := a.g.ask(command)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID,
					"[the user denied this command. Do not retry it unchanged.]"))
				continue
			case abort:
				stop = true
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, "[the user stopped the session.]"))
				continue
			}

			a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: c.ID, Command: command})
			r := runBash(a.cfg.shell, command, a.cfg.timeout)
			rendered, truncated := r.render(a.cfg.maxOutput)
			a.bus.Emit(Event{
				Kind: KindCommandEnd, Turn: turn, ToolID: c.ID, Command: command,
				ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
				Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
			})
			results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, rendered))
		}
		msgs = append(msgs, results)
		if stop {
			return msgs
		}
	}
}

func (a *agent) emitResult(turn int, callID, content string) Block {
	a.bus.Emit(Event{Kind: KindToolResult, Turn: turn, ToolID: callID, Text: content})
	return ToolResultBlock(callID, content)
}

func (a *agent) resultsMsg(turn int, calls []Block, text func(Block) string) Msg {
	m := Msg{Role: RoleUser}
	for _, c := range calls {
		m.Blocks = append(m.Blocks, a.emitResult(turn, c.ID, text(c)))
	}
	return m
}
