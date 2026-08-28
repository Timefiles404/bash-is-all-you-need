// The introduction's four diagrams, hand-authored.
//
// No Mermaid, no diagram library, no network. Each one draws a mechanism the
// prose next to it cannot draw: the loop as an actual loop, one request's
// payload with the pairing that holds it together, the bill as an area rather
// than a sentence, and the shape of the course including the branch that is
// not on the trunk.
//
// Geometry and label live together on purpose. A label's {zh, en} pair is
// positioned by hand against the arrow it names, so moving it into a string
// table would separate two things that have to be edited at the same time —
// and i18n.js is not this file's to touch anyway.
//
// Every stroke is 1.5 to match the interface icons; every colour is a class
// defined in intro.css, so tokens.css stays the only place a colour is chosen.

import { L } from '../i18n.js';

let seq = 0;

/** A {zh, en} pair, rendered through the site's own fallback. */
const P = (zh, en = '') => L({ zh, en });

const esc = (s) =>
  String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

/**
 * Build the <svg>. innerHTML on an SVG element parses in the SVG namespace,
 * which is what icons.js relies on too.
 *
 * `title` is the accessible name twice over: as <title> for the a11y tree and
 * as aria-label, because a <title> deep in a shadowed subtree is not reliably
 * announced on its own.
 */
function figure({ viewBox, minWidth, title, desc, body }) {
  const uid = `if${++seq}`;
  const n = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  n.setAttribute('viewBox', viewBox);
  n.setAttribute('role', 'img');
  n.setAttribute('aria-label', title);
  n.setAttribute('class', 'fg');
  n.style.minWidth = `${minWidth}px`;
  n.innerHTML =
    `<title>${esc(title)}</title>` +
    (desc ? `<desc>${esc(desc)}</desc>` : '') +
    defs(uid) +
    body(uid);
  return n;
}

/** Two arrowheads: one for the outside world, one for the cycle. */
function defs(uid) {
  const head = (id, cls) =>
    `<marker id="${id}" viewBox="0 0 10 10" refX="7.6" refY="5"
       markerWidth="5.2" markerHeight="5.2" orient="auto-start-reverse">
       <path d="M1.4 1.6 L8.4 5 L1.4 8.4" class="${cls}"/>
     </marker>`;
  return `<defs>${head(uid + '-g', 'fg-head')}${head(uid + '-a', 'fg-head-a')}</defs>`;
}

/** A labelled box. `sub` is the mono second line, and may be omitted. */
function box(x, y, w, h, label, sub, cls = 'fg-box') {
  const cx = x + w / 2;
  const cy = y + h / 2;
  const main = sub ? cy - 3 : cy + 5;
  return (
    `<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="6" class="${cls}"/>` +
    `<text x="${cx}" y="${main}" class="fg-t" text-anchor="middle">${esc(label)}</text>` +
    (sub
      ? `<text x="${cx}" y="${cy + 15}" class="fg-m" text-anchor="middle">${esc(sub)}</text>`
      : '')
  );
}

const path = (d, cls, marker) =>
  `<path d="${d}" class="${cls}"${marker ? ` marker-end="url(#${marker})"` : ''}/>`;

const text = (x, y, s, cls, anchor = 'start') =>
  `<text x="${x}" y="${y}" class="${cls}" text-anchor="${anchor}">${esc(s)}</text>`;

/* ------------------------------------------------------------------ 1. loop */
//
// Drawn as a rectangle traversed clockwise, because that is what it is: four
// steps that hand off to each other and come back. The human sits outside it
// on the left — one arrow in, one arrow out — so the picture also says the
// thing the prose has to spend a paragraph on, that the model is one node on
// the ring rather than the thing doing the looping.

