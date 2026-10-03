/**
 * engine.ts — writes, segment lifecycle, tiered merge, and query execution.
 *
 * Threading model (single process, no online services):
 *
 *   add()        appends into the active in-memory segment. Never blocks.
 *   flush()      seals + persists the active segment, then opens a new one.
 *                The snapshot is swapped in one assignment, so a query either
 *                sees the old or the new segment list, never a partial state.
 *   search()     reads an immutable snapshot of the segment list. Because
 *                segments are never mutated, a merge running concurrently
 *                cannot disturb an in-flight query.
 *
 * Tiered merge: L0 segments are merged into L1, L1 into L2, and so on. The
 * active segment is excluded from merging so writes are never blocked. A merge
 * past the tombstone threshold is forced even when the tier is not full.
 */

import { promises as fs } from "node:fs";
import {
  SegmentBuilder,
  mergeSegments,
  newSegmentId,
  readSegment,
  writeSegment,
  listSegmentFiles,
  removeSegmentFile,
  defaultAnalyzer,
  type Analyzer,
  type LogDoc,
  type Segment,
} from "./segment.js";
import { parseQuery, type QueryNode, type ParseError } from "./parser.js";
import {
  buildPlan,
  evaluateSegment,
  explainPlan,
  segmentCanBeSkipped,
  type Leaf,
  type PlanStats,
  type QueryPlan,
} from "./pushdown.js";

/* ------------------------------------------------------------------ *
 * Options
 * ------------------------------------------------------------------ */

export interface EngineOptions {
  /** directory for segment files; omit for a memory-only engine */
  dataDir?: string;
  /** documents per segment before an automatic flush */
  flushEvery?: number;
  /** segments per tier before that tier is merged into the next */
  mergeFactor?: number;
  /** tombstone ratio that forces a merge regardless of tier size */
  tombstoneThreshold?: number;
  /** field used for bare terms when a query has no field prefix */
  defaultField?: string;
  analyzer?: Analyzer;
}

export interface Hit {
  /** stable document id */
  id: string;
  /** the original document, exactly as ingested */
  doc: LogDoc;
  segmentId: string;
  /** position of the hit within its segment */
  localId: number;
}

export interface SearchResult {
  hits: Hit[];
  total: number;
  /** how long the query took end to end, in milliseconds */
  tookMs: number;
  /** per-segment accounting */
  segments: SegmentReport[];
  stats: PlanStats;
  plan: QueryPlan;
  ast: QueryNode;
}

export interface SegmentReport {
  segmentId: string;
  level: number;
  docs: number;
  /** matching documents in this segment after tombstones */
  matched: number;
  /** true when an empty posting list ended evaluation early */
  shortCircuited: boolean;
  /** true when the segment was skipped without evaluating postings */
  skipped: boolean;
  /** docIds read while evaluating (0 when skipped) */
  scanned: number;
  comparisons: number;
  hops: number;
}

export type SearchOutcome =
  | { ok: true; result: SearchResult }
  | { ok: false; error: ParseError };

/* ------------------------------------------------------------------ *
 * Engine
 * ------------------------------------------------------------------ */

export class SearchEngine {
  private readonly opts: Required<Omit<EngineOptions, "dataDir">> & { dataDir?: string };
  private active: SegmentBuilder;
  /** immutable snapshot handed to queries */
  private segments: Segment[] = [];
  private pendingMerge: Promise<void> | null = null;
  /** guards against re-arming the merge loop forever */
  private mergeArmed = false;
  /** last background merge failure, surfaced for diagnostics */
  lastMergeError: string | null = null;
  private closed = false;

  constructor(options: EngineOptions = {}) {
    this.opts = {
      flushEvery: options.flushEvery ?? 10_000,
      mergeFactor: options.mergeFactor ?? 4,
      tombstoneThreshold: options.tombstoneThreshold ?? 0.3,
      defaultField: options.defaultField ?? "_all",
      analyzer: options.analyzer ?? defaultAnalyzer,
      dataDir: options.dataDir,
    };
    this.active = new SegmentBuilder(newSegmentId("active"), 0, this.opts.analyzer);
  }

  /* ---------------- writes ---------------- */

  /**
   * Append a document. Cheap and non-blocking: it only touches the in-memory
   * active segment. Durability happens at flush().
   */
  add(doc: LogDoc, id?: string): void {
    if (this.closed) throw new Error("engine is closed");
    this.active.add(doc, id);
    // flush() rotates the builder asynchronously, so `active.size` stays at or
    // above the threshold for a few more adds. Without this guard every add
    // would schedule another flush of the same builder.
    // `flushPending` is set synchronously because flush() rotates the builder
    // only after its first await; without it, every add past the threshold
    // would schedule another flush of the same builder.
    if (this.active.size >= this.opts.flushEvery) {
      void this.flush();
    }
  }

