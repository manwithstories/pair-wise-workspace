/**
 * src/inverted.ts —— 倒排表与 posting 跳表
 *
 * 结构选择（每一项都是为了让"百万文档 / 512MB"这条约束成立）：
 *
 *  1. 字段 -> term -> 升序 docId（Int32Array）。段内 docId 稠密且严格递增，
 *     交集/并集直接线性扫描，没有哈希和指针跳转。
 *
 *  2. block-max 跳表：跳表项记录块内最大 docId，`seek(d)` 可整块跳过。
 *     **只给 df 足够大的 posting 建跳表** —— 小 posting 线性扫本来就快，
 *     给它们建跳表纯属浪费内存（这是内存优化的关键之一）。
 *
 *  3. 构建期用「计数排序」而不是 `Map<term, number[]>`：
 *     先把 (term槽位, docId) 平铺进两个可增长 Int32Array，
 *     再按槽位前缀和一次性散射成最终 posting。
 *     前者要为每个 term 维护一个 JS 数组（每篇文档约 390 字节的额外开销），
 *     后者全程只有定长 TypedArray，内存与 GC 压力都低一个数量级。
 *
 *  4. 所有 posting 数组段内私有且构建后只读 —— 查询可以无锁并发读，
 *     这是"构建与查询两阶段分离"的前提。
 */

import type { LogDocument } from "./segment.ts";
import { growInPlace } from "./typedarray.ts";

/** 倒排表里的保留字段：全文兜底（裸词/裸短语落在这里）。 */
export const ALL_FIELD = "_all";

/**
 * 字段级"词"倒排的内部字段名后缀：`message\u0000phrase`。
 * 只有显式声明为文本的字段才建这份索引；裸词/裸短语查询对各文本字段做并集。
 */
export const FIELD_PHRASE_SUFFIX = "\u0000phrase";

/** df 小于该值的 posting 不建跳表：线性扫描已足够，跳表纯属浪费。 */
export const SKIP_LIST_MIN_DF = 32;

/**
 * 共享的空 TypedArray。
 *
 * 段内可能有几十万个 term，每个都持有一个 `new Int32Array(0)` 就是几十万个
 * 小对象 —— 在百万文档规模下这部分开销可观。所有"无跳表"的 posting 共用这一个实例。
 */
const EMPTY_INTS = new Int32Array(0);

/** 一个 term 的倒排链 + 跳表元信息（构建后只读）。 */
export interface Posting {
  /** 升序 docId 序列。 */
  docs: Int32Array;
  /** 跳表：每 stride 个 doc 记一个块内最大 docId；未建跳表时为空。 */
  skipList: Int32Array;
  /** 跳表粒度。 */
  skipStride: number;
  /** 命中文档数（含已删除，墓碑在段层过滤）。 */
  df: number;
}

/** 数值字段索引：值升序 + docId 升序（一一对应）。 */
export interface NumericFieldIndex {
  values: Float64Array;
  docs: Int32Array;
}

/** 倒排表本体：一个段持有的全部索引数据。 */
export interface InvertedIndex {
  /** 字段名 -> (term -> posting)。 */
  fields: Map<string, Map<string, Posting>>;
  /** 数值字段 -> 排序后的 (value, docId)，供范围查询二分。 */
  numeric: Map<string, NumericFieldIndex>;
  /** 建了词级索引的文本字段（裸词/裸短语在这里做并集）。 */
  textFields: Set<string>;
  /** 段内文档总数。 */
  docCount: number;
}

/* ------------------------------------------------------------------ *
 * 跳表
 * ------------------------------------------------------------------ */

/** 跳表粒度：约 sqrt(n) 量级，兼顾跳表内存与跳过效率。 */
export function skipStrideFor(docCount: number): number {
  return Math.max(Math.floor(Math.sqrt(Math.max(docCount, 1))), 1);
}

/**
 * block-max 跳表：`skipList[i] = docs[i*stride .. (i+1)*stride]` 内的最大 docId。
 * 整体单调不减，seek 才能二分。
 */
