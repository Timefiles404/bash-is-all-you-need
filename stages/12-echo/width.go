// Stage 06 — display width.
//
// Three different numbers get confused for each other, and only one of them is
// the one a terminal cares about:
//
//	len("你好")                     == 6   bytes
//	utf8.RuneCountInString("你好")  == 2   runes
//	dispWidth("你好")               == 4   COLUMNS  ← the only one that lays out
//
// Neither of the first two is columns. This matters the first time somebody
// pastes a Chinese filename into your beautifully aligned `%-20s` table and the
// whole thing shears in half — `%-20s` pads to twenty *bytes*, so a name with
// six CJK characters (18 bytes, 12 columns) gets no padding at all and the next
// column starts eight places early. Every other row still looks fine, which is
// why the bug reads as "the terminal is broken" rather than "my format verb is
// counting the wrong thing".
//
// The same file also has to survive ANSI escape sequences, because the strings
// a TUI measures are usually already coloured. "\x1b[31mred\x1b[0m" is 12 bytes
// and 3 columns; measure the bytes and every coloured cell in the table is nine
// places too wide. So every function here walks the string with an escape-aware
// scanner instead of ranging over it.
//
// Everything is standard library. There is a good third-party package for this
// (go-runewidth); the point of writing it out is that when your table shears you
// will know which of the three numbers went wrong.
package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	esc      = 0x1b
	sgrReset = "\x1b[0m"
)

// ---------------------------------------------------------------------------
// runeWidth
// ---------------------------------------------------------------------------

// runeWidth returns how many terminal columns a rune occupies: 0, 1 or 2.
//
// Control characters count as 0. That is a deliberate choice and it has a sharp
// edge: TAB IS ZERO. A tab does not have a width, it has a *destination* — its
// effect depends on the cursor's current column, which a pure function of one
// rune cannot know. Guessing 8 is wrong whenever the caller is not sitting on a
// multiple of 8; guessing 1 is wrong always. Callers must expand tabs before
// measuring. Newline and carriage return are 0 for the same reason: they are
// cursor motion, not ink — which also means dispWidth of a multi-line string is
// meaningless. Split on "\n" first, or hand it to wrapCols, which breaks there
// itself.
func runeWidth(r rune) int {
	// Fast path first: nearly everything a TUI measures is printable ASCII, and
	// it would be a shame to binary-search a 100-entry table to discover that
	// 'a' is one column wide.
	if r < 0x7f {
		if r < 0x20 {
			return 0 // C0 controls, \t and \n among them — see above
		}
		return 1
	}
	if r < 0xa0 {
		return 0 // DEL (0x7f) and the C1 control block
	}
	if isZeroWidth(r) {
		return 0
	}
	if inRanges(wideRanges, r) {
		return 2
	}
	// Everything unclaimed is one column. That includes East Asian *Ambiguous*
	// characters — Greek, Cyrillic, box drawing, "…", "±" — which are 2 columns
	// under a CJK locale and 1 everywhere else. We always answer 1: the
	// alternative is reading LANG at measure time, and a layout whose shape
	// depends on an environment variable is a worse bug than a box that is
	// occasionally a column narrow.
	return 1
}

// isZeroWidth covers the characters that occupy no column of their own because
// they attach to, or annotate, the character before them.
//
// It must run BEFORE the wide table, not after. U+3099 and U+309A — the
// combining kana voiced-sound marks — sit inside the Katakana block and would
// otherwise be measured as 2. They stack onto the preceding kana. They are 0.
func isZeroWidth(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200f:
		// ZWSP, ZWNJ, ZWJ (U+200D), LRM, RLM. The ZWJ is swept up by the range
		// rather than checked on its own — see the emoji note on wideRanges for
		// why zeroing it is necessary and nowhere near sufficient.
		return true
	case r == 0xfeff:
		return true // BOM / zero-width no-break space
	}
	// Mn = non-spacing mark (the accent in "é"), Me = enclosing mark.
	// Mc — *spacing* combining marks, used by Devanagari and friends — is
	// deliberately absent: those do advance the cursor.
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r)
}

// ---------------------------------------------------------------------------
// The wide table
// ---------------------------------------------------------------------------

