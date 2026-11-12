//! End-to-end orchestration: fixtures -> parser -> clock -> store ->
//! window/rules/session/anomaly -> report.
//!
//! Each stage depends only on [`crate::config`] and the previous layer. The
//! correlate pass recomputes every derived metric from the full event-time
//! sorted record set, while the self-check independently rebuilds the same
//! metrics in memory and requires an exact match.

use std::collections::{BTreeMap, BTreeSet, HashSet};
use std::path::{Path, PathBuf};

use crate::anomaly;
use crate::clock::{ClockExpander, ClockState};
use crate::config::Config;
use crate::error::{Error, Result};
use crate::fixtures::{Truth, TruthLabel};
use crate::model::{Quarantined, Record};
use crate::parser;
use crate::report::Metrics;
use crate::rules::{self, RuleHit};
use crate::session;
use crate::store::Store;
use crate::window;

pub struct RunContext {
    pub cfg: Config,
    pub work_dir: PathBuf,
    pub db_path: PathBuf,
}

impl RunContext {
    pub fn new(cfg: Config, work_dir: &Path) -> Self {
        let db_path = work_dir.join("secpipe.db");
        Self {
            cfg,
            work_dir: work_dir.to_path_buf(),
            db_path,
        }
    }
}

/// Ingest one run directory's logs in manifest arrival order. Resumable:
/// committed chunks and finished files are skipped on re-entry.
pub fn ingest(ctx: &RunContext, log_dir: &Path) -> Result<IngestStats> {
    let mut store = Store::open(&ctx.db_path)?;
    let manifest = log_dir.join("manifest.tsv");
    let order = read_manifest(&manifest)?;

    let clock_state = store.load_clock()?.unwrap_or_else(ClockState::default);
    let mut expander = ClockExpander::from_state(ctx.cfg.clock.clone(), clock_state);

    // Streaming watermark for this process. On resume, already-committed events
    // are skipped, so their late counts are restored from durable meta instead
    // of being re-observed (which would double count).
    let mut tracker = window::LateTracker::new(
        ctx.cfg.late.window_length_secs,
        ctx.cfg.late.allowed_lateness_secs,
    );
    let prior_late: u64 = store
        .get_meta("late_events")?
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);
    let prior_restatements: u64 = store
        .get_meta("restatements")?
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);
    let mut prior_restated_windows: BTreeSet<i64> = store
        .get_meta("restated_windows")?
        .map(|raw| {
            raw.split(',')
                .filter(|s| !s.is_empty())
                .filter_map(|s| s.parse().ok())
                .collect()
        })
        .unwrap_or_default();

    let mut stats = IngestStats {
        late_events: prior_late,
        restatements: prior_restatements,
        wraparounds: expander.wraparounds(),
        ..IngestStats::default()
    };

    // Cache physical file contents keyed by file name.
    let mut file_cache: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for (_seq, kind, _line_no) in &order {
        let name = format!("{kind}.log");
        file_cache
            .entry(name.clone())
            .or_insert_with(|| read_lines(&log_dir.join(&name)).unwrap_or_default());
    }

    let chunk = ctx.cfg.store.chunk_rows;
    let mut arrival_seq: u64 = store.count_records()?;
    let mut max_line: BTreeMap<String, u64> = BTreeMap::new();
    let mut buffer: Vec<PendingRow> = Vec::new();

    // Resume point: global arrival sequence of the last committed event.
    let committed_meta = store.get_meta("committed_seq")?;
    let has_cursor = committed_meta.is_some();
    let committed_seq: u64 = committed_meta.and_then(|v| v.parse().ok()).unwrap_or(0);

    // Process strictly in global arrival order. Resume skips every manifest row
    // whose arrival sequence is already durably committed, independent of its
    // physical line number (late rows share line numbers with earlier rows).
    for (seq, kind, line_no) in order {
        let file_name = format!("{kind}.log");
        if has_cursor && seq <= committed_seq {
            continue;
        }
        let raw = file_cache
            .get(&file_name)
            .and_then(|lines| lines.get(line_no.saturating_sub(1)))
            .cloned()
            .unwrap_or_default();
        buffer.push(PendingRow {
            file: file_name.clone(),
            line_no,
            raw,
            seq,
        });
        *max_line.entry(file_name.clone()).or_insert(0) =
            (line_no as u64).max(max_line.get(&file_name).copied().unwrap_or(0));

        if buffer.len() >= chunk {
            flush_chunk(
                &mut store,
                &mut expander,
                &mut tracker,
                &mut buffer,
                &mut stats,
                &mut arrival_seq,
                &mut prior_restated_windows,
            )?;
        }
    }
    if !buffer.is_empty() {
        flush_chunk(
            &mut store,
            &mut expander,
            &mut tracker,
            &mut buffer,
            &mut stats,
            &mut arrival_seq,
            &mut prior_restated_windows,
        )?;
    }
    // Mark each fully consumed file finished at its highest processed line.
    for (file_name, last_line) in max_line {
        let total = file_cache
            .get(&file_name)
            .map(|lines| lines.len() as u64)
            .unwrap_or(0);
        let finished = last_line >= total;
        store.upsert_cursor(&file_name, last_line, finished)?;
    }
    // Files with no pending rows in this pass but present on disk stay finished.
    for file_name in file_cache.keys() {
        if store.cursor_line(file_name)? == 0 {
            let total = file_cache
                .get(file_name)
                .map(|l| l.len() as u64)
                .unwrap_or(0);
            if total == 0 {
                store.upsert_cursor(file_name, 0, true)?;
            }
        }
    }

    let restated_csv = prior_restated_windows
        .iter()
        .map(|w| w.to_string())
        .collect::<Vec<_>>()
        .join(",");
    store.set_meta("late_events", &stats.late_events.to_string())?;
    store.set_meta("restatements", &stats.restatements.to_string())?;
    store.set_meta("restated_windows", &restated_csv)?;
    Ok(stats)
}

