// Runtime — the one object the interface talks to.
//
// Everything below the surface here is a Web Worker holding WebAssembly
// instances and an in-memory filesystem. Everything above it is the UI, which
// should not need to know that, and this file is the seam. The method set is
// fixed by agreement with the interface and is implemented exactly as agreed
// even where the shape is awkward; the awkward parts are marked and explained
// rather than quietly improved.
//
// What is real today and what is not:
//
//   shell     REAL. mvdan.cc/sh compiled to WebAssembly, over an in-memory
//             filesystem, with the stage 08 policy and audit log. Verified by
//             web/tools/fs-conformance.
//   fs        REAL, and it is the same filesystem the shell sees.
//   build     REAL against precomputed results. Every legal combination of a
//             level's options was compiled by `go build` at build time and the
//             outcome — the artifact or the exact diagnostics — was recorded.
//             Nothing compiles in your browser unless a toolchain has been
//             loaded on purpose, and status() says which it is.
//   check     REAL for recorded combinations; a documented stub off-script.
//   format    STUB. Returns the input unchanged and says so.
//   llm       REAL in replay; see llm.js for live mode and its threat model.
//
// A stub reports itself through status() and through the `origin` on its
// results. It never returns a plausible-looking lie.

import { REQ, EVENT, COMPONENT, RUNAWAY_HINT_MS } from './protocol.js';
import { STATE, ORIGIN, PHASE } from './status.js';

const DEFAULTS = {
  /** Where the runtime's own assets live, relative to the page. */
  base: '/assets/',
  /** Load the shell eagerly. Levels that never open a terminal can pass false. */
  shell: true,
  /** Persist the filesystem across reloads. */
  persist: true,
  /** 'replay' | 'live' | 'off' — see llm.js. */
  llm: 'replay',
};

class RuntimeError extends Error {
  constructor(message, detail) {
    super(message);
    this.name = 'RuntimeError';
    this.detail = detail;
  }
}

class RuntimeImpl {
  constructor() {
    this.opts = { ...DEFAULTS };
    this.worker = null;
    this.nextId = 1;
    this.pending = new Map(); // id -> {resolve, reject}
    this.runs = new Map(); // id -> {onStdout, onStderr, onExit}
    this.listeners = new Map(); // event -> Set<fn>

    this._status = {
      [COMPONENT.COMPILER]: STATE.IDLE,
      [COMPONENT.SHELL]: STATE.IDLE,
      [COMPONENT.LLM]: STATE.IDLE,
    };

    // Mirrors of worker state, kept because two methods in the agreed interface
    // are synchronous and the state they read lives on the other side of a
    // postMessage. See the note on cwd() below.
    this._cwd = '/work';
    this._nodes = [];

    this._initPromise = null;
    this._level = null;
  }

  // ---------------------------------------------------------------------------
  // lifecycle
  // ---------------------------------------------------------------------------

  /**
   * @param {object} opts see DEFAULTS
   * @returns {Promise<void>} resolves when the runtime is usable, even if some
   *   components came back unavailable. A component that cannot load is a
   *   status, not an exception: a learner behind a firewall should get a
   *   reduced site, not a blank one.
   */
  init(opts = {}) {
    if (this._initPromise) return this._initPromise;
    this.opts = { ...DEFAULTS, ...opts };
    this._initPromise = this._boot();
    return this._initPromise;
  }

  async _boot() {
    this._spawnWorker();
    try {
      const res = await this._send(REQ.INIT, {
        base: this.opts.base,
        shell: this.opts.shell,
        persist: this.opts.persist,
        level: this.opts.level || null,
      });
      this._cwd = res.cwd || this._cwd;
      this._nodes = res.nodes || [];
    } catch (err) {
      // The worker itself failed to come up — no module support, a blocked
      // fetch, a corrupt asset. Everything that needs it is unavailable and the
      // page still runs.
      this._setStatus(COMPONENT.SHELL, STATE.UNAVAILABLE, String(err.message));
      this._setStatus(COMPONENT.COMPILER, STATE.UNAVAILABLE, String(err.message));
    }
  }

