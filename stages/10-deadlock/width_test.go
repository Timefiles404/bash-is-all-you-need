package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 整个测试套件打字符串都用 %q。转义序列按定义在终端里就是看不见的——失败
// 消息要是把它们真渲染出来，只会把断言输出涂成红的、再吃掉几个字符，然后
// 你就从调试自己的代码变成了调试测试宿主。

// ---------------------------------------------------------------------------
// 这张表的不变式
// ---------------------------------------------------------------------------

// TestWideRangesSorted 守的是 inRanges 那个没被检查的前置条件。在没排序的
// 表上做二分不会崩，它只会悄悄漏掉。症状是某一台机器上、某一种字体里，某
// 一个字符的宽度不对——这种事查起来要命，断言起来只要一行。
func TestWideRangesSorted(t *testing.T) {
	for i, r := range wideRanges {
		if r.lo > r.hi {
			t.Errorf("wideRanges[%d] = {%#x, %#x}: lo is above hi, so this range matches nothing", i, r.lo, r.hi)
		}
		if i > 0 && r.lo <= wideRanges[i-1].hi {
			t.Errorf("wideRanges[%d] = {%#x, %#x} starts at or before wideRanges[%d].hi = %#x; "+
				"the table must be sorted and disjoint or inRanges' binary search will skip entries",
				i, r.lo, r.hi, i-1, wideRanges[i-1].hi)
		}
	}
}

