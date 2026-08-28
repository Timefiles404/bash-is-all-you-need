// One keydown listener, on the capture phase.
//
// Capture matters: CodeMirror installs its own handlers on the editor, and a
// bubbling listener would only see the keys CodeMirror decided not to keep.
// Everything here either claims a key outright or gets out of the way — in
// particular Ctrl/Cmd+Z is deliberately absent, because both the CodeMirror
// history and the browser's own textarea undo already do the right thing and
// intercepting them would make undo worse.

import { MOD, IS_MAC } from './dom.js';

function typing(target) {
  if (!target) return false;
  const tag = target.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || target.isContentEditable;
}

export function installKeys(h) {
  document.addEventListener(
    'keydown',
    (e) => {
      const mod = IS_MAC ? e.metaKey : e.ctrlKey;

      // The chooser owns arrows, digits and Enter while it is open.
      if (h.chooserKey(e)) {
        e.preventDefault();
        e.stopPropagation();
        return;
      }

      if (e.key === 'Escape') {
        if (h.closeOverlay() || h.stop()) {
          e.preventDefault();
          e.stopPropagation();
        }
        return;
      }

      if (!mod) {
        // `?` opens help, but not while the learner is typing one.
        if (e.key === '?' && !typing(e.target)) {
          e.preventDefault();
          h.help();
        }
        return;
      }

      const k = e.key.toLowerCase();
      let claimed = true;
      if (k === 's' && !e.shiftKey) h.save();
      else if (k === 'enter') h.run();
      else if (k === 'f' && e.shiftKey) h.format();
      else if (k === 'b' && !e.shiftKey) h.rail();
      else if (k === 'e' && !e.shiftKey) h.reading();
      else if (k === 'k' && !e.shiftKey) h.palette();
      else claimed = false;

      if (claimed) {
        e.preventDefault();
        e.stopPropagation();
      }
    },
    true,
  );
}

export { MOD };
