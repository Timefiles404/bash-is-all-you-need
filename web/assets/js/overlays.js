// Overlays: one sheet primitive, and the four things built on it. They share a
// stack so Escape always closes the topmost thing rather than whichever one
// happened to bind the key last.

import { $, el, clear, MOD } from './dom.js';
import { t, L, getLang, toggleLang } from './i18n.js';
import { svg, clearMark } from './icons.js';

const stack = [];

export function anyOverlay() {
  return stack.length > 0;
}

export function closeTopOverlay() {
  const top = stack.pop();
  if (!top) return false;
  top.node.remove();
  top.onClose?.();
  return true;
}

export function closeAllOverlays() {
  while (closeTopOverlay());
}

/**
 * @param title    heading text, or null for a bare card
 * @param body     a node
 * @param foot     an array of nodes, or null
 * @param wide     use the palette geometry instead of the sheet's
 */
export function openSheet({ title, body, foot, className = '', onClose, dismissable = true }) {
  const sheet = el('div.sheet' + (className ? '.' + className.split(' ').join('.') : ''));
  if (title) {
    sheet.append(
      el(
        'div.sheet-head',
        null,
        el('h2', null, title),
        el('span.spacer'),
        el(
          'button.btn.icon-only',
          { type: 'button', title: t('btn.close'), onclick: () => closeTopOverlay() },
          svg('close'),
        ),
      ),
    );
  }
  sheet.append(el('div.sheet-body', null, body));
  if (foot?.length) sheet.append(el('div.sheet-foot', null, ...foot));

  const scrim = el('div.scrim', {
    onmousedown: (e) => {
      if (dismissable && e.target === scrim) closeTopOverlay();
    },
  });
  scrim.append(sheet);
  $('#overlay-root').append(scrim);
  stack.push({ node: scrim, onClose });
  // Focus the sheet so Escape and Tab land somewhere sensible straight away.
  sheet.querySelector('button, input, [tabindex]')?.focus();
  return { close: () => closeTopOverlay(), sheet };
}

/* ------------------------------------------------------------------- toasts */

export function toast(msg, kind = '') {
  const node = el('div.toast' + (kind ? '.' + kind : ''), null, msg);
  $('#toasts').append(node);
  setTimeout(() => {
    node.style.transition = 'opacity 200ms';
    node.style.opacity = '0';
    setTimeout(() => node.remove(), 220);
  }, 3200);
}

/* --------------------------------------------------------------------- help */

export const BINDINGS = [
  ['save', `${MOD}+S`],
  ['run', `${MOD}+Enter`],
  ['stop', 'Esc'],
  ['format', `${MOD}+Shift+F`],
  ['rail', `${MOD}+B`],
  ['palette', `${MOD}+K`],
  ['undo', `${MOD}+Z`],
  ['redo', `Shift+${MOD}+Z`],
  ['help', '?'],
];

export function showHelp() {
  const grid = el('div.key-grid');
  for (const [key, combo] of BINDINGS) {
    grid.append(
      el('div.key-row', null, el('span.desc', null, t('key.' + key)), el('span.kbd', null, combo)),
    );
  }
  const body = el(
    'div',
    null,
    grid,
    el('p', { style: 'margin-top:16px' }, t('help.note')),
  );
  openSheet({
    title: t('help.title'),
    body,
    foot: [
      el('span.muted', null, t('help.lang')),
      el(
        'button.btn',
        {
          type: 'button',
          onclick: () => {
            toggleLang();
            closeTopOverlay();
            showHelp();
          },
        },
        getLang() === 'zh' ? 'English' : '中文',
      ),
    ],
  });
}

/* ---------------------------------------------------------- command palette */

export function showPalette(commands) {
  let filtered = commands;
  let cursor = 0;

  const list = el('ul.cmd-list');
  const input = el('input', {
    type: 'text',
    placeholder: t('palette.placeholder'),
    spellcheck: false,
    oninput: () => {
      const q = input.value.trim().toLowerCase();
      filtered = q
        ? commands.filter((c) => c.label.toLowerCase().includes(q) || c.id.includes(q))
        : commands;
      cursor = 0;
      paint();
    },
    onkeydown: (e) => {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        if (!filtered.length) return;
        cursor = (cursor + (e.key === 'ArrowDown' ? 1 : filtered.length - 1)) % filtered.length;
        paint();
      } else if (e.key === 'Enter') {
        e.preventDefault();
        const cmd = filtered[cursor];
        if (!cmd) return;
        closeTopOverlay();
        cmd.run();
      }
    },
  });

  function paint() {
    clear(list);
    if (!filtered.length) {
      list.append(el('li', null, el('div.cmd.muted', null, t('palette.empty'))));
      return;
    }
    filtered.forEach((cmd, i) => {
      const btn = el(
        'button.cmd',
        {
          type: 'button',
          onclick: () => {
            closeTopOverlay();
            cmd.run();
          },
        },
        svg(cmd.icon || 'dot'),
        el('span.desc', null, cmd.label),
        cmd.combo ? el('span.kbd', null, cmd.combo) : null,
      );
      if (i === cursor) btn.classList.add('cursor');
      list.append(el('li', null, btn));
    });
  }
  paint();

  const sheet = el('div.sheet.palette', null, input, list);
  const scrim = el('div.scrim', {
    onmousedown: (e) => {
      if (e.target === scrim) closeTopOverlay();
    },
  });
  scrim.append(sheet);
  $('#overlay-root').append(scrim);
  stack.push({ node: scrim });
  input.focus();
}

/* -------------------------------------------------------------- level clear */

/**
 * Brief and skippable: Escape, the scrim, or the button. The animation is two
 * strokes drawing themselves, which is over in under a second and does not
 * scatter anything across the page.
 */
export function showLevelClear({ title, sub, actions }) {
  const card = el(
    'div.clear-card',
    null,
    clearMark(),
    el('h2', null, title),
    el('div.sub', null, sub),
    el('div.row', null, ...actions),
  );
  const scrim = el('div.scrim', {
    onmousedown: (e) => {
      if (e.target === scrim) closeTopOverlay();
    },
  });
  scrim.append(card);
  $('#overlay-root').append(scrim);
  stack.push({ node: scrim });
  card.querySelector('button')?.focus();
}

/* ------------------------------------------------------------ closing panel */

export function showClosing(chapter) {
  const c = chapter.closing || {};
  const body = el('div');
  if (c.intro) body.append(el('p', null, L(c.intro)));

  if (c.reading?.length) {
    body.append(el('h3', null, t('closing.reading')));
    const ul = el('ul.closing-list');
    for (const item of c.reading) {
      ul.append(
        el(
          'li',
          null,
          item.href
            ? el('a.t.mono', { href: item.href, target: '_blank', rel: 'noreferrer' }, item.label)
            : el('span.t.mono', null, item.label),
          el('span.n', null, L(item.note)),
        ),
      );
    }
    body.append(ul);
  }

  if (c.tryThis?.length) {
    body.append(el('h3', null, t('closing.try')));
    const ul = el('ul.closing-list');
    for (const item of c.tryThis) {
      ul.append(el('li', null, el('span.t', null, L(item.title)), el('span.n', null, L(item.body))));
    }
    body.append(ul);
  }

  openSheet({
    title: `${t('closing.title')} — ${L(chapter.title)}`,
    body,
    foot: [
      el('span.spacer', { style: 'flex:1 1 auto' }),
      el('button.btn.primary', { type: 'button', onclick: () => closeTopOverlay() }, t('closing.back')),
    ],
  });
}
