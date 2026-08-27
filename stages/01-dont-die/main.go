// 阶段 01——别死。
//
// 阶段 00 讲的是想法。这是同一个 Agent 撞上现实之后的样子：命令
// 永远不返回，命令打出 40MB，模型说到一半被切断，还有你根本不想
// 让它跑的命令。
//
// 这一阶段加的每样东西，都是因为阶段 00 的 Agent 在这上面栽过。
// 文档（docs/01-dont-die.md）会先教你怎么复现每个故障——亲手把它
// 弄坏，才是重点。
//
// 本阶段新增：
//   - 输出截断（留头留尾，绝不留中间）
//   - 命令超时，杀掉整棵进程树，而不只是 shell
//   - finish_reason 状态机，包括不声不响的 `length` 截断
//   - 权限闸，拒绝会作为数据回喂给模型
//   - 输出清洗：ANSI 转义、CRLF、非法 UTF-8
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
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
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
// 配置。每一项都是旋钮，读文档的时候该动手拧一拧。
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

// ---------------------------------------------------------------------------
// 线上类型——除了 FinishReason 之外和阶段 00 一样，这次是真的会去
// 读它了。协议中立的重写版见阶段 03。
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
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string  `json:"finish_reason"`
		Message      message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
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
// API 调用。
// ---------------------------------------------------------------------------

type client struct {
	cfg  config
	http *http.Client
}

func (c *client) callModel(msgs []message) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:     c.cfg.model,
		MaxTokens: 4096,
		Messages:  msgs,
		Tools:     []toolDef{bashTool()},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.cfg.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode: %w (body: %.200s)", err, raw)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("api error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}
	return &parsed, nil
}

// ---------------------------------------------------------------------------
// 执行命令，而不至于永远挂住。
// ---------------------------------------------------------------------------

type execResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Unreaped bool // 杀了，但操作系统始终没放手：输出不能读
	Duration time.Duration
}

// runBash 给一条命令套上超时，时间一到就杀掉整棵进程树。
//
// 有两处细节值得花时间读：
//
// 只杀 cmd.Process 不够。`npm start &` 会留下孙子进程攥着同一根
// stdout 管道，而 cmd.Wait() 要一直阻塞到这根管道的写端全部消失
// 为止——所以杀一半不只是漏个进程，它会把那个正想逃出挂死的
// Agent 一起挂住。真正让超时生效的是 procGroup（proc_unix.go /
// proc_windows.go）。
//
// stdout 和 stderr 是分开抓的，不是合流。代价是丢掉交错顺序——你
// 再也看不出某条警告是打在两个结果**中间**的——换来的是归属：模型
// 读到"这条走的是 stderr"，判断故障的能力远强于只读到一坨不分家的
// 输出。合起来抓也站得住，是另一种选法；你只要知道自己选了哪个。
func runBash(cfg config, command string) execResult {
	started := time.Now()

	g, err := newProcGroup()
	if err != nil {
		return execResult{Stderr: fmt.Sprintf("could not create process group: %v", err), ExitCode: -1}
	}
	defer g.Close()

	cmd := exec.Command(cfg.shell, "-c", command)
	cmd.Stdin = nil // 交互式提问必须立刻失败，不能阻塞
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	g.attach(cmd)

	if err := cmd.Start(); err != nil {
		return execResult{Stderr: fmt.Sprintf("could not start command: %v", err), ExitCode: -1}
	}
	if err := g.adopt(cmd); err != nil {
		// 不致命：命令已经在跑了，通常也还杀得掉。把话说出来，别装作
		// 整棵树已经被圈住了。
		fmt.Fprintf(os.Stderr, "warning: process group adoption failed: %v\n", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timedOut, unreaped bool
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(cfg.timeout):
		timedOut = true
		g.kill()

		// 是这次 kill 把 Wait 从阻塞里放出来的——但这一章讲的就是逃生口
		// 自己也会挂死，所以这个逃生口也得有自己的截止时间。在这里放弃
		// 会漏掉 Wait 那个 goroutine（它一直占着输出缓冲区，直到操作系
		// 统终于放开管道）。这个取舍是对的：漏一个 goroutine 还活得下
		// 去，把 Agent 卡死就活不下去了。
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			unreaped = true
		}
	}

	res := execResult{
		TimedOut: timedOut,
		Unreaped: unreaped,
		Duration: time.Since(started),
	}
	if unreaped {
		// Wait 一直没返回，所以那些拷贝的 goroutine 可能还在往这些缓冲
		// 区里写。这时候去读就是数据竞争——什么都别拿，把情况报出来，
		// 而不是拿它赌一把。
		res.ExitCode = -1
		return res
	}

	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else if waitErr != nil {
		res.ExitCode = -1
		res.Stderr += "\n" + waitErr.Error()
	}
	return res
}

