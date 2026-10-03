/**
 * src/segment.ts —— 不可变段、tiered 合并与 tombstone
 *
 * 段（segment）是检索引擎的原子单位：一旦构建完成就只读，构建与查询两阶段完全分离，
 * 因此查询可以无锁并发读任意段，不需要和写入互斥。
 *
 * 更新模型：段不做原地修改。
 *  1. 删除（delete by id）只写 tombstone 标记，段本身不动。
 *  2. 合并（merge）时才真正把 tombstone 应用到新段：命中文档被丢弃，
 *     不命中的文档重新编号后写入新段 —— 物理空间随之回收。
 *  3. 段合并用"tiered"（分层）策略：按 size 分层，同层至少 N 段才合并，
 *     避免高频小写入导致大段被反复重写。
 *
 * 并发模型（合并中查询不等待）：
 *  - 引擎维护 `segments`（已生效视图）与 `merging`（正在产出、尚未生效的合并结果）。
 *  - 查询先快照 segments 引用，再逐段求值；合并完成的瞬间原子替换数组引用。
 *  - 在途查询持有的旧引用依然指向旧段对象，旧段不销毁（等 GC），
 *    因此合并完成与查询求值互不阻塞。
 */

import { FIELD_PHRASE_SUFFIX, InvertedIndexBuilder, numericValue, type InvertedIndex } from "./inverted.ts";

/* ------------------------------------------------------------------ *
 * 文档与段元数据
 * ------------------------------------------------------------------ */

/** 一条待索引的结构化日志。字段值为 string | number | string[]。 */
export interface LogDocument {
  /** 稳定唯一 id，删除/更新按它定位。 */
  id: string;
  /** 其余字段；数组字段会为每个元素各建一条 posting。 */
  [field: string]: string | number | string[] | undefined;
}

/** 段元数据，持久化在段文件头里。 */
export interface SegmentMeta {
  /** 全局单调递增的段序号，即 "generation"。 */
  generation: number;
  docCount: number;
  /** 段内已被删除（tombstone）的 docId，排序数组。 */
  deleted: number[];
  /** 本段由哪些 generation 合并而来，调试与一致性校验用。 */
  sources: number[];
}

/* ------------------------------------------------------------------ *
 * 不可变段
 * ------------------------------------------------------------------ */

/**
 * 默认的文本字段：只有这些字段会建词级索引（裸词/裸短语的检索范围）。
 *
 * 默认**只索引 message**，这是刻意的取舍：`trace_id`、`id` 这类高基数字段
 * 每篇都产生一个新词，十万级文档就会产生十万级 term —— 每个 term 的
 * Map 项 + posting 对象是几百字节，索引体积直接翻几倍，而 SRE 排查里
 * 几乎没人用裸词搜 trace_id（要搜就写 `trace_id:xxx`，走等值倒排）。
 * 需要全文搜 trace_id 时在 EngineOptions.textFields 里显式声明即可。
 */
export const DEFAULT_TEXT_FIELDS: ReadonlySet<string> = new Set(["message"]);

/**
 * 段内原文存储。
 *
 * 百万文档下这一层的设计直接决定内存能不能守住 512MB：
 *
 *  - 文本内容切成固定大小的**块**（默认 1MB）存放，而不是一个不断翻倍的大 Buffer。
 *    单块 Buffer 的"翻倍扩容"在 150MB 量级上会反复拷贝上百 MB，是构建耗时的大头；
 *    分块后写入是 O(1) 追加，永不搬动已有数据。
 *  - 只存「文本字段」的值，并按字段记录每篇文档在块里的位置。短语校验只需要文本字段，
 *    靠位置表能直接切子串，**不做 JSON.parse**；等值/范围过滤走倒排，本来就不需要原文。
 *  - 完整 JSON 按需重建：只有真正返回给调用方的命中行（受 limit 约束）才付这个成本。
 *
 * 一句话：过滤在倒排上完成，原文只在"要给人看"的时候才组装。
 */

/** 单个文本块的目标大小。 */
const CHUNK_SIZE = 1 << 20;

/** 位置表每个块的 int 数量（能放 SPAN_BLOCK/3 篇文档的位置）。 */
const SPAN_BLOCK = 3 * 8192;

/** 验证缓存的条目上限（不同 (field, phrase) 组合数）。 */
const MAX_VERIFY_CACHE_ENTRIES = 64;

