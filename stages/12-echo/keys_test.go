package main

import (
	"fmt"
	"testing"
)

// decCase 是一条断言："这几个字节代表这个键"。
//
// Raw 从不写进用例：它按定义就是被吃掉的那些字节，所以由测试宿主自己推
// 出来。这不只是图省事——它让每条用例都顺带复核了消耗字节数，因为 Raw
// 少一个字节和 n 少一个字节，本来就是同一个 bug 从两个角度看。
type decCase struct {
	name string
	in   string
	n    int // 消耗的字节数；0 表示"全部"
	want key
}

func (c decCase) consumed() int {
	if c.n == 0 {
		return len(c.in)
	}
	return c.n
}

// runCases 拿**两个**入口分别过一遍每条用例。这里的用例都是完整序列，
// decodeKeyFinal 必须和 decodeKey 给出一样的答案：第二个入口是为了裁定
// **不完整**的输入才存在的，在别的地方一个决定都不许改。
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
		// 多字节 rune 就是这个解码器不能一个字节一个字节干活的原因。有人
		// 打中文，或者打个 emoji，两到四个字节才凑出一次按键事件。
		{name: "two-byte rune", in: "é", n: 2, want: key{Kind: keyRune, Rune: 'é'}},
		{name: "three-byte rune", in: "中", n: 3, want: key{Kind: keyRune, Rune: '中'}},
		{name: "four-byte rune", in: "😀", n: 4, want: key{Kind: keyRune, Rune: '😀'}},
		// 只取第一个键，也只取它那几个字节。
		{name: "stops at the first rune", in: "ab", n: 1, want: key{Kind: keyRune, Rune: 'a'}},
		{name: "stops after a multi-byte rune", in: "中x", n: 3, want: key{Kind: keyRune, Rune: '中'}},
	})
}

func TestDecodeInvalidUTF8(t *testing.T) {
	// 永远不可能作为 rune 开头的字节。必须把它丢掉，一次丢一个，而且绝
	// 不能变成 U+FFFD：这里冒出个替换字符，等于用户没打过的字形跑进了他
	// 的文档。
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

		// 控制字符区里剩下的都算 Ctrl-字母，报的是那个字母加上 Ctrl 标志，
		// 而不是各占一个 Kind。一个绑定加一个 Kind，键枚举就是这么涨到六十
		// 项的。
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
	// 这张表里 SS3 那一半才是要紧的。只认 CSI 方向键的解码器一直好好的，
	// 直到哪天有人在 tmux 里跑它，或者 ssh 进某个开着 DECCKM 的 shell，
	// 方向键当场开始往外打字母。
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
	// 八种全在。四条血脉从没谈拢过，以后也不会；见 decodeCSI 里的注释。
	// 删掉这里任何一行，就是删掉某个人终端的支持。
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
		// CSI Z 没有修饰键参数，所以标志位保持干净：shift 在 Kind 里。
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
		// 以波浪号结尾的序列，修饰键也在同一个位置上。
		{name: "Ctrl-Delete", in: "\x1b[3;5~", want: key{Kind: keyDelete, Ctrl: true}},
		{name: "Shift-PageUp", in: "\x1b[5;2~", want: key{Kind: keyPageUp, Shift: true}},
		// 第一个参数省略是合法的，意思是"取默认值"。
		{name: "omitted first param", in: "\x1b[;5A", want: key{Kind: keyUp, Ctrl: true}},
		// xterm 的 modifyOtherKeys 和 kitty 会在冒号后面追加子参数。我们并
		// 不说这套协议，但基础键必须活下来。
		{name: "colon sub-parameter", in: "\x1b[1;5:3A", want: key{Kind: keyUp, Ctrl: true}},
	})
}

// TestModifierIsBitmaskPlusOne 直接把这个编码钉死，因为搞错了并不会让
// 方向键坏掉——它只会不声不响地把每个修饰键都错开一位，而这比方向键彻
// 底失灵难发现得多。
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
	// 不带修饰键的形式，必须让每个标志位都保持干净。
	if got, _, _ := decodeKey([]byte("\x1b[1;1A")); got.Shift || got.Alt || got.Ctrl {
		t.Errorf("decodeKey(\"\\x1b[1;1A\") = %v; parameter 1 means no modifiers at all", got)
	}
}

