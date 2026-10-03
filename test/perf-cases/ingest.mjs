/** 基准：微批摄入吞吐（要求 >= 2 万事件/秒）。 */
import { openLedger, ev, BASE_TS } from './_harness.mjs';

const { store, ingest } = await openLedger('ingest');
const PRODUCERS = 20;
const PER = 200;
const total = PRODUCERS * PER;

const t0 = process.hrtime.bigint();
await Promise.all(
  Array.from({ length: PRODUCERS }, (_, p) =>
    (async () => {
      for (let i = 0; i < PER; i += 50) {
        await ingest.submit('s1', `p${p}`, Array.from({ length: 50 }, (_, j) => ev(`p${p}`, i + j, BASE_TS + i + j)));
      }
    })(),
  ),
);
await ingest.flushAll();
const secs = Number(process.hrtime.bigint() - t0) / 1e9;

// 正确性：条数必须精确，否则吞吐数字没有意义
const snap = await store.openSnapshot(store.watermark, 's1');
const stored = (await store.replay(snap, total + 100)).length;
await snap.close();
await ingest.stop();
await store.close();

process.stdout.write(`${JSON.stringify({ total, secs, rate: total / secs, stored })}\n`);
if (stored !== total) process.exit(1);
