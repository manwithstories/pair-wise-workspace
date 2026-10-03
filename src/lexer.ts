/**
 * src/lexer.ts —— 查询语言词法层
 *
 * 职责：把 SRE 排查语句（`level:error AND service:auth AND message:"connection timeout"`）
 * 切成带精确位置信息的 token 流，非法字符在词法阶段就抛出带 offset/line/column 的错误。
 *
 * 设计要点：
 *  1. lexer 实例全局复用（chevrotain 的 Lexer 构造有固定开销，查询是高频路径）。
 *  2. `positionTracking: "full"`，AST 与错误都能回报 token 位置，满足"解析失败给 token 位置错误"。
 *  3. token 顺序即优先级：空白 → 括号/标点 → 关键字 → 引号短语 → 普通词。
 *     关键字必须排在普通词之前，`TO` 才能在 `[a TO b]` 里被识别出来。
 */

import { createToken, Lexer, type IToken, type TokenType } from "../vendor/chevrotain/index.js";

/** 源码位置（1-based 行/列，0-based offset），与 chevrotain 保持一致。 */
export interface SourceLocation {
  offset: number;
  line: number;
  column: number;
}

export interface QueryErrorOptions {
  /** 出错阶段：lex（词法）/ parse（语法）/ plan（语义，如未知字段） */
  phase: "lex" | "parse" | "plan";
  location?: SourceLocation | null;
  /** 出错处原文片段，便于终端直接定位 */
  token?: string | null;
}

/** 查询相关错误的统一基类，携带位置信息。 */
export class QueryError extends Error {
  readonly phase: "lex" | "parse" | "plan";
  readonly location: SourceLocation | null;
  readonly token: string | null;

  constructor(message: string, options: QueryErrorOptions) {
    super(message);
    this.name = "QueryError";
    this.phase = options.phase;
    this.location = options.location ?? null;
    this.token = options.token ?? null;
  }

  /** 终端友好的一行错误：位置 + 阶段 + 描述。 */
  describe(): string {
    if (this.location === null) return `${this.name}: ${this.message}`;
    const { line, column, offset } = this.location;
    const tok = this.token ? `，token=${JSON.stringify(this.token)}` : "";
    return `${this.name}(${this.phase}) @ line ${line}:${column} (offset ${offset})${tok} —— ${this.message}`;
  }
}

/** 词法/语法错误：`level: AND` `[a TO]` `foo("bar` 之类。 */
export class QuerySyntaxError extends QueryError {
  constructor(message: string, options: QueryErrorOptions) {
    super(message, options);
    this.name = "QuerySyntaxError";
  }
}

/** offset -> line/column，词法异常路径的兜底定位。 */
export function offsetToLocation(text: string, offset: number): SourceLocation {
  const clamped = Math.max(0, Math.min(offset, text.length));
  let line = 1;
  let lineStart = 0;
  for (let i = 0; i < clamped; i++) {
    if (text.charCodeAt(i) === 10) {
      line++;
      lineStart = i + 1;
    }
  }
  return { offset: clamped, line, column: clamped - lineStart + 1 };
}

/* ------------------------------------------------------------------ *
 * Token 定义
 * ------------------------------------------------------------------ */

const Whitespace = createToken({ name: "Whitespace", pattern: /[ \t\n\r]+/, line_breaks: true, group: Lexer.SKIPPED });
const LParen = createToken({ name: "LParen", pattern: /\(/ });
const RParen = createToken({ name: "RParen", pattern: /\)/ });
const LBracket = createToken({ name: "LBracket", pattern: /\[/ });
const RBracket = createToken({ name: "RBracket", pattern: /\]/ });
const Colon = createToken({ name: "Colon", pattern: /:/ });

// 关键字必须早于 Word 声明；`!` 作为 NOT 的简写。
const And = createToken({ name: "And", pattern: /\bAND\b/i });
const Or = createToken({ name: "Or", pattern: /\bOR\b/i });
const Not = createToken({ name: "Not", pattern: /\bNOT\b|!/ });
const To = createToken({ name: "To", pattern: /\bTO\b/i });
/** 范围通配：`[100 TO *]`，因此 `*` 必须从普通词里排除。 */
const Star = createToken({ name: "Star", pattern: /\*/ });

/** 引号短语：`message:"connection timeout"`，支持 \" \\ \n 转义。 */
const Phrase = createToken({
  name: "Phrase",
  pattern: /"(?:[^"\\\n]|\\.)*"/,
  line_breaks: false,
});

