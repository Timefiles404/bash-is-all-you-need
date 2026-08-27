// 阶段 02——看见一切。
//
// 和阶段 01 是同一个 Agent，只有一处结构上的改变：它什么都不
// 打印。它发出事件，你能看到的东西订阅它们。
//
//	Agent 核心 ──发出──▶ 总线 ──┬──▶ 渲染器   （终端，已插桩）
//	                           └──▶ TraceWriter（session.jsonl，每行一个事件）
//
//	重放：session.jsonl ──▶ Replay ──▶ 相同的渲染器，无网络，无密钥
//
// 这个阶段所有的新东西，都源于那一个决定。读 events.go 了解
// 论证，再读这个文件了解接线，最后读 render.go 了解数字的含义。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
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

// 线上类型。仍然手写，仍然只有 OpenAI 协议——到了阶段 03，
// 第二个协议加入进来，这些类型就会挪到中立核心背后。

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
	Tools     []toolDef `json:"tools"`
	Stream    bool      `json:"stream"`

	// 如果没有这个字段，真正的 OpenAI 端点不会流式返回 usage。这个
	// 仓库据以开发的那个网关，无论如何都会发送 usage——参见
	// docs/wire-notes.md §B5，那里实测证实这个标志是无操作的。但
	// 还是要发送它：这不花一分钱，不发的代价是某天有人把 Agent
	// 指向另一个供应商时，它会开始报告零 token。
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

func bashTool() toolDef {
	var t toolDef
	t.Type = "function"
	t.Function.Name = "bash"
	t.Function.Description = "Execute a bash command and return its stdout, stderr and exit code."
	t.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}
	return t
}

// ---------------------------------------------------------------------------

type config struct {
	baseURL   string
	apiKey    string
	model     string
	shell     string
	timeout   time.Duration
	maxOutput int
	maxTurns  int
	yolo      bool
}

type client struct {
	cfg  config
	http *http.Client
}

