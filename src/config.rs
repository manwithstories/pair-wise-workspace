//! TOML configuration loading and validation.
//!
//! Validation is intentionally strict: any nonsensical value (zero windows,
//! wrap baseline outside `u32`, negative thresholds, ...) fails fast with a
//! [`crate::error::Error::Config`] instead of silently producing bad numbers.

use std::path::{Path, PathBuf};

use serde::Deserialize;

use crate::error::{Error, Result};

/// Declarative rule kinds. All rules are evaluated over the event-time axis.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RuleKind {
    /// Count distinct occurrences of an event type inside a sliding window.
    Count,
    /// At least `threshold` events whose status code is in the 4xx family.
    Status4xx,
    /// A sequence of `a` events followed by a `b` event within the window.
    Sequence,
    /// Fires when an event of `event` is *not* seen within `window_secs`.
    Absent,
}

#[derive(Debug, Clone, Deserialize)]
pub struct RuleConfig {
    pub id: String,
    pub kind: RuleKind,
    /// Human readable description, surfaced in reports.
    #[serde(default)]
    pub description: String,
    /// Sliding / absence window length in seconds.
    pub window_secs: u64,
    /// Grouping key expression: `src_ip`, `user`, `src_ip,user_agent`, ...
    pub group_by: Vec<String>,
    /// Event(s) this rule matches against [`crate::model::Record::event`].
    #[serde(default)]
    pub event: Option<String>,
    /// Second event for `sequence` rules.
    #[serde(default)]
    pub then_event: Option<String>,
    /// Firing threshold (count rules) / required occurrences.
    #[serde(default = "default_threshold")]
    pub threshold: u32,
    /// Absence rules: how long after `event` to wait for the missing event.
    #[serde(default)]
    pub missing_event: Option<String>,
}

fn default_threshold() -> u32 {
    1
}

