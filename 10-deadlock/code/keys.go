// Stage 06 — the composer, input half.
//
// Bytes in, key events out. That is the whole job, and it is harder than it
// sounds for exactly one reason: a terminal keyboard is not a device, it is a
// *protocol*, and the protocol is forty years of sediment — VT100, VT220,
// rxvt, xterm, and everything that copied one of them. There is no registry and
// no version negotiation. Two terminals send different bytes for the Home key,
// and the *same* terminal sends different bytes for the Up arrow depending on a
// mode the application itself switched on earlier in the session.
//
// So this file is a parser for a protocol nobody ever wrote down, and its shape
// follows from that: recognise generously, consume exactly, and never guess.
//
// Two invariants hold everywhere below, and every caller depends on them:
//
//	Progress. Whenever ok is true, n > 0. A decoder that can return
//	"I decoded something, and it was zero bytes long" live-locks the read loop,
//	and it does it in production on the one sequence you never tested.
//
//	Honesty. ok=false means "these bytes are a prefix of something and I need
//	more", never "I don't recognise this". An unrecognised but well-formed
//	sequence is consumed whole and reported as keyUnknown with Raw set. Getting
//	that backwards is the same live-lock wearing a different hat: the caller
//	reads more bytes forever, waiting for a sequence that already finished.
package main

import (
	"bytes"
	"fmt"
	"strconv"
	"unicode/utf8"
)

type keyKind int

const (
	keyRune keyKind = iota // a printable character, in key.Rune
	keyEnter
	keyEsc
	keyTab
	keyShiftTab
	keyBackspace
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyPageUp
	keyPageDown
	keyDelete
	keyCtrlC
	keyCtrlD
	keyCtrlL
	keyMouse // details in key.Mouse
	keyPaste // bracketed-paste payload, in key.Text

	// keyUnknown is "bytes we consumed and will not act on": a well-formed
	// sequence we chose not to interpret (a function key, a device-attributes
	// reply), or input we could not make sense of at all. Raw always holds the
	// exact bytes, which is the difference between a mystery and a bug report.
	keyUnknown
)

type mouseEvent struct {
	Button int  // 0 left, 1 middle, 2 right, 64 wheel-up, 65 wheel-down
	X, Y   int  // 1-based columns/rows exactly as the terminal reports them
	Press  bool // true = press, false = release
}

type key struct {
	Kind  keyKind
	Rune  rune       // keyRune
	Text  string     // keyPaste
	Mouse mouseEvent // keyMouse
	Ctrl  bool       // a modifier the terminal reported (e.g. \x1b[1;5A)
	Alt   bool
	Shift bool
	Raw   string // the exact bytes consumed, for debugging and for keyUnknown
}

