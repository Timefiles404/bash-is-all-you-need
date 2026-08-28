// Stage 05 — Live Forever: the agent loop, with a context window it survives.
//
// Three horizons, and every agent needs all three:
//
//	within a request  — the messages array. Stages 00–04.
//	within a session  — compaction, when the array outgrows the window.
//	across sessions   — a file. That is the whole mechanism.
//
// The diff against stage 04 is small and lands in three places: the system
// prompt now carries memory and stable environment (memory.go), each user turn
// freezes a volatile snapshot alongside it, and the top of the tool loop checks
// whether the conversation is about to hit the wall (compact.go).
//
// One structural change worth noting: the long-lived pieces — provider, bus,
// gate, config, compactor — moved onto an `agent` struct. Stage 04's runTurn
// took eight parameters and stage 05 needs three more. A receiver is not an
// abstraction here; it is the same values with a shorter name.
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

const basePrompt = `You are a coding agent working in a terminal on the user's machine.

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

// memoryPrompt is the entire long-term-memory feature.
//
// No tool, no store, no embedding, no retrieval step: a file, and a sentence
// telling the model it may append to it with the tool it already has. The last
// line is the part that decides whether the file is worth reading in six
// months — "record what you learned, not what you did" is the difference
// between a knowledge base and a diary.
const memoryPrompt = `

Durable notes live in ` + memoryFileForWriting + ` in the working directory. If that file
exists, its contents are already in your context above.

When you learn something about this project that would cost you tool calls to
rediscover in a future session — a build command, where something lives, a
gotcha, a decision the user made — append it:

  printf '\n- <one short factual line>\n' >> ` + memoryFileForWriting + `