// render 把 execResult 变成模型将要看到的那段文字，一字不差。
//
// 模型再没有别的窗口能看到这个世界，所以这个函数就是世界。它藏起
// 来的东西，模型无从推理；它弄乱的东西，模型会推理错。
func (r execResult) render(maxOutput int) string {
	var b strings.Builder

	out, outCut := truncate(sanitize(r.Stdout), maxOutput*2/3)
	errOut, errCut := truncate(sanitize(r.Stderr), maxOutput/3)

	if strings.TrimSpace(out) != "" {
		b.WriteString(out)
	}
	if strings.TrimSpace(errOut) != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("<stderr>\n" + errOut + "\n</stderr>")
	}
	if b.Len() == 0 {
		b.WriteString("[no output]")
	}

	// 状态行放最后，这样模型自己那边不管怎么截，它都留得下来，而且
	// 离模型的下一个念头最近。
	status := fmt.Sprintf("\n[exit %d · %s]", r.ExitCode, r.Duration.Round(time.Millisecond))
	if r.TimedOut {
		status = fmt.Sprintf("\n[TIMED OUT after %s — the process tree was killed]", r.Duration.Round(time.Millisecond))
	}
	if r.Unreaped {
		status = fmt.Sprintf("\n[TIMED OUT after %s and could not be reaped — output was discarded as unsafe to read. Do not run this command again.]",
			r.Duration.Round(time.Millisecond))
	}
	if outCut || errCut {
		status += " [output truncated — rerun with a filter such as grep/head/tail]"
	}
	b.WriteString(status)
	return b.String()
}

