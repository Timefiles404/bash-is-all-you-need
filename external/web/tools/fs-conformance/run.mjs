// Does Go's js/wasm port actually work on memfs.js?
//
// This is the test the whole shell design rests on. Go's `syscall/fs_js.go`
// calls a JavaScript global named `fs`; memfs.js implements it; if the two
// disagree anywhere, `mvdan.cc/sh` in a browser is a nice idea and nothing
// more. The disagreement would not be a clean failure either — `mapJSError`
// panics on an unrecognised error code, so a wrong `.code` takes down the wasm
// instance rather than returning ENOENT.
//
// It runs under Node because Node can load the same ES modules the browser
// will, with no browser and no server, so it can run in CI and on a laptop with
// one command. What it exercises is Go's syscall layer, which is identical in
// both hosts.
//
//   node web/tools/fs-conformance/run.mjs path/to/shell.wasm
//
// Exit status is the number of failures.

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { MemFS } from '../../assets/js/runtime/memfs.js';

const here = dirname(fileURLToPath(import.meta.url));
const wasmPath = process.argv[2] || resolve(here, '../../assets/wasm/shell.wasm');
const wasmExecPath = resolve(here, '../../assets/js/runtime/vendor/wasm_exec.js');

let out = '';
let err = '';
const dec = new TextDecoder();

const fs = new MemFS({
  cwd: '/work',
  onStdout: (b) => {
    out += dec.decode(b);
  },
  onStderr: (b) => {
    err += dec.decode(b);
  },
});

// Replace the three globals Go's js port reads. wasm_exec.js only installs its
// ENOSYS stubs when they are absent, so setting them first is the whole
// installation step.
globalThis.fs = fs.asNodeFS();
globalThis.process = { ...fs.asNodeProcess(), argv: ['js'], env: {}, exit: () => {}, on: () => {} };
globalThis.path = fs.asNodePath();

// wasm_exec.js is a classic script that assigns globalThis.Go.
new Function(readFileSync(wasmExecPath, 'utf8'))();

const go = new Go();
const ready = new Promise((res) => {
  globalThis.__goshellReady = res;
});

const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject);
go.run(instance); // never resolves: the shell parks in select{}
await ready;

// -----------------------------------------------------------------------------

const shell = globalThis.__goshell;

function exec(line) {
  out = '';
  err = '';
  return new Promise((res) => {
    shell.exec(
      line,
      (s) => {
        out += s;
      },
      (s) => {
        err += s;
      },
      (code) => res({ code, out, err }),
    );
  });
}

let failures = 0;
let checks = 0;

async function want(line, expected, opts = {}) {
  checks++;
  const r = await exec(line);
  const got = (opts.stream === 'stderr' ? r.err : r.out).replace(/\r/g, '');
  const ok =
    expected instanceof RegExp ? expected.test(got) : got === expected;
  const codeOk = opts.code === undefined || r.code === opts.code;
  if (ok && codeOk) return;
  failures++;
  console.log(`FAIL  ${line}`);
  console.log(`  want ${JSON.stringify(String(expected))}${opts.code !== undefined ? ` code=${opts.code}` : ''}`);
  console.log(`  got  ${JSON.stringify(got)} code=${r.code}${r.err && opts.stream !== 'stderr' ? ` stderr=${JSON.stringify(r.err)}` : ''}`);
}

function check(name, cond) {
  checks++;
  if (cond) return;
  failures++;
  console.log(`FAIL  ${name}`);
}

// --- the shell language, which is mvdan.cc/sh and should simply work ---------
await want('echo hello', 'hello\n');
await want('echo a b   c', 'a b c\n');
await want('for i in 1 2 3; do echo $i; done', '1\n2\n3\n');
await want('x=5; echo $((x * 3))', '15\n');
await want('f() { echo "in $1"; }; f fn', 'in fn\n');
await want('echo ${UNSET:-fallback}', 'fallback\n');
await want('echo $(echo nested)', 'nested\n');
await want('true && echo yes || echo no', 'yes\n');
await want('false && echo yes || echo no', 'no\n');
await want("printf '%s-%d\\n' ab 7", 'ab-7\n');
await want('echo one; exit 3', 'one\n', { code: 3 });

// --- the filesystem, which is memfs.js through Go's os package ---------------
await want('pwd', '/work\n');
await want('echo written > f.txt; cat f.txt', 'written\n');
await want('echo more >> f.txt; cat f.txt', 'written\nmore\n');
await want('mkdir -p a/b/c && ls a', 'b\n');
await want('cd a/b/c && pwd', '/work/a/b/c\n');
await want('pwd', '/work/a/b/c\n'); // cwd persists across exec calls
await want('cd /work && pwd', '/work\n');
await want('cat missing.txt', /[Nn]o such file or directory/, { stream: 'stderr', code: 1 });
await want('[ -f f.txt ] && echo exists', 'exists\n');
await want('[ -d a ] && echo dir', 'dir\n');

