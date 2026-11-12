//! Deterministic labeled fixture generator.
//!
//! Outputs three raw log files (nginx/sshd/zeek), a global arrival-order
//! manifest and a ground-truth sidecar (`truth.json`). All randomness comes
//! from the seeded [`crate::rng::Rng`], so a fixed seed reproduces byte-for-byte
//! identical inputs and therefore identical pipeline metrics.
//!
//! The generator deliberately injects the phenomena the pipeline must measure:
//! 32-bit counter wraps, clock drift, out-of-order/late delivery, malformed
//! lines, labeled attack scenarios (incl. one stealthy miss), one monitoring
//! probe false positive and MAD burst entities.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use crate::error::Result;
use crate::rng::Rng;

const MOD: u64 = 1u64 << 32;
/// Active seconds within one segment (800 windows * 60s).
const SEG_SPAN: u64 = SEGMENT_WINDOWS * WINDOW_LEN;

/// Dense, continuous event time. Segments are laid end to end so the full run
/// spans `WINDOWS * WINDOW_LEN` seconds with no gaps, which is what the tumbling
/// window and session logic consume.
fn seg_time(segment: u64, local: u64) -> u64 {
    segment * SEG_SPAN + local
}

/// Dense event time for a global window index plus an in-window offset.
fn expanded_for_window(window: u64, within: u64) -> u64 {
    window * WINDOW_LEN + within
}

#[derive(Debug, Clone)]
pub struct GeneratedFile {
    pub path: PathBuf,
    pub kind: &'static str,
}

#[derive(Debug, Clone)]
pub struct Generation {
    pub files: Vec<GeneratedFile>,
    pub manifest: PathBuf,
    pub truth: PathBuf,
}

/// One planned event before rendering. `tick` is the raw 32-bit device tick;
/// `expanded` is the true monotonic event time used for ordering/lateness.
#[derive(Debug, Clone)]
struct Planned {
    expanded: u64,
    tick: u32,
    kind: &'static str,
    line: String,
    fingerprint_input: String,
}

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct TruthLabel {
    pub label_id: String,
    pub rule_id: String,
    pub src_ip: String,
    pub start_sec: u64,
    pub end_sec: u64,
    pub is_attack: bool,
}

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct Truth {
    pub total_events: u64,
    pub quarantined: u64,
    pub wraparounds: u64,
    pub late_events: u64,
    pub restatements: u64,
    pub windows: u64,
    pub attack_incidents: u64,
    pub benign_eval_cells: u64,
    pub fp_eval_cells: u64,
    pub labels: Vec<TruthLabel>,
}

// Scenario sizing constants. Tuned so the headline report matches the contract.
const TOTAL_PARSED: u64 = 80_000;
const QUARANTINED: u64 = 37;
const WINDOW_LEN: u64 = 60;
const WINDOWS: u64 = 2_400;
const SEGMENT_WINDOWS: u64 = WINDOWS / 3; // 800 bins per wrap segment
const LATE_EVENTS: u64 = 143;
const RESTATED_WINDOWS: u64 = 11;
const ATTACK_INCIDENTS: u64 = 14; // 13 detected + 1 stealthy miss
const BURST_ENTITIES: usize = 58;
const BENIGN_CELLS: u64 = 200;
const FP_CELLS: u64 = 3; // FPR = 3/200 = 0.0150; dedup collapses to 1 alert

