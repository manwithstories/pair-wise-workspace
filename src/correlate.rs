use std::collections::HashSet;

use serde::Serialize;

use crate::store::{RuleEvent, Store};

pub const RULE1_WINDOW_SECONDS: i64 = 300;
pub const RULE2_WINDOW_SECONDS: i64 = 900;
pub const RULE2_THRESHOLD: usize = 8;
pub const RULE3_WINDOW_SECONDS: i64 = 300;
pub const RULE3_THRESHOLD: usize = 6;

#[derive(Debug, Clone, Serialize)]
pub struct RuleResult {
    pub id: String,
    pub name: String,
    pub window_seconds: i64,
    pub threshold: usize,
    pub hits: usize,
    pub true_positives: usize,
    pub false_positives: usize,
    pub hit_rate: f64,
    pub false_positive_rate: f64,
}

#[derive(Debug, Clone, Serialize)]
pub struct CorrelationSummary {
    pub rules: Vec<RuleResult>,
    pub total_hits: usize,
    pub true_positives: usize,
    pub false_positives: usize,
    pub hit_rate: f64,
    pub false_positive_rate: f64,
}

impl CorrelationSummary {
    pub fn from_rules(rules: Vec<RuleResult>) -> Self {
        let total_hits = rules.iter().map(|rule| rule.hits).sum();
        let true_positives = rules.iter().map(|rule| rule.true_positives).sum();
        let false_positives = rules.iter().map(|rule| rule.false_positives).sum();
        let hit_rate = if total_hits == 0 {
            0.0
        } else {
            true_positives as f64 / total_hits as f64
        };
        let false_positive_rate = if total_hits == 0 {
            0.0
        } else {
            false_positives as f64 / total_hits as f64
        };
        Self {
            rules,
            total_hits,
            true_positives,
            false_positives,
            hit_rate,
            false_positive_rate,
        }
    }
}

pub fn run(store: &Store) -> rusqlite::Result<CorrelationSummary> {
    let edr = store.edr_rule_events()?;
    let nat = store.nat_rule_events()?;
    let mut rule1 = 0usize;
    let mut rule1_true = 0usize;
    let mut rule2 = 0usize;
    let mut rule2_true = 0usize;
    let mut rule3 = 0usize;
    let mut rule3_true = 0usize;

    let mut current_host = String::new();
    let mut scan: Option<(i64, bool)> = None;
    let mut failed: Option<(i64, bool)> = None;
    for event in &edr {
        if event.host != current_host {
            current_host.clone_from(&event.host);
            scan = None;
            failed = None;
        }
        match event.action.as_str() {
            "scan" => {
                scan = Some((event.event_ts, event_is_false_positive(event)));
                failed = None;
            }
            "auth_fail" => {
                if let Some((scan_ts, scan_false)) = scan {
                    if event.event_ts >= scan_ts && event.event_ts - scan_ts <= RULE1_WINDOW_SECONDS
                    {
                        failed =
                            Some((event.event_ts, scan_false || event_is_false_positive(event)));
                    }
                }
            }
            "login_success" => {
                if let (Some((scan_ts, scan_false)), Some((fail_ts, fail_false))) = (scan, failed) {
                    if fail_ts >= scan_ts
                        && event.event_ts >= fail_ts
                        && event.event_ts - scan_ts <= RULE1_WINDOW_SECONDS
                    {
                        rule1 += 1;
                        if !(scan_false || fail_false || event_is_false_positive(event)) {
                            rule1_true += 1;
                        }
                        scan = None;
                        failed = None;
                    }
                }
            }
            _ => {}
        }
    }

    let mut current: Option<(String, u16)> = None;
    let mut port_window: Vec<(i64, bool)> = Vec::new();
    for event in &nat {
        let Some(port) = event.dst_port else {
            continue;
        };
        if is_common_port(port) {
            continue;
        }
        let key = (event.host.clone(), port);
        if current.as_ref() != Some(&key) {
            current = Some(key);
            port_window.clear();
        }
        while let Some((timestamp, _)) = port_window.first() {
            if event.event_ts - *timestamp > RULE2_WINDOW_SECONDS {
                port_window.remove(0);
            } else {
                break;
            }
        }
        port_window.push((event.event_ts, event_is_false_positive(event)));
        if port_window.len() >= RULE2_THRESHOLD {
            rule2 += 1;
            if !port_window
                .iter()
                .any(|(_, false_positive)| *false_positive)
            {
                rule2_true += 1;
            }
            port_window.clear();
        }
    }

    let mut current_host = String::new();
    let mut failure_window: Vec<(i64, String, bool)> = Vec::new();
    for event in &edr {
        if event.action != "auth_fail" {
            continue;
        }
        if event.host != current_host {
            current_host.clone_from(&event.host);
            failure_window.clear();
        }
        while let Some((timestamp, _, _)) = failure_window.first() {
            if event.event_ts - *timestamp > RULE3_WINDOW_SECONDS {
                failure_window.remove(0);
            } else {
                break;
            }
        }
        let source = event.src_ip.clone().unwrap_or_else(|| "unknown".to_owned());
        failure_window.push((event.event_ts, source, event_is_false_positive(event)));
        let distinct_sources: HashSet<&str> = failure_window
            .iter()
            .map(|(_, source, _)| source.as_str())
            .collect();
        if distinct_sources.len() >= RULE3_THRESHOLD {
            rule3 += 1;
            if !failure_window
                .iter()
                .any(|(_, _, false_positive)| *false_positive)
            {
                rule3_true += 1;
            }
            failure_window.clear();
        }
    }

    let rules = vec![
        result(
            "R1",
            "扫描后认证失败再登录成功",
            RULE1_WINDOW_SECONDS,
            3,
            rule1,
            rule1_true,
        ),
        result(
            "R2",
            "罕见端口持续出站",
            RULE2_WINDOW_SECONDS,
            RULE2_THRESHOLD,
            rule2,
            rule2_true,
        ),
        result(
            "R3",
            "单主机多源认证失败",
            RULE3_WINDOW_SECONDS,
            RULE3_THRESHOLD,
            rule3,
            rule3_true,
        ),
    ];
    Ok(CorrelationSummary::from_rules(rules))
}

