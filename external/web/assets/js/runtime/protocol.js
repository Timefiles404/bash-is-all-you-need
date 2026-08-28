// The message protocol between the page and the runtime worker.
//
// Everything heavy lives in a Web Worker: the WebAssembly instances, the
// filesystem, the shell. The reason is measured rather than assumed — a
// 2000-iteration shell loop takes about 200ms of solid compute (see
// ARCHITECTURE.md), which on the main thread is 200ms of frozen scrolling, and
// a learner who writes `while true; do :; done` would otherwise have to close
// the tab. In a worker the page stays responsive and the runaway can be killed.
//
// Shape of every message:
//
//   → worker  { id, type, ...payload }
//   ← page    { id, ok: true, ...result }              a reply
//             { id, ok: false, error: {message, ...} } a failed reply
//             { type: EVENT.*, ...payload }            unsolicited, no id
//
// `id` is a monotonic integer from the page. Replies carry the same id.
// Streaming output arrives as unsolicited EVENT messages tagged with the id of
// the request that produced it, so a caller can route stdout to the right
// terminal without the worker knowing what a terminal is.
//
// Two rules that are easy to get wrong and expensive to debug:
//
//   1. Nothing structured-clones a class instance. Every payload is plain
//      objects, strings, numbers, and Uint8Array — the last because
//      structuredClone handles typed arrays natively and because a file's
//      bytes should not become a base64 string on the way past.
//
//   2. Output events are batched by the worker on a short timer rather than
//      posted per write. A shell printing 5000 lines would otherwise post 5000
//      messages, and postMessage is not free; the batching window is in
//      OUTPUT_FLUSH_MS and the cost of it is that output appears in chunks of
//      up to that long, which is below the threshold where a terminal looks
//      like it is stalling.

/** Requests the page sends to the worker. */
export const REQ = Object.freeze({
  INIT: 'init',

  BUILD: 'build',
  CHECK: 'check',
  FORMAT: 'format',

  RUN: 'run',
  RUN_STOP: 'run.stop',
  RUN_STDIN: 'run.stdin',

  SHELL_EXEC: 'shell.exec',
  SHELL_INTERRUPT: 'shell.interrupt',
  SHELL_AUDIT: 'shell.audit',
  SHELL_POLICY: 'shell.policy',
  SHELL_RESET: 'shell.reset',

  FS_READ: 'fs.read',
  FS_WRITE: 'fs.write',
  FS_LIST: 'fs.list',
  FS_REMOVE: 'fs.remove',
  FS_SNAPSHOT: 'fs.snapshot',
  FS_RESTORE: 'fs.restore',
  FS_MOUNT: 'fs.mount', // seed the tree with a level's files
});

/** Unsolicited messages the worker sends to the page. */
export const EVENT = Object.freeze({
  /** A component changed state: { component, state, detail? } */
  STATUS: 'status',
  /** Progress within one request: { id, phase, origin, loaded?, total? } */
  PROGRESS: 'progress',
  /** Output from a run or a shell command: { id, stream:'stdout'|'stderr', text } */
  OUTPUT: 'output',
  /** A run finished on its own: { id, code } */
  EXIT: 'exit',
  /** The filesystem or cwd changed: { cwd, nodes } — the page keeps a mirror. */
  FS_CHANGED: 'fs.changed',
  /** The worker is about to die, or has recovered: { reason } */
  FAULT: 'fault',
});

/** Components whose state appears in Runtime.status(). */
export const COMPONENT = Object.freeze({
  COMPILER: 'compiler',
  SHELL: 'shell',
  LLM: 'llm',
});

/**
 * How long the worker may accumulate output before posting it.
 *
 * 16ms is one frame. Below that the page cannot show the difference; above it a
 * terminal starts to feel like it is buffering. Chosen for that reason and not
 * measured against anything else.
 */
export const OUTPUT_FLUSH_MS = 16;

/**
 * How long a shell command may run before the page offers to kill the worker.
 *
 * Not a timeout the worker enforces — it cannot, since a Go program spinning in
 * a tight loop never returns to the JS event loop to be told. The page starts a
 * timer when it sends the request and, if it expires, shows the learner a stop
 * button that calls Worker.terminate(). See ARCHITECTURE.md on what that costs.
 */
export const RUNAWAY_HINT_MS = 5000;
