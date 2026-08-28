// The mutable things the panes have to agree about.
//
// It is a plain object rather than four modules each owning a slice of it,
// because the alternative is an import cycle every time the terminal needs to
// know whether a run is in flight. Nothing here is written from more than one
// place; the comments say which.

export const app = {
  Runtime: null, //  main.js, once, at boot
  chapter: null, //  level.js
  level: null, //    level.js
  editor: null, //   main.js, once
  runTerm: null, //  panes.js
  shellTerm: null, // panes.js
  tab: 'run', //     panes.js
  openChapters: new Set(),

  // A run in flight, as returned by Runtime.run. null when nothing is running.
  running: null, //  runner.js
  stoppedByUser: false, // runner.js
  runBuffer: '', //  runner.js — everything the current run has printed

  // {state: 'idle'|'busy'|'ok'|'error', count, diags}
  check: { state: 'idle', count: 0 }, // runner.js

  // Loading a document into the editor is itself a document change; this is how
  // the change listener tells the two apart.
  loadingDoc: false, // level.js
};
