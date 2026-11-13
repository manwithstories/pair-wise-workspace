use std::collections::HashSet;

use serde::Serialize;

use crate::store::{HourAggregate, Store};

pub const EWMA_ALPHA: f64 = 0.3;
pub const ROBUST_Z_THRESHOLD: f64 = 3.5;
pub const MIN_BASELINE_SAMPLES: usize = 8;

#[derive(Debug, Clone, Serialize)]
pub struct AnomalyPoint {
    pub host: String,
    pub hour_start: i64,
    pub bytes: u64,
    pub sessions: u64,
    pub ewma: f64,
    pub z: f64,
    pub direction: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct AnomalySummary {
    pub abnormal_sessions: u64,
    pub abnormal_hosts: usize,
    pub spike_buckets: usize,
    pub collapse_buckets: usize,
    pub points: Vec<AnomalyPoint>,
    pub alpha: f64,
    pub z_threshold: f64,
    pub min_samples: usize,
}

pub fn run(store: &Store) -> rusqlite::Result<AnomalySummary> {
    let hourly = store.nat_hourly()?;
    Ok(detect(&hourly))
}

pub fn detect(hourly: &[HourAggregate]) -> AnomalySummary {
    let mut points = Vec::new();
    let mut index = 0usize;
    while index < hourly.len() {
        let host = hourly[index].host.as_str();
        let mut end = index;
        while end < hourly.len() && hourly[end].host == host {
            end += 1;
        }
        detect_host(&hourly[index..end], &mut points);
        index = end;
    }

    let abnormal_hosts: HashSet<&str> = points.iter().map(|point| point.host.as_str()).collect();
    let abnormal_sessions = points.iter().map(|point| point.sessions).sum();
    let spike_buckets = points
        .iter()
        .filter(|point| point.direction == "spike")
        .count();
    let collapse_buckets = points
        .iter()
        .filter(|point| point.direction == "collapse")
        .count();

    AnomalySummary {
        abnormal_sessions,
        abnormal_hosts: abnormal_hosts.len(),
        spike_buckets,
        collapse_buckets,
        points,
        alpha: EWMA_ALPHA,
        z_threshold: ROBUST_Z_THRESHOLD,
        min_samples: MIN_BASELINE_SAMPLES,
    }
}

fn detect_host(buckets: &[HourAggregate], points: &mut Vec<AnomalyPoint>) {
    if buckets.is_empty() {
        return;
    }
    let mut mean = buckets[0].bytes as f64;
    let mut absolute_deviation = 0.0f64;
    let mut history = vec![buckets[0].bytes as f64];

    for bucket in buckets.iter().skip(1) {
        let value = bucket.bytes as f64;
        if history.len() >= MIN_BASELINE_SAMPLES {
            let median = median(&history);
            let mad = median_absolute_deviation(&history, median);
            let scale = (1.4826 * mad)
                .max(1.4826 * absolute_deviation)
                .max(mean.abs() * 0.02)
                .max(1.0);
            let z = (value - mean) / scale;
            if z.abs() > ROBUST_Z_THRESHOLD {
                points.push(AnomalyPoint {
                    host: bucket.host.clone(),
                    hour_start: bucket.hour_start,
                    bytes: bucket.bytes,
                    sessions: bucket.sessions,
                    ewma: mean,
                    z,
                    direction: if z > 0.0 { "spike" } else { "collapse" }.to_owned(),
                });
            }
        }

        let previous_mean = mean;
        mean = EWMA_ALPHA * value + (1.0 - EWMA_ALPHA) * mean;
        absolute_deviation =
            EWMA_ALPHA * (value - previous_mean).abs() + (1.0 - EWMA_ALPHA) * absolute_deviation;
        history.push(value);
    }
}

fn median(values: &[f64]) -> f64 {
    let mut ordered = values.to_vec();
    ordered.sort_by(|left, right| left.total_cmp(right));
    if ordered.len() % 2 == 1 {
        ordered[ordered.len() / 2]
    } else {
        (ordered[ordered.len() / 2 - 1] + ordered[ordered.len() / 2]) / 2.0
    }
}

fn median_absolute_deviation(values: &[f64], median_value: f64) -> f64 {
    let deviations: Vec<f64> = values
        .iter()
        .map(|value| (value - median_value).abs())
        .collect();
    median(&deviations)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn aggregate(host: &str, bytes: u64, sessions: u64) -> HourAggregate {
        HourAggregate {
            host: host.to_owned(),
            hour_start: 0,
            bytes,
            sessions,
        }
    }

    #[test]
    fn needs_eight_prior_samples_before_judging() {
        let mut buckets = Vec::new();
        for index in 0..8 {
            buckets.push(aggregate("host", 1000, 10));
            if index == 0 {
                break;
            }
        }
        let summary = detect(&buckets);
        assert!(summary.points.is_empty());
    }

    #[test]
    fn identifies_spike_after_baseline() {
        let mut buckets: Vec<HourAggregate> = (0..9).map(|_| aggregate("host", 1000, 4)).collect();
        buckets.push(aggregate("host", 80_000, 7));
        let summary = detect(&buckets);
        assert_eq!(summary.points.len(), 1);
        assert_eq!(summary.abnormal_sessions, 7);
        assert_eq!(summary.points[0].direction, "spike");
    }
}
