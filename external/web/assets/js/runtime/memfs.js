// An in-memory POSIX-ish filesystem, shaped to satisfy Go's js/wasm syscall layer.
//
// Go's `GOOS=js` port does not implement a filesystem. `syscall/fs_js.go` calls
// out to a JavaScript global named `fs` whose API is the callback half of
// Node's `fs` module, and `lib/wasm/wasm_exec.js` installs a stub where every
// method answers ENOSYS. That stub is why `os.ReadFile` fails in a browser.
//
// Replace the global and Go's entire `os` package starts working. Nothing is
// patched, nothing is forked: the seam is the one the Go authors put there.
// That is what lets `mvdan.cc/sh`, compiled unmodified, read and write files in
// a browser tab.
//
// The exact surface Go requires was read off the Go source tree rather than
// guessed, and it is smaller than Node's `fs`:
//
//   constants   O_WRONLY O_RDWR O_CREAT O_TRUNC O_APPEND O_EXCL O_DIRECTORY
//   calls       open close read write fstat stat lstat readdir mkdir rmdir
//               unlink rename truncate ftruncate utimes fsync chmod fchmod
//               chown fchown lchown link symlink readlink
//   sync        writeSync   (the runtime's panic path in wasm_exec.js)
//
// Two details in `fs_js.go` are load-bearing here and are easy to get wrong:
//
//   1. `mapJSError` looks the error's `.code` up in a fixed table and **panics**
//      on a code it does not know. An error without a `.code`, or with an
//      invented one, does not surface as a Go error — it takes the program down
//      with an unrecoverable JS exception. Every failure path below therefore
//      raises FSError with a code from that table.
//
//   2. `fsCall` buffers its result channel with capacity 1 before invoking the
//      JS function, so a callback that fires **synchronously** is delivered
//      without a trip through the event loop. Node's real `fs` never does that;
//      we can, and it removes one microtask per syscall from every `cat`, `ls`
//      and `find`. See web/tools/fs-conformance for the test that this is
//      actually true of the Go we build against, because it is a property of
//      Go's internals rather than a documented promise.
//
// Not implemented, deliberately: permissions (there is one user and it is root),
// hard-link semantics beyond a shared node, and O_NONBLOCK. A teaching sandbox
// has no second user to defend against.

// Flag bits. Go reads these from `fs.constants` rather than assuming them, so
// the values only have to be self-consistent — Linux's are used because a
// learner who prints them will find them familiar.
export const O_RDONLY = 0;
export const O_WRONLY = 1;
export const O_RDWR = 2;
export const O_CREAT = 0o100;
export const O_EXCL = 0o200;
export const O_TRUNC = 0o1000;
export const O_APPEND = 0o2000;
export const O_DIRECTORY = 0o200000;

const S_IFMT = 0o170000;
const S_IFREG = 0o100000;
const S_IFDIR = 0o040000;
const S_IFLNK = 0o120000;

const MAX_SYMLINK_DEPTH = 32;

/** An error carrying a `.code` from the table `syscall/tables_js.go` accepts. */
export class FSError extends Error {
  constructor(code, message) {
    super(message || code);
    this.name = 'FSError';
    this.code = code;
  }
}

const enoent = (p) => new FSError('ENOENT', `no such file or directory: ${p}`);
const enotdir = (p) => new FSError('ENOTDIR', `not a directory: ${p}`);
const eisdir = (p) => new FSError('EISDIR', `is a directory: ${p}`);
const eexist = (p) => new FSError('EEXIST', `file exists: ${p}`);
const ebadf = () => new FSError('EBADF', 'bad file descriptor');
const einval = (m) => new FSError('EINVAL', m || 'invalid argument');

let nextIno = 1;

function now() {
  return Date.now();
}

function makeNode(kind, mode) {
  const t = now();
  const node = {
    ino: nextIno++,
    kind, // 'file' | 'dir' | 'symlink'
    mode: mode,
    nlink: 1,
    atimeMs: t,
    mtimeMs: t,
    ctimeMs: t,
    birthtimeMs: t,
    data: null, // Uint8Array, for files
    length: 0, // bytes of `data` in use; data may be over-allocated
    children: null, // Map<string, node>, for dirs
    target: null, // string, for symlinks
  };
  if (kind === 'file') node.data = new Uint8Array(0);
  if (kind === 'dir') node.children = new Map();
  return node;
}

