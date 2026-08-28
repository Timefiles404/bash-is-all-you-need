package term

import (
	"fmt"
	"testing"
)

// decCase is one "these bytes mean this key" assertion.
//
// Raw is never written out in a case: it is by definition the bytes consumed, so
// the harness derives it. Every case therefore double-checks the consumed count,
// because a Raw one byte short and an n one byte short are the same bug seen
// twice.
type decCase struct {
	name string
	in   string
	n    int // bytes consumed; 0 means "all of them"
	want Key
}

func (c decCase) consumed() int {
	if c.n == 0 {
		return len(c.in)
	}
	return c.n
}

// runCases checks each case against BOTH entry points. Every case here is a
// complete sequence, and DecodeKeyFinal must agree with DecodeKey on all of
// them: the second entry point exists to resolve *incomplete* input and must not
// change a single decision anywhere else.
func runCases(t *testing.T, cases []decCase) {
	t.Helper()
	decoders := []struct {
		name string
		fn   func([]byte) (Key, int, bool)
	}{
		{"DecodeKey", DecodeKey},
		{"DecodeKeyFinal", DecodeKeyFinal},
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

func TestDecodeRunes(t *testing.T) {
	runCases(t, []decCase{
		{name: "ascii letter", in: "a", want: Key{Kind: KeyRune, Rune: 'a'}},
		{name: "ascii capital", in: "Z", want: Key{Kind: KeyRune, Rune: 'Z'}},
		{name: "space", in: " ", want: Key{Kind: KeyRune, Rune: ' '}},
		{name: "tilde is text not a final byte", in: "~", want: Key{Kind: KeyRune, Rune: '~'}},
		// Multi-byte runes are why this decoder cannot work a byte at a time.
		{name: "two-byte rune", in: "é", n: 2, want: Key{Kind: KeyRune, Rune: 'é'}},
		{name: "three-byte rune", in: "中", n: 3, want: Key{Kind: KeyRune, Rune: '中'}},
		{name: "four-byte rune", in: "😀", n: 4, want: Key{Kind: KeyRune, Rune: '😀'}},
		// Only the first key, and only its bytes.
		{name: "stops at the first rune", in: "ab", n: 1, want: Key{Kind: KeyRune, Rune: 'a'}},
		{name: "stops after a multi-byte rune", in: "中x", n: 3, want: Key{Kind: KeyRune, Rune: '中'}},
	})
}

func TestDecodeInvalidUTF8(t *testing.T) {
	// A byte that can never start a rune must be dropped one byte at a time and
	// never turned into U+FFFD: a replacement character here is a glyph the user
	// did not type appearing in their document.
	runCases(t, []decCase{
		{name: "0xff", in: "\xff", want: Key{Kind: KeyUnknown}},
		{name: "0xff then text", in: "\xffa", n: 1, want: Key{Kind: KeyUnknown}},
		{name: "stray continuation byte", in: "\xb8", want: Key{Kind: KeyUnknown}},
	})
}

func TestDecodeControlBytes(t *testing.T) {
	runCases(t, []decCase{
		{name: "CR is Enter", in: "\r", want: Key{Kind: KeyEnter}},
		{name: "LF is Enter", in: "\n", want: Key{Kind: KeyEnter}},
		{name: "Tab", in: "\t", want: Key{Kind: KeyTab}},
		{name: "DEL is Backspace", in: "\x7f", want: Key{Kind: KeyBackspace}},
		{name: "BS is Backspace", in: "\x08", want: Key{Kind: KeyBackspace}},
		{name: "Ctrl-C", in: "\x03", want: Key{Kind: KeyCtrlC}},
		{name: "Ctrl-D", in: "\x04", want: Key{Kind: KeyCtrlD}},
		{name: "Ctrl-L", in: "\x0c", want: Key{Kind: KeyCtrlL}},

		// Everything else in the control range is a Ctrl-letter, reported as the
		// letter with Ctrl set rather than as its own Kind. Adding a Kind per
		// binding is how a key enum reaches sixty entries.
		{name: "Ctrl-A", in: "\x01", want: Key{Kind: KeyRune, Rune: 'a', Ctrl: true}},
		{name: "Ctrl-Z", in: "\x1a", want: Key{Kind: KeyRune, Rune: 'z', Ctrl: true}},
		{name: "Ctrl-Space is NUL", in: "\x00", want: Key{Kind: KeyRune, Rune: ' ', Ctrl: true}},
		{name: "Ctrl-underscore", in: "\x1f", want: Key{Kind: KeyRune, Rune: '_', Ctrl: true}},
		{name: "Ctrl-backslash", in: "\x1c", want: Key{Kind: KeyRune, Rune: '\\', Ctrl: true}},
	})
}

func TestDecodeArrowsBothForms(t *testing.T) {
	// The SS3 half is the one that matters. A decoder that knows only CSI arrows
	// works until someone runs it inside tmux or over ssh into a shell that left
	// DECCKM on, and then the arrow keys type letters.
	runCases(t, []decCase{
		{name: "CSI up", in: "\x1b[A", want: Key{Kind: KeyUp}},
		{name: "CSI down", in: "\x1b[B", want: Key{Kind: KeyDown}},
		{name: "CSI right", in: "\x1b[C", want: Key{Kind: KeyRight}},
		{name: "CSI left", in: "\x1b[D", want: Key{Kind: KeyLeft}},
		{name: "SS3 up", in: "\x1bOA", want: Key{Kind: KeyUp}},
		{name: "SS3 down", in: "\x1bOB", want: Key{Kind: KeyDown}},
		{name: "SS3 right", in: "\x1bOC", want: Key{Kind: KeyRight}},
		{name: "SS3 left", in: "\x1bOD", want: Key{Kind: KeyLeft}},
	})
}

func TestDecodeHomeAndEndEveryForm(t *testing.T) {
	// All eight. Four lineages that never agreed and never will; see decodeCSI.
	// Deleting any row here deletes support for somebody's terminal.
	runCases(t, []decCase{
		{name: "xterm CSI H", in: "\x1b[H", want: Key{Kind: KeyHome}},
		{name: "xterm CSI F", in: "\x1b[F", want: Key{Kind: KeyEnd}},
		{name: "VT220 Find", in: "\x1b[1~", want: Key{Kind: KeyHome}},
		{name: "VT220 Select", in: "\x1b[4~", want: Key{Kind: KeyEnd}},
		{name: "rxvt home", in: "\x1b[7~", want: Key{Kind: KeyHome}},
		{name: "rxvt end", in: "\x1b[8~", want: Key{Kind: KeyEnd}},
		{name: "SS3 home", in: "\x1bOH", want: Key{Kind: KeyHome}},
		{name: "SS3 end", in: "\x1bOF", want: Key{Kind: KeyEnd}},
	})
}

func TestDecodeNavigationKeys(t *testing.T) {
	runCases(t, []decCase{
		{name: "PageUp", in: "\x1b[5~", want: Key{Kind: KeyPageUp}},
		{name: "PageDown", in: "\x1b[6~", want: Key{Kind: KeyPageDown}},
		{name: "Delete", in: "\x1b[3~", want: Key{Kind: KeyDelete}},
		// CSI Z has no modifier parameter, so the flags stay clear: the shift is
		// in the Kind.
		{name: "Shift-Tab", in: "\x1b[Z", want: Key{Kind: KeyShiftTab}},
	})
}

func TestDecodeModifiedKeys(t *testing.T) {
	runCases(t, []decCase{
		{name: "Ctrl-Up", in: "\x1b[1;5A", want: Key{Kind: KeyUp, Ctrl: true}},
		{name: "Shift-Right", in: "\x1b[1;2C", want: Key{Kind: KeyRight, Shift: true}},
		{name: "Alt-Left", in: "\x1b[1;3D", want: Key{Kind: KeyLeft, Alt: true}},
		{name: "Ctrl-Alt-Down", in: "\x1b[1;7B", want: Key{Kind: KeyDown, Ctrl: true, Alt: true}},
		{name: "Ctrl-Home", in: "\x1b[1;5H", want: Key{Kind: KeyHome, Ctrl: true}},
		{name: "Ctrl-End", in: "\x1b[1;5F", want: Key{Kind: KeyEnd, Ctrl: true}},
		// The modifier rides in the same position on tilde finals.
		{name: "Ctrl-Delete", in: "\x1b[3;5~", want: Key{Kind: KeyDelete, Ctrl: true}},
		{name: "Shift-PageUp", in: "\x1b[5;2~", want: Key{Kind: KeyPageUp, Shift: true}},
		// An omitted first parameter is legal and means "default".
		{name: "omitted first param", in: "\x1b[;5A", want: Key{Kind: KeyUp, Ctrl: true}},
		// xterm modifyOtherKeys / kitty append sub-parameters after a colon. The
		// base key must survive a protocol we do not otherwise speak.
		{name: "colon sub-parameter", in: "\x1b[1;5:3A", want: Key{Kind: KeyUp, Ctrl: true}},
	})
}

// TestModifierIsBitmaskPlusOne pins the encoding directly, because getting it
// wrong does not break arrows — it shifts every modifier one place along, which
// is far harder to notice than an arrow key that stopped working.
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
		got, _, ok := DecodeKey([]byte(in))
		if !ok {
			t.Fatalf("DecodeKey(%q) wants more bytes", in)
		}
		if got.Shift != c.shift || got.Alt != c.alt || got.Ctrl != c.ctrl {
			t.Errorf("DecodeKey(%q): shift=%v alt=%v ctrl=%v, want shift=%v alt=%v ctrl=%v.\n"+
				"The parameter is a bitmask PLUS ONE (1 = no modifiers), so it must be decremented "+
				"before masking: 1=shift, 2=alt, 4=ctrl.",
				in, got.Shift, got.Alt, got.Ctrl, c.shift, c.alt, c.ctrl)
		}
		if got.Kind != KeyUp {
			t.Errorf("DecodeKey(%q) = %v; a modifier must never change which key it is", in, got)
		}
	}
	// The unmodified form must leave every flag clear.
	if got, _, _ := DecodeKey([]byte("\x1b[1;1A")); got.Shift || got.Alt || got.Ctrl {
		t.Errorf("DecodeKey(\"\\x1b[1;1A\") = %v; parameter 1 means no modifiers at all", got)
	}
}

