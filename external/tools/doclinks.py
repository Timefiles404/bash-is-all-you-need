"""Every relative link and image in the docs has to point at something.

    python external/tools/doclinks.py

Thirteen chapters each carry a breadcrumb naming its neighbours, and every
chapter links diagrams out of its own images/ directory. Those are relative
paths written by hand, six directory levels deep in places, and a wrong one
looks exactly like a right one until somebody clicks it.

Checks every .md tracked in the repo: relative links resolve to a file that
exists, and an anchor-only link (#section) names a heading in the same file.
External links (http, https, mailto) are not fetched.
"""

import io
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]

# [text](target) and ![alt](target). Skips reference-style and bare autolinks,
# neither of which this repo uses.
LINK = re.compile(r"!?\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING = re.compile(r"^#{1,6}\s+(.*?)\s*#*$", re.M)
CODE = re.compile(r"```.*?```", re.S)
SKIP = ("http://", "https://", "mailto:", "#!", "data:")


def slug(text):
    """GitHub's heading anchor: lowercase, punctuation dropped, spaces to dashes."""
    s = text.strip().lower()
    s = re.sub(r"[`*_\[\]()]", "", s)
    s = re.sub(r"[^\w一-鿿\s-]", "", s)
    return re.sub(r"\s+", "-", s).strip("-")


# Trees whose .md files are not GitHub-rendered documentation.
#
#   sandbox/ .work/     gitignored scratch. Not shipped, often deliberately stale.
#   external/web/content/
#                       the browser course's reading material, rendered by its
#                       own markdown pass. Its anchors are a private scheme --
#                       #file:exec.go, #line:exec.go:20, #hole:1 -- resolved
#                       against a level's file tree, not against headings.
IGNORE = (".git/", ".work/", "sandbox/", "external/web/tools/", "external/web/content/")


def docs():
    for p in sorted(ROOT.rglob("*.md")):
        rel = p.relative_to(ROOT).as_posix()
        if rel.startswith(IGNORE):
            continue
        yield p


def check(md):
    text = io.open(md, encoding="utf-8").read()
    body = CODE.sub("", text)          # a path inside a fence is an example
    anchors = {slug(h) for h in HEADING.findall(body)}
    bad = []

    for target in LINK.findall(body):
        if target.startswith(SKIP):
            continue
        path, _, frag = target.partition("#")

        if not path:                    # same-file anchor
            if frag and slug(frag) not in anchors:
                bad.append((target, "no such heading in this file"))
            continue

        dest = (md.parent / path).resolve()
        if not dest.exists():
            bad.append((target, "no such file"))
            continue
        if frag and dest.suffix == ".md":
            other = CODE.sub("", io.open(dest, encoding="utf-8").read())
            if slug(frag) not in {slug(h) for h in HEADING.findall(other)}:
                bad.append((target, "file exists, heading does not"))
    return bad


def main():
    total = broken = 0
    for md in docs():
        rel = md.relative_to(ROOT).as_posix()
        bad = check(md)
        n = len([t for t in LINK.findall(CODE.sub("", io.open(md, encoding="utf-8").read()))
                 if not t.startswith(SKIP)])
        total += n
        broken += len(bad)
        if bad:
            print("BAD  %s" % rel)
            for target, why in bad:
                print("       %-56s %s" % (target, why))

    print()
    if broken:
        print("%d of %d relative links point at nothing." % (broken, total))
        return 1
    print("%d relative links, all resolve." % total)
    return 0


if __name__ == "__main__":
    sys.exit(main())
