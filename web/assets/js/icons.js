// Inline SVG, one 16-unit box, 1.5 stroke, no fills. Kept in one file so the
// stroke weight cannot drift between panes.

const PATHS = {
  chevron: '<path d="M6 3.5 10.5 8 6 12.5"/>',
  check: '<path d="M3.5 8.5 6.5 11.5 12.5 4.5"/>',
  lock:
    '<rect x="3.5" y="7" width="9" height="6.5" rx="1.2"/><path d="M5.75 7V5.25a2.25 2.25 0 0 1 4.5 0V7"/>',
  play: '<path d="M5 3.6 12.4 8 5 12.4Z"/>',
  stop: '<rect x="4.5" y="4.5" width="7" height="7" rx="1"/>',
  eraser: '<path d="M2.5 12.5h11"/><path d="M4.5 10.2 9 5.7l3.2 3.2-3.3 3.3H6z"/>',
  file: '<path d="M4 2.5h4.5L12 6v7.5H4z"/><path d="M8.5 2.5V6H12"/>',
  panel: '<rect x="2.5" y="3" width="11" height="10" rx="1.2"/><path d="M6.5 3v10"/>',
  wand: '<path d="M3.5 12.5 11 5"/><path d="M9.5 3.5 10 5l1.5.5L10 6l-.5 1.5L9 6l-1.5-.5L9 5z"/>',
  keyboard:
    '<rect x="1.8" y="4" width="12.4" height="8" rx="1.2"/><path d="M4.4 6.6h.01M7 6.6h.01M9.6 6.6h.01M12 6.6h.01M4.4 9.4h7.2"/>',
  save: '<path d="M3.5 2.5h7.2L13 4.8v8.7H3.5z"/><path d="M5.8 2.5v3.6h4.4V2.5"/><rect x="5.5" y="9" width="5" height="4.5"/>',
  arrowLeft: '<path d="M9.5 3.5 5 8l4.5 4.5"/>',
  arrowRight: '<path d="M6.5 3.5 11 8l-4.5 4.5"/>',
  close: '<path d="M4 4l8 8M12 4l-8 8"/>',
  book: '<path d="M2.5 3.2h4A2 2 0 0 1 8 4.7v8a1.6 1.6 0 0 0-1.3-.8H2.5z"/><path d="M13.5 3.2h-4A2 2 0 0 0 8 4.7v8a1.6 1.6 0 0 1 1.3-.8h4.2z"/>',
  quiz: '<circle cx="8" cy="8" r="5.5"/><path d="M6.4 6.4a1.7 1.7 0 1 1 2.2 1.9v1"/><path d="M8.6 11.2h.01"/>',
  dot: '<circle cx="8" cy="8" r="2.2"/>',
};

/** svg('check') -> an <svg class="icon"> node. */
export function svg(name, cls = 'icon') {
  const n = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  n.setAttribute('viewBox', '0 0 16 16');
  n.setAttribute('aria-hidden', 'true');
  n.setAttribute('class', cls);
  n.innerHTML = PATHS[name] || PATHS.dot;
  return n;
}

/** The level-clear mark is drawn rather than picked, because its two strokes
 *  are animated separately. */
export function clearMark() {
  const n = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  n.setAttribute('viewBox', '0 0 56 56');
  n.setAttribute('class', 'mark');
  n.innerHTML =
    '<circle cx="28" cy="28" r="24"/><path d="M18 28.5 25 35.5 38.5 22"/>';
  return n;
}
