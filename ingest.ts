/**
 * ingest.ts — 并发摄入层：微批合流、背压、按生产者保序、窗口去重。
 *
 * 这一层解决的是"几十个微服务同时打进来"时的三个工程问题：
 *   1) 每条事件一次 fsync 会把吞吐压到几百/s —— 用微批窗口把多条合并成一次写。
 *   2) 无界合流会把内存吃光 —— 用 backlog 水位 + 拒绝新批（429）保证内存有界。
 *   3) 同一个生产者重试会写重复数据 —— 用 (producer, seq) 在窗口内做幂等去重。
 *
 * 保序保证：同一 producer 的批次在合流队列里串行处理，不同 producer 之间并发。
 * 因此单个生产者的 LSN 顺序与其 producerSeq 顺序一致，跨生产者则允许交错。
 */

import { Store } from './store.js';
import type { LedgerEvent } from './validate.js';
import { AppError } from './errors.js';

export interface IngestOptions {
  /** 合流窗口：最多等多久就强制落盘。 */
  flushIntervalMs?: number;
  /** 合流窗口内最多攒多少条，超了立刻落盘。 */
  maxBatchSize?: number;
  /** 队列中待落盘事件总数上限，超过则拒绝新批（背压）。 */
  maxBacklog?: number;
  /** 单窗口内保留多少个去重指纹（内存有界）。 */
  dedupWindowSize?: number;
}

interface PendingBatch {
  stream: string;
  producer: string;
  events: LedgerEvent[];
  /** 与 events 等长的幂等键槽。 */
  dedupKeys: Array<Buffer | null>;
  resolve: (r: { startLsn: number; endLsn: number; written: number; deduped: number }) => void;
  reject: (e: unknown) => void;
}

export interface IngestStats {
  queued: number;
  batchesInFlight: number;
  accepted: number;
  rejected: number;
  deduped: number;
  written: number;
  flushes: number;
}

export class Ingest {
  private readonly opts: Required<IngestOptions>;
  private readonly store: Store;

  /** 按流分桶的合流队列：一个流一个待落盘批次。 */
  private pending = new Map<string, PendingBatch>();
  private timer: NodeJS.Timeout | null = null;
  private inflight = new Map<string, Promise<void>>();
  private closed = false;

  /** 窗口内去重指纹：producer -> seq 集合。窗口滚动时整体重建，保证内存有界。 */
  private dedupSeen = new Map<string, Set<number>>();
  private dedupOrder: string[] = [];

  private stats: IngestStats = {
    queued: 0, batchesInFlight: 0, accepted: 0, rejected: 0, deduped: 0, written: 0, flushes: 0,
  };

  constructor(store: Store, options: IngestOptions = {}) {
    this.store = store;
    this.opts = {
      flushIntervalMs: options.flushIntervalMs ?? 5,
      maxBatchSize: options.maxBatchSize ?? 256,
      maxBacklog: options.maxBacklog ?? 50_000,
      dedupWindowSize: options.dedupWindowSize ?? 20_000,
    };
  }

  start(): void {
    if (this.timer) return;
    // 注意：这里不能 unref。合流窗口的落盘依赖这个定时器驱动，
    // 一旦 unref，进程可能在批次落盘前就退出——队列里的事件会静默丢失。
    // 服务的生命周期由 server 的 onClose / SIGTERM 显式收敛（ingest.stop()），
    // 不依赖事件循环自然退出。
    this.timer = setInterval(() => {
      void this.flushAll();
    }, this.opts.flushIntervalMs);
  }

  async stop(): Promise<void> {
    this.closed = true;
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
    await this.flushAll();
    // 等所有在途批次落完，保证 stop 返回时数据已持久化。
    await Promise.all([...this.inflight.values()]);
  }

  getStats(): IngestStats {
    return { ...this.stats, queued: this.stats.queued };
  }

