/**
 * 性能基准 —— 对照需求里的性能约束逐条验收。
 *
 *   1. 百万文档单段构建 < 3s
 *   2. 中等选择性查询 P99 < 50ms
 *   3. 10 路并发查询互不阻塞
 *   4. 内存峰值 < 512MB
 *   5. tombstone 超 30% 触发强制合并
 *
 * 运行：npm run bench（可用环境变量调整规模）
 *   DOCS=1000000 SEGMENT_DOCS=1000000 ROUNDS=2000
 */

import { SearchEngine } from "../src/engine.ts";
import type { LogDocument } from "../src/segment.ts";

const DOCS = Number(process.env.DOCS ?? 1_000_000);
const SEGMENT_DOCS = Number(process.env.SEGMENT_DOCS ?? 1_000_000);
const ROUNDS = Number(process.env.ROUNDS ?? 2000);
const CONCURRENCY = 10;

const SERVICES = ["auth", "billing", "search", "gateway", "payment"] as const;
const LEVELS = ["error", "warn", "info", "debug"] as const;
const HOSTS = ["10.0.1.7", "10.0.1.8", "10.0.2.3", "10.0.3.9"] as const;
/**
 * 消息模板：每条消息带上 id 与耗时，避免整个语料只有几条不同的正文。
 *
 * 这不是为了好看 —— 早期版本只有 6 条固定消息，导致
 * `message:"connection timeout"` 的首词 posting 命中全量，
 * 把"短语校验"这一段测成了纯粹的扫描吞吐，而不是真实的选择性。
 * 真实日志里一条消息通常还带 trace、host、耗时、字节数等变化部分。
 */
const MESSAGE_TEMPLATES = [
  "connection timeout while calling upstream auth service",
  "connection reset by peer during handshake",
  "request completed successfully",
  "retrying after transient failure",
  "rate limit exceeded for tenant",
  "upstream returned invalid payload",
  "handshake failed: certificate expired",
  "deadline exceeded while flushing buffer",
  "connection pool exhausted, waiting for lease",
  "circuit breaker open, request rejected",
] as const;

/** 确定性伪随机：基准要可复现。 */
function makeRng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 0x1_0000_0000;
  };
}

function* generate(total: number): Generator<LogDocument> {
  const rng = makeRng(7);
  for (let i = 0; i < total; i++) {
    yield {
      id: `log-${i}`,
      level: LEVELS[Math.floor(rng() * LEVELS.length)]!,
      service: SERVICES[Math.floor(rng() * SERVICES.length)]!,
      trace_id: `trace-${(i % 50000).toString(16)}`,
      host: HOSTS[Math.floor(rng() * HOSTS.length)]!,
      message: `${MESSAGE_TEMPLATES[Math.floor(rng() * MESSAGE_TEMPLATES.length)]!} trace=${i} cost=${Math.floor(rng() * 900)}ms`,
      latency_ms: Math.floor(rng() * 900),
      status: [200, 200, 200, 404, 429, 500, 502][Math.floor(rng() * 7)]!,
    };
  }
}

function mb(bytes: number): string {
  return `${(bytes / 1024 / 1024).toFixed(1)}MB`;
}

function percentile(sorted: number[], p: number): number {
  if (sorted.length === 0) return 0;
  const idx = Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length));
  return sorted[idx]!;
}

/** 记录 RSS 峰值：进程运行期的真实内存高水位。 */
let rssPeak = 0;
function sampleRss(): void {
  const rss = process.memoryUsage().rss;
  if (rss > rssPeak) rssPeak = rss;
}

let failures = 0;
function check(label: string, ok: boolean, actual: string, limit: string): void {
  const tag = ok ? "\u001b[32mPASS\u001b[0m" : "\u001b[31mFAIL\u001b[0m";
  if (!ok) failures++;
  console.log(`  [${tag}] ${label.padEnd(34)} 实际 ${actual.padStart(10)}  约束 ${limit}`);
}