/** 普通词：字段名与字面量共用（`level`、`error`、`trace_id`）。 */
const Word = createToken({
  name: "Word",
  pattern: /[^\s:()[\]"*\\]+/,
  line_breaks: false,
});

/**
 * 词法 token 顺序即优先级。
 * 同一个数组同时供 Lexer 使用、以及作为 parser 的 tokenVocabulary——
 * parser 用 token 名做 CONSUME，必须能查到 TokenType 实例。
 */
export const QUERY_TOKENS: TokenType[] = [
  Whitespace,
  LParen,
  RParen,
  LBracket,
  RBracket,
  Colon,
  And,
  Or,
  Not,
  To,
  Star,
  Phrase,
  Word,
];

/**
 * token 实例表（供 parser 的 CONSUME 使用；CONSUME 要 TokenType 实例而不是名字）。
 */
export const TOKENS = {
  LParen,
  RParen,
  LBracket,
  RBracket,
  Colon,
  Star,
  And,
  Or,
  Not,
  To,
  Phrase,
  Word,
} as const;

/**
 * parser 的 token 词典（name -> TokenType）。
 * 必须传字典而不是数组：chevrotain 内部会把数组 clone 成带 "EOF" 字符串属性的伪对象，
 * 再对它 `values()` 时会把字符串当 token 处理而崩（tokens.js augmentTokenTypes）。
 */
export const TOKEN_VOCABULARY: Record<string, TokenType> = Object.fromEntries(
  QUERY_TOKENS.map((t) => [t.name, t]),
);

export const TokenNames = {
  LParen: LParen.name,
  RParen: RParen.name,
  LBracket: LBracket.name,
  RBracket: RBracket.name,
  Colon: Colon.name,
  Star: Star.name,
  And: And.name,
  Or: Or.name,
  Not: Not.name,
  To: To.name,
  Phrase: Phrase.name,
  Word: Word.name,
} as const;

const lexer = new Lexer(QUERY_TOKENS, {
  positionTracking: "full",
  // 出错立即抛出：查询串很短，容错续跑只会让错误位置更模糊。
  recoveryEnabled: false,
});

/**
 * 取词法 token 的归一化值：短语去掉首尾引号并反转义。
 * 容忍 undefined —— chevrotain 的文法记录阶段会以空输入跑一遍规则体。
 */
export function tokenValue(token: IToken | undefined): string {
  if (token === undefined) return "";
  const image = token.image;
  if (token.tokenType?.name === TokenNames.Phrase) {
    return image.slice(1, -1).replace(/\\(.)/g, "$1");
  }
  return image;
}

/** 把 chevrotain 的 token 位置转成引擎统一的 SourceLocation。 */
export function tokenLocation(token: IToken | undefined): SourceLocation {
  if (token === undefined) return { offset: 0, line: 1, column: 1 };
  return { offset: token.startOffset ?? 0, line: token.startLine ?? 0, column: token.startColumn ?? 0 };
}

export interface LexResult {
  tokens: IToken[];
  text: string;
}

/**
 * 词法分析。失败抛 QuerySyntaxError，带出错字符的 offset/line/column。
 */
export function tokenize(text: string): LexResult {
  if (typeof text !== "string") {
    throw new QuerySyntaxError("查询必须是字符串", { phase: "lex" });
  }
  if (text.trim().length === 0) {
    throw new QuerySyntaxError("查询为空", { phase: "lex", location: { offset: 0, line: 1, column: 1 } });
  }

  let result;
  try {
    result = lexer.tokenize(text);
  } catch (err) {
    // recoveryEnabled:false 时 chevrotain 直接抛错，message 里带 offset。
    const anyErr = err as { message?: string; offset?: number };
    const offset = anyErr.offset ?? 0;
    throw new QuerySyntaxError(anyErr.message ?? "词法分析失败", {
      phase: "lex",
      location: offsetToLocation(text, offset),
      token: text.slice(offset, offset + 1),
    });
  }

  const first = result.errors[0];
  if (first !== undefined) {
    const offset = first.offset ?? 0;
    const length = typeof first.length === "number" && first.length > 0 ? first.length : 1;
    throw new QuerySyntaxError(first.message, {
      phase: "lex",
      location: { offset, line: first.line ?? 1, column: first.column ?? 1 },
      token: text.slice(offset, offset + length),
    });
  }

  return { tokens: result.tokens, text };
}

/**
 * 从任意 chevrotain token 反推位置。
 * EOF token 的 offset/line/column 是 NaN，这里视为"没有位置"（由调用方兜底）。
 */
export function locationOf(token: { startOffset?: number; startLine?: number; startColumn?: number } | undefined | null): SourceLocation | null {
  if (!token || typeof token.startOffset !== "number" || Number.isNaN(token.startOffset)) return null;
  return { offset: token.startOffset, line: token.startLine ?? 0, column: token.startColumn ?? 0 };
}
