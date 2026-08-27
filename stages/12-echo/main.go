// Stage 12 — Echo: the cheapest tool call is the one you do not make.
//
// One idea, and it lives in echo.go: **if the model asks for a command we
// already ran, and nothing that command read has changed since, hand back the
// answer instead of starting a process.**
//
// The diff against stage 11 is one new file, one event kind, and a lookup at
// the top of runCommand. The flag that switches it on is off by default, which
// is unusual for this repo and is the honest conclusion of the chapter rather
// than caution: replayed against the sixteen sessions in this repo's own trace
// collection, the cache would have hit four times in 107 commands and saved
// 401 ms — against 864 s of model time in those same sessions.
//
// That is not a reason to skip the stage. It is the reason for --cache-audit,
// which replays the commands out of any trace through a cold cache and reports
// what it would have done, with no API key and nothing running. Deciding
// whether a cache is worth building is a measurement, and the measurement is
// cheaper than the cache.
//
// # Where the name comes from
//
// A repeated command, and where the repeats come from. In every recorded
// session here they follow either a compaction or a new user message: the model
// is not being redundant, it is recovering something it lost. Which is exactly
// why a hit cannot answer with "you ran this at turn 2" — the result it would
// point at is the thing that is no longer there.
//
// # What phase 1 and stages 09-11 still do
//
// Unchanged. A cache hit is a tool result like any other, so triage and the
// deadlines never see it, and it crosses stage 11's boundary before the lookup
// happens because the lookup is keyed on a command that has already been
// validated. The one visible change elsewhere is that a hit emits no
// command_start and no command_end, because no command started and none ended.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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
	maxTokens int
	maxTurns  int
	subTurns  int
	yolo      bool

	// wd is the directory commands run in. Stage 12 needs it because a cached
	// result is only the answer to a command *somewhere*, and because a witness
	// path from the command line is relative to it.
	//
	// It was already reachable through os.Getwd(). Recording it here instead
	// makes it a value the cache is given rather than one it goes and finds,
	// which is the difference between a key that is testable and a key that is
	// correct only when the test process happens to be in the right directory.
	wd string

	// env is what the shell will inherit, captured once. It is in the cache key
	// for the same reason wd is; see keyOf.
	env []string
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
	// lad replaced a plain `p Provider` in stage 09. A session no longer has
	// *a* provider; it has an ordered list and a current position, and the
	// position can move mid-session. Everything that used to read a.p now goes
	// through a.prov(), which is one lock away from the truth rather than a
	// field that was true at startup.
	lad   *ladder
	pol   retryPolicy
	dl    deadlines
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

	// Stage 11. Every tool call id this agent has ever put in its history.
	//
	// Session-scoped, not turn-scoped, and that is the whole point: a gateway
	// that reuses one id for every call it mints produces duplicates that live
	// in *different* assistant messages, so a per-turn check sees nothing while
	// the protocol rejects the request for `Found duplicate tool_use id`. See
	// uniqueIDs.
	//
	// Not shared with subagents. A child has its own message array, so its ids
	// only have to be unique within it, and sharing the map would mean two
	// concurrent children contending for it on every tool call.
	seenIDs map[string]bool

	// cutStreak counts consecutive turns in which every tool call was refused
	// as truncated. See maxCutStreak.
	cutStreak int

	// Stage 12. Nil when --cache is off, and every method on it tolerates a nil
	// receiver, so the disabled path is the same code path with one branch
	// rather than a second implementation of runCommand.
	//
	// Shared with children by pointer. See resultCache for why that is the
	// case worth having, and newChild for the rule that made it a decision
	// rather than an oversight.
	echo *resultCache

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

		// Stage 09. Three numbers, and the defaults are the argument: retrying
		// is on, because a single 429 losing a turn is worse than a two-second
		// pause, and it is bounded, because the only thing worse than a lost
		// turn is a turn that silently spends four minutes and six prompts
		// getting nowhere.
		fallbackTo  = flag.String("fallback", "", "provider names to fall back to, in order, comma-separated")
		retries     = flag.Int("retry", 3, "attempts per provider on a retryable failure; 1 disables retrying")
		retryBudget = flag.Duration("retry-budget", 30*time.Second, "total time one call may spend waiting between attempts")

		// Stage 10. Three clocks where stage 09 had one, and they are separate
		// flags because they answer different questions — see deadline.go. Any
		// of them may be 0, which switches that clock off; the wire probing in
		// docs/wire-notes.md needs all three off, because a probe that gets cut
		// short is not evidence.
		connectFor = flag.Duration("connect-timeout", 30*time.Second, "response headers must arrive within this")
		idleFor    = flag.Duration("stall-timeout", 45*time.Second, "longest tolerated gap between bytes of a stream")
		callFor    = flag.Duration("call-timeout", 15*time.Minute, "backstop on one whole model call, retries excluded")

		// Stage 12. Off by default, and the chapter is mostly an argument about
		// why: on sixteen recorded sessions this cache would have hit four
		// times in 107 commands and saved 0.4 s of 864 s. A feature that pays
		// that badly should be something you switch on when you know your
		// workload, not something a reader inherits without being told.
		useCache    = flag.Bool("cache", false, "serve a repeated read-only command from a result cache instead of running it")
		cacheMax    = flag.Int("cache-entries", 256, "result cache: maximum entries")
		cacheBytes  = flag.Int("cache-bytes", 8<<20, "result cache: maximum total bytes of stored output")
		cacheTTL    = flag.Duration("cache-ttl", 0, "result cache: expire entries this old; 0 means never, and staleness is decided by content")
		cacheAudit_ = flag.Bool("cache-audit", false, "replay the commands in the given traces through a cold cache and report what it would have done")

		// The interactive shell, in tui/. Not part of the course; see shell.go.
		printOnly  = flag.String("p", "", "run one prompt without a UI, print the reply, and exit")
		noTUI      = flag.Bool("no-tui", false, "use the plain line prompt instead of the interactive shell")
		settingsAt = flag.String("settings", "", "path to the saved settings file; empty means the one under your user config directory")
	)
	cfg := config{}
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "kill a command after this long")
	// Stage 11's experiment knob, in the spirit of stage 04's --break-cache: a
	// truncated tool call is not something you can wait around for, and the only
	// way to see one on demand is to make the budget too small on purpose. 4096
	// is the value every earlier stage hard-coded.
	flag.IntVar(&cfg.maxTokens, "max-tokens", 4096, "output token budget per call; lower it to force a truncated tool call (stage 11)")
	flag.IntVar(&cfg.maxOutput, "max-output", 8000, "bytes of command output the model may see")
	flag.IntVar(&cfg.maxTurns, "max-turns", 25, "tool-call rounds per user message")
	flag.IntVar(&cfg.subTurns, "sub-turns", 15, "tool-call rounds a subagent gets")
	flag.BoolVar(&cfg.yolo, "yolo", false, "run every command without asking")
	flag.Parse()

	// Same argument as the composer below, and a stronger one: --cache-audit
	// exists to be run before you decide whether to switch the cache on, which
	// is a decision you should be able to make on a laptop with no key in it.
	if *cacheAudit_ {
		wd, _ := os.Getwd()
		if err := cacheAudit(flag.Args(), wd, os.Stdout); err != nil {
			tui.Die(err)
		}
		return
	}

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

	var lad *ladder
	provErr := resolveErr
	if provErr == nil {
		provider, err := pcfg.build(!*noCache)
		if err != nil {
			provErr = err
		} else if l, err := buildLadder(pf, pname, pcfg, provider, *fallbackTo, !*noCache); err != nil {
			provErr = err
		} else {
			lad = l
		}
	}
	if lad == nil && !shellMode {
		tui.Die(provErr)
	}

	pol := retryPolicy{attempts: *retries, base: 500 * time.Millisecond, max: 8 * time.Second, budget: *retryBudget}
	shell, err := findBash()
	if err != nil {
		// Fatal in every mode, including the shell, and one of very few that
		// are. No shell means the one tool this agent has does not exist, and no
		// slash command can install one.
		tui.Die(err)
	}
	cfg.shell = shell

	bus := NewBus(view)

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

	// The first fact in the trace: who serves this session, and at what price.
	//
	// Until stage 09 a trace recorded no provider at all. You could read every
	// byte of a session - every request body, every token count - and not be
	// able to say which endpoint produced it, which made the cost figures in an
	// archived trace unreconstructable. Emitted after the trace writer
	// subscribes so the file gets it too, and it is the same event a fallback
	// emits later: one kind, one meaning, "this is who is answering now".
	if lad != nil {
		_, _, pinfo := lad.pos()
		bus.Emit(Event{Kind: KindProvider, Provider: &pinfo})
	}

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}

	wd, _ := os.Getwd()
	cfg.wd, cfg.env = wd, os.Environ()

	// The banner carries this under the shell; stderr is for the paths that have
	// no banner to carry it.
	if storeErr != nil && !shellMode {
		fmt.Fprintf(os.Stderr, "note: %v\nnote: the settings commands are off until that file is fixed or deleted\n", storeErr)
	}

	sh := &shellSession{
		pf: pf, view: view, bus: bus, store: store, trace: traces,
		pname: pname, pcfg: pcfg, storeErr: storeErr,
		opts: shellOpts{
			provider: *providerName, fallback: *fallbackTo, cacheBP: !*noCache,
			window: *window, noMemory: *noMemory, noSkills: *noSkills,
			breakCache:   *breakCache,
			cacheEntries: *cacheMax, cacheBytes: *cacheBytes, cacheTTL: *cacheTTL,
		},
	}

	// ---- the system prompt ----------------------------------------------
	//
	// Everything in it is stable for the whole session, which is what earns it a
	// place before the cache breakpoint; anything that moves goes into the
	// message stream instead — see memory.go's placement rule.
	//
	// It was assembled inline here, once, until stage 12's shell. It is a
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
	if pcfg.Window <= 0 && !*noCompact && lad != nil && !shellMode {
		fmt.Println("note: this provider has no `window` configured, so compaction can never fire. Set it, or pass --window.")
	}

	dl := deadlines{connect: *connectFor, idle: *idleFor, total: *callFor}

	// The client has no Timeout of its own any more, and that is the change.
	//
	// http.Client.Timeout covers the body read, so on a streamed response it is
	// a cap on how long the model may talk — which is not a thing anyone wants
	// to cap. The connect half of it moves to ResponseHeaderTimeout, which stops
	// at the headers and leaves the stream alone; the rest is the context's job.
	httpc := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: dl.connect,
		},
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	// One context for the whole session, and Ctrl-C is what ends it.
	//
	// signal.NotifyContext would be shorter and would cancel with
	// context.Canceled, which triage cannot tell apart from a stall. The cause
	// is the whole point, so the handler is written out.
	//
	// The shell does not install it, and that is not an oversight. Raw mode
	// turns off ISIG, so under the shell Ctrl-C arrives as byte 0x03 on stdin
	// rather than as a signal — a handler here would be a second reader of the
	// same keypress with a different meaning, racing the first.
	if !shellMode {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, os.Interrupt)
		go func() {
			<-sigc
			cancel(errInterrupted)
			// A second Ctrl-C is not more of the first. The first one asks the
			// agent to stop, which means unwinding: killing commands, closing
			// the trace, printing the bill. If that unwind is itself stuck, the
			// user needs a way out that does not depend on the code that is
			// stuck.
			signal.Stop(sigc)
		}()
	}

	a := &agent{
		lad: lad, pol: pol, dl: dl, httpc: httpc,
		g:   &gate{yolo: cfg.yolo, read: lineReader(stdin), available: interactive, out: os.Stdout},
		bus: bus, cfg: cfg, comp: comp, system: sys, memoryDir: wd,
		stable: stable, maxDepth: *maxDepth, out: os.Stdout,
		seenIDs: map[string]bool{},
	}
	if *useCache {
		a.echo = newResultCache(*cacheMax, *cacheBytes, *cacheTTL)
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
		msgs := child.runTurn(ctx, []Msg{TextMsg(RoleUser, *subagentAt)})
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
		msgs := a.runTurn(ctx, []Msg{userTurn(*printOnly, volatileContext(shell, time.Now()))})
		fmt.Println()
		fmt.Println(lastAssistantText(msgs))
		view.SessionSummary(a.lastPrompt)
		return
	}

	if shellMode {
		if err := sh.run(ctx); err == nil {
			return
		} else {
			// The shell could not take the terminal. Not fatal: fall through to
			// the plain prompt, which is what --no-tui would have given anyway.
			// A tool that refuses to run because it could not draw a status bar
			// is worse than one that draws nothing.
			fmt.Fprintf(os.Stderr, "the interactive shell could not start (%v); using the plain prompt\n", err)
			if lad == nil {
				tui.Die(provErr)
			}
		}
	}

	fmt.Printf("stage 12 · provider=%s (%s) · model=%s\ncwd=%s\n",
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
		if handled, next := a.command(ctx, line, msgs); handled {
			msgs = next
			continue
		}

		bus.Emit(Event{Kind: KindUserMessage, Text: line})
		// The volatile snapshot is taken HERE, once, and frozen into the
		// message. It is never recomputed, which is the entire reason the cache
		// survives a session that knows what time it is.
		msgs = append(msgs, userTurn(line, volatileContext(shell, time.Now())))
		msgs = a.runTurn(ctx, msgs)
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
func (a *agent) command(ctx context.Context, line string, msgs []Msg) (bool, []Msg) {
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
		out, err := a.comp.run(ctx, a.prov(), a.pol, a.httpc, a.bus, msgs, cut, base, a.dl)
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

// prov is the provider serving calls right now. See agent.lad.
func (a *agent) prov() Provider {
	_, p, _ := a.lad.pos()
	return p
}

// callWithRetry is one model call plus the decisions in triage.go.
//
// The body of what used to be call() moved to modelCall() in triage.go, where
// the compactor's copy of it moved too. What is left here is the agent naming
// what it wants retried, and it is worth noticing how little that is: the loop,
// the policy, the classifier and the ladder are all testable without an HTTP
// server, and the only thing that needs the agent is the closure.
func (a *agent) callWithRetry(ctx context.Context, turn int, msgs []Msg) (*CallResult, error) {
	return retryLoop(ctx, a.bus, turn, a.pol, a.lad, nil, nil,
		func(ctx context.Context, p Provider) (*CallResult, error) {
			return modelCall(ctx, p, a.httpc, a.bus, turn, a.system(), msgs, a.tools(), a.cfg.maxTokens, a.dl, nil)
		})
}

func (a *agent) runTurn(ctx context.Context, msgs []Msg) []Msg {
	for turn := 1; ; turn++ {
		if turn > a.cfg.maxTurns {
			a.bus.Notice("stopped: hit the %d-turn limit", a.cfg.maxTurns)
			return msgs
		}

		// The user asked to stop, so stop — before starting anything else.
		//
		// This check was missing until the interactive shell made it visible,
		// and the reason it was invisible is worth more than the fix. Before the
		// shell, the only way to interrupt was Ctrl-C, which cancelled the
		// session context and ended the process; a doomed extra model call on
		// the way out changed nothing anyone would see. Under the shell the
		// session survives an interrupt, and what the user saw after pressing
		// Escape on a running command was the command being killed correctly and
		// then two red lines about a failed HTTP POST — a report of this loop
		// dialling the model again after being told not to.
		if ctx.Err() != nil {
			a.bus.Notice("stopped: %v", context.Cause(ctx))
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
			} else if out, err := a.comp.run(ctx, a.prov(), a.pol, a.httpc, a.bus, msgs, cut, base, a.dl); err != nil {
				a.bus.Error("compaction failed: %v — continuing uncompacted", err)
			} else {
				msgs = out
			}
		}

		a.bus.Emit(Event{Kind: KindTurnStart, Turn: turn})

		sentChars := convChars(msgs) + base
		res, err := a.callWithRetry(ctx, turn, msgs)
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

		// Stage 11 — two repairs, both BEFORE the message is built, because
		// once it is appended it is in the prompt for the rest of the session
		// (§E14).
		//
		// First: the ids. Renaming has to happen here rather than in dispatch,
		// because dispatch builds the result blocks that name these ids, and a
		// call renamed after its answer exists is an orphaned result — the same
		// rejected request with a less helpful message.
		if n := uniqueIDs(res.Calls, a.seenIDs); n > 0 {
			a.bus.Notice("%d tool call id(s) in this turn collided with earlier ones and were renamed", n)
		}

		// Second: the markup leak, §A2 — and this one comes with an honest
		// caveat about when it can fire.
		//
		// The model does not emit JSON on the wire. It emits an XML-ish harness
		// syntax, `<tool_call><function=bash><parameter=command>…`, which the
		// gateway parses server-side into tool calls. Truncate it mid-syntax and
		// the parse fails, and §A2 records the fallback: the raw markup lands in
		// `message.content`. Keeping that costs twice — the human is shown
		// gateway internals as if the assistant said them, and the history
		// teaches the model that emitting this syntax as prose is normal here.
		//
		// The caveat is §E15, which was measured for stage 11 and which corrects
		// §A2: that fallback exists only when NOT streaming. Streamed, the parse
		// runs incrementally and forwards what it managed, so the client gets
		// partial argument JSON and no markup at all. This agent always streams.
		// So on this endpoint the branch below cannot fire, and it is kept for
		// two reasons that are about other endpoints rather than this one: a
		// non-streaming path would meet §A2's shape immediately, and a model
		// confused enough to write tool-call syntax as ordinary prose produces
		// the same bytes on any provider.
		//
		// Gated on StopMaxTokens rather than run on every turn, and the gate
		// costs something real: markup that leaks WITHOUT truncation is left in
		// place. That is the deliberate trade. On a truncated turn the text is
		// incomplete by definition, so cutting it loses nothing the model
		// finished saying; on a complete turn, an agent asked to explain this
		// very wire format would otherwise have its answer silently truncated at
		// the first `<tool_call>` it quoted — and this repo's own documentation
		// is full of them.
		text := res.Text
		if res.Stop == StopMaxTokens {
			if stripped, found := stripHarnessMarkup(text); found {
				a.bus.Emit(Event{
					Kind: KindToolCallInvalid, Turn: turn,
					Fault: string(faultNotJSON),
					Text:  "the gateway's own tool-call markup arrived as assistant text",
				})
				text = stripped
			}
		}

		am := Msg{Role: RoleAssistant}
		if text != "" {
			am.Blocks = append(am.Blocks, Block{Kind: BlockText, Text: text})
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
					// No "retry with a shorter command" here any more. This
					// string is replayed on every subsequent request, and an
					// imperative in the history reads as a fresh instruction
					// once its context has scrolled away — see faultText.
					return "[not executed: the reply was cut off at max_tokens]"
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
		blocks, out := a.dispatch(ctx, turn, res.Calls)
		results := Msg{Role: RoleUser, Blocks: blocks}
		msgs = append(msgs, results)
		if out.stop {
			return msgs
		}

		// The cut fuse. A turn where EVERY call was truncated advances the
		// streak; one that got anything through resets it.
		//
		// This exists because refusing correctly turned out not to be enough. A
		// live session at --max-tokens 110 made sixteen model calls, ran zero
		// commands, and was stopped only by the turn budget: each turn the model
		// was told its call had been cut off, and each turn it wrote another
		// command of the same length. It is not being stubborn — it cannot see
		// max_tokens, so "you were cut off" names a cause it has no way to act
		// on, and rewording is the only move it has.
		//
		// So the message goes to the human instead, who can change the number.
		// Three, because two in a row is plausibly the model shortening its
		// command and getting unlucky, and three is a pattern.
		if out.calls > 0 && out.cut == out.calls {
			a.cutStreak++
		} else {
			a.cutStreak = 0
		}
		if a.cutStreak >= maxCutStreak {
			a.bus.Error("%d turns in a row produced only truncated tool calls. The model cannot see the "+
				"output budget, so it will keep re-sending calls of the same length; raise --max-tokens "+
				"(currently %d)", a.cutStreak, a.cfg.maxTokens)
			return msgs
		}
	}
}

// maxCutStreak is how many consecutive all-truncated turns end the loop.
//
// A fuse, in the same family as maxTurns and maxDepth: not a fix, a bound on how
// much a known-unfixable loop is allowed to cost. maxTurns would eventually stop
// this too, at 25 calls instead of 3.
const maxCutStreak = 3

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
