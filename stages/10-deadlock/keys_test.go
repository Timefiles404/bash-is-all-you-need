package main

import (
	"fmt"
	"testing"
)

// decCase is one "these bytes mean this key" assertion.
//
// Raw is never written out in a case: it is by definition the bytes consumed,
// so the harness derives it. That is not just convenience — it means every
// case double-checks the consumed count, because a Raw that is one byte short
// and an n that is one byte short are the same bug seen twice.
type decCase struct {
	name string
	in   string
	n    int // bytes consumed; 0 means "all of them"
	want key
}

func (c decCase) consumed() int {
	if c.n == 0 {
		return len(c.in)
	}
	return c.n
}

// runCases checks each case against BOTH entry points. Every case here is a
// complete sequence, and decodeKeyFinal must agree with decodeKey on all of
// them: the second entry point exists to resolve *incomplete* input and must
// not change a single decision anywhere else.
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
// Ordinary text
// ---------------------------------------------------------------------------

func TestDecodeRunes(t *testing.T) {
	runCases(t, []decCase{
		{name: "ascii letter", in: "a", want: key{Kind: keyRune, Rune: 'a'}},
		{name: "ascii capital", in: "Z", want: key{Kind: keyRune, Rune: 'Z'}},
		{name: "space", in: " ", want: key{Kind: keyRune, Rune: ' '}},
		{name: "tilde is text not a final byte", in: "~", want: key{Kind: keyRune, Rune: '~'}},
		// Multi-byte runes are the reason this decoder cannot work a byte at a
		// time. Someone typing in Chinese, or an emoji, produces one key event
		// out of two to four bytes.
		{name: "two-byte rune", in: "é", n: 2, want: key{Kind: keyRune, Rune: 'é'}},
		{name: "three-byte rune", in: "中", n: 3, want: key{Kind: keyRune, Rune: '中'}},
		{name: "four-byte rune", in: "😀", n: 4, want: key{Kind: keyRune, Rune: '😀'}},
		// Only the first key, and only its bytes.
		{name: "stops at the first rune", in: "ab", n: 1, want: key{Kind: keyRune, Rune: 'a'}},
		{name: "stops after a multi-byte rune", in: "中x", n: 3, want: key{Kind: keyRune, Rune: '中'}},
	})
}

func TestDecodeInvalidUTF8(t *testing.T) {
	// A byte that can never start a rune. It must be dropped, one byte at a
	// time, and never turned into U+FFFD: a replacement character here is a
	// glyph the user did not type appearing in their document.
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

		// Everything else in the control range is a Ctrl-letter, reported as
		// the letter with Ctrl set rather than as its own Kind. Adding a Kind
		// per binding is how a key enum reaches sixty entries.
		{name: "Ctrl-A", in: "\x01", want: key{Kind: keyRune, Rune: 'a', Ctrl: true}},
		{name: "Ctrl-Z", in: "\x1a", want: key{Kind: keyRune, Rune: 'z', Ctrl: true}},
		{name: "Ctrl-Space is NUL", in: "\x00", want: key{Kind: keyRune, Rune: ' ', Ctrl: true}},
		{name: "Ctrl-underscore", in: "\x1f", want: key{Kind: keyRune, Rune: '_', Ctrl: true}},
		{name: "Ctrl-backslash", in: "\x1c", want: key{Kind: keyRune, Rune: '\\', Ctrl: true}},
	})
}

// ---------------------------------------------------------------------------
// Escape sequences
// ---------------------------------------------------------------------------

func TestDecodeArrowsBothForms(t *testing.T) {
	// The SS3 half of this table is the one that matters. A decoder that knows
	// only CSI arrows works perfectly until the day someone runs it inside
	// tmux or over ssh into a shell that left DECCKM on, and then the arrow
	// keys start typing letters.
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
	// All eight. Four lineages that never agreed and never will; see the
	// comment in decodeCSI. Deleting any row here is deleting support for
	// somebody's terminal.
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
		// CSI Z has no modifier parameter, so the flags stay clear: the shift
		// is in the Kind.
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
		// The modifier rides in the same position on tilde finals.
		{name: "Ctrl-Delete", in: "\x1b[3;5~", want: key{Kind: keyDelete, Ctrl: true}},
		{name: "Shift-PageUp", in: "\x1b[5;2~", want: key{Kind: keyPageUp, Shift: true}},
		// An omitted first parameter is legal and means "default".
		{name: "omitted first param", in: "\x1b[;5A", want: key{Kind: keyUp, Ctrl: true}},
		// xterm modifyOtherKeys / kitty append sub-parameters after a colon.
		// The base key must survive a protocol we do not otherwise speak.
		{name: "colon sub-parameter", in: "\x1b[1;5:3A", want: key{Kind: keyUp, Ctrl: true}},
	})
}