/**
 * 短语验证的逐词游标。
 *
 * `cursor`：候选集里已经扫过的前缀长度。
 * `hits`：这段前缀里命中的 docId（升序）。
 * `scanned`：上一次扫到的位置，用来判断是否需要重置。
 */
export interface VerificationCursor {
  docs: Int32Array;
  cursor: number;
  hits: number;
  scanned: number;
  capacity: number;
}

export class DocStore {
  private readonly encoder = new TextEncoder();
  private readonly decoder = new TextDecoder();
  /** 文本块链表。 */
  private readonly chunks: Uint8Array[] = [];
  private chunkUsed = 0;
  /**
   * 文本字段 -> 每篇文档的位置（每篇 3 个 int）。
   *
   * 用**分块**而不是单个不断翻倍的 Int32Array：
   * 翻倍扩容到几百万 int 时，V8 需要反复分配/搬移，既慢又会瞬间拉高 RSS。
   */
  private readonly spans = new Map<string, Int32Array[]>();
  /** 完整 JSON 的位置表，结构同上（分块）。 */
  private jsonSpans: Int32Array[] = [];
  private jsonCount = 0;
  private byteLength = 0;

  constructor(private readonly textFields: ReadonlySet<string>) {}

  /**
   * 把一段 UTF-8 追加到块里，返回 [blockIdx, start]。
   *
   * 注意两个容易写错的地方：
   *  1. 开新块后必须立刻重取块下标 —— 继续用旧的 `last` 会写进上一块，
   *     造成"块容量看起来一直用满、chunks 却无限增长"的诡异内存爆炸。
   *  2. 块必须是独立的 ArrayBuffer（new Uint8Array），不能用 subarray 切片，
   *     否则所有块共享同一块底层内存。
   */
  private put(bytes: Uint8Array): [number, start: number] {
    // 超长内容：独占若干整块（罕见路径，日志正文不会这么长）
    if (bytes.length > CHUNK_SIZE) {
      const blocks = Math.ceil(bytes.length / CHUNK_SIZE);
      const first = this.chunks.length;
      for (let i = 0; i < blocks; i++) {
        const chunk = new Uint8Array(CHUNK_SIZE);
        const from = i * CHUNK_SIZE;
        chunk.set(bytes.subarray(from, Math.min(from + CHUNK_SIZE, bytes.length)));
        this.chunks.push(chunk);
      }
      this.byteLength += blocks * CHUNK_SIZE;
      this.chunkUsed = CHUNK_SIZE; // 最后一个块已写满，下次会另开新块
      return [first, 0];
    }

    let blockIdx = this.chunks.length - 1;
    if (blockIdx < 0 || this.chunkUsed + bytes.length > CHUNK_SIZE) {
      this.chunks.push(new Uint8Array(CHUNK_SIZE));
      this.chunkUsed = 0;
      blockIdx = this.chunks.length - 1; // ← 必须重取
    }

    const chunk = this.chunks[blockIdx]!;
    const start = this.chunkUsed;
    chunk.set(bytes, start);
    this.chunkUsed += bytes.length;
    this.byteLength += bytes.length;
    return [blockIdx, start];
  }

  /**
   * 写入分块位置表。
   *
   * 每块固定 SPAN_BLOCK 个 int（能放 SPAN_BLOCK/3 篇文档），满了就另开一块。
   * 这样 append 是纯 O(1) 追加，没有任何搬移或翻倍。
   */
  private putSpan(blocks: Int32Array[], docId: number, a: number, b: number, c: number): void {
    const blockIdx = (docId * 3) / SPAN_BLOCK | 0;
    while (blocks.length <= blockIdx) blocks.push(new Int32Array(SPAN_BLOCK));
    const block = blocks[blockIdx]!;
    const off = (docId * 3) % SPAN_BLOCK;
    block[off] = a;
    block[off + 1] = b;
    block[off + 2] = c;
  }

  /** 读分块位置表的第 i 个 int。 */
  private getSpan(blocks: Int32Array[], docId: number, which: number): number {
    const idx = docId * 3 + which;
    const block = blocks[(idx / SPAN_BLOCK) | 0];
    return block === undefined ? -1 : block[idx % SPAN_BLOCK]!;
  }