// ---------------------------------------------------------------------------
// The Escape ambiguity
//
// This is why there are two entry points instead of one, and it is worth
// understanding properly, because it is not a wart in this decoder. It is a
// hole in the terminal protocol that no decoder anywhere has ever closed.
//
// Escape is 0x1b. It is also the first byte of every arrow key, every function
// key, every mouse report and every paste. So when a read returns a buffer
// ending in a bare 0x1b, there are exactly two possibilities:
//
//  1. the user pressed Escape, and that is the whole story;
//  2. the user pressed Up, and the tty handed us the first byte of "\x1b[A"
//     because the read happened to land between two bytes of the burst.
//
// The bytes are identical. There is no length prefix, no terminator, no flag —
// nothing in the stream distinguishes the two cases and nothing ever will,
// because the encoding was designed for a terminal on which "Escape" and
// "escape sequence introducer" were deliberately the same key.
//
// The only signal that separates them is TIME. A sequence emitted by the
// terminal arrives as one burst: the remaining bytes are already sitting in the
// pty buffer, microseconds behind. A human pressing Escape leaves a gap of tens
// of milliseconds before whatever they type next. So every terminal program on
// earth resolves this the same way — read, see a lone ESC, wait a short
// timeout, and if nothing arrives, call it Escape.
//
// That timeout is why Escape feels a beat slow in vim, in tmux, in your shell's
// vi-mode, in every TUI you have ever used. It is not slow code and it is not a
// bug: it is a program declining to guess. It is also why `set -sg escape-time
// 10` is in half the tmux configs on GitHub, and why turning it down to 0 makes
// arrow keys and Alt-chords start misfiring over ssh — across a real network
// the burst is no longer guaranteed to land in one read.
//
// The decision this file makes is to keep that policy OUT of the decoder:
//
//	decodeKey       — "ok=false": I need more bytes, and I will not guess.
//	decodeKeyFinal  — "the caller already waited; a lone ESC is Escape."
//
// Two reasons. First, correctness: the right timeout is not a property of the
// byte stream, it is a property of the link. 25ms is generous on a local pty
// and far too short over a saturated ssh session, so the number belongs to the
// layer that knows about the link, not to a parser that cannot see it.
//
// Second, and more practical: the moment a decoder owns a clock, it stops being
// testable. You cannot write a table-driven test for "50ms elapsed" — you can
// only write a sleep, and a suite full of sleeps is a suite people stop
// running. A decoder that is a pure function of its input takes ten thousand
// byte sequences in a millisecond, which is the only reason the table at the
// bottom of keys_test.go is affordable.
//
// The caller's half of the contract is four lines:
//
//	k, n, ok := decodeKey(buf)
//	if !ok {
//		// short read with a deadline; when it expires with nothing:
//		k, n, ok = decodeKeyFinal(buf)
//	}
//
// ---------------------------------------------------------------------------

// decodeKey decodes one key from the front of buf.
// Returns the key, how many bytes it consumed, and ok=false when buf holds
// only the beginning of a sequence and the caller must read more bytes.
func decodeKey(buf []byte) (key, int, bool) { return decodeOne(buf, false) }

// decodeKeyFinal is decodeKey for the case where the caller has waited and no
// more bytes are coming. It resolves the ambiguity that decodeKey cannot.
//
// It makes progress on any non-empty buffer, with exactly one exception: an
// unterminated bracketed paste, which still returns ok=false. See decodePaste
// for why that exception is the right call and not a leak in the abstraction.
func decodeKeyFinal(buf []byte) (key, int, bool) { return decodeOne(buf, true) }

func decodeOne(buf []byte, final bool) (key, int, bool) {
	if len(buf) == 0 {
		return key{}, 0, false
	}
	switch {
	case buf[0] == 0x1b:
		return decodeEscape(buf, final)
	case buf[0] < 0x20 || buf[0] == 0x7f:
		return decodeControl(buf), 1, true
	default:
		return decodeRune(buf, final)
	}
}

// ---------------------------------------------------------------------------
// Plain bytes
// ---------------------------------------------------------------------------

func decodeRune(buf []byte, final bool) (key, int, bool) {
	// utf8.DecodeRune cannot tell "invalid" from "not finished yet": it returns
	// (RuneError, 1) for both. FullRune is the call that separates them, and
	// skipping it is how a UI ends up showing a replacement character every
	// time someone types an emoji that straddles a read boundary. The bytes
	// were fine; the decoder just looked too early and then destroyed the
	// evidence by emitting U+FFFD.
	if !utf8.FullRune(buf) {
		if !final {
			return key{}, 0, false
		}
		// The caller waited and the rest never came, so this really is a
		// truncated rune — a disconnect mid-character, or a program writing
		// half a string to the tty. Consume the fragment so the loop can make
		// progress, but do not invent a character the user never typed.
		return key{Kind: keyUnknown, Raw: string(buf)}, len(buf), true
	}
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size == 1 {
		// A byte that can never begin a rune: 0xFF, or a stray continuation
		// byte left over from a fragment we already dropped. Skip exactly one
		// byte — resynchronising by hand is how you recover a stream, and
		// U+FFFD here would push a glyph into the user's document.
		return key{Kind: keyUnknown, Raw: string(buf[:1])}, 1, true
	}
	return key{Kind: keyRune, Rune: r, Raw: string(buf[:size])}, size, true
}

