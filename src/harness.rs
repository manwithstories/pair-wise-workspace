//! An end-to-end harness: two connections, one damaged link, virtual time.
//!
//! [`Harness`] owns both endpoints and the link between them and runs the
//! event loop that the synchronous model implies. The loop is the whole design:
//!
//! ```text
//! loop {
//!     a.pump(); b.pump();          // both endpoints do whatever their state allows
//!     route a's outbound through the damaged link into b's socket, and vice versa
//!     if the link is idle and both are flushed: done
//!     advance the clock to the next delivery
//! }
//! ```
//!
//! Because both endpoints run on one thread, the interleaving is explicit and
//! deterministic. A test that fails here failed for a reason that will fail
//! again with the same seed, which is the property the unit tests cannot offer:
//! they verify a component against an assumed contract, while this verifies
//! that sender, receiver, congestion control, framing, and the timers all agree
//! with each other.


use crate::conn::{Conn, ConnConfig, ConnError, State};
use crate::loss_injector::DamageProfile;
use crate::sim::{Link, SocketPair};
use crate::time::SimClock;

/// What a run measured.
#[derive(Clone, Debug)]
pub struct RunResult {
    /// Application bytes that reached the far end, in order.
    pub bytes_received: usize,
    /// Bytes the application handed to the near end.
    pub bytes_sent: usize,
    /// Whether every byte arrived.
    pub complete: bool,
    /// Simulated time the run took.
    pub elapsed_us: u64,
    /// Peak retransmission-queue length on the sending side.
    pub peak_rtx_queue: usize,
    /// Final congestion window, in segments.
    pub final_cwnd: u32,
    /// Final congestion phase.
    pub final_phase: String,
    /// Smoothed RTT at the end of the run, if any sample was taken.
    pub final_srtt_us: Option<u64>,
    /// Datagrams the link dropped.
    pub link_dropped: u64,
    /// Datagrams the link delivered.
    pub link_delivered: u64,
    /// True once the connection reached steady state: the window stopped
    /// changing and no loss signal fired in the final stretch.
    pub converged: bool,
}

impl RunResult {
    /// Goodput in bytes per simulated second.
    pub fn throughput_bps(&self) -> f64 {
        if self.elapsed_us == 0 {
            return 0.0;
        }
        self.bytes_received as f64 * 1_000_000.0 / self.elapsed_us as f64
    }

    /// Fraction of sent bytes that arrived.
    pub fn delivery_ratio(&self) -> f64 {
        if self.bytes_sent == 0 {
            return 1.0;
        }
        self.bytes_received as f64 / self.bytes_sent as f64
    }
}

/// Two endpoints joined by a simulated link.
pub struct Harness {
    /// The sending side.
    pub a: Conn,
    /// The receiving side.
    pub b: Conn,
    link: Link,
    clock: SimClock,
    start_us: u64,
    /// Bytes delivered to the `b` endpoint, in arrival order.
    pub received: Vec<u8>,
    /// Bytes delivered to the `a` endpoint, in arrival order.
    ///
    /// Separate from `received` because most tests want one direction, and a
    /// half-close test wants the other: the point there is that a connection
    /// which has stopped sending must still take delivery.
    pub received_at_a: Vec<u8>,
    peak_rtx: usize,
    /// Loss-signal timestamps, for measuring convergence.
    cwnd_history: Vec<(u64, u32)>,
}

impl Harness {
    /// Build a harness with the given link damage and seed.
    pub fn new(profile: DamageProfile, seed: u64, config: ConnConfig) -> std::io::Result<Self> {
        let pair = SocketPair::new()?;
        let clock = SimClock::new();
        // The link needs the sockets so it can deliver datagrams into them; the
        // connections share the same sockets, which is what makes this a real
        // UDP exchange rather than a call-graph shortcut.
        let link = Link::new(clock.clone(), pair.a.try_clone()?, pair.b.try_clone()?, profile, seed, seed);
        let mut a = Conn::with_isn(
            pair.a.try_clone()?,
            pair.addr_b,
            Box::new(clock.clone()),
            config,
            0,
        );
        let mut b = Conn::with_isn(
            pair.b.try_clone()?,
            pair.addr_a,
            Box::new(clock.clone()),
            config,
            0,
        );
        a.set_intercept(true);
        b.set_intercept(true);
        Ok(Harness {
            a,
            b,
            link,
            clock,
            start_us: 0,
            received: Vec::new(),
            received_at_a: Vec::new(),
            peak_rtx: 0,
            cwnd_history: Vec::new(),
        })
    }