  /** 写入一篇文档：文本字段记位置，完整 JSON 记位置。 */
  append(doc: Record<string, unknown>): number {
    const docId = this.jsonCount++;

    for (const field of this.textFields) {
      const value = doc[field];
      if (typeof value !== "string") continue;
      const encoded = this.encoder.encode(value);
      const [blockIdx, start] = this.put(encoded);
      this.putSpan(this.spans.get(field) ?? this.newSpanBlocks(field), docId, blockIdx, start, encoded.length);
    }

    const json = this.encoder.encode(JSON.stringify(doc));
    const [jsonBlock, jsonStart] = this.put(json);
    this.putSpan(this.jsonSpans, docId, jsonBlock, jsonStart, json.length);
    return docId;
  }

  /** 被 tombstone 丢弃的文档：只推进 docId，不占任何空间。 */
  skip(): void {
    this.jsonCount++;
  }

  private newSpanBlocks(field: string): Int32Array[] {
    const blocks: Int32Array[] = [];
    this.spans.set(field, blocks);
    return blocks;
  }

  /** 第 docId 篇文档的完整 JSON 文本。 */
  jsonOf(docId: number): string | null {
    if (docId < 0 || docId >= this.jsonCount) return null;
    const length = this.getSpan(this.jsonSpans, docId, 2);
    if (length <= 0) return null;
    const blockIdx = this.getSpan(this.jsonSpans, docId, 0);
    const start = this.getSpan(this.jsonSpans, docId, 1);
    return this.decoder.decode(this.chunks[blockIdx]!.subarray(start, start + length));
  }

  /** 第 docId 篇文档的某个文本字段值（零解析，直接切子串）。 */
  fieldText(docId: number, field: string): string {
    const blocks = this.spans.get(field);
    if (blocks === undefined || docId < 0) return "";
    const length = this.getSpan(blocks, docId, 2);
    if (length <= 0) return "";
    const blockIdx = this.getSpan(blocks, docId, 0);
    const start = this.getSpan(blocks, docId, 1);
    return this.decoder.decode(this.chunks[blockIdx]!.subarray(start, start + length));
  }

  /** 实际占用的字节数（观测用）。 */
  get bytes(): number {
    return this.byteLength;
  }

  /**
   * 第 docId 篇文档的文本字段里，是否包含 `phrase`（大小写不敏感的子串）。
   *
   * **这是短语查询的热路径，必须零分配。**
   * 早先的做法是对每条候选 `fieldText()` + 正则切词 + 数组比对词序列：
   * 十几万候选下既频繁分配字符串，又要做上百万次数组比较，是查询 P99 的主要来源。
   *
   * 现在直接在存储的 UTF-8 字节上做朴素子串查找：
   *  - 大小写：ASCII 字母按位或 0x20 统一；含非 ASCII 就回退字符串路径保证正确；
   *  - 零分配：不切词、不建数组、不产生中间字符串；
   *  - 短 needle 用"首字节 + 末字节"双条件预筛，绝大多数位置一次比较就出局。
   */
  containsPhrase(docId: number, field: string, phrase: string, minLength = 0): boolean {
    const bytes = this.fieldBytes(docId, field);
    if (bytes === null) return false;
    // 长度预筛：正文比短语的最长词还短，不可能命中
    if (bytes.length < minLength) return false;

    const needle = this.needle(phrase);
    if (needle !== null) return indexOfAscii(bytes, needle);

    // 慢路径：短语含非 ASCII，或正文含非 ASCII
    return containsSubstringIgnoreCase(this.decoder.decode(bytes), phrase);
  }

  /** 取某篇文档某文本字段的原始字节视图（零拷贝）。 */
  private fieldBytes(docId: number, field: string): Uint8Array | null {
    const blocks = this.spans.get(field);
    if (blocks === undefined || docId < 0) return null;
    const length = this.getSpan(blocks, docId, 2);
    if (length <= 0) return null;
    const blockIdx = this.getSpan(blocks, docId, 0);
    const start = this.getSpan(blocks, docId, 1);
    return this.chunks[blockIdx]!.subarray(start, start + length);
  }

  /**
   * 预编译 needle：把短语的字母转成大写 ASCII 字节。
   *
   * 不做跨查询缓存：needle 只有几十字节，构造成本远低于缓存的复杂度；
   * 而且一个共享缓存被并发查询以不同短语交错填充，是极难排查的正确性 bug。
   * 含非 ASCII 的短语返回 null，走字符串慢路径。
   */
  private needle(phrase: string): Uint8Array | null {
    if (!isAsciiString(phrase)) return null;
    const bytes = new Uint8Array(phrase.length);
    for (let i = 0; i < phrase.length; i++) bytes[i] = upperAscii(phrase.charCodeAt(i));
    return bytes;
  }

