/**
 * 查询层单元测试：DSL 解析成 AST + 参数绑定，以及各类注入变体被拦截。
 * 关键断言：字段名不进执行期结构，值只以参数槽参与比较。
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { planQuery, tokenize, parseQuery } from '../query.js';

const expectReject = (filter, code) => {
  try {
    planQuery(filter, 'm');
  } catch (e) {
    assert.equal(e.code, code, `filter=${JSON.stringify(filter)} expected ${code}, got ${e.code} (${JSON.stringify(e.details)})`);
    return e;
  }
  assert.fail(`filter=${JSON.stringify(filter)} should have been rejected`);
};

test('合法表达式解析为 AST 并把值放进 params', () => {
  const p = planQuery('ts >= 1000 and ts <= 2000', 'm');
  assert.equal(p.ast.root.kind, 'and');
  assert.deepEqual(p.ast.params, [1000, 2000]);
  assert.deepEqual(p.usedFields, ['ts']);
  assert.deepEqual(p.timeWindow, { fromTs: 1000, toTs: 2000 });
});

test('AND / OR / 括号与优先级', () => {
  const p = planQuery('(level == "warn" or level == "error") and region == "cn"', 'm');
  assert.equal(p.ast.root.kind, 'and');
  assert.equal(p.ast.root.children[0].kind, 'or');
  assert.deepEqual(p.ast.params, ['warn', 'error', 'cn']);
});

test('运算符按字段类型校验', () => {
  expectReject('service > "a"', 'E_QUERY_OPERATOR_NOT_ALLOWED');
  expectReject('ts == "abc"', 'E_QUERY_VALUE_INVALID');
  assert.equal(planQuery('value > 12.5', 'm').ast.params[0], 12.5);
});

test('白名单外的字段一律拒绝', () => {
  expectReject('secret_field == "x"', 'E_QUERY_FIELD_NOT_ALLOWED');
  expectReject('constructor == "x"', 'E_QUERY_FIELD_NOT_ALLOWED');
});

test('注入：多语句 / 注释 / 管道 / 数字走私', () => {
  expectReject('ts >= 1; DROP TABLE users', 'E_QUERY_INJECTION');
  expectReject('ts >= 1 -- x', 'E_QUERY_INJECTION');
  expectReject('ts >= 1 /* x */', 'E_QUERY_INJECTION');
  expectReject('service == "a" | cmd', 'E_QUERY_INJECTION');
  expectReject('ts >= 0x1f', 'E_QUERY_INJECTION');
  expectReject('ts >= 1e10', 'E_QUERY_INJECTION');
  expectReject('ts >= 1abc', 'E_QUERY_INJECTION');
  expectReject('ts >= 1 garbage', 'E_QUERY_INJECTION');
});

test('注入：点号原型链穿透', () => {
  expectReject('service.__proto__.polluted == "1"', 'E_QUERY_INJECTION');
  expectReject('service["x"] == "1"', 'E_QUERY_INJECTION');
});

test('注入：引号走私与未闭合字符串', () => {
  expectReject('service == "a\'b"', 'E_QUERY_INJECTION');
  expectReject('service == "abc', 'E_QUERY_SYNTAX');
  expectReject('service == "a\\u0000b"', 'E_QUERY_INJECTION');
});

test('注入：Unicode 同形 / 全角 / 零宽字段名被拒（不是静默折叠）', () => {
  expectReject('serviсe == "a"', 'E_QUERY_INJECTION');   // 西里尔 с
  expectReject('ｓervice == "a"', 'E_QUERY_INJECTION');   // 全角
  expectReject('servic\u200be == "a"', 'E_QUERY_INJECTION'); // 零宽
  const e = expectReject('serviсe == "a"', 'E_QUERY_INJECTION');
  assert.equal(e.details.reason, 'homoglyph_field_name');
});

test('注入：布尔走私 / 尾随 token', () => {
  expectReject('service == "a" or ts < 0 or "1"=="1"', 'E_QUERY_SYNTAX');
  expectReject('service == "a" and 1 == 1', 'E_QUERY_SYNTAX');
});

test('值里的引号与分号是数据，不是语法', () => {
  // 值永远以参数绑定参与比较，不会被二次解析，因此这些是安全的。
  const p = planQuery('message == "a; DROP TABLE t"', 'm');
  assert.deepEqual(p.ast.params, ['a; DROP TABLE t']);
});

test('复杂度预算：通配量词炸弹与超长 filter', () => {
  // 量词炸弹：必须是"被拦下"，具体错误码断言放宽到复杂度类
  expectReject('message ~ "a*?*?*?*b"', 'E_QUERY_TOO_COMPLEX');
  // 超长 filter 触发长度预算
  expectReject('x'.repeat(3000), 'E_QUERY_TOO_COMPLEX');
  // 大量 AND 串联触发 token/node 预算
  const many = Array.from({ length: 200 }, (_, i) => `ts >= ${i}`).join(' and ');
  expectReject(many, 'E_QUERY_TOO_COMPLEX');
});

test('通配匹配：* 与 ?', () => {
  const p = planQuery('service ~ "pay*"', 'm');
  const ev = { stream: 'm', service: 'paypal', lsn: 1, producerSeq: 1, ts: 1, value: 0, kind: 'log', level: 'info', producer: 'p', region: 'r', tenant: 't', message: '' };
  assert.equal(p.predicate(ev), true);
  assert.equal(planQuery('service ~ "paypal"', 'm').predicate(ev), true);
  // 无通配符 => 全等匹配，"pay" 不等于 "paypal"
  assert.equal(planQuery('service ~ "pay"', 'm').predicate(ev), false);
  // ? 匹配单个字符
  assert.equal(planQuery('service ~ "payp??"', 'm').predicate(ev), true);
  assert.equal(planQuery('service ~ "zz*"', 'm').predicate(ev), false);
});

test('谓词执行：比较语义正确', () => {
  const mk = (over) => ({ stream: 'm', lsn: 5, producerSeq: 1, ts: 1000, value: 7, kind: 'audit', level: 'error', producer: 'p', region: 'cn', service: 'checkout', tenant: 't1', message: 'hi', ...over });
  assert.equal(planQuery('service == "checkout"', 'm').predicate(mk()), true);
  assert.equal(planQuery('service == "other"', 'm').predicate(mk()), false);
  assert.equal(planQuery('ts >= 1000', 'm').predicate(mk()), true);
  assert.equal(planQuery('ts > 1000', 'm').predicate(mk()), false);
  assert.equal(planQuery('lsn <= 5', 'm').predicate(mk()), true);
  assert.equal(planQuery('level == "error" or level == "warn"', 'm').predicate(mk()), true);
  assert.equal(planQuery('kind == "log" and region == "us"', 'm').predicate(mk()), false);
});

test('stream() 引用也走字符集校验', () => {
  assert.equal(planQuery('stream("other")', 'm').ast.root.stream, 'other');
  expectReject('stream("a.b")', 'E_QUERY_INJECTION');
});

test('lexer 不在非 ASCII 处断词（同形字符不会被拆开）', () => {
  const toks = tokenize('serviсe == "a"');
  assert.equal(toks[0].type, 'ident');
  assert.equal(toks[0].value, 'serviсe');
});

test('parseQuery 拒绝空与超长 filter', () => {
  expectReject('', 'E_QUERY_SYNTAX');
  expectReject('x'.repeat(3000), 'E_QUERY_TOO_COMPLEX');
});