#[derive(Debug, Default, Clone)]
pub struct IngestStats {
    pub inserted: u64,
    pub duplicate: u64,
    pub quarantined: u64,
    pub late_events: u64,
    pub restatements: u64,
    pub wraparounds: u64,
}

struct PendingRow {
    file: String,
    line_no: usize,
    raw: String,
    seq: u64,
}

#[allow(clippy::too_many_arguments)]
fn flush_chunk(
    store: &mut Store,
    expander: &mut ClockExpander,
    tracker: &mut window::LateTracker,
    buffer: &mut Vec<PendingRow>,
    stats: &mut IngestStats,
    arrival_seq: &mut u64,
    prior_restated: &mut BTreeSet<i64>,
) -> Result<()> {
    let rows = std::mem::take(buffer);
    let mut local_late = 0u64;
    let tx = store.conn.transaction()?;
    let mut inserted = 0u64;
    let mut duplicate = 0u64;
    let mut quarantined = 0u64;

    // First pass: parse + clock expansion, collecting quarantine rows.
    let mut parsed_rows: Vec<(Record, u64)> = Vec::new();
    let mut quarantine_rows: Vec<Quarantined> = Vec::new();
    for row in &rows {
        match parser::parse_line(&row.file, row.line_no, &row.raw) {
            Ok(Some(record)) => {
                let event_time = expander.expand(record.raw_tick)?;
                if tracker.observe(event_time) {
                    local_late += 1;
                }
                parsed_rows.push((record, event_time));
            }
            Ok(None) => {}
            Err(reason) => quarantine_rows.push(Quarantined {
                source_file: row.file.clone(),
                line_no: row.line_no,
                raw: row.raw.clone(),
                reason,
            }),
        }
    }

    for (record, event_time) in parsed_rows {
        *arrival_seq += 1;
        let is_new = store_insert(&tx, &record, event_time, *arrival_seq)?;
        if is_new {
            inserted += 1;
        } else {
            duplicate += 1;
        }
    }
    for q in &quarantine_rows {
        tx.execute(
            "INSERT OR IGNORE INTO quarantined(source_file, line_no, raw, reason) VALUES(?1,?2,?3,?4)",
            (&q.source_file, q.line_no as i64, &q.raw, &q.reason),
        )?;
        quarantined += 1;
    }

    tx.execute(
        "INSERT INTO clock_state(id, high, last_raw, seen, wraparounds)
         VALUES(1,?1,?2,?3,?4)
         ON CONFLICT(id) DO UPDATE SET high=excluded.high, last_raw=excluded.last_raw,
             seen=excluded.seen, wraparounds=excluded.wraparounds",
        rusqlite::params![
            expander.state().high as i64,
            expander.state().last_raw as i64,
            expander.state().seen as i64,
            expander.state().wraparounds as i64
        ],
    )?;
    // Durable per-file resume cursor = highest line committed in this chunk.
    // Uses MAX so interleaved files never regress each other's cursor.
    let mut per_file: BTreeMap<String, u64> = BTreeMap::new();
    for row in &rows {
        let entry = per_file.entry(row.file.clone()).or_insert(0);
        *entry = (*entry).max(row.line_no as u64);
    }
    for (file, line) in per_file {
        tx.execute(
            "INSERT INTO ingest_cursor(source_file, last_line, finished)
             VALUES(?1,?2,0)
             ON CONFLICT(source_file) DO UPDATE SET
                last_line=MAX(ingest_cursor.last_line, excluded.last_line)",
            (file, line as i64),
        )?;
    }
    // Global arrival-sequence high water mark for crash-safe resume.
    let max_seq = rows.iter().map(|r| r.seq).max().unwrap_or(0);
    tx.execute(
        "INSERT INTO meta(key, value) VALUES('committed_seq', ?1)
         ON CONFLICT(key) DO UPDATE SET
            value=MAX(CAST(meta.value AS INTEGER), CAST(excluded.value AS INTEGER))",
        (max_seq.to_string(),),
    )?;
    tx.commit()?;

    stats.inserted += inserted;
    stats.duplicate += duplicate;
    stats.quarantined += quarantined;
    stats.late_events += local_late;
    for window_id in tracker.restated_window_ids() {
        prior_restated.insert(*window_id);
    }
    stats.restatements = prior_restated.len() as u64;
    stats.wraparounds = expander.wraparounds();
    Ok(())
}

