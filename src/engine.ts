/**
 * src/engine.ts —— 引擎：串联构建、查询、持久化与并发
 *
 * 数据流：
 *   写入  -> 内存缓冲（doc buffer）-> flush 成不可变段 -> 落盘（原子 rename）
 *   查询  -> 词法/语法 -> AST -> 下推计划 -> 逐段短路求值 -> 取原文
 *
 * 并发模型（本文件的核心设计）：
 *
 *   1. 段不可变 + 两阶段分离：段一旦 build 完成就不再修改，查询读它无需任何锁。
 *      写入线程只往"内存缓冲"追加，不碰已生效的段。
 *
 *   2. 视图原子替换：`this.segments` 是一个数组引用。合并完成时整体替换引用，
 *      而不是逐个修改数组元素。替换是单次赋值，JS 单线程下天然原子。
 *
 *   3. 合并中查询不等待：查询开始时 `const snapshot = this.segments` 拍下引用，
 *      之后无论合并完成多少次，这份快照一直有效（旧段对象由 GC 回收）。
 *      因此合并与查询完全解耦，不需要读写锁，也不需要等锁超时。
 *
 *   4. 查询之间互不阻塞：查询全程只读共享的段数据，所有中间缓冲（scratch buffer）
 *      都是每查询私有的。因此 10 路并发查询不会互相等待。
 */

import path from "node:path";
import {
  FIELD_PHRASE_SUFFIX,
  buildSkipList,
  makePosting,
  skipStrideFor,
  type InvertedIndex,
  type NumericFieldIndex,
  type Posting,
} from "./inverted.ts";
import { DEFAULT_TEXT_FIELDS, DocStore, Segment, pickMergeTier, tokenizeText, type LogDocument } from "./segment.ts";
import { parseQuery, type ParsedQuery } from "./parser.ts";
import {
  EvalArena,
  buildPlan,
  canSkipSegment,
  describePlan,
  evaluatePlan,
  skipReason,
  type EvalStats,
  type PlanNode,
} from "./pushdown.ts";
import { listSegmentFiles, readSegment, removeSegmentFile, writeSegment, type FieldEntry, type SegmentHeader } from "./persist.ts";

/* ------------------------------------------------------------------ *
 * 引擎配置
 * ------------------------------------------------------------------ */

export interface EngineOptions {
  /** 段目录；传 null 表示纯内存模式（不落盘）。 */
  dataDir?: string | null;
  /** 声明为数值型的字段，走范围索引。 */
  numericFields?: string[];
  /**
   * 需要建词级索引的文本字段（裸词/裸短语的检索范围）。
   * 默认只有 `message`；把 `trace_id` 之类高基数字段加进来会显著增大索引体积。
   */
  textFields?: string[];
  /** 内存缓冲攒够多少文档就 flush 成新段。 */
  flushThreshold?: number;
  /** tiered 合并的层内最小段数。 */
  mergeRunLength?: number;
  /** tombstone 占比超过该值强制合并。 */
  tombstoneThreshold?: number;
  /** 单次查询返回上限，防止一次查询把内存打爆。 */
  maxHits?: number;
  /**
   * tombstone 超阈值时自动触发后台合并（默认开）。
   * 关掉它可以由调用方显式控制合并时机（演示脚本用这个做确定性输出）。
   */
  autoMerge?: boolean;
}

export interface QueryOptions {
  limit?: number;
  /** 收集逐段统计（演示"逐段命中与短路跳过"用）。 */
  explain?: boolean;
}

export interface SegmentReport {
  generation: number;
  docCount: number;
  liveCount: number;
  /** 本段命中数（已扣 tombstone）。 */
  matched: number;
  /** 是否因剪枝整段跳过。 */
  skipped: boolean;
  /** 剪枝原因。 */
  reason: string | null;
  /** 本段求值耗时（毫秒）。 */
  elapsedMs: number;
}

