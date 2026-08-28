// Reading and replaying the repository's own JSONL traces.
//
// stages/02-see-everything writes one JSON object per line, and stage 02's
// `--replay` feeds those lines back through the same Subscriber the live agent
// used. That is the trick the whole site's default mode rests on: the agent
// core prints nothing, so a recorded event is indistinguishable from a live
// one, and a replay is not a second implementation of the interface — it is the
// interface, driven by a file.
//
// This file is the JavaScript half of the same bargain. It reads the format the
// Go writes, byte for byte, and it makes the same two decisions ReadTrace makes,
// for the same reasons:
//
//   * A final line that stops mid-object is not an error. It is the normal
//     shape of a trace whose agent was killed — which is the session you most
//     want to look at. Everything recoverable comes back, followed by a
//     synthetic notice saying what was wrong.
//   * A complete line that does not parse is real damage and is counted
//     separately, because "the writer died" and "the file is corrupt" call for
//     different reactions.
//
// The event vocabulary is the repository's, unchanged. Renaming a kind here
// would silently break replay of every session recorded before the rename,
// which is the warning events.go carries on the constants themselves.

/** Marks events this reader synthesises rather than read. Matches Go's constant. */
export const TRACE_NOTICE_PREFIX = '[trace] ';

/**
 * The longest replay will wait between two recorded events.
 *
 * Five seconds, the same as Go's maxReplayGap and for the same reason: a trace
 * records real gaps and a real session contains a human who went to lunch.
 * Everything replay exists to convey — time to first token, the pacing of text
 * deltas, a command's wall clock — is under it; everything above it is a person
 * being idle, which the timestamps report better than a wait does.
 */
export const MAX_REPLAY_GAP_MS = 5000;

/**
 * Kinds a level's replay needs at minimum. A trace may carry more; a viewer
 * that does not understand a kind should show it rather than drop it, which is
 * why nothing here filters by default.
 */
export const CORE_KINDS = Object.freeze([
  'user_message',
  'turn_start',
  'request',
  'first_token',
  'text_delta',
  'reasoning_delta',
  'tool_call_start',
  'tool_args_delta',
  'tool_call_ready',
  'gate_verdict',
  'command_start',
  'command_end',
  'tool_result',
  'usage',
  'response_end',
  'turn_end',
  'notice',
  'error',
]);

/**
 * Parse a JSONL trace.
 *
 * @param {string} text
 * @returns {{events: object[], corrupt: number, truncatedBytes: number}}
 */
export function parseTrace(text) {
  const events = [];
  let corrupt = 0;
  let truncatedBytes = 0;

  // Split on '\n' and treat a non-empty final piece with no terminator as the
  // interrupted write. `text.split` gives an empty last element for a file that
  // ends in a newline, which is exactly the distinction needed.
  const lines = text.split('\n');
  const lastIndex = lines.length - 1;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const trimmed = line.trim();
    if (!trimmed) continue;
    if (i === lastIndex) {
      truncatedBytes = new TextEncoder().encode(line).length;
      continue;
    }
    if (trimmed[0] !== '{') {
      corrupt++;
      continue;
    }
    try {
      events.push(JSON.parse(trimmed));
    } catch {
      corrupt++;
    }
  }

  if (corrupt > 0) {
    events.push(notice(`${corrupt} line(s) in this trace did not parse and were skipped`));
  }
  if (truncatedBytes > 0) {
    events.push(
      notice(
        `the last line of this trace is ${truncatedBytes} bytes and has no terminator — ` +
          `the agent that wrote it was killed mid-write. ${events.length} event(s) were recovered.`,
      ),
    );
  }
  return { events, corrupt, truncatedBytes };
}

function notice(text) {
  return { seq: -1, t: new Date().toISOString(), kind: 'notice', text: TRACE_NOTICE_PREFIX + text };
}

/**
 * A one-line summary of a whole trace, for the header above a filtered view.
 *
 * "This session made 47 model calls and you are looking at 3 of them" is the
 * context that stops a filtered view from being mistaken for the session.
 */
export function summarize(events) {
  let turns = 0;
  let commands = 0;
  let input = 0;
  let output = 0;
  let cacheRead = 0;
  let cacheWrite = 0;
  let first = null;
  let last = null;
  for (const e of events) {
    if (e.kind === 'turn_start') turns++;
    if (e.kind === 'command_end') commands++;
    if (e.usage) {
      input += e.usage.input || 0;
      output += e.usage.output || 0;
      cacheRead += e.usage.cache_read || 0;
      cacheWrite += e.usage.cache_write || 0;
    }
    if (e.t) {
      const t = Date.parse(e.t);
      if (!Number.isNaN(t)) {
        if (first === null || t < first) first = t;
        if (last === null || t > last) last = t;
      }
    }
  }
  return {
    events: events.length,
    turns,
    commands,
    durationMs: first !== null && last !== null ? last - first : 0,
    usage: { input, output, cacheRead, cacheWrite, prompt: input + cacheRead + cacheWrite },
  };
}

/**
 * Replay a trace into a subscriber, with the recorded timing.
 *
 * The subscriber is a plain function taking one event, which is what the Go
 * Subscriber interface is. Nothing about a UI appears here.
 *
 * @param {object[]} events
 * @param {(e:object)=>void} onEvent
 * @param {{speed?:number, step?:boolean, signal?:AbortSignal, filter?:(e:object)=>boolean}} opts
 * @returns {{promise: Promise<void>, pause:()=>void, resume:()=>void, stop:()=>void}}
 */
export function replay(events, onEvent, opts = {}) {
  const speed = opts.speed ?? 1;
  const shown = opts.filter ? events.filter(opts.filter) : events;

  let stopped = false;
  let paused = false;
  let resumeFn = null;

  const waitWhilePaused = () =>
    paused ? new Promise((res) => (resumeFn = res)) : Promise.resolve();

  const promise = (async () => {
    let prev = null;
    for (const e of shown) {
      if (stopped || opts.signal?.aborted) return;
      await waitWhilePaused();

      if (speed > 0 && prev !== null && e.t) {
        const t = Date.parse(e.t);
        // Negative gaps are possible and are not a bug to fix: two events can
        // share a timestamp, and a trace merged from two processes can go
        // backwards. Clamp at zero and keep moving forward.
        let gap = Math.max(0, t - prev);
        if (gap > MAX_REPLAY_GAP_MS) gap = MAX_REPLAY_GAP_MS;
        if (gap > 0) await sleep(gap / speed);
      }
      if (e.t) {
        const t = Date.parse(e.t);
        if (!Number.isNaN(t)) prev = t;
      }
      if (stopped) return;
      onEvent(e);
    }
  })();

  return {
    promise,
    pause() {
      paused = true;
    },
    resume() {
      paused = false;
      resumeFn?.();
      resumeFn = null;
    },
    stop() {
      stopped = true;
      resumeFn?.();
    },
  };
}

const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