function loop() {
  return figure({
    viewBox: '0 0 780 252',
    minWidth: 620,
    title: P(
      '循环示意图：用户提问进入模型；模型若要求工具调用，命令交给 shell 执行，结果追加回消息数组再回到模型，如此循环；模型不再要求工具调用时，输出最终回答给用户。',
      'The loop: a user question goes to the model; if the model asks for a tool call the command runs in a shell, the result is appended to the message array and goes back to the model, and round again; when the model asks for no tool call, the final answer goes to the user.',
    ),
    body: (u) =>
      [
        // the outside world
        box(24, 36, 176, 56, P('用户提问', 'user asks'), P('role: user', 'role: user')),
        box(24, 176, 176, 56, P('最终回答', 'final answer'), P('打给用户看', 'printed')),
        // the cycle
        box(274, 36, 192, 56, P('模型', 'model'), P('POST 整个数组', 'POST whole array'), 'fg-box fg-box-key'),
        box(540, 36, 216, 56, P('工具调用', 'tool call'), P('bash: <命令>', 'bash: <command>')),
        box(540, 176, 216, 56, P('执行', 'execute'), P('shell', 'shell')),
        box(274, 176, 192, 56, P('结果追加进数组', 'result appended'), P('role: tool', 'role: tool')),

        path('M 200,64 H 267', 'fg-line', u + '-g'),
        path('M 466,64 H 533', 'fg-flow', u + '-a'),
        path('M 648,92 V 169', 'fg-flow', u + '-a'),
        path('M 534,204 H 473', 'fg-flow', u + '-a'),
        path('M 370,172 V 99', 'fg-flow', u + '-a'),
        path('M 320,92 C 320,142 268,204 209,204', 'fg-line', u + '-g'),

        text(500, 26, P('有 tool_calls', 'has tool_calls'), 'fg-n fg-n-a', 'middle'),
        text(302, 130, P('没有 tool_calls', 'no tool_calls'), 'fg-n', 'end'),
      ].join(''),
  });
}

/* --------------------------------------------------------------- 2. payload */
//
// The point of drawing this is the two brackets. Prose can say "every tool
// call must get a result"; only the picture shows that the pairing is between
// two adjacent messages in a flat array, and that the array is what gets
// re-sent — there is nowhere else for the agent's memory to be.

function payload() {
  const rows = [
    ['system', P('系统提示 + 工具定义', 'system prompt + tool definitions')],
    ['user', P('用户的问题', "the user's question")],
    ['assistant', P('tool_calls: bash', 'tool_calls: bash')],
    ['tool', P('命令的 stdout + stderr', 'stdout + stderr of the command')],
    ['assistant', P('tool_calls: bash', 'tool_calls: bash')],
    ['tool', P('命令的 stdout + stderr', 'stdout + stderr of the command')],
  ];
  return figure({
    viewBox: '0 0 780 348',
    minWidth: 620,
    title: P(
      '一次请求的内容：六条消息依次是 system、user、assistant、tool、assistant、tool；两对 assistant 与 tool 消息由 tool_call_id 配对；整个数组每一轮重新发送一次，模型在两次请求之间不保存任何状态。',
      'One request: six messages — system, user, assistant, tool, assistant, tool. Each assistant tool call is paired with its tool result by tool_call_id. The whole array is re-sent every turn; the model keeps no state between requests.',
    ),
    body: (u) => {
      const parts = [];
      rows.forEach(([role, what], i) => {
        const y = 20 + i * 48;
        parts.push(
          `<rect x="100" y="${y}" width="380" height="40" rx="5" class="fg-box"/>`,
          text(114, y + 25, role, 'fg-m fg-m-key'),
          text(198, y + 25, what, 'fg-t'),
        );
      });
      // pairing: rows 3+4 and 5+6, centre to centre
      parts.push(
        path('M 100,136 H 78 V 184 H 100', 'fg-flow-plain'),
        path('M 100,232 H 78 V 280 H 100', 'fg-flow-plain'),
        `<text transform="rotate(-90 62 160)" x="62" y="160" class="fg-m fg-m-a" text-anchor="middle">tool_call_id</text>`,
        // the brace, and what it means
        path('M 492,20 C 508,20 506,152 520,160 C 506,168 508,300 492,300', 'fg-line'),
        text(536, 138, P('每一轮都把这一整个', 'every turn re-sends'), 'fg-t'),
        text(536, 158, P('数组重新发一遍', 'this entire array'), 'fg-t'),
        text(536, 186, P('模型在两次请求之间', 'the model keeps no state'), 'fg-n'),
        text(536, 203, P('不保存任何状态', 'between two requests'), 'fg-n'),
        text(100, 328, P('第 1 轮 2 条 · 第 2 轮 4 条 · 第 3 轮 6 条 · 第 n 轮 2n 条', 'turn 1: 2 · turn 2: 4 · turn 3: 6 · turn n: 2n'), 'fg-n'),
      );
      void u;
      return parts.join('');
    },
  });
}