func TestDecodeAltPrefixedKeys(t *testing.T) {
	// 终端把 Alt 实现成 ESC 前缀（"metaSendsEscape"），所以 Alt-a 和"先
	// Escape 再 a"是同样的字节。在同一个缓冲区里我们选 Alt，所有编辑器
	// 都是这么干的。
	runCases(t, []decCase{
		{name: "Alt-a", in: "\x1ba", want: key{Kind: keyRune, Rune: 'a', Alt: true}},
		{name: "Alt-Enter", in: "\x1b\r", want: key{Kind: keyEnter, Alt: true}},
		{name: "Alt-Backspace", in: "\x1b\x7f", want: key{Kind: keyBackspace, Alt: true}},
		{name: "Alt-multibyte", in: "\x1b中", n: 4, want: key{Kind: keyRune, Rune: '中', Alt: true}},
		// Alt-Up 经 tmux 转发出来就是 ESC ESC [ A。
		{name: "ESC ESC CSI is Alt-Up", in: "\x1b\x1b[A", want: key{Kind: keyUp, Alt: true}},
	})
}

func TestDecodeUnknownButWellFormed(t *testing.T) {
	// 这些用例查的是同一条规矩：整段序列吃掉，报 keyUnknown，字节留在
	// Raw 里。只是认不出来的东西，绝不能回"还要更多字节"——那是活锁，不
	// 是解析错误。
	runCases(t, []decCase{
		{name: "Insert", in: "\x1b[2~", want: key{Kind: keyUnknown}},
		{name: "F5", in: "\x1b[15~", want: key{Kind: keyUnknown}},
		{name: "SS3 F1", in: "\x1bOP", want: key{Kind: keyUnknown}},
		{name: "device attributes reply", in: "\x1b[?1;2c", want: key{Kind: keyUnknown}},
		{name: "cursor position report", in: "\x1b[24;80R", want: key{Kind: keyUnknown}},
		{name: "focus in", in: "\x1b[I", want: key{Kind: keyUnknown}},
		{name: "stray paste terminator", in: "\x1b[201~", want: key{Kind: keyUnknown}},
		// 参数和结尾字节之间夹了个中间字节（0x20-0x2f）：DECSCUSR，改光标
		// 形状的序列。输入里很少见，而它没把字节流搞失步，靠的是解析器认
		// 的是字节类别，不是一张形状清单。
		{name: "intermediate byte", in: "\x1b[ q", want: key{Kind: keyUnknown}},
		{name: "unrecognised final byte", in: "\x1b[999X", want: key{Kind: keyUnknown}},
		// 序列中间插进来一个控制字节，这段序列就作废。我们吃到控制字节为
		// 止、不含它，这样当另一个往 tty 写东西的进程和鼠标上报交错时，
		// Ctrl-C 照样管用。
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
		// 滚轮保留它那个位：在这套线上格式里，滚轮上滚**就是**按钮 64。它
		// 只报按下；滚一格没有对应的松开。
		{name: "wheel up", in: "\x1b[<64;5;6M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 64, X: 5, Y: 6, Press: true}}},
		{name: "wheel down", in: "\x1b[<65;5;6M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 65, X: 5, Y: 6, Press: true}}},
		// 带上移动位 0x20：按着左键在拖。它必须报按钮 0，不是按钮 32。
		{name: "drag with left button", in: "\x1b[<32;7;8M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 7, Y: 8, Press: true}}},
		// 修饰键的位挪到 key 的标志位上去，按钮本身不动。
		{name: "ctrl-click", in: "\x1b[<16;9;9M",
			want: key{Kind: keyMouse, Ctrl: true, Mouse: mouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "shift-click", in: "\x1b[<4;9;9M",
			want: key{Kind: keyMouse, Shift: true, Mouse: mouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "alt-right-click", in: "\x1b[<10;2;2M",
			want: key{Kind: keyMouse, Alt: true, Mouse: mouseEvent{Button: 2, X: 2, Y: 2, Press: true}}},
		{name: "ctrl-wheel-up", in: "\x1b[<80;1;1M",
			want: key{Kind: keyMouse, Ctrl: true, Mouse: mouseEvent{Button: 64, X: 1, Y: 1, Press: true}}},

		// SGR 存在的全部理由。老的 X10 编码把坐标塞进一个字节里、再偏移
		// 32，于是它说不出 223 以后的列号——只要窗口比 1980 年代的终端宽，
		// 这就是个真会撞上的限制。
		{name: "column past the X10 limit", in: "\x1b[<0;300;150M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 300, Y: 150, Press: true}}},
		{name: "release past the X10 limit", in: "\x1b[<2;1000;500m",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 2, X: 1000, Y: 500, Press: false}}},

		// 格式不对，但作为 CSI 是完整的：整段吃掉，报 unknown。
		{name: "too few parameters", in: "\x1b[<0;10M", want: key{Kind: keyUnknown}},
	})
}

func TestX10MouseReportIsSwallowedWhole(t *testing.T) {
	// X10 鼠标（开了 1000 没开 1006）是 CSI M 再加三个**裸**字节，那三个
	// 字节不属于 CSI 序列。这次点击我们不上报——这套编码说不出 223 以后
	// 的列号——但那三个字节还是得吃掉，否则它们会被解成三次按键，打进用
	// 户当时正在编辑的东西里。
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
	// 载荷被截断了，那是货真价实的"还要更多字节"。
	if k, n, ok := decodeKey([]byte("\x1b[M\x20")); ok {
		t.Errorf("decodeKey on a truncated X10 report returned %v n=%d; the payload is 3 bytes and only 1 arrived", k, n)
	}
}

// ---------------------------------------------------------------------------
// 括号粘贴模式
// ---------------------------------------------------------------------------

func TestDecodePaste(t *testing.T) {
	runCases(t, []decCase{
		{name: "simple", in: "\x1b[200~hello\x1b[201~",
			want: key{Kind: keyPaste, Text: "hello"}},
		{name: "empty", in: "\x1b[200~\x1b[201~",
			want: key{Kind: keyPaste, Text: ""}},

		// 要紧的就是这条。载荷里含转义序列时，必须逐字节原样当文本吐出来
		// ——**不许**解码。把载荷再过一遍解码器，粘进来的 shell 脚本就变成
		// 一串方向键。
		{name: "payload contains an escape sequence", in: "\x1b[200~x\x1b[Ay\x1b[201~",
			want: key{Kind: keyPaste, Text: "x\x1b[Ay"}},
		{name: "payload is a bare ESC", in: "\x1b[200~\x1b\x1b[201~",
			want: key{Kind: keyPaste, Text: "\x1b"}},

		// 另一条要紧的。在 prompt 界面里换行就是提交，所以粘贴内容里的换
		// 行一旦漏成 keyEnter，半段粘贴就发给模型了。括号粘贴模式当初被发
		// 明出来，防的就是这个。
		{name: "payload contains newlines", in: "\x1b[200~line one\nline two\n\x1b[201~",
			want: key{Kind: keyPaste, Text: "line one\nline two\n"}},
		{name: "payload contains a carriage return", in: "\x1b[200~a\r\nb\x1b[201~",
			want: key{Kind: keyPaste, Text: "a\r\nb"}},
		{name: "payload contains control bytes", in: "\x1b[200~a\x03b\x1b[201~",
			want: key{Kind: keyPaste, Text: "a\x03b"}},
		{name: "payload contains multi-byte runes", in: "\x1b[200~中😀\x1b[201~",
			want: key{Kind: keyPaste, Text: "中😀"}},

		// 只吃标记和载荷，一个字节都不多吃。
		{name: "stops at the terminator", in: "\x1b[200~a\x1b[201~b", n: 13,
			want: key{Kind: keyPaste, Text: "a"}},
	})
}

func TestUnterminatedPasteNeedsMoreBytesEvenWhenFinal(t *testing.T) {
	// "decodeKeyFinal 总能往前走"这条规矩，明面上就这一个例外。转义超
	// 时回答的是人的手指有多快；而粘贴是机器在按管道速度往里写，要分好
	// 多次读才到齐。粘到一半出现停顿，并不能说明粘贴结束了，硬要裁定就
	// 会把半截载荷当文本吐出去，剩下的当按键解掉。
	for _, in := range []string{
		"\x1b[200~",
		"\x1b[200~half a payl",
		"\x1b[200~payload with an \x1b in it",
		"\x1b[200~almost there\x1b[201", // 结束标记本身被拆到了两次读里
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
// 那处歧义，和流式的约定
// ---------------------------------------------------------------------------

func TestLoneEscapeIsAmbiguous(t *testing.T) {
	// 这一对断言就是整个设计的文档。同样一个字节，两种不同的答案，而区
	// 别不在字节里——是调用方告诉解码器：我已经等过了。
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
	// 这里每一条都是某个更长东西的前缀。decodeKey 必须说出这一点，而且
	// 一个字节都不能消耗，好让调用方往同一个缓冲区里续上再问一遍。
	for _, in := range []string{
		"",           // 什么都没有
		"\x1b",       // 那处歧义
		"\x1b[",      // 只有 CSI 引导符
		"\x1bO",      // 只有 SS3 引导符
		"\x1b[1",     // 有参数，没有结尾字节
		"\x1b[1;",    // 同上
		"\x1b[1;5",   // 同上
		"\x1b[<0;10", // 鼠标上报，参数还没完
		"\x1b[<0;10;20",
		"\x1b[ ",   // 有中间字节，没有结尾字节
		"\x1b\x1b", // Alt-什么，那个"什么"还没到
		"\x1b[M",   // X10 上报，载荷一个字节都没到
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
	// 单拎出来说，是因为这个坑实在太好踩：utf8.DecodeRune 碰上截断的
	// rune，返回的和碰上非法字节一样，都是 (RuneError, 1)，于是顺手写出
	// 来的代码会吐出 U+FFFD——而那个字符完全正常，只是刚好跨了读边界。
	// 证据到这一步就毁了——剩下的字节会解出更多垃圾。
	for _, in := range []string{"\xe4", "\xe4\xb8", "\xf0", "\xf0\x9f", "\xf0\x9f\x98"} {
		k, n, ok := decodeKey([]byte(in))
		if ok {
			t.Errorf("decodeKey(%q) = %v n=%d; want ok=false. A truncated rune needs more bytes, "+
				"not a replacement character.", in, k, n)
		}
	}
	// 同样这些字节，一旦完整，就是一个键。
	if k, n, ok := decodeKey([]byte("中")); !ok || n != 3 || k.Rune != '中' {
		t.Errorf("decodeKey(\"中\") = %v n=%d ok=%v; want the rune, 3 bytes", k, n, ok)
	}
}

func TestDecodeOneByteAtATime(t *testing.T) {
	// 解码器在现实里就是这么用的。在 tty 上读一次，拿到的是那一瞬间缓冲
	// 区里碰巧有的字节；负载一高，或者走 ssh，一次一个字节是常态。每个
	// 真前缀都必须报"还要"，而最后一个字节必须把这个键补完。
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
	// 一次读常常带着好几个键——按键重复、手快的人、或者一次鼠标拖动。调
	// 用方按 n 往前挪再解一次，所以这些计数加起来必须正好等于缓冲区长度。
	// 任何一处差一，后面每个键都会错开一个字节，而且不会大声报错：它给
	// 出的是一批看着挺像样的错键。
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
	// decodeKeyFinal 是调用方最后的手段：读超时到了，再没别的可试。要是
	// 它对着一段永远不会再长的输入还能回"还要更多字节"，调用方就会空转
	// 到死。所以只要缓冲区非空，它至少得消耗一个字节——包括那些它读不懂
	// 的碎片，这种它报 keyUnknown，而不是替用户编一次按键出来。（括号粘
	// 贴模式是唯一一个有意为之的例外，见
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
	// 还有退化情形：空缓冲区里没有键，也不消耗任何字节。
	if k, n, ok := decodeKeyFinal(nil); ok || n != 0 {
		t.Errorf("decodeKeyFinal(nil) = %v n=%d ok=%v; want no key, 0 bytes", k, n, ok)
	}
}

func TestRawAlwaysHoldsTheConsumedBytes(t *testing.T) {
	// Raw 是留给调试的抓手：日志里冒出一段认不出的序列时，只有它能指认
	// 那是什么。对每一类键，它都必须严格等于被消耗的那些字节——包括经
	// Alt 前缀递归拼出来的那些，那里最容易出错。
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
