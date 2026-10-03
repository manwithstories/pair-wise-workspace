/**
 * server.ts — Fastify 装配：路由注册、生命周期、统一错误兜底。
 *
 * 本文件只做三件事：把 store / ingest / query 接到 HTTP 上、在启动关闭时管好资源、
 * 以及保证任何异常都经 errors.ts 的统一出口返回。这里不放业务判断。
 *
 * 数据流（写入）：
 *   POST /v1/streams/:stream/events
 *     -> validate.sanitizeBatch        消毒 + schema
 *     -> ingest.submit                合流 / 背压 / 幂等
 *     -> store.appendBatch            分配 LSN + 落盘
 *     -> 202 { lsn }
 * 数据流（读取）：
 *   POST /v1/streams/:stream/query
 *     -> validate.sanitizeStreamParam + sanitizeQuery
 *     -> query.planQuery              AST + 参数绑定
 *     -> store.openSnapshot(lsn)      MVCC 一致性快照
 *     -> store.scanRange              LSN/时间裁剪
 *     -> query 谓词逐条过滤           200 { events }
 */

import Fastify from 'fastify';
import path from 'node:path';
import { Store } from './store.js';
import { Ingest } from './ingest.js';
import { planQuery } from './query.js';
import { AppError, installErrorHandling, toErrorResponse } from './errors.js';
import {
  ingestBatchSchema,
  querySchema,
  streamParamSchema,
  sanitizeBatch,
  sanitizeQuery,
  sanitizeStreamParam,
  LIMITS,
} from './validate.js';

/** 单次查询最多扫描的候选事件数，限制读放大与最坏延迟。 */
const QUERY_MAX_SCAN = 20_000;

export interface ServerOptions {
  port?: number;
  host?: string;
  dataDir?: string;
  retentionMs?: number;
  logger?: boolean;
}

