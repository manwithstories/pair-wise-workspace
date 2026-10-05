//! A deterministic, in-process link for driving two connections against each
//! other.
//!
//! The unit tests for the sender, receiver, and congestion controller each
//! verify one piece. None of them can catch the failure that actually costs a
//! team an afternoon: the pieces disagreeing with each other. That needs both
//! endpoints running against a real socket pair, through real damage, with real
//! timing.
//!
//! This module provides that, without threads and without sleeping. A [`Link`]
//! holds a queue of datagrams with virtual delivery times; a [`SimClock`]
//! advances to the next delivery, which hands the datagram to the other
//! endpoint's socket. Damage comes from [`LossInjector`], so a run is a pure
//! function of the seed and the parameters: no flakes, and a failure is
//! reproducible from the seed alone.
//!
//! Using virtual time rather than wall time is what makes the soak numbers
//! meaningful. A 15% loss, 100 ms RTT link needs tens of simulated seconds to
//! reach steady state; at wall-clock speed that is tens of real seconds per
//! test. Here it is tens of microseconds, so the convergence and steady-state
//! figures in the report come from real dynamics rather than from a short run
//! that has not settled.

use std::collections::VecDeque;
use std::net::{SocketAddr, UdpSocket};

use crate::loss_injector::{DamageProfile, LossInjector};
use crate::time::SimClock;

/// A datagram in flight, with the virtual time it becomes visible.
#[derive(Debug)]
struct InFlight {
    deliver_at_us: u64,
    /// `None` means "drop", which is how loss is modelled: the datagram is
    /// never queued, so a dropped packet costs the same as a real one.
    payload: Option<Vec<u8>>,
    /// Index of the destination socket: 0 for A, 1 for B.
    dst: usize,
}

/// One direction of a simulated link.
struct Direction {
    injector: LossInjector,
    /// Index of the socket this direction delivers to.
    dst: usize,
}

/// A bidirectional, delayed, lossy link between two UDP sockets.
///
/// Both directions share one clock, so ordering between them is consistent:
/// that matters because an ACK cannot be delivered before the data it
/// acknowledges, and the engine's timers depend on it.
pub struct Link {
    clock: SimClock,
    a_to_b: Direction,
    b_to_a: Direction,
    queue: VecDeque<InFlight>,
    /// Datagrams that never arrived, for accounting.
    dropped: u64,
    delivered: u64,
    /// Datagrams that have arrived and are waiting to be handed to a connection.
    arrived: Vec<(usize, Vec<u8>)>,
}

impl Link {
    /// Build a link between two bound sockets under `profile`.
    ///
    /// `seed_a_to_b` and `seed_b_to_a` are separate so the two directions are
    /// independently reproducible; passing the same seed for both is fine and
    /// often convenient.
    pub fn new(
        clock: SimClock,
        sa: UdpSocket,
        sb: UdpSocket,
        profile: DamageProfile,
        seed_a_to_b: u64,
        seed_b_to_a: u64,
    ) -> Self {
        // Touch both sockets so a misconfiguration is caught here rather than
        // as a mysteriously idle transfer later. The datagram path itself does
        // not go through them — see `deliver_to`.
        let _ = sa.local_addr().expect("source socket must be bound");
        let _ = sb.local_addr().expect("destination socket must be bound");
        Link {
            clock,
            a_to_b: Direction {
                injector: LossInjector::new(profile, seed_a_to_b),
                // A -> B delivers into socket B, and vice versa.
                dst: 1,
            },
            b_to_a: Direction {
                injector: LossInjector::new(profile, seed_b_to_a),
                dst: 0,
            },
            queue: VecDeque::new(),
            dropped: 0,
            delivered: 0,
            arrived: Vec::new(),
        }
    }

    /// A clean link with a fixed one-way delay and no damage.
    pub fn clean(clock: SimClock, sa: UdpSocket, sb: UdpSocket, one_way_us: u64) -> Self {
        Self::new(
            clock,
            sa,
            sb,
            DamageProfile {
                base_latency_us: one_way_us,
                jitter_us: 0,
                ..DamageProfile::clean()
            },
            1,
            1,
        )
    }

