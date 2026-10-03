/**
 * inverted.ts — inverted index primitives.
 *
 * Pure data structures: no I/O, no engine state.
 *   PostingList  : ascending docId list + skip table for galloping seeks
 *   InvertedIndex: per-field term dictionary, position lists, numeric columns
 *   DocIdBuf     : reusable growable uint32 accumulator (avoids per-query GC)
 *
 * docIds are *local* to a segment and dense/contiguous within it, which is what
 * makes skip-table galloping worthwhile.
 */

/** Postings per skip-table entry. */
export const SKIP_BLOCK = 128;

/* ------------------------------------------------------------------ *
 * DocIdBuf
 * ------------------------------------------------------------------ */

/** Reusable growable uint32 buffer used as a posting-set accumulator. */
export class DocIdBuf {
  private buf: Uint32Array;
  private len = 0;

  constructor(capacity = 64) {
    this.buf = new Uint32Array(Math.max(capacity, 1));
  }

  get length(): number {
    return this.len;
  }

  get capacity(): number {
    return this.buf.length;
  }

  reset(): void {
    this.len = 0;
  }

  push(v: number): void {
    if (this.len === this.buf.length) {
      const next = new Uint32Array(this.buf.length * 2);
      next.set(this.buf);
      this.buf = next;
    }
    this.buf[this.len++] = v;
  }

  /** Copy out without clearing. */
  toArray(): Uint32Array {
    return this.buf.slice(0, this.len);
  }

  /** Ascending sort in place. */
  sort(): this {
    if (this.len > 1) this.buf.subarray(0, this.len).sort();
    return this;
  }

  at(i: number): number {
    return this.buf[i] as number;
  }
}

/* ------------------------------------------------------------------ *
 * PostingList
 * ------------------------------------------------------------------ */

export interface PostingListInit {
  docIds: Uint32Array;
  freqs: Uint32Array;
  /** Start index into posData per doc; length = docIds.length + 1. */
  posOffsets: Uint32Array | null;
  /** Concatenated per-doc positions (text fields only). */
  posData: Uint32Array | null;
}

export class PostingList {
  readonly docIds: Uint32Array;
  readonly freqs: Uint32Array;
  readonly posOffsets: Uint32Array | null;
  readonly posData: Uint32Array | null;
  /** start index of every SKIP_BLOCK-th posting */
  readonly skip: Uint32Array;
  readonly size: number;

  constructor(init: PostingListInit) {
    this.docIds = init.docIds;
    this.freqs = init.freqs;
    this.posOffsets = init.posOffsets;
    this.posData = init.posData;
    this.size = init.docIds.length;
    this.skip = buildSkipTable(this.docIds, SKIP_BLOCK);
  }

  docIdAt(i: number): number {
    return this.docIds[i] as number;
  }