func decodeControl(buf []byte) key {
	b := buf[0]
	k := key{Raw: string(buf[:1])}

	// Order matters here, and it encodes a decision rather than a lookup.
	// Ctrl-M and Enter are the same byte. So are Ctrl-I and Tab, Ctrl-J and
	// newline, Ctrl-H and Backspace. The terminal cannot tell them apart and
	// neither can we, so the named key wins — which is what every editor,
	// shell and TUI does, and why no application has ever bound Ctrl-M to
	// something other than Enter and got away with it.
	switch b {
	case 0x0d, 0x0a: // CR (raw mode never translates it) and LF
		k.Kind = keyEnter
	case 0x09:
		k.Kind = keyTab
	case 0x7f, 0x08:
		// Both, always. 0x7f (DEL) is what a modern terminal sends for
		// Backspace and 0x08 (BS) is what the terminfo entry claims it sends;
		// which one you get depends on the emulator, on stty erase, and on
		// whether you are inside tmux. Treating only one as Backspace produces
		// the classic "my backspace prints ^H over ssh" bug report.
		k.Kind = keyBackspace
	case 0x03:
		k.Kind = keyCtrlC
	case 0x04:
		k.Kind = keyCtrlD
	case 0x0c:
		k.Kind = keyCtrlL
	case 0x00:
		// NUL is what terminals send for Ctrl-Space, historically Ctrl-@.
		// Reporting it as a space with Ctrl set is the form applications
		// actually bind against.
		k.Kind, k.Rune, k.Ctrl = keyRune, ' ', true
	default:
		switch {
		case b >= 0x01 && b <= 0x1a:
			// Ctrl-A..Ctrl-Z. Lowercase, because Ctrl-A and Ctrl-Shift-A are
			// the same byte and the shifted form does not exist on the wire.
			k.Kind, k.Rune, k.Ctrl = keyRune, rune('a'+b-1), true
		case b >= 0x1c && b <= 0x1f:
			// Ctrl-\ Ctrl-] Ctrl-^ Ctrl-_ — the four controls above Z. The
			// byte is exactly the ASCII character minus 0x40.
			k.Kind, k.Rune, k.Ctrl = keyRune, rune(b+0x40), true
		default:
			// Unreachable: the only remaining byte under 0x20 is 0x1b, and
			// decodeOne routes that to decodeEscape before we are called.
			k.Kind = keyUnknown
		}
	}
	return k
}

// ---------------------------------------------------------------------------
// Escape sequences
// ---------------------------------------------------------------------------

func decodeEscape(buf []byte, final bool) (key, int, bool) {
	if len(buf) == 1 {
		// The centrepiece. See the comment block above decodeKey.
		if !final {
			return key{}, 0, false
		}
		return key{Kind: keyEsc, Raw: "\x1b"}, 1, true
	}

	switch buf[1] {
	case '[':
		return decodeCSI(buf, final)
	case 'O':
		return decodeSS3(buf, final)
	}

	// ESC followed by anything else is Alt+that key, because terminals
	// implement Alt as "metaSendsEscape": hold Alt, get an ESC prefix. Which
	// means Alt-a and "Escape, then a" are also byte-identical, and the same
	// timing argument as above applies. We choose Alt, unconditionally, for the
	// same reason every editor does: within one read, Alt is overwhelmingly the
	// likelier intent, and a caller that genuinely needs the distinction has
	// the timing information we do not.
	//
	// Recursing is not laziness. It gets Alt-Enter, Alt-Backspace and even
	// ESC ESC [ A (Alt-Up, which tmux forwards) right for free, and each of
	// those is a sequence somebody eventually files a bug about.
	k, n, ok := decodeOne(buf[1:], final)
	if !ok {
		return key{}, 0, false
	}
	k.Alt = true
	k.Raw = string(buf[:n+1])
	return k, n + 1, true
}

