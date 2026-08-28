#!/usr/bin/env python3
"""Teach mvdan.cc/sh how to make a pipe in a browser.

Go's js/wasm port has no os.Pipe -- `syscall.Pipe` returns ENOSYS -- and unlike
the filesystem there is no JavaScript seam to shim it through, because pipes are
not routed through a global the host can replace. So `ls | wc -l` in a browser
build of the interpreter fails with

    pipe: not implemented on js

and a shell without pipelines cannot teach anything about an agent whose entire
tool is bash. This was found by running the interpreter, not by reading about
it: web/tools/fs-conformance/run.mjs failed on the first `|` in its suite.

The interpreter needs a pipe in exactly four places, all internal:

    interp/api.go      stdinFile, when stdin is not already an *os.File
    interp/runner.go   the `|` and `|&` operators
    interp/runner.go   heredocs
    interp/runner.go   here-strings (<<<)

Every one wants "a reader and a writer joined end to end", and io.Pipe is that,
in pure Go, on every platform. os.Pipe is there because a real pipeline may hand
the read end to a child process, which needs a file descriptor. A browser has no
child processes, so nothing needs one.

Checked before writing this: mvdan.cc/sh v3.13.1, the newest release, has the
same four call sites unchanged, so there is no version to upgrade to. It would
also raise the module's go directive to 1.25.0, which the repository avoids on
purpose.

Why the vendor directory rather than -overlay: the go command refuses an overlay
whose key lives under GOMODCACHE ("Files beneath GOMODCACHE must not be
replaced"), which is the whole module cache. A vendor tree is ordinary files, so
`go mod vendor` and then patch is the shape that works.

The substitutions are textual on purpose. Each asserts how many times it must
apply, so an upstream change makes the build fail loudly instead of producing a
half-patched interpreter that compiles and then deadlocks.

Usage:
    cd web/tools/wasmshell
    go mod vendor
    python3 jspipe.py            # patches ./vendor in place; idempotent
"""

import argparse
import pathlib
import sys

MODULE_REL = "vendor/mvdan.cc/sh/v3"

SHIM_NAME = "interp/zz_jspipe.go"
SHIM = '''// Added by web/tools/wasmshell/jspipe.py. Not part of mvdan.cc/sh.

package interp

import "io"

// jsPipe is os.Pipe without the operating system.
//
// io.Pipe is synchronous where os.Pipe has a kernel buffer, so a writer blocks
// until a reader takes the bytes rather than until 64KB have accumulated. Under
// js/wasm that is not a deadlock: goroutines are cooperatively scheduled on one
// thread and a blocked write yields exactly as a blocked read does. It does
// change one observable thing -- a pipeline whose reader never reads now blocks
// at the first byte instead of the 65537th -- and a shell that hangs is a shell
// that hangs either way.
func jsPipe() (io.ReadCloser, io.WriteCloser, error) {
	pr, pw := io.Pipe()
	return pr, pw, nil
}
'''

