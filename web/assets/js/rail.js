// The chapter rail. Two jobs: say where you are, and say what is reachable.
//
// The collapsed form keeps both. It drops the names, not the position: the
// chapter number stays, every level keeps its dot, the current one keeps its
// accent bar, and the progress track turns vertical.

import { $, el, clear } from './dom.js';
import { t, L } from './i18n.js';
import { svg } from './icons.js';
import { isDone, levelStatus, progress } from './state.js';

let ctx = null;
let wired = false;

export function mountRail(context) {
  ctx = context;
  // mountRail is called again on every level change to move the cursor, so the
  // language listener has to be attached once rather than once per call.
  if (!wired) {
    wired = true;
    window.addEventListener('langchange', render);
  }
  render();
}

/** A level is reachable when the one before it is done. */
export function levelState(chapter, index) {
  const id = chapter.levelData[index].id;
  const s = levelStatus(id);
  if (s === 'cleared' || s === 'skipped') return s;
  if (index === 0) return 'available';
  return isDone(chapter.levelData[index - 1].id) ? 'available' : 'locked';
}

export function allLevelsDone(chapter) {
  return chapter.levelData.every((l) => isDone(l.id));
}

function dot(state) {
  const d = el('span.dot');
  if (state === 'cleared') {
    d.classList.add('has-icon');
    d.append(svg('check'));
  } else if (state === 'skipped') {
    d.classList.add('has-icon');
    d.append(svg('wand'));
  } else if (state === 'locked') {
    d.classList.add('has-icon');
    d.append(svg('lock'));
  }
  return d;
}

export function render() {
  if (!ctx) return;
  const root = clear($('#chapters'));

  let cleared = 0;
  let total = 0;

  for (const chapter of ctx.chapters) {
    total += chapter.levelData.length;
    cleared += chapter.levelData.filter((l) => isDone(l.id)).length;

    const open = ctx.openChapters.has(chapter.id);
    const box = el('div.chapter', { 'data-open': String(open) });
    const num = chapter.id.replace(/\D/g, '');

    box.append(
      el(
        'button.chapter-head',
        {
          type: 'button',
          title: `${num} · ${L(chapter.title)}`,
          onclick: () => {
            if (open) ctx.openChapters.delete(chapter.id);
            else ctx.openChapters.add(chapter.id);
            render();
          },
        },
        svg('chevron', 'icon chev'),
        el('span.num', null, num),
        el('span.name', null, L(chapter.title)),
      ),
    );

    const list = el('ul.levels');
    chapter.levelData.forEach((level, i) => {
      const state = levelState(chapter, i);
      const label = `${i + 1}. ${L(level.title)}`;
      list.append(
        el(
          'li',
          null,
          el(
            'button.level-btn',
            {
              type: 'button',
              'data-state': state,
              'aria-current': String(ctx.currentLevelId === level.id),
              title: `${label} — ${t('level.' + state)}`,
              disabled: state === 'locked',
              onclick: () => ctx.onOpenLevel(chapter, level.id),
            },
            dot(state),
            el('span.label', null, label),
          ),
        ),
      );
    });

    // The quiz and the closing panel sit in the same list as the levels: they
    // are steps in the chapter, not chrome hanging off the side of it.
    const quizOpen = allLevelsDone(chapter);
    const quizDone = !!progress.quiz[chapter.id];
    list.append(
      el(
        'li',
        null,
        el(
          'button.level-btn.quiz-btn',
          {
            type: 'button',
            'data-state': quizOpen ? (quizDone ? 'cleared' : 'available') : 'locked',
            title: quizOpen ? t('rail.quiz') : t('rail.lockedHint'),
            disabled: !quizOpen,
            onclick: () => ctx.onOpenQuiz(chapter),
          },
          quizOpen && quizDone ? dot('cleared') : quizOpen ? dot('available') : dot('locked'),
          el('span.label', null, t('rail.quiz')),
        ),
      ),
      el(
        'li',
        null,
        el(
          'button.level-btn.quiz-btn',
          {
            type: 'button',
            'data-state': quizOpen ? 'available' : 'locked',
            title: quizOpen ? t('rail.closing') : t('rail.lockedHint'),
            disabled: !quizOpen,
            onclick: () => ctx.onOpenClosing(chapter),
          },
          dot(quizOpen ? 'available' : 'locked'),
          el('span.label', null, t('rail.closing')),
        ),
      ),
    );

    box.append(list);
    root.append(box);
  }

  const pct = total ? Math.round((cleared / total) * 100) : 0;
  $('#progress-text').textContent = t('app.progress');
  $('#progress-count').textContent = `${cleared} / ${total}`;
  $('#progress-fill').style.width = `${pct}%`;
  $('#progress-fill-mini').style.height = `${pct}%`;
}
