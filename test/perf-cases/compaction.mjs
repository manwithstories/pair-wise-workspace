/**
 * 基准：压缩对读写延迟的影响（要求 p99 恶化不超过基线 2 倍）。
 *
 * 测量方法上踩过几个坑，这里逐条规避：
 *
 *  1) 时间戳必须相对 now。保留期语义是"距今多久"，若样本比 now 早几个月，
 *     压缩会把整段全淘汰——那是"清空整库"的极端情况，不是稳态负载。
 *
 *  2) 过期数据与存活数据要在时间轴上明显分开，压缩才有确定的淘汰量。
 *
 *  3) 查询窗口必须对齐到真的有数据的时刻（事件按 1 秒粒度分布，
 *     500ms 窗口有一半概率落在空隙里）。
 *
 *  4) 被测的那一轮压缩必须有真实工作量。压缩一次就把过期数据清完了，
 *     第二次调用会 scanned=0，测出来的是"空转"，毫无意义。
 *     因此在被测轮次之前重新灌一批过期数据。
 *
 *  5) 基线要在底层 LSM 完全沉降后测（compactLsm），否则量到的是
 *     上一轮写入的压缩尾流，而不是稳定态读延迟。
 */
import { Store } from '../../store.js';
import { report, ms } from './_harness.mjs';
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';

const RETENTION = 3_600_000;
const NOW = Date.now();
const N = 60_000;
const EXPIRED_COUNT = 20_000;      // 1/3 过期
const FRESH_SPAN_S = 300;          // 存活数据分布在最近 300 秒
const STEP_MS = 1000;

const dir = path.join(os.tmpdir(), `obsledger-bench-compact-${process.pid}`);
fs.mkdirSync(dir, { recursive: true });
const store = new Store({ path: path.join(dir, 'db'), retentionMs: RETENTION });
await store.open();

const ev = (seq, ts) => ({
  stream: 's1', producer: 'bulk', producerSeq: seq, kind: 'metric', level: 'info',
  region: 'cn', service: 'svc', tenant: 't1', ts, value: seq, message: 'm',
});

const FRESH_FIRST_TS = NOW - (FRESH_SPAN_S - 1) * STEP_MS;
const tsFor = (seq) =>
  seq < EXPIRED_COUNT ? NOW - 2 * 3_600_000 : FRESH_FIRST_TS + ((seq - EXPIRED_COUNT) % FRESH_SPAN_S) * STEP_MS;

const writeAll = async () => {
  for (let i = 0; i < N; i += 1000) {
    const batch = [];
    for (let j = 0; j < 1000; j++) {
      const seq = i + j;
      batch.push(ev(seq, tsFor(seq)));
    }
    await store.appendBatch('s1', batch);
  }
};
await writeAll();

/** 短生命周期快照查询：与 HTTP 层每请求 open/close 快照的行为一致。 */
const queryOnce = async (from) => {
  const snap = await store.openSnapshot(store.watermark, 's1');
  try {
    await store.scanRange(snap, { fromTs: from, toTs: from + STEP_MS, limit: 100, order: 'asc' });
  } finally {
    await snap.close();
  }
};

const measure = async (n, salt) => {
  const lat = [];
  for (let i = 0; i < n; i++) {
    const offsetSec = Math.abs(((i + salt) * 7919) % FRESH_SPAN_S);
    const from = FRESH_FIRST_TS + offsetSec * STEP_MS;
    const t0 = process.hrtime.bigint();
    await queryOnce(from);
    lat.push(ms(t0));
  }
  return report(lat);
};

await measure(500, 0); // 预热

// 沉降：writeAll 触发的 LevelDB 压缩若仍在后台跑，基线会量到那条尾巴
// （实测 p99 在 6ms 与 22ms 之间跳动）。compactLsm 等它彻底结束。
await store.compactLsm();
await new Promise((r) => setTimeout(r, 300));

const SAMPLES = 3000;
const baseline = await measure(SAMPLES, 11);

// 被测轮次：重新灌一批过期数据，保证这一轮压缩确有工作量。
// 写完同样要沉降，否则量到的是写入引起的压缩，而非本次逻辑压缩的影响。
await writeAll();
await store.compactLsm();
await new Promise((r) => setTimeout(r, 300));

const compacting = store.maybeCompact(true);
const during = await measure(SAMPLES, 11);
const stats = await compacting;

await store.close();

process.stdout.write(
  `${JSON.stringify({
    baselineP50: baseline.p50,
    baselineP99: baseline.p99,
    duringP50: during.p50,
    duringP99: during.p99,
    ratio: Number((during.p99 / Math.max(baseline.p99, 0.001)).toFixed(2)),
    scanned: stats.eventsScanned,
    dropped: stats.eventsDropped,
    segmentsMerged: stats.segmentsMerged,
    withinBudget: stats.withinBudget,
  })}\n`,
);
