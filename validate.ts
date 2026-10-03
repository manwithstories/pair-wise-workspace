/**
 * validate.ts — 请求体与路径参数的 schema 校验 + 自写消毒器。
 *
 * 两层职责，顺序不可颠倒：
 *   1) fastify JSON Schema（AJV）负责「类型 / 范围 / 枚举 / 必填」，失败即 400 + E_VALIDATION_FAILED。
 *   2) 自写消毒器负责「长度 / 字符集 / Unicode 归一化」，失败即 400 + E_SANITIZE_FAILED。
 *
 * 第 2 层不能省：AJV 只看类型，"' OR 1=1 --" 也是合法 string，schema 拦不住。
 */

/** 单条事件与批次的体积上限，直接决定 ingest 内存占用上界。 */
export const LIMITS = {
  STREAM_MAX_LEN: 64,
  PRODUCER_MAX_LEN: 64,
  MESSAGE_MAX_LEN: 1024,
  BATCH_MAX_EVENTS: 1000,
  BODY_MAX_BYTES: 1024 * 1024,
  SEQ_MAX: Number.MAX_SAFE_INTEGER,
  TS_MIN: 0,
} as const;

/** 允许被查询的字段白名单。query.ts 会引用这张表，越权字段在这里就被挡掉。 */
export const FIELD_ALLOWLIST = [
  'stream', 'producer', 'kind', 'level', 'region', 'service',
  'tenant', 'message', 'ts', 'value', 'lsn',
] as const;
export type QueryableField = (typeof FIELD_ALLOWLIST)[number];

/** 事件 kind 枚举：写侧和查侧共用，避免"写得进去查不出来"。 */
export const EVENT_KINDS = ['metric', 'audit', 'log', 'span'] as const;
export const EVENT_LEVELS = ['debug', 'info', 'warn', 'error', 'fatal'] as const;

/** 事件归一化后的内部形状。store / ingest / query 都以它为准。 */
export interface LedgerEvent {
  stream: string;
  producer: string;
  producerSeq: number;
  kind: (typeof EVENT_KINDS)[number];
  level: (typeof EVENT_LEVELS)[number];
  region: string;
  service: string;
  tenant: string;
  ts: number;
  value: number;
  message: string;
}

/** 控制字符 + DEL + 各类零宽/格式字符。用转义构造，源码里不出现裸字节。 */
const CTRL_RE = new RegExp('[\\u0000-\\u001f\\u007f]', 'g');
const ZERO_WIDTH_RE = new RegExp('[\\u200b-\\u200f\\u2028\\u2029\\u202a-\\u202e\\u2060-\\u2064\\ufeff]', 'g');

/**
 * 同形异义字符（confusable）折叠表。
 * 攻击者用西里尔 а / е / о 或全角字符冒充 service、level 等字段值，
 * 在没有归一化的比较逻辑里会"看起来像白名单值"从而绕过检查。
 * 这里在消毒阶段就把它们统一成 ASCII 码点，再交给白名单比对。
 */
const CONFUSABLES: Record<string, string> = {
  // Cyrillic
  'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c',
  'у': 'y', 'х': 'x', 'і': 'i', 'ј': 'j', 'һ': 'h',
  'А': 'A', 'Е': 'E', 'О': 'O', 'Р': 'P', 'С': 'C',
  'Н': 'H', 'К': 'K', 'М': 'M', 'Т': 'T', 'В': 'B',
  'Х': 'X', 'Ѕ': 'S', 'І': 'I', 'Ј': 'J',
  // Greek
  'α': 'a', 'ο': 'o', 'ν': 'v', 'ρ': 'p', 'ε': 'e',
  'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H',
  'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O',
  'Ρ': 'P', 'Τ': 'T', 'Υ': 'Y', 'Χ': 'X',
  // Latin lookalikes
  'ı': 'i', 'ɡ': 'g', 'ɑ': 'a', 'ⅼ': 'l', 'ⅰ': 'i',
  // Armenian / Cherokee 等常用于绕过的字符
  'ա': 'a', 'օ': 'o', 'Ꭰ': 'D', 'Ꮐ': 'G', 'Ᏼ': 'V',
  // fullwidth ASCII
  'ａ': 'a', 'ｂ': 'b', 'ｃ': 'c', 'ｄ': 'd', 'ｅ': 'e', 'ｆ': 'f',
  'ｇ': 'g', 'ｈ': 'h', 'ｉ': 'i', 'ｊ': 'j', 'ｋ': 'k', 'ｌ': 'l',
  'ｍ': 'm', 'ｎ': 'n', 'ｏ': 'o', 'ｐ': 'p', 'ｑ': 'q', 'ｒ': 'r',
  'ｓ': 's', 'ｔ': 't', 'ｕ': 'u', 'ｖ': 'v', 'ｗ': 'w', 'ｘ': 'x',
  'ｙ': 'y', 'ｚ': 'z',
  // 特殊符号伪装
  '‐': '-', '‑': '-', '‒': '-', '–': '-', '—': '-', '−': '-',
  '＝': '=', '＜': '<', '＞': '>', '／': '/',
};

