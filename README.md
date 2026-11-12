# secpipe — local, replayable, auditable security log pipeline

`secpipe` turns three raw text log sources (Nginx access, `sshd` authentication,
Zeek connection logs) into a deterministic, re-auditable security dataset. It
reconstructs event time across 32-bit counter wraps, handles out-of-order and
late delivery, correlates sliding-window rules, rebuilds sessions, scores
per-entity anomalies with a robust median+MAD baseline, and emits terminal, JSON
and CSV reports.

Everything runs locally with no network services and no async runtime. Output is
a pure function of the input seed, so a fixed seed always reproduces identical
counts and an identical report hash.

## Quick start

```bash
# One command installs all dependencies (bundled SQLite) and runs the demo.
cargo run --release -- all \
  --config configs/default.toml \
  --seed 42 \
  --out out/demo \
  --self-check
```

Expected final line (seed 42):

```
parsed=80000 quarantined=37 wraparounds=2 late_events=143 restatements=11 windows=2400 anomalous_sessions=58 rules_hit=14 tp=13 fp=1 fn=1 hit_rate=0.9286 false_positive_rate=0.0150 dedup_stable=yes self_check=PASS
```

A failed self-check exits non-zero (exit code 5). Other stable exit codes are
documented in `src/error.rs`.

## Subcommands

- `all` — generate fixtures, ingest, correlate, verify idempotency, self-check, report.
- `ingest` — parse logs in manifest arrival order into SQLite (resumable, chunked).
- `correlate` — recompute windows, rules, sessions and anomalies from event time.
- `report` — render `report.txt`, `report.json`, `report.csv`.
- `stress` — throughput harness over a large synthetic dataset.

## Data flow and modules

Dependency direction is strictly one way; each layer only uses `config` and the
layers before it.

```
fixtures -> parser -> clock -> store -> window / rules / session / anomaly -> report
```

- `fixtures.rs` — seeded LCG generator producing labeled logs, an arrival-order
  manifest and a `truth.json` ground-truth sidecar.
- `parser.rs` — single auto-detecting entry point shared by ingest and replay;
  bad lines are isolated, never fatal.
- `clock.rs` — 32-bit wrap expansion, wrap/jump ambiguity rejection, watermark
  and late-event classification.
- `store.rs` — SQLite schema, unique fingerprint index, chunked transactions and
  per-file/global resume cursors.
- `window.rs` — arrival-order late tracking plus full event-time recompute.
- `rules.rs` — declarative count / status4xx / sequence / absent rules.
- `anomaly.rs` — median + MAD baseline with cold start and burst merging.
- `session.rs` — `(src_ip, user_agent)` idle-gap sessions, wrap/order safe.
- `report.rs` — deterministic tables, JSON, CSV and FNV report hash.
- `pipeline.rs` — orchestration, truth-vs-alert metrics and the independent
  in-memory self-check.

## Key correctness properties

- **Wraps.** A backward raw-tick delta at or above `wrap_threshold` counts a wrap;
  a forward delta above `jump_threshold` is ambiguous and aborts non-zero.
- **Lateness.** Events up to `allowed_lateness_secs` behind the watermark are
  accepted; they restate already-closed windows. The self-check proves the
  persisted metrics equal an independent full-sort recomputation.
- **Idempotency.** Re-importing the same batch only increments the fingerprint
  conflict counter; rows, windows and the report hash never change.
- **Resume.** Committed chunks and the global arrival-sequence cursor make an
  interrupted ingest converge to the same final state as a one-shot run.

## Performance

`scripts/stress_500k.sh` processes 500,000 rows. On the reference machine the
full ingest+correlate pass finishes in roughly 8-9 seconds; the streaming design
keeps peak memory comfortably below 512 MB (the on-disk database for 500k rows is
about 73 MB).

## Testing

```bash
cargo test --release
```

Unit tests cover clock wrap/drift/ambiguity, rule sequence/absent logic and
window late tracking. `tests/pipeline.rs` adds end-to-end checks for the seed-42
contract, determinism across runs, idempotent duplicate import, crash-safe
resume and parser quarantine behavior.