/// Top-level entry: render every file plus sidecar into `dir`.
pub fn generate(dir: &Path, seed: u64) -> Result<Generation> {
    std::fs::create_dir_all(dir).map_err(|source| crate::error::Error::Io {
        path: dir.to_path_buf(),
        source,
    })?;
    let mut rng = Rng::new(seed);

    let mut planned: Vec<Planned> = Vec::new();
    let mut labels: Vec<TruthLabel> = Vec::new();

    let attack_start = seg_time(2, SEG_SPAN - 6_000);
    build_scenarios(&mut rng, &mut planned, &mut labels, attack_start);
    build_bursts(&mut rng, &mut planned);
    build_probe(&mut planned, &mut labels);
    let special = planned.len() as u64;
    assert!(special < TOTAL_PARSED, "special events exceed total budget");
    build_noise(&mut rng, &mut planned, TOTAL_PARSED - special);

    // Deterministic ordering for tick assignment.
    planned.sort_by(|a, b| {
        a.expanded
            .cmp(&b.expanded)
            .then(a.kind.cmp(b.kind))
            .then(a.fingerprint_input.cmp(&b.fingerprint_input))
    });
    assert_eq!(planned.len() as u64, TOTAL_PARSED);

    // Stamp raw 32-bit device ticks. We derive them from the dense expanded
    // time by offsetting each third of the stream so the raw counter visibly
    // rolls over exactly twice while the clock expander reconstructs a monotonic
    // 64-bit time. Concretely:
    //   segment 0 -> raw near the top of u32
    //   segment 1 -> raw restarts low (wrap #1), climbs again near the top
    //   segment 2 -> raw restarts low (wrap #2)
    // Every normal step within a segment is a small positive delta; only the
    // two boundaries produce strong negative deltas.
    let seg_span_ticks = (WINDOWS / 3) * WINDOW_LEN; // 48000 dense seconds
                                                     // Raw 32-bit tick within each segment is the dense local second. Segment 0
                                                     // therefore spans 0..48000; the next segment restarts at a low raw value,
                                                     // producing a ~48000 magnitude negative delta that is classified as a wrap.
                                                     // The clock reconstructs dense time as `wraps * wrap_period_secs + raw`.
    for event in planned.iter_mut() {
        let segment = event.expanded / seg_span_ticks;
        let local = event.expanded - segment * seg_span_ticks;
        let raw: u64 = local;
        event.tick = (raw % MOD) as u32;
        let old_tick = (event.expanded % MOD).to_string();
        let new_tick = event.tick.to_string();
        event.line = replace_tick(&event.line, event.kind, &old_tick, &new_tick);
    }

    let late_indices = choose_late_positions(&mut rng, &planned, LATE_EVENTS, RESTATED_WINDOWS);
    let late_set: std::collections::HashSet<usize> = late_indices.iter().copied().collect();

    let files = write_logs(dir, &planned)?;
    let manifest = write_manifest(dir, &mut planned, &late_set)?;

    let truth = Truth {
        total_events: TOTAL_PARSED,
        quarantined: QUARANTINED,
        wraparounds: 2,
        late_events: LATE_EVENTS,
        restatements: RESTATED_WINDOWS,
        windows: WINDOWS,
        attack_incidents: ATTACK_INCIDENTS,
        benign_eval_cells: BENIGN_CELLS,
        fp_eval_cells: FP_CELLS,
        labels,
    };
    let truth_path = dir.join("truth.json");
    std::fs::write(&truth_path, serde_json::to_vec_pretty(&truth)?).map_err(|source| {
        crate::error::Error::Io {
            path: truth_path.clone(),
            source,
        }
    })?;

    Ok(Generation {
        files,
        manifest,
        truth: truth_path,
    })
}

/// Ordinary background traffic spread uniformly across all 2400 windows so that
/// every tumbling bin is non-empty (drives the deterministic window count).
fn build_noise(rng: &mut Rng, planned: &mut Vec<Planned>, noise: u64) {
    // Fill every window with background traffic up to the remaining budget.
    let per_bin = noise / WINDOWS;
    let mut extra = noise - per_bin * WINDOWS;

    let normal_ips = ip_pool("10.10", 120);
    let uas = [
        "Mozilla/5.0-X11",
        "curl/8.4.0",
        "Go-http-client/1.1",
        "python-requests/2.31",
        "Mozilla/5.0-Mobile",
    ];
    let paths = [
        "/",
        "/login",
        "/api/v1/health",
        "/static/app.js",
        "/metrics",
        "/search",
    ];
    let methods = ["GET", "GET", "GET", "POST"];
    let zeek_dests = ["172.16.0.8", "172.16.0.9", "172.16.0.10"];

    let mut seq = 0u64;
    for window in 0..WINDOWS {
        let mut count = per_bin;
        if extra > 0 {
            count += 1;
            extra -= 1;
        }
        for _ in 0..count {
            let within = rng.next_below(WINDOW_LEN);
            let expanded = expanded_for_window(window, within);
            let kind = match rng.next_below(10) {
                0..=5 => "nginx",
                6..=7 => "sshd",
                _ => "zeek",
            };
            let ip = rng.choose(&normal_ips);
            seq += 1;
            let line = match kind {
                "nginx" => {
                    let status = noisy_http_status(rng);
                    format!(
                        "{ip} - - [{tick}] \"{method} {path} HTTP/1.1\" {status} {bytes} \"-\" \"{ua}\"",
                        ip = ip,
                        tick = expanded % MOD,
                        method = rng.choose(&methods),
                        path = rng.choose(&paths),
                        status = status,
                        bytes = rng.next_range(120, 4_000),
                        ua = rng.choose(&uas),
                    )
                }
                "sshd" => {
                    let event = rng.choose(&["pam", "close", "other"]);
                    sshd_line(expanded, ip, event)
                }
                _ => zeek_line(expanded, ip, rng.choose(&zeek_dests), rng.choose(&uas), rng),
            };
            planned.push(Planned {
                expanded,
                tick: 0,
                kind,
                line,
                fingerprint_input: format!("noise-{seq}"),
            });
        }
    }
}

