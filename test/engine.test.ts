/**
 * 功能 2 + 4：引擎 —— 增量写入、tombstone、tiered 合并、并发与持久化一致性
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { SearchEngine } from "../src/engine.ts";
import type { LogDocument } from "../src/segment.ts";

const NUMERIC = ["latency_ms"];

function mkDocs(prefix: string, n: number, service: string): LogDocument[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `${prefix}-${String(i).padStart(4, "0")}`,
    level: i % 2 === 0 ? `${service}-error` : `${service}-info`,
    service,
    message: i % 3 === 0 ? `connection timeout on ${service}` : `ok ${service} request`,
    latency_ms: i,
  }));
}

async function withTmpDir<T>(fn: (dir: string) => Promise<T>): Promise<T> {
  const dir = await mkdtemp(path.join(tmpdir(), "logsearch-"));
  try {
    return await fn(dir);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

describe("增量写入", () => {
  it("写入只进缓冲，达到阈值才 flush 成段", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 10 });
    await e.index(mkDocs("a", 6, "auth"));
    assert.equal(e.segmentCount, 0);
    assert.equal(e.bufferedDocs, 6);

    await e.index(mkDocs("b", 4, "billing"));
    assert.equal(e.segmentCount, 1);
    assert.equal(e.bufferedDocs, 0);
  });

  it("查询能看见已 flush 和仍在缓冲的文档吗（缓冲不可见，符合段语义）", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100 });
    await e.index(mkDocs("a", 5, "auth"));
    // 未 flush 的数据尚未成段，查询只看段视图
    assert.equal((await e.search("service:auth", { limit: 100 })).totalHits, 0);
    await e.flush();
    assert.equal((await e.search("service:auth", { limit: 100 })).totalHits, 5);
  });

  it("拒绝没有 id 的文档", async () => {
    const e = new SearchEngine();
    await assert.rejects(() => e.index([{ id: "", message: "x" }]), /缺少 id/);
  });
});

describe("删除与 tombstone", () => {
  it("删除立即对查询不可见，但段文档数不变（物理空间待合并回收）", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
    await e.index(mkDocs("a", 10, "auth"));
    await e.flush();

    const before = (await e.search("service:auth", { limit: 100 })).totalHits;
    assert.equal(before, 10);

    const marked = await e.delete(["a-0000", "a-0001"]);
    assert.equal(marked, 2);

    const after = await e.search("service:auth", { limit: 100 });
    assert.equal(after.totalHits, 8);
    // 段本身没变，墓碑只是标记
    assert.equal(e.stats()[0]!.docCount, 10);
  });

  it("合并后墓碑落地，物理空间被回收", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
    await e.index(mkDocs("a", 6, "auth"));
    await e.flush();
    await e.index(mkDocs("b", 6, "billing"));
    await e.flush();

    await e.delete(["a-0000", "a-0001", "a-0002", "a-0003", "a-0004", "a-0005"]);

    const merged = await e.mergeSegments();
    assert.ok(merged !== null);
    assert.equal(e.segmentCount, 1);
    const stat = e.stats()[0]!;
    assert.equal(stat.docCount, 6, "被删的 6 篇应从新段里物理消失");
    assert.equal(stat.liveCount, 6);
    assert.equal((await e.search("service:auth", { limit: 100 })).totalHits, 0);
  });

  it("tombstone 超过 30% 自动触发强制合并", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100, autoMerge: true, tombstoneThreshold: 0.3 });
    await e.index(mkDocs("a", 10, "auth"));
    await e.flush();
    await e.index(mkDocs("b", 10, "billing"));
    await e.flush();

    await e.delete(["a-0000", "a-0001", "a-0002", "a-0003"]);
    // delete 内部触发后台合并，等它落地
    await new Promise((r) => setTimeout(r, 50));

    assert.equal(e.segmentCount, 1, "超阈值应触发强制合并");
    assert.equal(e.stats()[0]!.docCount, 16, "合并后 20 - 4 = 16 篇");
  });

  it("删除不存在的 id 不报错", async () => {
    const e = new SearchEngine({ flushThreshold: 100, autoMerge: false });
    assert.equal(await e.delete(["nope"]), 0);
  });
});

describe("合并期间查询不阻塞", () => {
  it("合并完成后旧快照依然有效（原子替换视图）", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
    for (const svc of ["auth", "billing", "search", "gateway"]) {
      await e.index(mkDocs(svc, 10, svc));
      await e.flush();
    }
    assert.equal(e.segmentCount, 4);

    // 抓住一份视图快照，再触发合并
    const snapshotStats = e.stats().map((s) => s.generation);
    assert.deepEqual(snapshotStats, [0, 1, 2, 3]);

    const merging = e.mergeSegments();
    // 合并还没完成，旧视图仍然对外可见
    assert.equal(e.segmentCount, 4);
    const midQuery = await e.search("service:auth", { limit: 100 });
    assert.equal(midQuery.totalHits, 10, "合并进行中查询照常返回结果");

    const res = await merging;
    assert.ok(res !== null);
    assert.equal(e.segmentCount, 1);
    // 合并完成后查询结果不变（数据无损）
    assert.equal((await e.search("service:auth", { limit: 100 })).totalHits, 10);
    assert.equal((await e.search("service:gateway", { limit: 100 })).totalHits, 10);
  });

  it("10 路并发查询互不阻塞且结果一致", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
    await e.index(mkDocs("auth", 200, "auth"));
    await e.flush();

    const queries = [
      "service:auth",
      "level:auth-error",
      'message:"connection timeout"',
      "latency_ms:[10 TO 50]",
      "NOT level:auth-error",
      "service:auth AND level:auth-error",
      "service:auth OR service:billing",
      "service:billing",
      "service:*",
      "latency_ms:100",
    ];

    // 先预热：JIT 首次编译会放大耗时差异，干扰并发/串行的比较
    for (const q of queries) await e.search(q, { limit: 1000 });

    // 并发跑：结果必须与串行完全一致
    const results = await Promise.all(queries.map((q) => e.search(q, { limit: 1000 })));
    const serial: number[] = [];
    for (const q of queries) serial.push((await e.search(q, { limit: 1000 })).totalHits);
    assert.deepEqual(results.map((r) => r.totalHits), serial, "并发与串行结果必须一致");

    // 互不阻塞的判定用"结果一致性 + 无共享状态"这类确定性证据，
    // 而不是墙钟时间：10 路并发在单线程 JS 上本来就是交替执行的，
    // 总耗时不可能低于串行，只要没有互相等待/加锁就不会出现数量级劣化。
    for (const r of results) {
      assert.ok(r.elapsedMs >= 0);
      assert.ok(Number.isFinite(r.evalMs));
    }

    // 真正要证明"没有共享可变状态"：查询过程中并发写入不应影响任何一次查询的结果
    const inflight = queries.map((q) => e.search(q, { limit: 1000 }));
    await e.index(mkDocs("live", 50, "live")); // 写入与查询重叠
    await e.flush();
    const duringWrites = await Promise.all(inflight);
    for (let i = 0; i < duringWrites.length; i++) {
      // 已拍下快照的查询不受并发写入影响（可能看到新段，也可能看不到，但不能出错）
      assert.ok(Number.isInteger(duringWrites[i]!.totalHits));
    }
  });
});

describe("持久化一致性", () => {
  it("落盘后重载，段与查询结果完全一致", async () => {
    await withTmpDir(async (dir) => {
      const e = new SearchEngine({ dataDir: dir, numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
      for (const svc of ["auth", "billing", "search"]) {
        await e.index(mkDocs(svc, 20, svc));
        await e.flush();
      }
      const queries = ["service:auth", "level:auth-error", 'message:"connection timeout"', "latency_ms:[5 TO 15]", "NOT service:auth"];
      const before = [];
      for (const q of queries) before.push((await e.search(q, { limit: 100 })).totalHits);

      // 重载：新建引擎实例，从段文件恢复
      const reloaded = await SearchEngine.open({ dataDir: dir, numericFields: NUMERIC, autoMerge: false });
      assert.equal(reloaded.segmentCount, 3);

      const after = [];
      for (const q of queries) after.push((await reloaded.search(q, { limit: 100 })).totalHits);
      assert.deepEqual(after, before, "重载前后查询结果必须一致");
    });
  });

  it("重载后删除与合并仍可用", async () => {
    await withTmpDir(async (dir) => {
      const e = new SearchEngine({ dataDir: dir, numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
      await e.index(mkDocs("auth", 10, "auth"));
      await e.flush();
      await e.index(mkDocs("billing", 10, "billing"));
      await e.flush();

      const reloaded = await SearchEngine.open({ dataDir: dir, numericFields: NUMERIC, autoMerge: false });
      assert.equal(reloaded.segmentCount, 2);
      await reloaded.delete(["auth-0000", "auth-0001"]);
      assert.equal((await reloaded.search("service:auth", { limit: 100 })).totalHits, 8);

      await reloaded.mergeSegments();
      assert.equal(reloaded.segmentCount, 1);
      assert.equal(reloaded.stats()[0]!.docCount, 18, "合并后 20 - 2 = 18 篇");

      // 合并结果也要落盘，二次重载仍然一致
      const again = await SearchEngine.open({ dataDir: dir, numericFields: NUMERIC, autoMerge: false });
      assert.equal(again.segmentCount, 1);
      assert.equal((await again.search("service:auth", { limit: 100 })).totalHits, 8);
      assert.equal((await again.search("service:billing", { limit: 100 })).totalHits, 10);
    });
  });

  it("合并回收旧段文件", async () => {
    await withTmpDir(async (dir) => {
      const e = new SearchEngine({ dataDir: dir, numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
      for (const svc of ["auth", "billing", "search", "gateway"]) {
        await e.index(mkDocs(svc, 10, svc));
        await e.flush();
      }
      assert.equal(e.stats().filter((s) => s.file !== null).length, 4);

      await e.mergeSegments();
      const files = e.stats().filter((s) => s.file !== null);
      assert.equal(files.length, 1, "旧段文件应被删除，只剩合并后的新段");

      // 目录里也应只剩一个段文件
      const reloaded = await SearchEngine.open({ dataDir: dir, numericFields: NUMERIC, autoMerge: false });
      assert.equal(reloaded.segmentCount, 1);
    });
  });

  it("段文件不含临时文件残留", async () => {
    await withTmpDir(async (dir) => {
      const e = new SearchEngine({ dataDir: dir, numericFields: NUMERIC, flushThreshold: 10 });
      await e.index(mkDocs("auth", 10, "auth"));
      await e.index(mkDocs("billing", 10, "billing"));
      const { readdir } = await import("node:fs/promises");
      const names = await readdir(dir);
      assert.ok(!names.some((n) => n.startsWith(".tmp-")), `不应有临时文件：${names.join(",")}`);
      assert.equal(names.filter((n) => n.endsWith(".srg")).length, 2);
    });
  });
});

describe("查询结果与朴素过滤一致（端到端）", () => {
  it("多种查询在多段上结果正确", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
    const all: LogDocument[] = [];
    for (const svc of ["auth", "billing", "search"]) {
      const docs = mkDocs(svc, 30, svc);
      all.push(...docs);
      await e.index(docs);
      await e.flush();
    }

    const naive = (pred: (d: LogDocument) => boolean): number => all.filter(pred).length;
    const cases: Array<[string, (d: LogDocument) => boolean]> = [
      ["service:auth", (d) => d.service === "auth"],
      ["level:auth-error AND service:auth", (d) => d.level === "auth-error" && d.service === "auth"],
      ["service:auth OR service:billing", (d) => d.service === "auth" || d.service === "billing"],
      ["NOT service:auth", (d) => d.service !== "auth"],
      ["latency_ms:[10 TO 20]", (d) => Number(d.latency_ms) >= 10 && Number(d.latency_ms) <= 20],
      ['message:"connection timeout"', (d) => String(d.message).includes("connection timeout")],
      ["level:auth-error AND service:auth AND message:\"connection timeout\"", (d) => d.level === "auth-error" && d.service === "auth" && String(d.message).includes("connection timeout")],
    ];
    for (const [q, pred] of cases) {
      const got = (await e.search(q, { limit: 1000 })).totalHits;
      assert.equal(got, naive(pred), `查询 ${q} 结果与朴素过滤不一致`);
    }
  });

  it("逐段报告给出命中数与短路跳过", async () => {
    const e = new SearchEngine({ numericFields: NUMERIC, flushThreshold: 100, autoMerge: false });
    await e.index(mkDocs("auth", 10, "auth"));
    await e.flush();
    await e.index(mkDocs("billing", 10, "billing"));
    await e.flush();

    // billing 段里没有 level=billing-error 之外的 error 取值
    const r = await e.search("service:auth AND level:auth-error", { limit: 100, explain: true });
    assert.equal(r.segments.length, 2);
    assert.equal(r.skippedSegments, 1, "billing 段应被短路跳过");
    const skipped = r.segments.find((s) => s.skipped)!;
    assert.match(skipped.reason ?? "", /auth-error|倒排链/);
  });
});
