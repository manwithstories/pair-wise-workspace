/**
 * src/demo.ts —— 终端演示
 *
 * 运行：npx tsx src/demo.ts
 *
 * 输出顺序：
 *   1. 样例日志原文（写入前）
 *   2. 解析出的 AST 与过滤下推计划
 *   3. 逐段命中数与短路跳过数
 *   4. 命中文档 + 各阶段耗时
 */

import { SearchEngine } from "./engine.js";
import { formatAst } from "./parser.js";
import { describePlan, planConditions } from "./pushdown.js";
import type { LogDocument } from "./segment.js";

/* ------------------------------------------------------------------ *
 * 1. 样例日志
 * ------------------------------------------------------------------ */

const SERVICES = ["auth", "billing", "search", "gateway"];
const LEVELS = ["error", "warn", "info"];
const HOSTS = ["10.0.1.7", "10.0.1.8", "10.0.2.3"];

const MESSAGES = [
  "connection timeout while calling upstream auth service",
  "connection timeout while calling upstream billing service",
  "connection reset by peer during handshake",
  "request completed successfully",
  "retrying after transient failure",
  "rate limit exceeded for tenant",
];

/** 确定性伪随机，保证每次演示输出稳定。 */
function makeRng(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state * 1664525 + 1013904223) >>> 0;
    return state / 0x1_0000_0000;
  };
}

/**
 * 造一批结构化日志（模拟 SRE 平台每天千万条的其中一小片）。
 *
 * 关键设计：不同批次刻意落在不同的 (service, level) 组合上。
 * 真实日志按来源分片（auth 集群、billing 集群……），所以一个只关心
 * auth 集群的段，压根就没有其它 service 的倒排链 —— 这正是段级剪枝
 * 能整段跳过的现实基础，也让演示里的"短路跳过"是真实发生的而不是摆设。
 */
const SHARDS: Array<{ service: string; level: string }> = [
  { service: "auth", level: "error" },
  { service: "billing", level: "info" },
  { service: "search", level: "warn" },
  { service: "gateway", level: "info" },
];

function makeDocs(count: number, shardIndex: number, startIndex: number): LogDocument[] {
  const rng = makeRng(42 + shardIndex * 7919);
  const shard = SHARDS[shardIndex]!;
  const docs: LogDocument[] = [];
  for (let i = 0; i < count; i++) {
    // 段内 80% 是该分片的主 service / level，剩下 20% 是别的来源
    const service = rng() < 0.8 ? shard.service : SERVICES[Math.floor(rng() * SERVICES.length)]!;
    const level = rng() < 0.8 ? shard.level : LEVELS[Math.floor(rng() * LEVELS.length)]!;
    const message = MESSAGES[Math.floor(rng() * MESSAGES.length)]!;
    docs.push({
      id: `log-${String(startIndex + i).padStart(6, "0")}`,
      level,
      service,
      trace_id: `trace-${(startIndex + i).toString(16).padStart(8, "0")}`,
      host: HOSTS[Math.floor(rng() * HOSTS.length)]!,
      message,
      latency_ms: Math.floor(rng() * 900),
      status: [200, 200, 200, 201, 400, 401, 404, 429, 500, 502, 503][Math.floor(rng() * 11)]!,
    });
  }
  return docs;
}

/* ------------------------------------------------------------------ *
 * 终端小工具
 * ------------------------------------------------------------------ */

const useColor = process.stdout.isTTY === true && process.env.NO_COLOR === undefined;
const c = (code: string, text: string): string => (useColor ? `\u001b[${code}m${text}\u001b[0m` : text);
const bold = (t: string): string => c("1", t);
const dim = (t: string): string => c("2", t);
const cyan = (t: string): string => c("36", t);
const green = (t: string): string => c("32", t);
const yellow = (t: string): string => c("33", t);
const magenta = (t: string): string => c("35", t);

function heading(step: number, title: string): void {
  console.log();
  console.log(bold(cyan(`── ${step}. ${title} ${"─".repeat(Math.max(0, 62 - title.length))}`)));
}

