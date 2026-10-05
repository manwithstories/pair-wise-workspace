//! Connection lifecycle: the FIN handshake, half-close semantics, timeouts,
//! malformed input, and the runtime state dump.
//!
//! The FIN rules are the fiddly part of a reliable transport and the easiest
//! to get subtly wrong: a FIN that overtakes in-flight data truncates the
//! stream, and a half-closed connection that stops reading silently discards
//! the peer's remaining bytes. Both failures are invisible until data is lost,
//! so they are tested directly rather than inferred from a transfer completing.

use std::time::Duration;

use rudpx::conn::{ConnConfig, State};
use rudpx::dump::{dump, ConnDump};
use rudpx::harness::Harness;
use rudpx::loss_injector::DamageProfile;
use rudpx::wire::SEGMENT_PAYLOAD;

/// A link with a plausible amount of damage on it.
fn link() -> DamageProfile {
    DamageProfile {
        loss_rate: 0.02,
        reorder_rate: 0.05,
        duplicate_rate: 0.0,
        reorder_window: 8,
        base_latency_us: 10_000,
        jitter_us: 2_000,
    }
}

fn payload(n: usize) -> Vec<u8> {
    (0..n).map(|i| (i % 251) as u8).collect()
}

#[test]
fn a_connection_starts_established() {
    let h = Harness::new(link(), 1, ConnConfig::default()).unwrap();
    assert_eq!(h.a.state(), State::Established);
    assert_eq!(h.b.state(), State::Established);
}

#[test]
fn write_and_recv_deliver_bytes_in_order() {
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    let p = payload(3 * SEGMENT_PAYLOAD);
    assert_eq!(h.a.write(&p), p.len(), "write must accept everything it is given");
    for _ in 0..10_000 {
        if !h.step() || h.received.len() == p.len() {
            break;
        }
    }
    assert_eq!(h.received, p);
}

#[test]
fn is_flushed_is_false_until_everything_is_acknowledged() {
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    h.a.write(&payload(50 * SEGMENT_PAYLOAD));
    assert!(!h.a.is_flushed(), "freshly written data is not flushed");
    for _ in 0..50_000 {
        if !h.step() || h.a.is_flushed() {
            break;
        }
    }
    assert!(h.a.is_flushed(), "everything written should eventually be acknowledged");
}

#[test]
fn shutdown_queues_a_fin_behind_the_buffered_data() {
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    h.a.write(&payload(3 * SEGMENT_PAYLOAD));
    assert!(h.a.shutdown(), "shutdown must accept a close on an open connection");
    assert!(h.a.sender().fin_allocated());
    assert!(h.a.sender().fin_pending(), "the FIN must be outstanding until acknowledged");
    assert_eq!(h.a.state(), State::FinWait);
    // The FIN sits behind whatever was already buffered, so the peer sees every
    // byte before the end of stream. A FIN that overtook here would truncate the
    // stream at three segments and the failure would look like mysterious loss.
    let fin_seq = h.a.sender().queue().find(|s| s.fin).map(|s| s.seq).unwrap();
    assert!(fin_seq >= 3, "the FIN must not overtake the three buffered segments");
}

#[test]
fn a_half_closed_connection_still_receives() {
    // The defining property of a half-close: sending is over, receiving is not.
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    h.a.shutdown();
    assert!(h.a.state().is_half_closed());
    assert!(!h.a.state().can_send(), "a half-closed connection must refuse new writes");
    assert!(h.a.state().can_recv(), "a half-closed connection must still receive");
    assert_eq!(h.a.write(b"more"), 0, "writes after shutdown must be refused");

    // The peer keeps sending, and the half-closed side must still take it.
    let p = payload(2 * SEGMENT_PAYLOAD);
    h.b.write(&p);
    for _ in 0..20_000 {
        if !h.step() || h.received_at_a.len() >= p.len() {
            break;
        }
    }
    assert_eq!(h.received_at_a, p, "a half-closed connection must still deliver inbound data");
}

#[test]
fn shutdown_is_idempotent() {
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    assert!(h.a.shutdown());
    assert!(!h.a.shutdown(), "a second shutdown must be refused");
    assert!(!h.a.shutdown());
}

#[test]
fn the_fin_is_acknowledged_by_the_peer() {
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    h.a.write(&payload(2 * SEGMENT_PAYLOAD));
    h.a.shutdown();
    for _ in 0..50_000 {
        if !h.step() || h.a.sender().fin_acked() {
            break;
        }
    }
    assert!(h.a.sender().fin_acked(), "the peer must acknowledge the FIN");
    assert!(!h.a.sender().fin_pending());
}

#[test]
fn the_peer_observes_the_fin() {
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    h.a.shutdown();
    for _ in 0..20_000 {
        if !h.step() || h.b.receiver().fin_seen() {
            break;
        }
    }
    assert!(h.b.receiver().fin_seen(), "the peer must see the FIN");
    assert!(h.b.receiver().fin_ready(), "with no data outstanding, the FIN completes the stream");
}

