package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"bash-is-all-you-need/external/tui/term"
)

// ErrExit ends the session. Returning it from a command is how /exit works, and
// it is a sentinel rather than a bool on Command so that a host command can end
// the session too — /open on a directory that turns out to be gone, for
// instance, would rather stop than continue against a path that is not there.
var ErrExit = errors.New("exit")

// Command is one slash command.
type Command struct {
	// Name includes the leading slash. Prefix matching means /prov reaches
	// /provider as long as it is unambiguous, so names that share a prefix
	// should differ early.
	Name string

	// Args is the placeholder shown in help: "<dir>", "[on|off]". Empty means
	// the command takes none, and passing one is then an error the command
	// itself should report — the registry does not check, because "extra
	// argument ignored" is worse than a message from the code that knows what
	// the argument would have meant.
	Args string

	Help  string // one line, lower case, no full stop
	Group string // help section; unknown groups are appended in first-seen order

	// Secret marks a command whose argument must never be readable.
	//
	// /provider-apikey is the case. Without this the key is typed in plain view
	// into the composer, echoed into the output pane, kept in the line history,
	// and then written to the real terminal when the pane is reprinted on the
	// way out — four copies of a credential, from one convenience command.
	Secret bool

	// Run executes the command. Whatever it writes to w lands in the
	// scrollback. The context is cancelled when the user interrupts, so a
	// command that makes a network call — /compact does — stops with everything
	// else.
	Run func(ctx context.Context, arg string, w io.Writer) error
}

type registry struct {
	cmds   []Command
	groups []string
}

func (r *registry) add(cs ...Command) {
	for _, c := range cs {
		r.cmds = append(r.cmds, c)
		if c.Group != "" && !contains(r.groups, c.Group) {
			r.groups = append(r.groups, c.Group)
		}
	}
}

// names returns every command name, sorted, for completion display.
func (r *registry) names() []string {
	out := make([]string, 0, len(r.cmds))
	for _, c := range r.cmds {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// find resolves a typed name.
//
// Exact match wins outright, before prefixes are considered. Without that rule
// adding /provider-url would silently break /provider, which is the sort of
// regression that only shows up in a bug report from someone else.
func (r *registry) find(name string) (Command, []Command) {
	for _, c := range r.cmds {
		if c.Name == name {
			return c, nil
		}
	}
	var hits []Command
	for _, c := range r.cmds {
		if strings.HasPrefix(c.Name, name) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Name < hits[j].Name })
	return Command{}, hits
}

// complete returns the candidates for a prefix and the longest common prefix
// they share, which is what Tab inserts.
func (r *registry) complete(prefix string) (common string, hits []Command) {
	for _, c := range r.cmds {
		if strings.HasPrefix(c.Name, prefix) {
			hits = append(hits, c)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Name < hits[j].Name })
	if len(hits) == 0 {
		return "", nil
	}
	common = hits[0].Name
	for _, c := range hits[1:] {
		for !strings.HasPrefix(c.Name, common) {
			common = common[:len(common)-1]
		}
	}
	return common, hits
}

// help renders the command list, or one command when topic names it.
func (r *registry) help(topic string, w int) []string {
	if topic != "" {
		if !strings.HasPrefix(topic, "/") {
			topic = "/" + topic
		}
		c, hits := r.find(topic)
		if c.Name == "" {
			if len(hits) == 0 {
				return []string{"  no command matches " + topic}
			}
			out := []string{"  " + topic + " is ambiguous:"}
			for _, h := range hits {
				out = append(out, "    "+h.Name)
			}
			return out
		}
		return []string{"  " + label(c), "      " + c.Help}
	}

	width := 0
	for _, c := range r.cmds {
		if n := term.DispWidth(label(c)); n > width {
			width = n
		}
	}
	// Help that wraps mid-column is unreadable, so on a narrow window the
	// description moves to its own line rather than being truncated. A
	// truncated help line is the one line in the program that must not need a
	// wider terminal to be understood.
	twoColumn := width+4+24 <= w

	var out []string
	for _, g := range append(r.groups, "") {
		var in []Command
		for _, c := range r.cmds {
			if c.Group == g {
				in = append(in, c)
			}
		}
		if len(in) == 0 {
			continue
		}
		sort.Slice(in, func(i, j int) bool { return in[i].Name < in[j].Name })
		if g != "" {
			out = append(out, "", "  "+g)
		}
		for _, c := range in {
			if twoColumn {
				out = append(out, "    "+term.PadCols(label(c), width)+"  "+c.Help)
				continue
			}
			out = append(out, "    "+label(c), "        "+c.Help)
		}
	}
	return out
}

func label(c Command) string {
	if c.Args == "" {
		return c.Name
	}
	return c.Name + " " + c.Args
}

// splitCommand separates a slash command from its argument.
func splitCommand(line string) (name, arg string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// maskFrom reports the rune offset in line at which typed text must be drawn as
// bullets, or -1 when it need not be.
//
// The check runs on every frame against the partly-typed line, so it has to
// treat an unfinished name as secret if any secret command starts with it —
// otherwise the first characters of the key are visible for exactly as long as
// it takes to finish typing the command name, which is the whole time someone
// is looking at the screen.
func (r *registry) maskFrom(line string) int {
	if !strings.HasPrefix(line, "/") {
		return -1
	}
	name, _ := splitCommand(line)
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return -1
	}
	if c, _ := r.find(name); c.Name != "" {
		if !c.Secret {
			return -1
		}
		return len([]rune(line[:i])) + 1
	}
	for _, c := range r.cmds {
		if c.Secret && strings.HasPrefix(c.Name, name) {
			return len([]rune(line[:i])) + 1
		}
	}
	return -1
}

// secret reports whether a submitted line carries a credential, so that neither
// the echo nor the history keeps it.
func (r *registry) secret(line string) bool {
	name, _ := splitCommand(line)
	if c, _ := r.find(name); c.Name != "" {
		return c.Secret
	}
	return false
}

// onOff parses the argument shared by every toggle command.
//
// An empty argument means "flip it", which is what people type; an explicit
// value means set it, which is what a paste from the docs does. Anything else
// is an error rather than a guess, because guessing wrong on /yolo runs commands
// nobody approved.
func onOff(arg string, current bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		return !current, nil
	case "on", "yes", "true", "1":
		return true, nil
	case "off", "no", "false", "0":
		return false, nil
	default:
		return current, fmt.Errorf("want on or off, got %q", arg)
	}
}