// incompleteEscape answers "this is definitely the start of an escape sequence,
// and it definitely has not finished yet".
func incompleteEscape(buf []byte, final bool) (key, int, bool) {
	if !final {
		return key{}, 0, false
	}
	// The caller waited and the rest never arrived. We know it began a
	// sequence (there is a `[` or an `O` right there) and we know it never
	// ended, so there is no keystroke to report — but we must still consume it,
	// or the caller asks the same question forever. keyUnknown carries the Raw
	// bytes; log them and you will usually find a terminal doing something
	// undocumented, which is worth knowing.
	return key{Kind: keyUnknown, Raw: string(buf)}, len(buf), true
}

// decodeSS3 handles the "SS3" introducer, ESC O.
//
// This exists because of DECCKM, "application cursor keys" — a mode an
// application turns on, after which the arrows arrive as ESC O A instead of
// ESC [ A. A decoder that only knows the CSI form looks perfect on your laptop
// and then breaks the moment someone runs it inside tmux, inside screen, or
// under a readline-based program that left the mode on. The bug report says
// "the arrow keys print a letter", and the letter is the one right here.
func decodeSS3(buf []byte, final bool) (key, int, bool) {
	if len(buf) < 3 {
		return incompleteEscape(buf, final)
	}
	k := key{Raw: string(buf[:3])}
	switch buf[2] {
	case 'A':
		k.Kind = keyUp
	case 'B':
		k.Kind = keyDown
	case 'C':
		k.Kind = keyRight
	case 'D':
		k.Kind = keyLeft
	case 'H':
		k.Kind = keyHome
	case 'F':
		k.Kind = keyEnd
	default:
		// ESC O P/Q/R/S are F1-F4, and the rest of the range is the numeric
		// keypad in application mode. Consumed, reported, not interpreted.
		k.Kind = keyUnknown
	}
	return k, 3, true
}

// pasteEnd terminates a bracketed paste.
var pasteEnd = []byte("\x1b[201~")