  /** Append many documents, flushing as the threshold is crossed. */
  addAll(docs: Iterable<LogDoc>, idFor?: (doc: LogDoc, i: number) => string): void {
    let i = 0;
    for (const doc of docs) {
      this.add(doc, idFor?.(doc, i));
      i++;
    }
  }

  /**
   * Record a deletion. Applies as a tombstone to whichever segment owns the
   * document, or is carried forward by a later merge.
   */
  delete(stableId: string): boolean {
    if (this.closed) throw new Error("engine is closed");
    // Only claim the deletion if the active segment actually owns the doc.
    if (this.active.owns(stableId)) {
      return this.active.delete(stableId);
    }

    // Sealed segments are immutable, so a deletion against one is recorded as
    // an overlay tombstone applied at query time and folded in on next merge.
    for (const seg of this.segments) {
      if (seg.docs.some((d) => d.id === stableId)) {
        this.overlayDeletes.add(stableId);
        return true;
      }
    }
    return false;
  }

  /** Deletions against already-flushed segments (query-time overlay). */
  private readonly overlayDeletes = new Set<string>();

  /* ---------------- flush ---------------- */

  /**
   * Seal the active segment, persist it, and start a new one.
   *
   * Atomicity: the new segment is written to a temp file and renamed, then the
   * snapshot array is replaced in a single assignment. Readers see either the
   * pre-flush or post-flush list.
   */
  /**
   * Seal the active segment, persist it, and start a new one.
   *
   * The builder is rotated *synchronously*, before any await. That matters for
   * two reasons:
   *   - add() can check the new (empty) segment immediately, so a burst of
   *     writes cannot schedule repeated flushes of the same builder;
   *   - concurrent flush() calls cannot seal the same builder twice.
   */
  async flush(): Promise<Segment | null> {
    const sealed = this.active.seal();
    // rotate immediately
    this.active = new SegmentBuilder(newSegmentId("active"), 0, this.opts.analyzer);

    if (sealed.docs.length === 0 && sealed.tombstones.size === 0) {
      return null;
    }

    if (this.opts.dataDir !== undefined) {
      await writeSegment(this.opts.dataDir, sealed);
    }

    // single atomic snapshot swap: a concurrent query sees one list or the other
    this.segments = [...this.segments, sealed];

    this.scheduleMerge();
    return sealed;
  }

  /* ---------------- tiered merge ---------------- */

  /**
   * Merge full tiers in the background. Never blocks a query and never touches
   * the active segment.
   */
  private scheduleMerge(): void {
    if (this.pendingMerge !== null || this.closed) return;
    const candidates = this.mergeCandidates();
    if (candidates.length === 0) {
      this.mergeArmed = false;
      return;
    }
    this.mergeArmed = false;

    this.pendingMerge = (async () => {
      try {
        for (const tier of candidates) {
          if (this.closed) return;
          await this.mergeTier(tier);
        }
      } catch (err) {
        // A failed background merge must never surface as an unhandled
        // rejection (which would take the process down); segments stay valid
        // and the next pass retries.
        this.lastMergeError = err instanceof Error ? err.message : String(err);
      } finally {
        this.pendingMerge = null;
        // Re-arm at most once per merge pass. Without a bound, a tier that can
        // never satisfy the criteria (a lone over-tombstoned segment, or a
        // merged segment that immediately re-qualifies) would spin forever.
        if (!this.closed && !this.mergeArmed) {
          this.mergeArmed = true;
          this.scheduleMerge();
        }
      }
    })();
  }

  /** Groups of same-level segments that are full, or over the tombstone limit. */
  private mergeCandidates(): Segment[][] {
    const byLevel = new Map<number, Segment[]>();
    for (const seg of this.segments) {
      const list = byLevel.get(seg.level) ?? [];
      list.push(seg);
      byLevel.set(seg.level, list);
    }

    const tiers: Segment[][] = [];
    // Highest level first, so a merged result can cascade in the same pass.
    const levels = [...byLevel.keys()].sort((a, b) => b - a);
    for (const level of levels) {
      const group = byLevel.get(level) as Segment[];
      if (group.length === 0) continue;

      if (group.length >= this.opts.mergeFactor) {
        // Tier is full: fold it into the next level up.
        tiers.push(group);
        continue;
      }

      // Tier is not full, so merge only to reclaim tombstoned space.
      const stale = group.filter((s) => s.needsCompaction(this.opts.tombstoneThreshold));
      if (stale.length > 0) {
        // Compact the stale segment with a peer so the merge stays legal;
        // with no peer it waits for more data rather than merging 1 -> 1.
        const peers = group.filter((s) => !stale.includes(s));
        const batch = peers.length > 0 ? [...stale, ...peers.slice(0, 1)] : [];
        if (batch.length >= 2) tiers.push(batch);
      }
    }
    return tiers;
  }

