// Everything that talks to the runtime on the learner's behalf: checking,
// saving, formatting, building, running, and deciding whether that run cleared
// the level.

import { el } from './dom.js';
import { t } from './i18n.js';
import { app } from './app.js';
import { renderFile, locateRanges, verdicts, answerKey } from './holes.js';
import { renderFileTree } from './filetree.js';
import { updateStatus, switchTab } from './panes.js';
import { render as renderRail, allLevelsDone } from './rail.js';
import { fileMap, openFile, openLevel, rerenderFiles } from './level.js';
import * as ov from './overlays.js';
import { showQuiz } from './quiz.js';
import { showDiff } from './diff.js';
import { session, choices as choicesFor, setChoice, setLevelStatus, isDone } from './state.js';

let checkTimer = 0;

/* -------------------------------------------------------------------- check */

export function scheduleCheck() {
  clearTimeout(checkTimer);
  checkTimer = setTimeout(runCheck, 500);
}

export async function runCheck() {
  const rt = app.Runtime;
  if (!rt?.check) return;
  app.check = { state: 'busy', count: 0 };
  updateStatus();
  try {
    const diags = await rt.check(fileMap());
    const errs = diags.filter((d) => d.severity === 'error');
    app.check = errs.length
      ? { state: 'error', count: errs.length, diags }
      : { state: 'ok', count: 0, diags };
  } catch (err) {
    app.check = { state: 'error', count: 1, diags: [] };
    ov.toast(t('toast.checkFail', { msg: String(err.message || err) }), 'bad');
  }
  updateStatus();
}

/* ------------------------------------------------------------- save, format */

export async function save() {
  const f = session.files[session.openPath];
  if (!f) return;
  f.text = app.editor.getText();
  try {
    await app.Runtime.fs.write(session.openPath, f.text);
  } catch {
    // A write that fails should not also clear the dirty flag: the learner
    // would be told the file is on disk when it is not.
    return ov.toast(t('toast.checkFail', { msg: session.openPath }), 'bad');
  }
  f.dirty = false;
  renderFileTree();
  updateStatus();
  ov.toast(t('toast.saved', { path: session.openPath }), 'ok');
  runCheck();
}

export async function format() {
  const f = session.files[session.openPath];
  if (!f || f.readonly) return;
  try {
    const res = await app.Runtime.format({ [session.openPath]: app.editor.getText() });
    let text = res.files[session.openPath];
    if (text == null) return;

    const picks = choicesFor(app.level.id);
    let ranges = locateRanges(text, app.level.holes, picks, f.order);
    if (!ranges) {
      // Formatting moved a hole somewhere the scan cannot follow. Rebuilding
      // from the template loses free-typed edits, and it is still much better
      // than drawing chips over the wrong code.
      const re = renderFile(f.template, app.level.holes, picks);
      text = re.text;
      ranges = re.ranges;
    }
    f.text = text;
    f.ranges = ranges;
    f.dirty = true;
    openFile(session.openPath);
    ov.toast(t('toast.formatted'), 'ok');
    scheduleCheck();
  } catch (err) {
    ov.toast(t('toast.formatFail', { msg: String(err.message || err) }), 'warn');
  }
}

/* ---------------------------------------------------------------------- run */

/**
 * Tell a mock runtime what this assembly should print.
 *
 * A real runtime compiles and needs none of this. The mock cannot, so it says
 * so in its own voice — the `[mock]` prefix exists so nobody mistakes the
 * notice for output from their program.
 */
export function refreshFixture() {
  const rt = app.Runtime;
  if (!rt.__mock || !app.level) return;
  const picks = choicesFor(app.level.id);
  const v = verdicts(app.level.holes, picks);
  if (v.allRight) {
    rt.__mock.setFixture({ stdout: app.level.run?.stdout || [], stderr: [], exit: 0 });
    return;
  }
  const bad = app.level.holes
    .filter((h) => {
      const o = h.options.find((x) => x.id === picks[h.id]);
      return o && !o.correct;
    })
    .map((h) => h.id);
  rt.__mock.setFixture({
    stdout: [],
    exit: 1,
    stderr: bad.length
      ? [t('mock.nocompile'), t('mock.blanks', { ids: bad.join(', ') })]
      : [t('mock.nocompile')],
  });
}

