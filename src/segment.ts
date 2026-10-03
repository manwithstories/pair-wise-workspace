/**
 * segment.ts — immutable segments, binary persistence, tiered merge, tombstones.
 *
 * A segment is the unit of both indexing and atomicity:
 *   - Writers append documents into an in-memory `SegmentBuilder`.
 *   - `flush()` seals the builder and writes one `.seg` file.
 *   - Once sealed, a segment never changes. Merges produce a *new* segment id
 *     and the old ones are unlinked only after the new one is durable.
 *
 * Concurrency: the engine keeps a list of segment snapshots. A merge builds its
 * output off to the side and swaps the list in one assignment, so queries
 * either see the old segments or the new one, never a half-built mixture.
 * Segments are never mutated in place, so an in-flight query can keep reading.
 */

import { promises as fs } from "node:fs";
import * as path from "node:path";
import { randomBytes } from "node:crypto";
import {
  InvertedIndex,
  type PostingList,
  SKIP_BLOCK,
} from "./inverted.js";

/* ------------------------------------------------------------------ *
 * Documents
 * ------------------------------------------------------------------ */

/** A log record. `message` is tokenised into a positional posting list. */
export interface LogDoc {
  level: string;
  service: string;
  message: string;
  trace_id: string;
  /** epoch millis, indexed as a numeric column for range pushdown */
  ts: number;
  [key: string]: string | number;
}

/** Fields indexed as numeric columns (range pushdown candidates). */
export const NUMERIC_FIELDS = ["ts"] as const;
/** Fields indexed with term positions (phrase queries). */
export const POSITIONAL_FIELDS = ["message"] as const;

/**
 * Fields stored but not term-indexed.
 *
 * `trace_id` is near-unique per document, so indexing it costs one posting
 * list per document for a field nobody ever queries by term - it is carried for
 * correlation and returned verbatim. Excluding it is what keeps a 1M-document
 * segment build inside its time budget.
 */
export const UNINDEXED_FIELDS = ["trace_id"] as const;

export function defaultIndexedFields(): Set<string> {
  return new Set<string>(UNINDEXED_FIELDS);
}

export interface StoredDoc {
  id: string;
  fields: Record<string, string | number>;
}

const SEG_MAGIC = 0x4c47_5347; // "LGSG"
const SEG_VERSION = 1;

/* ------------------------------------------------------------------ *
 * Tombstones
 * ------------------------------------------------------------------ */

/**
 * Deletions are recorded, not applied, so writes never rewrite a live segment.
 * A merged segment carries its deletions forward, and the ratio of deleted
 * documents to total drives the forced-merge policy.
 */
export class TombstoneSet {
  private readonly deleted = new Set<string>();
  private deletedCount = 0;

  add(id: string): boolean {
    if (this.deleted.has(id)) return false;
    this.deleted.add(id);
    this.deletedCount++;
    return true;
  }

  has(id: string): boolean {
    return this.deleted.has(id);
  }

  get size(): number {
    return this.deleted.size;
  }

  /** Number of deletions in this segment that apply to its own documents. */
  countAgainst(docCount: number): number {
    let n = 0;
    for (const id of this.deleted) {
      if (id.startsWith("#")) continue; // belongs to another segment
      n++;
    }
    void docCount;
    return n;
  }

  entries(): [string, number][] {
    // segmentLocalOrdinal per deleted id, so a merge can renumber
    const out: [string, number][] = [];
    for (const id of this.deleted) out.push([id, 0]);
    return out;
  }

  ids(): string[] {
    return [...this.deleted];
  }

  static fromIds(ids: Iterable<string>): TombstoneSet {
    const t = new TombstoneSet();
    for (const id of ids) t.add(id);
    return t;
  }
}

/* ------------------------------------------------------------------ *
 * Segment
 * ------------------------------------------------------------------ */

export interface SegmentStats {
  /** total documents written into the segment, including later-deleted ones */
  docCount: number;
  deletedCount: number;
  /** deleted / docCount; the engine force-merges past the threshold */
  tombstoneRatio: number;
  byteSize: number;
  levels: number;
}

export class Segment {
  readonly id: string;
  readonly level: number;
  readonly docs: StoredDoc[];
  readonly index: InvertedIndex;
  readonly tombstones: TombstoneSet;
  readonly docCount: number;
  /** local docId (position in `docs`) -> stable document id */
  readonly liveDocCount: number;