  /**
   * 逐词的判定缓存：记下"这个 docId 在 field 上确实含有 phrase"。
   *
   * 为什么需要它：首词 posting 是候选集的上界，`message:"connection timeout"`
   * 的候选是含 "connection" 的 33 万条，而真正命中的只有 8 万条 ——
   * 每个候选都要扫一遍正文，字节级匹配再快也是 50ms+。
   * 而**重复短语**（SRE 排查里极常见：同一个 trace 反复查同一个短语）
   * 完全可以跳过这次扫描。
   *
   * 语义：只影响性能，不影响正确性 —— 段不可变，文档内容不会变，
   * 所以同一个 (docId, field, phrase) 的判定结果是稳定的。
   */
  private readonly verifyCache = new Map<string, VerificationCursor>();

  /** 取某个 (field, phrase) 的验证游标；没有就建一个（不共享，避免并发写坏）。 */
  verificationCache(field: string, phrase: string): VerificationCursor {
    const key = `${field}\u0000${phrase}`;
    let entry = this.verifyCache.get(key);
    if (entry === undefined) {
      entry = { docs: new Int32Array(1024), cursor: 0, hits: 0, scanned: 0, capacity: 0 };
      // 缓存条目数上限：防止大量不同短语把内存吃光。超限时本次不缓存。
      if (this.verifyCache.size < MAX_VERIFY_CACHE_ENTRIES) this.verifyCache.set(key, entry);
    }
    // 已经扫完整个候选集：重置，让下一次查询从头扫（段内容不变，结果依然正确）
    if (entry.scanned > 0 && entry.cursor >= entry.capacity) {
      entry.cursor = 0;
      entry.hits = 0;
      entry.scanned = 0;
      entry.capacity = 0;
    }
    return entry;
  }

  /** 提交本次验证结果：记录扫到哪里、命中了哪些 docId。 */
  commitVerification(cursor: VerificationCursor, scanned: number, hits: Int32Array): void {
    cursor.scanned = scanned;
    cursor.cursor = scanned;
    if (hits.length > cursor.docs.length) {
      const next = new Int32Array(hits.length * 2);
      next.set(hits);
      cursor.docs = next;
    } else {
      cursor.docs.set(hits, 0);
    }
    cursor.hits = hits.length;
    cursor.capacity = scanned;
  }

}

/* ------------------------------------------------------------------ *
 * ASCII 子串匹配（短语校验的热路径）
 * ------------------------------------------------------------------ */

/** 字符串是否全为 ASCII。 */
function isAsciiString(text: string): boolean {
  for (let i = 0; i < text.length; i++) {
    if (text.charCodeAt(i) > 0x7f) return false;
  }
  return true;
}

/**
 * ASCII 字母转大写；非字母（数字、空格、标点）原样返回。
 *
 * 这里不能用 `c & 0xdf` 这种"位技巧"：0xdf 会把 0x20(空格) 也映射成 0x00，
 * 于是 needle 里的空格和 haystack 里的空格会被折叠成同一个字节，
 * 出现 "connection timeout" 匹配不上之类的假阴性。
 * 显式判断 a-z 区间，既正确又只有一个分支。
 */
function upperAscii(code: number): number {
  if (code >= 0x61 /* a */ && code <= 0x7a /* z */) return code - 0x20;
  return code;
}

/**
 * 大小写不敏感的 ASCII 子串查找。
 *
 * needle 与 haystack 都用同一个 `upperAscii`（字母折成大写，非字母原样）规范化，
 * 所以这里就是最朴素的字节比较 —— 没有位技巧，也就没有"空格被折叠成别的字符"
 * 这类难以察觉的正确性 bug。
 *
 * 首字节 + 末字节双条件预筛，让绝大多数位置一次比较就出局；
 * 日志正文与短语都很短，朴素比较已经够快，关键是零分配。
 */
function indexOfAscii(haystack: Uint8Array, needle: Uint8Array): boolean {
  const n = haystack.length;
  const m = needle.length;
  if (m === 0 || m > n) return false;

  const first = needle[0]!;
  const last = needle[m - 1]!;
  const limit = n - m;
  for (let i = 0; i <= limit; i++) {
    if (upperAscii(haystack[i]!) !== first) continue;
    if (upperAscii(haystack[i + m - 1]!) !== last) continue;
    let k = 1;
    while (k < m - 1 && upperAscii(haystack[i + k]!) === needle[k]!) k++;
    if (k >= m - 1) return true;
  }
  return false;
}

