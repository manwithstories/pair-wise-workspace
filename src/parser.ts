/**
 * parser.ts — query grammar and AST construction.
 *
 * Grammar:
 *
 *   expression  := orExpr EOF
 *   orExpr      := andExpr (OR andExpr)*
 *   andExpr     := unary (AND? unary)*          // adjacent clauses imply AND
 *   unary       := NOT* primary
 *   primary     := '(' expression ')'
 *               | Identifier                    // bare word, field:value, field:[a TO b]
 *               | Phrase                        // "two words"
 *
 * Chevrotain is driven in embedded-actions mode: each rule returns the AST
 * fragment it built, so no CST is materialised. Note two chevrotain
 * requirements that the implementation below depends on:
 *   - `performSelfAnalysis()` must run before the first parse;
 *   - tokens are supplied by assigning `parser.input`, not as an argument.
 */

import {
  createTokenInstance,
  EmbeddedActionsParser,
  EOF,
} from "../vendor/chevrotain/src/api.js";
import type { IToken, TokenType } from "@chevrotain/types";
import type { MixedInParser } from "../vendor/chevrotain/src/parse/parser/traits/parser_traits.js";
import { QueryToken, tokenizeQuery, offsetToLineCol } from "./lexer.js";

/* ------------------------------------------------------------------ *
 * AST
 * ------------------------------------------------------------------ */

export type QueryNode =
  | MatchAllNode
  | FieldTermNode
  | PhraseNode
  | RangeNode
  | AndNode
  | OrNode
  | NotNode;

export interface MatchAllNode {
  kind: "matchAll";
}

/** Exact-term filter, the unit that gets pushed into posting-list intersection. */
export interface FieldTermNode {
  kind: "fieldTerm";
  field: string;
  term: string;
  offset: number;
}

/** Terms must appear consecutively and in order. */
export interface PhraseNode {
  kind: "phrase";
  field: string;
  terms: string[];
  offset: number;
}

/** Numeric range, evaluated against the segment's sorted numeric column. */
export interface RangeNode {
  kind: "range";
  field: string;
  lo: number | null;
  hi: number | null;
  loInclusive: boolean;
  hiInclusive: boolean;
  offset: number;
}

export interface AndNode {
  kind: "and";
  left: QueryNode;
  right: QueryNode;
}

export interface OrNode {
  kind: "or";
  left: QueryNode;
  right: QueryNode;
}

export interface NotNode {
  kind: "not";
  operand: QueryNode;
}

/* ------------------------------------------------------------------ *
 * Errors
 * ------------------------------------------------------------------ */

export interface ParseError {
  message: string;
  offset: number;
  line: number;
  column: number;
  excerpt: string;
}

export type ParseResult =
  | { ok: true; ast: QueryNode; tokens: IToken[] }
  | { ok: false; error: ParseError };

/* ------------------------------------------------------------------ *
 * Internal parse-time shapes (pre field-splitting)
 * ------------------------------------------------------------------ */

interface RawIdent {
  readonly kind: "rawIdent";
  readonly image: string;
  readonly offset: number;
}

interface RawPhrase {
  readonly kind: "rawPhrase";
  readonly terms: string[];
  readonly offset: number;
}

type RawNode =
  | RawIdent
  | RawPhrase
  | { kind: "and"; left: RawNode; right: RawNode }
  | { kind: "or"; left: RawNode; right: RawNode }
  | { kind: "not"; operand: RawNode };

/* ------------------------------------------------------------------ *
 * Parser
 * ------------------------------------------------------------------ */

const T = QueryToken;

function parserVocabulary(): TokenType[] {
  return Object.values(QueryToken).filter((t) => t !== T.Whitespace) as TokenType[];
}

/**
 * Grammar rule implementations, declared so the chevrotain DSL resolves.
 * chevrotain mixes these traits into the parser at runtime.
 */
type QueryRule = ((this: QueryDsl) => RawNode) & {
  ruleName: string;
  originalGrammarAction: Function;
};

interface QueryDsl extends MixedInParser {
  expression: QueryRule;
  orExpr: QueryRule;
  andExpr: QueryRule;
  unary: QueryRule;
  primary: QueryRule;
}