export function buildSkipList(docs: Int32Array, stride: number): Int32Array {
  if (stride <= 1 || docs.length <= 1) return new Int32Array(0);
  const blocks = Math.ceil(docs.length / stride);
  const out = new Int32Array(blocks);
  for (let b = 0; b < blocks; b++) {
    const start = b * stride;
    const end = Math.min(start + stride, docs.length);
    let max = docs[start]!;
    for (let i = start + 1; i < end; i++) {
      const v = docs[i]!;
      if (v > max) max = v;
    }
    out[b] = max;
  }
  return out;
}

/**
 * block-max 跳表查找：返回第一个 >= target 的下标。
 * 没有跳表（小 posting）时退化为线性扫描。
 */
export function seekPosting(posting: Posting, target: number): number {
  const { docs, skipList, skipStride } = posting;
  const n = docs.length;
  if (n === 0) return 0;
  if (skipList.length === 0) {
    let i = 0;
    while (i < n && docs[i]! < target) i++;
    return i;
  }
  // 跳表有序，二分定位块
  let lo = 0;
  let hi = skipList.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (skipList[mid]! < target) lo = mid + 1;
    else hi = mid;
  }
  let i = lo * skipStride;
  while (i < n && docs[i]! < target) i++;
  return i;
}

/** 由升序 docId 数组封装成 posting（按 df 决定是否建跳表）。 */
export function makePosting(docs: Int32Array, docCount: number): Posting {
  const df = docs.length;
  // 小 posting 线性扫本来就快，跳表纯属浪费内存
  if (df < SKIP_LIST_MIN_DF) {
    return { docs, skipList: EMPTY_INTS, skipStride: 1, df };
  }
  const stride = skipStrideFor(docCount);
  return { docs, skipList: buildSkipList(docs, stride), skipStride: stride, df };
}

/* ------------------------------------------------------------------ *
 * posting 集合运算
 * ------------------------------------------------------------------ */

/**
 * 求交：结果写入 out（调用方提供复用缓冲区）。
 *
 * 短路的关键：以较短的一条为主链，副链用 block-max 跳表推进 ——
 * 当"下一块的最大 docId"仍小于当前 x 时整块跳过，一次比较干掉 skipStride 个元素。
 */
export function intersectPostings(a: Posting, b: Posting, out: Int32Array): Int32Array {
  const [small, large]: [Posting, Posting] = a.docs.length <= b.docs.length ? [a, b] : [b, a];
  const n = small.docs.length;
  const m = large.docs.length;
  const sdocs = small.docs;
  const ldocs = large.docs;
  const stride = large.skipStride;
  const skip = large.skipList;
  const hasSkip = skip.length > 0;
  let k = 0;
  let j = 0;
  for (let i = 0; i < n; i++) {
    const x = sdocs[i]!;
    while (j < m && ldocs[j]! < x) {
      if (hasSkip) {
        const nextBlock = ((j / stride) | 0) + 1;
        if (nextBlock < skip.length && skip[nextBlock]! < x) {
          j = (nextBlock + 1) * stride;
          if (j > m) j = m;
          continue;
        }
      }
      j++;
    }
    if (j >= m) break;
    if (ldocs[j] === x) out[k++] = x;
  }
  return out.subarray(0, k);
}

/** 求并（升序合并去重）。 */
export function unionPostings(a: Posting, b: Posting, out: Int32Array): Int32Array {
  const n = a.docs.length;
  const m = b.docs.length;
  const adocs = a.docs;
  const bdocs = b.docs;
  let i = 0;
  let j = 0;
  let k = 0;
  while (i < n && j < m) {
    const x = adocs[i]!;
    const y = bdocs[j]!;
    if (x < y) {
      out[k++] = x;
      i++;
    } else if (y < x) {
      out[k++] = y;
      j++;
    } else {
      out[k++] = x;
      i++;
      j++;
    }
  }
  while (i < n) out[k++] = adocs[i++]!;
  while (j < m) out[k++] = bdocs[j++]!;
  return out.subarray(0, k);
}

/** 差集：a 中不在 b 里的部分（NOT 走这条路）。 */
export function subtractPostings(a: Posting, b: Posting, out: Int32Array): Int32Array {
  const n = a.docs.length;
  const m = b.docs.length;
  let j = 0;
  let k = 0;
  for (let i = 0; i < n; i++) {
    const x = a.docs[i]!;
    while (j < m && b.docs[j]! < x) j++;
    if (j < m && b.docs[j] === x) continue;
    out[k++] = x;
  }
  return out.subarray(0, k);
}

