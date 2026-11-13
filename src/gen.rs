use std::fmt::Write as _;
use std::fs::{self, File};
use std::io::{BufWriter, Write};
use std::path::Path;

use rand::rngs::StdRng;
use rand::seq::SliceRandom;
use rand::{Rng, SeedableRng};
use serde::Serialize;

pub const DEFAULT_RAW_LINES: usize = 120_000;
pub const DEFAULT_BATCHES: usize = 48;
pub const DEFAULT_BASE_TS: i64 = 1_700_006_400;
pub const BATCH_SPACING_SECONDS: i64 = 900;
pub const SCENARIO_HOURS: i64 = 12;

const REFERENCE_ACCEPTED: usize = 119_431;
const REFERENCE_REJECTED: usize = 569;
const REFERENCE_DHCP: usize = 1_200;
const REFERENCE_NAT: usize = 60_000;
const REFERENCE_LATE: usize = 112;

const NORMAL_HOSTS: [&str; 20] = [
    "finance-01",
    "finance-02",
    "finance-03",
    "eng-01",
    "eng-02",
    "eng-03",
    "lab-01",
    "lab-02",
    "lab-03",
    "qa-01",
    "qa-02",
    "qa-03",
    "ops-01",
    "ops-02",
    "hr-01",
    "hr-02",
    "maintenance-01",
    "maintenance-02",
    "visitor-01",
    "visitor-02",
];

const ANOMALY_HOSTS: [&str; 6] = [
    "anomaly-01",
    "anomaly-02",
    "anomaly-03",
    "anomaly-04",
    "anomaly-05",
    "anomaly-06",
];

const BENIGN_ACTIONS: [&str; 8] = [
    "file_open",
    "process_start",
    "dns_query",
    "network_connect",
    "config_read",
    "policy_update",
    "usb_insert",
    "service_status",
];

#[derive(Debug, Clone, Serialize)]
pub struct Manifest {
    pub schema_version: u32,
    pub seed: u64,
    pub raw_lines: usize,
    pub accepted_lines: usize,
    pub malformed_lines: usize,
    pub batches: usize,
    pub base_ts: i64,
    pub late_injected: usize,
    pub wrap_injected: usize,
    pub dhcp_lines: usize,
    pub nat_lines: usize,
    pub edr_lines: usize,
}

#[derive(Debug, thiserror::Error)]
pub enum GenError {
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
    #[error("invalid fixture request: {0}")]
    Invalid(String),
}

#[derive(Default)]
struct LineArena {
    data: String,
    lines: Vec<(u32, u32)>,
}

impl LineArena {
    fn push(&mut self, line: String) {
        let offset = self.data.len();
        assert!(offset <= u32::MAX as usize, "line arena exceeded 4 GiB");
        assert!(line.len() < u32::MAX as usize, "log line exceeded 4 GiB");
        self.lines.push((offset as u32, (line.len() + 1) as u32));
        self.data.push_str(&line);
        self.data.push('\n');
    }

    fn shuffle(&mut self, rng: &mut StdRng) {
        self.lines.shuffle(rng);
    }

    fn write_to(&self, path: &Path) -> Result<(), GenError> {
        let mut writer = BufWriter::new(File::create(path)?);
        for (start, length) in &self.lines {
            let start = *start as usize;
            let end = start + *length as usize;
            writer.write_all(&self.data.as_bytes()[start..end])?;
        }
        writer.flush()?;
        Ok(())
    }
}

#[derive(Default)]
struct BatchLines {
    dhcp: LineArena,
    nat: LineArena,
    edr: LineArena,
}

struct Generator {
    rng: StdRng,
    base_ts: i64,
    batches: usize,
    next_event_seq: u64,
    lines: Vec<BatchLines>,
}

#[derive(Clone)]
struct LeaseDefinition {
    ip: String,
    host: String,
    start: i64,
    end: i64,
    event: &'static str,
}

