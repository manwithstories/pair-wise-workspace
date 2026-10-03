import { test } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { SearchEngine } from "../src/engine.js";
import type { LogDoc } from "../src/segment.js";

function log(i: number, over: Partial<LogDoc> = {}): LogDoc {
  return {
    level: i % 3 === 0 ? "error" : "info",
    service: i % 2 === 0 ? "auth" : "billing",
    message: i % 4 === 0 ? "connection timeout upstream" : "request completed",
    trace_id: `trace-${i}`,
    ts: 1_700_000_000_000 + i * 1000,
    ...over,
  };
}

function seeded(n: number, flushEvery = 1000) {
  const e = new SearchEngine({ flushEvery, mergeFactor: 4 });
  for (let i = 0; i < n; i++) e.add(log(i), `doc-${i}`);
  return e;
}

test("searches the active (unflushed) segment", async () => {
  const e = seeded(40);
  const out = e.search("level:error");
  assert.ok(out.ok);
  assert.ok(out.result.total > 0);
  await e.close();
});

test("finds error+auth+timeout across multiple segments", async () => {
  const e = new SearchEngine({ flushEvery: 10, mergeFactor: 4 });
  for (let i = 0; i < 60; i++) {
    e.add(
      log(i, i % 2 === 0 ? { level: "error", service: "auth" } : {}),
      `doc-${i}`,
    );
  }
  await e.flush();
  await e.whenMerged();

  const out = e.search('level:error AND service:auth AND message:"connection timeout"');
  assert.ok(out.ok);
  assert.ok(out.result.total > 0, "should match the seeded error/auth docs");
  for (const hit of out.result.hits) {
    assert.equal(hit.doc.level, "error");
    assert.equal(hit.doc.service, "auth");
    assert.match(hit.doc.message, /connection timeout/);
  }
  await e.close();
});

test("pushdown short-circuits segments with no matching term", async () => {
  const e = new SearchEngine({ flushEvery: 10, mergeFactor: 10 });
  // segment 1: only info; segment 2: only error
  for (let i = 0; i < 10; i++) e.add(log(i, { level: "info" }), `a-${i}`);
  await e.flush();
  for (let i = 0; i < 10; i++) e.add(log(i, { level: "error" }), `b-${i}`);
  await e.flush();

  const out = e.search("level:error");
  assert.ok(out.ok);
  assert.ok(out.result.stats.segmentsSkipped >= 1, "the info-only segment must be skipped");
  assert.equal(out.result.segments.filter((s) => s.skipped).length >= 1, true);
  for (const skipped of out.result.segments.filter((s) => s.skipped)) {
    assert.equal(skipped.scanned, 0, "a skipped segment must not be scanned");
  }
  await e.close();
});

test("phrase queries require adjacency, not just co-occurrence", async () => {
  const e = new SearchEngine({ flushEvery: 100 });
  e.add(log(0, { message: "connection timeout" }), "adjacent");
  e.add(log(1, { message: "timeout after connection retries" }), "reversed");
  e.add(log(2, { message: "connection lost timeout" }), "gap");
  await e.flush();

  const out = e.search('message:"connection timeout"');
  assert.ok(out.ok);
  assert.deepEqual(out.result.hits.map((h) => h.id), ["adjacent"]);
  await e.close();
});

test("range pushdown filters on a numeric column", async () => {
  const e = new SearchEngine({ flushEvery: 100 });
  e.add(log(0, { ts: 100 }), "low");
  e.add(log(1, { ts: 500 }), "mid");
  e.add(log(2, { ts: 900 }), "high");
  await e.flush();

  const out = e.search("ts:[200 TO 800]");
  assert.ok(out.ok);
  assert.deepEqual(out.result.hits.map((h) => h.id), ["mid"]);
  await e.close();
});

test("deletes hide documents from results", async () => {
  const e = new SearchEngine({ flushEvery: 100 });
  e.add(log(0, { level: "error" }), "keep");
  e.add(log(1, { level: "error" }), "drop");
  await e.flush();

  assert.equal(e.delete("drop"), true, "active segment deletion is recorded");
  assert.equal(e.delete("nope"), false, "deleting an unknown id reports false");
  const out = e.search("level:error");
  assert.ok(out.ok);
  assert.deepEqual(out.result.hits.map((h) => h.id), ["keep"]);

  // and the deletion survives a flush (tombstone carried into the segment)
  await e.flush();
  const after = e.search("level:error");
  assert.ok(after.ok);
  assert.deepEqual(after.result.hits.map((h) => h.id), ["keep"]);
  await e.close();
});

