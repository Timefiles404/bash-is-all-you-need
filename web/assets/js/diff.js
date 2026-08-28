// The step-through after a reveal.
//
// It is not a diff between two files. It is the file being assembled: the empty
// template, then one blank filled per step, in the order they appear in the
// source. That order is the one the reader would have worked in, so stepping
// forward reads like watching someone write it.

import { el, clear } from './dom.js';
import { t, L } from './i18n.js';
import { svg } from './icons.js';
import { buildSteps } from './holes.js';
import { openSheet } from './overlays.js';

export function showDiff(level, path) {
  const spec = level.files.find((f) => f.path === path) || level.files[0];
  const steps = buildSteps(spec.template, level.holes);
  let at = 0;

  const code = el('pre.diff-code');
  const label = el('span.step-label');
  const counter = el('span.mono');

  const prev = el(
    'button.btn',
    { type: 'button', onclick: () => go(at - 1) },
    svg('arrowLeft'),
    t('diff.prev'),
  );
  const next = el(
    'button.btn.primary',
    { type: 'button', onclick: () => go(at + 1) },
    t('diff.next'),
    svg('arrowRight'),
  );

  function go(i) {
    at = Math.max(0, Math.min(steps.length - 1, i));
    paint();
  }

  function paint() {
    const cur = steps[at].text.split('\n');
    const before = at > 0 ? steps[at - 1].text.split('\n') : null;
    clear(code);

    // Only the line that changed is marked. Every option in this chapter is a
    // single line, so a positional compare is enough; if that ever stops being
    // true the guard below keeps the view honest rather than mis-marking.
    const sameShape = !before || before.length === cur.length;
    cur.forEach((line, i) => {
      const changed = sameShape && before && before[i] !== line;
      code.append(el('span.ln' + (changed ? '.add' : ''), null, (line || ' ') + '\n'));
    });

    const hole = steps[at].filled;
    label.textContent =
      at === 0
        ? t('diff.blank')
        : `${t('diff.filled', { i: hole })} — ${L(level.holes.find((h) => h.id === hole)?.label)}`;
    counter.textContent = t('diff.step', { i: at, n: steps.length - 1 });
    prev.disabled = at === 0;
    next.disabled = at === steps.length - 1;

    const marked = code.querySelector('.ln.add');
    marked?.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }

  paint();

  openSheet({
    title: `${t('diff.title')} — ${spec.path}`,
    body: el('div.diff-shell', null, el('div.diff-bar', null, counter, label), code),
    foot: [prev, next],
  });
}
