//go:build js && wasm

// wasmshell — stage 08's shell, hosted in a browser tab.
//
// Stage 08 embeds mvdan.cc/sh and drives it through two handlers:
//
//	ExecHandler  every program the shell runs, with its final argv
//	OpenHandler  every file the SHELL itself opens — redirections
//
// The chapter's point is that those are the only places where the truth about a
// command is complete. The browser's point is that they are also the only two
// places where a browser differs from a machine: there are no programs to exec,
// and there is no disk to open. Both differences land exactly on a seam the
// lesson already put there, which is why the shell a learner drives on the site
// is the same interpreter the chapter is about rather than a mock of it.
//
// What is real here:
//
//   - the parser, expansion, quoting, globbing, pipelines, redirections,
//     subshells, functions, arithmetic, `eval` and every shell builtin, because
//     that is mvdan.cc/sh compiled unmodified;
//   - the filesystem, because Go's js/wasm port routes `os` through a
//     JavaScript global and web/assets/js/runtime/memfs.js implements it;
//   - the policy and the audit log, because they are stage 08's, copied here
//     with their behaviour intact.
//
// What is not real, and is labelled as such wherever a learner can see it: the
// external commands. `cat`, `grep`, `sed` and the rest are re-implementations
// in coreutils.go, not GNU's. They are close enough to teach a pipeline with
// and different enough that a learner should be told, so the shell prints a
// one-line banner saying so and `help` lists exactly what exists.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall/js"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// session is one shell, alive for as long as the tab is.
//
// Stage 08 builds a fresh interp.Runner per command, because an agent's tool
// calls are documented as non-persistent — "cd and environment variables do not
// survive between calls" is in stage 00's system prompt. A human at a terminal
// expects the opposite, so the interactive shell on the site keeps one runner
// and the agent-driven one asks for a fresh command context. Both are this
// type; `Persistent` chooses.
type session struct {
	mu     sync.Mutex
	runner *interp.Runner
	audit  audit
	policy *policy
	cancel context.CancelFunc
}

// audit is stage 08's sandbox bookkeeping: every argv, every path, every
// refusal. The UI reads it to draw the sandbox panel, so it is kept as three
// ordered slices rather than counters — a count answers "how many" and the
// panel has to answer "which".
type audit struct {
	mu      sync.Mutex
	execs   []string
	opens   []string
	blocked []blockRecord
}

type blockRecord struct {
	Command string `json:"command"`
	Reason  string `json:"reason"`
	Level   string `json:"level"`
}

func (a *audit) exec(argv string) {
	a.mu.Lock()
	a.execs = append(a.execs, argv)
	a.mu.Unlock()
}

func (a *audit) open(path string) {
	a.mu.Lock()
	a.opens = append(a.opens, path)
	a.mu.Unlock()
}

func (a *audit) block(rec blockRecord) {
	a.mu.Lock()
	a.blocked = append(a.blocked, rec)
	a.mu.Unlock()
}

func main() {
	s := &session{policy: defaultPolicy()}
	if err := s.reset("/work"); err != nil {
		fmt.Fprintln(os.Stderr, "wasmshell:", err)
		return
	}

	js.Global().Set("__goshell", js.ValueOf(map[string]any{
		"exec":      js.FuncOf(s.jsExec),
		"cwd":       js.FuncOf(s.jsCwd),
		"interrupt": js.FuncOf(s.jsInterrupt),
		"audit":     js.FuncOf(s.jsAudit),
		"setPolicy": js.FuncOf(s.jsSetPolicy),
		"commands":  js.FuncOf(s.jsCommands),
		"reset":     js.FuncOf(s.jsReset),
	}))

	// Tell the host we are up, then park forever. Returning from main would
	// tear the instance down and take the shell's state with it.
	if ready := js.Global().Get("__goshellReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}
	select {}
}

func (s *session) reset(dir string) error {
	r, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(
			"HOME=/home/learner",
			"PATH=/bin:/usr/bin",
			"PWD="+dir,
			"SHELL=/bin/sh",
			"TERM=xterm-256color",
			"USER=learner",
		)),
		interp.ExecHandlers(s.execMiddleware),
		interp.OpenHandler(s.openHandler),
	)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.runner = r
	s.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// The two handlers
