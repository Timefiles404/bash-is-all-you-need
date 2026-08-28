// The introduction: what this course is, and what a coding agent actually is.
//
// It is the one page here that is read rather than operated, so it gets its own
// chrome — a measure narrow enough to read, real headings, and its own scroll —
// laid over the editor and the terminal instead of squeezed between them. It
// occupies the same grid tracks they do (see intro.css), so it never takes
// width away from a workbench it is covering anyway.
//
// Two things this module deliberately does not do:
//
//   * It does not touch index.html. The stylesheet is linked from here and the
//     pane is built here, so the page's markup belongs to whoever owns the
//     workbench.
//   * It does not add a key to i18n.js. Every string it shows is a {zh, en}
//     pair in content/intro/intro.json, exactly like a chapter's prose, which
//     is what keeps the introduction editable without touching the chrome.

import { $, el, clear } from '../dom.js';
import { L } from '../i18n.js';
import { svg } from '../icons.js';
import { FIGURES } from './intro-figures.js';

const CSS_URL = new URL('../../css/intro.css', import.meta.url).href;
const DATA_URL = new URL('../../../content/intro/intro.json', import.meta.url).href;

// Separate from state.js's key on purpose: this is not progress the learner
// earned, it is one boolean about one page, and state.js is shared.
const SEEN_KEY = 'biayn.intro.v1';

let doc = null; // the parsed content, once
let pane = null; // the mounted element, once
let ctx = null; // { openLevel, chapters }, handed over by rail.js
let considered = false; // the first-visit decision is made once per load

/**
 * Fetched at module load rather than on first open, because the rail needs the
 * entry's name before anybody has clicked anything, and one small JSON is
 * cheaper than a placeholder that changes under the reader.
 */
const ready = fetch(DATA_URL, { cache: 'no-cache' })
  .then((res) => {
    if (!res.ok) throw new Error(String(res.status));
    return res.json();
  })
  .then((data) => {
    doc = data;
  })
  .catch((err) => {
    doc = { error: String(err.message || err), ui: {} };
  })
  .then(() => {
    // The rail drew itself with the fallback name; tell it to draw again.
    window.dispatchEvent(new CustomEvent('intro:toggle', { detail: { open: isOpen() } }));
  });

// The rail draws before that fetch can land, and a nameless button in the rail
// reads as a bug. These two are the only strings in this file, they are the
// placeholder for the first frame, and they must match intro.json's `ui`.
const FALLBACK_LABEL = { zh: '开始之前', en: 'Before you start' };
const FALLBACK_HINT = { zh: '引言 · 随时可以回来', en: 'Introduction — always open' };

/** The rail's label for this page, and its tooltip. */
export const label = () => L(doc?.ui?.railLabel) || L(FALLBACK_LABEL);
export const hint = () => L(doc?.ui?.railHint) || L(FALLBACK_HINT);

/* --------------------------------------------------------------- the switch */

function seen() {
  try {
    return localStorage.getItem(SEEN_KEY) === '1';
  } catch {
    // Storage refused (private mode). Showing the introduction once per visit
    // is a better failure than never showing it.
    return false;
  }
}

function markSeen() {
  try {
    localStorage.setItem(SEEN_KEY, '1');
  } catch {
    /* nothing to do, and nothing worth saying about it */
  }
}

/**
 * Called by rail.js on the first mount, before any level is opened.
 *
 * `hasProgress` is read from state.js by the caller rather than imported here,
 * so this module has no opinion about how progress is stored. A reader who has
 * been here before lands where they left off; the introduction stays one click
 * away in the rail.
 */
export function maybeAutoOpen(hasProgress) {
  if (considered) return;
  considered = true;
  if (seen() || hasProgress) return;
  openIntro();
}

export function setContext(next) {
  ctx = next;
}

export function isOpen() {
  return !!pane && pane.dataset.open === 'true';
}

export async function openIntro() {
  markSeen();
  ensureStyles();
  const node = ensurePane();
  node.dataset.open = 'true';
  window.dispatchEvent(new CustomEvent('intro:toggle', { detail: { open: true } }));
  await ready;
  if (!rendered) render();
  // After the paint, so the scroll box exists and focus does not fight the
  // editor coming up behind it.
  requestAnimationFrame(() => $('.intro-scroll', node)?.focus());
}

export function closeIntro() {
  if (!isOpen()) return false;
  pane.dataset.open = 'false';
  window.dispatchEvent(new CustomEvent('intro:toggle', { detail: { open: false } }));
  return true;
}

/* ------------------------------------------------------------------- mounts */

function ensureStyles() {
  if (document.querySelector('link[data-intro-css]')) return;
  const link = el('link', { rel: 'stylesheet', href: CSS_URL });
  link.setAttribute('data-intro-css', '');
  document.head.append(link);
}

function ensurePane() {
  if (pane) return pane;
  pane = el('section.intro', {
    id: 'intro-pane',
    'aria-label': 'introduction',
    'data-open': 'false',
  });
  pane.append(
    el(
      'header.intro-head',
      null,
      el('div.intro-head-in', null, el('span.intro-kicker'), el('nav.intro-nav')),
    ),
    el('div.intro-scroll', { tabindex: '-1' }, el('article.intro-doc')),
  );
  $('#app').append(pane);
  window.addEventListener('langchange', () => {
    if (doc) render();
  });
  return pane;
}

/* ------------------------------------------------------------------- render */

let rendered = false;

