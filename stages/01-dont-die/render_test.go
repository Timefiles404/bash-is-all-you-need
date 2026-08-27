package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsBothEnds(t *testing.T) {
	body := strings.Repeat("x", 5000)
	s := "HEAD-MARKER" + body + "TAIL-MARKER"
	out, cut := truncate(s, 1000)
	if !cut {
		t.Fatal("expected truncation")
	}
	if !strings.HasPrefix(out, "HEAD-MARKER") {
		t.Error("lost the head")
	}
	if !strings.HasSuffix(out, "TAIL-MARKER") {
		t.Error("lost the tail")
	}
	if !strings.Contains(out, "bytes elided") {
		t.Error("no elision marker")
	}
}

func TestTruncateShortStringUntouched(t *testing.T) {
	if out, cut := truncate("small", 1000); cut || out != "small" {
		t.Errorf("short string was modified: %q cut=%v", out, cut)
	}
}

func TestTruncateNeverSplitsRunes(t *testing.T) {
	// 中文文本：每个 rune 是 3 字节，所以朴素的字节切片对于大多数预算会
	// 落在字符中间。
	s := strings.Repeat("中文测试", 500)
	for budget := 256; budget < 1200; budget += 7 {
		out, _ := truncate(s, budget)
		if !utf8.ValidString(out) {
			t.Fatalf("budget %d produced invalid UTF-8", budget)
		}
	}
}

func TestSanitizeStripsANSI(t *testing.T) {
	in := "\x1b[0;32mPASS\x1b[0m \x1b[1;31mFAIL\x1b[0m"
	if got := sanitize(in); got != "PASS FAIL" {
		t.Errorf("got %q", got)
	}
}

func TestSanitizeNormalisesCRLF(t *testing.T) {
	if got := sanitize("a\r\nb\r\n"); got != "a\nb\n" {
		t.Errorf("got %q", got)
	}
}

func TestSanitizeRepairsInvalidUTF8(t *testing.T) {
	// 合乎 GBK 编码、但不合乎 UTF-8 编码的字节——本地 Windows 程序
	// 在中文系统上会打印出来的东西。
	gbk := string([]byte{0xD6, 0xD0, 0xCE, 0xC4})
	got := sanitize(gbk)
	if !utf8.ValidString(got) {
		t.Fatal("sanitize left invalid UTF-8 in place")
	}
	if !strings.Contains(got, "�") {
		t.Error("expected replacement characters to make the failure visible")
	}
}

func TestRenderSurfacesTimeout(t *testing.T) {
	r := execResult{Stdout: "partial", TimedOut: true}
	got := r.render(8000)
	if !strings.Contains(got, "TIMED OUT") {
		t.Error("timeout not surfaced to the model")
	}
	if !strings.Contains(got, "partial") {
		t.Error("output captured before the timeout was thrown away")
	}
}

func TestRenderLabelsStderr(t *testing.T) {
	r := execResult{Stdout: "ok", Stderr: "boom", ExitCode: 1}
	got := r.render(8000)
	if !strings.Contains(got, "<stderr>") || !strings.Contains(got, "boom") {
		t.Errorf("stderr not attributed: %q", got)
	}
	if !strings.Contains(got, "exit 1") {
		t.Error("exit code not surfaced")
	}
}

// 这些情况逐字取自网关的真实返回——就是那次工具调用被切短
// 时，网关实际返回的内容。详情见 parseBashArgs。
func TestParseBashArgsRejectsUnusableCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"observed raw_arguments empty", `{"raw_arguments": ""}`},
		{"observed raw_arguments partial", `{"raw_arguments": "{\"command\": \"find"}`},
		{"command key absent", `{}`},
		{"command empty", `{"command": ""}`},
		{"command whitespace", `{"command": "   \n "}`},
		{"truncated json", `{"command": "grep -rn`},
	} {
		if got, err := parseBashArgs(tc.raw); err == nil {
			t.Errorf("%s: accepted an unusable call and produced %q", tc.name, got)
		}
	}
}

func TestParseBashArgsAcceptsRealCalls(t *testing.T) {
	got, err := parseBashArgs(`{"command": "grep -rn \"deadline\" ."}`)
	if err != nil {
		t.Fatalf("rejected a valid call: %v", err)
	}
	if got != `grep -rn "deadline" .` {
		t.Errorf("got %q", got)
	}
}