  /**
   * 提交一批事件。返回该批的 LSN 区间（写入完成后才 resolve）。
   *
   * 背压：queued 超过 maxBacklog 直接抛 E_BACKPRESSURE（HTTP 429），
   * 且带 retryAfterMs，让上游自己退避而不是把内存撑爆。
   */
  async submit(stream: string, producer: string, events: LedgerEvent[]): Promise<{ startLsn: number; endLsn: number; written: number; deduped: number }> {
    if (this.closed) throw new AppError('E_SHUTTING_DOWN');
    // 背压：队列已达水位就立刻拒绝本批（HTTP 429），并告诉上游退避多久。
    // 关键：这里必须以「返回 rejected promise」而非「同步 throw」的形式失败，
    // 否则调用方写 .catch() 也会漏掉，错误会变成未捕获异常。
    if (this.stats.queued >= this.opts.maxBacklog) {
      this.stats.rejected++;
      return Promise.reject(new AppError('E_BACKPRESSURE', {
        details: {
          limit: this.opts.maxBacklog,
          actual: this.stats.queued,
          retryAfterMs: this.opts.flushIntervalMs * 4,
          reason: 'backlog_above_watermark',
        },
      }));
    }
    if (events.length === 0) {
      const lsn = this.store.watermark;
      return { startLsn: lsn, endLsn: lsn, written: 0, deduped: 0 };
    }

    // 同生产者保序：已有在途批次时必须排在其后。
    const key = `${stream}\u0000${producer}`;

    return new Promise((resolve, reject) => {
      let batch = this.pending.get(key);
      if (!batch) {
        batch = { stream, producer, events: [], dedupKeys: [], resolve, reject };
        this.pending.set(key, batch);
      } else {
        // 合流到同一批次：改写 resolve/reject 为链式，调用方共享最终结果。
        const prevResolve = batch.resolve;
        const prevReject = batch.reject;
        batch.resolve = (r) => { prevResolve(r); resolve(r); };
        batch.reject = (e) => { prevReject(e); reject(e); };
      }
      for (const ev of events) {
        batch.events.push(ev);
        batch.dedupKeys.push(null);
      }
      this.stats.queued += events.length;
      this.stats.accepted += events.length;

      // 达到合流上限立刻落盘，不等定时器。
      if (batch.events.length >= this.opts.maxBatchSize) {
        void this.flushKey(key);
      }
    });
  }

  /**
   * 窗口去重：把本批中 (producer, seq) 已见过的挑出来。
   * 命中的不进入写入，直接在结果里计入 deduped，实现重试幂等。
   */
  private applyWindowDedup(batch: PendingBatch): LedgerEvent[] {
    const producer = batch.producer;
    const windowKey = `${batch.stream}\u0000${producer}`;
    let seen = this.dedupSeen.get(windowKey);
    if (!seen) {
      seen = new Set<number>();
      this.dedupSeen.set(windowKey, seen);
      this.dedupOrder.push(windowKey);
    }
    const keep: LedgerEvent[] = [];
    let deduped = 0;

    for (const ev of batch.events) {
      if (seen.has(ev.producerSeq)) {
        deduped++;
        continue;
      }
      seen.add(ev.producerSeq);
      keep.push(ev);
    }

    if (deduped > 0) this.stats.deduped += deduped;

    // 内存有界：producer 数量超限时整体清空窗口（跨窗口重复会被 store 的
    // 持久化幂等键再挡一次，所以清空不会造成重复写入）。
    if (this.dedupOrder.length > this.opts.dedupWindowSize) {
      this.dedupSeen.clear();
      this.dedupOrder = [];
    }
    return keep;
  }

  /** 落盘一个 (stream, producer) 的待写批次。 */
  private async flushKey(key: string): Promise<void> {
    const batch = this.pending.get(key);
    if (!batch) return;
    this.pending.delete(key);
    // 同一 key 已有批次在途时排队，保证该生产者严格保序。
    const prev = this.inflight.get(key) ?? Promise.resolve();
    const task = prev.then(() => this.writeBatch(batch));
    this.inflight.set(key, task.catch(() => undefined));
    try {
      await task;
    } finally {
      if (this.inflight.get(key) === task.catch(() => undefined)) {
        this.inflight.delete(key);
      }
    }
  }

  private async writeBatch(batch: PendingBatch): Promise<void> {
    const events = this.applyWindowDedup(batch);
    this.stats.queued -= batch.events.length;
    if (events.length === 0) {
      this.stats.flushes++;
      const lsn = this.store.watermark;
      batch.resolve({ startLsn: lsn, endLsn: lsn, written: 0, deduped: batch.events.length });
      return;
    }
    this.stats.batchesInFlight++;
    try {
      const res = await this.store.appendBatch(batch.stream, events, {
        dedupKeys: events.map((e) => this.store.dedupKeyFor(batch.stream, batch.producer, e.producerSeq)),
      });
      this.stats.written += res.written;
      this.stats.flushes++;
      batch.resolve({
        startLsn: res.startLsn,
        endLsn: res.endLsn,
        written: res.written,
        deduped: res.deduped + (batch.events.length - events.length),
      });
    } catch (err) {
      batch.reject(err);
    } finally {
      this.stats.batchesInFlight--;
    }
  }

  /** 定时器触发：把当前所有待写批次并发落盘。 */
  async flushAll(): Promise<void> {
    if (this.pending.size === 0) return;
    const keys = [...this.pending.keys()];
    await Promise.all(keys.map((k) => this.flushKey(k).catch(() => undefined)));
  }
}
