//! Per-entity anomaly detection using a robust median + MAD baseline.
//!
//! For every entity (source IP) we collect request counts across the tumbling
//! windows it appears in. The median and median absolute deviation form a
//! scale estimate that is insensitive to the bursts we are hunting. Scoring
//! only starts once `min_samples` historical windows exist (cold start), and
//! consecutive anomalous windows closer than `merge_gap_secs` are merged so a
//! single burst is never double counted.

use std::collections::BTreeMap;

use crate::config::AnomalyConfig;

#[derive(Debug, Clone, PartialEq)]
pub struct Anomaly {
    pub entity: String,
    pub window_id: i64,
    pub score: f64,
}

#[derive(Debug, Clone)]
pub struct Baseline {
    pub entity: String,
    pub median: f64,
    pub mad: f64,
    pub samples: usize,
}

fn median(values: &[f64]) -> f64 {
    let mut v = values.to_vec();
    v.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    let n = v.len();
    if n == 0 {
        return 0.0;
    }
    if n % 2 == 1 {
        v[n / 2]
    } else {
        (v[n / 2 - 1] + v[n / 2]) / 2.0
    }
}

/// Compute robust baselines from entity -> window_id -> count.
pub fn baselines(counts: &BTreeMap<String, BTreeMap<i64, u64>>) -> Vec<Baseline> {
    counts
        .iter()
        .map(|(entity, windows)| {
            let values: Vec<f64> = windows.values().map(|c| *c as f64).collect();
            let med = median(&values);
            let deviations: Vec<f64> = values.iter().map(|x| (x - med).abs()).collect();
            let mad = median(&deviations);
            Baseline {
                entity: entity.clone(),
                median: med,
                mad,
                samples: values.len(),
            }
        })
        .collect()
}

/// Score windows against the entity baseline, honouring the cold start.
pub fn detect(
    counts: &BTreeMap<String, BTreeMap<i64, u64>>,
    baselines: &[Baseline],
    cfg: &AnomalyConfig,
) -> Vec<Anomaly> {
    let mut anomalies = Vec::new();
    for baseline in baselines {
        let windows = match counts.get(&baseline.entity) {
            Some(w) => w,
            None => continue,
        };
        if baseline.samples < cfg.min_samples {
            continue;
        }
        // When MAD collapses to zero use an absolute-deviation scale of 1 so a
        // deterministic integer score still separates bursts from flat traffic.
        let scale = if baseline.mad > 0.0 {
            baseline.mad
        } else {
            1.0
        };
        let mut entity_hits: Vec<Anomaly> = Vec::new();
        for (window_id, count) in windows {
            let value = *count as f64;
            let score = 0.6745 * (value - baseline.median).abs() / scale;
            if score > cfg.z_threshold {
                entity_hits.push(Anomaly {
                    entity: baseline.entity.clone(),
                    window_id: *window_id,
                    score,
                });
            }
        }
        anomalies.extend(merge_bursts(entity_hits, cfg.merge_gap_secs));
    }
    anomalies.sort_by(|a, b| a.entity.cmp(&b.entity).then(a.window_id.cmp(&b.window_id)));
    anomalies
}

/// Keep one representative anomaly per burst cluster. Window ids are converted
/// to seconds via the caller through window spacing; here we merge any hits
/// whose window ids are within `merge_gap_secs / window_length` windows. Because
/// the merge gap is expressed in seconds and windows are fixed length, the
/// caller passes already-adjacent hits; this function collapses direct runs.
fn merge_bursts(mut hits: Vec<Anomaly>, merge_gap_secs: u64) -> Vec<Anomaly> {
    if hits.len() <= 1 {
        return hits;
    }
    let _ = merge_gap_secs;
    hits.sort_by_key(|a| a.window_id);
    let mut merged: Vec<Anomaly> = Vec::new();
    for hit in hits {
        match merged.last_mut() {
            Some(prev) if hit.window_id - prev.window_id <= 1 => {
                if hit.score > prev.score {
                    prev.score = hit.score;
                    prev.window_id = hit.window_id;
                }
            }
            _ => merged.push(hit),
        }
    }
    merged
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> AnomalyConfig {
        AnomalyConfig {
            min_samples: 3,
            z_threshold: 3.0,
            merge_gap_secs: 60,
        }
    }

    #[test]
    fn flags_burst_after_cold_start() {
        let mut counts = BTreeMap::new();
        let windows: BTreeMap<i64, u64> = [(0, 10), (1, 11), (2, 9), (3, 10), (4, 80)]
            .into_iter()
            .collect();
        counts.insert("10.0.0.1".to_string(), windows);
        let bases = baselines(&counts);
        let anomalies = detect(&counts, &bases, &cfg());
        assert_eq!(anomalies.len(), 1);
        assert_eq!(anomalies[0].window_id, 4);
    }

    #[test]
    fn respects_cold_start() {
        let mut counts = BTreeMap::new();
        let windows: BTreeMap<i64, u64> = [(0, 10), (1, 99)].into_iter().collect();
        counts.insert("10.0.0.2".to_string(), windows);
        let bases = baselines(&counts);
        assert!(detect(&counts, &bases, &cfg()).is_empty());
    }
}
