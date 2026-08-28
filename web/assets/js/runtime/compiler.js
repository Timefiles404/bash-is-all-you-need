// The build oracle: what happens when a learner presses Run.
//
// A learner fills a level's holes by choosing from a fixed list of options, so
// the set of programs they can produce is finite and known when the level is
// authored. web/tools/genlevels compiles them with a real `go build` and writes
// down what happened. This file looks the answer up.
//
// The claim that makes this worth doing is that the error text a learner sees
// is the error `go build` actually produced — not an approximation, not a
// JavaScript reimplementation of Go's type checker, not a hand-written hint
// pretending to be a compiler. It is a transcript.
//
// The cost is stated plainly in ARCHITECTURE.md and again here: nothing
// compiles. Edit the program off-script and there is nothing to look up. The
// oracle says so rather than guessing.
//
// ---------------------------------------------------------------------------
// The combinatorial problem, and what it is contained with
// ---------------------------------------------------------------------------
//
// A level with 6 holes of 4 options each has 4096 combinations. Compiling all
// of them at build time is minutes of CPU and, worse, up to 4096 recorded
// artifacts. So genlevels has two modes and records which one it used:
//
//   "full"      every combination compiled. Used when the product is small
//               (the default cap is 256). Every answer is a verbatim
//               transcript.
//
//   "per-hole"  the correct combination, plus, for each hole and each wrong
//               option, that one option wrong and all others correct. Σ rather
//               than Π: the same 6×4 level costs 19 builds instead of 4096.
//
// Is per-hole faithful? For one wrong choice, exactly — that entry IS the
// verbatim build. For two or more, it is a composition, and composition can be
// wrong in both directions:
//
//   * Under-report. Two wrong options can produce an error neither produces
//     alone — most commonly `declared and not used`, when both wrong branches
//     drop the only use of a variable.
//   * Over-report. Go stops after 10 errors per file and reports some errors
//     only when an earlier one did not mask them, so concatenating two
//     single-fault transcripts can show an error the real build would have
//     suppressed.
//
// So a composed answer is labelled `composed: true` and the UI says "the first
// problem in each of the N holes you changed", which is true, instead of
// "the compiler said", which would not be. A learner with several holes wrong
// is being told which holes are wrong — which is the useful thing — and is not
// being shown a transcript that no build ever produced.

import { ORIGIN, PHASE } from './status.js';

/** @typedef {{file:string,line:number,col:number,severity:string,message:string}} Diagnostic */

export class BuildOracle {
  /**
   * @param {string} base URL prefix for assets
   * @param {(phase:string, detail?:object)=>void} onPhase
   */
  constructor(base, onPhase = () => {}) {
    this.base = base;
    this.onPhase = onPhase;
    this.tables = new Map(); // levelId -> table
  }

  /**
   * Load a level's build table. Small: a few KB of JSON even in "full" mode,
   * because the artifacts live beside it rather than inside it.
   */
  async table(levelId) {
    if (this.tables.has(levelId)) return this.tables.get(levelId);
    const url = new URL(`levels/${levelId}/build-table.json`, this.base);
    const p = (async () => {
      const res = await fetch(url);
      if (!res.ok) throw new Error(`no build table for level ${levelId} (${res.status})`);
      return res.json();
    })();
    this.tables.set(levelId, p);
    return p;
  }

  /**
   * Resolve a selection to a build result.
   *
   * @param {string} levelId
   * @param {Record<string,string>} selection holeId -> optionId
   * @returns {Promise<{ok:boolean, diagnostics:Diagnostic[], artifactId:?string,
   *                    origin:string, composed:boolean, exact:boolean}>}
   */
  async resolve(levelId, selection) {
    this.onPhase(PHASE.MATCHING, { origin: ORIGIN.RECORDED });
    const table = await this.table(levelId);
    const key = table.holes.map((h) => selection[h] ?? '').join('|');

    // 1. An exact transcript, which is the case in "full" mode and the
    //    one-wrong-option case in "per-hole" mode.
    const exact = table.combinations?.[key];
    if (exact) {
      return {
        ok: exact.ok,
        diagnostics: stamp(exact.diagnostics, false),
        artifactId: exact.artifact || null,
        origin: ORIGIN.RECORDED,
        composed: false,
        exact: true,
      };
    }

    // 2. The correct answer, which is always recorded whatever the mode.
    if (table.correct && key === table.holes.map((h) => table.correct.selection[h]).join('|')) {
      return {
        ok: true,
        diagnostics: [],
        artifactId: table.correct.artifact,
        origin: ORIGIN.RECORDED,
        composed: false,
        exact: true,
      };
    }

    // 3. Compose from per-hole entries.
    if (table.mode === 'per-hole' && table.perHole) {
      const wrong = table.holes.filter((h) => selection[h] !== table.correct.selection[h]);
      /** @type {Diagnostic[]} */
      const diags = [];
      let missing = false;
      for (const h of wrong) {
        const entry = table.perHole[h]?.[selection[h]];
        if (!entry) {
          missing = true;
          continue;
        }
        // The first diagnostic per hole, not all of them: the rest are usually
        // knock-on errors from the same mistake, and a wall of them is how a
        // learner stops reading compiler output.
        if (entry.diagnostics?.length) diags.push(entry.diagnostics[0]);
      }
      if (!missing && diags.length) {
        return {
          ok: false,
          diagnostics: stamp(diags, wrong.length > 1),
          artifactId: null,
          origin: ORIGIN.RECORDED,
          composed: wrong.length > 1,
          exact: false,
        };
      }
    }

    // 4. Off the map. Say so, in the one place a learner will read it.
    return {
      ok: false,
      diagnostics: [
        {
          file: '',
          line: 0,
          col: 0,
          severity: 'info',
          message:
            'This exact combination was not compiled when the level was built, so there is ' +
            'nothing recorded to show you. Load the Go toolchain to compile it here, or ' +
            'return the holes to one of the offered options.',
          origin: ORIGIN.RECORDED,
          unmapped: true,
        },
      ],
      artifactId: null,
      origin: ORIGIN.RECORDED,
      composed: false,
      exact: false,
    };
  }

  /**
   * Where a resolved artifact lives.
   * @returns {URL}
   */
  artifactURL(levelId, artifactId) {
    return new URL(`levels/${levelId}/${artifactId}`, this.base);
  }

  /**
   * Diagnostics without producing an artifact.
   *
   * The same lookup as resolve(). Go does not separate "type-check" from
   * "compile" at the level of what a beginner sees — `go build` reports type
   * errors and there is no second, cheaper answer to give — so pretending
   * check() is a different, faster analysis would be inventing a distinction
   * the toolchain does not have.
   */
  async check(levelId, selection) {
    const r = await this.resolve(levelId, selection);
    return r.diagnostics;
  }
}

function stamp(diags, composed) {
  return (diags || []).map((d) => ({
    file: d.file || '',
    line: d.line || 0,
    col: d.col || 0,
    severity: d.severity || 'error',
    message: d.message || '',
    origin: ORIGIN.RECORDED,
    composed: !!composed,
  }));
}
