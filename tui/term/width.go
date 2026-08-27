package term

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	esc      = 0x1b
	sgrReset = "\x1b[0m"
)

// RuneWidth returns how many terminal columns a rune occupies: 0, 1 or 2.
// Control characters are 0, tab included — a tab has a destination, not a width —
// so callers must expand tabs and split on "\n" before measuring.
func RuneWidth(r rune) int {
	if r < 0x7f { // fast path: nearly everything a TUI measures is ASCII
		if r < 0x20 {
			return 0
		}
		return 1
	}
	if r < 0xa0 {
		return 0 // DEL and the C1 control block
	}
	if isZeroWidth(r) {
		return 0
	}
	if inRanges(wideRanges, r) {
		return 2
	}
	// East Asian *Ambiguous* answers 1, though it is 2 under a CJK locale: reading
	// LANG here would make a layout's shape depend on an environment variable.
	return 1
}

// isZeroWidth must run BEFORE the wide table: U+3099 and U+309A, the combining
// kana voiced-sound marks, sit inside the Katakana block and would otherwise
// measure 2.
func isZeroWidth(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200f:
		return true // ZWSP, ZWNJ, ZWJ, LRM, RLM
	case r == 0xfeff:
		return true // BOM
	}
	// Mc — *spacing* marks — is absent: those advance the cursor.
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r)
}

type wrange struct{ lo, hi rune }

