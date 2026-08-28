package tui

import "strings"

// escByte begins every ANSI sequence. See md.line for why this file looks for it.
const escByte = 0x1b

// The SGR parameters this file draws with.
//
// Bare codes rather than calls to style's dim/bold/cyan helpers, because a run
// of text often carries a block's attribute and its own at once — a dim marker
// inside a bold heading — and the two have to arrive as a single sequence.
// Nesting the helpers instead produces st.bold(a + st.dim(b) + c), whose inner
// reset closes the bold as well, so c comes out unstyled. Every sequence here
// still goes out through style.wrap, which is the one place that knows whether
// colour is on at all.
const (
	sgrDim       = "2"
	sgrBold      = "1"
	sgrItalic    = "3"
	sgrUnderline = "4"
	sgrCyan      = "36"
	sgrYellow    = "33"

	// A top heading gets weight and colour; the deeper ones only weight, so the
	// levels stay distinguishable in a reply that uses several.
	sgrHeading = sgrBold + ";" + sgrCyan

	// Code spans are yellow rather than cyan because cyan is already this file's
	// colour for structure — bullets, numbers, heading text. Two roles, two
	// colours, and a reader can tell a command from a heading at a glance.
	sgrCode = sgrYellow
)

// md renders the model's prose as styled terminal text, one logical line at a
// time.
//
// One line at a time is the constraint the rest of this file follows from. The
// pane receives a reply as it streams and re-wraps it on every resize, so there
// is never a moment when a whole document exists to parse: the only markdown
// that can be rendered is the markdown a single line decides on its own. Fenced
// code blocks are the exception, and the fence flag is what carries them.
//
// Because line mutates that flag, one md belongs to one pane and to one
// goroutine.
type md struct {
	st    style
	fence bool
}

func newMD(st style) *md { return &md{st: st} }

// reset drops the carried fence state. The pane can be cleared in the middle of
// a code block, and a flag left set would then dim every line of whatever came
// next.
func (m *md) reset() { m.fence = false }

// line returns s rendered as ANSI. It must be called once per complete logical
// line, in order.
//
// Two kinds of input come back untouched. With colour off the pane is being read
// as text — redirected to a file, or on a terminal that asked for none — and an
// escape sequence there is a bug report. A line that already contains ESC was
// styled by something upstream, and re-styling it would interleave our sequences
// with its: ours close by resetting everything, so the caller's colour would
// come off half way along its own line. The model's prose never contains ESC, so
// refusing to touch a line that does costs nothing.
//
// The colour-off return is not what enforces the first of those — style.wrap
// emits nothing with colour off, so the line would come back unchanged anyway.
// It states the rule in one place and skips the work. It does return before the
// fence flag is updated, which is harmless only because st never changes after
// newMD: a pane that starts plain stays plain, so there is no run of lines whose
// fence state was missed.
//
// Everything below only ever wraps existing bytes in SGR sequences. No byte of s
// is dropped, added or replaced, and that is what keeps the rendered line exactly
// as wide as the raw one. The pane wraps on display width, so a renderer that
// changed it would tear the layout on the next resize.
// preview renders a line that is not finished yet, without remembering it.
//
// A model writes a paragraph as one logical line and sends no newline until the
// end of it, so the line currently being streamed is usually the whole of what
// the reader is watching. Rendering it only once it is complete would leave the
// reply in raw asterisks for as long as the model takes to finish the sentence,
// and then snap.
//
// It must not advance the fence state, which is why this exists rather than a
// flag on line: a partial line is rendered again on every frame, and toggling
// the fence thirty times a second on a chunk that happens to start with three
// backticks would leave the rest of the session dimmed or not depending on where
// the repaints landed.
func (m *md) preview(s string) string {
	fence := m.fence
	out := m.line(s)
	m.fence = fence
	return out
}

func (m *md) line(s string) string {
	if !m.st.on {
		return s
	}
	if strings.IndexByte(s, escByte) >= 0 {
		return s
	}

	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		m.fence = !m.fence
		return m.st.dim(s)
	}
	if m.fence {
		// No inline pass inside a fence. An asterisk in a shell command is a
		// glob and an underscore is part of a name, and dimming the block whole
		// is also what shows the reader where it ends.
		return m.st.dim(s)
	}
	if t == "" {
		return s
	}
	if isThematicBreak(t) {
		return m.st.dim(s)
	}
	if level, end := atxHeading(s); level > 0 {
		code := sgrBold
		if level <= 2 {
			code = sgrHeading
		}
		return m.st.dim(s[:end]) + m.draw(inlineSpans(s[end:]), code)
	}
	if s[0] == '>' {
		return m.st.dim(">") + m.draw(inlineSpans(s[1:]), sgrDim)
	}
	if isTableRow(t) {
		return m.tableRow(s)
	}
	if start, end := listMarker(s); end > start {
		// The indent goes out as it arrived: colouring a space is invisible, and
		// leaving it out of the marker keeps the range easy to reason about.
		return s[:start] + m.paint(sgrCyan, s[start:end]) + m.draw(inlineSpans(s[end:]), "")
	}
	return m.draw(inlineSpans(s), "")
}