/** Split an absolute, normalised path into its components. */
function parts(p) {
  return p.split('/').filter((s) => s.length > 0);
}

/**
 * Normalise a path lexically: collapse `//`, resolve `.` and `..` textually.
 *
 * Lexical, not symlink-aware, which is exactly what Node's `path.resolve` does
 * and therefore what Go expects when it calls it before `open`. It also means
 * `a/symlink-to-dir/..` does not land where a kernel would put it. Naming the
 * gap is cheaper than a resolution loop nothing in a lesson will exercise.
 */
export function normalize(p) {
  const out = [];
  for (const seg of parts(p)) {
    if (seg === '.') continue;
    if (seg === '..') {
      out.pop();
      continue;
    }
    out.push(seg);
  }
  return '/' + out.join('/');
}

export class MemFS {
  /**
   * @param {object} opts
   * @param {string} [opts.cwd] initial working directory; created if absent
   * @param {(bytes: Uint8Array) => void} [opts.onStdout]
   * @param {(bytes: Uint8Array) => void} [opts.onStderr]
   * @param {() => Uint8Array|null} [opts.readStdin] returns null for EOF
   */
  constructor(opts = {}) {
    this.root = makeNode('dir', S_IFDIR | 0o755);
    this.fds = new Map();
    this.nextFd = 3;
    this._cwd = '/';
    this.onStdout = opts.onStdout || (() => {});
    this.onStderr = opts.onStderr || (() => {});
    this.readStdin = opts.readStdin || (() => null);

    // Every write bumps this. The persistence layer uses it to decide whether a
    // snapshot is needed, so it does not serialise an unchanged tree on a timer.
    this.revision = 0;

    this.mkdirp(opts.cwd || '/work');
    this._cwd = normalize(opts.cwd || '/work');
  }

  // -------------------------------------------------------------------------
  // Path resolution
  // -------------------------------------------------------------------------

  /** Resolve `p` against the cwd and normalise it. Mirrors `path.resolve`. */
  resolve(p) {
    if (typeof p !== 'string') throw einval('path must be a string');
    if (p === '') throw einval('empty path');
    return normalize(p.startsWith('/') ? p : this._cwd + '/' + p);
  }

  cwd() {
    return this._cwd;
  }

  chdir(p) {
    const abs = this.resolve(p);
    const node = this._lookup(abs, true);
    if (node.kind !== 'dir') throw enotdir(abs);
    this._cwd = abs;
  }

  /** Walk to `p`'s node. `follow` controls whether a final symlink is followed. */
  _lookup(p, follow, depth = 0) {
    if (depth > MAX_SYMLINK_DEPTH) throw new FSError('ELOOP', `too many symlinks: ${p}`);
    const segs = parts(p);
    let node = this.root;
    for (let i = 0; i < segs.length; i++) {
      if (node.kind === 'symlink') {
        // An intermediate symlink: splice its target in and start again.
        const rest = '/' + segs.slice(i).join('/');
        return this._lookup(this._expand(node, segs.slice(0, i)) + rest, follow, depth + 1);
      }
      if (node.kind !== 'dir') throw enotdir(p);
      const next = node.children.get(segs[i]);
      if (!next) throw enoent(p);
      node = next;
    }
    if (node.kind === 'symlink' && follow) {
      return this._lookup(this._expand(node, segs.slice(0, -1)), follow, depth + 1);
    }
    return node;
  }

  _expand(link, dirSegs) {
    return link.target.startsWith('/')
      ? link.target
      : normalize('/' + dirSegs.join('/') + '/' + link.target);
  }

  /** Walk to `p`'s parent directory node, returning it and the final name. */
  _parent(p) {
    const segs = parts(p);
    if (segs.length === 0) throw new FSError('EBUSY', 'cannot operate on the root');
    const name = segs[segs.length - 1];
    const dirPath = '/' + segs.slice(0, -1).join('/');
    const dir = this._lookup(dirPath, true);
    if (dir.kind !== 'dir') throw enotdir(dirPath);
    return [dir, name];
  }

  // -------------------------------------------------------------------------
  // Host-side helpers. These are the API the rest of the runtime uses; the
  // Node-shaped callback API below is only for Go.
  // -------------------------------------------------------------------------

