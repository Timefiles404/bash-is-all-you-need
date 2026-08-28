// The reading pane: the material for a level, beside the code rather than on
// top of it.
//
// Two things it owns and nothing else does: the split geometry (the divider
// between the material and the editor, in both the wide and the stacked
// layout), and the cross-references — a link in the prose that points at a
// hole or a line, and moves the editor when it is followed.
//
// It knows nothing about levels beyond their id and title. `showReading` is
// called with a level and hands the jump back out through the handler given at
// mount, so this file never imports level.js and the two cannot form a cycle.

import { $, el, clear, MOD } from './dom.js';
import { t, L } from './i18n.js';
import { svg } from './icons.js';
import { highlightCode } from './editor.js';
import { closeChooser } from './chooser.js';
import { renderMarkdown, splitLangs } from './markdown.js';
import { readingCollapsed, setReadingCollapsed, readingSize, setReadingSize } from './state.js';

const MIN_READ = 220; //  the narrowest column that still holds a line of prose
const MIN_CODE = 260; //  below this the editor is a scrollbar with code in it
const MIN_READ_H = 120;
const MIN_CODE_H = 160;

// Where the code column's toolbar stops fitting with words on its buttons:
// a file name, six controls, three of them labelled.
const NARROW_CODE = 420;

const cache = new Map(); // url -> {zh, en} | null

let onXref = null;
let current = null; // {level, doc} for the level on screen
let wanted = null; //  the level the newest showReading() call is fetching for
let bodyNode = null;
let titleNode = null;

/* -------------------------------------------------------------- geometry */

const stacked = () => window.matchMedia('(max-width: 1279px)').matches;

function pane() {
  return $('#editor-pane');
}

export function readingOpen() {
  return pane()?.classList.contains('reading-on') ?? false;
}

/**
 * Show or hide the pane and remember the choice for this level. The default
 * when a level has never been opened is shown: the material is the point, and
 * a learner who wants only code says so once and is believed from then on.
 */
export function applyReading(open, { remember = true } = {}) {
  const on = open && !!current?.doc;
  // The chooser is placed over a hole in fixed coordinates. Moving the split
  // moves the hole out from under it, so the popover has to go.
  closeChooser();
  pane().classList.toggle('reading-on', on);
  if (remember && current) setReadingCollapsed(current.level.id, !on);
  labelToggle();
  if (on) sizeToFit();
  syncNarrow();
}

export function toggleReading() {
  if (!current?.doc) return;
  applyReading(!readingOpen());
}

/** Keep the split inside its bounds — after a drag, and after a resize that
 *  left the editor narrower than it can usefully be. */
function sizeToFit() {
  const box = pane().getBoundingClientRect();
  const saved = readingSize();
  // The same handle is a column divider in one layout and a row divider in the
  // other, so the axis it announces has to follow the layout.
  $('#reading-split')?.setAttribute('aria-orientation', stacked() ? 'horizontal' : 'vertical');
  if (stacked()) {
    const max = Math.max(MIN_READ_H, box.height - 6 - MIN_CODE_H);
    const want = saved.h || Math.round(box.height * 0.34);
    pane().style.setProperty('--read-h', `${Math.round(clamp(want, MIN_READ_H, max))}px`);
  } else {
    const max = Math.max(MIN_READ, box.width - 6 - MIN_CODE);
    const want = saved.w || Math.round(box.width * 0.42);
    pane().style.setProperty('--read-w', `${Math.round(clamp(want, MIN_READ, max))}px`);
  }
}

function clamp(v, lo, hi) {
  return Math.min(Math.max(v, lo), hi);
}

/** Reads the column back after the grid has taken the new size, which is what
 *  makes this work for a drag as well as for a window resize. */
function syncNarrow() {
  const code = $('#code-col');
  pane().classList.toggle('code-narrow', readingOpen() && code.clientWidth < NARROW_CODE);
}

function installSplitter(node) {
  let axis = 'col';

  const move = (e) => {
    const box = pane().getBoundingClientRect();
    if (axis === 'row') {
      const max = Math.max(MIN_READ_H, box.height - 6 - MIN_CODE_H);
      const h = Math.round(clamp(e.clientY - box.top, MIN_READ_H, max));
      pane().style.setProperty('--read-h', `${h}px`);
      setReadingSize({ h });
    } else {
      const max = Math.max(MIN_READ, box.width - 6 - MIN_CODE);
      const w = Math.round(clamp(e.clientX - box.left, MIN_READ, max));
      pane().style.setProperty('--read-w', `${w}px`);
      setReadingSize({ w });
    }
    syncNarrow();
  };

  node.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    axis = stacked() ? 'row' : 'col';
    closeChooser();
    node.setPointerCapture(e.pointerId);
    node.classList.add('dragging');
    $('#app').classList.add('splitting');
    $('#app').classList.toggle('splitting-row', axis === 'row');
    e.preventDefault();
  });

  node.addEventListener('pointermove', (e) => {
    if (node.hasPointerCapture(e.pointerId)) move(e);
  });

  const end = (e) => {
    if (!node.hasPointerCapture?.(e.pointerId)) return;
    node.releasePointerCapture(e.pointerId);
    node.classList.remove('dragging');
    $('#app').classList.remove('splitting', 'splitting-row');
  };
  node.addEventListener('pointerup', end);
  node.addEventListener('pointercancel', end);

  // The same handle from the keyboard. A divider that only answers to a mouse
  // is a divider some readers cannot move at all.
  node.addEventListener('keydown', (e) => {
    const step = e.shiftKey ? 48 : 16;
    const row = stacked();
    const key = e.key;
    const back = row ? 'ArrowUp' : 'ArrowLeft';
    const fwd = row ? 'ArrowDown' : 'ArrowRight';
    if (key !== back && key !== fwd) return;
    e.preventDefault();
    const box = pane().getBoundingClientRect();
    const prop = row ? '--read-h' : '--read-w';
    const now = parseFloat(getComputedStyle(pane()).getPropertyValue(prop)) || 0;
    const next = now + (key === fwd ? step : -step);
    const max = row
      ? Math.max(MIN_READ_H, box.height - 6 - MIN_CODE_H)
      : Math.max(MIN_READ, box.width - 6 - MIN_CODE);
    const v = Math.round(clamp(next, row ? MIN_READ_H : MIN_READ, max));
    pane().style.setProperty(prop, `${v}px`);
    setReadingSize(row ? { h: v } : { w: v });
    syncNarrow();
  });
}