#[derive(Debug, Clone, Deserialize)]
pub struct ClockConfig {
    /// Tick value the device counter starts from on first sighting.
    pub wrap_baseline: u32,
    /// A backward delta at least this negative magnitude means a 32-bit wrap.
    pub wrap_threshold: u64,
    /// A forward delta larger than this cannot be disambiguated from a wrap.
    pub jump_threshold: u64,
    /// Dense seconds added to reconstructed time on each detected wrap.
    pub wrap_period_secs: u64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct LateConfig {
    /// Events arriving up to this many seconds behind the watermark are late.
    pub allowed_lateness_secs: u64,
    /// Tumbling aggregation window length in seconds.
    pub window_length_secs: u64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SessionConfig {
    /// A new session starts after this idle gap between same (ip, ua) events.
    pub idle_gap_secs: u64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct AnomalyConfig {
    /// Minimum historical windows per entity before scoring is allowed.
    pub min_samples: usize,
    /// MAD z-score threshold above which a window is anomalous.
    pub z_threshold: f64,
    /// Bursts closer than this (seconds) are merged into one anomaly.
    pub merge_gap_secs: u64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct StoreConfig {
    /// Rows per committed transaction chunk / resumable batch.
    pub chunk_rows: usize,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Config {
    pub clock: ClockConfig,
    pub late: LateConfig,
    pub session: SessionConfig,
    pub anomaly: AnomalyConfig,
    pub store: StoreConfig,
    #[serde(default)]
    pub rules: Vec<RuleConfig>,
}

impl Config {
    pub fn load(path: &Path) -> Result<Config> {
        let raw = std::fs::read_to_string(path).map_err(|source| Error::Io {
            path: path.to_path_buf(),
            source,
        })?;
        let cfg: Config = toml::from_str(&raw).map_err(|e| Error::Toml(e.to_string()))?;
        cfg.validate()?;
        Ok(cfg)
    }

    pub fn validate(&self) -> Result<()> {
        if self.late.window_length_secs == 0 {
            return Err(Error::Config("late.window_length_secs must be > 0".into()));
        }
        if self.clock.wrap_threshold == 0 {
            return Err(Error::Config("clock.wrap_threshold must be > 0".into()));
        }
        if self.clock.jump_threshold == 0 {
            return Err(Error::Config("clock.jump_threshold must be > 0".into()));
        }
        if self.clock.jump_threshold < self.clock.wrap_threshold {
            return Err(Error::Config(
                "clock.jump_threshold must be >= clock.wrap_threshold".into(),
            ));
        }
        if self.clock.wrap_period_secs == 0 {
            return Err(Error::Config("clock.wrap_period_secs must be > 0".into()));
        }
        if self.session.idle_gap_secs == 0 {
            return Err(Error::Config("session.idle_gap_secs must be > 0".into()));
        }
        if self.anomaly.min_samples < 2 {
            return Err(Error::Config("anomaly.min_samples must be >= 2".into()));
        }
        if self.anomaly.z_threshold <= 0.0 || !self.anomaly.z_threshold.is_finite() {
            return Err(Error::Config(
                "anomaly.z_threshold must be a positive finite number".into(),
            ));
        }
        if self.store.chunk_rows == 0 {
            return Err(Error::Config("store.chunk_rows must be > 0".into()));
        }
        if self.rules.is_empty() {
            return Err(Error::Config("at least one rule must be declared".into()));
        }
        let mut seen = std::collections::HashSet::new();
        for rule in &self.rules {
            if rule.id.is_empty() {
                return Err(Error::Config("rule id must be non-empty".into()));
            }
            if !seen.insert(rule.id.as_str()) {
                return Err(Error::Config(format!("duplicate rule id `{}`", rule.id)));
            }
            if rule.window_secs == 0 {
                return Err(Error::Config(format!(
                    "rule `{}` window_secs must be > 0",
                    rule.id
                )));
            }
            if rule.group_by.is_empty() {
                return Err(Error::Config(format!(
                    "rule `{}` needs >= 1 group_by field",
                    rule.id
                )));
            }
            for field in &rule.group_by {
                if !is_group_field(field) {
                    return Err(Error::Config(format!(
                        "rule `{}` groups by unknown field `{}`",
                        rule.id, field
                    )));
                }
            }
            match rule.kind {
                RuleKind::Absent => {
                    if rule.event.is_none() || rule.missing_event.is_none() {
                        return Err(Error::Config(format!(
                            "absent rule `{}` needs event and missing_event",
                            rule.id
                        )));
                    }
                }
                RuleKind::Sequence => {
                    if rule.event.is_none() || rule.then_event.is_none() {
                        return Err(Error::Config(format!(
                            "sequence rule `{}` needs event and then_event",
                            rule.id
                        )));
                    }
                }
                RuleKind::Count => {
                    if rule.event.is_none() {
                        return Err(Error::Config(format!(
                            "count rule `{}` needs event",
                            rule.id
                        )));
                    }
                    if rule.threshold == 0 {
                        return Err(Error::Config(format!(
                            "count rule `{}` threshold must be > 0",
                            rule.id
                        )));
                    }
                }
                RuleKind::Status4xx => {
                    if rule.threshold == 0 {
                        return Err(Error::Config(format!(
                            "status4xx rule `{}` threshold must be > 0",
                            rule.id
                        )));
                    }
                }
            }
        }
        Ok(())
    }
}

fn is_group_field(name: &str) -> bool {
    matches!(
        name,
        "src_ip" | "user" | "user_agent" | "dest_ip" | "dest_port" | "status" | "event"
    )
}

/// Files discovered for one run, in deterministic sorted order.
pub fn discover_log_files(dir: &Path) -> Result<Vec<PathBuf>> {
    let mut files = Vec::new();
    let entries = std::fs::read_dir(dir).map_err(|source| Error::Io {
        path: dir.to_path_buf(),
        source,
    })?;
    for entry in entries {
        let entry = entry.map_err(|source| Error::Io {
            path: dir.to_path_buf(),
            source,
        })?;
        let path = entry.path();
        if path.is_file() {
            match path.extension().and_then(|e| e.to_str()) {
                Some("log") | Some("txt") => files.push(path),
                _ => {}
            }
        }
    }
    files.sort();
    Ok(files)
}