  mkdirp(p) {
    const segs = parts(this.resolve(p));
    let node = this.root;
    for (const seg of segs) {
      let next = node.children.get(seg);
      if (!next) {
        next = makeNode('dir', S_IFDIR | 0o755);
        node.children.set(seg, next);
        this.revision++;
      } else if (next.kind === 'symlink') {
        next = this._lookup(this._expand(next, []), true);
      }
      if (next.kind !== 'dir') throw enotdir(p);
      node = next;
    }
    return node;
  }

  /** Write a whole file, creating parent directories. Bytes or text. */
  writeFile(p, contents) {
    const abs = this.resolve(p);
    const segs = parts(abs);
    if (segs.length > 1) this.mkdirp('/' + segs.slice(0, -1).join('/'));
    const [dir, name] = this._parent(abs);
    let node = dir.children.get(name);
    if (node && node.kind === 'dir') throw eisdir(abs);
    if (!node) {
      node = makeNode('file', S_IFREG | 0o644);
      dir.children.set(name, node);
    }
    const bytes = typeof contents === 'string' ? new TextEncoder().encode(contents) : contents;
    node.data = bytes.slice();
    node.length = bytes.length;
    node.mtimeMs = node.ctimeMs = now();
    this.revision++;
    return node;
  }

  /** Read a whole file as bytes. */
  readFileBytes(p) {
    const abs = this.resolve(p);
    const node = this._lookup(abs, true);
    if (node.kind === 'dir') throw eisdir(abs);
    return node.data.subarray(0, node.length).slice();
  }

  /** Read a whole file as UTF-8 text. */
  readFile(p) {
    return new TextDecoder().decode(this.readFileBytes(p));
  }

  exists(p) {
    try {
      this._lookup(this.resolve(p), true);
      return true;
    } catch {
      return false;
    }
  }

  /** Every path in the tree, depth-first, sorted, with its kind. */
  list(from = '/') {
    const out = [];
    const walk = (node, prefix) => {
      const names = [...node.children.keys()].sort();
      for (const name of names) {
        const child = node.children.get(name);
        const p = prefix === '/' ? '/' + name : prefix + '/' + name;
        if (child.kind === 'dir') {
          out.push({ path: p, kind: 'dir' });
          walk(child, p);
        } else {
          out.push({ path: p, kind: 'file', size: child.length });
        }
      }
    };
    const start = this._lookup(this.resolve(from), true);
    if (start.kind !== 'dir') throw enotdir(from);
    walk(start, this.resolve(from));
    return out;
  }

  remove(p) {
    const abs = this.resolve(p);
    const [dir, name] = this._parent(abs);
    if (!dir.children.delete(name)) throw enoent(abs);
    this.revision++;
  }

  /**
   * A structural snapshot: plain JSON-and-bytes, no class instances, so it can
   * go straight into IndexedDB or postMessage without a serialiser.
   */
  snapshot() {
    const dump = (node) => {
      if (node.kind === 'dir') {
        const children = {};
        for (const [name, child] of node.children) children[name] = dump(child);
        return { k: 'd', m: node.mode, t: node.mtimeMs, c: children };
      }
      if (node.kind === 'symlink') return { k: 'l', m: node.mode, t: node.mtimeMs, s: node.target };
      return { k: 'f', m: node.mode, t: node.mtimeMs, b: node.data.subarray(0, node.length).slice() };
    };
    return { version: 1, cwd: this._cwd, tree: dump(this.root) };
  }

  static fromSnapshot(snap, opts = {}) {
    const fs = new MemFS({ ...opts, cwd: '/' });
    const load = (rec) => {
      if (rec.k === 'd') {
        const node = makeNode('dir', rec.m);
        node.mtimeMs = rec.t;
        for (const [name, child] of Object.entries(rec.c)) node.children.set(name, load(child));
        return node;
      }
      if (rec.k === 'l') {
        const node = makeNode('symlink', rec.m);
        node.target = rec.s;
        node.mtimeMs = rec.t;
        return node;
      }
      const node = makeNode('file', rec.m);
      node.data = rec.b instanceof Uint8Array ? rec.b : new Uint8Array(rec.b);
      node.length = node.data.length;
      node.mtimeMs = rec.t;
      return node;
    };
    fs.root = load(snap.tree);
    fs._cwd = snap.cwd && fs.existsRaw(snap.cwd) ? snap.cwd : '/';
    return fs;
  }

  existsRaw(p) {
    try {
      this._lookup(normalize(p), true);
      return true;
    } catch {
      return false;
    }
  }