/* ------------------------------------------------------------------ mount */

export function mountReading(handlers = {}) {
  onXref = handlers.onXref || null;

  const host = $('#reading');
  titleNode = el('span.level');
  host.append(
    el(
      'div.pane-head',
      null,
      el('span', null, t('read.title')),
      titleNode,
      el('span.spacer'),
      el(
        'button.btn.icon-only',
        { type: 'button', id: 'btn-reading-close', onclick: () => applyReading(false) },
        svg('close'),
      ),
    ),
  );
  bodyNode = el('div.md', { id: 'reading-body' });
  host.append(bodyNode);

  installSplitter($('#reading-split'));
  $('#btn-reading').addEventListener('click', toggleReading);
  labelToggle();

  window.addEventListener('langchange', () => {
    host.querySelector('.pane-head span').textContent = t('read.title');
    labelToggle();
    paint();
  });
  window.addEventListener('resize', () => {
    if (readingOpen()) sizeToFit();
    syncNarrow();
  });
}

function labelToggle() {
  const btn = $('#btn-reading');
  if (!btn) return;
  const on = readingOpen();
  const none = !current?.doc;
  btn.title = none ? t('read.none') : `${t(on ? 'btn.readingHide' : 'btn.reading')} · ${MOD}+E`;
  btn.setAttribute('aria-pressed', String(on));
  btn.disabled = none;
  btn.replaceChildren(svg('read'));
  const close = $('#btn-reading-close');
  if (close) close.title = t('btn.readingHide');
}

/* ------------------------------------------------------------ the material */

async function fetchReading(chapterId, levelId) {
  const url = `./content/${chapterId}/reading/${levelId}.md`;
  if (cache.has(url)) return cache.get(url);
  let doc = null;
  try {
    const res = await fetch(url, { cache: 'no-cache' });
    // A level with no material is an ordinary state, not a failure: the pane
    // stays shut and its button says why.
    if (res.ok) doc = splitLangs(await res.text());
  } catch {
    doc = null;
  }
  cache.set(url, doc);
  return doc;
}

/** Load and show the material for a level. Safe to call for every level; the
 *  ones with no file simply leave the pane collapsed. */
export async function showReading(chapterId, level) {
  wanted = level;
  const doc = await fetchReading(chapterId, level.id);
  // A slow fetch that lost a race to the next level must not paint over it.
  if (wanted !== level) return;
  current = { level, doc };
  paint();
  applyReading(doc ? !readingCollapsed(level.id) : false, { remember: false });
}

function paint() {
  if (!bodyNode || !current) return;
  titleNode.textContent = L(current.level.title);
  clear(bodyNode);
  if (!current.doc) {
    bodyNode.append(el('p.empty', null, t('read.none')));
    return;
  }
  bodyNode.innerHTML = renderMarkdown(L(current.doc));
  decorate(bodyNode);
  bodyNode.scrollTop = 0;
}

/** The two things the renderer deliberately left for someone who knows about
 *  the editor: highlighted Go, and links that move it. */
function decorate(root) {
  for (const pre of root.querySelectorAll('pre.md-code[data-lang="go"]')) {
    const code = pre.textContent;
    const host = el('div');
    if (!highlightCode(host, code, 'go')) continue;
    pre.classList.add('hl');
    pre.replaceChildren(host);
  }

  for (const a of root.querySelectorAll('a.xref')) {
    const ref = parseXref(a.dataset.xref);
    a.prepend(svg('arrowRight'));
    a.title = ref ? t(`read.jump.${ref.kind}`) : '';
    a.addEventListener('click', (e) => {
      e.preventDefault();
      if (ref) onXref?.(ref);
    });
  }
}

/**
 * `#hole:1`, `#line:27`, `#line:transcript.go:27`, `#file:transcript.go`.
 *
 * Deliberately shaped like an anchor so the material stays readable as plain
 * Markdown outside this page, and so a reference that nothing here understands
 * degrades to a link that goes nowhere rather than to broken text.
 */
export function parseXref(raw) {
  const parts = String(raw || '').split(':');
  const kind = parts.shift();
  if (kind === 'hole' && parts[0]) return { kind: 'hole', id: parts[0] };
  if (kind === 'file' && parts[0]) return { kind: 'file', path: parts.join(':') };
  if (kind === 'line' && parts.length === 1) return { kind: 'line', n: Number(parts[0]) };
  if (kind === 'line' && parts.length >= 2) {
    return { kind: 'line', path: parts[0], n: Number(parts[1]) };
  }
  return null;
}
