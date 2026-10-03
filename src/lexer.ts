/**
 * lexer.ts — token definitions for the log query language.
 *
 * Chevrotain requires longest-match-wins ordering, so the token list below is
 * ordered deliberately: `LSQB` before `LBRACKET`, `TO` before the identifier
 * rule, and the phrase operators ahead of everything they can shadow.
 */

import { createToken, Lexer } from "../vendor/chevrotain/src/api.js";
import type { ILexingError, IToken } from "@chevrotain/types";

export const QueryToken = {
  LParen: createToken({ name: "LParen", pattern: /\(/ }),
  RParen: createToken({ name: "RParen", pattern: /\)/ }),
  LSqb: createToken({ name: "LSqb", pattern: /\[/ }),
  RSqb: createToken({ name: "RSqb", pattern: /\]/ }),
  Colon: createToken({ name: "Colon", pattern: /:/ }),
  // range operator forms: inclusive, exclusive, and open-ended
  Gte: createToken({ name: "Gte", pattern: />=/ }),
  Lte: createToken({ name: "Lte", pattern: /<=/ }),
  Gt: createToken({ name: "Gt", pattern: />/ }),
  Lt: createToken({ name: "Lt", pattern: /</ }),
  /** the `TO` keyword inside a range, e.g. [a TO b] */
  To: createToken({ name: "To", pattern: /TO(?=\s)/ }),
  LBrace: createToken({ name: "LBrace", pattern: /\{/ }),
  RBrace: createToken({ name: "RBrace", pattern: /\}/ }),
  Star: createToken({ name: "Star", pattern: /\*/ }),
  // keywords are anchored so `android` stays a plain term
  And: createToken({ name: "And", pattern: /AND(?=\s|$|\))/ }),
  Or: createToken({ name: "Or", pattern: /OR(?=\s|$|\))/ }),
  Not: createToken({ name: "Not", pattern: /NOT(?=\s|$|\))/ }),
  // quoted phrase: greedy across spaces, positions recorded for phrase match
  Phrase: createToken({
    name: "Phrase",
    pattern: /"[^"]*"/,
    line_breaks: false,
  }),
  /**
   * Bare word, or a whole `field:value` clause including range and comparison
   * forms: `error`, `level:error`, `latency:[100 TO 500]`, `status:>=500`.
   * Matching the clause as one token keeps field/value splitting out of the
   * grammar, so `finalizeNode` decides what the value means.
   */
  Identifier: createToken({
    name: "Identifier",
    pattern:
      /[A-Za-z0-9_][A-Za-z0-9_./@\-]*(?::(?:\{[^}]*\}|\[[^\]]*\]|>=?\s?-?[0-9.]+|<=?\s?-?[0-9.]+|"[^"]*"|=[A-Za-z0-9_./\-]+|[A-Za-z0-9_./\-]+))?/,
  }),
  Whitespace: createToken({
    name: "Whitespace",
    pattern: /\s+/,
    line_breaks: true,
    group: Lexer.SKIPPED,
  }),
} as const;

export type QueryTokenName = keyof typeof QueryToken;

/** All tokens in match-priority order. */
export const allTokens = Object.values(QueryToken);

/** Chevrotain lexer with full position tracking for error messages. */
export const queryLexer = new Lexer(allTokens, {
  positionTracking: "full",
  ensureOptimizations: false,
  skipValidations: true,
});

export interface TokenizeOk {
  ok: true;
  tokens: IToken[];
}

export interface PositionedError {
  message: string;
  offset: number;
  line: number;
  column: number;
}

export interface TokenizeErr {
  ok: false;
  error: PositionedError;
}

export type TokenizeResult = TokenizeOk | TokenizeErr;

/** Convert a char offset into a 1-based line/column for display. */
export function offsetToLineCol(
  text: string,
  offset: number,
): { line: number; column: number } {
  let line = 1;
  let lastBreak = -1;
  for (let i = 0; i < offset && i < text.length; i++) {
    if (text.charCodeAt(i) === 10) {
      line++;
      lastBreak = i;
    }
  }
  return { line, column: offset - lastBreak };
}

/** Render a source excerpt with a caret under the offending offset. */
export function excerpt(text: string, offset: number, width = 32): string {
  const start = Math.max(0, offset - width);
  const end = Math.min(text.length, offset + width);
  const slice = text.slice(start, end);
  const caret = offset - start;
  const prefix = start > 0 ? "..." : "";
  const suffix = end < text.length ? "..." : "";
  return `${prefix}${slice}${suffix}\n${" ".repeat(prefix.length + caret)}^`;
}

/**
 * Tokenize a query. On failure returns a positioned error rather than throwing,
 * so callers can surface the offset directly.
 */
export function tokenizeQuery(input: string): TokenizeResult {
  const lexResult = queryLexer.tokenize(input);
  if (lexResult.errors.length > 0) {
    const first = lexResult.errors[0]!;
    const offset = first.offset ?? 0;
    const { line, column } = offsetToLineCol(input, offset);
    return {
      ok: false,
      error: {
        message: first.message,
        offset,
        line,
        column,
      },
    };
  }
  return { ok: true, tokens: lexResult.tokens };
}
