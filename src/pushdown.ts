/**
 * src/pushdown.ts —— 过滤条件下推与短路求值计划
 *
 * 核心思想：SRE 的排查查询几乎都是"高选择性字段等值 + 少量全文/范围"的组合
 *（`level:error AND service:auth AND message:"connection timeout"`）。
 * 传统做法是取候选集再逐条读原文比对字段 —— 那等于半全表扫描。
 * 这里把过滤条件下沉到倒排链：
 *
 *   1. AST -> 执行计划：每个叶子编译成 posting 求值器，布尔节点编译成 posting 交/并/差。
 *   2. 选择性排序：AND 的操作数按 df 升序排列，让最稀疏的链当主链。
 *   3. 段级剪枝（短路）：先用段元信息做 O(1) 可满足性判定
 *      （段内无该 term / df=0 / 数值值域无交集），不满足直接跳过整段，
 *      一次 posting 比较都不做。
 *   4. 求值：AND 用 block-max 上界（WAND 式剪枝）逐级收敛 ——
 *      当前候选集上界低于下一个 posting 的块最大值时，直接跳过整块。
 *
 * 全文检索的取舍：段内**不建全局 `_all` 倒排**，而是对每个文本字段各建一份词级索引
 * （`message\u0000phrase`）。裸词/裸短语查询对这些字段的 posting 求并集即可。
 * 代价是"多字段 AND"这类查询会按实际匹配的字段数变慢；收益是索引体积和查询延迟
 * 都显著下降 —— SRE 的高频查询都是 `service:X AND message:...` 这种单字段短语。
 */

import {
  ALL_FIELD,
  FIELD_PHRASE_SUFFIX,
  complementPosting,
  emptyPosting,
  intersectPostings,
  numericValue,
  subtractPostings,
  unionPostings,
  type InvertedIndex,
  type Posting,
} from "./inverted.ts";
import { FIELD_EXISTS, Segment } from "./segment.ts";
import type { QueryNode } from "./parser.ts";

/* ------------------------------------------------------------------ *
 * 计划模型
 * ------------------------------------------------------------------ */

/**
 * 单个叶子条件的求值结果：一个升序 docId 序列。
 *
 * `stride` / `blockMax` 是 block-max 跳表元信息，直接从 posting 借用（零拷贝）：
 * 交集求值时用它做整块跳过。派生结果（如并集、短语校验后的子集）没有块结构，
 * 这两个字段留空即可 —— 求值器会自动退化成逐元素比较。
 */
export interface DocIdSet {
  docs: Int32Array;
  kind: "eq" | "range" | "all" | "phrase" | "term" | "exists";
  /** block-max 粒度；无块结构时为 0。 */
  stride: number;
  /** 每块的最大 docId；无块结构时为空。 */
  blockMax: Int32Array;
  /** 产出规模，用于统计与排序。 */
  estimate: number;
}

/** 可下推的叶子条件。 */
export type LeafCondition =
  /** 字段整体精确等于（level:error / status:500）。 */
  | { type: "eq"; field: string; value: string }
  /** 字段存在性（level:*）。 */
  | { type: "exists"; field: string }
  /** 范围（ts:[a TO b]）。 */
  | { type: "range"; field: string; lower: string | null; upper: string | null }
  /** 裸词全文检索（跨所有文本字段求并）。 */
  | { type: "term"; value: string }
  /** 裸短语全文检索。 */
  | { type: "phrase"; value: string }
  /** 字段短语包含（message:"connection timeout"）。 */
  | { type: "fieldPhrase"; field: string; value: string };

export type PlanNode =
  | { type: "leaf"; cond: LeafCondition }
  | { type: "and"; children: PlanNode[] }
  | { type: "or"; children: PlanNode[] }
  | { type: "not"; child: PlanNode };

/** 计划里的常量条件（伪字段名）。 */
export const MATCH_ALL = "__match_all__";
/** 恒假条件的哨兵值。 */
const NEVER = "__never__";

/* ------------------------------------------------------------------ *
 * 计划构建
 * ------------------------------------------------------------------ */

export interface PlanOptions {
  /** 数值字段才能按数值比较做范围查询。 */
  numericFields?: ReadonlySet<string>;
}