Record what you learned, not what you did. Notes written now take effect in your
next session, not this one.`

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

// agent holds everything that lives for the whole session.
type agent struct {
	p     Provider
	httpc *http.Client
	g     *gate
	bus   *Bus
	cfg   config
	comp  *compactor

	// system is a function, not a string, because of stage 04's --break-cache
	// experiment: a value computed once at startup is a constant prefix, and
	// only a value recomputed per request invalidates anything. Keeping the
	// indirection makes that difference expressible.
	system func() string

	memoryDir  string
	lastPrompt int
}

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

		noCache    = flag.Bool("no-cache", false, "omit cache_control breakpoints (stage 04 control arm)")
		breakCache = flag.Bool("break-cache", false, "put a fresh timestamp in the system prompt on every request (stage 04)")

		// Stage 05.
		compactAt = flag.Float64("compact-at", 0.70, "compact when the estimated prompt passes this fraction of the window")
		keepAt    = flag.Float64("keep", 0.30, "fraction of the window to leave in place after compacting")
		noCompact = flag.Bool("no-compact", false, "never compact — ride the window until the API refuses (control arm)")
		window    = flag.Int("window", 0, "override the provider's context window, in tokens")
		noMemory  = flag.Bool("no-memory", false, "do not read AGENTS.md / MEMORY.md")
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
	if *window > 0 {
		pcfg.Window = *window
	}

	view := newRenderer(os.Stdout, colorEnabled(os.Stdout),
		prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
			cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite},
		pcfg.Window)
	view.showRequest = *showReq

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

	wd, _ := os.Getwd()
	fmt.Printf("stage 05 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, provider.Protocol(), provider.Model(), wd)

	// ---- the system prompt, assembled once ------------------------------
	//
	// Everything here is stable for the whole session, which is what earns it a
	// place before the cache breakpoint. Anything that moves goes into the
	// message stream instead — see memory.go's placement rule.
	memory := ""
	if !*noMemory {
		memory, _ = loadMemory(wd, bus)
	}
	full := basePrompt + "\n\n" + stableContext(shell, wd) + memoryPrompt
	if memory != "" {
		full += "\n\n" + memory
	}
	sys := func() string { return full }
	if *breakCache {
		sys = func() string {
			return "Current time: " + time.Now().Format(time.RFC3339Nano) + "\n\n" + full
		}
		fmt.Println("--break-cache: a fresh timestamp goes into the system prompt on every request")
	}

	comp := newCompactor(pcfg.Window, *compactAt, *keepAt)
	if *noCompact {
		comp.threshold = 0
	}
	if pcfg.Window <= 0 && !*noCompact {
		fmt.Println("note: this provider has no `window` configured, so compaction can never fire. Set it, or pass --window.")
	}

	a := &agent{
		p: provider, httpc: &http.Client{Timeout: 10 * time.Minute},
		g:   &gate{yolo: cfg.yolo, in: stdin, available: interactive, out: os.Stdout},
		bus: bus, cfg: cfg, comp: comp, system: sys, memoryDir: wd,
	}

	var msgs []Msg
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
		if handled, next := a.command(line, msgs); handled {
			msgs = next
			continue
		}

		bus.Emit(Event{Kind: KindUserMessage, Text: line})
		// The volatile snapshot is taken HERE, once, and frozen into the
		// message. It is never recomputed, which is the entire reason the cache
		// survives a session that knows what time it is.
		msgs = append(msgs, userTurn(line, volatileContext(shell, time.Now())))
		msgs = a.runTurn(msgs)
	}
	view.SessionSummary(a.lastPrompt)
}

// command handles the slash commands. They exist for the experiments in
// 05-live-forever/doc/: compaction that only fires when the window is nearly
// full is hard to demonstrate and harder to test.
func (a *agent) command(line string, msgs []Msg) (bool, []Msg) {
	switch {
	case line == "/help":
		fmt.Println("  /compact          compact the conversation now")
		fmt.Println("  /remember <note>  append a line to " + memoryFileForWriting)
		fmt.Println("  /context          show what the conversation currently costs")
		return true, msgs

	case line == "/compact":
		base := len(a.system()) + toolChars()
		cut, why := a.comp.plan(msgs, base)
		if cut < 0 {
			a.bus.Notice("%s", why)
			return true, msgs
		}
		out, err := a.comp.run(a.p, a.httpc, a.bus, msgs, cut, base)
		if err != nil {
			a.bus.Error("compaction failed: %v — the conversation is unchanged", err)
		}
		return true, out

	case line == "/context":
		base := len(a.system()) + toolChars()
		fmt.Printf("  %d messages · %d chars of history + %d chars of system/tools\n",
			len(msgs), convChars(msgs), base)
		fmt.Printf("  estimated prompt: ~%d tokens at %.2f chars/token (%d calibration samples)\n",
			a.comp.estimate(msgs, base), a.comp.est.ratio, a.comp.est.obs)
		if a.lastPrompt > 0 {
			fmt.Printf("  last call actually billed: %d prompt tokens\n", a.lastPrompt)
		}
		if problem := validConversation(msgs); problem != "" {
			fmt.Printf("  MALFORMED: %s\n", problem)
		}
		return true, msgs

	case strings.HasPrefix(line, "/remember "):
		note := strings.TrimSpace(strings.TrimPrefix(line, "/remember "))
		if err := remember(a.memoryDir, note); err != nil {
			a.bus.Error("could not write memory: %v", err)
			return true, msgs
		}
		a.bus.Notice("noted in %s — it takes effect next session, not this one", memoryFileForWriting)
		return true, msgs
	}
	return false, msgs
}

// toolChars is the character cost of the tool definitions, which are part of
// every prompt and are otherwise invisible to the estimator.
func toolChars() int {
	n := 0
	for _, t := range []Tool{bashToolDef()} {
		n += len(t.Name) + len(t.Description) + 200 // the schema, near enough
	}
	return n
}

// call performs one model call.
func (a *agent) call(turn int, msgs []Msg) (*CallResult, error) {
	req, body, err := a.p.BuildRequest(a.system(), msgs, []Tool{bashToolDef()}, 4096)
	if err != nil {
		return nil, err
	}
	a.bus.Emit(Event{Kind: KindRequest, Turn: turn, Request: body})

	started := time.Now()
	resp, err := a.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return a.p.ParseStream(resp.Body, a.bus, turn, started)
}

func (a *agent) runTurn(msgs []Msg) []Msg {
	for turn := 1; ; turn++ {
		if turn > a.cfg.maxTurns {
			a.bus.Notice("stopped: hit the %d-turn limit", a.cfg.maxTurns)
			return msgs
		}

		// ---- the wall check -------------------------------------------
		//
		// It goes HERE, at the top of the tool loop, not at the top of the user
		// loop. The thing that fills a context window is not the conversation,
		// it is the tool output inside one turn: a single `find /` can add more
		// than an hour of chat. Checking only between user messages means the
		// wall is hit mid-turn, which is the one place there is no graceful
		// recovery.
		base := len(a.system()) + toolChars()
		if est := a.comp.estimate(msgs, base); a.comp.due(est) {
			cut, why := a.comp.plan(msgs, base)
			if cut < 0 {
				a.bus.Notice("%s", why)
			} else if out, err := a.comp.run(a.p, a.httpc, a.bus, msgs, cut, base); err != nil {
				a.bus.Error("compaction failed: %v — continuing uncompacted", err)
			} else {
				msgs = out
			}
		}

		a.bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		sentChars := convChars(msgs) + base
		res, err := a.call(turn, msgs)
		if err != nil {
			a.bus.Error("%v", err)
			return msgs
		}
		a.lastPrompt = res.Usage.Prompt()
		// Calibrate. This is the only reason the agent can decide when to
		// compact without vendoring a tokenizer: the server just told us
		// exactly how many tokens the characters we sent turned into.
		a.comp.est.observe(sentChars, res.Usage.Prompt())

		am := Msg{Role: RoleAssistant}
		if res.Text != "" {
			am.Blocks = append(am.Blocks, Block{Kind: BlockText, Text: res.Text})
		}
		am.Blocks = append(am.Blocks, res.Calls...)

		// A model can return nothing at all — no text, no tool call — and
		// appending that produces a message with an empty content array, which
		// the Anthropic protocol rejects on the *next* request. Stage 04 had
		// this latent; validConversation() in compact.go is what found it.
		if len(am.Blocks) == 0 {
			a.bus.Notice("the model returned an empty response (wire: %q) — not adding it to the history", res.RawStop)
			return msgs
		}
		msgs = append(msgs, am)

		switch res.Stop {
		case StopMaxTokens:
			a.bus.Notice("the model was cut off at max_tokens (wire: %q)", res.RawStop)
			if len(res.Calls) == 0 {
				return msgs
			}
			msgs = append(msgs, a.resultsMsg(turn, res.Calls,
				func(Block) string {
					return "[not executed: your reply was cut off at max_tokens. Retry with a shorter command.]"
				}))
			continue

		case StopFiltered:
			a.bus.Notice("the provider filtered this response (wire: %q)", res.RawStop)
			return msgs

		case StopUnknown, "":
			a.bus.Notice("unknown stop reason %q — treating the turn as finished", res.RawStop)
			return msgs
		}

		if len(res.Calls) == 0 {
			a.bus.Emit(Event{Kind: KindTurnEnd, Turn: turn})
			return msgs
		}

		results := Msg{Role: RoleUser}
		stop := false
		for _, c := range res.Calls {
			if stop {
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, "[not executed: the session was stopped.]"))
				continue
			}
			command, err := parseBashArgs(c.Args)
			if err != nil {
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, fmt.Sprintf("[%v]", err)))
				continue
			}
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name, Command: command})

			v, why := a.g.ask(command)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID,
					"[the user denied this command. Do not retry it unchanged.]"))
				continue
			case abort:
				stop = true
				results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, "[the user stopped the session.]"))
				continue
			}

			a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: c.ID, Command: command})
			r := runBash(a.cfg.shell, command, a.cfg.timeout)
			rendered, truncated := r.render(a.cfg.maxOutput)
			a.bus.Emit(Event{
				Kind: KindCommandEnd, Turn: turn, ToolID: c.ID, Command: command,
				ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
				Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
			})
			results.Blocks = append(results.Blocks, a.emitResult(turn, c.ID, rendered))
		}
		msgs = append(msgs, results)
		if stop {
			return msgs
		}
	}
}

func (a *agent) emitResult(turn int, callID, content string) Block {
	a.bus.Emit(Event{Kind: KindToolResult, Turn: turn, ToolID: callID, Text: content})
	return ToolResultBlock(callID, content)
}

func (a *agent) resultsMsg(turn int, calls []Block, text func(Block) string) Msg {
	m := Msg{Role: RoleUser}
	for _, c := range calls {
		m.Blocks = append(m.Blocks, a.emitResult(turn, c.ID, text(c)))
	}
	return m
}