pub fn generate(seed: u64, output: &Path, raw_lines: usize) -> Result<Manifest, GenError> {
    if raw_lines < 20_000 {
        return Err(GenError::Invalid(
            "raw_lines must be at least 20000".to_owned(),
        ));
    }
    let malformed_lines = raw_lines * REFERENCE_REJECTED / DEFAULT_RAW_LINES;
    let accepted_lines = raw_lines - malformed_lines;
    let dhcp_lines = accepted_lines * REFERENCE_DHCP / REFERENCE_ACCEPTED;
    let nat_lines = accepted_lines * REFERENCE_NAT / REFERENCE_ACCEPTED;
    let edr_lines = accepted_lines - dhcp_lines - nat_lines;
    let late_injected = accepted_lines * REFERENCE_LATE / REFERENCE_ACCEPTED;

    if dhcp_lines < 41 {
        return Err(GenError::Invalid("not enough DHCP rows".to_owned()));
    }
    let fixed_nat = anomaly_flow_count() + 90;
    if nat_lines <= fixed_nat {
        return Err(GenError::Invalid("not enough NAT rows".to_owned()));
    }
    let fixed_edr = 42 + 132 + 2 + late_injected;
    if edr_lines <= fixed_edr {
        return Err(GenError::Invalid("not enough EDR rows".to_owned()));
    }

    let mut generator = Generator::new(seed, accepted_lines, late_injected);
    generator.generate_leases(dhcp_lines);
    generator.generate_anomaly_flows();
    generator.generate_rule2_flows();
    generator.generate_general_nat(nat_lines - fixed_nat);
    generator.generate_rule1_events();
    generator.generate_rule3_events();
    generator.generate_legacy_wrap_events();
    generator.generate_late_events(late_injected);
    generator.generate_edr_noise(edr_lines - fixed_edr);
    generator.generate_malformed(malformed_lines);

    fs::create_dir_all(output)?;
    for (batch, batch_lines) in generator.lines.iter_mut().enumerate() {
        let directory = output.join(format!("batch-{:03}", batch + 1));
        fs::create_dir_all(&directory)?;
        batch_lines.dhcp.shuffle(&mut generator.rng);
        batch_lines.nat.shuffle(&mut generator.rng);
        batch_lines.edr.shuffle(&mut generator.rng);
        batch_lines.dhcp.write_to(&directory.join("dhcp.log"))?;
        batch_lines.nat.write_to(&directory.join("nat.log"))?;
        batch_lines.edr.write_to(&directory.join("edr.log"))?;
    }

    let manifest = Manifest {
        schema_version: 1,
        seed,
        raw_lines,
        accepted_lines,
        malformed_lines,
        batches: DEFAULT_BATCHES,
        base_ts: DEFAULT_BASE_TS,
        late_injected,
        wrap_injected: 1,
        dhcp_lines,
        nat_lines,
        edr_lines,
    };
    let manifest_file = File::create(output.join("manifest.json"))?;
    serde_json::to_writer_pretty(BufWriter::new(manifest_file), &manifest)?;
    Ok(manifest)
}

impl Generator {
    fn new(seed: u64, _accepted: usize, _late: usize) -> Self {
        Self {
            rng: StdRng::seed_from_u64(seed),
            base_ts: DEFAULT_BASE_TS,
            batches: DEFAULT_BATCHES,
            next_event_seq: 0,
            lines: (0..DEFAULT_BATCHES)
                .map(|_| BatchLines::default())
                .collect(),
        }
    }

    fn next_seq(&mut self) -> u64 {
        self.next_event_seq += 1;
        self.next_event_seq
    }

    fn window_start(&self, batch: usize) -> i64 {
        self.base_ts + batch as i64 * BATCH_SPACING_SECONDS
    }

    fn random_batch(&mut self) -> usize {
        self.rng.gen_range(0..self.batches)
    }

    fn random_jitter(&mut self) -> i64 {
        self.rng.gen_range(0..300)
    }

    fn emit_dhcp(&mut self, batch: usize, line: String) {
        self.lines[batch % self.batches].dhcp.push(line);
    }

    fn emit_nat(&mut self, batch: usize, line: String) {
        self.lines[batch % self.batches].nat.push(line);
    }

    fn emit_edr(&mut self, batch: usize, line: String) {
        self.lines[batch % self.batches].edr.push(line);
    }

