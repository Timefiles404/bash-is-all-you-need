"""Every line of Go quoted in a chapter must really be in that stage's code.

    python external/tools/quotecheck.py            # every chapter
    python external/tools/quotecheck.py 00-loop    # one of them

A chapter that quotes code is making a claim about the repository, and it is the
kind of claim that rots silently: the code gets a new argument, the prose keeps
the old call, and nothing fails. Six wrong claims were found by hand in one
chapter before this existed, which is the whole argument for it.

The rule: inside a ```go fence, every line has to appear verbatim -- ignoring
indentation -- somewhere in <stage>/code/*.go.

Three escapes, because not every fenced block is a quote:

    ```go wrong      code the chapter is showing you NOT to write.
    // ...           an elision. The chapter skipped something here.
    // <Chinese>     a comment written for the chapter rather than quoted.
                     Real code comments are English (see AGENTS.md), so a
                     comment with CJK in it cannot have come from the source.

Exit status is 1 if anything does not match, and the offending lines are
printed. That is also how it runs in CI.
"""

import io
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
FENCE = re.compile(r"```go( +wrong)?\n(.*?)```", re.S)
CJK = re.compile(r"[一-鿿]")


def quoted_lines(md):
    """The lines in md that claim to be code, stripped of indentation."""
    text = io.open(md, encoding="utf-8").read()
    for m in FENCE.finditer(text):
        if m.group(1):  # ```go wrong -- deliberately not the source
            continue
        for line in m.group(2).split("\n"):
            s = line.strip()
            if not s or s.startswith("// ..."):
                continue
            if s.startswith("//") and CJK.search(s):
                continue
            yield s


def source_lines(stage):
    """Every line of Go in a stage, stripped, as a set."""
    out = set()
    for p in sorted((stage / "code").glob("*.go")):
        for line in io.open(p, encoding="utf-8").read().split("\n"):
            out.add(line.strip())
    return out


def check(stage):
    docs = sorted((stage / "doc").glob("*.md"))
    if not docs:
        return 0, 0
    have = source_lines(stage)
    total = bad = 0
    for md in docs:
        rel = md.relative_to(ROOT).as_posix()
        misses = []
        n = 0
        for s in quoted_lines(md):
            n += 1
            if s not in have:
                misses.append(s)
        total += n
        bad += len(misses)
        if misses:
            print("BAD  %s" % rel)
            for s in misses:
                print("       %s" % s)
        else:
            print("ok   %-52s %3d quoted lines" % (rel, n))
    return total, bad


def main(argv):
    if argv:
        stages = [ROOT / a.rstrip("/\\") for a in argv]
    else:
        stages = sorted(p for p in ROOT.glob("[0-9][0-9]-*") if p.is_dir())

    total = bad = 0
    for s in stages:
        t, b = check(s)
        total += t
        bad += b

    print()
    if bad:
        print("%d of %d quoted lines are not in the code they claim to be from."
              % (bad, total))
        print("Either the quote is stale, or the code moved and the quote has to follow.")
        return 1
    print("%d quoted lines, all still byte-identical to their source." % total)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