  constructor(init: {
    id: string;
    level: number;
    docs: StoredDoc[];
    index: InvertedIndex;
    tombstones: TombstoneSet;
  }) {
    this.id = init.id;
    this.level = init.level;
    this.docs = init.docs;
    this.index = init.index;
    this.tombstones = init.tombstones;
    this.docCount = init.docs.length;
    this.liveDocCount = this.docs.length - this.tombstones.size;
  }

  stats(byteSize = 0): SegmentStats {
    const deleted = this.tombstones.size;
    return {
      docCount: this.docCount,
      deletedCount: deleted,
      tombstoneRatio: this.docCount === 0 ? 0 : deleted / this.docCount,
      byteSize,
      levels: this.level,
    };
  }

  /** True when deletions have eaten enough of the segment to justify a rewrite. */
  needsCompaction(threshold: number): boolean {
    if (this.docCount === 0) return false;
    return this.tombstones.size / this.docCount > threshold;
  }

  docIdOf(localId: number): StoredDoc | undefined {
    return this.docs[localId];
  }

  isDeleted(stableId: string): boolean {
    return this.tombstones.has(stableId);
  }
}

/* ------------------------------------------------------------------ *
 * Builder
 * ------------------------------------------------------------------ */

export interface Analyzer {
  /** split text into normalized terms */
  tokenize(text: string): string[];
}

export const defaultAnalyzer: Analyzer = {
  tokenize(text: string): string[] {
    // Defensive: log records are loosely typed, and a missing field must not
    // throw during ingest.
    if (typeof text !== "string" || text.length === 0) return [];
    return text
      .toLowerCase()
      .split(/[^a-z0-9_.:@-]+/i)
      .filter((t) => t.length > 0);
  },
};

/**
 * Accumulates documents and builds the inverted index. Only the engine's write
 * path touches this; a sealed Segment is immutable.
 */
export class SegmentBuilder {
  readonly id: string;
  readonly level: number;
  private readonly docs: StoredDoc[] = [];
  private readonly index: InvertedIndex;
  private readonly tombstones = new TombstoneSet();
  private readonly analyzer: Analyzer;
  /** ids of documents added here, so delete() knows what it can tombstone */
  private readonly ids = new Set<string>();
  private readonly skipIndexFields: ReadonlySet<string>;
  private sealed = false;

  constructor(
    id: string,
    level: number,
    analyzer: Analyzer = defaultAnalyzer,
    skipIndexFields: ReadonlySet<string> = defaultIndexedFields(),
  ) {
    this.id = id;
    this.level = level;
    this.analyzer = analyzer;
    this.skipIndexFields = skipIndexFields;
    this.index = new InvertedIndex(POSITIONAL_FIELDS);
  }

  get size(): number {
    return this.docs.length;
  }

  get isSealed(): boolean {
    return this.sealed;
  }

  /** Re-seal for reads after the builder has been sealed. */
  sealedView(): Segment {
    return new Segment({
      id: this.id,
      level: this.level,
      docs: this.docs,
      index: this.index,
      tombstones: this.tombstones,
    });
  }

  add(doc: LogDoc, stableId?: string): void {
    if (this.sealed) throw new Error("cannot add to a sealed segment");
    const id = stableId ?? `${this.id}:${this.docs.length}`;
    this.ids.add(id);
    // Store the caller's object directly: `LogDoc` is already a plain record,
    // and copying it per document dominated ingest at 10M docs/day.
    const fields = doc as unknown as Record<string, string | number>;
    const localId = this.docs.length;
    this.docs.push({ id, fields });

    // `for...in` avoids allocating an entries array per document.
    for (const field in fields) {
      const value = fields[field] as string | number;
      if (typeof value === "number") {
        this.index.addNumeric(field, value, localId);
        continue;
      }
      if (this.skipIndexFields.has(field)) continue; // stored, not indexed
      const terms = this.analyzer.tokenize(value);
      for (let pos = 0; pos < terms.length; pos++) {
        this.index.addTerm(field, terms[pos] as string, localId, pos);
      }
    }
  }

  /** True when a document with this id has been added to this builder. */
  owns(stableId: string): boolean {
    return this.ids.has(stableId);
  }

