// 阶段 03——Babel：Agent 主循环。
//
// 拿这个文件跟阶段 02 的 main.go 比一比。那份 diff 就是这一章的全部内容：所
// 有属于厂商的词都不见了。没有 `tool_calls`，没有 `finish_reason`，没有
// `input_tokens`，没有 `chat/completions`。主循环讲的是 Msg、Block 和
// StopReason，翻译由 Provider 在线上完成。
//
// 这种抽象好不好，不看它能不能编译过，看的是加上第二个协议之后这个文件有没
// 有变。它没变——你正在读的这个主循环就是阶段 02 那个，只是换掉了词汇。
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

	view := newRenderer(os.Stdout, colorEnabled(os.Stdout),
		prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
			cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite},
		pcfg.Window)
	view.showRequest = *showReq

	// 重放不需要 key、不需要 shell、不需要网络——现在也不需要供应商了。对着某
	// 个协议录下来的 trace，重放出来一模一样，因为录下的是事件，不是线上格式。
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
	provider, err := pcfg.build()
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
	fmt.Printf("stage 03 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, provider.Protocol(), provider.Model(), wd)

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
		msgs, lastPrompt = runTurn(provider, httpc, g, bus, cfg, msgs, lastPrompt)
	}
	view.SessionSummary(lastPrompt)
}

// call 执行一次模型调用。注意它没提到任何协议：向供应商要请求，发出去，再让
// 供应商去解析回复。
func call(p Provider, httpc *http.Client, bus *Bus, turn int, msgs []Msg) (*CallResult, error) {
	req, body, err := p.BuildRequest(systemPrompt, msgs, []Tool{bashToolDef()}, 4096)
	if err != nil {
		return nil, err
	}

	// 请求检查器；trace 里也只有它记下了模型到底看到什么。故意放在翻译之后才
	// 发：值得看的字节是真上了线的那些，不是中立形式。
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	started := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// 写重试策略之前值得先知道：在这个网关上，认不出的模型 id 返
		// 回的是 401（不是 404），畸形的 body 返回 500。
		// "5xx 一律重试"会把客户端的 bug 永远重试下去。
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return p.ParseStream(resp.Body, bus, turn, started)
}

func runTurn(p Provider, httpc *http.Client, g *gate, bus *Bus, cfg config, msgs []Msg, lastPrompt int) ([]Msg, int) {
	for turn := 1; ; turn++ {
		if turn > cfg.maxTurns {
			bus.Notice("stopped: hit the %d-turn limit", cfg.maxTurns)
			return msgs, lastPrompt
		}
		bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		res, err := call(p, httpc, bus, turn, msgs)
		if err != nil {
			bus.Error("%v", err)
			return msgs, lastPrompt
		}
		lastPrompt = res.Usage.Prompt()

		// 为历史重建 assistant 这一轮。thinking 故意不回放：两个协议
		// 都不要求把它送回去，其中一个还要为它收费，而且反正 trace 里
		// 都有。
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
			// 认不出来的状态，绝不当成成功。这里把 RawStop 打出来，是
			// 因为只有那个原字符串才能告诉你到底发生了什么。
			//
			// 空这一支不是疑神疑鬼：它是 StopReason 的零值。适配器哪天
			// 忘了调 normaliseStop，或者流在任何停止原因到达之前就断
			// 了，拿到的就是它。没有这一支，"始终没弄清生成为什么停"就
			// 会掉进跟"模型把话说完了"同一个分支。
			bus.Notice("unknown stop reason %q — treating the turn as finished", res.RawStop)
			return msgs, lastPrompt
		}

		if len(res.Calls) == 0 {
			bus.Emit(Event{Kind: KindTurnEnd, Turn: turn})
			return msgs, lastPrompt
		}

		// 每次调用都要有结果，被拒的也要：调用没人应答，**下一次**请
		// 求就是畸形的——而且可能要等好几条用户消息之后才发作。结果
		// 都放进一条中立消息里；它在线上长什么样由各个适配器自己定，
		// 而它们的看法完全不一致。
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

// emitResult 把工具结果发布出去，同时返回该追加的块——这样用户看到的和告诉
// 模型的，就永远不会飘开。
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