// decodeCSI parses a CSI sequence: ESC [ , then parameter bytes, then
// intermediate bytes, then exactly one final byte.
//
// The byte ranges come from ECMA-48 and they are the whole reason this decoder
// can consume a sequence it has never seen before. That matters more than
// recognising every key: terminals send unsolicited CSI replies (cursor
// position, device attributes, focus in/out) that no keyboard produced, and the
// only safe thing to do with them is swallow them whole. Guessing a length, or
// bailing out with "need more bytes", turns a stray status report into a hung
// UI.
func decodeCSI(buf []byte, final bool) (key, int, bool) {
	p := 2
	for p < len(buf) && buf[p] >= 0x30 && buf[p] <= 0x3f { // parameters 0-9 : ; < = > ?
		p++
	}
	q := p
	for q < len(buf) && buf[q] >= 0x20 && buf[q] <= 0x2f { // intermediates, space through /
		q++
	}
	if q >= len(buf) {
		return incompleteEscape(buf, final)
	}

	fb := buf[q]
	if fb < 0x40 || fb > 0x7e {
		// A byte belonging to no CSI class — in practice a control character
		// injected mid-sequence by another writer to the same tty. ECMA-48
		// says the control is executed and the sequence abandoned, so we
		// consume the wreckage up to but NOT including the offending byte and
		// leave it for the next call. Ctrl-C must still work when it lands in
		// the middle of a mouse report.
		return key{Kind: keyUnknown, Raw: string(buf[:q])}, q, true
	}

	n := q + 1
	params := buf[2:p]
	raw := string(buf[:n])

	if fb == '~' && bytes.Equal(params, []byte("200")) {
		return decodePaste(buf, n)
	}
	if (fb == 'M' || fb == 'm') && len(params) > 0 && params[0] == '<' {
		return decodeMouse(params[1:], fb == 'M', raw, n)
	}
	if fb == 'M' && len(params) == 0 {
		// Legacy X10 mouse reporting (mode 1000 without 1006): CSI M followed
		// by three RAW bytes — button+32, column+32, row+32 — which are not
		// part of the CSI sequence at all. We swallow them and report nothing.
		//
		// Deliberately unsupported, not merely unimplemented. A coordinate is
		// one byte biased by 32, so the largest column X10 can express is
		// 255-32 = 223; past that it either clamps or wraps, depending on the
		// emulator, and a click on the right-hand side of a wide window
		// silently reports the wrong cell. SGR (1006) exists precisely because
		// that encoding could not be fixed compatibly, so we ask for SGR and
		// decode only SGR.
		//
		// But we still have to eat those three bytes: they are ordinary bytes
		// in the stream, and letting them fall through to decodeRune types
		// three garbage characters into whatever the user was editing.
		if len(buf) < n+3 {
			return incompleteEscape(buf, final)
		}
		return key{Kind: keyUnknown, Raw: string(buf[:n+3])}, n + 3, true
	}

	ps, ok := csiParams(params)
	if !ok {
		// Parameters we cannot read as numbers — a private-mode reply such as
		// the device-attributes answer \x1b[?1;2c. Well-formed, not a key.
		return key{Kind: keyUnknown, Raw: raw}, n, true
	}

	k := key{Raw: raw}
	switch fb {
	case 'A':
		k.Kind = keyUp
	case 'B':
		k.Kind = keyDown
	case 'C':
		k.Kind = keyRight
	case 'D':
		k.Kind = keyLeft

	// Home and End are the worst keys on the keyboard, and the eight forms
	// below are why. Four independent lineages, all still in the wild:
	//
	//	\x1b[1~ \x1b[4~   VT220 numbering, back when the keys were labelled
	//	                  Find and Select. The Linux console sends this.
	//	\x1b[7~ \x1b[8~   rxvt renumbered them, and everything descended from
	//	                  rxvt kept the renumbering.
	//	\x1b[H  \x1b[F    xterm's own form, in normal cursor mode.
	//	\x1bOH  \x1bOF    the same two keys once DECCKM is on — which tmux and
	//	                  screen switch on for you, without mentioning it.
	//
	// None of these will ever be deprecated, because deprecating one breaks a
	// terminal somebody is still using. Accepting all eight costs six lines;
	// accepting six of them costs you a bug report that reads "Home does
	// nothing when I ssh in from my Mac".
	case 'H':
		k.Kind = keyHome
	case 'F':
		k.Kind = keyEnd

	case 'Z':
		// CSI Z is CBT, "cursor backward tabulation" — what Shift-Tab sends.
		// The Shift flag stays false: it is reserved for a modifier the
		// terminal reported as a parameter, and this sequence has none. The
		// shift is in the Kind, where a caller can switch on it.
		k.Kind = keyShiftTab
		return k, n, true

	case '~':
		switch csiParam(ps, 0, 0) {
		case 1, 7:
			k.Kind = keyHome
		case 4, 8:
			k.Kind = keyEnd
		case 3:
			k.Kind = keyDelete
		case 5:
			k.Kind = keyPageUp
		case 6:
			k.Kind = keyPageDown
		default:
			// 2 is Insert; 11-15 and 17-24 are F1-F12; 201 is a paste
			// terminator with no opener, which means we lost sync and would
			// rather say so than swallow it. All consumed, none interpreted.
			k.Kind = keyUnknown
			return k, n, true
		}

	default:
		k.Kind = keyUnknown
		return k, n, true
	}

	// The modifier is always the SECOND parameter, for both the letter finals
	// (\x1b[1;5A) and the tilde finals (\x1b[3;5~). The first parameter of a
	// letter final is a repeat count when a terminal *emits* CSI as output; as
	// input it is always 1, and ignoring it is correct.
	applyModifier(&k, csiParam(ps, 1, 1))
	return k, n, true
}

