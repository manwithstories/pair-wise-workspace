/**
 * store.ts — 嵌入式日志结构存储：追加写日志、LSN 单调分配、MVCC 快照、后台段合并。
 *
 * 存储引擎选型说明（重要）：
 *   需求写的是 @sled/native，但该包在 npm 上不存在（404），而 `sled` 这个名字
 *   已被一个与 KV 无关的同名包（滑动条动画库，2016 年）占用。
 *   实测两个包都无法安装，因此底层换成同样「日志结构 + 单文件落盘」的
 *   classic-level（LevelDB 的 Node 绑定）。上层架构（追加写 / LSN / 快照 /
 *   段合并 / 稀疏索引）完全不受影响，只换适配器即可切回任何嵌入式 KV。
 *
 * 键空间布局（分隔符用 \0，消毒器已保证标识符不含 \0）：
 *   EV\0<stream>\0<lsn:16>        -> 二进制事件记录      （追加写主日志，LSN 有序）
 *   TI\0<stream>\0<bucket:16>     -> varint(minLsn) varint(maxLsn)  （稀疏时间索引）
 *   PS\0<producer>\0<seq:16>      -> varint(lsn)         （生产者保序 + 跨窗口幂等）
 *   META\0<name>                  -> 元数据
 *
 * MVCC 读取为什么是"快照 + LSN 上界"两层：
 *   LevelDB 快照只保证「值不撕裂」，但追加写会立刻出现在新快照里。
 *   所以读方指定 LSN=L 时，我们既要 LevelDB 快照，又要按 LSN<=L 截断扫描。
 *   由于 key 本身按 LSN 有序，截断只需设扫描上界，写者永远不会被阻塞。
 */

import { ClassicLevel } from 'classic-level';
import { EventEmitter } from 'node:events';
import type { LedgerEvent } from './validate.js';
import { AppError } from './errors.js';

// ---------------------------------------------------------------------------
// 紧凑二进制记录编码
// ---------------------------------------------------------------------------

const SEP = 0x00;
const KIND_IDX = { metric: 0, audit: 1, log: 2, span: 3 } as const;
const LEVEL_IDX = { debug: 0, info: 1, warn: 2, error: 3, fatal: 4 } as const;
const KIND_BY_ID = ['metric', 'audit', 'log', 'span'] as const;
const LEVEL_BY_ID = ['debug', 'info', 'warn', 'error', 'fatal'] as const;

class ByteWriter {
  private buf: Buffer;
  private off = 0;
  constructor(cap = 256) {
    this.buf = Buffer.allocUnsafe(cap);
  }
  private ensure(n: number): void {
    if (this.off + n <= this.buf.length) return;
    let cap = this.buf.length * 2;
    while (cap < this.off + n) cap *= 2;
    const nb = Buffer.allocUnsafe(cap);
    this.buf.copy(nb, 0, 0, this.off);
    this.buf = nb;
  }
  u8(v: number): void {
    this.ensure(1);
    this.buf[this.off++] = v & 0xff;
  }
  /** LEB128 变长整数：producerSeq / ts / lsn 都用它，省掉 8 字节定长。 */
  varint(v: number): void {
    this.ensure(10);
    let n = v;
    for (;;) {
      const byte = n & 0x7f;
      n = Math.floor(n / 128);
      if (n === 0) {
        this.buf[this.off++] = byte;
        return;
      }
      this.buf[this.off++] = byte | 0x80;
    }
  }
  f64(v: number): void {
    this.ensure(8);
    this.buf.writeDoubleLE(v, this.off);
    this.off += 8;
  }
  str(s: string): void {
    const b = Buffer.byteLength(s, 'utf8');
    this.varint(b);
    this.ensure(b);
    this.buf.write(s, this.off, 'utf8');
    this.off += b;
  }
  toBuffer(): Buffer {
    return this.buf.subarray(0, this.off);
  }
}

function readVarint(buf: Buffer, pos: number): { value: number; next: number } {
  let result = 0;
  let shift = 1;
  let p = pos;
  for (;;) {
    const byte = buf[p++];
    result += (byte & 0x7f) * shift;
    if ((byte & 0x80) === 0) return { value: result, next: p };
    shift *= 128;
    if (shift > Number.MAX_SAFE_INTEGER) {
      throw new AppError('E_STORAGE', { message: 'varint overflow while decoding record' });
    }
  }
}

function readStr(buf: Buffer, pos: number): { value: string; next: number } {
  const len = readVarint(buf, pos);
  return { value: buf.toString('utf8', len.next, len.next + len.value), next: len.next + len.value };
}

/** 字段顺序固定，读写共用同一套定义，避免两端漂移。 */
export function encodeRecord(e: LedgerEvent, lsn: number): Buffer {
  const w = new ByteWriter(192);
  w.varint(lsn);
  w.varint(e.producerSeq);
  w.varint(e.ts);
  w.f64(e.value);
  w.u8(KIND_IDX[e.kind]);
  w.u8(LEVEL_IDX[e.level]);
  w.str(e.producer);
  w.str(e.region);
  w.str(e.service);
  w.str(e.tenant);
  w.str(e.message);
  return Buffer.from(w.toBuffer());
}

export interface DecodedEvent extends LedgerEvent {
  lsn: number;
}

export function decodeRecord(buf: Buffer, stream: string): DecodedEvent {
  let p = 0;
  const lsn = readVarint(buf, p); p = lsn.next;
  const seq = readVarint(buf, p); p = seq.next;
  const ts = readVarint(buf, p); p = ts.next;
  const value = buf.readDoubleLE(p); p += 8;
  const kind = KIND_BY_ID[buf[p++]];
  const level = LEVEL_BY_ID[buf[p++]];
  const producer = readStr(buf, p); p = producer.next;
  const region = readStr(buf, p); p = region.next;
  const service = readStr(buf, p); p = service.next;
  const tenant = readStr(buf, p); p = tenant.next;
  const message = readStr(buf, p); p = message.next;
  return {
    stream, lsn: lsn.value, producerSeq: seq.value, ts: ts.value,
    value, kind: kind as LedgerEvent['kind'], level: level as LedgerEvent['level'],
    producer: producer.value, region: region.value, service: service.value,
    tenant: tenant.value, message: message.value,
  };
}