  _spawnWorker() {
    const url = new URL('./worker.js', import.meta.url);
    this.worker = new Worker(url, { type: 'module' });
    this.worker.onmessage = (ev) => this._onMessage(ev.data);
    this.worker.onerror = (ev) => {
      this._emit(EVENT.FAULT, { reason: ev.message || 'worker error' });
      for (const [, p] of this.pending) p.reject(new RuntimeError(ev.message || 'worker error'));
      this.pending.clear();
    };
  }

  _onMessage(msg) {
    if (msg.id !== undefined && this.pending.has(msg.id)) {
      const { resolve, reject } = this.pending.get(msg.id);
      this.pending.delete(msg.id);
      if (msg.ok) resolve(msg);
      else reject(new RuntimeError(msg.error?.message || 'runtime error', msg.error));
      return;
    }
    switch (msg.type) {
      case EVENT.STATUS:
        this._setStatus(msg.component, msg.state, msg.detail);
        break;
      case EVENT.PROGRESS:
        this._emit('progress', msg);
        break;
      case EVENT.OUTPUT: {
        const run = this.runs.get(msg.id);
        if (!run) break;
        if (msg.stream === 'stderr') run.onStderr?.(msg.text);
        else run.onStdout?.(msg.text);
        break;
      }
      case EVENT.EXIT: {
        const run = this.runs.get(msg.id);
        this.runs.delete(msg.id);
        run?.onExit?.(msg.code);
        break;
      }
      case EVENT.FS_CHANGED:
        if (msg.cwd) this._cwd = msg.cwd;
        if (msg.nodes) this._nodes = msg.nodes;
        this._emit('fs', msg);
        break;
      case EVENT.FAULT:
        this._emit(EVENT.FAULT, msg);
        break;
      default:
        break;
    }
  }

  _send(type, payload = {}, transfer) {
    if (!this.worker) return Promise.reject(new RuntimeError('runtime is not initialised'));
    const id = this.nextId++;
    const p = new Promise((resolve, reject) => this.pending.set(id, { resolve, reject }));
    this.worker.postMessage({ id, type, ...payload }, transfer || []);
    return p;
  }

  _setStatus(component, state, detail) {
    if (!component) return;
    if (this._status[component] === state) return;
    this._status[component] = state;
    // The payload is the whole status map *and* the delta.
    //
    // The interface's mock emits `{compiler, shell, llm}` and its listeners read
    // `payload.compiler`; this file wants to say which component moved and why.
    // Spreading both costs three fields and removes an integration bug that
    // would only show up as a status light that never changes.
    this._emit('status', {
      ...this.status(),
      component,
      state,
      detail,
      status: this.status(),
    });
  }

  _emit(event, payload) {
    const set = this.listeners.get(event);
    if (!set) return;
    for (const fn of set) {
      try {
        fn(payload);
      } catch (err) {
        // A listener that throws must not take the runtime with it.
        console.error('Runtime listener threw:', err);
      }
    }
  }

  /** @returns {{compiler:string, shell:string, llm:string}} */
  status() {
    return { ...this._status };
  }

  /**
   * @param {'status'|'progress'|'fs'|'fault'} event
   * @param {Function} fn
   * @returns {() => void} an unsubscribe function
   */
  on(event, fn) {
    if (!this.listeners.has(event)) this.listeners.set(event, new Set());
    this.listeners.get(event).add(fn);
    return () => this.listeners.get(event)?.delete(fn);
  }

  // ---------------------------------------------------------------------------
  // the Go side
  // ---------------------------------------------------------------------------