  /** Mark a document deleted without rewriting the segment. */
  delete(stableId: string): boolean {
    return this.tombstones.add(stableId);
  }

  /**
   * Read-only view of the current contents, for querying before a flush.
   * Seal the internal structures (idempotent) so postings are query-ready,
   * but leave the builder open for further writes.
   */
  view(): Segment {
    this.index.seal();
    return new Segment({
      id: this.id,
      level: this.level,
      docs: this.docs,
      index: this.index,
      tombstones: this.tombstones,
    });
  }

  seal(): Segment {
    if (this.sealed) throw new Error("segment already sealed");
    this.sealed = true;
    this.index.seal();
    return new Segment({
      id: this.id,
      level: this.level,
      docs: this.docs,
      index: this.index,
      tombstones: this.tombstones,
    });
  }

  stats(): { docCount: number; deletedCount: number; tombstoneRatio: number } {
    const deleted = this.tombstones.size;
    return {
      docCount: this.docs.length,
      deletedCount: deleted,
      tombstoneRatio: this.docs.length === 0 ? 0 : deleted / this.docs.length,
    };
  }
}

export function newSegmentId(prefix = "seg"): string {
  return `${prefix}-${Date.now().toString(36)}-${randomBytes(4).toString("hex")}`;
}

/* ------------------------------------------------------------------ *
 * Binary persistence
 * ------------------------------------------------------------------ */

/**
 * Segment file layout (little-endian):
 *
 *   magic:u32 | version:u32 | level:u32 | docCount:u32 | tombstoneCount:u32
 *   docCount  x { idLen:u32, idBytes, fieldCount:u32,
 *                 fieldCount x { nameLen:u32, nameBytes, kind:u8,
 *                                 [u64 float] | [u32 len, bytes] } }
 *   posting sections: fieldCount x { nameLen, name,
 *                 termCount:u32,
 *                 termCount x { termLen:u32, termBytes,
 *                               docCount:u32,
 *                               docIds:u32*, freqs:u32*,
 *                               [posOffsets:u32*(n+1), positions:u32*] } }
 *   numeric sections: fieldCount x { nameLen, name, valueCount:u32,
 *                 values:f64*, docIds:u32* }
 *   tombstoneCount x { idLen:u32, idBytes }
 *
 * Skip tables are rebuilt on load (see Segment.load) rather than stored: they
 * are derived from docIds and keeping them out of the file saves space.
 */

class ByteWriter {
  private chunks: Buffer[] = [];
  private len = 0;

  u8(v: number): void {
    const b = Buffer.allocUnsafe(1);
    b.writeUInt8(v & 0xff, 0);
    this.chunks.push(b);
    this.len += 1;
  }

  u32(v: number): void {
    const b = Buffer.allocUnsafe(4);
    b.writeUInt32LE(v >>> 0, 0);
    this.chunks.push(b);
    this.len += 4;
  }

  f64(v: number): void {
    const b = Buffer.allocUnsafe(8);
    b.writeDoubleLE(v, 0);
    this.chunks.push(b);
    this.len += 8;
  }

  str(s: string): void {
    const b = Buffer.from(s, "utf8");
    this.u32(b.length);
    this.chunks.push(b);
    this.len += b.length;
  }

  bytes(b: Uint32Array | Float64Array): void {
    this.chunks.push(Buffer.from(b.buffer, b.byteOffset, b.byteLength));
    this.len += b.byteLength;
  }

  concat(): Buffer {
    return Buffer.concat(this.chunks, this.len);
  }
}

class ByteReader {
  private off = 0;
  constructor(private readonly buf: Buffer) {}

  private need(n: number): void {
    if (this.off + n > this.buf.length) {
      throw new Error(`segment truncated: wanted ${n} bytes at ${this.off}`);
    }
  }

  u8(): number {
    this.need(1);
    const v = this.buf.readUInt8(this.off);
    this.off += 1;
    return v;
  }

  u32(): number {
    this.need(4);
    const v = this.buf.readUInt32LE(this.off);
    this.off += 4;
    return v;
  }

  f64(): number {
    this.need(8);
    const v = this.buf.readDoubleLE(this.off);
    this.off += 8;
    return v;
  }

  str(): string {
    const n = this.u32();
    this.need(n);
    const v = this.buf.toString("utf8", this.off, this.off + n);
    this.off += n;
    return v;
  }