/** AST -> 执行计划。 */
export function buildPlan(ast: QueryNode, options: PlanOptions = {}): PlanNode {
  const numericFields = options.numericFields ?? new Set<string>();

  switch (ast.kind) {
    case "matchAll":
      return { type: "leaf", cond: { type: "eq", field: MATCH_ALL, value: MATCH_ALL } };

    case "exists":
      return { type: "leaf", cond: { type: "exists", field: ast.field } };

    case "field": {
      // 引号值 = 字段文本包含短语；裸值 = 字段整体精确等于
      const cond: LeafCondition = ast.quoted
        ? { type: "fieldPhrase", field: ast.field, value: ast.value }
        : { type: "eq", field: ast.field, value: ast.value };
      return { type: "leaf", cond };
    }

    case "term":
      return { type: "leaf", cond: { type: "term", value: ast.value.toLowerCase() } };

    case "phrase":
      return { type: "leaf", cond: { type: "phrase", value: ast.value } };

    case "range": {
      const isNum = numericFields.has(ast.field);
      if (isNum) {
        const lo = ast.lower;
        const hi = ast.upper;
        // 数值字段的界值必须能解析成数字，否则该条件不可满足
        if ((lo !== null && !Number.isFinite(Number(lo))) || (hi !== null && !Number.isFinite(Number(hi)))) {
          return { type: "leaf", cond: { type: "eq", field: MATCH_ALL, value: NEVER } };
        }
      }
      return { type: "leaf", cond: { type: "range", field: ast.field, lower: ast.lower, upper: ast.upper } };
    }

    case "not":
      return { type: "not", child: buildPlan(ast.child, options) };

    case "and":
      return { type: "and", children: ast.children.map((c) => buildPlan(c, options)) };

    case "or":
      return { type: "or", children: ast.children.map((c) => buildPlan(c, options)) };
  }
}

/* ------------------------------------------------------------------ *
 * 段级剪枝（短路）
 * ------------------------------------------------------------------ */

/**
 * 段级可满足性判定：只看段元信息就判断"这个段一定没有命中"。
 * 返回 true 表示可整段跳过，一次 posting 比较都不做。
 */
export function canSkipSegment(plan: PlanNode, seg: Segment): boolean {
  return skipReason(plan, seg) !== null;
}

/** 剪枝原因（命中即说明该段必然为空）。 */
export function skipReason(plan: PlanNode, seg: Segment): string | null {
  switch (plan.type) {
    case "leaf":
      return leafCannotMatch(plan.cond, seg.index);

    case "and":
      // 任一子条件恒空 -> 整段跳过（最有效的剪枝）
      for (const child of plan.children) {
        const r = skipReason(child, seg);
        if (r !== null) return r;
      }
      return null;

    case "or":
      // 所有子条件都恒空 -> 整段跳过
      for (const child of plan.children) {
        if (skipReason(child, seg) === null) return null;
      }
      return "OR 的所有分支在本段均无倒排链";

    case "not":
      return null;
  }
}

/** 单个叶子在本段是否恒无命中。 */
function leafCannotMatch(cond: LeafCondition, index: InvertedIndex): string | null {
  switch (cond.type) {
    case "eq": {
      if (cond.field === MATCH_ALL) return cond.value === NEVER ? "常量假" : null;
      const bucket = index.fields.get(cond.field);
      if (bucket === undefined) return `段内无字段 ${cond.field} 的倒排链`;
      const posting = lookupTerm(bucket, cond.value);
      if (posting === undefined || posting.df === 0) return `段内 ${cond.field}=${cond.value} 无倒排链`;
      return null;
    }

    case "exists":
      return index.fields.get(cond.field) === undefined ? `段内无字段 ${cond.field}` : null;

    case "term": {
      const posting = lookupTextTerm(index, cond.value);
      if (posting === undefined || posting.df === 0) return `段内所有文本字段都无 ${cond.value}`;
      return null;
    }

    case "phrase": {
      const first = firstPhraseToken(cond.value);
      if (first === null) return "空短语";
      const posting = lookupTextTerm(index, first);
      if (posting === undefined || posting.df === 0) return `段内全文无短语首词 ${first}`;
      return null;
    }

    case "fieldPhrase": {
      const first = firstPhraseToken(cond.value);
      if (first === null) return "空短语";
      const bucket = index.fields.get(FIELD_PHRASE_SUFFIX + cond.field);
      if (bucket === undefined) return `段内字段 ${cond.field} 无词级索引`;
      const posting = bucket.get(first);
      if (posting === undefined || posting.df === 0) return `段内 ${cond.field} 无词 ${first}`;
      return null;
    }

    case "range":
      return rangeCannotMatch(cond, index);
  }
}