export async function run() {
  if (app.running) return;
  const term = app.runTerm;
  switchTab('run');
  app.runBuffer = '';
  term.writeln('$ go run .', 'info');

  const built = await app.Runtime.build(fileMap());
  const errs = (built.diagnostics || []).filter((d) => d.severity === 'error');
  app.check = errs.length
    ? { state: 'error', count: errs.length, diags: built.diagnostics }
    : { state: 'ok', count: 0, diags: built.diagnostics };
  updateStatus();

  for (const d of built.diagnostics || []) {
    term.writeln(
      `${d.file}:${d.line}:${d.col}: ${d.severity}: ${d.message}`,
      d.severity === 'error' ? 'err' : 'out',
    );
  }
  if (!built.ok) return ov.toast(t('toast.buildFail'), 'bad');

  app.running = app.Runtime.run(built.artifactId, {
    argv: app.level.run?.argv || [],
    stdin: app.level.run?.stdin || '',
    onStdout: (s) => {
      app.runBuffer += s;
      term.write(s);
    },
    onStderr: (s) => {
      app.runBuffer += s;
      term.write(s, 'err');
    },
    onExit: (code) => {
      app.running = null;
      term.writeln(`[exit ${code}]`, code === 0 ? 'info' : 'err');
      updateStatus();
      // A run the learner stopped has nothing to say about their answers.
      // Telling someone their output was wrong right after they cancelled it
      // is noise, so a cancelled run is not judged.
      if (!app.stoppedByUser) judge(code);
      app.stoppedByUser = false;
    },
  });
  updateStatus();
}

export function stop() {
  if (!app.running) return false;
  app.stoppedByUser = true;
  app.running.stop();
  app.running = null;
  updateStatus();
  ov.toast(t('toast.stopped'), 'warn');
  return true;
}

/**
 * A level clears when the assembly is the right one AND the program printed
 * what the level asks for. Both halves matter: the answer key alone would clear
 * a level the learner never ran, and the output alone would clear one they got
 * right by accident.
 */
function judge(code) {
  const level = app.level;
  const v = verdicts(level.holes, choicesFor(level.id));
  const want = level.run?.expect?.stdoutIncludes || [];
  const missing = want.filter((s) => !app.runBuffer.includes(s));

  if (!v.allRight) {
    if (v.filled === v.total) app.runTerm.writeln(t('run.wrongPicks'), 'err');
    return;
  }
  if (code !== 0 || missing.length) {
    app.runTerm.writeln(t('run.expectFail'), 'err');
    for (const s of missing) app.runTerm.writeln(t('run.expectWant', { s }), 'err');
    return;
  }
  clearLevel();
}

function clearLevel() {
  const { chapter, level } = app;
  const already = isDone(level.id);
  setLevelStatus(level.id, 'cleared');
  renderRail();
  if (already) return ov.toast(t('toast.cleared'), 'ok');

  const i = chapter.levelData.findIndex((l) => l.id === level.id);
  const next = chapter.levelData[i + 1];
  ov.showLevelClear({
    title: t('clear.title'),
    sub: next ? t('clear.sub') : t('clear.subLast'),
    actions: [
      el(
        'button.btn.primary',
        {
          type: 'button',
          onclick: () => {
            ov.closeTopOverlay();
            if (next) openLevel(chapter, next.id);
            else openQuiz();
          },
        },
        next ? t('clear.next') : t('clear.quiz'),
      ),
      el('button.btn', { type: 'button', onclick: () => ov.closeTopOverlay() }, t('clear.stay')),
    ],
  });
}

/* ------------------------------------------------------------------- reveal */

export function reveal() {
  const level = app.level;
  if (!level?.holes.length) return ov.toast(t('toast.noHoles'), 'warn');
  for (const [holeId, optionId] of Object.entries(answerKey(level.holes))) {
    setChoice(level.id, holeId, optionId);
  }
  setLevelStatus(level.id, 'skipped');
  rerenderFiles();
  refreshFixture();
  renderRail();
  ov.toast(t('toast.revealed'), 'warn');

  ov.showLevelClear({
    title: t('reveal.title'),
    sub: t('reveal.sub'),
    actions: [
      el(
        'button.btn.primary',
        {
          type: 'button',
          onclick: () => {
            ov.closeTopOverlay();
            showDiff(level, session.openPath);
          },
        },
        t('btn.diff'),
      ),
      el('button.btn', { type: 'button', onclick: () => ov.closeTopOverlay() }, t('clear.stay')),
    ],
  });
}

export function openQuiz() {
  if (!allLevelsDone(app.chapter)) return ov.toast(t('toast.locked'), 'warn');
  showQuiz(app.chapter, () => {
    renderRail();
    ov.showClosing(app.chapter);
  });
}
