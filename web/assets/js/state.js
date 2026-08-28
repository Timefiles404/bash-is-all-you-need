// Two kinds of state, kept apart on purpose.
//
// `progress` is what the learner earned: which levels are cleared, which were
// revealed, what the quiz scored. It survives a reload.
//
// `session` is what the learner is doing right now: the open file, the text in
// the buffer, which option is picked in each hole. It does not survive a
// reload, because a half-typed buffer restored out of nowhere is worse than a
// clean start — but the picks do, since re-clicking four choosers to get back
// where you were is pure tax.

const KEY = 'biayn.progress.v1';

const listeners = new Set();

function load() {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw) return JSON.parse(raw);
  } catch {
    // A corrupt or unreadable store is not worth a dialog: start clean.
  }
  return {};
}

const stored = load();

export const progress = {
  levels: stored.levels || {}, // id -> {status, choices, revealed}
  quiz: stored.quiz || {}, // chapterId -> {answers, score, total}
  railCollapsed: !!stored.railCollapsed,
  last: stored.last || null, // {chapter, level}

  // The reading pane. Collapsing is per level, because whether the material is
  // worth the width depends on the level; the divider's position is not,
  // because it is a statement about this screen.
  reading: stored.reading || {}, // levelId -> true when collapsed
  readingSize: stored.readingSize || {}, // {w, h} in px
};

let saveTimer = 0;
function persist() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(() => {
    try {
      localStorage.setItem(KEY, JSON.stringify(progress));
    } catch {
      // Private-mode storage refusals are not the learner's problem to solve.
    }
  }, 120);
}

export function onChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function changed() {
  persist();
  for (const fn of listeners) fn();
}

export function levelRecord(id) {
  return (progress.levels[id] ||= { status: 'untouched', choices: {} });
}

export function levelStatus(id) {
  return progress.levels[id]?.status || 'untouched';
}

export function isDone(id) {
  const s = levelStatus(id);
  return s === 'cleared' || s === 'skipped';
}

export function setLevelStatus(id, status) {
  const rec = levelRecord(id);
  // A cleared level does not get demoted by a later reveal: the learner already
  // did the work once.
  if (rec.status === 'cleared' && status === 'skipped') return;
  rec.status = status;
  changed();
}

export function setChoice(levelId, holeId, optionId) {
  levelRecord(levelId).choices[holeId] = optionId;
  changed();
}

export function choices(levelId) {
  return levelRecord(levelId).choices;
}

export function setQuiz(chapterId, answers, score, total) {
  progress.quiz[chapterId] = { answers, score, total };
  changed();
}

export function setRail(collapsed) {
  progress.railCollapsed = collapsed;
  changed();
}

export function setLast(chapter, level) {
  progress.last = { chapter, level };
  changed();
}

/** Absent means shown: a level the learner has never collapsed opens with its
 *  material out. */
export function readingCollapsed(levelId) {
  return !!progress.reading[levelId];
}

export function setReadingCollapsed(levelId, collapsed) {
  if (collapsed) progress.reading[levelId] = true;
  else delete progress.reading[levelId];
  changed();
}

export function readingSize() {
  return progress.readingSize;
}

export function setReadingSize(patch) {
  Object.assign(progress.readingSize, patch);
  changed();
}

export function resetAll() {
  progress.levels = {};
  progress.quiz = {};
  progress.last = null;
  changed();
}

/** The live buffer for the level currently open. Rebuilt on every level load. */
export const session = {
  chapter: null,
  level: null,
  files: {}, // path -> {text, dirty, readonly}
  openPath: null,
  lastRunOutput: '',
  runningStop: null,
  revealedThisLevel: false,
};
