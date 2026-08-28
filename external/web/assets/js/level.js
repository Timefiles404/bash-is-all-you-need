// Opening a level, opening a file inside it, and the hole mechanic.
//
// This module and runner.js refer to each other — a cleared level opens the
// next one, and opening a level schedules a check. Both only declare functions,
// so the cycle resolves before anything runs.

import { $, el } from './dom.js';
import { t, L } from './i18n.js';
import { app } from './app.js';
import { renderFile, holeOrder, shiftRanges } from './holes.js';
import { openChooser, closeChooser, forgetChooserMemory } from './chooser.js';
import { mountRail } from './rail.js';
import { renderFileTree } from './filetree.js';
import { updateStatus } from './panes.js';
import { scheduleCheck, refreshFixture, openQuiz } from './runner.js';
import { showReading } from './reading.js';
import * as ov from './overlays.js';
import { session, choices as choicesFor, setChoice, setLast } from './state.js';

export async function openLevel(chapter, levelId) {
  const level = chapter.byId.get(levelId);
  if (!level) return;
  app.chapter = chapter;
  app.level = level;

  session.chapter = chapter.id;
  session.level = levelId;
  session.files = {};
  session.revealedThisLevel = false;
  forgetChooserMemory();
  closeChooser();

  const picks = choicesFor(level.id);
  for (const spec of level.files) {
    const { text, ranges } = renderFile(spec.template, level.holes, picks);
    session.files[spec.path] = {
      text,
      ranges,
      dirty: false,
      readonly: !!spec.readonly,
      template: spec.template,
      order: holeOrder(spec.template),
    };
  }

  await seedRuntimeFS(level);
  refreshFixture();
  setLast(chapter.id, levelId);
  remountRail();
  // Not awaited: the material is a fetch of a text file, and the editor should
  // not wait behind it to show the code.
  showReading(chapter.id, level);
  openFile(level.entry || level.files[0].path);

  app.runTerm.clear();
  app.runBuffer = '';
  app.runTerm.writeln(`${L(chapter.title)} · ${L(level.title)}`, 'info');
  for (const line of L(level.brief).split('\n')) app.runTerm.writeln(line, 'info');
  app.runTerm.writeln('', 'info');

  app.check = { state: 'idle', count: 0 };
  updateStatus();
  // Checked on open, deliberately: the unfilled blanks are real errors and the
  // strip should say so before the learner discovers it by pressing run.
  scheduleCheck();
}

export function remountRail() {
  mountRail({
    chapters: [app.chapter],
    openChapters: app.openChapters,
    currentLevelId: app.level?.id ?? null,
    onOpenLevel: (ch, id) => openLevel(ch, id),
    onOpenQuiz: () => openQuiz(),
    onOpenClosing: () => ov.showClosing(app.chapter),
  });
}

/**
 * Replace the sandbox with this level's files.
 *
 * `fs.mount` is the one that is actually correct: it drops what the previous
 * level left behind, so `ls` in the shell matches the file list. Writing each
 * file is the fallback for a runtime without it, and it leaks stale files.
 */
async function seedRuntimeFS(level) {
  const files = {};
  for (const [path, f] of Object.entries(session.files)) files[path] = f.text;
  const rt = app.Runtime;
  await rt.setLevel?.(level.id);
  if (rt.fs?.mount) await rt.fs.mount(files, '/sandbox');
  else await Promise.all(Object.entries(files).map(([p, txt]) => rt.fs.write(p, txt)));
}

export function fileMap() {
  const map = {};
  for (const [path, f] of Object.entries(session.files)) map[path] = f.text;
  return map;
}

/* --------------------------------------------------------------------- file */

export function openFile(path) {
  const f = session.files[path];
  if (!f) return;
  session.openPath = path;
  $('#open-file-name').textContent = path;
  app.loadingDoc = true;
  app.editor.setDoc(f.text, f.ranges, { readonly: f.readonly });
  app.loadingDoc = false;
  renderFileTree();
  renderHoleList();
  updateStatus();
}

