/**
 * 统一错误出口测试：错误码目录、HTTP 映射、响应包装、details 白名单。
 * 重点：5xx 不得泄漏内部信息，4xx 必须给出结构化可操作的 details。
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ERROR_CATALOG, AppError, appError, isAppError, toErrorResponse, describeCode,
} from '../errors.js';

test('目录：所有错误码都有 4xx/5xx 状态与文案', () => {
  for (const [code, entry] of Object.entries(ERROR_CATALOG)) {
    assert.ok(entry.status >= 400 && entry.status < 600, `${code} 状态码不合法`);
    assert.equal(typeof entry.message, 'string');
    assert.ok(entry.message.length > 0, `${code} 缺少文案`);
    assert.equal(typeof entry.retryable, 'boolean');
  }
});

test('AppError 携带码 / 状态 / details', () => {
  const e = appError('E_BACKPRESSURE', { limit: 100, retryAfterMs: 50 });
  assert.ok(isAppError(e));
  assert.equal(e.code, 'E_BACKPRESSURE');
  assert.equal(e.status, 429);
  assert.equal(e.retryable, true);
  assert.equal(e.details.limit, 100);
});

test('4xx 保留 details 与错误码', () => {
  const { status, body } = toErrorResponse(new AppError('E_QUERY_FIELD_NOT_ALLOWED', {
    details: { field: 'secret', reason: 'not_in_allowlist' },
  }));
  assert.equal(status, 400);
  assert.equal(body.error.code, 'E_QUERY_FIELD_NOT_ALLOWED');
  assert.equal(body.error.details.field, 'secret');
});

test('5xx 降级为通用文案，不泄漏内部异常', () => {
  const { status, body } = toErrorResponse(new Error('ENOENT /var/secret/path.db'));
  assert.equal(status, 500);
  assert.equal(body.error.code, 'E_INTERNAL');
  assert.ok(!body.error.message.includes('/var/secret'), '不得泄漏内部路径');
  assert.ok(!body.error.message.includes('ENOENT'), '不得泄漏底层错误');
});

test('details 只输出白名单字段', () => {
  const { body } = toErrorResponse(new AppError('E_BAD_REQUEST', {
    details: { field: 'ok', evil: 'should-be-dropped', stack: 'drop-me' },
  }));
  assert.equal(body.error.details.field, 'ok');
  assert.equal(body.error.details.evil, undefined);
  assert.equal(body.error.details.stack, undefined);
});

test('details 中的控制字符被剥离并截断', () => {
  const { body } = toErrorResponse(new AppError('E_BAD_REQUEST', {
    details: { field: 'a'.repeat(500), reason: 'x\u0000y' },
  }));
  assert.ok(body.error.details.field.length <= 120, '超长 details 必须截断');
  assert.ok(!body.error.details.reason.includes('\u0000'), '控制字符必须剥离');
});

test('Fastify 内部错误码映射到目录', () => {
  assert.equal(toErrorResponse({ code: 'FST_ERR_CTP_INVALID_JSON_BODY' }).body.error.code, 'E_BAD_REQUEST');
  assert.equal(toErrorResponse({ code: 'FST_ERR_CTP_BODY_TOO_LARGE' }).body.error.code, 'E_PAYLOAD_TOO_LARGE');
  assert.equal(toErrorResponse({ code: 'FST_ERR_CTP_EMPTY_JSON_BODY' }).body.error.code, 'E_BAD_REQUEST');
});

test('Ajv 校验失败映射为 E_VALIDATION_FAILED 并带路径', () => {
  const { status, body } = toErrorResponse({
    code: 'FST_ERR_VALIDATION',
    validation: [{ instancePath: '/body/events/0/ts', keyword: 'type', message: 'must be integer' }],
  });
  assert.equal(status, 400);
  assert.equal(body.error.code, 'E_VALIDATION_FAILED');
  assert.equal(body.error.details.path, '/body/events/0/ts');
  assert.equal(body.error.details.keyword, 'type');
});

test('未知 Fastify 错误码不被原样泄漏', () => {
  const { body } = toErrorResponse({ code: 'FST_ERR_SOMETHING_INTERNAL' });
  assert.equal(body.error.code, 'E_INTERNAL');
  assert.ok(!JSON.stringify(body).includes('FST_ERR'), '内部错误码不得出现在响应里');
});

test('响应结构始终含 requestId（若提供）', () => {
  const { body } = toErrorResponse(new AppError('E_NOT_FOUND'), 'req-42');
  assert.equal(body.requestId, 'req-42');
  assert.equal(body.error.code, 'E_NOT_FOUND');
});

test('非对象异常也能归一化', () => {
  for (const weird of ['a string', 123, null, undefined, [], true]) {
    const { status, body } = toErrorResponse(weird);
    assert.equal(status, 500);
    assert.equal(body.error.code, 'E_INTERNAL');
    assert.ok(body.error && typeof body.error.message === 'string');
  }
});

test('describeCode 复用同一套文案', () => {
  assert.deepEqual(describeCode('E_NOT_FOUND'), {
    status: ERROR_CATALOG.E_NOT_FOUND.status,
    message: ERROR_CATALOG.E_NOT_FOUND.message,
  });
});
