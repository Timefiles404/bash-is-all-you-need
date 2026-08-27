package tui

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"bash-is-all-you-need/tui/settings"
	"bash-is-all-you-need/tui/term"
)

// EnvNames are the environment variables the provider commands write.
//
// They are passed in rather than hard-coded because they belong to the host's
// configuration, not to a terminal UI. An empty name disables the matching
// command, which is how a host that configures itself some other way avoids
// shipping a command that does nothing.
type EnvNames struct {
	BaseURL  string
	APIKey   string
	Protocol string
	Model    string
}

// builtins are the commands the shell owns. The host's own commands are added
// after these, and a host command with the same name shadows nothing — the
// registry keeps both and the first exact match wins, so a duplicate is a bug
// the host author will see immediately rather than a silent override.
func (a *App) builtins() []Command {
	cmds := []Command{
		{
			Name: "/help", Args: "[command]", Group: "shell",
			Help: "list the commands, or explain one",
			Run: func(_ context.Context, arg string, w io.Writer) error {
				for _, l := range a.reg.help(arg, a.width()) {
					fmt.Fprintln(w, l)
				}
				return nil
			},
		},
		{
			Name: "/keys", Group: "shell",
			Help: "the keyboard map",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				for _, l := range keymap(a.st, a.width()) {
					fmt.Fprintln(w, l)
				}
				return nil
			},
		},
		{
			Name: "/clear", Group: "shell",
			Help: "empty the output pane; the conversation is untouched",
			Run: func(_ context.Context, _ string, _ io.Writer) error {
				// The scroll offset is deliberately not reset here. It is
				// loop-owned state, and the view clamps it against a pane that
				// is now empty, so touching it from this goroutine would be a
				// race that buys nothing.
				a.back.clear()
				return nil
			},
		},
		{
			Name: "/exit", Group: "shell",
			Help: "leave the shell",
			Run:  func(_ context.Context, _ string, _ io.Writer) error { return ErrExit },
		},
		{
			Name: "/quit", Group: "shell",
			Help: "leave the shell",
			Run:  func(_ context.Context, _ string, _ io.Writer) error { return ErrExit },
		},
		{
			Name: "/status", Group: "session",
			Help: "everything this session is currently configured to do",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				secs := a.shellStatus()
				if a.cfg.Status != nil {
					secs = append(secs, a.cfg.Status()...)
				}
				for _, l := range renderStatus(secs, a.width(), a.st) {
					fmt.Fprintln(w, l)
				}
				return nil
			},
		},
	}

	if a.cfg.Open != nil {
		cmds = append(cmds, Command{
			Name: "/open", Args: "<dir>", Group: "workspace",
			Help: "point the agent at another directory",
			Run:  a.runOpen,
		})
	}
	return append(cmds, a.settingsCommands()...)
}