  /** First index whose docId >= target, else `size`. Skip-table narrowed. */
  seekGE(target: number): number {
    const docIds = this.docIds;
    let lo = 0;
    let hi = this.size;

    const blocks = this.skip.length;
    if (blocks > 0) {
      // largest block whose first docId <= target
      let bl = 0;
      let br = blocks - 1;
      while (bl < br) {
        const mid = (bl + br + 1) >> 1;
        if ((docIds[this.skip[mid] as number] as number) <= target) bl = mid;
        else br = mid - 1;
      }
      lo = this.skip[bl] as number;
      hi = bl + 1 < blocks ? (this.skip[bl + 1] as number) : this.size;
    }

    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if ((docIds[mid] as number) < target) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  /** Term positions within the doc at posting index `i`. */
  positionsAt(i: number): Uint32Array | null {
    const { posOffsets, posData } = this;
    if (posOffsets === null || posData === null) return null;
    return posData.subarray(posOffsets[i] as number, posOffsets[i + 1] as number);
  }
}

function buildSkipTable(docIds: Uint32Array, block: number): Uint32Array {
  const count = Math.floor((docIds.length + block - 1) / block);
  const out = new Uint32Array(Math.max(count, 1));
  for (let i = 0; i < count; i++) out[i] = i * block;
  return out;
}

/**
 * True when some document contains every term at consecutive positions.
 *
 * Drives iteration from the rarest posting list, then requires term k to sit at
 * startPos + k within the same document. Terms may not repeat, which keeps the
 * candidate window unambiguous.
 */
export function hasPhrase(termLists: readonly PostingList[]): boolean {
  if (termLists.length === 0) return false;
  for (const t of termLists) {
    if (t.size === 0) return false;
    if (t.posData === null || t.posOffsets === null) return false;
  }

  // anchor = rarest list; other lists index terms 1..n-1 in phrase order
  let anchorIdx = 0;
  for (let i = 1; i < termLists.length; i++) {
    if ((termLists[i] as PostingList).size < (termLists[anchorIdx] as PostingList).size) {
      anchorIdx = i;
    }
  }
  const anchor = termLists[anchorIdx] as PostingList;
  const others = termLists.map((t, i) => ({ list: t, k: i })).filter((o) => o.k !== anchorIdx);

  // cursor per other list, monotonically advanced across anchor docs
  const cursors = others.map(() => 0);

  for (let a = 0; a < anchor.size; a++) {
    const docId = anchor.docIdAt(a);

    for (let c = 0; c < others.length; c++) {
      const list = (others[c] as { list: PostingList; k: number }).list;
      let i = cursors[c] as number;
      if (i > 0 && (i >= list.size || list.docIdAt(i - 1) === docId)) continue;
      i = list.seekGE(docId);
      cursors[c] = i;
    }

    const starts = anchor.positionsAt(a);
    if (starts === null) continue;

    let hit = false;
    for (const startPos of starts) {
      let ok = true;
      for (let c = 0; ok && c < others.length; c++) {
        const entry = others[c] as { list: PostingList; k: number };
        const ci = cursors[c] as number;
        if (ci >= entry.list.size || entry.list.docIdAt(ci) !== docId) {
          ok = false;
          break;
        }
        const ops = entry.list.positionsAt(ci);
        if (ops === null) {
          ok = false;
          break;
        }
        // relative phrase slot for this term given the anchor position
        const want = startPos + (entry.k - anchorIdx);
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
      if (ok) {
        hit = true;
        break;
      }
    }
    if (hit) return true;
  }
  return false;
}

/* ------------------------------------------------------------------ *
 * NumericColumn
 * ------------------------------------------------------------------ */

/** Value-sorted column supporting range pushdown. */
export class NumericColumn {
  readonly values: Float64Array;
  readonly docIds: Uint32Array;

  constructor(values: Float64Array, docIds: Uint32Array) {
    this.values = values;
    this.docIds = docIds;
  }

  get size(): number {
    return this.values.length;
  }

  /** Build from parallel arrays that are already in any order. */
  static fromPairs(values: Float64Array, docIds: Uint32Array): NumericColumn {
    const n = values.length;
    const order = Array.from({ length: n }, (_, i) => i).sort(
      (x, y) => (values[x] as number) - (values[y] as number),
    );
    const sv = new Float64Array(n);
    const sd = new Uint32Array(n);
    for (let i = 0; i < n; i++) {
      const j = order[i] as number;
      sv[i] = values[j] as number;
      sd[i] = docIds[j] as number;
    }
    return new NumericColumn(sv, sd);
  }

  static from(values: number[], docIds: number[]): NumericColumn {
    const n = values.length;
    const order = new Uint32Array(n);
    for (let i = 0; i < n; i++) order[i] = i;
    const idx = Array.from(order).sort((x, y) => (values[x] as number) - (values[y] as number));
    const sv = new Float64Array(n);
    const sd = new Uint32Array(n);
    for (let i = 0; i < n; i++) {
      const j = idx[i] as number;
      sv[i] = values[j] as number;
      sd[i] = docIds[j] as number;
    }
    return new NumericColumn(sv, sd);
  }

  /** First index with value >= target. */
  private lowerBound(target: number): number {
    const v = this.values;
    let lo = 0;
    let hi = v.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if ((v[mid] as number) < target) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  /** First index with value > target. */
  private upperBound(target: number): number {
    const v = this.values;
    let lo = 0;
    let hi = v.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if ((v[mid] as number) <= target) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  /** Ascending docIds within the range; null when nothing qualifies. */
  sliceRange(
    lo: number | null,
    hi: number | null,
    loInclusive: boolean,
    hiInclusive: boolean,
  ): Uint32Array {
    let start = 0;
    let end = this.size;
    if (lo !== null) start = loInclusive ? this.lowerBound(lo) : this.upperBound(lo);
    if (hi !== null) end = hiInclusive ? this.upperBound(hi) : this.lowerBound(hi);
    if (start >= end) return new Uint32Array(0);

    const out = new DocIdBuf(end - start);
    for (let i = start; i < end; i++) out.push(this.docIds[i] as number);
    return out.sort().toArray();
  }
}

/* ------------------------------------------------------------------ *
 * Builders
 * ------------------------------------------------------------------ */

/**
 * Growable posting under construction.
 *
 * Typed arrays with explicit capacity double in place, which avoids both the
 * per-posting array churn and the boxing cost of a `number[]` holding millions
 * of docIds.
 */
class PostingBuilder {
  docIds: Uint32Array;
  freqs: Uint32Array;
  positions: Uint32Array | null;
  posOffsets: Uint32Array | null;
  size = 0;
  posSize = 0;
  offsetCount = 0;

  constructor(positional: boolean, capacity = 8) {
    this.docIds = new Uint32Array(capacity);
    this.freqs = new Uint32Array(capacity);
    this.positions = positional ? new Uint32Array(capacity) : null;
    this.posOffsets = positional ? new Uint32Array(capacity + 1) : null;
  }

  private growDocs(): void {
    const n = this.docIds.length * 2;
    const d = new Uint32Array(n);
    d.set(this.docIds);
    this.docIds = d;
    const f = new Uint32Array(n);
    f.set(this.freqs);
    this.freqs = f;
  }

  private growPositions(): void {
    const n = (this.positions?.length ?? 8) * 2;
    const p = new Uint32Array(n);
    if (this.positions) p.set(this.positions);
    this.positions = p;
    const o = new Uint32Array(n + 1);
    if (this.posOffsets) o.set(this.posOffsets);
    this.posOffsets = o;
  }

  push(docId: number, position: number | undefined): void {
    if (this.size === this.docIds.length) this.growDocs();
    if (this.positions !== null && position !== undefined && this.posSize === this.positions.length) {
      this.growPositions();
    }
    this.docIds[this.size] = docId;
    this.freqs[this.size] = 1;
    if (this.positions !== null && this.posOffsets !== null) {
      this.posOffsets[this.offsetCount] = this.posSize;
      this.offsetCount++;
      if (position !== undefined) {
        this.positions[this.posSize] = position;
        this.posSize++;
      }
    }
    this.size++;
  }

  bumpFreq(position: number | undefined): void {
    const i = this.size - 1;
    this.freqs[i] = (this.freqs[i] as number) + 1;
    if (this.positions !== null && position !== undefined) {
      if (this.posSize === this.positions.length) this.growPositions();
      this.positions[this.posSize] = position;
      this.posSize++;
    }
  }
}

interface NumericBuilder {
  values: number[];
  docIds: number[];
}

/* ------------------------------------------------------------------ *
 * InvertedIndex
 * ------------------------------------------------------------------ */

export class InvertedIndex {
  /**
   * Per-field term dictionary, keyed by term string. Terms are looked up once
   * per document and reused, so hot fields (level, service) cost a single Map
   * hit while high-cardinality fields (trace_id) stay linear.
   */
  private readonly builders = new Map<string, Map<string, PostingBuilder>>();
  private readonly numBuilders = new Map<string, NumericBuilder>();
  private readonly sealed = new Map<string, Map<string, PostingList>>();
  private readonly numSealed = new Map<string, NumericColumn>();
  /** Fields recording term positions (text fields). */
  readonly positionalFields: Set<string>;

  constructor(positionalFields: Iterable<string> = []) {
    this.positionalFields = new Set(positionalFields);
  }

  get sealed_(): boolean {
    return this.sealed.size > 0;
  }

  addTerm(field: string, term: string, docId: number, position?: number): void {
    let dict = this.builders.get(field);
    if (dict === undefined) {
      dict = new Map();
      this.builders.set(field, dict);
    }
    let p = dict.get(term);
    if (p === undefined) {
      p = new PostingBuilder(this.positionalFields.has(field));
      dict.set(term, p);
    }
    // docIds arrive in ascending order, so equal ids are adjacent
    if (p.docIds[p.size - 1] === docId) {
      p.bumpFreq(position);
    } else {
      p.push(docId, position);
    }
  }

  addNumeric(field: string, value: number, docId: number): void {
    let col = this.numBuilders.get(field);
    if (col === undefined) {
      col = { values: [], docIds: [] };
      this.numBuilders.set(field, col);
    }
    col.values.push(value);
    col.docIds.push(docId);
  }

  /** Freeze builders into query-ready structures. Idempotent. */
  seal(): void {
    for (const [field, dict] of this.builders) {
      if (this.sealed.has(field)) continue;
      const out = new Map<string, PostingList>();
      for (const [term, p] of dict) out.set(term, sealPosting(p));
      this.sealed.set(field, out);
    }
    for (const [field, col] of this.numBuilders) {
      if (this.numSealed.has(field)) continue;
      this.numSealed.set(field, NumericColumn.from(col.values, col.docIds));
    }
  }

  lookup(field: string, term: string): PostingList | undefined {
    return this.sealed.get(field)?.get(term);
  }

  hasField(field: string): boolean {
    return this.sealed.has(field) || this.numSealed.has(field);
  }

  fields(): string[] {
    return [...new Set([...this.sealed.keys(), ...this.numSealed.keys()])];
  }

  isNumeric(field: string): boolean {
    return this.numSealed.has(field);
  }

  termCount(field: string): number {
    return this.sealed.get(field)?.size ?? 0;
  }

  numericCount(field: string): number {
    return this.numSealed.get(field)?.size ?? 0;
  }

  /** Ascending docIds in range; empty array when the field is absent. */
  range(
    field: string,
    lo: number | null,
    hi: number | null,
    loInclusive: boolean,
    hiInclusive: boolean,
  ): Uint32Array {
    const col = this.numSealed.get(field);
    if (col === undefined) return new Uint32Array(0);
    return col.sliceRange(lo, hi, loInclusive, hiInclusive);
  }

  /** Merge hook: visit every (term, postingList) of a field. */
  eachFieldTerms(field: string, fn: (term: string, list: PostingList) => void): void {
    const dict = this.sealed.get(field);
    if (dict === undefined) return;
    for (const [term, list] of dict) fn(term, list);
  }

  fieldNames(): string[] {
    return this.fields();
  }

  dictFor(field: string): ReadonlyMap<string, PostingList> | undefined {
    return this.sealed.get(field);
  }

  /**
   * Install an already-sorted posting list (segment load path). The skip table
   * is rebuilt by the PostingList constructor.
   */
  addSealedTerm(
    field: string,
    term: string,
    docIds: Uint32Array,
    freqs: Uint32Array,
    posOffsets: Uint32Array | null,
    posData: Uint32Array | null,
  ): void {
    let dict = this.sealed.get(field);
    if (dict === undefined) {
      dict = new Map();
      this.sealed.set(field, dict);
    }
    dict.set(term, new PostingList({ docIds, freqs, posOffsets, posData }));
  }

  /** Install a persisted numeric column: value -> docId pairs in any order. */
  attachNumeric(field: string, pairs: ReadonlyArray<readonly [number, number]>): void {
    const values = new Float64Array(pairs.length);
    const docIds = new Uint32Array(pairs.length);
    for (let i = 0; i < pairs.length; i++) {
      values[i] = (pairs[i] as readonly [number, number])[0];
      docIds[i] = (pairs[i] as readonly [number, number])[1];
    }
    this.numSealed.set(field, NumericColumn.fromPairs(values, docIds));
  }

  /** Numeric column sorted by value (persist path). */
  numericColumn(field: string): NumericColumn | undefined {
    return this.numSealed.get(field);
  }

  /** (localId, value) pairs for a numeric field, in docId order. */
  eachNumericValues(field: string, fn: (localId: number, value: number) => void): void {
    const col = this.numSealed.get(field);
    if (col === undefined) return;
    const pairs = [...col.docIds.keys()].map((_, i) => i);
    // col stores value-sorted rows; map back to (docId, value)
    for (let i = 0; i < col.size; i++) {
      fn(col.docIds[i] as number, col.values[i] as number);
    }
    void pairs;
  }

  /** Empty index with the same positional shape (for merge output). */
  static emptyLike(other: InvertedIndex): InvertedIndex {
    return new InvertedIndex(other.positionalFields);
  }
}

function sealPosting(p: PostingBuilder): PostingList {
  // slice() copies exactly the used prefix - no re-iteration, no boxing
  const docIds = p.docIds.slice(0, p.size);
  const freqs = p.freqs.slice(0, p.size);
  if (p.positions === null || p.posOffsets === null) {
    return new PostingList({ docIds, freqs, posOffsets: null, posData: null });
  }
  // posOffsets holds one entry per doc; append the closing sentinel.
  const offsets = new Uint32Array(p.size + 1);
  offsets.set(p.posOffsets.subarray(0, p.size));
  offsets[p.size] = p.posSize;
  return new PostingList({
    docIds,
    freqs,
    posOffsets: offsets,
    posData: p.positions.slice(0, p.posSize),
  });
}

/* ------------------------------------------------------------------ *
 * Set operations
 * ------------------------------------------------------------------ */

export interface SetOpStats {
  /** postings stepped over via galloping, without a docId comparison */
  hops: number;
  /** docId comparisons performed */
  comparisons: number;
  /** an input was empty, so the result is provably empty */
  shortCircuited: boolean;
  /** the caller had enough results and stopped early */
  capped?: boolean;
}

/**
 * Galloping intersection of two ascending docId arrays.
 * Returns false when the result is provably empty.
 */
export function intersectSorted(
  a: Uint32Array,
  b: Uint32Array,
  out: DocIdBuf,
  stats: SetOpStats,
): boolean {
  out.reset();
  if (a.length === 0 || b.length === 0) {
    stats.shortCircuited = true;
    return false;
  }
  // Drive the shorter list with the outer loop.
  let i = 0;
  let j = 0;
  const aLen = a.length;
  const bLen = b.length;
  while (i < aLen && j < bLen) {
    const x = a[i] as number;
    const y = b[j] as number;
    stats.comparisons++;
    if (x === y) {
      out.push(x);
      i++;
      j++;
    } else if (x < y) {
      const start = i;
      let step = 1;
      i++;
      while (i < aLen && (a[i] as number) < y) {
        i += step;
        step *= 2;
      }
      if (i > aLen) i = aLen;
      stats.hops += i - start - 1;
    } else {
      const start = j;
      let step = 1;
      j++;
      while (j < bLen && (b[j] as number) < x) {
        j += step;
        step *= 2;
      }
      if (j > bLen) j = bLen;
      stats.hops += j - start - 1;
    }
  }
  return out.length > 0;
}

/** Union of ascending arrays, de-duplicated. */
export function unionSorted(a: Uint32Array, b: Uint32Array, out: DocIdBuf): void {
  if (a.length === 0 && b.length === 0) {
    out.reset();
    return;
  }
  if (a.length === 0) {
    out.reset();
    for (let i = 0; i < b.length; i++) out.push(b[i] as number);
    return;
  }
  if (b.length === 0) {
    out.reset();
    for (let i = 0; i < a.length; i++) out.push(a[i] as number);
    return;
  }
  out.reset();
  let i = 0;
  let j = 0;
  let prev = -1;
  while (i < a.length && j < b.length) {
    const x = a[i] as number;
    const y = b[j] as number;
    const v = x < y ? x : y;
    if (v !== prev) {
      out.push(v);
      prev = v;
    }
    if (x < y) i++;
    else if (y < x) j++;
    else {
      i++;
      j++;
    }
  }
  while (i < a.length) {
    const v = a[i++] as number;
    if (v !== prev) {
      out.push(v);
      prev = v;
    }
  }
  while (j < b.length) {
    const v = b[j++] as number;
    if (v !== prev) {
      out.push(v);
      prev = v;
    }
  }
}

/** Ascending difference a \ b. */
export function exceptSorted(a: Uint32Array, b: Uint32Array, out: DocIdBuf): void {
  out.reset();
  if (a.length === 0) return;
  if (b.length === 0) {
    for (let i = 0; i < a.length; i++) out.push(a[i] as number);
    return;
  }
  let j = 0;
  for (let i = 0; i < a.length; i++) {
    const x = a[i] as number;
    while (j < b.length && (b[j] as number) < x) j++;
    if (j < b.length && (b[j] as number) === x) continue;
    out.push(x);
  }
}

/** Complement within [0, maxDocExclusive). */
export function complementSorted(
  a: Uint32Array,
  maxDocExclusive: number,
  out: DocIdBuf,
): void {
  out.reset();
  let prev = 0;
  for (let i = 0; i < a.length; i++) {
    const x = a[i] as number;
    for (let v = prev; v < x; v++) out.push(v);
    prev = x + 1;
  }
  for (let v = prev; v < maxDocExclusive; v++) out.push(v);
}
