/**
 * src/parser.ts —— 查询语法层（chevrotain）
 *
 * 语法（递归下降）：
 *
 *   query       := orExpr EOF
 *   orExpr      := andExpr (OR andExpr)*
 *   andExpr     := notExpr ((AND)? notExpr)*        // 相邻条件隐式 AND
 *   notExpr     := NOT notExpr | primary
 *   primary     := '(' orExpr ')' | fieldQuery | phraseQuery | termQuery
 *   fieldQuery  := Field ':' (phrase | word | range)
 *   range       := '[' endpoint TO endpoint ']'
 *   endpoint    := word | phrase | '*'
 *   phraseQuery := phrase                          // 裸短语：`_all` 上的短语
 *   termQuery   := word                            // 裸词：`_all` 上的 term
 *
 * 用 EmbeddedActionsParser（不产出 CST）：规则直接 return 语义值，父规则拿 SUBRULE
 * 的返回值边解析边折叠 AST，省掉一次 CST 遍历。
 *
 * 两个必须知道的 chevrotain 细节：
 *  1. CstParser / EmbeddedActionsParser 的构造签名是 `(tokenVocabulary, config)`。
 *  2. `performSelfAnalysis()` 的文法记录阶段会以**空参数**执行一遍规则体，
 *     所以规则体不能依赖外部可变上下文，语义只能靠返回值传递。
 */

import {
  EOF,
  EmbeddedActionsParser,
  isRecognitionException,
  type IToken,
} from "../vendor/chevrotain/index.js";
import {
  QuerySyntaxError,
  TOKENS,
  TOKEN_VOCABULARY,
  TokenNames,
  locationOf,
  offsetToLocation,
  tokenize,
  tokenLocation,
  tokenValue,
  type SourceLocation,
} from "./lexer.js";

/* ------------------------------------------------------------------ *
 * AST
 * ------------------------------------------------------------------ */

/**
 * 字段匹配：`level:error`、`service:auth`。
 *
 * `quoted` 区分两种语义（SRE 排查里两者都用得到）：
 *  - `quoted: false` —— 字段**整体精确等于** value（level/service 这类枚举字段）。
 *  - `quoted: true`  —— 字段文本**包含该短语**（message:"connection timeout"）。
 */
export interface FieldTerm {
  kind: "field";
  field: string;
  value: string;
  quoted: boolean;
  loc: SourceLocation;
}

/** 字段存在性：`level:*`。 */
export interface ExistsNode {
  kind: "exists";
  field: string;
  loc: SourceLocation;
}

/** 裸词 / 裸短语：落在 `_all` 字段上。 */
export interface WildcardTerm {
  kind: "term" | "phrase";
  field: "_all";
  value: string;
  loc: SourceLocation;
}

/** 范围：`ts:[2026-10-01 TO 2026-10-03]`、`latency:[100 TO *]`；`*` 为无界端。 */
export interface RangeTerm {
  kind: "range";
  field: string;
  /** null 表示 `*`（无界） */
  lower: string | null;
  upper: string | null;
  lowerInclusive: boolean;
  upperInclusive: boolean;
  loc: SourceLocation;
}

export type LeafNode = FieldTerm | WildcardTerm | RangeTerm;

export interface BoolNode {
  kind: "and" | "or";
  children: QueryNode[];
  loc: SourceLocation;
}

export interface NotNode {
  kind: "not";
  child: QueryNode;
  loc: SourceLocation;
}

/** 恒真匹配（空节点列表归一化），下推层对这类条件直接短路放行。 */
export interface MatchAllNode {
  kind: "matchAll";
  loc: SourceLocation;
}

export type QueryNode = LeafNode | ExistsNode | BoolNode | NotNode | MatchAllNode;

/** 解析结果：AST + 归一化元信息。 */
export interface ParsedQuery {
  query: string;
  ast: QueryNode;
  /** AST 涉及的逻辑字段（下推层据此选 posting 链）。 */
  fields: string[];
  /** 是否含范围条件（可用于选择范围专用求值路径）。 */
  hasRange: boolean;
  /** 词法 token 数，用于诊断解析开销。 */
  tokenCount: number;
}

/* ------------------------------------------------------------------ *
 * 语法规则
 * ------------------------------------------------------------------ */

/** parser 直接消费 lexer 的 token（按 TokenType 名匹配，无需重复声明 token）。 */
const T = TokenNames;

/**
 * `this.LA(n)` 返回的是 IToken，不是 token 名；比较要用 `tokenType.name`。
 * 文法记录阶段 LA 可能返回 undefined，这里统一降级成空串。
 */
function laName(parser: EmbeddedActionsParser, howMuch: number): string {
  const tok = (parser as unknown as { LA: (n: number) => IToken | undefined }).LA(howMuch);
  return tok?.tokenType?.name ?? "";
}