    fn generate_leases(&mut self, target: usize) {
        let duration = SCENARIO_HOURS * 3600;
        let mut definitions: Vec<LeaseDefinition> = Vec::new();
        for (index, host) in NORMAL_HOSTS.iter().chain(ANOMALY_HOSTS.iter()).enumerate() {
            definitions.push(LeaseDefinition {
                ip: if index < NORMAL_HOSTS.len() {
                    normal_ip(index)
                } else {
                    anomaly_ip(index - NORMAL_HOSTS.len())
                },
                host: (*host).to_owned(),
                start: self.base_ts,
                end: self.base_ts + duration,
                event: "new",
            });
        }

        for index in 0..4 {
            let old_host = ["finance-03", "eng-03", "lab-03", "qa-03"][index];
            let new_host = ["visitor-03", "visitor-04", "visitor-05", "visitor-06"][index];
            let ip = format!("10.10.2.{}", 50 + index);
            definitions.push(LeaseDefinition {
                ip: ip.clone(),
                host: old_host.to_owned(),
                start: self.base_ts,
                end: self.base_ts + duration / 2,
                event: "new",
            });
            definitions.push(LeaseDefinition {
                ip,
                host: new_host.to_owned(),
                start: self.base_ts + duration / 2,
                end: self.base_ts + duration,
                event: "change",
            });
        }

        for index in 0..7 {
            definitions.push(LeaseDefinition {
                ip: normal_ip(index),
                host: format!("contractor-{:02}", index + 1),
                start: self.base_ts + duration * 7 / 12,
                end: self.base_ts + duration,
                event: "new",
            });
        }

        if target < definitions.len() {
            return;
        }
        for lease in &definitions {
            let seq = self.next_seq();
            let batch = ((lease.start - self.base_ts) / BATCH_SPACING_SECONDS) as usize;
            let line = dhcp_line(
                seq,
                batch,
                "dhcp-dc-01",
                lease.start,
                &lease.ip,
                &lease.host,
                lease.start,
                lease.end,
                lease.event,
            );
            self.emit_dhcp(batch, line);
        }

        let renewals = target - definitions.len();
        let stable_count = NORMAL_HOSTS.len() + ANOMALY_HOSTS.len();
        for index in 0..renewals {
            let host_index = index % stable_count;
            let batch = self.random_batch();
            let ts = self.window_start(batch) + self.random_jitter();
            let ip = if host_index < NORMAL_HOSTS.len() {
                normal_ip(host_index)
            } else {
                anomaly_ip(host_index - NORMAL_HOSTS.len())
            };
            let host = if host_index < NORMAL_HOSTS.len() {
                NORMAL_HOSTS[host_index]
            } else {
                ANOMALY_HOSTS[host_index - NORMAL_HOSTS.len()]
            };
            let seq = self.next_seq();
            let line = dhcp_line(
                seq,
                batch,
                "dhcp-cache-02",
                ts,
                &ip,
                host,
                self.base_ts,
                self.base_ts + duration,
                "renew",
            );
            self.emit_dhcp(batch, line);
        }
    }

    fn generate_anomaly_flows(&mut self) {
        let anomaly_counts = [6usize, 6, 6, 6, 7, 6];
        for hour in 0..SCENARIO_HOURS as usize {
            for (host_index, _) in ANOMALY_HOSTS.iter().enumerate() {
                let count = if hour == 9 {
                    anomaly_counts[host_index]
                } else {
                    4
                };
                for flow_index in 0..count {
                    let batch = hour * 4 + flow_index % 4;
                    let ts = self.window_start(batch) + 20 + flow_index as i64 * 7;
                    let bytes = if hour == 9 {
                        if host_index < 5 {
                            55_000 + host_index as u64 * 1_100 + flow_index as u64 * 37
                        } else {
                            120 + flow_index as u64
                        }
                    } else {
                        2_000 + host_index as u64 * 37 + hour as u64 * 11
                    };
                    let seq = self.next_seq();
                    let line = nat_line(
                        seq,
                        batch,
                        "nat-cache-03",
                        ts,
                        Some((&anomaly_ip(host_index), 40_000 + flow_index as u16)),
                        "203.0.113.21",
                        46_000 + host_index as u16 * 100 + hour as u16,
                        "198.51.100.7",
                        443,
                        bytes,
                        None,
                    );
                    self.emit_nat(batch, line);
                }
            }
        }
    }

    fn generate_rule2_flows(&mut self) {
        let rare_ports = [
            4444u16, 5555, 9001, 12_000, 31_337, 47_001, 50_000, 60_001, 62_000,
        ];
        for (cluster, port) in rare_ports.iter().enumerate() {
            let host_index = (cluster * 2) % NORMAL_HOSTS.len();
            let batch = (cluster * 5 + 3) % self.batches;
            let start = self.window_start(batch) + 10;
            let (dst_ip, signature) = if cluster == 0 {
                ("10.10.100.5", Some("known_probe"))
            } else {
                ("198.51.100.99", None)
            };
            for flow in 0..10usize {
                let seq = self.next_seq();
                let line = nat_line(
                    seq,
                    batch,
                    "nat-cache-03",
                    start + flow as i64 * 5,
                    Some((&normal_ip(host_index), (41_000 + flow) as u16)),
                    "203.0.113.22",
                    61_000 + cluster as u16 * 10 + flow as u16,
                    dst_ip,
                    *port,
                    4_096,
                    signature,
                );
                self.emit_nat(batch, line);
            }
        }
    }