class QueryParser extends EmbeddedActionsParser {
  constructor(private readonly defaultField: string) {
    super(parserVocabulary(), {
      recoveryEnabled: false,
      skipValidations: true,
      outputCst: false,
    });

    // chevrotain's DSL methods must be invoked with the parser as `this`;
    // arrow functions keep `dsl` as the receiver, which is what the mixed-in
    // traits expect.
    //
    // Every repetition below carries an explicit GATE. Adjacency alone (implicit
    // AND) gives chevrotain no lookahead to key on, and a bare MANY on such a
    // production either re-enters forever or stops before the operator.
    const dsl = this as unknown as QueryDsl;

    dsl.RULE("expression", (): RawNode => dsl.ACTION(() => dsl.SUBRULE(dsl.orExpr)));

    dsl.RULE("orExpr", (): RawNode => {
      const parts: RawNode[] = [dsl.SUBRULE(dsl.andExpr)];
      dsl.MANY({
        GATE: () => dsl.LA(1).tokenTypeIdx === T.Or.tokenTypeIdx,
        DEF: () => {
          dsl.CONSUME(T.Or);
          parts.push(dsl.SUBRULE2(dsl.andExpr));
        },
      });
      return dsl.ACTION(() => leftFold(parts, "or"));
    });

    dsl.RULE("andExpr", (): RawNode => {
      const parts: RawNode[] = [dsl.SUBRULE(dsl.unary)];
      // explicit AND, or an implicit AND when clauses are simply adjacent
      dsl.MANY({
        GATE: () => startsClause(dsl.LA(1)),
        DEF: () => {
          if (dsl.LA(1).tokenTypeIdx === T.And.tokenTypeIdx) dsl.CONSUME(T.And);
          parts.push(dsl.SUBRULE2(dsl.unary));
        },
      });
      return dsl.ACTION(() => leftFold(parts, "and"));
    });

    dsl.RULE("unary", (): RawNode => {
      let negated = false;
      dsl.MANY({
        GATE: () => dsl.LA(1).tokenTypeIdx === T.Not.tokenTypeIdx,
        DEF: () => {
          dsl.CONSUME(T.Not);
          negated = true;
        },
      });
      const inner = dsl.SUBRULE(dsl.primary);
      return dsl.ACTION(() => (negated ? { kind: "not", operand: inner } : inner));
    });

    dsl.RULE("primary", (): RawNode =>
      dsl.ACTION(() => {
        const la = dsl.LA(1);
        if (la.tokenTypeIdx === T.LParen.tokenTypeIdx) {
          dsl.CONSUME(T.LParen);
          const inner = dsl.SUBRULE(dsl.expression);
          dsl.CONSUME(T.RParen);
          return inner;
        }
        if (la.tokenTypeIdx === T.Phrase.tokenTypeIdx) {
          const tok = dsl.CONSUME(T.Phrase);
          const body = tok.image.slice(1, -1).trim();
          return {
            kind: "rawPhrase",
            terms: body.length > 0 ? body.split(/\s+/) : [],
            offset: tok.startOffset ?? 0,
          } satisfies RawPhrase;
        }
        const tok = dsl.CONSUME(T.Identifier);
        return {
          kind: "rawIdent",
          image: tok.image,
          offset: tok.startOffset ?? 0,
        } satisfies RawIdent;
      }),
    );

    (this as unknown as MixedInParser).performSelfAnalysis();
  }

  /**
   * Run the grammar. Tokens must be assigned to `input` (chevrotain's
   * contract) and an explicit EOF token terminates the stream.
   */
  run(tokens: readonly IToken[]): { ok: true; ast: RawNode } | { ok: false; errors: unknown[] } {
    const withEof = tokens.slice();
    const last = withEof[withEof.length - 1];
    const endOffset = last?.endOffset ?? 0;
    const endLine = last?.endLine ?? 1;
    const endColumn = last?.endColumn ?? 1;
    withEof.push(
      createTokenInstance(EOF, "", endOffset, endOffset, endLine, endColumn, endLine, endColumn),
    );

    const dsl = this as unknown as QueryDsl;
    dsl.input = withEof;
    const ast = dsl.expression();
    if (dsl.errors.length > 0) {
      return { ok: false, errors: dsl.errors };
    }
    return { ok: true, ast };
  }
}