/**
 * Follow a cross-reference from the reading pane.
 *
 * The hole case deliberately reads the ranges back out of the editor rather
 * than out of `session`: after a pick or a format pass the editor is the one
 * that knows where the hole ended up, and pointing at a stale offset is worse
 * than not pointing at all.
 */
export function jumpTo(ref) {
  if (!ref) return;

  if (ref.kind === 'file') return openFile(ref.path);

  if (ref.kind === 'hole') {
    const path = Object.keys(session.files).find((p) => session.files[p].order.includes(ref.id));
    if (!path) return;
    if (path !== session.openPath) openFile(path);
    const r = app.editor.getRanges().find((x) => x.id === ref.id);
    if (r) app.editor.flash(r.from, r.to);
    return;
  }

  if (ref.kind === 'line' && Number.isFinite(ref.n)) {
    const path = ref.path || session.openPath;
    if (!session.files[path]) return;
    if (path !== session.openPath) openFile(path);
    app.editor.flashLine(ref.n);
  }
}

export function onEditorChange(text) {
  if (app.loadingDoc) return;
  const f = session.files[session.openPath];
  if (!f) return;
  f.text = text;
  f.ranges = app.editor.getRanges();
  if (!f.readonly) f.dirty = true;
  renderFileTree();
  updateStatus();
  scheduleCheck();
}

/* -------------------------------------------------------------------- holes */

export function onHoleClick(holeId, rect) {
  const hole = app.level?.holes.find((h) => h.id === holeId);
  if (!hole) return;
  openChooser({
    hole,
    rect,
    chosen: choicesFor(app.level.id)[holeId],
    onPick: (optionId) => pick(holeId, optionId),
  });
}

function pick(holeId, optionId) {
  const { level, editor } = app;
  const f = session.files[session.openPath];
  const hole = level.holes.find((h) => h.id === holeId);
  const opt = hole?.options.find((o) => o.id === optionId);
  const ranges = editor.getRanges();
  const i = ranges.findIndex((r) => r.id === holeId);
  if (i < 0 || !opt) return;

  const next = shiftRanges(ranges, i, opt.text.length, optionId, !!opt.correct);
  editor.replaceRange(ranges[i].from, ranges[i].to, opt.text, next);
  f.text = editor.getText();
  f.ranges = next;
  f.dirty = true;

  setChoice(level.id, holeId, optionId);
  refreshFixture();
  renderHoleList();
  renderFileTree();
  updateStatus();
  scheduleCheck();
  ov.toast(opt.correct ? t('toast.right') : t('toast.wrong'), opt.correct ? 'ok' : 'warn');
}

/** Redraw every file from the template and the recorded picks. Used by reveal,
 *  which changes several holes across several files at once. */
export function rerenderFiles() {
  const picks = choicesFor(app.level.id);
  for (const f of Object.values(session.files)) {
    const re = renderFile(f.template, app.level.holes, picks);
    f.text = re.text;
    f.ranges = re.ranges;
    f.dirty = true;
  }
  openFile(session.openPath);
}

/** With no CodeMirror there is nothing to click in the text, so the holes get
 *  their own row of buttons under the box. */
export function renderHoleList() {
  const root = $('#hole-fallback');
  root.replaceChildren();
  if (!app.editor?.degraded || !app.level) return;
  const f = session.files[session.openPath];
  if (!f?.ranges.length) return;

  const picks = choicesFor(app.level.id);
  for (const r of f.ranges) {
    const hole = app.level.holes.find((h) => h.id === r.id);
    const opt = hole?.options.find((o) => o.id === picks[r.id]);
    const state = !opt ? '' : opt.correct ? 'state-ok' : 'state-warn';
    root.append(
      el(
        'button.btn',
        {
          type: 'button',
          style: 'margin:2px 4px 2px 0',
          onclick: (e) => onHoleClick(r.id, e.currentTarget.getBoundingClientRect()),
        },
        el('span.mono', null, `[[${r.id}]]`),
        el('span', { class: state }, L(hole?.label)),
      ),
    );
  }
}
