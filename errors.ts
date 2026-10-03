/**
 * errors.ts — 集中错误码目录、HTTP 映射与统一响应包装。
 *
 * 全服务（server / store / ingest / query / validate）只允许通过本模块抛错，
 * Fastify 的 setErrorHandler / setNotFoundHandler 是唯一的 HTTP 出口。
 * 这样保证：任何内部异常都不会把堆栈、文件路径或存储细节泄漏给调用方，
 * 同时调用方拿到的永远是稳定的 { error: { code, message, details? } } 结构。
 */

/** 目录条目形状。显式声明，避免 as const 把字面量收窄成不可赋值的类型。 */
export interface CatalogEntryShape {
  status: number;
  message: string;
  retryable: boolean;
}

/** 错误码 -> HTTP 状态 / 默认文案 / 是否可重试。目录是单点，改这里即可。 */
export const ERROR_CATALOG = {
  // ---- 400 请求本身不合法 ----
  E_BAD_REQUEST: { status: 400, message: 'malformed request', retryable: false },
  E_VALIDATION_FAILED: { status: 400, message: 'request failed schema validation', retryable: false },
  E_SANITIZE_FAILED: { status: 400, message: 'input rejected by sanitizer', retryable: false },
  E_PATH_PARAM_INVALID: { status: 400, message: 'invalid path parameter', retryable: false },

  // ---- 400 查询层：语法 / 注入 / 越权字段 ----
  E_QUERY_SYNTAX: { status: 400, message: 'query parse error', retryable: false },
  E_QUERY_INJECTION: { status: 400, message: 'query rejected by injection guard', retryable: false },
  E_QUERY_FIELD_NOT_ALLOWED: { status: 400, message: 'query field not in allowlist', retryable: false },
  E_QUERY_OPERATOR_NOT_ALLOWED: { status: 400, message: 'operator not allowed for field', retryable: false },
  E_QUERY_VALUE_INVALID: { status: 400, message: 'query literal type mismatch', retryable: false },
  E_QUERY_TOO_COMPLEX: { status: 400, message: 'query exceeds complexity budget', retryable: false },

  // ---- 400 快照 / LSN ----
  E_LSN_INVALID: { status: 400, message: 'lsn must be a non-negative integer', retryable: false },
  E_LSN_AHEAD: { status: 400, message: 'lsn is ahead of the committed watermark', retryable: false },

  // ---- 404 / 405 / 409 / 410 ----
  E_NOT_FOUND: { status: 404, message: 'resource not found', retryable: false },
  E_METHOD_NOT_ALLOWED: { status: 405, message: 'method not allowed', retryable: false },
  E_CONFLICT: { status: 409, message: 'conflicting write (producer sequence)', retryable: false },
  E_SNAPSHOT_EXPIRED: { status: 410, message: 'requested lsn has been compacted away', retryable: false },

  // ---- 413 / 429 ----
  E_PAYLOAD_TOO_LARGE: { status: 413, message: 'payload too large', retryable: false },
  E_BACKPRESSURE: { status: 429, message: 'ingest backlog above watermark, batch rejected', retryable: true },

  // ---- 5xx / 503 ----
  E_STORAGE: { status: 500, message: 'storage engine failure', retryable: false },
  E_INTERNAL: { status: 500, message: 'internal error', retryable: false },
  E_SHUTTING_DOWN: { status: 503, message: 'service is shutting down', retryable: true },
} as const satisfies Record<string, CatalogEntryShape>;

/** 错误码联合类型由目录 key 反推，保证新增码必须显式处理。 */
export type ErrorCode = keyof typeof ERROR_CATALOG;
type CatalogEntry = (typeof ERROR_CATALOG)[ErrorCode];

/** 对外暴露的 details 白名单：只有这些 key 会进入响应体。 */
const SAFE_DETAIL_KEYS = new Set([
  'field', 'path', 'limit', 'max', 'min', 'actual', 'expected', 'allowed',
  'operator', 'reason', 'retryAfterMs', 'hint', 'index', 'keyword', 'depth', 'width', 'lsn',
]);

/** C0 控制字符 + DEL。用转义写法构造，避免源码里出现裸控制字节。 */
export const CTRL_RE = new RegExp('[\\u0000-\\u001f\\u007f]', 'g');

export interface ErrorBody {
  error: {
    code: ErrorCode;
    message: string;
    retryable: boolean;
    details?: Record<string, unknown>;
  };
  requestId?: string;
}

/** 内部异常载体。4xx 可安全外发；5xx 的 message 一律不外泄。 */
export class AppError extends Error {
  readonly code: ErrorCode;
  readonly status: number;
  readonly retryable: boolean;
  readonly details: Record<string, unknown>;

  constructor(
    code: ErrorCode,
    options: { details?: Record<string, unknown>; cause?: unknown; message?: string } = {},
  ) {
    const entry: CatalogEntry = ERROR_CATALOG[code];
    super(options.message ?? entry.message);
    if (options.cause !== undefined) (this as { cause?: unknown }).cause = options.cause;
    this.name = 'AppError';
    this.code = code;
    this.status = entry.status;
    this.retryable = entry.retryable;
    this.details = options.details ?? {};
  }
}

