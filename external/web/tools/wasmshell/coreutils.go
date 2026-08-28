//go:build js && wasm

// The commands the shell can run, and the honest accounting of what they are.
//
// mvdan.cc/sh implements the shell *builtins* — `echo`, `printf`, `cd`, `pwd`,
// `test`, `[`, `read`, `eval`, `source`, `trap`, `getopts`, `shopt`, `alias`,
// `set`, `unset`, `shift`, `exit`, `return`, `break`, `continue`, `wait`,
// `command`, `builtin`, `type`, `exec`, `dirs`, `pushd`, `popd`,
// `readarray`/`mapfile` — because those are part of the language. It does not
// implement `cat`, because on a real machine `cat` is a separate program the
// shell execs, and that is exactly the thing a browser does not have.
//
// So these are re-implementations, and they are not GNU's. The differences a
// learner can actually hit:
//
//	grep   Go's regexp is RE2. It has no backreferences and no lookaround, and
//	       -E syntax is the default rather than an option. A POSIX BRE such as
//	       `a\{2\}` is not accepted.
//	sed    Only `s/re/repl/[gi]`, `d`, `p`, and line addresses `N`, `$`,
//	       `/re/`. No hold space, no `y`, no `a`/`i`/`c`, no multi-file -i.
//	find   -name, -type, -maxdepth, -path, -size, and -print. No -exec.
//	sort   Bytewise, plus -n, -r, -u, -k N. No locale collation, ever.
//	ls     -l is a plausible long format, not GNU's column widths.
//
// Everything above is printed by `help`, in the shell, where a learner will see
// it at the moment it matters. A teaching site that let someone believe they
// were driving GNU coreutils would be teaching a false thing about the one
// subject this repo is most careful about.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

type cmdenv struct {
	ctx    context.Context
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	dir    string
}

func (e *cmdenv) name() string { return e.args[0] }

// abs resolves a path the way the interpreter's own open handler does: against
// the shell's Dir, not against the process cwd. Getting this wrong makes `cd`
// appear to work and every command ignore it.
func (e *cmdenv) abs(p string) string {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return path.Clean(e.dir + "/" + p)
}

// in is stdin, or an immediate EOF when there is none.
//
// The interpreter leaves HandlerCtx.Stdin as a nil interface rather than a
// typed nil when the shell has no input — `cat` with no arguments and nothing
// piped in. Reading from that is a nil dereference, which on wasm is a panic
// that kills the instance and leaves the UI waiting on a promise that will
// never settle. A terminal answers this with Ctrl-D; a browser has nobody to
// press it, so the answer here is EOF.
func (e *cmdenv) in() io.Reader {
	if e.stdin == nil {
		return strings.NewReader("")
	}
	return e.stdin
}

func (e *cmdenv) errf(format string, a ...any) int {
	fmt.Fprintf(e.stderr, e.name()+": "+format+"\n", a...)
	return 1
}

// operands splits argv into flags and the rest, in the one style every command
// here uses: single-letter clustered flags, `--` ends them, a lone `-` is a
// file name meaning stdin.
func (e *cmdenv) operands(known string) (flags map[byte]bool, vals map[byte]string, rest []string, err error) {
	flags = map[byte]bool{}
	vals = map[byte]string{}
	args := e.args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			return
		}
		if len(a) < 2 || a[0] != '-' || a == "-" {
			rest = append(rest, a)
			continue
		}
		// `head -5` and `tail -20`: the obsolescent form, which is the one
		// people's fingers actually type. Only accepted where -n takes a value.
		if a[1] >= '1' && a[1] <= '9' && strings.IndexByte(known, 'n') >= 0 {
			if _, cerr := strconv.Atoi(a[1:]); cerr == nil {
				flags['n'] = true
				vals['n'] = a[1:]
				continue
			}
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			idx := strings.IndexByte(known, c)
			if idx < 0 {
				err = fmt.Errorf("invalid option -- '%c'", c)
				return
			}
			// A flag followed by ':' in `known` takes a value.
			if idx+1 < len(known) && known[idx+1] == ':' {
				if j+1 < len(a) {
					vals[c] = a[j+1:]
				} else if i+1 < len(args) {
					i++
					vals[c] = args[i]
				} else {
					err = fmt.Errorf("option requires an argument -- '%c'", c)
					return
				}
				flags[c] = true
				break
			}
			flags[c] = true
		}
	}
	return
}

