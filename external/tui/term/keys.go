package term

import (
	"bytes"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// Two invariants callers depend on. Progress: whenever ok is true, n > 0, because
// a zero-length decode live-locks the read loop. Honesty: ok=false means "this is
// a prefix", never "unrecognised" — an unrecognised but well-formed sequence is
// consumed whole as KeyUnknown, or the caller reads forever.

type KeyKind int

const (
	KeyRune KeyKind = iota // a printable character, in Key.Rune
	KeyEnter
	KeyEsc
	KeyTab
	KeyShiftTab
	KeyBackspace
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyDelete
	KeyCtrlC
	KeyCtrlD
	KeyCtrlL
	KeyMouse // details in Key.Mouse
	KeyPaste // bracketed-paste payload, in Key.Text

	// KeyUnknown is "bytes we consumed and will not act on". Raw holds the exact
	// bytes, which is the difference between a mystery and a bug report.
	KeyUnknown
)

type MouseEvent struct {
	Button int  // 0 left, 1 middle, 2 right, 64 wheel-up, 65 wheel-down
	X, Y   int  // 1-based columns/rows exactly as the terminal reports them
	Press  bool // true = press, false = release
}

type Key struct {
	Kind  KeyKind
	Rune  rune       // KeyRune
	Text  string     // KeyPaste
	Mouse MouseEvent // KeyMouse
	Ctrl  bool       // a modifier the terminal reported (e.g. \x1b[1;5A)
	Alt   bool
	Shift bool
	Raw   string // the exact bytes consumed, for debugging and for KeyUnknown
}

// There are two entry points because Escape is 0x1b and so is the first byte of
// every arrow key, mouse report and paste: a buffer ending in a bare 0x1b is
// either the Escape key or "\x1b[A" caught mid-burst, and only TIME separates
// them. The clock stays out of the decoder — the right timeout is a property of
// the link, generous on a local pty and far too short over a saturated ssh
// session — and a decoder that owns one stops being testable, because "50ms
// elapsed" can only be tested with a sleep.

// DecodeKey decodes one key from the front of buf. Returns the key, how many
// bytes it consumed, and ok=false when buf holds only the beginning of a
// sequence and the caller must read more bytes.
func DecodeKey(buf []byte) (Key, int, bool) { return decodeOne(buf, false) }

// DecodeKeyFinal is DecodeKey for the case where the caller has waited and no
// more bytes are coming. It makes progress on any non-empty buffer, with exactly
// one exception: an unterminated bracketed paste. See decodePaste.
func DecodeKeyFinal(buf []byte) (Key, int, bool) { return decodeOne(buf, true) }

func decodeOne(buf []byte, final bool) (Key, int, bool) {
	if len(buf) == 0 {
		return Key{}, 0, false
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

func decodeRune(buf []byte, final bool) (Key, int, bool) {
	// utf8.DecodeRune returns (RuneError, 1) for "invalid" and "not finished yet"
	// alike. Skipping FullRune shows a replacement character every time an emoji
	// straddles a read boundary.
	if !utf8.FullRune(buf) {
		if !final {
			return Key{}, 0, false
		}
		// The rest never came: a disconnect mid-character. Consume the fragment so
		// the loop progresses, but do not invent a character nobody typed.
		return Key{Kind: KeyUnknown, Raw: string(buf)}, len(buf), true
	}
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size == 1 {
		// A byte that can never begin a rune. Skip exactly one; U+FFFD here would
		// push a glyph into the user's document.
		return Key{Kind: KeyUnknown, Raw: string(buf[:1])}, 1, true
	}
	return Key{Kind: KeyRune, Rune: r, Raw: string(buf[:size])}, size, true
}

func decodeControl(buf []byte) Key {
	b := buf[0]
	k := Key{Raw: string(buf[:1])}

	// Ctrl-M and Enter are the same byte, as are Ctrl-I/Tab and Ctrl-H/Backspace.
	// The terminal cannot tell them apart, so the named key wins.
	switch b {
	case 0x0d, 0x0a: // CR (raw mode never translates it) and LF
		k.Kind = KeyEnter
	case 0x09:
		k.Kind = KeyTab
	case 0x7f, 0x08:
		// Both, always: which one arrives depends on the emulator, on stty erase
		// and on tmux, and treating only one as Backspace prints ^H over ssh.
		k.Kind = KeyBackspace
	case 0x03:
		k.Kind = KeyCtrlC
	case 0x04:
		k.Kind = KeyCtrlD
	case 0x0c:
		k.Kind = KeyCtrlL
	case 0x00:
		// NUL is Ctrl-Space; a space with Ctrl set is what applications bind.
		k.Kind, k.Rune, k.Ctrl = KeyRune, ' ', true
	default:
		switch {
		case b >= 0x01 && b <= 0x1a:
			// Lowercase, because Ctrl-A and Ctrl-Shift-A are the same byte and the
			// shifted form does not exist on the wire.
			k.Kind, k.Rune, k.Ctrl = KeyRune, rune('a'+b-1), true
		case b >= 0x1c && b <= 0x1f:
			k.Kind, k.Rune, k.Ctrl = KeyRune, rune(b+0x40), true
		default:
			// Unreachable: the only remaining byte under 0x20 is 0x1b, which
			// decodeOne routes to decodeEscape.
			k.Kind = KeyUnknown
		}
	}
	return k
}

func decodeEscape(buf []byte, final bool) (Key, int, bool) {
	if len(buf) == 1 {
		if !final {
			return Key{}, 0, false
		}
		return Key{Kind: KeyEsc, Raw: "\x1b"}, 1, true
	}

	switch buf[1] {
	case '[':
		return decodeCSI(buf, final)
	case 'O':
		return decodeSS3(buf, final)
	}

	// ESC followed by anything else is Alt+that key ("metaSendsEscape"), which
	// makes Alt-a and "Escape, then a" byte-identical; within one read Alt is
	// overwhelmingly the likelier intent. Recursing gets Alt-Enter,
	// Alt-Backspace and ESC ESC [ A (Alt-Up, which tmux forwards) right for free.
	k, n, ok := decodeOne(buf[1:], final)
	if !ok {
		return Key{}, 0, false
	}
	k.Alt = true
	k.Raw = string(buf[:n+1])
	return k, n + 1, true
}

// incompleteEscape answers "this definitely began an escape sequence and
// definitely has not finished". Once the caller has waited there is no keystroke
// to report, but it must still be consumed or the caller asks forever.
func incompleteEscape(buf []byte, final bool) (Key, int, bool) {
	if !final {
		return Key{}, 0, false
	}
	return Key{Kind: KeyUnknown, Raw: string(buf)}, len(buf), true
}

// decodeSS3 handles the SS3 introducer, ESC O. It exists because of DECCKM
// ("application cursor keys"), after which arrows arrive as ESC O A instead of
// ESC [ A — so a CSI-only decoder looks perfect until it runs inside tmux or
// screen, and then the arrow keys print a letter.
func decodeSS3(buf []byte, final bool) (Key, int, bool) {
	if len(buf) < 3 {
		return incompleteEscape(buf, final)
	}
	k := Key{Raw: string(buf[:3])}
	switch buf[2] {
	case 'A':
		k.Kind = KeyUp
	case 'B':
		k.Kind = KeyDown
	case 'C':
		k.Kind = KeyRight
	case 'D':
		k.Kind = KeyLeft
	case 'H':
		k.Kind = KeyHome
	case 'F':
		k.Kind = KeyEnd
	default:
		// F1-F4 and the keypad in application mode: consumed, not interpreted.
		k.Kind = KeyUnknown
	}
	return k, 3, true
}

var pasteEnd = []byte("\x1b[201~")

// decodeCSI parses ESC [ , parameter bytes, intermediate bytes, one final byte.
// Knowing the ECMA-48 byte classes rather than a list of keys is what lets this
// consume a sequence it has never seen: terminals send unsolicited CSI replies no
// keyboard produced, and guessing a length or bailing with "need more bytes"
// turns a stray status report into a hung UI.
func decodeCSI(buf []byte, final bool) (Key, int, bool) {
	p := 2
	for p < len(buf) && buf[p] >= 0x30 && buf[p] <= 0x3f { // parameters
		p++
	}
	q := p
	for q < len(buf) && buf[q] >= 0x20 && buf[q] <= 0x2f { // intermediates
		q++
	}
	if q >= len(buf) {
		return incompleteEscape(buf, final)
	}

	fb := buf[q]
	if fb < 0x40 || fb > 0x7e {
		// A byte belonging to no CSI class — a control character injected
		// mid-sequence by another writer to the same tty. ECMA-48 abandons the
		// sequence and executes the control, so consume up to but NOT including it:
		// Ctrl-C must still work when it lands inside a mouse report.
		return Key{Kind: KeyUnknown, Raw: string(buf[:q])}, q, true
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
		// Legacy X10 mouse reporting: CSI M plus three RAW bytes outside the CSI
		// sequence. Deliberately unsupported — a coordinate biased by 32 in one byte
		// cannot name a column past 223, so a click on the right of a wide window
		// reports the wrong cell. The three bytes still have to be eaten: falling
		// through to decodeRune types three garbage characters into whatever the
		// user was editing.
		if len(buf) < n+3 {
			return incompleteEscape(buf, final)
		}
		return Key{Kind: KeyUnknown, Raw: string(buf[:n+3])}, n + 3, true
	}

	ps, ok := csiParams(params)
	if !ok {
		// Parameters that are not numbers — a private-mode reply such as the
		// device-attributes answer \x1b[?1;2c. Well-formed, not a key.
		return Key{Kind: KeyUnknown, Raw: raw}, n, true
	}

	k := Key{Raw: raw}
	switch fb {
	case 'A':
		k.Kind = KeyUp
	case 'B':
		k.Kind = KeyDown
	case 'C':
		k.Kind = KeyRight
	case 'D':
		k.Kind = KeyLeft

	// Home and End have four lineages, all still in the wild: \x1b[1~/\x1b[4~
	// (VT220, the Linux console), \x1b[7~/\x1b[8~ (rxvt), \x1b[H/\x1b[F (xterm),
	// \x1bOH/\x1bOF (the same two under DECCKM, which tmux switches on silently).
	case 'H':
		k.Kind = KeyHome
	case 'F':
		k.Kind = KeyEnd

	case 'Z':
		// CSI Z is CBT, what Shift-Tab sends. The Shift flag stays false — it is
		// reserved for a modifier the terminal reported as a parameter, and this
		// sequence has none. The shift is in the Kind.
		k.Kind = KeyShiftTab
		return k, n, true

	case '~':
		switch csiParam(ps, 0, 0) {
		case 1, 7:
			k.Kind = KeyHome
		case 4, 8:
			k.Kind = KeyEnd
		case 3:
			k.Kind = KeyDelete
		case 5:
			k.Kind = KeyPageUp
		case 6:
			k.Kind = KeyPageDown
		default:
			// Insert, the function keys, and a paste terminator with no opener —
			// which means lost sync and is better said than swallowed.
			k.Kind = KeyUnknown
			return k, n, true
		}

	default:
		k.Kind = KeyUnknown
		return k, n, true
	}

	// The modifier is always the SECOND parameter, for the letter finals
	// (\x1b[1;5A) and the tilde finals (\x1b[3;5~) alike.
	applyModifier(&k, csiParam(ps, 1, 1))
	return k, n, true
}

// decodePaste handles \x1b[200~ ... \x1b[201~ (bracketed paste, mode 2004).
//
// The payload is copied verbatim and never re-decoded, which is the point of the
// mode: pasted text routinely contains 0x1b and 0x0d, and re-running it through
// the decoder turns a pasted script into arrow keys. The protocol has no
// escaping, so a payload literally containing "\x1b[201~" ends the paste early;
// terminals paper over that by filtering ESC out of pasted text.
func decodePaste(buf []byte, start int) (Key, int, bool) {
	i := bytes.Index(buf[start:], pasteEnd)
	if i < 0 {
		// Unterminated: ok=false even from DecodeKeyFinal, the one place that rule is
		// broken, and broken on purpose. A paste is a machine writing at pipe speed
		// over many reads, so an expired timeout is not evidence it ended; resolving
		// here would emit half the payload as text and decode the rest as keystrokes
		// — including a newline, which in a prompt UI submits a half-finished line.
		return Key{}, 0, false
	}
	n := start + i + len(pasteEnd)
	return Key{
		Kind: KeyPaste,
		Text: string(buf[start : start+i]),
		Raw:  string(buf[:n]),
	}, n, true
}

// Modifier and motion bits packed into the SGR button field.
const (
	mouseShift  = 0x04
	mouseAlt    = 0x08
	mouseCtrl   = 0x10
	mouseMotion = 0x20 // set on drag/move reports
)

// decodeMouse parses an SGR (1006) mouse report: \x1b[<b;x;yM for press,
// \x1b[<b;x;ym for release. Only SGR — see the X10 note in decodeCSI.
func decodeMouse(params []byte, press bool, raw string, n int) (Key, int, bool) {
	ps, ok := csiParams(params)
	if !ok || len(ps) < 3 || ps[0] < 0 || ps[1] < 0 || ps[2] < 0 {
		return Key{Kind: KeyUnknown, Raw: raw}, n, true
	}
	b, x, y := ps[0], ps[1], ps[2]

	k := Key{Kind: KeyMouse, Raw: raw}
	k.Shift = b&mouseShift != 0
	k.Alt = b&mouseAlt != 0
	k.Ctrl = b&mouseCtrl != 0
	k.Mouse = MouseEvent{
		// Strip the modifier and motion bits, or Ctrl-click reports button 16 and a
		// left-button drag reports 32, and every switch on Button grows a default
		// that quietly eats real clicks. The wheel bit (0x40) is NOT stripped: on
		// this wire wheel-up genuinely is button 64.
		Button: b &^ (mouseShift | mouseAlt | mouseCtrl | mouseMotion),
		X:      x,
		Y:      y,
		// Motion reports carry the final byte 'M', so a drag surfaces as a press at
		// the new cell; a caller reconstructs it from presses with no release
		// between.
		Press: press,
	}
	return k, n, true
}

// csiParams splits CSI parameter bytes into integers, or reports that they are
// not numbers at all. An omitted parameter becomes -1 rather than 0: ECMA-48
// says omitted means "use the default", and the default is not always zero —
// \x1b[;5A is Ctrl-Up, not Ctrl-something-zero.
func csiParams(p []byte) ([]int, bool) {
	if len(p) == 0 {
		return nil, true
	}
	out := make([]int, 0, 4)
	for _, f := range bytes.Split(p, []byte{';'}) {
		// xterm's modifyOtherKeys and kitty's protocol pack sub-parameters after a
		// colon. Dropping them beats rejecting the sequence: a terminal that opts
		// into a richer protocol must not lose its arrow keys.
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

// applyModifier decodes the xterm modifier parameter, which is a bitmask PLUS
// ONE — unmodified is 1, not 0 — so it must be decremented before masking.
// Forget that and every modifier reads as the next one along: Ctrl-Up arrives
// with shift set, and arrows still "work", just with the wrong modifier.
func applyModifier(k *Key, mod int) {
	if mod <= 1 {
		return
	}
	m := mod - 1
	k.Shift = m&1 != 0
	k.Alt = m&2 != 0
	k.Ctrl = m&4 != 0
	// Bit 8 is Meta, ignored: folding a phantom modifier into Alt would make Alt
	// untrustworthy.
}

// Debug rendering, so a failing test names the key rather than its ordinal.
var keyKindNames = [...]string{
	KeyRune:      "KeyRune",
	KeyEnter:     "KeyEnter",
	KeyEsc:       "KeyEsc",
	KeyTab:       "KeyTab",
	KeyShiftTab:  "KeyShiftTab",
	KeyBackspace: "KeyBackspace",
	KeyUp:        "KeyUp",
	KeyDown:      "KeyDown",
	KeyLeft:      "KeyLeft",
	KeyRight:     "KeyRight",
	KeyHome:      "KeyHome",
	KeyEnd:       "KeyEnd",
	KeyPageUp:    "KeyPageUp",
	KeyPageDown:  "KeyPageDown",
	KeyDelete:    "KeyDelete",
	KeyCtrlC:     "KeyCtrlC",
	KeyCtrlD:     "KeyCtrlD",
	KeyCtrlL:     "KeyCtrlL",
	KeyMouse:     "KeyMouse",
	KeyPaste:     "KeyPaste",
	KeyUnknown:   "KeyUnknown",
}

func (k KeyKind) String() string {
	if k >= 0 && int(k) < len(keyKindNames) {
		return keyKindNames[k]
	}
	return fmt.Sprintf("KeyKind(%d)", int(k))
}

func (k Key) String() string {
	s := k.Kind.String()
	switch k.Kind {
	case KeyRune:
		s += fmt.Sprintf("(%q)", k.Rune)
	case KeyPaste:
		s += fmt.Sprintf("(%q)", k.Text)
	case KeyMouse:
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