#[test]
fn a_fin_survives_a_lossy_link() {
    // The FIN is a segment like any other and must be retransmitted until it is
    // acknowledged, or a close on a lossy path is simply lost.
    for seed in 1..=5u64 {
        let mut h = Harness::new(link(), seed * 31, ConnConfig::default()).unwrap();
        h.a.write(&payload(2 * SEGMENT_PAYLOAD));
        h.a.shutdown();
        for _ in 0..200_000 {
            if !h.step() || h.a.sender().fin_acked() {
                break;
            }
        }
        assert!(
            h.a.sender().fin_acked(),
            "seed {seed}: the FIN was never acknowledged on a lossy link"
        );
    }
}

#[test]
fn the_close_handshake_times_out_rather_than_hanging() {
    // A peer that never answers must not hold the process open forever.
    let cfg = ConnConfig {
        fin_timeout: Duration::from_millis(50),
        ..ConnConfig::default()
    };
    let mut h = Harness::new(DamageProfile::clean(), 1, cfg).unwrap();
    h.a.write(&payload(SEGMENT_PAYLOAD));
    h.a.shutdown();
    // Drive A alone, so the FIN is never acknowledged.
    for _ in 0..10_000 {
        let _ = h.a.pump();
        h.advance(5_000);
        if h.a.is_closed() {
            break;
        }
    }
    assert!(h.a.is_closed(), "an unacknowledged FIN must force the connection down");
}

#[test]
fn abort_closes_immediately() {
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    h.a.write(&payload(SEGMENT_PAYLOAD));
    h.a.abort();
    assert!(h.a.is_closed());
    assert!(!h.a.state().can_send());
}

#[test]
fn a_malformed_datagram_is_dropped_without_killing_the_connection() {
    // On a public link, garbage is expected traffic rather than an exceptional
    // event. One bad packet must not reset a working connection.
    let mut h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    h.a.write(&payload(SEGMENT_PAYLOAD));
    for _ in 0..200 {
        h.step();
    }
    let before = h.b.stats().datagrams_received;
    h.b.feed_inbound(b"this is not a valid rudpx datagram at all");
    h.b.feed_inbound(&[0u8; 8]);
    h.b.feed_inbound(&[0xFFu8; 64]);
    assert!(h.b.stats().datagrams_malformed > 0, "garbage must be counted as malformed");
    assert!(!h.b.is_closed(), "malformed input must not close the connection");
    assert!(h.b.stats().datagrams_received > before);
    // And the connection still works afterwards.
    h.b.write(&payload(SEGMENT_PAYLOAD));
    for _ in 0..5_000 {
        if !h.step() {
            break;
        }
    }
    assert!(!h.b.is_closed());
}

#[test]
fn the_state_dump_covers_the_four_required_sections() {
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    h.a.write(&payload(20 * SEGMENT_PAYLOAD));
    for _ in 0..200 {
        h.step();
    }
    let text = dump(&h.a);
    for section in [
        "-- send window --",
        "-- congestion window --",
        "-- retransmission queue --",
        "-- RTT estimate --",
    ] {
        assert!(text.contains(section), "the dump is missing the {section:?} section");
    }
}

#[test]
fn the_state_dump_reflects_a_live_connection() {
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    h.a.write(&payload(20 * SEGMENT_PAYLOAD));
    // Run until the first window is actually in flight, so the dump shows a
    // connection doing work rather than one that has not started.
    for _ in 0..200 {
        h.step();
        if h.a.sender().next() > h.a.sender().una() {
            break;
        }
    }
    let d = ConnDump::capture(&h.a).with_receive(&h.a);
    assert_eq!(d.state, "ESTABLISHED");
    assert!(d.next > d.una, "the window should have opened after sending");
    assert!(d.cwnd > 0);
    assert!(d.rtx_queue_peak > 0, "sending should have populated the queue at some point");
    assert_eq!(d.window(), format!("[{},{})", d.una, d.next));
}

#[test]
fn the_state_dump_answers_the_obvious_questions() {
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    h.a.write(&payload(20 * SEGMENT_PAYLOAD));
    for _ in 0..100 {
        h.step();
    }
    let text = dump(&h.a);
    // A reader in an incident needs these on screen, not merely computable.
    for field in ["una=", "next=", "cwnd=", "ssthresh=", "phase=", "len=", "head_seq=", "srtt=", "rttvar="] {
        assert!(text.contains(field), "the dump is missing {field:?}");
    }
    assert!(text.contains(">> "), "the dump must end with a diagnosis line");
}

#[test]
fn the_state_dump_reports_an_untouched_connection_without_panicking() {
    let h = Harness::new(DamageProfile::clean(), 1, ConnConfig::default()).unwrap();
    let text = dump(&h.a);
    assert!(text.contains("len=0"));
    assert!(text.contains("none (no sample yet)"), "a connection with no samples must say so");
}

#[test]
fn poll_timeout_reports_the_next_deadline() {
    let mut h = Harness::new(link(), 42, ConnConfig::default()).unwrap();
    h.a.write(&payload(10 * SEGMENT_PAYLOAD));
    // Stop as soon as data is genuinely outstanding, since a connection with an
    // empty queue has nothing to time out on.
    for _ in 0..200 {
        h.step();
        if h.a.sender().queue_len() > 0 {
            break;
        }
    }
    assert!(h.a.sender().queue_len() > 0, "the write should have put segments in flight");
    assert!(h.a.poll_timeout().is_some(), "a connection with outstanding data must have a deadline");
}