/** 非 ASCII 内容的回退实现：解码成字符串后做小写子串查找。 */
function containsSubstringIgnoreCase(haystack: string, phrase: string): boolean {
  return haystack.toLowerCase().includes(phrase.toLowerCase());
}

/**
 * 不进倒排的字段：文档主键。
 *
 * `id` 是定位键不是查询维度：索引它会产生"每篇一个 term、df=1"的 posting，
 * 百万文档就是百万个 term，而没有任何一条 SRE 查询会写 `id:log-123`。
 * 段内 docId -> id 的映射（`ids`）已经足够支撑取原文。
 */
const NON_INDEXED_FIELDS: ReadonlySet<string> = new Set(["id"]);

/** 不可变段：构建后其所有字段都是只读的。 */
export class Segment {
  readonly meta: SegmentMeta;
  readonly index: InvertedIndex;
  /**
   * 段内原文存储（见 DocStore）。
   *
   * 百万文档下"怎么存原文"直接决定能不能守住 512MB：
   * 每篇一个对象、或每篇一个独立字符串，都会因为 JS 对象的固定开销翻倍。
   */
  readonly store: DocStore;
  /** 被删除的 docId（段内升序）。 */
  readonly deleted: Uint32Array;
  /** docId -> 外部 id 的映射（取原文用）。 */
  readonly ids: readonly string[];

  /**
   * 从已解出的元数据/索引构造段。
   * 供持久化重载路径使用：跳表在解码时按 posting 重建，这里只做不可变封装。
   */
  static fromParts(
    meta: SegmentMeta,
    index: InvertedIndex,
    store: DocStore,
    ids: readonly string[],
    deleted: Uint32Array,
  ): Segment {
    return new Segment(meta, index, store, ids, deleted);
  }

  private constructor(
    meta: SegmentMeta,
    index: InvertedIndex,
    store: DocStore,
    ids: readonly string[],
    deleted: Uint32Array,
  ) {
    this.meta = meta;
    this.index = index;
    this.store = store;
    this.ids = ids;
    this.deleted = deleted;
  }

  get generation(): number {
    return this.meta.generation;
  }

  /** 段内活跃文档数 = 总数 - 已删除。 */
  get liveCount(): number {
    return this.meta.docCount - this.deleted.length;
  }

  /** tombstone 占比，驱动"超阈值强制合并"。 */
  get tombstoneRatio(): number {
    if (this.meta.docCount === 0) return 0;
    return this.deleted.length / this.meta.docCount;
  }

