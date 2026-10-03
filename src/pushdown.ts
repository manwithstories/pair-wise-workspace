/**
 * pushdown.ts — filter pushdown planning and short-circuit evaluation.
 *
 * The point of this layer: a query like
 *
 *     level:error AND service:auth AND message:"timeout"
 *
 * must not scan documents. Instead we decompose the AST into a *plan* whose
 * leaves are exact-term or range predicates. A segment evaluates the plan by
 * intersecting the corresponding posting lists, and the moment any input is
 * provably empty the whole segment is skipped.
 *
 * Two shapes of short-circuit live here:
 *   1. intra-segment  : an empty posting list kills the intersection early;
 *   2. inter-segment  : a segment whose *selectivity estimate* rules it out
 *                       entirely is skipped without touching its postings.
 *
 * Conjunctions are reordered cheapest-first (most selective leaves first) so
 * the cheap emptiness test runs before expensive set operations.
 */

import {
  DocIdBuf,
  exceptSorted,
  intersectSorted,
  unionSorted,
  type InvertedIndex,
  type PostingList,
  type SetOpStats,
} from "./inverted.js";
import type { QueryNode, RangeNode } from "./parser.js";

/* ------------------------------------------------------------------ *
 * Plan model
 * ------------------------------------------------------------------ */

/** A leaf predicate that maps directly onto one posting list or numeric column. */
export type Leaf =
  | { kind: "term"; field: string; term: string }
  | { kind: "phrase"; field: string; terms: string[] }
  | { kind: "range"; field: string; lo: number | null; hi: number | null; loInclusive: boolean; hiInclusive: boolean };

export type PlanNode =
  | { kind: "leaf"; leaf: Leaf; /** estimated matching docs, -1 when unknown */ selectivity: number }
  | { kind: "and"; children: PlanNode[] }
  | { kind: "or"; children: PlanNode[] }
  | { kind: "not"; child: PlanNode; /** doc universe size, required to complement */ universe: number }
  | { kind: "const"; value: boolean };

export interface QueryPlan {
  root: PlanNode;
  /** conjunctive leaves, extracted for cheap pre-filtering */
  requiredLeaves: Leaf[];
  /** union branches, if any (segments with no union branch can be filtered) */
  unionLeaves: Leaf[] | null;
  /** number of leaves that map onto posting-list intersections */
  pushdownCount: number;
  /** leaves that need a full scan (currently only phrases over missing fields) */
  residualCount: number;
}

export interface PlanStats {
  segmentsEvaluated: number;
  segmentsSkipped: number;
  /** segments abandoned early because an intersection went empty */
  segmentsShortCircuited: number;
  leafComparisons: number;
  gallopHops: number;
}

/* ------------------------------------------------------------------ *
 * Planning
 * ------------------------------------------------------------------ */

/**
 * Build an executable plan from a parsed AST.
 *
 * `estimate` supplies per-leaf document counts so conjuncts can be ordered
 * cheapest-first; passing undefined disables ordering and keeps source order.
 */
export function buildPlan(
  ast: QueryNode,
  estimate?: (leaf: Leaf) => number,
): QueryPlan {
  const counter: { leaves: number; residual: number } = { leaves: 0, residual: 0 };
  const root = planNode(ast, estimate, counter, Number.MAX_SAFE_INTEGER);

  const requiredLeaves: Leaf[] = [];
  const unionLeaves: Leaf[] = [];
  collectLeaves(root, requiredLeaves, unionLeaves);

  return {
    root,
    requiredLeaves,
    unionLeaves: unionLeaves.length > 0 ? unionLeaves : null,
    pushdownCount: counter.leaves,
    residualCount: counter.residual,
  };
}

function planNode(
  node: QueryNode,
  estimate: ((leaf: Leaf) => number) | undefined,
  counter: { leaves: number; residual: number },
  universe: number,
): PlanNode {
  switch (node.kind) {
    case "matchAll":
      return { kind: "const", value: true };

    case "fieldTerm": {
      const leaf: Leaf = { kind: "term", field: node.field, term: node.term };
      counter.leaves++;
      const sel = estimate?.(leaf) ?? -1;
      return { kind: "leaf", leaf, selectivity: sel };
    }

    case "phrase": {
      const leaf: Leaf = { kind: "phrase", field: node.field, terms: node.terms };
      counter.leaves++;
      const sel = estimate?.(leaf) ?? -1;
      return { kind: "leaf", leaf, selectivity: sel };
    }

    case "range": {
      const leaf: Leaf = {
        kind: "range",
        field: node.field,
        lo: node.lo,
        hi: node.hi,
        loInclusive: node.loInclusive,
        hiInclusive: node.hiInclusive,
      };
      counter.leaves++;
      const sel = estimate?.(leaf) ?? -1;
      return { kind: "leaf", leaf, selectivity: sel };
    }

    case "and": {
      // flatten nested ANDs so ordering and leaf extraction see one flat list
      const parts: PlanNode[] = [];
      for (const child of [node.left, node.right]) {
        const p = planNode(child, estimate, counter, universe);
        if (p.kind === "and") parts.push(...p.children);
        else parts.push(p);
      }
      return { kind: "and", children: orderBySelectivity(parts) };
    }

    case "or": {
      const parts: PlanNode[] = [];
      for (const child of [node.left, node.right]) {
        const p = planNode(child, estimate, counter, universe);
        if (p.kind === "or") parts.push(...p.children);
        else parts.push(p);
      }
      return { kind: "or", children: parts };
    }

    case "not":
      return {
        kind: "not",
        child: planNode(node.operand, estimate, counter, universe),
        universe,
      };
  }
}