    fn generate_general_nat(&mut self, count: usize) {
        let mut anchors: Vec<(String, u16)> = Vec::new();
        for index in 0..count {
            let batch = index % self.batches;
            let host_index = index % NORMAL_HOSTS.len();
            let ts = self.window_start(batch) + self.random_jitter();
            let src_ip = normal_ip(host_index);
            let src_port = (40_000 + index % 20_000) as u16;

            if index < 600 {
                anchors.push((src_ip.clone(), src_port));
                let seq = self.next_seq();
                let line = nat_line(
                    seq,
                    batch,
                    "nat-gw-01",
                    ts,
                    Some((&src_ip, src_port)),
                    "203.0.113.20",
                    (20_000 + index) as u16,
                    common_dst_ip(index),
                    common_dst_port(index),
                    normal_flow_bytes(host_index),
                    None,
                );
                self.emit_nat(batch, line);
            } else if index < 1_200 {
                let anchor_index = index - 600;
                let seq = self.next_seq();
                let line = nat_line(
                    seq,
                    batch,
                    "nat-gw-01",
                    ts,
                    None,
                    "203.0.113.20",
                    (20_000 + anchor_index) as u16,
                    common_dst_ip(index),
                    common_dst_port(index),
                    normal_flow_bytes(host_index),
                    None,
                );
                self.emit_nat(batch, line);
                // This line exercises the reverse index only when the anchor
                // endpoint is present in the first parsing pass.
                let _ = &anchors[anchor_index];
            } else if index < 1_280 {
                let seq = self.next_seq();
                let line = nat_line(
                    seq,
                    batch,
                    "nat-gw-01",
                    ts,
                    None,
                    "198.18.255.1",
                    (64_000 + index - 1_200) as u16,
                    common_dst_ip(index),
                    common_dst_port(index),
                    normal_flow_bytes(host_index),
                    None,
                );
                self.emit_nat(batch, line);
            } else {
                let seq = self.next_seq();
                let line = nat_line(
                    seq,
                    batch,
                    "nat-gw-01",
                    ts,
                    Some((&src_ip, src_port)),
                    "203.0.113.20",
                    (30_000 + (index - 1_280) % 30_000) as u16,
                    common_dst_ip(index),
                    common_dst_port(index),
                    normal_flow_bytes(host_index),
                    None,
                );
                self.emit_nat(batch, line);
            }
        }
    }

    fn generate_rule1_events(&mut self) {
        for index in 0..14usize {
            let batch = (index * 7 + 2) % self.batches;
            let host = NORMAL_HOSTS[index % NORMAL_HOSTS.len()];
            let start = self.window_start(batch) + 20;
            let signature = if index < 3 { Some("known_probe") } else { None };
            let attacker = format!("203.0.113.{}", 30 + index);

            let seq = self.next_seq();
            let line = edr_line(
                seq,
                batch,
                "edr-hub-01",
                start,
                host,
                "scan",
                Some(&normal_ip(index % NORMAL_HOSTS.len())),
                Some(&attacker),
                None,
                None,
                Some("security-admin"),
                signature,
                "",
            );
            self.emit_edr(batch, line);

            let seq = self.next_seq();
            let line = edr_line(
                seq,
                batch,
                "edr-hub-01",
                start + 40,
                host,
                "auth_fail",
                Some(&normal_ip(index % NORMAL_HOSTS.len())),
                Some(&attacker),
                None,
                None,
                Some("security-admin"),
                signature,
                "",
            );
            self.emit_edr(batch, line);

            let seq = self.next_seq();
            let line = edr_line(
                seq,
                batch,
                "edr-hub-01",
                start + 80,
                host,
                "login_success",
                Some(&normal_ip(index % NORMAL_HOSTS.len())),
                Some(&attacker),
                None,
                None,
                Some("security-admin"),
                signature,
                "",
            );
            self.emit_edr(batch, line);
        }
    }