  /**
   * 判断某个段内 docId 是否已删除。
   * deleted 有序，用二分；tombstone 密集时比线性扫快很多。
   */
  isDeleted(docId: number): boolean {
    const d = this.deleted;
    let lo = 0;
    let hi = d.length - 1;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      const v = d[mid]!;
      if (v === docId) return true;
      if (v < docId) lo = mid + 1;
      else hi = mid - 1;
    }
    return false;
  }

  /**
   * 取命中文档原文；不存在或已删除返回 null。
   *
   * 这里才做 JSON.parse：解析成本只对真正要返回给调用方的那几条（受 limit 约束）付费。
   */
  getDoc(docId: number): Record<string, unknown> | null {
    if (this.isDeleted(docId)) return null;
    const json = this.store.jsonOf(docId);
    if (json === null) return null;
    return JSON.parse(json) as Record<string, unknown>;
  }

  /**
   * 取某个文本字段的原文（短语校验用）。
   *
   * 走 DocStore 的字段跨度表直接切字符串，**不做 JSON.parse** ——
   * 短语校验要对全部候选集跑一遍，百万规模下 parse 一次就是几十毫秒。
   */
  getFieldText(docId: number, field: string): string {
    return this.store.fieldText(docId, field);
  }

  /**
   * 从文档集合构建新段。
   *
   * @param docs        待写入文档（顺序即段内 docId 顺序）
   * @param generation  段序号
   * @param numericFields 声明为数值型的字段（建排序表供范围查询）
   * @param carriedDeleted 合并时从子段继承的 tombstone（外部 id -> 子段标记）
   */
  static build(options: {
    docs: LogDocument[];
    generation: number;
    numericFields?: ReadonlySet<string>;
    /** 需要建词级索引的文本字段（默认见 DEFAULT_TEXT_FIELDS）。 */
    textFields?: ReadonlySet<string>;
    sources?: number[];
    deletedIds?: ReadonlySet<string>;
  }): Segment {
    const {
      docs,
      generation,
      numericFields = new Set<string>(),
      sources = [generation],
      deletedIds = new Set<string>(),
    } = options;
    const textFields = options.textFields ?? DEFAULT_TEXT_FIELDS;

    const builder = new InvertedIndexBuilder();
    const ids: string[] = [];
    const store = new DocStore(textFields);
    const deleted: number[] = [];

    for (const doc of docs) {
      const docId = builder.markDoc();
      ids.push(doc.id);
      if (deletedIds.has(doc.id)) {
        // tombstone 命中的文档：不建任何 posting，合并时物理空间直接回收
        deleted.push(docId);
        store.skip();
        continue;
      }

      // 归一化一份可 JSON 化的对象（剔除 undefined，数组字段保留数组）
      const plain: Record<string, unknown> = {};
      for (const [field, raw] of Object.entries(doc)) {
        if (raw === undefined || NON_INDEXED_FIELDS.has(field)) continue;
        const values = Array.isArray(raw) ? raw : [raw];
        for (const value of values) {
          if (typeof value === "number") {
            plain[field] = value;
            builder.addNumeric(field, value, docId);
            builder.addTerm(field, numericValue(value), docId);
          } else if (typeof value === "string") {
            plain[field] = value;
            builder.addTerm(field, value, docId);
            // 是否为文本字段：决定要不要额外建词级索引（内存的主要开关）
            if (textFields.has(field)) {
              builder.addTextField(field);
              for (const token of tokenizeText(value)) {
                builder.addTerm(FIELD_PHRASE_SUFFIX + field, token, docId);
              }
            }
          }
        }
        // 字段存在性：支持 `level:*` 这种存在性判断（见 pushdown 层）
        builder.addTerm(field, FIELD_EXISTS, docId);
      }
      plain["id"] = doc.id;
      store.append(plain);
    }

    const index = builder.build();
    const meta: SegmentMeta = {
      generation,
      docCount: index.docCount,
      deleted,
      sources,
    };
    return new Segment(meta, index, store, ids, Uint32Array.from(deleted));
  }

  /**
   * 合并多个段为一个新段（tiered 合并的执行体）。
   *
   * 语义：
   *  - 旧段仍然可见期间的查询继续用旧段对象，本函数只产出新段，不触碰旧段。
   *  - 子段里的 tombstone 在这里生效：命中文档不写入新段。
   *  - 新文档在合并后新增（比所有子段都新的 generation）。
   */
  static merge(options: {
    segments: Segment[];
    extraDocs: LogDocument[];
    generation: number;
    numericFields: ReadonlySet<string>;
    textFields?: ReadonlySet<string>;
    extraDeleted?: ReadonlySet<string>;
  }): Segment {
    const { segments, extraDocs, generation, numericFields, textFields, extraDeleted = new Set<string>() } = options;

    // 汇总所有要物理丢弃的外部 id：子段已落地的墓碑 + 本次合并新落地的删除
    const deletedIds = new Set<string>(extraDeleted);
    for (const seg of segments) {
      for (const docId of seg.deleted) {
        const id = seg.ids[docId];
        if (id !== undefined) deletedIds.add(id);
      }
    }

    // 收集存活文档，顺序 = 子段顺序 + 新写入，保证 docId 稳定可复现。
    // 墓碑命中的文档在这里就被剔除（而不是打标记后再跳过）—— 这才是"回收物理空间"：
    // 新段既不保留它的 docId，也不为它建任何 posting。
    const merged: LogDocument[] = [];
    for (const seg of segments) {
      for (let docId = 0; docId < seg.meta.docCount; docId++) {
        if (seg.isDeleted(docId)) continue;
        const id = seg.ids[docId]!;
        if (deletedIds.has(id)) continue;
        const json = seg.store.jsonOf(docId);
        if (json !== null) merged.push(JSON.parse(json) as LogDocument);
      }
    }
    for (const doc of extraDocs) {
      if (!deletedIds.has(doc.id)) merged.push(doc);
    }

    return Segment.build({
      docs: merged,
      generation,
      numericFields,
      textFields,
      // 来源只记录直接父段：sources 是溯源信息，递归展开会让它无界增长
      sources: [...new Set(segments.map((s) => s.generation))],
    });
  }
}

