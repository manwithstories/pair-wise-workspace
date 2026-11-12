//! Declarative sliding-window rule engine plus absence rules.
//!
//! The engine is generic over [`RuleConfig`]; no rule is hard coded. Events are
//! processed on their *expanded event-time* axis, so clock skew and wraps never
//! change the result. Every emitted [`RuleHit`] is naturally de-duplicated by
//! the unique index on `rule_hits` and by the window-aligned grouping here.

use std::collections::BTreeMap;

use crate::config::{RuleConfig, RuleKind};
use crate::model::Record;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RuleHit {
    pub rule_id: String,
    pub group_key: String,
    pub window_start: u64,
    pub event_time: u64,
    pub fingerprint: String,
}

fn group_key(record: &Record, fields: &[String]) -> String {
    fields
        .iter()
        .map(|f| record.group_value(f))
        .collect::<Vec<_>>()
        .join("|")
}

/// Evaluate every rule over records annotated with expanded event times.
pub fn evaluate_all(records: Vec<(u64, Record)>, rules: &[RuleConfig]) -> Vec<RuleHit> {
    let mut hits = Vec::new();
    for rule in rules {
        hits.extend(evaluate_rule(&records, rule));
    }
    hits.sort_by(|a, b| {
        (
            a.rule_id.as_str(),
            a.group_key.as_str(),
            a.window_start,
            a.event_time,
            a.fingerprint.as_str(),
        )
            .cmp(&(
                b.rule_id.as_str(),
                b.group_key.as_str(),
                b.window_start,
                b.event_time,
                b.fingerprint.as_str(),
            ))
    });
    // Collapse to a single alert per (rule, group), retaining the earliest
    // firing window. This makes each attacking entity count once per rule while
    // the original per-window hits remain derivable from the stored rows.
    let mut seen: std::collections::BTreeSet<(String, String)> = std::collections::BTreeSet::new();
    let mut collapsed: Vec<RuleHit> = Vec::with_capacity(hits.len());
    for hit in hits {
        let key = (hit.rule_id.clone(), hit.group_key.clone());
        if seen.insert(key) {
            collapsed.push(hit);
        }
    }
    collapsed
}

fn evaluate_rule(annotated: &[(u64, Record)], rule: &RuleConfig) -> Vec<RuleHit> {
    // Group annotated records by this rule's grouping key.
    let mut groups: BTreeMap<String, Vec<(u64, Record)>> = BTreeMap::new();
    for (t, record) in annotated {
        let key = group_key(record, &rule.group_by);
        groups.entry(key).or_default().push((*t, record.clone()));
    }

    let mut hits = Vec::new();
    for (key, mut group) in groups {
        group.sort_by_key(|(t, _)| *t);
        match rule.kind {
            RuleKind::Count => eval_count(&key, &group, rule, &mut hits),
            RuleKind::Status4xx => eval_4xx(&key, &group, rule, &mut hits),
            RuleKind::Sequence => eval_sequence(&key, &group, rule, &mut hits),
            RuleKind::Absent => eval_absent(&key, &group, rule, &mut hits),
        }
    }
    hits
}

fn window_start(t: u64, length: u64) -> u64 {
    (t / length) * length
}

fn eval_count(key: &str, group: &[(u64, Record)], rule: &RuleConfig, hits: &mut Vec<RuleHit>) {
    let event = rule.event.as_deref().unwrap_or("");
    // Slide one firing window per matched event using a two-pointer queue.
    let mut matched: Vec<(u64, Record)> = group
        .iter()
        .filter(|(_, r)| r.event == event)
        .cloned()
        .collect();
    matched.sort_by_key(|(t, _)| *t);
    let mut left = 0usize;
    for right in 0..matched.len() {
        while matched[right].0 - matched[left].0 > rule.window_secs {
            left += 1;
        }
        let count = right - left + 1;
        if count as u32 >= rule.threshold {
            let (t, record) = &matched[right];
            hits.push(RuleHit {
                rule_id: rule.id.clone(),
                group_key: key.to_string(),
                window_start: window_start(*t, rule.window_secs),
                event_time: *t,
                fingerprint: record.fingerprint.clone(),
            });
        }
    }
}