/* ------------------------------------------------------------------ 3. bill */
//
// The numbers are the six prompt-token counts from the run recorded in
// docs/00-loop.md. Each column is one request; each band inside it is what one
// earlier turn added. The bottom band is turn 1, and it is present in all six
// columns — which is the whole argument, and it is an area, so it is drawn as
// one rather than asserted.

const PROMPTS = [429, 613, 737, 932, 1079, 1192];

function bill() {
  const base = 250;
  const scale = 190 / PROMPTS[PROMPTS.length - 1];
  const colW = 62;
  const x0 = 70;
  const step = 80;
  const first = PROMPTS[0] * scale;

  return figure({
    viewBox: '0 0 780 300',
    minWidth: 640,
    title: P(
      '六根柱子代表六次请求的大小，依次是 429、613、737、932、1079、1192 个 prompt token，逐根变高。每根柱子底部同样高的一段是第 1 轮的 429 个 token，它在六次请求里各付了一次，合计 2574。六根柱子的面积之和就是账单 4982，而最终对话只有 1192，相当于付了 4.2 倍。',
      'Six columns, one per request: 429, 613, 737, 932, 1079, 1192 prompt tokens, each taller than the last. The band at the foot of every column is turn 1, paid six times over: 2574 tokens. The area of all six columns is the bill, 4982, against a final conversation of 1192 — 4.2 times over.',
    ),
    body: () => {
      const parts = [];
      PROMPTS.forEach((tokens, i) => {
        const x = x0 + i * step;
        const h = tokens * scale;
        const top = base - h;
        parts.push(
          `<rect x="${x}" y="${top.toFixed(1)}" width="${colW}" height="${h.toFixed(1)}" class="fg-col"/>`,
        );
        // the internal boundaries: one per earlier turn folded into this one
        for (let j = 1; j < i + 1; j++) {
          const y = base - PROMPTS[j - 1] * scale;
          parts.push(`<path d="M ${x},${y.toFixed(1)} h ${colW}" class="fg-hair"/>`);
        }
        parts.push(
          `<rect x="${x}" y="${(base - first).toFixed(1)}" width="${colW}" height="${first.toFixed(1)}" class="fg-band"/>`,
          text(x + colW / 2, top - 8, String(tokens), 'fg-m', 'middle'),
          text(x + colW / 2, base + 20, P(`轮 ${i + 1}`, `turn ${i + 1}`), 'fg-n', 'middle'),
        );
      });
      parts.push(
        `<path d="M 60,${base} H 546" class="fg-line"/>`,
        text(560, 74, P('账单 · 六次请求的和', 'billed · the six requests'), 'fg-n'),
        text(560, 102, '4982', 'fg-big'),
        text(560, 132, P('最终对话 · 第 6 根柱子', 'final conversation · column 6'), 'fg-n'),
        text(560, 160, '1192', 'fg-big fg-big-dim'),
        text(560, 188, P('你为它付了 4.2 倍', 'paid 4.2x over'), 'fg-t'),
        `<path d="M 560,208 H 762" class="fg-hair"/>`,
        `<rect x="560" y="226" width="11" height="11" rx="2" class="fg-band"/>`,
        text(580, 236, P('第 1 轮的 429 个 token', "turn 1's 429 tokens"), 'fg-n'),
        text(580, 254, P('被重发了 6 次 = 2574', 're-sent 6 times = 2574'), 'fg-n'),
        text(580, 272, P('超过账单的一半', 'more than half the bill'), 'fg-n fg-n-a'),
      );
      return parts.join('');
    },
  });
}

/* ---------------------------------------------------------------- 4. stages */
//
// A list would carry the names. What only the drawing carries is the shape:
// one trunk, a side road at 08 that nothing downstream depends on, and phase 2
// resuming from 07 rather than from where the branch left.