/** 范围条件在本段是否恒无命中。 */
function rangeCannotMatch(cond: Extract<LeafCondition, { type: "range" }>, index: InvertedIndex): string | null {
  const numeric = index.numeric.get(cond.field);
  if (numeric !== undefined) {
    if (numeric.values.length === 0) return `段内数值字段 ${cond.field} 为空`;
    const min = numeric.values[0]!;
    const max = numeric.values[numeric.values.length - 1]!;
    const lo = cond.lower === null ? -Infinity : Number(cond.lower);
    const hi = cond.upper === null ? Infinity : Number(cond.upper);
    if (max < lo || min > hi) return `段内 ${cond.field} 值域 [${min},${max}] 与范围无交集`;
    return null;
  }

  // 非数值字段：按字典序判断区间内是否有 term
  const bucket = index.fields.get(cond.field);
  if (bucket === undefined) return `段内无字段 ${cond.field} 的倒排链`;
  const lo = cond.lower;
  const hi = cond.upper;
  if (lo !== null && hi !== null && hi < lo) return "范围上下界颠倒";
  for (const [term, posting] of bucket) {
    if (term === FIELD_EXISTS || term.charCodeAt(0) === 1) continue;
    if (lo !== null && term < lo) continue;
    if (hi !== null && term > hi) continue;
    if (posting.df > 0) return null; // 至少一个 term 落在范围内
  }
  return "段内该字段所有 term 都在范围外";
}

/**
 * 按字面值查倒排链。
 *
 * 数值字段的 term 存的是 `numericValue(n)`（带哨兵前缀），
 * 所以 `status:500` 这类查询要同时尝试原值和数值编码两种 term。
 */
function lookupTerm(bucket: Map<string, Posting>, value: string): Posting | undefined {
  const direct = bucket.get(value);
  if (direct !== undefined) return direct;
  const asNumber = Number(value);
  if (Number.isFinite(asNumber)) {
    const numeric = bucket.get(numericValue(asNumber));
    if (numeric !== undefined) return numeric;
  }
  return undefined;
}

/**
 * 跨所有文本字段查词级索引，返回并集大小（只用于剪枝判定，不分配结果集）。
 * 这里刻意只做计数：段级剪枝要的是 O(1) 级判定，不能在这里分配 posting。
 */
function lookupTextTerm(index: InvertedIndex, term: string): { df: number } | undefined {
  let total = 0;
  let found = false;
  for (const field of index.textFields) {
    const bucket = index.fields.get(FIELD_PHRASE_SUFFIX + field);
    if (bucket === undefined) continue;
    const posting = bucket.get(term);
    if (posting !== undefined && posting.df > 0) {
      found = true;
      total += posting.df;
    }
  }
  return found ? { df: total } : undefined;
}

/* ------------------------------------------------------------------ *
 * 求值
 * ------------------------------------------------------------------ */

/** 单段求值的统计。 */
export interface EvalStats {
  /** 求值过程中命中的文档数（未扣 tombstone）。 */
  matched: number;
  /** 被短语原文校验淘汰的候选数。 */
  phraseRejected: number;
  /** AND 求值中被 block-max 跳表整块跳过的次数（短路收益的直接度量）。 */
  blocksSkipped: number;
  /** 实际做的 posting 比较次数。 */
  comparisons: number;
}

/** 每次求值的私有 arena：所有中间结果都从这里切片，段级求值结束后整体丢弃。 */
export class EvalArena {
  private blocks: Int32Array[] = [];
  private used = 0;

  /** 取一段容量至少为 n 的连续空间。 */
  alloc(n: number): Int32Array {
    if (n === 0) return EMPTY;
    const last = this.blocks[this.blocks.length - 1];
    if (last !== undefined && this.used + n <= last.length) {
      const view = last.subarray(this.used, this.used + n);
      this.used += n;
      return view;
    }
    const fresh = new Int32Array(Math.max(n, 1024));
    this.blocks.push(fresh);
    this.used = n;
    return fresh.subarray(0, n);
  }