// Globbing needs readdir to work, which is the call Go makes on every open of a
// directory. It is the one most likely to be subtly wrong.
await want('touch g1.txt g2.txt && echo *.txt', 'f.txt g1.txt g2.txt\n');
await want('ls *.txt | wc -l', '      3\n');

// A pipeline of three re-implemented commands, reading a file the shell wrote.
await want(
  'printf "pear\\napple\\npear\\nfig\\n" > fruit && sort fruit | uniq -c | sort -rn | head -1',
  '      2 pear\n',
);
await want('grep -c pear fruit', '2\n');
await want('grep zzz fruit', '', { code: 1 });
await want('sed "s/pear/PEAR/" fruit | head -1', 'PEAR\n');
await want('find . -name "g*.txt" | sort', './g1.txt\n./g2.txt\n');
await want('wc -l < fruit', '      4\n');
await want('tail -2 fruit', 'pear\nfig\n');
await want('cat fruit | tr a-z A-Z | head -1', 'PEAR\n');
await want('seq 1 4 | paste-not-a-command', /not available in the browser shell/, {
  stream: 'stderr',
  code: 127,
});

// A loop that appends 300 times exercises the growth path in memfs `_ensure`.
await want('rm -f big; for i in $(seq 1 300); do echo line$i >> big; done; wc -l < big', '    300\n');

// Redirection into a subdirectory, then reading it back through a relative path
// from a different cwd — the case where `path.resolve` and the interpreter's
// Dir have to agree.
await want('mkdir -p sub && echo deep > sub/d.txt && cd sub && cat d.txt', 'deep\n');
await want('cd /work && cat sub/d.txt', 'deep\n');

// --- the stage 08 lesson ------------------------------------------------------
await exec('cd /work');
// Write the secret with the policy off. With it on, the open handler blocks the
// redirect too — which is correct, and which quietly made an earlier version of
// this suite test an empty file.
shell.setPolicy({ level: 'off', enforce: false, secret: '.env' });
await exec('printf "KEY=secret\\n" > .env');
shell.setPolicy({ level: 'argv', enforce: true, secret: '.env' });

await want('cat .env', /blocked by the sandbox\/exec policy/, { stream: 'stderr', code: 1 });
// The bypass from 08-sandbox/code/bypass_test.go, verbatim: the value does not
// exist until eval runs, so only a check standing after expansion can see it.
await want("X=.en; eval 'cat ${X}v'", /blocked by the sandbox\/exec policy/, {
  stream: 'stderr',
  code: 1,
});
// The same string one level down. The AST cannot resolve $X, so it allows it.
// This is the chapter's central claim, asserted rather than described.
shell.setPolicy({ level: 'ast', enforce: true, secret: '.env' });
await want("X=.en; eval 'cat ${X}v'", 'KEY=secret\n');
shell.setPolicy({ level: 'argv', enforce: true, secret: '.env' });
// A command with no input must end, not wait for a Ctrl-D nobody can press.
await want('cat', '');
// The one argv alone would miss: the shell opens the file, cat gets no argument.
await want('cat < .env', /blocked by the sandbox\/open policy/, { stream: 'stderr', code: 1 });

shell.setPolicy({ level: 'string', enforce: true, secret: '.env' });
await want('cat ".e""nv"', 'KEY=secret\n'); // quoting beats the string check

shell.setPolicy({ level: 'off', enforce: false, secret: '.env' });
await want('cat .env', 'KEY=secret\n');

const audit = shell.audit();
check('audit recorded execs', audit.execs.length > 0);
check('audit recorded opens', audit.opens.includes('.env'));
check('audit recorded blocks', audit.blocked.length >= 3);

// --- host-side view of the same tree -----------------------------------------
check('host sees the shell\'s file', fs.readFile('/work/fruit').startsWith('pear\n'));
check('host list finds sub/d.txt', fs.list('/work').some((n) => n.path === '/work/sub/d.txt'));
fs.writeFile('/work/from-host.txt', 'host wrote this\n');
await want('cat from-host.txt', 'host wrote this\n');

// --- snapshot round-trip, which is what IndexedDB persistence stores ---------
const snap = fs.snapshot();
const restored = MemFS.fromSnapshot(snap);
check('snapshot round-trips file contents', restored.readFile('/work/fruit').includes('apple'));
check('snapshot round-trips the tree', restored.list('/work').length === fs.list('/work').length);

console.log(`\n${checks - failures}/${checks} checks passed`);
process.exit(failures === 0 ? 0 : 1);