// ---------------------------------------------------------------------------
// runeWidth
// ---------------------------------------------------------------------------

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		name string
		r    rune
		want int
	}{
		{"ASCII letter", 'a', 1},
		{"ASCII space", ' ', 1},
		{"ASCII digit", '7', 1},
		{"tab is zero — callers must expand tabs before measuring", '\t', 0},
		{"newline is cursor motion, not ink", '\n', 0},
		{"carriage return", '\r', 0},
		{"NUL", 0x00, 0},
		{"DEL", 0x7f, 0},
		{"C1 control", 0x9b, 0},
		{"precomposed e-acute is one rune, one column", 'é', 1},
		{"combining acute accent stacks, occupies nothing", 0x0301, 0},
		{"combining kana voiced mark lives inside the Katakana block", 0x3099, 0},
		{"zero-width space", 0x200b, 0},
		{"zero-width joiner", 0x200d, 0},
		{"BOM", 0xfeff, 0},
		{"CJK ideograph", '你', 2},
		{"Hangul syllable", '한', 2},
		{"Hiragana", 'あ', 2},
		{"Katakana", 'ア', 2},
		{"fullwidth A", 'Ａ', 2},
		{"ideographic comma", '、', 2},
		{"ideographic space is two columns of nothing", 0x3000, 2},
		{"CJK Extension B", 0x20000, 2},
		{"emoji", '😀', 2},
		{"regional indicator (see the flag caveat)", 0x1f1e6, 2},
		{"white heavy check mark is emoji-presentation", 0x2705, 2},
		{"plain check mark is a narrow dingbat", '✓', 1},
		{"East Asian Ambiguous resolves narrow, always", '±', 1},
		{"box drawing stays narrow", '│', 1},
	}
	for _, c := range cases {
		if got := runeWidth(c.r); got != c.want {
			t.Errorf("runeWidth(%q / %#x) = %d, want %d — %s", c.r, c.r, got, c.want, c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// dispWidth
// ---------------------------------------------------------------------------

func TestDispWidth(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want int
	}{
		{"empty", "", 0},
		{"pure ASCII", "hello world", 11},
		{"four CJK characters are eight columns, not four and not twelve", "你好世界", 8},
		{"mixed CJK and ASCII", "ab你好cd", 8},
		{"CJK punctuation counts too", "你好，世界！", 12},
		{"fullwidth forms", "ＡＢ", 4},
		{"Hangul", "한글", 4},
		{"kana", "こんにちは", 10},
		{"combining accent adds no column", "é", 1},
		{"several accents on one base", "é̂̃", 1},
		{"zero-width space", "a​b", 2},
		{"SGR colour is not visible ink", "\x1b[31mred\x1b[0m", 3},
		{"nested colours", "\x1b[1m\x1b[38;5;204mbold pink\x1b[0m", 9},
		{"cursor moves are skipped as well", "\x1b[2Kabc", 3},
		{"OSC-8 hyperlink: the URL is payload, not text", "\x1b]8;;https://example.com\x07link\x1b]8;;\x07", 4},
		{"coloured CJK", "\x1b[32m你好\x1b[0m", 4},
	}
	for _, c := range cases {
		if got := dispWidth(c.s); got != c.want {
			t.Errorf("dispWidth(%q) = %d, want %d — %s\n"+
				"  for reference: %d bytes, %d runes",
				c.s, got, c.want, c.name, len(c.s), utf8.RuneCountInString(c.s))
		}
	}
}

// TestBytesRunesColumnsAreThreeDifferentNumbers 就是本文件的论点，做成了可
// 执行的。这个包里你只记得住一个测试的话，就记住：这三个数彼此不一致，而
// 只有最后那个能排出一屏东西来。
func TestBytesRunesColumnsAreThreeDifferentNumbers(t *testing.T) {
	const s = "é" // "e" 加上 COMBINING ACUTE ACCENT——渲染出来是 "é"

	if got := len(s); got != 3 {
		t.Errorf("len(%q) = %d, want 3 — this is the BYTE count, and it is the number "+
			"%%-20s pads to", s, got)
	}
	if got := utf8.RuneCountInString(s); got != 2 {
		t.Errorf("utf8.RuneCountInString(%q) = %d, want 2 — this is the RUNE count, and it "+
			"is the number a naive []rune truncator uses", s, got)
	}
	if got := dispWidth(s); got != 1 {
		t.Errorf("dispWidth(%q) = %d, want 1 — this is the COLUMN count, the only one the "+
			"terminal agrees with. The accent stacks onto the e; it takes no column of its own", s, got)
	}

	// 同样这三个数，换成 CJK，它们朝另一个方向岔开。
	const cjk = "你好"
	if len(cjk) != 6 || utf8.RuneCountInString(cjk) != 2 || dispWidth(cjk) != 4 {
		t.Errorf("%q: bytes=%d runes=%d columns=%d, want 6/2/4 — bytes over-count, runes "+
			"under-count, and only columns are right",
			cjk, len(cjk), utf8.RuneCountInString(cjk), dispWidth(cjk))
	}
}

// ---------------------------------------------------------------------------
// truncCols
// ---------------------------------------------------------------------------

func TestTruncCols(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"fits exactly, nothing added", "hello", 5, "hello"},
		{"shorter than n is NOT padded — that is padCols' job", "hello", 10, "hello"},
		{"plain cut", "hello", 3, "hel"},
		{"n is zero", "hello", 0, ""},
		{"n is negative", "hello", -1, ""},
		{"empty input", "", 4, ""},
		{"CJK fits exactly", "你好世界", 8, "你好世界"},
		{"CJK cut on an even boundary", "你好世界", 4, "你好"},
		{"already-closed colour is not re-closed", "\x1b[31mred\x1b[0m", 10, "\x1b[31mred\x1b[0m"},
		{"cut inside coloured text closes the colour", "\x1b[31mred\x1b[0m", 2, "\x1b[31mre\x1b[0m"},
		{"combining mark rides along with its base", "éx", 1, "é"},
	}
	for _, c := range cases {
		got := truncCols(c.s, c.n)
		if got != c.want {
			t.Errorf("truncCols(%q, %d) = %q, want %q — %s", c.s, c.n, got, c.want, c.name)
		}
	}
}

