// genlevels — turn the repository's real source into a level a browser can run.
//
// The rule this program exists to enforce:
//
//	A level's correct answer must reproduce the repository's real source, byte
//	for byte, or the level does not build.
//
// A course that transcribes its subject's code into its own files starts
// identical and diverges quietly, and six months later the site is teaching a
// version of `truncate` that no longer exists. Here a level's correct option is
// not a copy of the repository's code — it is a *claim about* the repository's
// code, and this program checks it by extracting the real thing and comparing
// bytes. When stages/ changes, either the claim still holds or this fails and
// somebody has to look.
//
// Usage:
//
//	go run ./web/tools/genlevels -repo . -level web/content/ch02/levels/ch02-l3.json -out web/assets/levels
//	go run ./web/tools/genlevels -repo . -check web/content    # verify every level, build nothing
//
// The -check mode is the one that belongs in CI, on every commit that touches
// stages/. It is fast (parse and compare, no compiler) and it is the whole
// defence against drift.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// the level file
// ---------------------------------------------------------------------------

type Level struct {
	ID      string `json:"id"`
	Chapter string `json:"chapter"`
	Stage   string `json:"stage"`
	Title   string `json:"title"`

	Source struct {
		File    string `json:"file"`
		Extract []struct {
			Symbol string `json:"symbol"`
		} `json:"extract"`
		DocComment string `json:"docComment"` // keep | drop | asProse
	} `json:"source"`

	Program struct {
		Package string            `json:"package"`
		Imports []string          `json:"imports"`
		Harness string            `json:"harness"`
		Argv    []string          `json:"argv"`
		Files   map[string]string `json:"files"`
	} `json:"program"`

	Holes []Hole `json:"holes"`

	// dir is where this level's JSON was read from. Not a field of the format.
	dir string `json:"-"`
}

type Hole struct {
	ID      string   `json:"id"`
	Anchor  string   `json:"anchor"`
	Prompt  string   `json:"prompt"`
	Options []Option `json:"options"`
}

type Option struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Why     string `json:"why"`
	Correct bool   `json:"correct"`
}

// ---------------------------------------------------------------------------
// extraction
// ---------------------------------------------------------------------------

// extracted is the repository's own bytes for the declarations a level names,
// plus the doc comments lifted out when the level asked for prose.
type extracted struct {
	Code  string
	Prose []string
}

// extract pulls named top-level declarations out of a Go file, with their doc
// comments, in source order.
//
// The bytes come from the file, not from go/printer. Printing the AST would
// normalise formatting and comments, and the whole point is a byte-for-byte
// comparison against what a reader sees when they open the file. Formatting is
// part of what the repository is; a level that quietly reformats it is showing
// the learner something else.
func extract(fset *token.FileSet, src []byte, file *ast.File, symbols []string, docs string) (extracted, error) {
	want := map[string]bool{}
	for _, s := range symbols {
		want[s] = true
	}

	type span struct {
		start, end int
		name       string
		doc        *ast.CommentGroup
	}
	var spans []span

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = receiverName(d.Recv.List[0].Type) + "." + name
			}
			if !want[name] {
				continue
			}
			spans = append(spans, span{fset.Position(d.Pos()).Offset, fset.Position(d.End()).Offset, name, d.Doc})
			delete(want, name)
		case *ast.GenDecl:
			// A `var ( … )` block declares several names in one node. Take the
			// whole block when any of its names is wanted: splitting a grouped
			// declaration would produce source the file does not contain.
			var hit string
			for _, spec := range d.Specs {
				for _, n := range specNames(spec) {
					if want[n] {
						hit = n
					}
				}
			}
			if hit == "" {
				continue
			}
			spans = append(spans, span{fset.Position(d.Pos()).Offset, fset.Position(d.End()).Offset, hit, d.Doc})
			for _, spec := range d.Specs {
				for _, n := range specNames(spec) {
					delete(want, n)
				}
			}
		}
	}

	if len(want) > 0 {
		var missing []string
		for n := range want {
			missing = append(missing, n)
		}
		sort.Strings(missing)
		return extracted{}, fmt.Errorf("not found in the file: %s", strings.Join(missing, ", "))
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	var out extracted
	var b strings.Builder
	for i, s := range spans {
		start := s.start
		if s.doc != nil {
			docStart := fset.Position(s.doc.Pos()).Offset
			switch docs {
			case "asProse":
				out.Prose = append(out.Prose, undoc(string(src[docStart:fset.Position(s.doc.End()).Offset])))
			case "drop":
			default: // "keep"
				start = docStart
			}
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.Write(src[start:s.end])
	}
	out.Code = b.String()
	return out, nil
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverName(t.X)
	}
	return ""
}

func specNames(spec ast.Spec) []string {
	switch s := spec.(type) {
	case *ast.ValueSpec:
		var out []string
		for _, n := range s.Names {
			out = append(out, n.Name)
		}
		return out
	case *ast.TypeSpec:
		return []string{s.Name.Name}
	}
	return nil
}