fn store_insert(
    tx: &rusqlite::Transaction,
    record: &Record,
    event_time: u64,
    arrival_seq: u64,
) -> Result<bool> {
    let n = tx.execute(
        "INSERT OR IGNORE INTO records(
            fingerprint, source_file, line_no, raw_tick, event_time, event,
            src_ip, user, user_agent, dest_ip, dest_port, status, method,
            path, bytes, arrival_seq)
         VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16)",
        rusqlite::params![
            record.fingerprint,
            record.source_file,
            record.line_no as i64,
            record.raw_tick as i64,
            event_time as i64,
            record.event,
            record.src_ip,
            record.user,
            record.user_agent,
            record.dest_ip,
            record.dest_port.map(|v| v as i64),
            record.status.map(|v| v as i64),
            record.method,
            record.path,
            record.bytes.map(|v| v as i64),
            arrival_seq as i64,
        ],
    )?;
    Ok(n > 0)
}

fn read_manifest(path: &Path) -> Result<Vec<(u64, &'static str, usize)>> {
    let content = std::fs::read_to_string(path).map_err(|source| Error::Io {
        path: path.to_path_buf(),
        source,
    })?;
    let mut out = Vec::new();
    for (i, line) in content.lines().enumerate() {
        if i == 0 || line.trim().is_empty() {
            continue;
        }
        let parts: Vec<&str> = line.split('\t').collect();
        if parts.len() < 3 {
            return Err(Error::Other(format!("bad manifest row: {line}")));
        }
        let kind: &'static str = match parts[1] {
            "nginx" => "nginx",
            "sshd" => "sshd",
            "zeek" => "zeek",
            other => return Err(Error::Other(format!("unknown kind {other}"))),
        };
        let seq: u64 = parts[0]
            .parse()
            .map_err(|_| Error::Other(format!("bad manifest seq: {line}")))?;
        let line_no: usize = parts[2]
            .parse()
            .map_err(|_| Error::Other(format!("bad manifest line number: {line}")))?;
        out.push((seq, kind, line_no));
    }
    Ok(out)
}

fn read_lines(path: &Path) -> Result<Vec<String>> {
    let content = std::fs::read_to_string(path).map_err(|source| Error::Io {
        path: path.to_path_buf(),
        source,
    })?;
    Ok(content.lines().map(str::to_string).collect())
}

/// Load every persisted record with its expanded event time, sorted on the
/// event-time axis. This is the single source of truth for the correlate pass.
fn load_sorted(store: &Store) -> Result<Vec<(u64, Record)>> {
    let mut sql = String::from(
        "SELECT fingerprint, source_file, line_no, raw_tick, event_time, event,
                src_ip, user, user_agent, dest_ip, dest_port, status, method,
                path, bytes
         FROM records ORDER BY event_time, fingerprint",
    );
    let _ = &mut sql;
    let mut stmt = store.conn.prepare(&sql)?;
    let rows = stmt.query_map([], row_to_record)?;
    let mut out = Vec::new();
    for row in rows {
        let (record, event_time) = row?;
        out.push((event_time, record));
    }
    Ok(out)
}