/**
 * 规则方法表（对应 chevrotain 在实例上挂出的规则方法）。
 * 只声明类型，运行时由 `this.RULE(...)` 生成。
 */
interface RuleTable {
  query: () => QueryNode;
  OR_EXPR: () => QueryNode;
  AND_EXPR: () => QueryNode;
  NOT_EXPR: () => QueryNode;
  PRIMARY: () => QueryNode;
  RANGE: () => QueryNode;
  ENDPOINT: () => IToken;
}

class QueryParser extends EmbeddedActionsParser {
  constructor() {
    // vocabulary 传字典而非数组：chevrotain 内部会把数组 clone 成带 "EOF" 键的伪对象，
    // 再 values() 时把那个字符串当成 TokenType 处理而崩溃。
    super(TOKEN_VOCABULARY, {
      // chevrotain 的 IParserConfig 类型里没有 "name"，但构造器内部用它做诊断名
      ...({ name: "logQuery" } as Record<string, unknown>),
      // 默认开启的 resync 恢复会吞掉识别异常并返回 undefined，
      // 查询场景必须让错误直接冒泡，否则会拿到半成品 AST。
      recoveryEnabled: false,
    });

    /**
     * query := orExpr EOF
     *
     * 规则体里一律用 `this.XXX` 引用子规则（chevrotain 在 RULE() 时就把 SUBRULE
     * 换成了记录版本，前向局部变量在那时还是 undefined）。
     */
    this.RULE(
      "query",
      () => {
        const node = this.SUBRULE(this.OR_EXPR);
        this.CONSUME(EOF);
        return node;
      },
      // 即使 recoveryEnabled:false，chevrotain 对"最外层规则"的异常也会调用
      // recoveryValueFunc 并吞掉它（invokeRuleCatch 的 isFirstInvokedRule 分支），
      // 结果是返回 undefined 而不抛错。这里改成重新抛出，错误才能被 translateParserError 接住。
      { recoveryValueFunc: (e: unknown) => { throw e; } },
    );

    /** OR_EXPR := AND_EXPR (OR AND_EXPR)* —— 同层 OR 合并成一个 or 节点 */
    this.RULE("OR_EXPR", () => {
      const operands: QueryNode[] = [this.SUBRULE(this.AND_EXPR)];
      while (laName(this, 1) === T.Or) {
        this.CONSUME(TOKENS.Or);
        operands.push(this.SUBRULE(this.AND_EXPR));
      }
      return fold("or", operands);
    });

    /** AND_EXPR := NOT_EXPR ((AND)? NOT_EXPR)* —— 相邻条件隐式 AND */
    this.RULE("AND_EXPR", () => {
      const operands: QueryNode[] = [this.SUBRULE(this.NOT_EXPR)];
      for (;;) {
        if (laName(this, 1) === T.And) {
          this.CONSUME(TOKENS.And);
          operands.push(this.SUBRULE(this.NOT_EXPR));
          continue;
        }
        // 隐式 AND：紧跟一个 primary（'(' / 字段词 / 短语）
        const la = laName(this, 1);
        if (la === T.LParen || la === T.Word || la === T.Phrase) {
          operands.push(this.SUBRULE(this.NOT_EXPR));
          continue;
        }
        break;
      }
      return fold("and", operands);
    });

    /** NOT_EXPR := NOT NOT_EXPR | PRIMARY */
    this.RULE("NOT_EXPR", () => {
      if (laName(this, 1) === T.Not) {
        const notTok = this.LA(1);
        this.CONSUME(TOKENS.Not);
        const child = this.SUBRULE(this.NOT_EXPR);
        return { kind: "not", child, loc: tokenLocation(notTok) } as QueryNode;
      }
      return this.SUBRULE(this.PRIMARY);
    });

    /** PRIMARY := '(' OR_EXPR ')' | fieldQuery | phraseQuery | termQuery */
    this.RULE("PRIMARY", () => {
      if (laName(this, 1) === T.LParen) {
        this.CONSUME(TOKENS.LParen);
        const inner = this.SUBRULE(this.OR_EXPR);
        this.CONSUME(TOKENS.RParen);
        return inner;
      }

      if (laName(this, 1) === T.Phrase) {
        const tok = this.LA(1);
        this.CONSUME(TOKENS.Phrase);
        return phraseNode(tok);
      }

      // `field:值` 的分支判定用 lookahead：第二个 token 是 Colon 才走字段查询。
      // （不能用"先消费再判断"——那会让 Chevrotain 的 LL(k) 分析把裸词分支判为不可达。）
      if (laName(this, 2) === T.Colon) {
        const fieldTok = this.CONSUME(TOKENS.Word);
        this.CONSUME(TOKENS.Colon);

        if (laName(this, 1) === T.LBracket) {
          return this.SUBRULE(this.RANGE);
        }

        if (laName(this, 1) === T.Star) {
          // `level:*` —— 字段存在性（`*` 必须显式消费掉）
          this.CONSUME(TOKENS.Star);
          return existsNode(fieldTok);
        }

        const la = laName(this, 1);
        if (la !== T.Word && la !== T.Phrase) {
          // 缺字段值：`level:` / `level:AND`，显式消费 Word 让错误定位落在真正缺 token 处
          this.CONSUME(TOKENS.Word, {
            LABEL: "FieldValue",
            ERR_MSG: "字段缺少取值（期望 word 或 \"引号短语\"）",
          });
        }
        // 引号值 = 字段文本包含短语；裸值 = 字段整体精确等于
        const quoted = la === T.Phrase;
        const valueTok = this.CONSUME(quoted ? TOKENS.Phrase : TOKENS.Word);
        return fieldTerm(fieldTok, valueTok, quoted);
      }

      const word = this.LA(1);
      this.CONSUME(TOKENS.Word);
      return termNode(word);
    });

    /**
     * RANGE := '[' ENDPOINT TO ENDPOINT ']'
     *
     * 字段名不能用 SUBRULE 的 ARGS 传进来：chevrotain 的文法记录阶段会以空参数
     * 跑一遍规则体（那里 LA(1) 是 undefined），字段名会退化成空串。
     * 改为在 PRIMARY 里消费完 `field :` 之后读 LA(-1)（上一个 token 就是字段名）。
     */
    this.RULE("RANGE", () => {
      const fieldTok = this.LA(-1);
      this.CONSUME(TOKENS.LBracket);
      // chevrotain 要求同一规则内重复出现的 SUBRULE 用数字后缀区分唯一性
      const lowerTok = this.SUBRULE1(this.ENDPOINT);
      this.CONSUME(TOKENS.To);
      const upperTok = this.SUBRULE2(this.ENDPOINT);
      this.CONSUME(TOKENS.RBracket);
      return rangeNode(fieldTok, lowerTok, upperTok);
    });

    /** ENDPOINT := word | phrase | '*'（`*` 表示无界） */
    this.RULE("ENDPOINT", () => {
      const la = laName(this, 1);
      if (la === T.Star) return this.CONSUME(TOKENS.Star);
      if (la === T.Phrase) return this.CONSUME(TOKENS.Phrase);
      // 文法记录阶段 / 端点缺失：统一消费 Word，错误定位落在真正缺 token 处
      return this.CONSUME(TOKENS.Word);
    });

    // 文法自分析（LL(k) 可达性记录）只在构造时做一次，是查询路径的必要初始化。
    this.performSelfAnalysis();
  }
}

