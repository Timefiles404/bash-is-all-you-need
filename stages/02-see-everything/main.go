// 阶段 02——看见一切。
//
// 还是阶段 01 那个 Agent，结构上只改一处：它什么都不打印。它发事件，
// 你能看见的那些东西去订阅。
//
//	agent core ──emit──▶ Bus ──┬──▶ renderer   (装了仪表的终端)
//	                           └──▶ TraceWriter (session.jsonl，一行一条事件)
//
//	replay: session.jsonl ──▶ Replay ──▶ 还是那个渲染器，不联网，不要 key
//
// 这一阶段所有的新东西，都是从那一个决定里长出来的。先读 events.go 看
// 论证，再读这个文件看接线，再读 render.go 看那些数字是什么意思。
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

// ---------------------------------------------------------------------------
// 线上类型。还是手写的，还是只有 OpenAI 协议——第二种协议阶段 03 才到，
// 到那时这些类型会挪到中立内核后面去。
// ---------------------------------------------------------------------------

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

	// 真正的 OpenAI 端点不加这个就不会流式发 usage。本仓库开发时对着的那个
	// 网关，加不加都发——见 docs/wire-notes.md §B5，那里实测这个 flag 就是
	// 空操作。还是加上：它不花什么代价，而不加的下场是，哪天有人把它指向
	// 别的供应商，Agent 就报零 token。
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

// stream 发一次请求，然后让 SSE 解析器把响应变成事件。注意这个函数
// **不做**什么：它从不为人排版。它之所以一屏就能读完，是因为呈现层已经
// 搬走了。
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

	// 请求检查器就靠它，它也是任何 trace 里最有用的一行：模型究竟看到了
	// 什么，只有它记着。会话记录里其余的一切，都是重建出来的。
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	req, err := http.NewRequest("POST", c.cfg.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	// Started 尽可能晚地打上时间戳，这样 TTFT 量的是用户实际的体感——网络
	// 也算在里面——而不是模型干了多久。
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// 写重试策略之前值得知道：在这个网关上，不认识的 model id 返回
		// 401，body 格式不对返回 500。天真的"5xx 全都重试"循环，会拿着
		// 客户端的 bug 一直重试下去。
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseOpenAIStream(resp.Body, bus, turn, started)
}

// ---------------------------------------------------------------------------
// 权限闸。实质上和阶段 01 一样；现在它把裁决作为事件报出去，半年后翻
// trace 也能看见某次拒绝。
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

	// 重放不要 key，不要 shell，也不要网。这正是重点：学生可以研究一段
	// 自己没花钱的真实会话，你也可以拿用户寄来的文件调他那一次运行。
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

// runTurn 把一条用户消息推到收尾，返回长大之后的历史，以及最后发出的
// 那次 prompt 有多大（那就是上下文水位线）。
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

		// 把 assistant 消息重新拼出来，拼成 API 不走流式时会返回的样子，
		// 因为要回填进历史的正是这个东西。重组是流式要交的税；忘了交，
		// 流式 Agent 就会"弄丢"自己的工具调用。
		am := message{Role: "assistant", Content: res.Text}
		for _, tc := range res.ToolCalls {
			var call toolCall
			call.ID, call.Type = tc.ID, "function"
			call.Function.Name, call.Function.Arguments = tc.Name, tc.Args
			am.ToolCalls = append(am.ToolCalls, call)
		}
		msgs = append(msgs, am)

		// 注意这里**没有**什么：没有 KindResponseEnd。流解析器已经发过一
		// 条了，因为响应究竟什么时候结束、结束得干不干净，只有它这个组
		// 件知道。在这里再发第二条，就是这条注释要拦住你重新引入的那个
		// bug——两个组件都以为某条事件归自己管，这是事件驱动设计最常见
		// 的翻车方式，而它露出来的样子不是崩溃，是一块重复的、半空的
		// 面板。

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

		// 每一次工具调用都要有结果，被我们拒掉的也一样。有调用没答复，
		// **下一次**请求就是畸形的，而那可能是好几条用户消息之后了。
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

// toolResult 把结果发出去，同时返回要追加的消息，用户看到的东西和告诉
// 模型的东西就永远不会跑偏成两样。
func toolResult(bus *Bus, turn int, callID, content string) message {
	bus.Emit(Event{Kind: KindToolResult, Turn: turn, ToolID: callID, Text: content})
	return message{Role: "tool", ToolCallID: callID, Content: content}
}