export interface SearchResult {
  query: string;
  parsed: ParsedQuery;
  plan: PlanNode;
  planText: string;
  /** 命中文档原文。 */
  hits: Array<Record<string, unknown>>;
  /** 命中总数（受 limit 限制前的真实值）。 */
  totalHits: number;
  /** 逐段统计。 */
  segments: SegmentReport[];
  /** 被整段跳过的段数（短路收益）。 */
  skippedSegments: number;
  /** block-max 整块跳过的次数（posting 层短路收益）。 */
  blocksSkipped: number;
  /** posting 逐元素比较次数。 */
  comparisons: number;
  /** 解析 + 计划 + 求值总耗时（毫秒）。 */
  elapsedMs: number;
  parseMs: number;
  evalMs: number;
}

/* ------------------------------------------------------------------ *
 * 内部：已落盘的段引用
 * ------------------------------------------------------------------ */

interface LoadedSegment {
  segment: Segment;
  /** 段文件名，合并回收时按名删除。 */
  file: string | null;
}

/* ------------------------------------------------------------------ *
 * 引擎
 * ------------------------------------------------------------------ */

export class SearchEngine {
  /** 已生效的段视图。整体替换引用，绝不原地改。 */
  private segments: LoadedSegment[] = [];
  /** 待 flush 的内存文档缓冲。 */
  private buffer: LogDocument[] = [];
  /** 下一个段序号。 */
  private nextGeneration = 0;
  /** 正在进行的合并（用于观测，不阻塞查询）。 */
  private merging: { sources: number[]; generation: number } | null = null;

  private readonly dataDir: string | null;
  private readonly numericFields: Set<string>;
  private readonly textFields: Set<string> | undefined;
  private readonly flushThreshold: number;
  private readonly mergeRunLength: number;
  private readonly tombstoneThreshold: number;
  private readonly maxHits: number;
  private readonly autoMerge: boolean;
  /**
   * 逻辑 id -> 所在段，用于删除定位。
   *
   * 只索引**非 id 字段之外**的文档：每个 id 一条 Map 项在百万规模下是几十 MB，
   * 而删除是低频操作。段内已有 docId -> id 的映射，缺失时退化为按段线性扫。
   */
  private readonly location = new Map<string, { generation: number; docId: number }>();
  /** 段内 id 的均值长度，线性扫退化时的跳过阈值。 */
  private static readonly LOCATION_INDEX_MAX = 200_000;

  constructor(options: EngineOptions = {}) {
    this.dataDir = options.dataDir ?? null;
    this.numericFields = new Set(options.numericFields ?? []);
    this.textFields = new Set(options.textFields ?? DEFAULT_TEXT_FIELDS);
    this.flushThreshold = options.flushThreshold ?? 1000;
    this.mergeRunLength = options.mergeRunLength ?? 4;
    this.tombstoneThreshold = options.tombstoneThreshold ?? 0.3;
    this.maxHits = options.maxHits ?? 10_000;
    this.autoMerge = options.autoMerge ?? true;
  }

  /* ---------------- 写入 ---------------- */

  /**
   * 追加文档。只写内存缓冲，绝不触碰已生效的段 —— 写入不阻塞查询。
   * 达到阈值时自动 flush 成新段。
   */
  async index(docs: Iterable<LogDocument>): Promise<void> {
    for (const doc of docs) {
      if (doc.id === undefined || doc.id === "") throw new Error("文档缺少 id");
      this.buffer.push(doc);
    }
    if (this.buffer.length >= this.flushThreshold) await this.flush();
  }

  /**
   * 追加文档并立即 flush 成新段（每条一个段）。
   *
   * 这是千万级日志的主写入路径：`index()` 的语义是"攒够一批再 flush"，
   * 而实时日志是来一条就要能查。逐条成段意味着 segment 数量会涨得很快，
   * 所以生产上通常配 flushThreshold 更大的批量模式，或者接受后台合并来消化小段。
   */
  async add(doc: LogDocument): Promise<Segment | null> {
    if (doc.id === undefined || doc.id === "") throw new Error("文档缺少 id");
    this.buffer.push(doc);
    if (this.buffer.length >= this.flushThreshold) return this.flush();
    return this.flush();
  }

  /** 把内存缓冲刷成一个新的不可变段，并原子加入段视图。 */
  async flush(): Promise<Segment | null> {
    if (this.buffer.length === 0) return null;
    const docs = this.buffer;
    // 先断开引用再构建：Segment.build 期间 docs 数组仍可达，
    // 但 flush 结束后 buffer 不再持有它，原始文档就能被 GC 回收。
    this.buffer = [];

    const generation = this.nextGeneration++;
    const segment = Segment.build({
      docs,
      generation,
      numericFields: this.numericFields,
      textFields: this.textFields,
    });

    const file = await this.persist(segment);
    // 原子替换：先算好新数组，再一次性赋值，读侧要么看到旧视图要么看到新视图
    this.segments = [...this.segments, { segment, file }];
    this.indexLocation(segment);
    return segment;
  }