  // -------------------------------------------------------------------------
  // The `fs` global Go talks to.
  // -------------------------------------------------------------------------

  /**
   * Build the object to install as `globalThis.fs`.
   *
   * Every method takes a Node-style trailing callback and every one of them
   * invokes it synchronously — see the note at the top of this file.
   */
  asNodeFS() {
    const fs = this;
    // `cb(err)` / `cb(null, value)`, with FSError passed through untouched so
    // Go's mapJSError finds a `.code` it recognises.
    const done = (cb, fn) => {
      let value;
      try {
        value = fn();
      } catch (err) {
        cb(err instanceof FSError ? err : new FSError('EIO', String(err && err.message)));
        return;
      }
      cb(null, value);
    };

    return {
      constants: {
        O_RDONLY,
        O_WRONLY,
        O_RDWR,
        O_CREAT,
        O_EXCL,
        O_TRUNC,
        O_APPEND,
        O_DIRECTORY,
      },

      open(path, flags, mode, cb) {
        done(cb, () => fs._open(path, flags, mode));
      },
      close(fd, cb) {
        done(cb, () => {
          if (fd > 2 && !fs.fds.delete(fd)) throw ebadf();
        });
      },
      read(fd, buffer, offset, length, position, cb) {
        done(cb, () => fs._read(fd, buffer, offset, length, position));
      },
      write(fd, buffer, offset, length, position, cb) {
        done(cb, () => fs._write(fd, buffer, offset, length, position));
      },

      // The runtime's own output path. wasm_exec.js calls this, not `write`,
      // when Go panics — so a broken program still gets its stack trace out.
      writeSync(fd, buffer) {
        return fs._write(fd, buffer, 0, buffer.length, null);
      },

      fstat(fd, cb) {
        done(cb, () => statObject(fs._fdNode(fd)));
      },
      stat(path, cb) {
        done(cb, () => statObject(fs._lookup(fs.resolve(path), true)));
      },
      lstat(path, cb) {
        done(cb, () => statObject(fs._lookup(fs.resolve(path), false)));
      },
      readdir(path, cb) {
        done(cb, () => {
          const abs = fs.resolve(path);
          const node = fs._lookup(abs, true);
          if (node.kind !== 'dir') throw enotdir(abs);
          return [...node.children.keys()].sort();
        });
      },
      mkdir(path, mode, cb) {
        done(cb, () => {
          const abs = fs.resolve(path);
          const [dir, name] = fs._parent(abs);
          if (dir.children.has(name)) throw eexist(abs);
          dir.children.set(name, makeNode('dir', S_IFDIR | (mode & 0o7777)));
          fs.revision++;
        });
      },
      rmdir(path, cb) {
        done(cb, () => {
          const abs = fs.resolve(path);
          const [dir, name] = fs._parent(abs);
          const node = dir.children.get(name);
          if (!node) throw enoent(abs);
          if (node.kind !== 'dir') throw enotdir(abs);
          if (node.children.size > 0) throw new FSError('ENOTEMPTY', `directory not empty: ${abs}`);
          dir.children.delete(name);
          fs.revision++;
        });
      },
      unlink(path, cb) {
        done(cb, () => {
          const abs = fs.resolve(path);
          const [dir, name] = fs._parent(abs);
          const node = dir.children.get(name);
          if (!node) throw enoent(abs);
          if (node.kind === 'dir') throw eisdir(abs);
          dir.children.delete(name);
          fs.revision++;
        });
      },
      rename(from, to, cb) {
        done(cb, () => {
          const src = fs.resolve(from);
          const dst = fs.resolve(to);
          const [sdir, sname] = fs._parent(src);
          const node = sdir.children.get(sname);
          if (!node) throw enoent(src);
          const [ddir, dname] = fs._parent(dst);
          sdir.children.delete(sname);
          ddir.children.set(dname, node);
          node.ctimeMs = now();
          fs.revision++;
        });
      },
      truncate(path, length, cb) {
        done(cb, () => fs._truncate(fs._lookup(fs.resolve(path), true), length));
      },
      ftruncate(fd, length, cb) {
        done(cb, () => fs._truncate(fs._fdNode(fd), length));
      },
      utimes(path, atime, mtime, cb) {
        done(cb, () => {
          const node = fs._lookup(fs.resolve(path), true);
          // Node accepts seconds or a Date; Go passes seconds.
          node.atimeMs = Number(atime) * 1000;
          node.mtimeMs = Number(mtime) * 1000;
          fs.revision++;
        });
      },
      fsync(fd, cb) {
        // Nothing is buffered below this layer, so this is genuinely a no-op
        // rather than a stub. Persistence is a separate, explicit snapshot.
        done(cb, () => fs._fdNode(fd));
      },

      // Ownership and permissions exist so that code which sets them links and
      // runs. They are recorded and never enforced: there is one user here.
      chmod(path, mode, cb) {
        done(cb, () => {
          const node = fs._lookup(fs.resolve(path), true);
          node.mode = (node.mode & S_IFMT) | (mode & 0o7777);
          fs.revision++;
        });
      },
      fchmod(fd, mode, cb) {
        done(cb, () => {
          const node = fs._fdNode(fd);
          node.mode = (node.mode & S_IFMT) | (mode & 0o7777);
          fs.revision++;
        });
      },
      chown(path, uid, gid, cb) {
        done(cb, () => fs._lookup(fs.resolve(path), true));
      },
      fchown(fd, uid, gid, cb) {
        done(cb, () => fs._fdNode(fd));
      },
      lchown(path, uid, gid, cb) {
        done(cb, () => fs._lookup(fs.resolve(path), false));
      },

      link(from, to, cb) {
        done(cb, () => {
          const src = fs.resolve(from);
          const node = fs._lookup(src, true);
          const [ddir, dname] = fs._parent(fs.resolve(to));
          if (ddir.children.has(dname)) throw eexist(to);
          node.nlink++;
          ddir.children.set(dname, node);
          fs.revision++;
        });
      },
      symlink(target, path, cb) {
        done(cb, () => {
          const abs = fs.resolve(path);
          const [dir, name] = fs._parent(abs);
          if (dir.children.has(name)) throw eexist(abs);
          const node = makeNode('symlink', S_IFLNK | 0o777);
          node.target = target;
          dir.children.set(name, node);
          fs.revision++;
        });
      },
      readlink(path, cb) {
        done(cb, () => {
          const abs = fs.resolve(path);
          const node = fs._lookup(abs, false);
          if (node.kind !== 'symlink') throw einval(`not a symlink: ${abs}`);
          return node.target;
        });
      },
    };
  }

