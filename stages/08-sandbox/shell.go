// Stage 08 — level 3: be the shell.
//
// policy.go inspects a command from outside and loses, twice, for the same
// reason both times: it is looking at a description of what will happen while
// the shell is deciding what actually happens. Quoting beats the string check.
// Expansion beats the parse check.
//
// The move that works is to stop being an outside observer. Embed the
// interpreter, and every command it is about to execute arrives as a finished
// argument vector — **after** quote removal, parameter expansion, command
// substitution, arithmetic, tilde expansion, globbing, and `eval`. There is no
// syntax left to hide behind, because syntax is what was just consumed.
//
//	command:  X=.en; eval "cat \$X"'v'
//	argv:     ["cat", ".env"]
//
// Two handlers, and needing both is the non-obvious part:
//
//	ExecHandler  every program the shell runs, with its final argv
//	OpenHandler  every file the SHELL itself opens — redirections
//
// `cat < .env` runs `cat` with **no arguments**. The shell opens the file and
// hands over a file descriptor. A policy that only inspects argv never sees the
// filename at all, and would have let that through at every level including
// this one.
//
// And then the honest part, which is the rest of the chapter: this is a
// **policy and observability layer, not a security boundary.** It sees every
// command. It cannot see inside one. `python -c "..."` is one exec, and after
// that exec the sandbox has no further opinions about anything.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// sandbox is the policy and the audit log, shared by every agent in the tree.
//
// It deliberately holds no bus. Which agent is running a command is a property
// of the *call*, not of the sandbox — the same reason render.go reads the depth
// off the event rather than off renderer state. One sandbox serves the parent
// and every subagent, so a bus stored here would be whichever agent happened to
// build it, and every exec, open and refusal in the tree would be reported as
// that agent's. Stage 07 made the tree concurrent; a field like that is how a
// trace of it comes out confidently mislabelled.
type sandbox struct {
	// enforce=false makes this an observer: it reports every exec and every
	// open and blocks nothing. Worth having as a mode of its own — most of the
	// value here is seeing what a shell command actually does, and that value
	// does not require refusing anything. Written once, at construction.
	enforce bool

	mu   sync.Mutex
	root string // where commands run; /open moves it

	execs   []string // every argv seen, after expansion
	opens   []string // every path the shell opened
	blocked []string
}

func newSandbox(root string, enforce bool) *sandbox {
	return &sandbox{root: root, enforce: enforce}
}

// setRoot moves where commands run. /open calls it; nothing else should.
//
// Under the mutex because run reads it, and the two can be on different
// goroutines. Today they are not — the shell will not dispatch /open while a
// turn is in flight — but that is a property of a state machine three files
// away, and a struct shared by every agent in the tree should not need one read
// to know it is safe.
func (s *sandbox) setRoot(dir string) {
	s.mu.Lock()
	s.root = dir
	s.mu.Unlock()
}

func (s *sandbox) dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// run executes one command inside the embedded interpreter, reporting on the
// bus of whichever agent asked for it.
//
// It returns the same execResult as runBash so the rest of the agent cannot
// tell the difference — which is the point of having kept exec.go's rendering
// separate from its running since stage 01.
//
// The bus is an argument rather than a field for the reason on the struct: one
// sandbox serves the whole agent tree, and the answer to "who ran this" changes
// per call. A fresh interpreter is built here anyway, so the handlers it gets
// can close over that answer at no cost.
func (s *sandbox) run(command string, timeout time.Duration, bus *Bus) execResult {
	started := time.Now()

	file, err := syntax.NewParser().Parse(strings.NewReader(command), "cmd")
	if err != nil {
		// A parse error here is a *shell* parse error, reported before anything
		// ran. Better than bash's behaviour, which is to execute everything up
		// to the syntax error and then complain.
		return execResult{
			Stderr:   "sandbox: " + err.Error(),
			ExitCode: 2,
			Duration: time.Since(started),
		}
	}

	var stdout, stderr bytes.Buffer
	runner, err := interp.New(
		interp.Dir(s.dir()),
		interp.StdIO(nil, &stdout, &stderr),
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.ExecHandlers(s.execHandler(bus)),
		interp.OpenHandler(s.openHandler(bus)),
	)
	if err != nil {
		return execResult{Stderr: "sandbox: " + err.Error(), ExitCode: -1, Duration: time.Since(started)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	runErr := runner.Run(ctx, file)
	res := execResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(started),
	}

	// The timeout is the context's, and cancelling the context kills the child
	// process the default handler started — so stage 01's process-group work is
	// still doing its job here, one layer down.
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}

	var status interp.ExitStatus
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &status):
		res.ExitCode = int(status)
	default:
		// A handler returned a non-status error — a policy refusal, most
		// likely. Surface the text: it is the only thing that will tell the
		// model what to do differently.
		res.ExitCode = 1
		if res.Stderr != "" && !strings.HasSuffix(res.Stderr, "\n") {
			res.Stderr += "\n"
		}
		res.Stderr += runErr.Error()
	}
	return res
}