// stream 发送一个请求，让 **SSE** 解析器把响应变成事件。注意
// 这个函数**不**做的事：它从不为人类格式化任何东西。它之所以
// 能在一屏之内读完，唯一的原因就是这里完全不管"怎么展示给
// 人看"。
func (c *client) stream(msgs []message, bus *Bus, turn int) (*streamResult, error) {
	body, err := json.Marshal(chatRequest{
		Model:         c.cfg.model,
		MaxTokens:     4096,
		Messages:      msgs,
		Tools:         []toolDef{bashTool()},
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return nil, err
	}

	// 请求检查器，以及任何 **trace** 中最有用的单行：
	// 模型实际看到的唯一记录。转录中的其他东西都是重建。
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	req, err := http.NewRequest("POST", c.cfg.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	// Started 尽可能晚地加时间戳，这样 TTFT 衡量的是用户实际体验到的
	// ——把网络耗时也算在内——而不是模型自己做了什么。
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// 在写重试策略之前，有一件事值得知道：在这个网关上，未知的
		// 模型 id 返回 401，畸形体返回 500。朴素的"重试所有 5xx"循环
		// 会永远重试客户端 bug。
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseOpenAIStream(resp.Body, bus, turn, started)
}

// ---------------------------------------------------------------------------
// 权限闸。较之阶段 01，实质上没有变化；它现在把裁决报告
// 为一个事件，这样一次拒绝在六个月后仍然能在 **trace** 中
// 看到。
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
		tracePath  = flag.String("trace", "", "write a JSONL event trace to this file")
		replayPath = flag.String("replay", "", "replay a trace instead of running the agent")
		speed      = flag.Float64("speed", 1, "replay speed: 0 = instant, 1 = original timing, 2 = double")
		step       = flag.Bool("step", false, "replay: wait for Enter before each event")
		window     = flag.Int("window", 0, "model context window, for the watermark")
		showReq    = flag.Bool("show-request", false, "print the full request body before each call")
		pIn        = flag.Float64("price-in", 0, "$ per 1M input tokens")
		pOut       = flag.Float64("price-out", 0, "$ per 1M output tokens")
		pRead      = flag.Float64("price-cache-read", 0, "$ per 1M cached-read tokens")
		pWrite     = flag.Float64("price-cache-write", 0, "$ per 1M cache-write tokens")
	)
	cfg := config{
		baseURL: strings.TrimSuffix(os.Getenv("AGENT_BASE_URL"), "/"),
		apiKey:  os.Getenv("AGENT_API_KEY"),
		model:   os.Getenv("AGENT_MODEL"),
	}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds per user message")
	flag.BoolVar(&cfg.yolo, "yolo", false, "run every command without asking")
	flag.Parse()

	prices := prices{in: *pIn, out: *pOut, cacheRead: *pRead, cacheWrite: *pWrite}
	view := newRenderer(os.Stdout, colorEnabled(os.Stdout), prices, *window)
	view.showRequest = *showReq

	// 重放不需要密钥、shell，也不需要网络。这就是意义所在：一个
	// 学生能够研究一次自己完全没有付费的真实会话，你也能直接用
	// 用户发给你的文件，调试他们的那次运行。
	if *replayPath != "" {
		events, err := ReadTrace(*replayPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		opts := ReplayOpts{Speed: *speed, Step: *step}
		if err := Replay(events, view, opts, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if cfg.baseURL == "" || cfg.apiKey == "" || cfg.model == "" {
		fmt.Fprintln(os.Stderr, "set AGENT_BASE_URL, AGENT_API_KEY and AGENT_MODEL (see .env.example)")
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

	c := &client{cfg: cfg, http: &http.Client{Timeout: 10 * time.Minute}}
	g := &gate{yolo: cfg.yolo, in: stdin, available: interactive, out: os.Stdout}

	wd, _ := os.Getwd()
	fmt.Printf("stage 02 · model=%s · cwd=%s\n", cfg.model, wd)
	if *tracePath != "" {
		fmt.Printf("trace: %s\n", *tracePath)
	}

	msgs := []message{{Role: "system", Content: systemPrompt}}
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
		msgs = append(msgs, message{Role: "user", Content: line})
		msgs, lastPrompt = runTurn(c, g, bus, cfg, msgs, lastPrompt)
	}

	view.SessionSummary(lastPrompt)
}

// runTurn 把一条用户消息推进到完成，返回增长后的历史，
// 以及发送的最后一个 prompt 的大小（这是上下文水位线）。
func runTurn(c *client, g *gate, bus *Bus, cfg config, msgs []message, lastPrompt int) ([]message, int) {
	for turn := 1; ; turn++ {
		if turn > cfg.maxTurns {
			bus.Notice("stopped: hit the %d-turn limit", cfg.maxTurns)
			return msgs, lastPrompt
		}
		bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		res, err := c.stream(msgs, bus, turn)
		if err != nil {
			bus.Error("%v", err)
			return msgs, lastPrompt
		}
		lastPrompt = res.Usage.Prompt()

		// 重建 API 在非流式情况下本应返回的那条 assistant 消息，因为
		// 回到历史里的，必须是这个版本。重新组装，是你为流式付出的
		// 税，忘掉这一点，就是流式 Agent 会"丢"工具调用的原因。
		am := message{Role: "assistant", Content: res.Text}
		for _, tc := range res.ToolCalls {
			var call toolCall
			call.ID, call.Type = tc.ID, "function"
			call.Function.Name, call.Function.Arguments = tc.Name, tc.Args
			am.ToolCalls = append(am.ToolCalls, call)
		}
		msgs = append(msgs, am)

		// 注意这里**没有**什么：一个 KindResponseEnd。流解析器已经
		// 发出了一个，因为知道响应到底什么时候结束、结束得干不干净
		// 的，正是这个组件。从这里再发出第二个，就是这条注释想要
		// 阻止你重新引入的那个 bug——两个组件各自都以为自己拥有
		// 某个事件，是事件驱动设计出错最常见的方式，它显示为一个
		// 复制的、半空的面板，而不是崩溃。

		switch res.FinishReason {
		case "length", "max_tokens":
			bus.Notice("the model was cut off at max_tokens")
			if len(res.ToolCalls) == 0 {
				return msgs, lastPrompt
			}
			for _, tc := range res.ToolCalls {
				msgs = append(msgs, toolResult(bus, turn, tc.ID,
					"[not executed: your reply was cut off at max_tokens. Retry with a shorter command.]"))
			}
			continue
		case "content_filter":
			bus.Notice("the provider filtered this response")
			return msgs, lastPrompt
		}

		if len(res.ToolCalls) == 0 {
			bus.Emit(Event{Kind: KindTurnEnd, Turn: turn})
			return msgs, lastPrompt
		}

		// 每个工具调用都会得到一个结果，包括我们拒绝的那些。未回答
		// 的调用会让**下一个**请求变得畸形——可能是好几条用户消息
		// 之后的事。
		stop := false
		for _, tc := range res.ToolCalls {
			if stop {
				msgs = append(msgs, toolResult(bus, turn, tc.ID, "[not executed: the session was stopped.]"))
				continue
			}
			command, err := parseBashArgs(tc.Args)
			if err != nil {
				msgs = append(msgs, toolResult(bus, turn, tc.ID, fmt.Sprintf("[%v]", err)))
				continue
			}
			bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: tc.ID, ToolName: tc.Name, Command: command})

			v, why := g.ask(command)
			bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: tc.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				msgs = append(msgs, toolResult(bus, turn, tc.ID,
					"[the user denied this command. Do not retry it unchanged.]"))
				continue
			case abort:
				stop = true
				msgs = append(msgs, toolResult(bus, turn, tc.ID, "[the user stopped the session.]"))
				continue
			}

			bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: tc.ID, Command: command})
			r := runBash(cfg.shell, command, cfg.timeout)
			rendered, truncated := r.render(cfg.maxOutput)
			bus.Emit(Event{
				Kind: KindCommandEnd, Turn: turn, ToolID: tc.ID, Command: command,
				ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
				Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
			})
			msgs = append(msgs, toolResult(bus, turn, tc.ID, rendered))
		}
		if stop {
			return msgs, lastPrompt
		}
	}
}

// toolResult 发出结果，并返回待追加的那条消息，这样用户看到
// 的东西和模型被告知的东西，就永远不会彼此漂移开。
func toolResult(bus *Bus, turn int, callID, content string) message {
	bus.Emit(Event{Kind: KindToolResult, Turn: turn, ToolID: callID, Text: content})
	return message{Role: "tool", ToolCallID: callID, Content: content}
}