/** True when the lookahead token can begin a clause (incl. an AND operator). */
function startsClause(la: IToken): boolean {
  const i = la.tokenTypeIdx;
  return (
    i === T.Identifier.tokenTypeIdx ||
    i === T.Phrase.tokenTypeIdx ||
    i === T.LParen.tokenTypeIdx ||
    i === T.Not.tokenTypeIdx ||
    i === T.And.tokenTypeIdx
  );
}

/** Left-associative fold so `a AND b AND c` reads naturally in the AST. */
function leftFold(parts: readonly RawNode[], op: "and" | "or"): RawNode {
  let acc = parts[0] as RawNode;
  for (let i = 1; i < parts.length; i++) {
    const right = parts[i] as RawNode;
    acc = op === "and"
      ? { kind: "and", left: acc, right }
      : { kind: "or", left: acc, right };
  }
  return acc;
}

/* ------------------------------------------------------------------ *
 * Parsing entry point
 * ------------------------------------------------------------------ */

let cached: { parser: QueryParser; field: string } | null = null;

function getParser(defaultField: string): QueryParser {
  if (cached === null || cached.field !== defaultField) {
    cached = { parser: new QueryParser(defaultField), field: defaultField };
  }
  return cached.parser;
}

/** Render a source excerpt with a caret under `offset`. */
export function excerptAt(text: string, offset: number, width = 28): string {
  const start = Math.max(0, offset - width);
  const end = Math.min(text.length, offset + width);
  const prefix = start > 0 ? "..." : "";
  const suffix = end < text.length ? "..." : "";
  const caret = " ".repeat(prefix.length + (offset - start));
  return `${prefix}${text.slice(start, end)}${suffix}\n${caret}^`;
}

/**
 * Parse a query into an AST. Never throws: lexer and grammar failures come back
 * as a positioned error so callers can point at the offending character.
 */
export function parseQuery(input: string, defaultField = "_all"): ParseResult {
  const lex = tokenizeQuery(input);
  if (!lex.ok) {
    return {
      ok: false,
      error: {
        message: `lex error: ${lex.error.message}`,
        offset: lex.error.offset,
        line: lex.error.line,
        column: lex.error.column,
        excerpt: excerptAt(input, lex.error.offset),
      },
    };
  }

  const parser = getParser(defaultField);
  const result = parser.run(lex.tokens);
  if (!result.ok) {
    const first = result.errors[0] as
      | { message?: string; token?: IToken; previousToken?: IToken }
      | undefined;
    const token = first?.token ?? first?.previousToken;
    const offset = token?.startOffset ?? 0;
    const { line, column } = offsetToLineCol(input, offset);
    return {
      ok: false,
      error: {
        message: first?.message ?? "parse error",
        offset,
        line,
        column,
        excerpt: excerptAt(input, offset),
      },
    };
  }

  return {
    ok: true,
    ast: finalizeNode(result.ast, defaultField),
    tokens: lex.tokens,
  };
}

/* ------------------------------------------------------------------ *
 * Raw node -> AST (split field:value, recognise ranges)
 * ------------------------------------------------------------------ */

function finalizeNode(node: RawNode, df: string): QueryNode {
  switch (node.kind) {
    case "and":
      return { kind: "and", left: finalizeNode(node.left, df), right: finalizeNode(node.right, df) };
    case "or":
      return { kind: "or", left: finalizeNode(node.left, df), right: finalizeNode(node.right, df) };
    case "not":
      return { kind: "not", operand: finalizeNode(node.operand, df) };
    case "rawPhrase":
      return { kind: "phrase", field: df, terms: node.terms, offset: node.offset };
    case "rawIdent":
      return splitIdentifier(node.image, node.offset, df);
  }
}