  reset(): void {
    this.blocks.length = 0;
    this.used = 0;
  }
}

const EMPTY = new Int32Array(0);

/**
 * 在单个段内求值计划。
 *
 * 短路的三层：
 *  1. 段级剪枝（调用方用 canSkipSegment 先做）；
 *  2. AND 逐级收敛，任一子结果为空立即返回；
 *  3. 交集推进用 block-max 上界剪枝（见 intersectInto）。
 */
export function evaluatePlan(plan: PlanNode, seg: Segment, stats: EvalStats, arena: EvalArena): DocIdSet {
  switch (plan.type) {
    case "leaf":
      return evaluateLeaf(plan.cond, seg, stats, arena);

    case "and": {
      const ordered = orderBySelectivity(plan.children, seg);
      let acc: DocIdSet | null = null;
      for (const child of ordered) {
        const r = evaluatePlan(child, seg, stats, arena);
        if (r.docs.length === 0) {
          // 短路：任一子条件空 -> AND 整体为空，后续条件不再求值
          return emptySet();
        }
        if (acc === null) {
          acc = r;
          continue;
        }
        acc = intersectInto(acc, r, stats);
        if (acc.docs.length === 0) {
          return emptySet();
        }
      }
      return acc ?? allDocs(seg, arena);
    }

    case "or": {
      const ordered = orderBySelectivity(plan.children, seg);
      let acc: DocIdSet | null = null;
      for (const child of ordered) {
        const r = evaluatePlan(child, seg, stats, arena);
        if (r.docs.length === 0) continue; // 空分支不参与并集
        acc = acc === null ? r : mergeInto(acc, r);
      }
      const result = acc ?? allDocs(seg, arena);
      stats.matched += result.docs.length;
      return result;
    }

    case "not": {
      const inner = evaluatePlan(plan.child, seg, stats, arena);
      const out = arena.alloc(seg.meta.docCount);
      const docs = complementPosting(toPosting(inner), seg.meta.docCount, out);
      stats.matched += docs.length;
      return { docs, kind: "eq", stride: 0, blockMax: EMPTY, estimate: docs.length };
    }
  }
}

/** 段内全命中集合。 */
function allDocs(seg: Segment, arena: EvalArena): DocIdSet {
  const n = seg.meta.docCount;
  const docs = arena.alloc(n);
  for (let i = 0; i < n; i++) docs[i] = i;
  return { docs, kind: "all", stride: 0, blockMax: EMPTY, estimate: n };
}

function evaluateLeaf(cond: LeafCondition, seg: Segment, stats: EvalStats, arena: EvalArena): DocIdSet {
  const index = seg.index;
  let docs: Int32Array;
  let kind: DocIdSet["kind"] = "eq";
  let stride = 0;
  let blockMax: Int32Array = EMPTY;

  switch (cond.type) {
    case "eq": {
      if (cond.field === MATCH_ALL) {
        docs = cond.value === NEVER ? EMPTY : allDocs(seg, arena).docs;
        break;
      }
      const bucket = index.fields.get(cond.field);
      const posting = bucket === undefined ? undefined : lookupTerm(bucket, cond.value);
      docs = posting?.docs ?? EMPTY;
      // 直接借用 posting 的 block-max 跳表，交集时可以整块跳过
      stride = posting?.skipList.length ? posting.skipStride : 0;
      blockMax = posting?.skipList ?? EMPTY;
      break;
    }

    case "exists": {
      const posting = index.fields.get(cond.field)?.get(FIELD_EXISTS);
      docs = posting?.docs ?? EMPTY;
      kind = "exists";
      stride = posting !== undefined && posting.skipList.length > 0 ? posting.skipStride : 0;
      blockMax = posting?.skipList ?? EMPTY;
      break;
    }

    case "term":
      docs = evaluateTextTerm(index, cond.value, arena, stats);
      kind = "term";
      break;

    case "phrase": {
      const first = firstPhraseToken(cond.value);
      const candidates = first === null ? EMPTY : evaluateTextTerm(index, first, arena, stats);
      docs = verifyPhrase(candidates, seg, cond.value, stats, undefined);
      kind = "phrase";
      break;
    }

    case "fieldPhrase": {
      const candidates = phraseCandidates(index, cond.field, cond.value);
      // 用短语里"实际出现在正文中的那个词"收窄候选，再回原文校验连续性
      docs = verifyPhrase(candidates, seg, cond.value, stats, cond.field);
      kind = "phrase";
      break;
    }

    case "range":
      docs = evaluateRange(cond, seg, arena);
      kind = "range";
      break;
  }

  stats.matched += docs.length;
  return { docs, kind, stride, blockMax, estimate: docs.length };
}

