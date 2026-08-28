// Getting model output into the page, in the two honest ways there are.
//
// Replay is the default and is what the site is designed around. Live is an
// opt-in for a learner who wants to point the thing at their own endpoint, and
// it comes with a threat model that is stated to their face rather than in a
// footnote.
//
// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------
//
// The repository writes JSONL event traces and reads them back through the same
// subscriber the live agent used. A recorded session therefore replays with
// real timing, real token counts, real tool calls, real command output, and the
// exact request bodies that were posted — with no key, no network, no CORS, no
// cost, and no variance between two learners on the same level. It is the mode
// in which a lesson can promise what the learner will see.
//
// ---------------------------------------------------------------------------
// Live, and the threat model
// ---------------------------------------------------------------------------
//
// The repository's own config file is careful in a way this cannot be. It never
// stores a key: `api_key_env` names an environment variable, and the comment
// says why — "a config file gets committed eventually, every one of them does,
// and the only reliable defence is for the secret to have nowhere to sit in the
// file at all."
//
// A browser has no environment. So the key has to sit somewhere, and every
// option is worse than an environment variable:
//
//   in memory only (the default here)
//       Gone when the tab closes. Readable by any script running on this
//       origin, which is this site's own scripts and nothing else, because a
//       strict CSP and no third-party scripts is the whole defence. Survives
//       nothing, which is the point.
//
//   sessionStorage (opt-in)
//       Survives a reload, dies with the tab. Same script-visibility as memory.
//
//   localStorage (opt-in, and the page says this in these words)
//       Survives everything, including the learner forgetting it is there.
//       Any XSS on this origin, ever, reads it. Any browser extension with
//       access to page storage reads it. A shared machine reads it.
//
// What is never done, in any mode: the key does not go to this site's server —
// there is no server — and it does not go through any proxy, ours or anyone
// else's. It is sent by the learner's browser directly to the endpoint the
// learner typed, and to nowhere else. A design that proxied a key to make CORS
// work would be asking a learner to hand their credential to a third party to
// solve a browser configuration problem, and no amount of convenience is worth
// teaching that.
//
// The consequence is CORS, and it is not a small one. A browser will not let
// this page read a response from an endpoint that does not opt in with
// Access-Control-Allow-Origin, and the major hosted providers do not, because
// browser-side API keys are exactly what they are trying to discourage. So live
// mode works with: a local model server (Ollama, llama.cpp, vLLM, LM Studio)
// started with CORS enabled; a gateway the learner runs themselves; a corporate
// proxy that sets the header. It does not work by typing a frontier vendor's
// URL into it, and preflight() says so before the learner spends a key finding
// out.

import { parseTrace, replay as replayTrace, summarize } from './trace.js';
import { STATE } from './status.js';

export const MODE = Object.freeze({
  REPLAY: 'replay',
  LIVE: 'live',
  OFF: 'off',
});

export const KEY_STORAGE = Object.freeze({
  MEMORY: 'memory',
  SESSION: 'session',
  LOCAL: 'local',
});

const SETTINGS_KEY = 'biayn.provider';
const KEY_KEY = 'biayn.provider.key';

/**
 * The four fields the repository's own `/provider-*` commands set, and nothing
 * more. A local Ollama and a frontier API are the same four fields; that is
 * stage 03's claim and it holds here.
 */
export const DEFAULT_PROVIDER = Object.freeze({
  protocol: 'openai', // 'openai' | 'anthropic'
  baseURL: 'http://localhost:11434/v1',
  model: '',
  window: 0,
});

export class LLM {
  constructor(base) {
    this.base = base;
    this.mode = MODE.REPLAY;
    this.provider = { ...DEFAULT_PROVIDER };
    this.keyStorage = KEY_STORAGE.MEMORY;
    this._key = '';
    this.state = STATE.IDLE;
    this._traces = new Map();
    this._restore();
  }

  // -------------------------------------------------------------------------
  // configuration
  // -------------------------------------------------------------------------

  _restore() {
    try {
      const raw = localStorage.getItem(SETTINGS_KEY);
      if (raw) {
        const saved = JSON.parse(raw);
        this.provider = { ...DEFAULT_PROVIDER, ...saved.provider };
        this.keyStorage = saved.keyStorage || KEY_STORAGE.MEMORY;
      }
      if (this.keyStorage === KEY_STORAGE.SESSION) this._key = sessionStorage.getItem(KEY_KEY) || '';
      if (this.keyStorage === KEY_STORAGE.LOCAL) this._key = localStorage.getItem(KEY_KEY) || '';
    } catch {
      // Storage blocked. Settings simply do not persist; nothing else changes.
    }
  }

