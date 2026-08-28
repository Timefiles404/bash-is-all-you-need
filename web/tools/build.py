#!/usr/bin/env python3
"""Build every asset the site needs, offline, and check the ones it cannot build.

Serving the site needs no build step. Producing its assets does, and this is it.
Nothing here runs in a browser and nothing here needs a network except the one
`go mod vendor`, which is cached after the first run.

    python3 web/tools/build.py            # build everything, then verify
    python3 web/tools/build.py --check    # verify only; no compiler, fast, for CI
    python3 web/tools/build.py --shell    # just the shell

Order matters. The shell is built before the conformance suite runs, because the
suite tests the shell that was just built rather than whatever was there before —
a suite that passes against a stale binary is worse than no suite.
"""

import argparse
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[2]
WEB = ROOT / "web"
ASSETS = WEB / "assets"
WASM_OUT = ASSETS / "wasm"
VENDOR_JS = ASSETS / "js" / "runtime" / "vendor"

# Budgets from web/ARCHITECTURE.md §7. A build that blows one is not a failure —
# it is a number somebody has to look at and either accept or fix — so this
# warns rather than exits. A budget that fails the build silently gets raised
# until it means nothing.
BUDGETS_BROTLI = {
    "shell.wasm": 1_400_000,
}


def run(cmd, cwd=None, env=None, capture=False):
    e = dict(os.environ)
    if env:
        e.update(env)
    r = subprocess.run(cmd, cwd=cwd, env=e, text=True,
                       capture_output=capture, shell=False)
    if r.returncode != 0:
        if capture:
            sys.stderr.write(r.stdout or "")
            sys.stderr.write(r.stderr or "")
        raise SystemExit(f"build: `{' '.join(cmd)}` failed with {r.returncode}")
    return (r.stdout or "") if capture else ""


def go_version() -> str:
    return run(["go", "version"], capture=True).strip()


def goroot() -> pathlib.Path:
    return pathlib.Path(run(["go", "env", "GOROOT"], capture=True).strip())


def short_version(s: str) -> str:
    m = re.search(r"go1\.\d+(\.\d+)?", s)
    return m.group(0) if m else ""


def sizes(path: pathlib.Path) -> dict:
    """Raw, gzip and brotli, so ARCHITECTURE's table can be regenerated rather
    than remembered."""
    import gzip
    data = path.read_bytes()
    out = {"raw": len(data), "gzip": len(gzip.compress(data, 9))}
    try:
        import brotli  # type: ignore
        out["brotli"] = len(brotli.compress(data, quality=11))
    except ImportError:
        # Node has brotli in its standard library and this repo already needs
        # node for the conformance suite, so use it rather than adding a Python
        # dependency the rest of the build does not have.
        try:
            js = (
                "const z=require('zlib'),f=require('fs');"
                "process.stdout.write(String(z.brotliCompressSync("
                "f.readFileSync(process.argv[1]),"
                "{params:{[z.constants.BROTLI_PARAM_QUALITY]:11}}).length))"
            )
            out["brotli"] = int(run(["node", "-e", js, str(path)], capture=True).strip())
        except Exception:
            out["brotli"] = None
    return out


# ---------------------------------------------------------------------------


def sync_wasm_exec():
    """Copy wasm_exec.js from this toolchain and record which one it was.

    Go's runtime/host interface is internal and changes between releases. A
    `.wasm` run against a `wasm_exec.js` from a different Go does not fail
    cleanly — it crashes inside the runtime or hangs with no output — so the
    version is recorded here, written into every manifest, and checked in
    gohost.js before anything runs.
    """
    src = goroot() / "lib" / "wasm" / "wasm_exec.js"
    if not src.exists():  # Go 1.21 and earlier
        src = goroot() / "misc" / "wasm" / "wasm_exec.js"
    VENDOR_JS.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(src, VENDOR_JS / "wasm_exec.js")
    (VENDOR_JS / "WASM_EXEC_VERSION.txt").write_text(go_version() + "\n", encoding="utf-8")
    print(f"  wasm_exec.js  from {src}")


