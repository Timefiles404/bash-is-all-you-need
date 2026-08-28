// A Markdown renderer, small enough to read in one sitting.
//
// It covers exactly what the reading material uses: headings, paragraphs,
// lists, blockquotes, pipe tables, horizontal rules, fenced code, inline code,
// emphasis and links. Anything else is left as text rather than guessed at.
//
// It emits an HTML string. Every piece of the source is escaped before it
// reaches that string, and the only tags in the output are the ones written
// here, so the caller can hand the result to innerHTML. Raw HTML in the source
// is escaped too — the material has no reason to contain any, and letting it
// through would mean auditing content files as if they were code.
//
// Code blocks come out as `<pre data-lang="...">`; reading.js is what turns a
// Go one into a highlighted view, because that is the editor's job and this
// file knows nothing about the editor.

const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' };

function esc(s) {
  return String(s).replace(/[&<>"]/g, (c) => ESCAPES[c]);
}

/* --------------------------------------------------------------- inline */

// A code span must not have emphasis or links found inside it, so each one is
// lifted out first and put back last. NUL is the placeholder because a text
// file the fetch layer decoded as UTF-8 will not contain one.
const HOLD = '\u0000';

function inline(src) {
  const spans = [];
  let s = String(src).replace(/`([^`]+)`/g, (_, code) => {
    spans.push(code);
    return `${HOLD}${spans.length - 1}${HOLD}`;
  });

  s = esc(s);
  s = s.replace(/\[([^\]]*)\]\(([^)\s]+)\)/g, (_, text, href) => link(text, href));
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*\w])\*([^*\n]+)\*(?![\w*])/g, '$1<em>$2</em>');
  s = s.replace(new RegExp(`${HOLD}(\\d+)${HOLD}`, 'g'), (_, i) => `<code>${esc(spans[i])}</code>`);
  return s;
}

/**
 * `[text](#hole:1)` and friends become cross-references into the editor;
 * everything else is an ordinary link that opens elsewhere. The `#` prefix is
 * the whole rule, and it is the one piece of syntax the material has to know.
 */
function link(text, href) {
  if (href.startsWith('#')) {
    return `<a class="xref" href="${esc(href)}" data-xref="${esc(href.slice(1))}">${text}</a>`;
  }
  return `<a href="${esc(href)}" target="_blank" rel="noreferrer">${text}</a>`;
}

/* ---------------------------------------------------------------- blocks */

// ```go 00-loop/code/main.go:256-258
//
// Everything after the language is where the snippet came from, printed under
// the block. Code in the reading is quoted from the repository rather than
// retyped, and a quote whose source is not written down is the thing this
// repository keeps having to go back and fix.
const FENCE = /^```(\w*)[ \t]*(.*)$/;
const HEADING = /^(#{1,6})\s+(.*)$/;
const BULLET = /^(\s*)([-*])\s+(.*)$/;
const NUMBER = /^(\s*)(\d+)\.\s+(.*)$/;
const RULE = /^(-{3,}|\*{3,})\s*$/;
const QUOTE = /^>\s?(.*)$/;
const ROW = /^\s*\|(.+)\|\s*$/;
const DIVIDER = /^\s*\|[\s|:-]+\|\s*$/;

function isBlank(line) {
  return !line || !line.trim();
}

/** One pass over a run of lines, appending HTML for each block it recognises. */
function blocks(lines) {
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    if (isBlank(line)) {
      i++;
      continue;
    }

    const fence = line.match(FENCE);
    if (fence) {
      const body = [];
      i++;
      while (i < lines.length && !FENCE.test(lines[i])) body.push(lines[i++]);
      i++; // the closing fence, or the end of the document
      const lang = fence[1] || '';
      const src = (fence[2] || '').trim();
      out.push(
        `<pre class="md-code"${lang ? ` data-lang="${esc(lang)}"` : ''}><code>${esc(
          body.join('\n'),
        )}</code></pre>`,
      );
      if (src) out.push(`<div class="md-src">${esc(src)}</div>`);
      continue;
    }

    const heading = line.match(HEADING);
    if (heading) {
      const level = Math.min(heading[1].length + 1, 6); // h1 is the pane's own title
      out.push(`<h${level}>${inline(heading[2])}</h${level}>`);
      i++;
      continue;
    }

    if (RULE.test(line)) {
      out.push('<hr />');
      i++;
      continue;
    }

    if (QUOTE.test(line)) {
      const body = [];
      while (i < lines.length && QUOTE.test(lines[i])) body.push(lines[i++].match(QUOTE)[1]);
      out.push(`<blockquote>${blocks(body)}</blockquote>`);
      continue;
    }

    if (ROW.test(line) && i + 1 < lines.length && DIVIDER.test(lines[i + 1])) {
      const rows = [];
      while (i < lines.length && ROW.test(lines[i])) rows.push(lines[i++]);
      out.push(table(rows));
      continue;
    }

    if (BULLET.test(line) || NUMBER.test(line)) {
      const run = [];
      while (i < lines.length && !isBlank(lines[i]) && !HEADING.test(lines[i])) run.push(lines[i++]);
      out.push(list(run, 0));
      continue;
    }

    // A paragraph runs to the next blank line or to the next block that starts
    // with punctuation of its own.
    const para = [];
    while (
      i < lines.length &&
      !isBlank(lines[i]) &&
      !HEADING.test(lines[i]) &&
      !FENCE.test(lines[i]) &&
      !QUOTE.test(lines[i]) &&
      !RULE.test(lines[i]) &&
      !BULLET.test(lines[i]) &&
      !NUMBER.test(lines[i])
    ) {
      para.push(lines[i++]);
    }
    if (para.length) out.push(`<p>${inline(para.join('\n'))}</p>`);
    else i++; // a line no rule claimed; stepping over it beats looping forever
  }

  return out.join('\n');
}

/**
 * Nested lists by indentation. `depth` is how far in the current list started,
 * so a deeper line opens a sub-list and a shallower one ends this call.
 */
function list(lines, depth) {
  const first = lines[0].match(BULLET) || lines[0].match(NUMBER);
  const ordered = !lines[0].match(BULLET);
  const items = [];
  let i = 0;

  while (i < lines.length) {
    const m = lines[i].match(BULLET) || lines[i].match(NUMBER);
    if (!m) {
      // A continuation line: it belongs to the item above it.
      if (items.length) items[items.length - 1].body.push(lines[i].trim());
      i++;
      continue;
    }
    const indent = m[1].length;
    if (indent < depth) break;
    if (indent > depth) {
      const sub = [];
      while (i < lines.length) {
        const n = lines[i].match(BULLET) || lines[i].match(NUMBER);
        if (n && n[1].length < indent) break;
        sub.push(lines[i++]);
      }
      if (items.length) items[items.length - 1].sub = list(sub, indent);
      continue;
    }
    items.push({ body: [m[3]], sub: '' });
    i++;
  }

  const tag = ordered ? 'ol' : 'ul';
  const start = ordered && first[2] !== '1' ? ` start="${esc(first[2])}"` : '';
  const html = items
    .map((it) => `<li>${inline(it.body.join(' '))}${it.sub}</li>`)
    .join('');
  return `<${tag}${start}>${html}</${tag}>`;
}

function cells(row) {
  return row
    .replace(/^\s*\|/, '')
    .replace(/\|\s*$/, '')
    .split('|')
    .map((c) => c.trim());
}

function table(rows) {
  const head = cells(rows[0]);
  const align = cells(rows[1]).map((c) => {
    if (/^:-+:$/.test(c)) return 'center';
    if (/-+:$/.test(c)) return 'right';
    return '';
  });
  const at = (i) => (align[i] ? ` style="text-align:${align[i]}"` : '');

  const thead = head.map((c, i) => `<th${at(i)}>${inline(c)}</th>`).join('');
  const tbody = rows
    .slice(2)
    .map((r) => `<tr>${cells(r).map((c, i) => `<td${at(i)}>${inline(c)}</td>`).join('')}</tr>`)
    .join('');
  return `<div class="md-table"><table><thead><tr>${thead}</tr></thead><tbody>${tbody}</tbody></table></div>`;
}

/* ------------------------------------------------------------------ entry */

export function renderMarkdown(src) {
  return blocks(String(src ?? '').replace(/\r\n?/g, '\n').split('\n'));
}

/**
 * Reading files hold both languages in one file, separated by `<!--lang:xx-->`
 * markers, so the two versions of a passage stay next to each other while they
 * are being written. Text before the first marker is treated as Chinese, which
 * is what a file that predates the convention would be.
 */
export function splitLangs(text) {
  const out = { zh: '', en: '' };
  const parts = String(text).split(/^<!--\s*lang:([a-z]{2})\s*-->[ \t]*$/m);
  out.zh = (parts[0] || '').trim();
  for (let i = 1; i < parts.length; i += 2) {
    const lang = parts[i];
    const body = (parts[i + 1] || '').trim();
    out[lang] = out[lang] ? `${out[lang]}\n\n${body}` : body;
  }
  return out;
}
