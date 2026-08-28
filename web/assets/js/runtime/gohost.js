// Hosting a Go program compiled with GOOS=js GOARCH=wasm.
//
// Go's wasm output is not standalone: it needs `wasm_exec.js`, a support file
// the Go distribution ships, which builds the import object, drives the
// scheduler, and marshals values between Go and JavaScript. It is vendored at
// vendor/wasm_exec.js.
//
// **It must match the toolchain that produced the .wasm.** The interface
// between Go's runtime and this file is internal and changes between releases;
// a mismatch does not fail cleanly, it fails as a wrong-looking crash inside
// `runtime.wasmExit` or a hang with no output. vendor/WASM_EXEC_VERSION.txt
// records the version it came from, the build tool writes the same version into
// every manifest it produces, and assertVersion below refuses to run a module
// built by a different one. That check exists because the failure it prevents
// is one nobody would diagnose from the symptom.
//
// The other half of the job is the three globals Go's js port reads —
// `fs`, `process`, `path`. wasm_exec.js installs stubs that answer ENOSYS to
// everything, which is why `os.ReadFile` does not work in a browser. Installing
// a MemFS first is what makes the Go standard library work here at all; see
// memfs.js.

let wasmExecLoaded = false;
let vendoredVersion = null;

/**
 * Load wasm_exec.js once, into this worker's global scope.
 * @param {string} base URL prefix for runtime assets
 */
export async function loadWasmExec(base) {
  if (wasmExecLoaded) return;
  const url = new URL('js/runtime/vendor/wasm_exec.js', base);
  const src = await (await fetch(url)).text();
  // A classic script that assigns globalThis.Go. Evaluated rather than imported
  // because it is not a module and rewriting it would mean maintaining a fork
  // of a file the Go project ships.
  // eslint-disable-next-line no-new-func
  new Function(src)();
  wasmExecLoaded = true;

  try {
    const v = await (await fetch(new URL('js/runtime/vendor/WASM_EXEC_VERSION.txt', base))).text();
    vendoredVersion = v.trim();
  } catch {
    vendoredVersion = null; // absent is not fatal; a mismatch is
  }
}

/**
 * Refuse to run a module built by a toolchain other than the vendored one.
 * @param {string|undefined} builtWith e.g. "go version go1.26.3 windows/amd64"
 */
export function assertVersion(builtWith) {
  if (!builtWith || !vendoredVersion) return;
  const norm = (s) => (s.match(/go1\.\d+(\.\d+)?/) || [''])[0];
  if (norm(builtWith) && norm(vendoredVersion) && norm(builtWith) !== norm(vendoredVersion)) {
    throw new Error(
      `this .wasm was built with ${norm(builtWith)} but vendor/wasm_exec.js came from ` +
        `${norm(vendoredVersion)}. Go's runtime/host interface is internal and version-locked; ` +
        `re-run web/tools/build.py with a matching toolchain.`,
    );
  }
}

/**
 * Install a MemFS as the filesystem every Go instance in this worker will see.
 *
 * One filesystem, shared: the shell writes a file and the level's program reads
 * it, which is the point. Output routing is per-run and is swapped by
 * {@link runModule} rather than fixed here, because two instances writing to fd
 * 1 at once would interleave into one stream with no way to tell them apart.
 * The worker runs one Go program at a time, which is what makes that safe.
 *
 * @param {import('./memfs.js').MemFS} fs
 */
export function installGlobals(fs) {
  globalThis.fs = fs.asNodeFS();
  globalThis.process = {
    ...fs.asNodeProcess(),
    argv: ['js'],
    env: {},
    exit: () => {},
    on: () => {},
  };
  globalThis.path = fs.asNodePath();
  globalThis.__memfs = fs;
}

/**
 * Compile a .wasm once. Cache it: WebAssembly.Module is transferable and
 * re-instantiating a cached module skips the compile entirely, which is the
 * difference between 20ms and sub-millisecond for a program a learner runs
 * repeatedly.
 */
const moduleCache = new Map();

export async function compileModule(url) {
  const key = String(url);
  if (moduleCache.has(key)) return moduleCache.get(key);
  const p = (async () => {
    const res = await fetch(url);
    if (!res.ok) throw new Error(`fetch ${url}: ${res.status} ${res.statusText}`);
    // compileStreaming avoids buffering the whole binary, but needs the server
    // to send application/wasm. A static host that gets the MIME type wrong is
    // common enough that the fallback is not optional.
    if (WebAssembly.compileStreaming && res.headers.get('content-type')?.includes('wasm')) {
      return WebAssembly.compileStreaming(Promise.resolve(res));
    }
    return WebAssembly.compile(await res.arrayBuffer());
  })();
  moduleCache.set(key, p);
  return p;
}

/**
 * Run a Go program to completion, streaming its output.
 *
 * @param {WebAssembly.Module} mod
 * @param {object} io
 * @param {string[]} [io.argv]
 * @param {string}   [io.stdin]
 * @param {(text:string)=>void} io.onStdout
 * @param {(text:string)=>void} io.onStderr
 * @returns {Promise<number>} the exit code
 */
export async function runModule(mod, io) {
  const fs = globalThis.__memfs;
  const dec = new TextDecoder();

  const prevOut = fs.onStdout;
  const prevErr = fs.onStderr;
  const prevIn = fs.readStdin;

  fs.onStdout = (b) => io.onStdout(dec.decode(b, { stream: true }));
  fs.onStderr = (b) => io.onStderr(dec.decode(b, { stream: true }));

  // stdin is handed over once, whole. A level's program that wants a line at a
  // time gets it, because Go's os.Stdin reads through the same fd; what it does
  // not get is a prompt-and-wait loop, which would need the run to suspend and
  // ask the page. Levels that need that use the shell instead.
  const enc = new TextEncoder();
  let stdinBytes = io.stdin ? enc.encode(io.stdin) : null;
  fs.readStdin = () => {
    const chunk = stdinBytes;
    stdinBytes = null;
    return chunk;
  };

  const go = new Go();
  go.argv = ['program', ...(io.argv || [])];
  let code = 0;
  const origExit = go.exit;
  go.exit = (c) => {
    code = c;
    origExit?.call(go, c);
  };
  try {
    const inst = await WebAssembly.instantiate(mod, go.importObject);
    await go.run(inst);
  } catch (err) {
    io.onStderr(`\n[runtime] ${err && err.message ? err.message : String(err)}\n`);
    code = 70;
  } finally {
    fs.onStdout = prevOut;
    fs.onStderr = prevErr;
    fs.readStdin = prevIn;
  }
  return code;
}

/**
 * Start a long-lived Go program that installs an API on globalThis and parks.
 *
 * This is how the shell runs: `main` never returns, so `go.run` never resolves,
 * and the program signals readiness through a callback the host plants first.
 * Waiting on go.run() here would wait for ever.
 *
 * @param {WebAssembly.Module} mod
 * @param {string} readyGlobal e.g. '__goshellReady'
 * @param {number} timeoutMs
 */
export async function startResident(mod, readyGlobal, timeoutMs = 15000) {
  const go = new Go();
  const ready = new Promise((resolve, reject) => {
    globalThis[readyGlobal] = resolve;
    setTimeout(
      () => reject(new Error(`${readyGlobal} never fired within ${timeoutMs}ms`)),
      timeoutMs,
    );
  });
  const inst = await WebAssembly.instantiate(mod, go.importObject);
  go.run(inst).catch(() => {}); // resolves only if the program exits, which is a fault
  await ready;
  return go;
}