// ---------------------------------------------------------------------------

// execMiddleware is stage 08's execHandler with one extra job.
//
// On a machine, the middleware inspects argv and then calls `next`, which
// forks. Here there is nothing to fork into, so `next` is never reached for a
// command we implement and reaching it at all is the error path. The audit and
// the policy sit in front, unchanged, in the same order and for the same
// reason: argv here is final — after quote removal, expansion, substitution and
// globbing — and that is the only place a policy can stand.
func (s *session) execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		joined := strings.Join(args, " ")
		s.audit.exec(joined)

		if r := s.policy.checkArgv(args); r != nil {
			s.audit.block(blockRecord{Command: joined, Reason: r.Why, Level: "sandbox/exec"})
			if s.policy.enforce {
				return r
			}
		}

		hc := interp.HandlerCtx(ctx)
		if fn, ok := coreutils[args[0]]; ok {
			code := fn(&cmdenv{
				ctx:    ctx,
				args:   args,
				stdin:  hc.Stdin,
				stdout: hc.Stdout,
				stderr: hc.Stderr,
				dir:    hc.Dir,
			})
			if code == 0 {
				return nil
			}
			return interp.NewExitStatus(uint8(code))
		}

		// The honest failure. A learner who types `python3` should be told what
		// is actually going on, not handed bash's "command not found" as though
		// installing something would fix it.
		fmt.Fprintf(hc.Stderr, "%s: not available in the browser shell — there are no external programs here, only the interpreter and the commands `help` lists\n", args[0])
		return interp.NewExitStatus(127)
	}
}

// openHandler is stage 08's, and the reason it still matters in a browser is
// the reason it mattered on a machine: `cat < .env` runs cat with no arguments,
// so a policy that only reads argv never sees the filename.
//
// The default handler underneath goes to os.OpenFile, which under js/wasm goes
// to the JavaScript `fs` global, which is memfs.js. No special case needed.
func (s *session) openHandler(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if path != "/dev/null" {
		s.audit.open(path)
	}
	if r := s.policy.checkPath(path); r != nil {
		s.audit.block(blockRecord{Command: "redirect " + path, Reason: r.Why, Level: "sandbox/open"})
		if s.policy.enforce {
			return nil, r
		}
	}
	return interp.DefaultOpenHandler()(ctx, path, flag, perm)
}

// ---------------------------------------------------------------------------
// The JavaScript bridge
// ---------------------------------------------------------------------------

