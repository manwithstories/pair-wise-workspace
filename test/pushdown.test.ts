/**
 * 功能 3：过滤条件下推与短路求值
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { parseAst } from "../src/parser.ts";
import {
  EvalArena,
  buildPlan,
  canSkipSegment,
  describePlan,
  evaluatePlan,
  planConditions,
  type EvalStats,
} from "../src/pushdown.ts";
import { Segment, type LogDocument } from "../src/segment.ts";

const NUMERIC = new Set(["latency_ms"]);

/**
 * 造一个"分片"段：level 与 message 只用该段独有的取值，
 * 这样"段内没有该 term -> 整段跳过"才是可验证的真实场景。
 */
function makeSegment(gen: number, service: string, count: number): Segment {
  const docs: LogDocument[] = Array.from({ length: count }, (_, i) => ({
    id: `s${gen}-${i}`,
    level: i % 3 === 0 ? `${service}-error` : `${service}-info`,
    service,
    // message 的三种形态里，只有第一种同时含 "connection" 和 "timeout"，
    // 因此倒排给的候选集必然大于真实命中数 —— 可以验证"原文校验淘汰多余候选"这一步。
    message:
      i % 3 === 0
        ? `connection timeout on ${service}`
        : i % 3 === 1
          ? `connection established to ${service}`
          : `ok ${service} request`,
    latency_ms: i * 10,
  }));
  return Segment.build({ docs, generation: gen, numericFields: NUMERIC });
}

function run(seg: Segment, query: string): { ids: string[]; skipped: boolean; stats: EvalStats } {
  const plan = buildPlan(parseAst(query), { numericFields: NUMERIC });
  const stats: EvalStats = { matched: 0, phraseRejected: 0, blocksSkipped: 0, comparisons: 0 };
  const skipped = canSkipSegment(plan, seg);
  if (skipped) return { ids: [], skipped, stats };
  const res = evaluatePlan(plan, seg, stats, new EvalArena());
  const ids = Array.from(res.docs)
    .map((d) => seg.getDoc(d))
    .filter((d): d is Record<string, unknown> => d !== null)
    .map((d) => String(d.id));
  return { ids, skipped, stats };
}

describe("下推计划", () => {
  it("把等值/范围/短语编译成 posting 运算", () => {
    const plan = buildPlan(parseAst('level:error AND service:auth AND message:"connection timeout"'));
    const text = describePlan(plan);
    assert.match(text, /posting\(level=error\)/);
    assert.match(text, /posting\(service=auth\)/);
    assert.match(text, /connection timeout/);
    assert.match(text, /∩/);
  });

  it("下推条件可枚举出字段", () => {
    const plan = buildPlan(parseAst("level:error AND latency_ms:[10 TO 100]"));
    const conds = planConditions(plan);
    assert.equal(conds.length, 2);
    assert.deepEqual(conds.map((c) => c.type).sort(), ["eq", "range"]);
  });

  it("数值字段的非法界值编译为恒假，段级直接跳过", () => {
    const seg = makeSegment(0, "auth", 10);
    const { skipped } = run(seg, "latency_ms:[abc TO def]");
    assert.equal(skipped, true);
  });
});

describe("段级短路", () => {
  const authSeg = makeSegment(0, "auth", 12);
  const billingSeg = makeSegment(1, "billing", 12);

  it("段内无该 term 时整段跳过，一次 posting 比较都不做", () => {
    const { skipped } = run(billingSeg, "service:auth");
    assert.equal(skipped, true);

    const { skipped: own } = run(authSeg, "service:auth");
    assert.equal(own, false);
  });

  it("AND 中任一条件恒空 -> 整段跳过", () => {
    const plan = buildPlan(parseAst("service:billing AND level:error"), { numericFields: NUMERIC });
    // billing 段没有 level:error 的倒排链
    assert.equal(canSkipSegment(plan, billingSeg), true);
  });

  it("数值范围与段内值域无交集时跳过", () => {
    const plan = buildPlan(parseAst("latency_ms:[100000 TO 200000]"), { numericFields: NUMERIC });
    assert.equal(canSkipSegment(plan, authSeg), true);
  });

  it("OR 的所有分支都无倒排链时才跳过", () => {
    const plan = buildPlan(parseAst("service:zzz OR level:yyy"), { numericFields: NUMERIC });
    assert.equal(canSkipSegment(plan, authSeg), true);

    const orPlan = buildPlan(parseAst("service:zzz OR level:auth-error"), { numericFields: NUMERIC });
    assert.equal(canSkipSegment(orPlan, authSeg), false);
  });
});