function render() {
  if (!pane || !doc) return;
  rendered = true;
  const art = clear($('.intro-doc', pane));
  const nav = clear($('.intro-nav', pane));
  $('.intro-kicker', pane).textContent = L(doc.kicker) || '';

  if (doc.error) {
    art.append(el('p.intro-err', null, `content/intro/intro.json: ${doc.error}`));
    return;
  }

  art.append(
    el('h1', null, L(doc.title)),
    el('p.intro-lede', null, ...inline(L(doc.lede))),
  );

  for (const sec of doc.sections) {
    const id = `intro-${sec.id}`;
    const node = el('section.intro-sec', { id }, el('h2', null, L(sec.heading)));
    for (const b of sec.blocks) node.append(...block(b));
    art.append(node);
    nav.append(
      el(
        'button.intro-navlink',
        {
          type: 'button',
          onclick: () => {
            document.getElementById(id)?.scrollIntoView({ block: 'start', behavior: 'smooth' });
          },
        },
        L(sec.heading),
      ),
    );
  }

  const head = $('.intro-head-in', pane);
  if (!$('.intro-close', head)) {
    head.append(
      el('span.spacer'),
      el(
        'button.btn.icon-only.intro-close',
        { type: 'button', onclick: closeIntro },
        svg('close'),
      ),
    );
  }
  $('.intro-close', head).title = L(doc.ui?.close) || '';
}

function block(b) {
  switch (b.t) {
    case 'p':
      return [el('p', null, ...inline(L(b.x)))];

    case 'h':
      return [el('h3', null, L(b.x))];

    case 'quote':
      return [el('blockquote', null, ...inline(L(b.x)))];

    case 'note':
      return [el('aside.intro-note', null, ...inline(L(b.x)))];

    case 'list':
      return [
        el('ul.intro-list', null, ...b.items.map((i) => el('li', null, ...inline(L(i))))),
      ];

    case 'steps':
      return [
        el('ol.intro-steps', null, ...b.items.map((i) => el('li', null, ...inline(L(i))))),
      ];

    case 'defs':
      return [
        el(
          'dl.intro-defs',
          null,
          ...b.items.flatMap((it) => [
            el('dt', null, ...inline(L(it.k))),
            el('dd', null, ...inline(L(it.v))),
          ]),
        ),
      ];

    case 'code':
      return [
        el(
          'pre.intro-code',
          { 'data-lang': b.lang || '' },
          el('code', null, typeof b.x === 'string' ? b.x : L(b.x)),
        ),
      ];

    case 'fig': {
      const make = FIGURES[b.id];
      if (!make) return [];
      return [
        el(
          'figure.intro-fig',
          null,
          el('div.intro-fig-box', null, make()),
          b.cap ? el('figcaption', null, ...inline(L(b.cap))) : null,
        ),
      ];
    }

    case 'turns':
      return [el('div.intro-turns', null, ...b.rows.map((r) => turn(r)))];

    case 'start':
      return [
        el(
          'p.intro-cta',
          null,
          el(
            'button.btn.primary.intro-start',
            { type: 'button', onclick: startCourse },
            L(doc.ui?.start) || '',
          ),
        ),
      ];

    default:
      return [];
  }
}

/** One line of a recorded session: who spoke, and the bytes they produced. */
function turn(r) {
  const label = L(doc.roleLabels?.[r.role]) || r.role;
  const body =
    r.role === 'call'
      ? el('pre.turn-code', null, el('code', null, `$ ${r.text}`))
      : r.role === 'out'
        ? el('pre.turn-code.turn-out', null, el('code', null, r.text))
        : el('pre.turn-say', null, r.text);
  return el(
    'div.turn',
    { 'data-role': r.role },
    el(
      'div.turn-head',
      null,
      el('span.turn-role', null, label),
      r.meta ? el('span.turn-meta', null, L(r.meta)) : null,
    ),
    body,
  );
}

function startCourse() {
  closeIntro();
  const chapter = ctx?.chapters?.[0];
  const first = chapter?.levelData?.[0];
  if (chapter && first) ctx.openLevel(chapter, first.id);
}

/* ------------------------------------------------------------ inline markup */

/**
 * Two marks and no more: `code` and **strong**.
 *
 * A full Markdown renderer would be a second parser to keep honest, and the
 * prose here does not need one. Text nodes rather than innerHTML, so a stray
 * angle bracket in a command stays a stray angle bracket.
 */
const INLINE = /`([^`]+)`|\*\*([^*]+)\*\*|\*([^*]+)\*/g;

function inline(s) {
  const out = [];
  let last = 0;
  for (const m of String(s).matchAll(INLINE)) {
    if (m.index > last) out.push(document.createTextNode(s.slice(last, m.index)));
    if (m[1] !== undefined) out.push(el('code', null, m[1]));
    else if (m[2] !== undefined) out.push(el('strong', null, m[2]));
    else out.push(el('em', null, m[3]));
    last = m.index + m[0].length;
  }
  if (last < s.length) out.push(document.createTextNode(s.slice(last)));
  return out;
}

/* --------------------------------------------------------------------- keys */

// Registered at module load, which is before main.js installs the global
// handler — so Escape closes the introduction rather than reaching the runner's
// stop(). Capture, for the same reason keys.js uses capture.
document.addEventListener(
  'keydown',
  (e) => {
    if (e.key !== 'Escape' || !isOpen()) return;
    if (document.querySelector('#overlay-root .scrim')) return; // an overlay is on top
    closeIntro();
    e.preventDefault();
    e.stopPropagation();
  },
  true,
);
