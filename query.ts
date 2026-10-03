/**
 * query.ts — 查询 DSL 的词法分析 / AST 生成 / 参数化绑定 / 注入拦截。
 *
 * 安全模型（这是整个查询层的核心设计）：
 *   查询串永远不被拼接进任何底层语句，也从不被当作路径或键名使用。
 *   词法分析把 filter 切成 token，解析成 AST，字段名对照白名单解析成「列号」，
 *   字面量变成「参数槽」。执行时 AST 只携带 (columnIndex, paramSlot)，
 *   真正的值走 params 数组。这样字段名与值两个层面都不存在注入面：
 *     - 字段名只能是白名单里的标识符，越权字段（含 Unicode 同形伪装）直接拒；
 *     - 值必须是合法字面量，绑定后只做等值/比较，不参与任何结构拼装。
 *
 * 反例对照（这些都必须被拒，而不是"跑起来再说"）：
 *   ts > 1; DROP TABLE users      -> 分号/多语句
 *   service.__proto__.polluted    -> 点号穿透
 *   service == "a" || ts < 0       -> 操作符走私（|| 拼接）
 *   ѕervice == "a"                -> Unicode 同形字符冒充字段名
 */

import { AppError } from './errors.js';
import { FIELD_ALLOWLIST, normalizeText } from './validate.js';
import type { DecodedEvent } from './store.js';

// ---------------------------------------------------------------------------
// Token
// ---------------------------------------------------------------------------

export const enum TokType {
  IDENT = 'ident',
  NUMBER = 'number',
  STRING = 'string',
  OP = 'op',
  LPAREN = 'lparen',
  RPAREN = 'rparen',
  COMMA = 'comma',
  EOF = 'eof',
}

export interface Token {
  type: TokType;
  /** 归一化后的文本（标识符已折叠同形字符并 trim）。 */
  value: string;
  /** 原始文本，用于错误定位与注入检测。 */
  raw: string;
  pos: number;
}

const OPERATORS = ['>=', '<=', '!=', '==', '>', '<', '=', '~'] as const;

/** 标识符允许的字符集，与写入侧的消毒器保持一致。 */
const IDENT_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/**
 * 字段名解析 -> 白名单列号。
 *
 * 策略是「严格拒绝」而不是「静默折叠」：
 * 字段名决定读哪一列，一旦放过同形字符，运维在终端上看到的 `sеrvice`
 * 与真正的 `service` 长得一模一样却命中不同列，属于典型的误导性越权。
 * 所以规则是——字段名必须是纯 ASCII 标识符，且必须原样（忽略 ASCII 大小写）
 * 命中白名单。任何需要归一化（NFKC / 去零宽 / 同形折叠）才能命中的写法，
 * 一律按注入处理并拒绝。
 *
 * 对比：字面量「值」不做这个限制。值永远以参数绑定参与比较，
 * 写入侧也已用同一套 normalizeText 归一化，所以值里的同形字符
 * 既不会改变查询结构，也不会与存储侧不一致。
 */
