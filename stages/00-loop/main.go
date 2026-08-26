// Stage 00 — The Loop.
//
// This is the entire idea of a coding agent, with everything that makes it
// survivable deliberately left out. One tool (bash), one loop, raw net/http,
// no SDK, no streaming, no output truncation, no command timeout, and no
// permission gate. Stage 01 adds the parts that stop it from hurting itself.
//
// Read main() first, then callModel(), then runBash(). That is the whole thing.
//
// Run it in a scratch directory. It executes whatever the model asks for.
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

// A fuse. Without it, a model that keeps calling tools loops until your key
// runs dry. Stage 01 turns this into a real budget.
const maxTurns = 25

// ---------------------------------------------------------------------------
// Wire types — the OpenAI chat-completions protocol, hand-written.
//
// Every field here exists because the API sends it. Nothing is abstracted yet;
// stage 03 (Babel) is where these get split behind a provider-neutral type so a
// second protocol can plug in.
// ---------------------------------------------------------------------------

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	// Only set on role:"tool" messages — it pairs a result with the call that
	// asked for it. The Anthropic protocol does this differently; see stage 03.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Note: a JSON *string* containing JSON, not a nested object. This trips
		// up everyone once. Always json.Unmarshal it; never string-match it.
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

// bashTool is the only tool this agent will ever have.
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
// The API call. Raw net/http — there is no magic under an SDK, only this.
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
// The tool. Every action the agent can take goes through these ten lines.
// ---------------------------------------------------------------------------

// findBash locates a POSIX shell. On Windows that means Git Bash, which every
// developer with git installed already has. Stage 08 replaces this with an
// embedded interpreter; until then we borrow the system's.
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

// runBash executes one command and returns everything the model should see.
//
// Note what it does NOT do: no timeout, so a dev server hangs the agent
// forever; no output cap, so `find /` floods the context window. Both are
// stage 01. Note also that a non-zero exit is not an error here — it is an
// observation, and the model is the one who should react to it.
func runBash(shell, command string) string {
	cmd := exec.Command(shell, "-c", command)
	cmd.Stdin = nil // never let a command block waiting on input
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
// The loop.
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

	// The conversation. It only ever grows — that is the agent's short-term
	// memory, and stage 05 is where it stops growing forever.
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

		// Inner loop: keep going while the model wants to use tools. This
		// while-there-are-tool-calls shape IS the agent. Everything else in this
		// repo is instrumentation around it.
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
			msgs = append(msgs, choice.Message) // echo the assistant turn back verbatim

			fmt.Printf("  [tokens: prompt=%d completion=%d]\n",
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

			if choice.Message.Content != "" {
				fmt.Printf("\n%s\n", choice.Message.Content)
			}
			if len(choice.Message.ToolCalls) == 0 {
				fmt.Println()
				break // no tools requested: the turn is over
			}

			// Execute every requested call, then return ALL results before the
			// next request. Splitting them across separate requests teaches the
			// model to stop batching calls.
			for _, call := range choice.Message.ToolCalls {
				var args struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					// Malformed arguments are the model's problem to fix, so hand
					// the parse error back instead of crashing.
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
