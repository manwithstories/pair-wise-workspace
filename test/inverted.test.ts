import { test } from "node:test";
import assert from "node:assert/strict";
import {
  DocIdBuf,
  InvertedIndex,
  NumericColumn,
  PostingList,
  complementSorted,
  exceptSorted,
  hasPhrase,
  intersectSorted,
  unionSorted,
  type SetOpStats,
} from "../src/inverted.js";

const stats = (): SetOpStats => ({ hops: 0, comparisons: 0, shortCircuited: false });

test("DocIdBuf grows, sorts, and snapshots", () => {
  const b = new DocIdBuf(2);
  for (const v of [9, 3, 7, 1, 3, 0]) b.push(v);
  assert.deepEqual([...b.toArray()], [9, 3, 7, 1, 3, 0], "insertion order preserved");
  assert.equal(b.length, 6);
  assert.deepEqual([...b.sort().toArray()], [0, 1, 3, 3, 7, 9]);
  b.reset();
  assert.equal(b.length, 0);
});

test("intersectSorted matches a reference implementation", () => {
  const a = Uint32Array.from([1, 3, 5, 7, 9, 11, 40, 41, 42]);
  const b = Uint32Array.from([2, 5, 6, 9, 11, 42, 100]);
  const out = new DocIdBuf();
  const s = stats();
  assert.equal(intersectSorted(a, b, out, s), true);
  assert.deepEqual([...out.toArray()], [5, 9, 11, 42]);
  assert.equal(s.shortCircuited, false);
});

test("intersectSorted short-circuits on an empty input", () => {
  const out = new DocIdBuf();
  const s = stats();
  assert.equal(intersectSorted(new Uint32Array(0), Uint32Array.from([1, 2]), out, s), false);
  assert.equal(out.length, 0);
  assert.equal(s.shortCircuited, true);
});

test("intersectSorted gallops over dense disjoint ranges", () => {
  const a = Uint32Array.from(Array.from({ length: 5000 }, (_, i) => i));
  const b = Uint32Array.from(Array.from({ length: 5000 }, (_, i) => i + 100000));
  const out = new DocIdBuf();
  const s = stats();
  assert.equal(intersectSorted(a, b, out, s), false);
  // Without galloping this would cost ~5000 comparisons; with it, ~O(log n).
  assert.ok(s.comparisons < 100, `comparisons=${s.comparisons}`);
  assert.ok(s.hops > 0);
});

test("unionSorted is sorted and de-duplicated", () => {
  const out = new DocIdBuf();
  unionSorted(Uint32Array.from([1, 4, 9]), Uint32Array.from([2, 4, 9, 11]), out);
  assert.deepEqual([...out.toArray()], [1, 2, 4, 9, 11]);
});

test("unionSorted handles empty sides", () => {
  const out = new DocIdBuf();
  unionSorted(new Uint32Array(0), new Uint32Array(0), out);
  assert.equal(out.length, 0);
  unionSorted(Uint32Array.from([3, 5]), new Uint32Array(0), out);
  assert.deepEqual([...out.toArray()], [3, 5]);
  unionSorted(new Uint32Array(0), Uint32Array.from([3, 5]), out);
  assert.deepEqual([...out.toArray()], [3, 5]);
});

test("exceptSorted and complementSorted", () => {
  const out = new DocIdBuf();
  exceptSorted(Uint32Array.from([1, 2, 3, 9]), Uint32Array.from([2, 9]), out);
  assert.deepEqual([...out.toArray()], [1, 3]);
  complementSorted(Uint32Array.from([0, 2, 4]), 5, out);
  assert.deepEqual([...out.toArray()], [1, 3]);
});

test("PostingList.seekGE agrees with linear search", () => {
  const docIds = Uint32Array.from(Array.from({ length: 1000 }, (_, i) => i * 3));
  const list = new PostingList({
    docIds,
    freqs: new Uint32Array(1000).fill(1),
    posOffsets: null,
    posData: null,
  });
  for (let target = 0; target < 3000; target += 7) {
    let expected = 0;
    while (expected < docIds.length && (docIds[expected] as number) < target) expected++;
    assert.equal(list.seekGE(target), expected, `target=${target}`);
  }
  assert.equal(list.seekGE(100000), docIds.length);
});

