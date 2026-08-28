// The one switch. The real runtime is owned by another agent and lands at
// ./runtime/api.js; the mock next to this file keeps the UI demonstrable
// before that exists, and keeps it demonstrable offline afterwards.
//
// `?runtime=real` or `?runtime=mock` in the URL overrides this without an edit,
// which is what makes it possible to check both without touching the tree.
const OVERRIDE = new URLSearchParams(location.search).get('runtime');

export const RUNTIME_MODULE =
  OVERRIDE === 'real'
    ? './runtime/api.js'
    : OVERRIDE === 'mock'
      ? './mock-runtime.js'
      : './mock-runtime.js'; // ← flip this default when the real runtime lands

// Pinned CDN modules. Pinned rather than ranged because an editor that changes
// under the reader between two visits is a bug report nobody can reproduce.
export const CDN = {
  cmCore: 'https://esm.sh/codemirror@6.0.1',
  cmState: 'https://esm.sh/@codemirror/state@6.7.1',
  cmView: 'https://esm.sh/@codemirror/view@6.43.9',
  cmCommands: 'https://esm.sh/@codemirror/commands@6.11.0',
  cmLangGo: 'https://esm.sh/@codemirror/lang-go@6.0.1',
  xterm: 'https://esm.sh/@xterm/xterm@5.5.0',
  xtermFit: 'https://esm.sh/@xterm/addon-fit@0.10.0',
};

// `?nocdn=1` forces the degraded path, so the textarea and the <pre> log can be
// tested without unplugging anything.
export const FORCE_NO_CDN =
  new URLSearchParams(location.search).get('nocdn') === '1';

export const CHAPTERS = ['ch00'];
