// The runtime worker: where the WebAssembly and the filesystem actually live.
//
// Everything here is off the main thread for one measured reason. A 2000-
// iteration shell loop is about 200ms of solid compute, and a Go program in a
// tight loop never returns to the JavaScript event loop at all. On the main
// thread the first is dropped frames and the second is a tab the learner has to
// close. Here, the page stays responsive and a runaway can be killed by
// terminating this worker — which is the only stop that always works, and which
// costs whatever the filesystem has not snapshotted.
//
// One Go program runs at a time. Not a limitation worth removing: Go's js/wasm
// instances share this worker's globals — `fs`, `process`, `path` — and two
// instances writing to fd 1 at once would interleave into one stream with no
// way to tell them apart. Requests queue.

import { MemFS } from './memfs.js';
import { REQ, EVENT, COMPONENT, OUTPUT_FLUSH_MS } from './protocol.js';
import { STATE, ORIGIN, PHASE } from './status.js';
import { BuildOracle } from './compiler.js';
import { SnapshotStore } from './persist.js';
import {
  loadWasmExec,
  assertVersion,
  installGlobals,
  compileModule,
  runModule,
  startResident,
} from './gohost.js';

let base = '/assets/';
let fs = null;
let oracle = null;
let store = null;
let shell = null; // the __goshell object the resident program installs
let level = null;
let persist = true;

// A queue of one: everything that touches a Go instance runs in order.
let chain = Promise.resolve();
const serial = (fn) => {
  const next = chain.then(fn, fn);
  chain = next.then(
    () => {},
    () => {},
  );
  return next;
};

// ---------------------------------------------------------------------------
// output batching
// ---------------------------------------------------------------------------

// A shell printing 5000 lines would otherwise be 5000 postMessage calls. The
// window is one frame; the cost is that output arrives in chunks of up to that
// long, which is below where a terminal starts to look stalled.
const pendingOut = new Map(); // `${id}:${stream}` -> text
let flushTimer = null;

function emitOutput(id, stream, text) {
  if (!text) return;
  const key = `${id}:${stream}`;
  pendingOut.set(key, (pendingOut.get(key) || '') + text);
  if (flushTimer === null) flushTimer = setTimeout(flushOutput, OUTPUT_FLUSH_MS);
}

function flushOutput() {
  flushTimer = null;
  for (const [key, text] of pendingOut) {
    const idx = key.lastIndexOf(':');
    postMessage({
      type: EVENT.OUTPUT,
      id: Number(key.slice(0, idx)),
      stream: key.slice(idx + 1),
      text,
    });
  }
  pendingOut.clear();
}

function status(component, state, detail) {
  postMessage({ type: EVENT.STATUS, component, state, detail });
}

function progress(id, phase, extra = {}) {
  postMessage({ type: EVENT.PROGRESS, id, phase, ...extra });
}

/**
 * Startup progress, in the shape the interface's loading indicator expects:
 * `{phase, done, total}` where phase names the component being loaded.
 *
 * Deliberately a different vocabulary from status.js's PHASE, and the
 * difference is the point. These are load phases — "the shell is coming down
 * the wire". Those are build phases — "a compiler is running". Sharing one
 * enum would be how `compiling` ends up on a progress bar that is fetching a
 * file, which is the exact lie §3.6 of ARCHITECTURE.md exists to prevent.
 */
function bootProgress(phase, done, total) {
  postMessage({ type: EVENT.PROGRESS, id: 0, phase, done, total, boot: true });
}

function announceFS() {
  postMessage({
    type: EVENT.FS_CHANGED,
    cwd: shell ? shell.cwd() : fs.cwd(),
    nodes: fs.list('/'),
  });
}

// ---------------------------------------------------------------------------
// boot
// ---------------------------------------------------------------------------