/** 补集：段内 `[0, docCount)` 中不在 b 里的部分。 */
export function complementPosting(b: Posting, docCount: number, out: Int32Array): Int32Array {
  const m = b.docs.length;
  const bdocs = b.docs;
  let j = 0;
  let k = 0;
  for (let doc = 0; doc < docCount; doc++) {
    while (j < m && bdocs[j]! < doc) j++;
    if (j < m && bdocs[j] === doc) continue;
    out[k++] = doc;
  }
  return out.subarray(0, k);
}

/**
 * 数值统一编码，保证数值与字符串能落在同一个 term 空间里。
 * 前缀 \u0001 是哨兵字符，不会与真实字段值冲突。
 */
export function numericValue(n: number): string {
  return `\u0001${n}`;
}

/** 空 posting（无 df 的 term），保证集合运算的形状统一。 */
export function emptyPosting(): Posting {
  return { docs: EMPTY_INTS, skipList: EMPTY_INTS, skipStride: 1, df: 0 };
}

/* ------------------------------------------------------------------ *
 * 构建期：计数排序
 * ------------------------------------------------------------------ */

/** 槽位表每个块的 int 数量（能放 SLOT_BLOCK/2 个槽位）。 */
const SLOT_BLOCK = 2 * 8192;

/**
 * term 槽位：记录每个 term 在扁平 postings 数组里的 [起点, 长度]。
 *
 * 这里刻意不用 `Slot[]` 对象数组 —— 段内 term 数是十万级，
 * 每个一个对象就是十万个小对象；一个 Int32Array 是定长且连续的。
 */
const SLOT_OFFSET = 0;
const SLOT_LENGTH = 1;

/**
 * 段内倒排表构建器（计数排序，全程 TypedArray）。
 *
 * 用法：逐篇 `markDoc()` 取 docId，再 `addTerm` / `addNumeric`，最后 `build()` 冻结。
 * 构建完成后所有 posting 都是只读的，段即可对外提供无锁并发读。
 */
export class InvertedIndexBuilder {
  /** field -> (term -> 槽位下标)。 */
  private readonly terms = new Map<string, Map<string, number>>();
  /** 槽位表（每槽 2 个 int：[offset, length]），按块增长。 */
  private slots: Int32Array[] = [];
  /** (槽位下标, docId) 平铺数组，计数排序的输入。 */
  private pairSlot = new Int32Array(1 << 16);
  private pairDoc = new Int32Array(1 << 16);
  private pairCount = 0;
  /** 数值字段 -> [(value, docId)]，量级小（每篇每字段一条），直接用普通数组。 */
  private readonly numericRaw = new Map<string, Array<[number, number]>>();
  /** 建了词级索引的文本字段。 */
  private readonly textFields = new Set<string>();
  private count = 0;
  /** 已分配的槽位数。 */
  private slotCount = 0;

  /** 登记一个文本字段：它的取值会额外按词切分建索引。 */
  addTextField(field: string): void {
    this.textFields.add(field);
  }

  /** 写入一条文档的一个字段值。 */
  addTerm(field: string, term: string, docId: number): void {
    let bucket = this.terms.get(field);
    if (bucket === undefined) {
      bucket = new Map<string, number>();
      this.terms.set(field, bucket);
    }
    let slotIdx = bucket.get(term);
    if (slotIdx === undefined) {
      slotIdx = this.slotCount++;
      bucket.set(term, slotIdx);
    }
    this.appendSlot(slotIdx);
    this.appendPair(slotIdx, docId);
  }

  /** 写入数值字段的一个取值（范围查询用）。 */
  addNumeric(field: string, value: number, docId: number): void {
    let list = this.numericRaw.get(field);
    if (list === undefined) {
      list = [];
      this.numericRaw.set(field, list);
    }
    list.push([value, docId]);
  }

  markDoc(): number {
    return this.count++;
  }

  /** 槽位计数 +1。槽位表按块增长，不做翻倍搬移。 */
  private appendSlot(slotIdx: number): void {
    const blockIdx = (slotIdx * 2) / SLOT_BLOCK | 0;
    while (this.slots.length <= blockIdx) this.slots.push(new Int32Array(SLOT_BLOCK));
    const block = this.slots[blockIdx]!;
    const off = (slotIdx * 2) % SLOT_BLOCK;
    block[off + SLOT_LENGTH] = block[off + SLOT_LENGTH]! + 1;
  }