type wrange struct{ lo, hi rune }

// inRanges binary-searches a sorted, non-overlapping range table.
//
// The obvious implementation is a chain of `if r >= x && r <= y`, and it is fine
// right until the table has a hundred entries and runs on every rune of every
// frame of a redraw: seven comparisons instead of a hundred. The precondition —
// sorted and disjoint — is not checked here because checking would cost more
// than the search; it is asserted once in the test suite, which is the right
// place for an invariant about a constant.
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

// wideRanges lists the code points that occupy two columns: East Asian Wide and
// Fullwidth, plus the emoji that terminals draw double-width.
//
// MUST STAY SORTED BY lo AND NON-OVERLAPPING. inRanges binary-searches it, so a
// misplaced entry produces neither a compile error nor a panic — it produces a
// character that is silently the wrong width, in one font, on one machine.
// TestWideRangesSorted exists to make that failure loud.
//
// EMOJI HONESTY. Single-code-point emoji below are measured correctly. Composed
// ones are not, and cannot be by anything with runeWidth's signature:
//
//	"👨‍👩‍👧‍👦"  a family: 4 people joined by 3 ZWJs. One glyph, 2 columns on
//	           screen. We measure 4×2 + 3×0 = 8. Six columns too wide.
//	"🇯🇵"      a flag: 2 regional indicators. One glyph, 2 columns on screen.
//	           We measure 4. Two columns too wide.
//	"👍🏽"      skin tone: base + modifier. One glyph, 2 columns. We measure 4.
//	"❤️"       text-presentation base + VS16. One glyph, 2 columns. We measure 1.
//
// All four are the same bug from both directions: width is a property of a
// *grapheme cluster*, and runeWidth is handed one rune at a time. Fixing it
// needs UAX #29 extended grapheme cluster segmentation — split the string into
// clusters, then width each cluster from its base plus the emoji-presentation
// rules — which is a table-driven state machine about the size of this whole
// file, and which is why the good third-party packages exist.
//
// This is written down rather than hidden because the failure is otherwise
// inscrutable: the table looks perfect for a week and then one user with an
// emoji in a commit message reports ragged borders. If you put user-supplied
// emoji in a fixed layout, you need the real thing.
var wideRanges = []wrange{
	{0x1100, 0x115f}, // Hangul Jamo, initial consonants
	{0x231a, 0x231b}, // ⌚⌛
	{0x2329, 0x232a}, // 〈〉
	{0x23e9, 0x23ec},
	{0x23f0, 0x23f0},
	{0x23f3, 0x23f3},
	{0x25fd, 0x25fe},
	{0x2614, 0x2615},
	{0x2648, 0x2653}, // zodiac
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
	// U+2600–U+27BF is mostly NARROW, so it is picked apart above rather than
	// widened in one blanket range. Only the entries listed have
	// Emoji_Presentation=Yes and get drawn double-width; ✓ (U+2713), ★, ☺, ✗ and
	// the rest of the dingbats are one column. Widening the whole block is the
	// usual shortcut and it breaks every box someone drew with ✓ and ✗.
	{0x2b1b, 0x2b1c},
	{0x2b50, 0x2b50},
	{0x2b55, 0x2b55},
	{0x2e80, 0x2e99}, // CJK Radicals Supplement
	{0x2e9b, 0x2ef3},
	{0x2f00, 0x2fd5}, // Kangxi Radicals
	{0x2ff0, 0x2ffb}, // Ideographic Description Characters
	{0x3000, 0x303e}, // CJK Symbols and Punctuation. Includes U+3000, the
	//                   ideographic space: two columns of nothing, and the
	//                   reason TrimSpace-then-measure can still come out short.
	{0x3041, 0x3096}, // Hiragana
	{0x3099, 0x30ff}, // combining kana marks (zeroed above) + Katakana
	{0x3105, 0x312f}, // Bopomofo
	{0x3131, 0x318e}, // Hangul Compatibility Jamo
	{0x3190, 0x31e3}, // Kanbun, CJK strokes
	{0x31f0, 0x321e}, // Katakana phonetic extensions, enclosed CJK
	{0x3220, 0x3247},
	{0x3250, 0x4dbf}, // enclosed CJK + CJK Unified Ideographs Extension A
	{0x4e00, 0xa48c}, // CJK Unified Ideographs + Yi Syllables
	{0xa490, 0xa4c6}, // Yi Radicals
	{0xa960, 0xa97c}, // Hangul Jamo Extended-A
	{0xac00, 0xd7a3}, // Hangul Syllables — 한글
	{0xf900, 0xfaff}, // CJK Compatibility Ideographs
	{0xfe10, 0xfe19}, // vertical forms
	{0xfe30, 0xfe52}, // CJK Compatibility Forms
	{0xfe54, 0xfe66},
	{0xfe68, 0xfe6b},
	{0xff01, 0xff60}, // Fullwidth ASCII forms: Ａ is 3 bytes, 1 rune, 2 columns
	{0xffe0, 0xffe6}, // fullwidth currency and signs
	{0x16fe0, 0x16fe4},
	{0x17000, 0x187f7}, // Tangut
	{0x18800, 0x18cd5}, // Tangut components
	{0x1b000, 0x1b152}, // Kana Supplement / Extended-A
	{0x1b164, 0x1b167},
	{0x1b170, 0x1b2fb}, // Nushu
	{0x1f004, 0x1f004}, // 🀄
	{0x1f0cf, 0x1f0cf}, // 🃏
	{0x1f18e, 0x1f18e},
	{0x1f191, 0x1f19a},
	// Regional indicators 🇦–🇿. Each is 2 here, matching Unicode's
	// Emoji_Presentation property and the width a terminal gives an *unpaired*
	// one. A flag is a pair rendered as a single 2-column glyph, so flags
	// measure 4. Counting each as 1 would fix flags and break the lone
	// indicator — which is exactly what you are left holding when something
	// cuts a flag in half, i.e. the case truncCols exists to prevent.
	{0x1f1e6, 0x1f1ff},
	{0x1f200, 0x1f320}, // enclosed ideographic supplement, then early emoji
	// U+1F300–U+1F9FF is likewise broken into sub-ranges rather than widened
	// wholesale: the gaps (U+1F321 🌡, U+1F336 🌶, U+1F397 …) are the
	// text-presentation code points, which render as one narrow column unless
	// the string carries a VS16 after them. We do not track VS16, so those stay
	// at 1 — under-measuring a rare glyph beats over-measuring a common one.
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
	{0x1f7e0, 0x1f7eb}, // coloured circles and squares
	{0x1f7f0, 0x1f7f0},
	{0x1f90c, 0x1f93a},
	{0x1f93c, 0x1f945},
	{0x1f947, 0x1f9ff}, // … out to the end of Supplemental Symbols
	{0x1fa70, 0x1faff}, // the 2019-and-later additions
	{0x20000, 0x2fffd}, // CJK Ext. B–F: rare Han, still two columns
	{0x30000, 0x3fffd}, // CJK Ext. G and beyond
}