// tableRow dims the pipes and leaves the cells to the inline pass.
//
// Nothing is aligned or measured. A row is styled knowing only itself, and a
// table looks like a table only because the model already padded the cells —
// which is exactly why the pipes must not move.
func (m *md) tableRow(s string) string {
	var b strings.Builder
	prev := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '|' {
			continue
		}
		b.WriteString(m.draw(inlineSpans(s[prev:i]), ""))
		b.WriteString(m.paint(sgrDim, "|"))
		prev = i + 1
	}
	b.WriteString(m.draw(inlineSpans(s[prev:]), ""))
	return b.String()
}

// span is a run of the line's own bytes together with the SGR parameters it is
// drawn with. An empty code means the run goes out as it arrived.
type span struct {
	text string
	code string
}

// draw renders spans, giving each one base's attributes underneath its own.
//
// base is how a block-level decision reaches the inline pass: a blockquote's
// body is dim, and a dim marker inside it still has to come out as one sequence
// carrying both, for the reason given at the top of this file.
func (m *md) draw(spans []span, base string) string {
	var b strings.Builder
	for _, sp := range spans {
		b.WriteString(m.paint(joinSGR(base, sp.code), sp.text))
	}
	return b.String()
}

// paint wraps text in one SGR sequence, or hands it back untouched when there is
// nothing to say. The guard matters: style.wrap with an empty code would emit
// "\x1b[m", which a terminal reads as a reset and which would therefore undo
// whatever the caller had open.
func (m *md) paint(code, text string) string {
	if code == "" {
		return text
	}
	return m.st.wrap(code, text)
}

// joinSGR combines two sets of SGR parameters into one sequence's worth.
//
// The only pair here that can contradict is bold with dim, both of which set
// weight, and that happens for the markers inside a heading. The terminal
// decides which one wins and either reading is legible, so it is not worth a
// table of which attributes may be combined.
func joinSGR(base, own string) string {
	switch {
	case base == "":
		return own
	case own == "":
		return base
	default:
		return base + ";" + own
	}
}

// inlineSpans styles the code spans, emphasis and links in s.
//
// Markers are dimmed and left exactly where they are, never removed. Removing
// them would change the line's width, which the pane cannot survive; it would
// also cost the reader something real, because when the model is explaining
// markdown the asterisks are the answer.
//
// One pass, leftmost match wins, no recursion: the content of a bold span is
// drawn bold and not looked at again. Overlapping markup is rare in a reply and
// every way of resolving it costs more than it returns.
func inlineSpans(s string) []span {
	in := inliner{s: s}
	in.run()
	return in.out
}

// inliner is the state of one line's inline pass.
type inliner struct {
	s     string
	out   []span
	plain int      // start of the bytes to be copied through, not yet emitted
	dead  []string // closers a scan already proved absent from the rest of s
}

func (in *inliner) run() {
	s := in.s
	// Scanning by byte rather than by rune is safe because every marker here is
	// ASCII and no byte of a multi-byte character is ever below 0x80. Untouched
	// bytes are copied through in runs, so a rune is never split.
	for i := 0; i < len(s); {
		switch s[i] {
		case '`':
			// Code first, so a code span wins over anything inside it. Writing
			// `**x**` and getting literal asterisks is the whole point of it.
			if j := in.closer("`", i+1, false); j >= 0 {
				in.flush(i)
				in.emit("`", sgrDim)
				in.emit(s[i+1:j], sgrCode)
				in.emit("`", sgrDim)
				i = j + 1
				in.plain = i
				continue
			}
		case '[':
			// "](" is searched for as one two-byte marker rather than as a "]"
			// and then a "(". A line of unmatched brackets would otherwise scan
			// the tail once per bracket, and this way a failure is remembered.
			if j := in.closer("](", i+1, false); j >= 0 {
				if k := in.closer(")", j+2, false); k >= 0 {
					in.flush(i)
					in.emit("[", sgrDim)
					in.emit(s[i+1:j], sgrUnderline)
					in.emit(s[j:k+1], sgrDim)
					i = k + 1
					in.plain = i
					continue
				}
			}
		case '*', '_':
			// A doubled marker is taken as one, so ** is tried before *. Read as
			// a single asterisk, "**a**" would open on the first and close on
			// the third, and emphasise "*a".
			n := 1
			if i+1 < len(s) && s[i+1] == s[i] {
				n = 2
			}
			if in.opens(i, n) {
				if j := in.closer(s[i:i+n], i+n+1, true); j >= 0 {
					code := sgrItalic
					if n == 2 {
						code = sgrBold
					}
					in.flush(i)
					in.emit(s[i:i+n], sgrDim)
					in.emit(s[i+n:j], code)
					in.emit(s[j:j+n], sgrDim)
					i = j + n
					in.plain = i
					continue
				}
			}
			// Past both bytes of a doubled marker that led nowhere. Coming back
			// for the second one would let "**a *b*" open at it, run to the last
			// asterisk on the line and swallow the emphasis that follows.
			i += n
			continue
		}
		i++
	}
	in.flush(len(s))
}

