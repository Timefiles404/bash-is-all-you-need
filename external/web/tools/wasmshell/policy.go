//go:build js && wasm

// The stage 08 policy, carried over so the lesson can be driven from a browser.
//
// 08-sandbox/code/policy.go is three implementations of one rule — "the agent
// may not read .env" — each better than the last, and the chapter's value is
// that the first two are what everybody ships and both are defeated in one
// line. The site lets a learner switch between them and re-run the same bypass,
// which is the one thing reading the chapter cannot do.
//
// This is a copy, and copies rot. web/tools/genlevels checks it: the bypass
// corpus it extracts comes from 08-sandbox/code/bypass_test.go, and the
// generated level asserts the verdicts this file produces against the verdicts
// the repo's own test asserts. If the two ever disagree, the level fails to
// build rather than teaching something the repo no longer does.
package main

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type refusal struct {
	Level string
	What  string
	Why   string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("blocked by the %s policy: %s (matched %q)", r.Level, r.Why, r.What)
}

type policy struct {
	// level is "off", "string", "ast" or "argv" — the three inspectors plus a
	// mode where the sandbox observes and refuses nothing, which is stage 08's
	// enforce=false and is where most of the chapter's value actually is.
	level   string
	enforce bool
	secret  string
}

func defaultPolicy() *policy {
	return &policy{level: "argv", enforce: true, secret: ".env"}
}

// isSecret is the repo's base-name match, limitations and all: no symlink
// resolution, no canonicalisation of `..`, no idea that /proc/self/cwd/.env is
// the same file. Keeping the weakness is the point — a learner who finds it has
// found what the chapter says is there.
func (p *policy) isSecret(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return path.Base(path.Clean(s)) == p.secret
}

// checkLine is level 1: look at the string. Defeated by quoting.
func (p *policy) checkLine(line string) *refusal {
	if p.level != "string" {
		return nil
	}
	re := regexp.MustCompile(regexp.QuoteMeta(p.secret) + `\b`)
	if m := re.FindString(line); m != "" {
		return &refusal{Level: "string", What: m, Why: "the command mentions " + p.secret}
	}
	return nil
}

// checkAST is level 2: parse it and look at the words. Defeated by expansion.
func (p *policy) checkAST(line string) *refusal {
	if p.level != "ast" {
		return nil
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(line), "cmd")
	if err != nil {
		return &refusal{Level: "ast", What: line, Why: "the command could not be parsed, so it could not be checked"}
	}
	var found *refusal
	syntax.Walk(f, func(node syntax.Node) bool {
		if found != nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			for _, w := range n.Args {
				if lit, ok := literalWord(w); ok && p.isSecret(lit) {
					found = &refusal{Level: "ast", What: lit, Why: "an argument resolves to " + p.secret}
					return false
				}
			}
		case *syntax.Redirect:
			if n.Word != nil {
				if lit, ok := literalWord(n.Word); ok && p.isSecret(lit) {
					found = &refusal{Level: "ast", What: lit, Why: "a redirect targets " + p.secret}
					return false
				}
			}
		}
		return true
	})
	return found
}

// checkArgv is level 3, and it runs in the exec handler where argv is final.
func (p *policy) checkArgv(args []string) *refusal {
	if p.level != "argv" {
		return nil
	}
	for _, a := range args[1:] {
		if p.isSecret(a) {
			return &refusal{Level: "sandbox/exec", What: a,
				Why: "an argument resolves to " + p.secret + " after expansion"}
		}
	}
	if len(args) >= 3 && args[1] == "-c" {
		switch args[0] {
		case "sh", "bash", "dash", "zsh", "ksh":
			return &refusal{Level: "sandbox/exec", What: strings.Join(args, " "),
				Why: "a nested shell would run outside this sandbox's view"}
		}
	}
	return nil
}

// checkPath runs in the open handler, for redirections.
func (p *policy) checkPath(target string) *refusal {
	if p.level != "argv" && p.level != "ast" {
		return nil
	}
	if p.isSecret(target) {
		return &refusal{Level: "sandbox/open", What: target, Why: "a redirect targets " + p.secret}
	}
	return nil
}

// literalWord returns a word's value only if the whole word is literal. A word
// holding an expansion has no value at parse time, and saying so is the whole
// difference between level 2 and level 3.
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch t := part.(type) {
		case *syntax.Lit:
			b.WriteString(t.Value)
		case *syntax.SglQuoted:
			if t.Dollar {
				return "", false
			}
			b.WriteString(t.Value)
		case *syntax.DblQuoted:
			for _, q := range t.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}