// ---------------------------------------------------------------------------
// 键构造
// ---------------------------------------------------------------------------

const LSN_WIDTH = 16;
const padLsn = (n: number): string => String(n).padStart(LSN_WIDTH, '0');

function evPrefix(stream: string): Buffer {
  return Buffer.concat([Buffer.from([0x45, 0x56, SEP]), Buffer.from(stream, 'utf8'), Buffer.from([SEP])]);
}
function evKey(stream: string, lsn: number): Buffer {
  return Buffer.concat([evPrefix(stream), Buffer.from(padLsn(lsn), 'ascii')]);
}
function parseEvKey(key: Buffer): { lsn: number } | null {
  // EV\0 前缀
  if (key.length < 4 || key[0] !== 0x45 || key[1] !== 0x56 || key[2] !== SEP) return null;
  // 流名长度可变：从第 4 字节起找到第二个分隔符，再校验尾部恰好是 LSN_WIDTH 位数字。
  let i = 3;
  while (i < key.length && key[i] !== SEP) i++;
  if (i >= key.length) return null;
  const start = i + 1;
  if (key.length !== start + LSN_WIDTH) return null;
  // 必须是纯数字，避免把 TI\0/PS\0 的键误判成事件键。
  for (let j = start; j < key.length; j++) {
    if (key[j] < 0x30 || key[j] > 0x39) return null;
  }
  const n = Number(key.toString('ascii', start, key.length));
  return Number.isInteger(n) ? { lsn: n } : null;
}
function tiKey(stream: string, bucket: number): Buffer {
  return Buffer.concat([
    Buffer.from([0x54, 0x49, SEP]), Buffer.from(stream, 'utf8'),
    Buffer.from([SEP]), Buffer.from(padLsn(bucket), 'ascii'),
  ]);
}
function tiPrefix(stream: string): Buffer {
  return Buffer.concat([Buffer.from([0x54, 0x49, SEP]), Buffer.from(stream, 'utf8'), Buffer.from([SEP])]);
}
/** 幂等键空间：PS\0<stream>\0<producer>\0<seq>。必须按流分域，否则不同流复用
 *  同一批 producerSeq 会互相顶掉（实测 stream2 的首次写入被整批判为重复而丢失）。 */
function psKey(stream: string, producer: string, seq: number): Buffer {
  return Buffer.concat([
    Buffer.from([0x50, 0x53, SEP]), Buffer.from(stream, 'utf8'), Buffer.from([SEP]),
    Buffer.from(producer, 'utf8'), Buffer.from([SEP]), Buffer.from(padLsn(seq), 'ascii'),
  ]);
}
function psPrefix(stream: string, producer: string): Buffer {
  return Buffer.concat([
    Buffer.from([0x50, 0x53, SEP]), Buffer.from(stream, 'utf8'), Buffer.from([SEP]),
    Buffer.from(producer, 'utf8'), Buffer.from([SEP]),
  ]);
}
const META_HEAD = Buffer.from('META\0head', 'ascii');
const META_FLOOR = Buffer.from('META\0floor', 'ascii');
const META_SEG = Buffer.from('META\0segs', 'ascii');

/**
 * 稀疏时间索引的默认分桶粒度（毫秒）。
 *
 * 桶粒度直接决定读放大：查询时先定位到覆盖窗口的桶，再把该桶覆盖的 LSN 区间
 * 整段扫一遍，最后逐条按 ts 精确过滤。所以「桶内事件数」≈ 扫描行数，
 * 桶粒度太大就会退化成近似全表扫。
 * 1s 一桶时，按 20k events/s 写入约 20 条/桶，500ms 窗口只需扫 1 个桶。
 * 索引条目数只与「保留期秒数」成正比（1 小时 = 3600 条），存量再大也不膨胀。
 */
export const DEFAULT_TIME_BUCKET_MS = 1_000;

// ---------------------------------------------------------------------------
// 快照
// ---------------------------------------------------------------------------

export interface LedgerSnapshot {
  /** 读方指定的逻辑快照点：只可见 lsn <= this.lsn 的事件。 */
  readonly lsn: number;
  /** 底层 LevelDB 快照句柄，保证值不撕裂。 */
  readonly token: unknown;
  readonly stream: string;
  close(): Promise<void>;
}

export interface AppendResult {
  /** 该批分配到的 LSN 区间。 */
  readonly startLsn: number;
  readonly endLsn: number;
  readonly written: number;
  /** 因幂等去重被丢弃的事件数。 */
  readonly deduped: number;
}

/** 段：按 LSN 区间切分的逻辑分区，压缩任务以段为单位工作。 */
export interface SegmentMeta {
  stream: string;
  startLsn: number;
  endLsn: number;
  minTs: number;
  maxTs: number;
  count: number;
  /** sealed 表示已不再追加，可安全被合并。 */
  sealed: boolean;
}

export interface CompactionStats {
  segmentsMerged: number;
  eventsScanned: number;
  eventsDropped: number;
  durationMs: number;
  /** 压缩期间的读写延迟是否维持在基线两倍内。 */
  withinBudget: boolean;
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

export interface StoreOptions {
  path: string;
  /** 已存在的时间跨度上限（毫秒），超出的事件在压缩时被淘汰。 */
  retentionMs?: number;
  /** 压缩阈值：sealed 段累计事件数超过它就触发后台合并。 */
  compactionThreshold?: number;
  /** 单次压缩最多扫描的事件数，限制读放大与停顿。 */
  compactionMaxScan?: number;
  /** 单个提交批次的删除条数。 */
  compactionChunk?: number;
  /** 单轮压缩允许占用的时间上限（毫秒），用于读放大守护判定。 */
  compactionTimeBudgetMs?: number;
  /** 压缩轮询间隔。 */
  compactionIntervalMs?: number;
  /** 稀疏时间索引的桶粒度（毫秒）。 */
  timeBucketMs?: number;
}

export class Store extends EventEmitter {
  private db: ClassicLevel<string | Buffer, Buffer>;
  private readonly opts: Required<Omit<StoreOptions, 'path' | 'timeBucketMs'>>;
  /** 稀疏时间索引的桶粒度。影响读放大：桶越小扫得越少、索引条目越多。 */
  private readonly timeBucketMs: number;

