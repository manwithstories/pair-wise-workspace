//! Print a connection state dump for a live, impaired connection.
//!
//! The dump is the tool this engine exists to support: when throughput
//! collapses on a real link, one snapshot should say whether the send window is
//! stuck, the retransmission queue is backing up, the congestion window has
//! collapsed, or the RTT estimate has run away.
//!
//! ```text
//! cargo run --release --example dump            # 5% loss
//! cargo run --release --example dump -- 0.15    # 15% loss
//! ```

use rudpx::conn::ConnConfig;
use rudpx::dump::dump;
use rudpx::harness::Harness;
use rudpx::loss_injector::DamageProfile;

fn main() {
    let loss: f64 = std::env::args()
        .nth(1)
        .and_then(|a| a.parse().ok())
        .unwrap_or(0.05);

    let profile = DamageProfile {
        loss_rate: loss,
        reorder_rate: 0.05,
        duplicate_rate: 0.01,
        reorder_window: 16,
        base_latency_us: 20_000,
        jitter_us: 5_000,
    };
    let mut h = Harness::new(profile, 42, ConnConfig::default()).expect("harness");
    let payload: Vec<u8> = (0..2000 * 1400).map(|i| (i % 251) as u8).collect();
    h.a.write(&payload);

    // Run well into the transfer, so the dump shows a connection that has
    // adapted and has something interesting to report. Sampling in the first
    // window only ever shows "still in slow start", which is true of every
    // connection ever built and tells an operator nothing.
    for _ in 0..2_000_000 {
        if !h.step() {
            break;
        }
        let s = h.a.sender();
        if s.loss_events().0 + s.loss_events().1 >= 3 && s.queue_len() > 2 {
            break;
        }
        if h.a.is_flushed() && h.is_idle() {
            break;
        }
    }

    println!("# dump at {:.1}% loss, mid-transfer", loss * 100.0);
    println!("{}", dump(&h.a));
}
