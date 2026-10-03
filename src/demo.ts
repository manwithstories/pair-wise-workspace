/**
 * demo.ts — end-to-end walkthrough of the search engine.
 *
 *   npx tsx src/demo.ts
 *
 * Prints, in order:
 *   1. the sample log records, exactly as ingested
 *   2. the parsed AST and the pushdown plan for a query
 *   3. per-segment hit counts and short-circuit skips
 *   4. the matching documents and timings
 */

import { SearchEngine } from "./engine.js";
import { parseQuery, formatAst } from "./parser.js";
import { buildPlan, explainPlan } from "./pushdown.js";
import type { LogDoc } from "./segment.js";

/* ------------------------------------------------------------------ *
 * Formatting helpers
 * ------------------------------------------------------------------ */

const RULE = "\u2500".repeat(78);

/**
 * Colors are enabled only for an interactive terminal, so piping the demo into
 * a file or a log yields clean text with no escape sequences. NO_COLOR is
 * honoured as the de-facto standard opt-out.
 */
const COLOR = process.stdout.isTTY === true && process.env["NO_COLOR"] === undefined;
const wrap = (code: number) => (s: string) => (COLOR ? `\u001b[${code}m${s}\u001b[0m` : s);
const bold = wrap(1);
const dim = wrap(2);
const cyan = wrap(36);
const green = wrap(32);
const yellow = wrap(33);

function heading(step: number, title: string): void {
  console.log(`\n${bold(RULE)}\n${bold(`[${step}] ${title}`)}\n${bold(RULE)}`);
}

/** One log line, as an operator would read it. */
function formatLog(doc: LogDoc, id: string): string {
  const level = doc.level === "error" ? yellow("ERROR") : dim(doc.level.toUpperCase());
  return `${dim(new Date(doc.ts).toISOString())} ${level} ` +
    `${cyan(doc.service.padEnd(8))} ${dim(id.padEnd(12))} ${doc.message}`;
}

/* ------------------------------------------------------------------ *
 * Sample data
 * ------------------------------------------------------------------ */

/**
 * Small, hand-checkable corpus. `trace_id` is deliberately excluded from the
 * term index (see UNINDEXED_FIELDS): it is returned with hits for correlation
 * but never queried by term.
 */
const SAMPLE_LOGS: Array<{ id: string; doc: LogDoc }> = [
  {
    id: "log-0001",
    doc: {
      level: "error", service: "auth", trace_id: "tr-9f2a",
      message: "upstream connection timeout after 5000ms",
      ts: Date.UTC(2026, 2, 14, 10, 0, 0),
    },
  },
  {
    id: "log-0002",
    doc: {
      level: "error", service: "auth", trace_id: "tr-1b7c",
      message: "token refresh timeout while calling auth-service",
      ts: Date.UTC(2026, 2, 14, 10, 0, 1),
    },
  },
  {
    id: "log-0003",
    doc: {
      level: "info", service: "auth", trace_id: "tr-4d1e",
      message: "login accepted for user 4821",
      ts: Date.UTC(2026, 2, 14, 10, 0, 2),
    },
  },
  {
    id: "log-0004",
    doc: {
      level: "error", service: "billing", trace_id: "tr-77aa",
      message: "connection timeout to payments gateway",
      ts: Date.UTC(2026, 2, 14, 10, 0, 3),
    },
  },
  {
    id: "log-0005",
    doc: {
      level: "warn", service: "auth", trace_id: "tr-0c3d",
      message: "retry backoff applied to auth-service",
      ts: Date.UTC(2026, 2, 14, 10, 0, 4),
    },
  },
  {
    id: "log-0006",
    doc: {
      level: "error", service: "search", trace_id: "tr-3e55",
      message: "query shard unavailable",
      ts: Date.UTC(2026, 2, 14, 10, 0, 5),
    },
  },
  {
    id: "log-0007",
    doc: {
      level: "error", service: "auth", trace_id: "tr-6f90",
      // note: "timeout" and "connection" are NOT adjacent here
      message: "connection to auth-service reset, timeout after retry",
      ts: Date.UTC(2026, 2, 14, 10, 0, 6),
    },
  },
  {
    id: "log-0008",
    doc: {
      level: "info", service: "billing", trace_id: "tr-8b22",
      message: "invoice 9912 settled",
      ts: Date.UTC(2026, 2, 14, 10, 0, 7),
    },
  },
];

/** The query from the brief. */
const QUERY = 'level:error AND service:auth AND message:"connection timeout"';

/* ------------------------------------------------------------------ *
 * Demo
 * ------------------------------------------------------------------ */