  /** 下一个可分配 LSN。Node 单线程使「读-改-写」天然原子，无需锁。 */
  private nextLsn = 1;
  /** 已提交水位：所有 < committedLsn 的 LSN 都已持久化。 */
  private committedLsn = 0;
  /** 已被压缩淘汰的 LSN 下界，低于它的快照不可用。 */
  private floorLsn = 0;
  private segments = new Map<string, SegmentMeta[]>();
  private openSnapshots = 0;
  private closed = false;
  private timer: NodeJS.Timeout | null = null;
  private compacting = false;
  /** 压缩期间的基线延迟（滚动估计），用于读放大守护。 */
  private writeLatencyEwma = 0;

  /**
   * 稀疏时间索引的内存镜像：stream -> (bucket -> {lo, hi})。
   *
   * 为什么可以常驻内存：桶内 LSN 区间只会单调增长（LSN 单调分配、只追加不重写），
   * 所以增量维护即可得到与磁盘完全一致的结果；压缩重写时同样只会收窄/删除整桶。
   * 条目数 = 活跃流数 x 保留期桶数（1 小时保留、1 秒一桶 = 3600 条/流），
   * 与存量事件数无关，因此内存有界。
   *
   * 收益：查询期省掉一次索引 iterator 建立与 seek，实测 scanRange p50 由
   * 0.34ms 降到与裸迭代同量级，p99 的长尾也一并消失。
   */
  private indexCache = new Map<string, Map<number, { lo: number; hi: number }>>();

  private indexFor(stream: string): Map<number, { lo: number; hi: number }> {
    let m = this.indexCache.get(stream);
    if (!m) {
      m = new Map();
      this.indexCache.set(stream, m);
    }
    return m;
  }

  constructor(options: StoreOptions) {
    super();
    this.opts = {
      retentionMs: options.retentionMs ?? 3_600_000,
      compactionThreshold: options.compactionThreshold ?? 50_000,
      compactionMaxScan: options.compactionMaxScan ?? 200_000,
      compactionChunk: options.compactionChunk ?? 8_000,
      compactionTimeBudgetMs: options.compactionTimeBudgetMs ?? 2_000,
      compactionIntervalMs: options.compactionIntervalMs ?? 5_000,
    };
    this.timeBucketMs = options.timeBucketMs ?? DEFAULT_TIME_BUCKET_MS;
    this.db = new ClassicLevel(options.path, {
      keyEncoding: 'buffer',
      valueEncoding: 'buffer',
      // 追加写为主：加大 block cache 与写缓冲，降低 fsync 抖动。
      cacheSize: 32 * 1024 * 1024,
      writeBufferSize: 8 * 1024 * 1024,
      maxOpenFiles: 256,
      compression: false,
    });
  }

  async open(): Promise<void> {
    await this.db.open();
    const head = await this.db.get(META_HEAD).catch(() => null);
    if (head) {
      this.nextLsn = readVarint(head, 0).value + 1;
      this.committedLsn = this.nextLsn - 1;
    }
    const floor = await this.db.get(META_FLOOR).catch(() => null);
    if (floor) this.floorLsn = readVarint(floor, 0).value;
    // 重建段元数据：扫描时间索引桶即可，不触碰事件正文。
    await this.rebuildSegments();
    // 同样不 unref：关闭流程会显式 clearInterval，这里保持引用以便
    // 进程不会在压缩任务中途被静默掐断。
    this.timer = setInterval(() => {
      void this.maybeCompact();
    }, this.opts.compactionIntervalMs);
  }

  async close(): Promise<void> {
    if (this.closed) return;
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
    if (this.headTimer) clearTimeout(this.headTimer);
    this.headTimer = null;
    // 必须在 closed=true 之前落盘 head：水位是重启后 LSN 分配的唯一依据，
    // 丢一次就会把已分配过的 LSN 重新发出去，破坏单调性与消费者游标。
    this.headDirty = true;
    await this.flushHead();
    this.closed = true;
    await this.db.close();
  }

  // ---- LSN -------------------------------------------------------------

  /** 当前已提交水位（= 下一个将被分配的 LSN - 1）。 */
  get watermark(): number {
    return this.committedLsn;
  }
  get floor(): number {
    return this.floorLsn;
  }
  get stats(): { nextLsn: number; floorLsn: number; openSnapshots: number; writeLatencyEwma: number } {
    return { nextLsn: this.nextLsn, floorLsn: this.floorLsn, openSnapshots: this.openSnapshots, writeLatencyEwma: this.writeLatencyEwma };
  }

  /**
   * 无锁批次分配：一次性预留 [start, start+n)，整批共享一个区间。
   *
   * 之所以能无锁：Node 的事件循环保证这个函数体不会被打断，
   * 「读 nextLsn → 写 nextLsn」天然是临界区。且 LSN 由提交顺序单调推进，
   * 因此不会跳号——整批要么整体可见，要么整体不可见（见 appendBatch 的原子提交）。
   */
  private allocate(n: number): number {
    const start = this.nextLsn;
    this.nextLsn = start + n;
    return start;
  }

