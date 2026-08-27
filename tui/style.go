package tui

import (
	"os"
	"strings"
)

// style is the whole of this package's colour handling.
//
// Kept to one type with an on/off flag because colour has to be *absent*
// correctly, not just present prettily: the same frames are what a reader sees
// when they redirect the pane to a file, and an escape sequence that survives
// into a text file is a bug report.
type style struct{ on bool }

func (s style) wrap(code, t string) string {
	if !s.on || t == "" {
		return t
	}
	return "\x1b[" + code + "m" + t + "\x1b[0m"
}

func (s style) dim(t string) string    { return s.wrap("2", t) }
func (s style) bold(t string) string   { return s.wrap("1", t) }
func (s style) red(t string) string    { return s.wrap("31", t) }
func (s style) green(t string) string  { return s.wrap("32", t) }
func (s style) yellow(t string) string { return s.wrap("33", t) }
func (s style) cyan(t string) string   { return s.wrap("36", t) }

// bar is the status line's own styling: reverse video, so it reads as furniture
// rather than as output. Reverse rather than a background colour because it is
// the one attribute that looks deliberate on both a light and a dark theme.
func (s style) bar(t string) string { return s.wrap("7", t) }

// colorEnabled decides whether to emit escape sequences at all.
//
// NO_COLOR is honoured because it is the one convention every tool in a
// pipeline agrees on. An *empty* TERM is deliberately not treated as dumb: on
// Windows the variable is simply not set and the console understands escapes
// perfectly well, so reading empty as dumb would ship a monochrome UI to the
// platform this repo is most often read on.
func colorEnabled(out *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	if fi, err := out.Stat(); err == nil {
		return fi.Mode()&os.ModeCharDevice != 0
	}
	return false
}
