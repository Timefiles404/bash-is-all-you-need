// 阶段 02——跑命令。
//
// 从阶段 01 搬过来的，只改了一条原则：这个文件里什么都不打印。runBash
// 返回结果，由调用方把它变成事件。同一条命令因此能出现在终端上、trace
// 文件里、几个月后的重放里，而排版代码只有一份，不是三份。
//
// 超时、杀进程树、头+尾截断，还有 sanitize 那三件事，背后的道理全在
// docs/01-dont-die.md 里。道理没变，变的只是管道。
package main

import (
	"bytes"
	"context"
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
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Cancelled bool
	Unreaped  bool
	Duration  time.Duration
}

// runBash 在阶段 10 拿到了 context，而且**不是**通过 exec.CommandContext。
//
// exec.CommandContext 是显然的答案，在这里也是错的答案：它只给
// cmd.Process 发信号，别的都不管，于是每个孙进程都活了下来——而那正是阶
// 段 01 用进程组换来的容器化。把 context 接进已有的那个 select，就能复
// 用 g.kill()，进程树死掉的方式和它本来在超时时死掉的一样。
//
// 两个出口保持可区分，因为对模型来说它们的意思不同。超时是关于这条命令
// 的：它说的是"这条太慢了，换个窄一点的试"。取消是关于这次会话的：没有
// 下一条命令了，让模型重试就是骗它。
func runBash(ctx context.Context, shell, command string, timeout time.Duration) execResult {
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

	var timedOut, cancelled, unreaped bool
	var waitErr error

	// 杀掉整棵树，再给五秒让它被回收。两个出口共用这一段：超时的命令会清
	// 掉进程组，被取消的命令不能反倒把它漏在那儿。
	stop := func() {
		g.kill()
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			unreaped = true
		}
	}

	select {
	case waitErr = <-done:
	case <-ctx.Done():
		cancelled = true
		stop()
	case <-time.After(timeout):
		timedOut = true
		stop()
	}

	res := execResult{TimedOut: timedOut, Cancelled: cancelled, Unreaped: unreaped, Duration: time.Since(started)}
	if unreaped {
		// 负责拷贝的 goroutine 可能还在往 buffer 里写，这时候读就是
		// data race。只报告，什么都不取。
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
		// 进程没圈住，但命令还是跑了。把这事说出来，说在模型和 trace 都
		// 看得到的那一处。
		res.Stderr += fmt.Sprintf("\n[harness: process group adoption failed: %v — a timeout can only kill the shell itself]", adoptErr)
	}
	return res
}

// render 把结果变成模型将看到的那段文本，一字不差；同时报告有没有东西
// 被丢掉，好让调用方写进事件里。
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
	if r.Cancelled {
		// 故意不写成建议。这里其他每条状态都在告诉模型下一步做什么；而这一
		// 条正是没有下一步的那种情况，给建议就等于给出它执行不了的指令。
		status = fmt.Sprintf("\n[CANCELLED after %s — the session is stopping and the process tree was killed]",
			r.Duration.Round(time.Millisecond))
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

// truncate 留头留尾，丢中间。只留头的截断为什么偏偏丢掉那行要紧的，
// 见 docs/01-dont-die.md。
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

// sanitize 剥掉 ANSI 转义、把 CRLF 归一化、替换非法 UTF-8，这样按本地
// 代码页输出的程序会明明白白地坏掉，而不是悄没声地坏掉。
func sanitize(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return s
}

// parseBashArgs 先校验再派发。unmarshal 不报错，不等于这次调用校验过
// ——它挡的是真见过的那种 `{"raw_arguments": ""}` payload，见
// docs/01-dont-die.md。
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
