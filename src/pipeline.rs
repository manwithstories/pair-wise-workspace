use std::collections::HashSet;
use std::fs;
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};

use serde::Serialize;

use crate::identity::{IdentityEngine, NatReverseIndex};
use crate::parse::{parse_line, Lease, ParseOutcome, ParseReject, ParsedRecord};
use crate::store::{EventDisposition, EventRow, Store};
use crate::timewin::{
    window_start, PushResult, ReorderBuffer, TimeNormalizer, ALLOWED_LATENESS_SECONDS,
};

const SOURCE_FILES: [(&str, &str); 3] =
    [("dhcp", "dhcp.log"), ("nat", "nat.log"), ("edr", "edr.log")];

#[derive(Debug, Clone, Default, Serialize)]
pub struct IngestStats {
    pub batches: usize,
    pub accepted_parsed: usize,
    pub rejected_parsed: usize,
    pub inserted_events: usize,
    pub duplicate_events: usize,
    pub late_dropped: usize,
    pub quarantined: usize,
    pub new_wraps: usize,
    pub new_identity_conflicts: usize,
    pub reopened_windows: usize,
}

#[derive(Debug, thiserror::Error)]
pub enum PipelineError {
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("database error: {0}")]
    Database(#[from] rusqlite::Error),
    #[error("pipeline error: {0}")]
    Message(String),
}

#[derive(Debug)]
struct BatchDirectory {
    path: PathBuf,
    name: String,
}

struct TimedRecord {
    fingerprint: String,
    record: ParsedRecord,
    raw_ts: i64,
    event_ts: i64,
    window_start: i64,
}

struct QuarantinedRecord {
    fingerprint: String,
    kind: &'static str,
    device: String,
    raw_ts: i64,
}

pub fn ingest(
    input: &Path,
    database_path: &Path,
    force_reopen_closed: bool,
) -> Result<IngestStats, PipelineError> {
    let batches = discover_batches(input)?;
    if batches.is_empty() {
        return Err(PipelineError::Message(format!(
            "no batch directories found under {}",
            input.display()
        )));
    }

    let (leases, reverse) = collect_identity_inputs(&batches)?;
    let engine = IdentityEngine::new(&leases, reverse);

    if let Some(parent) = database_path.parent() {
        if !parent.as_os_str().is_empty() {
            fs::create_dir_all(parent)?;
        }
    }
    let mut store = Store::open(database_path)?;

    let mut stats = IngestStats {
        batches: batches.len(),
        ..IngestStats::default()
    };

    let reopened_ids = if force_reopen_closed {
        let ids = store.reopen_closed_windows()?;
        stats.reopened_windows = ids.len();
        ids.into_iter().collect::<HashSet<i64>>()
    } else {
        HashSet::new()
    };

    store.in_transaction(|transaction| {
        for conflict in engine.lease_resolver().conflicts() {
            if Store::record_conflict(transaction, conflict)? {
                stats.new_identity_conflicts += 1;
            }
        }
        Ok(())
    })?;

    let mut normalizer = TimeNormalizer::new();
    let mut reorder = ReorderBuffer::new(ALLOWED_LATENESS_SECONDS);
    let mut wrap_cursor = 0usize;

    for batch in &batches {
        let (records, rejections) = parse_batch(batch)?;
        stats.accepted_parsed += records.len();
        stats.rejected_parsed += rejections.len();

        let (mut timed, quarantined) = normalize_records(records, &mut normalizer);
        timed.sort_by_key(|record| record.event_ts);

        let mut late_records = Vec::new();
        let mut ready_records = Vec::new();
        for timed_record in timed {
            match reorder.push(timed_record.event_ts, timed_record) {
                PushResult::Buffered => {}
                PushResult::Late(record) => late_records.push(record),
            }
        }
        ready_records.extend(reorder.drain_ready().into_iter().map(|(_, record)| record));

        let wrap_notices = normalizer.wrap_notices()[wrap_cursor..].to_vec();
        wrap_cursor = normalizer.wrap_notices().len();

        let watermark = reorder.watermark().unwrap_or(0);
        store.in_transaction(|transaction| {
            for rejection in &rejections {
                Store::record_rejection(transaction, rejection)?;
            }
            for notice in &wrap_notices {
                if Store::record_wrap(transaction, notice)? {
                    stats.new_wraps += 1;
                }
            }
            for quarantine in &quarantined {
                if Store::record_quarantine(
                    transaction,
                    &quarantine.fingerprint,
                    quarantine.kind,
                    &quarantine.device,
                    quarantine.raw_ts,
                    quarantine.raw_ts,
                    window_start(quarantine.raw_ts),
                    "suspicious device clock delta",
                )? {
                    stats.quarantined += 1;
                }
            }
            for record in &late_records {
                if reopened_ids.contains(&record.window_start) {
                    insert_event(
                        transaction,
                        record,
                        &engine,
                        &mut stats,
                        false,
                        "reopened closed window",
                    )?;
                } else if Store::record_late(
                    transaction,
                    &build_event_row(record, &engine),
                    "watermark lateness beyond 120s",
                )? {
                    stats.late_dropped += 1;
                } else {
                    stats.duplicate_events += 1;
                }
            }
            for record in &ready_records {
                insert_event(
                    transaction,
                    record,
                    &engine,
                    &mut stats,
                    reopened_ids.contains(&record.window_start),
                    "watermark lateness beyond 120s",
                )?;
            }
            Store::close_windows_through(transaction, watermark)?;
            Ok(())
        })?;
    }

    let final_ready: Vec<TimedRecord> = reorder
        .drain_all()
        .into_iter()
        .map(|(_, record)| record)
        .collect();
    let final_watermark = reorder.watermark().unwrap_or(0);
    store.in_transaction(|transaction| {
        for record in &final_ready {
            insert_event(
                transaction,
                record,
                &engine,
                &mut stats,
                reopened_ids.contains(&record.window_start),
                "watermark lateness beyond 120s",
            )?;
        }
        Store::close_all_windows(transaction, final_watermark)?;
        Ok(())
    })?;

    Ok(stats)
}

