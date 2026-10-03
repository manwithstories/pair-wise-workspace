import { test } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  SegmentBuilder,
  mergeSegments,
  serializeSegment,
  deserializeSegment,
  writeSegment,
  readSegment,
  listSegmentFiles,
  removeSegmentFile,
  newSegmentId,
  type LogDoc,
} from "../src/segment.js";

function doc(i: number, over: Partial<LogDoc> = {}): LogDoc {
  return {
    level: i % 2 === 0 ? "error" : "info",
    service: i % 3 === 0 ? "auth" : "billing",
    message: i % 2 === 0 ? "connection timeout" : "request ok",
    trace_id: `t-${i}`,
    ts: 1_700_000_000_000 + i,
    ...over,
  };
}

test("builder indexes terms, positions, and numeric fields", () => {
  const b = new SegmentBuilder("s1", 0);
  b.add(doc(0), "d0");
  b.add(doc(1), "d1");
  const seg = b.seal();

  assert.equal(seg.docCount, 2);
  assert.deepEqual([...seg.index.lookup("level", "error")!.docIds], [0]);
  assert.deepEqual([...seg.index.lookup("level", "info")!.docIds], [1]);
  // numeric column supports a range
  assert.deepEqual([...seg.index.range("ts", 1_700_000_000_001, 1_700_000_000_001, true, true)], [1]);
  // positions are recorded for the message field
  const timeout = seg.index.lookup("message", "timeout")!;
  assert.deepEqual([...(timeout.positionsAt(0) ?? [])], [1]);
});

test("sealed segments reject further writes", () => {
  const b = new SegmentBuilder("s1", 0);
  b.add(doc(0), "d0");
  b.seal();
  assert.throws(() => b.add(doc(1), "d1"), /sealed/);
});

test("tombstones hide documents without rewriting the segment", () => {
  const b = new SegmentBuilder("s1", 0);
  for (let i = 0; i < 10; i++) b.add(doc(i), `d${i}`);
  for (let i = 0; i < 4; i++) b.delete(`d${i}`);
  const seg = b.seal();

  assert.equal(seg.docCount, 10, "doc count still reflects what was written");
  assert.equal(seg.tombstones.size, 4);
  assert.equal(seg.liveDocCount, 6);
  assert.equal(seg.isDeleted("d0"), true);
  assert.equal(seg.isDeleted("d9"), false);
  assert.ok(seg.needsCompaction(0.3), "40% deletions should trigger compaction");
  assert.ok(!seg.needsCompaction(0.5));
});

test("binary round-trip preserves postings, positions, and numeric columns", () => {
  const b = new SegmentBuilder("s1", 2);
  for (let i = 0; i < 20; i++) b.add(doc(i), `d${i}`);
  b.delete("d3");
  const seg = b.seal();

  const restored = deserializeSegment(serializeSegment(seg), "s1");

  assert.equal(restored.level, 2);
  assert.equal(restored.docCount, 20);
  assert.equal(restored.tombstones.has("d3"), true);
  assert.deepEqual(
    [...restored.index.lookup("level", "error")!.docIds],
    [...seg.index.lookup("level", "error")!.docIds],
  );
  assert.deepEqual([...restored.index.range("ts", null, null, true, true)], [
    ...seg.index.range("ts", null, null, true, true),
  ]);
  // skip tables are rebuilt on load
  const list = restored.index.lookup("message", "connection")!;
  assert.equal(list.seekGE(1), 1);

  // positions survive the round-trip, so phrase queries still work
  const timeout = restored.index.lookup("message", "timeout")!;
  assert.deepEqual([...(timeout.positionsAt(0) ?? [])], [1]);
});

test("binary round-trip is byte-identical", () => {
  const b = new SegmentBuilder("s1", 0);
  for (let i = 0; i < 6; i++) b.add(doc(i), `d${i}`);
  b.delete("d1");
  const seg = b.seal();
  const once = serializeSegment(seg);
  const restored = deserializeSegment(once, "s1");
  assert.ok(once.equals(serializeSegment(restored)), "re-serialization must match");
});

test("persisted segments survive a restart", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "segtest-"));
  try {
    const b = new SegmentBuilder("persisted", 0);
    for (let i = 0; i < 5; i++) b.add(doc(i), `d${i}`);
    const seg = b.seal();
    await writeSegment(dir, seg);

    const files = await listSegmentFiles(dir);
    assert.deepEqual(files, ["persisted.seg"]);

    const loaded = await readSegment(dir, "persisted");
    assert.equal(loaded.docCount, 5);
    assert.deepEqual([...loaded.index.lookup("service", "auth")!.docIds], [0, 3]);

    await removeSegmentFile(dir, "persisted");
    assert.deepEqual(await listSegmentFiles(dir), []);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test("segment files are written atomically via a temp file", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "segtest-"));
  try {
    const b = new SegmentBuilder("atomic", 0);
    b.add(doc(0), "d0");
    await writeSegment(dir, b.seal());
    const entries = await fs.readdir(dir);
    assert.deepEqual(entries, ["atomic.seg"], "no .tmp left behind");
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test("merge drops deleted documents and reclaims space", () => {
  const a = new SegmentBuilder("a", 0);
  for (let i = 0; i < 6; i++) a.add(doc(i), `d${i}`);
  a.delete("d1");
  a.delete("d2");
  const segA = a.seal();

  const b = new SegmentBuilder("b", 0);
  for (let i = 6; i < 10; i++) b.add(doc(i), `d${i}`);
  const segB = b.seal();

  const merged = mergeSegments([segA, segB]);

  assert.equal(merged.level, 1, "merge level is one above its inputs");
  assert.equal(merged.docCount, 8, "8 live docs remain (2 were deleted)");
  assert.equal(merged.tombstones.size, 0, "resolved deletions are reclaimed");

  // deleted docs must not appear in postings
  const ids = new Set(merged.docs.map((x) => x.id));
  assert.equal(ids.has("d1"), false);
  assert.equal(ids.has("d2"), false);
  assert.equal(ids.has("d0"), true);
  assert.equal(ids.has("d9"), true);

  // local docIds stay dense after the merge
  const levelErr = merged.index.lookup("level", "error")!;
  for (const localId of levelErr.docIds) {
    assert.ok(localId >= 0 && localId < merged.docCount);
  }
});

test("merge carries forward tombstones aimed at other segments", () => {
  const a = new SegmentBuilder("a", 0);
  a.add(doc(0), "d0");
  a.delete("other:5"); // belongs to a segment not part of this merge
  const segA = a.seal();

  const b = new SegmentBuilder("b", 0);
  b.add(doc(1), "d1");
  const segB = b.seal();

  const merged = mergeSegments([segA, segB]);
  assert.equal(merged.docCount, 2);
  assert.ok(merged.tombstones.has("other:5"), "unresolved tombstone is preserved");
});

test("newSegmentId produces unique ids", () => {
  const ids = new Set(Array.from({ length: 200 }, () => newSegmentId()));
  assert.equal(ids.size, 200);
});