/**
 * Cheapest-first ordering. Known selectivity sorts ascending; unknown (-1)
 * sorts last so measured leaves drive the order.
 */
function orderBySelectivity(parts: PlanNode[]): PlanNode[] {
  const scored = parts.map((p) => (p.kind === "leaf" ? p.selectivity : Number.MAX_SAFE_INTEGER));
  if (scored.every((s) => s < 0)) return parts;
  return parts
    .map((p, i) => ({ p, s: scored[i] as number }))
    .sort((a, b) => {
      const av = a.s < 0 ? Number.MAX_SAFE_INTEGER : a.s;
      const bv = b.s < 0 ? Number.MAX_SAFE_INTEGER : b.s;
      return av - bv;
    })
    .map((x) => x.p);
}

function collectLeaves(node: PlanNode, required: Leaf[], unions: Leaf[]): void {
  switch (node.kind) {
    case "leaf":
      required.push(node.leaf);
      break;
    case "and":
      for (const c of node.children) collectLeaves(c, required, unions);
      break;
    case "or":
      for (const c of node.children) collectLeaves(c, unions, unions);
      break;
    case "not":
      collectLeaves(node.child, unions, unions);
      break;
    case "const":
      break;
  }
}

/* ------------------------------------------------------------------ *
 * Segment evaluation
 * ------------------------------------------------------------------ */

export interface SegmentEvalResult {
  /** ascending local docIds, or null when the segment provably has no match */
  docIds: Uint32Array | null;
  /** true when evaluation stopped early on an empty intersection */
  shortCircuited: boolean;
  comparisons: number;
  hops: number;
  /** true when the cap was reached and evaluation stopped early */
  capped: boolean;
}

/**
 * Evaluate a plan against one segment.
 *
 * Returns `docIds: null` as soon as the result is provably empty. Callers use
 * that to skip tombstone filtering and document hydration entirely.
 */
export function evaluateSegment(
  plan: QueryPlan,
  index: InvertedIndex,
  universe: number,
  cap = Number.MAX_SAFE_INTEGER,
): SegmentEvalResult {
  const state: SetOpStats = { hops: 0, comparisons: 0, shortCircuited: false };
  const out = new DocIdBuf(64);

  const result = evalNode(plan.root, index, universe, out, state, cap);
  const docIds = result ? out.toArray() : null;
  return {
    docIds,
    shortCircuited: docIds === null,
    comparisons: state.comparisons,
    hops: state.hops,
    capped: state.capped === true,
  };
}