// TestModifierIsBitmaskPlusOne pins the encoding directly, because getting it
// wrong does not break arrows — it silently shifts every modifier one place
// along, which is far harder to notice than an arrow key that stops working.
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
	// The unmodified form must leave every flag clear.
	if got, _, _ := decodeKey([]byte("\x1b[1;1A")); got.Shift || got.Alt || got.Ctrl {
		t.Errorf("decodeKey(\"\\x1b[1;1A\") = %v; parameter 1 means no modifiers at all", got)
	}
}

func TestDecodeAltPrefixedKeys(t *testing.T) {
	// Terminals implement Alt as an ESC prefix ("metaSendsEscape"), so Alt-a
	// and "Escape then a" are the same bytes. Within one buffer we choose Alt,
	// which is what every editor does.
	runCases(t, []decCase{
		{name: "Alt-a", in: "\x1ba", want: key{Kind: keyRune, Rune: 'a', Alt: true}},
		{name: "Alt-Enter", in: "\x1b\r", want: key{Kind: keyEnter, Alt: true}},
		{name: "Alt-Backspace", in: "\x1b\x7f", want: key{Kind: keyBackspace, Alt: true}},
		{name: "Alt-multibyte", in: "\x1b中", n: 4, want: key{Kind: keyRune, Rune: '中', Alt: true}},
		// ESC ESC [ A is what tmux forwards for Alt-Up.
		{name: "ESC ESC CSI is Alt-Up", in: "\x1b\x1b[A", want: key{Kind: keyUp, Alt: true}},
	})
}

func TestDecodeUnknownButWellFormed(t *testing.T) {
	// The rule these all check: consume the whole sequence, report keyUnknown,
	// keep the bytes in Raw. Never return "need more bytes" for something that
	// is merely unrecognised — that is a live-lock, not a parse error.
	runCases(t, []decCase{
		{name: "Insert", in: "\x1b[2~", want: key{Kind: keyUnknown}},
		{name: "F5", in: "\x1b[15~", want: key{Kind: keyUnknown}},
		{name: "SS3 F1", in: "\x1bOP", want: key{Kind: keyUnknown}},
		{name: "device attributes reply", in: "\x1b[?1;2c", want: key{Kind: keyUnknown}},
		{name: "cursor position report", in: "\x1b[24;80R", want: key{Kind: keyUnknown}},
		{name: "focus in", in: "\x1b[I", want: key{Kind: keyUnknown}},
		{name: "stray paste terminator", in: "\x1b[201~", want: key{Kind: keyUnknown}},
		// An intermediate byte (0x20-0x2f) between parameters and the final
		// byte: DECSCUSR, the cursor-shape sequence. Rare on input, and the
		// only thing keeping it from desynchronising the stream is that the
		// parser knows the byte classes rather than a list of shapes.
		{name: "intermediate byte", in: "\x1b[ q", want: key{Kind: keyUnknown}},
		{name: "unrecognised final byte", in: "\x1b[999X", want: key{Kind: keyUnknown}},
		// A control byte injected mid-sequence aborts it. We consume up to but
		// not including the control byte, so Ctrl-C still works when another
		// writer to the tty interleaves with a mouse report.
		{name: "control byte aborts CSI", in: "\x1b[1;\x03A", n: 4, want: key{Kind: keyUnknown}},
	})
}

