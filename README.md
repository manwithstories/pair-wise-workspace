# LogLens

An embeddable inverted-index search engine for structured logs. Filters are
pushed down into the inverted lists and evaluated with short-circuiting set
operations, so queries return in milliseconds without scanning documents.

Built to be embedded: no server, no client, no external service. TypeScript 5.4,
chevrotain 11.1 for query parsing (source vendored under `vendor/chevrotain`),
`node:test` for tests, in-memory segments with file-backed persistence.

```bash
npm install
npx tsx src/demo.ts        # the full walkthrough
npm test                    # 58 tests
npm run typecheck
```

## Why it is shaped this way

The workload is ~10M structured log records a day, queried like:

```
level:error AND service:auth AND message:"timeout"
```

Four constraints drove the design:

- **Full scans do not scale.** Filters must be answered from posting lists, so
  every leaf condition becomes an intersection over `docIds`, never a document
  scan.
- **Writes cannot stop.** `add()` only touches an in-memory segment; durability
  happens at `flush()`, which publishes a new immutable segment in a single
  assignment.
- **A half-built segment must never be visible.** Segments are immutable once
  sealed. Readers hold a snapshot reference, so a concurrent merge cannot
  disturb an in-flight query.
- **Frequent deletes bloat the index.** Deletes are tombstones; a merge drops
  the deleted documents and reclaims the space.

## Layout

| File | Responsibility |
| --- | --- |
| `src/lexer.ts` | Token definitions; positioned lexical errors |
| `src/parser.ts` | chevrotain grammar → AST; positioned parse errors |
| `src/inverted.ts` | Posting lists, skip tables, galloping set ops, numeric columns |
| `src/segment.ts` | Immutable segments, binary format, merge, tombstones |
| `src/pushdown.ts` | Filter planning, short-circuit evaluation, plan explain |
| `src/engine.ts` | Writes, flush, tiered merge, concurrency, query execution |

### Data flow

```
add() ──▶ SegmentBuilder (in-memory) ──flush()──▶ sealed Segment ──▶ *.seg file
                │                                        │
                │ searchable immediately                 └─▶ tiered merge ──▶ L1, L2 …
                ▼
search() ──▶ parse ──▶ AST ──▶ pushdown plan ──▶ per-segment intersect (short-circuit)
                                                          │
                                                          ▼
                                                  tombstone filter ──▶ original documents
```

## Query language

```
level:error                          exact term
service:auth AND level:error         boolean
level:error service:auth             implicit AND
level:error AND (a:b OR c:d)         grouping
NOT level:debug                      negation
message:"connection timeout"         phrase: adjacent, in order
latency:[100 TO 500]                 inclusive range
latency:{100 TO 500}                 exclusive range
latency:[100 TO *]                   open-ended
status:>=500                         comparison
timeout                              bare term (default field)
```

Errors carry a position and a caret excerpt rather than throwing:

```
lex error: unexpected character: ->@<- at offset: 4
level:error AND ...
             ^
```

## Execution model

**Pushdown.** `buildPlan` decomposes the AST, flattens conjunctions, and orders
them cheapest-first using per-leaf document counts. Leaves map to posting-list
intersections; ranges map to a binary-searched numeric column.

**Short-circuiting, at two levels.**

1. *Intra-segment.* Galloping intersection with skip tables. The first empty
   posting list ends evaluation, and the result is never materialized.
2. *Inter-segment.* A segment missing a conjunctive term is skipped without
   touching postings at all (`scanned: 0` in the demo output).

**Early exit.** Only `limit` hits are returned, so each segment is given the
budget of hits still missing and stops as soon as it can satisfy it. This is
what keeps a query like `level:error` (250k matches) as fast as a selective one.

**Negation** is emitted lazily: gaps in the excluded set are produced up to the
cap instead of materializing a 300k-element complement.

## Segments, merge, and durability

- `flush()` rotates the builder **synchronously**, before any `await`. That is
  what stops a burst of writes from scheduling repeated flushes of the same
  builder.
- The segment list is replaced in one assignment, so a query sees the old list
  or the new one — never a partial state.
- Tiered merge folds full tiers upward (L0 → L1 → L2). A tier is merged only
  when full, or when a segment crosses the tombstone threshold (default 30%).
- Merges run in the background, are bounded so the scheduler cannot spin, and
  swallow failures rather than raising unhandled rejections.
- The segment file is written to a temp path and renamed, so a reader never
  observes a partial segment. On load, skip tables are rebuilt rather than
  stored; round-trips are byte-identical.

## Performance

Measured on this machine, single-threaded Node (`--expose-gc` where noted):

| Constraint | Target | Measured |
| --- | --- | --- |
| Build 1M docs, single segment | < 3 s | **1.17 s** |
| Medium-selectivity query p99 | < 50 ms | **0.02 – 0.08 ms** (6.9 ms for a wide range) |
| 10 concurrent query streams | no blocking | **p99 < 16 ms** |
| Peak memory, 1M docs | < 512 MB | **216 MB heapUsed** |
| Tombstone ratio trigger | > 30% | enforced, forces merge |

Two decisions matter most here:

- **`trace_id` is stored but not term-indexed** (`UNINDEXED_FIELDS`). It is
  near-unique, so indexing it costs one posting list per document for a field
  nobody queries by term. It is still returned with every hit. This alone took
  the 1M build from 3.6 s to 1.2 s.
- **Postings are built into growable typed arrays**, not `number[]`. `trace_id`
  produced 1M small postings; per-posting `Uint32Array.from` and array churn
  dominated the build.

Memory is reported as `heapUsed`, not RSS: RSS never shrinks after a GC, so it
overstates steady-state usage. `engine.memoryUsage()` exposes both.

## Concurrency guarantees

Within one process:

- Segments are immutable once sealed, so queries need no locks.
- A query takes one snapshot reference; a concurrent merge cannot disturb it.
- The active (unflushed) segment is searchable through a read-only view, so
  writes are visible immediately and never pay for a flush.
- Writes and queries do not block each other; merges happen in the background.

Note that `search()` is synchronous and CPU-bound: concurrent callers on one
thread interleave rather than run in parallel. Across worker threads, each
engine instance owns its own segments.

## Tests

```
test/inverted.test.ts   posting lists, skip tables, galloping, ranges, phrases
test/parser.test.ts     grammar, ranges, phrases, error positions
test/segment.test.ts    binary round-trip, tombstones, merge, atomic writes
test/engine.test.ts     search, pushdown, merge races, restart, concurrency
```

Several tests exist because they cover bugs found during development, notably:
auto-flush re-flushing one builder, concurrent flushes double-counting a
segment, the merge scheduler re-arming forever, and phrase queries matching
co-occurrence instead of adjacency.

## Vendored chevrotain

`vendor/chevrotain/src` is the upstream TypeScript source from
`chevrotain@11.1.0`, copied verbatim so the parser is built on a version-locked
toolkit. Regenerate with `./scripts/vendor-chevrotain.sh`, which also re-emits
the declaration surface. Upstream does not compile under this project's strict
flags, so the vendored files carry `@ts-nocheck` — runtime behaviour is
unchanged.