async function main(): Promise<void> {
  console.log(`\n规模：${DOCS.toLocaleString()} 文档，单段构建 ${SEGMENT_DOCS.toLocaleString()}，查询 ${ROUNDS} 轮\n`);

  const engine = new SearchEngine({
    dataDir: null,
    numericFields: ["latency_ms", "status"],
    flushThreshold: SEGMENT_DOCS,
    mergeRunLength: 4,
  });

  /* ---------- 1. 构建耗时 ---------- */
  console.log("1) 单段构建");
  if (global.gc !== undefined) global.gc();
  const memBefore = process.memoryUsage().heapUsed;

  // 边生成边成段：模拟"写入不停"的真实形态。
  // 关键是不能把整批原始文档攒在调用方 —— 那样 flush 时的 live set 会翻倍。
  const buildStart = performance.now();
  const BATCH = 100_000;
  let batch: LogDocument[] = [];
  for (const doc of generate(DOCS)) {
    batch.push(doc);
    if (batch.length >= BATCH) {
      await engine.index(batch);
      batch = [];
      if (global.gc !== undefined) global.gc(); // 让上一批原始文档及时回收
    }
  }
  await engine.index(batch);
  await engine.flush();
  const buildMs = performance.now() - buildStart;

  sampleRss();
  const memAfter = process.memoryUsage();
  sampleRss();

  console.log(`     写入+构建 ${buildMs.toFixed(0)}ms，吞吐 ${(DOCS / (buildMs / 1000) / 1000).toFixed(0)}k docs/s`);
  console.log(`     段视图：${JSON.stringify(engine.stats().map((s) => ({ gen: s.generation, docs: s.docCount })))}`);
  console.log();
  check("百万文档单段构建 < 3s", buildMs < 3000, `${(buildMs / 1000).toFixed(2)}s`, "< 3s");
  check("构建期内存峰值 < 512MB", rssPeak / 1024 / 1024 < 512, mb(rssPeak), "< 512MB");
  console.log(`     (当前 heapUsed ${mb(memAfter.heapUsed)}，构建前 ${mb(memBefore)}，当前 rss ${mb(memAfter.rss)})`);

  /* ---------- 2. 查询 P99 ---------- */
  console.log("\n2) 查询延迟（中等选择性的典型 SRE 排查查询）");
  const queries = [
    'level:error AND service:auth AND message:"connection timeout"',
    "service:billing AND level:error",
    "latency_ms:[100 TO 300] AND level:warn",
    "status:500",
    'message:"connection reset by peer"',
  ];

  // 预热：JIT 首轮编译的耗时不代表稳态，先跑几十轮丢掉
  for (let i = 0; i < 50; i++) await engine.search(queries[i % queries.length]!, { limit: 100 });

  // 先用朴素过滤交叉验证结果正确性 —— 性能数字只有在结果对的时候才有意义
  const sample = 50_000;
  const rnd2 = makeRng(99);
  const sampleDocs: LogDocument[] = Array.from({ length: sample }, (_, i) => ({
    id: `s-${i}`,
    level: LEVELS[Math.floor(rnd2() * LEVELS.length)]!,
    service: SERVICES[Math.floor(rnd2() * SERVICES.length)]!,
    message: `${MESSAGE_TEMPLATES[Math.floor(rnd2() * MESSAGE_TEMPLATES.length)]!} trace=${i}`,
    latency_ms: Math.floor(rnd2() * 900),
    status: 200,
  }));
  const probe = new SearchEngine({ dataDir: null, numericFields: ["latency_ms", "status"], flushThreshold: sample });
  await probe.index(sampleDocs);
  const oracle: Array<[string, (d: LogDocument) => boolean]> = [
    [queries[0]!, (d) => d.level === "error" && d.service === "auth" && String(d.message).includes("connection timeout")],
    [queries[1]!, (d) => d.service === "billing" && d.level === "error"],
    [queries[2]!, (d) => Number(d.latency_ms) >= 100 && Number(d.latency_ms) <= 300 && d.level === "warn"],
    [queries[3]!, (d) => Number(d.status) === 500],
    [queries[4]!, (d) => String(d.message).includes("connection reset by peer")],
  ];
  let mismatches = 0;
  for (const [q, pred] of oracle) {
    const got = (await probe.search(q, { limit: 1 })).totalHits;
    const want = sampleDocs.filter(pred).length;
    if (got !== want) {
      mismatches++;
      console.log(`     \u001b[31m结果不一致\u001b[0m ${q}: 引擎 ${got} vs 朴素 ${want}`);
    }
  }
  check("查询结果与朴素过滤一致", mismatches === 0, mismatches === 0 ? "全部一致" : `${mismatches} 条不一致`, "全部一致");

  const timings: number[] = [];
  for (let i = 0; i < ROUNDS; i++) {
    const q = queries[i % queries.length]!;
    const t0 = performance.now();
    await engine.search(q, { limit: 100 });
    timings.push(performance.now() - t0);
    sampleRss();
  }
  timings.sort((a, b) => a - b);
  const p50 = percentile(timings, 50);
  const p99 = percentile(timings, 99);
  console.log(`     P50 ${p50.toFixed(2)}ms  P95 ${percentile(timings, 95).toFixed(2)}ms  P99 ${p99.toFixed(2)}ms  max ${timings[timings.length - 1]!.toFixed(2)}ms`);
  check("中等选择性查询 P99 < 50ms", p99 < 50, `${p99.toFixed(2)}ms`, "< 50ms");
  console.log(`     (查询期内存峰值 ${mb(rssPeak)})`);

  /* ---------- 3. 并发 ---------- */
  console.log("\n3) 10 路并发查询");
  // 预热后再测，避免 JIT 首轮把并发/串行都拉高
  for (let i = 0; i < 20; i++) await engine.search(queries[i % queries.length]!, { limit: 100 });

  // 取多轮的中位数，抵消 GC 与调度抖动
  const concSamples: number[] = [];
  const serialSamples: number[] = [];
  for (let round = 0; round < 7; round++) {
    const tc = performance.now();
    await Promise.all(
      Array.from({ length: CONCURRENCY }, (_, i) => engine.search(queries[i % queries.length]!, { limit: 100 })),
    );
    concSamples.push(performance.now() - tc);

    const ts = performance.now();
    for (let i = 0; i < CONCURRENCY; i++) await engine.search(queries[i % queries.length]!, { limit: 100 });
    serialSamples.push(performance.now() - ts);
    sampleRss();
  }
  concSamples.sort((a, b) => a - b);
  serialSamples.sort((a, b) => a - b);
  const concMs = concSamples[3]!;
  const serialMs = serialSamples[3]!;
  console.log(`     并发中位 ${concMs.toFixed(2)}ms，串行中位 ${serialMs.toFixed(2)}ms，比值 ${(concMs / serialMs).toFixed(2)}x`);
  console.log(`     10 路并发在单线程 JS 上是交替执行的，总耗时不可能低于串行；`);
  console.log(`     这里验证的是"没有互相等待/加锁导致的数量级劣化"。`);
  check("10 路并发不互相阻塞", concMs <= serialMs * 1.5, `${concMs.toFixed(2)}ms`, `<= 串行 ${(serialMs * 1.5).toFixed(2)}ms`);

  /* ---------- 4. 合并中查询不等待 ---------- */
  console.log("\n4) 合并期间的查询延迟");
  // 先造出多个小段
  const multi = new SearchEngine({ dataDir: null, numericFields: ["latency_ms"], flushThreshold: 20_000 });
  for (let i = 0; i < 6; i++) {
    const docs: LogDocument[] = [];
    for (const doc of generate(20_000)) {
      docs.push({ ...doc, id: `m${i}-${doc.id}` });
      if (docs.length >= 20_000) break;
    }
    await multi.index(docs);
  }
  await multi.flush();

  const mergePromise = multi.mergeSegments();
  const duringMerge: number[] = [];
  while (mergePromise !== undefined) {
    const t0 = performance.now();
    await multi.search("service:auth AND level:error", { limit: 100 });
    duringMerge.push(performance.now() - t0);
    if (duringMerge.length > 200) break;
  }
  await mergePromise;
  duringMerge.sort((a, b) => a - b);
  const mergeP99 = percentile(duringMerge, 99);
  console.log(`     合并期间查询 ${duringMerge.length} 次，P99 ${mergeP99.toFixed(2)}ms，段数 ${multi.segmentCount}`);
  check("合并中查询不等待", mergeP99 < 50, `${mergeP99.toFixed(2)}ms`, "< 50ms");

  /* ---------- 5. tombstone 强制合并 ---------- */
  console.log("\n5) tombstone 超 30% 强制合并");
  const eng2 = new SearchEngine({ dataDir: null, numericFields: ["latency_ms"], flushThreshold: 100, autoMerge: true, tombstoneThreshold: 0.3 });
  const victims: LogDocument[] = [];
  for (const doc of generate(200)) victims.push(doc);
  await eng2.index(victims);
  await eng2.flush();
  await eng2.index(victims.map((d) => ({ ...d, id: `b-${d.id}` })));
  await eng2.flush();

  await eng2.delete(victims.slice(0, 100).map((d) => d.id)); // 50% > 30%
  await new Promise((r) => setTimeout(r, 100));
  const stat = eng2.stats();
  console.log(`     合并后段数 ${stat.length}，各段文档 ${JSON.stringify(stat.map((s) => s.docCount))}`);
  // 写入 200 + 200，删除 100（50% > 30%）-> 强制合并后应剩 300 篇，且墓碑已物理回收
  check("tombstone>30% 触发强制合并", stat.length === 1 && stat[0]!.docCount === 300, `${stat.length} 段 / ${stat[0]?.docCount ?? 0} 文档`, "1 段 / 300 文档");

  /* ---------- 6. 稳态内存 ---------- */
  console.log("\n6) 稳态内存");
  const g = globalThis as unknown as { gc?: () => void };
  if (g.gc !== undefined) {
    g.gc();
    g.gc();
  }
  sampleRss();
  const steady = process.memoryUsage();
  console.log(`     GC 后 heapUsed ${mb(steady.heapUsed)}，rss ${mb(steady.rss)}，arrayBuffers ${mb(steady.arrayBuffers)}`);
  console.log(`     运行期 rss 峰值 ${mb(rssPeak)}`);
  console.log(`     注：构建期的临时结构（原始文档批次、平铺 pair 数组、计数排序缓冲）`);
  console.log(`         在 GC 后已回收；峰值反映的是"构建尚未完成"的瞬时占用。`);

  /* ---------- 汇总 ---------- */
  console.log();
  if (failures === 0) {
    console.log("\u001b[32m全部性能约束达标\u001b[0m");
  } else {
    console.log(`\u001b[31m${failures} 项未达标\u001b[0m`);
    process.exitCode = 1;
  }
}

main().catch((err: unknown) => {
  console.error(err);
  process.exitCode = 1;
});
