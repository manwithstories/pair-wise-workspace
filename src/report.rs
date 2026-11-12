//! Deterministic reporting: terminal table, JSON and CSV plus a stable hash.
//!
//! All collections are sorted before rendering so repeated runs and replays
//! produce byte-identical artifacts. The report hash covers the canonical
//! metric lines and is therefore stable under duplicate import.

use std::collections::BTreeMap;
use std::path::Path;

use crate::error::Result;

#[derive(Debug, Clone)]
pub struct Metrics {
    pub parsed: u64,
    pub quarantined: u64,
    pub wraparounds: u64,
    pub late_events: u64,
    pub restatements: u64,
    pub windows: u64,
    pub anomalous_sessions: u64,
    pub rules_hit: u64,
    pub tp: u64,
    pub fp: u64,
    pub fn_: u64,
    pub hit_rate: f64,
    pub false_positive_rate: f64,
    pub dedup_stable: bool,
    pub self_check_pass: bool,
}

impl Metrics {
    /// Canonical, ordered key=value lines. Hashing these makes the report hash
    /// independent of map iteration order.
    pub fn canonical_lines(&self) -> Vec<String> {
        vec![
            format!("parsed={}", self.parsed),
            format!("quarantined={}", self.quarantined),
            format!("wraparounds={}", self.wraparounds),
            format!("late_events={}", self.late_events),
            format!("restatements={}", self.restatements),
            format!("windows={}", self.windows),
            format!("anomalous_sessions={}", self.anomalous_sessions),
            format!("rules_hit={}", self.rules_hit),
            format!("tp={}", self.tp),
            format!("fp={}", self.fp),
            format!("fn={}", self.fn_),
            format!("hit_rate={:.4}", self.hit_rate),
            format!("false_positive_rate={:.4}", self.false_positive_rate),
        ]
    }

    pub fn summary_line(&self) -> String {
        format!(
            "{} dedup_stable={} self_check={}",
            self.canonical_lines().join(" "),
            if self.dedup_stable { "yes" } else { "no" },
            if self.self_check_pass { "PASS" } else { "FAIL" }
        )
    }
}

/// FNV-1a 64-bit hash over the canonical report lines.
pub fn report_hash(metrics: &Metrics) -> String {
    let mut hash: u64 = 0xcbf29ce484222325;
    for line in metrics.canonical_lines() {
        for byte in line.as_bytes() {
            hash ^= *byte as u64;
            hash = hash.wrapping_mul(0x100000001b3);
        }
        hash ^= b'\n' as u64;
        hash = hash.wrapping_mul(0x100000001b3);
    }
    format!("{:016x}", hash)
}

pub fn write_artifacts(dir: &Path, metrics: &Metrics) -> Result<()> {
    std::fs::create_dir_all(dir).map_err(|source| crate::error::Error::Io {
        path: dir.to_path_buf(),
        source,
    })?;

    // Terminal table (also persisted as report.txt).
    let table = render_table(metrics);
    std::fs::write(dir.join("report.txt"), &table).map_err(|source| crate::error::Error::Io {
        path: dir.join("report.txt"),
        source,
    })?;
    println!("{table}");

    let json = serde_json::json!({
        "metrics": serde_json::Value::Object(
            metrics
                .canonical_lines()
                .iter()
                .filter_map(|line| line.split_once('=').map(|(k, v)| (k.to_string(), parse_value(v))))
                .collect::<BTreeMap<_, _>>()
                .into_iter()
                .collect()
        ),
        "dedup_stable": metrics.dedup_stable,
        "self_check": if metrics.self_check_pass { "PASS" } else { "FAIL" },
        "report_hash": report_hash(metrics),
    });
    std::fs::write(dir.join("report.json"), serde_json::to_vec_pretty(&json)?).map_err(
        |source| crate::error::Error::Io {
            path: dir.join("report.json"),
            source,
        },
    )?;

    let mut csv = String::from("metric,value\n");
    for line in metrics.canonical_lines() {
        let (k, v) = line.split_once('=').unwrap();
        csv.push_str(&format!("{k},{v}\n"));
    }
    csv.push_str(&format!("dedup_stable,{}\n", metrics.dedup_stable));
    csv.push_str(&format!(
        "self_check,{}\n",
        if metrics.self_check_pass {
            "PASS"
        } else {
            "FAIL"
        }
    ));
    csv.push_str(&format!("report_hash,{}\n", report_hash(metrics)));
    std::fs::write(dir.join("report.csv"), &csv).map_err(|source| crate::error::Error::Io {
        path: dir.join("report.csv"),
        source,
    })?;
    Ok(())
}

fn parse_value(raw: &str) -> serde_json::Value {
    if let Ok(int) = raw.parse::<i64>() {
        serde_json::Value::from(int)
    } else if let Ok(float) = raw.parse::<f64>() {
        serde_json::Value::from(float)
    } else {
        serde_json::Value::String(raw.to_string())
    }
}

fn render_table(metrics: &Metrics) -> String {
    let mut out = String::new();
    out.push_str("secpipe security pipeline report\n");
    out.push_str("--------------------------------\n");
    for line in metrics.canonical_lines() {
        let (k, v) = line.split_once('=').unwrap();
        out.push_str(&format!("{:<22} {}\n", k, v));
    }
    out.push_str(&format!(
        "{:<22} {}\n",
        "dedup_stable",
        if metrics.dedup_stable { "yes" } else { "no" }
    ));
    out.push_str(&format!(
        "{:<22} {}\n",
        "self_check",
        if metrics.self_check_pass {
            "PASS"
        } else {
            "FAIL"
        }
    ));
    out.push_str(&format!("{:<22} {}\n", "report_hash", report_hash(metrics)));
    out
}