// ---------------------------------------------------------------------------
// ANSI escape scanning
// ---------------------------------------------------------------------------

// ansiLen returns the byte length of the escape sequence starting at s[i], or 0
// if there is no escape sequence there.
//
// This is the load-bearing primitive of the file: everything below is "walk the
// string, skip what ansiLen claims, measure the rest". It has to be a real
// scanner rather than a regexp or an "is it a letter yet" loop, because the two
// forms that actually appear in terminal output terminate differently:
//
//	CSI  ESC [  0x30-0x3f*  0x20-0x2f*  0x40-0x7e     ← colour, cursor moves
//	OSC  ESC ]  ...         BEL | ESC \               ← window title, hyperlinks
//
// OSC is the one that bites. An OSC-8 hyperlink embeds a URL, and a URL is full
// of letters — a scanner that stops at the first letter eats four bytes of
// "ESC ]8;;h" and then measures "ttps://example.com" as visible text, so every
// hyperlinked cell in the table comes out eighteen columns too wide.
//
// A truncated or malformed sequence swallows the rest of the string. That is
// deliberate: the alternative is emitting the tail as visible text, and half an
// escape printed literally corrupts the terminal for every line after it.
//
// exec.go's sanitize() recognises the same grammar with a regexp, and that is
// the right tool there: it strips every escape from a blob in one pass, because
// tool output going to the model must not carry colour at all. Here we need the
// opposite shape — the LENGTH of the sequence at a given offset — so the walk
// can keep a running column count as it goes. A regexp cannot answer that
// without re-scanning from the start for every rune.
func ansiLen(s string, i int) int {
	if i >= len(s) || s[i] != esc {
		return 0
	}
	if i+1 >= len(s) {
		return 1 // a lone trailing ESC: no width, no payload
	}
	switch s[i+1] {
	case '[': // CSI
		j := i + 2
		for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
			j++ // parameter bytes: digits, ';', and the private-use markers
		}
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++ // intermediate bytes
		}
		if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
			return j + 1 - i // final byte: 'm' is SGR, 'H'/'J'/'K'/… are the rest
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
		// Two-byte escapes: ESC M (reverse index), ESC 7 / ESC 8 (save and
		// restore cursor), ESC ( B (charset selection — three bytes, and we get
		// that one wrong by one). None carry width and none show up in strings a
		// TUI measures, so two is the pragmatic answer rather than the correct
		// one, and this comment is the receipt.
		return 2
	}
}

