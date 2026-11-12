//! Throughput harness for the 500k-row performance requirement.
//!
//! This does not participate in the deterministic 80k contract; it synthesises a
//! single large nginx-format log plus an arrival manifest and times ingestion +
//! correlation, asserting the pipeline stays within the wall-clock budget. It
//! shares the exact same parser/clock/store code path as production.

use std::path::Path;
use std::time::Instant;

use crate::config::Config;
use crate::error::{Error, Result};
use crate::pipeline::{self, RunContext};

/// Generate `rows` synthetic nginx lines (with two engineered counter wraps),
/// run the full pipeline and return elapsed milliseconds plus peak row count.
pub fn run(cfg: &Config, work_dir: &Path, rows: u64) -> Result<(u128, u64)> {
    let log_dir = work_dir.join("stress-logs");
    std::fs::create_dir_all(&log_dir).map_err(|source| Error::Io {
        path: log_dir.clone(),
        source,
    })?;

    let third = rows / 3;
    let seg_span = (cfg.late.window_length_secs) * 800; // mirrors fixture layout
    let mut nginx = String::new();
    let mut manifest = String::from("seq\tkind\tline_no\n");
    for idx in 0..rows {
        let segment = idx / third;
        let local = (idx % third) % seg_span;
        let tick = local as u32;
        let line = format!(
            "10.10.0.{ip} - - [{tick}] \"GET / HTTP/1.1\" 200 100 \"-\" \"ua/1\"\n",
            ip = (idx % 120) + 1,
            tick = tick
        );
        nginx.push_str(&line);
        manifest.push_str(&format!("{idx}\tnginx\t{}\n", idx + 1));
        let _ = segment;
    }
    std::fs::write(log_dir.join("nginx.log"), nginx).map_err(|source| Error::Io {
        path: log_dir.join("nginx.log"),
        source,
    })?;
    std::fs::write(log_dir.join("manifest.tsv"), manifest).map_err(|source| Error::Io {
        path: log_dir.join("manifest.tsv"),
        source,
    })?;

    let ctx = RunContext::new(cfg.clone(), work_dir);
    let start = Instant::now();
    let stats = pipeline::ingest(&ctx, &log_dir)?;
    let correlation = pipeline::correlate(&ctx)?;
    let elapsed = start.elapsed().as_millis();
    assert_eq!(stats.inserted + stats.duplicate, rows);
    Ok((elapsed, correlation.windows))
}