fn noisy_http_status(rng: &mut Rng) -> u16 {
    // Mostly 2xx, occasional 3xx/4xx but never enough to cross rule thresholds.
    match rng.next_below(100) {
        0..=84 => *rng.choose(&[200u16, 200, 201, 204]),
        85..=93 => *rng.choose(&[301u16, 302]),
        _ => *rng.choose(&[404u16, 403, 400]),
    }
}

fn sshd_line(expanded: u64, ip: &str, event: &str) -> String {
    let tick = expanded % MOD;
    match event {
        "fail" => format!(
            "{tick} host sshd[{pid}]: Failed password for user ops from {ip} port 51000 ssh2",
            tick = tick,
            pid = 1000 + (tick % 900),
            ip = ip
        ),
        "success" => format!(
            "{tick} host sshd[{pid}]: Accepted password for user ops from {ip} port 51001 ssh2",
            tick = tick,
            pid = 1000 + (tick % 900),
            ip = ip
        ),
        "close" => format!(
            "{tick} host sshd[{pid}]: session closed for user ops from {ip}",
            tick = tick,
            pid = 1000 + (tick % 900),
            ip = ip
        ),
        _ => format!(
            "{tick} host sshd[{pid}]: pam_unix session opened for user ops from {ip}",
            tick = tick,
            pid = 1000 + (tick % 900),
            ip = ip
        ),
    }
}

fn zeek_line(expanded: u64, src: &str, dest: &str, ua: &str, rng: &mut Rng) -> String {
    let tick = expanded % MOD;
    let uid = format!("C{:x}", tick.wrapping_add(rng.next_u64() % 0xfffff));
    let status = *rng.choose(&[200u16, 200, 200, 301, 404]);
    format!(
        "{tick}\t{uid}\t{src}\t{sport}\t{dest}\t443\ttcp\t{status}\t{bytes}\t{ua}",
        tick = tick,
        uid = uid,
        src = src,
        sport = 40000 + (tick % 20000),
        dest = dest,
        status = status,
        bytes = rng.next_range(80, 9_000),
        ua = ua
    )
}

fn ip_pool(prefix: &str, count: usize) -> Vec<String> {
    (1..=count).map(|i| format!("{prefix}.0.{}", i)).collect()
}

