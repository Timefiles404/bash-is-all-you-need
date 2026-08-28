// The words the runtime is allowed to use about itself.
//
// This file exists because of one rule, and the rule is the whole reason the
// site can be trusted:
//
//   The interface must never say "compiling" when it is replaying a result
//   that was computed months ago on somebody else's machine.
//
// Almost everything a learner runs on this site was compiled at build time and
// is being played back. That is a good design — it is instant, it works
// offline, and the error text is the error `go build` really produced — but it
// is only honest if the difference is visible. A progress bar that says
// "Compiling…" for 40ms of table lookup is a small lie that costs the site
// exactly the thing this repository is careful about.
//
// So the vocabulary is closed. A phase name carries where the answer came from,
// and PHASE_LABEL is the only approved English for it. If the UI needs a word
// that is not here, the runtime is doing something it has not admitted to.

/** The four states in Runtime.status(). Fixed by the runtime interface. */
export const STATE = Object.freeze({
  IDLE: 'idle', // nothing loaded yet, and nothing has been asked for
  LOADING: 'loading', // fetching or instantiating
  READY: 'ready', // usable now
  UNAVAILABLE: 'unavailable', // asked for, and it will not work here
});

/**
 * Where an answer came from. Reported alongside every build and every
 * diagnostic, and surfaced in the UI as a badge rather than buried.
 */
export const ORIGIN = Object.freeze({
  /** Computed at build time by web/tools/genlevels, shipped as a table. */
  RECORDED: 'recorded',
  /** Produced just now by a Go toolchain running in this browser. */
  LIVE: 'live',
});

/**
 * Phases a build or run passes through. Each name says what is happening, and
 * the recorded ones are deliberately not spelled like the live ones.
 */
export const PHASE = Object.freeze({
  // --- recorded path ---------------------------------------------------------
  /** Looking the chosen options up in the precomputed table. Microseconds. */
  MATCHING: 'matching',
  /** Fetching the prebuilt .wasm for this combination. */
  FETCHING: 'fetching',
  /** WebAssembly.compile + instantiate on an artifact we already had. */
  STARTING: 'starting',

  // --- live path, only ever reached with a real toolchain loaded -------------
  /** Type-checking in the browser. */
  CHECKING: 'checking',
  /** A Go compiler is running, here, now. */
  COMPILING: 'compiling',
  /** A Go linker is running, here, now. */
  LINKING: 'linking',

  // --- shared ---------------------------------------------------------------
  RUNNING: 'running',
  DONE: 'done',
});

/**
 * The approved English for each phase. The UI should render these rather than
 * inventing its own, because the distinction the words carry is the point.
 */
export const PHASE_LABEL = Object.freeze({
  [PHASE.MATCHING]: 'Looking up this combination',
  [PHASE.FETCHING]: 'Loading the prebuilt program',
  [PHASE.STARTING]: 'Starting',
  [PHASE.CHECKING]: 'Type-checking',
  [PHASE.COMPILING]: 'Compiling',
  [PHASE.LINKING]: 'Linking',
  [PHASE.RUNNING]: 'Running',
  [PHASE.DONE]: 'Done',
});

/** Phases that mean a compiler really ran in this browser. */
export const LIVE_PHASES = Object.freeze([PHASE.CHECKING, PHASE.COMPILING, PHASE.LINKING]);

/**
 * One sentence the UI can put next to a result to say where it came from.
 *
 * Not decoration: a learner who hits a compiler error deserves to know whether
 * they are looking at what the compiler said about *their* edit or at what it
 * said about this option combination when the site was built. The two are the
 * same text and very different claims.
 */
export function originNote(origin) {
  return origin === ORIGIN.LIVE
    ? 'Compiled in your browser just now.'
    : 'Recorded from a real `go build` when this level was built. Choosing options replays it; it does not re-run the compiler.';
}

/** True when `phase` may only be shown while a real toolchain is loaded. */
export function isLivePhase(phase) {
  return LIVE_PHASES.includes(phase);
}
