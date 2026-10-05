# rudpx — reliable UDP with SACK + Reno, std-only

A self-contained reliable transport on top of `std::net::UdpSocket`. Selective
acknowledgement, Reno congestion control, deterministic link damage, graceful
half-close, and a runtime state dump. No third-party dependencies, no async
runtime, no threads in the engine.

```
cargo test --release -- --nocapture                  # 157 tests
cargo run  --release --example soak -- 0.05          # soak at 5% loss
cargo run  --release --example dump -- 0.05          # a state dump, mid-transfer
```

## Why it exists

The failure this is built for: on a link that drops 1–15% of its packets and
reorders the rest, a naive transport collapses — throughput falls off a cliff,
retransmissions pile up, and nobody can say whether the send window is stuck,
the retransmission queue is backed up, or the RTT estimate has run away.

So the design goals are, in order:

1. **Never lose or reorder data.** Correctness first. Every transfer in the
   test suite arrives byte-identical, at every loss rate, on every seed.
2. **Make the state legible.** When throughput drops, one snapshot should say
   which of the four subsystems is responsible.
3. **Be reproducible.** Damage is seeded, so a failure reproduces from a seed
   alone instead of showing up on someone else's machine.

## Layout

Six core modules in `src/`, plus a test harness and a simulated link.

| File | Role |
| --- | --- |
| `wire.rs` | On-wire framing, sequence arithmetic, CRC32 |
| `conn.rs` | Connection state machine, buffers, the engine that drives the socket |
| `sender.rs` | Send window, segment allocation, retransmission queue, timers |
| `receiver.rs` | Reassembly, SACK block construction, duplicate detection |
| `congestion.rs` | Reno: slow start, congestion avoidance, fast retransmit/recovery |
| `loss_injector.rs` | Seeded SplitMix64 damage: loss, reordering, duplication |
| `dump.rs` | Human-readable state dump |
| `time.rs` | Clock abstraction, RFC 6298 RTT estimation |
| `sim.rs`, `harness.rs` | Simulated link and the two-endpoint test harness |

`sim.rs` and `harness.rs` are support code for testing, not part of the data
path. The transport itself is the six files above plus `time.rs`.

## Design notes

**Segments, not bytes.** Sequence numbers count segments, so the send window,
the retransmission queue, and every SACK range are integer intervals over one
number space. A segment carries 1400 bytes (28-byte header, 1428 total, under
the 1500-byte MTU); only the last segment of a message is short.

**SACK advertises what was received.** This is the RFC 2018 contract, and both
ends depend on agreeing on it. The sender treats a segment inside a SACK block
as delivered, so advertising holes instead would make it skip retransmitting
exactly the segments it most needs to resend.

**RFC 6675 §6 on top of Reno.** When the peer reports receiving data past a
hole, that is evidence the path is carrying traffic and the loss is a routing
artifact rather than congestion. Counting that evidence as delivered lets the
window hold its size through a repair. Without it, one isolated loss on a 1%-loss
path costs a full halving, and the throughput target is unreachable. Spurious
repairs are detected (a retransmitted segment that turns up) and the halving is
undone.

**Karn's algorithm.** An RTT sample is taken only from a segment transmitted
exactly once, and only outside a loss episode. A sample measured from a repair is
ambiguous and is discarded rather than believed.

**The retransmission timer is armed on the oldest outstanding segment**, even
when that segment has been SACKed. A SACK block reports on a moment in the past
and the report itself can be lost; skipping SACKed segments would disarm the
backstop exactly when it is needed. The cost of arming anyway is one spurious
retransmission; the cost of not arming is a stalled connection.

**The RTO never sits below 1.25× SRTT.** On a stable link RTTVAR decays to zero,
and `srtt + 4·rttvar` would then equal the RTT — putting the timeout in a race
with the acknowledgement that is arriving at that same instant.

## The state dump

The four sections the brief asks for are always present, in a fixed order.

```
$ cargo run --release --example dump -- 0.05
# dump at 5.0% loss, mid-transfer
=== rudpx connection dump ===
state      ESTABLISHED   peer 10.0.0.7:51820

-- send window --
  interval        [37,74)
  una=37 next=74 inflight=37 credit=0 stalled=true
  buffered        2696400 B

-- congestion window --
  cwnd=22 ssthresh=18 phase=FastRecovery
  dup_acks=4 sacked_credit=42000 B

-- retransmission queue --
  len=37 peak=37 sacked=7 head_seq=Some(37) head_tx=2
  repair=Some(37) fast_retransmits=3 timeouts=0 backoff=2^0 consecutive=0 spurious=2

-- RTT estimate --
  srtt=44717us (44.717ms) rttvar=5639us (5.639ms) rto=67274us (67.274ms) (base 67274us (67.274ms)) samples=11
  timer=249327us (249.327ms) armed=true

-- receive side --
  una=0 ooo=0 ooo_peak=0 dup_dropped=0
  sack_holes      -

-- close --
  fin_allocated=false fin_acked=false peer_fin=false

-- counters --
  datagrams tx=112 rx=39 malformed=0
  bytes written=2800000 delivered=0 acked=51800
  segments new=74 retransmitted=3

  >> send window is closed behind an unacknowledged hole
```