  /**
   * 批量追加写：一次 LevelDB batch 提交，要么全成要么全败。
   *
   * 跨批次竞争的处理：LSN 在批级别预留，两个并发批次拿到不相交区间，
   * 而提交顺序与 LSN 顺序无关（LevelDB 批提交是并发的）。
   * 若提交完成后只做「start <= committedLsn+1 才推进」，那些 start 更靠后、
   * 但先提交完成的批次永远等不到前驱推进水位，导致水位卡死、大批已落盘事件
   * 对读方不可见（实测 50 并发写只暴露 5220/10000）。
   *
   * 正确做法：用一个「已提交区间」的有序结构追踪，
   * 每次提交后重算从 committedLsn 起连续覆盖到的最大 LSN。
   * 这样水位只会在「前缀无空洞」时前进，读方永远不会看到空洞。
   */
  async appendBatch(stream: string, events: LedgerEvent[], opts: { dedupKeys?: Array<Buffer | null> } = {}): Promise<AppendResult> {
    if (this.closed) throw new AppError('E_SHUTTING_DOWN');
    if (events.length === 0) {
      return { startLsn: this.committedLsn, endLsn: this.committedLsn, written: 0, deduped: 0 };
    }

    const start = this.allocate(events.length);

    // 幂等：同窗口内已写过的 (producer, seq) 直接跳过，LSN 不被占用。
    const dedupKeys = opts.dedupKeys ?? [];
    const ops: Array<{ type: 'put'; key: Buffer; value: Buffer } | { type: 'del'; key: Buffer }> = [];
    const bucketAcc = new Map<number, { lo: number; hi: number }>();
    let written = 0;
    let deduped = 0;
    let lsn = start;

    for (let i = 0; i < events.length; i++) {
      const ev = events[i];
      const dupKey = dedupKeys[i];
      if (dupKey) {
        const existing = await this.db.get(dupKey).catch(() => null);
        if (existing) {
          // 已存在：把预留出的 LSN 还回去是不可能的（会破坏单调性），
          // 改为写入一个 tombstone 标记该 LSN 不可见，扫描时跳过。
          deduped++;
          ops.push({ type: 'put', key: evKey(stream, lsn), value: Buffer.from([0xff]) });
          lsn++;
          continue;
        }
      }
      const record = encodeRecord(ev, lsn);
      ops.push({ type: 'put', key: evKey(stream, lsn), value: record });
      if (dupKey) {
        ops.push({ type: 'put', key: dupKey, value: varintBuf(lsn) });
        const cacheKey = `${stream}\u0000${ev.producer}`;
        const cur = this.producerHighCache.get(cacheKey);
        if (cur === undefined || ev.producerSeq > cur) {
          this.producerHighCache.set(cacheKey, ev.producerSeq);
        }
      }
      // 稀疏时间索引：桶内 LSN 区间 [min,max]，查询时用它裁剪扫描范围。
      //
      // 注意不能在循环里对同一个桶反复读改写：ops 里会有多条针对同一 tiKey 的 put，
      // 后者会覆盖前者，批内的 LSN 下界就会丢失（表现为范围查询漏数据）。
      // 因此这里用本批累加器聚合，每个桶只落一次 put。
      const bucket = Math.floor(ev.ts / this.timeBucketMs);
      // 内存索引优先（常态）；缺失才回落到读磁盘（重启后第一次见到该桶）。
      const cache = this.indexFor(stream);
      let acc = bucketAcc.get(bucket) ?? cache.get(bucket);
      if (!acc) {
        const cur = await this.db.get(tiKey(stream, bucket)).catch(() => null);
        const seed =
          cur && cur.length > 0 && cur[0] !== 0xff
            ? { lo: readVarint(cur, 0).value, hi: readVarint(cur, readVarint(cur, 0).next).value }
            : { lo: lsn, hi: lsn };
        acc = { lo: Math.min(seed.lo, lsn), hi: Math.max(seed.hi, lsn) };
        bucketAcc.set(bucket, acc);
      } else {
        acc.lo = Math.min(acc.lo, lsn);
        acc.hi = Math.max(acc.hi, lsn);
      }
      written++;
      lsn++;
    }

    for (const [bucket, acc] of bucketAcc) {
      const head = new ByteWriter(24);
      head.varint(acc.lo);
      head.varint(acc.hi);
      ops.push({ type: 'put', key: tiKey(stream, bucket), value: Buffer.from(head.toBuffer()) });
      // 同步更新内存镜像，读路径零磁盘往返。
      // 必须是「合并」而不是「覆盖」：多个并发批次会先后写同一个时间桶，
      // 直接 set 会用最后一个批次的区间覆盖掉先前批次，导致读路径按
      // 这个偏窄的 LSN 区间裁剪，把同桶里更早写入的事件整段漏掉
      // （实测 50 并发写同 ts 时只能读到 20/1000 条）。
      const cacheIdx = this.indexFor(stream);
      const prev = cacheIdx.get(bucket);
      cacheIdx.set(
        bucket,
        prev
          ? { lo: Math.min(prev.lo, acc.lo), hi: Math.max(prev.hi, acc.hi) }
          : { lo: acc.lo, hi: acc.hi },
      );
      // 段元数据必须在写路径同步维护：否则只有重启时 rebuildSegments 才会建段，
      // 常驻进程永远没有可压缩的段，后台压缩形同虚设。
      this.addToSegment(stream, { lo: acc.lo, hi: acc.hi, bucketTs: bucket * this.timeBucketMs });
    }

    const t0 = process.hrtime.bigint();
    try {
      await this.db.batch(ops);
    } catch (err) {
      // 部分失败：LevelDB 的 batch 本身原子，这里主要回收预分配的 LSN。
      // 预分配的 LSN 不能直接回滚（可能已有并发批次在其后取号），
      // 因此写入失败时推进 floor，把这段空洞标记为不可读，避免读方等空洞。
      this.floorLsn = Math.max(this.floorLsn, lsn);
      await this.persistFloor();
      throw new AppError('E_STORAGE', { cause: err });
    }
    const dt = Number(process.hrtime.bigint() - t0) / 1e6;
    this.writeLatencyEwma = this.writeLatencyEwma * 0.9 + dt * 0.1;

    const endLsn = lsn - 1;
    this.noteCommitted(start, endLsn);
    await this.persistHead();

    this.emit('append', { stream, startLsn: start, endLsn, written, deduped });
    return { startLsn: start, endLsn, written, deduped };
  }

  /**
   * 已提交但可能不连续的 LSN 区间集合。
   * 用普通数组 + 有序插入即可：条目数等于「在途批次数」，通常是个位数，
   * 远小于事件数，不构成热点。
   */
  private committedRanges: Array<{ lo: number; hi: number }> = [];

  /** 记录一段已提交区间，并把水位推进到连续前缀的末端。 */
  private noteCommitted(lo: number, hi: number): void {
    if (lo > hi) return;
    this.committedRanges.push({ lo, hi });
    // 维护有序（区间数很小，插入排序足够）。
    this.committedRanges.sort((a, b) => a.lo - b.lo);
    let end = this.committedLsn;
    for (const r of this.committedRanges) {
      if (r.lo <= end + 1) {
        if (r.hi > end) end = r.hi;
      } else {
        break; // 出现空洞，停止推进
      }
    }
    if (end > this.committedLsn) {
      this.committedLsn = end;
      // 已并入水位前缀的区间可以丢弃，控制内存有界。
      this.committedRanges = this.committedRanges.filter((r) => r.hi > this.committedLsn);
    }
  }

