//! Session reconstruction from event-time-sorted records.
//!
//! A session is the run of consecutive events sharing `(src_ip, user_agent)`
//! with no idle gap longer than `idle_gap_secs`. Because input is sorted by
//! expanded event time, clock wraps and out-of-order arrival can neither split
//! a real session nor double count events.

use std::collections::BTreeMap;

use crate::model::Record;

#[derive(Debug, Clone)]
pub struct Session {
    pub session_id: i64,
    pub src_ip: String,
    pub user_agent: String,
    pub start_sec: u64,
    pub end_sec: u64,
    pub event_count: u64,
    pub anomalous: bool,
}

/// Rebuild sessions. `annotated` is `(expanded_event_time, record)` and need
/// not be pre-sorted; it is sorted here deterministically.
pub fn reconstruct(annotated: &[(u64, Record)], idle_gap_secs: u64) -> Vec<Session> {
    let mut ordered: Vec<&(u64, Record)> = annotated.iter().collect();
    ordered.sort_by(|a, b| {
        a.1.src_ip
            .cmp(&b.1.src_ip)
            .then_with(|| ua(&a.1).cmp(ua(&b.1)))
            .then_with(|| a.0.cmp(&b.0))
            .then_with(|| a.1.fingerprint.cmp(&b.1.fingerprint))
    });

    #[derive(Default)]
    struct Acc {
        start: u64,
        end: u64,
        count: u64,
    }
    let mut keyed: BTreeMap<(String, String), Vec<Acc>> = BTreeMap::new();

    for (time, record) in ordered {
        let key = (record.src_ip.clone(), ua(record).to_string());
        let runs = keyed.entry(key).or_default();
        match runs.last_mut() {
            Some(last) if *time <= last.end || *time - last.end <= idle_gap_secs => {
                last.end = last.end.max(*time);
                last.count += 1;
            }
            _ => runs.push(Acc {
                start: *time,
                end: *time,
                count: 1,
            }),
        }
    }

    let mut sessions = Vec::new();
    let mut id = 0i64;
    for ((src_ip, user_agent), runs) in keyed {
        for run in runs {
            id += 1;
            sessions.push(Session {
                session_id: id,
                src_ip: src_ip.clone(),
                user_agent: user_agent.clone(),
                start_sec: run.start,
                end_sec: run.end,
                event_count: run.count,
                anomalous: false,
            });
        }
    }
    sessions
}

fn ua(record: &Record) -> &str {
    record.user_agent.as_deref().unwrap_or("unknown")
}

/// Mark sessions overlapping an anomalous window of the same source IP.
pub fn mark_anomalous(sessions: &mut [Session], anomaly_window_secs: &[(String, u64)]) {
    let by_ip: BTreeMap<&str, Vec<u64>> = {
        let mut map: BTreeMap<&str, Vec<u64>> = BTreeMap::new();
        for (ip, start) in anomaly_window_secs {
            map.entry(ip.as_str()).or_default().push(*start);
        }
        for v in map.values_mut() {
            v.sort_unstable();
            v.dedup();
        }
        map
    };
    for session in sessions.iter_mut() {
        if let Some(starts) = by_ip.get(session.src_ip.as_str()) {
            let overlaps = starts.iter().any(|start| {
                let end = start.saturating_add(1);
                session.start_sec < end && *start <= session.end_sec
            });
            if overlaps {
                session.anomalous = true;
            }
        }
    }
}
