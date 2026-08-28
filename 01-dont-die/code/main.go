// Stage 01 — Don't Die.
//
// Stage 00 was the idea. This is the same agent after meeting reality: a
// command that never returns, a command that prints 40MB, a model that gets cut
// off mid-sentence, and a command you really did not want run.
//
// Everything added here exists because the stage-00 agent failed at it. The
// docs (docs/01-dont-die.md) show you how to reproduce each failure first —
// breaking it yourself is the point.
//
// New in this stage:
//   - output truncation (head + tail, never the middle)
//   - command timeouts that kill the whole process tree, not just the shell
//   - a finish_reason state machine, including the silent `length` truncation
//   - a permission gate, where a denial is fed back to the model as data
//   - output sanitising: ANSI escapes, CRLF, and invalid UTF-8
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
// Configuration. All of it is a knob you should turn while reading the docs.
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
// Wire types — unchanged from stage 00 except for FinishReason, which we now
// actually read. See stage 03 for the protocol-neutral rewrite.
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
// The API call.
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
// Running a command without hanging forever.
// ---------------------------------------------------------------------------

type execResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Unreaped bool // killed, but the OS never released it: output is unsafe to read
	Duration time.Duration
}

// runBash executes one command under a timeout, killing the entire process tree
// if it expires.
//
// Two subtleties worth the reading time:
//
// Killing only cmd.Process is not enough. `npm start &` leaves a grandchild
// holding the same stdout pipe, and cmd.Wait() blocks until every writer to
// that pipe is gone — so a half-kill does not just leak a process, it hangs the
// agent that was trying to escape the hang. procGroup (proc_unix.go /
// proc_windows.go) is what makes the timeout actually work.
//
// stdout and stderr are captured separately rather than combined. That loses
// interleaving — you can no longer tell that a warning was printed *between*
// two results — but it gains attribution, and a model reading "this went to
// stderr" reasons about failure much better than one reading an undifferentiated
// blob. Combining is the other defensible choice; know which one you picked.
func runBash(cfg config, command string) execResult {
	started := time.Now()

	g, err := newProcGroup()
	if err != nil {
		return execResult{Stderr: fmt.Sprintf("could not create process group: %v", err), ExitCode: -1}
	}
	defer g.Close()

	cmd := exec.Command(cfg.shell, "-c", command)
	cmd.Stdin = nil // an interactive prompt must fail fast, not block
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	g.attach(cmd)

	if err := cmd.Start(); err != nil {
		return execResult{Stderr: fmt.Sprintf("could not start command: %v", err), ExitCode: -1}
	}
	if err := g.adopt(cmd); err != nil {
		// Not fatal: the command is already running and usually still killable.
		// Say so rather than pretending the tree is contained.
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

		// The kill is what unblocks Wait — but this chapter's whole lesson is
		// that escape hatches can hang too, so this one gets its own deadline.
		// If we give up here the Wait goroutine leaks (it holds the output
		// buffers until the OS finally releases the pipe). That is the right
		// trade: leaking one goroutine is survivable, wedging the agent is not.
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
		// Wait never returned, so the copying goroutines may still be writing
		// into these buffers. Reading them now is a data race — take nothing
		// and report the situation instead of gambling on it.
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

// render turns an execResult into the exact text the model will see.
//
// The model has no other window onto the world, so this function is the world.
// Everything it hides, the model cannot reason about; everything it garbles,
// the model reasons about wrongly.
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

	// Status line last, so it survives any truncation the model does on its own
	// side and is the closest thing to its next thought.
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

// truncate keeps the head and the tail and drops the middle.
//
// Head-only truncation is the common shortcut and it is the wrong one: the
// interesting part of a failing build is the last twenty lines, and the
// interesting part of a directory listing is the first twenty. Keeping both
// ends costs nothing and saves a re-run.
func truncate(s string, max int) (string, bool) {
	if max < 256 {
		max = 256
	}
	if len(s) <= max {
		return s, false
	}
	head := max * 2 / 3
	tail := max - head

	// Cut on rune boundaries — a half-written multi-byte character becomes an
	// invalid-UTF-8 byte in the JSON body, which some APIs reject outright.
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

// sanitize makes command output safe to put in a JSON request body.
//
// Three separate problems, all of which look like "weird characters" until you
// know which one you have:
//
//   - ANSI escapes: colour codes are pure noise to a model and cost tokens.
//   - CRLF: on Windows, every line ends \r\n and the \r survives into the
//     context window, where it is invisible and doubles nothing useful.
//   - invalid UTF-8: a program that writes in the local code page (GBK on a
//     Chinese Windows, Shift-JIS on a Japanese one) produces bytes that are not
//     valid UTF-8 at all. Left alone they either corrupt the request or arrive
//     as mojibake. We replace them so the failure is visible rather than silent;
//     if you need real transcoding, that is golang.org/x/text/encoding, and it
//     is deliberately not a dependency of this repo.
func sanitize(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return s
}

// ---------------------------------------------------------------------------
// The permission gate.
// ---------------------------------------------------------------------------

type gate struct {
	yolo      bool
	always    bool
	in        *bufio.Scanner
	available bool // false when stdin is piped: there is nobody to ask
}

type verdict int

const (
	allow verdict = iota
	deny
	abort
)

// ask shows the command and waits for a decision.
//
// The design point is what happens on a denial: the model is told, in a tool
// result, that the user refused. It is not an error and it does not end the
// turn. That keeps the agent in a position to adapt — propose something
// narrower, or ask why — instead of dying at the one moment a human was paying
// attention.
//
// This gate is also the honest argument against "bash is all you need": all it
// can show a user is an opaque command string. A dedicated `write_file` tool
// could show a diff; a dedicated `send_email` tool could show the recipient.
// Breadth costs you the ability to ask a good question.
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
// Shell discovery.
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
// The loop.
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

	// A piped stdin has no human behind it, so there is nobody to answer the
	// gate. Detect that up front instead of silently denying every command.
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

// runTurn drives one user message to completion and returns the grown history.
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

		// The finish_reason state machine. Stage 00 branched only on "are there
		// tool calls", which silently treats a cut-off answer as a finished one.
		switch choice.FinishReason {
		case "stop", "end_turn", "":
			if len(choice.Message.ToolCalls) == 0 {
				fmt.Println()
				return msgs
			}
			// Some providers say "stop" while still emitting tool calls; trust
			// the calls, not the label.

		case "length", "max_tokens":
			// The model hit max_tokens mid-generation. If it was mid-tool-call,
			// the arguments are a truncated JSON string and must not be run:
			// half a shell command is not a safer shell command.
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
			// The normal path; fall through.

		default:
			fmt.Printf("\n[unknown finish_reason %q — treating as a finished turn]\n\n", choice.FinishReason)
			return msgs
		}

		if len(choice.Message.ToolCalls) == 0 {
			fmt.Println()
			return msgs
		}

		// Every tool_call must come back with a result, including the ones we
		// decide not to run. Breaking out of this loop early leaves an
		// unanswered call in the history, and the *next* request — possibly
		// several user messages later — is rejected as malformed. Bugs like
		// that are why the rule is "answer all of them, always".
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

// parseBashArgs turns the model's tool arguments into a command, and refuses
// everything that is not one.
//
// This function exists because of a real, observed failure. Ask this gateway for
// a tool call with too small a max_tokens and it returns:
//
//	stop_reason: "tool_use"          <- claims the call is fine
//	input:       {"raw_arguments":""} <- the schema's `command` key is absent
//
// Now watch what the obvious Go code does with that:
//
//	var args struct{ Command string `json:"command"` }
//	json.Unmarshal(data, &args)   // returns nil. no error. none at all.
//	args.Command                  // ""
//
// The unmarshal SUCCEEDS. Go fills absent keys with the zero value, so a missing
// required field and an empty one are indistinguishable — and the agent goes on
// to run an empty command as if the model had asked for it.
//
// **Unmarshalling without error is not validation.** A *string tells absent
// (nil) apart from empty (""), and both are rejected here with a message the
// model can act on. Whatever protocol you are on, validate the arguments
// against the schema you published; the envelope's own stop_reason is not
// evidence that what it wrapped is usable.
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
