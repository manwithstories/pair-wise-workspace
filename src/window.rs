//! Event-time tumbling windows with late-event handling.
//!
//! Two complementary views are provided:
//!
//! * [`LateTracker`] consumes events in **arrival order**, advances a watermark
//!   and counts late events plus the distinct closed windows they restate.
//! * [`aggregate_windows`] recomputes every tumbling window from the full,
//!   event-time-sorted record set. The report uses the recomputed values, which
//!   the self-check proves equal to a completely independent in-memory rebuild.

use std::collections::BTreeMap;
use std::collections::BTreeSet;

use crate::clock::Watermark;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WindowAgg {
    pub window_id: i64,
    pub start_sec: u64,
    pub end_sec: u64,
    pub event_count: u64,
}

/// Streaming arrival-order bookkeeping.
#[derive(Debug)]
pub struct LateTracker {
    watermark: Watermark,
    window_length: u64,
    late_events: u64,
    restated_windows: BTreeSet<i64>,
}

impl LateTracker {
    pub fn new(window_length: u64, allowed_lateness: u64) -> Self {
        Self {
            watermark: Watermark::new(allowed_lateness),
            window_length,
            late_events: 0,
            restated_windows: BTreeSet::new(),
        }
    }

    /// Feed one event in arrival order together with its expanded event time.
    pub fn observe(&mut self, event_time: u64) -> bool {
        let was_late = self.watermark.is_late(event_time, self.window_length);
        if was_late {
            self.late_events += 1;
            self.restated_windows
                .insert((event_time / self.window_length) as i64);
        }
        self.watermark.observe(event_time);
        was_late
    }

    pub fn late_events(&self) -> u64 {
        self.late_events
    }

    pub fn restatements(&self) -> u64 {
        self.restated_windows.len() as u64
    }

    pub fn watermark(&self) -> &Watermark {
        &self.watermark
    }

    pub fn restated_window_ids(&self) -> std::collections::btree_set::Iter<'_, i64> {
        self.restated_windows.iter()
    }
}

/// Recompute all tumbling windows from expanded event times.
pub fn aggregate_windows(event_times: &[u64], window_length: u64) -> Vec<WindowAgg> {
    let mut counts: BTreeMap<i64, u64> = BTreeMap::new();
    for t in event_times {
        let id = (*t / window_length) as i64;
        *counts.entry(id).or_insert(0) += 1;
    }
    counts
        .into_iter()
        .map(|(id, event_count)| {
            let start = id as u64 * window_length;
            WindowAgg {
                window_id: id,
                start_sec: start,
                end_sec: start + window_length,
                event_count,
            }
        })
        .collect()
}

/// Per-entity (e.g. src_ip) request counts keyed by window id.
pub fn entity_window_counts(
    events: &[(String, u64)],
    window_length: u64,
) -> BTreeMap<String, BTreeMap<i64, u64>> {
    let mut out: BTreeMap<String, BTreeMap<i64, u64>> = BTreeMap::new();
    for (entity, t) in events {
        let id = (*t / window_length) as i64;
        *out.entry(entity.clone())
            .or_default()
            .entry(id)
            .or_insert(0) += 1;
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counts_late_and_distinct_restatements() {
        let mut tracker = LateTracker::new(10, 2);
        for t in [0u64, 5, 12, 30, 35] {
            tracker.observe(t);
        }
        // watermark now 35; window [0,10) end=10, closed once wm >= 12.
        tracker.observe(3); // late -> window 0
        tracker.observe(4); // late -> same window 0, no extra restatement
        tracker.observe(13); // window 1 [10,20), end 20, closed once wm>=22 -> late
        assert_eq!(tracker.late_events(), 3);
        assert_eq!(tracker.restatements(), 2);
    }

    #[test]
    fn aggregates_deterministically() {
        let agg = aggregate_windows(&[0, 9, 10, 60, 61], 10);
        assert_eq!(agg.len(), 3);
        assert_eq!(agg[0].event_count, 2);
        assert_eq!(agg[1].event_count, 1);
        assert_eq!(agg[2].event_count, 2);
    }
}
