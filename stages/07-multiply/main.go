// Stage 07 — Multiply: the same loop, called again.
//
// Two features, and neither of them is a subsystem:
//
//	subagents  a fresh []Msg, a different system prompt, the same everything
//	           else — and only TEXT comes back. subagent.go.
//	skills     a directory of Markdown files, and one paragraph in the system
//	           prompt saying they exist. skills.go.
//
// The diff against stage 06 is small. call() asks a.tools() instead of naming
// bash; the forty-line tool dispatch in runTurn became one call to dispatch(),
// which runs subagents concurrently and returns their results in the order the
// model asked for them; and the Bus grew a Fork so that a tree of agents writes
// into one ordered stream.
//
// What did NOT change is the point. There is no scheduler, no message queue, no
// agent registry, and no protocol between agents. A subagent is a function call
// whose return value is a paragraph.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"bash-is-all-you-need/tui"
	"bash-is-all-you-need/tui/settings"
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

// para is a blank line. It is a constant rather than a literal at each use
// site only because it appears in four places that must agree: the system
// prompt the parent sees and the one every subagent sees have to be
// byte-identical below the first paragraph, or the two share no cache prefix.
var para = string([]rune{0x0A, 0x0A})

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
	subTurns  int
	yolo      bool
}

// ---------------------------------------------------------------------------
// The permission gate. Unchanged since stage 01 except that it reports through
// the bus.
// ---------------------------------------------------------------------------

type gate struct {
	yolo, always bool
	available    bool
	out          io.Writer

	// read is where the answer comes from.
	//
	// A function rather than the *bufio.Scanner this held from stage 01 until
	// the interactive shell in tui/ arrived. That shell puts the terminal in raw
	// mode and owns stdin; a Scanner reading the same descriptor would take
	// keystrokes out of the line the user is typing, and the two readers would
	// each get half the answer. So the reader is supplied, and a nil one is the
	// same case as `available: false` — there is nothing to ask on.
	read func() (string, bool)

	// One question at a time, and it genuinely contends.
	//
	// The first version of this comment claimed it would not: dispatch() asks
	// every question on one goroutine before starting any concurrency, so the
	// parent's questions are serial. That reasoning was wrong, and the way it
	// was wrong is worth more than the lock.
	//
	// A subagent runs the same dispatch() on its own goroutine, and its bash
	// calls ask this same shared gate — mid-concurrency, alongside its
	// siblings. The lock stops two prompts from interleaving character by
	// character. It does not stop something worse, because the command text and
	// the question reach the terminal by different paths under different locks:
	//
	//	the command   printed by the RENDERER, off the bus, under bus.core.mu
	//	the question  printed HERE, under gate.mu
	//
	// Two locks, one terminal, no ordering between them. The fix is not a third
	// lock — it is in ask() below, where the question now names its own subject.
	mu sync.Mutex
}

type verdict string

const (
	allow verdict = "allow"
	deny  verdict = "deny"
	abort verdict = "abort"
)

