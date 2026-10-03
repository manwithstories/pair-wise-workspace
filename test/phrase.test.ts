/**
 * 短语校验的字节级子串匹配（查询热路径，必须零分配且大小写不敏感）
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { DocStore } from "../src/segment.ts";

function storeOf(docs: Array<Record<string, unknown>>): DocStore {
  const store = new DocStore(new Set(["message"]));
  for (const d of docs) store.append(d);
  return store;
}

describe("短语字节匹配", () => {
  const store = storeOf([
    { message: "connection timeout while calling upstream" },
    { message: "CONNECTION TIMEOUT on retry" },
    { message: "connection reset by peer" },
    { message: "timeout connection reversed order" },
    { message: "no match here" },
    { message: "中文内容超时 timeout" },
    { message: "" },
  ]);

  it("大小写不敏感地命中子串", () => {
    assert.equal(store.containsPhrase(0, "message", "connection timeout"), true);
    assert.equal(store.containsPhrase(1, "message", "connection timeout"), true);
    assert.equal(store.containsPhrase(1, "message", "CONNECTION TIMEOUT"), true);
  });

  it("子串必须连续出现（词序错就不命中）", () => {
    assert.equal(store.containsPhrase(3, "message", "connection timeout"), false);
    assert.equal(store.containsPhrase(3, "message", "timeout"), true);
  });

  it("不命中返回 false", () => {
    assert.equal(store.containsPhrase(2, "message", "connection timeout"), false);
    assert.equal(store.containsPhrase(4, "message", "anything"), false);
  });

  it("单字符 needle", () => {
    assert.equal(store.containsPhrase(0, "message", "c"), true);
    assert.equal(store.containsPhrase(0, "message", "z"), false);
    // 大小写不敏感：'C' 与 'c' 等价
    assert.equal(store.containsPhrase(1, "message", "C"), true);
    assert.equal(store.containsPhrase(0, "message", "W"), true, "正文含 while 的 W");
    assert.equal(store.containsPhrase(0, "message", "q"), false);
  });

  it("needle 比正文长返回 false", () => {
    assert.equal(store.containsPhrase(4, "message", "a very long phrase that cannot fit"), false);
  });

  it("空正文 / 越界 docId / 未知字段都不命中", () => {
    assert.equal(store.containsPhrase(6, "message", "x"), false);
    assert.equal(store.containsPhrase(999, "message", "x"), false);
    assert.equal(store.containsPhrase(0, "nosuchfield", "x"), false);
  });

  it("非 ASCII 正文走回退路径仍然正确", () => {
    assert.equal(store.containsPhrase(5, "message", "timeout"), true);
    assert.equal(store.containsPhrase(5, "message", "中文内容超时"), true);
    assert.equal(store.containsPhrase(5, "message", "中文"), true);
    assert.equal(store.containsPhrase(4, "message", "中文"), false);
  });

  it("空 needle 不命中（避免匹配一切）", () => {
    assert.equal(store.containsPhrase(0, "message", ""), false);
  });

  it("与 String.includes 的判定一致（随机对拍）", () => {
    const docs: Array<Record<string, unknown>> = [];
    const alphabet = "abcXYZ -_0123";
    let seed = 12345;
    const rnd = (): number => {
      seed = (seed * 1103515245 + 12345) % 2147483648;
      return seed / 2147483648;
    };
    for (let i = 0; i < 200; i++) {
      // 固定 6 字符：docId 就是字符串下标，方便对拍失败时直接复现
      const len = 6;
      let s = "";
      for (let k = 0; k < len; k++) s += alphabet[Math.floor(rnd() * alphabet.length)];
      docs.push({ message: s });
    }
    const s2 = storeOf(docs);
    // 短语长度覆盖 1..=6（含超长），大小写混合 —— 引擎语义是大小写不敏感的子串包含
    const phrases = ["abc", "XYZ", "a", "0", "abc xyz", "zzz", "-", "a-b", "0123", "abcdefghij"];
    for (let d = 0; d < docs.length; d++) {
      const text = String(docs[d]!.message);
      for (const p of phrases) {
        assert.equal(
          s2.containsPhrase(d, "message", p),
          text.toLowerCase().includes(p.toLowerCase()),
          `doc=${d} phrase=${JSON.stringify(p)} text=${JSON.stringify(text)}`,
        );
      }
    }
  });
});
