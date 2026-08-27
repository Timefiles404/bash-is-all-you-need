// 阶段 01 ——别死。
//
// 阶段 00 是这个思想。这是同一个 Agent 在遇到现实后
// 的样子：一条永不返回的命令，一条打印 40MB 的命令，
// 一个说到一半被截断的 Agent，和一条你真的不想运行的命令。
//
// 这里加的每一样东西，都是因为阶段 00 的 Agent 在这上面
// 栽过跟头。文档（docs/01-dont-die.md）展示了如何先重现
// 每一种失败——自己动手破坏它，才是这里的重点。
//
// 这个阶段的新内容：
//   - 输出截断（头 + 尾，永不中间）
//   - 杀掉整个进程树而不只是 shell 的命令超时
//   - 一个 finish_reason 状态机，包括无声的 `length` 截断
//   - 一个权限闸，否决会以数据反馈给 Agent
//   - 输出清理：ANSI 转义、CRLF 和无效 UTF-8
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
// 配置。全部是你读文档时应该调整的旋钮。
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
// 线上格式——除了 FinishReason 之外都和阶段 00 一样，
// 这个字段我们现在真的会去读了。见阶段 03 的协议中立重写。
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
// 运行一个命令而不永远挂起。
// ---------------------------------------------------------------------------

type execResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Unreaped bool // 被杀了，但 OS 从未释放它：读输出不安全
	Duration time.Duration
}