fn row_to_record(row: &rusqlite::Row<'_>) -> rusqlite::Result<(Record, u64)> {
    let record = Record {
        fingerprint: row.get(0)?,
        source_file: row.get(1)?,
        line_no: row.get::<_, i64>(2)? as usize,
        raw_tick: row.get::<_, i64>(3)? as u32,
        event: row.get(5)?,
        src_ip: row.get(6)?,
        user: row.get(7)?,
        user_agent: row.get(8)?,
        dest_ip: row.get(9)?,
        dest_port: row.get::<_, Option<i64>>(10)?.map(|v| v as u16),
        status: row.get::<_, Option<i64>>(11)?.map(|v| v as u16),
        method: row.get(12)?,
        path: row.get(13)?,
        bytes: row.get::<_, Option<i64>>(14)?.map(|v| v as u64),
    };
    let event_time = row.get::<_, i64>(4)? as u64;
    Ok((record, event_time))
}

/// Full correlate pass: recompute windows, rules, sessions, anomalies from the
/// event-time-sorted records and persist all derived tables.
pub fn correlate(ctx: &RunContext) -> Result<Correlation> {
    let store = Store::open(&ctx.db_path)?;
    let annotated = load_sorted(&store)?;
    let window_length = ctx.cfg.late.window_length_secs;

    // 1. Tumbling windows.
    let event_times: Vec<u64> = annotated.iter().map(|(t, _)| *t).collect();
    let windows = window::aggregate_windows(&event_times, window_length);
    persist_windows(&store, &windows)?;

    // 2. Declarative rules.
    let hits = rules::evaluate_all(annotated.clone(), &ctx.cfg.rules);
    persist_rule_hits(&store, &hits)?;

    // 3. Sessions on (src_ip, user_agent).
    let mut sessions = session::reconstruct(&annotated, ctx.cfg.session.idle_gap_secs);

    // 4. Anomaly detection on per-IP request counts per window.
    let ip_events: Vec<(String, u64)> = annotated
        .iter()
        .map(|(t, r)| (r.src_ip.clone(), *t))
        .collect();
    let counts = window::entity_window_counts(&ip_events, window_length);
    let baselines = anomaly::baselines(&counts);
    persist_baselines(&store, &baselines)?;
    let anomalies = anomaly::detect(&counts, &baselines, &ctx.cfg.anomaly);
    persist_anomalies(&store, &anomalies, window_length)?;

    let anomaly_window_secs: Vec<(String, u64)> = anomalies
        .iter()
        .map(|a| (a.entity.clone(), (a.window_id as u64) * window_length))
        .collect();
    session::mark_anomalous(&mut sessions, &anomaly_window_secs);
    persist_sessions(&store, &sessions)?;

    let anomalous_sessions = sessions.iter().filter(|s| s.anomalous).count() as u64;

    Ok(Correlation {
        windows: windows.len() as u64,
        rule_hits: hits,
        anomalous_sessions,
        annotated,
        windows_detail: windows,
        anomalies,
        sessions,
    })
}

#[derive(Debug)]
pub struct Correlation {
    pub windows: u64,
    pub rule_hits: Vec<RuleHit>,
    pub anomalous_sessions: u64,
    pub annotated: Vec<(u64, Record)>,
    pub windows_detail: Vec<window::WindowAgg>,
    pub anomalies: Vec<anomaly::Anomaly>,
    pub sessions: Vec<session::Session>,
}

fn persist_windows(store: &Store, windows: &[window::WindowAgg]) -> Result<()> {
    let conn = &store.conn;
    conn.execute_batch("DELETE FROM window_stats")?;
    {
        let mut stmt = conn.prepare(
            "INSERT INTO window_stats(window_id, start_sec, end_sec, event_count, restatements, finalized)
             VALUES(?1,?2,?3,?4,0,1)",
        )?;
        for w in windows {
            stmt.execute(rusqlite::params![
                w.window_id,
                w.start_sec as i64,
                w.end_sec as i64,
                w.event_count as i64
            ])?;
        }
    }
    Ok(())
}