func (g *gate) ask(command string) (verdict, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.yolo || g.always {
		return allow, ""
	}
	if !g.available || g.read == nil {
		return deny, "no terminal to ask on — rerun with --yolo to allow commands"
	}
	// The question names the command it is about.
	//
	// It did not until stage 07, and until stage 07 it did not need to: under a
	// strictly sequential print-then-ask loop, "run?" can only ever refer to
	// the line above it. Concurrent subagents removed that guarantee, and the
	// failure it leaves is not a display glitch:
	//
	//	│ $ rm -rf /tmp/build            <- child A's command, via the bus
	//	│ $ echo hello                   <- child B's command, via the bus
	//	  run? [y / n / a = all / q]     <- child A's question
	//
	// The user answers for the command they just read and authorises the other
	// one. A prompt that carries its own subject cannot be misread whatever
	// else is on the screen, and it costs one line.
	//
	// Note also what `a` is now honest about. It sets `always` on the SHARED
	// gate, so one subagent's "allow all" disarms the gate for the parent and
	// every sibling too. Scoping it per-agent would be safer and would mean
	// being asked again for every child, which is how people end up running
	// --yolo. The choice stays; the prompt stops hiding it.
	fmt.Fprintf(g.out, "  run? %s\n  [y / n / a = all, this session, every agent / q = stop] ",
		oneLineDim(command, 72))
	line, ok := g.read()
	if !ok {
		return abort, "input closed"
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
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

// lineReader is the gate's reader for the plain prompt: one line off the same
// Scanner the user types their messages on.
func lineReader(in *bufio.Scanner) func() (string, bool) {
	return func() (string, bool) {
		if !in.Scan() {
			return "", false
		}
		return in.Text(), true
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

	// stable is the environment + memory + skills block, shared verbatim with
	// every subagent. Computed once; see stage 05's placement rule for why it
	// must never be recomputed.
	stable string

	// Stage 07.
	depth    int // 0 is the agent the human is talking to
	maxDepth int
	subTurns int

	// out is where this file's own writing goes — the slash commands below, and
	// nothing else. Stdout for the plain prompt, and the shell's output pane
	// under the interactive one. It exists because a bare fmt.Println inside an
	// alternate screen lands underneath the frame, where it corrupts the layout
	// and is never seen.
	out io.Writer

	mu        sync.Mutex
	children  int
	spent     Usage // this agent's own token consumption, for the subagent report
	turnsUsed int
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

		// Stage 06. The composer is a *reader*, so it takes a path and needs
		// nothing else — no key, no provider, no network. That is not a
		// limitation, it is the payoff of stage 02's decision to make the trace
		// the source of truth rather than a debug log.
		composerAt = flag.String("composer", "", "open the TUI on a trace file instead of running the agent")

		// The same views, printed instead of drawn.
		//
		// This is not a debug hatch. A TUI is a dead end for anything you want
		// to diff, grep, paste into an issue, or check in CI, and "what did the
		// model see on call 12" is exactly the kind of question whose answer
		// you want to pipe. Rendering and drawing were separate functions
		// already (views.go returns lines; term.go paints them), so this costs
		// eight lines — the payoff of not letting the UI own the data.
		dumpAt   = flag.String("composer-dump", "", "print one composer view for a trace and exit")
		dumpView = flag.String("view", "model", "composer-dump: god | model | wire")
		dumpCall = flag.Int("call", 1, "composer-dump: which model call (1-based)")
		dumpW    = flag.Int("width", 100, "composer-dump: render width in columns")

		noCache    = flag.Bool("no-cache", false, "omit cache_control breakpoints (stage 04 control arm)")
		breakCache = flag.Bool("break-cache", false, "put a fresh timestamp in the system prompt on every request (stage 04)")

		// Stage 05.
		compactAt = flag.Float64("compact-at", 0.70, "compact when the estimated prompt passes this fraction of the window")
		keepAt    = flag.Float64("keep", 0.30, "fraction of the window to leave in place after compacting")
		noCompact = flag.Bool("no-compact", false, "never compact — ride the window until the API refuses (control arm)")
		window    = flag.Int("window", 0, "override the provider's context window, in tokens")
		noMemory  = flag.Bool("no-memory", false, "do not read AGENTS.md / MEMORY.md")

		// Stage 07.
		maxDepth = flag.Int("max-depth", 1, "how deep subagents may nest; 0 removes the task tool entirely")
		noSkills = flag.Bool("no-skills", false, "do not index skills/*/SKILL.md")

		// The out-of-process subagent, for the comparison in docs/07. One prompt
		// in, one report out, no REPL — which is all a subagent ever was, and is
		// why `agent --subagent "..."` run from bash is a working subagent
		// mechanism with no task tool involved at all.
		subagentAt = flag.String("subagent", "", "run one subagent task, print its report, and exit")

		// The interactive shell, in tui/. Not part of the course; see shell.go.
		printOnly  = flag.String("p", "", "run one prompt without a UI, print the reply, and exit")
		noTUI      = flag.Bool("no-tui", false, "use the plain line prompt instead of the interactive shell")
		settingsAt = flag.String("settings", "", "path to the saved settings file; empty means the one under your user config directory")
	)
	cfg := config{}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds per user message")
	flag.IntVar(&cfg.subTurns, "sub-turns", 15, "tool-call rounds a subagent gets")
	flag.BoolVar(&cfg.yolo, "yolo", false, "run every command without asking")
	flag.Parse()

	// Before anything else: the composer never needs a provider, and making it
	// wait for one would mean you cannot read a trace on a machine that has no
	// key configured — which is most of the machines you would want to read a
	// trace on.
	if *dumpAt != "" {
		if err := dumpComposer(*dumpAt, *dumpView, *dumpCall, *dumpW, os.Stdout); err != nil {
			tui.Die(err)
		}
		return
	}
	if *composerAt != "" {
		if err := runComposer(*composerAt); err != nil {
			tui.Die(err)
		}
		return
	}

	// Saved settings, read into the environment before anything looks at it.
	//
	// They lose to anything already set, which is the rule that keeps `.env`,
	// CI and `set -a` behaving exactly as they did before this file existed —
	// see settings.ExportMissing. A file that cannot be parsed is reported and
	// then left completely alone: the settings commands turn themselves off
	// rather than risk writing over a key that is in there somewhere.
	store, storeErr := settings.Load(*settingsAt)
	if storeErr != nil {
		// Reported later, not here. Printing at the point of failure puts the
		// message on the screen a fraction of a second before the alternate
		// screen goes up on top of it, so under the shell the settings commands
		// vanish and the only explanation was displayed and then covered over.
		store = nil
	} else {
		store.ExportMissing()
	}

	pf, err := loadProviders(*providersAt)
	if err != nil {
		tui.Die(err)
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
	// The window, in the order the three sources beat each other: the flag, then
	// what /provider-window saved, then whatever providers.json said. A session
	// configured entirely through the shell has no entry in that file, so
	// without the middle line it has no window at all — and with no window the
	// compactor can never fire and the context field on the status row stays
	// blank with nothing on screen to say why.
	switch {
	case *window > 0:
		pcfg.Window = *window
	case savedWindow() > 0:
		pcfg.Window = savedWindow()
	}

	view := newRenderer(os.Stdout, colorEnabled(os.Stdout),
		prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
			cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite},
		pcfg.Window)
	view.showRequest = *showReq

	if *replayPath != "" {
		events, err := ReadTrace(*replayPath)
		if err != nil {
			tui.Die(err)
		}
		if err := Replay(events, view, ReplayOpts{Speed: *speed, Step: *step}, os.Stdin, os.Stdout); err != nil {
			tui.Die(err)
		}
		return
	}

	// The provider is built if it can be — and it is NOT fatal if it cannot, as
	// long as there is a UI that can fix it.
	//
	// This is the one startup behaviour the interactive shell changed, and it is
	// the whole of the flash-and-close fix. A binary started from a file manager
	// has no environment: no AGENT_BASE_URL, no key, nothing `set -a && . ./.env`
	// would have put there. Every version of this program before the shell
	// printed one line to stderr and exited — and on Windows the console it was
	// given is destroyed a few microseconds later, so the message was correct
	// and unreadable, and the bug report is "it just flashes".
	//
	// So the failure is carried to the UI, which starts anyway, says what is
	// missing, and offers the commands that fix it. With no UI — piped stdin,
	// -p, --no-tui — it stays fatal, because there is nobody to fix it.
	shellMode := useShell(*noTUI, *printOnly)

	var provider Provider
	provErr := resolveErr
	if provErr == nil {
		p, err := pcfg.build(!*noCache)
		if err != nil {
			provErr = err
		} else {
			provider = p
		}
	}
	if provider == nil && !shellMode {
		tui.Die(provErr)
	}

	shell, err := findBash()
	if err != nil {
		// Fatal in every mode, including the shell, and one of very few that
		// are. No shell means the one tool this agent has does not exist, and no
		// slash command can install one.
		tui.Die(err)
	}
	cfg.shell = shell

	// The fold sink is named before the renderer, and the order is the contract:
	// Emit delivers to subscribers in order, so this one gets to say what class
	// of line the renderer is about to write before the renderer writes it. See
	// foldSink in shell.go.
	folds := &foldSink{}
	bus := NewBus(folds, view)

	// The trace sits behind a switch so /trace can move it mid-session. See
	// traceSink in shell.go for why the bus has no Unsubscribe instead.
	traces := &traceSink{}
	bus.Subscribe(traces)
	if *tracePath != "" {
		if err := traces.open(*tracePath); err != nil {
			tui.Die(err)
		}
	}
	defer traces.close()

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}

	wd, _ := os.Getwd()

	// The banner carries this under the shell; stderr is for the paths that
	// have no banner to carry it.
	if storeErr != nil && !shellMode {
		fmt.Fprintf(os.Stderr, "note: %v\nnote: the settings commands are off until that file is fixed or deleted\n", storeErr)
	}

	sh := &shellSession{
		storeErr: storeErr,
		pf:       pf, view: view, bus: bus, store: store, trace: traces, folds: folds,
		pname: pname, pcfg: pcfg, wd: wd,
		opts: shellOpts{
			provider: *providerName, cacheBP: !*noCache,
			window: *window, noMemory: *noMemory, noSkills: *noSkills,
			breakCache: *breakCache,
		},
	}

	// ---- the system prompt ----------------------------------------------
	//
	// Everything in it is stable for the whole session, which is what earns it a
	// place before the cache breakpoint; anything that moves goes into the
	// message stream instead — see memory.go's placement rule.
	//
	// It was assembled inline here, once, until the shell in tui/ arrived. It is a
	// function now for one reason: /open changes the working directory, and the
	// memory files and the skills index are read out of that directory. Moving
	// three of the four things that depend on the directory is worse than moving
	// none of them.
	sys, stable := sh.assemble(shell, wd)
	if *breakCache && !shellMode {
		fmt.Println("--break-cache: a fresh timestamp goes into the system prompt on every request")
	}

	comp := newCompactor(pcfg.Window, *compactAt, *keepAt)
	if *noCompact {
		comp.threshold = 0
	}
	if pcfg.Window <= 0 && !*noCompact && provider != nil && !shellMode {
		fmt.Println("note: this provider has no `window` configured, so compaction can never fire. Set it, or pass --window.")
	}

	a := &agent{
		p: provider, httpc: &http.Client{Timeout: 10 * time.Minute},
		g:   &gate{yolo: cfg.yolo, read: lineReader(stdin), available: interactive, out: os.Stdout},
		bus: bus, cfg: cfg, comp: comp, system: sys, memoryDir: wd,
		stable: stable, maxDepth: *maxDepth, out: os.Stdout,
	}
	sh.a = a

	// --subagent: one task, one report, no conversation.
	//
	// This is the whole of the out-of-process subagent mechanism, and it is
	// worth seeing how little there is. An agent that can run bash can run
	// `agent --subagent "..."`, which means recursion needs no `task` tool at
	// all — the shell is the orchestrator. docs/07 measures what that costs,
	// and the answer is: not tokens, but every number on the instrument panel.
	if *subagentAt != "" {
		child := a.newChild("cli", func() string { return subagentSystem + para + stable })
		msgs := child.runTurn([]Msg{TextMsg(RoleUser, *subagentAt)})
		fmt.Println()
		fmt.Println(lastAssistantText(msgs))
		return
	}

	// -p: one prompt, the panel, no UI, exit.
	//
	// The non-interactive contract written down as a flag rather than left to
	// depend on whether stdin happens to be a pipe. Everything the shell can do
	// this can do through flags; the one thing it cannot do is ask, so the gate
	// is closed explicitly and a command that needs approval is denied with a
	// reason — rather than hanging on a terminal nobody is watching.
	if *printOnly != "" {
		a.g.available = false
		bus.Emit(Event{Kind: KindUserMessage, Text: *printOnly})
		msgs := a.runTurn([]Msg{userTurn(*printOnly, volatileContext(shell, time.Now()))})
		fmt.Println()
		fmt.Println(lastAssistantText(msgs))
		view.SessionSummary(a.lastPrompt)
		return
	}

	if shellMode {
		if err := sh.run(context.Background()); err == nil {
			return
		} else {
			// The shell could not take the terminal. Not fatal: fall through to
			// the plain prompt, which is what --no-tui would have given anyway.
			// A tool that refuses to run because it could not draw a status bar
			// is worse than one that draws nothing.
			fmt.Fprintf(os.Stderr, "the interactive shell could not start (%v); using the plain prompt\n", err)
			if provider == nil {
				tui.Die(provErr)
			}
		}
	}

	fmt.Printf("stage 07 · provider=%s (%s) · model=%s\ncwd=%s\n",
		pname, pcfg.Protocol, pcfg.Model, wd)

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

// useShell decides whether to draw a UI.
//
// Four ways to end up with the plain prompt instead, and each is somebody's real
// situation: --no-tui for a reader who wants the loop the chapters describe, -p
// for a script, a piped stdin because `echo hi | agent` has worked since stage
// 00 and a full-screen UI would break every script that does it, and TERM=dumb
// because a terminal saying it cannot do this is telling the truth.
//
// Stdout is checked as well as stdin. Redirecting output to a file with the
// terminal still attached is how you get a log full of escape sequences and a
// screen that repaints over the top of your shell.
func useShell(noTUI bool, printOnly string) bool {
	if noTUI || printOnly != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	fo, err := os.Stdout.Stat()
	return err == nil && fo.Mode()&os.ModeCharDevice != 0
}

// command handles the slash commands. They exist for the experiments in
// docs/05-live-forever.md: compaction that only fires when the window is nearly
// full is hard to demonstrate and harder to test.
func (a *agent) command(line string, msgs []Msg) (bool, []Msg) {
	switch {
	case line == "/help":
		fmt.Fprintln(a.out, "  /compact          compact the conversation now")
		fmt.Fprintln(a.out, "  /remember <note>  append a line to "+memoryFileForWriting)
		fmt.Fprintln(a.out, "  /context          show what the conversation currently costs")
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
		fmt.Fprintf(a.out, "  %d messages · %d chars of history + %d chars of system/tools\n",
			len(msgs), convChars(msgs), base)
		fmt.Fprintf(a.out, "  estimated prompt: ~%d tokens at %.2f chars/token (%d calibration samples)\n",
			a.comp.estimate(msgs, base), a.comp.est.ratio, a.comp.est.obs)
		if a.lastPrompt > 0 {
			fmt.Fprintf(a.out, "  last call actually billed: %d prompt tokens\n", a.lastPrompt)
		}
		if problem := validConversation(msgs); problem != "" {
			fmt.Fprintf(a.out, "  MALFORMED: %s\n", problem)
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
	for _, t := range []Tool{bashToolDef(), taskToolDef()} {
		n += len(t.Name) + len(t.Description) + 200 // the schema, near enough
	}
	return n
}

// call performs one model call.
func (a *agent) call(turn int, msgs []Msg) (*CallResult, error) {
	req, body, err := a.p.BuildRequest(a.system(), msgs, a.tools(), 4096)
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
		a.spent = addUsage(a.spent, res.Usage)
		a.turnsUsed = turn
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

		// One line, where stage 06 had forty. dispatch() runs every tool call
		// in this turn — subagents concurrently, everything else in order — and
		// hands back the results in the order the model asked for them.
		blocks, stop := a.dispatch(turn, res.Calls)
		results := Msg{Role: RoleUser, Blocks: blocks}
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