// inRanges binary-searches a sorted, non-overlapping table. The precondition is
// asserted in the tests, not here: this runs on every rune of every frame.
func inRanges(t []wrange, r rune) bool {
	lo, hi := 0, len(t)-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		switch {
		case r < t[mid].lo:
			hi = mid - 1
		case r > t[mid].hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// wideRanges lists the code points that occupy two columns.
//
// MUST STAY SORTED BY lo AND NON-OVERLAPPING: inRanges binary-searches it, so a
// misplaced entry is neither a compile error nor a panic but a character silently
// the wrong width. TestWideRangesSorted makes that loud.
//
// Composed emoji come out wrong and cannot be fixed at this signature: width is a
// property of a grapheme cluster, so a ZWJ family measures 8 instead of 2.
var wideRanges = []wrange{
	{0x1100, 0x115f},
	{0x231a, 0x231b},
	{0x2329, 0x232a},
	{0x23e9, 0x23ec},
	{0x23f0, 0x23f0},
	{0x23f3, 0x23f3},
	{0x25fd, 0x25fe},
	{0x2614, 0x2615},
	{0x2648, 0x2653},
	{0x267f, 0x267f},
	{0x2693, 0x2693},
	{0x26a1, 0x26a1},
	{0x26aa, 0x26ab},
	{0x26bd, 0x26be},
	{0x26c4, 0x26c5},
	{0x26ce, 0x26ce},
	{0x26d4, 0x26d4},
	{0x26ea, 0x26ea},
	{0x26f2, 0x26f3},
	{0x26f5, 0x26f5},
	{0x26fa, 0x26fa},
	{0x26fd, 0x26fd},
	{0x2705, 0x2705},
	{0x270a, 0x270b},
	{0x2728, 0x2728},
	{0x274c, 0x274c},
	{0x274e, 0x274e},
	{0x2753, 0x2755},
	{0x2757, 0x2757},
	{0x2795, 0x2797},
	{0x27b0, 0x27b0},
	{0x27bf, 0x27bf},
	// U+2600-U+27BF is mostly NARROW: widening the whole block breaks every box
	// drawn with a check mark.
	{0x2b1b, 0x2b1c},
	{0x2b50, 0x2b50},
	{0x2b55, 0x2b55},
	{0x2e80, 0x2e99},
	{0x2e9b, 0x2ef3},
	{0x2f00, 0x2fd5},
	{0x2ff0, 0x2ffb},
	{0x3000, 0x303e}, // includes U+3000, two columns of nothing
	{0x3041, 0x3096}, // Hiragana
	{0x3099, 0x30ff}, // combining kana marks (zeroed above) + Katakana
	{0x3105, 0x312f},
	{0x3131, 0x318e},
	{0x3190, 0x31e3},
	{0x31f0, 0x321e},
	{0x3220, 0x3247},
	{0x3250, 0x4dbf},
	{0x4e00, 0xa48c}, // CJK Unified Ideographs + Yi Syllables
	{0xa490, 0xa4c6},
	{0xa960, 0xa97c},
	{0xac00, 0xd7a3}, // Hangul Syllables
	{0xf900, 0xfaff},
	{0xfe10, 0xfe19},
	{0xfe30, 0xfe52},
	{0xfe54, 0xfe66},
	{0xfe68, 0xfe6b},
	{0xff01, 0xff60}, // Fullwidth ASCII forms
	{0xffe0, 0xffe6},
	{0x16fe0, 0x16fe4},
	{0x17000, 0x187f7},
	{0x18800, 0x18cd5},
	{0x1b000, 0x1b152},
	{0x1b164, 0x1b167},
	{0x1b170, 0x1b2fb},
	{0x1f004, 0x1f004},
	{0x1f0cf, 0x1f0cf},
	{0x1f18e, 0x1f18e},
	{0x1f191, 0x1f19a},
	// 2 each, the width a terminal gives an *unpaired* one, so a flag measures 4.
	// Counting each as 1 breaks the lone indicator.
	{0x1f1e6, 0x1f1ff},
	{0x1f200, 0x1f320},
	// The gaps below are text-presentation code points, one narrow column unless
	// a VS16 follows. VS16 is not tracked, so they stay at 1.
	{0x1f32d, 0x1f335},
	{0x1f337, 0x1f37c},
	{0x1f37e, 0x1f393},
	{0x1f3a0, 0x1f3ca},
	{0x1f3cf, 0x1f3d3},
	{0x1f3e0, 0x1f3f0},
	{0x1f3f4, 0x1f3f4},
	{0x1f3f8, 0x1f43e},
	{0x1f440, 0x1f440},
	{0x1f442, 0x1f4fc},
	{0x1f4ff, 0x1f53d},
	{0x1f54b, 0x1f54e},
	{0x1f550, 0x1f567},
	{0x1f57a, 0x1f57a},
	{0x1f595, 0x1f596},
	{0x1f5a4, 0x1f5a4},
	{0x1f5fb, 0x1f64f},
	{0x1f680, 0x1f6c5},
	{0x1f6cc, 0x1f6cc},
	{0x1f6d0, 0x1f6d2},
	{0x1f6d5, 0x1f6d7},
	{0x1f6dc, 0x1f6df},
	{0x1f6eb, 0x1f6ec},
	{0x1f6f4, 0x1f6fc},
	{0x1f7e0, 0x1f7eb},
	{0x1f7f0, 0x1f7f0},
	{0x1f90c, 0x1f93a},
	{0x1f93c, 0x1f945},
	{0x1f947, 0x1f9ff},
	{0x1fa70, 0x1faff},
	{0x20000, 0x2fffd}, // CJK Ext. B-F
	{0x30000, 0x3fffd},
}

// ANSILen returns the byte length of the escape sequence starting at s[i], or 0
// if there is none.
//
//	CSI  ESC [  0x30-0x3f*  0x20-0x2f*  0x40-0x7e
//	OSC  ESC ]  ...         BEL | ESC \
//
// OSC is the one that bites: an OSC-8 hyperlink embeds a URL, so a scanner that
// stops at the first letter eats four bytes of "ESC ]8;;h" and measures
// "ttps://example.com" as visible text. A malformed sequence swallows the rest of
// the string on purpose — emitting the tail prints half an escape literally,
// corrupting every line after it.
func ANSILen(s string, i int) int {
	if i >= len(s) || s[i] != esc {
		return 0
	}
	if i+1 >= len(s) {
		return 1 // a lone trailing ESC
	}
	switch s[i+1] {
	case '[': // CSI
		j := i + 2
		for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
			j++ // parameter bytes
		}
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++ // intermediate bytes
		}
		if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
			return j + 1 - i
		}
		return len(s) - i
	case ']': // OSC
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1 - i // BEL terminator
			}
			if s[j] == esc && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2 - i // ST terminator
			}
		}
		return len(s) - i
	default:
		// ESC ( B is three bytes and comes out one short. None of these carry
		// width or appear in strings a TUI measures.
		return 2
	}
}

// isSGR reports whether seq is a colour/attribute sequence, and whether it is a
// reset. Only SGR carries state that outlives the sequence, and a colour still
// open when the string is cut leaks onto everything printed afterwards —
// including the shell prompt after the program exits.
func isSGR(seq string) (sgr, reset bool) {
	if len(seq) < 3 || seq[0] != esc || seq[1] != '[' || seq[len(seq)-1] != 'm' {
		return false, false
	}
	for _, p := range strings.Split(seq[2:len(seq)-1], ";") {
		if strings.Trim(p, "0") != "" {
			return true, false
		}
	}
	return true, true
}

// DispWidth returns the display width of s in terminal columns, skipping ANSI
// escape sequences. Invalid UTF-8 counts one column per bad byte, which is what
// a terminal draws.
func DispWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := ANSILen(s, i); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += RuneWidth(r)
		i += size
	}
	return w
}