// TestTruncColsWideBoundary 是所有人都会写错的那一个。
//
// 把 "你好世界" 切到 3 列，没法切干净：你 占 2，加上 好 就成了 4。半个 好
// 绝不能送到终端上，所以我们停在它前面——然后就欠了调用方那落单的一列，因
// 为格子少一列跟多一列，把表格错开的程度一模一样。
func TestTruncColsWideBoundary(t *testing.T) {
	// 注释放在各自那行的上面，不是后面。gofmt 按 rune 数对齐行尾注释，所以
	// 在一张塞满 CJK 字面量的表上，它排出来的东西它自己认为是对齐的，你的终
	// 端不这么认为——正是这个包要修的那个 bug，出现在格式化工具身上。
	cases := []struct {
		s    string
		n    int
		want string
	}{
		// 装得下一个宽字符，然后一个空格补那落单的列。
		{"你好世界", 3, "你 "},
		// 装得下两个宽字符，然后一个空格。
		{"你好世界", 5, "你好 "},
		// 一个都装不下，结果照样正好是一列。
		{"你好世界", 1, " "},
		// 宽字符正好跨在上限上。
		{"a你b", 2, "a "},
		// reset 排在补空格之前，所以填进去的空格不会被还开着的背景色涂上。
		{"\x1b[36m你好", 3, "\x1b[36m你\x1b[0m "},
	}
	for _, c := range cases {
		got := truncCols(c.s, c.n)
		if got != c.want {
			t.Errorf("truncCols(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
		if w := dispWidth(got); w != c.n {
			t.Errorf("truncCols(%q, %d) is %d columns wide, want exactly %d — a truncator "+
				"that stops short of a wide character still owes the caller the leftover column",
				c.s, c.n, w, c.n)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("truncCols(%q, %d) = %q contains U+FFFD: the cut landed inside a "+
				"multi-byte rune", c.s, c.n, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncCols(%q, %d) = %q is not valid UTF-8 — half a character escaped",
				c.s, c.n, got)
		}
	}
}

// TestTruncColsDoesNotSplitEscape 在一个正好落在转义序列字节中间的列上切字
// 符串。天真的 s[:n] 会留下悬空的 "\x1b[3"；终端接着就会把程序里任何地方打
// 印的下一个字符吞掉，当成这段序列缺的那个结束字节。
func TestTruncColsDoesNotSplitEscape(t *testing.T) {
	const s = "ab\x1b[31mcd"
	const want = "ab\x1b[31mc\x1b[0m"

	got := truncCols(s, 3)
	if got != want {
		t.Fatalf("truncCols(%q, 3) = %q, want %q — the escape must be copied whole "+
			"and the colour it opened must be closed", s, got, want)
	}
	if !strings.Contains(got, "\x1b[31m") {
		t.Errorf("truncCols(%q, 3) = %q: the SGR sequence did not survive intact", s, got)
	}
	if dispWidth(got) != 3 {
		t.Errorf("truncCols(%q, 3) = %q is %d columns, want 3 — escape bytes must not "+
			"count toward n", s, got, dispWidth(got))
	}
	// 做个对照，也让这种失败具体起来：字节切片会切成什么样。
	if broken := s[:3]; broken != "ab\x1b" {
		t.Errorf("sanity check on the counter-example changed: s[:3] = %q", broken)
	}
}

// TestTruncColsClosesOpenSGR 就是那个防止程序把用户的 shell 提示符永久染红
// 的测试。
func TestTruncColsClosesOpenSGR(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
	}{
		{"colour opened and never closed in the source", "\x1b[31mredtext", 3},
		{"colour closed in the source but after the cut", "\x1b[31mredtext\x1b[0m", 3},
		{"bold plus 256-colour", "\x1b[1m\x1b[38;5;204mpink", 2},
		{"colour opened before CJK", "\x1b[32m你好世界", 4},
	}
	for _, c := range cases {
		got := truncCols(c.s, c.n)
		if !strings.HasSuffix(got, "\x1b[0m") {
			t.Errorf("truncCols(%q, %d) = %q does not end with a reset — %s. Whatever is "+
				"printed next inherits this colour, including the shell prompt after the "+
				"program exits", c.s, c.n, got, c.name)
		}
		if dispWidth(got) != c.n {
			t.Errorf("truncCols(%q, %d) = %q is %d columns, want %d", c.s, c.n, got, dispWidth(got), c.n)
		}
	}

	// 反过来的一面：从没开过颜色的字符串，别给它硬钉上一个 reset；源串自己
	// 已经关掉了的，也别再加第二个。
	if got := truncCols("plain", 3); strings.Contains(got, "\x1b") {
		t.Errorf("truncCols(%q, 3) = %q — an uncoloured string must come back with no "+
			"escapes at all", "plain", got)
	}
	if got := truncCols("\x1b[31mred\x1b[0m", 5); strings.Count(got, "\x1b[0m") != 1 {
		t.Errorf("truncCols(%q, 5) = %q has %d resets, want 1 — a colour the source already "+
			"closed does not need closing again", "\x1b[31mred\x1b[0m", got, strings.Count(got, "\x1b[0m"))
	}
}

// ---------------------------------------------------------------------------
// padCols
// ---------------------------------------------------------------------------

func TestPadCols(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string // "" 表示：只检查列数
	}{
		{"ASCII short", "ab", 5, "ab   "},
		{"ASCII exact", "abc", 3, "abc"},
		{"ASCII too long", "abcdef", 3, "abc"},
		{"empty", "", 3, "   "},
		{"CJK short — two runes, four columns, one space of padding", "你好", 5, "你好 "},
		{"CJK exact", "你好", 4, "你好"},
		{"CJK too long, cut on an even boundary", "你好世界", 4, "你好"},
		{"CJK too long, cut on a wide boundary", "你好世界", 5, "你好 "},
		{"combining sequence pads as one column", "é", 3, "é  "},
		{"coloured, short", "\x1b[31mred\x1b[0m", 8, "\x1b[31mred\x1b[0m     "},
		{"coloured, too long", "\x1b[31mredder\x1b[0m", 3, ""},
		{"fullwidth", "ＡＢ", 6, "ＡＢ  "},
	}
	for _, c := range cases {
		got := padCols(c.s, c.n)
		if c.want != "" && got != c.want {
			t.Errorf("padCols(%q, %d) = %q, want %q — %s", c.s, c.n, got, c.want, c.name)
		}
		if w := dispWidth(got); w != c.n {
			t.Errorf("padCols(%q, %d) = %q is %d columns, want exactly %d — %s. This is the "+
				"function a table's alignment rests on; \"about right\" is not a value it may return",
				c.s, c.n, got, w, c.n, c.name)
		}
	}
}