fn insert_event(
    transaction: &rusqlite::Transaction<'_>,
    record: &TimedRecord,
    engine: &IdentityEngine,
    stats: &mut IngestStats,
    allow_closed: bool,
    closed_reason: &str,
) -> rusqlite::Result<()> {
    let row = build_event_row(record, engine);
    if allow_closed {
        match Store::record_event(transaction, &row)? {
            EventDisposition::Inserted => stats.inserted_events += 1,
            EventDisposition::Duplicate => stats.duplicate_events += 1,
            EventDisposition::Closed => {
                if Store::record_late(transaction, &row, closed_reason)? {
                    stats.late_dropped += 1;
                } else {
                    stats.duplicate_events += 1;
                }
            }
        }
    } else {
        match Store::record_event(transaction, &row)? {
            EventDisposition::Inserted => stats.inserted_events += 1,
            EventDisposition::Duplicate => stats.duplicate_events += 1,
            EventDisposition::Closed => {
                if Store::record_late(transaction, &row, "event belongs to a closed window")? {
                    stats.late_dropped += 1;
                } else {
                    stats.duplicate_events += 1;
                }
            }
        }
    }
    Ok(())
}

fn build_event_row(record: &TimedRecord, engine: &IdentityEngine) -> EventRow {
    let common = record.record.common();
    let mut row = EventRow {
        fingerprint: record.fingerprint.clone(),
        kind: record.record.kind().to_owned(),
        device: common.device.clone(),
        batch_seq: common.batch_seq,
        event_seq: common.event_seq,
        raw_ts: record.raw_ts,
        event_ts: record.event_ts,
        window_start: record.window_start,
        host: "unknown".to_owned(),
        src_ip: None,
        src_port: None,
        dst_ip: None,
        dst_port: None,
        public_ip: None,
        public_port: None,
        action: None,
        signature: None,
        bytes: 0,
        direction: None,
    };

    match &record.record {
        ParsedRecord::Lease(lease) => {
            row.host.clone_from(&lease.host);
            row.src_ip = Some(lease.ip.clone());
            row.action = Some(lease.event.clone());
        }
        ParsedRecord::NatFlow(flow) => {
            let (src_ip, src_port, host) = engine.resolve_nat(flow, record.event_ts);
            row.host = host;
            row.src_ip = src_ip;
            row.src_port = src_port;
            row.dst_ip = Some(flow.dst_ip.clone());
            row.dst_port = Some(flow.dst_port);
            row.public_ip = Some(flow.public_ip.clone());
            row.public_port = Some(flow.public_port);
            row.action = Some(flow.action.clone());
            row.signature.clone_from(&flow.signature);
            row.bytes = flow.bytes;
            row.direction = Some(flow.direction.clone());
        }
        ParsedRecord::HostEvent(event) => {
            row.host = engine.resolve_edr(&event.host_id, event.ip.as_deref(), record.event_ts);
            row.src_ip.clone_from(&event.src_ip);
            row.dst_ip.clone_from(&event.dst_ip);
            row.dst_port = event.dst_port;
            row.action = Some(event.action.clone());
            row.signature.clone_from(&event.signature);
        }
    }
    row
}