fn result(
    id: &str,
    name: &str,
    window_seconds: i64,
    threshold: usize,
    hits: usize,
    true_positives: usize,
) -> RuleResult {
    let false_positives = hits.saturating_sub(true_positives);
    RuleResult {
        id: id.to_owned(),
        name: name.to_owned(),
        window_seconds,
        threshold,
        hits,
        true_positives,
        false_positives,
        hit_rate: if hits == 0 {
            0.0
        } else {
            true_positives as f64 / hits as f64
        },
        false_positive_rate: if hits == 0 {
            0.0
        } else {
            false_positives as f64 / hits as f64
        },
    }
}

fn is_common_port(port: u16) -> bool {
    matches!(port, 22 | 53 | 80 | 443 | 8080 | 8443)
}

fn event_is_false_positive(event: &RuleEvent) -> bool {
    event
        .signature
        .as_deref()
        .is_some_and(is_known_probe_signature)
        || event.src_ip.as_deref().is_some_and(is_whitelisted_ip)
        || event.dst_ip.as_deref().is_some_and(is_whitelisted_ip)
}

pub fn is_known_probe_signature(signature: &str) -> bool {
    let signature = signature.to_ascii_lowercase();
    signature.contains("known_probe")
        || signature.contains("baseline_scanner")
        || signature.contains("internal_scan")
}

pub fn is_whitelisted_ip(ip: &str) -> bool {
    let octets: Option<Vec<u8>> = ip.split('.').map(|part| part.parse::<u8>().ok()).collect();
    let Some(octets) = octets else {
        return false;
    };
    if octets.len() != 4 {
        return false;
    }
    (octets[0] == 10 && octets[1] == 10 && octets[2] == 100)
        || (octets[0] == 192 && octets[1] == 168)
        || (octets[0] == 172 && (16..=31).contains(&octets[1]))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn whitelist_covers_test_ranges_only() {
        assert!(is_whitelisted_ip("10.10.100.5"));
        assert!(!is_whitelisted_ip("203.0.113.8"));
    }

    #[test]
    fn known_probe_signatures_are_case_insensitive() {
        assert!(is_known_probe_signature("Known_Probe_v2"));
        assert!(!is_known_probe_signature("ransomware"));
    }
}