// TestPadColsBuildsAlignedTable 就是文档注释许诺的那个验收测试：一列名字，
// ASCII、CJK、带颜色的混在一起，全都落在同一个位置上。fmt 的 %-12s 做不到
// 这件事，而这就是 padCols 存在的全部理由。
func TestPadColsBuildsAlignedTable(t *testing.T) {
	names := []string{"main.go", "读我.md", "\x1b[36mREADME\x1b[0m", "日本語テキスト.txt", "éclair.txt"}
	const col = 12
	for _, name := range names {
		row := padCols(name, col) + "|"
		if w := dispWidth(row); w != col+1 {
			t.Errorf("row for %q is %d columns, want %d — the separator would land in a "+
				"different place on this row than on the others, which is what a sheared "+
				"table looks like", name, w, col+1)
		}
	}
}

// ---------------------------------------------------------------------------
// wrapCols
// ---------------------------------------------------------------------------

func TestWrapCols(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want []string
	}{
		{"ASCII, even split", "abcdef", 3, []string{"abc", "def"}},
		{"ASCII, ragged tail", "abcdefg", 3, []string{"abc", "def", "g"}},
		{"fits on one line", "abc", 10, []string{"abc"}},
		{"empty input is one blank line, not zero lines", "", 5, []string{""}},
		{"CJK, two per line", "你好世界", 4, []string{"你好", "世界"}},
		{"CJK with an odd width never splits a character", "你好世界", 5, []string{"你好", "世界"}},
		{"mixed: the wide char is pushed to the next line whole", "ab你cd", 3, []string{"ab", "你c", "d"}},
		{"embedded newline forces a break", "ab\ncd", 5, []string{"ab", "cd"}},
		{"combining mark stays with its base", "ééé", 2, []string{"éé", "é"}},
		{"n is zero: nil, not an infinite run of empty lines", "abc", 0, nil},
		{"n is negative", "abc", -3, nil},
	}
	for _, c := range cases {
		got := wrapCols(c.s, c.n)
		if len(got) != len(c.want) {
			t.Errorf("wrapCols(%q, %d) produced %d lines %q, want %d lines %q — %s",
				c.s, c.n, len(got), got, len(c.want), c.want, c.name)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("wrapCols(%q, %d) line %d = %q, want %q — %s",
					c.s, c.n, i, got[i], c.want[i], c.name)
			}
		}
		for i, line := range got {
			if w := dispWidth(line); w > c.n {
				t.Errorf("wrapCols(%q, %d) line %d = %q is %d columns — it overflows the pane "+
					"it was supposed to fit, and the terminal will wrap it a second time",
					c.s, c.n, i, line, w)
			}
		}
	}
}

