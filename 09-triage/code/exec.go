// Stage 02 — running commands.
//
// Carried over from stage 01 with one change of principle: nothing in this file
// prints. runBash returns a result; the caller turns it into events. That is
// what lets the same command appear on a terminal, in a trace file, and in a
// replay months later without three copies of the formatting.
//
// The reasoning behind the timeout, the process-tree kill, the head+tail
// truncation and the sanitising trio is all in 01-dont-die/doc/. It has not
// changed; only its plumbing has.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

type execResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Unreaped bool
	Duration time.Duration
}

func runBash(shell, command string, timeout time.Duration) execResult {
	started := time.Now()

	g, err := newProcGroup()
	if err != nil {
		return execResult{Stderr: fmt.Sprintf("could not create process group: %v", err), ExitCode: -1}
	}
	defer g.Close()

	cmd := exec.Command(shell, "-c", command)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	g.attach(cmd)

	if err := cmd.Start(); err != nil {
		return execResult{Stderr: fmt.Sprintf("could not start command: %v", err), ExitCode: -1}
	}
	adoptErr := g.adopt(cmd)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timedOut, unreaped bool
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(timeout):
		timedOut = true
		g.kill()
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			unreaped = true
		}
	}

	res := execResult{TimedOut: timedOut, Unreaped: unreaped, Duration: time.Since(started)}
	if unreaped {
		// The copying goroutines may still be writing into the buffers; reading
		// them here would be a data race. Report, take nothing.
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
	if adoptErr != nil {
		// Containment was lost but the command still ran. Say so in the one
		// place the model and the trace will both see it.
		res.Stderr += fmt.Sprintf("\n[harness: process group adoption failed: %v — a timeout can only kill the shell itself]", adoptErr)
	}
	return res
}

// render turns a result into the exact text the model will see, and reports
// whether anything was dropped so the caller can put that in an event.
func (r execResult) render(maxOutput int) (string, bool) {
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

	status := fmt.Sprintf("\n[exit %d · %s]", r.ExitCode, r.Duration.Round(time.Millisecond))
	if r.TimedOut {
		status = fmt.Sprintf("\n[TIMED OUT after %s — the process tree was killed]", r.Duration.Round(time.Millisecond))
	}
	if r.Unreaped {
		status = fmt.Sprintf("\n[TIMED OUT after %s and could not be reaped — output was discarded as unsafe to read. Do not run this command again.]",
			r.Duration.Round(time.Millisecond))
	}
	cut := outCut || errCut
	if cut {
		status += " [output truncated — rerun with a filter such as grep/head/tail]"
	}
	b.WriteString(status)
	return b.String(), cut
}

// truncate keeps the head and the tail and drops the middle. See
// 01-dont-die/doc/ for why head-only truncation loses the line that mattered.
func truncate(s string, max int) (string, bool) {
	if max < 256 {
		max = 256
	}
	if len(s) <= max {
		return s, false
	}
	head := max * 2 / 3
	tail := max - head

	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	cut := len(s) - tail
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return fmt.Sprintf("%s\n\n[... %d bytes elided ...]\n\n%s", s[:head], cut-head, s[cut:]), true
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\a\x1b]*(\a|\x1b\\)|\x1b[@-Z\\-_]`)

// sanitize strips ANSI escapes, normalises CRLF, and replaces invalid UTF-8 so
// a program writing in the local code page fails visibly rather than silently.
func sanitize(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return s
}

// parseBashArgs validates before dispatching. An unmarshal that returns no
// error is not a validated call — see 01-dont-die/doc/ for the observed
// `{"raw_arguments": ""}` payload this exists to reject.
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