    fn generate_rule3_events(&mut self) {
        for scenario in 0..22usize {
            let batch = (scenario * 5 + 7) % self.batches;
            let host = NORMAL_HOSTS[scenario % NORMAL_HOSTS.len()];
            let start = self.window_start(batch) + 25;
            for source_index in 0..6usize {
                let source_ip = if scenario < 5 {
                    format!("10.10.100.{}", 10 + source_index)
                } else {
                    format!("203.0.113.{}", 60 + (scenario * 6 + source_index) % 180)
                };
                let seq = self.next_seq();
                let line = edr_line(
                    seq,
                    batch,
                    "edr-hub-01",
                    start + source_index as i64 * 5,
                    host,
                    "auth_fail",
                    Some(&normal_ip(scenario % NORMAL_HOSTS.len())),
                    Some(&source_ip),
                    None,
                    None,
                    Some("admin"),
                    if scenario < 5 {
                        Some("known_probe")
                    } else {
                        None
                    },
                    "",
                );
                self.emit_edr(batch, line);
            }
        }
    }

    fn generate_legacy_wrap_events(&mut self) {
        let batch = 24usize;
        let raw_zero_ts = self.window_start(batch) + 50;
        let modulus = 1_i64 << 32;
        let raw0 = modulus - 10;
        let anchor = raw_zero_ts - raw0;

        let seq = self.next_seq();
        let clock_fields = format!("bits=32 anchor={anchor}");
        let line = edr_line(
            seq,
            batch,
            "edr-legacy-01",
            modulus - 20,
            "lab-legacy",
            "process_start",
            Some("10.10.2.30"),
            None,
            None,
            None,
            Some("operator"),
            None,
            &clock_fields,
        );
        self.emit_edr(batch, line);

        let seq = self.next_seq();
        let line = edr_line(
            seq,
            batch,
            "edr-legacy-01",
            5,
            "lab-legacy",
            "file_open",
            Some("10.10.2.30"),
            None,
            None,
            None,
            Some("operator"),
            None,
            &clock_fields,
        );
        self.emit_edr(batch, line);
    }

    fn generate_late_events(&mut self, count: usize) {
        for index in 0..count {
            let batch = 1 + index % (self.batches - 1);
            let old_ts = self.window_start(batch) - 750 - (index % 50) as i64;
            let host_index = index % NORMAL_HOSTS.len();
            let seq = self.next_seq();
            let line = edr_line(
                seq,
                batch,
                "edr-late-buffer",
                old_ts,
                NORMAL_HOSTS[host_index],
                BENIGN_ACTIONS[index % BENIGN_ACTIONS.len()],
                Some(&normal_ip(host_index)),
                None,
                None,
                None,
                Some("backfill"),
                None,
                "",
            );
            self.emit_edr(batch, line);
        }
    }

    fn generate_edr_noise(&mut self, count: usize) {
        for index in 0..count {
            let batch = index % self.batches;
            let ts = self.window_start(batch) + self.random_jitter();
            let anomaly = index % 7 == 0;
            let host = if anomaly {
                ANOMALY_HOSTS[index % ANOMALY_HOSTS.len()]
            } else {
                NORMAL_HOSTS[index % NORMAL_HOSTS.len()]
            };
            let ip = if anomaly {
                anomaly_ip(index % ANOMALY_HOSTS.len())
            } else {
                normal_ip(index % NORMAL_HOSTS.len())
            };
            let seq = self.next_seq();
            let line = edr_line(
                seq,
                batch,
                "edr-hub-01",
                ts,
                host,
                BENIGN_ACTIONS[index % BENIGN_ACTIONS.len()],
                Some(&ip),
                None,
                None,
                None,
                Some("employee"),
                None,
                "",
            );
            self.emit_edr(batch, line);
        }
    }

    fn generate_malformed(&mut self, count: usize) {
        for index in 0..count {
            let batch = index % self.batches;
            let seq = self.next_seq();
            let line = malformed_line(index, seq, batch);
            match index % 3 {
                0 => self.emit_dhcp(batch, line),
                1 => self.emit_nat(batch, line),
                _ => self.emit_edr(batch, line),
            }
        }
    }
}

fn normal_ip(index: usize) -> String {
    format!("10.10.2.{}", 10 + index)
}

fn anomaly_ip(index: usize) -> String {
    format!("10.10.2.{}", 40 + index)
}

fn common_dst_ip(index: usize) -> &'static str {
    const DESTINATIONS: [&str; 4] = [
        "198.51.100.10",
        "198.51.100.11",
        "198.51.100.12",
        "198.51.100.13",
    ];
    DESTINATIONS[index % DESTINATIONS.len()]
}

