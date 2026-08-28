// 阶段 11——畸形：在模型的输出和 exec.Command 之间只留一道关卡。
//
// 只有一个想法，就写在 toolcall.go 里：**每一次工具调用都要过同一道检查，
// 而且是在它能跑之前，也在它能被记住之前。**
//
// 模型产出的其他字段都是文本，文本错了不过是答得不好。参数不一样，因为参数
// 最后会落进一个进程里。工具调用因此是整个循环里唯一一处，"模型说了句怪话"
// 和"机器干了件事"是同一个事件——这个阶段做的全部事情，就是把这两件事分开。
//
// 跟阶段 10 的差别是一个新文件，加上每一处创建、重放、显示工具调用的地方各
// 改一点。checkCall 顶掉了阶段 10 那些一个工具一个的手写解析器；faultText
// 说明被拒的是什么，而且说得不带任何指令——那句话会一直留在 transcript 里，
// 四十个回合之后模型还在读它；uniqueIDs 修的是给每一次调用都发同一个 id 的
// 网关。
//
// # 名字是怎么来的
//
// 不是"JSON 非法"，那只是最吵的一种。参数不成其为参数，调用就是畸形的：在
// 字符串中间被截断、JSON 合法但形状不对、满足 schema 却什么也不指、或者同
// 一个 id 底下同一个调用来了两次。每种情况两个协议实际发的是什么，
// docs/11-malformed.md 一个字节一个字节地探过。
//
// # 第一部分和阶段 09、10 还照旧做什么
//
// 没变。被拒的调用就是一条普通的工具结果，所以 triage 根本看不见它，也不牵
// 涉任何期限。阶段 11 唯一往回够的地方是截断保险丝：模型在 max_tokens 里拼
// 不出一个完整的调用，下一次也拼不出来，所以循环必须停——而这个决定要交到
// 人手上。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
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

