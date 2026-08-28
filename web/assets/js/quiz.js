// The chapter quiz.
//
// Every question shows the right answer and the reasoning once it has been
// answered, whether or not the learner got it — a quiz that only says "wrong"
// teaches the score, not the subject. The picked wrong option keeps its own
// reason too, because the useful comparison is between the two.

import { el, clear } from './dom.js';
import { t, L } from './i18n.js';
import { openSheet, closeTopOverlay } from './overlays.js';
import { progress, setQuiz } from './state.js';

const MARKS = ['A', 'B', 'C', 'D', 'E', 'F'];

export function showQuiz(chapter, onFinish) {
  const questions = chapter.quiz || [];
  const answers = { ...(progress.quiz[chapter.id]?.answers || {}) };

  const body = el('div');
  body.append(el('p', null, t('quiz.intro')));
  const holder = el('div');
  body.append(holder);

  const scoreLine = el('span.q-score');
  const finishBtn = el(
    'button.btn.primary',
    {
      type: 'button',
      onclick: () => {
        commit();
        closeTopOverlay();
        onFinish?.(score());
      },
    },
    t('quiz.finish'),
  );
  const retryBtn = el(
    'button.btn',
    {
      type: 'button',
      onclick: () => {
        for (const k of Object.keys(answers)) delete answers[k];
        paint();
      },
    },
    t('quiz.retry'),
  );

  function score() {
    let n = 0;
    for (const q of questions) {
      const opt = q.options.find((o) => o.id === answers[q.id]);
      if (opt?.correct) n++;
    }
    return n;
  }

  function commit() {
    setQuiz(chapter.id, answers, score(), questions.length);
  }

  function paint() {
    clear(holder);
    questions.forEach((q, qi) => {
      const answered = q.id in answers;
      const box = el('div.q');
      box.append(
        el('div.stem', null, el('span.n', null, `${qi + 1}.`), L(q.stem)),
      );

      q.options.forEach((opt, oi) => {
        const picked = answers[q.id] === opt.id;
        const verdict = !answered ? null : opt.correct ? 'right' : picked ? 'wrong' : null;
        const btn = el(
          'button.q-opt',
          {
            type: 'button',
            'data-verdict': verdict,
            disabled: answered,
            onclick: () => {
              answers[q.id] = opt.id;
              paint();
            },
          },
          el('span.mk', null, MARKS[oi]),
          el('span', null, L(opt.text)),
        );
        box.append(btn);
        // The reason is shown for the right answer always, and for the wrong
        // one the learner actually chose. The other distractors stay quiet so
        // the panel does not turn into four paragraphs.
        if (answered && (opt.correct || picked)) {
          box.append(el('div.q-why', null, L(opt.why)));
        }
      });

      if (answered) {
        const right = q.options.findIndex((o) => o.correct);
        box.append(
          el(
            'div.q-why.muted',
            null,
            t('quiz.correctIs', { mk: MARKS[right] }),
          ),
        );
      }
      holder.append(box);
    });

    const answeredCount = questions.filter((q) => q.id in answers).length;
    const done = answeredCount === questions.length;
    scoreLine.textContent = done
      ? t('quiz.score', { n: score(), m: questions.length })
      : t('quiz.answered', { n: answeredCount, m: questions.length });
    finishBtn.disabled = !done;
    retryBtn.disabled = answeredCount === 0;
  }

  paint();

  openSheet({
    title: `${t('quiz.title')} — ${L(chapter.title)}`,
    body,
    foot: [scoreLine, el('span', { style: 'flex:1 1 auto' }), retryBtn, finishBtn],
    onClose: commit,
  });
}