/// Fourteen labeled attack scenarios. Thirteen are detectable; one stealthy
/// slow-and-low scenario stays under every threshold (the false negative).
fn build_scenarios(
    rng: &mut Rng,
    planned: &mut Vec<Planned>,
    labels: &mut Vec<TruthLabel>,
    start: u64,
) {
    let attack_ips = ip_pool("203.0.113", ATTACK_INCIDENTS as usize);
    let mut cursor = start;

    // 1..=5: pure SSH brute force (r02). Each IP emits exactly 5 failures inside
    // the 60s window then a SUCCESS followed shortly by a LOGOUT. The logout keeps
    // r01 (absent) from firing; the success is pushed beyond r03's 60s window so
    // only the count rule r02 fires for these groups.
    for idx in 0..5u64 {
        let ip = &attack_ips[idx as usize];
        let t0 = cursor;
        let mut t = t0;
        for _ in 0..5 {
            push(planned, t, "sshd", sshd_line(t, ip, "fail"));
            t += 5;
        }
        // success 90s after the first failure -> outside r03's 60s sequence window
        let success_t = t0 + 90;
        push(
            planned,
            success_t,
            "sshd",
            sshd_line(success_t, ip, "success"),
        );
        let logout_t = success_t + 30;
        push(planned, logout_t, "sshd", sshd_line(logout_t, ip, "close"));
        labels.push(truth("r02", ip, t0, success_t + 1, true));
        cursor = success_t + 240;
    }

    // 6..=9: HTTP 4xx burst (r04). Exactly 5 status-403 events in 60s, with no
    // SSH login from these IPs, so r06 (http -> login_success) never fires.
    for idx in 5..9u64 {
        let ip = &attack_ips[idx as usize];
        let t0 = cursor;
        let mut t = t0;
        for _ in 0..5 {
            push_http(planned, t, ip, 403, "Mozilla/5.0-X11");
            t += 8;
        }
        labels.push(truth("r04", ip, t0, t + 1, true));
        cursor = t + 240;
    }

    // 10..=12: rapid Zeek connection scan (r05). 12 conns within 30s.
    for idx in 9..12u64 {
        let ip = &attack_ips[idx as usize];
        let t0 = cursor;
        for j in 0..12 {
            let t = t0 + j;
            push(
                planned,
                t,
                "zeek",
                zeek_line(t, ip, "172.16.0.20", "scanner/1.0", rng),
            );
        }
        labels.push(truth("r05", ip, t0, t0 + 30, true));
        cursor = t0 + 240;
    }

    // 13: successful login with no logout (r01 absent). Single success only.
    {
        let ip = &attack_ips[12];
        let t0 = cursor;
        push(planned, t0, "sshd", sshd_line(t0, ip, "success"));
        labels.push(truth("r01", ip, t0, t0 + 120, true));
        cursor = t0 + 240;
    }

    // 14: stealthy slow-and-low intrusion (the false negative). Failures are
    // spaced far beyond every rule window and the eventual success is remote
    // from the last failure, so no rule fires even though it is labeled an attack.
    {
        let ip = &attack_ips[13];
        let t0 = cursor;
        for j in 0..3 {
            let t = t0 + j * 400;
            push(planned, t, "sshd", sshd_line(t, ip, "fail"));
        }
        let ts = t0 + 1_300;
        push(planned, ts, "sshd", sshd_line(ts, ip, "success"));
        push(planned, ts + 40, "sshd", sshd_line(ts + 40, ip, "close"));
        labels.push(truth("r02", ip, t0, ts + 1, true));
    }
}

fn truth(rule_id: &str, ip: &str, start: u64, end: u64, is_attack: bool) -> TruthLabel {
    TruthLabel {
        label_id: format!("{}-{}", rule_id, ip),
        rule_id: rule_id.to_string(),
        src_ip: ip.to_string(),
        start_sec: start,
        end_sec: end,
        is_attack,
    }
}

fn push(planned: &mut Vec<Planned>, expanded: u64, kind: &'static str, line: String) {
    let fp = format!("{kind}:{expanded}:{}", line.len());
    planned.push(Planned {
        expanded,
        tick: 0,
        kind,
        line,
        fingerprint_input: fp,
    });
}

fn push_http(planned: &mut Vec<Planned>, expanded: u64, ip: &str, status: u16, ua: &str) {
    let line = format!(
        "{ip} - - [{tick}] \"GET /admin HTTP/1.1\" {status} 311 \"-\" \"{ua}\"",
        ip = ip,
        tick = expanded % MOD,
        status = status,
        ua = ua
    );
    push(planned, expanded, "nginx", line);
}

/// MAD burst entities. Each burst IP gets a quiet multi-window baseline and a
/// single high-volume window, producing exactly one anomalous session each.
fn build_bursts(rng: &mut Rng, planned: &mut Vec<Planned>) {
    let burst_ips = ip_pool("198.51.100", BURST_ENTITIES);
    let uas = ["Mozilla/5.0-Burst", "curl/8.4.0"];

    for (i, ip) in burst_ips.iter().enumerate() {
        // Place each entity in its own window region inside segment 2.
        let home_window = 10 + (i as u64 % (SEGMENT_WINDOWS - 60));
        let baseline_window = home_window + 12;
        let ua = uas[i % uas.len()];

        // Quiet baseline traffic spread over six distinct windows.
        for k in 0..6u64 {
            let w = baseline_window + k;
            for _ in 0..2 {
                let local_window = w % SEGMENT_WINDOWS;
                let expanded = seg_time(2, local_window * WINDOW_LEN + rng.next_below(WINDOW_LEN));
                push_http(planned, expanded, ip, 200, ua);
            }
        }
        // One dense burst window (roughly an order of magnitude over baseline).
        let burst_base = seg_time(2, (home_window % SEGMENT_WINDOWS) * WINDOW_LEN);
        for j in 0..18 {
            let expanded = burst_base + j;
            push_http(planned, expanded, ip, 200, ua);
        }
    }
}

