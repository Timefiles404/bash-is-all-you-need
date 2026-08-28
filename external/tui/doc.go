// Package tui is the interactive shell the agent runs in when you start it
// without arguments: a scrollback pane, a status bar, a line editor, and a set
// of slash commands.
//
// # This package is not part of the course
//
// Everything under stages/ is written to be read: each file is a teaching
// artifact, each comment argues for a decision, and the chapters in docs/ walk
// through them line by line. This package is the opposite. It is a tool, it is
// allowed to be boring, and no chapter explains it.
//
// It exists because the two things a reader needs from the binary pull in
// opposite directions. Stage 06 builds a terminal UI from three functions and a
// select, and the whole point of that chapter is that you can hold all of it in
// your head — so it must stay small. But a reader who is *using* the agent to
// poke at stage 09's triage or stage 12's cache wants something else entirely:
// a window that does not close when a config value is missing, a way to fix
// that value without editing a file, and a key that stops a runaway turn.
// Those are not lessons. They are the difference between a repo you read and a
// repo you can work in.
//
// So the shell lives here, out of the way, and the stages keep their small
// hand-written UI. Nothing in stages/ was deleted to make room for it.
//
// # What it knows
//
// Nothing about agents, models, or HTTP. The host program passes a Config with
// a Submit function and a list of Commands, and points its existing renderer at
// App.Out. That is the whole seam, and it is deliberately narrow: this package
// must never become the place where agent behaviour is decided, because then it
// would be teaching material after all, and untaught teaching material is worse
// than none.
//
// # One trade-off worth knowing about
//
// The shell paints whole frames on the alternate screen, which is what makes a
// status bar and a scrollable pane possible — and it means the terminal's own
// scrollback holds nothing while the shell is running. The transcript is
// reprinted on the way out to recover most of that, but it is a copy, not the
// real thing: no native selection while running, no scrolling with the mouse
// past the pane's own buffer.
//
// The alternative is to print output inline and redraw only the last few rows,
// which is how several agent CLIs do it and is genuinely better on this point.
// It is also a different program: every line that wraps moves the footer, and
// the footer has to be erased and reprinted around each write. Named here so the
// choice is visible rather than accidental.
//
// # Sub-packages
//
//	tui/term      raw mode, one-frame repaints, key decoding, display width
//	tui/settings  the credentials and settings file, outside any git tree
//
// tui/term is a lift of stage 06's term.go, keys.go and width.go. The stage
// copies are the version with the essays in them and they stay; a behaviour
// change in one has to be mirrored in the other or 06-the-composer/doc/
// stops being true.
package tui