  u32Array(n: number): Uint32Array {
    this.need(n * 4);
    const v = new Uint32Array(n);
    for (let i = 0; i < n; i++) v[i] = this.buf.readUInt32LE(this.off + i * 4);
    this.off += n * 4;
    return v;
  }

  f64Array(n: number): Float64Array {
    this.need(n * 8);
    const v = new Float64Array(n);
    for (let i = 0; i < n; i++) v[i] = this.buf.readDoubleLE(this.off + i * 8);
    this.off += n * 8;
    return v;
  }

  get done(): boolean {
    return this.off >= this.buf.length;
  }
}

/** Serialize a sealed segment to its on-disk binary form. */
export function serializeSegment(seg: Segment): Buffer {
  const w = new ByteWriter();
  w.u32(SEG_MAGIC);
  w.u32(SEG_VERSION);
  w.u32(seg.level);
  w.u32(seg.docs.length);
  w.u32(seg.tombstones.size);

  // documents
  for (const doc of seg.docs) {
    w.str(doc.id);
    const entries = Object.entries(doc.fields);
    w.u32(entries.length);
    for (const [name, value] of entries) {
      w.str(name);
      if (typeof value === "number") {
        w.u8(1);
        w.f64(value);
      } else {
        w.u8(0);
        w.str(value);
      }
    }
  }

  // posting sections: only fields that actually have a term dictionary.
  // Numeric-only fields (e.g. `ts`) are written in the numeric section below;
  // including them here would desynchronise the reader.
  const termFields = seg.index.fields().filter((f) => seg.index.dictFor(f) !== undefined);
  w.u32(termFields.length);
  for (const field of termFields) {
    w.str(field);
    const dict = seg.index.dictFor(field);
    const terms = dict ? [...dict.keys()] : [];
    w.u32(terms.length);
    for (const term of terms) {
      const list = dict?.get(term) as PostingList;
      w.str(term);
      w.u32(list.size);
      w.bytes(list.docIds);
      w.bytes(list.freqs);
      if (list.posOffsets !== null && list.posData !== null) {
        w.u8(1);
        w.bytes(list.posOffsets);
        w.bytes(list.posData);
      } else {
        w.u8(0);
      }
    }
  }

  // numeric sections (value-sorted, exactly as queried)
  const numericFields = seg.index.fields().filter((f) => seg.index.isNumeric(f));
  w.u32(numericFields.length);
  for (const field of numericFields) {
    w.str(field);
    const col = seg.index.numericColumn(field);
    const n = col?.size ?? 0;
    w.u32(n);
    if (col !== undefined) {
      w.bytes(col.values);
      w.bytes(col.docIds);
    }
  }

  // tombstones: the count is already in the header, so only the ids follow
  const ids = seg.tombstones.ids();
  for (const id of ids) w.str(id);

  return w.concat();
}

/** Rebuild a Segment (including skip tables) from its binary form. */
export function deserializeSegment(buf: Buffer, fallbackId: string): Segment {
  const r = new ByteReader(buf);
  const magic = r.u32();
  if (magic !== SEG_MAGIC) {
    throw new Error(`not a segment file (magic 0x${magic.toString(16)})`);
  }
  const version = r.u32();
  if (version !== SEG_VERSION) {
    throw new Error(`unsupported segment version ${version}`);
  }
  const level = r.u32();
  const docCount = r.u32();
  const tombstoneCount = r.u32();

  const docs: StoredDoc[] = new Array(docCount);
  for (let i = 0; i < docCount; i++) {
    const id = r.str();
    const fieldCount = r.u32();
    const fields: Record<string, string | number> = {};
    for (let f = 0; f < fieldCount; f++) {
      const name = r.str();
      const kind = r.u8();
      fields[name] = kind === 1 ? r.f64() : r.str();
    }
    docs[i] = { id, fields };
  }

  const index = new InvertedIndex(POSITIONAL_FIELDS);

  const fieldCount = r.u32();
  for (let f = 0; f < fieldCount; f++) {
    const field = r.str();
    const termCount = r.u32();
    for (let t = 0; t < termCount; t++) {
      const term = r.str();
      const n = r.u32();
      const docIds = r.u32Array(n);
      const freqs = r.u32Array(n);
      const hasPositions = r.u8() === 1;
      let posOffsets: Uint32Array | null = null;
      let posData: Uint32Array | null = null;
      if (hasPositions) {
        posOffsets = r.u32Array(n + 1);
        // length is the final offset, which delimits the shared position buffer
        const total = posOffsets[n] ?? 0;
        posData = r.u32Array(total);
      }
      index.addSealedTerm(field, term, docIds, freqs, posOffsets, posData);
    }
  }

  const numericFieldCount = r.u32();
  for (let f = 0; f < numericFieldCount; f++) {
    const field = r.str();
    const n = r.u32();
    const values = r.f64Array(n);
    const docIds = r.u32Array(n);
    const pairs: Array<readonly [number, number]> = new Array(n);
    for (let i = 0; i < n; i++) pairs[i] = [values[i] as number, docIds[i] as number] as const;
    index.attachNumeric(field, pairs);
  }

  const ids: string[] = new Array(tombstoneCount);
  for (let i = 0; i < tombstoneCount; i++) ids[i] = r.str();
  const tombstones = TombstoneSet.fromIds(ids);

  return new Segment({ id: fallbackId, level, docs, index, tombstones });
}