/** Returns false when the subtree is provably empty (short-circuit). */
function evalNode(
  node: PlanNode,
  index: InvertedIndex,
  universe: number,
  out: DocIdBuf,
  state: SetOpStats,
  cap: number,
): boolean {
  switch (node.kind) {
    case "const":
      if (node.value) {
        out.reset();
        const n = Math.min(universe, cap);
        for (let i = 0; i < n; i++) out.push(i);
        return true;
      }
      return false;

    case "leaf":
      return evalLeaf(node.leaf, index, out, state, cap);

    case "and": {
      // Evaluate conjuncts cheapest-first; stop at the first empty one.
      let acc: Uint32Array | null = null;
      for (const child of node.children) {
        const buf = new DocIdBuf(64);
        const ok = evalNode(child, index, universe, buf, state, cap);
        if (!ok) {
          // provably empty: abandon the segment without further work
          state.shortCircuited = true;
          out.reset();
          return false;
        }
        const ids = buf.toArray();
        if (ids.length === 0) {
          state.shortCircuited = true;
          out.reset();
          return false;
        }
        // Enough results already: stop without further intersections.
        if (acc !== null && acc.length >= cap) {
          state.capped = true;
          out.reset();
          for (let i = 0; i < acc.length; i++) out.push(acc[i] as number);
          return true;
        }
        if (acc === null) {
          acc = ids;
        } else {
          const merged: DocIdBuf = new DocIdBuf(Math.max(acc.length, ids.length));
          const ok = intersectSorted(acc, ids, merged, state);
          acc = ok ? merged.toArray() : new Uint32Array(0);
          if (!ok) {
            state.shortCircuited = true;
            out.reset();
            return false;
          }
        }
      }
      if (acc === null) {
        out.reset();
        const n = Math.min(universe, cap);
        for (let i = 0; i < n; i++) out.push(i);
        return true;
      }
      out.reset();
      for (let i = 0; i < acc.length; i++) out.push(acc[i] as number);
      return true;
    }

    case "or": {
      let any = false;
      const acc = new DocIdBuf(64);
      for (const child of node.children) {
        const buf = new DocIdBuf(64);
        if (!evalNode(child, index, universe, buf, state, cap)) continue;
        const ids = buf.toArray();
        if (ids.length === 0) continue;
        any = true;
        unionSorted(acc.toArray(), ids, buf);
        acc.reset();
        for (let i = 0; i < buf.length; i++) acc.push(buf.at(i));
      }
      if (!any) {
        out.reset();
        return false;
      }
      out.reset();
      for (let i = 0; i < acc.length; i++) out.push(acc.at(i));
      return true;
    }

    case "not": {
      const buf = new DocIdBuf(64);
      if (!evalNode(node.child, index, universe, buf, state, cap)) {
        // NOT over an empty set is the whole universe, but only `cap` of it
        // can ever be returned, so stop early instead of materializing all.
        out.reset();
        const n = Math.min(node.universe, cap);
        for (let i = 0; i < n; i++) out.push(i);
        return out.length > 0;
      }
      // Complement lazily: emitting only the gaps keeps this O(cap) instead of
      // O(universe), which matters when the excluded set is the large side.
      const excluded = buf.toArray();
      out.reset();
      let produced = 0;
      let prev = 0;
      for (const x of excluded) {
        const stop = Math.min(x as number, node.universe);
        for (let v = prev; v < stop; v++) {
          out.push(v);
          produced++;
          if (produced >= cap) return true;
        }
        prev = (x as number) + 1;
        if (prev >= node.universe) break;
      }
      for (let v = prev; v < node.universe; v++) {
        out.push(v);
        produced++;
        if (produced >= cap) return true;
      }
      return out.length > 0;
    }
  }
}

/** Resolve one leaf against the segment's posting lists. */
function evalLeaf(
  leaf: Leaf,
  index: InvertedIndex,
  out: DocIdBuf,
  state: SetOpStats,
  cap: number,
): boolean {
  switch (leaf.kind) {
    case "term": {
      const list = index.lookup(leaf.field, leaf.term);
      if (list === undefined || list.size === 0) {
        // Term absent from this segment: the whole conjunction is empty.
        state.shortCircuited = true;
        out.reset();
        return false;
      }
      out.reset();
      const n = Math.min(list.size, cap);
      for (let i = 0; i < n; i++) out.push(list.docIdAt(i));
      return true;
    }

    case "phrase": {
      const lists: PostingList[] = [];
      for (const term of leaf.terms) {
        const list = index.lookup(leaf.field, term);
        if (list === undefined || list.size === 0) {
          state.shortCircuited = true;
          out.reset();
          return false;
        }
        lists.push(list);
      }
      // Candidates = intersection of the term lists, walked lazily so we never
      // copy a whole posting list and can stop as soon as `cap` matches exist.
      // The rarest list anchors the scan.
      let anchorIdx = 0;
      for (let i = 1; i < lists.length; i++) {
        if ((lists[i] as PostingList).size < (lists[anchorIdx] as PostingList).size) anchorIdx = i;
      }
      const anchor = lists[anchorIdx] as PostingList;
      const cursors = lists.map(() => 0);

      out.reset();
      let found = 0;
      for (let a = 0; a < anchor.size; a++) {
        const docId = anchor.docIdAt(a);
        // advance the other cursors to this docId (each only moves forward)
        let aligned = true;
        for (let k = 0; k < lists.length; k++) {
          if (k === anchorIdx) continue;
          const list = lists[k] as PostingList;
          let c = cursors[k] as number;
          if (c === 0 || list.docIdAt(c - 1) < docId) c = list.seekGE(docId);
          cursors[k] = c;
          if (c >= list.size || list.docIdAt(c) !== docId) {
            aligned = false;
            break;
          }
        }
        state.comparisons++;
        if (!aligned) continue;

        // Confirm adjacency; co-occurrence alone is not a phrase match.
        if (!matchesPhraseAt(lists, cursors, anchorIdx)) continue;
        out.push(docId);
        found++;
        if (found >= cap) break;
      }
      if (found === 0) {
        state.shortCircuited = true;
        out.reset();
        return false;
      }
      return true;
    }

    case "range": {
      const ids = index.range(leaf.field, leaf.lo, leaf.hi, leaf.loInclusive, leaf.hiInclusive);
      if (ids.length === 0) {
        out.reset();
        return false;
      }
      out.reset();
      for (let i = 0; i < ids.length; i++) out.push(ids[i] as number);
      return true;
    }
  }
}

