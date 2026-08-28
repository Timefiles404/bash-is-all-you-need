// A stand-in for the real runtime, exporting exactly the interface the UI codes
// against. It exists so the front end is demonstrable on its own, and so the
// degraded and pending states can be exercised without a compiler.
//
// It does not compile Go and does not pretend to. What it does is real:
// - `check` finds unfilled blanks, unbalanced brackets and a missing package
//   clause, which is most of what a half-assembled level actually gets wrong;
// - `shell` runs a handful of commands against a real in-memory file system;
// - everything is genuinely asynchronous and paced, so a pane that does not
//   handle "pending" will look broken here rather than in production.
//
// Program output cannot be derived from the source, so it comes from a fixture
// the level supplies. That hook is namespaced `__mock` and nothing in the UI is
// allowed to require it — see `main.js`, which calls it only if present.

const bus = new Map();
const emit = (ev, payload) => {
  for (const fn of bus.get(ev) || []) fn(payload);
};

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let state = { compiler: 'idle', shell: 'idle', llm: 'idle' };
let files = {}; // path -> text
let readonly = new Set();
let cwd = '/sandbox';
let artifacts = new Map();
let fixture = null;
let seq = 0;

function setStatus(patch) {
  state = { ...state, ...patch };
  emit('status', { ...state });
}

export const Runtime = {
  async init(opts = {}) {
    files = { ...(opts.files || {}) };
    readonly = new Set(opts.readonly || []);
    if (opts.cwd) cwd = opts.cwd;

    setStatus({ compiler: 'loading', shell: 'loading', llm: 'unavailable' });
    emit('progress', { phase: 'shell', done: 0, total: 2 });
    await sleep(180);
    setStatus({ shell: 'ready' });
    emit('progress', { phase: 'compiler', done: 1, total: 2 });
    await sleep(320);
    setStatus({ compiler: 'ready' });
    emit('progress', { phase: 'done', done: 2, total: 2 });
  },

  status() {
    return { ...state };
  },

  on(event, fn) {
    if (!bus.has(event)) bus.set(event, new Set());
    bus.get(event).add(fn);
    return () => bus.get(event).delete(fn);
  },

  async check(map) {
    await sleep(140);
    const out = [];
    for (const [path, text] of Object.entries(map)) {
      if (!path.endsWith('.go')) continue;
      const lines = text.split('\n');

      if (!/^\s*package\s+\w+/m.test(text)) {
        out.push({
          file: path,
          line: 1,
          col: 1,
          severity: 'error',
          message: 'expected package clause',
        });
      }

      lines.forEach((line, i) => {
        const hole = line.match(/\[\[(\w+)\]\]/);
        if (hole) {
          out.push({
            file: path,
            line: i + 1,
            col: line.indexOf(hole[0]) + 1,
            severity: 'error',
            message: `blank [[${hole[1]}]] is still empty`,
          });
        }
      });

      const depth = bracketBalance(text);
      for (const [ch, n] of Object.entries(depth)) {
        if (n !== 0) {
          out.push({
            file: path,
            line: lines.length,
            col: 1,
            severity: 'error',
            message:
              n > 0 ? `unclosed ${ch}` : `unexpected closing ${ch === '{' ? '}' : ch === '(' ? ')' : ']'}`,
          });
        }
      }
    }
    // A fixture may add diagnostics that only a compiler would find.
    if (fixture?.diagnostics?.length) out.push(...fixture.diagnostics);
    return out;
  },

  async format(map) {
    await sleep(120);
    const out = {};
    for (const [path, text] of Object.entries(map)) {
      out[path] = path.endsWith('.go') ? gofmtish(text) : text;
    }
    return { files: out };
  },

  async build(map) {
    setStatus({ compiler: 'loading' });
    await sleep(260);
    const diagnostics = await Runtime.check(map);
    setStatus({ compiler: 'ready' });
    const ok = !diagnostics.some((d) => d.severity === 'error');
    const artifactId = ok ? `mock-${++seq}` : null;
    if (ok) artifacts.set(artifactId, fixture);
    return { ok, diagnostics, artifactId };
  },

  run(artifactId, opts = {}) {
    const fx = artifacts.get(artifactId) || fixture || {};
    const stdout = fx.stdout || [];
    const stderr = fx.stderr || [];
    const exit = fx.exit ?? 0;
    let stopped = false;
    let timer = 0;

    // Paced, not dumped: a run that appears all at once hides whether the pane
    // handles streaming, and the pacing is what makes "stop" mean anything.
    const script = [
      ...stdout.map((l) => ['out', l]),
      ...stderr.map((l) => ['err', l]),
    ];
    let i = 0;
    const step = () => {
      if (stopped) return;
      if (i >= script.length) {
        opts.onExit?.(exit);
        return;
      }
      const [kind, line] = script[i++];
      (kind === 'out' ? opts.onStdout : opts.onStderr)?.(line + '\n');
      timer = setTimeout(step, 26);
    };
    timer = setTimeout(step, 90);

    return {
      stop() {
        if (stopped) return;
        stopped = true;
        clearTimeout(timer);
        opts.onStderr?.('\n[stopped]\n');
        opts.onExit?.(130);
      },
    };
  },

  shell: {
    cwd: () => cwd,
    async exec(line, opts = {}) {
      await sleep(60);
      const out = (s) => opts.onStdout?.(s.endsWith('\n') ? s : s + '\n');
      const err = (s) => opts.onStderr?.(s.endsWith('\n') ? s : s + '\n');
      const argv = line.trim().split(/\s+/).filter(Boolean);
      if (!argv.length) return { code: 0 };
      const [cmd, ...rest] = argv;

      switch (cmd) {
        case 'pwd':
          out(cwd);
          return { code: 0 };
        case 'ls': {
          const names = Object.keys(files).sort();
          if (rest.includes('-l') || rest.includes('-la')) {
            for (const n of names) out(`-rw-r--r--  ${String(files[n].length).padStart(6)}  ${n}`);
          } else if (names.length) {
            out(names.join('  '));
          }
          return { code: 0 };
        }
        case 'cat': {
          if (!rest.length) {
            err('cat: missing operand');
            return { code: 1 };
          }
          let code = 0;
          for (const p of rest) {
            if (p in files) out(files[p].replace(/\n$/, ''));
            else {
              err(`cat: ${p}: No such file or directory`);
              code = 1;
            }
          }
          return { code };
        }
        case 'echo':
          out(rest.join(' '));
          return { code: 0 };
        case 'wc': {
          const p = rest[rest.length - 1];
          if (!(p in files)) {
            err(`wc: ${p}: No such file or directory`);
            return { code: 1 };
          }
          out(`${files[p].split('\n').length} ${p}`);
          return { code: 0 };
        }
        case 'grep': {
          const [pat, ...paths] = rest;
          let hits = 0;
          for (const p of paths.length ? paths : Object.keys(files)) {
            (files[p] || '').split('\n').forEach((l, i) => {
              if (l.includes(pat)) {
                hits++;
                out(`${p}:${i + 1}:${l}`);
              }
            });
          }
          return { code: hits ? 0 : 1 };
        }
        case 'go':
          if (rest[0] === 'version') {
            out('go version go1.24.0 mock/wasm');
            return { code: 0 };
          }
          err(`go ${rest[0] || ''}: not supported by the mock runtime`);
          return { code: 1 };
        case 'help':
          out('pwd  ls  cat  echo  wc  grep  go version  help');
          out('This is the mock shell. The real one runs a POSIX interpreter.');
          return { code: 0 };
        default:
          err(`sh: ${cmd}: command not found`);
          return { code: 127 };
      }
    },
  },

  fs: {
    list() {
      return Object.keys(files)
        .sort()
        .map((path) => ({ path, kind: 'file', readonly: readonly.has(path) }));
    },
    async read(path) {
      await sleep(20);
      if (!(path in files)) throw new Error(`no such file: ${path}`);
      return files[path];
    },
    async write(path, text) {
      await sleep(20);
      files[path] = text;
    },
    /** Replace the tree. A level switch needs this: without it the previous
     *  level's files stay in the sandbox and `ls` disagrees with the file
     *  list. The real runtime grew the same method for the same reason. */
    async mount(next, dir) {
      await sleep(20);
      files = { ...next };
      readonly = new Set();
      if (dir) cwd = dir;
    },
  },

  // Mock-only. Nothing in the UI may depend on this existing.
  __mock: {
    setFixture(next) {
      fixture = next;
    },
  },
};