// runOpen resolves and checks the path before handing it over.
//
// The check is here rather than in the host because the failure it prevents is
// a shell failure: /open on a typo would otherwise reload the memory files, the
// skills index and the system prompt against a directory that does not exist,
// and the session would then be pointed somewhere the user cannot see.
func (a *App) runOpen(_ context.Context, arg string, w io.Writer) error {
	if arg == "" {
		return fmt.Errorf("/open needs a directory")
	}
	if strings.HasPrefix(arg, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			arg = filepath.Join(home, strings.TrimPrefix(arg, "~"))
		}
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}
	msg, err := a.cfg.Open(abs)
	if err != nil {
		return err
	}
	if msg != "" {
		fmt.Fprintf(w, "  %s\n", msg)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// settingsCommands are the ones that write to disk.
//
// They exist for one situation: the binary was started by double-clicking it, so
// there is no shell, no .env and no environment. Every other way of configuring
// the agent assumes someone typed `set -a && . ./.env` first, and the whole
// point of the shell is to be usable by someone who did not.
func (a *App) settingsCommands() []Command {
	store := a.cfg.Settings
	if store == nil {
		return nil
	}
	set := func(name, human string, check func(string) (string, error)) func(context.Context, string, io.Writer) error {
		return func(_ context.Context, arg string, w io.Writer) error {
			if name == "" {
				return fmt.Errorf("this build does not configure %s from the shell", human)
			}
			arg = strings.TrimSpace(arg)
			if arg == "" {
				cur, ok := store.Get(name)
				if !ok {
					cur = os.Getenv(name)
				}
				fmt.Fprintf(w, "  %s is %s\n", name, settings.Redact(name, cur))
				return nil
			}
			v, err := check(arg)
			if err != nil {
				return err
			}
			store.Set(name, v)
			// The process environment is updated as well as the file. Without
			// this the command would appear to do nothing until the next
			// launch, because the host reads its configuration from the
			// environment — see the host's config.go, where the key
			// deliberately has nowhere to sit except an environment variable.
			if err := os.Setenv(name, v); err != nil {
				return err
			}
			if err := store.Save(); err != nil {
				return fmt.Errorf("set for this session, but not saved: %w", err)
			}
			fmt.Fprintf(w, "  %s = %s\n  saved to %s\n", name, settings.Redact(name, v), store.Path())
			return a.reconfigure(w)
		}
	}

	protocols := a.cfg.Protocols
	if len(protocols) == 0 {
		protocols = []string{"openai", "anthropic"}
	}

	return []Command{
		{
			Name: "/provider-url", Args: "<url>", Group: "provider",
			Help: "set and save the endpoint base URL",
			Run: set(a.cfg.Env.BaseURL, "the base URL", func(s string) (string, error) {
				u, err := url.Parse(s)
				if err != nil {
					return "", err
				}
				if u.Scheme != "http" && u.Scheme != "https" {
					return "", fmt.Errorf("want an http or https URL, got %q", s)
				}
				if u.Host == "" {
					return "", fmt.Errorf("%q has no host", s)
				}
				// A trailing slash is removed here rather than at every use.
				// The adapters append their own path, and two slashes is a 404
				// on some gateways and a redirect that drops the body on
				// others.
				return strings.TrimSuffix(s, "/"), nil
			}),
		},
		{
			Name: "/provider-protocol", Args: "<" + strings.Join(protocols, "|") + ">", Group: "provider",
			Help: "set and save the wire protocol",
			Run: set(a.cfg.Env.Protocol, "the protocol", func(s string) (string, error) {
				s = strings.ToLower(s)
				for _, p := range protocols {
					if s == p {
						return s, nil
					}
				}
				return "", fmt.Errorf("unknown protocol %q (want %s)", s, strings.Join(protocols, " or "))
			}),
		},
		{
			Name: "/provider-model", Args: "<id>", Group: "provider",
			Help: "set and save the model id",
			Run: set(a.cfg.Env.Model, "the model", func(s string) (string, error) {
				if strings.ContainsAny(s, " \t") {
					return "", fmt.Errorf("a model id has no spaces in it: %q", s)
				}
				return s, nil
			}),
		},
		{
			Name: "/provider-apikey", Args: "<key>", Group: "provider",
			Help: "set and save the API key", Secret: true,
			Run: set(a.cfg.Env.APIKey, "the API key", func(s string) (string, error) {
				if strings.ContainsAny(s, " \t\r\n") {
					// Almost always a paste that picked up the shell prompt or
					// a line break, and the resulting 401 says nothing useful.
					return "", fmt.Errorf("that key contains whitespace — check the paste")
				}
				return s, nil
			}),
		},
		{
			Name: "/settings", Group: "provider",
			Help: "show the saved settings file",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				fmt.Fprintf(w, "  %s\n", store.Path())
				names := store.Names()
				if len(names) == 0 {
					fmt.Fprintf(w, "  %s\n", a.st.dim("nothing saved yet"))
					return nil
				}
				width := 0
				for _, n := range names {
					if d := term.DispWidth(n); d > width {
						width = d
					}
				}
				for _, n := range names {
					v, _ := store.Get(n)
					live := ""
					if os.Getenv(n) != v {
						// The environment wins on startup, so a difference here
						// is the reason a saved value appears to be ignored.
						live = a.st.dim("   (the environment has a different value, which wins)")
					}
					fmt.Fprintf(w, "  %s  %s%s\n", term.PadCols(n, width), settings.Redact(n, v), live)
				}
				return nil
			},
		},
		{
			Name: "/settings-forget", Args: "<NAME>", Group: "provider",
			Help: "remove one saved value",
			Run: func(_ context.Context, arg string, w io.Writer) error {
				arg = strings.TrimSpace(arg)
				if arg == "" {
					return fmt.Errorf("/settings-forget needs a variable name; /settings lists them")
				}
				if _, ok := store.Get(arg); !ok {
					return fmt.Errorf("%s is not saved", arg)
				}
				store.Unset(arg)
				if err := store.Save(); err != nil {
					return err
				}
				// The process environment is deliberately left alone. Unsetting
				// it here would change the behaviour of the running session in
				// a way the command does not claim to, and the value may have
				// come from the environment in the first place.
				fmt.Fprintf(w, "  %s removed from %s\n  %s\n", arg, store.Path(),
					a.st.dim("this session keeps using it until you restart"))
				return nil
			},
		},
	}
}

