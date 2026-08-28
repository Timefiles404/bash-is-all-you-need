// The terminal, in two forms, behind one interface — same bargain as the
// editor. xterm.js when the CDN answers, a <pre> log when it does not.
//
// Callers never write escape codes. They say what kind of line it is
// ('out' | 'err' | 'info') and each backend renders that its own way, which is
// the only reason the plain log can colour anything at all.

import { CDN, FORCE_NO_CDN } from './config.js';

let xt = null; // {Terminal, FitAddon} or null

export async function loadXterm() {
  if (FORCE_NO_CDN) return null;
  try {
    const [core, fit] = await Promise.all([
      import(/* @vite-ignore */ CDN.xterm),
      import(/* @vite-ignore */ CDN.xtermFit),
    ]);
    xt = { Terminal: core.Terminal, FitAddon: fit.FitAddon };
    return xt;
  } catch {
    xt = null;
    return null;
  }
}

const SGR = { out: '', err: '\x1b[31m', info: '\x1b[2m' };

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export function createTerm(host, opts = {}) {
  return xt ? new XTermView(host, opts) : new LogView(host, opts);
}

/* --------------------------------------------------------------------- xterm */

class XTermView {
  constructor(host, opts) {
    this.degraded = false;
    this.opts = opts;
    this.line = '';
    this.host = host;

    this.term = new xt.Terminal({
      convertEol: true,
      cursorBlink: !!opts.interactive,
      disableStdin: !opts.interactive,
      fontFamily: cssVar('--font-mono') || 'monospace',
      fontSize: 12.5,
      lineHeight: 1.35,
      scrollback: 4000,
      theme: {
        background: cssVar('--bg-inset'),
        foreground: cssVar('--fg-0'),
        cursor: cssVar('--accent-bright'),
        selectionBackground: 'rgba(63,178,198,0.28)',
        black: '#1a1f25',
        red: cssVar('--danger'),
        green: cssVar('--accent'),
        yellow: cssVar('--warn'),
        blue: '#6f9fd8',
        magenta: '#b08cd0',
        cyan: cssVar('--accent-bright'),
        white: cssVar('--fg-0'),
        brightBlack: cssVar('--fg-2'),
      },
    });
    this.fitAddon = new xt.FitAddon();
    this.term.loadAddon(this.fitAddon);
    this.term.open(host);
    this.fit();

    if (opts.interactive) this.#wireInput();
  }

  #wireInput() {
    this.term.onData((data) => {
      if (this.busy) return;
      for (const ch of data) {
        if (ch === '\r') {
          this.term.write('\r\n');
          const line = this.line;
          this.line = '';
          this.opts.onLine?.(line);
        } else if (ch === '\x7f') {
          if (this.line.length) {
            this.line = this.line.slice(0, -1);
            this.term.write('\b \b');
          }
        } else if (ch === '\x03') {
          this.line = '';
          this.term.write('^C\r\n');
          this.opts.onInterrupt?.();
          this.prompt();
        } else if (ch >= ' ') {
          this.line += ch;
          this.term.write(ch);
        }
      }
    });
  }

  write(text, kind = 'out') {
    const sgr = SGR[kind] || '';
    this.term.write(sgr ? sgr + text.replace(/\n/g, '\r\n') + '\x1b[0m' : text.replace(/\n/g, '\r\n'));
  }

  writeln(text = '', kind = 'out') {
    this.write(text + '\n', kind);
  }

  prompt() {
    this.write(`\x1b[36m${this.opts.promptText?.() ?? '$'}\x1b[0m `);
  }

  clear() {
    this.term.clear();
    this.term.write('\x1b[2J\x1b[H');
  }

  fit() {
    try {
      this.fitAddon.fit();
    } catch {
      // Fitting a pane that is display:none throws; the next fit after it is
      // shown does the real work.
    }
  }

  focus() {
    this.term.focus();
  }
}

/* ----------------------------------------------------------------- plain log */

class LogView {
  constructor(host, opts) {
    this.degraded = true;
    this.opts = opts;
    this.pre = document.createElement('pre');
    this.pre.id = 'term-fallback';
    host.append(this.pre);

    if (opts.interactive) {
      this.input = document.createElement('input');
      this.input.id = 'term-fallback-input';
      this.input.spellcheck = false;
      this.input.placeholder = opts.promptText?.() ?? '$';
      this.input.addEventListener('keydown', (e) => {
        if (e.key !== 'Enter') return;
        const line = this.input.value;
        this.input.value = '';
        this.write(`${opts.promptText?.() ?? '$'} ${line}\n`, 'info');
        opts.onLine?.(line);
      });
      host.append(this.input);
      // The <pre> has to give the input room, so it stops being 100% tall.
      this.pre.style.height = 'calc(100% - 26px)';
    }
  }

  write(text, kind = 'out') {
    const span = document.createElement('span');
    if (kind === 'err') span.className = 'e';
    if (kind === 'info') span.className = 'i';
    span.textContent = text;
    this.pre.append(span);
    this.pre.scrollTop = this.pre.scrollHeight;
  }

  writeln(text = '', kind = 'out') {
    this.write(text + '\n', kind);
  }

  prompt() {
    /* the input's placeholder is the prompt here */
  }

  clear() {
    this.pre.textContent = '';
  }

  fit() {}

  focus() {
    this.input?.focus();
  }
}
