# How a chapter is written

The rule everything below serves:

> **A reader meets a problem before they meet a solution, and every step is
> something they could have arrived at themselves.**

The failure this exists to prevent has a name: *writing backwards*. You finish
the system, then explain it by taking it apart — here is the loop, here are its
two invariants, here is the error philosophy, here is a table of what each
function does. Everything in it is true. Someone who has already built one will
nod at every line. A learner will not know why any of it was necessary, because
they were never shown the moment where it became necessary.

A chapter is not a description of the code. It is the sequence of small
decisions that produced the code, in the order they were forced.

## The shape

Every document — a chapter, or one part of a chapter — has these sections, in
this order, with these exact headings.

```
# 阶段 NN：<名字> —— <一句话，说这一章要解决什么>
# Stage NN: <name> — <one line, what this chapter is for>

<breadcrumb>

## 问题  / The problem      the scene. no code.
## 办法  / The idea         one picture, and the idea in five lines.
## 怎么做的 / Building it    numbered steps that build the code in front of the reader.
## 跑一下 / Run it          what to type, and what to watch for.
## 量一量 / Measured        (only when there is a real measurement)
## 接下来 / Next            the new problem this chapter's answer created.
```

In English, 观察重点 is **What to watch for**. A part file's headings are the
same; its title is `## Stage NN · part K: <topic>`.

### 问题

A concrete scene, in the second person, that the reader has been in — or can
picture in one read. Something they typed, something that happened, why it is
not good enough.

No code. No terminology. If a term is unavoidable, the sentence it first appears
in has to also be the sentence that explains it, in ordinary words.

It ends by naming the pain in one sentence. That sentence is what the rest of
the document is for.

The test: **could a reader who has never seen this repo feel the problem?** If
the section only makes sense to someone who already knows the answer, it is a
summary wearing the costume of a problem.

### 办法

The idea, small enough to hold. One diagram, and about five lines of text — very
often a three-row table is the whole thing.

This section is not where it gets built. It is where the reader learns what they
are about to build, so that every step afterwards has somewhere to land.

### 怎么做的

Numbered steps. Each step is one or two sentences saying why this step is
needed, and then a code block of **at most a dozen lines**.

The code accumulates. Step 4 is step 3 plus one thing. At the end, the pieces
are assembled into the real function, and the reader recognises every line
because they watched each one arrive.

Never open with the finished function and walk down it. That is the failure this
document exists to prevent, and it is the most natural thing in the world to do
once you know the answer.

Quote real code — actually real. `external/tools/quotecheck.py` checks that every
line inside a ```` ```go ```` fence exists verbatim in that stage's `code/`, and
CI runs it. Three escapes, for the blocks that are not quotes:

- ```` ```go wrong ```` — code you are showing the reader *not* to write.
- `// ...` on its own line — you skipped something here.
- A comment containing Chinese — real code comments are English, so this is
  visibly the chapter talking, not the source.

The third escape is unavailable in the English edition, because real comments are
English there too. So an English chapter either quotes the source's own comment
or puts the annotation in the prose — annotated arrows belong inside a
```` ```go wrong ```` block, which is unchecked.

Keep the source's own English comments when you quote a line that has one, and
put the Chinese explanation in the prose around the block. Rewriting a comment
into Chinese inside a quote makes the quote a paraphrase, and then nothing
checks it.

### 跑一下

Exact commands, and two or three concrete things to type at the agent. Then
**观察重点** — what to watch for and what it means. A reader who runs this
should see the thing the chapter is about actually happen.

### 量一量

Only when this chapter measured something. Real numbers from real runs, with
what was measured and how. If a measurement undercuts the chapter's own thesis,
it goes in anyway and the chapter says so — that is the most valuable kind.

Never invent a number. A chapter with no measurement simply has no 量一量.

### 接下来

The chapter's answer created a new problem. Name it, as a question, and say
which stage picks it up.

This is the hinge of the whole course. Chapter N+1's 问题 is chapter N's 接下来.
If you cannot write this section, the chapters are in the wrong order.

## Limits

- **200–350 lines per document**, and past 350 stop and ask whether it should
  split: `README_zh.md` keeps the chapter's spine, and each part becomes its own
  file in the same folder, each one obeying this whole document in miniature.

  This is a trigger for judgement, not a cap. What it protects against is a wall
  of text with six unrelated things in it. What it must not do is cut a single
  argument in half to hit a number — stage 08's 怎么做的 is an escalation
  ladder where each step defeats the one before it, and splitting it anywhere
  would leave both halves less convincing than the whole. That chapter runs
  long on purpose. If you go over, say in one line why the thing is indivisible;
  if you cannot, it was divisible.
- **A dozen lines per code block**, except the final assembly in 怎么做的.
- **A term is introduced at most once, in the sentence that needs it.** No
  glossary paragraphs. No "recall that". If a chapter needs four new terms in
  its first page, it is trying to teach four things.
- **Every claim about behaviour is either shown in 跑一下 or measured in
  量一量.** No third option.

## Language

Chinese and English are written separately, from the source, by whoever is
writing that edition. **They are not translations of each other.** The previous
attempt made English first and derived Chinese from it, and the result read like
translated English — long sentences shaped by English clause order, technical
terms transliterated instead of said plainly.

Both editions teach the same code in the same order, and are free to differ
everywhere else: different scenes in 问题, different analogies, different
sentence lengths.

Naming: `README_zh.md`, `1-<topic>_zh.md` for Chinese; `README.md`,
`1-<topic>.md` for English. Diagrams go in `doc/images/`; a diagram with words
in it gets one file per language (`loop_zh.svg`, `loop.svg`), a diagram without
words is shared.

**Code comments stay English in every edition.** A reader of either edition is
reading the same source files.

## Register

Plain, unhurried, specific. Explain slowly and deeply; do not simplify by
making things vague.

- No childish analogies. The reader is an engineer.
- No jargon used as an explanation. "It uses an event bus" explains nothing;
  "everything that happens gets announced in one place, so the printing code and
  the trace file can both listen without knowing about each other" explains it.
- No enthusiasm. No "很简单!", no "接下来就是见证奇迹的时刻".
- Prefer the concrete number to the adjective: not "很贵", but "4982 tokens，
  而这段对话最后只有 1192 tokens".
- Say what went wrong, including when it was the author's own mistake. The
  chapters where the measurement contradicted the plan are the good ones.

## Diagrams

Plenty of them, and each one earns its place by showing something the prose
would need a paragraph for: a sequence, a shape, a before/after.

Hand-written SVG, in `doc/images/`, referenced with a normal image link so it
renders on GitHub. Keep them plain — a light background, a couple of accent
colours, real text rather than paths, and a `viewBox` so they scale. No external
fonts, no scripts, no CSS files.

A diagram of a loop should show the loop. A diagram that is a box labelled
"Agent" with an arrow to a box labelled "LLM" is a decoration; delete it.