// isSGR reports whether seq is a colour/attribute sequence, and whether it is a
// reset.
//
// The distinction matters because only SGR carries state that outlives the
// sequence. A cursor move is an event; a colour is a *mode*, and a mode still
// open when you cut the string leaks onto everything printed afterwards —
// including the shell prompt after your program exits, which is how a user ends
// up with a permanently red terminal and no idea which program did it.
//
// "Reset" means an SGR whose parameters are empty or all zero: \x1b[m, \x1b[0m,
// \x1b[00m. Everything else counts as opening state, including \x1b[39m
// (default foreground), which genuinely does clear the colour. That errs toward
// closing too often, and a redundant \x1b[0m is invisible where a missing one
// is not.
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

// ---------------------------------------------------------------------------
// dispWidth
// ---------------------------------------------------------------------------

// dispWidth returns the display width of s in terminal columns, skipping any
// ANSI escape sequences it contains.
//
// Invalid UTF-8 decodes to U+FFFD one byte at a time and is counted as one
// column per bad byte, which is what a terminal draws for it. That keeps a
// corrupted byte from silently shrinking a column instead of making a visible
// mess — a layout that quietly absorbs bad input is a layout that hides it.
func dispWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += size
	}
	return w
}

// ---------------------------------------------------------------------------
// truncCols
// ---------------------------------------------------------------------------

