// The hole model. No DOM here: a hole is a span of text plus which option is
// sitting in it, and everything the UI needs is derivable from those two.
//
// The marker in a template is `[[id]]`. It was picked because it survives both
// renderers — CodeMirror draws a chip over it, and the plain textarea fallback
// still shows something a reader recognises as a blank.

const MARKER = /\[\[(\w+)\]\]/g;

/** Ordered list of hole ids as they appear in this file's template. */
export function holeOrder(template) {
  return [...template.matchAll(MARKER)].map((m) => m[1]);
}

function pick(hole, choices) {
  const id = choices[hole.id];
  if (!id) return null;
  return hole.options.find((o) => o.id === id) || null;
}

/**
 * Substitute the chosen options into a template.
 * Returns the text plus, for every hole in this file, where it ended up.
 */
export function renderFile(template, holes, choices) {
  const byId = new Map(holes.map((h) => [h.id, h]));
  let text = '';
  let last = 0;
  const ranges = [];
  for (const m of template.matchAll(MARKER)) {
    text += template.slice(last, m.index);
    const hole = byId.get(m[1]);
    const opt = hole ? pick(hole, choices) : null;
    const body = opt ? opt.text : m[0];
    ranges.push({
      id: m[1],
      from: text.length,
      to: text.length + body.length,
      optionId: opt ? opt.id : null,
      correct: opt ? !!opt.correct : null,
    });
    text += body;
    last = m.index + m[0].length;
  }
  return { text: text + template.slice(last), ranges };
}

/**
 * Find the holes again in text that was changed from outside — a format pass,
 * say. Returns null when any hole's text is no longer findable, which is the
 * signal to fall back to a full re-render rather than draw chips in the wrong
 * places.
 */
export function locateRanges(text, holes, choices, order) {
  const byId = new Map(holes.map((h) => [h.id, h]));
  const ranges = [];
  let cursor = 0;
  for (const id of order) {
    const hole = byId.get(id);
    if (!hole) return null;
    const opt = pick(hole, choices);
    const needle = opt ? opt.text : `[[${id}]]`;
    const at = text.indexOf(needle, cursor);
    if (at < 0) return null;
    ranges.push({
      id,
      from: at,
      to: at + needle.length,
      optionId: opt ? opt.id : null,
      correct: opt ? !!opt.correct : null,
    });
    cursor = at + needle.length;
  }
  return ranges;
}

/** Ranges after one hole was replaced, without re-scanning the document. */
export function shiftRanges(ranges, index, insertLen, optionId, correct) {
  const r = ranges[index];
  const delta = insertLen - (r.to - r.from);
  return ranges.map((x, j) => {
    if (j < index) return x;
    if (j === index) return { ...x, to: r.from + insertLen, optionId, correct };
    return { ...x, from: x.from + delta, to: x.to + delta };
  });
}

export function answerKey(holes) {
  const key = {};
  for (const h of holes) {
    const right = h.options.find((o) => o.correct);
    if (right) key[h.id] = right.id;
  }
  return key;
}

export function verdicts(holes, choices) {
  let filled = 0;
  let wrong = 0;
  for (const h of holes) {
    const opt = pick(h, choices);
    if (!opt) continue;
    filled++;
    if (!opt.correct) wrong++;
  }
  return {
    total: holes.length,
    filled,
    wrong,
    allRight: filled === holes.length && wrong === 0,
  };
}

/**
 * The build-up used by the diff stepper: the empty template, then one snapshot
 * per hole with the correct option filled in, in document order.
 */
export function buildSteps(template, holes) {
  const order = holeOrder(template);
  const key = answerKey(holes);
  const steps = [{ text: renderFile(template, holes, {}).text, filled: null }];
  const choices = {};
  for (const id of order) {
    choices[id] = key[id];
    steps.push({ text: renderFile(template, holes, choices).text, filled: id });
  }
  return steps;
}