// ---------------------------------------------------------------------------
// Mouse
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
		// The wheel keeps its bit: on this wire, wheel-up genuinely IS button
		// 64. It only ever reports a press; there is no notch release.
		{name: "wheel up", in: "\x1b[<64;5;6M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 64, X: 5, Y: 6, Press: true}}},
		{name: "wheel down", in: "\x1b[<65;5;6M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 65, X: 5, Y: 6, Press: true}}},
		// Motion bit 0x20 set: a drag with the left button held. It must
		// report button 0, not button 32.
		{name: "drag with left button", in: "\x1b[<32;7;8M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 7, Y: 8, Press: true}}},
		// Modifier bits move to the key's flags and leave the button alone.
		{name: "ctrl-click", in: "\x1b[<16;9;9M",
			want: key{Kind: keyMouse, Ctrl: true, Mouse: mouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "shift-click", in: "\x1b[<4;9;9M",
			want: key{Kind: keyMouse, Shift: true, Mouse: mouseEvent{Button: 0, X: 9, Y: 9, Press: true}}},
		{name: "alt-right-click", in: "\x1b[<10;2;2M",
			want: key{Kind: keyMouse, Alt: true, Mouse: mouseEvent{Button: 2, X: 2, Y: 2, Press: true}}},
		{name: "ctrl-wheel-up", in: "\x1b[<80;1;1M",
			want: key{Kind: keyMouse, Ctrl: true, Mouse: mouseEvent{Button: 64, X: 1, Y: 1, Press: true}}},

		// The entire reason SGR exists. The legacy X10 encoding puts a
		// coordinate in one byte biased by 32, so it cannot name a column past
		// 223 — a real limit on any window wider than a 1980s terminal.
		{name: "column past the X10 limit", in: "\x1b[<0;300;150M",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 0, X: 300, Y: 150, Press: true}}},
		{name: "release past the X10 limit", in: "\x1b[<2;1000;500m",
			want: key{Kind: keyMouse, Mouse: mouseEvent{Button: 2, X: 1000, Y: 500, Press: false}}},

		// Malformed but well-formed-as-CSI: consumed whole, reported unknown.
		{name: "too few parameters", in: "\x1b[<0;10M", want: key{Kind: keyUnknown}},
	})
}

func TestX10MouseReportIsSwallowedWhole(t *testing.T) {
	// X10 mouse (mode 1000 without 1006) is CSI M plus three RAW bytes that
	// are not part of the CSI sequence. We do not report the click — the
	// encoding cannot name a column past 223 — but we must still eat those
	// three bytes, or they are decoded as three keystrokes typed into whatever
	// the user was editing.
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
	// Truncated payload is a genuine "need more bytes".
	if k, n, ok := decodeKey([]byte("\x1b[M\x20")); ok {
		t.Errorf("decodeKey on a truncated X10 report returned %v n=%d; the payload is 3 bytes and only 1 arrived", k, n)
	}
}

// ---------------------------------------------------------------------------
// Bracketed paste
// ---------------------------------------------------------------------------

func TestDecodePaste(t *testing.T) {
	runCases(t, []decCase{
		{name: "simple", in: "\x1b[200~hello\x1b[201~",
			want: key{Kind: keyPaste, Text: "hello"}},
		{name: "empty", in: "\x1b[200~\x1b[201~",
			want: key{Kind: keyPaste, Text: ""}},

		// The one that matters. A payload containing an escape sequence must
		// come out byte-for-byte as text — NOT decoded. Re-running the payload
		// through the decoder turns a pasted shell script into arrow keys.
		{name: "payload contains an escape sequence", in: "\x1b[200~x\x1b[Ay\x1b[201~",
			want: key{Kind: keyPaste, Text: "x\x1b[Ay"}},
		{name: "payload is a bare ESC", in: "\x1b[200~\x1b\x1b[201~",
			want: key{Kind: keyPaste, Text: "\x1b"}},

		// The other one that matters. In a prompt UI a newline submits, so a
		// pasted newline that leaks out as keyEnter sends half a paste to the
		// model. This is what bracketed paste was invented to prevent.
		{name: "payload contains newlines", in: "\x1b[200~line one\nline two\n\x1b[201~",
			want: key{Kind: keyPaste, Text: "line one\nline two\n"}},
		{name: "payload contains a carriage return", in: "\x1b[200~a\r\nb\x1b[201~",
			want: key{Kind: keyPaste, Text: "a\r\nb"}},
		{name: "payload contains control bytes", in: "\x1b[200~a\x03b\x1b[201~",
			want: key{Kind: keyPaste, Text: "a\x03b"}},
		{name: "payload contains multi-byte runes", in: "\x1b[200~中😀\x1b[201~",
			want: key{Kind: keyPaste, Text: "中😀"}},

		// Consume exactly the markers and the payload, nothing after.
		{name: "stops at the terminator", in: "\x1b[200~a\x1b[201~b", n: 13,
			want: key{Kind: keyPaste, Text: "a"}},
	})
}