// TestWrapColsReopensColour 检查跨过折行点的颜色在两边都活了下来。什么都没
// 继承到的续行，只要有东西在这两行之间重绘了屏幕，颜色就没了。
func TestWrapColsReopensColour(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want []string
	}{
		{
			"colour opened before the break and closed after it",
			"\x1b[31mabcdef\x1b[0m", 3,
			[]string{"\x1b[31mabc\x1b[0m", "\x1b[31mdef\x1b[0m"},
		},
		{
			"colour opened mid-line, never closed by the source",
			"ab\x1b[32mcdef", 3,
			[]string{"ab\x1b[32mc\x1b[0m", "\x1b[32mdef\x1b[0m"},
		},
		{
			"two attributes both carry over",
			"\x1b[1m\x1b[4mabcd", 2,
			[]string{"\x1b[1m\x1b[4mab\x1b[0m", "\x1b[1m\x1b[4mcd\x1b[0m"},
		},
		{
			"colour closed before the break does not carry over",
			"\x1b[31mab\x1b[0mcdef", 3,
			[]string{"\x1b[31mab\x1b[0mc", "def"},
		},
		{
			"colour survives an embedded newline",
			"\x1b[35mab\ncd", 5,
			[]string{"\x1b[35mab\x1b[0m", "\x1b[35mcd\x1b[0m"},
		},
	}
	for _, c := range cases {
		got := wrapCols(c.s, c.n)
		if len(got) != len(c.want) {
			t.Errorf("wrapCols(%q, %d) = %q (%d lines), want %q (%d lines) — %s",
				c.s, c.n, got, len(got), c.want, len(c.want), c.name)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("wrapCols(%q, %d) line %d = %q, want %q — %s",
					c.s, c.n, i, got[i], c.want[i], c.name)
			}
			if w := dispWidth(got[i]); w > c.n {
				t.Errorf("wrapCols(%q, %d) line %d = %q measures %d columns; escape bytes "+
					"must not consume the pane's budget", c.s, c.n, i, got[i], w)
			}
		}
	}
}