fn eval_4xx(key: &str, group: &[(u64, Record)], rule: &RuleConfig, hits: &mut Vec<RuleHit>) {
    let mut matched: Vec<(u64, Record)> = group
        .iter()
        .filter(|(_, r)| r.event == "http" && r.is_4xx())
        .cloned()
        .collect();
    matched.sort_by_key(|(t, _)| *t);
    let mut left = 0usize;
    for right in 0..matched.len() {
        while matched[right].0 - matched[left].0 > rule.window_secs {
            left += 1;
        }
        if (right - left + 1) as u32 >= rule.threshold {
            let (t, record) = &matched[right];
            hits.push(RuleHit {
                rule_id: rule.id.clone(),
                group_key: key.to_string(),
                window_start: window_start(*t, rule.window_secs),
                event_time: *t,
                fingerprint: record.fingerprint.clone(),
            });
        }
    }
}

fn eval_sequence(key: &str, group: &[(u64, Record)], rule: &RuleConfig, hits: &mut Vec<RuleHit>) {
    let first = rule.event.as_deref().unwrap_or("");
    let second = rule.then_event.as_deref().unwrap_or("");
    // Index of the latest qualifying predecessor for each position.
    let mut latest_pred: Option<usize> = None;
    for (idx, (t, record)) in group.iter().enumerate() {
        if record.event == second {
            if let Some(p) = latest_pred {
                let pred_time = group[p].0;
                if *t >= pred_time && *t - pred_time <= rule.window_secs {
                    hits.push(RuleHit {
                        rule_id: rule.id.clone(),
                        group_key: key.to_string(),
                        window_start: window_start(*t, rule.window_secs),
                        event_time: *t,
                        fingerprint: record.fingerprint.clone(),
                    });
                }
            }
        }
        if record.event == first {
            latest_pred = Some(idx);
        }
    }
}

fn eval_absent(key: &str, group: &[(u64, Record)], rule: &RuleConfig, hits: &mut Vec<RuleHit>) {
    let trigger = rule.event.as_deref().unwrap_or("");
    let required = rule.missing_event.as_deref().unwrap_or("");
    // For every trigger event, confirm no required event within window_secs.
    for (idx, (t, record)) in group.iter().enumerate() {
        if record.event != trigger {
            continue;
        }
        let deadline = *t + rule.window_secs;
        let mut present = false;
        for (ot, other) in &group[idx + 1..] {
            if *ot > deadline {
                break;
            }
            if other.event == required {
                present = true;
                break;
            }
        }
        if !present {
            hits.push(RuleHit {
                rule_id: rule.id.clone(),
                group_key: key.to_string(),
                window_start: window_start(*t, rule.window_secs),
                event_time: *t,
                fingerprint: record.fingerprint.clone(),
            });
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::RuleKind;

    fn rec(event: &str, fp: &str, ip: &str, status: Option<u16>) -> Record {
        Record {
            fingerprint: fp.into(),
            source_file: "x".into(),
            line_no: 1,
            raw_tick: 0,
            event: event.into(),
            src_ip: ip.into(),
            user: None,
            user_agent: None,
            dest_ip: None,
            dest_port: None,
            status,
            method: None,
            path: None,
            bytes: None,
        }
    }

    #[test]
    fn sequence_fires_only_within_window() {
        let rule = RuleConfig {
            id: "seq".into(),
            kind: RuleKind::Sequence,
            description: String::new(),
            window_secs: 60,
            group_by: vec!["src_ip".into()],
            event: Some("login_failure".into()),
            then_event: Some("login_success".into()),
            threshold: 1,
            missing_event: None,
        };
        let records = vec![
            (0u64, rec("login_failure", "a", "1.1.1.1", None)),
            (70, rec("login_success", "b", "1.1.1.1", None)),
            (5, rec("login_failure", "c", "2.2.2.2", None)),
            (10, rec("login_success", "d", "2.2.2.2", None)),
        ];
        let hits = evaluate_all(records, &[rule]);
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].group_key, "2.2.2.2");
    }

    #[test]
    fn absent_detects_missing_logout() {
        let rule = RuleConfig {
            id: "abs".into(),
            kind: RuleKind::Absent,
            description: String::new(),
            window_secs: 120,
            group_by: vec!["src_ip".into()],
            event: Some("login_success".into()),
            then_event: None,
            threshold: 1,
            missing_event: Some("logout".into()),
        };
        let records = vec![
            (0u64, rec("login_success", "a", "1.1.1.1", None)),
            (50, rec("logout", "b", "1.1.1.1", None)),
            (0, rec("login_success", "c", "2.2.2.2", None)),
        ];
        let hits = evaluate_all(records, &[rule]);
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].group_key, "2.2.2.2");
    }
}