func TestDecodeAltPrefixedKeys(t *testing.T) {
	// Terminals implement Alt as an ESC prefix, so Alt-a and "Escape then a" are
	// the same bytes. Within one buffer we choose Alt, which is what every editor
	// does.
	runCases(t, []decCase{
		{name: "Alt-a", in: "\x1ba", want: Key{Kind: KeyRune, Rune: 'a', Alt: true}},
		{name: "Alt-Enter", in: "\x1b\r", want: Key{Kind: KeyEnter, Alt: true}},
		{name: "Alt-Backspace", in: "\x1b\x7f", want: Key{Kind: KeyBackspace, Alt: true}},
		{name: "Alt-multibyte", in: "\x1b中", n: 4, want: Key{Kind: KeyRune, Rune: '中', Alt: true}},
		// ESC ESC [ A is what tmux forwards for Alt-Up.
		{name: "ESC ESC CSI is Alt-Up", in: "\x1b\x1b[A", want: Key{Kind: KeyUp, Alt: true}},
	})
}

func TestDecodeUnknownButWellFormed(t *testing.T) {
	// Consume the whole sequence, report KeyUnknown, keep the bytes in Raw. Never
	// return "need more bytes" for something merely unrecognised — that is a
	// live-lock, not a parse error.
	runCases(t, []decCase{
		{name: "Insert", in: "\x1b[2~", want: Key{Kind: KeyUnknown}},
		{name: "F5", in: "\x1b[15~", want: Key{Kind: KeyUnknown}},
		{name: "SS3 F1", in: "\x1bOP", want: Key{Kind: KeyUnknown}},
		{name: "device attributes reply", in: "\x1b[?1;2c", want: Key{Kind: KeyUnknown}},
		{name: "cursor position report", in: "\x1b[24;80R", want: Key{Kind: KeyUnknown}},
		{name: "focus in", in: "\x1b[I", want: Key{Kind: KeyUnknown}},
		{name: "stray paste terminator", in: "\x1b[201~", want: Key{Kind: KeyUnknown}},
		// An intermediate byte between parameters and the final byte: DECSCUSR.
		// The only thing keeping it from desynchronising the stream is that the
		// parser knows the byte classes rather than a list of shapes.
		{name: "intermediate byte", in: "\x1b[ q", want: Key{Kind: KeyUnknown}},
		{name: "unrecognised final byte", in: "\x1b[999X", want: Key{Kind: KeyUnknown}},
		// A control byte injected mid-sequence aborts it. We consume up to but not
		// including the control byte, so Ctrl-C still works when another writer to
		// the tty interleaves with a mouse report.
		{name: "control byte aborts CSI", in: "\x1b[1;\x03A", n: 4, want: Key{Kind: KeyUnknown}},
	})
}

