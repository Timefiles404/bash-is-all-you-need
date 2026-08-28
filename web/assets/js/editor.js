// The editor, in two forms. CodeMirror when the CDN answers, a textarea when
// it does not, behind one interface so nothing above here has to ask which.
//
// The degraded form is not a stub: it edits, it saves, it keeps hole positions
// in sync, and the holes are reachable from a list under the box. What it
// loses is highlighting, the in-place chip, and undo granularity.

import { CDN, FORCE_NO_CDN } from './config.js';

let cm = null; // the loaded CodeMirror namespace, or null

export async function loadCodeMirror() {
  if (FORCE_NO_CDN) return null;
  try {
    const [core, state, view, commands, langGo] = await Promise.all([
      import(/* @vite-ignore */ CDN.cmCore),
      import(/* @vite-ignore */ CDN.cmState),
      import(/* @vite-ignore */ CDN.cmView),
      import(/* @vite-ignore */ CDN.cmCommands),
      import(/* @vite-ignore */ CDN.cmLangGo),
    ]);
    // The meta package exports only EditorView and basicSetup; the pieces the
    // hole decorations need come from state, view and commands directly. All
    // four resolve to one copy of @codemirror/state, which matters: two copies
    // make every extension unrecognised.
    cm = { ...core, ...state, ...view, ...commands, go: langGo.go };
    return cm;
  } catch {
    // Any failure here — offline, blocked, a CDN outage — lands on the plain
    // path rather than a blank pane.
    cm = null;
    return null;
  }
}