def build_shell() -> pathlib.Path:
    mod = WEB / "tools" / "wasmshell"
    print("* vendoring mvdan.cc/sh")
    run(["go", "mod", "vendor"], cwd=mod, env={"GOFLAGS": "-mod=mod"}, capture=True)

    print("* patching it for js/wasm (io.Pipe for os.Pipe)")
    run([sys.executable, str(mod / "jspipe.py"), "--root", str(mod)])

    print("* building shell.wasm")
    WASM_OUT.mkdir(parents=True, exist_ok=True)
    out = WASM_OUT / "shell.wasm"
    run(
        ["go", "build", "-mod=vendor", "-ldflags=-s -w", "-o", str(out), "."],
        cwd=mod,
        env={"GOOS": "js", "GOARCH": "wasm"},
        capture=True,
    )
    return out


def write_manifest(entries: dict):
    manifest = {
        "go": go_version(),
        "builtAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "shell": "wasm/shell.wasm",
        "assets": entries,
    }
    (WASM_OUT / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")


def check_levels():
    print("* verifying levels against stages/ (the drift check)")
    tool = WEB / "tools" / "genlevels"
    # Content first, then the tool's own fixture, so a repository with no
    # authored levels yet still exercises the check.
    targets = []
    if (WEB / "content").is_dir():
        targets.append("web/content")
    if (tool / "testdata").is_dir():
        targets.append("web/tools/genlevels/testdata")
    for t in targets:
        r = subprocess.run(
            ["go", "run", ".", "-repo", str(ROOT), "-check", t],
            cwd=tool, text=True, capture_output=True,
        )
        sys.stderr.write(r.stderr)
        if r.returncode != 0:
            raise SystemExit(
                "build: a level no longer matches the repository. Either the level's "
                "claim about stages/ is stale, or stages/ changed and the level has to "
                "follow. Both need a person; neither is a build flag."
            )


def check_snippets():
    """The reading material's half of the same promise as check_levels.

    genlevels refuses to build a level whose source moved. This refuses to ship
    reading material that quotes code which moved. Same failure, same direction,
    and the reading is the half a person actually reads beside the code.
    """
    print("* verifying quoted code in the reading material")
    r = subprocess.run(
        [sys.executable, str(WEB / "tools" / "snippets.py")],
        text=True, capture_output=True,
    )
    sys.stdout.write(r.stdout)
    sys.stderr.write(r.stderr)
    if r.returncode != 0:
        raise SystemExit(
            "build: reading material quotes code that is no longer in stages/. "
            "Either the quote is stale or the source moved and the quote has to "
            "follow; the report above says which way."
        )


def check_conformance(wasm: pathlib.Path):
    print("* running the filesystem conformance suite")
    if shutil.which("node") is None:
        print("  node not found — SKIPPED. This is the test that proves the shell works;"
              " a build that skips it has not verified anything.")
        return
    r = subprocess.run(
        ["node", str(WEB / "tools" / "fs-conformance" / "run.mjs"), str(wasm)],
        cwd=ROOT, text=True, capture_output=True,
    )
    sys.stdout.write(r.stdout)
    sys.stderr.write(r.stderr)
    if r.returncode != 0:
        raise SystemExit("build: the shell does not pass its own conformance suite")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true", help="verify only; build nothing")
    ap.add_argument("--shell", action="store_true", help="build only the shell")
    args = ap.parse_args()

    print(f"Go: {go_version()}")

    if args.check:
        check_levels()
        check_snippets()
        wasm = WASM_OUT / "shell.wasm"
        if wasm.exists():
            check_conformance(wasm)
        else:
            print("  shell.wasm not built; skipping the conformance suite")
        return 0

    print("* syncing the Go host support file")
    sync_wasm_exec()

    wasm = build_shell()
    entries = {}
    s = sizes(wasm)
    entries["wasm/shell.wasm"] = s
    print(f"  shell.wasm  raw={s['raw']:,}  gzip={s['gzip']:,}  brotli={s['brotli']:,}"
          if s["brotli"] else f"  shell.wasm  raw={s['raw']:,}  gzip={s['gzip']:,}")

    for name, budget in BUDGETS_BROTLI.items():
        got = entries.get(f"wasm/{name}", {}).get("brotli")
        if got and got > budget:
            print(f"  ! {name} is {got:,} brotli, over the {budget:,} budget in "
                  f"ARCHITECTURE.md §7. Raise the budget on purpose or find the bytes.")

    write_manifest(entries)

    if args.shell:
        return 0

    check_levels()
    check_conformance(wasm)

    print("\nBuilt. Serve web/ with any static file server; there is no build step to run it.")
    print("  python3 -m http.server --directory web 8000")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