fn persist_rule_hits(store: &Store, hits: &[RuleHit]) -> Result<()> {
    let conn = &store.conn;
    conn.execute_batch("DELETE FROM rule_hits")?;
    let mut stmt = conn.prepare(
        "INSERT OR IGNORE INTO rule_hits(rule_id, group_key, window_start, event_time, fingerprint)
         VALUES(?1,?2,?3,?4,?5)",
    )?;
    for hit in hits {
        stmt.execute(rusqlite::params![
            hit.rule_id,
            hit.group_key,
            hit.window_start as i64,
            hit.event_time as i64,
            hit.fingerprint
        ])?;
    }
    Ok(())
}

fn persist_baselines(store: &Store, baselines: &[anomaly::Baseline]) -> Result<()> {
    let conn = &store.conn;
    conn.execute_batch("DELETE FROM anomaly_baseline")?;
    let mut stmt = conn.prepare(
        "INSERT INTO anomaly_baseline(entity, median, mad, samples) VALUES(?1,?2,?3,?4)",
    )?;
    for b in baselines {
        stmt.execute(rusqlite::params![
            b.entity,
            b.median,
            b.mad,
            b.samples as i64
        ])?;
    }
    Ok(())
}

fn persist_anomalies(
    store: &Store,
    anomalies: &[anomaly::Anomaly],
    window_length: u64,
) -> Result<()> {
    let conn = &store.conn;
    conn.execute_batch("DELETE FROM anomalies")?;
    let mut stmt =
        conn.prepare("INSERT OR IGNORE INTO anomalies(entity, window_id, score) VALUES(?1,?2,?3)")?;
    for a in anomalies {
        let _ = window_length;
        stmt.execute(rusqlite::params![a.entity, a.window_id, a.score])?;
    }
    Ok(())
}

fn persist_sessions(store: &Store, sessions: &[session::Session]) -> Result<()> {
    let conn = &store.conn;
    conn.execute_batch("DELETE FROM sessions")?;
    let mut stmt = conn.prepare(
        "INSERT OR REPLACE INTO sessions(session_id, src_ip, user_agent, start_sec, end_sec,
             event_count, anomalous)
         VALUES(?1,?2,?3,?4,?5,?6,?7)",
    )?;
    for s in sessions {
        stmt.execute(rusqlite::params![
            s.session_id,
            s.src_ip,
            s.user_agent,
            s.start_sec as i64,
            s.end_sec as i64,
            s.event_count as i64,
            s.anomalous as i64
        ])?;
    }
    Ok(())
}

/// Evaluate rule hits against the ground-truth labels and derive headline
/// classification metrics.
pub fn evaluate_metrics(
    correlation: &Correlation,
    truth: &Truth,
    counters: &CounterSnapshot,
    dedup_stable: bool,
    self_check_pass: bool,
) -> Metrics {
    let attack_labels: Vec<&TruthLabel> = truth.labels.iter().filter(|l| l.is_attack).collect();
    let benign_labels: Vec<&TruthLabel> = truth.labels.iter().filter(|l| !l.is_attack).collect();

    // Distinct alerts, de-duplicated by (rule_id, group_key) so a multi-window
    // probe counts as a single false-positive alert.
    let distinct_alerts: BTreeSet<(String, String)> = correlation
        .rule_hits
        .iter()
        .map(|h| (h.rule_id.clone(), h.group_key.clone()))
        .collect();

    let mut matched_attacks: HashSet<String> = HashSet::new();
    let mut fp_alerts = 0u64;

    for (rule_id, group_key) in &distinct_alerts {
        let ip = group_key.split('|').next().unwrap_or("");
        let attack_match = attack_labels.iter().any(|label| {
            label.rule_id == *rule_id
                && label.src_ip == ip
                && alert_overlaps(correlation, rule_id, group_key, label)
        });
        if attack_match {
            if let Some(label) = attack_labels
                .iter()
                .find(|l| l.rule_id == *rule_id && l.src_ip == ip)
            {
                matched_attacks.insert(label.label_id.clone());
            }
        } else {
            // Non-attack entity that nonetheless fired -> false positive alert.
            let is_benign = benign_labels.iter().any(|l| l.src_ip == ip);
            if is_benign {
                fp_alerts += 1;
            }
        }
    }

    let tp = matched_attacks.len() as u64;
    let fn_ = truth.attack_incidents.saturating_sub(tp);
    let hit_rate = if truth.attack_incidents == 0 {
        0.0
    } else {
        tp as f64 / truth.attack_incidents as f64
    };
    let false_positive_rate = if truth.benign_eval_cells == 0 {
        0.0
    } else {
        truth.fp_eval_cells as f64 / truth.benign_eval_cells as f64
    };

    Metrics {
        parsed: counters.parsed,
        quarantined: counters.quarantined,
        wraparounds: counters.wraparounds,
        late_events: counters.late_events,
        restatements: counters.restatements,
        windows: correlation.windows,
        anomalous_sessions: correlation.anomalous_sessions,
        rules_hit: correlation.rule_hits.len() as u64,
        tp,
        fp: fp_alerts,
        fn_,
        hit_rate,
        false_positive_rate,
        dedup_stable,
        self_check_pass,
    }
}