export async function buildServer(options: ServerOptions = {}) {
  const app = Fastify({
    // 关掉默认请求体日志与自动 JSON 序列化大对象，减少热路径开销。
    logger: options.logger === true
      ? { level: 'info' }
      : false,
    disableRequestLogging: true,
    bodyLimit: LIMITS.BODY_MAX_BYTES,
    trustProxy: false,
    // ajv 默认会 coerce 类型，这里关掉：类型必须由 schema 严格判定。
    ajv: { customOptions: { coerceTypes: false, removeAdditional: false, allErrors: false } },
  });

  // ---- 统一错误出口：任何路由异常都在这里收敛 ----
  installErrorHandling(app, app.log);

  // ---- 依赖装配 ----
  // DATA_DIR 允许把账本落到自定义目录（verify.sh 与测试都用它隔离数据）。
  const dataDir = options.dataDir ?? process.env.DATA_DIR ?? path.resolve(process.cwd(), 'data');
  const store = new Store({
    path: path.join(dataDir, 'ledger'),
    retentionMs: options.retentionMs ?? 3_600_000,
  });
  const ingest = new Ingest(store, {
    flushIntervalMs: 5,
    maxBatchSize: 256,
    maxBacklog: 50_000,
  });

  await store.open();
  ingest.start();

  // ---- 关闭协调：先拒新请求，再把队列落盘，最后关存储 ----
  let shuttingDown = false;
  const shutdown = async (reason: string) => {
    if (shuttingDown) return;
    shuttingDown = true;
    app.log.info({ reason }, 'shutting down');
    try {
      await ingest.stop();
      await store.close();
    } catch (err) {
      app.log.error({ err }, 'error during shutdown');
    }
  };
  app.addHook('onClose', async () => {
    await shutdown('close');
  });

  const guardOpen = async () => {
    if (shuttingDown) throw new AppError('E_SHUTTING_DOWN');
  };

  // ---- 健康 / 指标 ----
  app.get('/healthz', async () => ({ status: 'ok', lsn: store.watermark, floor: store.floor }));

  app.get('/v1/stats', async () => ({
    lsn: store.watermark,
    floorLsn: store.floor,
    store: store.stats,
    ingest: ingest.getStats(),
    segments: store.segmentReport().length,
  }));

  // ---- 写入：批量追加 ----
  app.post<{ Params: { stream: string }; Body: unknown }>(
    '/v1/streams/:stream/events',
    { schema: { params: streamParamSchema, body: ingestBatchSchema } },
    async (req, reply) => {
      await guardOpen();
      // 1) 路径参数单独消毒：它会进入 KV 键，必须先过穿透检测。
      const stream = sanitizeStreamParam(req.params.stream);
      // 2) 请求体消毒：schema 已过一遍，这里补字符集/长度/归一化。
      const batch = sanitizeBatch(req.body);
      // 3) 路径与体内的 stream 必须一致，防止跨流混淆写入。
      if (batch.stream !== stream) {
        throw new AppError('E_VALIDATION_FAILED', {
          details: { path: '/body/stream', reason: 'mismatch_with_path_param', field: 'stream' },
        });
      }
      // 4) 合流 + 背压 + 幂等；提交完成即已持久化，返回 LSN 区间。
      const res = await ingest.submit(stream, batch.producer, batch.events);
      return reply.status(202).send({
        accepted: res.written,
        deduped: res.deduped,
        lsn: res.endLsn,
        lsnStart: res.startLsn,
        watermark: store.watermark,
      });
    },
  );

  // ---- 读取：参数化查询 ----
  app.post<{ Params: { stream: string }; Body: unknown }>(
    '/v1/streams/:stream/query',
    { schema: { params: streamParamSchema, body: querySchema } },
    async (req) => {
      await guardOpen();
      const stream = sanitizeStreamParam(req.params.stream);
      const q = sanitizeQuery(req.body);

      // 解析 DSL -> AST + 参数绑定。注入在这一步被拒（400）。
      const plan = planQuery(q.filter, stream);

      // LSN 未指定时读最新已提交水位。
      const targetLsn = q.lsn ?? store.watermark;
      const snap = await store.openSnapshot(targetLsn, stream);
      try {
        // 顺序很重要：先取候选集（只按时间/LSN 裁剪，不按条数截断），
        // 再跑 AST 谓词过滤，最后才按 limit 截断。
        // 若让存储层先按 limit 截断，命中的事件落在截断点之后时会返回空结果。
        const candidates = await store.scanCandidates(snap, {
          fromTs: plan.timeWindow.fromTs,
          toTs: plan.timeWindow.toTs,
          maxScan: QUERY_MAX_SCAN,
          order: q.order,
        });
        const matched: typeof candidates = [];
        for (const ev of candidates) {
          if (plan.predicate(ev)) matched.push(ev);
          if (matched.length >= q.limit) break;
        }
        return {
          lsn: snap.lsn,
          count: matched.length,
          scanned: candidates.length,
          fields: plan.usedFields,
          events: matched,
        };
      } finally {
        // 快照必须释放，否则 openSnapshots 计数只增不减。
        await snap.close();
      }
    },
  );

  // ---- 读取：按 LSN 补偿回放 ----
  app.post<{ Params: { stream: string }; Body: unknown }>(
    '/v1/streams/:stream/replay',
    { schema: { params: streamParamSchema, body: querySchema } },
    async (req) => {
      await guardOpen();
      const stream = sanitizeStreamParam(req.params.stream);
      const q = sanitizeQuery(req.body);
      const targetLsn = q.lsn ?? store.watermark;
      const snap = await store.openSnapshot(targetLsn, stream);
      try {
        const events = await store.replay(snap, q.limit);
        return { lsn: snap.lsn, count: events.length, events };
      } finally {
        await snap.close();
      }
    },
  );

  // ---- 运维：手动触发一次后台压缩（验证保留策略与读放大守护） ----
  app.post('/v1/admin/compact', async () => {
    await guardOpen();
    const stats = await store.maybeCompact(true);
    return { compaction: stats ?? { segmentsMerged: 0, eventsScanned: 0, eventsDropped: 0, durationMs: 0, withinBudget: true } };
  });

  return {
    app,
    store,
    ingest,
    listen: (port = options.port ?? 8080, host = options.host ?? '127.0.0.1') =>
      app.listen({ port, host }),
    shutdown,
  };
}

// 直接执行时启动服务（`node build/server.js`）。
const isMain = process.argv[1] && import.meta.url === `file://${process.argv[1]}`;
if (isMain) {
  const port = Number(process.env.PORT ?? 8080);
  const host = process.env.HOST ?? '127.0.0.1';
  let built;
  try {
    built = await buildServer({ port, host, logger: process.env.LOG !== '0' });
    await built.listen(port, host);
  } catch (err) {
    // 启动失败（端口占用 / 目录被锁 / 权限）必须走统一错误出口，
    // 而不是让顶层 await 抛出未捕获拒绝。
    const { status, body } = toErrorResponse(err);
    process.stderr.write(`${JSON.stringify(body)}\n`);
    process.exit(status === 409 || status === 503 ? 1 : 1);
  }
  built.app.log.info({ port, host, dataDir: process.env.DATA_DIR }, 'obs-ledger listening');

  const stop = async (signal: string) => {
    built.app.log.info({ signal }, 'signal received');
    try {
      await built.app.close();
      process.exit(0);
    } catch {
      process.exit(1);
    }
  };
  process.on('SIGINT', () => void stop('SIGINT'));
  process.on('SIGTERM', () => void stop('SIGTERM'));
}
