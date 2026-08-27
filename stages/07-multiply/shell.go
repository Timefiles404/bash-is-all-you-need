// Stage 07 — wiring the interactive shell.
//
// This file is not part of the lesson, and neither is the tui/ package it
// hands things to. No chapter explains either. They exist because reading a
// stage and *working in* one want different programs: the chapters want a UI
// small enough to hold in your head, and a reader poking at subagents or skills
// wants a window that does not close when a value is missing, a key that stops a
// runaway turn, and a way to fix the endpoint without editing a file.
//
// So the shell lives in tui/ and this file is the seam. What is worth noticing
// about the seam is how narrow it is: the renderer written in stage 02 is
// pointed at a different io.Writer, the permission gate written in stage 01
// reads its answer from a function instead of a Scanner, and nothing else in the
// agent knows a UI exists. That is not luck. It is what stage 02 bought when it
// put an event bus between the agent and everything that watches it, and the
// return is a whole front end arriving five stages later without needing one
// line changed in that bus.
//
// Everything the shell can change while a session runs is a field on the struct
// below rather than a local in main(), which is the one structural cost. A value
// computed once in main() and captured by a closure is a value /open cannot
// change, and a working directory the agent believes in but does not use is a
// worse bug than not having /open at all.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bash-is-all-you-need/tui"
	"bash-is-all-you-need/tui/settings"
)

// shellOpts are the command-line values the shell needs in order to rebuild
// something it has changed. They are copied rather than referenced so that a
// flag's value and the session's current value cannot drift apart silently.
type shellOpts struct {
	provider   string
	cacheBP    bool // cache_control breakpoints; the inverse of --no-cache
	window     int
	noMemory   bool
	noSkills   bool
	breakCache bool
}

// shellSession is one interactive session.
type shellSession struct {
	pf    *providersFile
	view  *renderer
	bus   *Bus
	app   *tui.App
	store *settings.Store
	trace *traceSink
	opts  shellOpts

	// storeErr is why there is no settings store, when there is none. It goes in
	// the banner rather than to a log, because the commands it disables are the
	// ones a user with no environment has to reach for first.
	storeErr error

	// mu guards everything below. The shell runs one turn or one command at a
	// time, so the contention is not between two turns — it is between a turn
	// and the status bar, which is repainted thirty times a second on another
	// goroutine while the turn appends to msgs.
	mu    sync.Mutex
	a     *agent
	pname string
	pcfg  providerConfig
	msgs  []Msg

	// wd is where commands run, and it lives here because /open moves it.
	//
	// config has no working-directory field: nothing in this stage sets
	// cmd.Dir, so the process's own directory is what decides where a command
	// runs, and os.Chdir is what changes it. This field is the copy the banner
	// and /status report, and open() is the only writer.
	wd string
}

// ---------------------------------------------------------------------------
// The trace, switchable
// ---------------------------------------------------------------------------

// traceSink is one permanent bus subscriber whose file can be changed.
//
// The bus has no Unsubscribe and should not get one: a subscriber list that can
// shrink is a list where the trace can be removed halfway through a session and
// the file then describes a session that did not happen. So /trace moves the
// file behind a subscriber that never leaves.
type traceSink struct {
	mu sync.Mutex
	w  *TraceWriter
}

func (t *traceSink) OnEvent(e Event) {
	t.mu.Lock()
	w := t.w
	t.mu.Unlock()
	if w != nil {
		w.OnEvent(e)
	}
}

func (t *traceSink) open(path string) error {
	w, err := NewTraceWriter(path)
	if err != nil {
		return err
	}
	t.mu.Lock()
	old := t.w
	t.w = w
	t.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

func (t *traceSink) close() {
	t.mu.Lock()
	w := t.w
	t.w = nil
	t.mu.Unlock()
	if w != nil {
		w.Close()
	}
}

func (t *traceSink) path() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w == nil {
		return ""
	}
	return t.w.Path()
}

// ---------------------------------------------------------------------------
// Running
// ---------------------------------------------------------------------------