/* ------------------------------------------------------------------ *
 * 语义动作
 *
 * 单独抽出来有两个原因：
 *  1. chevrotain 的文法记录阶段会以空参数跑一遍规则体，LA(1) 可能取不到 token；
 *     这里对 undefined 全部降级为"占位位置"，不影响自分析。
 *  2. 语义动作集中在一处，规则体只负责消费 token。
 * ------------------------------------------------------------------ */

const PLACEHOLDER_LOC: SourceLocation = { offset: 0, line: 1, column: 1 };

/** token -> 归一化值，兼容文法记录阶段的 undefined。 */
function safeImage(tok: IToken | undefined): string {
  return tok === undefined ? "" : tokenValue(tok);
}

function safeLoc(tok: IToken | undefined): SourceLocation {
  return tok === undefined ? PLACEHOLDER_LOC : tokenLocation(tok);
}

function fieldTerm(fieldTok: IToken | undefined, valueTok: IToken | undefined, quoted: boolean): QueryNode {
  return {
    kind: "field",
    field: safeImage(fieldTok),
    value: safeImage(valueTok),
    quoted,
    loc: safeLoc(fieldTok),
  };
}

function existsNode(fieldTok: IToken | undefined): QueryNode {
  return { kind: "exists", field: safeImage(fieldTok), loc: safeLoc(fieldTok) };
}

function termNode(tok: IToken | undefined): QueryNode {
  return { kind: "term", field: "_all", value: safeImage(tok), loc: safeLoc(tok) };
}

function phraseNode(tok: IToken | undefined): QueryNode {
  return { kind: "phrase", field: "_all", value: safeImage(tok), loc: safeLoc(tok) };
}

/** 范围端点：`*` -> null（无界），word/phrase -> 归一化值。 */
function endpointNode(tok: IToken | undefined): string | null {
  if (tok === undefined || tok.tokenType?.name === T.Star) return null;
  return tokenValue(tok);
}