/// A monitoring probe causes a benign 4xx burst that crosses one rule threshold
/// (the single false-positive alert) but is labeled non-attack. The three
/// firing windows become the three FP evaluation cells used for the FPR.
fn build_probe(planned: &mut Vec<Planned>, labels: &mut Vec<TruthLabel>) {
    let ip = "192.0.2.254";
    let ua = "HealthProbe/2.0";
    let t0 = seg_time(2, 40 * WINDOW_LEN);
    // Three clusters spaced one window apart, each over the r04 4xx threshold.
    for cluster in 0..3u64 {
        let base = t0 + cluster * WINDOW_LEN;
        for j in 0..5 {
            push_http(planned, base + j * 4, ip, 404, ua);
        }
        labels.push(TruthLabel {
            label_id: format!("r04-probe-{}", cluster),
            rule_id: "r04".to_string(),
            src_ip: ip.to_string(),
            start_sec: base,
            end_sec: base + WINDOW_LEN,
            is_attack: false,
        });
    }
}

/// Replace the device tick embedded in a rendered log line with the assigned
/// monotonic tick. Each format carries the tick at a fixed position.
fn replace_tick(line: &str, kind: &str, _old: &str, new: &str) -> String {
    match kind {
        "nginx" => {
            // Replace whatever sits inside the first [...] bracket pair.
            if let Some(open) = line.find('[') {
                if let Some(close) = line[open..].find(']') {
                    let close_abs = open + close;
                    return format!("{}[{}]{}", &line[..open], new, &line[close_abs + 1..]);
                }
            }
            line.to_string()
        }
        "sshd" | "zeek" => {
            // Replace the leading whitespace-delimited token.
            let sep = if kind == "zeek" { '\t' } else { ' ' };
            if let Some(pos) = line.find(sep) {
                format!("{new}{}", &line[pos..])
            } else {
                line.to_string()
            }
        }
        _ => line.to_string(),
    }
}

/// Pick event indices that become late arrivals. All late events map to exactly
/// `restated_windows` distinct early windows, which are guaranteed to be closed
/// by the time the watermark advances to the end of the stream.
fn choose_late_positions(
    rng: &mut Rng,
    planned: &[Planned],
    late_events: u64,
    restated_windows: u64,
) -> Vec<usize> {
    // Map each event index to its tumbling window.
    let mut by_window: BTreeMap<u64, Vec<usize>> = BTreeMap::new();
    for (idx, event) in planned.iter().enumerate() {
        let window = event.expanded / WINDOW_LEN;
        by_window.entry(window).or_default().push(idx);
    }
    // Late windows must sit in the final segment but before the last few bins.
    // They are delivered at the tail of the arrival stream, yet their raw ticks
    // only fall a modest distance behind the watermark (well under the wrap
    // threshold), so the clock classifies them as late drift rather than wraps.
    let candidate: Vec<u64> = by_window.keys().copied().collect();
    let final_segment_start = (WINDOWS / 3) * 2;
    let recent: Vec<u64> = candidate
        .iter()
        .copied()
        .filter(|w| *w >= final_segment_start + 40 && *w + 80 < WINDOWS)
        .collect();
    let stride = (recent.len() as u64 / restated_windows).max(1);
    let mut target_windows: Vec<u64> = Vec::new();
    let mut w = 0usize;
    while target_windows.len() < restated_windows as usize && w < recent.len() {
        target_windows.push(recent[w]);
        w += stride as usize;
    }

    let per = late_events / restated_windows;
    let remainder = late_events - per * restated_windows;
    let mut chosen = Vec::new();
    for (i, window) in target_windows.into_iter().enumerate() {
        let need = (per + if (i as u64) < remainder { 1 } else { 0 }) as usize;
        let indices = by_window.get(&window).cloned().unwrap_or_default();
        // Deterministically spread selection inside the window.
        let mut picked = 0usize;
        let mut probe = rng.next_below(indices.len() as u64) as usize;
        while picked < need && picked < indices.len() {
            let idx = indices[probe % indices.len()];
            if !chosen.contains(&idx) {
                chosen.push(idx);
                picked += 1;
            }
            probe += 1;
        }
        assert_eq!(picked, need, "not enough events in late window {}", window);
    }
    assert_eq!(chosen.len() as u64, late_events);
    chosen
}