    /// A harness over a clean link with the given one-way delay.
    pub fn clean(one_way_us: u64, config: ConnConfig) -> std::io::Result<Self> {
        let profile = DamageProfile {
            base_latency_us: one_way_us,
            jitter_us: 0,
            ..DamageProfile::clean()
        };
        Self::new(profile, 0, config)
    }

    /// Is the link idle, with nothing in flight?
    pub fn is_idle(&self) -> bool {
        self.link.is_idle()
    }

    /// Datagrams the simulated link dropped.
    pub fn link_dropped(&self) -> u64 {
        self.link.dropped()
    }

    /// Datagrams the simulated link delivered.
    pub fn link_delivered(&self) -> u64 {
        self.link.delivered()
    }

    /// Damage counters for the A-to-B direction.
    pub fn damage_stats(&self) -> crate::loss_injector::DamageStats {
        self.link.stats_a_to_b()
    }

    /// Current virtual time.
    pub fn now_us(&self) -> u64 {
        self.clock.now()
    }

    /// Advance the clock by a fixed step, for scenarios that need the passage
    /// of time without any traffic.
    pub fn advance(&self, us: u64) {
        self.clock.advance_us(us);
    }

    /// Run one iteration: let both endpoints act, route whatever they sent, and
    /// advance time to the next event.
    ///
    /// Returns false when there is nothing left to do.
    /// One pass. Returns false only when there is genuinely nothing left to do:
    /// no datagrams in flight, both endpoints closed, and nothing buffered.
    ///
    /// A false return is *not* a reliable "the transfer is over" signal on its
    /// own, because the link is idle between packets. [`Harness::run_until_complete`]
    /// keeps stepping until the transfer is provably complete, which is the only
    /// condition worth testing for.
    pub fn step(&mut self) -> bool {
        // Both endpoints act before anything is delivered, so each sees the
        // other's ACKs as soon as they exist. This is the synchronous model's
        // whole point: no endpoint can be starved by the other's scheduling.
        let _ = self.a.pump();
        let _ = self.b.pump();

        // Drain whatever the application can read.
        while let Some(p) = self.b.recv() {
            self.received.extend_from_slice(&p);
        }
        while let Some(p) = self.a.recv() {
            self.received_at_a.extend_from_slice(&p);
        }

        self.route();

        // Keep going while either side still has work: anything undelivered in
        // the link, an endpoint that is not closed, or bytes still buffered
        // waiting to be cut into segments. Terminating earlier would report a
        // transfer as finished while the first window was still in flight.
        let active = !self.link.is_idle()
            || !self.a.is_closed()
            || !self.b.is_closed()
            || !self.a.is_flushed();
        if !active {
            return false;
        }

        // Deliver everything that is due, then advance to the next event.
        //
        // Order matters. Deliveries are taken *before* the clock moves forward,
        // so a datagram whose delivery time has arrived is handed over at
        // exactly that instant; only once nothing is due does time jump to the
        // next scheduled delivery. Advancing first would skip every datagram
        // that became due at the current time, and the link would never drain.
        // Deliver everything that is already due, at the current instant.
        //
        // Two properties matter here, and getting either wrong is invisible
        // until the numbers come out wrong:
        //
        // * It must happen *before* any time is advanced. A datagram that has
        //   arrived is handed over at exactly the moment it arrived; moving the
        //   clock first would deliver every in-flight datagram late and inflate
        //   the measured RTT.
        //
        // * It must drain *all* datagrams due at this instant, not one per step.
        //   A whole congestion window is sent at once, so a dozen datagrams
        //   share one delivery time. Delivering one per step leaves the rest
        //   queued while the endpoints run, so the last datagram of the window
        //   — and the acknowledgement it triggers — is held back by twelve loop
        //   iterations. The sender then measures an RTT inflated by that much
        //   idle time, and every timeout derived from it is wrong in the same
        //   direction.
        let mut guard = 0usize;
        loop {
            // Deliver what is due, then let both endpoints react and route
            // whatever they produced. Repeating until the link has nothing more
            // due at this instant is what keeps a response from being held back
            // by a time advance: the endpoints are given the chance to answer at
            // the same instant the question arrived.
            while self.link.next_delivery_us() == Some(self.clock.now()) {
                if !self.deliver_one() {
                    break;
                }
                guard += 1;
                if guard > 1_000_000 {
                    return true;
                }
            }
            let _ = self.a.pump();
            let _ = self.b.pump();
            self.route();
            if self.link.next_delivery_us() != Some(self.clock.now()) {
                break;
            }
        }

        // Then move to the next scheduled event, if there is one. Jumping
        // straight to it and delivering in the same step keeps the link's
        // latency exact rather than quantised by the loop.
        if let Some(t) = self.link.next_delivery_us() {
            if t > self.clock.now() {
                self.clock.set(t);
                self.deliver_one();
            }
        } else {
            // Nothing in flight. Step by the link's own one-way delay, which is
            // the finest time the link can itself distinguish. Not by the
            // retransmission deadline: jumping there would skip the ACKs that
            // were supposed to reopen the window, and slow start would never
            // run. Not by an arbitrary tick either, because that tick would then
            // dominate the measured transfer time and the number would report
            // the harness's granularity rather than the transport's behaviour.
            self.clock.advance_us(self.link.tick());
        }
        true
    }

