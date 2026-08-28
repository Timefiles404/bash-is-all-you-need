// Stage 08 — why you cannot secure a shell by reading the command.
//
// This file is three implementations of one rule, each better than the last,
// and the point of having all three is that the first two are what everybody
// ships and both of them are defeated in one line.
//
// The rule, deliberately tiny so it can be reasoned about completely:
//
//	**the agent may not read .env**
//
// Not "the agent may not do dangerous things" — a rule that vague cannot be
// tested, and a policy you cannot test is a policy you are guessing about. One
// file, one verb.
//
//	inspectString   look at the command text            defeated by quoting
//	inspectAST      parse it and look at the words      defeated by expansion
//	sandbox.exec    BE the shell and look at the argv   see shell.go
//
// The progression is not "add more patterns". Each level moves the check to a
// place where more of the truth is available, and the last one moves it to the
// only place where the truth is complete: after expansion, when the argument
// vector is final and there is nothing left to hide behind.
package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// secretName is the file the policy protects.
const secretName = ".env"

// refusal is a blocked operation and the reason, in the words the model will
// read. A policy that says "denied" teaches the model nothing except to try
// again; a policy that says what it objected to lets the model do the task
// another way.
type refusal struct {
	Level string // which inspector caught it
	What  string // the exact text or argument that matched
	Why   string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("blocked by the %s policy: %s (matched %q)", r.Level, r.Why, r.What)
}

// isSecretPath reports whether a path refers to the protected file.
//
// Base-name matching, and the limitation is deliberate and worth naming: this
// does not resolve symlinks, does not canonicalise `..`, and does not know that
// `/proc/self/cwd/.env` is the same file. A real policy needs
// filepath.EvalSymlinks and a containment check against a root — and even then
// it races, because a path can be replaced between the check and the open.
// TOCTOU is not an edge case in a sandbox, it is the standard attack.
func isSecretPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	return filepath.Base(filepath.Clean(p)) == secretName
}

// ---------------------------------------------------------------------------
// Level 1: look at the string
// ---------------------------------------------------------------------------

// denyPattern is what a first implementation always looks like.
var denyPattern = regexp.MustCompile(`\.env\b`)

// inspectString is the check almost every agent harness ships, in some form.
//
// It works on the commands you thought of. It is defeated by the shell's own
// syntax — not by anything clever, just by the ordinary features of the
// language the string is written in. `cat ".e""nv"` is the same command to
// bash and a different string to this function, and no amount of pattern
// refinement fixes that, because the pattern is looking at source text while
// the shell is looking at what the source text *means*.
//
// See bypass_test.go for the measured list.
func inspectString(command string) *refusal {
	if m := denyPattern.FindString(command); m != "" {
		return &refusal{Level: "string", What: m, Why: "the command mentions " + secretName}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Level 2: parse it
// ---------------------------------------------------------------------------

// inspectAST parses the command and inspects the words a real shell parser
// produced.
//
// This is a genuine improvement and not a cosmetic one. The parser knows that
// `".e""nv"` is one word whose literal parts concatenate to `.env`, that
// `'.env'` is a quoted literal, and that `cat<.env` has a redirect even though
// there is no whitespace. Every quoting trick that beats level 1 dies here,
// because the parser is doing the same job the shell does.
//
// What it cannot know is any value that does not exist yet. `$X`, `$(...)`,
// `${x:-...}` and `eval` all mean "the value is computed later", and later has
// not happened at parse time. That is not a gap in the implementation. It is a
// property of the language: the shell is not a syntax, it is an evaluator, and
// a parse tree of an evaluator's input does not tell you what the evaluator
// will do.
//
// A parse error is treated as a refusal rather than as a pass. A command this
// cannot parse is a command it cannot judge, and "I could not understand it, so
// I allowed it" is the wrong default for a check whose whole job is judging.
func inspectAST(command string) *refusal {
	f, err := syntax.NewParser().Parse(strings.NewReader(command), "cmd")
	if err != nil {
		return &refusal{Level: "ast", What: command, Why: "the command could not be parsed, so it could not be checked"}
	}

	var found *refusal
	syntax.Walk(f, func(node syntax.Node) bool {
		if found != nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			for _, w := range n.Args {
				if lit, ok := literalWord(w); ok && isSecretPath(lit) {
					found = &refusal{Level: "ast", What: lit,
						Why: "an argument resolves to " + secretName}
					return false
				}
			}
		case *syntax.Redirect:
			// The one a string check misses entirely and an argv check misses
			// too: `cat < .env` runs `cat` with NO arguments. The file is
			// opened by the shell, not by the program, so a policy that only
			// looks at argv never sees the filename at all.
			if n.Word != nil {
				if lit, ok := literalWord(n.Word); ok && isSecretPath(lit) {
					found = &refusal{Level: "ast", What: lit,
						Why: "a redirect targets " + secretName}
					return false
				}
			}
		}
		return true
	})
	return found
}

// literalWord returns a word's value if — and only if — the whole word is
// literal.
//
// The second return value is the honest part. A word containing a parameter
// expansion or a command substitution has no value at parse time, and this
// says so rather than returning the parts it happens to understand. Returning
// `".en"` for `.en$X` would be worse than returning nothing, because a caller
// would then compare a partial value against a policy and conclude it was safe.
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				// $'...' is C-style escaping: $'\x2eenv' is .env, and the
				// parser stores the escape text, not the decoded bytes. We do
				// not decode it here — that would be re-implementing a piece of
				// the shell inside the policy, which is the whole trap this
				// chapter is about. Report "not literal" and let level 3 see
				// the real value.
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false // an expansion inside the quotes
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false // ParamExp, CmdSubst, ArithmExp, ExtGlob, …
		}
	}
	return b.String(), true
}