test("tiered merge collapses small segments into a higher level", async () => {
  const e = new SearchEngine({ flushEvery: 10, mergeFactor: 4, tombstoneThreshold: 0.3 });
  for (let i = 0; i < 40; i++) e.add(log(i), `doc-${i}`);
  for (let i = 0; i < 4; i++) await e.flush();
  await e.whenMerged();

  const stats = e.segmentStats();
  assert.ok(stats.length < 4, `expected merging to reduce segment count, got ${stats.length}`);
  assert.ok(stats.some((s) => s.level > 0), "a higher tier should exist");
  await e.close();
});

test("queries still return correct results while a merge runs", async () => {
  const e = new SearchEngine({ flushEvery: 10, mergeFactor: 4 });
  for (let i = 0; i < 40; i++) e.add(log(i), `doc-${i}`);
  for (let i = 0; i < 4; i++) await e.flush();

  // query concurrently with background merging
  const expected = e.search("level:error").ok ? (e.search("level:error") as any).result.total : -1;
  const results = await Promise.all(
    Array.from({ length: 10 }, () => Promise.resolve(e.search("level:error"))),
  );
  for (const r of results) {
    assert.ok(r.ok);
    assert.equal(r.result.total, expected, "concurrent queries must agree during merge");
  }
  await e.whenMerged();
  await e.close();
});