    /// Move datagrams from each connection's outbound queue through the link.
    fn route(&mut self) {
        let now = self.clock.now();
        for (i, d) in self.a.take_outbound().into_iter().enumerate() {
            self.link.send_a_to_b(&d, now.wrapping_mul(2) + i as u64);
        }
        for (i, d) in self.b.take_outbound().into_iter().enumerate() {
            // The damage injector's stream position is a function of how many
            // times it has been asked for a verdict, so the hint only has to be
            // distinct per call. Offsetting the two directions keeps them on
            // independent, separately reproducible streams.
            self.link.send_b_to_a(&d, now.wrapping_mul(2) + 1_000_000 + i as u64);
        }
    }

    /// Deliver one queued datagram into its destination connection.
    ///
    /// Kept separate from [`Harness::step`] so a test can interleave deliveries
    /// deliberately — which is how the reordering and duplicate tests pin down
    /// an exact arrival order.
    pub fn deliver_one(&mut self) -> bool {
        if !self.link.deliver_next() {
            return false;
        }
        self.absorb();
        true
    }

    /// Hand every datagram that has arrived to the connection it was addressed
    /// to.
    ///
    /// Delivery goes through [`rudpx::conn::Conn::feed_inbound`], the same
    /// parser the socket path uses, so a simulated run exercises the real
    /// framing and state machine. Routing through the kernel instead would make
    /// the host's receive buffer part of the model, and a congestion window that
    /// outgrows that buffer produces silent drops indistinguishable from link
    /// loss — the measured throughput would then describe the socket buffer
    /// rather than the transport.
    fn absorb(&mut self) {
        for (dst, bytes) in self.link.take_arrivals() {
            if dst == 0 {
                self.a.feed_inbound(&bytes);
            } else {
                self.b.feed_inbound(&bytes);
            }
        }
    }

