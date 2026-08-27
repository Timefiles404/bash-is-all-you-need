// 阶段 05——Live Forever：Agent 主循环，配上熬得过去的上下文窗口。
//
// 三层视野，而每个 Agent 三层都需要：
//
//	一次请求之内 —— messages 数组。阶段 00–04。
//	一次会话之内 —— 上下文压缩，数组长过窗口的时候。
//	跨会话       —— 就是个文件。机制全在这儿。
//
// 跟阶段 04 的差异不大，落在三个地方：系统提示词里现在带上了记忆和稳定
// 环境（memory.go）；每个用户回合会在旁边冻结一份易变快照；工具循环的
// 开头会检查对话是不是快撞墙了（compact.go）。
//
// 有一处结构变动值得说：活得长的那几样——provider、总线、权限闸、
// config、compactor——搬到了 `agent` 结构体上。阶段 04 的 runTurn 要八
// 个参数，阶段 05 还要再加三个。这里的接收者不是抽象，就是同一堆值，
// 换了个短一点的名字。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
	in           *bufio.Scanner
	available    bool
	out          io.Writer
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
	if !g.available {
		return deny, "no terminal to ask on — rerun with --yolo to allow commands"
	}
	fmt.Fprintf(g.out, "  run? [y / n / a = all / q = stop] ")
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

		noCache    = flag.Bool("no-cache", false, "omit cache_control breakpoints (stage 04 control arm)")
		breakCache = flag.Bool("break-cache", false, "put a fresh timestamp in the system prompt on every request (stage 04)")

		// 阶段 05。
		compactAt = flag.Float64("compact-at", 0.70, "compact when the estimated prompt passes this fraction of the window")
		keepAt    = flag.Float64("keep", 0.30, "fraction of the window to leave in place after compacting")
		noCompact = flag.Bool("no-compact", false, "never compact — ride the window until the API refuses (control arm)")
		window    = flag.Int("window", 0, "override the provider's context window, in tokens")
		noMemory  = flag.Bool("no-memory", false, "do not read AGENTS.md / MEMORY.md")
	)
	cfg := config{}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds per user message")
	flag.BoolVar(&cfg.yolo, "yolo", false, "run every command without asking")
	flag.Parse()

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

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}

	wd, _ := os.Getwd()
	fmt.Printf("stage 05 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, provider.Protocol(), provider.Model(), wd)

	// ---- 系统提示词，一次装配好 -----------------------------------------
	//
	// 这里的一切在整个会话里都是稳定的，这才让它有资格待在缓存断点之前。
	// 会动的东西一律进消息流——见 memory.go 的放置规则。
	memory := ""
	if !*noMemory {
		memory, _ = loadMemory(wd, bus)
	}
	full := basePrompt + "\n\n" + stableContext(shell, wd) + memoryPrompt
	if memory != "" {
		full += "\n\n" + memory
	}
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
		p: provider, httpc: &http.Client{Timeout: 10 * time.Minute},
		g:   &gate{yolo: cfg.yolo, in: stdin, available: interactive, out: os.Stdout},
		bus: bus, cfg: cfg, comp: comp, system: sys, memoryDir: wd,
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
		// 易变快照在**这里**取，只取一次，然后冻进消息里。它永不重算——
		// 会话知道现在几点，缓存却还活着，全靠这一点。
		msgs = append(msgs, userTurn(line, volatileContext(shell, time.Now())))
		msgs = a.runTurn(msgs)
	}
	view.SessionSummary(a.lastPrompt)
}

// command 处理斜杠命令。它们是为 docs/05-live-forever.md 里的实验准备
// 的：压缩只在窗口快满时才触发，这很难演示，更难测试。
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
		out, err := a.comp.run(a.p, a.httpc, a.bus, msgs, cut, base)
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