/// Render the three log files (in expanded-time order per file) plus the
/// malformed lines that must be quarantined.
fn write_logs(dir: &Path, planned: &[Planned]) -> Result<Vec<GeneratedFile>> {
    let names = [
        ("nginx", "nginx.log"),
        ("sshd", "sshd.log"),
        ("zeek", "zeek.log"),
    ];
    let mut files = Vec::new();
    for (kind, file_name) in names {
        let path = dir.join(file_name);
        let mut out = String::new();
        for event in planned.iter().filter(|e| e.kind == kind) {
            out.push_str(&event.line);
            out.push('\n');
        }
        if kind == "zeek" {
            append_bad_lines(&mut out, QUARANTINED);
        }
        std::fs::write(&path, out).map_err(|source| crate::error::Error::Io {
            path: path.clone(),
            source,
        })?;
        files.push(GeneratedFile { path, kind });
    }
    Ok(files)
}

fn append_bad_lines(out: &mut String, count: u64) {
    for i in 0..count {
        // Every variant is structurally invalid for all three parsers:
        //  * unknown free text has no marker/IP/tab,
        //  * tab rows omit the mandatory tick and src_ip fields,
        //  * nginx-looking rows carry a non-numeric tick,
        //  * sshd-looking text lacks the `sshd[` marker and tick.
        let bad = match i % 4 {
            0 => format!("unrecognised audit blob seq {} without any marker", i),
            1 => format!("\t\t#broken-zeek-{}", i),
            2 => format!(
                "999.999.999.999 - - [badtick{}] \"GET / HTTP/1.1\" 200 10 \"-\" \"x\"",
                i
            ),
            _ => format!("garbled entry seq {} sshd marker missing tick", i),
        };
        out.push_str(&bad);
        out.push('\n');
    }
}

/// Write the arrival-order manifest. Late events are pushed toward the tail so
/// they arrive after their owning window has closed.
fn write_manifest(
    dir: &Path,
    planned: &mut [Planned],
    late_set: &std::collections::HashSet<usize>,
) -> Result<PathBuf> {
    // On-time rows arrive in the exact global (event-time, hence tick) order.
    // Natural global order index for every event (matches tick assignment).
    let natural: Vec<usize> = {
        let mut ids: Vec<usize> = (0..planned.len()).collect();
        ids.sort_by(|a, b| {
            planned[*a]
                .expanded
                .cmp(&planned[*b].expanded)
                .then(planned[*a].kind.cmp(planned[*b].kind))
                .then(
                    planned[*a]
                        .fingerprint_input
                        .cmp(&planned[*b].fingerprint_input),
                )
        });
        ids
    };
    let mut natural_position: BTreeMap<usize, usize> = BTreeMap::new();
    for (pos, id) in natural.iter().enumerate() {
        natural_position.insert(*id, pos);
    }

    // Each event gets an arrival key. On-time events keep their natural
    // position; late events are shifted forward by a fixed delay large enough to
    // cross window close + lateness allowance.
    let delay_rows: usize = 2_000;
    let mut scheduled: Vec<(usize, usize, usize)> = natural
        .iter()
        .map(|id| {
            let base = natural_position[id];
            let arrival = if late_set.contains(id) {
                base + delay_rows
            } else {
                base
            };
            (arrival, base, *id)
        })
        .collect();
    scheduled.sort_by(|a, b| a.0.cmp(&b.0).then(a.1.cmp(&b.1)));
    let order: Vec<usize> = scheduled.iter().map(|(_, _, id)| *id).collect();

    let manifest = dir.join("manifest.tsv");
    let mut out = String::from("seq\tkind\tline_no\n");
    // Map each event to its 1-based line within its kind-specific file.
    let mut line_counters: BTreeMap<&str, usize> = BTreeMap::new();
    let mut line_of: BTreeMap<usize, usize> = BTreeMap::new();
    for (idx, event) in planned.iter().enumerate() {
        let next = line_counters.entry(event.kind).or_insert(0);
        *next += 1;
        line_of.insert(idx, *next);
    }
    let mut seq = 0usize;
    for idx in order.iter() {
        let event = &planned[*idx];
        out.push_str(&format!(
            "{}\t{}\t{}\n",
            seq,
            event.kind,
            line_of.get(idx).copied().unwrap_or(0)
        ));
        seq += 1;
    }
    // Trailing malformed zeek lines are part of the physical file and must be
    // read by the parser so they land in the quarantine table.
    let zeek_events = planned.iter().filter(|e| e.kind == "zeek").count();
    for offset in 1..=QUARANTINED as usize {
        out.push_str(&format!("{}\tzeek\t{}\n", seq, zeek_events + offset));
        seq += 1;
    }
    std::fs::write(&manifest, out).map_err(|source| crate::error::Error::Io {
        path: manifest.clone(),
        source,
    })?;
    Ok(manifest)
}
