#!/usr/bin/env python3
"""Check that every code block in the reading material is really in stages/.

    python3 web/tools/snippets.py           # report and exit non-zero on drift
    python3 web/tools/snippets.py --list    # just list what is quoted, and from where

A reading block declares where it came from:

    ```go 00-loop/code/main.go:256-258

and this resolves that path and range and compares the block byte for byte
against the file. A mismatch is reported with both texts, because the useful
question is never "did it drift" but "which way".

This exists because prose about code decays in one direction only. The code
moves, the prose does not, and nothing fails — five separate false claims have
been found in this repository's own chapters that way, every one of them by a
person reading carefully rather than by a test. `genlevels` already refuses to
build a level whose source has moved. This is the same rule applied to the
material a reader is asked to read *beside* that source, which is the other half
of the same promise.

Line numbers are inclusive and 1-based, matching what an editor shows.
"""

import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[3]
READING = ROOT / "external" / "web" / "content"

# ```go 00-loop/code/main.go:256-258   — the language, then a path, then a range.
# The range is optional: without one, the block must match the whole file, which
# is almost never what you want but is unambiguous when it is.
FENCE = re.compile(
    r"^```(?P<lang>[a-z]*)[ \t]+(?P<path>[\w./-]+\.\w+)(?::(?P<a>\d+)-(?P<b>\d+))?[ \t]*$"
)


def blocks(md: pathlib.Path):
    """Yield (line_no, path, a, b, text) for every attributed fence in one file."""
    lines = md.read_text(encoding="utf-8").split("\n")
    i = 0
    while i < len(lines):
        m = FENCE.match(lines[i])
        if not m:
            i += 1
            continue
        start = i
        i += 1
        body = []
        while i < len(lines) and not lines[i].startswith("```"):
            body.append(lines[i])
            i += 1
        i += 1  # the closing fence
        a = int(m["a"]) if m["a"] else None
        b = int(m["b"]) if m["b"] else None
        yield start + 1, m["path"], a, b, "\n".join(body)


def expected(path: str, a, b) -> tuple[str, str]:
    """The source text a block claims to be, or ('', reason)."""
    f = ROOT / path
    if not f.exists():
        return "", "no such file: %s" % path
    src = f.read_text(encoding="utf-8").split("\n")
    if a is None:
        return "\n".join(src).rstrip("\n"), ""
    if a < 1 or b > len(src) or a > b:
        return "", "range %d-%d is outside %s, which has %d lines" % (a, b, path, len(src))
    return "\n".join(src[a - 1 : b]), ""


def check(verbose: bool) -> int:
    mds = sorted(READING.rglob("*.md"))
    if not mds:
        print("no reading material found under %s" % READING)
        return 0

    total, bad = 0, 0
    for md in mds:
        rel = md.relative_to(ROOT).as_posix()
        for line, path, a, b, got in blocks(md):
            total += 1
            want, why = expected(path, a, b)
            where = "%s:%d" % (rel, line)
            if why:
                print("BAD  %s\n     %s" % (where, why))
                bad += 1
                continue
            if got.rstrip("\n") != want.rstrip("\n"):
                bad += 1
                print("DRIFT %s  claims %s:%s-%s" % (where, path, a, b))
                print("      the file says:")
                for l in want.split("\n"):
                    print("        | %s" % l)
                print("      the reading says:")
                for l in got.split("\n"):
                    print("        | %s" % l)
            elif verbose:
                print("ok   %s  <- %s:%s-%s" % (where, path, a, b))

    if bad:
        print("\n%d of %d quoted blocks no longer match stages/" % (bad, total))
        return 1
    print("%d quoted blocks, all still byte-identical to their source" % total)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true", help="print every block that matched too")
    return check(ap.parse_args().list)


if __name__ == "__main__":
    sys.exit(main())