// A dark theme defined against the same custom properties as the rest of the
// page, so the palette really does live in one file.
function theme() {
  const v = (n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim();
  return cm.EditorView.theme(
    {
      '&': { color: v('--fg-0'), backgroundColor: v('--bg-inset'), height: '100%' },
      '.cm-content': { caretColor: v('--accent-bright'), fontFamily: v('--font-mono') },
      '.cm-cursor, .cm-dropCursor': { borderLeftColor: v('--accent-bright') },
      '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection':
        { backgroundColor: 'rgba(63,178,198,0.22)' },
      '.cm-gutters': {
        backgroundColor: v('--bg-inset'),
        color: v('--fg-2'),
        border: 'none',
        borderRight: `1px solid ${v('--line')}`,
      },
      '.cm-activeLine': { backgroundColor: 'rgba(255,255,255,0.025)' },
      '.cm-activeLineGutter': { backgroundColor: 'transparent', color: v('--fg-1') },
      '.cm-lineNumbers .cm-gutterElement': { padding: '0 8px 0 12px' },
      '.cm-scroller': { lineHeight: '1.6' },
      '.cm-panels': { backgroundColor: v('--bg-2'), color: v('--fg-1') },
      '.cm-searchMatch': { backgroundColor: 'rgba(63,178,198,0.25)' },
      '.cm-tooltip': {
        backgroundColor: v('--bg-2'),
        border: `1px solid ${v('--line-strong')}`,
      },
    },
    { dark: true },
  );
}

export function createEditor(host, hooks) {
  return cm ? new CMEditor(host, hooks) : new PlainEditor(host, hooks);
}

/* ------------------------------------------------------- read-only snippets */

// A code block in the reading pane wants the highlighting and none of the
// furniture. `minimalSetup` is exactly that list: the meta package builds it
// from syntaxHighlighting plus a history and a keymap, with no gutters and no
// active-line background.
function blockTheme() {
  return cm.EditorView.theme(
    { '&': { backgroundColor: 'transparent' }, '.cm-scroller': { overflowX: 'auto' } },
    { dark: true },
  );
}

/**
 * Render `code` into `host` with the same Go highlighting the editor uses.
 * Returns false when CodeMirror is not loaded or the language is not one it
 * was given, and the caller keeps its plain <pre>.
 */
export function highlightCode(host, code, lang) {
  if (!cm || lang !== 'go') return false;
  new cm.EditorView({
    parent: host,
    state: cm.EditorState.create({
      doc: code,
      extensions: [
        cm.minimalSetup ?? cm.basicSetup,
        cm.go(),
        blockTheme(),
        cm.EditorView.editable.of(false),
        cm.EditorState.readOnly.of(true),
      ],
    }),
  });
  return true;
}

/* ------------------------------------------------------------------ CodeMirror */

class CMEditor {
  constructor(host, hooks) {
    this.degraded = false;
    this.hooks = hooks;
    this.ranges = [];
    this.setRanges = cm.StateEffect.define();
    this.readonly = new cm.Compartment();

    const rangeField = cm.StateField.define({
      create: () => [],
      update: (value, tr) => {
        for (const e of tr.effects) if (e.is(this.setRanges)) return e.value;
        if (!tr.docChanged) return value;
        // Typing near a hole moves it; mapping the endpoints keeps the chip on
        // the code it belongs to instead of on whatever is now at that offset.
        return value.map((r) => ({
          ...r,
          from: tr.changes.mapPos(r.from, -1),
          to: tr.changes.mapPos(r.to, 1),
        }));
      },
    });
    this.rangeField = rangeField;

    const decorations = cm.EditorView.decorations.compute([rangeField], (st) => {
      const rs = st.field(rangeField).filter((r) => r.to > r.from);
      return cm.Decoration.set(
        rs.map((r) =>
          cm.Decoration.mark({
            class:
              'cm-hole ' +
              (r.optionId == null
                ? 'cm-hole-empty'
                : r.correct
                  ? 'cm-hole-right'
                  : 'cm-hole-wrong'),
            attributes: { 'data-hole': r.id },
            inclusive: false,
          }).range(r.from, r.to),
        ),
        true,
      );
    });

    // A second, short-lived decoration layer, kept apart from the holes: the
    // flash is where a reading passage pointed, and it must not survive an
    // edit that has already moved the code out from under it.
    this.flashEffect = cm.StateEffect.define();
    const flashField = cm.StateField.define({
      create: () => cm.Decoration.none,
      update: (value, tr) => {
        for (const e of tr.effects) {
          if (!e.is(this.flashEffect)) continue;
          if (!e.value) return cm.Decoration.none;
          const marks = [];
          if (e.value.line != null) {
            marks.push(cm.Decoration.line({ class: 'cm-flash-line' }).range(e.value.line));
          }
          if (e.value.to > e.value.from) {
            marks.push(cm.Decoration.mark({ class: 'cm-flash' }).range(e.value.from, e.value.to));
          }
          return cm.Decoration.set(marks, true);
        }
        return tr.docChanged ? cm.Decoration.none : value;
      },
      provide: (f) => cm.EditorView.decorations.from(f),
    });

    const clicks = cm.EditorView.domEventHandlers({
      mousedown: (event) => {
        const chip = event.target.closest?.('[data-hole]');
        if (!chip) return false;
        event.preventDefault();
        hooks.onHoleClick?.(chip.dataset.hole, chip.getBoundingClientRect());
        return true;
      },
    });

    this.view = new cm.EditorView({
      parent: host,
      state: cm.EditorState.create({
        doc: '',
        extensions: [
          cm.basicSetup,
          cm.go(),
          theme(),
          rangeField,
          decorations,
          flashField,
          clicks,
          this.readonly.of(cm.EditorState.readOnly.of(false)),
          cm.EditorView.updateListener.of((u) => {
            if (u.docChanged) hooks.onChange?.(u.state.doc.toString());
          }),
        ],
      }),
    });
  }

  setDoc(text, ranges, opts = {}) {
    this.ranges = ranges;
    this.view.dispatch({
      changes: { from: 0, to: this.view.state.doc.length, insert: text },
      effects: [
        this.setRanges.of(ranges),
        this.readonly.reconfigure(cm.EditorState.readOnly.of(!!opts.readonly)),
      ],
      selection: { anchor: 0 },
      scrollIntoView: true,
    });
  }

  replaceRange(from, to, text, nextRanges) {
    this.ranges = nextRanges;
    this.view.dispatch({
      changes: { from, to, insert: text },
      effects: this.setRanges.of(nextRanges),
    });
  }

  getRanges() {
    return this.view.state.field(this.rangeField);
  }

  getText() {
    return this.view.state.doc.toString();
  }

  rectFor(holeId) {
    const node = this.view.dom.querySelector(`[data-hole="${holeId}"]`);
    return node ? node.getBoundingClientRect() : null;
  }

  /** Scroll a span into the middle of the box and mark it for a moment. */
  flash(from, to, line = null) {
    const len = this.view.state.doc.length;
    const a = Math.max(0, Math.min(len, from));
    const b = Math.max(a, Math.min(len, to));
    this.view.dispatch({
      effects: [
        this.flashEffect.of({ from: a, to: b, line }),
        cm.EditorView.scrollIntoView(a, { y: 'center' }),
      ],
    });
    clearTimeout(this.flashTimer);
    this.flashTimer = setTimeout(() => {
      this.view.dispatch({ effects: this.flashEffect.of(null) });
    }, 2400);
  }

  flashLine(n) {
    const doc = this.view.state.doc;
    const line = doc.line(Math.max(1, Math.min(doc.lines, n)));
    this.flash(line.from, line.to, line.from);
  }

  focus() {
    this.view.focus();
  }

  undo() {
    return cm.undo(this.view);
  }

  redo() {
    return cm.redo(this.view);
  }
}

/* ----------------------------------------------------------------- textarea */

class PlainEditor {
  constructor(host, hooks) {
    this.degraded = true;
    this.hooks = hooks;
    this.ranges = [];
    this.ta = document.createElement('textarea');
    this.ta.id = 'editor-fallback';
    this.ta.spellcheck = false;
    this.ta.addEventListener('input', () => hooks.onChange?.(this.ta.value));
    host.append(this.ta);
  }

  setDoc(text, ranges, opts = {}) {
    this.ranges = ranges;
    this.ta.value = text;
    this.ta.readOnly = !!opts.readonly;
    this.ta.scrollTop = 0;
  }

  replaceRange(from, to, text, nextRanges) {
    this.ranges = nextRanges;
    const v = this.ta.value;
    this.ta.value = v.slice(0, from) + text + v.slice(to);
    this.ta.setSelectionRange(from, from + text.length);
    this.hooks.onChange?.(this.ta.value);
  }

  getRanges() {
    return this.ranges;
  }

  getText() {
    return this.ta.value;
  }

  rectFor() {
    return null;
  }

  // A textarea has one way to point at a span, and it is the selection. It
  // needs the focus to be drawn, which is fair: following a cross-reference is
  // a request to look at the code.
  flash(from, to) {
    const len = this.ta.value.length;
    const a = Math.max(0, Math.min(len, from));
    const b = Math.max(a, Math.min(len, to));
    this.ta.focus();
    this.ta.setSelectionRange(a, b);
    const rows = this.ta.value.slice(0, a).split('\n').length - 1;
    const lineHeight = parseFloat(getComputedStyle(this.ta).lineHeight) || 20;
    this.ta.scrollTop = Math.max(0, rows * lineHeight - this.ta.clientHeight / 2);
  }

  flashLine(n) {
    const lines = this.ta.value.split('\n');
    const i = Math.max(1, Math.min(lines.length, n)) - 1;
    let from = 0;
    for (let j = 0; j < i; j++) from += lines[j].length + 1;
    this.flash(from, from + lines[i].length);
  }

  focus() {
    this.ta.focus();
  }

  // The browser's own undo stack is what a textarea has. Returning false lets
  // the key handler leave the event alone so the native behaviour runs.
  undo() {
    return false;
  }

  redo() {
    return false;
  }
}