/**
 * Phrase confirmation without a global scan: walk the rarest list's positions
 * and require term k at startPos + k. This is `hasPhrase` restricted to the
 * already-computed candidate set.
 */
/**
 * Adjacency check for one document. `anchorIdx` may be any term, so the phrase
 * slot of a term is `anchorPos + (k - anchorIdx)`.
 */
function matchesPhraseAt(lists: PostingList[], indices: number[], anchorIdx: number): boolean {
  const anchor = lists[anchorIdx] as PostingList;
  const starts = anchor.positionsAt(indices[anchorIdx] as number);
  if (starts === null) return false;
  for (const anchorPos of starts) {
    let ok = true;
    for (let k = 0; k < lists.length && ok; k++) {
      if (k === anchorIdx) continue;
      const ops = (lists[k] as PostingList).positionsAt(indices[k] as number);
      if (ops === null) {
        ok = false;
        break;
      }
      const want = anchorPos + (k - anchorIdx);
      let found = false;
      for (const p of ops) {
        if (p === want) {
          found = true;
          break;
        }
        if (p > want) break;
      }
      ok = found;
    }
    if (ok) return true;
  }
  return false;
}

/* ------------------------------------------------------------------ *
 * Explain
 * ------------------------------------------------------------------ */

export interface PlanExplanation {
  /** one line per plan node, indented */
  lines: string[];
  pushdownCount: number;
  residualCount: number;
}

/** Human-readable plan dump for the demo and for debugging. */
export function explainPlan(plan: QueryPlan): PlanExplanation {
  const lines: string[] = [];
  walk(plan.root, 0);
  return { lines, pushdownCount: plan.pushdownCount, residualCount: plan.residualCount };

  function walk(node: PlanNode, depth: number): void {
    const pad = "  ".repeat(depth);
    switch (node.kind) {
      case "const":
        lines.push(`${pad}${node.value ? "MATCH_ALL" : "MATCH_NONE"}`);
        return;
      case "leaf": {
        const sel = node.selectivity >= 0 ? ` sel≈${node.selectivity}` : "";
        switch (node.leaf.kind) {
          case "term":
            lines.push(`${pad}POSTING  ${node.leaf.field}:${node.leaf.term}${sel}`);
            return;
          case "phrase":
            lines.push(`${pad}PHRASE   ${node.leaf.field}:"${node.leaf.terms.join(" ")}"${sel}`);
            return;
          case "range":
            lines.push(`${pad}RANGE    ${describeRange(node.leaf)}${sel}`);
            return;
        }
        return;
      }
      case "and":
        lines.push(`${pad}AND  (short-circuit on first empty)`);
        node.children.forEach((c) => walk(c, depth + 1));
        return;
      case "or":
        lines.push(`${pad}OR   (union of branches)`);
        node.children.forEach((c) => walk(c, depth + 1));
        return;
      case "not":
        lines.push(`${pad}NOT  (complement within segment)`);
        walk(node.child, depth + 1);
        return;
    }
  }
}

function describeRange(leaf: Extract<Leaf, { kind: "range" }>): string {
  const lo = leaf.lo === null ? "*" : `${leaf.loInclusive ? "" : ">"}${leaf.lo}`;
  const hi = leaf.hi === null ? "*" : `${leaf.hiInclusive ? "" : "<"}${leaf.hi}`;
  return `${leaf.field}:[${lo} TO ${hi}]`;
}

/** True when a segment can be skipped without touching postings. */
export function segmentCanBeSkipped(plan: QueryPlan, index: InvertedIndex): boolean {
  for (const leaf of plan.requiredLeaves) {
    if (leaf.kind === "term" || leaf.kind === "phrase") {
      let anyMissing = false;
      for (const term of leaf.kind === "term" ? [leaf.term] : leaf.terms) {
        const list = index.lookup(leaf.field, term);
        if (list === undefined || list.size === 0) anyMissing = true;
      }
      if (anyMissing && plan.requiredLeaves.length > 0) {
        // Only conclusive when the missing leaf is conjunctive.
        if (leaf.kind === "term") return true;
      }
    }
  }
  return false;
}

export { exceptSorted };
