/**
 * 存储层测试：LSN 单调无空洞、MVCC 快照隔离、时间范围裁剪、幂等键。
 * 用临时目录，不碰生产数据目录。
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import { Store } from '../store.js';

let counter = 0;
const tmpStore = async (opts = {}) => {
  const dir = path.join(os.tmpdir(), `obsledger-test-${process.pid}-${counter++}`);
  fs.mkdirSync(dir, { recursive: true });
  const store = new Store({ path: path.join(dir, 'db'), ...opts });
  await store.open();
  return { store, dir };
};

const ev = (producer, seq, ts, over = {}) => ({
  stream: 's1', producer, producerSeq: seq, kind: 'audit', level: 'info',
  region: 'cn', service: 'svc', tenant: 't1', ts, value: seq, message: `m${seq}`, ...over,
});

test('追加写分配连续 LSN', async () => {
  const { store } = await tmpStore();
  const r = await store.appendBatch('s1', [ev('p', 0, 1000), ev('p', 1, 2000), ev('p', 2, 3000)]);
  assert.equal(r.startLsn, 1);
  assert.equal(r.endLsn, 3);
  assert.equal(store.watermark, 3);
  await store.close();
});

test('并发批次：LSN 唯一、连续、无空洞，全部可见', async () => {
  const { store } = await tmpStore();
  const PRODUCERS = 20;
  const PER = 50;
  await Promise.all(
    Array.from({ length: PRODUCERS }, (_, p) =>
      (async () => {
        for (let i = 0; i < PER; i += 10) {
          const batch = Array.from({ length: 10 }, (_, j) => ev(`p${p}`, i + j, 1000 + i + j));
          await store.appendBatch('s1', batch);
        }
      })(),
    ),
  );
  const snap = await store.openSnapshot(store.watermark, 's1');
  const all = await store.replay(snap, 10_000);
  assert.equal(all.length, PRODUCERS * PER, '所有并发写入的事件都应可见');

  const lsns = all.map((e) => e.lsn).sort((a, b) => a - b);
  assert.equal(new Set(lsns).size, lsns.length, 'LSN 必须唯一');
  assert.equal(lsns[0], 1);
  assert.equal(lsns[lsns.length - 1], lsns.length, 'LSN 必须连续无空洞');
  await snap.close();
  await store.close();
});

test('MVCC：旧快照看不到后续写入，且不会被撕裂', async () => {
  const { store } = await tmpStore();
  await store.appendBatch('s1', [ev('p', 0, 1000), ev('p', 1, 2000)]);
  const snap = await store.openSnapshot(store.watermark, 's1');
  const before = await store.replay(snap, 100);
  assert.equal(before.length, 2);

  // 写入继续，旧快照内容不变
  await store.appendBatch('s1', [ev('p', 2, 3000), ev('p', 3, 4000)]);
  const stillTwo = await store.replay(snap, 100);
  assert.equal(stillTwo.length, 2, '旧快照应保持 2 条');
  assert.deepEqual(stillTwo.map((e) => e.lsn), before.map((e) => e.lsn));

  // 新快照看到 4 条
  const snap2 = await store.openSnapshot(store.watermark, 's1');
  assert.equal((await store.replay(snap2, 100)).length, 4);
  await snap.close();
  await snap2.close();
  await store.close();
});

test('时间范围查询：闭区间精确', async () => {
  const { store } = await tmpStore();
  await store.appendBatch('s1', [ev('p', 0, 1000), ev('p', 1, 2000), ev('p', 2, 3000), ev('p', 3, 4000), ev('p', 4, 5000)]);
  const snap = await store.openSnapshot(store.watermark, 's1');
  const range = await store.scanRange(snap, { fromTs: 2000, toTs: 4000, limit: 100, order: 'asc' });
  assert.deepEqual(range.map((e) => e.ts), [2000, 3000, 4000]);
  await snap.close();
  await store.close();
});

test('时间范围查询：桶内精确过滤（窄窗口不能漏数据）', async () => {
  const { store } = await tmpStore({ timeBucketMs: 1000 });
  // 同一个时间桶内放 100 条，窗口只取其中 5 条
  const many = Array.from({ length: 100 }, (_, i) => ev('p', i, 10_000 + i * 10));
  await store.appendBatch('s1', many);
  const snap = await store.openSnapshot(store.watermark, 's1');
  const range = await store.scanRange(snap, { fromTs: 10_000, toTs: 10_040, limit: 100, order: 'asc' });
  assert.deepEqual(range.map((e) => e.ts), [10_000, 10_010, 10_020, 10_030, 10_040]);
  await snap.close();
  await store.close();
});

test('时间范围查询：窗口外为空', async () => {
  const { store } = await tmpStore();
  await store.appendBatch('s1', [ev('p', 0, 1000), ev('p', 1, 2000)]);
  const snap = await store.openSnapshot(store.watermark, 's1');
  const empty = await store.scanRange(snap, { fromTs: 50_000, toTs: 60_000, limit: 100, order: 'asc' });
  assert.equal(empty.length, 0);
  await snap.close();
  await store.close();
});

test('幂等：同一 (stream,producer,seq) 重发被去重', async () => {
  const { store } = await tmpStore();
  const batch = [ev('p', 0, 1000), ev('p', 1, 2000)];
  const keys = batch.map((e) => store.dedupKeyFor('s1', 'p', e.producerSeq));
  await store.appendBatch('s1', batch, { dedupKeys: keys });

  const retry = await store.appendBatch('s1', batch, { dedupKeys: keys });
  assert.equal(retry.written, 0);
  assert.equal(retry.deduped, 2);

  const snap = await store.openSnapshot(store.watermark, 's1');
  assert.equal((await store.replay(snap, 100)).length, 2, '重发不应产生重复数据');
  await snap.close();
  await store.close();
});

test('幂等键按流分域：不同流复用 seq 互不影响', async () => {
  const { store } = await tmpStore();
  const batch = [ev('p', 0, 1000), ev('p', 1, 2000)];
  await store.appendBatch('streamA', batch, { dedupKeys: batch.map((e) => store.dedupKeyFor('streamA', 'p', e.producerSeq)) });
  // 同样的 producer+seq，但在另一个流里必须是新数据
  const r = await store.appendBatch('streamB', batch, { dedupKeys: batch.map((e) => store.dedupKeyFor('streamB', 'p', e.producerSeq)) });
  assert.equal(r.written, 2, '不同流不应互相去重');
  const snapA = await store.openSnapshot(store.watermark, 'streamA');
  const snapB = await store.openSnapshot(store.watermark, 'streamB');
  assert.equal((await store.replay(snapA, 100)).length, 2);
  assert.equal((await store.replay(snapB, 100)).length, 2);
  await snapA.close();
  await snapB.close();
  await store.close();
});

test('快照校验：非法 / 超前 LSN 被拒', async () => {
  const { store } = await tmpStore();
  await store.appendBatch('s1', [ev('p', 0, 1000)]);
  await assert.rejects(() => store.openSnapshot(-1, 's1'), (e) => e.code === 'E_LSN_INVALID');
  await assert.rejects(() => store.openSnapshot(99999, 's1'), (e) => e.code === 'E_LSN_AHEAD');
  await store.close();
});

test('重开后 LSN 与数据可恢复', async () => {
  const dir = path.join(os.tmpdir(), `obsledger-reopen-${process.pid}`);
  fs.mkdirSync(dir, { recursive: true });
  const p = path.join(dir, 'db');
  const s1 = new Store({ path: p });
  await s1.open();
  await s1.appendBatch('s1', [ev('p', 0, 1000), ev('p', 1, 2000)]);
  await s1.close();

  const s2 = new Store({ path: p });
  await s2.open();
  assert.equal(s2.watermark, 2, '重开后水位应恢复');
  const snap = await s2.openSnapshot(s2.watermark, 's1');
  assert.equal((await s2.replay(snap, 100)).length, 2);
  await snap.close();
  await s2.close();
});

test('字段往返：二进制编解码不丢字段', async () => {
  const { store } = await tmpStore();
  const rich = ev('prod-x', 7, 1234567890, {
    kind: 'span', level: 'fatal', region: 'ap-southeast', service: 'checkout-api',
    tenant: 'tenant-42', value: -3.75, message: 'a message with spaces & symbols',
  });
  await store.appendBatch('s1', [rich]);
  const snap = await store.openSnapshot(store.watermark, 's1');
  const [got] = await store.replay(snap, 10);
  for (const k of ['producer', 'producerSeq', 'kind', 'level', 'region', 'service', 'tenant', 'ts', 'value', 'message', 'stream']) {
    assert.equal(got[k], rich[k], `字段 ${k} 应原样往返`);
  }
  await snap.close();
  await store.close();
});

test('倒序查询：返回最新的 N 条（回归测试）', async () => {
  // 曾经的 bug：desc 实现为「正序取前 limit*4 条再反转」，
  // limit=5 时返回的是区间中段的 5 条（既不是最新也不是最旧）。
  const { store } = await tmpStore({ timeBucketMs: 1000 });
  await store.appendBatch('s1', Array.from({ length: 500 }, (_, i) => ev('p', i, 1000 + i)));
  const snap = await store.openSnapshot(store.watermark, 's1');

  const top5 = await store.scanRange(snap, { fromTs: 1000, toTs: 1500, limit: 5, order: 'desc' });
  assert.deepEqual(top5.map((e) => e.ts), [1499, 1498, 1497, 1496, 1495], 'desc 必须返回最新的 5 条');

  const head5 = await store.scanRange(snap, { fromTs: 1000, toTs: 1500, limit: 5, order: 'asc' });
  assert.deepEqual(head5.map((e) => e.ts), [1000, 1001, 1002, 1003, 1004], 'asc 必须返回最旧的 5 条');

  const all = await store.scanRange(snap, { fromTs: 1000, toTs: 1500, limit: 1000, order: 'desc' });
  assert.equal(all.length, 500, 'limit 超过可用条数时返回全部');
  assert.deepEqual(all[0].ts, 1499);
  assert.deepEqual(all[all.length - 1].ts, 1000);
  // 严格降序，无重复无遗漏
  for (let i = 1; i < all.length; i++) assert.ok(all[i].ts < all[i - 1].ts, 'desc 必须严格降序');
  await snap.close();
  await store.close();
});
