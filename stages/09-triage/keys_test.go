package main

import (
	"fmt"
	"testing"
)

// decCase 是一个"这些字节意味着这个键"的断言。
//
// Raw 永远不会写在一个 case 里：根据定义它是消耗的字节，
// 所以宿主派生它。那不仅仅是便利——它意味着每个
// case 都双重检查消耗计数，因为一个少一个字节的 Raw
// 和一个少一个字节的 n 是同一个 bug 被看到两次。
type decCase struct {
	name string
	in   string
	n    int // 消耗的字节；0 表示"全部"
	want key
}

func (c decCase) consumed() int {
	if c.n == 0 {
		return len(c.in)
	}
	return c.n
}

// runCases 根据**两个**入口点检查每个 case。这里
// 每个 case 都是一个完整的序列，decodeKeyFinal 必须在全部
// 情况下都与 decodeKey 一致：第二个入口点存在是为了解决
// **不完整的**输入，且不能在其他任何地方改变一个决定。
func runCases(t *testing.T, cases []decCase) {
	t.Helper()
	decoders := []struct {
		name string
		fn   func([]byte) (key, int, bool)
	}{
		{"decodeKey", decodeKey},
		{"decodeKeyFinal", decodeKeyFinal},
	}
	for _, c := range cases {
		for _, d := range decoders {
			t.Run(c.name+"/"+d.name, func(t *testing.T) {
				got, n, ok := d.fn([]byte(c.in))
				if !ok {
					t.Fatalf("%s(%q) asked for more bytes, but that is a complete sequence.\n"+
						"ok=false means 'this is a prefix'; returning it for finished input makes the "+
						"caller read forever waiting for bytes the terminal already sent.", d.name, c.in)
				}
				want := c.want
				want.Raw = c.in[:c.consumed()]
				if n != c.consumed() {
					t.Errorf("%s(%q) consumed %d bytes, want %d.\n"+
						"The caller advances the buffer by this number, so being off by one here means "+
						"the next key is decoded starting inside this one.", d.name, c.in, n, c.consumed())
				}
				if got != want {
					t.Errorf("%s(%q)\n got: %v\nwant: %v", d.name, c.in, got, want)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 普通文本
// ---------------------------------------------------------------------------

func TestDecodeRunes(t *testing.T) {
	runCases(t, []decCase{
		{name: "ascii letter", in: "a", want: key{Kind: keyRune, Rune: 'a'}},
		{name: "ascii capital", in: "Z", want: key{Kind: keyRune, Rune: 'Z'}},
		{name: "space", in: " ", want: key{Kind: keyRune, Rune: ' '}},
		{name: "tilde is text not a final byte", in: "~", want: key{Kind: keyRune, Rune: '~'}},
		// 多字节 rune 是这个解码器无法一次工作一个字节的原因。
		// 有人用中文打字，或者一个 emoji，会从两到四个字节产生
		// 一个按键事件。
		{name: "two-byte rune", in: "é", n: 2, want: key{Kind: keyRune, Rune: 'é'}},
		{name: "three-byte rune", in: "中", n: 3, want: key{Kind: keyRune, Rune: '中'}},
		{name: "four-byte rune", in: "😀", n: 4, want: key{Kind: keyRune, Rune: '😀'}},
		// 只有第一个键，只有它的字节。
		{name: "stops at the first rune", in: "ab", n: 1, want: key{Kind: keyRune, Rune: 'a'}},
		{name: "stops after a multi-byte rune", in: "中x", n: 3, want: key{Kind: keyRune, Rune: '中'}},
	})
}

func TestDecodeInvalidUTF8(t *testing.T) {
	// 一个永远不可能是 rune 开头的字节。它必须被丢弃，一次一个字节，绝不能变成
	// U+FFFD——这里的替换字符，就是一个用户没有敲出来、却出现在了他们文档里的
	// 字形。
	runCases(t, []decCase{
		{name: "0xff", in: "\xff", want: key{Kind: keyUnknown}},
		{name: "0xff then text", in: "\xffa", n: 1, want: key{Kind: keyUnknown}},
		{name: "stray continuation byte", in: "\xb8", want: key{Kind: keyUnknown}},
	})
}

func TestDecodeControlBytes(t *testing.T) {
	runCases(t, []decCase{
		{name: "CR is Enter", in: "\r", want: key{Kind: keyEnter}},
		{name: "LF is Enter", in: "\n", want: key{Kind: keyEnter}},
		{name: "Tab", in: "\t", want: key{Kind: keyTab}},
		{name: "DEL is Backspace", in: "\x7f", want: key{Kind: keyBackspace}},
		{name: "BS is Backspace", in: "\x08", want: key{Kind: keyBackspace}},
		{name: "Ctrl-C", in: "\x03", want: key{Kind: keyCtrlC}},
		{name: "Ctrl-D", in: "\x04", want: key{Kind: keyCtrlD}},
		{name: "Ctrl-L", in: "\x0c", want: key{Kind: keyCtrlL}},

		// 控制范围内的其他所有字节，都是 Ctrl-字母，会被报告成带 Ctrl 标志的字母，
		// 而不是单独作为它自己的 Kind。"每个绑定都加一个 Kind"，正是按键枚举膨胀
		// 到六十个条目的原因。
		{name: "Ctrl-A", in: "\x01", want: key{Kind: keyRune, Rune: 'a', Ctrl: true}},
		{name: "Ctrl-Z", in: "\x1a", want: key{Kind: keyRune, Rune: 'z', Ctrl: true}},
		{name: "Ctrl-Space is NUL", in: "\x00", want: key{Kind: keyRune, Rune: ' ', Ctrl: true}},
		{name: "Ctrl-underscore", in: "\x1f", want: key{Kind: keyRune, Rune: '_', Ctrl: true}},
		{name: "Ctrl-backslash", in: "\x1c", want: key{Kind: keyRune, Rune: '\\', Ctrl: true}},
	})
}

// ---------------------------------------------------------------------------
// 转义序列
// ---------------------------------------------------------------------------

func TestDecodeArrowsBothForms(t *testing.T) {
	// 这个表的 SS3 一半是重要的那个。一个只知道
	// CSI 箭头的解码器工作得很完美，直到有一天有人在
	// tmux 内部运行它，或通过 ssh 进入一个留下 DECCKM
	// 打开的 shell，然后箭头键开始打字母。
	runCases(t, []decCase{
		{name: "CSI up", in: "\x1b[A", want: key{Kind: keyUp}},
		{name: "CSI down", in: "\x1b[B", want: key{Kind: keyDown}},
		{name: "CSI right", in: "\x1b[C", want: key{Kind: keyRight}},
		{name: "CSI left", in: "\x1b[D", want: key{Kind: keyLeft}},
		{name: "SS3 up", in: "\x1bOA", want: key{Kind: keyUp}},
		{name: "SS3 down", in: "\x1bOB", want: key{Kind: keyDown}},
		{name: "SS3 right", in: "\x1bOC", want: key{Kind: keyRight}},
		{name: "SS3 left", in: "\x1bOD", want: key{Kind: keyLeft}},
	})
}

func TestDecodeHomeAndEndEveryForm(t *testing.T) {
	// 全部八种。四个系统从来没有一致过，也永远不会；
	// 见 decodeCSI 中的评论。删除这里的任何一行是
	// 删除对某个人的终端的支持。
	runCases(t, []decCase{
		{name: "xterm CSI H", in: "\x1b[H", want: key{Kind: keyHome}},
		{name: "xterm CSI F", in: "\x1b[F", want: key{Kind: keyEnd}},
		{name: "VT220 Find", in: "\x1b[1~", want: key{Kind: keyHome}},
		{name: "VT220 Select", in: "\x1b[4~", want: key{Kind: keyEnd}},
		{name: "rxvt home", in: "\x1b[7~", want: key{Kind: keyHome}},
		{name: "rxvt end", in: "\x1b[8~", want: key{Kind: keyEnd}},
		{name: "SS3 home", in: "\x1bOH", want: key{Kind: keyHome}},
		{name: "SS3 end", in: "\x1bOF", want: key{Kind: keyEnd}},
	})
}

func TestDecodeNavigationKeys(t *testing.T) {
	runCases(t, []decCase{
		{name: "PageUp", in: "\x1b[5~", want: key{Kind: keyPageUp}},
		{name: "PageDown", in: "\x1b[6~", want: key{Kind: keyPageDown}},
		{name: "Delete", in: "\x1b[3~", want: key{Kind: keyDelete}},
		// CSI Z 没有修饰符参数，所以标志保持清除：
		// Shift 在 Kind 里。
		{name: "Shift-Tab", in: "\x1b[Z", want: key{Kind: keyShiftTab}},
	})
}

func TestDecodeModifiedKeys(t *testing.T) {
	runCases(t, []decCase{
		{name: "Ctrl-Up", in: "\x1b[1;5A", want: key{Kind: keyUp, Ctrl: true}},
		{name: "Shift-Right", in: "\x1b[1;2C", want: key{Kind: keyRight, Shift: true}},
		{name: "Alt-Left", in: "\x1b[1;3D", want: key{Kind: keyLeft, Alt: true}},
		{name: "Ctrl-Alt-Down", in: "\x1b[1;7B", want: key{Kind: keyDown, Ctrl: true, Alt: true}},
		{name: "Ctrl-Home", in: "\x1b[1;5H", want: key{Kind: keyHome, Ctrl: true}},
		{name: "Ctrl-End", in: "\x1b[1;5F", want: key{Kind: keyEnd, Ctrl: true}},
		// 修饰符在波浪号结尾的同一位置。
		{name: "Ctrl-Delete", in: "\x1b[3;5~", want: key{Kind: keyDelete, Ctrl: true}},
		{name: "Shift-PageUp", in: "\x1b[5;2~", want: key{Kind: keyPageUp, Shift: true}},
		// 一个省略的第一个参数是合法的，意味着"默认"。
		{name: "omitted first param", in: "\x1b[;5A", want: key{Kind: keyUp, Ctrl: true}},
		// xterm modifyOtherKeys / kitty 会在冒号后附加子参数。基础键必须能在这个
		// 我们平时并不会说的协议里活下来。
		{name: "colon sub-parameter", in: "\x1b[1;5:3A", want: key{Kind: keyUp, Ctrl: true}},
	})
}

// TestModifierIsBitmaskPlusOne 直接固定编码，因为弄错它
// 不会破坏箭头——它悄悄地把每个修饰符移位一个位置，
// 这远比箭头键停止工作更难注意到。
func TestModifierIsBitmaskPlusOne(t *testing.T) {
	cases := []struct {
		mod   int
		shift bool
		alt   bool
		ctrl  bool
	}{
		{2, true, false, false},
		{3, false, true, false},
		{4, true, true, false},
		{5, false, false, true},
		{6, true, false, true},
		{7, false, true, true},
		{8, true, true, true},
	}
	for _, c := range cases {
		in := fmt.Sprintf("\x1b[1;%dA", c.mod)
		got, _, ok := decodeKey([]byte(in))
		if !ok {
			t.Fatalf("decodeKey(%q) wants more bytes", in)
		}
		if got.Shift != c.shift || got.Alt != c.alt || got.Ctrl != c.ctrl {
			t.Errorf("decodeKey(%q): shift=%v alt=%v ctrl=%v, want shift=%v alt=%v ctrl=%v.\n"+
				"The parameter is a bitmask PLUS ONE (1 = no modifiers), so it must be decremented "+
				"before masking: 1=shift, 2=alt, 4=ctrl.",
				in, got.Shift, got.Alt, got.Ctrl, c.shift, c.alt, c.ctrl)
		}
		if got.Kind != keyUp {
			t.Errorf("decodeKey(%q) = %v; a modifier must never change which key it is", in, got)
		}
	}
	// 未修饰的形式必须让每个标志保持清除。
	if got, _, _ := decodeKey([]byte("\x1b[1;1A")); got.Shift || got.Alt || got.Ctrl {
		t.Errorf("decodeKey(\"\\x1b[1;1A\") = %v; parameter 1 means no modifiers at all", got)
	}
}

func TestDecodeAltPrefixedKeys(t *testing.T) {
	// 终端实现 Alt 作为 ESC 前缀（"metaSendsEscape"），
	// 所以 Alt-a 和"Escape 然后 a"是相同的字节。
	// 在一个缓冲区内我们选择 Alt，这是每个编辑器做的。
	runCases(t, []decCase{
		{name: "Alt-a", in: "\x1ba", want: key{Kind: keyRune, Rune: 'a', Alt: true}},
		{name: "Alt-Enter", in: "\x1b\r", want: key{Kind: keyEnter, Alt: true}},
		{name: "Alt-Backspace", in: "\x1b\x7f", want: key{Kind: keyBackspace, Alt: true}},
		{name: "Alt-multibyte", in: "\x1b中", n: 4, want: key{Kind: keyRune, Rune: '中', Alt: true}},
		// ESC ESC [ A 是 tmux 为 Alt-Up 转发的。
		{name: "ESC ESC CSI is Alt-Up", in: "\x1b\x1b[A", want: key{Kind: keyUp, Alt: true}},
	})
}

func TestDecodeUnknownButWellFormed(t *testing.T) {
	// 所有这些检查的都是同一条规则：消耗整个序列，报告 keyUnknown，把字节保
	// 留在 Raw 里。不要因为一个东西只是无法识别，就返回"需要更多字节"——那是
	// 活锁，不是解析错误。
	runCases(t, []decCase{
		{name: "Insert", in: "\x1b[2~", want: key{Kind: keyUnknown}},
		{name: "F5", in: "\x1b[15~", want: key{Kind: keyUnknown}},
		{name: "SS3 F1", in: "\x1bOP", want: key{Kind: keyUnknown}},
		{name: "device attributes reply", in: "\x1b[?1;2c", want: key{Kind: keyUnknown}},
		{name: "cursor position report", in: "\x1b[24;80R", want: key{Kind: keyUnknown}},
		{name: "focus in", in: "\x1b[I", want: key{Kind: keyUnknown}},
		{name: "stray paste terminator", in: "\x1b[201~", want: key{Kind: keyUnknown}},
		// 参数和最终字节之间的中间字节（0x20-0x2f）：DECSCUSR，光标形状序列。这
		// 种字节在输入里很少见，唯一能防止它把整个流弄得不同步的，是解析器认的是
		// 字节的类别，而不是一份形状列表。
		{name: "intermediate byte", in: "\x1b[ q", want: key{Kind: keyUnknown}},
		{name: "unrecognised final byte", in: "\x1b[999X", want: key{Kind: keyUnknown}},
		// 一个在序列中间被注入的控制字节，会让这个序列中止。我们会一路消耗到控制
		// 字节之前为止，但不包括控制字节本身，所以当另一个写入者与鼠标报告的内容
		// 交错时，Ctrl-C 依然有效。
		{name: "control byte aborts CSI", in: "\x1b[1;\x03A", n: 4, want: key{Kind: keyUnknown}},
	})
}

// ---------------------------------------------------------------------------
// 鼠标
// ---------------------------------------------------------------------------

func TestDecodeSGRMouse(t *testing.T) {
	runCases(t, []decCase{
		{name: "left press", in: "\x1b[<0;10;20M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 10, Y: 20, Press: true}}},
		{name: "left release", in: "\x1b[<0;10;20m",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 10, Y: 20, Press: false}}},
		{name: "middle press", in: "\x1b[<1;1;1M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 1, X: 1, Y: 1, Press: true}}},
		{name: "right press", in: "\x1b[<2;3;4M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 2, X: 3, Y: 4, Press: true}}},
		{name: "right release", in: "\x1b[<2;3;4m",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 2, X: 3, Y: 4, Press: false}}},
		// 滚轮保留它的位：在这条线上，滚轮向上确实
		// 是按钮 64。它只报告一次按下；没有凹口释放。
		{name: "wheel up", in: "\x1b[<64;5;6M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 64, X: 5, Y: 6, Press: true}}},
		{name: "wheel down", in: "\x1b[<65;5;6M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 65, X: 5, Y: 6, Press: true}}},
		// 运动位 0x20 被设置：这是按住左键的拖拽。它必须报告按钮 0，而不是按钮 32。
		{name: "drag with left button", in: "\x1b[<32;7;8M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 7, Y: 8, Press: true}}},
		// 修饰符位移到键的标志上，按钮本身不受影响。
		{name: "ctrl-click", in: "\x1b[<16;9;9M",
			want: key{Kind: keyMouse, Ctrl: true, Mouse: mouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "shift-click", in: "\x1b[<4;9;9M",
			want: key{Kind: keyMouse, Shift: true, Mouse: mouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "alt-right-click", in: "\x1b[<10;2;2M",
			want: key{Kind: keyMouse, Alt: true, Mouse: mouseEvent{Button: 2, X: 2, Y: 2, Press: true}}},
		{name: "ctrl-wheel-up", in: "\x1b[<80;1;1M",
			want: key{Kind: keyMouse, Ctrl: true, Mouse: mouseEvent{Button: 64, X: 1, Y: 1, Press: true}}},

		// SGR 存在的整个理由。上一代的 X10 编码把坐标塞进一个字节里，用 32 做偏
		// 置，所以列数一旦超过 223 就没法表示——这在任何比 1980 年代终端更宽的窗
		// 口上，都是一个真实的限制。
		{name: "column past the X10 limit", in: "\x1b[<0;300;150M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 300, Y: 150, Press: true}}},
		{name: "release past the X10 limit", in: "\x1b[<2;1000;500m",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 2, X: 1000, Y: 500, Press: false}}},

		// 格式错误但作为 CSI 格式良好：消耗完整，报告未知。
		{name: "too few parameters", in: "\x1b[<0;10M", want: key{Kind: keyUnknown}},
	})
}

func TestX10MouseReportIsSwallowedWhole(t *testing.T) {
	// X10 鼠标（模式 1000，没有 1006）是 CSI M 加上三个不属于 CSI 序列本身的
	// **原始**字节。我们不会报告这次点击——编码没法表示 223 之后的列——但仍然
	// 必须吃掉这三个字节，否则它们会被解码成三个按键，敲进用户当时正在编辑的
	// 东西里。
	buf := []byte("\x1b[M\x20\x21\x22a")
	k, n, ok := decodeKey(buf)
	if !ok || n != 6 || k.Kind != keyUnknown {
		t.Fatalf("decodeKey(%q) = %v n=%d ok=%v; want keyUnknown n=6.\n"+
			"CSI M is followed by three raw payload bytes; consuming only the CSI part lets them "+
			"through as phantom input.", buf, k, n, ok)
	}
	next, _, ok := decodeKey(buf[n:])
	if !ok || next.Kind != keyRune || next.Rune != 'a' {
		t.Fatalf("after the X10 report the next key is %v; want the literal 'a' that followed it", next)
	}
	// 截断的有效载荷是一个真正的"需要更多字节"。
	if k, n, ok := decodeKey([]byte("\x1b[M\x20")); ok {
		t.Errorf("decodeKey on a truncated X10 report returned %v n=%d; the payload is 3 bytes and only 1 arrived", k, n)
	}
}

// ---------------------------------------------------------------------------
// 括号粘贴
// ---------------------------------------------------------------------------

func TestDecodePaste(t *testing.T) {
	runCases(t, []decCase{
		{name: "simple", in: "\x1b[200~hello\x1b[201~",
			want: key{Kind: keyPaste, Text: "hello"}},
		{name: "empty", in: "\x1b[200~\x1b[201~",
			want: key{Kind: keyPaste, Text: ""}},

		// 真正重要的那一条。包含转义序列的有效载荷，必须逐字节原样输出为文本——
		// **不是**被解码。把有效载荷重新送回解码器跑一遍，会把粘贴的 shell 脚本
		// 变成箭头键。
		{name: "payload contains an escape sequence", in: "\x1b[200~x\x1b[Ay\x1b[201~",
			want: key{Kind: keyPaste, Text: "x\x1b[Ay"}},
		{name: "payload is a bare ESC", in: "\x1b[200~\x1b\x1b[201~",
			want: key{Kind: keyPaste, Text: "\x1b"}},

		// 另一个真正重要的那一条。在提示符 UI 里，换行符本身就会触发提交，所以一
		// 个粘贴进来的换行符，一旦被当成 keyEnter 泄漏出去，就会把半段粘贴内容发
		// 给模型。这正是括号粘贴发明出来要阻止的事。
		{name: "payload contains newlines", in: "\x1b[200~line one\nline two\n\x1b[201~",
			want: key{Kind: keyPaste, Text: "line one\nline two\n"}},
		{name: "payload contains a carriage return", in: "\x1b[200~a\r\nb\x1b[201~",
			want: key{Kind: keyPaste, Text: "a\r\nb"}},
		{name: "payload contains control bytes", in: "\x1b[200~a\x03b\x1b[201~",
			want: key{Kind: keyPaste, Text: "a\x03b"}},
		{name: "payload contains multi-byte runes", in: "\x1b[200~中😀\x1b[201~",
			want: key{Kind: keyPaste, Text: "中😀"}},

		// 只精确消耗标记和有效载荷本身，此后一个字节都不多拿。
		{name: "stops at the terminator", in: "\x1b[200~a\x1b[201~b", n: 13,
			want: key{Kind: keyPaste, Text: "a"}},
	})
}

func TestUnterminatedPasteNeedsMoreBytesEvenWhenFinal(t *testing.T) {
	// 关于"decodeKeyFinal 总是有进展"这条规则，唯一有文档记录的例外。转义超
	// 时回答的是一个关于人类手指的问题；粘贴则是一台机器在以管道速度写入，要
	// 经过很多次读取才能到齐。粘贴过程中间出现的间隙，并不能证明粘贴已经结束，
	// 若就此认定结束，就会把前一半有效载荷当文本发出，再把剩下的部分解码成一
	// 个个按键。
	for _, in := range []string{
		"\x1b[200~",
		"\x1b[200~half a payl",
		"\x1b[200~payload with an \x1b in it",
		"\x1b[200~almost there\x1b[201", // 终止符本身跨读取分割
	} {
		for _, d := range []struct {
			name string
			fn   func([]byte) (key, int, bool)
		}{{"decodeKey", decodeKey}, {"decodeKeyFinal", decodeKeyFinal}} {
			if k, n, ok := d.fn([]byte(in)); ok {
				t.Errorf("%s(%q) = %v n=%d ok=true; want ok=false.\n"+
					"Emitting a partial paste is the worst available outcome: the remainder is then "+
					"decoded as individual keystrokes, and a newline in it submits a half-finished line.",
					d.name, in, k, n)
			} else if n != 0 {
				t.Errorf("%s(%q) returned ok=false but claimed %d bytes consumed; must be 0", d.name, in, n)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 歧义和流式契约
// ---------------------------------------------------------------------------

func TestLoneEscapeIsAmbiguous(t *testing.T) {
	// 这一对断言，记录了整个设计。同一个字节，两个不同的答案，区别不在字节本
	// 身——而在于调用者有没有告诉解码器：我已经等过了。
	buf := []byte{0x1b}

	k, n, ok := decodeKey(buf)
	if ok {
		t.Errorf("decodeKey(ESC) = %v n=%d ok=true; want ok=false.\n"+
			"A lone trailing ESC is either the Escape key or the first byte of an arrow key that has "+
			"not finished arriving, and nothing in the bytes can tell you which. Deciding here means "+
			"every arrow key over a slow link becomes Escape plus two garbage characters.", k, n)
	}
	if n != 0 {
		t.Errorf("decodeKey(ESC) returned ok=false but claimed %d bytes consumed; must be 0", n)
	}

	k, n, ok = decodeKeyFinal(buf)
	if !ok || n != 1 || k.Kind != keyEsc {
		t.Errorf("decodeKeyFinal(ESC) = %v n=%d ok=%v; want keyEsc n=1 ok=true.\n"+
			"Once the caller has waited out the escape timeout with nothing arriving, a lone ESC is "+
			"the Escape key. Resolving that is the entire reason this second entry point exists.", k, n, ok)
	}
}

func TestNeedsMoreBytes(t *testing.T) {
	// 这些全都是某个更长内容的前缀。decodeKey 必须说明这一点，并且不能消耗任
	// 何字节，这样调用者才能把新数据追加到同一个缓冲区里，再问一次。
	for _, in := range []string{
		"",           // 根本没有
		"\x1b",       // 歧义
		"\x1b[",      // 只有 CSI 引入符
		"\x1bO",      // 只有 SS3 引入符
		"\x1b[1",     // 参数，没有最终字节
		"\x1b[1;",    // 同上
		"\x1b[1;5",   // 同上
		"\x1b[<0;10", // 鼠标报告，中间参数
		"\x1b[<0;10;20",
		"\x1b[ ",   // 中间字节，没有最终字节
		"\x1b\x1b", // Alt-某，某还没到
		"\x1b[M",   // X10 报告，没有有效载荷
		"\x1b[M\x20\x21",
		"\xe4",         // 三字节 rune 的第一个字节
		"\xe4\xb8",     // 三字节 rune 的前两个字节
		"\xf0\x9f\x98", // 四字节 rune 的前三个字节
	} {
		k, n, ok := decodeKey([]byte(in))
		if ok {
			t.Errorf("decodeKey(%q) = %v n=%d ok=true; want ok=false.\n"+
				"That input is a prefix. Answering it produces a keystroke the user never made, and "+
				"leaves the rest of the real sequence to be decoded as garbage.", in, k, n)
		}
		if n != 0 {
			t.Errorf("decodeKey(%q) returned ok=false but claimed %d bytes consumed; must be 0 so the "+
				"caller can append and retry against the same buffer", in, n)
		}
	}
}

func TestTruncatedRuneIsNotAReplacementCharacter(t *testing.T) {
	// 之所以要单独拎出来说，是因为这个坑太容易踩：对于一个被截断的 rune，
	// utf8.DecodeRune 返回的 (RuneError, 1) 跟处理一个真正无效的 rune 时一模一
	// 样，于是照直觉写的代码，会对一个只是刚好跨在读取边界上、完好无损的字符
	// 发出 U+FFFD。而这一发，证据就被销毁了——剩下的字节只会解码出更多垃圾。
	for _, in := range []string{"\xe4", "\xe4\xb8", "\xf0", "\xf0\x9f", "\xf0\x9f\x98"} {
		k, n, ok := decodeKey([]byte(in))
		if ok {
			t.Errorf("decodeKey(%q) = %v n=%d; want ok=false. A truncated rune needs more bytes, "+
				"not a replacement character.", in, k, n)
		}
	}
	// 同样的字节，一旦完成，就是一个键。
	if k, n, ok := decodeKey([]byte("中")); !ok || n != 3 || k.Rune != '中' {
		t.Errorf("decodeKey(\"中\") = %v n=%d ok=%v; want the rune, 3 bytes", k, n, ok)
	}
}

func TestDecodeOneByteAtATime(t *testing.T) {
	// 这是解码器真正被使用的方式。一个 tty 上的读取
	// 返回那个时刻缓冲区中碰巧出现的任何字节，
	// 在负载下或通过 ssh 这经常是一个字节。
	// 每个适当的前缀必须报告"更多"，
	// 最后一个字节必须完成键。
	for _, s := range []struct{ name, in string }{
		{"CSI arrow", "\x1b[A"},
		{"SS3 arrow", "\x1bOA"},
		{"Ctrl-Up", "\x1b[1;5A"},
		{"Delete", "\x1b[3~"},
		{"Home", "\x1b[7~"},
		{"Shift-Tab", "\x1b[Z"},
		{"SGR mouse", "\x1b[<0;300;150M"},
		{"paste", "\x1b[200~hi there\x1b[201~"},
		{"paste containing ESC", "\x1b[200~a\x1b[Ab\x1b[201~"},
		{"three-byte rune", "中"},
		{"four-byte rune", "😀"},
		{"Alt-Up via ESC ESC", "\x1b\x1b[A"},
	} {
		t.Run(s.name, func(t *testing.T) {
			full := []byte(s.in)
			for i := 1; i < len(full); i++ {
				k, n, ok := decodeKey(full[:i])
				if ok {
					t.Fatalf("decodeKey(%q) — the first %d of %d bytes — returned %v (n=%d).\n"+
						"That is a proper prefix of %q, so answering it invents a keystroke and leaves "+
						"the remaining bytes to be decoded as garbage.", full[:i], i, len(full), k, n, s.in)
				}
				if n != 0 {
					t.Fatalf("decodeKey(%q) returned ok=false but consumed %d bytes", full[:i], n)
				}
			}
			_, n, ok := decodeKey(full)
			if !ok || n != len(full) {
				t.Fatalf("decodeKey(%q) on the complete sequence: ok=%v n=%d; want ok=true n=%d",
					s.in, ok, n, len(full))
			}
		})
	}
}

func TestConsumedCountsTileTheBuffer(t *testing.T) {
	// 一次读取经常携带多个键——按键重复、快速打字，或者一次鼠标拖拽。调用者按
	// n 前进，再解码一次，所以这些计数加起来必须正好等于缓冲区长度。任何地方
	// 出现一个差一错误，都会让后面每一个键都错开一个字节，而这不会大张旗鼓地
	// 出错：它只会产生一堆看起来合理、实则错误的键。
	buf := []byte("\x1b[1;5A" + "中" + "\x1b[<2;10;20m" + "\x1b[200~hi\x1b[201~" + "x" + "\x1b[3~")
	want := []struct {
		kind keyKind
		n    int
	}{
		{keyUp, 6},
		{keyRune, 3},
		{keyMouse, 11},
		{keyPaste, 14},
		{keyRune, 1},
		{keyDelete, 4},
	}

	pos := 0
	for i, w := range want {
		k, n, ok := decodeKey(buf[pos:])
		if !ok {
			t.Fatalf("key %d at offset %d: decodeKey(%q) wants more bytes, but a whole key is there",
				i, pos, buf[pos:])
		}
		if k.Kind != w.kind || n != w.n {
			t.Fatalf("key %d at offset %d: got %v consuming %d bytes; want %v consuming %d",
				i, pos, k, n, w.kind, w.n)
		}
		pos += n
	}
	if pos != len(buf) {
		t.Fatalf("the keys consumed %d of %d bytes. The counts must tile the buffer exactly; %d bytes "+
			"unaccounted for means the caller is about to decode from the middle of a sequence.",
			pos, len(buf), len(buf)-pos)
	}
	if _, _, ok := decodeKey(buf[pos:]); ok {
		t.Errorf("decodeKey on the empty remainder returned a key")
	}
}

func TestDecodeKeyFinalAlwaysMakesProgress(t *testing.T) {
	// decodeKeyFinal 是调用者的最后手段：读取已经超时，也没有别的办法可试。
	// 如果对一段永远不会再增长的输入，它还能回答"需要更多字节"，调用者就会永
	// 远空转下去。所以只要缓冲区非空，它就必须至少消耗一个字节——哪怕是它没法
	// 理解的片段，也会被报告成 keyUnknown，而不是被凭空捏造成一个按键。（括
	// 号粘贴是唯一一个故意的例外；见
	// TestUnterminatedPasteNeedsMoreBytesEvenWhenFinal。）
	for _, s := range []string{
		"\x1b", "\x1b[", "\x1bO", "\x1b[1;5", "\x1b[<0;10;20", "\x1b[ ",
		"\x1b[M", "\x1b[M\x20\x21", "\x1b\x1b", "\x1b\x1b[",
		"\xe4", "\xe4\xb8", "\xf0\x9f\x98", "\xff", "a", "\x1b[A", "中",
	} {
		for i := 1; i <= len(s); i++ {
			frag := s[:i]
			k, n, ok := decodeKeyFinal([]byte(frag))
			if !ok {
				t.Errorf("decodeKeyFinal(%q) returned ok=false. No more bytes are coming, so there is "+
					"nothing the caller can do with that answer except ask again — forever.", frag)
				continue
			}
			if n <= 0 || n > len(frag) {
				t.Errorf("decodeKeyFinal(%q) = %v consuming %d bytes; must consume between 1 and %d",
					frag, k, n, len(frag))
			}
		}
	}
	// 以及退化的情况：一个空缓冲区里没有键，也不消耗任何字节。
	if k, n, ok := decodeKeyFinal(nil); ok || n != 0 {
		t.Errorf("decodeKeyFinal(nil) = %v n=%d ok=%v; want no key, 0 bytes", k, n, ok)
	}
}

func TestRawAlwaysHoldsTheConsumedBytes(t *testing.T) {
	// Raw 是调试时的便利：当一个未知序列出现在日志里，它是唯一能识别出这个序
	// 列的东西。对于每一种键，它都必须精确地等于被消耗的字节数——包括那些通过
	// Alt 前缀递归组装出来的键，这也是最容易出错的地方。
	for _, in := range []string{
		"a", "中", "\x03", "\x1b[A", "\x1bOA", "\x1b[1;5A", "\x1b[<0;10;20M",
		"\x1b[200~hi\x1b[201~", "\x1ba", "\x1b\x1b[A", "\x1b[999X", "\xff",
	} {
		k, n, ok := decodeKeyFinal([]byte(in))
		if !ok {
			t.Fatalf("decodeKeyFinal(%q) wants more bytes", in)
		}
		if k.Raw != in[:n] {
			t.Errorf("decodeKeyFinal(%q) consumed %d bytes but Raw = %q; want %q.\n"+
				"Raw is what a bug report is made of; it has to be the literal bytes consumed.",
				in, n, k.Raw, in[:n])
		}
	}
}