// jsExec runs one command line.
//
// Signature from JS: exec(line, onStdout, onStderr, onDone).
//
// The work happens on a goroutine and the result arrives through onDone, and
// that is not a style choice. A JS→Go call runs on the caller's stack inside
// the wasm instance; blocking it in interp.Run would block the browser's event
// loop, which means the memfs callbacks Go is waiting on could never fire. The
// shell would deadlock on its first `cat`. Returning immediately and completing
// through a callback is the only shape that works.
func (s *session) jsExec(this js.Value, args []js.Value) any {
	line := args[0].String()
	onStdout, onStderr, onDone := args[1], args[2], args[3]

	go func() {
		// A panic on this goroutine is not a crash the caller can see: it is a
		// promise that never settles and a UI that spins for ever. Worse, in
		// wasm an unrecovered panic tears down the instance and every later
		// command fails with "Go program has already exited". Catch it here,
		// name it as a bug in the shell rather than in the learner's command,
		// and keep the session alive.
		defer func() {
			if r := recover(); r != nil {
				onStderr.Invoke(fmt.Sprintf(
					"wasmshell: internal error while running this command: %v\n"+
						"This is a bug in the site's shell, not in your command.\n", r))
				onDone.Invoke(70, fmt.Sprint(r))
			}
		}()

		s.mu.Lock()
		runner := s.runner
		s.mu.Unlock()

		file, err := syntax.NewParser().Parse(strings.NewReader(line), "cmd")
		if err != nil {
			// A shell parse error, reported before anything ran — better than
			// bash, which executes up to the syntax error and then complains.
			onStderr.Invoke(err.Error() + "\n")
			onDone.Invoke(2, "")
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.cancel = cancel
		s.mu.Unlock()
		defer cancel()

		// Deliberately no Reset() here. Reset restores Dir to the value the
		// runner was constructed with, so calling it per command makes `cd`
		// appear to work and then silently undoes it before the next one.
		// Runner.Run resets itself the first time and never again, which is
		// exactly the persistence an interactive shell needs.
		interp.StdIO(nil, &jsWriter{fn: onStdout}, &jsWriter{fn: onStderr})(runner)

		runErr := runner.Run(ctx, file)

		// `exit` leaves the runner in a terminal state where every later Run
		// returns immediately. A tab is not a process and closing it is not
		// what the learner meant, so the shell comes back — at the same
		// directory, with the same variables, which Reset would otherwise throw
		// away.
		if runner.Exited() {
			dir, vars := runner.Dir, runner.Vars
			runner.Reset()
			runner.Dir = dir
			runner.Vars = vars
		}
		code := 0
		var status interp.ExitStatus
		switch {
		case runErr == nil:
		case errorsAs(runErr, &status):
			code = int(status)
		default:
			onStderr.Invoke(runErr.Error() + "\n")
			code = 1
		}
		onDone.Invoke(code, "")
	}()
	return nil
}

func (s *session) jsCwd(this js.Value, args []js.Value) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner == nil {
		return "/"
	}
	return s.runner.Dir
}

func (s *session) jsInterrupt(this js.Value, args []js.Value) any {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
	return nil
}

func (s *session) jsAudit(this js.Value, args []js.Value) any {
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	blocked := make([]any, 0, len(s.audit.blocked))
	for _, b := range s.audit.blocked {
		blocked = append(blocked, map[string]any{"command": b.Command, "reason": b.Reason, "level": b.Level})
	}
	return map[string]any{
		"execs":   toAnySlice(s.audit.execs),
		"opens":   toAnySlice(s.audit.opens),
		"blocked": blocked,
	}
}

// jsSetPolicy is how the stage 08 lesson is driven from the UI: the three
// inspectors the chapter compares are three settings of one object, and the
// learner flips between them and watches the same bypass succeed or fail.
func (s *session) jsSetPolicy(this js.Value, args []js.Value) any {
	cfg := args[0]
	p := &policy{
		enforce: cfg.Get("enforce").Truthy(),
		level:   cfg.Get("level").String(),
		secret:  ".env",
	}
	if v := cfg.Get("secret"); v.Type() == js.TypeString && v.String() != "" {
		p.secret = v.String()
	}
	s.policy = p
	return nil
}

func (s *session) jsCommands(this js.Value, args []js.Value) any {
	names := make([]string, 0, len(coreutils))
	for name := range coreutils {
		names = append(names, name)
	}
	sort.Strings(names)
	return toAnySlice(names)
}

func (s *session) jsReset(this js.Value, args []js.Value) any {
	dir := "/work"
	if len(args) > 0 && args[0].Type() == js.TypeString {
		dir = args[0].String()
	}
	s.audit = audit{}
	if err := s.reset(dir); err != nil {
		return err.Error()
	}
	return ""
}

// jsWriter turns Go writes into JS calls.
//
// One call per Write, and no buffering here: the interpreter already writes a
// line at a time for most commands, and a terminal that shows output as it is
// produced is the difference between a shell and a batch job. The copy into a
// string is required — the byte slice is a view into wasm memory that Go may
// reuse the moment this returns.
type jsWriter struct{ fn js.Value }

func (w *jsWriter) Write(p []byte) (int, error) {
	w.fn.Invoke(string(p))
	return len(p), nil
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// errorsAs avoids importing "errors" for one call; interp.ExitStatus is a
// concrete type and the assertion is exact.
func errorsAs(err error, target *interp.ExitStatus) bool {
	for err != nil {
		if st, ok := err.(interp.ExitStatus); ok {
			*target = st
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