  /**
   * Diagnostics for a set of files, without producing an artifact.
   *
   * On the recorded path this is a table lookup: the level's build table holds
   * the diagnostics `go build` produced for every legal combination of options,
   * so what comes back is a real compiler's real message, and `origin` says
   * 'recorded'. Off-script — a learner editing freely — there is nothing to look
   * up, and this returns a single informational diagnostic saying so rather than
   * inventing an opinion. Load the toolchain and it becomes 'live'.
   *
   * @param {Record<string,string>} files
   * @returns {Promise<Array<{file:string,line:number,col:number,severity:string,message:string,origin:string}>>}
   */
  async check(files) {
    const res = await this._send(REQ.CHECK, { files });
    return res.diagnostics;
  }

  /**
   * gofmt. Currently a documented stub: it returns the input unchanged.
   *
   * Honest because it is cheap to be: `go/format` compiled to wasm is a real
   * option (it needs go/parser and go/printer, not the compiler) and is
   * budgeted in ARCHITECTURE.md, but it is not built yet. Returning
   * pretty-printed text produced by a JavaScript approximation of gofmt would
   * teach a formatting style that gofmt does not actually produce.
   *
   * @param {Record<string,string>} files
   * @returns {Promise<{files: Record<string,string>, changed: boolean, stub: boolean}>}
   */
  async format(files) {
    const res = await this._send(REQ.FORMAT, { files });
    return { files: res.files, changed: res.changed, stub: res.stub };
  }

  /**
   * Produce something runnable from a set of files.
   *
   * @param {Record<string,string>} files
   * @returns {Promise<{ok:boolean, diagnostics:Array, artifactId:?string, origin:string}>}
   */
  async build(files) {
    const res = await this._send(REQ.BUILD, { files, level: this._level });
    return {
      ok: res.ok,
      diagnostics: res.diagnostics,
      artifactId: res.artifactId,
      origin: res.origin,
    };
  }

  /**
   * Run a built artifact.
   *
   * Returns synchronously with a handle, because the interface says so and
   * because a UI needs the stop button before the program has started, not
   * after.
   *
   * `stop()` is not polite. A Go program in a tight loop never yields to the
   * JavaScript event loop and cannot be asked to stop, so stopping means
   * terminating the worker and starting a new one. The filesystem is restored
   * from its last snapshot, which is taken after each completed command — so
   * work done by the killed program is lost, exactly as it is when stage 01
   * kills a process tree on timeout.
   *
   * @param {string} artifactId from build()
   * @param {{argv?:string[], stdin?:string, onStdout?:Function, onStderr?:Function, onExit?:Function}} io
   * @returns {{stop: () => void, id: number}}
   */
  run(artifactId, io = {}) {
    const id = this.nextId++;
    this.runs.set(id, io);

    const timer = setTimeout(() => {
      this._emit('progress', {
        id,
        phase: PHASE.RUNNING,
        runaway: true,
        detail: 'still running',
      });
    }, RUNAWAY_HINT_MS);

    const done = () => clearTimeout(timer);
    const wrapped = this.runs.get(id);
    const origExit = wrapped.onExit;
    wrapped.onExit = (code) => {
      done();
      origExit?.(code);
    };

    this.worker?.postMessage({
      id,
      type: REQ.RUN,
      artifactId,
      argv: io.argv || [],
      stdin: io.stdin || '',
    });

    return {
      id,
      stop: () => {
        done();
        this._hardStop(id);
      },
    };
  }

  /**
   * Kill whatever is running by replacing the worker.
   *
   * The blunt instrument, and the only one that always works. Everything the
   * worker held is gone: the wasm instances, and the filesystem back to its
   * last snapshot. The runtime reboots itself and reports through status() the
   * whole way, so a UI can show "restarting" rather than freezing.
   */
  async _hardStop(runId) {
    const run = this.runs.get(runId);
    this.runs.delete(runId);
    for (const [, p] of this.pending) p.reject(new RuntimeError('runtime was restarted to stop a run'));
    this.pending.clear();

    this.worker?.terminate();
    this._setStatus(COMPONENT.SHELL, STATE.LOADING, 'restarting');
    this._setStatus(COMPONENT.COMPILER, STATE.LOADING, 'restarting');
    run?.onExit?.(-1);

    this._spawnWorker();
    this._initPromise = this._boot();
    await this._initPromise;
  }

