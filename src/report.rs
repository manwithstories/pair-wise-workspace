use std::fs;
use std::io::BufWriter;
use std::path::Path;

use serde::Serialize;

use crate::anomaly::{self, AnomalySummary};
use crate::correlate::{self, CorrelationSummary};
use crate::store::{Store, StoreStats};

#[derive(Debug, Clone, Serialize)]
pub struct WindowReport {
    pub total: u64,
    pub closed: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct ParsingReport {
    pub raw_events: u64,
    pub parsed_successfully: u64,
    pub rejected: u64,
    pub stored_events: u64,
    pub quarantined: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct IdentityReport {
    pub wraps: u64,
    pub conflicts: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct TimingReport {
    pub late_dropped: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct Report {
    pub schema_version: u32,
    pub windows: WindowReport,
    pub parsing: ParsingReport,
    pub identity: IdentityReport,
    pub timing: TimingReport,
    pub rules: CorrelationSummary,
    pub anomalies: AnomalySummary,
}

#[derive(Debug, thiserror::Error)]
pub enum ReportError {
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("database error: {0}")]
    Database(#[from] rusqlite::Error),
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
}

pub fn build_and_write(
    database_path: &Path,
    json_path: &Path,
    display_path: &str,
) -> Result<Report, ReportError> {
    let store = Store::open(database_path)?;
    let stats = store.stats()?;
    let rules = correlate::run(&store)?;
    let anomalies = anomaly::run(&store)?;
    let report = assemble(stats, rules, anomalies);

    if let Some(parent) = json_path.parent() {
        if !parent.as_os_str().is_empty() {
            fs::create_dir_all(parent)?;
        }
    }
    let file = fs::File::create(json_path)?;
    serde_json::to_writer_pretty(BufWriter::new(file), &report)?;
    print_report(&report, display_path);
    Ok(report)
}

fn assemble(stats: StoreStats, rules: CorrelationSummary, anomalies: AnomalySummary) -> Report {
    Report {
        schema_version: 1,
        windows: WindowReport {
            total: stats.windows,
            closed: stats.closed_windows,
        },
        parsing: ParsingReport {
            raw_events: stats.raw_events(),
            parsed_successfully: stats.parse_success,
            rejected: stats.parse_rejected,
            stored_events: stats.stored_events,
            quarantined: stats.quarantined,
        },
        identity: IdentityReport {
            wraps: stats.wraps,
            conflicts: stats.identity_conflicts,
        },
        timing: TimingReport {
            late_dropped: stats.late_dropped,
        },
        rules,
        anomalies,
    }
}

fn print_report(report: &Report, display_path: &str) {
    println!(
        "窗口数={} 事件={} 解析成功={} 拒绝={}",
        report.windows.total,
        report.parsing.raw_events,
        report.parsing.parsed_successfully,
        report.parsing.rejected
    );
    println!(
        "回绕={} 身份冲突={} 迟到丢弃={}",
        report.identity.wraps, report.identity.conflicts, report.timing.late_dropped
    );
    let rules: Vec<String> = report
        .rules
        .rules
        .iter()
        .map(|rule| {
            format!(
                "{} 命中={} 真={} 误报={}",
                rule.id, rule.hits, rule.true_positives, rule.false_positives
            )
        })
        .collect();
    println!("规则 {}", rules.join("; "));
    println!(
        "规则命中率={:.3} 误报率={:.3}",
        report.rules.hit_rate, report.rules.false_positive_rate
    );
    println!(
        "异常会话数={} 异常主机={}",
        report.anomalies.abnormal_sessions, report.anomalies.abnormal_hosts
    );
    println!("report 写入 {display_path}");
}
