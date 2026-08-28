"""Catch the diagram mistakes that valid XML does not.

    python external/tools/svgcheck.py

The diagrams in the chapters are hand-written SVG, which is the right trade --
they are small, they diff, they need no toolchain, and they render on GitHub.
The cost is that a diagram can be perfectly well-formed and still be wrong on
screen: a label running off the right edge, CJK text with no font that can draw
it, an external reference that will not load.

Text width is *estimated*, because measuring it properly means a font engine.
The estimate is deliberately generous, so this catches the label that overflows
by a third of its length and stays quiet about the one that overflows by two
pixels. Every overflow it has reported so far has been real.

Checks, per file:

  - parses as XML
  - has a viewBox, and width/height agree with it
  - text that contains CJK has a CJK-capable font-family somewhere
  - no <script>, no external href -- these render nowhere useful and would not
    survive GitHub's sanitiser anyway
  - every <text> fits inside the viewBox, honouring text-anchor
  - the last <text> baseline is inside the viewBox (a footer added without
    growing the canvas is the most common version of this)

Exit status 1 if anything is reported. Runs in CI.
"""

import glob
import io
import re
import sys
import xml.etree.ElementTree as ET

CJK = re.compile(r"[⺀-鿿豈-﫿＀-￯]")
CJK_FONT = re.compile(r"PingFang|YaHei|Noto|Hiragino|Heiti|Song|sans-serif")
TEXT = re.compile(r"<text\b([^>]*)>(.*?)</text>", re.S)
ATTR = re.compile(r'([\w:-]+)\s*=\s*"([^"]*)"')
TAGS = re.compile(r"<[^>]+>")

# Rough advance width as a multiple of font-size. Generous on purpose: a real
# CJK glyph is 1.0 em, Latin averages nearer 0.5 for lower case prose.
W_CJK = 1.0
W_LATIN = 0.56


def est_width(s, size):
    s = TAGS.sub("", s)
    s = s.replace("&lt;", "<").replace("&gt;", ">").replace("&amp;", "&")
    cjk = len(CJK.findall(s))
    return (cjk * W_CJK + (len(s) - cjk) * W_LATIN) * size


def check(path):
    raw = io.open(path, encoding="utf-8").read()
    out = []

    try:
        ET.fromstring(raw)
    except Exception as e:
        return ["does not parse: %s" % e]

    m = re.search(r'viewBox="0 0 (\d+(?:\.\d+)?) (\d+(?:\.\d+)?)"', raw)
    if not m:
        return ["no viewBox='0 0 W H' on the root element"]
    vw, vh = float(m.group(1)), float(m.group(2))

    for dim, want in (("width", vw), ("height", vh)):
        d = re.search(r'<svg\b[^>]*\b%s="(\d+(?:\.\d+)?)"' % dim, raw)
        if d and abs(float(d.group(1)) - want) > 0.5:
            out.append("%s=%s disagrees with viewBox %s" % (dim, d.group(1), want))

    if CJK.search(raw) and not CJK_FONT.search(raw):
        out.append("CJK text but no CJK-capable font-family")
    if "<script" in raw:
        out.append("<script> -- will not render and will be stripped")
    for h in re.findall(r'(?:xlink:)?href="(https?:[^"]*)"', raw):
        out.append("external reference: %s" % h)

    last_y = 0.0
    for m in TEXT.finditer(raw):
        attrs, body = m.group(1), m.group(2)
        a = dict(ATTR.findall(attrs))
        try:
            x = float(a.get("x", 0))
            y = float(a.get("y", 0))
        except ValueError:
            continue
        # A transform means the coordinates are in some other space; measuring
        # them against the viewBox would be wrong, so skip those.
        if "transform" in a:
            continue
        size = float(a.get("font-size", 12))
        w = est_width(body, size)
        anchor = a.get("text-anchor", "start")
        left = x - w if anchor == "end" else x - w / 2 if anchor == "middle" else x
        right = left + w
        label = TAGS.sub("", body).strip()[:34]
        if right > vw - 4:
            out.append("text overflows right edge by ~%dpx (x=%g, %s): %s"
                       % (right - vw, x, anchor, label))
        if left < -4:
            out.append("text overflows left edge by ~%dpx: %s" % (-left, label))
        last_y = max(last_y, y)

    if last_y > vh - 2:
        out.append("last text baseline y=%g is at or past the bottom edge %g"
                   % (last_y, vh))
    return out


def main():
    files = sorted(glob.glob("[0-9][0-9]-*/doc/images/*.svg"))
    if not files:
        print("no diagrams found -- run this from the repository root")
        return 1
    bad = 0
    for f in files:
        notes = check(f)
        rel = f.replace("\\", "/")
        if notes:
            bad += 1
            print("BAD  %s" % rel)
            for n in notes:
                print("       %s" % n)
        else:
            print("ok   %s" % rel)

    print()
    if bad:
        print("%d of %d diagrams have problems." % (bad, len(files)))
        return 1
    print("%d diagrams, all well-formed and inside their own canvas." % len(files))
    return 0


if __name__ == "__main__":
    sys.exit(main())