// truncCols returns a prefix of s occupying at most n columns. It never cuts
// inside an ANSI escape sequence and never leaves the terminal in a coloured
// state: if any SGR sequence was still open at the cut, it appends "\x1b[0m".
// Escape sequences do not count toward n.
//
// Two failure modes this exists to prevent, both of which s[:n] produces:
//
//  1. A cut inside "\x1b[31m" leaves "\x1b[3" in the output. The terminal takes
//     the next character you print as that sequence's final byte and eats it.
//     One missing letter, several lines later, from a completely different part
//     of the program.
//
//  2. A cut inside a multi-byte rune emits half a CJK character — a replacement
//     glyph whose width nothing agrees on, which desynchronises the column count
//     for the rest of the line.
//
// The wide-character boundary is the interesting case. If one column is left and
// the next rune wants two, we stop before it and pad the orphan column with a
// space. Returning n-1 columns would be as broken as returning n+1: a cell one
// short shears the table exactly like a cell one long, and callers reasonably
// read "at most n" from a truncator as "exactly n" whenever it truncated.
func truncCols(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	w, open, cut := 0, false, false
	for i := 0; i < len(s); {
		if l := ansiLen(s, i); l > 0 {
			seq := s[i : i+l]
			b.WriteString(seq)
			if sgr, reset := isSGR(seq); sgr {
				open = !reset
			}
			i += l
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runeWidth(r)
		if w+rw > n {
			cut = true
			break
		}
		b.WriteString(s[i : i+size]) // copy the bytes; never re-encode the rune
		w += rw
		i += size
	}
	// Close the colour before padding, not after. Padding under an open
	// background colour paints a coloured block into the gap, which is precisely
	// the artefact that makes a truncated cell look like a rendering bug.
	if open {
		b.WriteString(sgrReset)
	}
	// Pad only when we actually stopped early. A short string is left alone —
	// padding it is padCols' job, and conflating the two makes truncCols
	// surprising. n-w is always exactly 1 here (the only way to break with
	// w < n is rw == 2 and n-w == 1), but the general form makes that visible.
	if cut && w < n {
		b.WriteString(strings.Repeat(" ", n-w))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// padCols
// ---------------------------------------------------------------------------

// padCols returns s padded with spaces to exactly n display columns, or
// truncated (via truncCols) if it is wider.
//
// Note the asymmetry with truncCols: when s fits, padCols does NOT append a
// reset even if s leaves an SGR open. Nothing was cut, so nothing was broken,
// and a caller who opens a colour in one cell and closes it in the next is doing
// something legitimate that a "helpful" reset would silently destroy. The price
// is that padding a string with an open background colour paints coloured
// spaces — that is the caller's colour, faithfully applied.
func padCols(s string, n int) string {
	w := dispWidth(s)
	switch {
	case w == n:
		return s
	case w > n:
		return truncCols(s, n)
	case n <= 0:
		// n negative with an empty s: fall through to truncCols rather than
		// hand strings.Repeat a negative count and panic inside a redraw.
		return truncCols(s, n)
	}
	return s + strings.Repeat(" ", n-w)
}

// ---------------------------------------------------------------------------
// wrapCols
// ---------------------------------------------------------------------------

// wrapCols hard-wraps s into lines of at most n columns each. ANSI-aware: it
// never splits an escape sequence or a multi-byte rune, and it re-opens the
// active SGR state at the start of each continuation line.
//
// Hard wrap, not word wrap, and that is the right default for what this is for:
// command output and model text in a fixed pane, where one long unbreakable
// token — a path, a base64 blob, a stack frame — must not be allowed to shove
// the layout wider than the pane it lives in.
//
// Re-opening the colour on every line is not cosmetic. Terminals do not carry
// SGR state across a line that something else may redraw: the moment another
// pane, a scroll, or a repaint lands between two of these lines, an un-reopened
// colour vanishes from line two onward. Emitting the state at each line start
// makes every line independently correct, which is also what lets a caller
// scroll, reorder, or reprint any single line on its own.
//
// An embedded "\n" forces a break, because a raw newline written into a fixed
// pane escapes the pane. Colour state survives across it.
func wrapCols(s string, n int) []string {
	if n <= 0 {
		// A zero-width pane holds nothing. nil is the honest answer and, more to
		// the point, the one that terminates — the loop below would otherwise
		// emit an unbounded run of empty lines.
		return nil
	}
	var (
		lines []string
		cur   strings.Builder
		w     int
		state []string // SGR sequences currently in effect, in the order seen
	)
	// We accumulate whole SGR sequences rather than parsing them into attribute
	// slots. "\x1b[31m\x1b[32m" re-emits both and lets the terminal resolve it
	// the way it did originally — last one wins. Modelling nine attributes
	// separately would be more code and no more correct for anything a TUI emits.
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
		if l := ansiLen(s, i); l > 0 {
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
		rw := runeWidth(r)
		if w+rw > n {
			if w == 0 {
				// The rune is wider than the whole pane: n == 1 and a CJK
				// character. It can never fit, so "break the line and retry" is
				// an infinite loop. We emit it alone and overflow by one column,
				// because dropping the user's text is the worse of two bad
				// options — an overflowing glyph is visible and diagnosable, a
				// missing one is neither.
				//
				// Setting w past n rather than flushing here is what keeps a
				// spurious empty line off the end: the flush happens lazily, on
				// the next rune, only if there is a next rune.
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
	// Always append the tail, even when it is empty: wrapCols("") is one blank
	// line, not zero lines, so a caller drawing a box still gets a row. The loop
	// only flushes when something did not fit, so this cannot bolt a spurious
	// empty line onto input that ends exactly on a wrap point.
	return append(lines, end())
}
