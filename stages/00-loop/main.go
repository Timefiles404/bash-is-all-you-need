// 阶段 00 ——主循环。
//
// 这是编写 coding Agent 的整个核心思想，所有让它能
// 活下去的东西都被刻意排除了。一个工具（bash），一个
// 主循环，裸 net/http，没有 SDK，没有流式，没有输出
// 截断，没有命令超时，没有权限闸。阶段 01 添加那些
// 防止它伤害自己的部分。
//
// 先读 main()，再读 callModel()，再读 runBash()。
// 这就是全部。
//
// 在临时目录中运行。它执行 Agent 要求的任何东西。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
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

When the task is done, reply with a short plain-text summary and no tool call.`

// 一个保险丝。没有它，一个 Agent 不停调用工具就会
// 陷入循环，直到你的密钥用尽。阶段 01 把这变成真正
// 的预算。
const maxTurns = 25

// ---------------------------------------------------------------------------
// 线上格式——OpenAI chat-completions 协议，手写版。
//
// API 发送的每个字段都在这里；还没有任何抽象。
// 阶段 03（Babel）是这些字段被拆到供应商中立类型后面的
// 地方，这样第二个协议就能接入。
// ---------------------------------------------------------------------------

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	// 只在 role:"tool" 消息上设置——它把结果和要求它的
	// 调用配对。Anthropic 协议做法不同；见阶段 03。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// 注意：一个 JSON **字符串**，里面装着 JSON，不是
		// 嵌套对象。这会绊倒每个人一次。总是 json.Unmarshal 它；
		// 永远不要字符串匹配它。
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

// bashTool 是这个 Agent 仅有的工具。
func bashTool() toolDef {
	var t toolDef
	t.Type = "function"
	t.Function.Name = "bash"
	t.Function.Description = "Execute a bash command and return its combined stdout and stderr."
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
// API 调用。裸 net/http——SDK 下面没有魔法，只有这个。
// ---------------------------------------------------------------------------

type client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func (c *client) callModel(msgs []message) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:     c.model,
		MaxTokens: 4096,
		Messages:  msgs,
		Tools:     []toolDef{bashTool()},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

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
// 工具。Agent 能采取的每个动作都要过这十行。
// ---------------------------------------------------------------------------

// findBash 定位 POSIX shell。在 Windows 上，那意味着
// Git Bash，每个装了 git 的开发者都已经有了。
// 阶段 08 用嵌入式解释器替换这个；在那之前我们借用系统的。
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

// runBash 执行一条命令并返回 Agent 应该看到的所有东西。
//
// 注意它**不**做什么：没有超时，所以开发服务器会让
// Agent 永远挂起；没有输出上限，所以 `find /` 会淹没
// 上下文窗口。两个都是阶段 01 的事。还要注意，非零
// 退出码在这里不是错误——它是一次观察，Agent 应该
// 对它做出反应。
func runBash(shell, command string) string {
	cmd := exec.Command(shell, "-c", command)
	cmd.Stdin = nil // 永远不要让命令阻塞在等待输入上
	out, err := cmd.CombinedOutput()

	result := string(out)
	if err != nil {
		result += fmt.Sprintf("\n[exit: %v]", err)
	}
	if strings.TrimSpace(result) == "" {
		result = "[no output]"
	}
	return result
}

// ---------------------------------------------------------------------------
// 主循环。
// ---------------------------------------------------------------------------

func main() {
	c := &client{
		baseURL: strings.TrimSuffix(os.Getenv("AGENT_BASE_URL"), "/"),
		apiKey:  os.Getenv("AGENT_API_KEY"),
		model:   os.Getenv("AGENT_MODEL"),
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
	if c.baseURL == "" || c.apiKey == "" || c.model == "" {
		fmt.Fprintln(os.Stderr, "set AGENT_BASE_URL, AGENT_API_KEY and AGENT_MODEL (see .env.example)")
		os.Exit(1)
	}

	shell, err := findBash()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	wd, _ := os.Getwd()
	fmt.Printf("stage 00 · model=%s · shell=%s\n", c.model, shell)
	fmt.Printf("cwd=%s\n", wd)
	fmt.Println("no permission gate in this stage: it runs whatever the model says. use a scratch dir.")
	fmt.Println()

	// 对话。它只会增长——这是 Agent 的短期记忆，
	// 阶段 05 是它停止永远增长的地方。
	msgs := []message{{Role: "system", Content: systemPrompt}}

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)

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

		// 内层循环：当 Agent 想用工具时保持继续。
		// 这个有工具调用的 while 形状**就是** Agent。
		// 这个仓库里的其他一切都是它周围的仪表。
		for turn := 1; ; turn++ {
			if turn > maxTurns {
				fmt.Printf("\n[stopped: hit %d turns]\n\n", maxTurns)
				break
			}

			resp, err := c.callModel(msgs)
			if err != nil {
				fmt.Printf("\n[error: %v]\n\n", err)
				break
			}
			choice := resp.Choices[0]
			msgs = append(msgs, choice.Message) // 把助手回合原样回显

			fmt.Printf("  [tokens: prompt=%d completion=%d]\n",
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

			if choice.Message.Content != "" {
				fmt.Printf("\n%s\n", choice.Message.Content)
			}
			if len(choice.Message.ToolCalls) == 0 {
				fmt.Println()
				break // 没请求工具：回合结束
			}

			// 执行每个请求的调用，然后在下一个请求前
			// 返回**所有**结果。把它们分散到单独的请求中会
			// 教 Agent 停止批处理调用。
			for _, call := range choice.Message.ToolCalls {
				var args struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					// 格式错误的参数是 Agent 要修复的问题，
					// 所以把解析错误递回去而不是崩溃。
					msgs = append(msgs, message{
						Role:       "tool",
						ToolCallID: call.ID,
						Content:    fmt.Sprintf("could not parse tool arguments: %v", err),
					})
					continue
				}

				fmt.Printf("\n  $ %s\n", args.Command)
				started := time.Now()
				output := runBash(shell, args.Command)
				fmt.Printf("%s\n  [%d bytes in %s]\n", indent(output), len(output), took(started))

				msgs = append(msgs, message{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    output,
				})
			}
		}
	}
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  | " + strings.ReplaceAll(s, "\n", "\n  | ")
}

func took(start time.Time) time.Duration {
	return time.Since(start).Round(time.Millisecond)
}