func TestDecodeSGRMouse(t *testing.T) {
	runCases(t, []decCase{
		{name: "left press", in: "\x1b[<0;10;20M",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 0, X: 10, Y: 20, Press: true}}},
		{name: "left release", in: "\x1b[<0;10;20m",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 0, X: 10, Y: 20, Press: false}}},
		{name: "middle press", in: "\x1b[<1;1;1M",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 1, X: 1, Y: 1, Press: true}}},
		{name: "right press", in: "\x1b[<2;3;4M",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 2, X: 3, Y: 4, Press: true}}},
		{name: "right release", in: "\x1b[<2;3;4m",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 2, X: 3, Y: 4, Press: false}}},
		// The wheel keeps its bit: on this wire, wheel-up genuinely IS button 64.
		// It only ever reports a press; there is no notch release.
		{name: "wheel up", in: "\x1b[<64;5;6M",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 64, X: 5, Y: 6, Press: true}}},
		{name: "wheel down", in: "\x1b[<65;5;6M",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 65, X: 5, Y: 6, Press: true}}},
		// Motion bit 0x20 set: a drag with the left button held. It must report
		// button 0, not button 32.
		{name: "drag with left button", in: "\x1b[<32;7;8M",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 0, X: 7, Y: 8, Press: true}}},
		// Modifier bits move to the key's flags and leave the button alone.
		{name: "ctrl-click", in: "\x1b[<16;9;9M",
			want: Key{Kind: KeyMouse, Ctrl: true, Mouse: MouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "shift-click", in: "\x1b[<4;9;9M",
			want: Key{Kind: KeyMouse, Shift: true, Mouse: MouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "alt-right-click", in: "\x1b[<10;2;2M",
			want: Key{Kind: KeyMouse, Alt: true, Mouse: MouseEvent{Button: 2, X: 2, Y: 2, Press: true}}},
		{name: "ctrl-wheel-up", in: "\x1b[<80;1;1M",
			want: Key{Kind: KeyMouse, Ctrl: true, Mouse: MouseEvent{Button: 64, X: 1, Y: 1, Press: true}}},

		// The entire reason SGR exists. The legacy X10 encoding puts a coordinate
		// in one byte biased by 32, so it cannot name a column past 223.
		{name: "column past the X10 limit", in: "\x1b[<0;300;150M",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 0, X: 300, Y: 150, Press: true}}},
		{name: "release past the X10 limit", in: "\x1b[<2;1000;500m",
			want: Key{Kind: KeyMouse, Mouse: MouseEvent{Button: 2, X: 1000, Y: 500, Press: false}}},

		// Malformed but well-formed-as-CSI: consumed whole, reported unknown.
		{name: "too few parameters", in: "\x1b[<0;10M", want: Key{Kind: KeyUnknown}},
	})
}

