package settings

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIsSecret(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"AGENT_BASE_URL", false},
		{"AGENT_MODEL", false},
		{"AGENT_PROTOCOL", false},
		{"AGENT_API_KEY", true},
		{"openai_token", true},
		{"MY_PASSWD", true},
		{"MY_PASSWORD", true},
		{"client_secret", true},
		{"AWS_CREDENTIAL_FILE", true},
		{"", false},
	}
	for _, c := range cases {
		if got := IsSecret(c.name); got != c.want {
			t.Errorf("IsSecret(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRedact(t *testing.T) {
	cases := []struct {
		what  string
		name  string
		value string
		want  string
	}{
		{"non-secret passes through", "AGENT_BASE_URL", "https://api.example.com/v1", "https://api.example.com/v1"},
		{"non-secret empty passes through", "AGENT_MODEL", "", ""},
		{"secret unset", "AGENT_API_KEY", "", "(unset)"},
		{"secret of exactly 8", "AGENT_API_KEY", "sk-01234", "(set, 8 chars)"},
		{"secret of 9 shows four", "AGENT_API_KEY", "sk-012345", "sk-0… (9 chars)"},
		{"realistic key", "AGENT_API_KEY", "sk-abcdefghijklmnopqrstuvwxyz", "sk-a… (29 chars)"},
		// Ten CJK characters are thirty bytes. A byte-based implementation
		// would slice mid-character and report 30.
		{"CJK secret counts runes", "AGENT_API_KEY", "密钥密钥密钥密钥密钥", "密钥密钥… (10 chars)"},
		{"CJK secret of eight", "MY_PASSWD", "密钥密钥密钥密钥", "(set, 8 chars)"},
		{"lower-case name is still a secret", "openai_token", "tok-abcdefghij", "tok-… (14 chars)"},
	}
	for _, c := range cases {
		if got := Redact(c.name, c.value); got != c.want {
			t.Errorf("%s: Redact(%q, %q) = %q, want %q", c.what, c.name, c.value, got, c.want)
		}
	}
}

func TestRedactRevealsNeitherMiddleNorTail(t *testing.T) {
	values := []string{
		"sk-proj-0123456789abcdefghijklmnopqrstuvwxyz",
		"密钥这是一个很长的密钥值需要被遮盖起来",
		"tok_AAAABBBBCCCCDDDD",
	}
	for _, value := range values {
		got := Redact("AGENT_API_KEY", value)
		runes := []rune(value)

		if !utf8.ValidString(got) {
			t.Errorf("Redact(%q) = %q, which is not valid UTF-8 — a character was cut in half", value, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("Redact(%q) = %q, which contains a replacement character", value, got)
		}
		// Only the first four characters may appear. Any later four-character
		// window of the value must be absent, which rules out both the middle
		// and the tail without hard-coding the format.
		for i := 1; i+4 <= len(runes); i++ {
			window := string(runes[i : i+4])
			if strings.Contains(got, window) {
				t.Errorf("Redact(%q) = %q leaks %q from offset %d", value, got, window, i)
			}
		}
		if last := string(runes[len(runes)-1:]); strings.Contains(got, last) && !strings.Contains(string(runes[:4]), last) {
			t.Errorf("Redact(%q) = %q shows the final character %q", value, got, last)
		}
	}
}