// runBash 在超时下执行一条命令，如果过期就杀掉
// 整个进程树。
//
// 两个细微差别值得花时间读：
//
// 只杀 cmd.Process 还不够。`npm start &` 会留下一个
// 孙进程持有同一个 stdout 管道，cmd.Wait() 会阻塞直到
// 该管道的每个写者都消失——所以半杀不只会泄漏一个进程，
// 它会挂起试图逃离这个挂起的 Agent。procGroup（proc_unix.go
// / proc_windows.go）是让超时真的起作用的东西。
//
// stdout 和 stderr 被分别捕获而不是合并。这失去了交错——
// 你不再能说一个警告打印在两个结果**之间**——但它得到了
// 归属，一个读到"这条去了 stderr"的 Agent，推理失败原因
// 的能力，比读到一团未加区分的内容要强得多。合并是另一个
// 可辩护的选择；知道你选了哪一个。
func runBash(cfg config, command string) execResult {
	started := time.Now()

	g, err := newProcGroup()
	if err != nil {
		return execResult{Stderr: fmt.Sprintf("could not create process group: %v", err), ExitCode: -1}
	}
	defer g.Close()

	cmd := exec.Command(cfg.shell, "-c", command)
	cmd.Stdin = nil // 交互式提示必须快速失败，不能阻塞
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	g.attach(cmd)

	if err := cmd.Start(); err != nil {
		return execResult{Stderr: fmt.Sprintf("could not start command: %v", err), ExitCode: -1}
	}
	if err := g.adopt(cmd); err != nil {
		// 不致命：命令已经在运行，通常仍可杀死。
		// 说出来，而不是装作这棵树被包含了。
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

		// 解开 Wait 的，正是这次 kill——但这一章的整个教训是，
		// 逃生出口也可能会挂起，所以这一步也有自己的期限。
		// 如果我们在这里放弃，Wait goroutine 就会泄漏（它会一直
		// 持有输出缓冲，直到 OS 最终释放管道）。那是正确的权衡：
		// 泄漏一个 goroutine 是能承受的，Agent 卡死不行。
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
		// Wait 没有返回，所以负责复制的 goroutine 可能仍在写入
		// 这些缓冲区。现在去读它们是一次数据竞争——不读取任何
		// 东西，只报告这个情况，而不是拿它赌一把。
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

// render 把 execResult 变成 Agent 会看到的确切文本。
//
// Agent 没有其他看向世界的窗口，所以这个函数**就是**
// 世界。它隐藏的一切，Agent 无法推理；它搞乱的一切，
// Agent 推理错了。
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

	// 状态行最后，所以它活过 Agent 自己做的任何截断，
	// 是离它下一个想法最近的东西。
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

// truncate 保留头和尾并丢弃中间。
//
// 仅截断开头是常见的捷径，但它是错的：一次失败构建里，
// 有趣的部分是最后二十行；一次目录列表里，有趣的部分是
// 前二十行。两端都保留不花什么成本，还能省一次重跑。
func truncate(s string, max int) (string, bool) {
	if max < 256 {
		max = 256
	}
	if len(s) <= max {
		return s, false
	}
	head := max * 2 / 3
	tail := max - head

	// 在 rune 边界上切割——半写的多字节字符变成
	// JSON 正文中无效的 UTF-8 字节，有些 API 直接拒绝。
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

// sanitize 让命令输出安全到能放进 JSON 请求正文中。
//
// 三个分离的问题，全部看起来像"奇怪字符"直到你
// 知道你有哪个：
//
//   - ANSI 转义：颜色码对 Agent 纯粹是噪音，花 token。
//   - CRLF：在 Windows 上，每行以 \r\n 结尾，\r 会活着
//     进入上下文窗口，在那里它不可见，徒增一份没用的重复。
//   - 无效 UTF-8：写本地代码页的程序（中文 Windows 上
//     的 GBK，日文上的 Shift-JIS）生成根本不是有效
//     UTF-8 的字节。放着不管，它们要么污染请求，要么
//     作为乱码到达。我们把它们替换掉，让失败可见，
//     而不是无声无息；如果你需要真正转码，那是
//     golang.org/x/text/encoding，故意不是这个仓库
//     的依赖。
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
	available bool // stdin 被管道连接时为假：没人可问
}

type verdict int

const (
	allow verdict = iota
	deny
	abort
)

// ask 展示命令并等待一个裁决。
//
// 设计点是拒绝时发生什么：Agent 会在一个工具结果里
// 被告知，用户拒绝了。它不是错误，它不结束回合。那让
// Agent 留在一个还能随机应变的位置上——建议更窄的
// 东西，或问为什么——而不是偏偏在一个人类正盯着看的
// 那一刻死掉。
//
// 这个闸也是反对"bash 就是你需要的全部"的诚实论证：
// 它能展示给用户的全部是一个不透明的命令字符串。
// 一个专用的 `write_file` 工具能展示一个 diff；
// 一个专用的 `send_email` 工具能展示收件人。广度让你
// 丧失了问出一个好问题的能力。
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
// Shell 发现。
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

	// 一个管道 stdin 后面没有人类，所以没人回答这个闸。
	// 事先检测那个而不是无声地否决每条命令。
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

// runTurn 把一个用户消息驱动到完成，返回增长的历史。
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

		// finish_reason 状态机。阶段 00 仅在"是否有工具调用"上
		// 分支，这无声地把一个被切断的回答当作完成的。
		switch choice.FinishReason {
		case "stop", "end_turn", "":
			if len(choice.Message.ToolCalls) == 0 {
				fmt.Println()
				return msgs
			}
			// 有些供应商说"stop"同时还在发出工具调用；相信
			// 调用，不是标签。

		case "length", "max_tokens":
			// Agent 在生成过程中途撞上了 max_tokens。如果那时正在
			// 进行一次工具调用，参数就是一段被截断的 JSON 字符串，
			// 绝不能运行：半条 shell 命令不会是一条更安全的 shell 命令。
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
			// 正常路径，继续往下走。

		default:
			fmt.Printf("\n[unknown finish_reason %q — treating as a finished turn]\n\n", choice.FinishReason)
			return msgs
		}

		if len(choice.Message.ToolCalls) == 0 {
			fmt.Println()
			return msgs
		}

		// 每个 tool_call 都必须带着一个结果回来，包括我们决定
		// 不运行的那些。提前跳出这个循环，会在历史里留下
		// 一个没有答案的调用，而**下一个**请求——可能是好几条
		// 用户消息之后——就会被当成格式错误拒绝掉。像这样的
		// bug，正是"全部回答，一次不落"这条规则存在的原因。
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

// parseBashArgs 把 Agent 的工具参数变成一条命令，并拒绝一切不是命令的东西。
//
// 这个函数的存在，是因为一次真实发生过的失败。问这个网关要一个
// max_tokens 太小的 tool_call，它返回：
//
//	stop_reason: "tool_use"          <- 声称调用没问题
//	input:       {"raw_arguments":""} <- 模式的 `command` 键不见了
//
// 现在看看，显而易见的 Go 代码会拿它怎么办：
//
//	var args struct{ Command string `json:"command"` }
//	json.Unmarshal(data, &args)   // 返回 nil。没有错误。一点都没有。
//	args.Command                  // ""
//
// Unmarshal **成功了**。Go 用零值填充缺失的键，所以缺失的必需字段和空字
// 段分不出来——Agent 会接着运行一条空命令，就像模型真的这样要求过一样。
//
// **Unmarshal 不报错，不等于验证通过。** 一个 *string 能分清缺失（nil）
// 和空（""），两个都会在这里被拒绝，并附上一条 Agent 能据此行动的消息。
// 不管你用的是哪种协议，都要对照你发布的模式去验证参数；信封自己的
// stop_reason，并不能证明它包裹的东西是可用的。
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