func TestX10MouseReportIsSwallowedWhole(t *testing.T) {
	// X10 mouse is CSI M plus three RAW bytes that are not part of the CSI
	// sequence. We do not report the click — the encoding cannot name a column
	// past 223 — but we must still eat those three bytes, or they are decoded as
	// three keystrokes typed into whatever the user was editing.
	buf := []byte("\x1b[M\x20\x21\x22a")
	k, n, ok := DecodeKey(buf)
	if !ok || n != 6 || k.Kind != KeyUnknown {
		t.Fatalf("DecodeKey(%q) = %v n=%d ok=%v; want KeyUnknown n=6.\n"+
			"CSI M is followed by three raw payload bytes; consuming only the CSI part lets them "+
			"through as phantom input.", buf, k, n, ok)
	}
	next, _, ok := DecodeKey(buf[n:])
	if !ok || next.Kind != KeyRune || next.Rune != 'a' {
		t.Fatalf("after the X10 report the next key is %v; want the literal 'a' that followed it", next)
	}
	// Truncated payload is a genuine "need more bytes".
	if k, n, ok := DecodeKey([]byte("\x1b[M\x20")); ok {
		t.Errorf("DecodeKey on a truncated X10 report returned %v n=%d; the payload is 3 bytes and only 1 arrived", k, n)
	}
}

