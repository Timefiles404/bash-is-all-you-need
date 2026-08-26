package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The chapter's evidence, generated rather than asserted from memory.
//
// One rule — "do not read .env" — and one question per command: does the bypass
// actually work, and which of the three inspectors catches it?
//
// Every case is *verified to be a real bypass first*, by running it with the
// policy switched off and checking the file's contents actually came out. A
// bypass table full of commands that never worked would prove nothing, and that
// is the commonest way this kind of table is wrong.

const canary = "CANARY_dc41f0b7"

// bypassCase is one attempt to read the protected file.
type bypassCase struct {
	name    string
	command string
	note    string
}

var bypassCases = []bypassCase{
	{"plain", `cat .env`, "the command everybody blocks"},
	{"single quotes", `cat '.env'`, ""},
	{"split across quotes", `cat ".e""nv"`, "one word to the shell, two strings to a regexp"},
	{"empty quotes inside", `cat .en''v`, ""},
	{"backslash", `cat .en\v`, ""},
	{"leading ./", `cat ./.env`, "a different string, the same file"},
	{"ANSI-C quoting", `cat $'\x2eenv'`, "the name never appears as text at all"},
	{"variable", `X=.env; cat $X`, "the value does not exist until runtime"},
	{"command substitution", `cat $(echo .env)`, ""},
	{"eval", `eval "cat .env"`, "the program is data until it is not"},
	{"parameter default", `cat "${MISSING:-.env}"`, ""},
	{"loop", `for f in .env; do cat "$f"; done`, ""},
	{"redirect", `cat < .env`, "argv is just [\"cat\"] — no filename anywhere in it"},
	{"nested shell", `sh -c 'cat .env'`, "a whole program smuggled in one argument"},
}

// runIn executes a command in a scratch directory holding the canary file, and
// reports whether the secret escaped and whether the sandbox blocked anything.
func runIn(t *testing.T, dir, command string, enforce bool) (leaked, blocked bool, out string) {
	t.Helper()
	sb := newSandbox(dir, NewBus(), enforce)
	r := sb.run(command, 10*time.Second)
	out = r.Stdout + r.Stderr
	return strings.Contains(r.Stdout, canary), len(sb.blocked) > 0, out
}

func bypassDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, secretName), []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTheBypassTable is the chapter. It prints the table it measures.
func TestTheBypassTable(t *testing.T) {
	dir := bypassDir(t)

	// If the environment cannot run the baseline, none of the rest means
	// anything. Skip loudly rather than reporting a table of zeroes.
	if leaked, _, out := runIn(t, dir, "cat .env", false); !leaked {
		t.Skipf("this machine cannot run `cat` through the interpreter, so the table cannot be measured: %s", out)
	}

	type row struct {
		c                      bypassCase
		works, l1, l2, l3      bool
		outputWhenNotEnforcing string
	}
	var rows []row

	for _, c := range bypassCases {
		var r row
		r.c = c

		// 1. Does the bypass actually work with nothing in the way?
		r.works, _, r.outputWhenNotEnforcing = runIn(t, dir, c.command, false)

		// 2. The two static inspectors.
		r.l1 = inspectString(c.command) != nil
		r.l2 = inspectAST(c.command) != nil

		// 3. The interpreter, enforcing.
		leaked, blocked, _ := runIn(t, dir, c.command, true)
		r.l3 = blocked && !leaked

		rows = append(rows, r)
	}

	t.Log("")
	t.Logf("%-22s %-7s %-8s %-6s %-9s", "command", "works?", "string", "ast", "sandbox")
	for _, r := range rows {
		mark := func(b bool) string {
			if b {
				return "blocked"
			}
			return "  --   "
		}
		t.Logf("%-22s %-7v %-8s %-6s %-9s  %s", r.c.name, r.works,
			mark(r.l1), mark(r.l2), mark(r.l3), r.c.note)
	}
	t.Log("")

	// --- the assertions the table has to satisfy -------------------------

	var stringMissed, astMissed int
	for _, r := range rows {
		if !r.works {
			// A case that does not read the file is not a bypass and should not
			// be in the table pretending to be one.
			t.Errorf("%q did not actually read the file, so it is not a bypass — fix or remove the case.\noutput: %s",
				r.c.command, r.outputWhenNotEnforcing)
			continue
		}
		if !r.l1 {
			stringMissed++
		}
		if !r.l2 {
			astMissed++
		}
		if !r.l3 {
			t.Errorf("the sandbox did NOT stop %q (%s).\n"+
				"This is the claim the whole chapter rests on: after expansion there is nowhere left to hide.\n"+
				"output: %s", r.c.command, r.c.name, r.outputWhenNotEnforcing)
		}
	}

	// Both static checks must lose, and — the finding that is more interesting
	// than the one this table was built to show — they must lose on DIFFERENT
	// commands.
	//
	// The string check misses `cat ".e""nv"` because the text is split. The AST
	// check catches that one and misses `eval "cat .env"`, where the text is
	// right there but the word belongs to a program that does not exist yet.
	// Neither set contains the other.
	//
	// Which kills the obvious response to this chapter — "run both checks" —
	// because an attacker does not have to defeat a conjunction. Every command
	// only has to defeat one property at a time, and shell syntax offers a
	// choice of which. TestExpansionBeatsParsing is one line that defeats both.
	if stringMissed == 0 {
		t.Error("the regexp check caught every bypass, which would mean shell quoting does not exist; the table is wrong")
	}
	if astMissed == 0 {
		t.Error("the AST check caught everything, which would mean expansion is decidable at parse time. It is not; the table is wrong")
	}
	stringOnly, astOnly := 0, 0
	for _, r := range rows {
		if !r.l1 && r.l2 {
			astOnly++ // parsing caught what pattern matching missed
		}
		if r.l1 && !r.l2 {
			stringOnly++ // pattern matching caught what parsing missed
		}
	}
	t.Logf("string missed %d · ast missed %d · caught only by ast: %d · caught only by string: %d",
		stringMissed, astMissed, astOnly, stringOnly)
	if astOnly == 0 || stringOnly == 0 {
		t.Errorf("one check's misses are a subset of the other's (ast-only %d, string-only %d) — "+
			"the chapter claims they fail on disjoint sets, and that claim is the reason "+
			"'just run both' does not work", astOnly, stringOnly)
	}
}

