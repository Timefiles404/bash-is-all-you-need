// The strip under the editor. Three facts and the objective, on one line,
// because the objective is the thing a learner loses track of first and it
// should never require opening anything to re-read.

import { $, el, clear } from './dom.js';
import { t } from './i18n.js';
import { svg } from './icons.js';

export function renderStatusStrip({ check, dirty, holes, objective }) {
  const c = clear($('#st-check'));
  if (check.state === 'busy') {
    c.className = 'seg state-busy pulsing';
    c.append(svg('dot'), t('st.checking'));
  } else if (check.state === 'ok') {
    c.className = 'seg state-ok';
    c.append(svg('check'), t('st.checkOk'));
  } else if (check.state === 'error') {
    c.className = 'seg state-bad';
    c.append(svg('close'), t('st.checkErr', { n: check.count }));
  } else {
    c.className = 'seg muted';
    c.append(svg('dot'), t('st.checkIdle'));
  }

  const d = clear($('#st-dirty'));
  d.className = dirty ? 'seg state-warn' : 'seg muted';
  d.textContent = dirty ? t('st.dirty') : t('st.saved');

  const h = clear($('#st-holes'));
  if (!holes.total) {
    h.className = 'seg muted';
    h.textContent = '';
  } else if (holes.filled < holes.total) {
    h.className = 'seg';
    h.textContent = t('st.holes', { n: holes.total - holes.filled });
  } else if (holes.wrong) {
    h.className = 'seg state-warn';
    h.textContent = t('st.holesWrong', { n: holes.wrong });
  } else {
    h.className = 'seg state-ok';
    h.textContent = t('st.holesDone');
  }

  const o = $('#st-objective');
  o.textContent = objective ? `${t('st.objective')}: ${objective}` : '';
  o.title = o.textContent;
}

export function renderRuntimeStatus(status) {
  const root = clear($('#rt-status'));
  for (const key of ['compiler', 'shell', 'llm']) {
    const v = status[key] || 'idle';
    root.append(
      el(
        'span.seg',
        {
          class:
            'seg ' +
            (v === 'ready' ? 'state-ok' : v === 'unavailable' ? 'muted' : 'state-busy pulsing'),
          style: 'display:inline-flex;gap:4px;align-items:center;margin-right:12px',
        },
        el('span', null, t('rt.' + key)),
        el('span', null, t('rt.' + v)),
      ),
    );
  }
}