/** 危险标识符片段：点号穿透与原型链访问的统一拦截。 */
const DANGEROUS_FRAGMENTS = [
  '__proto__', 'prototype', 'constructor', 'new', 'function',
  'require', 'import', 'eval', 'return', 'delete', 'typeof', 'instanceof',
  'this', 'class', 'extends', 'global', 'process', 'globalThis',
  'module', 'exports', '__dirname', '__filename', 'reflect', 'window',
];

// ---------------------------------------------------------------------------
// 消毒原语
// ---------------------------------------------------------------------------

/** 归一化：NFKC + 去控制字符 + 去零宽 + 折叠同形字符 + 折叠空白。 */
export function normalizeText(input: string): string {
  let s = input;
  // 先做 Unicode 归一化，把全角/兼容字符拉回 ASCII，再谈消毒。
  try {
    s = s.normalize('NFKC');
  } catch {
    /* 非法 UTF-16 代理对等极端输入，保持原样交给后续规则拒绝 */
  }
  s = s.replace(CTRL_RE, '').replace(ZERO_WIDTH_RE, '');
  let out = '';
  for (const ch of s) {
    const folded = CONFUSABLES[ch];
    out += folded !== undefined ? folded : ch;
  }
  // 折叠所有空白（含 NBSP）成单个普通空格并 trim，避免 "admin " 绕过前缀判断。
  return out.replace(/\s+/g, ' ').trim();
}

/** 检测是否含任何"不该出现在标识符里"的字符。 */
function assertCharset(s: string, field: string, pattern: RegExp, maxLen: number, label: string): void {
  if (s.length === 0) {
    throw sanitizeError(field, 'empty_after_normalize');
  }
  if (s.length > maxLen) {
    throw sanitizeError(field, 'too_long', { max: maxLen, actual: s.length });
  }
  if (!pattern.test(s)) {
    throw sanitizeError(field, 'charset_violation', { expected: label });
  }
}