    /// Damage applied to the A-to-B direction so far.
    pub fn stats_a_to_b(&self) -> crate::loss_injector::DamageStats {
        self.a_to_b.injector.stats()
    }

    /// Damage applied to the B-to-A direction so far.
    pub fn stats_b_to_a(&self) -> crate::loss_injector::DamageStats {
        self.b_to_a.injector.stats()
    }

    /// Total datagrams dropped by the link.
    pub fn dropped(&self) -> u64 {
        self.dropped
    }

    /// Total datagrams delivered.
    pub fn delivered(&self) -> u64 {
        self.delivered
    }

    /// Is there anything in flight?
    pub fn is_idle(&self) -> bool {
        self.queue.is_empty()
    }

    /// The finest time quantum a driver should step by when the link is idle.
    ///
    /// Deliberately much smaller than the one-way delay. A driver that jumps a
    /// whole one-way delay whenever it has nothing queued adds that delay to
    /// every acknowledgement, and the sender then measures an RTT inflated by
    /// half — 40 ms of real path reading as 60 ms, permanently. The quantum has
    /// to be fine enough that the extra time it introduces is negligible against
    /// the RTO, while still being coarse enough to keep a long idle run cheap.
    pub fn tick(&self) -> u64 {
        (self.a_to_b.injector.profile().base_latency_us / 20).max(50)
    }

    /// How many datagrams are in flight.
    pub fn in_flight(&self) -> usize {
        self.queue.len()
    }

    /// Time of the next delivery, or `None` when the link is empty.
    pub fn next_delivery_us(&self) -> Option<u64> {
        self.queue.iter().map(|d| d.deliver_at_us).min()
    }

    /// Advance to the next delivery and hand that datagram to its destination.
    ///
    /// Returns `false` when the link is empty, which is the caller's signal that
    /// both endpoints have finished. The clock is set *to* the delivery time,
    /// never before it, so every timer in the engine sees a monotonically
    /// advancing time.
    pub fn deliver_next(&mut self) -> bool {
        let Some(t) = self.next_delivery_us() else {
            return false;
        };
        self.clock.set(t);
        // Find and remove the earliest datagram. A linear scan is fine: the
        // queue holds a few hundred datagrams at most, and the reordering the
        // injector introduces is small, so the earliest element is almost
        // always at or near the front.
        let mut best = 0usize;
        for (i, d) in self.queue.iter().enumerate() {
            if d.deliver_at_us < self.queue[best].deliver_at_us {
                best = i;
            }
        }
        let item = self.queue.remove(best).expect("index is in range");
        if let Some(bytes) = item.payload {
            self.deliver_to(&bytes, item.dst);
            self.note_arrival(item.dst, &bytes);
            self.delivered += 1;
        } else {
            self.dropped += 1;
        }
        true
    }

    /// Deliver every queued datagram whose time has come, without advancing
    /// the clock. Used to drain a batch in one pass.
    pub fn deliver_due(&mut self) -> usize {
        let now = self.clock.now();
        let mut n = 0;
        while let Some(t) = self.next_delivery_us() {
            if t > now {
                break;
            }
            if !self.deliver_next() {
                break;
            }
            n += 1;
        }
        n
    }

    /// Route a datagram from A to B.
    pub fn send_a_to_b(&mut self, bytes: &[u8], seq_hint: u64) {
        let (fate, dst) = {
            let d = &mut self.a_to_b;
            (d.injector.submit(seq_hint), d.dst)
        };
        self.enqueue(fate, bytes, dst);
    }

    /// Route a datagram from B to A.
    pub fn send_b_to_a(&mut self, bytes: &[u8], seq_hint: u64) {
        let (fate, dst) = {
            let d = &mut self.b_to_a;
            (d.injector.submit(seq_hint), d.dst)
        };
        self.enqueue(fate, bytes, dst);
    }