describe("短路求值语义", () => {
  const seg = makeSegment(0, "auth", 12);

  it("等值、AND、OR、NOT 结果正确", () => {
    assert.equal(run(seg, "level:auth-error").ids.length, 4);
    assert.equal(run(seg, "service:auth").ids.length, 12);
    assert.equal(run(seg, "NOT level:auth-error").ids.length, 8);
    assert.equal(run(seg, "service:auth AND level:auth-error").ids.length, 4);
    assert.equal(run(seg, "service:auth OR service:billing").ids.length, 12);
    assert.equal(run(seg, "service:billing").ids.length, 0);
  });

  it("范围查询落在区间内", () => {
    const ids = run(seg, "latency_ms:[20 TO 50]").ids;
    // latency = i*10 -> i in {2,3,4,5}
    assert.equal(ids.length, 4);
    assert.deepEqual(ids, ["s0-2", "s0-3", "s0-4", "s0-5"]);
  });

  it("范围结果必须是 docId 升序（数值表按值排序，不能直接当 posting 用）", () => {
    // 回归测试：数值索引按"值"排序，命中区间里的 docId 是乱序的。
    // 如果不排就交给求交，会静默漏命中（表现为命中数偏小）。
    const shuffled: LogDocument[] = [
      { id: "r-0", level: "l", service: "s", message: "m", latency_ms: 500 },
      { id: "r-1", level: "l", service: "s", message: "m", latency_ms: 100 },
      { id: "r-2", level: "l", service: "s", message: "m", latency_ms: 300 },
      { id: "r-3", level: "l", service: "s", message: "m", latency_ms: 200 },
      { id: "r-4", level: "l", service: "s", message: "m", latency_ms: 400 },
    ];
    const s2 = Segment.build({ docs: shuffled, generation: 0, numericFields: NUMERIC });

    const plan = buildPlan(parseAst("latency_ms:[100 TO 300]"), { numericFields: NUMERIC });
    const stats: EvalStats = { matched: 0, phraseRejected: 0, blocksSkipped: 0, comparisons: 0 };
    const res = evaluatePlan(plan, s2, stats, new EvalArena());
    const docs = Array.from(res.docs);
    // 升序是集合运算的前提
    for (let i = 1; i < docs.length; i++) assert.ok(docs[i]! > docs[i - 1]!, "范围结果必须严格升序");
    assert.deepEqual(docs, [1, 2, 3]);

    // 与另一个 posting 求交也不能漏
    const both = buildPlan(parseAst("latency_ms:[100 TO 300] AND service:s"), { numericFields: NUMERIC });
    const st2: EvalStats = { matched: 0, phraseRejected: 0, blocksSkipped: 0, comparisons: 0 };
    const r2 = evaluatePlan(both, s2, st2, new EvalArena());
    assert.equal(r2.docs.length, 3, "范围与等值求交必须保留全部命中");
  });

  it("数值等值查询命中（数值编码 term）", () => {
    assert.deepEqual(run(seg, "latency_ms:30").ids, ["s0-3"]);
  });

  it("字段短语用最稀疏词收窄候选，再原文校验连续性", () => {
    const plan = buildPlan(parseAst('message:"connection timeout"'), { numericFields: NUMERIC });
    const stats: EvalStats = { matched: 0, phraseRejected: 0, blocksSkipped: 0, comparisons: 0 };
    const res = evaluatePlan(plan, seg, stats, new EvalArena());
    const ids = Array.from(res.docs)
      .map((d) => seg.getDoc(d))
      .filter((d): d is Record<string, unknown> => d !== null)
      .map((d) => String(d.id));
    // i % 3 === 0 的文档才是真命中
    assert.deepEqual(ids, ["s0-0", "s0-3", "s0-6", "s0-9"]);
    // 候选集取的是短语里最稀疏的词（timeout, df=4）而不是首词（connection, df=8），
    // 所以这里被淘汰的候选数是 0 —— 收窄已经发生在倒排层。
    assert.equal(stats.phraseRejected, 0);
  });

  it("候选集必须收窄到短语中最稀疏的词", () => {
    const phraseIdx = seg.index.fields.get("\u0000phrasemessage")!;
    const connDf = phraseIdx.get("connection")!.df;
    const timeoutDf = phraseIdx.get("timeout")!.df;
    assert.ok(timeoutDf < connDf, "本用例成立的前提：timeout 比 connection 稀疏");

    const plan = buildPlan(parseAst('message:"connection timeout"'), { numericFields: NUMERIC });
    const stats: EvalStats = { matched: 0, phraseRejected: 0, blocksSkipped: 0, comparisons: 0 };
    const res = evaluatePlan(plan, seg, stats, new EvalArena());
    // 命中数应等于最稀疏词的 df：不存在需要靠原文淘汰的多余候选
    assert.equal(res.docs.length, Math.min(connDf, timeoutDf));
  });

  it("短语验证缓存：重复查询结果一致且不再重扫", () => {
    const fresh = makeSegment(9, "auth", 12);
    const query = 'message:"connection timeout"';
    const first = run(fresh, query);
    assert.ok(first.ids.length > 0, "应有命中");

    // 段不可变 -> 同一短语的判定结果稳定，第二次直接命中缓存
    const second = run(fresh, query);
    assert.deepEqual(second.ids, first.ids);
    assert.equal(second.stats.phraseRejected, 0, "命中缓存后不应再有被淘汰的候选");
  });

  it("词序错误的短语不命中", () => {
    assert.equal(run(seg, 'message:"timeout connection"').ids.length, 0);
  });

  it("裸短语走全文链，语义与字段短语一致", () => {
    assert.deepEqual(run(seg, '"connection timeout"').ids, run(seg, 'message:"connection timeout"').ids);
  });

  it("字段存在性 field:* 命中所有含该字段的文档", () => {
    assert.equal(run(seg, "service:*").ids.length, 12);
  });

  it("结果与朴素过滤（读原文逐条比对）一致", () => {
    const naive = (pred: (d: Record<string, unknown>) => boolean): number => {
      let n = 0;
      for (let docId = 0; docId < seg.meta.docCount; docId++) {
        const d = seg.getDoc(docId);
        if (d !== null && pred(d)) n++;
      }
      return n;
    };
    const cases: Array<[string, (d: Record<string, unknown>) => boolean]> = [
      ["level:auth-error", (d) => d.level === "auth-error"],
      ["service:auth AND level:auth-error", (d) => d.service === "auth" && d.level === "auth-error"],
      ["NOT level:auth-error", (d) => d.level !== "auth-error"],
      ["latency_ms:[20 TO 50]", (d) => Number(d.latency_ms) >= 20 && Number(d.latency_ms) <= 50],
      ['message:"connection timeout"', (d) => String(d.message).includes("connection timeout")],
      ["level:auth-error OR service:auth", (d) => d.level === "auth-error" || d.service === "auth"],
    ];
    for (const [q, pred] of cases) {
      assert.equal(run(seg, q).ids.length, naive(pred), `查询 ${q} 与朴素过滤结果不一致`);
    }
  });
});