// truncate 留头留尾，丢中间。
//
// 只留头是常见的图省事做法，而且是错的：构建失败时有意思的是最后
// 二十行，目录列表里有意思的是最前二十行。两头都留不花什么代价，
// 还省掉一次重跑。
func truncate(s string, max int) (string, bool) {
	if max < 256 {
		max = 256
	}
	if len(s) <= max {
		return s, false
	}
	head := max * 2 / 3
	tail := max - head

	// 按 rune 边界切——写了一半的多字节字符会在 JSON body 里变成非法
	// UTF-8 字节，有些 API 直接就拒。
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	cut := len(s) - tail
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}

	elided := cut - head
	return fmt.Sprintf("%s\n\n[... %d bytes elided ...]\n\n%s", s[:head], elided, s[cut:]), true
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\a\x1b]*(\a|\x1b\\)|\x1b[@-Z\\-_]`)

// sanitize 让命令输出可以安全地放进 JSON 请求体。
//
// 三个各不相干的问题，在你弄清手上是哪一个之前，看起来都是"乱码"：
//
//   - ANSI 转义：颜色码对模型来说是纯噪声，还费 token。
//   - CRLF：Windows 上每行都以 \r\n 结尾，\r 会一路活到上下文窗口
//     里，在那儿看不见，多出来的这一份也没有任何用。
//   - 非法 UTF-8：按本地代码页输出的程序（中文 Windows 上是 GBK，
//     日文的是 Shift-JIS）打出来的字节根本不是合法 UTF-8。放着不
//     管，它们要么把请求搞坏，要么变成一堆 mojibake。这里把它们替
//     换掉，让故障看得见，而不是无声无息；真要转码，那是
//     golang.org/x/text/encoding，而这个仓库故意不依赖它。
func sanitize(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return s
}

// ---------------------------------------------------------------------------
// 权限闸。
// ---------------------------------------------------------------------------

type gate struct {
	yolo      bool
	always    bool
	in        *bufio.Scanner
	available bool // stdin 被管道接走时为 false：没人可问
}

type verdict int

const (
	allow verdict = iota
	deny
	abort
)

// ask 把命令摆出来，等一个决定。
//
// 设计的要害在于被拒之后会怎样：模型会在工具结果里收到一句"用户
// 拒绝了"。这不是错误，也不结束这一回合。Agent 因此仍然处在能调整
// 的位置——提个更窄的方案，或者问一句为什么——而不是恰好在有人盯着
// 的那一刻死掉。
//
// 这道闸也是"bash is all you need"最诚实的反证：它能给用户看的，
// 只有一串不透明的命令字符串。专门的 `write_file` 工具能给出 diff，
// 专门的 `send_email` 工具能给出收件人。要了广度，就赔上了问一个好
// 问题的能力。
func (g *gate) ask(command string) verdict {
	if g.yolo || g.always {
		return allow
	}
	if !g.available {
		fmt.Println("  [denied: no terminal to ask on — rerun with --yolo to allow commands]")
		return deny
	}
	fmt.Printf("  run? [y = yes / n = no / a = yes to all this session / q = stop] ")
	if !g.in.Scan() {
		return abort
	}
	switch strings.ToLower(strings.TrimSpace(g.in.Text())) {
	case "y", "yes":
		return allow
	case "a", "all":
		g.always = true
		return allow
	case "q", "quit":
		return abort
	default:
		return deny
	}
}

// ---------------------------------------------------------------------------
// shell 探测。
// ---------------------------------------------------------------------------

func findBash() (string, error) {
	if p := os.Getenv("AGENT_BASH"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
		return "", fmt.Errorf("no bash found — install Git for Windows, or set AGENT_BASH")
	}
	return "", fmt.Errorf("no bash found on PATH")
}

// ---------------------------------------------------------------------------
// 主循环。
// ---------------------------------------------------------------------------

func main() {
	cfg := config{
		baseURL: strings.TrimSuffix(os.Getenv("AGENT_BASE_URL"), "/"),
		apiKey:  os.Getenv("AGENT_API_KEY"),
		model:   os.Getenv("AGENT_MODEL"),
	}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds allowed per user message")
	flag.BoolVar(&cfg.yolo, "yolo", false, "run every command without asking")
	flag.Parse()

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

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// stdin 是管道接来的，后面就没有人，权限闸也就没人来答。一开始
	// 就把这件事查出来，而不是一声不响地拒掉每一条命令。
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}

	c := &client{cfg: cfg, http: &http.Client{Timeout: 5 * time.Minute}}
	g := &gate{yolo: cfg.yolo, in: stdin, available: interactive}

	wd, _ := os.Getwd()
	fmt.Printf("stage 01 · model=%s · shell=%s\n", cfg.model, cfg.shell)
	fmt.Printf("cwd=%s · timeout=%s · max-output=%d\n", wd, cfg.timeout, cfg.maxOutput)
	if cfg.yolo {
		fmt.Println("--yolo: every command runs unasked.")
	}
	fmt.Println()

	msgs := []message{{Role: "system", Content: systemPrompt}}

	for {
		fmt.Print("> ")
		if !stdin.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(stdin.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return
		}
		msgs = append(msgs, message{Role: "user", Content: line})
		msgs = runTurn(c, g, cfg, msgs)
	}
}

// runTurn 把一条用户消息推到跑完为止，返回长大之后的历史。
func runTurn(c *client, g *gate, cfg config, msgs []message) []message {
	for turn := 1; ; turn++ {
		if turn > cfg.maxTurns {
			fmt.Printf("\n[stopped: hit the %d-turn limit]\n\n", cfg.maxTurns)
			return msgs
		}

		resp, err := c.callModel(msgs)
		if err != nil {
			fmt.Printf("\n[error: %v]\n\n", err)
			return msgs
		}
		choice := resp.Choices[0]
		msgs = append(msgs, choice.Message)

		fmt.Printf("  [tokens: prompt=%d completion=%d · finish=%s]\n",
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, choice.FinishReason)

		if choice.Message.Content != "" {
			fmt.Printf("\n%s\n", choice.Message.Content)
		}

		// finish_reason 状态机。阶段 00 只按"有没有工具调用"分支，那样
		// 一来，被切断的回答会一声不响地当成说完了的。
		switch choice.FinishReason {
		case "stop", "end_turn", "":
			if len(choice.Message.ToolCalls) == 0 {
				fmt.Println()
				return msgs
			}
			// 有些供应商一边说 "stop"，一边照样发工具调用；信调用，别信
			// 这个标签。

		case "length", "max_tokens":
			// 模型生成到一半撞上了 max_tokens。撞的时候要是正在发工具调用，
			// 参数就是一截被截断的 JSON 字符串，绝不能拿去跑：半条 shell 命
			// 令并不是更安全的 shell 命令。
			fmt.Println("\n[the model was cut off at max_tokens]")
			if len(choice.Message.ToolCalls) == 0 {
				fmt.Println()
				return msgs
			}
			for _, call := range choice.Message.ToolCalls {
				msgs = append(msgs, toolResult(call.ID,
					"[not executed: your reply was cut off at max_tokens, so this call was incomplete. Retry with a shorter command.]"))
			}
			continue

		case "content_filter":
			fmt.Println("\n[the provider filtered this response]")
			fmt.Println()
			return msgs

		case "tool_calls", "tool_use":
			// 正常路径，直接往下走。

		default:
			fmt.Printf("\n[unknown finish_reason %q — treating as a finished turn]\n\n", choice.FinishReason)
			return msgs
		}

		if len(choice.Message.ToolCalls) == 0 {
			fmt.Println()
			return msgs
		}

		// 每个 tool_call 都必须带着结果回来，包括那些我们决定不跑的。提前
		// break 出这个循环，历史里就留下一次没人应答的调用，而**下一次**
		// 请求——可能已经是好几条用户消息之后了——会被判成格式非法而拒掉。
		// 就是因为这种 bug，规矩才定成"全都答，每次都答"。
		stop := false
		for _, call := range choice.Message.ToolCalls {
			if stop {
				msgs = append(msgs, toolResult(call.ID, "[not executed: the session was stopped.]"))
				continue
			}

			command, err := parseBashArgs(call.Function.Arguments)
			if err != nil {
				msgs = append(msgs, toolResult(call.ID, fmt.Sprintf("[%v]", err)))
				continue
			}

			fmt.Printf("\n  $ %s\n", command)

			switch g.ask(command) {
			case deny:
				msgs = append(msgs, toolResult(call.ID,
					"[the user denied this command. Do not retry it unchanged.]"))
				continue
			case abort:
				stop = true
				msgs = append(msgs, toolResult(call.ID, "[the user stopped the session.]"))
				continue
			}

			res := runBash(cfg, command)
			rendered := res.render(cfg.maxOutput)
			fmt.Printf("%s\n", indent(rendered))
			msgs = append(msgs, toolResult(call.ID, rendered))
		}
		if stop {
			fmt.Println()
			return msgs
		}
	}
}

// parseBashArgs 把模型给的工具参数变成一条命令，凡是不成其为命令
// 的一律拒掉。
//
// 这个函数的存在，是因为一次真实观察到的故障。把 max_tokens 设得
// 太小，去问这个 gateway 要一次工具调用，它返回的是：
//
//	stop_reason: "tool_use"          <- 声称这次调用没问题
//	input:       {"raw_arguments":""} <- schema 里的 `command` 键不见了
//
// 再看看最顺手的那段 Go 代码拿它会怎么样：
//
//	var args struct{ Command string `json:"command"` }
//	json.Unmarshal(data, &args)   // 返回 nil。没有错误。一个都没有。
//	args.Command                  // ""
//
// unmarshal **成功了**。Go 会把缺席的键填成零值，于是"必填字段没来"
// 和"字段是空的"根本分不出来——Agent 就这么去跑了一条空命令，好像模
// 型真的要求过一样。
//
// **unmarshal 没报错，不等于校验过了。** *string 能把缺席（nil）和
// 空串（""）分开，这里两种都拒，并且给模型一句它能据此行动的话。不
// 管你用的是哪套协议，都要拿自己公布的 schema 去校验参数；信封自己
// 的 stop_reason，不能证明它包着的东西可用。
func parseBashArgs(raw string) (string, error) {
	var args struct {
		Command *string `json:"command"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", fmt.Errorf("could not parse tool arguments: %v — send valid JSON", err)
	}
	if args.Command == nil {
		return "", fmt.Errorf("tool call is missing the required \"command\" field — the call was probably cut short; send it again")
	}
	if strings.TrimSpace(*args.Command) == "" {
		return "", fmt.Errorf("the \"command\" field was empty — send an actual shell command")
	}
	return *args.Command, nil
}

func toolResult(callID, content string) message {
	return message{Role: "tool", ToolCallID: callID, Content: content}
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  | " + strings.ReplaceAll(s, "\n", "\n  | ")
}