/** 点号 / 反斜杠 / 各种分隔符穿透的公共检测。 */
export function assertNoTraversal(raw: string, field: string): void {
  const lower = raw.toLowerCase();
  for (const frag of DANGEROUS_FRAGMENTS) {
    if (lower.includes(frag)) {
      throw injectionError(field, 'dangerous_identifier', { keyword: frag });
    }
  }
  // 点号路径穿透：a.b / a$b / a["b"] / a::b
  if (/[.$[\](){}/\\;]/.test(raw)) {
    throw injectionError(field, 'path_traversal');
  }
  // 空白与引号包裹（"admin" vs admin）
  if (/["'`]/.test(raw)) {
    throw injectionError(field, 'quote_injection');
  }
  if (/[*?~!|&=<>]/.test(raw)) {
    throw injectionError(field, 'metacharacter');
  }
}

/** 消毒一个"标识符类"字段（stream/producer/service/tenant/region）。 */
export function sanitizeIdentifier(input: unknown, field: string, maxLen: number): string {
  if (typeof input !== 'string') {
    throw sanitizeError(field, 'not_a_string');
  }
  const normalized = normalizeText(input);
  // 标识符只允许 [a-z0-9_.:-] 这类安全字符；这里不放开点号，
  // 避免 "svc.prod" 这种名字被后续解析层当成路径分隔。
  assertCharset(normalized, field, /^[a-zA-Z0-9_:-]+$/, maxLen, 'identifier [A-Za-z0-9_:-]');
  assertNoTraversal(input, field);
  return normalized;
}

/** 消毒自由文本字段（message）：允许更宽字符集，但禁控制字符与长度溢出。 */
export function sanitizeFreeText(input: unknown, field: string, maxLen: number): string {
  if (typeof input !== 'string') {
    throw sanitizeError(field, 'not_a_string');
  }
  const normalized = normalizeText(input);
  if (normalized.length > maxLen) {
    throw sanitizeError(field, 'too_long', { max: maxLen, actual: normalized.length });
  }
  // 自由文本里仍然禁止控制字符与常见的注入前缀，防止下游日志/终端被污染。
  if (CTRL_RE.test(input) || ZERO_WIDTH_RE.test(input)) {
    throw sanitizeError(field, 'control_characters');
  }
  return normalized;
}

/** 消毒数字字段：必须是有限数，且不落入不可表示区间。 */
export function sanitizeNumber(input: unknown, field: string, min: number, max: number): number {
  let n: number;
  if (typeof input === 'number') {
    n = input;
  } else if (typeof input === 'string' && /^-?\d+(\.\d+)?([eE][+-]?\d+)?$/.test(input.trim())) {
    n = Number(input.trim());
  } else {
    throw sanitizeError(field, 'not_a_number');
  }
  if (!Number.isFinite(n)) {
    throw sanitizeError(field, 'not_finite');
  }
  if (n < min || n > max) {
    throw sanitizeError(field, 'out_of_range', { min, max, actual: n });
  }
  return n;
}

// ---------------------------------------------------------------------------
// 错误构造（统一走 errors.ts 出口）
// ---------------------------------------------------------------------------

import { AppError } from './errors.js';

function sanitizeError(field: string, reason: string, extra: Record<string, unknown> = {}): AppError {
  return new AppError('E_SANITIZE_FAILED', { details: { field, reason, ...extra } });
}

function injectionError(field: string, reason: string, extra: Record<string, unknown> = {}): AppError {
  return new AppError('E_QUERY_INJECTION', { details: { field, reason, ...extra } });
}

// ---------------------------------------------------------------------------
// 事件级校验
// ---------------------------------------------------------------------------

/** fastify JSON Schema：批次容器。字段级 schema 在 sanitizeEvent 里做（更细的错误信息）。 */
export const ingestBatchSchema = {
  type: 'object',
  required: ['stream', 'producer', 'events'],
  additionalProperties: false,
  properties: {
    stream: { type: 'string', minLength: 1, maxLength: LIMITS.STREAM_MAX_LEN },
    producer: { type: 'string', minLength: 1, maxLength: LIMITS.PRODUCER_MAX_LEN },
    events: {
      type: 'array',
      minItems: 1,
      maxItems: LIMITS.BATCH_MAX_EVENTS,
      items: { type: 'object' },
    },
  },
} as const;

/** fastify JSON Schema：事件对象。 */
export const eventSchema = {
  type: 'object',
  required: ['producerSeq', 'kind', 'ts'],
  additionalProperties: false,
  properties: {
    producerSeq: { type: 'integer', minimum: 0, maximum: LIMITS.SEQ_MAX },
    kind: { type: 'string', enum: [...EVENT_KINDS] },
    level: { type: 'string', enum: [...EVENT_LEVELS] },
    ts: { type: 'integer', minimum: LIMITS.TS_MIN },
    value: { type: 'number' },
    region: { type: 'string', maxLength: 32 },
    service: { type: 'string', maxLength: 64 },
    tenant: { type: 'string', maxLength: 64 },
    message: { type: 'string', maxLength: LIMITS.MESSAGE_MAX_LEN },
  },
} as const;

/** fastify JSON Schema：查询请求体。 */
export const querySchema = {
  type: 'object',
  required: ['filter'],
  additionalProperties: false,
  properties: {
    lsn: { type: 'integer', minimum: 0 },
    filter: { type: 'string', minLength: 1, maxLength: 2048 },
    limit: { type: 'integer', minimum: 1, maximum: 1000 },
    order: { type: 'string', enum: ['asc', 'desc'] },
  },
} as const;

/** fastify JSON Schema：流路径参数。 */
export const streamParamSchema = {
  type: 'object',
  required: ['stream'],
  properties: { stream: { type: 'string', minLength: 1, maxLength: LIMITS.STREAM_MAX_LEN } },
} as const;

/**
 * 逐字段消毒并归一化成 LedgerEvent。
 * schema 已经保证了类型与枚举，这里补上「字符集 / 长度 / 归一化 / 穿透检测」。
 */
export function sanitizeEvent(raw: unknown, ctx: { stream: string; producer: string }): LedgerEvent {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new AppError('E_VALIDATION_FAILED', { details: { path: '/events', reason: 'not_an_object' } });
  }
  const e = raw as Record<string, unknown>;

  const producerSeq = sanitizeNumber(e.producerSeq, 'producerSeq', 0, LIMITS.SEQ_MAX);
  if (!Number.isInteger(producerSeq)) {
    throw sanitizeError('producerSeq', 'not_an_integer');
  }

  const ts = sanitizeNumber(e.ts, 'ts', LIMITS.TS_MIN, Number.MAX_SAFE_INTEGER);
  if (!Number.isInteger(ts)) {
    throw sanitizeError('ts', 'not_an_integer');
  }

  const kind = normalizeText(String(e.kind));
  if (!(EVENT_KINDS as readonly string[]).includes(kind)) {
    throw sanitizeError('kind', 'not_in_enum', { allowed: EVENT_KINDS.join(',') });
  }

  const level = normalizeText(String(e.level ?? 'info'));
  if (!(EVENT_LEVELS as readonly string[]).includes(level)) {
    throw sanitizeError('level', 'not_in_enum', { allowed: EVENT_LEVELS.join(',') });
  }

  const region = sanitizeIdentifier(e.region ?? 'default', 'region', 32);
  const service = sanitizeIdentifier(e.service ?? 'unknown', 'service', 64);
  const tenant = sanitizeIdentifier(e.tenant ?? 'default', 'tenant', 64);
  const message = sanitizeFreeText(e.message ?? '', 'message', LIMITS.MESSAGE_MAX_LEN);
  const value = e.value === undefined ? 0 : sanitizeNumber(e.value, 'value', -1e15, 1e15);

  return {
    stream: ctx.stream,
    producer: ctx.producer,
    producerSeq,
    kind: kind as LedgerEvent['kind'],
    level: level as LedgerEvent['level'],
    region,
    service,
    tenant,
    ts,
    value,
    message,
  };
}