// TestWrapColsNarrowerThanAWideRune 是一个**终止性**测试。
//
// n == 1 碰上一个 2 列宽的字符，这是无解的：这个 rune 在空行上也装不下，于
// 是"刷掉这行、重试这个 rune"这个自然写法会一直重试下去。wrapCols 的第一版
// 就挂在这里，而且是挂在重绘里面，所以症状是界面冻住，没有栈回溯，也没有
// 谁想得起去看的 CPU 尖峰。
//
// 选定的行为是让这个 rune 单独成行、溢出一列，而不是丢掉它：溢出的字形看
// 得见、查得出，被悄悄删掉的字符两样都不是。
func TestWrapColsNarrowerThanAWideRune(t *testing.T) {
	inputs := []string{"你好", "a你b", "你", "\x1b[31m你好\x1b[0m", "你\n好"}

	for _, s := range inputs {
		done := make(chan []string, 1)
		go func() { done <- wrapCols(s, 1) }()

		select {
		case got := <-done:
			// 每一个字符都必须还在。溢出是可以接受的妥协；把用户的文字弄丢
			// 了不是。
			var joined string
			for _, line := range got {
				joined += line
			}
			for _, r := range s {
				if r != '\n' && !strings.ContainsRune(joined, r) {
					t.Errorf("wrapCols(%q, 1) = %q dropped %q — wrapping may overflow a "+
						"one-column pane, but it may not delete text", s, got, r)
				}
			}
			if len(got) == 0 {
				t.Errorf("wrapCols(%q, 1) returned no lines at all", s)
			}
			// 退化路径不许在末尾硬安一个空行。
			if len(got) > 1 && got[len(got)-1] == "" && !strings.HasSuffix(s, "\n") {
				t.Errorf("wrapCols(%q, 1) = %q ends with a spurious empty line", s, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("wrapCols(%q, 1) did not return within 2s — a rune wider than the whole "+
				"pane made the wrap loop retry the same rune forever", s)
		}
	}
}

// ---------------------------------------------------------------------------
// 直接测这个转义序列扫描器
// ---------------------------------------------------------------------------

func TestAnsiLen(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want int
	}{
		{"not an escape", "abc", 0},
		{"SGR colour", "\x1b[31m", 5},
		{"SGR reset", "\x1b[0m", 4},
		{"multi-parameter SGR", "\x1b[38;5;204m", 11},
		{"erase line", "\x1b[2K", 4},
		{"cursor position", "\x1b[10;20H", 8},
		{"OSC with BEL: the URL is payload, not eight bytes of it", "\x1b]8;;https://x.dev\x07", 19},
		{"OSC with ST", "\x1b]0;title\x1b\\", 11},
		{"two-byte escape", "\x1bM", 2},
		{"lone trailing ESC", "\x1b", 1},
		{"unterminated CSI swallows the tail rather than printing it", "\x1b[31", 4},
	}
	for _, c := range cases {
		if got := ansiLen(c.s, 0); got != c.want {
			t.Errorf("ansiLen(%q, 0) = %d, want %d — %s", c.s, got, c.want, c.name)
		}
	}
}

func TestIsSGR(t *testing.T) {
	cases := []struct {
		seq         string
		sgr, reset  bool
		explanation string
	}{
		{"\x1b[31m", true, false, "a colour opens state that must be closed later"},
		{"\x1b[0m", true, true, "the canonical reset"},
		{"\x1b[m", true, true, "empty parameters mean reset"},
		{"\x1b[00m", true, true, "padded zero is still zero"},
		{"\x1b[0;0m", true, true, "all-zero parameter list is still a reset"},
		{"\x1b[1;31m", true, false, "bold plus colour"},
		{"\x1b[10m", true, false, "10 is not 0 — do not trim digits blindly"},
		{"\x1b[2K", false, false, "erase-line is an event, not a mode; it must not be re-emitted per wrapped line"},
		{"\x1b]8;;x\x07", false, false, "OSC is not SGR"},
		{"abc", false, false, "not an escape at all"},
	}
	for _, c := range cases {
		sgr, reset := isSGR(c.seq)
		if sgr != c.sgr || reset != c.reset {
			t.Errorf("isSGR(%q) = (sgr=%v, reset=%v), want (sgr=%v, reset=%v) — %s",
				c.seq, sgr, reset, c.sgr, c.reset, c.explanation)
		}
	}
}
