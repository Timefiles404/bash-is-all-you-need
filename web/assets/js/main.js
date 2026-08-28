// Boot and wiring. Everything with an opinion lives in the module it belongs
// to; this file loads them in the right order and connects the buttons and the
// keyboard to them.

import { $, MOD } from './dom.js';
import { RUNTIME_MODULE, CHAPTERS } from './config.js';
import { t, toggleLang } from './i18n.js';
import { app } from './app.js';
import { loadChapter } from './content.js';
import { loadCodeMirror, createEditor } from './editor.js';
import { loadXterm } from './terminal.js';
import { chooserKey } from './chooser.js';
import { mountFileTree, renderFileTree } from './filetree.js';
import { renderRuntimeStatus } from './statusstrip.js';
import {
  buildTerminals,
  switchTab,
  fitTerminals,
  banner,
  fatal,
  labelButtons,
  applyRail,
  toggleRailPane,
  updateStatus,
} from './panes.js';
import { openLevel, openFile, onEditorChange, onHoleClick, remountRail, jumpTo } from './level.js';
import { mountReading, toggleReading } from './reading.js';
import { run, stop, save, format, runCheck, reveal, openQuiz } from './runner.js';
import * as ov from './overlays.js';
import { showDiff } from './diff.js';
import { installKeys } from './keys.js';
import { session, progress, resetAll, isDone } from './state.js';

async function boot() {
  try {
    const mod = await import(/* @vite-ignore */ RUNTIME_MODULE);
    // The real runtime exports `Runtime` and also defaults it; accepting either
    // means the switch in config.js is the only thing that has to change.
    app.Runtime = mod.Runtime ?? mod.default ?? mod;
  } catch (err) {
    return fatal(t('toast.runtimeFail', { msg: String(err.message || err) }));
  }

  const [cmReady, xtReady] = await Promise.all([loadCodeMirror(), loadXterm()]);
  const missing = [!cmReady && 'CodeMirror 6', !xtReady && 'xterm.js'].filter(Boolean);
  if (missing.length) banner(t('cdn.degraded', { what: missing.join(' + ') }));

  buildTerminals();
  app.editor = createEditor($('#editor-host'), { onChange: onEditorChange, onHoleClick });

  app.Runtime.on?.('status', renderRuntimeStatus);
  renderRuntimeStatus(app.Runtime.status?.() || {});

  let chapter;
  try {
    chapter = await loadChapter(CHAPTERS[0]);
  } catch (err) {
    return fatal(t('content.fail', { msg: String(err.message || err) }));
  }
  app.chapter = chapter;
  for (const id of CHAPTERS) app.openChapters.add(id);

  remountRail();
  mountFileTree(openFile);
  // Before openLevel, which is what asks the pane to show a level's material.
  mountReading({ onXref: jumpTo });
  wireButtons();
  wireKeys();
  applyRail(progress.railCollapsed);
  window.addEventListener('langchange', () => {
    labelButtons();
    updateStatus();
    renderFileTree();
  });

  // Come back to where the learner was, unless that level is gone; otherwise
  // start at the first one they have not finished.
  const firstUnfinished = chapter.levelData.find((l) => !isDone(l.id)) || chapter.levelData[0];
  const wanted =
    progress.last?.chapter === chapter.id && chapter.byId.has(progress.last.level)
      ? progress.last.level
      : firstUnfinished.id;

  await app.Runtime.init({ files: {}, cwd: '/sandbox' });
  await openLevel(chapter, wanted);

  new ResizeObserver(fitTerminals).observe($('#term-host'));
}

function wireButtons() {
  labelButtons();
  $('#btn-rail').onclick = toggleRailPane;
  $('#btn-format').onclick = format;
  $('#btn-save').onclick = save;
  $('#btn-reveal').onclick = reveal;
  $('#btn-help').onclick = ov.showHelp;
  $('#btn-run').onclick = run;
  $('#btn-stop').onclick = stop;
  $('#btn-clear').onclick = () => {
    const term = app.tab === 'run' ? app.runTerm : app.shellTerm;
    term.clear();
    if (app.tab === 'shell') term.prompt();
  };
  $('#tab-run').onclick = () => switchTab('run');
  $('#tab-shell').onclick = () => switchTab('shell');
}

/** The palette is the discoverable list of everything the toolbar and the
 *  keyboard can do, plus the few things neither has room for. */
function commands() {
  return [
    { id: 'run', label: t('cmd.run'), combo: `${MOD}+Enter`, icon: 'play', run },
    { id: 'stop', label: t('cmd.stop'), combo: 'Esc', icon: 'stop', run: stop },
    { id: 'save', label: t('cmd.save'), combo: `${MOD}+S`, icon: 'save', run: save },
    { id: 'format', label: t('cmd.format'), combo: `${MOD}+Shift+F`, icon: 'wand', run: format },
    { id: 'check', label: t('cmd.check'), icon: 'check', run: runCheck },
    { id: 'rail', label: t('cmd.rail'), combo: `${MOD}+B`, icon: 'panel', run: toggleRailPane },
    {
      id: 'reading',
      label: t('cmd.reading'),
      combo: `${MOD}+E`,
      icon: 'read',
      run: toggleReading,
    },
    { id: 'help', label: t('cmd.help'), combo: '?', icon: 'keyboard', run: ov.showHelp },
    { id: 'reveal', label: t('cmd.reveal'), icon: 'book', run: reveal },
    {
      id: 'diff',
      label: t('cmd.diff'),
      icon: 'file',
      run: () => showDiff(app.level, session.openPath),
    },
    { id: 'quiz', label: t('cmd.quiz'), icon: 'quiz', run: openQuiz },
    { id: 'closing', label: t('cmd.closing'), icon: 'book', run: () => ov.showClosing(app.chapter) },
    { id: 'lang', label: t('cmd.lang'), icon: 'dot', run: toggleLang },
    {
      id: 'reset',
      label: t('cmd.reset'),
      icon: 'eraser',
      run: () => {
        resetAll();
        ov.toast(t('toast.reset'), 'warn');
        openLevel(app.chapter, app.chapter.levelData[0].id);
      },
    },
  ];
}

function wireKeys() {
  installKeys({
    chooserKey,
    closeOverlay: () => ov.closeTopOverlay(),
    stop,
    help: ov.showHelp,
    save,
    run,
    format,
    rail: toggleRailPane,
    reading: toggleReading,
    palette: () => ov.showPalette(commands()),
  });
}

boot();