func (s *shellSession) run(ctx context.Context) error {
	s.app = tui.New(tui.Config{
		Title:       "stage 07",
		Banner:      s.banner(),
		Submit:      s.submit,
		Commands:    s.commands(),
		Status:      s.status,
		Segments:    s.segments,
		Ready:       s.ready,
		Open:        s.open,
		Reconfigure: s.reconfigure,
		Settings:    s.store,
		Env:         tui.EnvNames{BaseURL: "AGENT_BASE_URL", APIKey: "AGENT_API_KEY", Protocol: "AGENT_PROTOCOL", Model: "AGENT_MODEL"},
		// Escape is not offered here, and that is a fact about the stage rather
		// than a decision about the UI: runTurn takes no context until stage 10,
		// so there is nothing for a cancellation to reach. A hint row promising a
		// key that does nothing is worse than one that never mentions it.
		Uninterruptible: "cancellation is stage 10's idea, so a turn here runs to the end once it has started",
		OnExit:          s.onExit,
	})
	// The renderer moves from stdout to the shell's pane, and that is the whole
	// output change. Anything still writing to stdout would land on the
	// alternate screen underneath the frame and corrupt it, which is why
	// agent.out exists and why command() writes there.
	s.view.out = s.app.Out()
	s.mu.Lock()
	s.a.out = s.app.Out()
	s.a.g.out = s.app.Out()
	s.a.g.read = s.gateAsk
	s.mu.Unlock()
	return s.app.Run(ctx)
}

// gateAsk is the permission gate's line reader, replacing the Scanner it used
// from stage 01 until now.
//
// The gate cannot read stdin any more: the shell owns it, in raw mode, and a
// Scanner on the same descriptor would steal keystrokes from the composer. The
// question goes to the shell instead, and the shell's own reply is what comes
// back. A false here means the shell is shutting down, which the gate already
// handles — it is the same case as a closed stdin, and it denies.
func (s *shellSession) gateAsk() (string, bool) {
	return s.app.Ask("[y/n/a/q] ")
}

func (s *shellSession) onExit(w io.Writer) {
	s.trace.close()
	s.mu.Lock()
	last := s.a.lastPrompt
	s.mu.Unlock()
	// The summary goes to the real screen, so the renderer is pointed back at
	// it for one call. Leaving it aimed at the pane would print the bill onto a
	// buffer nobody will read again.
	s.view.out = w
	s.view.SessionSummary(last)
}

func (s *shellSession) banner() []string {
	s.mu.Lock()
	a, pname, pcfg, wd := s.a, s.pname, s.pcfg, s.wd
	s.mu.Unlock()

	out := []string{
		"  stage 07 · multiply — subagents by recursion, skills, and what PTC really is",
		"",
		fmt.Sprintf("  cwd       %s", wd),
		fmt.Sprintf("  shell     %s", a.cfg.shell),
	}
	if ok, _ := s.ready(); ok {
		out = append(out,
			fmt.Sprintf("  provider  %s (%s)", pname, pcfg.Protocol),
			fmt.Sprintf("  model     %s", pcfg.Model))
	} else {
		out = append(out,
			"",
			"  No provider is configured, so nothing can be sent yet. Two ways out:",
			"",
			"    /provider-url https://your-endpoint/v1",
			"    /provider-protocol openai",
			"    /provider-model your-model-id",
			"    /provider-apikey <key>",
			"",
			"  or quit, run `set -a && . ./.env && set +a`, and start again. The",
			"  commands above save to a file outside this repo; the .env route does not.")
	}

	if s.storeErr != nil {
		out = append(out, "",
			"  "+s.storeErr.Error(),
			"  The settings commands are off until that file is fixed or deleted.",
			"  Nothing was written to it: a file that cannot be read is not a file",
			"  to overwrite.")
	}

	// Running the agent in the repo root is how you get a model rewriting the
	// course while you are reading it. AGENTS.md says use sandbox/, and a
	// double-clicked binary starts wherever the .exe happens to be — which for
	// anyone who built with `go build -o agent .` is exactly the wrong place.
	if isRepoRoot(wd) {
		out = append(out, "",
			"  This is the repo root, and the agent runs what the model says.",
			"  Use /open sandbox — or any scratch directory — before asking for anything.")
	}
	return append(out, "", "  /help lists the commands · /keys the keyboard · /status everything else", "")
}