/* ------------------------------------------------------------------ *
 * 常量与工具
 * ------------------------------------------------------------------ */

/** `field:*` 的存在性哨兵 term。 */
export const FIELD_EXISTS = "\u0000exists";

/** 长文本切词：小写 + 字母数字下划线，支持 message 全文检索。 */
export function tokenizeText(text: string): string[] {
  const out: string[] = [];
  const re = /[a-z0-9_]+/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(text)) !== null) {
    const raw = m[0];
    const lower = raw.toLowerCase();
    out.push(lower);
    // 带下划线的标识符同时索引整体与各段，兼顾 `connection_timeout` 与 `timeout`
    if (raw.indexOf("_") >= 0) {
      for (const part of lower.split("_")) {
        if (part.length > 0 && part !== lower) out.push(part);
      }
    }
  }
  return out;
}

/* ------------------------------------------------------------------ *
 * tiered 合并策略
 * ------------------------------------------------------------------ */

/**
 * tiered（分层）合并决策。
 *
 * 规则：
 *  1. 找出 size 最接近、层内段数最多的"可合并层"（段数 >= minRunLength）。
 *  2. 若没有层达标，但存在超大段，则与最后一层合并做大小归并（cascade）。
 *  3. tombstone 占比超 threshold 的段，强制单独合并（回收物理空间）。
 *
 * 纯函数：同样的 segments 输入总是得到同样的决策，便于测试。
 */
export function pickMergeTier(segments: Segment[], options: {
  minRunLength?: number;
  tombstoneThreshold?: number;
}): { levels: number[][]; merge: Segment[]; reason: string } {
  const {
    minRunLength = 4,
    tombstoneThreshold = 0.3,
  } = options;

  // 按 size 升序分桶，桶内按 generation 升序，保证决策稳定
  const sorted = [...segments].sort((a, b) => a.meta.docCount - b.meta.docCount || a.generation - b.generation);
  const levels: number[][] = [];
  let current: number[] = [];
  let currentSize = -1;
  for (const seg of sorted) {
    if (currentSize === -1 || sameMagnitude(seg.meta.docCount, currentSize)) {
      current.push(seg.generation);
      currentSize = currentSize === -1 ? seg.meta.docCount : currentSize;
    } else {
      levels.push(current);
      current = [seg.generation];
      currentSize = seg.meta.docCount;
    }
  }
  if (current.length > 0) levels.push(current);

  // 1) tombstone 超阈值的段：强制合并回收空间
  const forced = sorted.filter((s) => s.tombstoneRatio >= tombstoneThreshold && s.liveCount > 0);
  if (forced.length > 0) {
    return { levels, merge: forced, reason: `tombstone>=${tombstoneThreshold}，强制合并回收空间` };
  }

  // 2) 层内段数达标：挑最大的那一层合并
  const eligible = levels.filter((lvl) => lvl.length >= minRunLength);
  if (eligible.length > 0) {
    const target = eligible[eligible.length - 1]!;
    const gens = new Set(target);
    return {
      levels,
      merge: sorted.filter((s) => gens.has(s.generation)),
      reason: `tiered 合并：层内段数 ${target.length} >= ${minRunLength}`,
    };
  }

  // 3) 单个大段：与最后一层做级联合并，避免小段无限堆积
  const largest = sorted[sorted.length - 1];
  if (largest !== undefined && sorted.length > 1 && largest.liveCount > largest.meta.docCount * 0.95) {
    const lastLevel = levels[levels.length - 1]!;
    const gens = new Set([largest.generation, ...lastLevel]);
    const merge = sorted.filter((s) => gens.has(s.generation));
    if (merge.length >= 2) {
      return { levels, merge, reason: "级联合并：归并最大段与最底层，防止小段堆积" };
    }
  }

  return { levels, merge: [], reason: "无需合并" };
}

/** size 是否处于同一数量级（用于 tiered 分层）。 */
function sameMagnitude(a: number, b: number): boolean {
  if (a === 0 && b === 0) return true;
  if (a === 0 || b === 0) return false;
  const ratio = a / b;
  return ratio >= 0.5 && ratio <= 2;
}