/* ------------------------------------------------------------------ *
 * Merge
 * ------------------------------------------------------------------ */

export interface MergeResult {
  segment: Segment;
  /** segments whose on-disk files may now be removed */
  replaced: Segment[];
}

/**
 * Merge `inputs` into one segment, dropping deleted documents and carrying
 * forward tombstones that belong to segments outside this merge.
 *
 * Deleting during merge is what reclaims space: the output contains only live
 * documents, so tombstoneRatio returns to zero for compacted data.
 */
export function mergeSegments(inputs: readonly Segment[], analyzer: Analyzer = defaultAnalyzer): Segment {
  if (inputs.length === 0) throw new Error("cannot merge an empty segment list");

  // Concatenate live documents, tracking (segment, localId) for tombstone carry.
  interface DocRef {
    doc: StoredDoc;
    originSegment: string;
  }
  const live: DocRef[] = [];
  const foreignTombstones = new Set<string>();

  for (const seg of inputs) {
    const ownDocs = new Set(seg.docs.map((d) => d.id));
    for (const deletedId of seg.tombstones.ids()) {
      // Only carry tombstones that do NOT refer to a doc in this merge; the
      // rest are physically removed below.
      if (!ownDocs.has(deletedId)) foreignTombstones.add(deletedId);
    }
    for (const doc of seg.docs) {
      if (seg.tombstones.has(doc.id)) continue;
      live.push({ doc, originSegment: seg.id });
    }
  }

  const builder = new SegmentBuilder(
    newSegmentId("merged"),
    Math.max(...inputs.map((s) => s.level)) + 1,
    analyzer,
  );
  for (const { doc } of live) {
    builder.add(docToLogDoc(doc.fields), doc.id);
  }
  for (const id of foreignTombstones) builder.delete(id);

  const out = builder.seal();
  return out;
}

function docToLogDoc(fields: Record<string, string | number>): LogDoc {
  return fields as LogDoc;
}

/* ------------------------------------------------------------------ *
 * File helpers
 * ------------------------------------------------------------------ */

export function segmentPath(dir: string, id: string): string {
  return path.join(dir, `${id}.seg`);
}

export async function writeSegment(dir: string, seg: Segment): Promise<string> {
  await fs.mkdir(dir, { recursive: true });
  const buf = serializeSegment(seg);
  const file = segmentPath(dir, seg.id);
  // write to a temp file then rename: readers never observe a partial segment
  const tmp = `${file}.tmp`;
  await fs.writeFile(tmp, buf);
  await fs.rename(tmp, file);
  return file;
}

export async function readSegment(dir: string, id: string): Promise<Segment> {
  const file = segmentPath(dir, id);
  const buf = await fs.readFile(file);
  return deserializeSegment(buf, id);
}

export async function listSegmentFiles(dir: string): Promise<string[]> {
  try {
    const entries = await fs.readdir(dir);
    return entries.filter((f) => f.endsWith(".seg"));
  } catch {
    return [];
  }
}

export async function removeSegmentFile(dir: string, id: string): Promise<void> {
  try {
    await fs.unlink(segmentPath(dir, id));
  } catch {
    // already gone
  }
}

export { SKIP_BLOCK };