// The specific pair the chapter leans on hardest, asserted on its own so a
// failure names the mechanism rather than a row number.
func TestExpansionBeatsParsing(t *testing.T) {
	// The filename appears nowhere in the text (so the string check has nothing
	// to match) and the word is `${X}v`, a parameter expansion glued to a
	// literal (so the AST check correctly reports "I cannot know this yet").
	// One line, both static checks defeated, and the file is read.
	const command = `X=.en; eval 'cat ${X}v'`
	if inspectString(command) != nil {
		t.Error("the string check matched a command in which the filename never appears; the case is not testing what it claims")
	}
	if r := inspectAST(command); r != nil {
		t.Errorf("the AST check claims to have resolved %q, which would require evaluating the shell at parse time", command)
	}
	dir := bypassDir(t)
	leaked, blocked, out := runIn(t, dir, command, true)
	if leaked || !blocked {
		t.Errorf("the interpreter did not stop a command it had already expanded to `cat .env` (leaked=%v blocked=%v)\n%s",
			leaked, blocked, out)
	}
}

// The redirect case gets its own test because it is the one that defeats an
// argv-only policy at EVERY level, including the sandbox — unless the sandbox
// also handles file opens. `cat < .env` runs cat with no arguments at all.
func TestRedirectIsNotVisibleInArgv(t *testing.T) {
	sb := newSandbox(bypassDir(t), NewBus(), true)
	_ = sb.run("cat < .env", 10*time.Second)

	for _, argv := range sb.execs {
		if strings.Contains(argv, secretName) {
			t.Fatalf("argv %q contained the filename, so this case no longer demonstrates the point", argv)
		}
	}
	if len(sb.blocked) == 0 {
		t.Error("the redirect was not blocked: OpenHandler is the only thing that can see a file the shell opens itself, " +
			"and without it a policy that inspects argv has a hole shaped exactly like `<`")
	}
}

// And the honest limit, asserted rather than merely admitted. This test PASSES
// when the sandbox fails to stop the command, which is the point: an
// interpreter sees every command and cannot see inside one.
func TestTheSandboxCannotSeeInsideAProgram(t *testing.T) {
	dir := bypassDir(t)

	// Any interpreter that takes a program as an argument will do, and the
	// filename is assembled INSIDE it so that it never appears in argv. The
	// sandbox sees `awk -v a=.en <a program>` and has no opinion about the
	// program's contents, because having one would mean implementing awk.
	//
	// Candidates in order of how likely they are to exist next to a Git Bash.
	// The chosen one has to be verified to actually work — on Windows,
	// exec.LookPath("python") happily finds an App Execution Alias stub that
	// prints an advert for the Microsoft Store, and a test built on that would
	// report a false negative.
	candidates := []struct{ name, command string }{
		{"awk", `awk -v a=.en 'BEGIN{f=a"v"; while((getline l < f)>0) print l}'`},
		{"perl", `perl -e '$f=".en"."v"; open(F,"<",$f); print <F>;'`},
		{"python3", `python3 -c "p='.en'+'v'; print(open(p).read())"`},
		{"python", `python -c "p='.en'+'v'; print(open(p).read())"`},
	}
	var command string
	for _, c := range candidates {
		if _, err := exec.LookPath(c.name); err != nil {
			continue
		}
		if leaked, _, _ := runIn(t, dir, c.command, false); leaked {
			command = c.command
			break
		}
	}
	if command == "" {
		t.Skip("no working scripting interpreter on PATH; the limit this test documents still holds")
	}

	leaked, blocked, out := runIn(t, dir, command, true)
	if blocked {
		t.Fatalf("the sandbox blocked %q — if it can now see inside an interpreter, the chapter's honest limit needs rewriting\n%s", command, out)
	}
	if !leaked {
		t.Fatalf("the command did not read the file, so it does not demonstrate the limit; output: %s", out)
	}
	t.Log("as documented: the sandbox saw one exec, allowed it, and the program did the rest. " +
		"An embedded interpreter is a policy and observability layer, not a security boundary.")
}
