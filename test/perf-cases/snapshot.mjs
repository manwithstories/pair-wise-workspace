/**
 * 基准：写入峰值期的快照读延迟。
 * 50 个生产者持续写入时，读者指定 LSN 取快照，延迟不应被写者显著拉高。
 */
import { openLedger, ev, warmup, report, ms, BASE_TS } from './_harness.mjs';

const WRITERS = 50;
const { store, ingest } = await openLedger('snapshot');

// 先铺一批存量，让查询有东西可读
for (let i = 0; i < 20_000; i += 1000) {
  await ingest.submit('s1', 'seed', Array.from({ length: 1000 }, (_, j) => ev('seed', i + j, BASE_TS + (i + j) * 10)));
}
await ingest.flushAll();

let writing = true;
const writers = Array.from({ length: WRITERS }, (_, p) =>
  (async () => {
    let seq = 0;
    while (writing) {
      await ingest.submit('s1', `w${p}`, Array.from({ length: 20 }, (_, j) => ev(`w${p}`, seq++, BASE_TS + (seq % 5000) * 10)));
      await new Promise((r) => setImmediate(r));
    }
  })(),
);

// 预热（写入持续进行中）
await warmup(async () => {
  const s = await store.openSnapshot(store.watermark, 's1');
  await store.scanRange(s, { fromTs: BASE_TS, toTs: BASE_TS + 5000, limit: 100, order: 'asc' });
  await s.close();
}, 100);

const lat = [];
for (let i = 0; i < 400; i++) {
  const snap = await store.openSnapshot(store.watermark, 's1');
  const t0 = process.hrtime.bigint();
  await store.scanRange(snap, { fromTs: BASE_TS, toTs: BASE_TS + 5000, limit: 100, order: 'asc' });
  lat.push(ms(t0));
  await snap.close();
}
writing = false;
await Promise.all(writers);
await ingest.stop();
await store.close();

process.stdout.write(`${JSON.stringify({ writers: WRITERS, ...report(lat) })}\n`);