const STAGES = [
  ['00', ['循环', 'The Loop'], ['请求 → 工具调用 → 执行 → 重复。一个文件，没有 SDK', 'request, tool call, execute, repeat — one file, no SDK']],
  ['01', ['别死', "Don't Die"], ['输出截断、命令超时、进程树杀死、finish_reason、权限闸', 'truncation, timeouts, process-tree kill, finish_reason, a permission gate']],
  ['02', ['看见一切', 'See Everything'], ['事件总线、流式、全套仪表、JSONL trace、回放', 'an event bus, streaming, full instrumentation, a JSONL trace, replay']],
  ['03', ['巴别塔', 'Babel'], ['一个 agent 两套协议：OpenAI 与 Anthropic 共用中立内核', 'two protocols behind one neutral core']],
  ['04', ['缓存', 'The Cache'], ['prompt 缓存作为一种纪律，以及它到底值多少钱', 'prompt caching as discipline, and what it is worth in money']],
  ['05', ['活下去', 'Live Forever'], ['压缩、上下文注入、长期记忆，以及压缩真实的代价', 'compaction, context injection, memory — and what compaction really costs']],
  ['06', ['作曲家', 'The Composer'], ['标准库里的 TUI：同一场会话的上帝视角与模型视角', 'a TUI in the standard library: god view and model view of one session']],
  ['07', ['分身', 'Multiply'], ['用递归做子 agent、skills，以及 PTC 到底是什么', 'subagents by recursion, skills, and what PTC really is']],
  ['08', ['沙箱（可选）', 'Sandbox (optional)'], ['内嵌 shell 解释器；这门课唯一引入依赖的一站', 'an embedded shell interpreter; the one stage with a dependency']],
  ['09', ['分诊', 'Triage'], ['错误是一个决定：一套分类、Retry-After、重试预算、降级梯子', 'an error is a decision: one taxonomy, Retry-After, a retry budget, a fallback ladder']],
  ['10', ['死锁', 'Deadlock'], ['不返回的工具与停住的流：每一次等待都要有期限和负责人', 'every wait gets a deadline and an owner']],
  ['11', ['畸形', 'Malformed'], ['工具调用不是合法 JSON：为什么修补是陷阱，一条校验边界', 'the tool call is not valid JSON — why repairing it is the trap']],
  ['12', ['回声', 'Echo'], ['最便宜的工具调用是你没发出去的那一次，先审计再动手', 'the cheapest tool call is the one you do not make — audit it first']],
];

function stages() {
  const y0 = 44;
  const step = 34;
  const yOf = (i) => y0 + i * step;

  return figure({
    viewBox: '0 0 780 486',
    minWidth: 660,
    title: P(
      '十三个阶段的顺序图。第一部分 00 到 08 是仪表盘，第二部分 09 到 12 是生产环境中会出的问题。主干从 00 一直走到 07，第 08 阶段以虚线支线挂在 07 旁边，主干从 07 继续走到 09、10、11、12。',
      'The thirteen stages in order. Phase 1 (00-08) is the instrument panel, phase 2 (09-12) is what fails in production. The trunk runs 00 to 07; stage 08 hangs off 07 on a dashed side road; the trunk continues from 07 to 09, 10, 11 and 12.',
    ),
    body: () => {
      const parts = [
        // phase brackets
        path('M 52,34 H 40 V 326 H 52', 'fg-line-soft'),
        path('M 52,340 H 40 V 462 H 52', 'fg-line-soft'),
        `<text transform="rotate(-90 26 180)" x="26" y="180" class="fg-n" text-anchor="middle">${esc(P('第一部分 · 仪表盘', 'phase 1 · the panel'))}</text>`,
        `<text transform="rotate(-90 26 401)" x="26" y="401" class="fg-n" text-anchor="middle">${esc(P('第二部分 · 生产环境', 'phase 2 · production'))}</text>`,
        // the trunk, straight through: stage 08 does not interrupt it
        path(`M 92,${yOf(0)} V ${yOf(12)}`, 'fg-spine'),
        // the side road
        path(`M 92,${yOf(7)} C 92,${yOf(7) + 22} 108,${yOf(8)} 132,${yOf(8)}`, 'fg-branch'),
      ];

      STAGES.forEach(([num, name, adds], i) => {
        const y = yOf(i);
        const side = num === '08';
        const cx = side ? 140 : 92;
        parts.push(
          `<circle cx="${cx}" cy="${y}" r="5" class="${side ? 'fg-node-side' : 'fg-node'}"/>`,
          text(cx + 16, y + 4, num, 'fg-m'),
          text(cx + 54, y + 4, P(name[0], name[1]), side ? 'fg-t fg-t-dim' : 'fg-t'),
          text(side ? 300 : 250, y + 4, P(adds[0], adds[1]), 'fg-n'),
        );
      });
      return parts.join('');
    },
  });
}

export const FIGURES = { loop, payload, bill, stages };