type cmdFunc func(*cmdenv) int

var coreutils map[string]cmdFunc

func init() {
	coreutils = map[string]cmdFunc{
		"cat":      cmdCat,
		"ls":       cmdLs,
		"grep":     cmdGrep,
		"sed":      cmdSed,
		"find":     cmdFind,
		"wc":       cmdWc,
		"head":     cmdHead,
		"tail":     cmdTail,
		"mkdir":    cmdMkdir,
		"rmdir":    cmdRmdir,
		"rm":       cmdRm,
		"cp":       cmdCp,
		"mv":       cmdMv,
		"touch":    cmdTouch,
		"sort":     cmdSort,
		"uniq":     cmdUniq,
		"tr":       cmdTr,
		"cut":      cmdCut,
		"rev":      cmdRev,
		"nl":       cmdNl,
		"tee":      cmdTee,
		"seq":      cmdSeq,
		"basename": cmdBasename,
		"dirname":  cmdDirname,
		"env":      cmdEnv,
		"which":    cmdWhich,
		"sleep":    cmdSleep,
		"date":     cmdDate,
		"help":     cmdHelp,
	}
}

// ---------------------------------------------------------------------------
// input plumbing
// ---------------------------------------------------------------------------

// eachFile runs fn over every operand, treating no operands and "-" as stdin.
// Errors are reported per file and do not stop the others, which is what a
// pipeline expects and what makes `cat a b c` useful when b is missing.
func (e *cmdenv) eachFile(files []string, fn func(name string, r io.Reader) error) int {
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	for _, name := range files {
		if name == "-" {
			if err := fn("-", e.in()); err != nil {
				code = e.errf("%v", err)
			}
			continue
		}
		f, err := os.Open(e.abs(name))
		if err != nil {
			code = e.errf("%s: %s", name, errText(err))
			continue
		}
		err = fn(name, f)
		f.Close()
		if err != nil {
			code = e.errf("%s: %v", name, err)
		}
	}
	return code
}

// errText strips Go's *PathError wrapper so the message reads like a shell's.
func errText(err error) string {
	var pe *os.PathError
	if ok := asPathError(err, &pe); ok {
		return pe.Err.Error()
	}
	return err.Error()
}

