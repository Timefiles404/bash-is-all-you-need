// Stage 02 — See Everything.
//
// The same agent as stage 01, with one structural change: it prints nothing.
// It emits events, and the things you can see subscribe to them.
//
//	agent core ──emit──▶ Bus ──┬──▶ renderer   (the terminal, instrumented)
//	                           └──▶ TraceWriter (session.jsonl, one event per line)
//
//	replay: session.jsonl ──▶ Replay ──▶ the same renderer, no network, no key
//
// Everything new in this stage falls out of that one decision. Read events.go
// for the argument, then this file for the wiring, then render.go for what the
// numbers mean.
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
// Wire types. Still hand-written, still the OpenAI protocol only — stage 03 is
// where a second protocol arrives and these move behind a neutral core.
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

	// A real OpenAI endpoint will not stream usage without this. The gateway
	// this repo was developed against sends usage either way — see
	// docs/wire-notes.md §B5, where the flag is measurably a no-op. Send it
	// anyway: it costs nothing and the alternative is an agent that reports
	// zero tokens the day someone points it at a different provider.
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

// stream sends one request and lets the SSE parser turn the response into
// events. Note what this function does NOT do: it never formats anything for a
// human. The only reason it can be read in one screen is that presentation left
// the building.
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

	// The request inspector, and the single most useful line in any trace: the
	// only record of what the model actually saw. Everything else in a
	// transcript is a reconstruction.
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	req, err := http.NewRequest("POST", c.cfg.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	// Started is stamped as late as possible, so TTFT measures what the user
	// experiences — network included — rather than what the model did.
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// Worth knowing before you write a retry policy: on this gateway an
		// unknown model id returns 401, and a malformed body returns 500. A
		// naive "retry all 5xx" loop will retry a client bug forever.
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseOpenAIStream(resp.Body, bus, turn, started)
}

// ---------------------------------------------------------------------------
// The permission gate. Unchanged in substance from stage 01; it now reports its
// verdict as an event so a denial is visible in the trace six months later.
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

	// Replay needs no key, no shell and no network. That is the point: a
	// student can study a real session they never paid for, and you can debug
	// a user's run from the file they sent you.
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

// runTurn drives one user message to completion, returning the grown history
// and the size of the last prompt sent (which is the context watermark).
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

		// Rebuild the assistant message the API would have returned
		// non-streamed, because that is what has to go back in the history.
		// Reassembly is the tax you pay for streaming, and forgetting it is
		// why streaming agents "lose" their tool calls.
		am := message{Role: "assistant", Content: res.Text}
		for _, tc := range res.ToolCalls {
			var call toolCall
			call.ID, call.Type = tc.ID, "function"
			call.Function.Name, call.Function.Arguments = tc.Name, tc.Args
			am.ToolCalls = append(am.ToolCalls, call)
		}
		msgs = append(msgs, am)

		// Note what is NOT here: a KindResponseEnd. The stream parser already
		// emitted one, because it is the component that knows when the response
		// actually ended and whether it ended cleanly. Emitting a second from
		// here is the bug this comment exists to stop you re-introducing — two
		// components each believing they own an event is the most common way an
		// event-driven design goes wrong, and it shows up as a duplicated,
		// half-empty panel rather than as a crash.

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

		// Every tool call gets a result, including the ones we refuse. An
		// unanswered call makes the *next* request malformed, possibly several
		// user messages later.
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

// toolResult emits the result and returns the message to append, so the thing
// the user sees and the thing the model is told can never drift apart.
func toolResult(bus *Bus, turn int, callID, content string) message {
	bus.Emit(Event{Kind: KindToolResult, Turn: turn, ToolID: callID, Text: content})
	return message{Role: "tool", ToolCallID: callID, Content: content}
}