func TestDecodePaste(t *testing.T) {
	runCases(t, []decCase{
		{name: "simple", in: "\x1b[200~hello\x1b[201~",
			want: Key{Kind: KeyPaste, Text: "hello"}},
		{name: "empty", in: "\x1b[200~\x1b[201~",
			want: Key{Kind: KeyPaste, Text: ""}},

		// The one that matters. A payload containing an escape sequence must come
		// out byte-for-byte as text — NOT decoded. Re-running the payload through
		// the decoder turns a pasted shell script into arrow keys.
		{name: "payload contains an escape sequence", in: "\x1b[200~x\x1b[Ay\x1b[201~",
			want: Key{Kind: KeyPaste, Text: "x\x1b[Ay"}},
		{name: "payload is a bare ESC", in: "\x1b[200~\x1b\x1b[201~",
			want: Key{Kind: KeyPaste, Text: "\x1b"}},

		// The other one that matters. In a prompt UI a newline submits, so a
		// pasted newline that leaks out as KeyEnter sends half a paste to the
		// model.
		{name: "payload contains newlines", in: "\x1b[200~line one\nline two\n\x1b[201~",
			want: Key{Kind: KeyPaste, Text: "line one\nline two\n"}},
		{name: "payload contains a carriage return", in: "\x1b[200~a\r\nb\x1b[201~",
			want: Key{Kind: KeyPaste, Text: "a\r\nb"}},
		{name: "payload contains control bytes", in: "\x1b[200~a\x03b\x1b[201~",
			want: Key{Kind: KeyPaste, Text: "a\x03b"}},
		{name: "payload contains multi-byte runes", in: "\x1b[200~中😀\x1b[201~",
			want: Key{Kind: KeyPaste, Text: "中😀"}},

		// Consume exactly the markers and the payload, nothing after.
		{name: "stops at the terminator", in: "\x1b[200~a\x1b[201~b", n: 13,
			want: Key{Kind: KeyPaste, Text: "a"}},
	})
}

