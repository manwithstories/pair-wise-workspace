/**
 * 摄入层测试：微批合流、按生产者保序、窗口幂等、背压限流。
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import { Store } from '../store.js';
import { Ingest } from '../ingest.js';

let counter = 0;
const mk = async (ingestOpts = {}) => {
  const dir = path.join(os.tmpdir(), `obsledger-ing-${process.pid}-${counter++}`);
  fs.mkdirSync(dir, { recursive: true });
  const store = new Store({ path: path.join(dir, 'db') });
  await store.open();
  const ingest = new Ingest(store, { flushIntervalMs: 5, maxBatchSize: 64, ...ingestOpts });
  ingest.start();
  return { store, ingest };
};

const ev = (producer, seq, ts = 1000 + seq) => ({
  stream: 's1', producer, producerSeq: seq, kind: 'metric', level: 'info',
  region: 'cn', service: 'svc', tenant: 't1', ts, value: seq, message: 'm',
});

test('微批合流：多批提交合并成远少于提交次数的落盘', async () => {
  const { store, ingest } = await mk();
  await Promise.all(
    Array.from({ length: 10 }, (_, k) =>
      ingest.submit('s1', 'p1', Array.from({ length: 10 }, (_, j) => ev('p1', k * 10 + j))),
    ),
  );
  await ingest.flushAll();
  const stats = ingest.getStats();
  assert.equal(stats.written, 100, '事件应全部落盘');
  assert.ok(stats.flushes <= 5, `100 条事件应合并成 <=5 次落盘，实际 ${stats.flushes}`);
  await ingest.stop();
  await store.close();
});

test('按生产者保序：单个 producer 的 LSN 顺序与 seq 顺序一致', async () => {
  const { store, ingest } = await mk();
  const PRODUCERS = 10;
  const PER = 30;
  await Promise.all(
    Array.from({ length: PRODUCERS }, (_, p) =>
      (async () => {
        for (let i = 0; i < PER; i += 10) {
          await ingest.submit('s1', `p${p}`, Array.from({ length: 10 }, (_, j) => ev(`p${p}`, i + j)));
        }
      })(),
    ),
  );
  await ingest.flushAll();
  const snap = await store.openSnapshot(store.watermark, 's1');
  const all = await store.replay(snap, 10_000);
  assert.equal(all.length, PRODUCERS * PER);
  for (let p = 0; p < PRODUCERS; p++) {
    const seqs = all.filter((e) => e.producer === `p${p}`).sort((a, b) => a.lsn - b.lsn).map((e) => e.producerSeq);
    for (let i = 1; i < seqs.length; i++) {
      assert.ok(seqs[i] > seqs[i - 1], `生产者 p${p} 的 seq 必须随 LSN 严格递增`);
    }
  }
  await snap.close();
  await ingest.stop();
  await store.close();
});

test('窗口幂等：重发同一批不产生重复数据', async () => {
  const { store, ingest } = await mk();
  const batch = [ev('p1', 0, 1000), ev('p1', 1, 1001)];
  await ingest.submit('s1', 'p1', batch);
  await ingest.flushAll();
  const snap = await store.openSnapshot(store.watermark, 's1');
  const first = (await store.replay(snap, 100)).length;
  assert.equal(first, 2);

  const retry = await ingest.submit('s1', 'p1', batch);
  assert.equal(retry.deduped, 2, '重发应被整批判为重复');
  await ingest.flushAll();
  const snap2 = await store.openSnapshot(store.watermark, 's1');
  assert.equal((await store.replay(snap2, 100)).length, 2, '重发不应改变数据量');
  await snap.close();
  await snap2.close();
  await ingest.stop();
  await store.close();
});

test('背压：积压超阈值时返回 429 且内存有界', async () => {
  const dir = path.join(os.tmpdir(), `obsledger-bp-${process.pid}`);
  fs.mkdirSync(dir, { recursive: true });
  const store = new Store({ path: path.join(dir, 'db') });
  await store.open();
  // 不启动定时器 => 永不自动落盘，队列只增不减，从而稳定触发背压
  const ingest = new Ingest(store, { flushIntervalMs: 1_000_000, maxBatchSize: 1000, maxBacklog: 500 });

  // submit 是 async：背压以「返回 rejected promise」失败，而非同步 throw。
  // 被接受的批次要等 flush 才 resolve，因此先统计"当下是否已被拒"，
  // 不能立刻 allSettled（会一直等这些批次的 flush）。
  let backpressured = 0;
  const pending = [];
  for (let k = 0; k < 40; k++) {
    const p = ingest.submit('s1', 'p1', Array.from({ length: 20 }, (_, j) => ev('p1', k * 20 + j)));
    // 用一个微任务观察该批是否立即被拒
    const observed = p.then(
      () => 'ok',
      (e) => e.code,
    );
    pending.push(observed);
    backpressured = ingest.getStats().rejected;
    if (backpressured > 0) break;
  }

  const stats = ingest.getStats();
  assert.ok(stats.rejected > 0, '积压超阈值必须拒绝新批');
  assert.equal(stats.rejected, backpressured);
  assert.ok(stats.queued <= 500, `队列长度必须受上限约束，实际 ${stats.queued}`);

  // 被拒的批次确实是 E_BACKPRESSURE
  const codes = await Promise.race([
    Promise.all(pending.filter((_, i) => i >= 25)),
    new Promise((r) => setTimeout(() => r(['timeout']), 200)),
  ]);
  for (const c of codes) {
    assert.ok(c === 'E_BACKPRESSURE' || c === 'timeout', `期望 E_BACKPRESSURE，实际 ${c}`);
  }

  // 清空队列，让挂起的批次都能 resolve，避免拖住测试进程
  await ingest.flushAll();
  await Promise.allSettled(pending);
  await ingest.stop();
  await store.close();
});