func (a *App) reconfigure(w io.Writer) error {
	if a.cfg.Reconfigure == nil {
		return nil
	}
	msg, err := a.cfg.Reconfigure()
	if err != nil {
		// Not the command's failure: the value was saved. Reporting it as an
		// error would send the user back to re-enter something that is already
		// on disk.
		fmt.Fprintf(w, "  %s\n", a.st.yellow("saved, but the provider still will not build: "+err.Error()))
		return nil
	}
	if msg != "" {
		fmt.Fprintf(w, "  %s\n", msg)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

func (a *App) shellStatus() []Section {
	lines, dropped := a.back.stats()
	rows := []Row{
		{Name: "window", Value: fmt.Sprintf("%d×%d", a.width(), a.height())},
		{Name: "colour", Value: yesNo(a.st.on)},
		{Name: "output pane", Value: fmt.Sprintf("%d lines", lines), Note: droppedNote(dropped)},
		{Name: "platform", Value: runtime.GOOS + "/" + runtime.GOARCH},
	}
	if a.cfg.Settings != nil {
		rows = append(rows, Row{Name: "settings file", Value: a.cfg.Settings.Path()})
	}
	return []Section{{Title: "shell", Rows: rows}}
}

func droppedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d older lines dropped", n)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func keymap(st style, w int) []string {
	rows := [][2]string{
		{"enter", "send"},
		{"alt-enter", "newline inside the prompt"},
		{"esc", "interrupt a running turn; otherwise clear the prompt"},
		{"ctrl-c", "interrupt; twice on an empty prompt to leave"},
		{"ctrl-d", "leave, on an empty prompt"},
		{"tab", "complete a slash command"},
		{"up / down", "history, or move between lines of a multi-line prompt"},
		{"pgup / pgdn", "scroll the output pane"},
		{"shift-up / shift-down", "scroll one line"},
		{"wheel", "scroll three lines"},
		{"ctrl-a / ctrl-e", "start / end of line"},
		{"ctrl-w", "delete the word before the caret"},
		{"ctrl-u / ctrl-k", "delete to the start / end of the line"},
		{"ctrl-y", "put back what was last deleted"},
		{"ctrl-l", "redraw"},
	}
	width := 0
	for _, r := range rows {
		if d := term.DispWidth(r[0]); d > width {
			width = d
		}
	}
	out := make([]string, 0, len(rows)+1)
	out = append(out, "")
	for _, r := range rows {
		out = append(out, term.TruncCols("  "+st.dim(term.PadCols(r[0], width))+"  "+r[1], w))
	}
	return out
}