func TestUnterminatedPasteNeedsMoreBytesEvenWhenFinal(t *testing.T) {
	// The single documented exception to "DecodeKeyFinal always makes progress".
	// The escape timeout answers a question about a human's fingers; a paste is a
	// machine writing at pipe speed and arrives over many reads. A gap mid-paste
	// is not evidence that the paste ended.
	for _, in := range []string{
		"\x1b[200~",
		"\x1b[200~half a payl",
		"\x1b[200~payload with an \x1b in it",
		"\x1b[200~almost there\x1b[201", // terminator itself split across reads
	} {
		for _, d := range []struct {
			name string
			fn   func([]byte) (Key, int, bool)
		}{{"DecodeKey", DecodeKey}, {"DecodeKeyFinal", DecodeKeyFinal}} {
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

func TestLoneEscapeIsAmbiguous(t *testing.T) {
	// Same one byte, two different answers, and the difference is not in the
	// bytes — it is the caller telling the decoder that it already waited.
	buf := []byte{0x1b}

	k, n, ok := DecodeKey(buf)
	if ok {
		t.Errorf("DecodeKey(ESC) = %v n=%d ok=true; want ok=false.\n"+
			"A lone trailing ESC is either the Escape key or the first byte of an arrow key that has "+
			"not finished arriving, and nothing in the bytes can tell you which. Deciding here means "+
			"every arrow key over a slow link becomes Escape plus two garbage characters.", k, n)
	}
	if n != 0 {
		t.Errorf("DecodeKey(ESC) returned ok=false but claimed %d bytes consumed; must be 0", n)
	}

	k, n, ok = DecodeKeyFinal(buf)
	if !ok || n != 1 || k.Kind != KeyEsc {
		t.Errorf("DecodeKeyFinal(ESC) = %v n=%d ok=%v; want KeyEsc n=1 ok=true.\n"+
			"Once the caller has waited out the escape timeout with nothing arriving, a lone ESC is "+
			"the Escape key. Resolving that is the entire reason this second entry point exists.", k, n, ok)
	}
}

func TestNeedsMoreBytes(t *testing.T) {
	// Every one of these is a prefix of something longer. DecodeKey must say so
	// and must consume nothing, so the caller can append to the same buffer and
	// ask again.
	for _, in := range []string{
		"",           // nothing at all
		"\x1b",       // the ambiguity
		"\x1b[",      // CSI introducer only
		"\x1bO",      // SS3 introducer only
		"\x1b[1",     // parameters, no final byte
		"\x1b[1;",    // ditto
		"\x1b[1;5",   // ditto
		"\x1b[<0;10", // mouse report, mid-parameters
		"\x1b[<0;10;20",
		"\x1b[ ",   // intermediate byte, no final byte
		"\x1b\x1b", // Alt-something, the something has not arrived
		"\x1b[M",   // X10 report, none of the payload
		"\x1b[M\x20\x21",
		"\xe4",         // first byte of a three-byte rune
		"\xe4\xb8",     // first two bytes of a three-byte rune
		"\xf0\x9f\x98", // first three bytes of a four-byte rune
	} {
		k, n, ok := DecodeKey([]byte(in))
		if ok {
			t.Errorf("DecodeKey(%q) = %v n=%d ok=true; want ok=false.\n"+
				"That input is a prefix. Answering it produces a keystroke the user never made, and "+
				"leaves the rest of the real sequence to be decoded as garbage.", in, k, n)
		}
		if n != 0 {
			t.Errorf("DecodeKey(%q) returned ok=false but claimed %d bytes consumed; must be 0 so the "+
				"caller can append and retry against the same buffer", in, n)
		}
	}
}

func TestTruncatedRuneIsNotAReplacementCharacter(t *testing.T) {
	// utf8.DecodeRune returns (RuneError, 1) for a truncated rune just as it does
	// for an invalid one, so the obvious code emits U+FFFD for a good character
	// that merely straddled a read boundary — and destroys the evidence.
	for _, in := range []string{"\xe4", "\xe4\xb8", "\xf0", "\xf0\x9f", "\xf0\x9f\x98"} {
		k, n, ok := DecodeKey([]byte(in))
		if ok {
			t.Errorf("DecodeKey(%q) = %v n=%d; want ok=false. A truncated rune needs more bytes, "+
				"not a replacement character.", in, k, n)
		}
	}
	// The same bytes, once complete, are one key.
	if k, n, ok := DecodeKey([]byte("中")); !ok || n != 3 || k.Rune != '中' {
		t.Errorf("DecodeKey(\"中\") = %v n=%d ok=%v; want the rune, 3 bytes", k, n, ok)
	}
}

func TestDecodeOneByteAtATime(t *testing.T) {
	// How the decoder is really used. A read on a tty returns whatever bytes
	// happen to be in the buffer at that instant, and under load or over ssh that
	// is routinely one byte.
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
				k, n, ok := DecodeKey(full[:i])
				if ok {
					t.Fatalf("DecodeKey(%q) — the first %d of %d bytes — returned %v (n=%d).\n"+
						"That is a proper prefix of %q, so answering it invents a keystroke and leaves "+
						"the remaining bytes to be decoded as garbage.", full[:i], i, len(full), k, n, s.in)
				}
				if n != 0 {
					t.Fatalf("DecodeKey(%q) returned ok=false but consumed %d bytes", full[:i], n)
				}
			}
			_, n, ok := DecodeKey(full)
			if !ok || n != len(full) {
				t.Fatalf("DecodeKey(%q) on the complete sequence: ok=%v n=%d; want ok=true n=%d",
					s.in, ok, n, len(full))
			}
		})
	}
}