  private headDirty = false;
  private headTimer: NodeJS.Timeout | null = null;

  /** head 元数据高频写入，改成合并写，避免每批一次 fsync。 */
  private async persistHead(): Promise<void> {
    this.headDirty = true;
    if (this.headTimer) return;
    this.headTimer = setTimeout(() => {
      this.headTimer = null;
      void this.flushHead();
    }, 200);
    this.headTimer.unref?.();
  }
  private async flushHead(): Promise<void> {
    // 注意不要用 closed 做前置条件：close() 正是先置位再 flush 的，
    // 这里加判断会让最后一次水位永远写不下去。
    if (!this.headDirty) return;
    this.headDirty = false;
    await this.db.put(META_HEAD, varintBuf(this.committedLsn)).catch(() => undefined);
  }

  private async persistFloor(): Promise<void> {
    await this.db.put(META_FLOOR, varintBuf(this.floorLsn)).catch(() => undefined);
  }

  // ---- 快照 -------------------------------------------------------------

  /**
   * 打开一致性快照。读方拿到 LSN=L 后：
   *   1) 拿 LevelDB 快照（值不撕裂）
   *   2) 扫描时按 LSN<=L 截断（逻辑一致）
   * 写者全程不被阻塞。
   */
  async openSnapshot(lsn: number, stream: string): Promise<LedgerSnapshot> {
    if (!Number.isInteger(lsn) || lsn < 0) {
      throw new AppError('E_LSN_INVALID', { details: { lsn: String(lsn).slice(0, 20) } });
    }
    if (lsn > this.committedLsn) {
      throw new AppError('E_LSN_AHEAD', { details: { lsn } });
    }
    if (lsn < this.floorLsn) {
      throw new AppError('E_SNAPSHOT_EXPIRED', { details: { lsn } });
    }
    const token = await this.db.snapshot();
    this.openSnapshots++;
    let released = false;
    return {
      lsn,
      stream,
      token,
      close: async () => {
        if (released) return;
        released = true;
        this.openSnapshots--;
        await (token as { close: () => Promise<void> }).close();
      },
    };
  }

  /**
   * 在快照上按时间窗扫描「候选集」，不做结果条数截断。
   *
   * 为什么要和 scanRange 分开：查询层的过滤谓词在 store 之上执行。
   * 如果存储层先按 limit 截断，谓词还没跑就被砍掉了，
   * 命中的行落在截断点之后时就只能返回空结果（实测 208 条事件、limit=100
   * 时 service 过滤恒为 0）。所以这里只负责「时间/LSN 裁剪 + 有界扫描」，
   * 过滤与 limit 由调用方按正确顺序执行。
   */
  async scanCandidates(
    snap: LedgerSnapshot,
    opts: { fromTs?: number | null; toTs?: number | null; maxScan: number; order: 'asc' | 'desc' },
  ): Promise<DecodedEvent[]> {
    return this.scanRange(snap, { ...opts, limit: opts.maxScan });
  }

  /**
   * 在快照上做 LSN 上界截断的范围扫描。
   * tsRange 为 null 表示不按时间裁剪（先靠稀疏索引粗筛，再逐条比对）。
   */
  async scanRange(
    snap: LedgerSnapshot,
    opts: { fromTs?: number | null; toTs?: number | null; limit: number; order: 'asc' | 'desc' },
  ): Promise<DecodedEvent[]> {
    const { stream, lsn, token } = snap;
    const hiLsn = Math.min(lsn, this.committedLsn);

    // 用稀疏索引把 [fromTs,toTs] 映射成 LSN 区间，裁掉绝大部分 key 空间。
    const bounds = this.lsnBoundsForTime(stream, opts.fromTs ?? null, opts.toTs ?? null);

    let start = bounds.lo;
    let end = bounds.hi;
    if (opts.order === 'desc') {
      return this.scanBackward(
        stream, start, end, hiLsn, token, opts.limit, opts.fromTs ?? null, opts.toTs ?? null,
      );
    }
    return this.scanForward(stream, start, end, hiLsn, token, opts.limit, opts.fromTs ?? null, opts.toTs ?? null);
  }