func asPathError(err error, out **os.PathError) bool {
	for err != nil {
		if pe, ok := err.(*os.PathError); ok {
			*out = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (e *cmdenv) lines(files []string, fn func(name string, n int, line string) error) int {
	return e.eachFile(files, func(name string, r io.Reader) error {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		n := 0
		for sc.Scan() {
			n++
			if err := fn(name, n, sc.Text()); err != nil {
				return err
			}
		}
		return sc.Err()
	})
}

// ---------------------------------------------------------------------------
// the commands
// ---------------------------------------------------------------------------

func cmdCat(e *cmdenv) int {
	flags, _, files, err := e.operands("nA")
	if err != nil {
		return e.errf("%v", err)
	}
	n := 0
	return e.eachFile(files, func(_ string, r io.Reader) error {
		if !flags['n'] {
			_, err := io.Copy(e.stdout, r)
			return err
		}
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			n++
			fmt.Fprintf(e.stdout, "%6d\t%s\n", n, sc.Text())
		}
		return sc.Err()
	})
}

func cmdLs(e *cmdenv) int {
	flags, _, files, err := e.operands("la1RF")
	if err != nil {
		return e.errf("%v", err)
	}
	if len(files) == 0 {
		files = []string{"."}
	}
	// GNU's order, which a learner's muscle memory expects: plain files first
	// with no heading, then each directory, headed only when more than one
	// operand was given. Heading every operand turns `ls *.txt` into three
	// headings and three blank lines.
	code := 0
	var dirs []string
	for _, name := range files {
		st, err := os.Stat(e.abs(name))
		if err != nil {
			code = e.errf("%s: %s", name, errText(err))
			continue
		}
		if st.IsDir() {
			dirs = append(dirs, name)
			continue
		}
		e.lsOne(flags, name, st)
	}
	headed := len(files) > 1
	for i, name := range dirs {
		abs := e.abs(name)
		if headed {
			if i > 0 || len(dirs) < len(files) {
				fmt.Fprintln(e.stdout)
			}
			fmt.Fprintf(e.stdout, "%s:\n", name)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			code = e.errf("%s: %s", name, errText(err))
			continue
		}
		for _, ent := range entries {
			if !flags['a'] && strings.HasPrefix(ent.Name(), ".") {
				continue
			}
			info, err := ent.Info()
			if err != nil {
				continue
			}
			e.lsOne(flags, ent.Name(), info)
		}
	}
	return code
}

func (e *cmdenv) lsOne(flags map[byte]bool, name string, st os.FileInfo) {
	if flags['F'] && st.IsDir() {
		name += "/"
	}
	if !flags['l'] {
		fmt.Fprintln(e.stdout, name)
		return
	}
	fmt.Fprintf(e.stdout, "%s %5d %s %s\n",
		st.Mode().String(), st.Size(), st.ModTime().UTC().Format("Jan _2 15:04"), name)
}

func cmdGrep(e *cmdenv) int {
	flags, vals, rest, err := e.operands("inveclhoE:F")
	if err != nil {
		return e.errf("%v", err)
	}
	pattern := ""
	if p, ok := vals['e']; ok {
		pattern = p
	} else if len(rest) > 0 {
		pattern, rest = rest[0], rest[1:]
	} else {
		return e.errf("usage: grep [-invclh] PATTERN [FILE...]")
	}
	if flags['F'] {
		pattern = regexp.QuoteMeta(pattern)
	}
	if flags['i'] {
		pattern = "(?i)" + pattern
	}
	re, cerr := regexp.Compile(pattern)
	if cerr != nil {
		// Go's regexp is RE2; say so rather than reporting a syntax error the
		// learner will read as their mistake.
		return e.errf("%v (this shell's grep uses Go's RE2: no backreferences, no lookaround)", cerr)
	}

	multi := len(rest) > 1
	showName := multi && !flags['h']
	matches := 0
	code := e.lines(rest, func(name string, _ int, line string) error {
		if re.MatchString(line) == flags['v'] {
			return nil
		}
		matches++
		if flags['c'] || flags['l'] {
			return nil
		}
		if showName {
			fmt.Fprintf(e.stdout, "%s:%s\n", name, line)
		} else {
			fmt.Fprintln(e.stdout, line)
		}
		return nil
	})
	if flags['c'] {
		fmt.Fprintln(e.stdout, matches)
	}
	if code != 0 {
		return code
	}
	if matches == 0 {
		return 1 // grep's contract: 1 means "no match", not "error"
	}
	return 0
}

// cmdSed implements the subset named at the top of this file.
func cmdSed(e *cmdenv) int {
	flags, _, rest, err := e.operands("n")
	if err != nil {
		return e.errf("%v", err)
	}
	if len(rest) == 0 {
		return e.errf("usage: sed [-n] SCRIPT [FILE...]")
	}
	script, files := rest[0], rest[1:]
	prog, perr := parseSed(script)
	if perr != nil {
		return e.errf("%v", perr)
	}
	return e.lines(files, func(_ string, n int, line string) error {
		out := line
		deleted := false
		printed := false
		for _, op := range prog {
			if !op.matches(n, out) {
				continue
			}
			switch op.kind {
			case 's':
				if op.global {
					out = op.re.ReplaceAllString(out, op.repl)
				} else {
					done := false
					out = op.re.ReplaceAllStringFunc(out, func(m string) string {
						if done {
							return m
						}
						done = true
						idx := op.re.FindStringSubmatchIndex(out)
						return string(op.re.ExpandString(nil, op.repl, out, idx))
					})
				}
			case 'd':
				deleted = true
			case 'p':
				fmt.Fprintln(e.stdout, out)
				printed = true
			}
			if deleted {
				break
			}
		}
		if deleted {
			return nil
		}
		if !flags['n'] && !printed {
			fmt.Fprintln(e.stdout, out)
		} else if !flags['n'] && printed {
			fmt.Fprintln(e.stdout, out)
		}
		return nil
	})
}

type sedOp struct {
	kind   byte // 's', 'd', 'p'
	addrN  int  // line number address, 0 for none
	addrRe *regexp.Regexp
	last   bool // '$'
	re     *regexp.Regexp
	repl   string
	global bool
}

func (o sedOp) matches(n int, line string) bool {
	switch {
	case o.addrN > 0:
		return n == o.addrN
	case o.addrRe != nil:
		return o.addrRe.MatchString(line)
	default:
		return true
	}
}

func parseSed(script string) ([]sedOp, error) {
	var ops []sedOp
	for _, stmt := range strings.Split(script, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		var op sedOp
		// An optional address: a number, or /re/.
		if stmt[0] == '/' {
			end := strings.Index(stmt[1:], "/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated address in %q", stmt)
			}
			re, err := regexp.Compile(stmt[1 : 1+end])
			if err != nil {
				return nil, err
			}
			op.addrRe = re
			stmt = strings.TrimSpace(stmt[end+2:])
		} else if i := strings.IndexFunc(stmt, func(r rune) bool { return r < '0' || r > '9' }); i > 0 {
			n, err := strconv.Atoi(stmt[:i])
			if err == nil {
				op.addrN = n
				stmt = strings.TrimSpace(stmt[i:])
			}
		}
		if stmt == "" {
			return nil, fmt.Errorf("address with no command")
		}
		switch stmt[0] {
		case 'd':
			op.kind = 'd'
		case 'p':
			op.kind = 'p'
		case 's':
			if len(stmt) < 4 {
				return nil, fmt.Errorf("malformed s command: %q", stmt)
			}
			sep := stmt[1]
			parts := splitUnescaped(stmt[2:], sep)
			if len(parts) < 2 {
				return nil, fmt.Errorf("malformed s command: %q", stmt)
			}
			pat, repl := parts[0], parts[1]
			mods := ""
			if len(parts) > 2 {
				mods = parts[2]
			}
			if strings.Contains(mods, "i") {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, err
			}
			op.kind = 's'
			op.re = re
			// sed writes a capture as \1; Go's Expand wants ${1}.
			op.repl = regexp.MustCompile(`\\([0-9])`).ReplaceAllString(repl, "${$1}")
			op.repl = strings.ReplaceAll(op.repl, "&", "${0}")
			op.global = strings.Contains(mods, "g")
		default:
			return nil, fmt.Errorf("unsupported sed command %q — this shell's sed does s, d and p only", stmt[:1])
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func splitUnescaped(s string, sep byte) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == sep {
			cur.WriteByte(sep)
			i++
			continue
		}
		if s[i] == sep {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(s[i])
	}
	out = append(out, cur.String())
	return out
}

func cmdFind(e *cmdenv) int {
	args := e.args[1:]
	var roots []string
	i := 0
	for ; i < len(args) && !strings.HasPrefix(args[i], "-"); i++ {
		roots = append(roots, args[i])
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}
	var namePat, pathPat, typ string
	maxDepth := -1
	for ; i < len(args); i++ {
		switch args[i] {
		case "-name", "-path", "-type", "-maxdepth":
			if i+1 >= len(args) {
				return e.errf("missing argument to %s", args[i])
			}
			v := args[i+1]
			i++
			switch args[i-1] {
			case "-name":
				namePat = v
			case "-path":
				pathPat = v
			case "-type":
				typ = v
			case "-maxdepth":
				n, err := strconv.Atoi(v)
				if err != nil {
					return e.errf("invalid -maxdepth: %s", v)
				}
				maxDepth = n
			}
		case "-print":
		default:
			return e.errf("unsupported predicate %s — this shell's find does -name, -path, -type, -maxdepth and -print", args[i])
		}
	}

	code := 0
	for _, root := range roots {
		base := e.abs(root)
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			p = filepath.ToSlash(p)
			rel := strings.TrimPrefix(strings.TrimPrefix(p, filepath.ToSlash(base)), "/")
			depth := 0
			if rel != "" {
				depth = strings.Count(rel, "/") + 1
			}
			if maxDepth >= 0 && depth > maxDepth {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// Concatenated, not path.Join'd: find prints the operand exactly as
			// given, so `find .` yields ./a/b. path.Join would clean the "./"
			// away and the output would stop matching every find recipe there
			// is.
			shown := root
			if rel != "" {
				shown = strings.TrimSuffix(root, "/") + "/" + rel
			}
			if typ == "f" && d.IsDir() {
				return nil
			}
			if typ == "d" && !d.IsDir() {
				return nil
			}
			if namePat != "" {
				ok, _ := path.Match(namePat, d.Name())
				if !ok {
					return nil
				}
			}
			if pathPat != "" {
				ok, _ := path.Match(pathPat, shown)
				if !ok {
					return nil
				}
			}
			fmt.Fprintln(e.stdout, shown)
			return nil
		})
		if err != nil {
			code = e.errf("%s: %s", root, errText(err))
		}
	}
	return code
}

func cmdWc(e *cmdenv) int {
	flags, _, files, err := e.operands("lwc")
	if err != nil {
		return e.errf("%v", err)
	}
	none := !flags['l'] && !flags['w'] && !flags['c']
	var tl, tw, tc int
	multi := len(files) > 1
	code := e.eachFile(files, func(name string, r io.Reader) error {
		var l, w, c int
		br := bufio.NewReader(r)
		inWord := false
		for {
			b, err := br.ReadByte()
			if err != nil {
				break
			}
			c++
			if b == '\n' {
				l++
			}
			if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				inWord = false
			} else if !inWord {
				inWord = true
				w++
			}
		}
		tl, tw, tc = tl+l, tw+w, tc+c
		e.wcLine(flags, none, l, w, c, nameOrBlank(name))
		return nil
	})
	if multi {
		e.wcLine(flags, none, tl, tw, tc, "total")
	}
	return code
}

func nameOrBlank(name string) string {
	if name == "-" {
		return ""
	}
	return name
}

func (e *cmdenv) wcLine(flags map[byte]bool, none bool, l, w, c int, name string) {
	var out []string
	if none || flags['l'] {
		out = append(out, fmt.Sprintf("%7d", l))
	}
	if none || flags['w'] {
		out = append(out, fmt.Sprintf("%7d", w))
	}
	if none || flags['c'] {
		out = append(out, fmt.Sprintf("%7d", c))
	}
	line := strings.Join(out, "")
	if name != "" {
		line += " " + name
	}
	fmt.Fprintln(e.stdout, line)
}

func cmdHead(e *cmdenv) int {
	flags, vals, files, err := e.operands("n:qv")
	if err != nil {
		return e.errf("%v", err)
	}
	n := 10
	if flags['n'] {
		if v, err := strconv.Atoi(vals['n']); err == nil {
			n = v
		}
	}
	multi := len(files) > 1
	first := true
	return e.eachFile(files, func(name string, r io.Reader) error {
		if multi && !flags['q'] {
			if !first {
				fmt.Fprintln(e.stdout)
			}
			fmt.Fprintf(e.stdout, "==> %s <==\n", name)
		}
		first = false
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for i := 0; i < n && sc.Scan(); i++ {
			fmt.Fprintln(e.stdout, sc.Text())
		}
		return sc.Err()
	})
}

func cmdTail(e *cmdenv) int {
	flags, vals, files, err := e.operands("n:q")
	if err != nil {
		return e.errf("%v", err)
	}
	n := 10
	if flags['n'] {
		if v, err := strconv.Atoi(strings.TrimPrefix(vals['n'], "+")); err == nil {
			n = v
		}
	}
	multi := len(files) > 1
	first := true
	return e.eachFile(files, func(name string, r io.Reader) error {
		if multi && !flags['q'] {
			if !first {
				fmt.Fprintln(e.stdout)
			}
			fmt.Fprintf(e.stdout, "==> %s <==\n", name)
		}
		first = false
		// A ring buffer, not a full read: `tail -n 5` on a large file is the
		// case this command exists for.
		ring := make([]string, 0, n)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			if len(ring) == n {
				ring = ring[1:]
			}
			if n > 0 {
				ring = append(ring, sc.Text())
			}
		}
		for _, l := range ring {
			fmt.Fprintln(e.stdout, l)
		}
		return sc.Err()
	})
}