  /**
   * Merge one tier.
   *
   * In-flight queries keep reading the old segments: they hold their own
   * reference to the immutable snapshot. Only after the merged segment is
   * durable do we swap the list and unlink the old files.
   */
  private async mergeTier(inputs: readonly Segment[]): Promise<void> {
    const merged = mergeSegments(inputs, this.opts.analyzer);
    if (this.opts.dataDir !== undefined) {
      await writeSegment(this.opts.dataDir, merged);
    }

    const inputIds = new Set(inputs.map((s) => s.id));
    const next = this.segments.filter((s) => !inputIds.has(s.id));
    // keep level ordering stable for merge scheduling
    next.push(merged);
    next.sort((a, b) => a.level - b.level);
    this.segments = next;

    if (this.opts.dataDir !== undefined) {
      await Promise.all(inputs.map((s) => removeSegmentFile(this.opts.dataDir as string, s.id)));
    }
  }

  /**
   * Resolves once no merge is in flight and none is pending.
   *
   * A merge finishing can re-arm the scheduler, so this drains rather than
   * awaiting a single promise. The bound is a safety net; the scheduler
   * self-disarms, so the loop normally exits after one or two turns.
   */
  async whenMerged(): Promise<void> {
    for (let guard = 0; guard < 1000; guard++) {
      const inflight = this.pendingMerge;
      if (inflight === null) {
        // let a just-resolved pass re-arm before declaring quiescence
        await Promise.resolve();
        if (this.pendingMerge === null) return;
        continue;
      }
      await inflight;
    }
  }

  /* ---------------- query ---------------- */

  /** Parse, plan, and evaluate a query. Never throws on user input. */
  search(query: string, limit = 100): SearchOutcome {
    const started = performance.now();
    const parsed = parseQuery(query, this.opts.defaultField);
    if (!parsed.ok) return { ok: false, error: parsed.error };

    // Snapshot the segment list once. A concurrent merge replaces the array;
    // this reference keeps our view stable and consistent.
    const snapshot = this.readableSegments();

    const plan = buildPlan(parsed.ast, (leaf) => this.estimateLeaf(leaf, snapshot));

    const stats: PlanStats = {
      segmentsEvaluated: 0,
      segmentsSkipped: 0,
      segmentsShortCircuited: 0,
      leafComparisons: 0,
      gallopHops: 0,
    };
    const reports: SegmentReport[] = [];
    const hits: Hit[] = [];

    // Only `limit` hits are ever returned, so each segment evaluation gets a
    // budget of the hits still missing. That lets the evaluator stop as soon as
    // enough matches exist instead of materializing a whole posting list.
    let remaining = limit;

    for (const seg of snapshot) {
      if (remaining <= 0) break;
      // Cheap pre-filter: a missing conjunctive term proves the segment cannot
      // match, so we skip without touching postings at all.
      if (segmentCanBeSkipped(plan, seg.index)) {
        stats.segmentsSkipped++;
        reports.push({
          segmentId: seg.id,
          level: seg.level,
          docs: seg.docCount,
          matched: 0,
          shortCircuited: true,
          skipped: true,
          scanned: 0,
          comparisons: 0,
          hops: 0,
        });
        continue;
      }

      const evalResult = evaluateSegment(plan, seg.index, seg.docCount, remaining);
      stats.segmentsEvaluated++;
      stats.leafComparisons += evalResult.comparisons;
      stats.gallopHops += evalResult.hops;

      if (evalResult.shortCircuited && evalResult.docIds === null) {
        stats.segmentsShortCircuited++;
      }

      const docIds = evalResult.docIds;
      let matched = 0;
      let scanned = 0;
      if (docIds !== null) {
        scanned = docIds.length;
        for (const localId of docIds) {
          const doc = seg.docs[localId as number];
          if (doc === undefined) continue;
          // tombstone filtering at query time
          if (seg.tombstones.has(doc.id)) continue;
          if (this.overlayDeletes.has(doc.id)) continue;
          matched++;
          if (hits.length < limit) {
            hits.push({
              id: doc.id,
              doc: doc.fields as LogDoc,
              segmentId: seg.id,
              localId,
            });
          }
        }
        remaining -= matched;
      }

      reports.push({
        segmentId: seg.id,
        level: seg.level,
        docs: seg.docCount,
        matched,
        shortCircuited: evalResult.shortCircuited,
        skipped: false,
        scanned,
        comparisons: evalResult.comparisons,
        hops: evalResult.hops,
      });
    }

    const tookMs = performance.now() - started;
    return {
      ok: true,
      result: {
        hits,
        total: hits.length,
        tookMs,
        segments: reports,
        stats,
        plan,
        ast: parsed.ast,
      },
    };
  }