export function isAppError(err: unknown): err is AppError {
  return err instanceof AppError;
}

export function appError(code: ErrorCode, details?: Record<string, unknown>, cause?: unknown): AppError {
  return new AppError(code, { details, cause });
}

function sanitizeDetails(details: Record<string, unknown> | undefined): Record<string, unknown> | undefined {
  if (!details) return undefined;
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(details)) {
    if (!SAFE_DETAIL_KEYS.has(k)) continue;
    if (typeof v === 'string') {
      out[k] = v.replace(CTRL_RE, '').slice(0, 120);
    } else if (typeof v === 'number' || typeof v === 'boolean' || v === null) {
      out[k] = v;
    } else if (Array.isArray(v)) {
      out[k] = v.slice(0, 16).map((x) => (typeof x === 'string' ? x.replace(CTRL_RE, '').slice(0, 60) : x));
    }
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** Fastify 内部错误码 -> 目录映射，避免内部码泄漏给调用方。 */
const FASTIFY_CODE_MAP: Record<string, ErrorCode> = {
  FST_ERR_CTP_EMPTY_JSON_BODY: 'E_BAD_REQUEST',
  FST_ERR_CTP_INVALID_MEDIA_TYPE: 'E_BAD_REQUEST',
  FST_ERR_CTP_BODY_TOO_LARGE: 'E_PAYLOAD_TOO_LARGE',
  FST_ERR_CTP_INVALID_JSON_BODY: 'E_BAD_REQUEST',
  FST_ERR_VALIDATION: 'E_VALIDATION_FAILED',
  FST_ERR_BAD_URL: 'E_BAD_REQUEST',
  FST_ERR_BAD_URL_SEARCH_PARAMS: 'E_BAD_REQUEST',
};

/**
 * 把任意 throw 出来的东西归一化成对外响应体 + 状态码。
 * 5xx 一律降级为通用文案，避免泄漏堆栈 / 路径 / 存储细节。
 */
export function toErrorResponse(err: unknown, requestId?: string): { status: number; body: ErrorBody } {
  let code: ErrorCode = 'E_INTERNAL';
  let message: string = ERROR_CATALOG.E_INTERNAL.message;
  let details: Record<string, unknown> | undefined;
  let retryable: boolean = ERROR_CATALOG.E_INTERNAL.retryable;

  if (isAppError(err)) {
    code = err.code;
    message = err.message;
    details = err.details;
    retryable = err.retryable;
  } else if (err && typeof err === 'object') {
    const e = err as { code?: unknown; validation?: unknown };
    if (typeof e.code === 'string' && e.code.startsWith('FST_ERR_')) {
      const mapped = FASTIFY_CODE_MAP[e.code];
      if (mapped) {
        code = mapped;
        message = ERROR_CATALOG[mapped].message;
        retryable = ERROR_CATALOG[mapped].retryable;
      }
    }
    if (Array.isArray(e.validation) && e.validation.length > 0) {
      code = 'E_VALIDATION_FAILED';
      message = ERROR_CATALOG.E_VALIDATION_FAILED.message;
      const first = e.validation[0] as { instancePath?: string; keyword?: string; message?: string };
      details = {
        path: first.instancePath || '(root)',
        keyword: first.keyword,
        expected: first.message,
        reason: 'schema',
      };
    }
  }

  const safe = sanitizeDetails(details);
  const body: ErrorBody = { error: { code, message, retryable, ...(safe ? { details: safe } : {}) } };
  if (requestId) body.requestId = requestId;
  return { status: ERROR_CATALOG[code].status, body };
}

/**
 * 挂载统一异常处理。所有路由异常（同步 throw、async reject、preHandler 抛错）
 * 都收敛到这里；业务代码里不允许再出现裸 reply.status(...).send({ error })。
 */
export function installErrorHandling(app: any, log: { error: (o: any, m?: string) => void }) {
  app.setNotFoundHandler((req: any, reply: any) => {
    const { status, body } = toErrorResponse(
      new AppError('E_NOT_FOUND', { details: { path: String(req.url).slice(0, 120) } }),
      req.id,
    );
    void reply.status(status).send(body);
  });

  app.setErrorHandler((err: any, req: any, reply: any) => {
    const { status, body } = toErrorResponse(err, req.id);
    if (status >= 500) {
      // 原始异常（含堆栈）只落服务端日志，响应体里只有通用文案。
      log.error({ err: { message: err?.message, stack: err?.stack, code: err?.code } }, 'unhandled request error');
    }
    if (body.error.retryable) {
      const ra = isAppError(err) && typeof err.details.retryAfterMs === 'number' ? err.details.retryAfterMs : 100;
      void reply.header('retry-after', String(Math.max(1, Math.ceil(ra / 1000))));
    }
    void reply.status(status).send(body);
  });
}

/** 供非 HTTP 场景（后台任务 / 生命周期）复用同一套文案。 */
export function describeCode(code: ErrorCode): { status: number; message: string } {
  return { status: ERROR_CATALOG[code].status, message: ERROR_CATALOG[code].message };
}
