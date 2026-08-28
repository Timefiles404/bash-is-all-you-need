// The chrome: the two terminals, the tab switch, the toolbar labels, the rail
// toggle and the status strip. Nothing here decides anything about a level; it
// only reflects what the level modules did.

import { $, el, MOD } from './dom.js';
import { t, L } from './i18n.js';
import { svg } from './icons.js';
import { app } from './app.js';
import { createTerm } from './terminal.js';
import { renderStatusStrip } from './statusstrip.js';
import { verdicts } from './holes.js';
import { session, choices as choicesFor, setRail } from './state.js';

/* ---------------------------------------------------------------- terminals */

export function buildTerminals() {
  const host = $('#term-host');
  const runView = el('div.term-view', { id: 'view-run' });
  const shellView = el('div.term-view', { id: 'view-shell' });
  host.append(runView, shellView);

  // Both are created while their boxes have real dimensions. xterm measures the
  // character cell when it opens, and it measures nothing useful inside a
  // display:none box.
  app.runTerm = createTerm(runView, {});
  app.shellTerm = createTerm(shellView, {
    interactive: true,
    promptText: () => `${app.Runtime.shell?.cwd?.() ?? '/'} $`,
    onLine: onShellLine,
    onInterrupt: () => {},
  });
  app.shellTerm.writeln(t('shell.hint'), 'info');
  app.shellTerm.prompt();
  switchTab('run', false);
}

export function switchTab(next, focus = true) {
  app.tab = next;
  $('#tab-run').setAttribute('aria-selected', String(next === 'run'));
  $('#tab-shell').setAttribute('aria-selected', String(next === 'shell'));
  $('#view-run').style.display = next === 'run' ? '' : 'none';
  $('#view-shell').style.display = next === 'shell' ? '' : 'none';
  const term = next === 'run' ? app.runTerm : app.shellTerm;
  term.fit();
  if (focus) term.focus();
}

export function fitTerminals() {
  app.runTerm?.fit();
  app.shellTerm?.fit();
}

async function onShellLine(line) {
  const term = app.shellTerm;
  const cmd = line.trim();
  if (!cmd) return term.prompt();
  if (cmd === 'clear') {
    term.clear();
    return term.prompt();
  }
  term.busy = true;
  try {
    const { code } = await app.Runtime.shell.exec(cmd, {
      onStdout: (s) => term.write(s),
      onStderr: (s) => term.write(s, 'err'),
    });
    if (code) term.writeln(`[exit ${code}]`, 'info');
  } catch (err) {
    term.writeln(String(err.message || err), 'err');
  }
  term.busy = false;
  term.prompt();
}

/* ------------------------------------------------------------------- banner */

export function banner(html) {
  const b = $('#banner');
  b.innerHTML = html;
  b.className = 'on';
  b.title = t('btn.close');
  b.onclick = () => {
    b.className = '';
  };
}

export function fatal(msg) {
  banner(msg);
  $('#editor-host').append(el('div', { style: 'padding:24px;color:var(--fg-1)' }, msg));
}

/* ---------------------------------------------------------------- the strip */

export function updateStatus() {
  renderStatusStrip({
    check: app.check,
    dirty: !!session.files[session.openPath]?.dirty,
    holes: app.level ? verdicts(app.level.holes, choicesFor(app.level.id)) : { total: 0 },
    objective: app.level ? L(app.level.objective) : '',
  });
  syncRunButtons();
}

export function syncRunButtons() {
  $('#btn-run').disabled = !!app.running;
  $('#btn-stop').disabled = !app.running;
}

/* ------------------------------------------------------------ rail + labels */

export function applyRail(collapsed) {
  $('#app').classList.toggle('rail-collapsed', collapsed);
  setRail(collapsed);
  labelButtons();
  // The grid animates for --dur; fitting before it settles measures the old
  // width and leaves the terminal a column short.
  setTimeout(fitTerminals, 200);
}

export function toggleRailPane() {
  applyRail(!$('#app').classList.contains('rail-collapsed'));
}

function tip(id, key, combo) {
  const b = $(id);
  b.title = combo ? `${t(key)} · ${combo}` : t(key);
  return b;
}

export function labelButtons() {
  const collapsed = $('#app').classList.contains('rail-collapsed');
  tip('#btn-rail', collapsed ? 'btn.railExpand' : 'btn.rail', `${MOD}+B`);
  tip('#btn-format', 'btn.format', `${MOD}+Shift+F`);
  tip('#btn-save', 'btn.save', `${MOD}+S`);
  tip('#btn-reveal', 'btn.reveal');
  tip('#btn-help', 'btn.help', '?');
  tip('#btn-run', 'btn.run', `${MOD}+Enter`);
  tip('#btn-stop', 'btn.stop', 'Esc');
  tip('#btn-clear', 'btn.clear');

  const text = (s) => document.createTextNode(s);
  $('#btn-format').replaceChildren(svg('wand'), text(t('btn.format')));
  $('#btn-save').replaceChildren(svg('save'), text(t('btn.save')));
  $('#btn-reveal').replaceChildren(svg('book'), text(t('btn.reveal')));
  $('#btn-run').replaceChildren(svg('play'), text(t('btn.run')));
  $('#btn-rail').replaceChildren(svg('panel'));
  $('#btn-help').replaceChildren(svg('keyboard'));
  $('#btn-stop').replaceChildren(svg('stop'));
  $('#btn-clear').replaceChildren(svg('eraser'));
  $('#tab-run').textContent = t('tab.run');
  $('#tab-shell').textContent = t('tab.shell');
}