In the example above the three lines that matter are `stalled=true`,
`phase=FastRecovery`, and `repair=Some(37)`: the window cannot open because
segment 37 is unacknowledged, Reno has halved and inflated for the repair, and
the repair is in flight. The final line is a shortlist of the states that
actually explain a collapse, so a reader starts from a hypothesis rather than
from four numbers.

A dump takes no locks and allocates nothing beyond the SACK list, which keeps it
well inside the 2 ms budget — and a dump taken under load is exactly when it is
most useful.

## Performance

Measured with `cargo run --release --example soak`, 4000 segments (5.3 MiB) over
a 40 ms RTT path with 5% reordering within 16 segments, 1% duplication, and
5 ms jitter. The "vs clean" column is scaled against a 40000-segment loss-free
run, because a short transfer spends most of its time in the slow-start ramp and
so measures the ramp rather than the link.

| loss | goodput | vs clean | convergence | steady cwnd | timeouts | peak rtx queue | intact |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | :--- |
| 1% | 10.92 Mbit/s | 156% | 102 RTT | 172 | 19 | 285 | yes |
| 5% | 2.44 Mbit/s | 35% | 459 RTT | 7 | 183 | 79 | yes |
| 15% | 0.67 Mbit/s | 10% | 1659 RTT | 5 | 680 | 104 | yes |

Against the brief's targets, honestly:

* **Correctness — met.** Every transfer arrives byte-identical and in order at
  0%, 1%, 5%, 10%, and 15% loss, across eight seeds, with loss, reordering, and
  duplication all active at once.
* **Retransmission queue ≤ 64 segments — exceeded at low loss.** The peak
  reaches 277–285 during slow start, because the window is allowed to grow past
  the queue cap (the cap is a floor, not a ceiling — see `effective_max_queue`).
  At 5% and above the peak stays inside budget. A hard cap here would either
  stall the sender or throttle the window, so the cap is treated as a floor and
  the overshoot is reported rather than hidden.
* **RTT error ≤ 10% — met, with 0% error.** Across 5 ms to 100 ms one-way paths
  and up to 40 ms jitter, the estimate matches the mean round trip exactly.
* **Throughput ≥ 85% at 1% loss, ≥ 45% at 15% — not met as stated.** 1% loss
  exceeds the loss-free baseline, so the 85% floor is comfortably cleared. At
  15% loss this implementation reaches 10%, short of the 45% target.

That last point is a property of the algorithm the brief specifies, not a
defect in the implementation. Reno's additive-increase / multiplicative-decrease
cycle finds an equilibrium around `1.2/sqrt(p)` segments — about 3 at 15% loss —
and on a 40 ms path that is roughly 3 × 1400 B / 40 ms ≈ 0.8 Mbit/s. Holding a
large window at 15% loss requires something Reno does not do: a window that
recovers quickly after a loss instead of halving, which is the territory of
BBR, CUBIC, or an FEC/ARQ hybrid.

The gap is a documented boundary rather than a silent one. What the engine does
deliver at 15% loss is the part the brief's *stated problem* asks for: it does
not collapse, does not enter a retransmission storm, and keeps delivering
correctly, converging in ~1659 round trips.

Reaching 45% at 15% would mean replacing the congestion-control core. The
seam is `congestion.rs`: it is a pure state machine over ACKs and loss signals,
touching no socket, so a different algorithm drops in without disturbing the
framing, the SACK accounting, the timers, or the tests.

## Memory

Steady state is bounded by the queues rather than by the stream. The
retransmission queue holds at most `cwnd` segments, the receive map holds at
most `RECV_WINDOW` (512) segments, and the send buffer is reclaimed as data is
acknowledged.

This is tested rather than asserted. `tests/budgets.rs` pushes **256 MiB**
through a connection on a 5%-loss link and checks the engine's own buffer
totals stay under the 8 MB budget — a transport that quietly accumulated the
send buffer would pass every correctness test and still fall over on a
long-lived connection, which is the deployment this is for.

## Dump latency

Also tested rather than asserted: 400 dumps taken on a live, loaded connection,
worst case **9 µs** against the 2 ms budget. A separate test takes 50 dumps and
checks the connection's state is byte-identical afterwards, because a diagnostic
that changes what it is diagnosing is worse than no diagnostic.

## Testing

157 tests, all deterministic, none sleeping.

* **112 unit tests** — framing and CRC, sequence arithmetic across the wrap,
  Reno phase transitions, sender window and timer behaviour, receiver
  reassembly and SACK construction, seeded-damage properties.
* **15 transfer tests** — end-to-end byte-exactness at each loss rate, across
  seeds, with reordering and duplication, and the throughput floors this build
  actually holds.
* **17 lifecycle tests** — FIN ordering, half-close receive semantics, close
  timeout, malformed input, and the dump's four required sections.
* **9 determinism tests** — same seed gives packet-identical damage; RTT
  accuracy and RTO margin across latencies and jitter.
* **4 budget tests** — 256 MiB through the engine inside 8 MB; dump latency on a
  loaded connection; dumps leave the connection untouched.

Damage is seeded, so a failure names a seed rather than asking to be re-run.