  /**
   * Build the object to install as `globalThis.process`.
   *
   * `wasm_exec.js` installs a stub whose `cwd` and `chdir` throw ENOSYS, which
   * is why `os.Getwd` fails in a browser today. A shell needs both.
   */
  asNodeProcess() {
    const fs = this;
    return {
      getuid: () => 0,
      getgid: () => 0,
      geteuid: () => 0,
      getegid: () => 0,
      getgroups: () => [0],
      pid: 1,
      ppid: 0,
      umask: () => 0o22,
      cwd: () => fs._cwd,
      chdir: (p) => fs.chdir(p),
      // Not read by Go's syscall layer, but code and libraries look for them.
      platform: 'js',
      env: {},
      argv: ['js'],
    };
  }

  /** Build the object to install as `globalThis.path`. Go calls `resolve` only. */
  asNodePath() {
    const fs = this;
    return {
      resolve: (...segs) => {
        let out = fs._cwd;
        for (const seg of segs) out = seg.startsWith('/') ? seg : out + '/' + seg;
        return normalize(out);
      },
    };
  }

  // -------------------------------------------------------------------------
  // fd plumbing
  // -------------------------------------------------------------------------

  _open(path, flags, mode) {
    const abs = this.resolve(path);
    const wantCreate = (flags & O_CREAT) !== 0;
    const wantExcl = (flags & O_EXCL) !== 0;
    const wantTrunc = (flags & O_TRUNC) !== 0;
    const append = (flags & O_APPEND) !== 0;
    const write = (flags & (O_WRONLY | O_RDWR)) !== 0;

    let node;
    try {
      node = this._lookup(abs, true);
      if (wantExcl && wantCreate) throw eexist(abs);
    } catch (err) {
      if (err.code !== 'ENOENT' || !wantCreate) throw err;
      const [dir, name] = this._parent(abs);
      node = makeNode('file', S_IFREG | (mode & 0o7777 || 0o644));
      dir.children.set(name, node);
      this.revision++;
    }
    if (node.kind === 'dir' && write) throw eisdir(abs);
    if (wantTrunc && node.kind === 'file') {
      node.length = 0;
      node.data = new Uint8Array(0);
      this.revision++;
    }

    const fd = this.nextFd++;
    this.fds.set(fd, { node, path: abs, pos: append ? node.length : 0, append, write });
    return fd;
  }