/**
 * Count unclosed brackets, skipping comments and all three of Go's literal
 * forms. This started as a few regexes and they were wrong in a way worth
 * recording: a raw string like `"command":"` contains an odd number of double
 * quotes, so stripping interpreted strings first swallowed the rest of the
 * file and the mock reported a syntax error in code that compiles. A single
 * pass with a state machine is barely longer and cannot make that mistake.
 */
function bracketBalance(text) {
  const depth = { '{': 0, '(': 0, '[': 0 };
  const closes = { '}': '{', ')': '(', ']': '[' };
  const n = text.length;
  let i = 0;
  while (i < n) {
    const c = text[i];
    if (c === '/' && text[i + 1] === '/') {
      while (i < n && text[i] !== '\n') i++;
    } else if (c === '/' && text[i + 1] === '*') {
      i += 2;
      while (i < n && !(text[i] === '*' && text[i + 1] === '/')) i++;
      i += 2;
    } else if (c === '`') {
      i++;
      while (i < n && text[i] !== '`') i++;
      i++;
    } else if (c === '"' || c === "'") {
      const quote = c;
      i++;
      while (i < n && text[i] !== quote) i += text[i] === '\\' ? 2 : 1;
      i++;
    } else {
      if (c in depth) depth[c]++;
      else if (c in closes) depth[closes[c]]--;
      i++;
    }
  }
  return depth;
}

/** Not gofmt. Two of the things gofmt does that a learner notices: leading
 *  spaces become tabs, and trailing whitespace goes away. */
function gofmtish(text) {
  return (
    text
      .split('\n')
      .map((line) => {
        const m = line.match(/^([ \t]+)/);
        if (m) {
          const lead = m[1].replace(/ {4}/g, '\t').replace(/ +$/, '');
          line = lead + line.slice(m[1].length);
        }
        return line.replace(/[ \t]+$/, '');
      })
      .join('\n')
      .replace(/\n{3,}/g, '\n\n')
      .replace(/\n*$/, '') + '\n'
  );
}

export default Runtime;