// isRepoRoot is a heuristic and is allowed to be one: the cost of a false
// positive is a line of advice nobody needed.
func isRepoRoot(dir string) bool {
	for _, name := range []string{"AGENTS.md", "go.mod", "docs"} {
		if _, err := os.Stat(dir + string(os.PathSeparator) + name); err != nil {
			return false
		}
	}
	return true
}

// ready reports whether a prompt can be sent at all.
//
// This is the whole reason a missing provider is no longer fatal at startup. A
// binary that exits before drawing anything is a binary that, when started from
// a file manager, shows a window for a few microseconds and then nothing — and
// the user has no way to find out why, let alone fix it.
func (s *shellSession) ready() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.a.p == nil {
		return false, "no provider is configured yet — /provider-url, /provider-protocol, /provider-model and /provider-apikey set one up, and /status shows what is missing"
	}
	return true, ""
}

// submit runs one user turn.
//
// The context is accepted and not used, because runTurn takes none until stage
// 10. Ctrl-C therefore stops the shell from starting another turn rather than
// stopping the turn that is running, and the status bar says "interrupting…"
// until the turn in flight finishes on its own.
func (s *shellSession) submit(_ context.Context, line string) error {
	s.mu.Lock()
	a := s.a
	s.msgs = append(s.msgs, userTurn(line, volatileContext(a.cfg.shell, time.Now())))
	msgs := s.msgs
	s.mu.Unlock()

	s.bus.Emit(Event{Kind: KindUserMessage, Text: line})
	out := a.runTurn(msgs)

	s.mu.Lock()
	s.msgs = out
	s.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// The status bar
// ---------------------------------------------------------------------------

func (s *shellSession) segments() []string {
	s.mu.Lock()
	a, pname, pcfg, wd := s.a, s.pname, s.pcfg, s.wd
	msgs := len(s.msgs)
	s.mu.Unlock()

	who := "no provider"
	if a.p != nil {
		who = pname + " (" + pcfg.Protocol + ")"
	}
	out := []string{who, shortModel(pcfg.Model), shortDir(wd)}
	if n := s.view.session.Prompt() + s.view.session.Output; n > 0 {
		out = append(out, fmt.Sprintf("%s tok", thousands(n)))
	}
	if s.view.prices.known() {
		out = append(out, fmt.Sprintf("$%.4f", s.view.sessionCost))
	}
	// The message count is worth a field only once there are messages. On a bar
	// that has to drop fields to fit, "0 msg" is one that answers nothing.
	if msgs > 0 {
		out = append(out, fmt.Sprintf("%d msg", msgs))
	}
	if p := s.trace.path(); p != "" {
		out = append(out, "rec")
	}
	if a.cfg.yolo {
		// Worth a place on a bar that has to drop fields to fit: it is the one
		// setting whose consequence is a command running without being asked
		// about.
		out = append(out, "yolo")
	}
	return out
}

func shortModel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 && i+1 < len(m) {
		return m[i+1:]
	}
	return m
}

func shortDir(d string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(d, home) {
		return "~" + d[len(home):]
	}
	return d
}

func thousands(n int) string {
	switch {
	case n < 10_000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	}
}

// ---------------------------------------------------------------------------
// /status
// ---------------------------------------------------------------------------