func TestUnterminatedPasteNeedsMoreBytesEvenWhenFinal(t *testing.T) {
	// The single documented exception to "decodeKeyFinal always makes
	// progress". The escape timeout answers a question about a human's
	// fingers; a paste is a machine writing at pipe speed and arrives over
	// many reads. A gap mid-paste is not evidence that the paste ended, and
	// resolving it would emit half the payload as text and decode the rest as
	// keystrokes.
	for _, in := range []string{
		"\x1b[200~",
		"\x1b[200~half a payl",
		"\x1b[200~payload with an \x1b in it",
		"\x1b[200~almost there\x1b[201", // terminator itself split across reads
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
// The ambiguity, and the streaming contract
// ---------------------------------------------------------------------------

func TestLoneEscapeIsAmbiguous(t *testing.T) {
	// The pair of assertions that documents the whole design. Same one byte,
	// two different answers, and the difference is not in the bytes — it is
	// the caller telling the decoder that it already waited.
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
	// Every one of these is a prefix of something longer. decodeKey must say
	// so and must consume nothing, so the caller can append to the same buffer
	// and ask again.
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
	// Called out separately because the failure is so tempting: utf8.DecodeRune
	// returns (RuneError, 1) for a truncated rune just as it does for an
	// invalid one, so the obvious code emits U+FFFD for a perfectly good
	// character that merely straddled a read boundary. The evidence is
	// destroyed at that point — the remaining bytes decode as more garbage.
	for _, in := range []string{"\xe4", "\xe4\xb8", "\xf0", "\xf0\x9f", "\xf0\x9f\x98"} {
		k, n, ok := decodeKey([]byte(in))
		if ok {
			t.Errorf("decodeKey(%q) = %v n=%d; want ok=false. A truncated rune needs more bytes, "+
				"not a replacement character.", in, k, n)
		}
	}
	// The same bytes, once complete, are one key.
	if k, n, ok := decodeKey([]byte("中")); !ok || n != 3 || k.Rune != '中' {
		t.Errorf("decodeKey(\"中\") = %v n=%d ok=%v; want the rune, 3 bytes", k, n, ok)
	}
}

func TestDecodeOneByteAtATime(t *testing.T) {
	// This is how the decoder is really used. A read on a tty returns whatever
	// bytes happen to be in the buffer at that instant, and under load or over
	// ssh that is routinely one byte. Every proper prefix must report "more",
	// and the last byte must complete the key.
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
	// One read often carries several keys — key repeat, a fast typist, or a
	// mouse drag. The caller advances by n and decodes again, so the counts
	// have to add up to exactly the buffer length. An off-by-one anywhere puts
	// every subsequent key one byte out of phase, which does not fail loudly:
	// it produces plausible-looking wrong keys.
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
	// decodeKeyFinal is the caller's last resort: the read deadline expired
	// and there is nothing else to try. If it can answer "need more bytes" for
	// input that will never grow, the caller spins forever. So on any
	// non-empty buffer it must consume at least one byte — including on
	// fragments it cannot interpret, which it reports as keyUnknown rather
	// than inventing a keystroke for. (Bracketed paste is the one deliberate
	// exception; see TestUnterminatedPasteNeedsMoreBytesEvenWhenFinal.)
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
	// And the degenerate case: an empty buffer holds no key and consumes none.
	if k, n, ok := decodeKeyFinal(nil); ok || n != 0 {
		t.Errorf("decodeKeyFinal(nil) = %v n=%d ok=%v; want no key, 0 bytes", k, n, ok)
	}
}

func TestRawAlwaysHoldsTheConsumedBytes(t *testing.T) {
	// Raw is the debugging affordance: when an unknown sequence shows up in a
	// log, it is the only thing that identifies it. It must equal the bytes
	// consumed, exactly, for every kind of key — including the ones assembled
	// through the Alt-prefix recursion, where it is easiest to get wrong.
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
