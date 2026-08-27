// 阶段 02——运行命令。
//
// 从阶段 01 继承而来，只改了一条原则：这个文件中没有东西
// 打印。runBash 返回一个结果；调用者把它转成事件。这就是为什么
// 同一条命令，能够出现在终端上、trace 文件里，以及几个月后的
// 一次重放中，却不需要三份格式化代码的原因。
//
// 超时、进程树杀死、头 + 尾截断，以及清理三部曲背后的推理，
// 都在 docs/01-dont-die.md 里。它没有改变；只是它的管道改了。
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
		// 复制 goroutine 仍然可能在缓冲区中写入；在这里读它们会是
		// 数据竞争。报告，不取任何东西。
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
		// 遏制失效了，但命令还是运行了。要在模型和 **trace** 都能看到
		// 的那个唯一地方，把这一点说清楚。
		res.Stderr += fmt.Sprintf("\n[harness: process group adoption failed: %v — a timeout can only kill the shell itself]", adoptErr)
	}
	return res
}

// render 把结果转成模型将看到的确切文本，并报告是否有内容
// 被丢弃，好让调用者把这一点放进事件里。
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

// truncate 保留头和尾，丢弃中间。参见 docs/01-dont-die.md，
// 了解为什么只截取开头会丢失真正重要的那一行。
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

// sanitize 剥离 ANSI 转义、标准化 CRLF、替换掉无效的 UTF-8，
// 这样一个用本地代码页写入的程序，会明显地失败，而不是悄无
// 声息地失败。
func sanitize(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return s
}

// parseBashArgs 会在分派前做验证。一次不返回错误的 unmarshal，
// 不等于一次经过验证的调用——具体见 docs/01-dont-die.md 中实际
// 观察到的 `{"raw_arguments": ""}` 载荷，这个函数存在，就是为了
// 拒绝这种载荷。
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