  private slotAt(slotIdx: number, which: number): number {
    const idx = slotIdx * 2 + which;
    const block = this.slots[(idx / SLOT_BLOCK) | 0];
    return block === undefined ? 0 : block[idx % SLOT_BLOCK]!;
  }

  private setSlotAt(slotIdx: number, which: number, value: number): void {
    const idx = slotIdx * 2 + which;
    const blockIdx = (idx / SLOT_BLOCK) | 0;
    while (this.slots.length <= blockIdx) this.slots.push(new Int32Array(SLOT_BLOCK));
    this.slots[blockIdx]![idx % SLOT_BLOCK] = value;
  }

  /**
   * (槽位, docId) 追加到平铺数组尾部。
   *
   * **这里必须原地扩权（把 ArrayBuffer 搬到新 backing store）而不是新建数组**：
   * 翻倍策略下，最后一版的旧数组要等 GC 才释放，百万文档规模会同时存在
   * 多份几十 MB 的 Int32Array，RSS 直接翻几倍。`transfer()` 从不复制，
   * 同一时刻只有一份存活。
   */
  private appendPair(slotIdx: number, docId: number): void {
    if (this.pairCount === this.pairSlot.length) this.growPairs();
    this.pairSlot[this.pairCount] = slotIdx;
    this.pairDoc[this.pairCount] = docId;
    this.pairCount++;
  }

  private growPairs(): void {
    this.pairSlot = growInPlace(this.pairSlot, this.pairSlot.length * 2);
    this.pairDoc = growInPlace(this.pairDoc, this.pairDoc.length * 2);
  }

  get docCount(): number {
    return this.count;
  }

  /**
   * 冻结成只读倒排表。
   *
   * 计数排序：按槽位做前缀和得到每个 term 的写入起点，再把 docId 一次性散射到位。
   * 由于文档是按 docId 升序写入的，每个 term 得到的片段天然升序，无需再排序。
   */
  build(): InvertedIndex {
    const slotCount = this.slotCount;
    const total = this.pairCount;

    // 前缀和：槽位 i 的起点落在扁平 postings 数组的哪个位置
    let cursor = 0;
    for (let i = 0; i < slotCount; i++) {
      this.setSlotAt(i, SLOT_OFFSET, cursor);
      cursor += this.slotAt(i, SLOT_LENGTH);
    }
    const postings = new Int32Array(total);

    // 散射：write[slot] 就是该 slot 当前的写入位置
    const write = new Int32Array(slotCount);
    for (let i = 0; i < slotCount; i++) write[i] = this.slotAt(i, SLOT_OFFSET);

    const pairSlot = this.pairSlot;
    const pairDoc = this.pairDoc;
    for (let i = 0; i < total; i++) {
      const s = pairSlot[i]!;
      postings[write[s]!] = pairDoc[i]!;
      write[s] = write[s]! + 1;
    }

    // 按字段装配 posting
    const fields = new Map<string, Map<string, Posting>>();
    for (const [field, bucket] of this.terms) {
      const out = new Map<string, Posting>();
      for (const [term, slotIdx] of bucket) {
        const start = this.slotAt(slotIdx, SLOT_OFFSET);
        const length = this.slotAt(slotIdx, SLOT_LENGTH);
        out.set(term, makePosting(postings.subarray(start, start + length), this.count));
      }
      fields.set(field, out);
    }

    // 构建期临时结构用完即弃（显式断开，避免 build 之后仍被引用）
    this.slots = [];
    this.pairSlot = EMPTY_INTS;
    this.pairDoc = EMPTY_INTS;
    this.pairCount = 0;

    // 数值排序表
    const numeric = new Map<string, NumericFieldIndex>();
    for (const [field, list] of this.numericRaw) {
      // 按值升序；同值按 docId 升序，保证范围二分的确定性
      list.sort((x, y) => x[0] - y[0] || x[1] - y[1]);
      const values = new Float64Array(list.length);
      const docs = new Int32Array(list.length);
      for (let i = 0; i < list.length; i++) {
        values[i] = list[i]![0];
        docs[i] = list[i]![1];
      }
      numeric.set(field, { values, docs });
    }

    return { fields, numeric, textFields: this.textFields, docCount: this.count };
  }
}
