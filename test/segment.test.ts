/**
 * 功能 2：不可变段、tiered 合并与 tombstone
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Segment, pickMergeTier, tokenizeText, type LogDocument } from "../src/segment.ts";

function mkDocs(prefix: string, n: number): LogDocument[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `${prefix}-${i}`,
    level: "error",
    service: "auth",
    message: `connection timeout ${i}`,
    latency_ms: i * 3,
  }));
}

describe("不可变段构建", () => {
  it("建立字段倒排、文本词级索引与数值索引", () => {
    const seg = Segment.build({
      docs: mkDocs("a", 5),
      generation: 0,
      numericFields: new Set(["latency_ms"]),
    });
    assert.equal(seg.meta.docCount, 5);
    assert.equal(seg.liveCount, 5);
    assert.equal(seg.index.fields.get("service")?.get("auth")?.df, 5);
    // 词级索引挂在 \u0000phrase 前缀的内部字段上（message 是文本字段）
    assert.ok(seg.index.textFields.has("message"));
    assert.equal(seg.index.fields.get("\u0000phrasemessage")?.get("timeout")?.df, 5);
    assert.equal(seg.index.numeric.get("latency_ms")?.values.length, 5);
  });

  it("textFields 限定词级索引范围（内存开关）", () => {
    const docs = [{ id: "x", message: "connection timeout", host: "10.0.1.7", service: "auth" }];
    const limited = Segment.build({ docs, generation: 0, textFields: new Set(["message"]) });
    assert.ok(limited.index.fields.get("\u0000phrasemessage")?.get("timeout"));
    assert.equal(limited.index.fields.has("\u0000phrasehost"), false, "非文本字段不该建词级索引");

    const wide = Segment.build({ docs, generation: 0, textFields: new Set(["message", "host"]) });
    assert.ok(wide.index.fields.get("\u0000phrasehost"), "显式声明后该字段才建词级索引");
  });

  it("默认只给 message 建词级索引（高基数字段不进倒排）", () => {
    const docs = Array.from({ length: 50 }, (_, i) => ({ id: `d${i}`, trace_id: `trace-${i}`, message: `msg ${i}`, service: "auth" }));
    const seg = Segment.build({ docs, generation: 0 });
    assert.deepEqual([...seg.index.textFields], ["message"]);
    // trace_id 高基数：不建词级索引，但等值倒排照常存在
    assert.equal(seg.index.fields.has("\u0000phrasetrace_id"), false);
    assert.equal(seg.index.fields.get("trace_id")?.get("trace-7")?.df, 1);
  });

  it("id 是定位键，不进倒排", () => {
    const seg = Segment.build({ docs: [{ id: "log-1", message: "x" }], generation: 0 });
    assert.equal(seg.index.fields.has("id"), false, "id 不应产生倒排链");
    // 但取原文时仍然带得出 id
    assert.equal(seg.getDoc(0)?.id, "log-1");
  });

  it("数组字段为每个元素各建一条 posting", () => {
    const seg = Segment.build({
      docs: [{ id: "x", tags: ["a", "b"], msg: "hello" }],
      generation: 0,
    });
    assert.equal(seg.index.fields.get("tags")?.get("a")?.df, 1);
    assert.equal(seg.index.fields.get("tags")?.get("b")?.df, 1);
  });

  it("字段存在性哨兵支持 field:*", () => {
    const seg = Segment.build({ docs: mkDocs("a", 2), generation: 0 });
    assert.equal(seg.index.fields.get("level")?.get("\u0000exists")?.df, 2);
  });

  it("文本切词支持下划线标识符", () => {
    assert.deepEqual(tokenizeText("connection_timeout failed"), ["connection_timeout", "connection", "timeout", "failed"]);
  });
});

describe("tombstone", () => {
  it("删除只打标记，文档从查询里消失但仍占物理空间", () => {
    const seg = Segment.build({
      docs: mkDocs("a", 4),
      generation: 0,
      deletedIds: new Set(["a-1", "a-2"]),
    });
    assert.equal(seg.meta.docCount, 4);
    assert.equal(seg.liveCount, 2);
    assert.equal(seg.tombstoneRatio, 0.5);
    assert.equal(seg.getDoc(1), null);
    assert.equal(seg.getDoc(0)?.id, "a-0");
  });

  it("合并时物理丢弃 tombstone 命中文档，回收空间", () => {
    const a = Segment.build({ docs: mkDocs("a", 3), generation: 0, deletedIds: new Set(["a-1"]) });
    const b = Segment.build({ docs: mkDocs("b", 3), generation: 1 });
    const merged = Segment.merge({
      segments: [a, b],
      extraDocs: [],
      generation: 2,
      numericFields: new Set(["latency_ms"]),
    });
    // 子段共 6 篇，a-1 被 tombstone 丢弃 -> 新段只承载 5 篇，且没有新的墓碑
    assert.equal(merged.meta.docCount, 5);
    assert.equal(merged.liveCount, 5);
    assert.equal(merged.tombstoneRatio, 0);
    assert.ok(!merged.ids.includes("a-1"));
    // sources 只记直接父段（溯源信息不递归展开，否则会无界增长）
    assert.deepEqual(merged.meta.sources.slice().sort((x, y) => x - y), [0, 1]);
  });
});

describe("tiered 合并策略", () => {
  const seg = (g: number, n: number, deleted = 0): Segment =>
    Segment.build({
      docs: mkDocs(`s${g}`, n),
      generation: g,
      deletedIds: new Set(Array.from({ length: deleted }, (_, i) => `s${g}-${i}`)),
    });

  it("同层段数达标时合并该层", () => {
    const segs = [seg(0, 10), seg(1, 10), seg(2, 10), seg(3, 10)];
    const d = pickMergeTier(segs, { minRunLength: 4 });
    assert.equal(d.merge.length, 4);
    assert.match(d.reason, /tiered/);
  });

  it("段数不足且无墓碑压力时交给级联规则", () => {
    const d = pickMergeTier([seg(0, 10), seg(1, 10)], { minRunLength: 4 });
    // 两个同量级段会走"级联合并"（防小段堆积），而不是 tiered 层合并
    assert.match(d.reason, /级联合并/);
    assert.equal(d.merge.length, 2);
  });

  it("tombstone 超 30% 触发强制合并", () => {
    const hot = seg(0, 10, 4); // 40% tombstone
    const d = pickMergeTier([hot, seg(1, 10)], { tombstoneThreshold: 0.3 });
    assert.equal(d.merge.length, 1);
    assert.equal(d.merge[0]!.generation, 0);
    assert.match(d.reason, /强制合并/);
  });

  it("阈值以下不触发强制合并（走级联而非回收）", () => {
    const warm = seg(0, 10, 2); // 20% < 30%
    const cold = seg(1, 10, 0);
    const d = pickMergeTier([warm, cold], { tombstoneThreshold: 0.3 });
    assert.ok(!/强制合并/.test(d.reason));
    // cold 段没有墓碑压力，不应单独被拉进合并
    assert.ok(!d.merge.some((s) => s.tombstoneRatio > 0.29));
  });

  it("决策是纯函数：同样输入同样输出", () => {
    const segs = [seg(3, 10), seg(1, 10), seg(2, 10), seg(0, 10)];
    const a = pickMergeTier(segs, { minRunLength: 4 });
    const b = pickMergeTier([...segs].reverse(), { minRunLength: 4 });
    assert.deepEqual(a.merge.map((s) => s.generation), b.merge.map((s) => s.generation));
  });
});
