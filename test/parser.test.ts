/**
 * 功能 1：查询语法解析 —— 语法层与 AST
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { formatAst, parseAst, parseQuery, type FieldTerm, type RangeTerm } from "../src/parser.ts";
import { QuerySyntaxError } from "../src/lexer.ts";

describe("查询解析 -> AST", () => {
  it("解析 field:value AND / OR / NOT", () => {
    assert.equal(formatAst(parseAst("level:error AND service:auth")), "(level:error AND service:auth)");
    assert.equal(formatAst(parseAst("level:error OR level:warn")), "(level:error OR level:warn)");
    assert.equal(formatAst(parseAst("NOT service:auth")), "NOT service:auth");
    assert.equal(formatAst(parseAst("!service:auth")), "NOT service:auth");
  });

  it("引号值是短语（包含语义），裸值是精确等于", () => {
    const quoted = parseAst('message:"connection timeout"') as FieldTerm;
    assert.equal(quoted.kind, "field");
    assert.equal(quoted.quoted, true);
    assert.equal(quoted.value, "connection timeout");

    const bare = parseAst("level:error") as FieldTerm;
    assert.equal(bare.quoted, false);
  });

  it("括号改变优先级", () => {
    assert.equal(
      formatAst(parseAst("level:error AND (service:auth OR service:billing)")),
      "(level:error AND (service:auth OR service:billing))",
    );
  });

  it("相邻条件隐式 AND", () => {
    assert.equal(formatAst(parseAst("level:error service:auth")), "(level:error AND service:auth)");
  });

  it("解析范围 [a TO b]，含无界端点", () => {
    const r = parseAst("ts:[2026-10-01 TO 2026-10-03]") as RangeTerm;
    assert.equal(r.kind, "range");
    assert.equal(r.field, "ts");
    assert.equal(r.lower, "2026-10-01");
    assert.equal(r.upper, "2026-10-03");

    const open = parseAst("latency:[100 TO *]") as RangeTerm;
    assert.equal(open.upper, null);
    const openLow = parseAst("latency:[* TO 100]") as RangeTerm;
    assert.equal(openLow.lower, null);
  });

  it("解析裸词与裸短语（落在 _all 字段）", () => {
    const term = parseAst("timeout");
    assert.equal(term.kind, "term");
    assert.equal(term.kind === "term" && term.field, "_all");

    const phrase = parseAst('"connection timeout"');
    assert.equal(phrase.kind, "phrase");
  });

  it("解析字段存在性 field:*", () => {
    assert.equal(formatAst(parseAst("level:*")), "level:*");
  });

  it("输出字段集合与范围标记", () => {
    const r = parseQuery("level:error AND latency:[1 TO 2]");
    assert.deepEqual(r.fields.sort(), ["latency", "level"]);
    assert.equal(r.hasRange, true);
  });

  it("解析失败给出带 token 位置的错误", () => {
    const cases = ["level:", "(a:1", "a:1 AND", "ts:[2026 TO]", "a:1 OR OR b:2", "level:AND"];
    for (const q of cases) {
      assert.throws(
        () => parseAst(q),
        (err: unknown) => {
          assert.ok(err instanceof QuerySyntaxError, `${q} 应该抛 QuerySyntaxError`);
          assert.equal(err.phase, "parse");
          assert.ok(err.location !== null, `${q} 应带位置`);
          assert.ok(Number.isFinite(err.location.offset), `${q} offset 应为有限数`);
          return true;
        },
        `查询 ${q} 应当报错`,
      );
    }
  });

  it("错误位置指向真正的问题 token", () => {
    try {
      parseAst("a:1 OR OR b:2");
      assert.fail("应当报错");
    } catch (err) {
      assert.ok(err instanceof QuerySyntaxError);
      assert.equal(err.location?.offset, 7);
      assert.equal(err.token, "OR");
    }
  });
});
