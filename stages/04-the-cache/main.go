// 阶段 03——Babel：agent 主循环。
//
// 将这个文件与阶段 02 的 main.go 进行比较。差异是
// 整个章节：每个厂商词都消失了。没有 `tool_calls`、
// 没有 `finish_reason`、没有 `input_tokens`、
// 没有 `chat/completions`。主循环用 Msg、Block 和
// StopReason 说话，Provider 在线上翻译。
//
// 像这样的抽象，检验标准不是它能不能编译通过，
// 而是添加第二个协议有没有改动这个文件。结果没有——
// 你正在读的这个主循环还是阶段 02 的那个，只是词汇表换了。
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

const systemPrompt = `You are a coding agent working in a terminal on the user's machine.

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
// 权限闸。自阶段 01 以来未改，除了它通过
// 总线报告。
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

		// docs/04-the-cache.md 里的两个对照组。两个都不属于一个
		// 真实的 Agent；它们存在，只是为了让这一章能把一个数字，
		// 安在原本就只是空口白话的建议上。
		noCache    = flag.Bool("no-cache", false, "omit cache_control breakpoints (control arm)")
		breakCache = flag.Bool("break-cache", false, "put a timestamp in the system prompt — the classic silent invalidator")
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

	view := newRenderer(os.Stdout, colorEnabled(os.Stdout),
		prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
			cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite},
		pcfg.Window)
	view.showRequest = *showReq

	// 重放不需要密钥、不需要 shell、不需要网络——
	// 现在也不需要供应商。一个针对一个协议记录的 trace
	// 相同地重放，因为记录的是事件，不是线上格式。
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
	g := &gate{yolo: cfg.yolo, in: stdin, available: interactive, out: os.Stdout}
	httpc := &http.Client{Timeout: 10 * time.Minute}

	wd, _ := os.Getwd()
	fmt.Printf("stage 04 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, provider.Protocol(), provider.Model(), wd)

	// --break-cache 演示的是缓存丢失最常见的那一种方式。
	//
	// 注意这是一个**函数**，在每个请求上都会重新求值，这个细节
	// 就是整个实验的关键。这个标志的第一版，只在启动时盖一次
	// 时间戳——缓存照样工作得完美无缺，因为一个在整个会话里
	// 恒定的值，就是那个会话恒定的前缀。实际会被人写出来的
	// bug，是塞在一个每次调用都会执行的 prompt 构建器里的
	// `datetime.now()`，只有这个版本才会让东西失效。
	//
	// 时间戳位于渲染出的前缀的**前面**，所以它在每一次请求上，
	// 都会让 tools、system，以及它们后面的每一条消息全部失效。
	// 不会报错，账单只会一路涨上去。
	sys := func() string { return systemPrompt }
	if *breakCache {
		sys = func() string {
			return "Current time: " + time.Now().Format(time.RFC3339Nano) + "\n\n" + systemPrompt
		}
		fmt.Println("--break-cache: a fresh timestamp goes into the system prompt on every request")
	}

	var msgs []Msg
	lastPrompt := 0
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
		bus.Emit(Event{Kind: KindUserMessage, Text: line})
		msgs = append(msgs, TextMsg(RoleUser, line))
		msgs, lastPrompt = runTurn(provider, httpc, g, bus, cfg, sys, msgs, lastPrompt)
	}
	view.SessionSummary(lastPrompt)
}

// call 执行一个模型调用。注意它不命名任何协议：
// 它要求供应商提供请求，发送它，并要求供应商解析回复。
func call(p Provider, httpc *http.Client, bus *Bus, turn int, system func() string, msgs []Msg) (*CallResult, error) {
	req, body, err := p.BuildRequest(system(), msgs, []Tool{bashToolDef()}, 4096)
	if err != nil {
		return nil, err
	}

	// 请求检查器，以及 trace 中唯一的记录是
	// 模型实际看到的。故意在翻译后发出：有趣的字节
	// 是在线上的那些，不是中立形式。
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	started := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// 值得在编写重试策略前知道：在这个网关上
		// 未知的 model id 返回 401（不是 404），格式错误的
		// body 返回 500。"每个 5xx 重试"会永远重试客户端 bug。
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return p.ParseStream(resp.Body, bus, turn, started)
}

func runTurn(p Provider, httpc *http.Client, g *gate, bus *Bus, cfg config, system func() string, msgs []Msg, lastPrompt int) ([]Msg, int) {
	for turn := 1; ; turn++ {
		if turn > cfg.maxTurns {
			bus.Notice("stopped: hit the %d-turn limit", cfg.maxTurns)
			return msgs, lastPrompt
		}
		bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		res, err := call(p, httpc, bus, turn, system, msgs)
		if err != nil {
			bus.Error("%v", err)
			return msgs, lastPrompt
		}
		lastPrompt = res.Usage.Prompt()

		// 为历史重建助手回合。思考故意不重放：
		// 都不要求回来，其中一个对此收费，
		// 它在 trace 中无论如何都在。
		am := Msg{Role: RoleAssistant}
		if res.Text != "" {
			am.Blocks = append(am.Blocks, Block{Kind: BlockText, Text: res.Text})
		}
		am.Blocks = append(am.Blocks, res.Calls...)
		msgs = append(msgs, am)

		switch res.Stop {
		case StopMaxTokens:
			bus.Notice("the model was cut off at max_tokens (wire: %q)", res.RawStop)
			if len(res.Calls) == 0 {
				return msgs, lastPrompt
			}
			msgs = append(msgs, resultsMsg(bus, turn, res.Calls,
				func(Block) string {
					return "[not executed: your reply was cut off at max_tokens. Retry with a shorter command.]"
				}))
			continue

		case StopFiltered:
			bus.Notice("the provider filtered this response (wire: %q)", res.RawStop)
			return msgs, lastPrompt

		case StopUnknown, "":
			// 永远不要将无法识别的状态视为成功。RawStop
			// 被打印因为字面字符串是唯一会告诉你
			// 实际发生了什么的东西。
			//
			// 空情况不是偏执：它是 StopReason 的零值，
			// 这是如果适配器曾经忘记调用 normaliseStop，
			// 或流在任何 stop 理由到达前死亡，你会得到的。
			// 没有它，"我们从没搞清楚生成为什么停止"就会落入
			// 和"模型说完了"相同的分支。
			bus.Notice("unknown stop reason %q — treating the turn as finished", res.RawStop)
			return msgs, lastPrompt
		}

		if len(res.Calls) == 0 {
			bus.Emit(Event{Kind: KindTurnEnd, Turn: turn})
			return msgs, lastPrompt
		}

		// 每个调用得到一个结果，包括被拒绝的：
		// 无应答的调用使**下一个**请求格式错误，
		// 可能几个用户消息之后。结果进入一个中立消息；
		// 每个适配器决定那在线上看起来像什么，它们完全不同。
		results := Msg{Role: RoleUser}
		stop := false
		for _, c := range res.Calls {
			if stop {
				results.Blocks = append(results.Blocks, emitResult(bus, turn, c.ID, "[not executed: the session was stopped.]"))
				continue
			}
			command, err := parseBashArgs(c.Args)
			if err != nil {
				results.Blocks = append(results.Blocks, emitResult(bus, turn, c.ID, fmt.Sprintf("[%v]", err)))
				continue
			}
			bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name, Command: command})

			v, why := g.ask(command)
			bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				results.Blocks = append(results.Blocks, emitResult(bus, turn, c.ID,
					"[the user denied this command. Do not retry it unchanged.]"))
				continue
			case abort:
				stop = true
				results.Blocks = append(results.Blocks, emitResult(bus, turn, c.ID, "[the user stopped the session.]"))
				continue
			}

			bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: c.ID, Command: command})
			r := runBash(cfg.shell, command, cfg.timeout)
			rendered, truncated := r.render(cfg.maxOutput)
			bus.Emit(Event{
				Kind: KindCommandEnd, Turn: turn, ToolID: c.ID, Command: command,
				ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
				Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
			})
			results.Blocks = append(results.Blocks, emitResult(bus, turn, c.ID, rendered))
		}
		msgs = append(msgs, results)
		if stop {
			return msgs, lastPrompt
		}
	}
}

// emitResult 发布工具结果并返回块以追加，
// 所以用户看到的和模型被告知的永远不能漂移。
func emitResult(bus *Bus, turn int, callID, content string) Block {
	bus.Emit(Event{Kind: KindToolResult, Turn: turn, ToolID: callID, Text: content})
	return ToolResultBlock(callID, content)
}

func resultsMsg(bus *Bus, turn int, calls []Block, text func(Block) string) Msg {
	m := Msg{Role: RoleUser}
	for _, c := range calls {
		m.Blocks = append(m.Blocks, emitResult(bus, turn, c.ID, text(c)))
	}
	return m
}