// TruncCols returns a prefix of s occupying at most n columns. It never cuts
// inside an escape sequence or a multi-byte rune, and appends "\x1b[0m" if an
// SGR was still open at the cut. Escape sequences do not count toward n.
//
// Both failures s[:n] produces: a cut inside "\x1b[31m" leaves "\x1b[3" and the
// terminal eats the next character printed anywhere in the program as that
// sequence's final byte; a cut inside a rune emits half a CJK character, whose
// width nothing agrees on. When one column is left and the next rune wants two we
// stop before it and pad the orphan column, because a cell one column short
// shears a table like one too long.
func TruncCols(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	w, open, cut := 0, false, false
	for i := 0; i < len(s); {
		if l := ANSILen(s, i); l > 0 {
			seq := s[i : i+l]
			b.WriteString(seq)
			if sgr, reset := isSGR(seq); sgr {
				open = !reset
			}
			i += l
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := RuneWidth(r)
		if w+rw > n {
			cut = true
			break
		}
		b.WriteString(s[i : i+size]) // copy the bytes; never re-encode the rune
		w += rw
		i += size
	}
	// Close the colour before padding, or the gap is painted with an open
	// background.
	if open {
		b.WriteString(sgrReset)
	}
	// Pad only when we stopped early; a short string is PadCols' business.
	if cut && w < n {
		b.WriteString(strings.Repeat(" ", n-w))
	}
	return b.String()
}

// PadCols returns s padded with spaces to exactly n display columns, or truncated
// via TruncCols if it is wider. Unlike TruncCols it does NOT append a reset when
// s fits: a caller who opens a colour in one cell and closes it in the next is
// doing something legitimate a "helpful" reset would destroy.
func PadCols(s string, n int) string {
	w := DispWidth(s)
	switch {
	case w == n:
		return s
	case w > n:
		return TruncCols(s, n)
	case n <= 0:
		// TruncCols rather than hand strings.Repeat a negative count and panic
		// inside a redraw.
		return TruncCols(s, n)
	}
	return s + strings.Repeat(" ", n-w)
}

// WrapCols hard-wraps s into lines of at most n columns each. ANSI-aware: it
// never splits an escape sequence or a rune, and re-opens the active SGR state at
// the start of each continuation line.
//
// Hard wrap, not word wrap: one unbreakable token must not shove the layout wider
// than its pane. An embedded "\n" forces a break, because a raw newline written
// into a fixed pane escapes the pane.
//
// Re-opening the colour per line is not cosmetic: terminals do not carry SGR
// state across a line something else may redraw, so an un-reopened colour
// vanishes from line two onward once a repaint lands between them.
func WrapCols(s string, n int) []string {
	if n <= 0 {
		// nil is the answer that terminates: the loop below would otherwise emit
		// empty lines forever.
		return nil
	}
	var (
		lines []string
		cur   strings.Builder
		w     int
		state []string // SGR sequences currently in effect, in the order seen
	)
	start := func() {
		cur.Reset()
		w = 0
		for _, seq := range state {
			cur.WriteString(seq)
		}
	}
	end := func() string {
		if len(state) > 0 {
			cur.WriteString(sgrReset)
		}
		return cur.String()
	}
	start()
	for i := 0; i < len(s); {
		if l := ANSILen(s, i); l > 0 {
			seq := s[i : i+l]
			cur.WriteString(seq)
			if sgr, reset := isSGR(seq); sgr {
				if reset {
					state = state[:0]
				} else {
					state = append(state, seq)
				}
			}
			i += l
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\n' {
			lines = append(lines, end())
			start()
			i += size
			continue
		}
		rw := RuneWidth(r)
		if w+rw > n {
			if w == 0 {
				// A rune wider than the whole pane can never fit, so "break the line
				// and retry" is an infinite loop. Emit it alone and overflow by one
				// column: an overflowing glyph is visible, a dropped one is not.
				// Setting w past n rather than flushing keeps a spurious empty line
				// off the end.
				cur.WriteString(s[i : i+size])
				w = rw
				i += size
				continue
			}
			lines = append(lines, end())
			start()
			continue // retry the same rune against a fresh line
		}
		cur.WriteString(s[i : i+size])
		w += rw
		i += size
	}
	// Always append the tail, even when empty: WrapCols("") is one blank line so a
	// caller drawing a box still gets a row. The loop only flushes when something
	// did not fit, so this cannot bolt an empty line onto an exact wrap point.
	return append(lines, end())
}