// undoc strips the leading "// " from each line of a doc comment so it can be
// rendered as prose rather than as code.
func undoc(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimPrefix(strings.TrimSpace(l), "//")
		lines = append(lines, strings.TrimPrefix(l, " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------------------
// the checks
// ---------------------------------------------------------------------------

// harnessPath resolves the harness beside the level file that names it, not
// against a fixed content root. A level is self-contained that way, which is
// what lets the tool's own testdata exercise the same code path the real
// content does.
func harnessPath(lv *Level) string { return filepath.Join(lv.dir, lv.Program.Harness) }

type problem struct {
	Level string
	What  string
}

// verify runs every check that does not need a compiler: the ones that catch
// drift, and the ones that catch a level that is internally inconsistent.
func verify(repo string, lv *Level) ([]problem, extracted) {
	var probs []problem
	bad := func(f string, a ...any) { probs = append(probs, problem{lv.ID, fmt.Sprintf(f, a...)}) }

	path := filepath.Join(repo, filepath.FromSlash(lv.Source.File))
	src, err := os.ReadFile(path)
	if err != nil {
		bad("source: %v", err)
		return probs, extracted{}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		bad("source: %v", err)
		return probs, extracted{}
	}

	var symbols []string
	for _, e := range lv.Source.Extract {
		symbols = append(symbols, e.Symbol)
	}
	ex, err := extract(fset, src, f, symbols, lv.Source.DocComment)
	if err != nil {
		bad("extract from %s: %v", lv.Source.File, err)
		return probs, ex
	}

	// 1. Every anchor must occur exactly once. Zero means the repository moved;
	//    more than one means the anchor is not specific enough to substitute
	//    into safely, which would silently corrupt the assembled program.
	seen := map[string]bool{}
	for _, h := range lv.Holes {
		if h.ID == "" {
			bad("a hole has no id")
		}
		if seen[h.ID] {
			bad("duplicate hole id %q", h.ID)
		}
		seen[h.ID] = true

		n := strings.Count(ex.Code, h.Anchor)
		switch {
		case h.Anchor == "":
			bad("hole %q has an empty anchor", h.ID)
		case n == 0:
			bad("hole %q: anchor no longer occurs in %s — the repository has changed:\n      %s",
				h.ID, lv.Source.File, oneline(h.Anchor))
		case n > 1:
			bad("hole %q: anchor occurs %d times in %s; make it more specific",
				h.ID, n, lv.Source.File)
		}

		// 2. Exactly one correct option, and every option explains itself.
		correct := 0
		optIDs := map[string]bool{}
		for _, o := range h.Options {
			if o.Correct {
				correct++
			}
			if strings.TrimSpace(o.Why) == "" {
				bad("hole %q option %q has no `why` — an option without a reason is a guess with a label on it", h.ID, o.ID)
			}
			if optIDs[o.ID] {
				bad("hole %q: duplicate option id %q", h.ID, o.ID)
			}
			optIDs[o.ID] = true
		}
		if correct != 1 {
			bad("hole %q has %d correct options; it must have exactly 1", h.ID, correct)
		}
	}

	// 3. The drift check. Substituting every correct option must reproduce the
	//    repository's bytes. It will, unless an option's text was edited without
	//    re-checking — which is the mistake this exists to catch.
	assembled := ex.Code
	for _, h := range lv.Holes {
		for _, o := range h.Options {
			if !o.Correct {
				continue
			}
			assembled = strings.Replace(assembled, h.Anchor, o.Text, 1)
		}
	}
	// Substituting each correct option for its own anchor is the identity when
	// the level is honest, so any difference is a level that is teaching
	// something the repository does not contain.
	if assembled != ex.Code {
		bad("drift: the correct options do not reproduce %s.\n%s", lv.Source.File, firstDiff(ex.Code, assembled))
	}

	// 4. The harness must exist. A level whose program cannot be assembled is a
	//    level nobody can run.
	if lv.Program.Harness != "" {
		if _, err := os.Stat(harnessPath(lv)); err != nil {
			bad("harness: %v", err)
		}
	}

	// 5. The stage must exist, because the trace binding and the diff both
	//    depend on it.
	if lv.Stage != "" {
		if _, err := os.Stat(filepath.Join(repo, "stages", lv.Stage)); err != nil {
			bad("stage %q: %v", lv.Stage, err)
		}
	}

	return probs, ex
}

func oneline(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\t", "\\t")
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}

// firstDiff reports where two texts stop agreeing, in the form that makes a
// whitespace difference visible — which is what this failure usually is.
func firstDiff(want, got string) string {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	i := 0
	for i < n && want[i] == got[i] {
		i++
	}
	line := 1 + strings.Count(want[:i], "\n")
	ctx := func(s string) string {
		end := i + 60
		if end > len(s) {
			end = len(s)
		}
		start := i - 20
		if start < 0 {
			start = 0
		}
		return oneline(s[start:end])
	}
	return fmt.Sprintf("      first difference at byte %d (line %d)\n      repo:  %s\n      level: %s",
		i, line, ctx(want), ctx(got))
}

// ---------------------------------------------------------------------------
// assembly
// ---------------------------------------------------------------------------

// assemble builds the Go source for one selection of options.
//
// The imports are the level's declared list. They are not inferred, and they
// are not trusted either: the build in step 6 type-checks the result, and Go
// rejects an unused import, so a level that declares one it does not need fails
// rather than shipping a program that would not compile.
func assemble(lv *Level, ex extracted, harness string, selection map[string]string) string {
	body := ex.Code
	for _, h := range lv.Holes {
		pick := selection[h.ID]
		for _, o := range h.Options {
			if o.ID == pick {
				body = strings.Replace(body, h.Anchor, o.Text, 1)
			}
		}
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Generated by web/tools/genlevels for level %s. Do not edit.\n", lv.ID)
	fmt.Fprintf(&b, "// The declarations below are %s, with the level's holes filled in.\n\n", lv.Source.File)
	fmt.Fprintf(&b, "package %s\n\n", orDefault(lv.Program.Package, "main"))
	if len(lv.Program.Imports) > 0 {
		b.WriteString("import (\n")
		for _, im := range lv.Program.Imports {
			fmt.Fprintf(&b, "\t%q\n", im)
		}
		b.WriteString(")\n\n")
	}
	b.WriteString(body)
	b.WriteString("\n")
	if harness != "" {
		b.WriteString("\n")
		// The harness carries its own package clause and imports for readability
		// when it is shown to the learner; strip them here.
		b.WriteString(stripHeader(harness))
	}

	// gofmt the result. Stripping the harness's header leaves blank lines where
	// its import block was, and `gofmt -l` over the generated tree has to be
	// empty for the same reason it has to be empty over stages/.
	//
	// A source that will not format is returned unformatted rather than
	// rejected here: the compiler is about to report it with a line number,
	// which is more useful than this function guessing what went wrong.
	if out, err := format.Source(b.Bytes()); err == nil {
		return string(out)
	}
	return b.String()
}

func stripHeader(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	inImports := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "package "):
			continue
		case t == "import (":
			inImports = true
			continue
		case inImports && t == ")":
			inImports = false
			continue
		case inImports:
			continue
		case strings.HasPrefix(t, "import \""):
			continue
		}
		out = append(out, l)
	}
	return strings.TrimLeft(strings.Join(out, "\n"), "\n")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---------------------------------------------------------------------------

func main() {
	repo := flag.String("repo", ".", "path to the bash-is-all-you-need checkout")
	check := flag.String("check", "", "verify every level under this directory and build nothing")
	one := flag.String("level", "", "a single level JSON file")
	out := flag.String("out", "", "where to write generated assets (with -level)")
	emit := flag.Bool("emit", false, "with -level, write the assembled correct program to stdout")
	flag.Parse()

	var files []string
	switch {
	case *check != "":
		err := filepath.WalkDir(underRepo(*repo, *check), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".json" {
				return nil
			}
			if strings.Contains(filepath.ToSlash(p), "/levels/") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			fatal("walk: %v", err)
		}
	case *one != "":
		files = []string{underRepo(*repo, *one)}
	default:
		fatal("give -check DIR or -level FILE")
	}

	if len(files) == 0 {
		fmt.Println("genlevels: no level files found")
		return
	}

	var all []problem
	ok := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			all = append(all, problem{f, err.Error()})
			continue
		}
		var lv Level
		if err := json.Unmarshal(data, &lv); err != nil {
			all = append(all, problem{f, "JSON: " + err.Error()})
			continue
		}
		lv.dir = filepath.Dir(f)
		probs, ex := verify(*repo, &lv)
		all = append(all, probs...)
		if len(probs) == 0 {
			ok++
		}

		if *emit && *one != "" && len(probs) == 0 {
			sel := map[string]string{}
			for _, h := range lv.Holes {
				for _, o := range h.Options {
					if o.Correct {
						sel[h.ID] = o.ID
					}
				}
			}
			harness := ""
			if lv.Program.Harness != "" {
				b, _ := os.ReadFile(harnessPath(&lv))
				harness = string(b)
			}
			fmt.Print(assemble(&lv, ex, harness, sel))
		}
	}

	for _, p := range all {
		fmt.Fprintf(os.Stderr, "  %s: %s\n", p.Level, p.What)
	}
	fmt.Fprintf(os.Stderr, "genlevels: %d/%d level(s) verified\n", ok, len(files))
	if len(all) > 0 {
		os.Exit(1)
	}

	if *out != "" {
		fmt.Fprintln(os.Stderr,
			"genlevels: -out is not implemented yet. Verification, extraction and assembly are;\n"+
				"           compiling the correct and wrong programs and writing build-table.json\n"+
				"           is the remaining work. See web/ARCHITECTURE.md §10.")
		os.Exit(2)
	}
}

// underRepo resolves a path given on the command line. A repo-relative path is
// the normal case; an absolute one is passed through, which is what lets a test
// fixture live outside the checkout without the join turning it into nonsense.
func underRepo(repo, p string) string {
	q := filepath.FromSlash(p)
	if filepath.IsAbs(q) {
		return q
	}
	return filepath.Join(repo, q)
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "genlevels: "+f+"\n", a...)
	os.Exit(2)
}