function resolveField(raw: string, pos: number): { field: string; index: number } {
  // 第一层：必须是纯 ASCII 标识符。挡住同形字符、全角、零宽、RTL 控制符等一切变体。
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(raw)) {
    if (/[.$[\](){}/\\;'"`|]/.test(raw)) {
      throw new AppError('E_QUERY_INJECTION', {
        details: { reason: 'path_traversal_in_field', path: String(pos) },
      });
    }
    // 归一化后能命中白名单 => 明确的同形/全角/零宽伪装，直接按注入拒。
    const folded = normalizeText(raw).toLowerCase();
    if ((FIELD_ALLOWLIST as readonly string[]).includes(folded)) {
      throw new AppError('E_QUERY_INJECTION', {
        details: { field: folded, reason: 'homoglyph_field_name', hint: 'use_ascii_field_name' },
      });
    }
    throw new AppError('E_QUERY_SYNTAX', {
      details: { reason: 'invalid_field_token', path: String(pos) },
    });
  }

  // 第二层：白名单精确匹配。
  const field = raw.toLowerCase();
  const index = (FIELD_ALLOWLIST as readonly string[]).indexOf(field);
  if (index < 0) {
    throw new AppError('E_QUERY_FIELD_NOT_ALLOWED', {
      details: {
        field: field.slice(0, 40),
        reason: 'not_in_allowlist',
        allowed: FIELD_ALLOWLIST.join(','),
      },
    });
  }
  return { field, index };
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

export function tokenize(input: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const n = input.length;

  while (i < n) {
    const ch = input[i];

    // 空白跳过
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r') {
      i++;
      continue;
    }

    // 括号 / 逗号
    if (ch === '(') { tokens.push({ type: TokType.LPAREN, value: '(', raw: '(', pos: i }); i++; continue; }
    if (ch === ')') { tokens.push({ type: TokType.RPAREN, value: ')', raw: ')', pos: i }); i++; continue; }
    if (ch === ',') { tokens.push({ type: TokType.COMMA, value: ',', raw: ',', pos: i }); i++; continue; }

    // 字符串字面量：只接受双引号或单引号包裹的、不含任何引号与控制字符的内容。
    if (ch === '"' || ch === "'") {
      const quote = ch;
      const start = i;
      i++;
      let val = '';
      let closed = false;
      while (i < n) {
        const c = input[i];
        if (c === '\\') {
          // 转义只放行有限几种，避免 \" 拼接出新语法。
          const nx = input[i + 1];
          if (nx === quote || nx === '\\') { val += nx; i += 2; continue; }
          throw new AppError('E_QUERY_INJECTION', { details: { reason: 'illegal_escape_sequence', path: String(start) } });
        }
        if (c === quote) { closed = true; i++; break; }
        if (c === '"' || c === "'") {
          // 未闭合就遇到另一种引号 => 典型的引号走私。
          throw new AppError('E_QUERY_INJECTION', { details: { reason: 'quote_confusion', path: String(start) } });
        }
        const code = c.charCodeAt(0);
        if (code < 0x20 || code === 0x7f) {
          throw new AppError('E_QUERY_INJECTION', { details: { reason: 'control_char_in_string', path: String(start) } });
        }
        val += c;
        i++;
      }
      if (!closed) {
        throw new AppError('E_QUERY_SYNTAX', { details: { reason: 'unterminated_string', path: String(start) } });
      }
      tokens.push({ type: TokType.STRING, value: val, raw: input.slice(start, i), pos: start });
      continue;
    }

    // 数字字面量：整数或小数，不接受科学计数法/十六进制（避免 0x.. 与 1e 变形）
    if (/[0-9]/.test(ch) || (ch === '-' && /[0-9]/.test(input[i + 1] ?? ''))) {
      const start = i;
      if (ch === '-') i++;
      while (i < n && /[0-9]/.test(input[i])) i++;
      if (input[i] === '.') {
        i++;
        while (i < n && /[0-9]/.test(input[i])) i++;
      }
      const text = input.slice(start, i);
      // 后面紧跟标识符字符 => 形如 1abc / 0x1f，属于走私
      if (i < n && /[A-Za-z_.]/.test(input[i])) {
        throw new AppError('E_QUERY_INJECTION', { details: { reason: 'numeric_literal_smuggling', path: String(start) } });
      }
      tokens.push({ type: TokType.NUMBER, value: text, raw: text, pos: start });
      continue;
    }

    // 标识符 / 关键字
    //
    // 非 ASCII 字符也必须被整段吃进同一个 IDENT token。早前的实现只吃 ASCII
    // [A-Za-z0-9_]，遇到同形字符就停下并把剩余部分重新分词，结果 "serviсe"
    // 被拆成 "servi" + "сe" 两个 token，"service​ == ..." 也被拆开，
    // 于是同形伪装绕过了字段校验。现在改为：只要不是分隔符，就一直吃到下一个分隔符，
    // 保证 resolveField 拿到完整的原始字段名去判定。
    if (/[A-Za-z_]/.test(ch) || ch.charCodeAt(0) > 0x7f) {
      const start = i;
      while (i < n && !/[\s(),"']/.test(input[i])) i++;
      tokens.push({ type: TokType.IDENT, value: input.slice(start, i), raw: input.slice(start, i), pos: start });
      continue;
    }

    // 操作符
    const two = input.slice(i, i + 2);
    if ((OPERATORS as readonly string[]).includes(two)) {
      tokens.push({ type: TokType.OP, value: two, raw: two, pos: i });
      i += 2;
      continue;
    }
    if ((OPERATORS as readonly string[]).includes(ch)) {
      tokens.push({ type: TokType.OP, value: ch, raw: ch, pos: i });
      i++;
      continue;
    }

    // 其余字符一律视为注入：分号、注释符、管道、&、< > 之外的逻辑符等。
    throw new AppError('E_QUERY_INJECTION', {
      details: {
        reason: 'illegal_character',
        keyword: ch,
        path: String(i),
      },
    });
  }

  tokens.push({ type: TokType.EOF, value: '', raw: '', pos: n });
  return tokens;
}

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

export type CmpOp = '==' | '!=' | '>' | '>=' | '<' | '<=' | '~';

/** 比较节点：字段（白名单列号）+ 操作符 + 参数槽。字段名不进入执行期。 */
export interface CompareNode {
  kind: 'compare';
  field: string;
  columnIndex: number;
  op: CmpOp;
  /** 参数槽下标，值在 params 里，执行期才绑定。 */
  paramSlot: number;
  /** ~ 匹配用的已消毒模式（纯字符串比较，不构造 RegExp 除非已验证）。 */
  pattern?: string;
}

/** 逻辑组合节点。 */
export interface LogicalNode {
  kind: 'and' | 'or';
  children: Node[];
}

export interface TimeRangeNode {
  kind: 'time_range';
  fromTs: number;
  toTs: number;
}

export interface StreamNode {
  kind: 'stream';
  stream: string;
}

export type Node = CompareNode | LogicalNode | TimeRangeNode | StreamNode;

export interface ParsedQuery {
  root: Node;
  /** 参数值数组，按 paramSlot 绑定。 */
  params: unknown[];
  /** 白名单里实际被引用的字段，供审计与 explain。 */
  usedFields: string[];
}

/** 各字段允许的操作符，避免对字符串做 > 比较这种无意义/危险的组合。 */
const OPS_BY_TYPE: Record<string, ReadonlySet<string>> = {
  string: new Set(['==', '!=', '~']),
  number: new Set(['==', '!=', '>', '>=', '<', '<=']),
};

const FIELD_TYPES: Record<string, 'string' | 'number'> = {
  stream: 'string', producer: 'string', kind: 'string', level: 'string',
  region: 'string', service: 'string', tenant: 'string', message: 'string',
  ts: 'number', value: 'number', lsn: 'number',
};

/** 复杂度预算：防止用超长布尔串把 CPU 打满。 */
const MAX_TOKENS = 256;
const MAX_NODES = 128;
const MAX_PARAMS = 64;

// ---------------------------------------------------------------------------
// Parser（递归下降）
// ---------------------------------------------------------------------------

class Parser {
  private i = 0;
  private params: unknown[] = [];
  private nodes = 0;
  readonly usedFields = new Set<string>();

  constructor(private readonly tokens: Token[], private readonly stream: string) {}

  parse(): ParsedQuery {
    const root = this.parseOr();
    if (this.peek().type !== TokType.EOF) {
      // 尾部还有东西 => 多语句/走私
      throw new AppError('E_QUERY_INJECTION', {
        details: { reason: 'trailing_tokens_after_expression', path: String(this.peek().pos) },
      });
    }
    return { root, params: this.params, usedFields: [...this.usedFields] };
  }

  private peek(offset = 0): Token {
    return this.tokens[Math.min(this.i + offset, this.tokens.length - 1)];
  }
  private next(): Token {
    return this.tokens[Math.min(this.i++, this.tokens.length - 1)];
  }
  private expect(type: TokType, reason: string): Token {
    const t = this.peek();
    if (t.type !== type) {
      throw new AppError('E_QUERY_SYNTAX', { details: { reason, path: String(t.pos) } });
    }
    return this.next();
  }

  private parseOr(): Node {
    let left = this.parseAnd();
    while (this.isKeyword('or')) {
      this.next();
      const right = this.parseAnd();
      left = { kind: 'or', children: [left, right] };
    }
    return left;
  }

  private isKeyword(kw: string): boolean {
    const t = this.peek();
    if (t.type !== TokType.IDENT) return false;
    // 关键字也走归一化，"OR"/"ＯＲ" 同样会被识别，避免大小写/同形绕过。
    return normalizeText(t.value).toLowerCase() === kw;
  }

  private parseAnd(): Node {
    let left = this.parsePrimary();
    while (this.isKeyword('and')) {
      this.next();
      const right = this.parsePrimary();
      left = { kind: 'and', children: [left, right] };
    }
    return left;
  }

  private parsePrimary(): Node {
    if (++this.nodes > MAX_NODES) {
      throw new AppError('E_QUERY_TOO_COMPLEX', { details: { max: MAX_NODES, reason: 'node_budget_exceeded' } });
    }
    const t = this.peek();

    // 括号分组
    if (t.type === TokType.LPAREN) {
      this.next();
      const inner = this.parseOr();
      this.expect(TokType.RPAREN, 'missing_closing_paren');
      return inner;
    }

    // stream("name") / stream == "name"
    if (t.type === TokType.IDENT && normalizeText(t.value).toLowerCase() === 'stream') {
      return this.parseStreamRef();
    }

    if (t.type !== TokType.IDENT) {
      throw new AppError('E_QUERY_SYNTAX', { details: { reason: 'expected_field_name', path: String(t.pos) } });
    }

    // 字段名 -> 白名单列号
    const { field, index } = resolveField(t.value, t.pos);
    this.usedFields.add(field);
    this.next();

    // 后继是 "(" => 调用形式，当前只支持 stream("x")
    if (this.peek().type === TokType.LPAREN) {
      throw new AppError('E_QUERY_OPERATOR_NOT_ALLOWED', { details: { field, reason: 'function_call_not_supported' } });
    }

    const opTok = this.peek();
    if (opTok.type !== TokType.OP) {
      throw new AppError('E_QUERY_SYNTAX', { details: { reason: 'expected_operator', path: String(opTok.pos) } });
    }
    this.next();
    const op = opTok.value as CmpOp;

    const type = FIELD_TYPES[field] ?? 'string';
    if (!OPS_BY_TYPE[type].has(op)) {
      throw new AppError('E_QUERY_OPERATOR_NOT_ALLOWED', {
        details: { field, operator: op, reason: 'operator_not_valid_for_type', allowed: [...OPS_BY_TYPE[type]].join(',') },
      });
    }

    // 值：字符串 / 数字
    const valTok = this.peek();
    let paramValue: unknown;
    if (op === '~') {
      if (valTok.type !== TokType.STRING) {
        throw new AppError('E_QUERY_VALUE_INVALID', { details: { field, operator: op, reason: 'expected_string_literal', path: String(valTok.pos) } });
      }
      this.next();
      const pattern = valTok.value;
      assertSafePattern(pattern, field);
      const slot = this.pushParam(pattern);
      return { kind: 'compare', field, columnIndex: index, op, paramSlot: slot, pattern };
    }

    if (type === 'number') {
      if (valTok.type !== TokType.NUMBER) {
        throw new AppError('E_QUERY_VALUE_INVALID', { details: { field, operator: op, reason: 'expected_numeric_literal', path: String(valTok.pos) } });
      }
      this.next(); // 消费操作数 token
      const num = Number(valTok.value);
      if (!Number.isFinite(num)) {
        throw new AppError('E_QUERY_VALUE_INVALID', { details: { field, reason: 'not_finite' } });
      }
      paramValue = num;
    } else {
      if (valTok.type !== TokType.STRING) {
        throw new AppError('E_QUERY_VALUE_INVALID', { details: { field, operator: op, reason: 'expected_string_literal', path: String(valTok.pos) } });
      }
      this.next(); // 消费操作数 token
      paramValue = valTok.value;
    }
    const slot = this.pushParam(paramValue);
    return { kind: 'compare', field, columnIndex: index, op, paramSlot: slot };
  }

  private pushParam(v: unknown): number {
    if (this.params.length >= MAX_PARAMS) {
      throw new AppError('E_QUERY_TOO_COMPLEX', { details: { max: MAX_PARAMS, reason: 'param_budget_exceeded' } });
    }
    this.params.push(v);
    return this.params.length - 1;
  }

  private parseStreamRef(): Node {
    const kw = this.next(); // stream
    void kw;
    const t = this.peek();
    let value: string;
    if (t.type === TokType.LPAREN) {
      this.next();
      const arg = this.expect(TokType.STRING, 'expected_stream_string');
      value = arg.value;
      this.expect(TokType.RPAREN, 'missing_closing_paren');
    } else if (t.type === TokType.OP && (t.value === '==' || t.value === '=')) {
      this.next();
      const arg = this.expect(TokType.STRING, 'expected_stream_string');
      value = arg.value;
    } else {
      throw new AppError('E_QUERY_SYNTAX', { details: { reason: 'expected_stream_value', path: String(t.pos) } });
    }
    // 流名必须与写入侧同规格消毒，防止路径穿透被带进键空间。
    const clean = normalizeText(value);
    if (!/^[A-Za-z0-9_:-]+$/.test(clean)) {
      throw new AppError('E_QUERY_INJECTION', { details: { field: 'stream', reason: 'stream_name_charset_violation', path: String(t.pos) } });
    }
    return { kind: 'stream', stream: clean };
  }
}

/** ~ 模式的安全校验：长度有界，且拒绝灾难性回溯的嵌套量词。 */
function assertSafePattern(pattern: string, field: string): void {
  if (pattern.length > 64) {
    throw new AppError('E_QUERY_VALUE_INVALID', { details: { field, max: 64, reason: 'pattern_too_long' } });
  }
  // 只放行字母数字、下划线、连字符、星号、问号（作为通配语义，不当正则处理）。
  if (!/^[A-Za-z0-9_*?-]*$/.test(pattern)) {
    throw new AppError('E_QUERY_INJECTION', { details: { field, reason: 'illegal_pattern_metacharacter' } });
  }
  if (/(\*|\?){4,}/.test(pattern)) {
    throw new AppError('E_QUERY_TOO_COMPLEX', { details: { field, reason: 'pattern_wildcard_bomb' } });
  }
}

/** 解析入口：filter 字符串 -> AST + 参数。stream 由路由消毒后传入。 */
export function parseQuery(filter: string, stream: string): ParsedQuery {
  if (typeof filter !== 'string' || filter.length === 0) {
    throw new AppError('E_QUERY_SYNTAX', { details: { reason: 'empty_filter' } });
  }
  if (filter.length > 2048) {
    throw new AppError('E_QUERY_TOO_COMPLEX', { details: { max: 2048, reason: 'filter_too_long' } });
  }
  const tokens = tokenize(filter);
  if (tokens.length > MAX_TOKENS) {
    throw new AppError('E_QUERY_TOO_COMPLEX', { details: { max: MAX_TOKENS, reason: 'token_budget_exceeded' } });
  }
  return new Parser(tokens, stream).parse();
}

// ---------------------------------------------------------------------------
// 执行：AST -> 绑定参数 -> 谓词
// ---------------------------------------------------------------------------

/**
 * 列号 -> 取值。列号来自 FIELD_ALLOWLIST 的下标，
 * 解析期就固定下来，执行期只按列号取值，不再看字段名字符串。
 */
function columnValue(ev: DecodedEvent, columnIndex: number): string | number {
  switch (columnIndex) {
    case 0: return ev.stream;
    case 1: return ev.producer;
    case 2: return ev.kind;
    case 3: return ev.level;
    case 4: return ev.region;
    case 5: return ev.service;
    case 6: return ev.tenant;
    case 7: return ev.message;
    case 8: return ev.ts;
    case 9: return ev.value;
    case 10: return ev.lsn;
    default: return '';
  }
}

/** 通配匹配：* 任意长度，? 单字符。纯字符串算法，不构造 RegExp，杜绝 ReDoS。 */
function wildcardMatch(pattern: string, text: string): boolean {
  let p = 0;
  let t = 0;
  let star = -1;
  let mark = 0;
  while (t < text.length) {
    if (p < pattern.length && (pattern[p] === '?' || pattern[p] === text[t])) {
      p++;
      t++;
    } else if (p < pattern.length && pattern[p] === '*') {
      star = p++;
      mark = t;
    } else if (star >= 0) {
      p = star + 1;
      t = ++mark;
    } else {
      return false;
    }
  }
  while (p < pattern.length && pattern[p] === '*') p++;
  return p === pattern.length;
}

/**
 * 把 AST 编译成闭包谓词。
 * 参数在此刻才绑定：eval 收到的 row 与 bound 参数都是「已消毒的 JS 值」，
 * 没有任何字符串拼接进入可执行结构。
 */
export function compilePredicate(ast: ParsedQuery): (ev: DecodedEvent) => boolean {
  const params = ast.params;
  const compile = (node: Node): ((ev: DecodedEvent) => boolean) => {
    switch (node.kind) {
      case 'stream':
        return (ev) => ev.stream === node.stream;
      case 'and': {
        const subs = node.children.map(compile);
        return (ev) => subs.every((f) => f(ev));
      }
      case 'or': {
        const subs = node.children.map(compile);
        return (ev) => subs.some((f) => f(ev));
      }
      case 'time_range':
        return (ev) => ev.ts >= node.fromTs && ev.ts <= node.toTs;
      case 'compare': {
        const p = params[node.paramSlot];
        const col = node.columnIndex;
        switch (node.op) {
          case '==': return (ev) => columnValue(ev, col) === p;
          case '!=': return (ev) => columnValue(ev, col) !== p;
          case '>': return (ev) => (columnValue(ev, col) as number) > (p as number);
          case '>=': return (ev) => (columnValue(ev, col) as number) >= (p as number);
          case '<': return (ev) => (columnValue(ev, col) as number) < (p as number);
          case '<=': return (ev) => (columnValue(ev, col) as number) <= (p as number);
          case '~': return (ev) => wildcardMatch(node.pattern as string, String(columnValue(ev, col)));
        }
      }
    }
  };
  return compile(ast.root);
}

export interface QueryPlan {
  ast: ParsedQuery;
  predicate: (ev: DecodedEvent) => boolean;
  usedFields: string[];
  /** 从 AST 里抽出时间窗，给存储层做 LSN 区间裁剪。 */
  timeWindow: { fromTs: number | null; toTs: number | null };
}

/** 解析 + 编译。返回给 server 的查询计划。 */
export function planQuery(filter: string, stream: string): QueryPlan {
  const ast = parseQuery(filter, stream);
  const predicate = compilePredicate(ast);
  const { fromTs, toTs } = extractTimeWindow(ast);
  return { ast, predicate, usedFields: ast.usedFields, timeWindow: { fromTs, toTs } };
}

/**
 * 从 AST 提取 AND 串联的时间比较，交由存储层裁剪 LSN 区间。
 * 只处理最外层的 AND（最常见也最可裁剪）；OR 里的时间条件退化为不裁剪，
 * 由逐条过滤兜底，保证结果正确性优先于性能。
 */
function extractTimeWindow(ast: ParsedQuery): { fromTs: number | null; toTs: number } {
  let fromTs: number | null = null;
  let toTs: number | null = null;

  const visit = (node: Node): void => {
    if (node.kind === 'and') {
      node.children.forEach(visit);
      return;
    }
    if (node.kind === 'compare' && node.field === 'ts') {
      const p = ast.params[node.paramSlot] as number;
      if (node.op === '>=' || node.op === '>') fromTs = fromTs === null ? p : Math.max(fromTs, p);
      if (node.op === '<=' || node.op === '<') toTs = toTs === null ? p : Math.min(toTs, p);
      if (node.op === '==') { fromTs = fromTs === null ? p : Math.max(fromTs, p); toTs = toTs === null ? p : Math.min(toTs, p); }
    }
  };
  visit(ast.root);

  // 半个窗口会导致扫全量；这里给一个明确的默认上限窗口语义。
  if (fromTs !== null && fromTs < 0) fromTs = 0;
  return { fromTs, toTs: toTs === null ? Number.MAX_SAFE_INTEGER : toTs };
}