  _fdNode(fd) {
    if (fd === 0 || fd === 1 || fd === 2) {
      // Stdio has no inode. Report a character device sized zero, which is what
      // makes Go's `os.Stdout.Stat()` say "not a regular file" as it should.
      return makeNode('file', 0o020000 | 0o666);
    }
    const entry = this.fds.get(fd);
    if (!entry) throw ebadf();
    return entry.node;
  }

  _read(fd, buffer, offset, length, position) {
    if (fd === 0) {
      const chunk = this.readStdin();
      if (!chunk || chunk.length === 0) return 0;
      const n = Math.min(length, chunk.length);
      buffer.set(chunk.subarray(0, n), offset);
      return n;
    }
    const entry = this.fds.get(fd);
    if (!entry) throw ebadf();
    const node = entry.node;
    if (node.kind === 'dir') throw eisdir(entry.path);
    const from = position === null || position === undefined ? entry.pos : position;
    if (from >= node.length) return 0;
    const n = Math.min(length, node.length - from);
    buffer.set(node.data.subarray(from, from + n), offset);
    if (position === null || position === undefined) entry.pos = from + n;
    node.atimeMs = now();
    return n;
  }

  _write(fd, buffer, offset, length, position) {
    // The buffer is a view onto wasm linear memory. It is invalidated by the
    // next allocation inside Go, so anything handed onward must be a copy —
    // this line is the difference between clean output and torn output under
    // load, and the bug it prevents looks like memory corruption.
    if (fd === 1 || fd === 2) {
      const copy = buffer.slice(offset, offset + length);
      (fd === 1 ? this.onStdout : this.onStderr)(copy);
      return length;
    }
    const entry = this.fds.get(fd);
    if (!entry) throw ebadf();
    const node = entry.node;
    if (node.kind === 'dir') throw eisdir(entry.path);

    const at = entry.append
      ? node.length
      : position === null || position === undefined
        ? entry.pos
        : position;
    this._ensure(node, at + length);
    node.data.set(buffer.subarray(offset, offset + length), at);
    if (at + length > node.length) node.length = at + length;
    if (position === null || position === undefined || entry.append) entry.pos = at + length;
    node.mtimeMs = node.ctimeMs = now();
    this.revision++;
    return length;
  }

  /**
   * Grow a file's backing array, doubling rather than fitting exactly.
   *
   * A shell loop that appends a line at a time — `for i in $(seq 5000); do echo
   * $i >> f; done` — would otherwise reallocate and copy the whole file 5000
   * times, which is quadratic and is what makes a naive in-memory FS feel
   * broken rather than slow.
   */
  _ensure(node, needed) {
    if (node.data.length >= needed) return;
    let cap = Math.max(64, node.data.length * 2);
    while (cap < needed) cap *= 2;
    const grown = new Uint8Array(cap);
    grown.set(node.data.subarray(0, node.length));
    node.data = grown;
  }

  _truncate(node, length) {
    if (node.kind !== 'file') throw einval('not a regular file');
    this._ensure(node, length);
    if (length < node.length) node.data.fill(0, length, node.length);
    node.length = length;
    node.mtimeMs = node.ctimeMs = now();
    this.revision++;
  }
}

/**
 * The stat shape Go reads, field for field, from `setStat` in fs_js.go, plus the
 * `isDirectory()` method `syscall.Open` calls to decide whether to read entries.
 */
function statObject(node) {
  return {
    dev: 1,
    ino: node.ino,
    mode: node.mode,
    nlink: node.nlink,
    uid: 0,
    gid: 0,
    rdev: 0,
    size: node.kind === 'file' ? node.length : node.kind === 'symlink' ? node.target.length : 4096,
    blksize: 4096,
    blocks: Math.ceil((node.kind === 'file' ? node.length : 0) / 512),
    atimeMs: node.atimeMs,
    mtimeMs: node.mtimeMs,
    ctimeMs: node.ctimeMs,
    birthtimeMs: node.birthtimeMs,
    isDirectory: () => node.kind === 'dir',
    isFile: () => node.kind === 'file',
    isSymbolicLink: () => node.kind === 'symlink',
  };
}
