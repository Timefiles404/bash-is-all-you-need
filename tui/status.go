package tui

import (
	"strings"

	"bash-is-all-you-need/tui/term"
)

// Row is one line of /status: a label, a value, and an optional note.
//
// The note exists so a value never has to be editorialised. "8000" and
// "8000 (bytes of output the model may see)" are the same fact, but only one of
// them can be aligned into a column, and only one of them can be read by
// someone who does not already know what the number is.
type Row struct {
	Name  string
	Value string
	Note  string
}

// Section groups rows under a heading.
type Section struct {
	Title string
	Rows  []Row
}

// renderStatus lays sections out as one table.
//
// The label column is measured across every section rather than per section, so
// the whole report reads as a single table. Sections that align only internally
// look like separate tables that happen to be adjacent, and then the eye stops
// comparing rows across them — which is the main thing this report is for.
func renderStatus(secs []Section, w int, st style) []string {
	// Two columns are measured, and the second one only over rows that have a
	// note. A value column padded to the widest value in the report would push
	// every note twenty columns right to make room for one long path; padded to
	// the widest value that a note has to clear, the notes line up and the long
	// values are simply long.
	label, value := 0, 0
	for _, s := range secs {
		for _, r := range s.Rows {
			if n := term.DispWidth(r.Name); n > label {
				label = n
			}
			if r.Note == "" {
				continue
			}
			if n := term.DispWidth(r.Value); n > value {
				value = n
			}
		}
	}

	var out []string
	for _, s := range secs {
		if len(s.Rows) == 0 {
			continue
		}
		if s.Title != "" {
			out = append(out, "", "  "+st.bold(s.Title))
		}
		for _, r := range s.Rows {
			line := "    " + st.dim(term.PadCols(r.Name, label)) + "  " + r.Value
			if r.Note != "" {
				line += st.dim(strings.Repeat(" ", pad(value-term.DispWidth(r.Value))) + "   " + r.Note)
			}
			out = append(out, term.TruncCols(line, w))
		}
	}
	return out
}

func pad(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// segments joins status-bar fields, dropping from the right until the line
// fits.
//
// Dropping rather than truncating, and from the right rather than the left,
// because the fields are given most-important-first: a status bar that has to
// lose something should lose the token count, not which provider is answering.
func segments(fields []string, w int, sep string) string {
	var keep []string
	var first string
	for _, f := range fields {
		if f == "" {
			continue
		}
		if first == "" {
			first = f
		}
		// Not append(keep, f): that writes into keep's spare capacity, which is
		// harmless here only by accident and is the classic way a slice shared
		// with a caller acquires a value nobody assigned.
		try := make([]string, len(keep), len(keep)+1)
		copy(try, keep)
		try = append(try, f)
		if term.DispWidth(strings.Join(try, sep)) > w {
			break
		}
		keep = try
	}
	if len(keep) == 0 {
		// Everything is too wide for the window. Show the first field cut to
		// fit rather than an empty bar: on a very narrow terminal the provider
		// name half-visible is still the answer to the question the bar exists
		// to answer. The first *non-empty* field — fields[0] is the host's
		// title, which is an ordinary optional string, and a host that leaves it
		// blank was getting a blank bar from the line that exists to prevent one.
		return term.TruncCols(first, w)
	}
	return strings.Join(keep, sep)
}

// RenderRows lays out one ungrouped list of rows.
//
// Exported for a host command that wants the same alignment as /status without
// the section headings — /context is that command. Without it the host either
// re-implements the layout or reaches into this package's internals, and the
// second one is how a package that is deliberately not part of the course ends
// up being read as if it were.
func RenderRows(rows []Row, w int) []string {
	return renderStatus([]Section{{Rows: rows}}, w, style{})
}