fn normalize_records(
    mut records: Vec<ParsedRecord>,
    normalizer: &mut TimeNormalizer,
) -> (Vec<TimedRecord>, Vec<QuarantinedRecord>) {
    records.sort_by(|left, right| {
        let left = left.common();
        let right = right.common();
        (
            left.device.as_str(),
            left.batch_seq,
            left.event_seq,
            left.ts,
        )
            .cmp(&(
                right.device.as_str(),
                right.batch_seq,
                right.event_seq,
                right.ts,
            ))
    });

    let mut accepted = Vec::with_capacity(records.len());
    let mut quarantined = Vec::new();
    for record in records {
        let fingerprint = record.fingerprint();
        let common = record.common();
        match normalizer.normalize(&common.device, common.ts, common.bits, common.anchor) {
            Ok(event_ts) => accepted.push(TimedRecord {
                fingerprint,
                raw_ts: common.ts,
                window_start: window_start(event_ts),
                event_ts,
                record,
            }),
            Err(error) => {
                let kind = record.kind();
                let device = common.device.clone();
                let raw_ts = common.ts;
                quarantined.push(QuarantinedRecord {
                    fingerprint,
                    kind,
                    device,
                    raw_ts,
                });
                let _ = error;
            }
        }
    }
    (accepted, quarantined)
}

fn collect_identity_inputs(
    batches: &[BatchDirectory],
) -> Result<(Vec<Lease>, NatReverseIndex), PipelineError> {
    let mut leases = Vec::new();
    let mut reverse = NatReverseIndex::default();
    for batch in batches {
        for (kind, file_name) in SOURCE_FILES {
            let path = batch.path.join(file_name);
            if !path.exists() {
                continue;
            }
            let source = source_name(&batch.name, file_name);
            for_each_line(&path, |line_no, line| {
                match parse_line(kind, line, &source, line_no) {
                    ParseOutcome::Accepted(ParsedRecord::Lease(lease)) => leases.push(lease),
                    ParseOutcome::Accepted(ParsedRecord::NatFlow(flow)) => {
                        reverse.insert_from_flow(&flow);
                    }
                    ParseOutcome::Accepted(ParsedRecord::HostEvent(_)) => {}
                    ParseOutcome::Rejected(_) => {}
                }
            })?;
        }
    }
    Ok((leases, reverse))
}

fn parse_batch(
    batch: &BatchDirectory,
) -> Result<(Vec<ParsedRecord>, Vec<ParseReject>), PipelineError> {
    let mut records = Vec::new();
    let mut rejections = Vec::new();
    for (kind, file_name) in SOURCE_FILES {
        let path = batch.path.join(file_name);
        if !path.exists() {
            continue;
        }
        let source = source_name(&batch.name, file_name);
        for_each_line(&path, |line_no, line| {
            match parse_line(kind, line, &source, line_no) {
                ParseOutcome::Accepted(record) => records.push(record),
                ParseOutcome::Rejected(rejection) => rejections.push(rejection),
            }
        })?;
    }
    Ok((records, rejections))
}

fn for_each_line(path: &Path, mut callback: impl FnMut(u64, &str)) -> Result<(), PipelineError> {
    let file = fs::File::open(path)?;
    let mut reader = BufReader::new(file);
    let mut bytes = Vec::new();
    let mut line_no = 0u64;
    loop {
        bytes.clear();
        let read = reader.read_until(b'\n', &mut bytes)?;
        if read == 0 {
            break;
        }
        line_no += 1;
        let text = String::from_utf8_lossy(&bytes);
        let line = text.trim_end_matches(['\n', '\r']);
        if line.trim().is_empty() {
            continue;
        }
        callback(line_no, line);
    }
    Ok(())
}

fn discover_batches(input: &Path) -> Result<Vec<BatchDirectory>, PipelineError> {
    let mut batches = Vec::new();
    for entry in fs::read_dir(input)? {
        let entry = entry?;
        let file_type = entry.file_type()?;
        if !file_type.is_dir() {
            continue;
        }
        let file_name = entry.file_name().to_string_lossy().into_owned();
        if file_name.starts_with("batch-") {
            batches.push(BatchDirectory {
                path: entry.path(),
                name: file_name,
            });
        }
    }
    batches.sort_by(|left, right| left.name.cmp(&right.name));
    Ok(batches)
}

fn source_name(batch_name: &str, file_name: &str) -> String {
    if batch_name.is_empty() {
        file_name.to_owned()
    } else {
        format!("{batch_name}/{file_name}")
    }
}
