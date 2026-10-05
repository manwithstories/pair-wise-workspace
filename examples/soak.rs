//! Soak test: drive a long transfer across a range of loss rates and report
//! convergence and steady-state throughput.
//!
//! ```text
//! cargo run --release --example soak -- 0.05
//! cargo run --release --example soak            # sweep 1% / 5% / 15%
//! cargo run --release --example soak -- 0.05 --explain
//! ```
//!
//! The first argument is the loss rate to focus on; without one the three rates
//! from the brief are swept. Everything is simulated against virtual time, so a
//! run that would take minutes of wall clock on a 200 ms link completes in
//! milliseconds, and the figures describe the transport rather than the host's
//! packet scheduler.
//!
//! The report distinguishes two things that are easy to conflate:
//!
//! * **convergence** — how long the window takes to reach a stable size, and
//!   the number of round trips it took. This is what an operator waits through
//!   after a path changes.
//! * **steady state** — the throughput once the window has stopped adapting.
//!   This is the number that decides whether a link is usable.

use std::fmt::Write as _;

use rudpx::conn::ConnConfig;
use rudpx::harness::Harness;
use rudpx::loss_injector::DamageProfile;
use rudpx::wire::SEGMENT_PAYLOAD;

/// Segments moved per run. Large enough for slow start to amortise and the
/// steady state to be visible, small enough to stay instant.
const SEGMENTS: usize = 4000;

/// Segments in the capacity baseline.
///
/// Longer than the measured runs, and deliberately so. A short transfer spends
/// most of its time in the slow-start ramp, so its throughput measures how fast
/// the window opens rather than what the path can carry. The baseline has to
/// come from a run long enough that the window, not the ramp, is the limit —
/// otherwise every ratio in the report is a ratio of two ramps and says nothing
/// about the link.
///
/// The baseline row itself is *not* printed: it is a different length from the
/// rows it is used to scale, and putting it in the same table would invite
/// exactly the comparison the number is meant to prevent.
const BASELINE_SEGMENTS: usize = 40_000;

/// One-way delay: a 40 ms round trip, a plausible edge-to-edge path.
const ONE_WAY_US: u64 = 20_000;

fn profile(loss: f64) -> DamageProfile {
    DamageProfile {
        loss_rate: loss,
        reorder_rate: 0.05,
        duplicate_rate: 0.01,
        reorder_window: 16,
        base_latency_us: ONE_WAY_US,
        jitter_us: 5_000,
    }
}

fn payload(segments: usize) -> Vec<u8> {
    (0..segments * SEGMENT_PAYLOAD).map(|i| (i % 251) as u8).collect()
}

/// One measured run.
struct Measurement {
    bytes: usize,
    elapsed_us: u64,
    bps: f64,
    /// Round trips until the congestion window last changed.
    convergence_rtts: u64,
    /// Size the window settled at.
    steady_cwnd: u32,
    /// Whether the payload arrived intact and in order.
    intact: bool,
    /// Bytes the run was asked to move.
    expected: usize,
    peak_rtx_queue: usize,
    timeouts: u32,
    fast_retransmits: u32,
    spurious: u64,
    srtt_us: Option<u64>,
    rto_us: u64,
    link_dropped: u64,
    link_delivered: u64,
    realised_loss: f64,
}

fn measure(loss: f64) -> Measurement {
    measure_at(loss, SEGMENTS)
}

fn measure_at(loss: f64, segments: usize) -> Measurement {
    let p = payload(segments);
    let mut h = Harness::new(profile(loss), 42, ConnConfig::default()).expect("harness");
    let start = h.now_us();
    h.a.write(&p);
    let rtt = 2 * ONE_WAY_US;
    let mut last_change = 0u64;
    let mut last_cwnd = h.a.sender().cc().cwnd();

    for _ in 0..4_000_000u64 {
        if !h.step() {
            break;
        }
        let cwnd = h.a.sender().cc().cwnd();
        if cwnd != last_cwnd {
            last_cwnd = cwnd;
            last_change = h.now_us().saturating_sub(start);
        }
        if h.a.is_flushed() && h.is_idle() {
            break;
        }
    }

    let s = h.a.sender();
    let dropped = h.link_dropped();
    let delivered = h.link_delivered();
    let total = dropped + delivered;
    Measurement {
        bytes: h.received.len(),
        elapsed_us: h.now_us().saturating_sub(start),
        bps: throughput_bps(h.received.len(), h.now_us().saturating_sub(start)),
        convergence_rtts: last_change / rtt,
        steady_cwnd: s.cc().cwnd(),
        intact: h.received == p,
        expected: segments * SEGMENT_PAYLOAD,
        peak_rtx_queue: s.peak_queue_len(),
        timeouts: s.loss_events().0,
        fast_retransmits: s.loss_events().1,
        spurious: s.spurious_retransmits(),
        srtt_us: s.rtt().srtt(),
        rto_us: s.rto_us(),
        link_dropped: h.link_dropped(),
        link_delivered: h.link_delivered(),
        realised_loss: if total > 0 { dropped as f64 / total as f64 } else { 0.0 },
    }
}

