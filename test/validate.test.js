/**
 * 消毒器单元测试：Unicode 归一化、字符集、长度、穿透检测。
 * 注入能否被挡在门外，取决于这一层。
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  sanitizeIdentifier, sanitizeFreeText, sanitizeNumber,
  sanitizeEvent, sanitizeBatch, sanitizeQuery, sanitizeStreamParam, normalizeText,
} from '../validate.js';

const expectReject = (fn, code) => {
  try {
    fn();
  } catch (e) {
    assert.equal(e.code, code, `expected ${code}, got ${e.code}`);
    return e;
  }
  assert.fail('expected a rejection, but the call succeeded');
};

test('标识符：合法值通过', () => {
  assert.equal(sanitizeIdentifier('svc-checkout', 'service', 64), 'svc-checkout');
  assert.equal(sanitizeIdentifier('a1:b_c-2', 'stream', 64), 'a1:b_c-2');
});

test('标识符：点号 / 原型链 / 引号全部拒绝', () => {
  expectReject(() => sanitizeIdentifier('a.b', 'service', 64), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeIdentifier('__proto__', 'service', 64), 'E_QUERY_INJECTION');
  expectReject(() => sanitizeIdentifier('constructor', 'service', 64), 'E_QUERY_INJECTION');
  expectReject(() => sanitizeIdentifier("ad'min", 'service', 64), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeIdentifier('a/b', 'service', 64), 'E_SANITIZE_FAILED');
});

test('标识符：超长与空值拒绝', () => {
  expectReject(() => sanitizeIdentifier('x'.repeat(100), 'service', 64), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeIdentifier('   ', 'service', 64), 'E_SANITIZE_FAILED');
});

test('同形异义字符被折叠归一化', () => {
  assert.equal(normalizeText('раypal'), 'paypal');
  assert.equal(normalizeText('ａｄｍｉｎ'), 'admin');
  assert.equal(normalizeText('ad\u200bmin'), 'admin');
  assert.equal(normalizeText('ad\u202emin'), 'admin');
});

test('自由文本：允许引号但拒绝控制字符与超长', () => {
  assert.equal(sanitizeFreeText("it's ok", 'message', 1024), "it's ok");
  expectReject(() => sanitizeFreeText('x'.repeat(2000), 'message', 1024), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeFreeText('a\u0000b', 'message', 1024), 'E_SANITIZE_FAILED');
});

test('数字：拒绝 NaN / Infinity / 越界 / 十六进制串', () => {
  assert.equal(sanitizeNumber(42, 'value', -1e15, 1e15), 42);
  assert.equal(sanitizeNumber('42', 'value', -1e15, 1e15), 42);
  expectReject(() => sanitizeNumber(NaN, 'value', -1e15, 1e15), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeNumber(Infinity, 'value', -1e15, 1e15), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeNumber(1e16, 'value', -1e15, 1e15), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeNumber('0x1f', 'value', -1e15, 1e15), 'E_SANITIZE_FAILED');
});

test('事件：枚举与整数约束生效', () => {
  const good = { producerSeq: 1, kind: 'audit', ts: 1000, value: 1.5, service: 'svc' };
  assert.equal(sanitizeEvent(good, { stream: 's', producer: 'p' }).kind, 'audit');
  expectReject(() => sanitizeEvent({ ...good, kind: 'evil' }, { stream: 's', producer: 'p' }), 'E_SANITIZE_FAILED');
  expectReject(() => sanitizeEvent({ ...good, producerSeq: -1 }, { stream: 's', producer: 'p' }), 'E_SANITIZE_FAILED');
});

test('批次：错误事件带上出错下标', () => {
  const good = { producerSeq: 1, kind: 'audit', ts: 1000 };
  assert.equal(sanitizeBatch({ stream: 's', producer: 'p', events: [good, good] }).events.length, 2);
  const err = expectReject(
    () => sanitizeBatch({ stream: 's', producer: 'p', events: [good, { ...good, service: 'a.b' }] }),
    'E_SANITIZE_FAILED',
  );
  assert.equal(err.details.path, '/events/1');
});

test('查询体：filter 控制字符被拒', () => {
  assert.equal(sanitizeQuery({ filter: 'ts >= 1' }).limit, 100);
  expectReject(() => sanitizeQuery({ filter: 'ts >= 1\u0000' }), 'E_QUERY_INJECTION');
});

test('路径参数 stream 受同一套消毒', () => {
  assert.equal(sanitizeStreamParam('metrics'), 'metrics');
  expectReject(() => sanitizeStreamParam('../etc'), 'E_SANITIZE_FAILED');
});
