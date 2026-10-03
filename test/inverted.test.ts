/**
 * 倒排表与 posting 跳表
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  InvertedIndexBuilder,
  SKIP_LIST_MIN_DF,
  buildSkipList,
  complementPosting,
  intersectPostings,
  makePosting,
  seekPosting,
  skipStrideFor,
  subtractPostings,
  unionPostings,
  type Posting,
} from "../src/inverted.ts";

function postingOf(docs: number[], docCount = 64): Posting {
  return makePosting(Int32Array.from(docs), docCount);
}

const arr = (v: Int32Array | Float64Array): number[] => Array.from(v);

describe("posting 跳表", () => {
  it("块内最大 docId 构成有序跳表", () => {
    const docs = Int32Array.from([0, 1, 2, 3, 10, 11, 20]);
    const skip = buildSkipList(docs, 3);
    assert.deepEqual(arr(skip), [2, 11, 20]);
    // 跳表整体单调不减，seek 才能二分
    for (let i = 1; i < skip.length; i++) assert.ok(skip[i]! >= skip[i - 1]!);
  });

  it("seek 定位到第一个 >= target 的下标", () => {
    const p = postingOf([0, 1, 2, 3, 10, 11, 20], 64);
    assert.equal(seekPosting(p, 0), 0);
    assert.equal(seekPosting(p, 4), 4);
    assert.equal(seekPosting(p, 10), 4);
    assert.equal(seekPosting(p, 21), p.docs.length);
  });

  it("stride 随段规模取 sqrt 量级", () => {
    assert.equal(skipStrideFor(0), 1);
    assert.equal(skipStrideFor(100), 10);
    assert.equal(skipStrideFor(10_000), 100);
  });
});

describe("posting 集合运算", () => {
  const a = postingOf([0, 1, 2, 5, 8], 16);
  const b = postingOf([1, 3, 5, 7], 16);
  const buf = new Int32Array(64);

  it("求交", () => {
    assert.deepEqual(arr(intersectPostings(a, b, buf)), [1, 5]);
    assert.deepEqual(arr(intersectPostings(b, a, buf)), [1, 5]); // 交换律
  });

  it("求并去重", () => {
    assert.deepEqual(arr(unionPostings(a, b, buf)), [0, 1, 2, 3, 5, 7, 8]);
  });

  it("差集", () => {
    assert.deepEqual(arr(subtractPostings(a, b, buf)), [0, 2, 8]);
  });

  it("补集", () => {
    assert.deepEqual(arr(complementPosting(b, 9, buf)), [0, 2, 4, 6, 8]);
  });

  it("与朴素实现结果一致（跳表推进不改变语义）", () => {
    const rnd = (seed: number): number[] => {
      const out: number[] = [];
      let s = seed;
      for (let i = 0; i < 200; i++) {
        s = (s * 1103515245 + 12345) % 2147483648;
        out.push(s % 100);
      }
      return [...new Set(out)].sort((x, y) => x - y);
    };
    for (let seed = 1; seed <= 20; seed++) {
      const x = postingOf(rnd(seed), 128);
      const y = postingOf(rnd(seed * 31), 128);
      const setY = new Set(Array.from(y.docs));
      const expected = Array.from(x.docs).filter((d) => setY.has(d));
      assert.deepEqual(arr(intersectPostings(x, y, buf)), expected, `seed=${seed}`);
    }
  });
});

describe("倒排表构建", () => {
  it("冻结为只读 posting，并带 df", () => {
    const b = new InvertedIndexBuilder();
    [["a", "b"], ["a"], ["b", "c"]].forEach((terms, docId) => {
      b.markDoc();
      for (const t of terms) b.addTerm("tag", t, docId);
    });
    const idx = b.build();
    assert.equal(idx.docCount, 3);
    assert.equal(idx.fields.get("tag")?.get("a")?.df, 2);
    assert.equal(idx.fields.get("tag")?.get("c")?.df, 1);
    assert.deepEqual(arr(idx.fields.get("tag")!.get("a")!.docs), [0, 1]);
  });

  it("计数排序：docId 天然升序，无需额外排序", () => {
    const b = new InvertedIndexBuilder();
    // 乱序写入多个 term，验证输出仍按 docId 升序
    for (let docId = 0; docId < 50; docId++) {
      b.markDoc();
      b.addTerm("f", "common", docId);
      b.addTerm("f", `t${docId % 5}`, docId);
    }
    const idx = b.build();
    const common = idx.fields.get("f")!.get("common")!;
    assert.deepEqual(arr(common.docs), Array.from({ length: 50 }, (_, i) => i));
    // 大 df 才会建跳表
    assert.ok(common.skipList.length > 0);
    for (let k = 0; k < 5; k++) {
      const p = idx.fields.get("f")!.get(`t${k}`)!;
      assert.equal(p.docs.length, 10);
      assert.deepEqual(arr(p.docs), Array.from({ length: 10 }, (_, i) => i * 5 + k));
    }
  });

  it("小 df 的 posting 不建跳表（省内存）", () => {
    const b = new InvertedIndexBuilder();
    for (let docId = 0; docId < 10; docId++) {
      b.markDoc();
      b.addTerm("f", "rare", docId);
    }
    const idx = b.build();
    const rare = idx.fields.get("f")!.get("rare")!;
    assert.equal(rare.df, 10);
    assert.equal(rare.skipList.length, 0, "df 低于阈值不应建跳表");
    // 无跳表时 seek 退化为线性扫描，语义仍然正确
    assert.equal(seekPosting(rare, 5), 5);
  });

  it("addTextField 登记文本字段", () => {
    const b = new InvertedIndexBuilder();
    b.addTextField("message");
    b.markDoc();
    b.addTerm("message", "hello", 0);
    assert.deepEqual([...b.build().textFields], ["message"]);
  });

  it("跳表阈值常量与实现一致（df 恰好等于阈值时建跳表）", () => {
    const b = new InvertedIndexBuilder();
    for (let docId = 0; docId < SKIP_LIST_MIN_DF; docId++) {
      b.markDoc();
      b.addTerm("f", "t", docId);
    }
    const posting = b.build().fields.get("f")!.get("t")!;
    assert.equal(posting.df, SKIP_LIST_MIN_DF);
    assert.ok(posting.skipList.length > 0, "df >= 阈值应当建跳表");
  });

  it("数值字段建排序表，供范围二分", () => {
    const b = new InvertedIndexBuilder();
    [30, 10, 20].forEach((v, docId) => {
      b.markDoc();
      b.addNumeric("latency", v, docId);
    });
    const idx = b.build();
    const num = idx.numeric.get("latency")!;
    assert.deepEqual(arr(num.values), [10, 20, 30]);
    assert.deepEqual(arr(num.docs), [1, 2, 0]);
  });
});