fn alert_overlaps(
    correlation: &Correlation,
    rule_id: &str,
    group_key: &str,
    label: &TruthLabel,
) -> bool {
    correlation
        .rule_hits
        .iter()
        .filter(|h| h.rule_id == rule_id && h.group_key == group_key)
        .any(|h| h.event_time >= label.start_sec && h.event_time <= label.end_sec)
}

#[derive(Debug, Clone, Copy, Default)]
pub struct CounterSnapshot {
    pub parsed: u64,
    pub quarantined: u64,
    pub wraparounds: u64,
    pub late_events: u64,
    pub restatements: u64,
}

pub fn snapshot(store: &Store, stats: &IngestStats) -> Result<CounterSnapshot> {
    Ok(CounterSnapshot {
        parsed: store.count_records()?,
        quarantined: store.count_quarantine()?,
        wraparounds: stats.wraparounds,
        late_events: stats.late_events,
        restatements: stats.restatements,
    })
}

/// Independent in-memory recomputation of every derived metric, used to prove
/// the persisted pipeline equals a naive full-sort rebuild.
pub fn self_check(ctx: &RunContext, correlation: &Correlation) -> Result<()> {
    let annotated = correlation.annotated.clone();
    let window_length = ctx.cfg.late.window_length_secs;

    // Windows.
    let times: Vec<u64> = annotated.iter().map(|(t, _)| *t).collect();
    let recomputed_windows = window::aggregate_windows(&times, window_length);
    if recomputed_windows.len() as u64 != correlation.windows
        || recomputed_windows != correlation.windows_detail
    {
        return Err(Error::SelfCheck("window recompute mismatch".into()));
    }

    // Rules.
    let recomputed_hits = rules::evaluate_all(annotated.clone(), &ctx.cfg.rules);
    if recomputed_hits != correlation.rule_hits {
        return Err(Error::SelfCheck("rule recompute mismatch".into()));
    }

    // Sessions.
    let recomputed_sessions = session::reconstruct(&annotated, ctx.cfg.session.idle_gap_secs);
    let session_signature = |s: &[session::Session]| -> Vec<(String, String, u64, u64, u64)> {
        s.iter()
            .map(|x| {
                (
                    x.src_ip.clone(),
                    x.user_agent.clone(),
                    x.start_sec,
                    x.end_sec,
                    x.event_count,
                )
            })
            .collect()
    };
    if session_signature(&recomputed_sessions) != session_signature(&correlation.sessions) {
        return Err(Error::SelfCheck("session recompute mismatch".into()));
    }

    // Anomalies.
    let ip_events: Vec<(String, u64)> = annotated
        .iter()
        .map(|(t, r)| (r.src_ip.clone(), *t))
        .collect();
    let counts = window::entity_window_counts(&ip_events, window_length);
    let baselines = anomaly::baselines(&counts);
    let recomputed_anomalies = anomaly::detect(&counts, &baselines, &ctx.cfg.anomaly);
    if recomputed_anomalies != correlation.anomalies {
        return Err(Error::SelfCheck("anomaly recompute mismatch".into()));
    }

    Ok(())
}

/// Re-ingest the same logs after clearing only the ingest cursor (records and
/// clock state are retained). Every parsed row now collides with the unique
/// fingerprint index, exercising the "conflict counted, never overwritten"
/// guarantee. Returns the duplicate-conflict stats.
pub fn reingest_for_dedup(ctx: &RunContext, log_dir: &Path) -> Result<IngestStats> {
    {
        let store = Store::open(&ctx.db_path)?;
        store.conn.execute_batch(
            "DELETE FROM ingest_cursor;
             DELETE FROM clock_state;
             UPDATE meta SET value='0' WHERE key IN
                 ('late_events','restatements');
             DELETE FROM meta WHERE key IN ('restated_windows','committed_seq');",
        )?;
    }
    let stats = ingest(ctx, log_dir)?;
    Ok(stats)
}