// para 就是一个空行。它之所以做成常量、而不是在每个用到的地方写字面量，只
// 是因为它出现在四个必须一致的位置：父 Agent 看到的系统提示词，和每个子
// Agent 看到的那份，第一段以下必须逐字节相同，否则两者就没有共同的缓存前
// 缀。
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
	maxTokens int
	maxTurns  int
	subTurns  int
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

	// 一次只问一个问题，而且它是真的会争。
	//
	// 这段注释的第一版声称它不会争：dispatch() 是在开始任何并发之前，在同一
	// 个 goroutine 上把所有问题问完的，所以父 Agent 的提问是串行的。那个推理
	// 错了，而它错的方式比这把锁本身更值钱。
	//
	// 子 Agent 在自己的 goroutine 上跑同一个 dispatch()，它的 bash 调用问的是
	// 同一个共享的权限闸——在并发中间，跟它的兄弟们一起问。这把锁挡住的是两
	// 段提示一个字符一个字符地交错。它挡不住更糟的事，因为命令文本和问题是走
	// 不同的路径、在不同的锁下面到终端的：
	//
	//	命令   由**渲染器**打印，从总线上下来，在 bus.core.mu 底下
	//	问题   在**这里**打印，在 gate.mu 底下
	//
	// 两把锁，一个终端，两者之间没有顺序。修法不是加第三把锁——修法在下面的
	// ask() 里，问题现在会自报它问的是谁。
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
	if !g.available || g.read == nil {
		return deny, "no terminal to ask on — rerun with --yolo to allow commands"
	}
	// 问题会点名它问的是哪条命令。
	//
	// 阶段 07 之前它不点名，而在阶段 07 之前它也不需要点：在严格串行的"先打
	// 印再询问"循环里，"run?" 只可能指它上面那一行。并发的子 Agent 把这个保
	// 证拿掉了，而留下的失败不是显示上的小毛病：
	//
	//	│ $ rm -rf /tmp/build            <- 子 A 的命令，经由总线
	//	│ $ echo hello                   <- 子 B 的命令，经由总线
	//	  run? [y / n / a = all / q]     <- 子 A 的问题
	//
	// 用户按自己刚读到的那条命令作答，于是授权了另外那条。自带主语的提示，不
	// 管屏幕上还有什么都不会被读错，代价是一行。
	//
	// 还要注意 `a` 现在对什么诚实了。它设的是**共享**权限闸上的 `always`，所
	// 以某个子 Agent 的"全部允许"，把父 Agent 和所有兄弟的权限闸一起卸了。按
	// Agent 分别限定会更安全，也意味着每个子 Agent 都要再问一遍，而人们就是这
	// 么最后跑上 --yolo 的。这个选择不改；只是提示不再瞒着它了。
	fmt.Fprintf(g.out, "  run? %s\n  [y / n / a = all, this session, every agent / q = stop] ",
		oneLineDim(command, 72))
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
	// 阶段 09 里，lad 取代了原来那个光秃秃的 `p Provider`。会话不再有*一个*
	// 供应商，它有一份有序的列表和一个当前位置，而这个位置会在会话中途移
	// 动。原先读 a.p 的地方现在全走 a.prov()——隔一把锁就是真相，而不是一个
	// 启动时为真的字段。
	lad   *ladder
	pol   retryPolicy
	dl    deadlines
	httpc *http.Client
	g     *gate
	bus   *Bus
	cfg   config
	comp  *compactor

	// system 是函数而不是字符串，原因在阶段 04 的 --break-cache 实验：启动
	// 时算一次的值是恒定前缀，只有每次请求都重算的值才会作废缓存。留着这
	// 一层间接，才能把这个差别表达出来。
	system func() string

	memoryDir  string
	lastPrompt int

	// stable 是环境 + 记忆 + 技能这一整块，逐字节共享给每个子 Agent。只算一
	// 次；为什么绝不能重算，见阶段 05 的摆放规则。
	stable string

	// 阶段 07。
	depth    int // 0 是人正在对话的那个 Agent
	maxDepth int
	subTurns int

	// 阶段 11。这个 Agent 往自己历史里放过的每一个工具调用 id。
	//
	// 作用域是会话，不是回合，关键正在这里：网关每铸一个调用都复用同一个 id，
	// 产出的重复分散在*不同的* assistant 消息里，按回合查什么都查不到，而协议
	// 照样以 `Found duplicate tool_use id` 拒掉整个请求。见 uniqueIDs。
	//
	// 不跟子 Agent 共用。子 Agent 有自己的消息数组，它的 id 只要在里面唯一就
	// 行；共用这张 map，两个并发的子 Agent 每次工具调用都得抢它。
	seenIDs map[string]bool

	// cutStreak 数的是连续多少个回合里，每一次工具调用都被判成截断而拒掉。见
	// maxCutStreak。
	cutStreak int

	// out 是这个文件自己往外写的去处——只有下面那些斜杠命令，别无其他。朴素行
	// 提示符下是 stdout，交互式外壳下是外壳的输出区。它存在，是因为在备用屏里
	// 光秃秃一句 fmt.Println 会落到帧的底下，把版面弄坏，而且永远不会被看见。
	out io.Writer

	mu        sync.Mutex
	children  int
	spent     Usage // 这个 Agent 自己的 token 消耗，给子 Agent 报告用
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

		// 阶段 07。
		maxDepth = flag.Int("max-depth", 1, "how deep subagents may nest; 0 removes the task tool entirely")
		noSkills = flag.Bool("no-skills", false, "do not index skills/*/SKILL.md")

		// 进程外的子 Agent，给 docs/07 里的对照用。一个 prompt 进去，一份报
		// 告出来，没有 REPL——子 Agent 从来就只是这些，也正因如此，从 bash
		// 里跑 `agent --subagent "..."` 就是一套能用的子 Agent 机制，全程没
		// 有 task 工具的事。
		subagentAt = flag.String("subagent", "", "run one subagent task, print its report, and exit")

		// 阶段 09。三个数字，而默认值本身就是论点：重试默认开着，因为单单
		// 一个 429 就丢掉一个回合，那比停两秒更糟；重试又是有界的，因为比
		// 丢掉回合更糟的，只有一声不响花掉四分钟六个 prompt 却什么都没干成
		// 的回合。
		fallbackTo  = flag.String("fallback", "", "provider names to fall back to, in order, comma-separated")
		retries     = flag.Int("retry", 3, "attempts per provider on a retryable failure; 1 disables retrying")
		retryBudget = flag.Duration("retry-budget", 30*time.Second, "total time one call may spend waiting between attempts")

		// 阶段 10。阶段 09 只有一个时钟，这里是三个；它们分成三个 flag，是
		// 因为它们回答的是不同的问题——见 deadline.go。任何一个都可以是 0，
		// 那就把那个时钟关掉；docs/wire-notes.md 里的线上探测需要三个全关，
		// 因为被中途砍断的探测不算证据。
		connectFor = flag.Duration("connect-timeout", 30*time.Second, "response headers must arrive within this")
		idleFor    = flag.Duration("stall-timeout", 45*time.Second, "longest tolerated gap between bytes of a stream")
		callFor    = flag.Duration("call-timeout", 15*time.Minute, "backstop on one whole model call, retries excluded")

		// 交互式外壳，在 tui/ 里。不是课程的一部分；见 shell.go。
		printOnly  = flag.String("p", "", "run one prompt without a UI, print the reply, and exit")
		noTUI      = flag.Bool("no-tui", false, "use the plain line prompt instead of the interactive shell")
		settingsAt = flag.String("settings", "", "path to the saved settings file; empty means the one under your user config directory")
	)
	cfg := config{}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	// 阶段 11 的实验旋钮，跟阶段 04 的 --break-cache 是一路货：被截断的工具调
	// 用不是干等就能等到的，想随叫随到地看一次，只能故意把预算调得太小。4096
	// 是之前每个阶段都写死的那个值。
	flag.IntVar(&cfg.maxTokens, "max-tokens", 4096, "output token budget per call; lower it to force a truncated tool call (stage 11)")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds per user message")
	flag.IntVar(&cfg.subTurns, "sub-turns", 15, "tool-call rounds a subagent gets")
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
	// 窗口，按这三个来源互相压过的顺序排：先是 flag，再是 /provider-window 存
	// 下来的，最后才是 providers.json 里写的。完全通过外壳配起来的会话在那个文
	// 件里没有条目，所以少了中间这一条，它就压根没有窗口——而没有窗口，压缩永远
	// 不会触发，状态栏上那个上下文字段一直是空的，屏幕上也没有任何东西说明为什
	// 么。
	switch {
	case *window > 0:
		pcfg.Window = *window
	case savedWindow() > 0:
		pcfg.Window = savedWindow()
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

	var lad *ladder
	provErr := resolveErr
	if provErr == nil {
		provider, err := pcfg.build(!*noCache)
		if err != nil {
			provErr = err
		} else if l, err := buildLadder(pf, pname, pcfg, provider, *fallbackTo, !*noCache); err != nil {
			provErr = err
		} else {
			lad = l
		}
	}
	if lad == nil && !shellMode {
		tui.Die(provErr)
	}

	pol := retryPolicy{attempts: *retries, base: 500 * time.Millisecond, max: 8 * time.Second, budget: *retryBudget}
	shell, err := findBash()
	if err != nil {
		// 在每种模式下都致命，外壳里也一样，而这样的东西极少。没有 shell 就意
		// 味着这个 Agent 唯一那件工具不存在，而没有任何一条斜杠命令能装一个出
		// 来。
		tui.Die(err)
	}
	cfg.shell = shell

	// 折叠订阅者写在渲染器前面，而这个顺序就是那份约定：Emit 按顺序派发给每个
	// 订阅者，所以它能赶在渲染器写之前，先说出渲染器接下来要写的是哪一类行。见
	// shell.go 里的 foldSink。
	folds := &foldSink{}
	bus := NewBus(folds, view)

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

	// trace 里的第一条事实：这场会话由谁来服务，什么价钱。
	//
	// 阶段 09 之前，trace 里根本没记供应商。你可以把一场会话的每个字节都读
	// 一遍——每个请求体、每个 token 计数——却说不出是哪个端点产出的；归档
	// 的 trace 里那些成本数字，也就因此没法复原。它在 trace writer 订阅之后
	// 才发出，好让文件也拿到；而它和降级稍后发的是同一个事件：一种事件，一
	// 个意思，"现在是这家在应答"。
	if lad != nil {
		_, _, pinfo := lad.pos()
		bus.Emit(Event{Kind: KindProvider, Provider: &pinfo})
	}

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
		pf:       pf, view: view, bus: bus, store: store, trace: traces, folds: folds,
		pname: pname, pcfg: pcfg, wd: wd,
		opts: shellOpts{
			provider: *providerName, fallback: *fallbackTo, cacheBP: !*noCache,
			window: *window, noMemory: *noMemory, noSkills: *noSkills,
			breakCache: *breakCache,
		},
	}

	// ---- 系统提示词 -----------------------------------------------------
	//
	// 它里面的一切在整个会话里都是稳定的，这才让它有资格待在缓存断点之前；会
	// 动的东西一律进消息流——见 memory.go 的放置规则。
	//
	// 在 tui/ 里那个外壳出现之前，它就是在这里当场拼好，只拼一次。现在这套装
	// 配是个函数，只为一个理由：/open 会换掉工作目录，而记忆文件和技能索引都
	// 是从那个目录里读的。依赖这个目录的东西一共四样，挪三样比一样都不挪更糟。
	sys, stable := sh.assemble(shell, wd)
	if *breakCache && !shellMode {
		fmt.Println("--break-cache: a fresh timestamp goes into the system prompt on every request")
	}

	comp := newCompactor(pcfg.Window, *compactAt, *keepAt)
	if *noCompact {
		comp.threshold = 0
	}
	if pcfg.Window <= 0 && !*noCompact && lad != nil && !shellMode {
		fmt.Println("note: this provider has no `window` configured, so compaction can never fire. Set it, or pass --window.")
	}

	dl := deadlines{connect: *connectFor, idle: *idleFor, total: *callFor}

	// 客户端自己不再有 Timeout 了，这就是那个改动。
	//
	// http.Client.Timeout 管着响应体读取，所以在流式响应上，它就是给"模型
	// 能说多久"设的上限——而那根本不是有人想设上限的东西。它里面 connect
	// 那一半搬去了 ResponseHeaderTimeout，那个停在响应头，不碰流；剩下的
	// 归 context 管。
	httpc := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: dl.connect,
		},
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	// 整个会话共用一个 context，结束它的是 Ctrl-C。
	//
	// signal.NotifyContext 会更短，但它取消时带的是 context.Canceled，分
	// 诊分不出它和卡住的区别。原因才是要点所在，所以这个 handler 是手写出
	// 来的。
	//
	// 外壳不装它，而这不是漏掉。原始模式会关掉 ISIG，所以在外壳底下，Ctrl-C 是
	// 以字节 0x03 从 stdin 进来的，不是以信号进来的——这里再装一个处理器，就成
	// 了同一次按键的第二个读者，含义还不一样，而且跟第一个抢。
	if !shellMode {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, os.Interrupt)
		go func() {
			<-sigc
			cancel(errInterrupted)
			// 第二次 Ctrl-C 不是第一次的加量。第一次是请 Agent 停下来，那意味
			// 着收摊：杀掉命令，关掉 trace，把账印出来。要是这个收摊本身卡住
			// 了，用户得有条出路，而这条出路不能依赖那段已经卡住的代码。
			signal.Stop(sigc)
		}()
	}

	a := &agent{
		lad: lad, pol: pol, dl: dl, httpc: httpc,
		g:   &gate{yolo: cfg.yolo, read: lineReader(stdin), available: interactive, out: os.Stdout},
		bus: bus, cfg: cfg, comp: comp, system: sys, memoryDir: wd,
		stable: stable, maxDepth: *maxDepth, out: os.Stdout,
		seenIDs: map[string]bool{},
	}
	sh.a = a

	// --subagent：一个任务，一份报告，没有对话。
	//
	// 进程外的子 Agent 机制全在这儿了，值得看看它有多小。跑得了 bash 的
	// Agent 就跑得了 `agent --subagent "..."`，这意味着递归压根不需要 `task`
	// 工具——shell 就是编排者。docs/07 量了这么做的代价，答案是：不是 token，
	// 而是仪表盘上的每一个数。
	if *subagentAt != "" {
		child := a.newChild("cli", func() string { return subagentSystem + para + stable })
		msgs := child.runTurn(ctx, []Msg{TextMsg(RoleUser, *subagentAt)})
		fmt.Println()
		fmt.Println(lastAssistantText(msgs))
		return
	}

	// -p：一条提示词，一份仪表盘，没有界面，退出。
	//
	// 把非交互的契约写成一个 flag，而不是听天由命地看 stdin 恰好是不是管道。外
	// 壳能做的每件事，这里都能通过 flag 做到；它唯一做不到的是问，所以权限闸被
	// 明确关掉，需要授权的命令会带着理由被拒——而不是挂在一个没人看的终端上。
	if *printOnly != "" {
		a.g.available = false
		bus.Emit(Event{Kind: KindUserMessage, Text: *printOnly})
		msgs := a.runTurn(ctx, []Msg{userTurn(*printOnly, volatileContext(shell, time.Now()))})
		fmt.Println()
		fmt.Println(lastAssistantText(msgs))
		view.SessionSummary(a.lastPrompt)
		return
	}

	if shellMode {
		if err := sh.run(ctx); err == nil {
			return
		} else {
			// 外壳拿不下这个终端。不致命：掉下去走朴素行提示符，反正 --no-tui
			// 给的也是这个。一个因为画不出状态栏就不肯跑的工具，比一个什么都
			// 不画的工具更糟。
			fmt.Fprintf(os.Stderr, "the interactive shell could not start (%v); using the plain prompt\n", err)
			if lad == nil {
				tui.Die(provErr)
			}
		}
	}

	fmt.Printf("stage 11 · provider=%s (%s) · model=%s\ncwd=%s\n",
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
		if handled, next := a.command(ctx, line, msgs); handled {
			msgs = next
			continue
		}

		bus.Emit(Event{Kind: KindUserMessage, Text: line})
		// 易变快照在**这里**取，只取一次，然后冻进消息里。它永不重算——
		// 会话知道现在几点，缓存却还活着，全靠这一点。
		msgs = append(msgs, userTurn(line, volatileContext(shell, time.Now())))
		msgs = a.runTurn(ctx, msgs)
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
func (a *agent) command(ctx context.Context, line string, msgs []Msg) (bool, []Msg) {
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
		out, err := a.comp.run(ctx, a.prov(), a.pol, a.httpc, a.bus, msgs, cut, base, a.dl)
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
	for _, t := range []Tool{bashToolDef(), taskToolDef()} {
		n += len(t.Name) + len(t.Description) + 200 // schema 大概就这么大
	}
	return n
}

// prov 是当下正在接调用的那家供应商。见 agent.lad。
func (a *agent) prov() Provider {
	_, p, _ := a.lad.pos()
	return p
}

// callWithRetry 就是一次模型调用，加上 triage.go 里的那些决策。
//
// 原来 call() 的函数体搬到了 triage.go 的 modelCall()，compactor 那份拷贝也
// 一起搬了过去。留在这里的，只是 Agent 说出它想重试什么；而值得注意的是
// 这有多么少：循环、策略、分类器和梯子，全都不需要 HTTP 服务器就能测，
// 唯一需要 Agent 的东西就是那个闭包。
func (a *agent) callWithRetry(ctx context.Context, turn int, msgs []Msg) (*CallResult, error) {
	return retryLoop(ctx, a.bus, turn, a.pol, a.lad, nil, nil,
		func(ctx context.Context, p Provider) (*CallResult, error) {
			return modelCall(ctx, p, a.httpc, a.bus, turn, a.system(), msgs, a.tools(), a.cfg.maxTokens, a.dl, nil)
		})
}

func (a *agent) runTurn(ctx context.Context, msgs []Msg) []Msg {
	for turn := 1; ; turn++ {
		if turn > a.cfg.maxTurns {
			a.bus.Notice("stopped: hit the %d-turn limit", a.cfg.maxTurns)
			return msgs
		}

		// 用户要它停，那就停——在开始任何别的事情之前。
		//
		// 这道检查一直缺着，直到交互式外壳把它显出来；而它此前为什么显不出
		// 来，比这个修法本身更值钱。外壳之前，打断的唯一办法是 Ctrl-C，它取消
		// 会话 context 并结束进程；收摊路上多打一次注定失败的模型调用，谁也看
		// 不出区别。外壳底下，会话扛得过一次打断，于是用户在一条正在跑的命令
		// 上按下 Escape 之后看到的，是命令被正确杀掉，紧接着两行红字说一次
		// HTTP POST 失败了——那正是这个循环在被叫停之后又去拨了一次模型的报告。
		if ctx.Err() != nil {
			a.bus.Notice("stopped: %v", context.Cause(ctx))
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
			} else if out, err := a.comp.run(ctx, a.prov(), a.pol, a.httpc, a.bus, msgs, cut, base, a.dl); err != nil {
				a.bus.Error("compaction failed: %v — continuing uncompacted", err)
			} else {
				msgs = out
			}
		}

		a.bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		sentChars := convChars(msgs) + base
		res, err := a.callWithRetry(ctx, turn, msgs)
		if err != nil {
			a.bus.Error("%v", err)
			return msgs
		}
		a.lastPrompt = res.Usage.Prompt()
		a.spent = addUsage(a.spent, res.Usage)
		a.turnsUsed = turn
		// 校准。Agent 能在不自带 tokenizer 的情况下决定何时压缩，全靠这一
		// 点：服务端刚刚告诉了我们，发出去的那些字符最后变成了多少 token。
		a.comp.est.observe(sentChars, res.Usage.Prompt())

		// 阶段 11——两处修补，都在消息被构造出来**之前**，因为消息一旦追加进
		// 去，这个会话剩下的每一次请求都带着它（§E14）。
		//
		// 第一处：id。改名必须在这儿做，不能挪进 dispatch，因为 dispatch 要拿
		// 这些 id 去建结果块；答案都已经存在了才给调用改名，得到的是孤立工具
		// 结果——同一个被拒的请求，换了条更没用的报错。
		if n := uniqueIDs(res.Calls, a.seenIDs); n > 0 {
			a.bus.Notice("%d tool call id(s) in this turn collided with earlier ones and were renamed", n)
		}

		// 第二处：markup 泄漏，§A2——而这一处还附一句老实话，说明它什么时候
		// 才可能触发。
		//
		// 模型在线上发的不是 JSON。它发的是一种类 XML 的宿主语法，
		// `<tool_call><function=bash><parameter=command>…`，由网关在服务端解析
		// 成工具调用。从语法中间截断，解析就失败，而 §A2 记下了那条兜底路径：
		// 原始 markup 直接落进 `message.content`。留着它，代价要付两遍——人看
		// 到的是网关内部的东西，却像是 assistant 说的；而历史则教会模型，在这
		// 儿把这套语法当散文写出来是正常的。
		//
		// 那句老实话是 §E15，为阶段 11 实测的，它修正了 §A2：那条兜底路径只
		// 在**不**流式的时候才有。走流式，解析是增量跑的，解析出多少就转发多
		// 少，所以客户端拿到的是残缺的参数 JSON，一点 markup 都没有。而这个
		// Agent 永远走流式。所以在这个端点上，下面这个分支根本触发不了；留着
		// 它的两条理由都是冲着别的端点去的：非流式那条路会立刻撞上 §A2 描述
		// 的形状，而一个糊涂到把工具调用语法当普通散文写的模型，在哪家供应商
		// 那儿都产出同样的字节。
		//
		// 它挂在 StopMaxTokens 上，不是每回合都跑，而这道闸有实打实的代价：
		// **没有**截断就泄漏出来的 markup 会被原样留着。这是笔有意为之的交
		// 易。截断的那一回合，文本按定义就是不完整的，切掉它不会丢掉模型说完
		// 了的任何东西；而在完整的一回合里，要是有个 Agent 被要求解释的正好
		// 是这套线上格式，它的回答会在它引用的第一个 `<tool_call>` 处被无声截
		// 断——而这个仓库自己的文档里，这玩意儿满地都是。
		text := res.Text
		if res.Stop == StopMaxTokens {
			if stripped, found := stripHarnessMarkup(text); found {
				a.bus.Emit(Event{
					Kind: KindToolCallInvalid, Turn: turn,
					Fault: string(faultNotJSON),
					Text:  "the gateway's own tool-call markup arrived as assistant text",
				})
				text = stripped
			}
		}

		am := Msg{Role: RoleAssistant}
		if text != "" {
			am.Blocks = append(am.Blocks, Block{Kind: BlockText, Text: text})
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
					// 这里不再写"换条短点的命令重试"。这段字符
					// 串会在之后的每一次请求里被重放，而历史里的
					// 祈使句，等它的上下文滚远了，读起来就成了一
					// 条新指令——见 faultText。
					return "[not executed: the reply was cut off at max_tokens]"
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

		// 一行，阶段 06 那里是四十行。dispatch() 跑掉这个回合里的每一次工具
		// 调用——子 Agent 并发，其余按顺序——再按模型要求的顺序把结果交回来。
		blocks, out := a.dispatch(ctx, turn, res.Calls)
		results := Msg{Role: RoleUser, Blocks: blocks}
		msgs = append(msgs, results)
		if out.stop {
			return msgs
		}

		// 截断保险丝。一个回合里**每一次**调用都被截断，连击就加一；只要有一
		// 次过了，就清零。
		//
		// 它存在，是因为"拒得对"最后被证明还不够。一次真实会话跑在
		// --max-tokens 110 下，发起了十六次模型调用，跑了零条命令，最后只被回
		// 合预算拦住：每一回合模型都被告知自己的调用被截断了，每一回合它又写
		// 出一条同样长的命令。这不是它犟——它看不见 max_tokens，所以"你被截断
		// 了"点的是一个它无从下手的原因，而换个说法是它唯一能做的动作。
		//
		// 所以这条消息改成说给人听，人能改那个数。定三，是因为连着两次还可能
		// 是模型缩短了命令又运气不好，三次就是规律了。
		if out.calls > 0 && out.cut == out.calls {
			a.cutStreak++
		} else {
			a.cutStreak = 0
		}
		if a.cutStreak >= maxCutStreak {
			a.bus.Error("%d turns in a row produced only truncated tool calls. The model cannot see the "+
				"output budget, so it will keep re-sending calls of the same length; raise --max-tokens "+
				"(currently %d)", a.cutStreak, a.cfg.maxTokens)
			return msgs
		}
	}
}

// maxCutStreak 是连续多少个"全被截断"的回合会结束主循环。
//
// 一根保险丝，跟 maxTurns、maxDepth 是一家的：它不是修法，只是给一个已知修不好
// 的循环划一条花费上限。maxTurns 最终也拦得住这件事，只不过是在 25 次调用之
// 后，而不是 3 次。
const maxCutStreak = 3

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