function splitIdentifier(image: string, offset: number, df: string): QueryNode {
  const colon = image.indexOf(":");
  if (colon < 0) return { kind: "fieldTerm", field: df, term: image, offset };

  const field = image.slice(0, colon);
  const raw = image.slice(colon + 1);
  if (raw.startsWith("[") || raw.startsWith("{")) {
    return parseRange(field, raw, offset);
  }
  // `status:>=500` and friends are ranges with one bound
  if (/^(>=|<=|>|<|=)/.test(raw)) {
    return parseComparison(field, raw, offset);
  }
  // field:"two words" -> phrase node scoped to that field
  if (raw.length >= 2 && raw.startsWith('"') && raw.endsWith('"')) {
    const body = raw.slice(1, -1).trim();
    return {
      kind: "phrase",
      field,
      terms: body.length > 0 ? body.split(/\s+/) : [],
      offset,
    };
  }
  return { kind: "fieldTerm", field, term: raw, offset };
}

/** Single-bound comparison such as `>=500`, `<500`, `=500`. */
function parseComparison(field: string, raw: string, offset: number): RangeNode {
  if (raw.startsWith(">=")) return { kind: "range", field, lo: toNumber(raw.slice(2)), hi: null, loInclusive: true, hiInclusive: true, offset };
  if (raw.startsWith("<=")) return { kind: "range", field, lo: null, hi: toNumber(raw.slice(2)), loInclusive: true, hiInclusive: true, offset };
  if (raw.startsWith(">")) return { kind: "range", field, lo: toNumber(raw.slice(1)), hi: null, loInclusive: false, hiInclusive: true, offset };
  if (raw.startsWith("<")) return { kind: "range", field, lo: null, hi: toNumber(raw.slice(1)), loInclusive: true, hiInclusive: false, offset };
  const v = toNumber(raw.slice(1));
  return { kind: "range", field, lo: v, hi: v, loInclusive: true, hiInclusive: true, offset };
}

function parseRange(field: string, raw: string, offset: number): RangeNode {
  const open = raw.startsWith("[") ? "[" : "{";
  const close = raw.endsWith("]") ? "]" : "}";
  let loInclusive = open === "[";
  let hiInclusive = close === "]";
  const body = raw.slice(1, -1);

  const parts = body.split(/\s+TO\s+/i);
  let lo: number | null = null;
  let hi: number | null = null;

  if (parts.length >= 2) {
    lo = toNumber(parts[0] ?? "");
    hi = toNumber(parts[1] ?? "");
  } else {
    const single = body.trim();
    if (single.startsWith(">=")) {
      lo = toNumber(single.slice(2));
      loInclusive = true;
    } else if (single.startsWith("<=")) {
      hi = toNumber(single.slice(2));
      hiInclusive = true;
    } else if (single.startsWith(">")) {
      lo = toNumber(single.slice(1));
      loInclusive = false;
    } else if (single.startsWith("<")) {
      hi = toNumber(single.slice(1));
      hiInclusive = false;
    } else if (single.startsWith("=")) {
      lo = toNumber(single.slice(1));
      hi = lo;
      loInclusive = true;
      hiInclusive = true;
    } else if (single !== "") {
      hi = toNumber(single);
    }
  }

  return { kind: "range", field, lo, hi, loInclusive, hiInclusive, offset };
}

function toNumber(text: string): number | null {
  const t = text.trim();
  if (t === "") return null;
  const n = Number(t);
  return Number.isFinite(n) ? n : null;
}

/** Pretty-print an AST as an indented s-expression (used by the demo). */
export function formatAst(node: QueryNode, indent = 0): string {
  const pad = "  ".repeat(indent);
  switch (node.kind) {
    case "matchAll":
      return `${pad}*`;
    case "fieldTerm":
      return `${pad}${node.field}:${node.term}`;
    case "phrase":
      return `${pad}${node.field}:"${node.terms.join(" ")}"`;
    case "range": {
      const lo = node.lo === null ? "*" : `${node.loInclusive ? "" : ">"}${node.lo}`;
      const hi = node.hi === null ? "*" : `${node.hiInclusive ? "" : "<"}${node.hi}`;
      return `${pad}${node.field}:[${lo} TO ${hi}]`;
    }
    case "and":
      return `${pad}AND\n${formatAst(node.left, indent + 1)}\n${formatAst(node.right, indent + 1)}`;
    case "or":
      return `${pad}OR\n${formatAst(node.left, indent + 1)}\n${formatAst(node.right, indent + 1)}`;
    case "not":
      return `${pad}NOT\n${formatAst(node.operand, indent + 1)}`;
  }
}

export { QueryToken };
