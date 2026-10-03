/**
 * HTTP 层集成测试：路由、状态码、错误结构、端到端读写。
 * 用 fastify 的 inject，不占端口，跑得快且确定性强。
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import { buildServer } from '../server.js';

let n = 0;
const mkServer = async () => {
  const dir = path.join(os.tmpdir(), `obsledger-http-${process.pid}-${n++}`);
  fs.mkdirSync(dir, { recursive: true });
  return buildServer({ dataDir: dir, logger: false });
};

// 摄入是异步微批：写入后要等水位推进才可查
const settle = async (server, want) => {
  for (let i = 0; i < 200; i++) {
    const r = await server.app.inject({ method: 'GET', url: '/healthz' });
    if (r.json().lsn >= want) return true;
    await new Promise((res) => setTimeout(res, 5));
  }
  return false;
};

test('healthz 与 stats 可用', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const h = await s.app.inject({ method: 'GET', url: '/healthz' });
  assert.equal(h.statusCode, 200);
  assert.equal(h.json().status, 'ok');
  const st = await s.app.inject({ method: 'GET', url: '/v1/stats' });
  assert.equal(st.statusCode, 200);
  assert.ok(typeof st.json().lsn === 'number');
});

test('写入返回 202 与 LSN', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const r = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events: [{ producerSeq: 1, kind: 'audit', ts: 1000 }] },
  });
  assert.equal(r.statusCode, 202);
  const body = r.json();
  assert.equal(body.accepted, 1);
  assert.ok(body.lsn > 0);
});

test('端到端：写入后按时间窗与字段查询', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const events = [1000, 2000, 3000].map((ts, i) => ({
    producerSeq: i, kind: 'metric', ts, service: i === 1 ? 'other' : 'checkout', value: i,
  }));
  const w = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events },
  });
  await settle(s, w.json().lsn);

  const q = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/query',
    payload: { filter: 'ts >= 1000 and ts <= 2000', limit: 10 },
  });
  assert.equal(q.statusCode, 200);
  const body = q.json();
  assert.equal(body.count, 2, '时间窗内应有 2 条');
  assert.deepEqual(body.events.map((e) => e.ts).sort((a, b) => a - b), [1000, 2000]);

  const f = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/query',
    payload: { filter: 'service == "checkout"', limit: 10 },
  });
  assert.equal(f.json().count, 2);
});

test('limit 不会被存储层提前截断（回归测试）', async (t) => {
  // 曾经有个 bug：存储层先按 limit 截断，谓词还没跑就被砍掉，
  // 导致命中行落在截断点之后时查询恒返回空。
  const s = await mkServer();
  t.after(() => s.app.close());
  const many = Array.from({ length: 300 }, (_, i) => ({ producerSeq: i, kind: 'metric', ts: 1000 + i }));
  for (let i = 0; i < many.length; i += 100) {
    await s.app.inject({
      method: 'POST', url: '/v1/streams/m/events',
      payload: { stream: 'm', producer: 'p', events: many.slice(i, i + 100) },
    });
  }
  const last = await s.app.inject({ method: 'GET', url: '/healthz' });
  await settle(s, last.json().lsn);

  // 目标事件排在最后，limit=100 只应影响返回条数，不应让它查不到
  await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p2', events: [{ producerSeq: 1, kind: 'metric', ts: 9999, service: 'needle' }] },
  });
  const h2 = await s.app.inject({ method: 'GET', url: '/healthz' });
  await settle(s, h2.json().lsn);

  const q = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/query',
    payload: { filter: 'service == "needle"', limit: 100 },
  });
  assert.equal(q.statusCode, 200);
  assert.equal(q.json().count, 1, '末尾的命中事件必须能被查到');
});

test('注入载荷返回 400 与结构化错误码', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const cases = [
    ['ts >= 1; DROP TABLE users', 'E_QUERY_INJECTION'],
    ['service.__proto__.x == "1"', 'E_QUERY_INJECTION'],
    ['serviсe == "a"', 'E_QUERY_INJECTION'],
    ['secret == "x"', 'E_QUERY_FIELD_NOT_ALLOWED'],
    ['ts >= 0x1f', 'E_QUERY_INJECTION'],
  ];
  for (const [filter, code] of cases) {
    const r = await s.app.inject({ method: 'POST', url: '/v1/streams/m/query', payload: { filter } });
    assert.equal(r.statusCode, 400, `filter=${filter} 应返回 400`);
    assert.equal(r.json().error.code, code, `filter=${filter} 错误码不符`);
    assert.ok(r.json().error.message, '错误必须带文案');
    assert.ok(r.json().requestId, '错误必须带 requestId');
  }
});

test('schema 与消毒失败均返回 400', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  // 类型不符（events 非数组）
  const bad = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events: 'nope' },
  });
  assert.equal(bad.statusCode, 400);
  // 注入字段值
  const inj = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events: [{ producerSeq: 1, kind: 'audit', ts: 1, service: 'a.b' }] },
  });
  assert.equal(inj.statusCode, 400);
  assert.equal(inj.json().error.code, 'E_SANITIZE_FAILED');
});

test('路径与体内 stream 不一致返回 400', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const r = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'other', producer: 'p', events: [{ producerSeq: 1, kind: 'audit', ts: 1 }] },
  });
  assert.equal(r.statusCode, 400);
  assert.equal(r.json().error.code, 'E_VALIDATION_FAILED');
});

test('非法路径参数被消毒拦截', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const r = await s.app.inject({
    method: 'POST', url: '/v1/streams/__proto__/events',
    payload: { stream: '__proto__', producer: 'p', events: [{ producerSeq: 1, kind: 'audit', ts: 1 }] },
  });
  assert.equal(r.statusCode, 400);
});

test('快照 LSN：旧快照不可见后续写入，最新可见', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const w1 = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events: [{ producerSeq: 1, kind: 'audit', ts: 1000 }] },
  });
  await settle(s, w1.json().lsn);
  const marker = (await s.app.inject({ method: 'GET', url: '/healthz' })).json().lsn;

  const w2 = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events: [{ producerSeq: 2, kind: 'audit', ts: 2000, service: 'later' }] },
  });
  await settle(s, w2.json().lsn);

  const oldQ = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/query',
    payload: { lsn: marker, filter: 'service == "later"' },
  });
  assert.equal(oldQ.json().count, 0, '旧快照不应看到后续写入');

  const now = (await s.app.inject({ method: 'GET', url: '/healthz' })).json().lsn;
  const newQ = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/query',
    payload: { lsn: now, filter: 'service == "later"' },
  });
  assert.equal(newQ.json().count, 1, '最新快照应看到该写入');
});

test('幂等：重发同批不增加数据量', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const payload = {
    stream: 'idem', producer: 'p',
    events: [{ producerSeq: 1, kind: 'audit', ts: 1000 }, { producerSeq: 2, kind: 'audit', ts: 1001 }],
  };
  const first = await s.app.inject({ method: 'POST', url: '/v1/streams/idem/events', payload });
  await settle(s, first.json().lsn);
  const retry = await s.app.inject({ method: 'POST', url: '/v1/streams/idem/events', payload });
  assert.equal(retry.statusCode, 202);
  assert.equal(retry.json().deduped, 2);
  await s.app.inject({ method: 'POST', url: '/v1/streams/idem/events', payload });
  const q = await s.app.inject({
    method: 'POST', url: '/v1/streams/idem/query', payload: { filter: 'lsn >= 1', limit: 100 },
  });
  assert.equal(q.json().count, 2, '重发不应产生重复');
});

test('replay 按 LSN 回放', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const w = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events: [{ producerSeq: 1, kind: 'audit', ts: 1000 }] },
  });
  await settle(s, w.json().lsn);
  const r = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/replay', payload: { filter: 'lsn >= 1', limit: 10 },
  });
  assert.equal(r.statusCode, 200);
  assert.ok(r.json().count >= 1);
});

test('未知路由返回 404 与统一错误体', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const r = await s.app.inject({ method: 'GET', url: '/v1/nope' });
  assert.equal(r.statusCode, 404);
  assert.equal(r.json().error.code, 'E_NOT_FOUND');
});

test('超前 LSN 返回 400', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const r = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/query', payload: { lsn: 999999999, filter: 'lsn >= 1' },
  });
  assert.equal(r.statusCode, 400);
  assert.equal(r.json().error.code, 'E_LSN_AHEAD');
});

test('admin/compact 触发后台压缩并返回统计', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  await s.app.inject({
    method: 'POST', url: '/v1/streams/m/events',
    payload: { stream: 'm', producer: 'p', events: [{ producerSeq: 1, kind: 'audit', ts: Date.now() }] },
  });
  const r = await s.app.inject({ method: 'POST', url: '/v1/admin/compact' });
  assert.equal(r.statusCode, 200);
  assert.ok(typeof r.json().compaction.durationMs === 'number');
});

test('并发写入：50 个生产者全部落盘且 LSN 连续', async (t) => {
  const s = await mkServer();
  t.after(() => s.app.close());
  const PRODUCERS = 50;
  const PER = 20;
  const writes = Array.from({ length: PRODUCERS }, (_, p) =>
    s.app.inject({
      method: 'POST', url: '/v1/streams/m/events',
      payload: {
        stream: 'm', producer: `p${p}`,
        events: Array.from({ length: PER }, (_, j) => ({ producerSeq: j, kind: 'metric', ts: 1000 + j })),
      },
    }),
  );
  const results = await Promise.all(writes);
  for (const r of results) assert.equal(r.statusCode, 202);
  await settle(s, PRODUCERS * PER);

  const q = await s.app.inject({
    method: 'POST', url: '/v1/streams/m/query', payload: { filter: 'lsn >= 1', limit: 1000 },
  });
  assert.equal(q.json().count, PRODUCERS * PER, '所有并发写入都应可见');
});