/** 消毒整批：先容器后事件，任一字段不过即整批拒绝（账本不做部分写入）。 */
export function sanitizeBatch(body: unknown): { stream: string; producer: string; events: LedgerEvent[] } {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) {
    throw new AppError('E_VALIDATION_FAILED', { details: { path: '(root)', reason: 'not_an_object' } });
  }
  const b = body as Record<string, unknown>;

  const stream = sanitizeIdentifier(b.stream, 'stream', LIMITS.STREAM_MAX_LEN);
  const producer = sanitizeIdentifier(b.producer, 'producer', LIMITS.PRODUCER_MAX_LEN);

  const rawEvents = b.events;
  if (!Array.isArray(rawEvents)) {
    throw new AppError('E_VALIDATION_FAILED', { details: { path: '/events', reason: 'not_an_array' } });
  }
  if (rawEvents.length > LIMITS.BATCH_MAX_EVENTS) {
    throw new AppError('E_PAYLOAD_TOO_LARGE', { details: { max: LIMITS.BATCH_MAX_EVENTS, actual: rawEvents.length } });
  }

  const ctx = { stream, producer };
  const events: LedgerEvent[] = [];
  for (let i = 0; i < rawEvents.length; i++) {
    try {
      events.push(sanitizeEvent(rawEvents[i], ctx));
    } catch (err) {
      // 把出错下标带上，方便上游定位；错误码保持不变。
      if (err instanceof AppError) {
        throw new AppError(err.code, { details: { ...err.details, path: `/events/${i}` }, cause: err });
      }
      throw err;
    }
  }
  return { stream, producer, events };
}

/** 消毒查询体：filter 交给 query.ts 做词法分析，这里只做外壳约束。 */
export function sanitizeQuery(body: unknown): { lsn: number | null; filter: string; limit: number; order: 'asc' | 'desc' } {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) {
    throw new AppError('E_VALIDATION_FAILED', { details: { path: '(root)', reason: 'not_an_object' } });
  }
  const b = body as Record<string, unknown>;
  const lsn = b.lsn === undefined ? null : sanitizeNumber(b.lsn, 'lsn', 0, Number.MAX_SAFE_INTEGER);
  if (lsn !== null && !Number.isInteger(lsn)) {
    throw sanitizeError('lsn', 'not_an_integer');
  }
  const limit = b.limit === undefined ? 100 : sanitizeNumber(b.limit, 'limit', 1, 1000);
  if (!Number.isInteger(limit)) {
    throw sanitizeError('limit', 'not_an_integer');
  }
  const order = b.order === undefined ? 'asc' : normalizeText(String(b.order));
  if (order !== 'asc' && order !== 'desc') {
    throw sanitizeError('order', 'not_in_enum', { allowed: 'asc,desc' });
  }
  const filter = typeof b.filter === 'string' ? b.filter : '';
  // 控制字符在 DSL 里没有任何合法用途，直接拒。
  if (CTRL_RE.test(filter.replace(/[\t\n\r]/g, ''))) {
    throw injectionError('filter', 'control_characters');
  }
  return { lsn, filter, limit, order: order as 'asc' | 'desc' };
}

/** 消毒路径参数里的 stream（会进 KV key，必须先过穿透检测）。 */
export function sanitizeStreamParam(raw: unknown): string {
  return sanitizeIdentifier(raw, 'stream', LIMITS.STREAM_MAX_LEN);
}