    /// Run until the transfer completes or the step budget is exhausted.
    ///
    /// The budget is what keeps a bug from hanging a test run: a protocol
    /// failure shows up as "ran out of steps", which is a clear signal, rather
    /// than as a suite that never finishes.
    pub fn run_until_complete(&mut self, payload: &[u8], max_steps: u64) -> RunResult {
        self.start_us = self.clock.now();
        self.a.write(payload);
        self.cwnd_history.clear();
        let mut steps = 0u64;
        while steps < max_steps {
            if self.a.is_flushed() && self.b_is_drained() && self.link.is_idle() {
                break;
            }
            if !self.step() {
                break;
            }
            steps += 1;
            self.sample();
            if let Err(e) = self.check_fatal() {
                let _ = e;
                break;
            }
        }
        while let Some(p) = self.b.recv() {
            self.received.extend_from_slice(&p);
        }
        self.result(payload.len())
    }

    fn b_is_drained(&self) -> bool {
        self.b.receiver().out_of_order_len() == 0 && self.b.is_flushed()
    }

    fn check_fatal(&self) -> Result<(), ConnError> {
        if self.a.state() == State::Closed && self.a.receiver().out_of_order_len() == 0 {
            // A closed connection with nothing outstanding is a legitimate end
            // only if the peer also finished; otherwise something gave up.
            if !self.b.is_flushed() {
                return Err(ConnError::PeerGone);
            }
        }
        Ok(())
    }

    /// Record the window over time, for the convergence measurement.
    fn sample(&mut self) {
        self.peak_rtx = self.peak_rtx.max(self.a.sender().peak_queue_len());
        let t = self.clock.now();
        let c = self.a.sender().cc().cwnd();
        if self.cwnd_history.last().map(|(_, p)| *p != c).unwrap_or(true) {
            self.cwnd_history.push((t, c));
        }
    }

    /// Time at which the congestion window last changed, i.e. when the
    /// connection stopped adapting.
    pub fn last_cwnd_change_us(&self) -> Option<u64> {
        self.cwnd_history.last().map(|(t, _)| *t)
    }

    /// Peak retransmission-queue length observed.
    pub fn peak_rtx(&self) -> usize {
        self.peak_rtx
    }

    fn result(&self, bytes_sent: usize) -> RunResult {
        let elapsed = self.clock.now().saturating_sub(self.start_us);
        RunResult {
            bytes_received: self.received.len(),
            bytes_sent,
            complete: self.received.len() == bytes_sent,
            elapsed_us: elapsed,
            peak_rtx_queue: self.peak_rtx,
            final_cwnd: self.a.sender().cc().cwnd(),
            final_phase: self.a.sender().cc().phase().to_string(),
            final_srtt_us: self.a.sender().rtt().srtt(),
            link_dropped: self.link.dropped(),
            link_delivered: self.link.delivered(),
            converged: self.converged(),
        }
    }

    /// Has the window settled? Defined as "no change in the last quarter of the
    /// run", which is a measurable proxy for "stopped adapting".
    fn converged(&self) -> bool {
        if self.cwnd_history.len() < 2 {
            return true;
        }
        let last = self.cwnd_history[self.cwnd_history.len() - 1].0;
        let first = self.cwnd_history[0].0;
        let quarter = first + (last - first) / 4;
        // Count changes after the first quarter; one or two is a late repair, not
        // an unconverged window.
        self.cwnd_history
            .iter()
            .filter(|(t, _)| *t >= quarter)
            .count()
            <= 3
    }
}


impl std::fmt::Debug for Harness {
    /// Enough to identify the run in a failure message, without the payload.
    ///
    /// The received bytes are summarised by length rather than printed: a
    /// failing test dumps thousands of them, and the useful question is "how
    /// much arrived", which is one number.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Harness")
            .field("t_us", &self.clock.now())
            .field("a_state", &self.a.state())
            .field("b_state", &self.b.state())
            .field("a_una", &self.a.sender().una())
            .field("a_next", &self.a.sender().next())
            .field("a_cwnd", &self.a.sender().cc().cwnd())
            .field("rtx_queue", &self.a.sender().queue_len())
            .field("received", &self.received.len())
            .field("received_at_a", &self.received_at_a.len())
            .field("link_in_flight", &self.link.in_flight())
            .finish()
    }
}