# (file, old, new, expected count). Anchors carry the line above where the bare
# call would be ambiguous: "pr, pw, err := os.Pipe()" is a substring of its own
# more-indented forms, so an unanchored match counts three sites as one.
SUBSTITUTIONS = [
    (
        "interp/api.go",
        "func stdinFile(r io.Reader) (*os.File, error) {",
        "func stdinFile(r io.Reader) (io.Reader, error) {",
        1,
    ),
    (
        "interp/api.go",
        "\t\tpr, pw, err := os.Pipe()",
        "\t\tpr, pw, err := jsPipe()",
        1,
    ),
    (
        "interp/runner.go",
        "case syntax.Pipe, syntax.PipeAll:\n\t\t\tpr, pw, err := os.Pipe()",
        "case syntax.Pipe, syntax.PipeAll:\n\t\t\tpr, pw, err := jsPipe()",
        1,
    ),
    (
        "interp/runner.go",
        "func (r *Runner) hdocReader(rd *syntax.Redirect) (*os.File, error) {\n\tpr, pw, err := os.Pipe()",
        "func (r *Runner) hdocReader(rd *syntax.Redirect) (io.ReadCloser, error) {\n\tpr, pw, err := jsPipe()",
        1,
    ),
    (
        "interp/runner.go",
        "case syntax.WordHdoc:\n\t\tpr, pw, err := os.Pipe()",
        "case syntax.WordHdoc:\n\t\tpr, pw, err := jsPipe()",
        1,
    ),
    # The Runner's stdin is the read end of whatever pipe it was handed, and it
    # is declared as a file. Nothing in the package dereferences it as one.
    (
        "interp/api.go",
        "\tstdin  *os.File // e.g. the read end of a pipe",
        "\tstdin  io.Reader // e.g. the read end of a pipe",
        1,
    ),
    # io.WriteCloser has no WriteString. io.WriteString does the same job and
    # still takes the fast path when the writer implements it.
    (
        "interp/runner.go",
        "\t\t\tpw.WriteString(hdoc)",
        "\t\t\tio.WriteString(pw, hdoc)",
        1,
    ),
    (
        "interp/runner.go",
        "\t\t\tpw.WriteString(arg)\n\t\t\tpw.WriteString(\"\\n\")",
        "\t\t\tio.WriteString(pw, arg)\n\t\t\tio.WriteString(pw, \"\\n\")",
        1,
    ),
    (
        "interp/api.go",
        "\torigStdin  *os.File",
        "\torigStdin  io.Reader",
        1,
    ),
    # `read` cancels a blocked terminal read by setting a deadline on the file.
    # An io.Pipe has none, so the capability is probed rather than assumed. The
    # cost is real and worth naming: with a piped stdin a `read` already waiting
    # for input is no longer interrupted by the context. In the browser stdin is
    # fed by the host a line at a time and there is nothing to interrupt, but a
    # level that demonstrates `read` blocking should know this is why.
    (
        "interp/builtin.go",
        "\tstopc := make(chan struct{})\n"
        "\tstop := context.AfterFunc(ctx, func() {\n"
        "\t\tr.stdin.SetReadDeadline(time.Now())\n"
        "\t\tclose(stopc)\n"
        "\t})",
        "\tstopc := make(chan struct{})\n"
        "\tdeadliner, _ := r.stdin.(interface{ SetReadDeadline(time.Time) error })\n"
        "\tstop := context.AfterFunc(ctx, func() {\n"
        "\t\tif deadliner != nil {\n"
        "\t\t\tdeadliner.SetReadDeadline(time.Now())\n"
        "\t\t}\n"
        "\t\tclose(stopc)\n"
        "\t})",
        1,
    ),
    (
        "interp/builtin.go",
        "\t\t\t<-stopc\n\t\t\tr.stdin.SetReadDeadline(time.Time{})",
        "\t\t\t<-stopc\n"
        "\t\t\tif deadliner != nil {\n"
        "\t\t\t\tdeadliner.SetReadDeadline(time.Time{})\n"
        "\t\t\t}",
        1,
    ),
]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--root",
        default=str(pathlib.Path(__file__).parent),
        help="the wasmshell module directory that holds vendor/",
    )
    ap.add_argument("--check", action="store_true", help="report status and change nothing")
    args = ap.parse_args()

    mod = pathlib.Path(args.root).resolve() / MODULE_REL
    if not mod.is_dir():
        print(f"jspipe: {mod} not found -- run `go mod vendor` first", file=sys.stderr)
        return 2

    shim = mod / SHIM_NAME
    if shim.exists():
        print("jspipe: already applied")
        return 0
    if args.check:
        print("jspipe: NOT applied")
        return 1

    # Read every file first and apply nothing until all assertions pass, so a
    # failure leaves the vendor tree exactly as `go mod vendor` produced it
    # rather than half-rewritten.
    texts = {}
    problems = []
    for rel, old, new, want in SUBSTITUTIONS:
        text = texts.get(rel)
        if text is None:
            path = mod / rel
            if not path.exists():
                problems.append(f"{rel}: missing from the vendor tree")
                continue
            text = path.read_text(encoding="utf-8")
        got = text.count(old)
        if got != want:
            problems.append(
                f"{rel}: expected {want} occurrence(s) of {old.splitlines()[-1].strip()!r}, "
                f"found {got}. mvdan.cc/sh has changed -- re-derive this patch."
            )
            continue
        texts[rel] = text.replace(old, new)

    if problems:
        for p in problems:
            print("jspipe: " + p, file=sys.stderr)
        return 1

    for rel, text in texts.items():
        (mod / rel).write_text(text, encoding="utf-8")
    shim.write_text(SHIM, encoding="utf-8")
    print(f"jspipe: patched {len(texts)} file(s) and added {SHIM_NAME}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