async function init(msg) {
  if (msg.base) base = new URL(msg.base, self.location.href).href;
  if (msg.level !== undefined) level = msg.level;
  if (msg.persist !== undefined) persist = msg.persist;

  if (!fs) {
    fs = new MemFS({ cwd: '/work' });
    installGlobals(fs);
    oracle = new BuildOracle(base, (phase, extra) => progress(0, phase, extra));
    store = new SnapshotStore();
  }

  // A saved tree comes back before anything else touches it, so a learner's
  // files survive a reload. It is restored only when the level matches: files
  // from chapter 7 in chapter 2's tree would be worse than starting clean.
  if (persist) {
    const snap = await store.load(level).catch(() => null);
    if (snap) {
      fs = MemFS.fromSnapshot(snap);
      installGlobals(fs);
    }
  }

  bootProgress('shell', 0, 2);
  if (msg.shell !== false && !shell) {
    await bootShell();
  }
  bootProgress('compiler', 1, 2);
  // The compiler is a build table, not a program: it is ready the moment a
  // level names one, and `unavailable` is reserved for the toolchain that is
  // not loaded rather than being used for this.
  status(COMPONENT.COMPILER, level ? STATE.READY : STATE.IDLE);
  status(COMPONENT.LLM, STATE.READY, 'replay');
  bootProgress('done', 2, 2);

  announceFS();
  return { cwd: shell ? shell.cwd() : fs.cwd(), nodes: fs.list('/') };
}

async function bootShell() {
  status(COMPONENT.SHELL, STATE.LOADING);
  try {
    await loadWasmExec(base);
    const manifest = await fetch(new URL('wasm/manifest.json', base))
      .then((r) => (r.ok ? r.json() : null))
      .catch(() => null);
    assertVersion(manifest?.go);

    const mod = await compileModule(new URL(manifest?.shell || 'wasm/shell.wasm', base));
    await startResident(mod, '__goshellReady');
    shell = globalThis.__goshell;
    if (!shell) throw new Error('the shell module did not install __goshell');
    status(COMPONENT.SHELL, STATE.READY);
  } catch (err) {
    // A shell that will not load is a reduced site, not a broken one: the
    // reading, the quizzes, the diffs and the replayed sessions all still work.
    shell = null;
    status(COMPONENT.SHELL, STATE.UNAVAILABLE, String(err.message || err));
  }
}

// ---------------------------------------------------------------------------
// requests
// ---------------------------------------------------------------------------

const handlers = {
  async [REQ.INIT](msg) {
    return init(msg);
  },

  async [REQ.BUILD](msg) {
    if (!msg.level && !level) throw new Error('build needs a level');
    const sel = msg.files?.__selection || msg.selection;
    if (!sel) {
      // Free editing. Nothing was precomputed for arbitrary text, and this is
      // where the toolchain would go if one were loaded.
      return {
        ok: false,
        origin: ORIGIN.RECORDED,
        artifactId: null,
        diagnostics: [
          {
            file: '',
            line: 0,
            col: 0,
            severity: 'info',
            origin: ORIGIN.RECORDED,
            message:
              'Free editing needs a Go toolchain, which is not loaded. Choose from the ' +
              'offered options to run a program that was compiled when this level was built.',
          },
        ],
      };
    }
    const r = await oracle.resolve(msg.level || level, sel);
    return { ok: r.ok, diagnostics: r.diagnostics, artifactId: r.artifactId, origin: r.origin };
  },

  async [REQ.CHECK](msg) {
    const sel = msg.files?.__selection || msg.selection;
    if (!sel) return { diagnostics: [] };
    return { diagnostics: await oracle.check(msg.level || level, sel) };
  },

  async [REQ.FORMAT](msg) {
    // Documented stub. See api.js: returning JavaScript's idea of gofmt would
    // teach a layout gofmt does not produce.
    return { files: msg.files, changed: false, stub: true };
  },

  async [REQ.SHELL_EXEC](msg) {
    if (!shell) throw new Error('the shell is not available in this browser session');
    return serial(
      () =>
        new Promise((resolve) => {
          shell.exec(
            msg.line,
            (s) => emitOutput(msg.id, 'stdout', s),
            (s) => emitOutput(msg.id, 'stderr', s),
            (code) => {
              flushOutput();
              announceFS();
              snapshotSoon();
              resolve({ code });
            },
          );
        }),
    );
  },

  async [REQ.SHELL_INTERRUPT]() {
    shell?.interrupt();
    return {};
  },

  async [REQ.SHELL_AUDIT](msg) {
    return {
      audit: shell ? shell.audit() : { execs: [], opens: [], blocked: [] },
      commands: msg.commands && shell ? shell.commands() : undefined,
    };
  },

  async [REQ.SHELL_POLICY](msg) {
    shell?.setPolicy(msg.policy);
    return {};
  },

  async [REQ.SHELL_RESET](msg) {
    shell?.reset(msg.cwd || '/work');
    announceFS();
    return {};
  },

  async [REQ.FS_READ](msg) {
    return { text: fs.readFile(msg.path) };
  },

  async [REQ.FS_WRITE](msg) {
    fs.writeFile(msg.path, msg.text);
    announceFS();
    snapshotSoon();
    return {};
  },

  async [REQ.FS_LIST](msg) {
    return { nodes: fs.list(msg.from || '/') };
  },

  async [REQ.FS_REMOVE](msg) {
    fs.remove(msg.path);
    announceFS();
    snapshotSoon();
    return {};
  },

  async [REQ.FS_MOUNT](msg) {
    for (const [p, text] of Object.entries(msg.files || {})) fs.writeFile(p, text);
    if (msg.cwd) {
      fs.mkdirp(msg.cwd);
      fs.chdir(msg.cwd);
      shell?.reset(msg.cwd);
    }
    announceFS();
    snapshotSoon();
    return {};
  },

  async [REQ.FS_SNAPSHOT]() {
    return { snapshot: fs.snapshot() };
  },

  async [REQ.FS_RESTORE](msg) {
    fs = MemFS.fromSnapshot(msg.snapshot);
    installGlobals(fs);
    announceFS();
    return {};
  },
};