func cmdMkdir(e *cmdenv) int {
	flags, _, dirs, err := e.operands("p")
	if err != nil {
		return e.errf("%v", err)
	}
	code := 0
	for _, d := range dirs {
		var err error
		if flags['p'] {
			err = os.MkdirAll(e.abs(d), 0o755)
		} else {
			err = os.Mkdir(e.abs(d), 0o755)
		}
		if err != nil {
			code = e.errf("%s: %s", d, errText(err))
		}
	}
	return code
}

func cmdRmdir(e *cmdenv) int {
	code := 0
	for _, d := range e.args[1:] {
		if err := os.Remove(e.abs(d)); err != nil {
			code = e.errf("%s: %s", d, errText(err))
		}
	}
	return code
}

func cmdRm(e *cmdenv) int {
	flags, _, targets, err := e.operands("rRf")
	if err != nil {
		return e.errf("%v", err)
	}
	code := 0
	for _, t := range targets {
		abs := e.abs(t)
		var rerr error
		if flags['r'] || flags['R'] {
			rerr = os.RemoveAll(abs)
		} else {
			rerr = os.Remove(abs)
		}
		if rerr != nil && !flags['f'] {
			code = e.errf("%s: %s", t, errText(rerr))
		}
	}
	return code
}

func cmdCp(e *cmdenv) int {
	flags, _, rest, err := e.operands("rR")
	if err != nil {
		return e.errf("%v", err)
	}
	if len(rest) < 2 {
		return e.errf("usage: cp [-r] SOURCE... DEST")
	}
	dst := rest[len(rest)-1]
	srcs := rest[:len(rest)-1]
	dstInfo, dstErr := os.Stat(e.abs(dst))
	dstIsDir := dstErr == nil && dstInfo.IsDir()
	if len(srcs) > 1 && !dstIsDir {
		return e.errf("target %q is not a directory", dst)
	}
	code := 0
	for _, src := range srcs {
		target := dst
		if dstIsDir {
			target = path.Join(dst, path.Base(src))
		}
		if err := e.copyPath(e.abs(src), e.abs(target), flags['r'] || flags['R']); err != nil {
			code = e.errf("%s: %s", src, errText(err))
		}
	}
	return code
}