  /**
   * 删除：只写 tombstone，段本身一个字节都不动 —— 因此删除不会阻塞查询。
   *
   * 落地时机是下一次合并：`mergeSegments()` 读 pendingTombstones，
   * 把命中文档从新段里物理剔除，顺带回收空间。
   *
   * @returns 实际命中并标记的 id 数量（不存在的 id 会被忽略）
   */
  async delete(ids: string[]): Promise<number> {
    if (ids.length === 0) return 0;
    let marked = 0;

    for (const id of ids) {
      const loc = this.locate(id);
      if (loc !== undefined) {
        let set = this.pendingTombstones.get(loc.generation);
        if (set === undefined) {
          set = new Set<number>();
          this.pendingTombstones.set(loc.generation, set);
        }
        if (!set.has(loc.docId)) {
          set.add(loc.docId);
          marked++;
        }
      }
    }

    // 还在内存缓冲里、尚未成段的文档可以直接剔除（省掉一次合并）
    const wanted = new Set(ids);
    const before = this.buffer.length;
    this.buffer = this.buffer.filter((d) => !wanted.has(d.id));
    marked += before - this.buffer.length;

    // tombstone 超阈值时后台强制合并，回收物理空间（不 await，写入路径不阻塞）
    if (this.autoMerge && this.shouldForceMerge()) void this.mergeSegments();

    return marked;
  }

  /** 待落地的 tombstone：generation -> 被删 docId 集合。 */
  private pendingTombstones = new Map<number, Set<number>>();

  private hasPendingTombstones(): boolean {
    for (const set of this.pendingTombstones.values()) {
      if (set.size > 0) return true;
    }
    return false;
  }

  private shouldForceMerge(): boolean {
    for (const loaded of this.segments) {
      const extra = this.pendingTombstones.get(loaded.segment.generation)?.size ?? 0;
      const total = loaded.segment.meta.docCount;
      if (total > 0 && (loaded.segment.deleted.length + extra) / total >= this.tombstoneThreshold) return true;
    }
    return false;
  }

  /* ---------------- 合并 ---------------- */

  /**
   * tiered 后台合并。
   *
   * 关键：合并只**新增**一个新段，最后一次性替换 segments 引用。
   * 在途查询持有的旧快照继续指向旧段，完全不受影响 —— 这就是"合并中查询不等待"。
   */
  async mergeSegments(): Promise<{ merged: number[]; into: number; reason: string } | null> {
    const current = this.segments;
    if (current.length === 0) return null;

    // 段数不足但有大量待落地 tombstone时，仍然合并一次单个段来回收空间
    if (current.length < 2 && !this.hasPendingTombstones()) return null;

    const live = current.map((l) => l.segment);
    const decision = pickMergeTier(live, {
      minRunLength: this.mergeRunLength,
      tombstoneThreshold: this.tombstoneThreshold,
    });
    if (decision.merge.length < 1) {
      // tiered 决策认为不需要合并，但若存在待落地 tombstone，仍需合并以落地删除
      const withPending = current.filter((l) => this.pendingTombstones.has(l.segment.generation));
      if (withPending.length === 0) return null;
      decision.merge = withPending.map((l) => l.segment);
      decision.reason = "落地待处理 tombstone";
    }

    const sources = decision.merge.map((s) => s.generation);
    this.merging = { sources, generation: this.nextGeneration };

    // 把待落地 tombstone 汇总成"外部 id"集合，交给 merge 应用
    const deletedIds = new Set<string>();
    for (const seg of decision.merge) {
      const extra = this.pendingTombstones.get(seg.generation);
      if (extra === undefined) continue;
      for (const docId of extra) {
        const id = seg.ids[docId];
        if (id !== undefined) deletedIds.add(id);
      }
    }

    const generation = this.nextGeneration++;
    const merged = Segment.merge({
      segments: decision.merge,
      extraDocs: this.buffer,
      generation,
      numericFields: this.numericFields,
      textFields: this.textFields,
      extraDeleted: deletedIds,
    });

    const file = await this.persist(merged);

    // 原子替换视图：被合并的旧段整批换成一个新段
    const mergedSet = new Set(sources);
    const survivors = current.filter((l) => !mergedSet.has(l.segment.generation));
    const next: LoadedSegment[] = [...survivors, { segment: merged, file }];
    // 一次赋值完成切换；读侧要么全旧要么全新，不会看到中间态
    this.segments = next;

    // 回收旧段文件（物理空间回收）。旧段对象仍被在途查询引用，不主动销毁。
    await Promise.all(
      current
        .filter((l) => mergedSet.has(l.segment.generation))
        .map((l) => (l.file === null ? Promise.resolve() : removeSegmentFile(this.dataDir!, l.file))),
    );

    // 清理已消费的 tombstone 与 location
    for (const gen of sources) this.pendingTombstones.delete(gen);
    this.rebuildLocation();

    this.merging = null;
    return { merged: sources, into: generation, reason: decision.reason };
  }