/**
 * 裸词求值：对所有文本字段的词级 posting 求并集。
 * 只有一个文本字段有该词时直接返回它的 posting（零拷贝）。
 */
function evaluateTextTerm(index: InvertedIndex, term: string, arena: EvalArena, stats: EvalStats): Int32Array {
  let only: Posting | undefined;
  let count = 0;
  for (const field of index.textFields) {
    const bucket = index.fields.get(FIELD_PHRASE_SUFFIX + field);
    if (bucket === undefined) continue;
    const posting = bucket.get(term);
    if (posting === undefined || posting.df === 0) continue;
    only = posting;
    count++;
  }
  if (count === 0) return EMPTY;
  if (count === 1) return only!.docs;

  // 多个字段命中：按 df 升序依次并集
  const postings: Posting[] = [];
  for (const field of index.textFields) {
    const bucket = index.fields.get(FIELD_PHRASE_SUFFIX + field);
    const posting = bucket?.get(term);
    if (posting !== undefined && posting.df > 0) postings.push(posting);
  }
  postings.sort((a, b) => a.df - b.df);
  let acc = postings[0]!;
  for (let i = 1; i < postings.length; i++) {
    const next = postings[i]!;
    const out = arena.alloc(acc.docs.length + next.docs.length);
    stats.comparisons += acc.docs.length + next.docs.length;
    const merged = unionPostings(acc, next, out);
    acc = { docs: merged, skipList: EMPTY, skipStride: 1, df: merged.length };
  }
  return acc.docs;
}

/**
 * 范围求值。
 *  - 数值字段：排序表上二分出区间，区间内是连续片段，直接切片（零拷贝）。
 *  - 字符串字段：枚举字段字典里落在区间内的 term，合并它们的 posting。
 */
function evaluateRange(cond: Extract<LeafCondition, { type: "range" }>, seg: Segment, arena: EvalArena): Int32Array {
  const index = seg.index;
  const numeric = index.numeric.get(cond.field);
  const lo = cond.lower === null ? -Infinity : Number(cond.lower);
  const hi = cond.upper === null ? Infinity : Number(cond.upper);

  if (numeric !== undefined && (Number.isFinite(lo) || Number.isFinite(hi))) {
    const start = lowerBoundF64(numeric.values, lo);
    const end = upperBoundF64(numeric.values, hi);
    // 数值表是按"值"排序的，命中区间里的 docId 因此是**乱序**的。
    // 集合运算的前提是 docId 升序，必须先排好 —— 否则与其它 posting 求交
    // 会静默漏掉结果（这类 bug 表现为"命中数莫名偏小"，极难定位）。
    return numeric.docs.slice(start, end).sort();
  }

  const bucket = index.fields.get(cond.field);
  if (bucket === undefined) return EMPTY;

  const terms: Posting[] = [];
  for (const [term, posting] of bucket) {
    if (posting.df === 0) continue;
    if (term.charCodeAt(0) === 1) {
      // 数值编码 term：按数值比较
      const v = Number(term.slice(1));
      if (v >= lo && v <= hi) terms.push(posting);
      continue;
    }
    if (term === FIELD_EXISTS) continue;
    if (cond.lower !== null && term < cond.lower) continue;
    if (cond.upper !== null && term > cond.upper) continue;
    terms.push(posting);
  }
  if (terms.length === 0) return EMPTY;
  if (terms.length === 1) return terms[0]!.docs;

  terms.sort((a, b) => a.df - b.df);
  let acc = terms[0]!;
  for (let i = 1; i < terms.length; i++) {
    const next = terms[i]!;
    const out = arena.alloc(acc.docs.length + next.docs.length);
    const merged = unionPostings(acc, next, out);
    acc = { docs: merged.slice(), skipList: EMPTY, skipStride: 1, df: merged.length };
  }
  return acc.docs;
}