  // ---------------------------------------------------------------------------
  // shell
  // ---------------------------------------------------------------------------

  get shell() {
    if (!this._shell) {
      const rt = this;
      this._shell = {
        /**
         * Run one command line to completion.
         *
         * @param {string} line
         * @param {{onStdout?:Function,onStderr?:Function}} io
         * @returns {Promise<{code:number}>}
         */
        async exec(line, io = {}) {
          const id = rt.nextId++;
          rt.runs.set(id, io);
          const p = new Promise((resolve, reject) => rt.pending.set(id, { resolve, reject }));
          rt.worker?.postMessage({ id, type: REQ.SHELL_EXEC, line });
          try {
            const res = await p;
            return { code: res.code };
          } finally {
            rt.runs.delete(id);
          }
        },

        /**
         * The shell's working directory.
         *
         * Synchronous, as agreed, and therefore a mirror: the real value lives
         * in the worker and arrives on every fs.changed event. It is correct
         * between commands, which is when a prompt is drawn, and it can be one
         * command stale if read while a command is mid-flight. If that ever
         * matters, the fix is an async cwd() and a changed signature — noted
         * rather than done, because the interface is being coded against now.
         */
        cwd() {
          return rt._cwd;
        },

        /** Ask the running command to stop. Cooperative; see run().stop for the blunt one. */
        interrupt() {
          rt.worker?.postMessage({ id: rt.nextId++, type: REQ.SHELL_INTERRUPT });
        },

        /** Everything the sandbox saw: {execs, opens, blocked}. Stage 08's audit log. */
        async audit() {
          const res = await rt._send(REQ.SHELL_AUDIT);
          return res.audit;
        },

        /**
         * Switch which of stage 08's three inspectors is in force.
         * @param {{level:'off'|'string'|'ast'|'argv', enforce:boolean, secret?:string}} p
         */
        async setPolicy(p) {
          await rt._send(REQ.SHELL_POLICY, { policy: p });
        },

        /** The external commands this shell has. They are not GNU's; see `help`. */
        async commands() {
          const res = await rt._send(REQ.SHELL_AUDIT, { commands: true });
          return res.commands || [];
        },
      };
    }
    return this._shell;
  }

  // ---------------------------------------------------------------------------
  // filesystem
  // ---------------------------------------------------------------------------

  get fs() {
    if (!this._fs) {
      const rt = this;
      this._fs = {
        /**
         * Every path in the tree.
         *
         * Synchronous, as agreed, so this is the mirror the worker pushes after
         * every mutation. It is a list of {path, kind, size} and it is small —
         * a level's tree is tens of entries, not thousands — which is what
         * makes mirroring it cheap enough to be the right answer.
         *
         * @returns {Array<{path:string, kind:'file'|'dir', size?:number}>}
         */
        list() {
          return rt._nodes;
        },

        /** @returns {Promise<string>} the file's contents as UTF-8 text */
        async read(path) {
          const res = await rt._send(REQ.FS_READ, { path });
          return res.text;
        },

        /** @returns {Promise<void>} */
        async write(path, text) {
          await rt._send(REQ.FS_WRITE, { path, text });
        },

        /** Not in the agreed interface; here because a level's reset needs it. */
        async remove(path) {
          await rt._send(REQ.FS_REMOVE, { path });
        },

        /** Replace the tree with a level's starting files. */
        async mount(files, cwd) {
          await rt._send(REQ.FS_MOUNT, { files, cwd });
        },
      };
    }
    return this._fs;
  }

  /** Tell the runtime which level's build table and traces to use. */
  async setLevel(level) {
    this._level = level;
    await this._send(REQ.INIT, { level, reinit: true });
  }
}

export const Runtime = new RuntimeImpl();
export { STATE, ORIGIN, PHASE };
export default Runtime;
