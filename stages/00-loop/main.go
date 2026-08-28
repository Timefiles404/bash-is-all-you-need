// 阶段 00——主循环。
//
// 这就是 coding Agent 的全部思路，凡是能让它活下来的东西，都故意
// 没写。一个工具（bash），一个循环，裸 net/http，没有 SDK，不流式，
// 不截断输出，命令不设超时，也没有权限闸。阶段 01 才补上那些拦着
// 它别伤到自己的部分。
//
// 先读 main()，再读 callModel()，然后 runBash()。全部就这么多。
//
// 在临时目录里跑。模型说什么，它就执行什么。
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

// 保险丝。没有它，模型只要一直调工具，循环就一直转，直到你的 key
// 见底。阶段 01 把它换成真正的预算。
const maxTurns = 25

// ---------------------------------------------------------------------------
// 线上类型——OpenAI chat-completions 协议，手写的。
//
// 这里每个字段都是因为 API 真的会发它才存在。眼下什么都没抽象；
// 到阶段 03（Babel）才会把它们拆到供应商中立的类型后面，好让
// 第二种协议能插进来。
// ---------------------------------------------------------------------------

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	// 只有 role:"tool" 的消息才设这个字段——它把结果和发出请求的那次
	// 调用对上号。Anthropic 协议不是这么做的，见阶段 03。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// 注意：这是装着 JSON 的 JSON **字符串**，不是嵌套对象。每个人
		// 都会在这里栽一次。永远 json.Unmarshal 它，别去字符串匹配。
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

// bashTool 是这个 Agent 从头到尾唯一的工具。
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
// API 调用。裸 net/http——SDK 底下没有魔法，就只有这些。
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
// 工具。Agent 能做的每一件事，都要走这十四行。
// ---------------------------------------------------------------------------

// findBash 负责找到 POSIX shell。在 Windows 上那就是 Git Bash，装了
// git 的开发者手上都已经有。阶段 08 会把它换成内嵌解释器，在那之前
// 先借系统的用。
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

// runBash 执行一条命令，把模型该看到的东西全部返回。
//
// 注意它**不**做什么：不设超时，所以起个 dev server 就能把 Agent
// 永远挂在那；不限输出，所以 `find /` 能把上下文窗口冲垮。这两样
// 都归阶段 01。另外注意，非零退出码在这里不算错误——它是一条观察，
// 而该对它作出反应的是模型。
func runBash(shell, command string) string {
	cmd := exec.Command(shell, "-c", command)
	cmd.Stdin = nil // 绝不让命令卡在等输入上
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

	// 对话。它只会变长——这就是 Agent 的短期记忆，而阶段 05 才让它不
	// 再无限长下去。
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

		// 内层循环：模型还想用工具，就接着转。这个"只要还有工具调用就
		// 继续"的形状**就是** Agent。这个仓库里其他所有东西，都是围着
		// 它装的仪表。
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
			msgs = append(msgs, choice.Message) // 原样把 assistant 这一回合塞回历史

			fmt.Printf("  [tokens: prompt=%d completion=%d]\n",
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

			if choice.Message.Content != "" {
				fmt.Printf("\n%s\n", choice.Message.Content)
			}
			if len(choice.Message.ToolCalls) == 0 {
				fmt.Println()
				break // 没请求工具：这一回合结束
			}

			// 请求的调用全部执行完，在下一次请求之前把**所有**结果一起交回
			// 去。拆成几次请求发，等于教模型别再批量调用。
			for _, call := range choice.Message.ToolCalls {
				var args struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					// 参数格式不对是模型自己要修的问题，所以把解析错误
					// 交回去，而不是崩掉。
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