  /** Endpoint, protocol and model. Never the key — see setKey. */
  setProvider(p) {
    this.provider = { ...this.provider, ...p };
    try {
      localStorage.setItem(
        SETTINGS_KEY,
        JSON.stringify({ provider: this.provider, keyStorage: this.keyStorage }),
      );
    } catch {
      /* not persisting settings is survivable */
    }
  }

  /**
   * @param {string} key
   * @param {'memory'|'session'|'local'} storage
   */
  setKey(key, storage = KEY_STORAGE.MEMORY) {
    this._key = key || '';
    this.keyStorage = storage;
    try {
      sessionStorage.removeItem(KEY_KEY);
      localStorage.removeItem(KEY_KEY);
      if (storage === KEY_STORAGE.SESSION) sessionStorage.setItem(KEY_KEY, this._key);
      if (storage === KEY_STORAGE.LOCAL) localStorage.setItem(KEY_KEY, this._key);
    } catch {
      // Storage refused; the key stays in memory and the caller is told so the
      // UI does not claim it was saved.
      this.keyStorage = KEY_STORAGE.MEMORY;
    }
    this.setProvider({});
  }

  forgetKey() {
    this._key = '';
    try {
      sessionStorage.removeItem(KEY_KEY);
      localStorage.removeItem(KEY_KEY);
    } catch {
      /* nothing to remove */
    }
  }

  /** What the settings panel shows about where the key is. Never the key. */
  keyStatus() {
    return {
      present: !!this._key,
      storage: this.keyStorage,
      warning:
        this.keyStorage === KEY_STORAGE.LOCAL
          ? 'This key is stored in this browser and survives closing the tab. Anything that can run script on this origin can read it. Use "this tab only" unless you have a reason not to.'
          : null,
    };
  }

  // -------------------------------------------------------------------------
  // replay
  // -------------------------------------------------------------------------

  /**
   * Load the trace a level ships.
   *
   * A level names its trace; the trace is a file under the level's directory.
   * The binding is by name and is checked: a trace whose `level` field does not
   * match is a trace from somewhere else, and replaying it under this level's
   * commentary would be describing a session that did not happen.
   */
  async loadTrace(levelId, name = 'session.jsonl') {
    const key = `${levelId}/${name}`;
    if (this._traces.has(key)) return this._traces.get(key);
    const p = (async () => {
      const res = await fetch(new URL(`levels/${levelId}/${name}`, this.base));
      if (!res.ok) throw new Error(`no trace ${name} for level ${levelId} (${res.status})`);
      const parsed = parseTrace(await res.text());
      return { ...parsed, summary: summarize(parsed.events) };
    })();
    this._traces.set(key, p);
    return p;
  }

  /**
   * Replay a level's recorded session into a subscriber.
   * @returns {{promise:Promise<void>, pause:Function, resume:Function, stop:Function}}
   */
  async replay(levelId, onEvent, opts = {}) {
    const { events } = await this.loadTrace(levelId, opts.trace);
    this.state = STATE.READY;
    return replayTrace(events, onEvent, opts);
  }

  // -------------------------------------------------------------------------
  // live
  // -------------------------------------------------------------------------

  /**
   * Find out whether live mode can work here, before a key is spent on it.
   *
   * A cross-origin fetch that CORS refuses fails as an opaque TypeError with no
   * status and no body — indistinguishable, from JavaScript, from the server
   * being down. So the message this returns names both possibilities instead of
   * picking one, because guessing would send a learner to debug the wrong
   * thing.
   */
  async preflight() {
    const url = joinURL(this.provider.baseURL, '/models');
    try {
      const res = await fetch(url, { method: 'GET', headers: this._headers() });
      if (res.ok) return { ok: true, detail: `${url} answered ${res.status}` };
      return {
        ok: false,
        reason: 'http',
        detail: `${url} answered ${res.status} ${res.statusText}. The endpoint is reachable; the request was refused.`,
      };
    } catch (err) {
      return {
        ok: false,
        reason: 'network-or-cors',
        detail:
          `The browser could not read a response from ${url}. Two causes look identical from ` +
          `here: the endpoint is not running, or it is running and does not send ` +
          `Access-Control-Allow-Origin. Most hosted providers do not send it, on purpose. ` +
          `A local server usually has a flag — Ollama takes OLLAMA_ORIGINS, llama.cpp and ` +
          `vLLM take --cors or equivalent.`,
        error: String(err && err.message),
      };
    }
  }