func (s *shellSession) status() []tui.Section {
	s.mu.Lock()
	a, pname, pcfg, wd := s.a, s.pname, s.pcfg, s.wd
	s.mu.Unlock()
	v := s.view

	prov := []Row{
		{Name: "name", Value: pname},
		{Name: "protocol", Value: pcfg.Protocol},
		{Name: "model", Value: pcfg.Model},
		{Name: "base url", Value: pcfg.BaseURL},
		{Name: "api key", Value: settings.Redact(pcfg.APIKeyEnv, os.Getenv(pcfg.APIKeyEnv)), Note: "from " + pcfg.APIKeyEnv},
		{Name: "window", Value: tokensOrUnknown(pcfg.Window)},
	}
	if a.p == nil {
		prov = append(prov, Row{Name: "state", Value: "not built", Note: "nothing can be sent"})
	}

	spend := []Row{
		{Name: "calls", Value: strconv.Itoa(v.calls)},
		{Name: "commands", Value: strconv.Itoa(v.commands)},
		{Name: "tokens in", Value: fmt.Sprintf("%d + %d cache-write + %d cache-read",
			v.session.Input, v.session.CacheWrite, v.session.CacheRead)},
		{Name: "tokens out", Value: strconv.Itoa(v.session.Output)},
		{Name: "cost", Value: costOrUnknown(v.prices.known(), v.sessionCost)},
	}

	work := []Row{
		{Name: "directory", Value: wd},
		{Name: "shell", Value: a.cfg.shell},
		{Name: "trace", Value: orNone(s.trace.path())},
		{Name: "gate", Value: gateMode(a.g)},
		{Name: "memory", Value: yesOrNo(!s.opts.noMemory), Note: "AGENTS.md and MEMORY.md, read at startup and on /open"},
		{Name: "skills", Value: yesOrNo(!s.opts.noSkills)},
	}

	return []tui.Section{
		{Title: "provider", Rows: prov},
		{Title: "conversation", Rows: s.conversationRows()},
		{Title: "session", Rows: spend},
		{Title: "workspace", Rows: work},
		{Title: "limits", Rows: s.limitRows()},
	}
}

// conversationRows is what /context prints and what /status shows in the middle,
// and it is one function because two copies of it would drift — the numbers here
// are exactly the ones people compare between the two.
func (s *shellSession) conversationRows() []Row {
	s.mu.Lock()
	a, msgs := s.a, s.msgs
	s.mu.Unlock()

	base := len(a.system()) + toolChars()
	out := []Row{
		{Name: "messages", Value: strconv.Itoa(len(msgs))},
		{Name: "history", Value: fmt.Sprintf("%d chars", convChars(msgs))},
		{Name: "system + tools", Value: fmt.Sprintf("%d chars", base)},
		{Name: "estimated prompt", Value: fmt.Sprintf("~%d tok", a.comp.estimate(msgs, base)),
			Note: fmt.Sprintf("at %.2f chars/token from %d samples", a.comp.est.ratio, a.comp.est.obs)},
		{Name: "last billed", Value: tokensOrUnknown(a.lastPrompt)},
		{Name: "compactions", Value: strconv.Itoa(s.view.compactions)},
	}
	if problem := validConversation(msgs); problem != "" {
		// A malformed conversation is rejected by the next request, not by the
		// one that created it, so a report like this is the only place it can be
		// seen before the failure.
		out = append(out, Row{Name: "MALFORMED", Value: problem})
	}
	return out
}

func (s *shellSession) limitRows() []Row {
	var out []Row
	for _, k := range s.knobs() {
		out = append(out, Row{Name: k.name, Value: k.get(), Note: k.help})
	}
	return out
}

func tokensOrUnknown(n int) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d tok", n)
}

func costOrUnknown(known bool, v float64) string {
	if !known {
		// A made-up zero is worse than no number, because it is the number
		// people quote. Same rule as the panel's.
		return "unknown — no prices configured"
	}
	return fmt.Sprintf("$%.4f", v)
}

func orNone(s string) string {
	if s == "" {
		return "not recording"
	}
	return s
}

func yesOrNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func onOrOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func gateMode(g *gate) string {
	switch {
	case g.yolo:
		return "off — every command runs unasked (--yolo)"
	case g.always:
		return "off for this session — you answered 'a'"
	default:
		return "asking"
	}
}

// ---------------------------------------------------------------------------
// Rebuilding
// ---------------------------------------------------------------------------

