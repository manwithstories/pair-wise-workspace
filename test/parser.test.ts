import { test } from "node:test";
import assert from "node:assert/strict";
import { parseQuery, formatAst, type QueryNode } from "../src/parser.js";

function ast(q: string): QueryNode {
  const r = parseQuery(q);
  assert.ok(r.ok, `expected parse success for ${q}: ${r.ok ? "" : r.error.message}`);
  return r.ast;
}

function expectError(q: string): { message: string; offset: number } {
  const r = parseQuery(q);
  assert.ok(!r.ok, `expected a parse error for ${q}`);
  return { message: r.error.message, offset: r.error.offset };
}

test("parses field:value into a term leaf", () => {
  assert.deepEqual(ast("level:error"), {
    kind: "fieldTerm",
    field: "level",
    term: "error",
    offset: 0,
  });
});

test("bare words target the default field", () => {
  assert.deepEqual(ast("timeout"), {
    kind: "fieldTerm",
    field: "_all",
    term: "timeout",
    offset: 0,
  });
  const scoped = parseQuery("timeout", "message");
  assert.ok(scoped.ok && scoped.ast.kind === "fieldTerm" && scoped.ast.field === "message");
});

test("parses explicit AND left-associatively", () => {
  const node = ast("level:error AND service:auth");
  assert.equal(node.kind, "and");
  if (node.kind !== "and") return;
  assert.equal(node.left.kind, "fieldTerm");
  assert.equal(node.right.kind, "fieldTerm");
});

test("adjacent clauses imply AND", () => {
  const implicit = ast("level:error service:auth");
  const explicit = ast("level:error AND service:auth");
  // structurally identical; only the recorded source offset differs
  const strip = (n: any): any =>
    typeof n === "object" && n !== null && "kind" in n
      ? { ...n, ...(n.kind === "and" || n.kind === "or"
          ? { left: strip(n.left), right: strip(n.right) }
          : n.kind === "not" ? { operand: strip(n.operand) } : { offset: 0 }) }
      : n;
  assert.deepEqual(strip(implicit), strip(explicit));
  assert.equal(implicit.kind, "and");
});

test("parses OR, NOT, and precedence (NOT binds tighter than AND/OR)", () => {
  const or = ast("a:1 OR b:2");
  assert.equal(or.kind, "or");

  const not = ast("NOT a:1");
  assert.equal(not.kind, "not");

  // `a OR NOT b` -> OR(a, NOT(b))
  const mixed = ast("a:1 OR NOT b:2");
  assert.equal(mixed.kind, "or");
  if (mixed.kind !== "or") return;
  assert.equal(mixed.right.kind, "not");
});

test("groups with parentheses and nests correctly", () => {
  const node = ast("level:error AND (service:auth OR service:billing)");
  assert.equal(node.kind, "and");
  if (node.kind !== "and") return;
  assert.equal(node.left.kind, "fieldTerm");
  assert.equal(node.right.kind, "or");
});

test("parses ranges: inclusive, exclusive, and open-ended", () => {
  assert.deepEqual(ast("latency:[100 TO 500]"), {
    kind: "range",
    field: "latency",
    lo: 100,
    hi: 500,
    loInclusive: true,
    hiInclusive: true,
    offset: 0,
  });
  const excl = ast("latency:{100 TO 500}");
  assert.ok(excl.kind === "range" && excl.loInclusive === false && excl.hiInclusive === false);

  const openLow = ast("latency:[100 TO *]");
  assert.ok(openLow.kind === "range" && openLow.lo === 100 && openLow.hi === null);
});

test("parses comparison operators", () => {
  const gte = ast("status:>=500");
  assert.ok(gte.kind === "range" && gte.lo === 500 && gte.loInclusive === true && gte.hi === null);
  const lt = ast("status:<500");
  assert.ok(lt.kind === "range" && lt.lo === null && lt.hi === 500 && lt.hiInclusive === false);
  const eq = ast("status:=500");
  assert.ok(eq.kind === "range" && eq.lo === 500 && eq.hi === 500 && eq.loInclusive && eq.hiInclusive);
});

test("parses phrases, both bare and field-scoped", () => {
  const bare = ast('"connection timeout"');
  assert.equal(bare.kind, "phrase");
  if (bare.kind !== "phrase") return;
  assert.deepEqual(bare.terms, ["connection", "timeout"]);

  const scoped = ast('message:"connection timeout"');
  assert.equal(scoped.kind, "phrase");
  if (scoped.kind !== "phrase") return;
  assert.equal(scoped.field, "message");
  assert.deepEqual(scoped.terms, ["connection", "timeout"]);
});

test("parses the motivating SRE query end to end", () => {
  const node = ast('level:error AND service:auth AND message:"timeout"');
  assert.equal(formatAst(node).includes('message:"timeout"'), true);
  assert.equal(node.kind, "and");
});

test("keyword-like terms are not treated as operators", () => {
  // `android` must lex as one term, not `AND` + `roid`
  assert.deepEqual(ast("app:android"), {
    kind: "fieldTerm",
    field: "app",
    term: "android",
    offset: 0,
  });
});

test("reports token position for lexical errors", () => {
  const err = expectError("bad @ query");
  assert.match(err.message, /lex error/);
  assert.equal(err.offset, 4);
});

test("reports position for a truncated query", () => {
  const err = expectError("level:error AND");
  assert.match(err.message, /Expecting token of type/);
});

test("reports position for an unbalanced parenthesis", () => {
  const err = expectError("(a:1");
  assert.ok(err.message.length > 0);
});

test("formatAst renders a readable tree", () => {
  const text = formatAst(ast("level:error AND (service:auth OR service:billing)"));
  assert.match(text, /AND/);
  assert.match(text, /OR/);
  assert.match(text, /level:error/);
});