test("state survives a restart via segment files", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "engine-"));
  try {
    const e1 = await SearchEngine.open({ dataDir: dir, flushEvery: 100 });
    for (let i = 0; i < 20; i++) e1.add(log(i, { service: "auth" }), `doc-${i}`);
    await e1.flush();
    await e1.close();

    const e2 = await SearchEngine.open({ dataDir: dir });
    const out = e2.search("service:auth");
    assert.ok(out.ok);
    assert.equal(out.result.total, 20, "all documents reloaded from disk");
    await e2.close();
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test("rejects malformed queries with a position, not an exception", async () => {
  const e = seeded(5);
  const out = e.search("level:error AND");
  assert.equal(out.ok, false);
  if (!out.ok) {
    assert.ok(out.error.offset >= 0);
    assert.ok(out.error.message.length > 0);
    assert.ok(out.error.excerpt.includes("^"));
  }
  await e.close();
});

test("auto-flush rotates the builder instead of re-flushing it", async () => {
  const e = new SearchEngine({ flushEvery: 10, mergeFactor: 4 });
  for (let i = 0; i < 50; i++) e.add(log(i), `doc-${i}`);
  await e.flush();
  await e.whenMerged();

  const stats = e.segmentStats();
  const total = stats.reduce((a, s) => a + s.docs, 0);
  assert.equal(total, 50, "every document lands in exactly one segment");
  assert.equal(e.pendingCount, 0);
  await e.close();
});

test("concurrent flushes never double-count a segment", async () => {
  const e = new SearchEngine({ flushEvery: 1000, mergeFactor: 100 });
  for (let i = 0; i < 20; i++) e.add(log(i), `doc-${i}`);
  await Promise.all([e.flush(), e.flush(), e.flush()]);
  const total = e.segmentStats().reduce((a, s) => a + s.docs, 0);
  assert.equal(total, 20, "documents are counted exactly once");
  await e.close();
});

test("tiered merge reaches a higher level", async () => {
  const e = new SearchEngine({ flushEvery: 10, mergeFactor: 4 });
  for (let i = 0; i < 40; i++) e.add(log(i), `doc-${i}`);
  for (let i = 0; i < 4; i++) await e.flush();
  await e.whenMerged();

  const stats = e.segmentStats();
  assert.ok(stats.some((s) => s.level > 0), `expected a higher tier, got ${JSON.stringify(stats)}`);
  assert.equal(stats.reduce((a, s) => a + s.docs, 0), 40);
  await e.close();
});

test("merge scheduler terminates (no runaway re-arming)", async () => {
  // A tier that can never satisfy the merge criteria must not spin forever.
  const e = new SearchEngine({ flushEvery: 5, mergeFactor: 100, tombstoneThreshold: 0.99 });
  for (let i = 0; i < 30; i++) e.add(log(i), `doc-${i}`);
  await e.flush();
  const done = await Promise.race([
    e.whenMerged().then(() => "settled"),
    new Promise((r) => setTimeout(() => r("timeout"), 2000)),
  ]);
  assert.equal(done, "settled", "whenMerged() must settle");
  await e.close();
});

test("tombstone overflow forces a compaction merge", async () => {
  const e = new SearchEngine({ flushEvery: 1000, mergeFactor: 100, tombstoneThreshold: 0.3 });
  // two segments so the compaction has a peer to merge with
  for (let i = 0; i < 10; i++) e.add(log(i), `a-${i}`);
  await e.flush();
  for (let i = 0; i < 10; i++) e.add(log(i), `b-${i}`);
  await e.flush();

  // delete 60% of the first segment
  for (let i = 0; i < 6; i++) e.delete(`a-${i}`);
  await e.whenMerged();

  const out = e.search("service:auth");
  assert.ok(out.ok);
  const ids = new Set(out.result.hits.map((h) => h.id));
  for (let i = 0; i < 6; i++) assert.equal(ids.has(`a-${i}`), false, `a-${i} must be deleted`);
  await e.close();
});

test("negation stays fast on a large segment", async () => {
  const e = new SearchEngine({ flushEvery: 100_000 });
  for (let i = 0; i < 200_000; i++) {
    e.add(log(i, { level: i % 2 === 0 ? "error" : "info" }), `doc-${i}`);
  }
  await e.flush();
  const t = performance.now();
  const out = e.search("NOT level:error", 10);
  const elapsed = performance.now() - t;
  assert.ok(out.ok);
  assert.equal(out.result.total, 10);
  assert.ok(elapsed < 1000, `negation took ${elapsed.toFixed(0)}ms`);
  await e.close();
});

test("10 concurrent query streams do not block each other", async () => {
  const e = new SearchEngine({ flushEvery: 20_000, mergeFactor: 4 });
  for (let i = 0; i < 60_000; i++) e.add(log(i), `doc-${i}`);
  await e.flush();

  const streams = Array.from({ length: 10 }, async () => {
    const times: number[] = [];
    for (let k = 0; k < 40; k++) {
      const t = performance.now();
      const r = e.search("level:error AND service:auth", 10);
      times.push(performance.now() - t);
      assert.ok(r.ok);
      assert.equal(r.result.total, 10);
    }
    return times;
  });
  const all = await Promise.all(streams);
  const flat = all.flat().sort((a, b) => a - b);
  const p99 = flat[Math.floor(flat.length * 0.99)] as number;
  assert.ok(p99 < 50, `concurrent p99 was ${p99.toFixed(2)}ms`);
  await e.close();
});

test("countLeaf estimates matching docs without materializing hits", async () => {
  const e = new SearchEngine({ flushEvery: 100 });
  for (let i = 0; i < 10; i++) e.add(log(i, { level: i % 2 === 0 ? "error" : "info" }), `doc-${i}`);
  await e.flush();

  assert.equal(e.countLeaf({ kind: "term", field: "level", term: "error" }), 5);
  assert.equal(e.countLeaf({ kind: "term", field: "level", term: "nope" }), 0);
  assert.equal(
    e.countLeaf({ kind: "range", field: "ts", lo: 1_700_000_000_000, hi: 1_700_000_003_000, loInclusive: true, hiInclusive: true }),
    4,
  );
  // phrase estimate is bounded by the rarest term
  const phrase = e.countLeaf({ kind: "phrase", field: "message", terms: ["connection", "timeout"] });
  assert.ok(phrase > 0 && phrase <= 5);
  await e.close();
});

test("explainPlan renders a readable pushdown plan", async () => {
  const e = seeded(10);
  const r = e.search('level:error AND service:auth AND message:"connection timeout"');
  assert.ok(r.ok);
  const text = r.result.plan;
  assert.ok(text.pushdownCount >= 1);
  await e.close();
});