async function main(): Promise<void> {
  console.log(bold("\nLogLens — inverted-index log search with pushdown + tiered segments\n"));

  /* ---- 1. sample logs -------------------------------------------- */
  heading(1, "Sample log records (raw, as ingested)");
  for (const { id, doc } of SAMPLE_LOGS) {
    console.log(formatLog(doc, id));
  }

  /* ---- build the engine ------------------------------------------ */
  // Two segments by flushing after every third record, so the demo can show a
  // segment being skipped by pushdown rather than being scanned.
  const engine = new SearchEngine({
    flushEvery: 3,
    mergeFactor: 1000, // keep the tiers separate so both remain visible
  });
  for (const { id, doc } of SAMPLE_LOGS) engine.add(doc, id);
  await engine.flush();

  /* ---- 2. AST + pushdown plan ------------------------------------ */
  heading(2, `Parsed AST for: ${QUERY}`);

  const parsed = parseQuery(QUERY, "message");
  if (!parsed.ok) {
    console.error(yellow(`parse failed: ${parsed.error.message}`));
    return;
  }
  console.log(formatAst(parsed.ast, 1));

  heading(3, "Pushdown plan (leaves evaluated as posting-list intersections)");
  const plan = buildPlan(parsed.ast, (leaf) => engine.countLeaf(leaf));
  const explanation = explainPlan(plan);
  for (const line of explanation.lines) console.log(line);
  console.log(
    dim(
      `\n  ${explanation.pushdownCount} condition(s) pushed into posting lists, ` +
        `${explanation.residualCount} requiring a residual check`,
    ),
  );

  /* ---- 3. per-segment results ------------------------------------ */
  heading(4, "Per-segment evaluation");

  const outcome = engine.search(QUERY, 100);
  if (!outcome.ok) {
    console.error(yellow(`query failed: ${outcome.error.message}`));
    return;
  }
  const { result } = outcome;

  console.log(
    `  ${"segment".padEnd(22)} ${"lvl".padEnd(4)} ${"docs".padEnd(7)} ` +
      `${"hits".padEnd(6)} ${"scanned".padEnd(9)} status`,
  );
  console.log(dim(`  ${"-".repeat(70)}`));
  for (const seg of result.segments) {
    const status = seg.skipped
      ? green("skipped (short-circuit: no matching posting)")
      : seg.shortCircuited && seg.matched === 0
        ? green("short-circuited")
        : dim("evaluated");
    console.log(
      `  ${seg.segmentId.slice(0, 22).padEnd(22)} ${String(seg.level).padEnd(4)} ` +
        `${String(seg.docs).padEnd(7)} ${String(seg.matched).padEnd(6)} ` +
        `${String(seg.scanned).padEnd(9)} ${status}`,
    );
  }
  console.log(
    dim(
      `\n  evaluated ${result.stats.segmentsEvaluated}, ` +
        `skipped ${result.stats.segmentsSkipped}, ` +
        `short-circuited ${result.stats.segmentsShortCircuited}`,
    ),
  );

  /* ---- 4. hits + timing ------------------------------------------ */
  heading(5, "Matching documents");
  for (const hit of result.hits) {
    console.log(`  ${formatLog(hit.doc, hit.id)}`);
    console.log(dim(`     segment ${hit.segmentId}  localId ${hit.localId}`));
  }
  console.log(
    `\n  ${bold(String(result.total))} hit(s) in ${bold(`${result.tookMs.toFixed(3)} ms`)} ` +
      `(parse → plan → pushdown → intersect → hydrate)`,
  );

  /* ---- extras: phrase + range + delete --------------------------- */
  heading(6, "Phrase semantics: adjacency is required, not just co-occurrence");
  for (const q of ['message:"connection timeout"', 'message:"timeout connection"']) {
    const r = engine.search(q, 10);
    const ids = r.ok ? r.result.hits.map((h) => h.id) : [];
    console.log(`  ${q.padEnd(34)} → ${ids.length > 0 ? ids.join(", ") : dim("(no match)")}`);
  }
  console.log(
    dim(
      "\n  log-0007 contains both words but not adjacently, so the phrase excludes it.",
    ),
  );

  heading(7, "Range pushdown and deletions");
  // derived from the sample timestamps so the demo cannot drift out of date
  const tsValues = SAMPLE_LOGS.map((l) => l.doc.ts).sort((a, b) => a - b);
  const rangeQuery = `ts:[${tsValues[0] as number} TO ${tsValues[3] as number}]`;
  const rangeOut = engine.search(rangeQuery, 10);
  console.log(
    `  ${rangeQuery.padEnd(34)} → ${rangeOut.ok ? rangeOut.result.hits.map((h) => h.id).join(", ") : "?"}`,
  );

  engine.delete("log-0002");
  const afterDelete = engine.search("service:auth", 10);
  console.log(
    `  deleted log-0002, then service:auth → ${
      afterDelete.ok ? afterDelete.result.hits.map((h) => h.id).join(", ") : "?"
    }`,
  );
  console.log(dim("  (the deletion is a tombstone; a merge will reclaim the space)"));

  await engine.close();

  /* ---- summary ---------------------------------------------------- */
  heading(8, "Summary");
  console.log(`  query      ${QUERY}`);
  console.log(`  hits       ${result.total}`);
  console.log(`  latency    ${result.tookMs.toFixed(3)} ms`);
  console.log(`  segments   ${result.segments.length} (${result.stats.segmentsSkipped} skipped without scanning)`);
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
