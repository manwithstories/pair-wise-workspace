//! End-to-end consistency tests for the full secpipe pipeline.
//!
//! These exercise the public library API against a temporary working directory
//! (tempfile is a dev-dependency only) and assert the contracts from the brief:
//! deterministic fixed-seed output, wrap/late handling, idempotent re-import and
//! crash-safe resumable ingest.

use std::path::Path;

use secpipe::config::Config;
use secpipe::fixtures;
use secpipe::pipeline::{self, RunContext};
use secpipe::store::Store;
use tempfile::TempDir;

const CONFIG: &str = include_str!("../configs/default.toml");

fn load_cfg() -> Config {
    let dir = TempDir::new().unwrap();
    let cfg_path = dir.path().join("default.toml");
    std::fs::write(&cfg_path, CONFIG).unwrap();
    Config::load(&cfg_path).unwrap()
}

fn run_full(root: &Path, seed: u64) -> secpipe::report::Metrics {
    let cfg = load_cfg();
    let log_dir = root.join("logs");
    fixtures::generate(&log_dir, seed).unwrap();
    let ctx = RunContext::new(cfg, root);

    let stats = pipeline::ingest(&ctx, &log_dir).unwrap();
    let store = Store::open(&ctx.db_path).unwrap();
    let counters = pipeline::snapshot(&store, &stats).unwrap();
    drop(store);

    let correlation = pipeline::correlate(&ctx).unwrap();
    let truth_path = log_dir.join("truth.json");
    let truth: fixtures::Truth =
        serde_json::from_slice(&std::fs::read(&truth_path).unwrap()).unwrap();
    pipeline::self_check(&ctx, &correlation).unwrap();
    pipeline::evaluate_metrics(&correlation, &truth, &counters, true, true)
}

#[test]
fn fixed_seed_reproduces_contract_metrics() {
    let dir = TempDir::new().unwrap();
    let metrics = run_full(dir.path(), 42);
    assert_eq!(metrics.parsed, 80_000);
    assert_eq!(metrics.quarantined, 37);
    assert_eq!(metrics.wraparounds, 2);
    assert_eq!(metrics.late_events, 143);
    assert_eq!(metrics.restatements, 11);
    assert_eq!(metrics.windows, 2_400);
    assert_eq!(metrics.anomalous_sessions, 58);
    assert_eq!(metrics.rules_hit, 14);
    assert_eq!(metrics.tp, 13);
    assert_eq!(metrics.fp, 1);
    assert_eq!(metrics.fn_, 1);
    assert!((metrics.hit_rate - 0.9286).abs() < 1e-4);
    assert!((metrics.false_positive_rate - 0.0150).abs() < 1e-4);
    assert!(metrics.self_check_pass);
}

#[test]
fn output_is_deterministic_across_runs_and_seeds() {
    let dir_a = TempDir::new().unwrap();
    let dir_b = TempDir::new().unwrap();
    let a = run_full(dir_a.path(), 42);
    let b = run_full(dir_b.path(), 42);
    assert_eq!(a.canonical_lines(), b.canonical_lines());
    assert_eq!(
        secpipe::report::report_hash(&a),
        secpipe::report::report_hash(&b)
    );

    // A different seed still satisfies every structural invariant.
    let dir_c = TempDir::new().unwrap();
    let c = run_full(dir_c.path(), 7);
    assert_eq!(c.parsed, 80_000);
    assert_eq!(c.windows, 2_400);
    assert_eq!(c.wraparounds, 2);
    assert!(c.self_check_pass);
}

#[test]
fn duplicate_import_is_idempotent() {
    let dir = TempDir::new().unwrap();
    let cfg = load_cfg();
    let log_dir = dir.path().join("logs");
    fixtures::generate(&log_dir, 42).unwrap();
    let ctx = RunContext::new(cfg, dir.path());

    let first = pipeline::ingest(&ctx, &log_dir).unwrap();
    let store = Store::open(&ctx.db_path).unwrap();
    let records_once = store.count_records().unwrap();
    drop(store);

    // Re-ingest with cursors cleared but records retained: every row must be a
    // fingerprint conflict and totals must not change.
    let second = pipeline::reingest_for_dedup(&ctx, &log_dir).unwrap();
    let store = Store::open(&ctx.db_path).unwrap();
    assert_eq!(store.count_records().unwrap(), records_once);
    assert_eq!(second.inserted, 0);
    assert_eq!(second.duplicate, records_once);
    assert!(first.inserted > 0);
}

#[test]
fn resumable_ingest_matches_one_shot_run() {
    // One-shot reference.
    let reference_dir = TempDir::new().unwrap();
    let reference = run_full(reference_dir.path(), 42);

    // Interrupted run: force tiny chunks and simulate a crash after the first
    // chunk by re-opening the store and resuming via the durable cursor.
    let dir = TempDir::new().unwrap();
    let mut cfg = load_cfg();
    cfg.store.chunk_rows = 1_371; // awkward prime to stress chunk boundaries
    let log_dir = dir.path().join("logs");
    fixtures::generate(&log_dir, 42).unwrap();
    let ctx = RunContext::new(cfg, dir.path());

    let mut _total = pipeline::IngestStats::default();
    // Each call resumes from committed cursors until everything is ingested.
    for _ in 0..200 {
        let batch = pipeline::ingest(&ctx, &log_dir).unwrap();
        _total.inserted += batch.inserted;
        _total.duplicate += batch.duplicate;
        _total.quarantined += batch.quarantined;
        _total.late_events = batch.late_events;
        _total.restatements = batch.restatements;
        _total.wraparounds = batch.wraparounds;
        let store = Store::open(&ctx.db_path).unwrap();
        if store.count_records().unwrap() == reference.parsed {
            break;
        }
    }

    let store = Store::open(&ctx.db_path).unwrap();
    assert_eq!(store.count_records().unwrap(), reference.parsed);
    assert_eq!(store.count_quarantine().unwrap(), reference.quarantined);
    drop(store);

    let correlation = pipeline::correlate(&ctx).unwrap();
    pipeline::self_check(&ctx, &correlation).unwrap();
    assert_eq!(correlation.windows, reference.windows);
    assert_eq!(correlation.rule_hits.len() as u64, reference.rules_hit);
    assert_eq!(correlation.anomalous_sessions, reference.anomalous_sessions);
}

#[test]
fn parser_quarantines_bad_lines_and_keeps_good_ones() {
    use secpipe::parser::parse_line;
    assert!(parse_line("x.log", 1, "").unwrap().is_none());
    assert!(parse_line("x.log", 2, "garbage without marker").is_err());

    let nginx = parse_line(
        "nginx.log",
        3,
        "10.0.0.1 - - [12345] \"GET /a HTTP/1.1\" 404 99 \"-\" \"curl/8\"",
    )
    .unwrap()
    .unwrap();
    assert_eq!(nginx.raw_tick, 12345);
    assert_eq!(nginx.event, "http");
    assert_eq!(nginx.status, Some(404));

    let sshd = parse_line(
        "sshd.log",
        4,
        "999 host sshd[7]: Failed password for user root from 10.0.0.2 port 1 ssh2",
    )
    .unwrap()
    .unwrap();
    assert_eq!(sshd.event, "login_failure");
    assert_eq!(sshd.src_ip, "10.0.0.2");

    let zeek = parse_line(
        "zeek.log",
        5,
        "555\tUid\t10.0.0.3\t443\t172.16.0.1\t443\ttcp\t200\t100\tua",
    )
    .unwrap()
    .unwrap();
    assert_eq!(zeek.event, "conn");
    assert_eq!(zeek.dest_port, Some(443));
}