// open points the session at another directory.
//
// Four things depend on the working directory and all four have to move
// together: where commands run, where memory is read and written, where skills
// are indexed, and what the system prompt says the directory is. Moving three of
// them is worse than moving none — an agent told it is in one directory while
// its commands run in another produces confident, wrong answers about a tree it
// cannot see.
func (s *shellSession) open(dir string) (string, error) {
	if err := os.Chdir(dir); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.a
	s.wd = dir
	a.memoryDir = dir
	sys, stable := s.assemble(a.cfg.shell, dir)
	a.system, a.stable = sys, stable

	// The conversation is deliberately kept. The history describes work that
	// was done, and deleting it because the directory changed would throw away
	// the reason the user changed directory. /new is the command for the other
	// intention, and having both is the point.
	msg := "now working in " + dir
	if len(s.msgs) > 0 {
		msg += fmt.Sprintf(" · %d messages kept, and they still refer to the old directory — /new starts over", len(s.msgs))
	}
	return msg, nil
}

// assemble builds the system prompt. Caller holds mu.
//
// It was a straight line in main() until the shell needed to run it twice.
// Everything it reads is a file on disk, which is exactly why it cannot be
// computed once: /open changes which files those are.
func (s *shellSession) assemble(shell, wd string) (func() string, string) {
	memory := ""
	if !s.opts.noMemory {
		memory, _ = loadMemory(wd, s.bus)
	}
	var skills []skill
	if !s.opts.noSkills {
		skills = loadSkills(wd)
	}
	if len(skills) > 0 {
		idx, bodies := skillsCost(skills)
		s.bus.Emit(Event{Kind: KindSkillsIndexed, Bytes: idx, TokensBefore: bodies,
			Text: fmt.Sprintf("%d skills", len(skills))})
	}
	stable := stableContext(shell, wd) + memoryPrompt
	if memory != "" {
		stable += para + memory
	}
	stable += skillsPrompt(skills)
	full := basePrompt + para + stable
	if s.opts.breakCache {
		return func() string {
			return "Current time: " + time.Now().Format(time.RFC3339Nano) + "\n\n" + full
		}, stable
	}
	return func() string { return full }, stable
}

// reconfigure rebuilds the provider after a setting changed.
//
// It is allowed to fail and say so without undoing anything. The value that was
// just typed is already saved, and a command that reverted a saved setting
// because the endpoint did not answer would make the two-step case — set the
// URL, then the key — impossible.
func (s *shellSession) reconfigure() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rebuildProvider()
}

