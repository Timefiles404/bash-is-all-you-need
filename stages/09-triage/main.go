// 阶段 09 — 分诊：第二阶段的第一个阶段，从这里开始出错。
//
// 一个想法，它就住在 triage.go 里：**一个错误是一个决策，不是一个字符串。**
// 一次失败的模型调用会被分类成重试 / 降级 / 停，而分类器扎根于
// docs/wire-notes.md §D11，那里两条显而易见的规则——"401 说明密钥是坏的"和
// "5xx 是瞬态的"——两条都是错的。
//
// 相比阶段 07 的差异，是一个新文件和一个变了的错误类型。原本是
//
//	res, err := a.call(turn, msgs)
//	if err != nil { a.bus.Error("%v", err); return msgs }
//
// 的地方，现在是 callWithRetry；而 call() 本身作为 modelCall 搬进了
// triage.go，好让阶段 05 的摘要器——它是一次真实的模型调用，也是每个 Agent 都忘
// 了给它装仪表的那一次——走同一套决策，而不是自己那份更差的副本。
//
// 注意这个分叉点：第二阶段接的是阶段 07，不是阶段 08。阶段 08 是这个仓库唯一的
// 依赖，而且它明说是可选的；把它一路带下主干，会悄悄让它变成必需。
//
// 第一阶段建起来的东西没有变。子 Agent 仍然只是一次函数调用，它的返回值是一段
// 话——只是现在它和父 Agent 共享一把梯子，因为"这个端点在拒绝调用"是一个关于端
// 点的事实。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
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

// memoryPrompt 是整个长期记忆特性。
//
// 没有工具，没有存储，没有嵌入，
// 没有检索步骤：一个文件，加一句
// 话，告诉模型可以用它已有的工具，
// 把内容追加进这个文件。最后一行
// 是决定文件是否值得在六个月后读
// 的部分——"记录你学到的，不是
// 你做了什么"是知识库和日记之间
// 的区别。
const memoryPrompt = `

Durable notes live in ` + memoryFileForWriting + ` in the working directory. If that file
exists, its contents are already in your context above.

When you learn something about this project that would cost you tool calls to
rediscover in a future session — a build command, where something lives, a
gotcha, a decision the user made — append it:

  printf '\n- <one short factual line>\n' >> ` + memoryFileForWriting + `

Record what you learned, not what you did. Notes written now take effect in your
next session, not this one.`

// para 是一个空行。它之所以被写成一个常量，而不是在每个用到的地方直接
// 写字面量，只是因为它要在四个地方保持一致：父 Agent 看到的系统提示词，
// 和每一个子 Agent 看到的系统提示词，必须在第一段之后逐字节相同，
// 否则两者就无法共享缓存前缀。
var para = string([]rune{0x0A, 0x0A})

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
	subTurns  int
	yolo      bool
}

// ---------------------------------------------------------------------------
// 权限闸。自阶段 01 以来未改，除了它通过
// 总线报告。
// ---------------------------------------------------------------------------

type gate struct {
	yolo, always bool
	in           *bufio.Scanner
	available    bool
	out          io.Writer

	// 一次只问一个问题，而这一点，是真的存在并发争用的。
	//
	// 这个注释的第一个版本声称不会有争用：dispatch() 会在同一个 goroutine
	// 上问完每一个问题，然后才开始任何并发，所以父 Agent 的问题都是串行的。
	// 那个推理是错的，而错在哪里，比这把锁本身更值得琢磨。
	//
	// 子 Agent 在自己的 goroutine 上运行同一个 dispatch()，它的 bash 调用
	// 会去问这同一个共享门——就在并发进行的过程中，和它的兄弟们一起。这把
	// 锁能阻止两个提示词逐字交错地打印出来。但它挡不住更糟的情况，因为
	// 命令文本和问题，是经由不同的路径、在不同的锁下，分别到达终端的：
	//
	//	命令   由**渲染器**打印，不经过总线，在 bus.core.mu 下
	//	问题   在**这里**打印，在 gate.mu 下
	//
	// 两个锁、一个终端，它们之间没有任何排序。修复的办法不是加第三把锁——
	// 而是在下面的 ask() 里，让问题自己点明它问的是哪条命令。
	mu sync.Mutex
}

type verdict string

const (
	allow verdict = "allow"
	deny  verdict = "deny"
	abort verdict = "abort"
)