/// Goodput in bytes per simulated second.
fn throughput_bps(bytes: usize, elapsed_us: u64) -> f64 {
    if elapsed_us == 0 {
        0.0
    } else {
        bytes as f64 * 1_000_000.0 / elapsed_us as f64
    }
}

fn mbps(bps: f64) -> f64 {
    bps * 8.0 / 1_000_000.0
}

fn report(losses: &[f64], explain: bool) {
    let base = measure_at(0.0, BASELINE_SEGMENTS);
    let baseline = base.bps;

    println!("rudpx soak: {} segments ({} KiB) over a {}-ms RTT path",
        SEGMENTS, SEGMENTS * SEGMENT_PAYLOAD / 1024, 2 * ONE_WAY_US / 1000);
    println!("damage: 5% reorder within 16 segments, 1% duplicate, 5 ms jitter\n");

    let mut out = String::new();
    let _ = writeln!(out, "```");
    let _ = writeln!(out, "{:<8} {:>10} {:>12} {:>10} {:>8} {:>9} {:>8} {:>7}",
        "loss", "goodput", "vs clean", "conv(RTT)", "cwnd", "timeouts", "fastrx", "rtx-queue");
    let _ = writeln!(out, "{:-<8} {:->10} {:->12} {:->10} {:->8} {:->9} {:->8} {:->7}",
        "", "", "", "", "", "", "", "");
    // The 0% row is the same 4000-segment run as the others, so the table is
    // internally comparable. The long baseline above exists only to establish
    // the link's capacity for the "vs clean" column, and is not shown here —
    // mixing its row into this table would compare a ramp with a steady state.
    let zero = measure(0.0);
    let zero_pct = if baseline > 0.0 { zero.bps / baseline * 100.0 } else { 0.0 };
    let _ = writeln!(out,
        "{:<8} {:>9.2}M {:>11.0}% {:>10} {:>8} {:>9} {:>8} {:>7}",
        "0%", mbps(zero.bps), zero_pct, "-", zero.steady_cwnd, zero.timeouts,
        zero.fast_retransmits, zero.peak_rtx_queue);
    let _ = writeln!(out, "{:<8} {:>9.2}M {:>11} {:>10} {:>8} {:>9} {:>8} {:>7}",
        "capacity", mbps(baseline), "-", "-", "-", "-", "-", "-");

    for &loss in losses {
        let m = measure(loss);
        let pct = if baseline > 0.0 { m.bps / baseline * 100.0 } else { 0.0 };
        let _ = writeln!(out,
            "{:<8} {:>9.2}M {:>10.0}% {:>10} {:>8} {:>9} {:>8} {:>7}",
            format!("{:.0}%", loss * 100.0), mbps(m.bps), pct,
            m.convergence_rtts, m.steady_cwnd, m.timeouts, m.fast_retransmits,
            m.peak_rtx_queue);
    }
    let _ = writeln!(out, "```");
    print!("{}", out);

    println!("\ncorrectness");
    println!("  (0% baseline for the ratio column: {} segments, {} Mbit/s)",
        BASELINE_SEGMENTS, mbps(baseline));
    for &loss in losses {
        let m = measure(loss);
        println!(
            "  loss {:>3.0}%  bytes {:>9}/{}  intact={}  realised loss {:.2}%",
            loss * 100.0,
            m.bytes,
            m.expected,
            if m.intact { "yes" } else { "NO" },
            m.realised_loss * 100.0
        );
    }

    if explain {
        println!("\ndetail");
        for &loss in losses {
            let m = measure(loss);
            println!(
                "  loss {:>3.0}%  elapsed={}us  srtt={:?}  rto={}us  spurious_retx={}  dropped={}  delivered={}",
                loss * 100.0, m.elapsed_us, m.srtt_us, m.rto_us, m.spurious, m.link_dropped, m.link_delivered
            );
        }
    }
}

fn parse_loss(arg: &str) -> Option<f64> {
    let v: f64 = arg.parse().ok()?;
    if (0.0..1.0).contains(&v) {
        Some(v)
    } else {
        eprintln!("loss rate must be in [0, 1), got {arg}");
        None
    }
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let explain = args.iter().any(|a| a == "--explain");

    // The first non-flag argument is a loss rate; with none, sweep the three
    // rates from the brief.
    let focus = args.iter().find(|a| !a.starts_with("--")).and_then(|a| parse_loss(a));
    let losses: Vec<f64> = match focus {
        Some(v) => vec![v],
        None => vec![0.01, 0.05, 0.15],
    };
    report(&losses, explain);
}
