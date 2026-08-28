package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Every test writes inside t.TempDir(). Nothing here may touch the real
// settings file: this repo is developed on the same machine that runs the
// agent, and a test that wrote to os.UserConfigDir() would overwrite a working
// API key.

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "settings.json")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestPathHonoursTheOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv(PathEnv, want)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathDefaultsUnderTheUsersOwnConfigDir(t *testing.T) {
	// Empty counts as unset, which is the only way t.Setenv can clear a
	// variable the developer may have exported.
	t.Setenv(PathEnv, "")

	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	if want := filepath.Join(dir, dirName, fileName); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}

	if runtime.GOOS == "windows" {
		// The 0o600 on the file means nothing here, so the protection has to
		// come from where the file is: under the user's profile, which Windows
		// gives a restrictive ACL by default. A path outside it — ProgramData,
		// a drive root — would be readable by every account on the machine.
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir: %v", err)
		}
		// Case-insensitive because %USERPROFILE% and %AppData% are two
		// separate strings Windows fills in, and a case difference between
		// them is not a security finding.
		prefix := strings.ToLower(home) + string(os.PathSeparator)
		if !strings.HasPrefix(strings.ToLower(got), prefix) {
			t.Errorf("Path() = %q, which is not under the user profile %q", got, home)
		}
	}
	// Logged rather than asserted: where the file lands is the first question
	// anyone asks about this package, and the answer differs per OS.
	t.Logf("default settings path on %s: %s", runtime.GOOS, got)
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	path := tempPath(t)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Path() != path {
		t.Errorf("Path() = %q, want %q", s.Path(), path)
	}
	if names := s.Names(); names == nil {
		t.Error("Names() on an empty store is nil, want an empty slice")
	} else if len(names) != 0 {
		t.Errorf("Names() = %v, want empty", names)
	}
	if v, ok := s.Get("AGENT_API_KEY"); ok || v != "" {
		t.Errorf("Get on an empty store = (%q, %v), want (\"\", false)", v, ok)
	}
	if p := s.Provider(); p != "" {
		t.Errorf("Provider() = %q, want empty", p)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Save did not create the file: %v", err)
	}
}

func TestGetSetUnsetAndProviderRoundTripThroughTheFile(t *testing.T) {
	path := tempPath(t)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("AGENT_BASE_URL", "https://api.example.com/v1")
	s.Set("AGENT_API_KEY", "sk-secret-value")
	s.Set("AGENT_GONE", "x")
	s.Unset("AGENT_GONE")
	s.SetProvider("kimi")

	if _, ok := s.Get("AGENT_GONE"); ok {
		t.Error("Unset left the variable in the store")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if v, ok := reloaded.Get("AGENT_API_KEY"); !ok || v != "sk-secret-value" {
		t.Errorf("after reload Get(AGENT_API_KEY) = (%q, %v)", v, ok)
	}
	if got, want := reloaded.Provider(), "kimi"; got != want {
		t.Errorf("after reload Provider() = %q, want %q", got, want)
	}
	want := []string{"AGENT_API_KEY", "AGENT_BASE_URL"}
	if got := reloaded.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("after reload Names() = %v, want %v", got, want)
	}
}

func TestSaveFormatIsSortedIndentedAndDiffable(t *testing.T) {
	path := tempPath(t)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Inserted out of order on purpose. encoding/json sorts map keys, and this
	// assertion is here so that a change in that behaviour shows up as a
	// failure rather than as a settings file whose lines shuffle on every
	// write, making every diff unreadable.
	s.Set("AGENT_PROTOCOL", "openai")
	s.Set("AGENT_MODEL", "some-model")
	s.Set("AGENT_API_KEY", "sk-0123456789")
	s.Set("AGENT_BASE_URL", "https://api.example.com/v1")
	s.SetProvider("kimi")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	want := `{
  "env": {
    "AGENT_API_KEY": "sk-0123456789",
    "AGENT_BASE_URL": "https://api.example.com/v1",
    "AGENT_MODEL": "some-model",
    "AGENT_PROTOCOL": "openai"
  },
  "provider": "kimi"
}
`
	if got := readFile(t, path); got != want {
		t.Errorf("file contents:\n%s\nwant:\n%s", got, want)
	}

	// A rewrite that changes nothing must produce no diff.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := reloaded.Save(); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	if got := readFile(t, path); got != want {
		t.Errorf("load/save round trip changed the file:\n%s", got)
	}
}