func (g *gate) ask(command string) (verdict, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.yolo || g.always {
		return allow, ""
	}
	if !g.available {
		return deny, "no terminal to ask on — rerun with --yolo to allow commands"
	}
	// 问题会说清楚，自己问的是哪条命令。
	//
	// 直到阶段 07 之前，都不是这样，而且在那之前，也确实不需要这样：在
	// 严格顺序的 print-then-ask 循环下，"run?" 只能指上面的那一行。并发
	// 子 Agent 抹掉了这个保证，而它留下的这个问题，不是什么显示上的小
	// 故障：
	//
	//	│ $ rm -rf /tmp/build            <- 子 Agent A 的命令，通过总线
	//	│ $ echo hello                   <- 子 Agent B 的命令，通过总线
	//	  run? [y / n / a = all / q]     <- 子 Agent A 的问题
	//
	// 用户是在为自己刚读到的那条命令作答，却顺带把另一条也批准了。一句话
	// 只要点明自己问的是哪条命令，不管屏幕上还有什么，都不会被看错——
	// 代价不过是多出一行。
	//
	// 还要注意 `a` 现在老实交代的是什么。它设置的 `always` 是在**共享**
	// 门上，所以只要有一个子 Agent 点了"允许全部"，父 Agent 和其他每一个
	// 兄弟 Agent 的门就都跟着解除了。把它的作用域收窄到每个 Agent 自己，
	// 会更安全，但也意味着每个子 Agent 都要再被问一遍——而这正是人们最后
	// 干脆一路 --yolo 跑下去的原因。选择保留；提示词停止隐藏它。
	fmt.Fprintf(g.out, "  run? %s\n  [y / n / a = all, this session, every agent / q = stop] ",
		oneLineDim(command, 72))
	if !g.in.Scan() {
		return abort, "input closed"
	}
	switch strings.ToLower(strings.TrimSpace(g.in.Text())) {
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

// ---------------------------------------------------------------------------

// agent 装着贯穿整个会话生命周期、
// 始终存在的一切。
type agent struct {
	// 阶段 09 里，lad 取代了原来那个朴素的 `p Provider`。一个会话不再有*一个*
	// 供应商；它有一个有序列表和一个当前位置，而这个位置可以在会话中途移动。原
	// 本读 a.p 的地方现在全都走 a.prov()，那是离真相一把锁的距离，而不是一个
	// "启动时曾经为真"的字段。
	lad   *ladder
	pol   retryPolicy
	httpc *http.Client
	g     *gate
	bus   *Bus
	cfg   config
	comp  *compactor

	// system 是函数，不是字符串，因为
	// 阶段 04 的 --break-cache 实验：
	// 启动时计算一次的值是常数前缀，
	// 只有每个请求重计算的值使任何东西
	// 无效。保持间接使那个区别可表达。
	system func() string

	memoryDir  string
	lastPrompt int

	// stable 是环境 + 记忆 + 技能这一块，逐字共享给每一个子 Agent。只
	// 计算一次；至于为什么绝不能重新计算，参见阶段 05 的位置规则。
	stable string

	// 阶段 07。
	depth    int // 0 是与人类交谈的 Agent
	maxDepth int
	subTurns int

	mu        sync.Mutex
	children  int
	spent     Usage // 这个 Agent 自己的 token 消耗，用于子 Agent 报告
	turnsUsed int
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

		// 阶段 06。Composer 是一个**读者**，所以它只需要一个路径，其他什么都不需
		// 要——没有密钥，没有供应商，没有网络。那不是限制，而是阶段 02 决定让
		// trace 成为真实来源、而不是调试日志的收获。
		composerAt = flag.String("composer", "", "open the TUI on a trace file instead of running the agent")

		// 同样的视图，印出来，而不是画出来。
		//
		// 这不是调试舱口。只要你想 diff、grep、把内容粘贴进一个 issue，或者在 CI
		// 里检查，TUI 就是一条死路；而"模型在第 12 次调用中看到了什么"这类问题，
		// 你恰恰想要一个能丢进管道里处理的答案。渲染和绘制原本就是两个独立的函数
		// （views.go 返回文本行；term.go 把它们画出来），所以这里只多花八行代码——
		// 这就是不让 UI 独占数据的回报。
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

		// 阶段 07。
		maxDepth = flag.Int("max-depth", 1, "how deep subagents may nest; 0 removes the task tool entirely")
		noSkills = flag.Bool("no-skills", false, "do not index skills/*/SKILL.md")

		// 进程外的子 Agent，用于 docs/07 里的对比：一个 prompt 进去，一个报告
		// 出来，没有 REPL——这就是子 Agent 从头到尾的全部，也正是为什么在 bash
		// 里运行 `agent --subagent "..."`，本身就是一套能用的子 Agent 机制，
		// 完全不需要任务工具参与。
		subagentAt = flag.String("subagent", "", "run one subagent task, print its report, and exit")

		// 阶段 09。三个数字，而默认值本身就是论点：重试是开着的，因为一个 429
		// 就丢掉一个回合，比停两秒更糟；重试也是有界的，因为比丢掉一个回合更糟
		// 的，只有一个悄无声息地花掉四分钟、六个 prompt，却什么也没换来的回
		// 合。
		fallbackTo  = flag.String("fallback", "", "provider names to fall back to, in order, comma-separated")
		retries     = flag.Int("retry", 3, "attempts per provider on a retryable failure; 1 disables retrying")
		retryBudget = flag.Duration("retry-budget", 30*time.Second, "total time one call may spend waiting between attempts")
	)
	cfg := config{}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds per user message")
	flag.IntVar(&cfg.subTurns, "sub-turns", 15, "tool-call rounds a subagent gets")
	flag.BoolVar(&cfg.yolo, "yolo", false, "run every command without asking")
	flag.Parse()

	// 首先要说清楚：Composer 从来不需要供应商；要是非让它等一个供应商，就意
	// 味着你没法在一台没配密钥的机器上读 trace——可那恰恰是你最想读 trace 的
	// 大多数机器。
	if *dumpAt != "" {
		if err := dumpComposer(*dumpAt, *dumpView, *dumpCall, *dumpW, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *composerAt != "" {
		if err := runComposer(*composerAt); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	pf, err := loadProviders(*providersAt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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

	// resolveErr 这里故意**不是**致命的。
	//
	// 重放不需要密钥、不需要 shell、不需要网络，也不需要
	// 供应商——那个承诺是阶段 02 的，它在 README 中，
	// 从阶段 03 直到这行被写它是假的：resolve() 移动到
	// 重放分支上方，并把它的 os.Exit(1) 带走了。
	// 在一台设置了 env vars 的机器上（这是作者测试过的
	// 每台机器），什么看起来都没错。在一台只有 trace 文件、
	// 别无他物——这正是该功能存在的机器上——
	// `--replay` 打印"no provider configured"。
	//
	// 所以错误被携带而不是被抛出，并在下面检查，
	// 在唯一实际需要供应商的路径上。配置错误应该只对
	// 依赖配置的代码致命，对其余代码则完全没有影响。
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
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := Replay(events, view, ReplayOpts{Speed: *speed, Step: *step}, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if resolveErr != nil {
		fmt.Fprintln(os.Stderr, resolveErr)
		os.Exit(1)
	}
	provider, err := pcfg.build(!*noCache)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lad, err := buildLadder(pf, pname, pcfg, provider, *fallbackTo, !*noCache)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pol := retryPolicy{attempts: *retries, base: 500 * time.Millisecond, max: 8 * time.Second, budget: *retryBudget}
	shell, err := findBash()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg.shell = shell

	bus := NewBus(view)
	if *tracePath != "" {
		tw, err := NewTraceWriter(*tracePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer tw.Close()
		bus.Subscribe(tw)
	}

	// trace 里的第一个事实：谁在服务这个会话，什么价钱。
	//
	// 阶段 09 之前，trace 里根本没有记下供应商。你可以把一个会话的每个字节都读
	// 一遍——每个请求体、每个 token 计数——却说不出它是哪个端点产出的，这让归档
	// trace 里的成本数字变得无法重建。它在 trace writer 订阅之后才发出，好让文
	// 件也拿到它；而这和后面一次降级发出的是同一个事件：一种 kind，一个意思，
	// "现在是这一位在应答"。
	_, _, pinfo := lad.pos()
	bus.Emit(Event{Kind: KindProvider, Provider: &pinfo})

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}

	wd, _ := os.Getwd()
	fmt.Printf("stage 09 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, provider.Protocol(), provider.Model(), wd)

	// ---- 系统提示词，一次组装 -----------
	//
	// 这里的一切，在整个会话期间都
	// 不会变——这就是它们能被排在
	// 缓存断点之前的原因。会变的东西，
	// 则进入消息流——参见 memory.go
	// 的放置规则。
	memory := ""
	if !*noMemory {
		memory, _ = loadMemory(wd, bus)
	}

	// 技能：只有名称和描述。正文一直留在磁盘上，直到模型判定某个技能
	// 适用，用 cat 去读它——这就是渐进披露的全部内容，也是四十个技能的
	// 成本，和一个技能的成本一样的原因。
	var skills []skill
	if !*noSkills {
		skills = loadSkills(wd)
	}
	if len(skills) > 0 {
		idx, bodies := skillsCost(skills)
		bus.Emit(Event{Kind: KindSkillsIndexed, Bytes: idx, TokensBefore: bodies,
			Text: fmt.Sprintf("%d skills", len(skills))})
	}

	// stable 是在进程运行期间不会改变的一切，它会逐字共享给每一个子
	// Agent。只组装一次——参见阶段 05。
	stable := stableContext(shell, wd) + memoryPrompt
	if memory != "" {
		stable += para + memory
	}
	stable += skillsPrompt(skills)

	full := basePrompt + para + stable
	sys := func() string { return full }
	if *breakCache {
		sys = func() string {
			return "Current time: " + time.Now().Format(time.RFC3339Nano) + "\n\n" + full
		}
		fmt.Println("--break-cache: a fresh timestamp goes into the system prompt on every request")
	}

	comp := newCompactor(pcfg.Window, *compactAt, *keepAt)
	if *noCompact {
		comp.threshold = 0
	}
	if pcfg.Window <= 0 && !*noCompact {
		fmt.Println("note: this provider has no `window` configured, so compaction can never fire. Set it, or pass --window.")
	}

	a := &agent{
		lad: lad, pol: pol, httpc: &http.Client{Timeout: 10 * time.Minute},
		g:   &gate{yolo: cfg.yolo, in: stdin, available: interactive, out: os.Stdout},
		bus: bus, cfg: cfg, comp: comp, system: sys, memoryDir: wd,
		stable: stable, maxDepth: *maxDepth,
	}

	// --subagent：一个任务，一个报告，没有对话。
	//
	// 这就是进程外子 Agent 机制的全部，而且这套机制小到什么程度，值得你
	// 亲眼看看。一个能运行 bash 的 Agent，就能运行 `agent --subagent
	// "..."`，这意味着递归根本不需要 `task` 工具——shell 本身就是那个编排者。
	// docs/07 量的就是这一点要付出什么代价，而答案是：付出的不是 token，
	// 而是仪表盘上的每一个数字。
	if *subagentAt != "" {
		child := a.newChild("cli", func() string { return subagentSystem + para + stable })
		msgs := child.runTurn([]Msg{TextMsg(RoleUser, *subagentAt)})
		fmt.Println()
		fmt.Println(lastAssistantText(msgs))
		return
	}

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
		// 易变快照在**这里**被取出一次，
		// 冻结进这条消息。它永远不会
		// 重新计算，这正是缓存能在一个
		// 知道时间的会话里活下来的
		// 全部原因。
		msgs = append(msgs, userTurn(line, volatileContext(shell, time.Now())))
		msgs = a.runTurn(msgs)
	}
	view.SessionSummary(a.lastPrompt)
}

// command 处理斜线命令。它们是
// 为 docs/05-live-forever.md 中的
// 实验而存在的：只有窗口快满时
// 才触发的压缩，很难演示，
// 更难测试。
func (a *agent) command(line string, msgs []Msg) (bool, []Msg) {
	switch {
	case line == "/help":
		fmt.Println("  /compact          compact the conversation now")
		fmt.Println("  /remember <note>  append a line to " + memoryFileForWriting)
		fmt.Println("  /context          show what the conversation currently costs")
		return true, msgs

	case line == "/compact":
		base := len(a.system()) + toolChars()
		cut, why := a.comp.plan(msgs, base)
		if cut < 0 {
			a.bus.Notice("%s", why)
			return true, msgs
		}
		out, err := a.comp.run(a.prov(), a.pol, a.httpc, a.bus, msgs, cut, base)
		if err != nil {
			a.bus.Error("compaction failed: %v — the conversation is unchanged", err)
		}
		return true, out

	case line == "/context":
		base := len(a.system()) + toolChars()
		fmt.Printf("  %d messages · %d chars of history + %d chars of system/tools\n",
			len(msgs), convChars(msgs), base)
		fmt.Printf("  estimated prompt: ~%d tokens at %.2f chars/token (%d calibration samples)\n",
			a.comp.estimate(msgs, base), a.comp.est.ratio, a.comp.est.obs)
		if a.lastPrompt > 0 {
			fmt.Printf("  last call actually billed: %d prompt tokens\n", a.lastPrompt)
		}
		if problem := validConversation(msgs); problem != "" {
			fmt.Printf("  MALFORMED: %s\n", problem)
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

// toolChars 是工具定义的字符成本，
// 它们是每个 prompt 的一部分，
// 否则对估算器不可见。
func toolChars() int {
	n := 0
	for _, t := range []Tool{bashToolDef(), taskToolDef()} {
		n += len(t.Name) + len(t.Description) + 200 // 模式，足够接近
	}
	return n
}

// prov 是现在正在服务调用的那个供应商。见 agent.lad。
func (a *agent) prov() Provider {
	_, p, _ := a.lad.pos()
	return p
}

// callWithRetry 是一次模型调用，加上 triage.go 里的那些决策。
//
// 原来 call() 的函数体搬到了 triage.go 的 modelCall()，压缩器那份副本也搬到了
// 同一个地方。留在这里的，只是 Agent 说清楚它要什么被重试；而值得注意的是这有
// 多么少：循环、策略、分类器和梯子全都能在没有 HTTP 服务器的情况下测试，唯一需
// 要 Agent 的东西就是那个闭包。
func (a *agent) callWithRetry(turn int, msgs []Msg) (*CallResult, error) {
	return retryLoop(a.bus, turn, a.pol, a.lad, time.Sleep, nil,
		func(p Provider) (*CallResult, error) {
			return modelCall(p, a.httpc, a.bus, turn, a.system(), msgs, a.tools(), 4096)
		})
}

func (a *agent) runTurn(msgs []Msg) []Msg {
	for turn := 1; ; turn++ {
		if turn > a.cfg.maxTurns {
			a.bus.Notice("stopped: hit the %d-turn limit", a.cfg.maxTurns)
			return msgs
		}

		// ---- 墙检查 ---------------------------------
		//
		// 它在**这里**，在工具循环的顶部，
		// 而不是用户循环的顶部。填满
		// 上下文窗口的不是对话，而是
		// 一个回合里的工具输出：单单
		// 一个 `find /` 就能加上超过一
		// 小时的聊天量。只在用户消息
		// 之间检查，意味着撞墙会发生在
		// 回合中途——那正是唯一没有
		// 优雅恢复余地的地方。
		base := len(a.system()) + toolChars()
		if est := a.comp.estimate(msgs, base); a.comp.due(est) {
			cut, why := a.comp.plan(msgs, base)
			if cut < 0 {
				a.bus.Notice("%s", why)
			} else if out, err := a.comp.run(a.prov(), a.pol, a.httpc, a.bus, msgs, cut, base); err != nil {
				a.bus.Error("compaction failed: %v — continuing uncompacted", err)
			} else {
				msgs = out
			}
		}

		a.bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		sentChars := convChars(msgs) + base
		res, err := a.callWithRetry(turn, msgs)
		if err != nil {
			a.bus.Error("%v", err)
			return msgs
		}
		a.lastPrompt = res.Usage.Prompt()
		a.spent = addUsage(a.spent, res.Usage)
		a.turnsUsed = turn
		// 校准。这是 Agent 能决定何时压缩
		// 而不厂商化分词器的唯一原因：
		// 服务器刚刚精确告诉了我们，我们
		// 发送的那些字符变成了多少个
		// token。
		a.comp.est.observe(sentChars, res.Usage.Prompt())

		am := Msg{Role: RoleAssistant}
		if res.Text != "" {
			am.Blocks = append(am.Blocks, Block{Kind: BlockText, Text: res.Text})
		}
		am.Blocks = append(am.Blocks, res.Calls...)

		// 模型可以返回什么都没有——无文本，
		// 无工具调用——附加它产生一条消息，
		// 带空内容数组，Anthropic 协议在
		// **下一个**请求拒绝。阶段 04 里
		// 这个问题就已经埋下了；
		// compact.go 中的 validConversation()
		// 是发现它的。
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

		// 一行，阶段 06 有 40。dispatch() 运行这一回合中的每个工具调用——
		// 子 Agent 并发，其他一切按顺序——并按模型要求的顺序交回结果。
		blocks, stop := a.dispatch(turn, res.Calls)
		results := Msg{Role: RoleUser, Blocks: blocks}
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
