// Package settings holds the agent's local configuration — endpoint, protocol,
// model and API key — in a file under the user's OS config directory.
//
// The key goes there rather than into the repo's providers.json because a
// config file that lives inside a git tree gets committed eventually, and the
// only reliable defence is for the secret to have nowhere to sit in that file
// at all. The OS config directory is outside every git tree and, on a normal
// machine, inside a directory the operating system already restricts to its
// owner, so it can hold the one value providers.json refuses to hold.
//
// The other half of the reason is that the binary has to work when it was
// double-clicked. `set -a && . ./.env` is a shell habit, and a user who never
// opened a shell has no environment variables at all; ExportMissing is what
// closes that gap without ever overriding a variable the shell did set.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	// PathEnv overrides where settings live. It exists so that tests — and a
	// second agent on the same machine — can point somewhere else, which is the
	// only way to test this package without writing to the developer's own
	// config directory.
	PathEnv = "AGENT_SETTINGS"

	// dirName keeps the file out of the config directory's top level, where a
	// bare settings.json would collide with every other program that had the
	// same idea.
	dirName  = "bash-is-all-you-need"
	fileName = "settings.json"

	// dirPerm and filePerm are enforced by the kernel on Unix only. On Windows
	// os.Chmod can toggle the read-only attribute and nothing else, so 0o600
	// there means "writable" and grants no protection whatsoever; what actually
	// keeps the key private is that the path sits under the user's own profile
	// (%AppData%), which carries a restrictive ACL by default. That is why the
	// permission test asserts modes on Unix and asserts the path on Windows.
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// rename is os.Rename everywhere except in the test that has to observe the one
// instant which makes Save atomic: the temp file complete on disk while the
// target still holds the previous version. That instant leaves no trace once the
// rename returns, so nothing outside the package can see it, and a Save that
// wrote into the target directly would look identical afterwards.
var rename = os.Rename

// Store is the agent's persisted local configuration.
type Store struct {
	// mu guards every field below it. The interactive shell calls Get from a
	// repaint while a slash command calls Set, and an unguarded map read
	// concurrent with a map write is not a stale value, it is a runtime
	// throw that kills the process.
	mu       sync.Mutex
	path     string
	env      map[string]string
	provider string

	// extra carries top-level fields this build does not know about, so a file
	// written by a newer version survives a load/save round trip here instead
	// of being silently truncated to the two fields below. Nested unknown
	// fields inside "env" cannot survive — "env" is a map[string]string and
	// anything else in it is a load error, not data to preserve.
	extra map[string]json.RawMessage
}

// Path returns where settings live: $AGENT_SETTINGS if set, else
// <os.UserConfigDir()>/bash-is-all-you-need/settings.json.
func Path() (string, error) {
	if p := os.Getenv(PathEnv); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		// Reachable when neither %AppData% nor $HOME/$XDG_CONFIG_HOME is set —
		// a service account, or a container with no home. The caller can still
		// pass an explicit path, so this is an error rather than a guess at
		// /tmp, which would put an API key somewhere world-readable.
		return "", fmt.Errorf("settings path: %w", err)
	}
	return filepath.Join(dir, dirName, fileName), nil
}

// Load reads a store. path == "" means Path(). A missing file is not an error:
// it returns an empty store whose Save() will create it.
func Load(path string) (*Store, error) {
	if path == "" {
		p, err := Path()
		if err != nil {
			return nil, err
		}
		path = p
	}
	s := &Store{
		path:  path,
		env:   map[string]string{},
		extra: map[string]json.RawMessage{},
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}

	// A damaged file is an error that names the path, and nothing in this
	// package deletes or rewrites it. The file still contains the user's API
	// key; a package that "recovered" by starting empty would destroy the one
	// value that is expensive to replace, and it would do it at startup, before
	// anybody had a chance to look. Whether to overwrite is the caller's
	// decision. An empty file counts as damaged for the same reason: it is what
	// a non-atomic writer leaves behind when it dies mid-write.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, value := range top {
		switch name {
		case "env":
			if err := json.Unmarshal(value, &s.env); err != nil {
				return nil, fmt.Errorf(`%s: field "env": %w`, path, err)
			}
		case "provider":
			if err := json.Unmarshal(value, &s.provider); err != nil {
				return nil, fmt.Errorf(`%s: field "provider": %w`, path, err)
			}
		default:
			s.extra[name] = value
		}
	}
	if s.env == nil { // literal "env": null
		s.env = map[string]string{}
	}
	return s, nil
}