// rebuildProvider re-resolves the provider from the current environment.
// Caller holds mu.
func (s *shellSession) rebuildProvider() (string, error) {
	pcfg, pname, err := s.pf.resolve(s.opts.provider)
	if err != nil {
		return "", err
	}
	if s.opts.window > 0 {
		pcfg.Window = s.opts.window
	}
	p, err := pcfg.build(s.opts.cacheBP)
	if err != nil {
		return "", err
	}
	s.pcfg, s.pname, s.a.p = pcfg, pname, p

	// Prices and the window belong to the provider, so they move with it. A
	// panel still pricing the previous endpoint is a bill that is wrong in a way
	// nothing on screen admits.
	s.view.prices = prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
		cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite}
	s.view.window = pcfg.Window
	s.a.comp.window = pcfg.Window

	out := fmt.Sprintf("provider %s (%s) · model %s", pname, pcfg.Protocol, pcfg.Model)
	if pcfg.Window <= 0 {
		out += " · no window configured, so compaction can never fire"
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

func (s *shellSession) commands() []tui.Command {
	return []tui.Command{
		{
			Name: "/compact", Group: "session",
			Help: "summarise the conversation now, without waiting for the watermark",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				s.mu.Lock()
				a, msgs := s.a, s.msgs
				s.mu.Unlock()
				base := len(a.system()) + toolChars()
				cut, why := a.comp.plan(msgs, base)
				if cut < 0 {
					return fmt.Errorf("%s", why)
				}
				out, err := a.comp.run(a.p, a.httpc, a.bus, msgs, cut, base)
				if err != nil {
					return fmt.Errorf("compaction failed: %w — the conversation is unchanged", err)
				}
				s.mu.Lock()
				s.msgs = out
				s.mu.Unlock()
				return nil
			},
		},
		{
			Name: "/context", Group: "session",
			Help: "what the conversation currently costs",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				for _, l := range tui.RenderRows(s.conversationRows(), s.app.Width()) {
					fmt.Fprintln(w, l)
				}
				return nil
			},
		},
		{
			Name: "/new", Group: "session",
			Help: "forget the conversation and start over; settings are kept",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				s.mu.Lock()
				n := len(s.msgs)
				s.msgs = nil
				s.mu.Unlock()
				fmt.Fprintf(w, "  %d messages dropped\n", n)
				return nil
			},
		},
		{
			Name: "/remember", Args: "<note>", Group: "session",
			Help: "append a line to " + memoryFileForWriting,
			Run: func(_ context.Context, arg string, w io.Writer) error {
				if arg == "" {
					return fmt.Errorf("/remember needs something to remember")
				}
				s.mu.Lock()
				dir := s.a.memoryDir
				s.mu.Unlock()
				if err := remember(dir, arg); err != nil {
					return err
				}
				fmt.Fprintf(w, "  noted in %s — it takes effect next session, not this one\n", memoryFileForWriting)
				return nil
			},
		},
		{
			Name: "/trace", Args: "[path|off]", Group: "session",
			Help: "start or stop writing a JSONL event trace",
			Run: func(_ context.Context, arg string, w io.Writer) error {
				switch arg {
				case "":
					fmt.Fprintf(w, "  %s\n", orNone(s.trace.path()))
				case "off":
					s.trace.close()
					fmt.Fprintln(w, "  not recording")
				default:
					if err := s.trace.open(arg); err != nil {
						return err
					}
					fmt.Fprintf(w, "  recording to %s\n", s.trace.path())
					// Where to read it back. A trace nobody opens is a file, and the
					// command that opens it is the one thing a reader who has just
					// switched recording on does not know yet.
					fmt.Fprintf(w, "  read it back with --composer %s, which needs no key\n", s.trace.path())
				}
				return nil
			},
		},
		{
			Name: "/provider", Args: "[name]", Group: "provider",
			Help: "list the providers file, or switch to one of its entries",
			Run: func(_ context.Context, arg string, w io.Writer) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				if arg == "" {
					if len(s.pf.Providers) == 0 {
						fmt.Fprintf(w, "  no providers file — this session is configured from the environment\n")
						return nil
					}
					names := providerNames(s.pf)
					sort.Strings(names)
					for _, n := range names {
						mark := " "
						if n == s.pname {
							mark = "*"
						}
						p := s.pf.Providers[n]
						fmt.Fprintf(w, "  %s %-16s %-10s %s\n", mark, n, p.Protocol, p.Model)
					}
					return nil
				}
				if _, ok := s.pf.Providers[arg]; !ok {
					return fmt.Errorf("no provider named %q (have: %s)", arg, strings.Join(providerNames(s.pf), ", "))
				}
				was := s.opts.provider
				s.opts.provider = arg
				msg, err := s.rebuildProvider()
				if err != nil {
					// Put it back. Unlike /provider-url, this command changed
					// nothing on disk, so leaving the session pointed at a
					// provider that will not build would be a pure loss.
					s.opts.provider = was
					return err
				}
				fmt.Fprintf(w, "  %s\n", msg)
				return nil
			},
		},
		{
			Name: "/set", Args: "[name [value]]", Group: "agent",
			Help: "show or change a limit without restarting",
			Run:  s.runSet,
		},
	}
}

// ---------------------------------------------------------------------------
// /set
// ---------------------------------------------------------------------------

// knob is one runtime-changeable setting.
//
// A table rather than one command each, because there are nine of them and they
// are all the same shape: read a number, write a number. The ones that are NOT
// here are the ones that cannot be changed after startup — the shell it found,
// the trace format — and /set says so by not listing them rather than by
// accepting the change and ignoring it.
type knob struct {
	name string
	help string
	get  func() string
	set  func(string) error
}