function rangeNode(fieldTok: IToken | undefined, lowerTok: IToken | undefined, upperTok: IToken | undefined): QueryNode {
  return {
    kind: "range",
    field: safeImage(fieldTok),
    lower: endpointNode(lowerTok),
    upper: endpointNode(upperTok),
    lowerInclusive: true,
    upperInclusive: true,
    loc: safeLoc(fieldTok),
  };
}

/** 单节点原样返回；多节点折叠成 and/or；空列表归一化成 matchAll。 */
function fold(kind: "and" | "or", nodes: QueryNode[]): QueryNode {
  if (nodes.length === 0) return { kind: "matchAll", loc: { offset: 0, line: 1, column: 1 } };
  if (nodes.length === 1) return nodes[0]!;
  return { kind, children: nodes, loc: nodes[0]!.loc };
}

/** 把 chevrotain 的识别异常翻译成带 token 位置的 QuerySyntaxError。 */
function translateParserError(err: unknown, text: string): QuerySyntaxError {
  // chevrotain 的类型签名收 Error，但运行时会抛任意 recognition exception；
  // 这里先做一次真实的 instanceof Error 判断再交给它。
  if (!(err instanceof Error) || !isRecognitionException(err)) {
    return new QuerySyntaxError(err instanceof Error ? err.message : "语法解析失败", { phase: "parse" });
  }
  const e = err as { message?: string; token?: IToken; previousToken?: IToken; resyncedTokens?: IToken[] };
  // NotAllInputParsedException 把出错 token 放在 resyncedTokens 里。
  const bad = e.token ?? e.resyncedTokens?.[0] ?? e.previousToken;
  // EOF / resync token 的位置是 NaN 或不存在，统一退到"查询串末尾"这个一定合法的位置
  return new QuerySyntaxError(e.message ?? "语法解析失败", {
    phase: "parse",
    location: locationOf(bad) ?? offsetToLocation(text, text.length),
    token: bad?.image ? bad.image : null,
  });
}

/**
 * chevrotain 在运行时把 RULE() 生成的规则方法挂到 parser 实例上，但 .d.ts 没声明它们。
 * 用 Declaration Merging 补上类型，规则体内部就能直接 `this.OR_EXPR` 引用子规则。
 */
interface QueryParser extends RuleTable {}

let parser: QueryParser | null = null;

/** chevrotain parser 首次构造要做文法自分析，全局只做一次。 */
function getParser(): QueryParser {
  if (parser === null) parser = new QueryParser();
  return parser;
}

function collectFields(node: QueryNode, out: Set<string>): void {
  switch (node.kind) {
    case "and":
    case "or":
      for (const c of node.children) collectFields(c, out);
      return;
    case "not":
      collectFields(node.child, out);
      return;
    case "range":
    case "exists":
      out.add(node.field);
      return;
    case "field":
    case "phrase":
    case "term":
      if (node.field !== "_all") out.add(node.field);
      return;
    case "matchAll":
      return;
  }
}

function hasRange(node: QueryNode): boolean {
  switch (node.kind) {
    case "and":
    case "or":
      return node.children.some(hasRange);
    case "not":
      return hasRange(node.child);
    case "range":
      return true;
    default:
      return false;
  }
}

/** 解析查询串 -> AST。词法/语法错误都带 token 位置。 */
export function parseQuery(query: string): ParsedQuery {
  const { tokens, text } = tokenize(query);
  const p = getParser();
  p.input = tokens;

  let ast: QueryNode;
  try {
    ast = p.query();
  } catch (err) {
    throw translateParserError(err, text);
  }

  const fields = new Set<string>();
  collectFields(ast, fields);
  return { query, ast, fields: [...fields], hasRange: hasRange(ast), tokenCount: tokens.length };
}

/** 便捷入口：只要 AST。 */
export function parseAst(query: string): QueryNode {
  return parseQuery(query).ast;
}

/** AST 的紧凑字符串形式，用于终端打印。 */
export function formatAst(node: QueryNode): string {
  switch (node.kind) {
    case "field":
      return node.quoted ? `${node.field}:"${node.value}"` : `${node.field}:${node.value}`;
    case "term":
      return `_all:${JSON.stringify(node.value)}`;
    case "phrase":
      return `_all:"${node.value}"`;
    case "range": {
      const lo = node.lower === null ? "*" : node.lower;
      const hi = node.upper === null ? "*" : node.upper;
      return `${node.field}:[${lo} TO ${hi}]`;
    }
    case "exists":
      return `${node.field}:*`;
    case "not":
      return `NOT ${formatAst(node.child)}`;
    case "matchAll":
      return "*";
    case "and":
      return `(${node.children.map(formatAst).join(" AND ")})`;
    case "or":
      return `(${node.children.map(formatAst).join(" OR ")})`;
  }
}
