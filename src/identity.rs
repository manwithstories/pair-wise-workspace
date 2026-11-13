use std::collections::HashMap;

use crate::parse::{Lease, NatFlow};

#[derive(Debug, Clone)]
pub struct EffectiveInterval {
    pub ip: String,
    pub start: i64,
    pub end: i64,
    pub host: String,
    pub lease_fingerprint: String,
}

#[derive(Debug, Clone)]
pub struct IdentityConflict {
    pub fingerprint: String,
    pub ip: String,
    pub previous_host: String,
    pub winning_host: String,
    pub effective_from: i64,
    pub reason: String,
}

#[derive(Debug, Default)]
pub struct LeaseResolver {
    intervals: HashMap<String, Vec<EffectiveInterval>>,
    conflicts: Vec<IdentityConflict>,
}

impl LeaseResolver {
    pub fn build(leases: &[Lease]) -> Self {
        let mut resolver = Self::default();
        let mut ordered: Vec<&Lease> = leases.iter().filter(|lease| lease.is_new).collect();
        ordered.sort_by(|left, right| {
            (
                &left.ip,
                left.lease_start,
                left.lease_end,
                left.common.batch_seq,
                left.common.event_seq,
                &left.host,
            )
                .cmp(&(
                    &right.ip,
                    right.lease_start,
                    right.lease_end,
                    right.common.batch_seq,
                    right.common.event_seq,
                    &right.host,
                ))
        });

        for lease in ordered {
            let fingerprint = format!(
                "lease-interval:{}:{}:{}:{}",
                lease.ip, lease.lease_start, lease.lease_end, lease.host
            );
            let entry = resolver.intervals.entry(lease.ip.clone()).or_default();
            if let Some(previous) = entry.last_mut() {
                if lease.lease_start < previous.end {
                    resolver.conflicts.push(IdentityConflict {
                        fingerprint: format!(
                            "identity-conflict:{}:{}:{}:{}",
                            lease.ip, lease.lease_start, previous.host, fingerprint
                        ),
                        ip: lease.ip.clone(),
                        previous_host: previous.host.clone(),
                        winning_host: lease.host.clone(),
                        effective_from: lease.lease_start,
                        reason: "overlapping lease; later effective lease wins".to_owned(),
                    });
                    previous.end = lease.lease_start;
                }
            }
            entry.push(EffectiveInterval {
                ip: lease.ip.clone(),
                start: lease.lease_start,
                end: lease.lease_end,
                host: lease.host.clone(),
                lease_fingerprint: fingerprint,
            });
        }
        resolver
    }

    pub fn resolve(&self, ip: &str, event_ts: i64) -> Option<&str> {
        let intervals = self.intervals.get(ip)?;
        let mut low = 0usize;
        let mut high = intervals.len();
        while low < high {
            let middle = (low + high) / 2;
            if intervals[middle].start <= event_ts {
                low = middle + 1;
            } else {
                high = middle;
            }
        }
        if low == 0 {
            return None;
        }
        let interval = &intervals[low - 1];
        if event_ts >= interval.start && event_ts < interval.end {
            Some(interval.host.as_str())
        } else {
            None
        }
    }

    pub fn conflicts(&self) -> &[IdentityConflict] {
        &self.conflicts
    }

    pub fn interval_count(&self) -> usize {
        self.intervals.values().map(Vec::len).sum()
    }
}

#[derive(Debug, Clone, Hash, PartialEq, Eq)]
pub struct PublicEndpoint {
    pub ip: String,
    pub port: u16,
}

#[derive(Debug, Clone)]
pub struct ReverseSource {
    pub src_ip: String,
    pub src_port: u16,
}

#[derive(Debug, Default)]
pub struct NatReverseIndex {
    entries: HashMap<PublicEndpoint, ReverseSource>,
}

impl NatReverseIndex {
    pub fn insert_from_flow(&mut self, flow: &NatFlow) {
        if let (Some(src_ip), Some(src_port)) = (&flow.src_ip, flow.src_port) {
            self.entries.insert(
                PublicEndpoint {
                    ip: flow.public_ip.clone(),
                    port: flow.public_port,
                },
                ReverseSource {
                    src_ip: src_ip.clone(),
                    src_port,
                },
            );
        }
    }

    pub fn lookup(&self, public_ip: &str, public_port: u16) -> Option<&ReverseSource> {
        self.entries.get(&PublicEndpoint {
            ip: public_ip.to_owned(),
            port: public_port,
        })
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }
}

#[derive(Debug)]
pub struct IdentityEngine {
    leases: LeaseResolver,
    reverse: NatReverseIndex,
}

impl IdentityEngine {
    pub fn new(leases: &[Lease], reverse: NatReverseIndex) -> Self {
        Self {
            leases: LeaseResolver::build(leases),
            reverse,
        }
    }

    pub fn lease_resolver(&self) -> &LeaseResolver {
        &self.leases
    }

    pub fn resolve_edr(&self, host_id: &str, ip: Option<&str>, event_ts: i64) -> String {
        ip.and_then(|ip| self.leases.resolve(ip, event_ts))
            .unwrap_or(host_id)
            .to_owned()
    }

    pub fn resolve_nat(
        &self,
        flow: &NatFlow,
        event_ts: i64,
    ) -> (Option<String>, Option<u16>, String) {
        let reverse_source = if flow.src_ip.is_none() || flow.src_port.is_none() {
            self.reverse.lookup(&flow.public_ip, flow.public_port)
        } else {
            None
        };
        let src_ip = flow
            .src_ip
            .clone()
            .or_else(|| reverse_source.map(|source| source.src_ip.clone()));
        let src_port = flow
            .src_port
            .or_else(|| reverse_source.map(|source| source.src_port));
        let host = src_ip
            .as_deref()
            .and_then(|ip| self.leases.resolve(ip, event_ts))
            .unwrap_or("unknown")
            .to_owned();
        (src_ip, src_port, host)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parse::Common;

    fn common(seq: u64) -> Common {
        Common {
            device: "dhcp".to_owned(),
            batch_seq: 1,
            event_seq: seq,
            ts: 100,
            bits: 64,
            anchor: None,
            content_hash: format!("h{seq}"),
        }
    }

    fn lease(ip: &str, host: &str, start: i64, end: i64, seq: u64) -> Lease {
        Lease {
            common: common(seq),
            ip: ip.to_owned(),
            host: host.to_owned(),
            mac: "00:00:00:00:00:01".to_owned(),
            lease_start: start,
            lease_end: end,
            event: "new".to_owned(),
            is_new: true,
        }
    }

    #[test]
    fn intervals_are_left_closed_right_open() {
        let resolver = LeaseResolver::build(&[lease("10.0.0.1", "a", 100, 200, 1)]);
        assert_eq!(resolver.resolve("10.0.0.1", 100), Some("a"));
        assert_eq!(resolver.resolve("10.0.0.1", 199), Some("a"));
        assert_eq!(resolver.resolve("10.0.0.1", 200), None);
    }

    #[test]
    fn later_overlapping_lease_wins() {
        let leases = [
            lease("10.0.0.1", "a", 100, 300, 1),
            lease("10.0.0.1", "b", 200, 300, 2),
        ];
        let resolver = LeaseResolver::build(&leases);
        assert_eq!(resolver.resolve("10.0.0.1", 150), Some("a"));
        assert_eq!(resolver.resolve("10.0.0.1", 250), Some("b"));
        assert_eq!(resolver.conflicts().len(), 1);
    }
}