function ms(v: number): string {
  return v < 1 ? `${v.toFixed(3)}ms` : `${v.toFixed(2)}ms`;
}

/* ------------------------------------------------------------------ *
 * 演示主体
 * ------------------------------------------------------------------ */

async function main(): Promise<void> {
  const QUERY = 'level:error AND service:auth AND message:"connection timeout"';

  /* ---------- 1. 样例日志原文 ---------- */
  heading(1, "样例日志原文（写入前）");

  const BATCH = 15;
  const all = SHARDS.flatMap((_, shard) => makeDocs(BATCH, shard, shard * BATCH));
  for (const doc of all.slice(0, 6)) {
    console.log(dim(JSON.stringify(doc)));
  }
  console.log(dim(`… 共 ${all.length} 条，按来源分 ${SHARDS.length} 个分片写入（每分片 flush 成一个不可变段）`));

  const engine = new SearchEngine({
    dataDir: null, // 演示不落盘；持久化路径见 --persist
    numericFields: ["latency_ms", "status"],
    flushThreshold: 15,
    mergeRunLength: 4,
    tombstoneThreshold: 0.3,
  });

  const tIndex0 = performance.now();
  for (let shard = 0; shard < SHARDS.length; shard++) {
    await engine.index(all.slice(shard * BATCH, (shard + 1) * BATCH));
  }
  const indexMs = performance.now() - tIndex0;
  console.log();
  console.log(bold(`写入 ${all.length} 条，耗时 ${ms(indexMs)}，当前段视图：`));
  engine.stats().forEach((s, i) => {
    const shard = SHARDS[i];
    const tag = shard === undefined ? "" : dim(`（来源 ${shard.service}）`);
    console.log(`   ${magenta(`seg#${s.generation}`)}  文档 ${s.docCount}  存活 ${s.liveCount}  tombstone ${(s.tombstoneRatio * 100).toFixed(0)}%${tag}`);
  });

  /* ---------- 2. AST 与下推计划 ---------- */
  heading(2, "解析的 AST 与过滤下推计划");
  console.log(bold("查询串  ") + yellow(QUERY));

  // 先单独跑一次拿 AST / 计划（不取命中），再跑完整查询。
  const probe = await engine.search(QUERY, { limit: 0 });
  console.log(bold("AST     ") + formatAst(probe.parsed.ast));
  console.log(bold("字段     ") + probe.parsed.fields.join(", "));
  console.log(bold("下推计划 ") + probe.planText);
  console.log(
    bold("下推条件 ") +
      planConditions(probe.plan)
        .map((cond) => {
          switch (cond.type) {
            case "eq":
              return `posting(${cond.field}=${cond.value})`;
            case "exists":
              return `posting(${cond.field}:*)`;
            case "range":
              return `range(${cond.field})`;
            case "term":
              return `posting(_all~${cond.value})`;
            case "phrase":
            case "fieldPhrase":
              return `phrase(${cond.value})`;
          }
        })
        .join(dim(" ∩ ")),
  );
  console.log(dim("说明：等值/范围条件下沉为倒排链交，短路求值；短语先取首词候选再回原文校验。"));

  /* ---------- 3. 逐段命中与短路 ---------- */
  heading(3, "逐段命中数与短路跳过数");

  const result = await engine.search(QUERY, { limit: 5, explain: true });

  console.log(bold("段         文档数  存活  命中  短路  耗时"));
  for (const r of result.segments) {
    const segTag = magenta(`seg#${r.generation}`);
    const hitTag = r.matched > 0 ? green(String(r.matched).padStart(4)) : dim("   0");
    const skipTag = r.skipped ? yellow("skip") : dim("  - ");
    console.log(
      `${segTag}   ${String(r.docCount).padStart(6)}  ${String(r.liveCount).padStart(4)}  ${hitTag}  ${skipTag}  ${dim(ms(r.elapsedMs))}` +
        (r.reason === null ? "" : dim(`   ← ${r.reason}`)),
    );
  }
  console.log();
  console.log(bold("短路汇总 ") + `跳过 ${yellow(String(result.skippedSegments))} / ${result.segments.length} 个段，累计命中 ${green(String(result.totalHits))} 条`);

  /* ---------- 4. 命中原文与耗时 ---------- */
  heading(4, "命中文档与耗时");
  for (const hit of result.hits) {
    console.log(green("  ✓ ") + JSON.stringify(hit));
  }
  if (result.hits.length === 0) console.log(dim("  （无命中）"));

  console.log();
  console.log(bold("耗时拆解 ") + `解析 ${ms(result.parseMs)}  计划+求值 ${ms(result.evalMs)}  ${bold(`合计 ${ms(result.elapsedMs)}`)}`);

  /* ---------- 5. 删除 + 合并 ---------- */
  heading(5, "增量更新：tombstone 与 tiered 合并");

  const before = engine.stats();
  const firstHit = result.hits[0];
  const victim = firstHit === undefined ? undefined : String(firstHit.id);
  if (victim !== undefined) {
    await engine.delete([victim]);
    console.log(bold("删除       ") + `${victim} -> 写 tombstone（段本身不动，写入不阻塞查询）`);
  }

  const afterDelete = await engine.search(QUERY, { limit: 5 });
  console.log(bold("删除后查询 ") + `命中 ${green(String(afterDelete.totalHits))} 条（原来 ${result.totalHits} 条），耗时 ${ms(afterDelete.elapsedMs)}`);

  // 制造 tombstone 超过 30% 的段，触发强制合并
  const seg0 = before[0];
  if (seg0 !== undefined) {
    const victimIds: string[] = [];
    for (let docId = 0; victimIds.length < Math.ceil(seg0.docCount * 0.35); docId++) {
      const id = `log-${String(docId).padStart(6, "0")}`;
      if (id !== victim) victimIds.push(id);
    }
    await engine.delete(victimIds);
  }
  await new Promise((r) => setTimeout(r, 30)); // 让后台强制合并跑完

  const merged = engine.stats();
  console.log();
  console.log(bold("合并前段数 ") + before.length + dim("  ->  ") + bold("合并后段数 ") + merged.length);
  for (const s of merged) {
    console.log(`   ${magenta(`seg#${s.generation}`)}  文档 ${s.docCount}  存活 ${s.liveCount}  tombstone ${(s.tombstoneRatio * 100).toFixed(0)}%`);
  }

  const afterMerge = await engine.search(QUERY, { limit: 5 });
  console.log();
  console.log(bold("合并后查询 ") + `命中 ${green(String(afterMerge.totalHits))} 条，耗时 ${ms(afterMerge.elapsedMs)}（合并后物理空间已回收）`);

  /* ---------- 6. 并发 ---------- */
  heading(6, "并发查询（10 路，互不阻塞）");
  const queries = [
    QUERY,
    "level:warn",
    "service:billing",
    'message:"connection reset"',
    "latency_ms:[100 TO 300]",
    "NOT service:auth",
    "level:error OR level:warn",
    "status:500",
    "trace_id:trace-0000002a",
    "level:*",
  ];
  const tConc = performance.now();
  const results = await Promise.all(queries.map((q) => engine.search(q, { limit: 1 })));
  const concMs = performance.now() - tConc;
  results.forEach((r, i) => {
    const q = queries[i] ?? "";
    console.log(`   ${String(i + 1).padStart(2)}. ${yellow(q.padEnd(42))} 命中 ${green(String(r.totalHits).padStart(3))}  ${dim(ms(r.elapsedMs))}`);
  });
  console.log();
  console.log(bold("10 路总耗时 ") + ms(concMs) + dim("（无共享锁，逐查询私有 scratch buffer）"));

  console.log();
  console.log(bold(green("演示完成")));
}

main().catch((err: unknown) => {
  if (err instanceof Error) {
    console.error(bold(red("演示失败：")) + err.message);
    if (err.stack !== undefined) console.error(dim(err.stack));
  } else {
    console.error(err);
  }
  process.exitCode = 1;
});

function red(t: string): string {
  return c("31", t);
}