test("InvertedIndex indexes terms, freq, and positions", () => {
  const idx = new InvertedIndex(["message"]);
  idx.addTerm("level", "error", 0);
  idx.addTerm("level", "error", 2);
  idx.addTerm("level", "info", 1);
  idx.addTerm("message", "connection", 0, 0);
  idx.addTerm("message", "timeout", 0, 2);
  idx.addTerm("message", "timeout", 2, 1);
  idx.seal();

  assert.deepEqual([...(idx.lookup("level", "error") as PostingList).docIds], [0, 2]);
  const err = idx.lookup("level", "error") as PostingList;
  assert.deepEqual([...err.freqs], [1, 1]);
  assert.equal(idx.lookup("level", "missing"), undefined);

  const timeout = idx.lookup("message", "timeout") as PostingList;
  assert.deepEqual([...(timeout.positionsAt(0) as Uint32Array)], [2]);
  assert.deepEqual([...(timeout.positionsAt(1) as Uint32Array)], [1]);
});

test("hasPhrase matches consecutive positions only", () => {
  const idx = new InvertedIndex(["message"]);
  // doc0: "connection timeout after 5s"  -> connection@0 timeout@1
  idx.addTerm("message", "connection", 0, 0);
  idx.addTerm("message", "timeout", 0, 1);
  // doc1: "timeout connection" -> adjacent, but the reverse phrase order
  idx.addTerm("message", "timeout", 1, 0);
  idx.addTerm("message", "connection", 1, 1);
  // doc2: "a connection b timeout c" -> not consecutive
  idx.addTerm("message", "a", 2, 0);
  idx.addTerm("message", "connection", 2, 1);
  idx.addTerm("message", "b", 2, 2);
  idx.addTerm("message", "timeout", 2, 3);
  idx.addTerm("message", "c", 2, 4);
  idx.seal();

  const p = (t: string) => idx.lookup("message", t) as PostingList;
  assert.equal(hasPhrase([p("connection"), p("timeout")]), true, "doc0 is adjacent");
  assert.equal(hasPhrase([p("connection"), p("b"), p("timeout")]), true, "doc2 triple");
  assert.equal(hasPhrase([p("timeout"), p("connection")]), true, "doc1 has that order");
  assert.equal(hasPhrase([p("connection"), p("connection")]), false, "repeated term");
});

test("hasPhrase works when the anchor term is not the first term", () => {
  const idx = new InvertedIndex(["message"]);
  // doc0: "upstream timeout"  -> timeout@1 upstream@2 ; timeout is rarer
  idx.addTerm("message", "timeout", 0, 1);
  idx.addTerm("message", "upstream", 0, 2);
  // doc1 has upstream alone to make upstream's list not the rarest
  idx.addTerm("message", "upstream", 1, 0);
  idx.seal();
  const p = (t: string) => idx.lookup("message", t) as PostingList;
  assert.equal(hasPhrase([p("timeout"), p("upstream")]), true);
});

test("hasPhrase returns false for empty or positionless lists", () => {
  const idx = new InvertedIndex([]);
  idx.addTerm("level", "error", 0);
  idx.seal();
  const l = idx.lookup("level", "error") as PostingList;
  assert.equal(hasPhrase([]), false);
  assert.equal(hasPhrase([l]), false, "no position data recorded");
});

test("NumericColumn range slices are exact at boundaries", () => {
  const col = NumericColumn.from([5, 1, 9, 3, 7], [10, 11, 12, 13, 14]);
  assert.deepEqual([...col.sliceRange(null, null, true, true)].sort((a, b) => a - b), [10, 11, 12, 13, 14]);
  // values 3,5,7 -> docs 13,10,14
  assert.deepEqual([...col.sliceRange(3, 7, true, true)].sort((a, b) => a - b), [10, 13, 14]);
  // exclusive of 3 and 7 -> only value 5 -> doc 10
  assert.deepEqual([...col.sliceRange(3, 7, false, false)].sort((a, b) => a - b), [10]);
  assert.deepEqual([...col.sliceRange(100, 200, true, true)], []);
  assert.deepEqual([...col.sliceRange(null, 1, true, true)].sort((a, b) => a - b), [11]);
  assert.deepEqual([...col.sliceRange(9, null, true, true)].sort((a, b) => a - b), [12]);
});

test("index-level range uses the numeric column", () => {
  const idx = new InvertedIndex([]);
  idx.addNumeric("latency_ms", 120, 0);
  idx.addNumeric("latency_ms", 30, 1);
  idx.addNumeric("latency_ms", 900, 2);
  idx.seal();
  assert.deepEqual([...idx.range("latency_ms", 100, 500, true, true)].sort((a, b) => a - b), [0]);
  assert.deepEqual([...idx.range("latency_ms", null, null, true, true)].sort((a, b) => a - b), [0, 1, 2]);
  assert.deepEqual([...idx.range("nope", 1, 2, true, true)], []);
  assert.equal(idx.isNumeric("latency_ms"), true);
  assert.equal(idx.isNumeric("level"), false);
});