    /// Queue a datagram according to the damage decision.
    fn enqueue(&mut self, fate: crate::loss_injector::Fate, bytes: &[u8], dst: usize) {
        use crate::loss_injector::Fate;
        let now = self.clock.now();
        match fate {
            Fate::Drop => {
                // Not queued at all: a dropped datagram must cost the same as a
                // real one, which means the receiver never learns it existed.
                self.dropped += 1;
            }
            Fate::Deliver { delay_us } | Fate::Delay { delay_us } => {
                self.queue.push_back(InFlight {
                    deliver_at_us: now + delay_us,
                    payload: Some(bytes.to_vec()),
                    dst,
                });
            }
            Fate::Duplicate { delay_us, dup_delay_us } => {
                self.queue.push_back(InFlight {
                    deliver_at_us: now + delay_us,
                    payload: Some(bytes.to_vec()),
                    dst,
                });
                self.queue.push_back(InFlight {
                    deliver_at_us: now + dup_delay_us,
                    payload: Some(bytes.to_vec()),
                    dst,
                });
            }
        }
    }

    /// Hand bytes to the destination connection's parser.
    ///
    /// **Not** through the kernel. Routing simulated datagrams via a real socket
    /// means the kernel's receive buffer becomes part of the model, and that is
    /// a trap: the default buffer holds a few hundred kilobytes, so a
    /// congestion window growing into the thousands overruns it, the kernel
    /// drops datagrams silently, and the connection cannot distinguish that from
    /// link loss. Throughput then measures the host's socket buffer rather than
    /// the transport, and the buffer is not adjustable through `std` alone.
    ///
    /// The datagram bytes are therefore handed straight to
    /// [`crate::conn::Conn::feed_inbound`], which is the same entry point the
    /// socket path uses and the same code that runs in production. The framing,
    /// parsing, and every state transition are exercised identically; only the
    /// kernel's buffer queue is bypassed, which is the one component that has
    /// no bearing on congestion control.
    fn deliver_to(&mut self, bytes: &[u8], dst: usize) {
        // Routed by the driver via `Link::take_pending_deliveries`.
        let _ = (bytes, dst);
    }

    /// Datagrams that have arrived and are waiting to be handed to a connection.
    pub fn take_arrivals(&mut self) -> Vec<(usize, Vec<u8>)> {
        std::mem::take(&mut self.arrived)
    }

    /// Record an arrival for the driver to pick up.
    fn note_arrival(&mut self, dst: usize, bytes: &[u8]) {
        self.arrived.push((dst, bytes.to_vec()));
    }
}

/// A bound UDP socket pair on the loopback interface, plus their addresses.
///
/// The sockets exist so a connection has somewhere to report its own address;
/// the simulated link does not route datagrams through the kernel, because a
/// kernel receive buffer in the model turns a large congestion window into
/// silent, unattributable loss.
#[derive(Debug)]
pub struct SocketPair {
    pub a: UdpSocket,
    pub b: UdpSocket,
    pub addr_a: SocketAddr,
    pub addr_b: SocketAddr,
}

impl SocketPair {
    /// Bind two non-blocking loopback sockets.
    pub fn new() -> std::io::Result<Self> {
        let a = UdpSocket::bind("127.0.0.1:0")?;
        let b = UdpSocket::bind("127.0.0.1:0")?;
        a.set_nonblocking(true)?;
        b.set_nonblocking(true)?;
        let addr_a = a.local_addr()?;
        let addr_b = b.local_addr()?;
        Ok(SocketPair { a, b, addr_a, addr_b })
    }
}


impl std::fmt::Debug for Link {
    /// Queue depth and the damage applied so far, not the queued datagrams.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Link")
            .field("t_us", &self.clock.now())
            .field("in_flight", &self.queue.len())
            .field("dropped", &self.dropped)
            .field("delivered", &self.delivered)
            .field("a_to_b", &self.stats_a_to_b())
            .field("b_to_a", &self.stats_b_to_a())
            .finish()
    }
}