  /* ---------------- 查询 ---------------- */

  /**
   * 查询主入口。
   *
   * 拍下段视图快照后逐段求值：合并无论何时完成，这份快照都保持有效。
   */
  async search(query: string, options: QueryOptions = {}): Promise<SearchResult> {
    const t0 = performance.now();

    const parsed = parseQuery(query);
    const tParsed = performance.now();

    const plan = buildPlan(parsed.ast, { numericFields: this.numericFields });
    const tPlanned = performance.now();

    const snapshot = this.segments; // ← 视图快照：后续合并不影响本次查询
    const reports: SegmentReport[] = [];
    const hits: Array<Record<string, unknown>> = [];
    const limit = options.limit ?? this.maxHits;
    let totalHits = 0;
    let skippedSegments = 0;
    let blocksSkipped = 0;
    let comparisons = 0;
    const explain = options.explain === true;

    for (const loaded of snapshot) {
      const seg = loaded.segment;
      const segStart = performance.now();

      const stats: EvalStats = { matched: 0, phraseRejected: 0, blocksSkipped: 0, comparisons: 0 };

      // 第一层短路：段级剪枝（只看段元信息，不碰 posting）
      const reason = skipReason(plan, seg);
      if (reason !== null) {
        skippedSegments++;
        if (explain) {
          reports.push({
            generation: seg.generation,
            docCount: seg.meta.docCount,
            liveCount: seg.liveCount,
            matched: 0,
            skipped: true,
            reason,
            elapsedMs: performance.now() - segStart,
          });
        }
        continue;
      }

      // 第二层短路：block-max 上界剪枝的 posting 逐级收敛
      // arena 是本次查询私有的中间结果空间，段之间不复用，查询之间更不复用
      const arena = new EvalArena();
      const result = evaluatePlan(plan, seg, stats, arena);
      blocksSkipped += stats.blocksSkipped;
      comparisons += stats.comparisons;

      // 第三层过滤：tombstone（段内墓碑 + 尚未合并的删除标记）
      const pending = this.pendingTombstones.get(seg.generation);
      let segMatched = 0;
      const docs = result.docs;
      for (let i = 0; i < docs.length; i++) {
        const docId = docs[i]!;
        if (pending !== undefined && pending.has(docId)) continue;
        const doc = seg.getDoc(docId);
        if (doc === null) continue;
        totalHits++;
        segMatched++;
        if (hits.length < limit) hits.push(doc);
      }

      if (explain) {
        reports.push({
          generation: seg.generation,
          docCount: seg.meta.docCount,
          liveCount: seg.liveCount,
          matched: segMatched,
          skipped: false,
          reason: null,
          elapsedMs: performance.now() - segStart,
        });
      }
    }

    const tEnd = performance.now();
    return {
      query,
      parsed,
      plan,
      planText: describePlan(plan),
      hits,
      totalHits,
      segments: reports,
      skippedSegments,
      blocksSkipped,
      comparisons,
      elapsedMs: tEnd - t0,
      parseMs: tParsed - t0,
      evalMs: tEnd - tPlanned,
    };
  }

  /* ---------------- 持久化 ---------------- */