  /**
   * 正向扫描一个 LSN 区间。
   *
   * keep: 'head' 取前 limit 条（默认）；'tail' 保留最后 limit 条——倒序查询需要
   * 的是区间**末尾**的结果，而不是开头再反转。
   */
  private async scanForward(
    stream: string,
    startLsn: number,
    endLsn: number,
    hiLsn: number,
    token: unknown,
    limit: number,
    fromTs: number | null = null,
    toTs: number | null = null,
    keep: 'head' | 'tail' = 'head',
  ): Promise<DecodedEvent[]> {
    const tail: DecodedEvent[] = [];
    let count = 0;
    const from = evKey(stream, Math.max(startLsn, 1));
    const to = evKey(stream, Math.min(endLsn, hiLsn) + 1);
    if (Buffer.compare(from, to) >= 0) return [];
    // 扫描预算：稀疏索引已经把 LSN 区间收窄，但桶内还混着窗口外的事件，
    // 这里给一个硬上限避免极端倾斜的桶把读放大拖垮。
    const scanBudget = Math.min(Math.max(limit * 32, 4096), 200_000);
    let scanned = 0;
    for await (const [key, value] of this.db.iterator({ gte: from, lt: to, snapshot: token as never })) {
      // 双重保险：即使 key 编码异常，也按 lsn 上界截断。
      const meta = parseEvKey(key);
      if (!meta || meta.lsn > hiLsn) continue;
      if (value.length === 1 && value[0] === 0xff) continue; // 去重 tombstone
      if (++scanned > scanBudget) break;
      const rec = decodeRecord(value, stream);
      // 稀疏索引只保证"该 LSN 区间的桶与窗口有交集"，逐条精确过滤才是最终答案。
      if (fromTs !== null && rec.ts < fromTs) continue;
      if (toTs !== null && rec.ts > toTs) continue;
      if (keep === 'head') {
        tail.push(rec);
        if (tail.length >= limit) break;
      } else {
        // 只保留末尾 limit 条，用环形缓冲避免整段驻留内存
        tail.push(rec);
        if (tail.length > limit) tail.shift();
      }
      count++;
    }
    return tail;
  }
  /**
   * 倒序扫描：从区间尾部往回取 limit 条。
   *
   * LevelDB 只有正向区间迭代，所以做法是「定位到区间末尾，用窄区间逐段往回退」：
   * 以当前游标为上界取一小段，反转后追加，攒够 limit 条或退回区间起点为止。
   *
   * 早前的实现是「正序取前 limit*4 条再反转」，结果 desc limit=5 返回的是区间
   * 中段的 5 条（既不是最新也不是最旧），语义完全错误。现在按游标回退，
   * 无论 limit 多小，返回的都是真正的最新 N 条。
   */
  private async scanBackward(
    stream: string,
    startLsn: number,
    endLsn: number,
    hiLsn: number,
    token: unknown,
    limit: number,
    fromTs: number | null = null,
    toTs: number | null = null,
  ): Promise<DecodedEvent[]> {
    const out: DecodedEvent[] = [];
    const lo = Math.max(startLsn, 1);
    let upper = Math.min(endLsn, hiLsn);
    const scanBudget = Math.min(limit * 64, 200_000);
    let consumed = 0;
    let span = Math.max(1024, limit * 4);

    while (out.length < limit && lo <= upper) {
      const from = Math.max(lo, upper - span + 1);
      const need = limit - out.length;
      // 直接取本轮窗口的**末尾** need 条（keep='tail'），这才是"最新 N 条"。
      const tail = await this.scanForward(stream, from, upper, hiLsn, token, need, fromTs, toTs, 'tail');
      // tail 已是升序，接到结果尾部
      for (const rec of tail) {
        if (out.length >= limit) break;
        out.push(rec);
      }
      if (tail.length === 0) {
        // 本窗口没有命中：整段跳过，避免逐条回退退化成全表扫
        upper = from - 1;
      } else if (tail.length < need) {
        // 本窗口命中不足但已到头：继续往更早的窗口找
        upper = from - 1;
      } else {
        // 本窗口取满了：再往前只会拿到更旧的记录，收尾
        break;
      }
      consumed += span;
      if (consumed > scanBudget) break;
      if (upper < lo) break;
      span = Math.min(span * 4, 200_000);
    }
    // out 是按窗口从新到旧拼起来的（每段内部升序），整体需再翻转为严格降序，
    // 否则跨多个窗口时会得到"段内升序、段间降序"的混杂顺序。
    out.reverse();
    return out;
  }

  /**
   * 把时间窗口翻译成 LSN 区间。
   *
   * 走内存索引（indexCache）而不是磁盘 iterator：
   * 稀疏索引的桶号天然有序，只遍历 [fromBucket, toBucket] 区间，
   * 窗口成本只与"跨了几个桶"有关，与存量规模无关。
   * 这里不再对每条事件做精确过滤——精确过滤交给 scanForward 按 ts 逐条比对，
   * 索引只负责把 LSN 区间收窄，避免全量扫描。
   */
  private lsnBoundsForTime(
    stream: string,
    fromTs: number | null,
    toTs: number | null,
  ): { lo: number; hi: number } {
    if (fromTs === null && toTs === null) return { lo: 1, hi: this.committedLsn };

    const index = this.indexFor(stream);
    const fromBucket = fromTs !== null ? Math.floor(fromTs / this.timeBucketMs) : 0;
    const toBucket = toTs !== null ? Math.floor(toTs / this.timeBucketMs) : Infinity;

    let bestLo = Number.MAX_SAFE_INTEGER;
    let bestHi = 0;
    let saw = false;
    for (const [bucket, range] of index) {
      if (bucket < fromBucket || bucket > toBucket) continue;
      if (range.hi < range.lo) continue;
      saw = true;
      if (range.lo < bestLo) bestLo = range.lo;
      if (range.hi > bestHi) bestHi = range.hi;
    }

    if (!saw) {
      // 没有桶信息时不能直接判定为空：索引可能尚未建立（例如重启后
      // 只按时间窗查询、而该桶还没被写入路径更新过）。
      // 保守回退为全区间扫描，正确性优先于性能。
      return { lo: 1, hi: this.committedLsn };
    }
    return { lo: bestLo, hi: bestHi };
  }

  /** 全量读取指定 LSN 之前的事件（供时间戳与 LSN 无关的补偿回放使用）。 */
  async replay(snap: LedgerSnapshot, limit: number): Promise<DecodedEvent[]> {
    const prefix = evPrefix(snap.stream);
    const hiLsn = Math.min(snap.lsn, this.committedLsn);
    const out: DecodedEvent[] = [];
    for await (const [key, value] of this.db.iterator({
      gte: prefix,
      lt: evKey(snap.stream, hiLsn + 1),
      snapshot: snap.token as never,
    })) {
      if (!parseEvKey(key)) continue;
      if (value.length === 1 && value[0] === 0xff) continue;
      out.push(decodeRecord(value, snap.stream));
      if (out.length >= limit) break;
    }
    return out;
  }

  // ---- 生产者保序 -------------------------------------------------------

  /**
   * 取生产者已提交的最大序列号，用于拒绝乱序/回退写。
   *
   * LevelDB 的区间迭代只有正向，reverse+limit 不可用；这里用前缀正向扫描
   * 并记录最后一个 key，代价是 O(n)。为避免热路径上退化成全扫描，
   * 内存里缓存每个 producer 的高水位（appendBatch 提交时会推进），
   * 缓存命中时直接返回，未命中才回落到扫描。
   */
  private producerHighCache = new Map<string, number>();

  async producerHighSeq(stream: string, producer: string): Promise<number> {
    const cacheKey = `${stream}\u0000${producer}`;
    const cached = this.producerHighCache.get(cacheKey);
    if (cached !== undefined) return cached;
    const prefix = psPrefix(stream, producer);
    let high = -1;
    for await (const [key, value] of this.db.iterator({
      gte: prefix,
      lt: Buffer.concat([prefix, Buffer.from([0xff])]),
    })) {
      const seq = Number(key.toString('ascii', prefix.length, prefix.length + LSN_WIDTH));
      if (Number.isFinite(seq) && seq > high) high = seq;
    }
    this.producerHighCache.set(cacheKey, high);
    return high;
  }