  _headers() {
    const h = { 'content-type': 'application/json' };
    if (!this._key) return h;
    if (this.provider.protocol === 'anthropic') {
      h['x-api-key'] = this._key;
      h['anthropic-version'] = '2023-06-01';
    } else {
      h.authorization = `Bearer ${this._key}`;
    }
    return h;
  }

  /**
   * One streaming model call, emitted as the repository's own event kinds.
   *
   * The point of normalising to those kinds here is that everything downstream
   * — the transcript view, the wire inspector, the usage row — cannot tell a
   * live call from a replayed one, which is the same property that makes
   * `--replay` fifty lines in Go instead of a second implementation.
   *
   * The two protocols are not cosmetically different: they disagree about where
   * the system prompt goes, how tool results are addressed, whether tool
   * arguments are a string or an object, and which direction token accounting
   * runs. docs/03-babel.md has the table. This is a reader for the wire, not a
   * full adapter; a level that needs the full adapter runs the Go one.
   */
  async *stream(request, { signal } = {}) {
    if (this.mode !== MODE.LIVE) throw new Error('live mode is not enabled');
    const isAnthropic = this.provider.protocol === 'anthropic';
    const url = joinURL(this.provider.baseURL, isAnthropic ? '/messages' : '/chat/completions');

    const started = performance.now();
    const res = await fetch(url, {
      method: 'POST',
      headers: this._headers(),
      body: JSON.stringify({ ...request, model: request.model || this.provider.model, stream: true }),
      signal,
    });
    if (!res.ok || !res.body) {
      const text = await res.text().catch(() => '');
      throw new Error(`${url} → ${res.status} ${res.statusText}${text ? `: ${text.slice(0, 400)}` : ''}`);
    }

    yield { kind: 'request', t: new Date().toISOString(), request: JSON.stringify(request) };

    let firstToken = false;
    for await (const data of sseLines(res.body, signal)) {
      if (data === '[DONE]') break;
      let ev;
      try {
        ev = JSON.parse(data);
      } catch {
        continue; // a keep-alive comment or a partial frame
      }
      const text = isAnthropic
        ? ev.delta?.text || ev.content_block?.text || ''
        : ev.choices?.[0]?.delta?.content || '';
      if (text) {
        if (!firstToken) {
          firstToken = true;
          yield {
            kind: 'first_token',
            t: new Date().toISOString(),
            ms: Math.round(performance.now() - started),
          };
        }
        yield { kind: 'text_delta', t: new Date().toISOString(), text };
      }
      const usage = isAnthropic ? ev.usage || ev.message?.usage : ev.usage;
      if (usage) yield { kind: 'usage', t: new Date().toISOString(), usage: normaliseUsage(usage, isAnthropic) };
    }
    yield {
      kind: 'response_end',
      t: new Date().toISOString(),
      ms: Math.round(performance.now() - started),
    };
  }
}

/**
 * Normalise token accounting, which is the one place the two protocols are
 * actively misleading rather than merely different.
 *
 * Anthropic's `input_tokens` is only the uncached remainder: an agent that ran
 * for an hour can report 18 input tokens while actually sending 18,000. The
 * total is input + cache_write + cache_read. OpenAI accounts in the opposite
 * direction — `prompt_tokens` is the full figure and `cached_tokens` is nested
 * inside it. Getting this backwards produces a cost display that is wrong by an
 * order of magnitude and looks plausible.
 */
function normaliseUsage(u, isAnthropic) {
  if (isAnthropic) {
    return {
      input: u.input_tokens || 0,
      cache_write: u.cache_creation_input_tokens || 0,
      cache_read: u.cache_read_input_tokens || 0,
      output: u.output_tokens || 0,
    };
  }
  const cached = u.prompt_tokens_details?.cached_tokens || 0;
  return {
    input: (u.prompt_tokens || 0) - cached,
    cache_read: cached,
    cache_write: 0,
    output: u.completion_tokens || 0,
    reasoning: u.completion_tokens_details?.reasoning_tokens || 0,
  };
}

/** Yield the payload of each `data:` line in an SSE stream. */
async function* sseLines(body, signal) {
  const reader = body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  try {
    for (;;) {
      if (signal?.aborted) return;
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      // Frames end at a blank line, but a single `data:` line per frame is what
      // every provider observed in docs/wire-notes.md actually sends, so split
      // on newlines and take the data lines. A multi-line data field would be
      // joined by the spec; none of the observed endpoints emits one.
      let nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).trimEnd();
        buf = buf.slice(nl + 1);
        if (line.startsWith('data:')) yield line.slice(5).trim();
      }
    }
  } finally {
    reader.releaseLock();
  }
}

function joinURL(base, path) {
  return String(base).replace(/\/+$/, '') + path;
}