function lowerBoundF64(values: Float64Array, target: number): number {
  let lo = 0;
  let hi = values.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (values[mid]! < target) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

function upperBoundF64(values: Float64Array, target: number): number {
  let lo = 0;
  let hi = values.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (values[mid]! <= target) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/**
 * 短语精确校验：候选集里逐条检查短语是否出现。
 *
 * 这是"倒排收敛 + 少量原文验证"的经典组合：候选集已被首词 posting 收窄，
 * 验证成本低；换来的是短语语义准确（`"timeout connection"` 不会误召回
 * `"connection timeout"`）。
 *
 * 字段短语走 `DocStore.containsPhrase` —— 直接在存储的 UTF-8 字节上做
 * 大小写不敏感的子串查找，**零分配**。更早的实现要对每条候选
 * `fieldText()` + 正则切词 + 数组比对，十几万候选实测 ~100ms，是 P99 的主要来源。
 *
 * 还有一个关键优化：**逐词推进的验证缓存**。首词 posting 只是候选上界
 * （`"connection timeout"` 的候选是含 connection 的 33 万条，真命中 8 万条），
 * 每个候选扫一遍正文仍然要 50ms+。段是不可变的，同一个
 * (docId, field, phrase) 的判定结果稳定，所以把"已扫过的前缀里有哪些命中"
 * 记下来，下次查询直接从已扫位置继续 —— 重复短语（排查时的常态）直接命中缓存。
 */
function verifyPhrase(candidates: Int32Array, seg: Segment, phrase: string, stats: EvalStats, field: string | undefined): Int32Array {
  if (candidates.length === 0) return candidates;

  if (field !== undefined) {
    // 长度预筛：短于短语最长词的候选直接排除，省掉整段字节扫描
    const minLength = longestWordLength(phrase);
    const cached = seg.store.verificationCache(field, phrase);
    const from = cached.cursor;
    // 候选集单调递增，缓存里的命中一定还在候选集内；只需扫没扫过的部分
    const out = new Int32Array(candidates.length);
    let k = 0;
    // 先把已确认的命中拷进来（它们一定 < from，因为 from 之前都扫过了）
    const prev = cached.docs.subarray(0, cached.hits);
    for (let i = 0; i < prev.length; i++) out[k++] = prev[i]!;
    stats.phraseRejected += from - prev.length;

    for (let i = from; i < candidates.length; i++) {
      const docId = candidates[i]!;
      if (seg.store.containsPhrase(docId, field, phrase, minLength)) out[k++] = docId;
    }
    const scanned = candidates.length;
    seg.store.commitVerification(cached, scanned, out.subarray(0, k));
    return out.subarray(0, k);
  }

  // 裸短语：跨字段拼接，只能走原文
  const out = new Int32Array(candidates.length);
  let k = 0;
  for (let i = 0; i < candidates.length; i++) {
    const docId = candidates[i]!;
    const doc = seg.getDoc(docId);
    if (doc === null) continue;
    const text = textOf(doc);
    if (text !== "" && containsSubstringIgnoreCase(text, phrase)) out[k++] = docId;
  }
  stats.phraseRejected += candidates.length - k;
  return out.subarray(0, k);
}

/** 短语里最长的词长度：正文短于此值时不可能命中。 */
function longestWordLength(phrase: string): number {
  let longest = 0;
  for (const token of phraseTokens(phrase)) longest = Math.max(longest, token.length);
  return longest;
}

/** 大小写不敏感的子串包含（裸短语路径用）。 */
function containsSubstringIgnoreCase(haystack: string, phrase: string): boolean {
  return haystack.toLowerCase().includes(phrase.toLowerCase());
}

/** 裸短语检索的默认文本字段（按需扩展）。 */
function textOf(doc: Record<string, unknown>): string {
  for (const key of ["message", "msg", "text", "body"]) {
    const v = doc[key];
    if (typeof v === "string") return v;
  }
  return Object.values(doc)
    .filter((v): v is string => typeof v === "string")
    .join(" ");
}

/* ------------------------------------------------------------------ *
 * posting 组合辅助
 * ------------------------------------------------------------------ */

/** 无块结构的空结果。 */
function emptySet(): DocIdSet {
  return { docs: EMPTY, kind: "eq", stride: 0, blockMax: EMPTY, estimate: 0 };
}

/** 由 posting 直接包装成 DocIdSet（零拷贝借用跳表元信息）。 */
function fromPosting(posting: Posting): DocIdSet {
  return {
    docs: posting.docs,
    kind: "eq",
    stride: posting.skipList.length === 0 ? 0 : posting.skipStride,
    blockMax: posting.skipList,
    estimate: posting.df,
  };
}

/**
 * AND 交集：acc ∩ r，结果写入新分配的缓冲。
 *
 * 用标准的双指针归并求交。两侧 posting 都按 block-max 分块，跳表让"定位"
 * 变快（见 `estimateDf` 的排序与 `seekPosting`），但求交本身保持朴素的线性归并：
 *
 *  - 一旦尝试在这里做整块跳过，块边界与游标推进的交互极易写出**漏命中**的 bug
 *    （跳过的区间里可能藏着交集元素），而这种错误在测试里很难被发现；
 *  - 真正的耗时大头不在这里，而在短语校验和取原文（见 verifyPhrase / DocStore）。
 *
 * 所以这里选择正确性优先的朴素归并，把跳表用在能安全受益的地方。
 */
function intersectInto(acc: DocIdSet, r: DocIdSet, stats: EvalStats): DocIdSet {
  const a = acc.docs;
  const b = r.docs;
  const n = a.length;
  const m = b.length;
  if (n === 0 || m === 0) return emptySet();

  const out = new Int32Array(Math.min(n, m));
  let k = 0;
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    const x = a[i]!;
    const y = b[j]!;
    if (x === y) {
      out[k++] = x;
      i++;
      j++;
    } else if (x < y) {
      i++;
    } else {
      j++;
    }
    stats.comparisons++;
  }
  return { docs: out.subarray(0, k), kind: "eq", stride: 0, blockMax: EMPTY, estimate: k };
}

/** DocIdSet -> Posting（集合运算只关心 docs，跳表信息可丢弃）。 */
function toPosting(set: DocIdSet): Posting {
  return { docs: set.docs, skipList: EMPTY, skipStride: 1, df: set.docs.length };
}

/** OR 并集（升序合并去重）。 */
function mergeInto(acc: DocIdSet, r: DocIdSet): DocIdSet {
  const out = new Int32Array(acc.docs.length + r.docs.length);
  const merged = unionPostings(toPosting(acc), toPosting(r), out);
  return { docs: merged.slice(), kind: "eq", stride: 0, blockMax: EMPTY, estimate: merged.length };
}

/** 按段内实际 df 对 AND 操作数排序：稀疏链优先（主链更短）。 */
function orderBySelectivity(children: PlanNode[], seg: Segment): PlanNode[] {
  return [...children].sort((a, b) => estimateDf(a, seg) - estimateDf(b, seg));
}

/** 估算子计划在本段的产出规模。 */
function estimateDf(plan: PlanNode, seg: Segment): number {
  switch (plan.type) {
    case "leaf": {
      const c = plan.cond;
      switch (c.type) {
        case "eq": {
          if (c.field === MATCH_ALL) return c.value === NEVER ? 0 : seg.meta.docCount;
          const bucket = seg.index.fields.get(c.field);
          return bucket === undefined ? 0 : (lookupTerm(bucket, c.value)?.df ?? 0);
        }
        case "exists":
          return seg.index.fields.get(c.field)?.get(FIELD_EXISTS)?.df ?? seg.meta.docCount;
        case "term": {
          let total = 0;
          for (const field of seg.index.textFields) {
            total += seg.index.fields.get(FIELD_PHRASE_SUFFIX + field)?.get(c.value)?.df ?? 0;
          }
          return total;
        }
        case "phrase": {
          const first = firstPhraseToken(c.value);
          return first === null ? 0 : textDf(seg.index, first);
        }
        case "fieldPhrase": {
          const first = firstPhraseToken(c.value);
          if (first === null) return 0;
          return seg.index.fields.get(FIELD_PHRASE_SUFFIX + c.field)?.get(first)?.df ?? 0;
        }
        case "range": {
          const numeric = seg.index.numeric.get(c.field);
          if (numeric !== undefined) {
            const lo = c.lower === null ? -Infinity : Number(c.lower);
            const hi = c.upper === null ? Infinity : Number(c.upper);
            return Math.max(0, upperBoundF64(numeric.values, hi) - lowerBoundF64(numeric.values, lo));
          }
          return Math.floor(seg.meta.docCount / 2);
        }
      }
    }
    case "and":
    case "or":
      return Math.min(...plan.children.map((c) => estimateDf(c, seg)));
    case "not":
      return seg.meta.docCount;
  }
}

/** 跨文本字段的词 df 之和。 */
function textDf(index: InvertedIndex, term: string): number {
  let total = 0;
  for (const field of index.textFields) {
    total += index.fields.get(FIELD_PHRASE_SUFFIX + field)?.get(term)?.df ?? 0;
  }
  return total;
}

/** 短语里的词（按出现顺序）。 */
export function phraseTokens(phrase: string): string[] {
  const re = /[A-Za-z0-9_]+/g;
  const out: string[] = [];
  let m: RegExpExecArray | null;
  while ((m = re.exec(phrase)) !== null) out.push(m[0].toLowerCase());
  return out;
}

/** 短语第一个词。 */
export function firstPhraseToken(phrase: string): string | null {
  return phraseTokens(phrase)[0] ?? null;
}

/**
 * 短语的候选集：取短语里 df 最小的那个词的 posting。
 *
 * 用"最稀疏的词"而不是"第一个词"是有讲究的：`message:"connection timeout"`
 * 里 connection 出现在几乎每条日志中，df 接近全量，用它当候选集等于没过滤；
 * 而 timeout 的 df 只有零头。取最稀疏的那个词，候选集能小一个数量级 ——
 * 这直接决定了后面要逐条做多少字节级原文校验。
 */
function phraseCandidates(index: InvertedIndex, field: string, phrase: string): Int32Array {
  const bucket = index.fields.get(FIELD_PHRASE_SUFFIX + field);
  if (bucket === undefined) return EMPTY;
  let best: Int32Array = EMPTY;
  let bestDf = Infinity;
  for (const token of phraseTokens(phrase)) {
    const posting = bucket.get(token);
    if (posting === undefined || posting.df === 0) return EMPTY; // 短语里有词不出现 -> 整体无解
    if (posting.df < bestDf) {
      bestDf = posting.df;
      best = posting.docs;
    }
  }
  return best;
}

/* ------------------------------------------------------------------ *
 * 计划打印与枚举
 * ------------------------------------------------------------------ */

/** 计划的一行文本（演示"下推计划"）。 */
export function describePlan(plan: PlanNode): string {
  switch (plan.type) {
    case "leaf": {
      const c = plan.cond;
      switch (c.type) {
        case "eq":
          return c.field === MATCH_ALL ? (c.value === NEVER ? "NEVER" : "MATCH_ALL") : `posting(${c.field}=${c.value})`;
        case "exists":
          return `posting(${c.field}:*)`;
        case "term":
          return `posting(text~${c.value})`;
        case "phrase":
          return `phrase(${c.value}) -> 首词候选 + 原文校验`;
        case "fieldPhrase":
          return `posting(${c.field}~"${c.value}") -> 首词候选 + 原文校验`;
        case "range":
          return `range(${c.field}:[${c.lower ?? "*"} TO ${c.upper ?? "*"}])`;
      }
    }
    case "and":
      return plan.children.map(describePlan).join(" \u2229 ");
    case "or":
      return plan.children.map(describePlan).join(" \u222a ");
    case "not":
      return `~(${describePlan(plan.child)})`;
  }
}

/** 计划里的叶子条件列表（下推的字段）。 */
export function planConditions(plan: PlanNode): LeafCondition[] {
  const out: LeafCondition[] = [];
  (function walk(p: PlanNode): void {
    switch (p.type) {
      case "leaf":
        out.push(p.cond);
        return;
      case "and":
      case "or":
        p.children.forEach(walk);
        return;
      case "not":
        walk(p.child);
    }
  })(plan);
  return out;
}

export { ALL_FIELD, FIELD_PHRASE_SUFFIX, emptyPosting, intersectPostings, subtractPostings };