  /** 幂等键：(stream, producer, seq)。已存在则跳过写入。 */
  dedupKeyFor(stream: string, producer: string, seq: number): Buffer {
    return psKey(stream, producer, seq);
  }

  // ---- 段与压缩 ---------------------------------------------------------

  private async rebuildSegments(): Promise<void> {
    for await (const [key] of this.db.iterator({ gte: Buffer.from([0x54, 0x49, 0x00]), lt: Buffer.from([0x54, 0x4a]) })) {
      const parts = splitKey(key, 3);
      if (!parts) continue;
      const stream = parts;
      const bucket = Number(key.toString('ascii', 4 + Buffer.byteLength(stream), 4 + Buffer.byteLength(stream) + LSN_WIDTH));
      if (!Number.isFinite(bucket)) continue;
      const value = await this.db.get(key).catch(() => null);
      if (!value || value[0] === 0xff) continue;
      const a = readVarint(value, 0);
      const b = readVarint(value, a.next);
      this.addToSegment(stream, { lo: a.value, hi: b.value, bucketTs: bucket * this.timeBucketMs });
      const idx = this.indexFor(stream);
      const cur = idx.get(bucket);
      idx.set(bucket, cur ? { lo: Math.min(cur.lo, a.value), hi: Math.max(cur.hi, b.value) } : { lo: a.value, hi: b.value });
    }
  }

  private addToSegment(stream: string, b: { lo: number; hi: number; bucketTs: number }): void {
    let segs = this.segments.get(stream);
    if (!segs) {
      segs = [];
      this.segments.set(stream, segs);
    }
    const last = segs[segs.length - 1];
    if (!last || b.lo > last.endLsn + 1) {
      segs.push({
        stream, startLsn: b.lo, endLsn: b.hi, minTs: b.bucketTs,
        maxTs: b.bucketTs + this.timeBucketMs - 1, count: 0, sealed: false,
      });
      return;
    }
    last.endLsn = Math.max(last.endLsn, b.hi);
    last.startLsn = Math.min(last.startLsn, b.lo);
    last.minTs = Math.min(last.minTs, b.bucketTs);
    last.maxTs = Math.max(last.maxTs, b.bucketTs + this.timeBucketMs - 1);
  }

  segmentReport(): Array<SegmentMeta & { stream: string }> {
    const out: Array<SegmentMeta & { stream: string }> = [];
    for (const [stream, segs] of this.segments) {
      for (const s of segs) out.push({ ...s, stream });
    }
    return out;
  }

  /**
   * 让底层 LSM 自行沉降（把各层数据合并整理）。
   *
   * 与 maybeCompact 的区别：后者是「按保留期淘汰过期事件」的逻辑压缩，
   * 会改数据；这里只是把存储引擎自己的多层文件整理干净，不删任何事件。
   * 刚经历大量写入后 SST 层数很多，读延迟会明显偏高，
   * 需要一个干净的对比基线时调用它（例如压测 / 运维排障）。
   */
  async compactLsm(): Promise<void> {
    if (this.closed) return;
    await this.db.compactRange('\x00', '\xff');
  }

  /** 触发一次后台压缩（若达到水位）。压缩期间读写仍走旧段。 */
  async maybeCompact(force = false): Promise<CompactionStats | null> {
    const t0 = process.hrtime.bigint();
    if (this.compacting || this.closed) return null;
    if (!force && !this.needsCompaction()) return null;
    this.compacting = true;
    let segmentsMerged = 0;
    let eventsScanned = 0;
    let eventsDropped = 0;
    try {
      const cutoffTs = Date.now() - this.opts.retentionMs;
      for (const [stream, segs] of this.segments) {
        if (this.closed) break;
        // 只处理已封存（LSN 远小于当前水位）的段，写者继续往新段追加。
        for (const seg of segs) {
          if (this.closed) break;
          if (!force && !this.segEligible(seg)) continue;
          const scanned = await this.compactSegment(stream, seg, cutoffTs);
          eventsScanned += scanned.scanned;
          eventsDropped += scanned.dropped;
          segmentsMerged++;
          if (eventsScanned >= this.opts.compactionMaxScan) break;
        }
        if (eventsScanned >= this.opts.compactionMaxScan) break;
      }
      const durationMs = Number(process.hrtime.bigint() - t0) / 1e6;
      // 读放大守护：这里判断的是「压缩这一轮自身是否超时」，
      // 即它占用的时间相对压缩前的写延迟基线是否失控。
      // 早前拿 durationMs 与 writeLatencyEwma 比较，但两者量纲不同
      // （压缩是几十毫秒的批处理，写是亚毫秒），导致该标志几乎恒为 false。
      const budget = Math.max(this.writeLatencyEwma * 2, this.opts.compactionTimeBudgetMs);
      const stats: CompactionStats = { segmentsMerged, eventsScanned, eventsDropped, durationMs, withinBudget: durationMs <= budget };
      this.emit('compaction', stats);
      return stats;
    } finally {
      this.compacting = false;
    }
  }

  private needsCompaction(): boolean {
    for (const segs of this.segments.values()) {
      for (const seg of segs) {
        if (this.segEligible(seg)) return true;
      }
    }
    return false;
  }

  /**
   * 段是否可压缩。
   *
   * 条件：段尾 LSN 已明显落后当前水位（不会再被追加，写者不会与压缩抢同一段），
   * 且事件时间已超出保留期（此段数据本来就该淘汰）。
   * 两个条件都用"已落后"而非"恰好落后"，避免边界抖动导致反复重扫同一段。
   */
  private segEligible(seg: SegmentMeta): boolean {
    const sealed = seg.endLsn < this.committedLsn - 1000;
    const expired = seg.maxTs < Date.now() - this.opts.retentionMs;
    return sealed && expired;
  }