  /** 段 -> 段文件。落盘失败不影响内存可见性（查询照常）。 */
  private async persist(segment: Segment): Promise<string | null> {
    if (this.dataDir === null) return null;
    const blob = encodeSegment(segment);
    return writeSegment(this.dataDir, blob);
  }

  /**
   * 从磁盘重载：按段逐个读回并重建跳表。
   * 跳表不落盘（它是纯派生结构），重载时按 posting 重新构建，
   * 这样段文件保持最小，同时跳表内存与查询性能完全恢复。
   */
  static async open(options: EngineOptions = {}): Promise<SearchEngine> {
    const engine = new SearchEngine(options);
    if (options.dataDir == null) return engine;

    const files = await listSegmentFiles(options.dataDir);
    const loaded: LoadedSegment[] = [];
    for (const file of files) {
      const { header, postingData } = await readSegment(options.dataDir, file);
      loaded.push({ segment: decodeSegment(header, postingData), file });
    }
    // 按 generation 升序，保证重载后的段顺序与原来一致（影响 docId 稳定性）
    loaded.sort((a, b) => a.segment.generation - b.segment.generation);
    engine.segments = loaded;
    engine.nextGeneration = loaded.reduce((max, l) => Math.max(max, l.segment.generation + 1), 0);
    engine.rebuildLocation();
    return engine;
  }

  /* ---------------- 观测 ---------------- */

  /** 当前段视图（只读快照）。 */
  get segmentCount(): number {
    return this.segments.length;
  }

  /** 段摘要，供终端打印。 */
  stats(): Array<{ generation: number; docCount: number; liveCount: number; tombstoneRatio: number; file: string | null }> {
    return this.segments.map((l) => ({
      generation: l.segment.generation,
      docCount: l.segment.meta.docCount,
      liveCount: l.segment.liveCount,
      tombstoneRatio: Number(l.segment.tombstoneRatio.toFixed(3)),
      file: l.file,
    }));
  }

  /** 正在进行的合并（无则 null）。 */
  get mergingInfo(): { sources: number[]; generation: number } | null {
    return this.merging;
  }

  get bufferedDocs(): number {
    return this.buffer.length;
  }

  /* ---------------- location 索引 ---------------- */

  private indexLocation(segment: Segment): void {
    // 段文档数超过阈值就不建 id 索引：删除改为按段线性定位，内存换时间
    if (segment.meta.docCount > SearchEngine.LOCATION_INDEX_MAX) return;
    for (let docId = 0; docId < segment.meta.docCount; docId++) {
      const id = segment.ids[docId];
      if (id !== undefined) this.location.set(id, { generation: segment.generation, docId });
    }
  }

  /**
   * 定位一个逻辑 id 落在哪个段的哪个 docId。
   *
   * 优先查 location 索引；大段没建索引时按段线性扫 ids 数组兜底。
   */
  private locate(id: string): { generation: number; docId: number } | undefined {
    const hit = this.location.get(id);
    if (hit !== undefined) return hit;
    for (const loaded of this.segments) {
      const seg = loaded.segment;
      if (seg.meta.docCount > SearchEngine.LOCATION_INDEX_MAX) continue;
      const docId = seg.ids.indexOf(id);
      if (docId >= 0) return { generation: seg.generation, docId };
    }
    return undefined;
  }

  private rebuildLocation(): void {
    this.location.clear();
    for (const l of this.segments) this.indexLocation(l.segment);
  }
}

/* ------------------------------------------------------------------ *
 * 段 <-> 二进制
 * ------------------------------------------------------------------ */