// decodePaste handles \x1b[200~ … \x1b[201~ (bracketed paste, mode 2004).
//
// The payload is copied out verbatim and never re-decoded. That is the entire
// point of the mode: pasted text routinely contains 0x1b, 0x0d and control
// bytes, and a decoder that ran them back through itself would turn a pasted
// shell script into arrow keys and a pasted multi-line commit message into
// however many Enter presses it takes to submit half of it. Bracketed paste
// exists because someone pasted into vim in insert mode once.
func decodePaste(buf []byte, start int) (key, int, bool) {
	i := bytes.Index(buf[start:], pasteEnd)
	if i < 0 {
		// Unterminated — and note this returns ok=false even from
		// decodeKeyFinal, the one place that rule is broken.
		//
		// It is broken on purpose. The escape timeout answers a question about
		// a human's fingers; a paste is a machine writing at pipe speed, and a
		// megabyte of it arrives over many reads with gaps that mean nothing.
		// "The timeout expired" is simply not evidence that a paste has ended,
		// so resolving it here would emit half the payload as text and then
		// decode the rest as individual keystrokes — including any newline in
		// it, which in a prompt UI means submitting a half-finished line. A
		// caller that wants a safety valve bounds the buffer and discards an
		// absurdly long unterminated paste. That is a policy, and policies
		// live in the caller, same as the clock does.
		return key{}, 0, false
	}
	n := start + i + len(pasteEnd)
	return key{
		Kind: keyPaste,
		Text: string(buf[start : start+i]),
		Raw:  string(buf[:n]),
	}, n, true
	// Worth knowing: the protocol has no escaping, so a payload that literally
	// contains "\x1b[201~" ends the paste early. That hole is in the protocol,
	// not here — terminals paper over it by filtering ESC out of pasted text
	// before they send it, which is also why you cannot paste an escape
	// sequence into a terminal and have it execute.
}

// ---------------------------------------------------------------------------
// Mouse
// ---------------------------------------------------------------------------

// Modifier and motion bits packed into the SGR button field.
const (
	mouseShift  = 0x04
	mouseAlt    = 0x08
	mouseCtrl   = 0x10
	mouseMotion = 0x20 // set on drag/move reports
)

// decodeMouse parses an SGR (1006) mouse report: \x1b[<b;x;yM for press,
// \x1b[<b;x;ym for release. Only SGR — see the X10 note in decodeCSI.
func decodeMouse(params []byte, press bool, raw string, n int) (key, int, bool) {
	ps, ok := csiParams(params)
	if !ok || len(ps) < 3 || ps[0] < 0 || ps[1] < 0 || ps[2] < 0 {
		return key{Kind: keyUnknown, Raw: raw}, n, true
	}
	b, x, y := ps[0], ps[1], ps[2]

	k := key{Kind: keyMouse, Raw: raw}
	k.Shift = b&mouseShift != 0
	k.Alt = b&mouseAlt != 0
	k.Ctrl = b&mouseCtrl != 0
	k.Mouse = mouseEvent{
		// Strip the modifier and motion bits so Button is a button number and
		// nothing else. Skip this and Ctrl-click reports button 16, a drag with
		// the left button held reports button 32, and every switch statement on
		// Button gets a `default:` that quietly eats real clicks. The wheel
		// bit (0x40) is NOT stripped, because on this wire wheel-up genuinely
		// is button 64 — that is how the encoding names it.
		Button: b &^ (mouseShift | mouseAlt | mouseCtrl | mouseMotion),
		X:      x,
		Y:      y,
		// Motion reports arrive with the final byte 'M', so a drag surfaces as
		// a press at the new cell. That is deliberate: mouseEvent has no Drag
		// field, and a caller reconstructs a drag as "presses with no release
		// in between", which is what it has to track anyway to know which
		// button is down. Motion only arrives at all if the caller asked for
		// mode 1002/1003.
		Press: press,
	}
	return k, n, true
}

// ---------------------------------------------------------------------------
// Parameters
// ---------------------------------------------------------------------------