// Path reports the file this store loads from and saves to.
func (s *Store) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// Save writes the store to its path, atomically.
func (s *Store) Save() error {
	// Rendering happens under the lock; the file I/O does not. Holding a mutex
	// across a disk write would stall the repaint that Get serves, and the
	// bytes are already a snapshot by then, so there is nothing to gain.
	s.mu.Lock()
	path := s.path
	data, err := s.marshal()
	s.mu.Unlock()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}

	// Temp file in the same directory, then rename over the target. os.Rename
	// replaces the target in one step within a single directory, so a crash, a
	// full disk or a killed process leaves the previous file whole instead of a
	// truncated one — the failure this shape prevents is a settings file
	// holding half an API key and no closing brace.
	tmp, err := os.CreateTemp(dir, ".settings-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Removes the temp file on every error path below, and is a harmless no-op
	// once the rename has moved it away. Without it a failing Save would leave
	// a file containing the API key sitting next to the real one.
	defer os.Remove(tmpName)

	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// The rename can otherwise reach the disk before the bytes do, and what
	// survives a power loss is a zero-length settings file. Load calls that an
	// error, which is the right answer to a question worth not asking.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := rename(tmpName, path); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// The directory entry itself is not fsynced. On Linux that leaves the
	// rename itself undurable across a power loss; the cost of getting it right
	// is a directory handle that Windows will not sync, and the outcome it
	// protects is one lost edit, not a corrupt file.
	return nil
}

// marshal renders the file. Caller holds the lock.
//
// The top level is a map rather than a struct so unknown fields ride along.
// encoding/json sorts map keys, which is what makes the file diffable and a
// rewrite that changes nothing produce no diff — asserted in a test, because
// depending on it without checking is how a format silently stops being stable.
func (s *Store) marshal() ([]byte, error) {
	top := make(map[string]json.RawMessage, len(s.extra)+2)
	for name, value := range s.extra {
		top[name] = value
	}
	env, err := json.Marshal(s.env)
	if err != nil {
		return nil, err
	}
	top["env"] = env
	if s.provider != "" {
		provider, err := json.Marshal(s.provider)
		if err != nil {
			return nil, err
		}
		top["provider"] = provider
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// A base URL with a query string contains an ampersand, which the default
	// HTML escaping rewrites into a numeric unicode escape: still valid JSON,
	// and no longer the string the person checking the endpoint by eye typed.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(top); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil // Encode supplies the trailing newline.
}

// Get returns a stored variable's value.
func (s *Store) Get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.env[name]
	return v, ok
}

// Set stores a variable in memory. Nothing reaches disk until Save.
func (s *Store) Set(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.env == nil {
		s.env = map[string]string{}
	}
	s.env[name] = value
}

// Unset removes a variable in memory. Nothing reaches disk until Save.
func (s *Store) Unset(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.env, name)
}

// Names returns the stored variable names, sorted.
func (s *Store) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A fresh slice every call. Returning the store's own backing array would
	// let a caller that keeps the slice — the shell's completion list, which
	// holds it for as long as the prompt is open — sort or overwrite the store's
	// keys from outside the lock.
	out := make([]string, 0, len(s.env))
	for name := range s.env {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Provider is the name of the selected provider, or "" if none is selected.
func (s *Store) Provider() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider
}

// SetProvider selects a provider in memory. Nothing reaches disk until Save.
func (s *Store) SetProvider(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = name
}

// ExportMissing sets every stored variable that is NOT already present in the
// process environment, and returns the names it set, sorted.
//
// The environment always wins. `set -a && . ./.env`, a CI secret and a
// one-off `AGENT_MODEL=... agent` keep behaving exactly as they did before this
// file existed, and the failure that prevents is the worst kind: a stale key in
// a config file quietly overriding the one the operator just exported, and a
// session that talks to the wrong endpoint while both values look correct.
//
// Presence is what counts, not emptiness. A variable exported as empty is
// something a shell did on purpose, and treating it as absent would make this
// function overwrite exactly the case it exists to respect.
func (s *Store) ExportMissing() []string {
	s.mu.Lock()
	pairs := make(map[string]string, len(s.env))
	for name, value := range s.env {
		pairs[name] = value
	}
	s.mu.Unlock()

	set := make([]string, 0, len(pairs))
	for name, value := range pairs {
		if _, ok := os.LookupEnv(name); ok {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			// A rejected name (empty, or containing '=') is the only way this
			// fails. Skipping it keeps a hand-edited settings file from taking
			// down the shell at startup over one bad line.
			continue
		}
		set = append(set, name)
	}
	sort.Strings(set)
	return set
}