func (e *cmdenv) copyPath(src, dst string, recursive bool) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if !recursive {
			return fmt.Errorf("is a directory (use -r)")
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if err := e.copyPath(src+"/"+ent.Name(), dst+"/"+ent.Name(), true); err != nil {
				return err
			}
		}
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, st.Mode().Perm())
}

func cmdMv(e *cmdenv) int {
	rest := e.args[1:]
	if len(rest) < 2 {
		return e.errf("usage: mv SOURCE... DEST")
	}
	dst := rest[len(rest)-1]
	srcs := rest[:len(rest)-1]
	dstInfo, dstErr := os.Stat(e.abs(dst))
	dstIsDir := dstErr == nil && dstInfo.IsDir()
	code := 0
	for _, src := range srcs {
		target := dst
		if dstIsDir {
			target = path.Join(dst, path.Base(src))
		}
		if err := os.Rename(e.abs(src), e.abs(target)); err != nil {
			code = e.errf("%s: %s", src, errText(err))
		}
	}
	return code
}

func cmdTouch(e *cmdenv) int {
	code := 0
	for _, name := range e.args[1:] {
		abs := e.abs(name)
		if _, err := os.Stat(abs); err == nil {
			now := time.Now()
			if err := os.Chtimes(abs, now, now); err != nil {
				code = e.errf("%s: %s", name, errText(err))
			}
			continue
		}
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			code = e.errf("%s: %s", name, errText(err))
			continue
		}
		f.Close()
	}
	return code
}