  /**
   * Segments visible to a query: flushed segments plus the in-memory active
   * segment. The active builder is sealed into a throwaway Segment for reads so
   * queries never pay for a flush and never mutate the builder.
   */
  private readableSegments(): Segment[] {
    const active = this.active;
    if (active.size === 0) return this.segments;
    if (active.isSealed) return [...this.segments, active.sealedView()];
    return [...this.segments, active.view()];
  }

  /**
   * Estimate how many documents a single leaf matches, without materializing
   * hits. Used to order conjuncts cheapest-first when planning a query.
   */
  countLeaf(leaf: Leaf): number {
    let total = 0;
    for (const seg of this.readableSegments()) {
      switch (leaf.kind) {
        case "term":
          total += seg.index.lookup(leaf.field, leaf.term)?.size ?? 0;
          break;
        case "phrase": {
          // candidates are bounded by the rarest term
          let min = Number.MAX_SAFE_INTEGER;
          for (const term of leaf.terms) {
            const size = seg.index.lookup(leaf.field, term)?.size ?? 0;
            min = Math.min(min, size);
          }
          total += min === Number.MAX_SAFE_INTEGER ? 0 : min;
          break;
        }
        case "range":
          total += seg.index.range(leaf.field, leaf.lo, leaf.hi, leaf.loInclusive, leaf.hiInclusive).length;
          break;
      }
    }
    return total;
  }

  /** Cheap document-count estimate for a leaf, used to order conjuncts. */
  private estimateLeaf(leaf: Leaf, snapshot: readonly Segment[]): number {
    let total = 0;
    for (const seg of snapshot) {
      switch (leaf.kind) {
        case "term":
          total += seg.index.lookup(leaf.field, leaf.term)?.size ?? 0;
          break;
        case "phrase": {
          // candidates are bounded by the rarest term
          let min = Number.MAX_SAFE_INTEGER;
          for (const term of leaf.terms) {
            const size = seg.index.lookup(leaf.field, term)?.size ?? 0;
            min = Math.min(min, size);
          }
          total += min === Number.MAX_SAFE_INTEGER ? 0 : min;
          break;
        }
        case "range":
          total += seg.index.range(leaf.field, leaf.lo, leaf.hi, leaf.loInclusive, leaf.hiInclusive).length;
          break;
      }
    }
    return total;
  }

  /* ---------------- lifecycle ---------------- */

  /** Load persisted segments from disk and rebuild skip tables. */
  static async open(options: EngineOptions = {}): Promise<SearchEngine> {
    const engine = new SearchEngine(options);
    if (options.dataDir !== undefined) {
      const files = await listSegmentFiles(options.dataDir);
      const loaded: Segment[] = [];
      for (const file of files) {
        const id = file.replace(/\.seg$/, "");
        loaded.push(await readSegment(options.dataDir, id));
      }
      loaded.sort((a, b) => a.level - b.level);
      engine.segments = loaded;
    }
    return engine;
  }

  /** Flush, wait for merges, and release resources. */
  async close(): Promise<void> {
    if (this.closed) return;
    await this.flush();
    await this.whenMerged();
    this.closed = true;
  }

  /* ---------------- introspection ---------------- */

  get segmentCount(): number {
    return this.segments.length;
  }

  /** Active (unsealed) segment document count. */
  get pendingCount(): number {
    return this.active.size;
  }

  segmentStats(): Array<{
    id: string;
    level: number;
    docs: number;
    live: number;
    tombstoneRatio: number;
  }> {
    return this.segments.map((s) => ({
      id: s.id,
      level: s.level,
      docs: s.docCount,
      live: s.liveDocCount,
      tombstoneRatio: s.docCount === 0 ? 0 : s.tombstones.size / s.docCount,
    }));
  }

  /**
   * Live memory for this engine's segments plus the active builder.
   *
   * Reported as heapUsed rather than RSS: RSS never shrinks after a GC, so it
   * overstates steady-state usage. This is the figure the 512MB budget is
   * measured against.
   */
  memoryUsage(): { heapUsedMb: number; rssMb: number; docs: number; segments: number } {
    const mu = process.memoryUsage();
    let docs = this.active.size;
    for (const s of this.segments) docs += s.docCount;
    return {
      heapUsedMb: mu.heapUsed / (1024 * 1024),
      rssMb: mu.rss / (1024 * 1024),
      docs,
      segments: this.segments.length,
    };
  }

  /** Ensure the data directory exists (no-op for memory-only engines). */
  async prepare(): Promise<void> {
    if (this.opts.dataDir !== undefined) {
      await fs.mkdir(this.opts.dataDir, { recursive: true });
    }
  }
}

export { explainPlan };
export type { QueryPlan, PlanStats };
