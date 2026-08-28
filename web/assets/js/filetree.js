// The file list. The sandbox a level ships is two or three files, so this is a
// flat list under one heading rather than a tree with one branch.

import { $, el, clear } from './dom.js';
import { t } from './i18n.js';
import { svg } from './icons.js';
import { session } from './state.js';

let onOpen = null;

export function mountFileTree(handler) {
  onOpen = handler;
  window.addEventListener('langchange', renderFileTree);
}

export function renderFileTree() {
  const root = clear($('#filetree'));
  root.append(el('div.tree-group', null, t('files.title')));
  const list = el('ul.tree-list');

  for (const [path, file] of Object.entries(session.files)) {
    list.append(
      el(
        'li',
        null,
        el(
          'button.tree-item',
          {
            type: 'button',
            'aria-current': String(session.openPath === path),
            title: file.readonly ? `${path} · ${t('files.readonly')}` : path,
            onclick: () => onOpen?.(path),
          },
          svg('file'),
          el('span', null, path),
          file.dirty ? el('span.dirty') : null,
          file.readonly ? el('span.ro', null, t('files.readonly')) : null,
        ),
      ),
    );
  }

  root.append(list);
}