fn common_dst_port(index: usize) -> u16 {
    const PORTS: [u16; 5] = [443, 80, 53, 8080, 22];
    PORTS[index % PORTS.len()]
}

fn normal_flow_bytes(host_index: usize) -> u64 {
    50_000 + host_index as u64 * 100
}

fn anomaly_flow_count() -> usize {
    6 * (SCENARIO_HOURS as usize - 1) * 4 + 37
}

#[allow(clippy::too_many_arguments)]
fn dhcp_line(
    seq: u64,
    batch: usize,
    device: &str,
    ts: i64,
    ip: &str,
    host: &str,
    lease_start: i64,
    lease_end: i64,
    event: &str,
) -> String {
    let mac = format!(
        "00:16:3e:{:02x}:{:02x}:{:02x}",
        (seq >> 16) & 0xff,
        (seq >> 8) & 0xff,
        seq & 0xff
    );
    format!(
        "ts={ts} dev={device} batch={} seq={seq} ip={ip} host=\"{host}\" mac={mac} lease_start={lease_start} lease_end={lease_end} event={event}",
        batch + 1
    )
}

#[allow(clippy::too_many_arguments)]
fn nat_line(
    seq: u64,
    batch: usize,
    device: &str,
    ts: i64,
    source: Option<(&str, u16)>,
    public_ip: &str,
    public_port: u16,
    dst_ip: &str,
    dst_port: u16,
    bytes: u64,
    signature: Option<&str>,
) -> String {
    let mut line = format!("ts={ts} dev={device} batch={} seq={seq}", batch + 1);
    if let Some((src_ip, src_port)) = source {
        let _ = write!(line, " src_ip={src_ip} src_port={src_port}");
    }
    let _ = write!(
        line,
        " pub_ip={public_ip} pub_port={public_port} dst_ip={dst_ip} dst_port={dst_port} proto=tcp bytes={bytes} direction=outbound action=open"
    );
    if let Some(signature) = signature {
        let _ = write!(line, " signature={signature}");
    }
    line
}

#[allow(clippy::too_many_arguments)]
fn edr_line(
    seq: u64,
    batch: usize,
    device: &str,
    ts: i64,
    host: &str,
    action: &str,
    ip: Option<&str>,
    src_ip: Option<&str>,
    dst_ip: Option<&str>,
    dst_port: Option<u16>,
    user: Option<&str>,
    signature: Option<&str>,
    clock_fields: &str,
) -> String {
    let mut line = format!("ts={ts} dev={device} batch={} seq={seq}", batch + 1);
    if !clock_fields.is_empty() {
        let _ = write!(line, " {clock_fields}");
    }
    let _ = write!(line, " host_id=\"{host}\"");
    if let Some(ip) = ip {
        let _ = write!(line, " ip={ip}");
    }
    let _ = write!(line, " action={action}");
    if let Some(src_ip) = src_ip {
        let _ = write!(line, " src_ip={src_ip}");
    }
    if let Some(dst_ip) = dst_ip {
        let _ = write!(line, " dst_ip={dst_ip}");
    }
    if let Some(dst_port) = dst_port {
        let _ = write!(line, " dst_port={dst_port}");
    }
    if let Some(user) = user {
        let _ = write!(line, " user=\"{user}\"");
    }
    if let Some(signature) = signature {
        let _ = write!(line, " signature={signature}");
    }
    line
}

fn malformed_line(index: usize, seq: u64, batch: usize) -> String {
    let long = "malformed-quoted-field-value-that-is-intentionally-longer-than-120-bytes-for-truncation-check";
    match index % 5 {
        0 => format!(
            "ts=nope dev=dhcp-dc-01 batch={} seq={seq} ip=10.10.2.1 host=bad malformed",
            batch + 1
        ),
        1 => format!(
            "ts=1700006400 dev=\"broken-{index} batch={} seq={seq} host=\"{long}",
            batch + 1
        ),
        2 => {
            format!("ts=1700006400 dev=edr-hub-01 batch=bad seq={seq} host_id=bad action=file_open")
        }
        3 => format!(
            "ts=1700006400 dev=nat-gw-01 batch={} seq={seq} broken_token pub_port=1000",
            batch + 1
        ),
        _ => format!(
            "ts=1700006400 dev=edr-hub-01 ts=1700006401 batch={} seq={seq} host_id=bad action=scan",
            batch + 1
        ),
    }
}