// csiParams splits CSI parameter bytes into integers, or reports that they are
// not numbers at all. An omitted parameter becomes -1 rather than 0, because
// ECMA-48 says an omitted parameter means "use the default" and the default is
// not always zero — \x1b[;5A is Ctrl-Up, not Ctrl-something-zero.
func csiParams(p []byte) ([]int, bool) {
	if len(p) == 0 {
		return nil, true
	}
	out := make([]int, 0, 4)
	for _, f := range bytes.Split(p, []byte{';'}) {
		// xterm's modifyOtherKeys and kitty's protocol pack sub-parameters
		// after a colon (\x1b[1;5:3A distinguishes press from repeat). Nothing
		// here needs them, and dropping them beats rejecting the sequence: the
		// base key and its modifier are still perfectly readable, and a
		// terminal that opts into a richer protocol should not become a
		// terminal whose arrow keys stopped working.
		if i := bytes.IndexByte(f, ':'); i >= 0 {
			f = f[:i]
		}
		if len(f) == 0 {
			out = append(out, -1)
			continue
		}
		v, err := strconv.Atoi(string(f))
		if err != nil {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

func csiParam(ps []int, i, def int) int {
	if i >= len(ps) || ps[i] < 0 {
		return def
	}
	return ps[i]
}

// applyModifier decodes the xterm modifier parameter.
//
// The encoding is a bitmask PLUS ONE, and the +1 is the single most reliable
// off-by-one in terminal code. Unmodified is 1, not 0, so you have to subtract
// before you mask. Forget it and every modifier reads as the next one along:
// Ctrl-Up (5) comes through with shift set, Shift-Right (2) comes through as
// Alt, and the resulting bug is maddening precisely because arrows still
// "work" — they just work with the wrong modifier, in one terminal, sometimes.
func applyModifier(k *key, mod int) {
	if mod <= 1 {
		return
	}
	m := mod - 1
	k.Shift = m&1 != 0
	k.Alt = m&2 != 0
	k.Ctrl = m&4 != 0
	// Bit 8 is Meta. Ignored: nothing in this decade's terminals emits it,
	// and folding a phantom modifier into Alt would make Alt untrustworthy.
}

// ---------------------------------------------------------------------------
// Debug rendering. Exists so a failing test says "wanted keyUp, got
// keyUnknown raw=\"\\x1bOA\"" instead of "wanted 6, got 20" — which is the
// difference between a fix and an afternoon.
// ---------------------------------------------------------------------------

var keyKindNames = [...]string{
	keyRune:      "keyRune",
	keyEnter:     "keyEnter",
	keyEsc:       "keyEsc",
	keyTab:       "keyTab",
	keyShiftTab:  "keyShiftTab",
	keyBackspace: "keyBackspace",
	keyUp:        "keyUp",
	keyDown:      "keyDown",
	keyLeft:      "keyLeft",
	keyRight:     "keyRight",
	keyHome:      "keyHome",
	keyEnd:       "keyEnd",
	keyPageUp:    "keyPageUp",
	keyPageDown:  "keyPageDown",
	keyDelete:    "keyDelete",
	keyCtrlC:     "keyCtrlC",
	keyCtrlD:     "keyCtrlD",
	keyCtrlL:     "keyCtrlL",
	keyMouse:     "keyMouse",
	keyPaste:     "keyPaste",
	keyUnknown:   "keyUnknown",
}

func (k keyKind) String() string {
	if k >= 0 && int(k) < len(keyKindNames) {
		return keyKindNames[k]
	}
	return fmt.Sprintf("keyKind(%d)", int(k))
}

func (k key) String() string {
	s := k.Kind.String()
	switch k.Kind {
	case keyRune:
		s += fmt.Sprintf("(%q)", k.Rune)
	case keyPaste:
		s += fmt.Sprintf("(%q)", k.Text)
	case keyMouse:
		s += fmt.Sprintf("(button %d at %d,%d press=%v)", k.Mouse.Button, k.Mouse.X, k.Mouse.Y, k.Mouse.Press)
	}
	for _, m := range []struct {
		on   bool
		name string
	}{{k.Ctrl, "ctrl"}, {k.Alt, "alt"}, {k.Shift, "shift"}} {
		if m.on {
			s += "+" + m.name
		}
	}
	return s + fmt.Sprintf(" raw=%q", k.Raw)
}