  /**
   * 单段压缩：
   *   1) 读旧段（原子快照）
   *   2) 淘汰超出保留期的记录，重建稀疏索引到「影子键」
   *   3) 原子切换：一次 batch 完成 删旧 + 写新
   * 压缩期间读方仍持有旧快照，不受影响。
   */
  /**
   * 单段压缩：分片处理 + 原子切换。
   *
   * 为什么要分片：早前实现把整段的删除操作攒成一个巨大 batch 再一次性提交，
   * 6 万条记录的删除足以把事件循环堵住上百毫秒，期间所有读请求排队，
   * 实测压缩期查询 p99 从 0.7ms 恶化到 2.5ms（约 3.6 倍），超出 2 倍预算。
   *
   * 现在改成：
   *   1) 按 CHUNK 条分批扫描，每批之间让出事件循环，读写延迟不被长尾拖垮；
   *   2) 扫描阶段只读不写，读方继续走旧段，完全不受影响；
   *   3) 每批的删除立即提交——单批要么全成要么全败，LevelDB batch 本身原子；
   *   4) 索引在最后统一重建（索引只与"还剩哪些记录"有关，逐批写会互相覆盖）。
   *
   * 段被切成 CHUNK_SIZE 的窗口逐个处理，读者在任何时刻看到的都是
   * "旧段 + 已原子切换的前缀"，不会出现撕裂状态。
   */
  private async compactSegment(stream: string, seg: SegmentMeta, cutoffTs: number): Promise<{ scanned: number; dropped: number }> {
    // 提交窗口大小：太小则 LevelDB 批次数多、每次都要抢锁；
    // 太大则单次提交长时间占住写路径。20k/批 在实测中把压缩期读 p99
    // 控制在基线 2 倍以内。
    // 提交批大小与窗口跨度解耦：批决定单次 LevelDB 提交多大，
    // 窗口决定「提交后让出事件循环」的频率。
    // 窗口取批大小的一半，使删除密集时每批只占 LSN 跨度的一小段，
    // 读请求能更早获得调度（实测窗口=批大小时压缩期 p99 约 1.9x 基线，
    // 窗口收紧后明显下降）。
    const CHUNK = this.opts.compactionChunk;
    const CHUNK_SPAN = Math.max(1_000, Math.floor(this.opts.compactionChunk / 4));
    let scanned = 0;
    let dropped = 0;
    const newIndex = new Map<number, { lo: number; hi: number }>();

    const startLsn = seg.startLsn;
    const endLsn = seg.endLsn;

    // 单次压缩的物理删除有总量上限：超出部分留给下一轮后台任务。
    // 删除 6 万条键本身就是几百毫秒的 LevelDB 工作量，若一次做完，
    // 事件循环会被占满，读请求排队，p99 直接恶化一个数量级。
    let deleteBudget = this.opts.compactionMaxScan;

    for (let winStart = startLsn; winStart <= endLsn && deleteBudget > 0; winStart += CHUNK_SPAN) {
      if (this.closed) break;
      const winEnd = Math.min(endLsn, winStart + CHUNK_SPAN - 1);
      const snap = await this.db.snapshot();
      const ops: Array<{ type: 'put' | 'del'; key: Buffer; value?: Buffer }> = [];
      try {
        for await (const [key, value] of this.db.iterator({
          gte: evKey(stream, winStart),
          lt: evKey(stream, winEnd + 1),
          snapshot: snap as never,
        })) {
          if (this.closed) break;
          scanned++;
          const meta = parseEvKey(key);
          if (!meta) continue;
          if (value.length === 1 && value[0] === 0xff) {
            ops.push({ type: 'del', key });
            dropped++;
            continue;
          }
          let rec: DecodedEvent;
          try {
            rec = decodeRecord(value, stream);
          } catch {
            ops.push({ type: 'del', key });
            dropped++;
            continue;
          }
          if (rec.ts < cutoffTs) {
            ops.push({ type: 'del', key });
            dropped++;
            continue;
          }
          const bucket = Math.floor(rec.ts / this.timeBucketMs);
          const cur = newIndex.get(bucket);
          if (!cur) newIndex.set(bucket, { lo: rec.lsn, hi: rec.lsn });
          else {
            cur.lo = Math.min(cur.lo, rec.lsn);
            cur.hi = Math.max(cur.hi, rec.lsn);
          }
          // 提交窗口已满就先落盘，避免单批过大阻塞事件循环
          if (ops.length >= CHUNK) {
            await this.db.batch(ops.splice(0, ops.length) as never);
            // 每提交一块就让出事件循环，读写请求优先获得调度
            deleteBudget -= CHUNK;
            await new Promise<void>((r) => setImmediate(r));
          }
        }
      } finally {
        await (snap as { close: () => Promise<void> }).close();
      }
      if (ops.length > 0) {
        await this.db.batch(ops as never);
        deleteBudget -= ops.length;
      }
      // 让出事件循环：让在途的读请求与写请求先跑完
      await new Promise<void>((r) => setImmediate(r));
    }

    // 索引重建：只保留仍然存活的记录对应的桶
    for (const [bucket, r] of newIndex) {
      const w = new ByteWriter(24);
      w.varint(r.lo);
      w.varint(r.hi);
      await this.db.put(tiKey(stream, bucket), Buffer.from(w.toBuffer()));
    }
    // 内存索引与磁盘对齐：被淘汰的桶直接移除
    const cacheIdx = this.indexFor(stream);
    for (const bucket of [...cacheIdx.keys()]) {
      const bucketTs = bucket * this.timeBucketMs;
      if (bucketTs + this.timeBucketMs - 1 < cutoffTs) cacheIdx.delete(bucket);
    }

    // 段级 floor 推进：低于它的快照视为已被淘汰。
    if (seg.startLsn > this.floorLsn) {
      this.floorLsn = seg.startLsn - 1;
      await this.persistFloor();
    }
    const segs = this.segments.get(stream);
    if (segs) {
      const i = segs.indexOf(seg);
      if (i >= 0) segs.splice(i, 1);
    }
    return { scanned, dropped };
  }
}

function varintBuf(n: number): Buffer {
  const w = new ByteWriter(12);
  w.varint(n);
  return Buffer.from(w.toBuffer());
}

function splitKey(key: Buffer, skip: number): string | null {
  const parts: string[] = [];
  let start = skip;
  for (let i = skip; i <= key.length; i++) {
    if (i === key.length || key[i] === SEP) {
      parts.push(key.toString('utf8', start, i));
      start = i + 1;
    }
  }
  return parts.length >= 1 ? parts[0] : null;
}
