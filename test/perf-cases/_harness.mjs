/**
 * 性能基准的公共装载逻辑。
 *
 * 每个基准用例都在自己的进程里跑（见 perf.test.js 的说明）：
 * LevelDB 后台压缩是异步的，上一个用例的压缩尾流会污染下一个用例的延迟统计。
 */
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import { Store } from '../../store.js';
import { Ingest } from '../../ingest.js';

export const BASE_TS = 1_700_000_000_000;

export function makeLedger(name) {
  const dir = path.join(os.tmpdir(), `obsledger-bench-${name}-${process.pid}-${Math.random().toString(36).slice(2)}`);
  fs.mkdirSync(dir, { recursive: true });
  const store = new Store({ path: path.join(dir, 'db') });
  return { store, dir };
}

export async function openLedger(name, ingestOpts = {}) {
  const { store, dir } = makeLedger(name);
  await store.open();
  const ingest = new Ingest(store, { flushIntervalMs: 5, maxBatchSize: 1000, ...ingestOpts });
  ingest.start();
  return { store, ingest, dir };
}

export const ev = (producer, seq, ts) => ({
  stream: 's1', producer, producerSeq: seq, kind: 'metric', level: 'info',
  region: 'cn', service: `svc-${seq % 20}`, tenant: `t${seq % 5}`, ts, value: seq, message: 'm',
});

/** 预热：排除 JIT 编译与块缓存冷启动。 */
export async function warmup(scan, times = 300) {
  for (let i = 0; i < times; i++) await scan(i);
}

/** 计算并输出分位数指标（JSON 一行，供父进程解析）。 */
export function report(lat) {
  lat.sort((a, b) => a - b);
  const q = (x) => Number(lat[Math.min(lat.length - 1, Math.floor(lat.length * x))].toFixed(3));
  return {
    n: lat.length,
    p50: q(0.5), p90: q(0.9), p99: q(0.99), p999: q(0.999),
    max: Number(lat[lat.length - 1].toFixed(2)),
    over5: lat.filter((x) => x > 5).length,
  };
}

export const ms = (t0) => Number(process.hrtime.bigint() - t0) / 1e6;