// RUN has no reply: it streams and ends with an EXIT event, because the page
// wants a stop handle before the program starts rather than after.
async function handleRun(msg) {
  return serial(async () => {
    try {
      progress(msg.id, PHASE.FETCHING, { origin: ORIGIN.RECORDED });
      const url = oracle.artifactURL(msg.level || level, msg.artifactId);
      const mod = await compileModule(url);
      progress(msg.id, PHASE.STARTING, { origin: ORIGIN.RECORDED });
      progress(msg.id, PHASE.RUNNING, { origin: ORIGIN.RECORDED });
      const code = await runModule(mod, {
        argv: msg.argv,
        stdin: msg.stdin,
        onStdout: (t) => emitOutput(msg.id, 'stdout', t),
        onStderr: (t) => emitOutput(msg.id, 'stderr', t),
      });
      flushOutput();
      announceFS();
      snapshotSoon();
      postMessage({ type: EVENT.EXIT, id: msg.id, code });
    } catch (err) {
      emitOutput(msg.id, 'stderr', `\n[runtime] ${err.message || err}\n`);
      flushOutput();
      postMessage({ type: EVENT.EXIT, id: msg.id, code: 70 });
    }
  });
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

let snapshotTimer = null;
let lastRevision = -1;

/**
 * Snapshot after things settle, not on every write.
 *
 * A shell loop appending 2000 lines touches the filesystem 2000 times. Writing
 * the whole tree to IndexedDB each time would dominate the runtime and would
 * still only ever be read once, at the next page load. Debounced, and skipped
 * entirely when nothing changed since the last one.
 */
function snapshotSoon() {
  if (!persist || !store) return;
  clearTimeout(snapshotTimer);
  snapshotTimer = setTimeout(() => {
    if (fs.revision === lastRevision) return;
    lastRevision = fs.revision;
    store.save(level, fs.snapshot()).catch((err) => {
      // Private browsing, a full quota, a browser with IndexedDB disabled. The
      // session still works; only its survival across a reload is lost.
      postMessage({
        type: EVENT.FAULT,
        reason: `files will not persist across a reload: ${err.message || err}`,
        fatal: false,
      });
      persist = false;
    });
  }, 400);
}

// ---------------------------------------------------------------------------

self.onmessage = async (ev) => {
  const msg = ev.data;
  if (msg.type === REQ.RUN) return handleRun(msg);

  const fn = handlers[msg.type];
  if (!fn) {
    postMessage({ id: msg.id, ok: false, error: { message: `unknown request ${msg.type}` } });
    return;
  }
  try {
    const result = await fn(msg);
    postMessage({ id: msg.id, ok: true, ...result });
  } catch (err) {
    postMessage({
      id: msg.id,
      ok: false,
      error: { message: err && err.message ? err.message : String(err), code: err?.code },
    });
  }
};