func (s *shellSession) knobs() []knob {
	a := s.a
	num := func(p *int, min int) func(string) error {
		return func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return err
			}
			if n < min {
				return fmt.Errorf("want at least %d", min)
			}
			*p = n
			return nil
		}
	}
	dur := func(p *time.Duration) func(string) error {
		return func(v string) error {
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			if d <= 0 {
				return fmt.Errorf("want a positive duration")
			}
			*p = d
			return nil
		}
	}
	frac := func(p *float64) func(string) error {
		return func(v string) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}
			if f < 0 || f > 1 {
				return fmt.Errorf("want a fraction between 0 and 1")
			}
			*p = f
			return nil
		}
	}
	return []knob{
		{"max-turns", "tool-call rounds per user message", func() string { return strconv.Itoa(a.cfg.maxTurns) }, num(&a.cfg.maxTurns, 1)},
		{"sub-turns", "rounds a subagent gets", func() string { return strconv.Itoa(a.cfg.subTurns) }, num(&a.cfg.subTurns, 1)},
		{"max-output", "bytes of command output the model may see", func() string { return strconv.Itoa(a.cfg.maxOutput) }, num(&a.cfg.maxOutput, 1)},
		{"max-depth", "how deep subagents may nest", func() string { return strconv.Itoa(a.maxDepth) }, num(&a.maxDepth, 0)},
		{"timeout", "kill a command after this long", func() string { return a.cfg.timeout.String() }, dur(&a.cfg.timeout)},
		{"compact-at", "compact past this fraction of the window", func() string { return fmt.Sprintf("%.2f", a.comp.threshold) }, frac(&a.comp.threshold)},
		{"keep", "fraction of the window left in place after compacting", func() string { return fmt.Sprintf("%.2f", a.comp.keepRatio) }, frac(&a.comp.keepRatio)},
		{"yolo", "run every command without asking", func() string { return onOrOff(a.cfg.yolo) }, func(v string) error {
			b, err := parseOnOff(v, a.cfg.yolo)
			if err != nil {
				return err
			}
			a.cfg.yolo, a.g.yolo = b, b
			return nil
		}},
		{"show-request", "print the full request body before each call", func() string { return onOrOff(s.view.showRequest) }, func(v string) error {
			b, err := parseOnOff(v, s.view.showRequest)
			if err != nil {
				return err
			}
			s.view.showRequest = b
			return nil
		}},
	}
}

func parseOnOff(v string, cur bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "yes", "true", "1":
		return true, nil
	case "off", "no", "false", "0":
		return false, nil
	case "":
		return !cur, nil
	default:
		return cur, fmt.Errorf("want on or off, got %q", v)
	}
}

func (s *shellSession) runSet(_ context.Context, arg string, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := s.knobs()

	name, value := arg, ""
	if i := strings.IndexAny(arg, " \t"); i >= 0 {
		name, value = arg[:i], strings.TrimSpace(arg[i+1:])
	}
	if name == "" {
		width := 0
		for _, k := range ks {
			if len(k.name) > width {
				width = len(k.name)
			}
		}
		for _, k := range ks {
			fmt.Fprintf(w, "  %-*s  %-12s  %s\n", width, k.name, k.get(), k.help)
		}
		fmt.Fprintf(w, "\n  /set <name> <value>. Everything not listed is fixed when the process starts.\n")
		return nil
	}
	for _, k := range ks {
		if k.name != name {
			continue
		}
		if value == "" {
			fmt.Fprintf(w, "  %s = %s   %s\n", k.name, k.get(), k.help)
			return nil
		}
		if err := k.set(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(w, "  %s = %s\n", k.name, k.get())
		return nil
	}
	return fmt.Errorf("no setting called %q — /set with no argument lists them", name)
}

// Row is tui.Row under the name the rest of this file uses. An alias rather than
// an import-and-qualify on every one of forty literals.
type Row = tui.Row