func TestConsumedCountsTileTheBuffer(t *testing.T) {
	// One read often carries several keys — key repeat, a fast typist, or a mouse
	// drag. The caller advances by n and decodes again, so the counts must add up
	// to exactly the buffer length. An off-by-one anywhere puts every subsequent
	// key one byte out of phase, which does not fail loudly: it produces
	// plausible-looking wrong keys.
	buf := []byte("\x1b[1;5A" + "中" + "\x1b[<2;10;20m" + "\x1b[200~hi\x1b[201~" + "x" + "\x1b[3~")
	want := []struct {
		kind KeyKind
		n    int
	}{
		{KeyUp, 6},
		{KeyRune, 3},
		{KeyMouse, 11},
		{KeyPaste, 14},
		{KeyRune, 1},
		{KeyDelete, 4},
	}

	pos := 0
	for i, w := range want {
		k, n, ok := DecodeKey(buf[pos:])
		if !ok {
			t.Fatalf("key %d at offset %d: DecodeKey(%q) wants more bytes, but a whole key is there",
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
	if _, _, ok := DecodeKey(buf[pos:]); ok {
		t.Errorf("DecodeKey on the empty remainder returned a key")
	}
}

func TestDecodeKeyFinalAlwaysMakesProgress(t *testing.T) {
	// DecodeKeyFinal is the caller's last resort: the read deadline expired and
	// there is nothing else to try. If it can answer "need more bytes" for input
	// that will never grow, the caller spins forever. (Bracketed paste is the one
	// deliberate exception.)
	for _, s := range []string{
		"\x1b", "\x1b[", "\x1bO", "\x1b[1;5", "\x1b[<0;10;20", "\x1b[ ",
		"\x1b[M", "\x1b[M\x20\x21", "\x1b\x1b", "\x1b\x1b[",
		"\xe4", "\xe4\xb8", "\xf0\x9f\x98", "\xff", "a", "\x1b[A", "中",
	} {
		for i := 1; i <= len(s); i++ {
			frag := s[:i]
			k, n, ok := DecodeKeyFinal([]byte(frag))
			if !ok {
				t.Errorf("DecodeKeyFinal(%q) returned ok=false. No more bytes are coming, so there is "+
					"nothing the caller can do with that answer except ask again — forever.", frag)
				continue
			}
			if n <= 0 || n > len(frag) {
				t.Errorf("DecodeKeyFinal(%q) = %v consuming %d bytes; must consume between 1 and %d",
					frag, k, n, len(frag))
			}
		}
	}
	// And the degenerate case: an empty buffer holds no key and consumes none.
	if k, n, ok := DecodeKeyFinal(nil); ok || n != 0 {
		t.Errorf("DecodeKeyFinal(nil) = %v n=%d ok=%v; want no key, 0 bytes", k, n, ok)
	}
}

func TestRawAlwaysHoldsTheConsumedBytes(t *testing.T) {
	// Raw is the debugging affordance: when an unknown sequence shows up in a
	// log, it is the only thing that identifies it. It must equal the bytes
	// consumed, exactly, including for keys assembled through the Alt-prefix
	// recursion, where it is easiest to get wrong.
	for _, in := range []string{
		"a", "中", "\x03", "\x1b[A", "\x1bOA", "\x1b[1;5A", "\x1b[<0;10;20M",
		"\x1b[200~hi\x1b[201~", "\x1ba", "\x1b\x1b[A", "\x1b[999X", "\xff",
	} {
		k, n, ok := DecodeKeyFinal([]byte(in))
		if !ok {
			t.Fatalf("DecodeKeyFinal(%q) wants more bytes", in)
		}
		if k.Raw != in[:n] {
			t.Errorf("DecodeKeyFinal(%q) consumed %d bytes but Raw = %q; want %q.\n"+
				"Raw is what a bug report is made of; it has to be the literal bytes consumed.",
				in, n, k.Raw, in[:n])
		}
	}
}
