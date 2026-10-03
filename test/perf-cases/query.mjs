/** 基准：参数化范围查询延迟（要求 p99 < 5ms）。 */
import { openLedger, ev, warmup, report, ms, BASE_TS } from './_harness.mjs';

const { store, ingest } = await openLedger('query');
const N = 50_000;
for (let i = 0; i < N; i += 1000) {
  await ingest.submit('s1', 'bulk', Array.from({ length: 1000 }, (_, j) => ev('bulk', i + j, BASE_TS + (i + j) * 10)));
}
await ingest.flushAll();
const snap = await store.openSnapshot(store.watermark, 's1');

const scan = (from) => store.scanRange(snap, { fromTs: from, toTs: from + 500, limit: 100, order: 'asc' });
await warmup((i) => scan(BASE_TS + i * 370));

// p99 需要足够样本才有统计意义
const lat = [];
for (let i = 0; i < 2000; i++) {
  const from = BASE_TS + Math.floor(Math.random() * N) * 10;
  const t0 = process.hrtime.bigint();
  await scan(from);
  lat.push(ms(t0));
}
await snap.close();
await ingest.stop();
await store.close();

process.stdout.write(`${JSON.stringify(report(lat))}\n`);