// opens reports whether the n-byte marker at i can begin emphasis.
//
// It must be preceded by the start of the line, a space or punctuation, and
// followed by a non-space. Without the first half, "a*b" — a glob, a
// multiplication, half a filename — becomes emphasis. Without the second, a
// sentence containing two unrelated asterisks with spaces around them turns the
// text between them italic.
//
// A byte above 0x7f is part of a multi-byte character and reads here as a
// letter, so emphasis does not open straight after CJK text. That is the
// conservative direction: the line is left as the model wrote it.
func (in *inliner) opens(i, n int) bool {
	if i > 0 {
		if p := in.s[i-1]; !isSpaceByte(p) && !isASCIIPunct(p) {
			return false
		}
	}
	return i+n < len(in.s) && !isSpaceByte(in.s[i+n])
}

// closer returns the index of the next occurrence of mark at or after from that
// can end a span, or -1 when there is none.
//
// flank asks for the emphasis rule, that the marker be preceded by a non-space,
// which is what stops the second asterisk of "a * b * c" from closing anything.
// Code spans and links have no such rule, because `x ` and [a ](b) are ordinary.
//
// A failed scan is remembered. Whether an occurrence can close depends only on
// its own bytes and the one before it, and every later scan for the same mark
// starts further along the same line, so once one scan has reached the end
// without a match no later one can find anything either. Without that memory a
// line of ten thousand unclosed "*a " groups would scan the whole tail once per
// group, and the pane would stop on a paragraph the model can produce by
// accident.
func (in *inliner) closer(mark string, from int, flank bool) int {
	for _, d := range in.dead {
		if d == mark {
			return -1
		}
	}
	for j := from; j < len(in.s); {
		k := strings.Index(in.s[j:], mark)
		if k < 0 {
			break
		}
		j += k
		if !flank || !isSpaceByte(in.s[j-1]) {
			return j
		}
		j++
	}
	in.dead = append(in.dead, mark)
	return -1
}

// flush moves the bytes copied through so far, up to end, into out.
func (in *inliner) flush(end int) {
	if end > in.plain {
		in.out = append(in.out, span{text: in.s[in.plain:end]})
	}
}

func (in *inliner) emit(text, code string) {
	in.out = append(in.out, span{text: text, code: code})
}

// atxHeading returns the level of the heading s begins with and the index just
// past the space that follows the hashes, or zero if it begins with none. The
// space is part of the returned marker because it belongs to the marker: the
// text starts after it.
func atxHeading(s string) (level, end int) {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	// Seven hashes is not a heading of any level, so a line of them is prose.
	if n == 0 || n > 6 {
		return 0, 0
	}
	if n == len(s) {
		return n, n
	}
	if s[n] != ' ' {
		return 0, 0
	}
	return n, n + 1
}

// listMarker returns the byte range of the bullet or number that starts s, not
// counting the indent before it, or a zero range if s starts with neither.
func listMarker(s string) (start, end int) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i == len(s) {
		return 0, 0
	}
	if c := s[i]; c == '-' || c == '*' || c == '+' {
		if i+1 < len(s) && s[i+1] == ' ' {
			return i, i + 1
		}
		return 0, 0
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	// Nine digits is the cap. Past that the line begins with a number rather
	// than with a list marker, and a long figure followed by a full stop is a
	// sentence.
	if j == i || j-i > 9 {
		return 0, 0
	}
	if j+1 < len(s) && (s[j] == '.' || s[j] == ')') && s[j+1] == ' ' {
		return i, j + 1
	}
	return 0, 0
}

// isThematicBreak reports whether t, already trimmed and known non-empty, is a
// horizontal rule: three or more of one of -, * or _, and nothing else but
// spaces. It is decided before the list markers, because "* * *" is otherwise a
// bullet followed by two asterisks.
func isThematicBreak(t string) bool {
	c := t[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	n := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// isTableRow reports whether t, already trimmed, is a row of a pipe table.
func isTableRow(t string) bool {
	return len(t) >= 2 && t[0] == '|' && t[len(t)-1] == '|'
}

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' }

// isASCIIPunct reports whether b is printable ASCII that is neither a letter,
// a digit, nor a space. Bytes above 0x7f are excluded on purpose; see opens.
func isASCIIPunct(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return false
	}
	return b > ' ' && b < 0x7f
}