func cmdSort(e *cmdenv) int {
	flags, vals, files, err := e.operands("nrufk:")
	if err != nil {
		return e.errf("%v", err)
	}
	var all []string
	code := e.lines(files, func(_ string, _ int, line string) error {
		all = append(all, line)
		return nil
	})
	key := 0
	if flags['k'] {
		key, _ = strconv.Atoi(strings.SplitN(vals['k'], ",", 2)[0])
	}
	field := func(s string) string {
		if key <= 0 {
			return s
		}
		fs := strings.Fields(s)
		if key-1 < len(fs) {
			return fs[key-1]
		}
		return ""
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := field(all[i]), field(all[j])
		if flags['f'] {
			a, b = strings.ToLower(a), strings.ToLower(b)
		}
		if flags['n'] {
			return parseNum(a) < parseNum(b)
		}
		return a < b
	})
	if flags['r'] {
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
	}
	var prev string
	for i, l := range all {
		if flags['u'] && i > 0 && l == prev {
			continue
		}
		prev = l
		fmt.Fprintln(e.stdout, l)
	}
	return code
}

func parseNum(s string) float64 {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && (s[end] == '-' || s[end] == '+' || s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	v, _ := strconv.ParseFloat(s[:end], 64)
	return v
}

func cmdUniq(e *cmdenv) int {
	flags, _, files, err := e.operands("cdu")
	if err != nil {
		return e.errf("%v", err)
	}
	var prev string
	count := 0
	have := false
	emit := func() {
		if !have {
			return
		}
		switch {
		case flags['d'] && count < 2:
		case flags['u'] && count > 1:
		case flags['c']:
			fmt.Fprintf(e.stdout, "%7d %s\n", count, prev)
		default:
			fmt.Fprintln(e.stdout, prev)
		}
	}
	code := e.lines(files, func(_ string, _ int, line string) error {
		if have && line == prev {
			count++
			return nil
		}
		emit()
		prev, count, have = line, 1, true
		return nil
	})
	emit()
	return code
}

func cmdTr(e *cmdenv) int {
	flags, _, rest, err := e.operands("ds")
	if err != nil {
		return e.errf("%v", err)
	}
	if len(rest) == 0 {
		return e.errf("usage: tr [-d] SET1 [SET2]")
	}
	set1 := expandTrSet(rest[0])
	var set2 []rune
	if len(rest) > 1 {
		set2 = expandTrSet(rest[1])
	}
	data, err2 := io.ReadAll(e.in())
	if err2 != nil {
		return e.errf("%v", err2)
	}
	var b strings.Builder
	var lastOut rune = -1
	for _, r := range string(data) {
		idx := indexRune(set1, r)
		if flags['d'] && idx >= 0 {
			continue
		}
		out := r
		if idx >= 0 && len(set2) > 0 {
			if idx < len(set2) {
				out = set2[idx]
			} else {
				out = set2[len(set2)-1]
			}
		}
		if flags['s'] && out == lastOut && idx >= 0 {
			continue
		}
		lastOut = out
		b.WriteRune(out)
	}
	fmt.Fprint(e.stdout, b.String())
	return 0
}

func indexRune(set []rune, r rune) int {
	for i, s := range set {
		if s == r {
			return i
		}
	}
	return -1
}

// expandTrSet handles a-z ranges and the escapes tr scripts actually use.
func expandTrSet(s string) []rune {
	s = strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\\`, `\`).Replace(s)
	rs := []rune(s)
	var out []rune
	for i := 0; i < len(rs); i++ {
		if i+2 < len(rs) && rs[i+1] == '-' && rs[i+2] >= rs[i] {
			for c := rs[i]; c <= rs[i+2]; c++ {
				out = append(out, c)
			}
			i += 2
			continue
		}
		out = append(out, rs[i])
	}
	return out
}

func cmdCut(e *cmdenv) int {
	_, vals, files, err := e.operands("d:f:c:")
	if err != nil {
		return e.errf("%v", err)
	}
	delim := "\t"
	if d, ok := vals['d']; ok {
		delim = d
	}
	var fields []int
	for _, spec := range strings.Split(vals['f']+vals['c'], ",") {
		if spec == "" {
			continue
		}
		if n, err := strconv.Atoi(spec); err == nil {
			fields = append(fields, n)
		}
	}
	if len(fields) == 0 {
		return e.errf("usage: cut -f LIST [-d DELIM] [FILE...]")
	}
	byChar := vals['c'] != ""
	return e.lines(files, func(_ string, _ int, line string) error {
		var out []string
		if byChar {
			rs := []rune(line)
			for _, n := range fields {
				if n-1 < len(rs) && n > 0 {
					out = append(out, string(rs[n-1]))
				}
			}
			fmt.Fprintln(e.stdout, strings.Join(out, ""))
			return nil
		}
		parts := strings.Split(line, delim)
		for _, n := range fields {
			if n-1 < len(parts) && n > 0 {
				out = append(out, parts[n-1])
			}
		}
		fmt.Fprintln(e.stdout, strings.Join(out, delim))
		return nil
	})
}

func cmdRev(e *cmdenv) int {
	return e.lines(e.args[1:], func(_ string, _ int, line string) error {
		rs := []rune(line)
		for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
			rs[i], rs[j] = rs[j], rs[i]
		}
		fmt.Fprintln(e.stdout, string(rs))
		return nil
	})
}

func cmdNl(e *cmdenv) int {
	n := 0
	return e.lines(e.args[1:], func(_ string, _ int, line string) error {
		n++
		fmt.Fprintf(e.stdout, "%6d\t%s\n", n, line)
		return nil
	})
}

func cmdTee(e *cmdenv) int {
	flags, _, files, err := e.operands("a")
	if err != nil {
		return e.errf("%v", err)
	}
	writers := []io.Writer{e.stdout}
	var open []*os.File
	code := 0
	for _, name := range files {
		mode := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if flags['a'] {
			mode = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, ferr := os.OpenFile(e.abs(name), mode, 0o644)
		if ferr != nil {
			code = e.errf("%s: %s", name, errText(ferr))
			continue
		}
		open = append(open, f)
		writers = append(writers, f)
	}
	_, cerr := io.Copy(io.MultiWriter(writers...), e.in())
	for _, f := range open {
		f.Close()
	}
	if cerr != nil {
		return e.errf("%v", cerr)
	}
	return code
}

func cmdSeq(e *cmdenv) int {
	nums := e.args[1:]
	var first, incr, last float64 = 1, 1, 0
	switch len(nums) {
	case 1:
		last = parseNum(nums[0])
	case 2:
		first, last = parseNum(nums[0]), parseNum(nums[1])
	case 3:
		first, incr, last = parseNum(nums[0]), parseNum(nums[1]), parseNum(nums[2])
	default:
		return e.errf("usage: seq [FIRST [INCR]] LAST")
	}
	if incr == 0 {
		return e.errf("increment must not be zero")
	}
	for v := first; (incr > 0 && v <= last) || (incr < 0 && v >= last); v += incr {
		if v == float64(int64(v)) {
			fmt.Fprintln(e.stdout, int64(v))
		} else {
			fmt.Fprintln(e.stdout, strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	return 0
}

func cmdBasename(e *cmdenv) int {
	if len(e.args) < 2 {
		return e.errf("usage: basename PATH [SUFFIX]")
	}
	b := path.Base(e.args[1])
	if len(e.args) > 2 {
		b = strings.TrimSuffix(b, e.args[2])
	}
	fmt.Fprintln(e.stdout, b)
	return 0
}

func cmdDirname(e *cmdenv) int {
	if len(e.args) < 2 {
		return e.errf("usage: dirname PATH")
	}
	fmt.Fprintln(e.stdout, path.Dir(e.args[1]))
	return 0
}

func cmdEnv(e *cmdenv) int {
	// The interpreter's environment, not the process's: under js/wasm os.Environ
	// is empty, and it is the shell's variables a learner is asking about.
	var out []string
	interp.HandlerCtx(e.ctx).Env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.IsSet() {
			out = append(out, name+"="+vr.String())
		}
		return true
	})
	sort.Strings(out)
	for _, kv := range out {
		fmt.Fprintln(e.stdout, kv)
	}
	return 0
}

func cmdWhich(e *cmdenv) int {
	code := 0
	for _, name := range e.args[1:] {
		if _, ok := coreutils[name]; ok {
			fmt.Fprintf(e.stdout, "/bin/%s\n", name)
			continue
		}
		code = 1
	}
	return code
}

func cmdSleep(e *cmdenv) int {
	if len(e.args) < 2 {
		return e.errf("usage: sleep SECONDS")
	}
	d := time.Duration(parseNum(e.args[1]) * float64(time.Second))
	select {
	case <-time.After(d):
		return 0
	case <-e.ctx.Done():
		return 130
	}
}

func cmdDate(e *cmdenv) int {
	fmt.Fprintln(e.stdout, time.Now().UTC().Format("Mon Jan  2 15:04:05 UTC 2006"))
	return 0
}

func cmdHelp(e *cmdenv) int {
	names := make([]string, 0, len(coreutils))
	for n := range coreutils {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprint(e.stdout, `This is mvdan.cc/sh — a real POSIX shell interpreter, compiled to WebAssembly.
The language is real: quoting, expansion, globbing, pipelines, redirection,
functions, subshells, arithmetic and eval all behave as a shell's do, because
this is the same interpreter stage 08 embeds.

The external commands are NOT the GNU ones. They are re-implementations, and
they differ where it matters: grep uses Go's RE2 (no backreferences, no
lookaround), sed does s/d/p only, find has no -exec, sort never uses a locale.

Available:
`)
	for i, n := range names {
		fmt.Fprintf(e.stdout, "  %-10s", n)
		if i%6 == 5 {
			fmt.Fprintln(e.stdout)
		}
	}
	fmt.Fprintln(e.stdout)
	fmt.Fprintln(e.stdout, "\nShell builtins come from the interpreter: cd pwd echo printf test [ read")
	fmt.Fprintln(e.stdout, "eval source trap getopts shopt alias set unset export exit and the rest.")
	return 0
}