func TestUnknownTopLevelFieldsSurviveARoundTrip(t *testing.T) {
	path := tempPath(t)
	original := `{
  "env": {
    "AGENT_MODEL": "m"
  },
  "future": {
    "nested": [
      1,
      2
    ]
  },
  "provider": "kimi"
}
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Errorf("round trip lost or reformatted an unknown field:\ngot:\n%s\nwant:\n%s", got, original)
	}
}

func TestSavePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bash-is-all-you-need")
	path := filepath.Join(dir, "settings.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("AGENT_API_KEY", "sk-0123456789")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if runtime.GOOS == "windows" {
		// Asserting 0o600 here would assert a fiction: Windows maps chmod onto
		// the read-only attribute and ignores the rest, so the mode bits Go
		// reports back are invented. What protects the file on Windows is the
		// ACL on the user profile directory, asserted by
		// TestPathDefaultsUnderTheUsersOwnConfigDir.
		t.Skip("file modes are not enforced on Windows")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("file mode = %#o, want %#o", got, filePerm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != dirPerm {
		t.Errorf("directory mode = %#o, want %#o", got, dirPerm)
	}
}

func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i, value := range []string{"first", "second", "third"} {
		s.Set("AGENT_MODEL", value)
		if err := s.Save(); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{"settings.json"}) {
		t.Errorf("directory holds %v, want only settings.json — a leftover temp file is a second copy of the API key", names)
	}
}

func TestSaveReplacesTheTargetInsteadOfWritingIntoIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("AGENT_MODEL", "first")
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	previous := readFile(t, path)

	// The hook runs at the moment the previous file is still whole and the new
	// one is complete somewhere else. A Save that opened the target and
	// truncated it would never get here, which is what the "called" check
	// below reports.
	original := rename
	t.Cleanup(func() { rename = original })
	called := 0
	rename = func(from, to string) error {
		called++
		if got := readFile(t, from); !strings.Contains(got, "second") {
			t.Errorf("temp file at the rename is not the complete new version: %q", got)
		}
		if got := readFile(t, to); got != previous {
			t.Errorf("target already changed before the rename: %q, want the previous version %q", got, previous)
		}
		return original(from, to)
	}

	s.Set("AGENT_MODEL", "second")
	if err := s.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if called != 1 {
		t.Fatalf("rename called %d times, want 1 — Save is writing the target in place, so a crash mid-write truncates it", called)
	}
	if got := readFile(t, path); !strings.Contains(got, "second") {
		t.Errorf("after Save the file is %q, want the new version", got)
	}
}

func TestSaveFailureLeavesThePreviousFileCompleteAndIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("AGENT_API_KEY", "sk-the-only-copy")
	s.SetProvider("kimi")
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	previous := readFile(t, path)

	original := rename
	t.Cleanup(func() { rename = original })
	rename = func(from, to string) error { return fmt.Errorf("injected failure") }

	s.Set("AGENT_API_KEY", "sk-replacement")
	if err := s.Save(); err == nil {
		t.Fatal("Save returned nil after the rename failed")
	}
	if got := readFile(t, path); got != previous {
		t.Errorf("a failed Save damaged the previous file:\ngot:\n%s\nwant:\n%s", got, previous)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "settings.json" {
			t.Errorf("failed Save left %q behind, a second file holding the API key", e.Name())
		}
	}
}

func TestSaveFailureLeavesThePreviousFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Making the rename target a directory is the cheapest portable way to
	// fail the last step of Save, after the temp file has been written in
	// full. What the marker file stands in for is the previous settings file:
	// the guarantee is that a failed Save destroys nothing.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	marker := filepath.Join(path, "previous")
	if err := os.WriteFile(marker, []byte("previous state"), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	s := &Store{path: path, env: map[string]string{"AGENT_API_KEY": "sk-0123456789"}}
	err := s.Save()
	if err == nil {
		t.Fatal("Save onto a directory returned nil, want an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %q", err, path)
	}
	if got := readFile(t, marker); got != "previous state" {
		t.Errorf("failed Save damaged what was there: %q", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "settings.json" {
			t.Errorf("failed Save left %q behind", e.Name())
		}
	}
}

func TestLoadCorruptFileErrorsAndChangesNothing(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"truncated object", "{\n  \"env\": {\n    \"AGENT_API_KEY\": \"sk-01234"},
		{"empty file", ""},
		{"env is not an object", `{"env": 7}`},
		{"top level is an array", `["env"]`},
		{"trailing garbage", `{"env": {}} and then some`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := tempPath(t)
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}

			s, err := Load(path)
			if err == nil {
				t.Fatalf("Load returned nil error for %s", c.name)
			}
			if s != nil {
				t.Error("Load returned a store alongside the error; a caller that ignores the error would then Save over the damaged file")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the path %q", err, path)
			}
			if got := readFile(t, path); got != c.content {
				t.Errorf("Load rewrote the file: %q", got)
			}
		})
	}
}

func TestExportMissingNeverOverwritesTheEnvironment(t *testing.T) {
	const (
		present      = "SETTINGS_TEST_PRESENT"
		presentEmpty = "SETTINGS_TEST_PRESENT_EMPTY"
		absent       = "SETTINGS_TEST_ABSENT"
	)
	t.Setenv(present, "from the shell")
	t.Setenv(presentEmpty, "")
	// t.Setenv only restores what t.Setenv set; ExportMissing sets this one
	// itself, so the test has to undo it or it leaks into the next test.
	t.Cleanup(func() { os.Unsetenv(absent) })

	s, err := Load(tempPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set(present, "from the file")
	s.Set(presentEmpty, "from the file")
	s.Set(absent, "from the file")

	got := s.ExportMissing()
	if want := []string{absent}; !reflect.DeepEqual(got, want) {
		t.Errorf("ExportMissing() = %v, want %v", got, want)
	}
	if v := os.Getenv(present); v != "from the shell" {
		t.Errorf("%s = %q; the environment must win over the file", present, v)
	}
	// A variable exported as empty is a deliberate act by a shell, and
	// presence — not emptiness — is what ExportMissing respects.
	if v := os.Getenv(presentEmpty); v != "" {
		t.Errorf("%s = %q, want empty", presentEmpty, v)
	}
	if v := os.Getenv(absent); v != "from the file" {
		t.Errorf("%s = %q, want the value from the file", absent, v)
	}
}

func TestExportMissingReturnsSortedNamesAndNeverNil(t *testing.T) {
	s, err := Load(tempPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s.ExportMissing(); got == nil || len(got) != 0 {
		t.Errorf("ExportMissing() on an empty store = %v, want an empty non-nil slice", got)
	}

	for _, name := range []string{"SETTINGS_TEST_SORT_C", "SETTINGS_TEST_SORT_A", "SETTINGS_TEST_SORT_B"} {
		s.Set(name, "v")
		t.Cleanup(func() { os.Unsetenv(name) })
	}
	want := []string{"SETTINGS_TEST_SORT_A", "SETTINGS_TEST_SORT_B", "SETTINGS_TEST_SORT_C"}
	if got := s.ExportMissing(); !reflect.DeepEqual(got, want) {
		t.Errorf("ExportMissing() = %v, want %v", got, want)
	}
}

func TestNamesReturnsACopy(t *testing.T) {
	s, err := Load(tempPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("AGENT_BASE_URL", "u")
	s.Set("AGENT_MODEL", "m")

	names := s.Names()
	names[0] = "clobbered"
	names = append(names, "appended")
	_ = names

	want := []string{"AGENT_BASE_URL", "AGENT_MODEL"}
	if got := s.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("after a caller mutated the returned slice, Names() = %v, want %v", got, want)
	}
	if v, ok := s.Get("AGENT_BASE_URL"); !ok || v != "u" {
		t.Errorf("Get(AGENT_BASE_URL) = (%q, %v) after the caller mutated its slice", v, ok)
	}
}

func TestConcurrentAccessIsRaceFree(t *testing.T) {
	// Runs under -race. The shape is the real one: a repaint reading while a
	// slash command writes and another saves.
	s, err := Load(tempPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("AGENT_MODEL", "start")

	const rounds = 200
	var wg sync.WaitGroup

	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				s.Get("AGENT_MODEL")
				s.Names()
				s.Provider()
				s.Path()
			}
		}()
	}
	for writer := 0; writer < 4; writer++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				name := fmt.Sprintf("AGENT_VAR_%d_%d", id, i)
				s.Set(name, "v")
				s.SetProvider(fmt.Sprintf("p%d", id))
				s.Unset(name)
			}
		}(writer)
	}
	// One saver only. Two goroutines renaming onto the same path is a race
	// between callers that no lock inside this package can order; testing it
	// would measure the filesystem, not the code.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := s.Save(); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