/** 内存段 -> 段文件负载。 */
export function encodeSegment(segment: Segment): {
  header: SegmentHeader;
  postingData: Int32Array;
} {
  const fields: Record<string, FieldEntry> = {};
  let total = 0;
  for (const [field, bucket] of segment.index.fields) {
    if (field.startsWith(FIELD_PHRASE_SUFFIX)) continue; // 派生索引：重载时按原文重建
    const terms: string[] = [];
    const offsets: number[] = [];
    const lengths: number[] = [];
    for (const [term, posting] of bucket) {
      terms.push(term);
      offsets.push(total);
      lengths.push(posting.docs.length);
      total += posting.docs.length;
    }
    fields[field] = { terms, offsets, lengths };
  }

  const postingData = new Int32Array(total);
  for (const field of Object.keys(fields)) {
    const entry = fields[field]!;
    const bucket = segment.index.fields.get(field)!;
    for (let i = 0; i < entry.terms.length; i++) {
      postingData.set(bucket.get(entry.terms[i]!)!.docs, entry.offsets[i]!);
    }
  }

  return {
    header: {
      generation: segment.meta.generation,
      docCount: segment.meta.docCount,
      deleted: Array.from(segment.deleted),
      sources: segment.meta.sources,
      numericFields: [...segment.index.numeric.keys()],
      textFields: [...segment.index.textFields],
      fields,
      ids: [...segment.ids],
      // 每篇文档的完整 JSON（加载时直接复用，无需重新序列化）
      docPayload: Array.from({ length: segment.meta.docCount }, (_, i) => segment.store.jsonOf(i) ?? ""),
    },
    postingData,
  };
}

/** 段文件负载 -> 内存段（含跳表重建）。 */
export function decodeSegment(header: SegmentHeader, postingData: Int32Array): Segment {
  const docCount = header.docCount;
  const stride = skipStrideFor(docCount);

  const fields = new Map<string, Map<string, Posting>>();
  for (const [field, entry] of Object.entries(header.fields)) {
    const bucket = new Map<string, Posting>();
    for (let i = 0; i < entry.terms.length; i++) {
      const off = entry.offsets[i]!;
      const len = entry.lengths[i]!;
      const docs = postingData.subarray(off, off + len);
      // 跳表不落盘（纯派生结构）：这里按 posting 重新构建，查询性能完全恢复
      bucket.set(entry.terms[i]!, makePosting(docs, docCount));
    }
    fields.set(field, bucket);
  }

  // 重建数值排序表
  const numeric = new Map<string, NumericFieldIndex>();
  for (const field of header.numericFields) {
    const entries: Array<[number, number]> = [];
    const bucket = fields.get(field);
    if (bucket !== undefined) {
      for (const [term, posting] of bucket) {
        if (term.charCodeAt(0) !== 1) continue; // 只有数值编码的 term 参与
        const value = Number(term.slice(1));
        for (const docId of posting.docs) entries.push([value, docId]);
      }
    }
    entries.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
    const values = new Float64Array(entries.length);
    const docs = new Int32Array(entries.length);
    entries.forEach(([value, docId], i) => {
      values[i] = value;
      docs[i] = docId;
    });
    numeric.set(field, { values, docs });
  }

  // 原文直接复用段文件里的 JSON 文本（无需重新序列化）
  const docJson: string[] = header.docPayload.map((p) => p ?? "");

  // 重建派生索引：文本字段的词级倒排（裸词/裸短语的候选集来源）
  const textFields = new Set<string>(header.textFields);
  const store = new DocStore(textFields);
  for (let docId = 0; docId < docJson.length; docId++) {
    const json = docJson[docId]!;
    if (json === "") {
      store.skip();
      continue;
    }
    const doc = JSON.parse(json) as Record<string, unknown>;
    store.append(doc);
    for (const [field, value] of Object.entries(doc)) {
      if (typeof value !== "string" || !textFields.has(field)) continue;
      for (const token of tokenizeText(value)) {
        pushTerm(fields, FIELD_PHRASE_SUFFIX + field, token, docId, stride);
      }
    }
  }

  const index: InvertedIndex = { fields, numeric, textFields, docCount };
  const deleted = Uint32Array.from(header.deleted);
  const meta = { generation: header.generation, docCount: header.docCount, deleted: header.deleted, sources: header.sources };
  return Segment.fromParts(meta, index, store, header.ids, deleted);
}

/** 解码时补一个 term 的 posting（用于重建派生索引）。 */
function pushTerm(fields: Map<string, Map<string, Posting>>, field: string, term: string, docId: number, stride: number): void {
  let bucket = fields.get(field);
  if (bucket === undefined) {
    bucket = new Map<string, Posting>();
    fields.set(field, bucket);
  }
  const existing = bucket.get(term);
  const docs = existing === undefined ? Int32Array.of(docId) : Int32Array.from([...existing.docs, docId].sort((a, b) => a - b));
  bucket.set(term, makePosting(docs, stride));
}
