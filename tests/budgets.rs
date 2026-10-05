//! The numeric budgets from the brief, checked as tests rather than asserted in
//! prose: memory must not track the stream, and a state dump must stay well
//! inside its 2 ms budget on a live, loaded connection.

use std::time::{Duration, Instant};

use rudpx::conn::ConnConfig;
use rudpx::dump::ConnDump;
use rudpx::harness::Harness;
use rudpx::loss_injector::DamageProfile;
use rudpx::wire::SEGMENT_PAYLOAD;

/// Everything the engine is holding right now, across both directions.
fn buffered_bytes(h: &Harness) -> usize {
    let s = h.a.sender();
    let r = h.b.receiver();
    s.pending_bytes()
        + s.queue().map(|x| x.payload.len()).sum::<usize>()
        + r.ready_bytes()
        + r.out_of_order_len() as usize * SEGMENT_PAYLOAD
}

/// A link bad enough that the queues are genuinely working.
fn link() -> DamageProfile {
    DamageProfile {
        loss_rate: 0.05,
        reorder_rate: 0.05,
        duplicate_rate: 0.01,
        reorder_window: 16,
        base_latency_us: 20_000,
        jitter_us: 5_000,
    }
}

/// Move `total` bytes through a connection, feeding a chunk at a time and
/// running until each is drained, and return the harness plus the peak buffer.
fn push_through(h: &mut Harness, total: usize, chunk: &[u8]) -> usize {
    let mut sent = 0usize;
    let mut peak = 0usize;
    while sent < total {
        let n = chunk.len().min(total - sent);
        h.a.write(&chunk[..n]);
        sent += n;
        for _ in 0..2_000_000 {
            if !h.step() {
                break;
            }
            if h.a.sender().pending_bytes() == 0 && h.is_idle() {
                break;
            }
        }
        peak = peak.max(buffered_bytes(h));
    }
    peak
}

#[test]
fn memory_does_not_track_the_stream() {
    // Moving an order of magnitude more than the budget must not raise the
    // buffers with it. A transport that accumulates the send buffer would pass
    // every correctness test and still fall over on a long-lived connection,
    // which is the deployment this is built for.
    let budget = 8 * 1024 * 1024;
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    let chunk = vec![7u8; 64 * 1024];
    // 64 MiB through the connection.
    let peak = push_through(&mut h, 64 * 1024 * 1024, &chunk);

    assert!(
        peak < budget,
        "engine buffered {} MiB, over the {} MiB budget",
        peak / (1024 * 1024),
        budget / (1024 * 1024)
    );
    assert_eq!(h.received.len(), 64 * 1024 * 1024, "every byte should have arrived");
}

#[test]
fn a_very_long_stream_stays_inside_the_memory_budget() {
    // The same check at four times the volume, to catch a slow leak that only
    // becomes visible over a long run.
    let budget = 8 * 1024 * 1024;
    let mut h = Harness::new(link(), 7, ConnConfig::default()).unwrap();
    let chunk = vec![3u8; 64 * 1024];
    let peak = push_through(&mut h, 256 * 1024 * 1024, &chunk);
    assert!(
        peak < budget,
        "engine buffered {} MiB over 256 MiB of payload, over the budget",
        peak / (1024 * 1024)
    );
    assert_eq!(h.received.len(), 256 * 1024 * 1024);
}

#[test]
fn a_state_dump_on_a_loaded_connection_is_well_inside_its_budget() {
    // The dump is taken while the connection is mid-transfer and under load,
    // which is when it is actually needed. The 2 ms budget is a hard number in
    // the brief; this checks the real figure is far inside it, and reports it so
    // a regression is visible in the test output rather than only on failure.
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    let chunk = vec![11u8; 32 * 1024];
    let mut worst = Duration::ZERO;
    let mut n = 0;

    for _ in 0..400 {
        h.a.write(&chunk);
        for _ in 0..100_000 {
            if !h.step() {
                break;
            }
            if h.a.sender().queue_len() > 8 {
                break;
            }
        }
        let t = Instant::now();
        let text = ConnDump::capture(&h.a).with_receive(&h.a).to_string();
        worst = worst.max(t.elapsed());
        n += 1;
        // Formatting must produce the full report, not a stub.
        assert!(text.contains("-- send window --"), "the dump lost a section under load");
    }

    eprintln!("{n} dumps on a loaded connection, slowest {worst:?}");
    assert!(
        worst.as_millis() < 2,
        "slowest dump took {worst:?}, over the 2 ms budget"
    );
}

#[test]
fn taking_a_dump_does_not_disturb_the_connection() {
    // A diagnostic that changes what it is diagnosing is worse than none. The
    // dump must borrow and copy, never mutate the state it reports.
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    h.a.write(&vec![1u8; 40 * SEGMENT_PAYLOAD]);
    for _ in 0..300 {
        h.step();
    }
    let before = format!("{:?}", {
        let s = h.a.sender();
        (s.una(), s.next(), s.queue_len(), s.cc().cwnd(), s.rtt().srtt())
    });
    for _ in 0..50 {
        let _ = ConnDump::capture(&h.a).with_receive(&h.a).to_string();
    }
    let after = format!("{:?}", {
        let s = h.a.sender();
        (s.una(), s.next(), s.queue_len(), s.cc().cwnd(), s.rtt().srtt())
    });
    assert_eq!(before, after, "taking a dump must not change the connection's state");
}
