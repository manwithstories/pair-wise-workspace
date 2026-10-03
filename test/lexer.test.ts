/**
 * 功能 1：查询语法解析 —— 词法层
 */
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { tokenize, tokenValue, QuerySyntaxError, TokenNames } from "../src/lexer.ts";

describe("词法分析", () => {
  it("切出 field:value / 关键字 / 短语，并带精确位置", () => {
    const { tokens } = tokenize('level:error AND message:"connection timeout"');
    const names = tokens.map((t) => t.tokenType.name);
    assert.deepEqual(names, [
      TokenNames.Word,
      TokenNames.Colon,
      TokenNames.Word,
      TokenNames.And,
      TokenNames.Word,
      TokenNames.Colon,
      TokenNames.Phrase,
    ]);
    // 短语去掉引号并保留内部空格
    assert.equal(tokenValue(tokens[6]!), "connection timeout");
    // 位置信息可用于错误定位
    assert.equal(tokens[0]!.startColumn, 1);
    assert.equal(tokens[2]!.startColumn, 7);
  });

  it("关键字大小写不敏感，NOT 支持 ! 简写", () => {
    assert.ok(tokenize("a:1 and b:2").tokens.some((t) => t.tokenType.name === TokenNames.And));
    assert.ok(tokenize("a:1 And b:2").tokens.some((t) => t.tokenType.name === TokenNames.And));
    assert.ok(tokenize("!a:1").tokens.some((t) => t.tokenType.name === TokenNames.Not));
  });

  it("短语支持转义", () => {
    const { tokens } = tokenize('m:"say \\"hi\\""');
    assert.equal(tokenValue(tokens[tokens.length - 1]!), 'say "hi"');
  });

  it("非法字符抛带位置的词法错误", () => {
    assert.throws(
      () => tokenize('m:"unterminated'),
      (err: unknown) => {
        assert.ok(err instanceof QuerySyntaxError);
        assert.equal(err.phase, "lex");
        assert.ok(err.location !== null);
        assert.match(err.describe(), /@ line \d+:\d+ \(offset \d+\)/);
        return true;
      },
    );
  });

  it("空查询与非字符串输入报错", () => {
    assert.throws(() => tokenize("   "), QuerySyntaxError);
    assert.throws(() => tokenize(undefined as unknown as string), QuerySyntaxError);
  });
});
