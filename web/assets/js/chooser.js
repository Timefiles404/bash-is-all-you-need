// The hole chooser.
//
// The rule this file exists to enforce: a wrong pick must not cost anything.
// So picking a wrong option still substitutes it — you get to see your own
// answer in the code — the reason appears under it, and the chooser stays open
// with the cursor where it was. Getting back is one more click, never a reset.
//
// Reasons already read stay visible for the rest of the level, because the
// point of the `why` string is to be compared against the other options, and
// that is impossible if it disappears when the box closes.

import { el, clear } from './dom.js';
import { t, L } from './i18n.js';
import { svg } from './icons.js';

let node = null;
let cursor = 0;
let current = null;
const revealed = new Map(); // holeId -> Set(optionId)

export function forgetChooserMemory() {
  revealed.clear();
}

export function chooserOpen() {
  return !!node;
}

export function closeChooser() {
  node?.remove();
  node = null;
  current = null;
  document.removeEventListener('mousedown', onOutside, true);
}

function onOutside(e) {
  if (node && !node.contains(e.target)) closeChooser();
}

/**
 * @param hole    the level's hole object
 * @param rect    where the hole is on screen, or null to centre the box
 * @param chosen  the option id currently in the document
 * @param onPick  (optionId) => void — substitution happens in the caller
 */
export function openChooser({ hole, rect, chosen, onPick }) {
  closeChooser();
  current = { hole, onPick };
  const seen = revealed.get(hole.id) || new Set();
  revealed.set(hole.id, seen);

  cursor = Math.max(
    0,
    hole.options.findIndex((o) => o.id === chosen),
  );

  node = el('div.chooser', { role: 'dialog', 'aria-modal': 'false' });
  node.append(
    el(
      'div.chooser-head',
      null,
      el('span.tag', null, `[[${hole.id}]]`),
      el('span', null, L(hole.label) || t('chooser.title')),
    ),
  );
  const list = el('ul.chooser-opts');
  node.append(list);
  node.append(
    el(
      'div.chooser-foot',
      null,
      svg('keyboard'),
      el('span', null, t('chooser.foot')),
    ),
  );

  paint(list, hole, chosen, seen);
  document.body.append(node);
  place(rect);
  document.addEventListener('mousedown', onOutside, true);
}

function paint(list, hole, chosen, seen) {
  clear(list);
  hole.options.forEach((opt, i) => {
    const shown = seen.has(opt.id);
    const verdict = shown ? (opt.correct ? 'right' : 'wrong') : null;
    const item = el(
      'button.opt',
      {
        type: 'button',
        'data-verdict': verdict,
        onclick: () => choose(i),
        onmouseenter: () => {
          cursor = i;
          [...list.children].forEach((c, j) =>
            c.firstChild.classList.toggle('cursor', j === i),
          );
        },
      },
      el('span.key', null, String(i + 1)),
      el('code', null, opt.text),
      shown
        ? el(
            'div.why',
            null,
            el('b', null, opt.correct ? t('chooser.right') : t('chooser.wrong')),
            ' — ',
            L(opt.why),
          )
        : null,
    );
    if (i === cursor) item.classList.add('cursor');
    if (opt.id === chosen) item.setAttribute('aria-pressed', 'true');
    list.append(el('li', null, item));
  });
}

function choose(i) {
  if (!current) return;
  const { hole, onPick } = current;
  const opt = hole.options[i];
  cursor = i;
  revealed.get(hole.id).add(opt.id);
  onPick(opt.id);

  if (opt.correct) {
    // Right answer: show it landed, then get out of the way. The 420ms is long
    // enough to read the tick and short enough not to feel like a wait.
    const list = node?.querySelector('.chooser-opts');
    if (list) paint(list, hole, opt.id, revealed.get(hole.id));
    setTimeout(() => {
      if (current?.hole === hole) closeChooser();
    }, 420);
    return;
  }

  const list = node?.querySelector('.chooser-opts');
  if (list) paint(list, hole, opt.id, revealed.get(hole.id));
}

/** Keyboard handling lives here so main.js does not have to know the box has
 *  a cursor. Returns true when the key was consumed. */
export function chooserKey(e) {
  if (!node) return false;
  const n = current.hole.options.length;
  if (e.key === 'Escape') {
    closeChooser();
    return true;
  }
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    cursor = (cursor + (e.key === 'ArrowDown' ? 1 : n - 1)) % n;
    const list = node.querySelector('.chooser-opts');
    [...list.children].forEach((c, j) =>
      c.firstChild.classList.toggle('cursor', j === cursor),
    );
    return true;
  }
  if (e.key === 'Enter') {
    choose(cursor);
    return true;
  }
  if (/^[1-9]$/.test(e.key) && Number(e.key) <= n) {
    choose(Number(e.key) - 1);
    return true;
  }
  return false;
}

function place(rect) {
  const box = node.getBoundingClientRect();
  const pad = 10;
  let left = rect ? rect.left : (window.innerWidth - box.width) / 2;
  let top = rect ? rect.bottom + 6 : (window.innerHeight - box.height) / 2;

  if (left + box.width > window.innerWidth - pad) {
    left = window.innerWidth - box.width - pad;
  }
  if (left < pad) left = pad;

  // Below the hole normally; above it when that would run off the bottom, so
  // the code being edited stays visible either way.
  if (rect && top + box.height > window.innerHeight - pad) {
    top = rect.top - box.height - 6;
    if (top < pad) top = Math.max(pad, window.innerHeight - box.height - pad);
  }

  node.style.left = `${Math.round(left)}px`;
  node.style.top = `${Math.round(top)}px`;
}
