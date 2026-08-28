// Stage 03 — Babel: the agent loop.
//
// Compare this file with stage 02's main.go. The diff is the whole chapter:
// every vendor word is gone. There is no `tool_calls`, no `finish_reason`, no
// `input_tokens`, no `chat/completions`. The loop talks in Msg, Block and
// StopReason, and a Provider translates at the wire.
//
// The test of an abstraction like this is not whether it compiles. It is
// whether adding the second protocol changed this file. It did not — the loop
// you are reading is stage 02's, with its vocabulary replaced.
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
// The permission gate. Unchanged since stage 01 except that it reports through
// the bus.
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

		// The two experiment arms of 04-the-cache/doc/. Neither belongs in a
		// real agent; both exist so the chapter can put a number on advice that
		// is otherwise just advice.
		noCache    = flag.Bool("no-cache", false, "omit cache_control breakpoints (control arm)")
		breakCache = flag.Bool("break-cache", false, "put a timestamp in the system prompt — the classic silent invalidator")
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

	// resolveErr is deliberately NOT fatal here.
	//
	// Replay needs no key, no shell, no network and no provider — that promise
	// is stage 02's, it is in the README, and from stage 03 until this line was
	// written it was false: resolve() moved above the replay branch and took
	// its os.Exit(1) with it. On a machine with the env vars set (which is
	// every machine the author tested on) nothing looked wrong. On a machine
	// with a trace file and nothing else — which is exactly the machine the
	// feature exists for — `--replay` printed "no provider configured".
	//
	// So the error is carried rather than raised, and checked below, on the one
	// path that actually needs a provider. A config error should be fatal to the
	// code that depends on the config and to nothing else.
	pcfg, pname, resolveErr := pf.resolve(*providerName)

	view := newRenderer(os.Stdout, colorEnabled(os.Stdout),
		prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
			cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite},
		pcfg.Window)
	view.showRequest = *showReq

	// Replay needs no key, no shell, no network — and now, no provider either.
	// A trace recorded against one protocol replays identically, because what
	// was recorded was events, not wire format.
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
	provider, err := pcfg.build(!*noCache)
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
	fmt.Printf("stage 04 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, provider.Protocol(), provider.Model(), wd)

	// --break-cache demonstrates the single most common way a cache is lost.
	//
	// Note that this is a FUNCTION, evaluated per request, and that detail is
	// the entire experiment. The first version of this flag stamped the time
	// once at startup — and the cache kept working perfectly, because a value
	// that is constant for a session is a constant prefix for that session. The
	// bug people actually ship is `datetime.now()` inside a prompt builder that
	// runs on every call, and only that version invalidates anything.
	//
	// The timestamp sits at the FRONT of the rendered prefix, so it invalidates
	// tools, system, and every message after them, on every single request.
	// Nothing errors. The bill just goes up.
	sys := func() string { return systemPrompt }
	if *breakCache {
		sys = func() string {
			return "Current time: " + time.Now().Format(time.RFC3339Nano) + "\n\n" + systemPrompt
		}
		fmt.Println("--break-cache: a fresh timestamp goes into the system prompt on every request")
	}

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
		msgs, lastPrompt = runTurn(provider, httpc, g, bus, cfg, sys, msgs, lastPrompt)
	}
	view.SessionSummary(lastPrompt)
}

// call performs one model call. Note that it names no protocol: it asks the
// provider for a request, sends it, and asks the provider to parse the reply.
func call(p Provider, httpc *http.Client, bus *Bus, turn int, system func() string, msgs []Msg) (*CallResult, error) {
	req, body, err := p.BuildRequest(system(), msgs, []Tool{bashToolDef()}, 4096)
	if err != nil {
		return nil, err
	}

	// The request inspector, and the only record in a trace of what the model
	// actually saw. Emitted post-translation, deliberately: the interesting
	// bytes are the ones that went on the wire, not the neutral form.
	bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	started := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// Worth knowing before writing a retry policy: on this gateway an
		// unknown model id returns 401 (not 404) and a malformed body returns
		// 500. "Retry every 5xx" will retry a client bug forever.
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return p.ParseStream(resp.Body, bus, turn, started)
}

func runTurn(p Provider, httpc *http.Client, g *gate, bus *Bus, cfg config, system func() string, msgs []Msg, lastPrompt int) ([]Msg, int) {
	for turn := 1; ; turn++ {
		if turn > cfg.maxTurns {
			bus.Notice("stopped: hit the %d-turn limit", cfg.maxTurns)
			return msgs, lastPrompt
		}
		bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		res, err := call(p, httpc, bus, turn, system, msgs)
		if err != nil {
			bus.Error("%v", err)
			return msgs, lastPrompt
		}
		lastPrompt = res.Usage.Prompt()

		// Rebuild the assistant turn for the history. Thinking is deliberately
		// not replayed: neither protocol requires it back, one of them charges
		// for it, and it is in the trace either way.
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
			// Never treat an unrecognised state as success. RawStop is printed
			// because the literal string is the only thing that will tell you
			// what actually happened.
			//
			// The empty case is not paranoia: it is the zero value of
			// StopReason, which is what you get if an adapter ever forgets to
			// call normaliseStop, or if a stream dies before any stop reason
			// arrived. Without it, "we never found out why generation stopped"
			// falls through to the same branch as "the model finished talking".
			bus.Notice("unknown stop reason %q — treating the turn as finished", res.RawStop)
			return msgs, lastPrompt
		}

		if len(res.Calls) == 0 {
			bus.Emit(Event{Kind: KindTurnEnd, Turn: turn})
			return msgs, lastPrompt
		}

		// Every call gets a result, including refused ones: an unanswered call
		// makes the NEXT request malformed, possibly several user messages
		// later. The results go into one neutral message; each adapter decides
		// what that looks like on the wire, and they disagree completely.
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

// emitResult publishes the tool result and returns the block to append, so what
// the user sees and what the model is told can never drift apart.
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