// execHandler wraps the interpreter's default exec handler, for one call.
//
// `args` inside is the finished argument vector. Everything the shell was going
// to do to the source text has already been done, which is exactly why this is
// the only place a policy can stand.
//
// A method returning a closure returning a closure, which is one layer more
// than the interpreter needs and the layer that carries the bus. The middle
// function is the middleware shape `interp.ExecHandlers` asks for; the innermost
// is the handler itself. Both see `bus` because it is a parameter of the method
// they are declared inside — which is the whole trick, and the reason the bus
// does not have to live on the sandbox where it would be shared by agents that
// do not share a bus.
func (s *sandbox) execHandler(bus *Bus) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			joined := strings.Join(args, " ")

			s.mu.Lock()
			s.execs = append(s.execs, joined)
			s.mu.Unlock()

			// Emitted for EVERY exec, including ones the model never wrote —
			// the ones a pipeline, a loop, an alias or an `eval` produced. This
			// is the "per-process interception" half of the chapter, and reading
			// a trace of it is the fastest way to find out what a shell command
			// really did.
			bus.Emit(Event{Kind: KindSandboxExec, Command: joined})

			if r := s.checkArgv(args); r != nil {
				s.mu.Lock()
				s.blocked = append(s.blocked, joined)
				s.mu.Unlock()
				bus.Emit(Event{Kind: KindSandboxBlock, Command: joined, Text: r.Error()})
				if s.enforce {
					return r
				}
			}
			return next(ctx, args)
		}
	}
}

// openHandler is called for every file the shell itself opens: redirections.
//
// Files opened by the *programs* the shell runs do not come through here — the
// interpreter has no visibility into another process's syscalls, and getting
// that would mean ptrace, seccomp-bpf, or a filesystem namespace, which is the
// OS-level answer this chapter ends on.
func (s *sandbox) openHandler(bus *Bus) interp.OpenHandlerFunc {
	return func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
		s.mu.Lock()
		s.opens = append(s.opens, path)
		s.mu.Unlock()
		bus.Emit(Event{Kind: KindSandboxOpen, Path: path})

		if isSecretPath(path) {
			r := &refusal{Level: "sandbox/open", What: path, Why: "a redirect targets " + secretName}
			s.mu.Lock()
			s.blocked = append(s.blocked, "< "+path)
			s.mu.Unlock()
			bus.Emit(Event{Kind: KindSandboxBlock, Command: "redirect " + path, Text: r.Error()})
			if s.enforce {
				return nil, r
			}
		}
		return interp.DefaultOpenHandler()(ctx, path, flag, perm)
	}
}

// checkArgv is the policy, applied to a finished argument vector.
func (s *sandbox) checkArgv(args []string) *refusal {
	for _, a := range args[1:] {
		if isSecretPath(a) {
			return &refusal{Level: "sandbox/exec", What: a,
				Why: "an argument resolves to " + secretName + " after expansion"}
		}
	}

	// The shell-in-a-shell case, and an honest half-measure.
	//
	// `sh -c 'cat .env'` is one exec whose argv contains a whole new program in
	// a string. Refusing to hand a nested shell a script means the sandbox
	// cannot be trivially stepped around, and it costs the agent a real
	// capability. What it does NOT do is generalise: perl, python, awk, ruby,
	// node, `find -exec`, `git -c core.pager=`, and `make` all take a program
	// as an argument too, and enumerating them is the denylist game that
	// level 1 already lost.
	//
	// It is here because it is worth something and stated as a half-measure
	// because pretending otherwise is how a sandbox gets trusted.
	if len(args) >= 3 {
		switch args[0] {
		case "sh", "bash", "dash", "zsh", "ksh":
			if args[1] == "-c" {
				return &refusal{Level: "sandbox/exec", What: strings.Join(args, " "),
					Why: "a nested shell would run outside this sandbox's view"}
			}
		}
	}
	return nil
}

// report summarises what the sandbox observed, for the end of a session.
func (s *sandbox) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("sandbox: %d commands executed · %d files opened by the shell · %d blocked",
		len(s.execs), len(s.opens), len(s.blocked))
}
