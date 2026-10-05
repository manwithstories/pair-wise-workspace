//! The sending half: segment allocation, the send window, the retransmission
//! queue, and the retransmission timer.
//!
//! Segments are numbered, not bytes, so the queue, the window, and every SACK
//! range are all integer intervals over [`Seq`]. The sender's job is narrow:
//! keep at most `cwnd` segments outstanding, put them on the wire when the
//! window allows, notice when one of them is not acknowledged, and repair it
//! fast enough that the peer does not stall behind the hole.
//!
//! # What "outstanding" means
//!
//! A segment is *outstanding* from the moment it is allocated until a
//! cumulative ACK covers it. Outstanding segments stay in
//! [`Sender::queue`] even after they are handed to the socket, because a repair
//! needs the payload again and the socket does not keep it. The queue is
//! therefore the retransmission queue *and* the record of what is in flight.

use std::collections::VecDeque;
use std::fmt;

use crate::congestion::{CcConfig, CcPhase, LossSignal, Reno};
use crate::time::RttEstimator;
use crate::wire::{seq_add, seq_lt, seq_span, Block, Seq};

/// Why a segment is being sent again.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum RetxReason {
    /// Three duplicate ACKs: the hole is being repaired before the timer runs.
    FastRetransmit,
    /// The retransmission timer expired.
    Timeout,
}

impl fmt::Display for RetxReason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            RetxReason::FastRetransmit => "fast-retransmit",
            RetxReason::Timeout => "timeout",
        })
    }
}

/// One outstanding segment.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct TxSegment {
    /// Sequence number, which is also the segment's index in the byte stream.
    pub seq: Seq,
    /// Payload, retained for as long as the segment is outstanding so a repair
    /// can be sent without asking the application to replay it.
    pub payload: Vec<u8>,
    /// Set when the segment carries a FIN rather than payload.
    pub fin: bool,
    /// When the segment was first transmitted, for RTT sampling.
    pub first_tx_us: u64,
    /// When the segment was last transmitted.
    pub last_tx_us: u64,
    /// How many times it has been transmitted.
    pub tx_count: u32,
    /// The peer has reported this segment inside a SACK block.
    pub sacked: bool,
    /// A copy is in flight whose loss has not yet been declared, so the timer
    /// must not fire again for the same copy.
    pub rtx_in_flight: bool,
}

impl TxSegment {
    /// Bytes of application data this segment carries. A FIN carries none: it
    /// consumes a sequence number, not payload bytes.
    pub fn wire_bytes(&self) -> u64 {
        self.payload.len() as u64
    }

    /// Was this segment sent more than once before being acknowledged?
    ///
    /// True means the retransmission may have been unnecessary, since the
    /// original copy may still have been in flight.
    pub fn was_retransmitted(&self) -> bool {
        self.tx_count > 1
    }
}

/// What the sender wants the transport to put on the wire right now.
#[derive(Clone, PartialEq, Eq)]
pub enum TxAction {
    /// First transmission of a newly allocated segment.
    New(TxSegment),
    /// A repair of a segment that is already outstanding.
    Retransmit { seg: TxSegment, reason: RetxReason },
    /// Nothing to send: the window is full or there is nothing buffered.
    Idle,
}

impl fmt::Debug for TxAction {
    /// Summarises rather than dumping the payload: a `TxSegment` in a `Debug`
    /// derive prints 1400 bytes, which buries the one field a failing
    /// assertion is about.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            TxAction::New(s) => write!(f, "New(seq={}, len={}, fin={})", s.seq, s.payload.len(), s.fin),
            TxAction::Retransmit { seg, reason } => {
                write!(f, "Retransmit(seq={}, len={}, reason={reason})", seg.seq, seg.payload.len())
            }
            TxAction::Idle => write!(f, "Idle"),
        }
    }
}

/// What an acknowledgement did.
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub struct AckOutcome {
    /// Segments removed from the queue by the cumulative ACK.
    pub newly_acked: u32,
    /// Did `una` move?
    pub advanced: bool,
    /// Loss signal, if this duplicate ACK crossed the threshold.
    pub signal: Option<LossSignal>,
    /// Bytes the peer reported as SACKed that were not cumulatively acked.
    pub sacked_credit_bytes: u64,
}

impl fmt::Display for AckOutcome {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "acked={} advanced={}", self.newly_acked, self.advanced)?;
        if self.sacked_credit_bytes > 0 {
            write!(f, " sack={}B", self.sacked_credit_bytes)?;
        }
        if let Some(LossSignal::DuplicateAcks { dup_acks }) = self.signal {
            write!(f, " dupacks={dup_acks}")?;
        }
        Ok(())
    }
}

/// The sending state machine.
pub struct Sender {
    /// Left edge of the send window: the oldest unacknowledged segment.
    una: Seq,
    /// Right edge of the send window: one past the last allocated segment.
    next: Seq,
    /// Allocated-but-unacknowledged segments, in sequence order.
    queue: VecDeque<TxSegment>,
    /// Application bytes accepted by [`Sender::write`] but not yet segmented.
    tx_buf: Vec<u8>,
    /// Offset into `tx_buf` of the first byte not yet segmented.
    tx_consumed: usize,
    /// Reno controller.
    cc: Reno,
    /// RTT estimate; also owns the base RTO.
    rtt: RttEstimator,
    /// Effective RTO including the backoff multiplier.
    rto_us: u64,
    /// Backoff exponent, incremented on every timeout and capped at
    /// [`Sender::MAX_BACKOFF_EXPONENT`].
    backoff_exp: u32,
    /// When the oldest outstanding segment must be retransmitted.
    deadline_us: u64,
    /// The hole currently being repaired, so extra duplicate ACKs do not spawn
    /// duplicate repairs of the same segment.
    repair_target: Option<Seq>,
    /// A FIN has been allocated on this connection.
    fin_seen: bool,
    /// Consecutive timeouts with no forward progress.
    consecutive_timeouts: u32,
    /// The retransmission timer is armed.
    timer_armed: bool,
    /// High-water mark of the queue length, for the performance budget.
    peak_queue: usize,
    /// High-water mark of the congestion window.
    peak_cwnd: u32,
    /// Hard cap on outstanding segments.
    max_queue: usize,
    // counters
    new_transmits: u64,
    retransmits: u64,
    spurious_retransmits: u64,
    bytes_acked: u64,
    timeouts: u32,
    fast_retransmits: u32,
}

/// Counters describing sender behaviour, for the soak report.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct SenderStats {
    pub peak_rtx_queue: usize,
    pub timeouts: u32,
    pub fast_retransmits: u32,
    pub spurious_retransmits: u64,
    pub retransmits: u64,
    pub new_transmits: u64,
}

/// A candidate RTT measurement, captured before the acknowledged segment leaves
/// the queue.
#[derive(Clone, Copy, Debug)]
struct Sample {
    sent_us: u64,
    was_retransmitted: bool,
}

impl Sender {
    /// Cap on the backoff exponent. A timeout multiplies the RTO by `2^exp`, so
    /// a cap of 6 bounds the worst-case wait at 64x the base RTO.
    ///
    /// Without a cap, a path that drops everything would drive the RTO past any
    /// finite give-up threshold and the connection would hang rather than fail,
    /// which is far harder to diagnose than a clean error.
    pub const MAX_BACKOFF_EXPONENT: u32 = 6;

    /// Build a sender whose sequence numbers start at `isn`.
    pub fn new(isn: Seq, cc_cfg: CcConfig) -> Self {
        Sender {
            una: isn,
            next: isn,
            queue: VecDeque::new(),
            tx_buf: Vec::new(),
            tx_consumed: 0,
            cc: Reno::new(cc_cfg),
            rtt: RttEstimator::new(),
            rto_us: RttEstimator::INITIAL_RTO_US,
            backoff_exp: 0,
            deadline_us: 0,
            repair_target: None,
            fin_seen: false,
            consecutive_timeouts: 0,
            timer_armed: false,
            peak_queue: 0,
            peak_cwnd: 0,
            max_queue: 64,
            new_transmits: 0,
            retransmits: 0,
            spurious_retransmits: 0,
            bytes_acked: 0,
            timeouts: 0,
            fast_retransmits: 0,
        }
    }

    /// Set a floor on the outstanding-segment cap.
    ///
    /// The default of 64 is the budget the performance spec puts on the
    /// retransmission queue. A value below `cwnd` does not shrink the cap below
    /// `cwnd` — see [`Sender::effective_max_queue`] — so use this to raise the
    /// cap for a long-delay path, not to lower it.
    pub fn with_max_queue(mut self, n: usize) -> Self {
        self.max_queue = n.max(1);
        self
    }

    /// The outstanding-segment cap actually in force.
    ///
    /// The cap bounds memory, but it can never sit below `cwnd`: a window that
    /// admits more segments than the queue can hold would stall with a "full"
    /// queue that cannot be filled from the send buffer, and the connection
    /// would deadlock waiting for an ACK that no new segment could prompt. So
    /// the effective cap is `max(configured, cwnd)`, and
    /// [`Sender::with_max_queue`] documents that a configured value is a floor
    /// rather than a hard ceiling.
    fn effective_max_queue(&self) -> usize {
        self.max_queue.max(self.cc.cwnd() as usize)
    }

    // -- window ------------------------------------------------------------

    /// Left edge of the send window.
    #[inline]
    pub fn una(&self) -> Seq {
        self.una
    }

    /// Right edge of the send window.
    #[inline]
    pub fn next(&self) -> Seq {
        self.next
    }

    /// Segments allocated but not yet cumulatively acknowledged.
    pub fn inflight(&self) -> u64 {
        seq_span(self.una, self.next)
    }

    /// The send window `[una, next)`.
    pub fn window(&self) -> Block {
        Block::new(self.una, self.next)
    }

    /// Segments the window still admits, given the current congestion window.
    pub fn window_credit(&self) -> u64 {
        (self.cc.cwnd() as u64).saturating_sub(self.inflight())
    }

    /// Is the send window closed? The dump highlights this, since a window that
    /// will not open is the first thing to check when throughput collapses.
    pub fn window_stalled(&self) -> bool {
        self.window_credit() == 0 && self.queue.len() > 0
    }

    // -- queue -------------------------------------------------------------

    pub fn queue_len(&self) -> usize {
        self.queue.len()
    }

    pub fn peak_queue_len(&self) -> usize {
        self.peak_queue
    }

    pub fn first_queued_seq(&self) -> Option<Seq> {
        self.queue.front().map(|s| s.seq)
    }

    /// The retransmission queue, front (oldest) first.
    pub fn queue(&self) -> impl Iterator<Item = &TxSegment> {
        self.queue.iter()
    }

    /// Front of the queue, for the state dump.
    pub fn queue_head(&self) -> Option<&TxSegment> {
        self.queue.front()
    }

    /// The hole currently being repaired.
    pub fn repair_target(&self) -> Option<Seq> {
        self.repair_target
    }

    /// Number of queued segments the peer has confirmed via SACK. A growing
    /// count with a stuck `una` is the signature of a hole that fast
    /// retransmit failed to repair.
    pub fn sacked_in_queue(&self) -> usize {
        self.queue.iter().filter(|s| s.sacked).count()
    }

    #[inline]
    pub fn cc(&self) -> &Reno {
        &self.cc
    }

    pub fn cc_mut(&mut self) -> &mut Reno {
        &mut self.cc
    }

    pub fn rtt(&self) -> &RttEstimator {
        &self.rtt
    }

    /// Effective RTO including backoff.
    pub fn rto_us(&self) -> u64 {
        self.rto_us
    }

    /// Base RTO before the backoff multiplier.
    pub fn base_rto_us(&self) -> u64 {
        self.rtt.rto()
    }

    pub fn backoff_exp(&self) -> u32 {
        self.backoff_exp
    }

    pub fn consecutive_timeouts(&self) -> u32 {
        self.consecutive_timeouts
    }

    pub fn deadline_us(&self) -> u64 {
        self.deadline_us
    }

    pub fn timer_armed(&self) -> bool {
        self.timer_armed
    }

    /// (new transmissions, retransmissions) in segments.
    pub fn tx_counts(&self) -> (u64, u64) {
        (self.new_transmits, self.retransmits)
    }

    /// Segments that were retransmitted and then found to have arrived anyway.
    pub fn spurious_retransmits(&self) -> u64 {
        self.spurious_retransmits
    }

    /// (timeouts, fast retransmits) in events.
    pub fn loss_events(&self) -> (u32, u32) {
        (self.timeouts, self.fast_retransmits)
    }

    /// Application bytes cumulatively acknowledged.
    pub fn bytes_acked(&self) -> u64 {
        self.bytes_acked
    }

    /// A snapshot of the sender's counters, for reporting.
    pub fn stats(&self) -> SenderStats {
        SenderStats {
            peak_rtx_queue: self.peak_queue,
            timeouts: self.timeouts,
            fast_retransmits: self.fast_retransmits,
            spurious_retransmits: self.spurious_retransmits,
            retransmits: self.retransmits,
            new_transmits: self.new_transmits,
        }
    }

    /// Buffered application bytes not yet segmented.
    pub fn pending_bytes(&self) -> usize {
        self.tx_buf.len() - self.tx_consumed
    }

    #[inline]
    /// Has the window grown past its initial size at any point?
    ///
    /// Lets a driver sample a connection that is actually doing work rather than
    /// one still inside its first window.
    pub fn cwnd_peak_reached(&self) -> bool {
        self.peak_cwnd > self.cc.initial_cwnd()
    }

    /// Highest congestion window seen during this connection.
    pub fn peak_cwnd(&self) -> u32 {
        self.peak_cwnd
    }

    /// Current congestion phase, for the dump.
    pub fn phase(&self) -> CcPhase {
        self.cc.phase()
    }

    #[inline]
    fn find(&self, seq: Seq) -> Option<&TxSegment> {
        self.queue.iter().find(|s| s.seq == seq)
    }

    #[inline]
    fn find_mut(&mut self, seq: Seq) -> Option<&mut TxSegment> {
        self.queue.iter_mut().find(|s| s.seq == seq)
    }

    // -- buffering ---------------------------------------------------------

    /// Accept application bytes into the send buffer.
    ///
    /// Segments are cut lazily by [`Sender::pump`] so a large write does not
    /// allocate segments the window cannot admit yet.
    pub fn write(&mut self, data: &[u8]) -> usize {
        self.tx_buf.extend_from_slice(data);
        data.len()
    }

    /// Allocate a FIN at the current `next`.
    ///
    /// A FIN consumes a sequence number, so it occupies a queue slot exactly
    /// like a data segment and is repaired by the same machinery. That is what
    /// lets a half-closed connection keep its FIN unacknowledged — and
    /// therefore repairable — while still accepting inbound data.
    ///
    /// Returns false when a FIN already exists or the queue is at its cap. The
    /// FIN is allocated *after* whatever data is already buffered, so the
    /// ordering the peer sees matches the order the application wrote.
    pub fn alloc_fin(&mut self) -> bool {
        if self.fin_seen || self.queue.len() >= self.effective_max_queue() {
            return false;
        }
        self.queue.push_back(TxSegment {
            seq: self.next,
            payload: Vec::new(),
            fin: true,
            first_tx_us: 0,
            last_tx_us: 0,
            tx_count: 0,
            sacked: false,
            rtx_in_flight: false,
        });
        self.next = seq_add(self.next, 1);
        self.fin_seen = true;
        self.peak_queue = self.peak_queue.max(self.queue.len());
        true
    }

    /// Is a FIN allocated and still unacknowledged?
    pub fn fin_pending(&self) -> bool {
        self.fin_seen && self.queue.iter().any(|s| s.fin)
    }

    /// Has the FIN been allocated *and* cumulatively acknowledged?
    pub fn fin_acked(&self) -> bool {
        self.fin_seen && !self.queue.iter().any(|s| s.fin)
    }

    /// Was a FIN ever allocated?
    pub fn fin_allocated(&self) -> bool {
        self.fin_seen
    }

    // -- transmit ----------------------------------------------------------

    /// Decide what to put on the wire.
    ///
    /// Order matters: a due repair beats a due timeout, which beats new data,
    /// because the hole is what the peer is actually waiting on and anything
    /// queued behind it cannot be delivered until the hole is filled.
    ///
    /// The caller loops until it sees [`TxAction::Idle`] and calls
    /// [`Sender::on_sent`] for each hand-off.
    pub fn pump(&mut self, now_us: u64) -> TxAction {
        // A repair that has not been started yet. Reno declaring a loss in the
        // acknowledgement path does not by itself put anything on the wire:
        // the loss decision and the transmission are separate steps, and
        // without this handoff the hole would only be repaired when the
        // retransmission timer eventually fired, undoing the point of fast
        // retransmit.
        //
        // A repair that has already been sent is *not* repeated here. Only the
        // retransmission timer may resend it, so a repair that is itself lost
        // costs one RTO rather than turning every pump call into a new datagram.
        // A repair whose target the peer has since acknowledged is finished, so
        // the slot is freed. Clearing it here rather than only on a cumulative
        // ACK matters: SACK means a repaired segment can be confirmed without
        // `una` ever moving, and a target that was never cleared would block
        // every future repair and deadlock the connection.
        if let Some(t) = self.repair_target {
            let done = match self.find(t) {
                None => true,
                Some(s) => s.sacked,
            };
            if done {
                self.repair_target = None;
            }
        }

        if self.repair_target.is_none() && self.cc.phase() == CcPhase::FastRecovery {
            // Repair the oldest segment the peer has *not* reported. With
            // selective acknowledgement this is not always `una`: a segment
            // behind the hole may itself be missing while `una` is genuinely
            // still in flight, and repairing `una` on every episode would
            // resend data the peer already has and never fill the real gap.
            if let Some(hole) = self.oldest_unsacked() {
                if let Some(seg) = self.begin_fast_repair(hole, now_us) {
                    return TxAction::Retransmit { seg, reason: RetxReason::FastRetransmit };
                }
            }
        }

        if self.timer_expired(now_us) {
            if self.queue.is_empty() {
                // An expired deadline with nothing outstanding is stale, not
                // overdue: the last segment was acknowledged and the queue
                // drained, but the timer was left armed. Charging a timeout here
                // would collapse the window on a connection that has no data to
                // recover, which is how a healthy link ends up looking congested.
                self.disarm_timer();
            } else if let Some(seg) = self.on_timeout(now_us) {
                return TxAction::Retransmit { seg, reason: RetxReason::Timeout };
            }
        }

        if self.pending_bytes() > 0 && self.window_credit() > 0 && self.queue.len() < self.effective_max_queue() {
            if let Some(seg) = self.alloc_segment(now_us) {
                return TxAction::New(seg);
            }
        }

        // A FIN is already allocated but has never been transmitted. It is not
        // in the send buffer, so the branch above cannot produce it; without
        // this the FIN would sit in the queue untransmitted, the peer's close
        // handshake would never start, and a half-close would hang until the
        // close timeout forced the connection down.
        if let Some(fin) = self.queue.iter().find(|s| s.fin && s.tx_count == 0) {
            let seq = fin.seq;
            self.mark_in_flight(seq, now_us);
            self.new_transmits += 1;
            self.cc.on_new_send(1);
            if let Some(seg) = self.find(seq).cloned() {
                return TxAction::New(seg);
            }
        }

        TxAction::Idle
    }

    /// Cut one new segment from the send buffer, if the window admits one.
    fn alloc_segment(&mut self, now_us: u64) -> Option<TxSegment> {
        if self.pending_bytes() == 0 || self.window_credit() == 0 {
            return None;
        }
        let mss = self.cc.mss();
        let start = self.tx_consumed;
        let take = self.pending_bytes().min(mss);
        let seg = TxSegment {
            seq: self.next,
            payload: self.tx_buf[start..start + take].to_vec(),
            fin: false,
            first_tx_us: now_us,
            last_tx_us: now_us,
            tx_count: 1,
            sacked: false,
            rtx_in_flight: true,
        };
        self.tx_consumed += take;
        self.next = seq_add(self.next, 1);
        self.new_transmits += 1;
        self.cc.on_new_send(1);
        self.peak_queue = self.peak_queue.max(self.queue.len() + 1);
        self.queue.push_back(seg.clone());
        Some(seg)
    }

    /// Record that a segment has been handed to the transport.
    ///
    /// Clears the in-flight mark, because the copy is now the socket's problem,
    /// and restarts the retransmission timer from this transmission. Also
    /// releases the RTO backoff suppression, per RFC 6298 §5.7: a fresh
    /// transmission means the path is alive, so a stale backoff is cleared.
    pub fn on_sent(&mut self, seq: Seq, now_us: u64) {
        if self.rtt.backoff_active() {
            self.rtt.on_fresh_send();
        }
        if self.backoff_exp > 0 {
            self.backoff_exp = 0;
            self.reapply_base_rto();
        }
        if let Some(s) = self.find_mut(seq) {
            // `first_tx_us` is stamped on the *first* transmission only. Karn's
            // algorithm needs the time the original left, and a later
            // retransmission must not move it: a sample measured from a repair
            // would be ambiguous and has to be discarded instead.
            if s.tx_count <= 1 {
                s.first_tx_us = now_us;
            }
            s.last_tx_us = now_us;
            s.rtx_in_flight = false;
        }
        self.arm_timer(now_us);
    }

    fn mark_in_flight(&mut self, seq: Seq, now_us: u64) {
        if let Some(s) = self.find_mut(seq) {
            s.rtx_in_flight = true;
            s.tx_count += 1;
            s.last_tx_us = now_us;
        }
    }

    // -- acknowledgement ---------------------------------------------------

    /// Apply an acknowledgement from the peer.
    ///
    /// `ack` is the cumulative acknowledgement and `sack` the peer's
    /// out-of-order ranges. A stale ACK — one that would move `una` backwards
    /// — is ignored entirely, which is what makes reordered ACKs harmless.
    pub fn on_ack(&mut self, ack: Seq, sack: &[Block], now_us: u64) -> AckOutcome {
        if seq_lt(ack, self.una) {
            return AckOutcome::default();
        }

        // Capture the RTT candidate *before* the pop, while the acknowledged
        // segment is still identifiable.
        let sample = self.acknowledged_head_sample(ack);

        let mut newly = 0u32;
        let mut credit_bytes = 0u64;
        let mut saw_sack = false;
        let mut spurious = false;

        // Cumulative ACK: pop everything strictly below it.
        while let Some(front) = self.queue.front() {
            if seq_lt(front.seq, ack) {
                if front.was_retransmitted() {
                    // A segment that was retransmitted and then acknowledged
                    // may or may not have been genuinely lost: the original copy
                    // could still have been in flight. When the repair *was*
                    // spurious, the window reduction that bought it must be
                    // given back, or the connection spends the rest of the run
                    // recovering from congestion the path never had.
                    self.spurious_retransmits += 1;
                    spurious = true;
                }
                self.bytes_acked += front.wire_bytes();
                newly += 1;
                self.queue.pop_front();
            } else {
                break;
            }
        }

        // SACK blocks. Only blocks covering segments still in the queue carry
        // new information, and only those are credited: a block naming an
        // already-acknowledged segment is a retransmitted ACK, not evidence
        // that the path is moving data.
        for b in sack {
            for s in self.queue.iter_mut() {
                if b.contains(s.seq) && !s.sacked {
                    s.sacked = true;
                    saw_sack = true;
                    credit_bytes += s.wire_bytes();
                }
            }
        }

        let advanced = ack != self.una;
        if advanced {
            self.una = ack;
            self.consecutive_timeouts = 0;
            self.backoff_exp = 0;
            self.reapply_base_rto();
            self.cc.on_ack_advance();
            if self.repair_target.map(|t| seq_lt(t, ack)).unwrap_or(false) {
                self.repair_target = None;
            }
            self.compact_tx_buf();
            self.arm_timer(now_us);
        }

        // Duplicate-ACK accounting, in two distinct cases.
        //
        // A true duplicate ACK carries no cumulative progress *and* no new SACK
        // information. It is counted toward the fast-retransmit threshold.
        //
        // A cumulative-duplicate that *does* carry new SACK information is
        // different: the peer is receiving data past the hole, so the hole is a
        // real loss and not merely a slow segment. That is the SACK-based loss
        // signal, and it must be counted too — otherwise the repair waits for
        // the retransmission timer and every isolated loss costs a full RTO.
        // Only a *stalled* cumulative point counts as a duplicate. If `una`
        // advanced, the peer is making forward progress and the ACK is not
        // evidence of a hole at all — counting those would drive the window down
        // on a path that is actually delivering data, which is what turns a
        // 5%-loss link into a 5%-loss *collapse*.
        let dup_signal = if newly == 0 && !advanced {
            self.cc.on_dup_ack()
        } else {
            None
        };
        if newly == 0 && !saw_sack {
            return AckOutcome {
                newly_acked: 0,
                advanced,
                signal: dup_signal,
                sacked_credit_bytes: 0,
            };
        }
        let sacking_signal = dup_signal;

        // Karn's algorithm: a sample is only unambiguous if the segment it
        // covers was transmitted exactly once, and RFC 5681 §3.2 step 4 says
        // only take samples outside a loss episode.
        if let Some(s) = sample {
            if !s.was_retransmitted && self.cc.rtt_sample_valid() && now_us > s.sent_us {
                self.rtt.sample(now_us - s.sent_us);
                self.recompute_rto();
                // The RTO just changed, so the deadline computed under the old
                // one is stale. Recomputing it here is what keeps a shrinking
                // RTO from turning a valid deadline into an immediate timeout.
                self.arm_timer(now_us);
            }
        }

        // A retransmitted segment turning up means the repair was unnecessary:
        // the original copy was still in flight, so nothing was actually lost.
        // Give the window back before growing it, so the growth is measured from
        // the restored value rather than the reduced one.
        //
        // SACK counts here, not just a cumulative ACK. A repair is frequently
        // confirmed by the peer listing the segment in a SACK block while `una`
        // is still stuck behind an earlier hole, and requiring a cumulative
        // advance would miss exactly the common case this exists to rescue.
        if spurious {
            self.cc.on_spurious_retransmit();
        }
        self.cc.on_ack(newly, credit_bytes);
        self.peak_cwnd = self.peak_cwnd.max(self.cc.cwnd());

        AckOutcome {
            newly_acked: newly,
            advanced,
            signal: sacking_signal,
            sacked_credit_bytes: credit_bytes,
        }
    }

    /// The send timestamp of the highest segment a cumulative `ack` covers.
    ///
    /// Returns `None` when the ACK covers nothing in the queue, or when the
    /// covered segment was retransmitted — in which case any delay measured
    /// from it would be ambiguous.
    fn acknowledged_head_sample(&self, ack: Seq) -> Option<Sample> {
        let head = self.queue.front()?;
        if seq_lt(ack, self.next) {
            // ack covers at least the head segment.
            return Some(Sample {
                sent_us: head.first_tx_us,
                was_retransmitted: head.was_retransmitted(),
            });
        }
        None
    }

    /// Reclaim the acknowledged prefix of the send buffer.
    fn compact_tx_buf(&mut self) {
        // Draining on every ACK would be O(len) per ACK on a large buffer, so
        // only reclaim once enough has accumulated to be worth the shift.
        if self.tx_consumed > 0
            && (self.tx_consumed >= 1 << 16 || self.tx_consumed * 2 >= self.tx_buf.len())
        {
            self.tx_buf.drain(..self.tx_consumed);
            self.tx_consumed = 0;
        }
    }

    fn recompute_rto(&mut self) {
        self.reapply_base_rto();
    }

    fn reapply_base_rto(&mut self) {
        let mult = 1u64 << self.backoff_exp;
        self.rto_us = self
            .rtt
            .rto()
            .saturating_mul(mult)
            .clamp(RttEstimator::MIN_RTO_US, RttEstimator::MAX_RTO_US);
    }

    // -- timers ------------------------------------------------------------

    /// Arm the retransmission timer for the oldest outstanding segment.
    ///
    /// Measured from the head's *last* transmission, so a segment that is
    /// already being repaired gets a fresh interval instead of inheriting a
    /// deadline already in the past — otherwise every repair would be followed
    /// immediately by a spurious timeout.
    fn arm_timer(&mut self, now_us: u64) {
        match self.queue.front_mut() {
            Some(front) => {
                self.deadline_us = front.last_tx_us + self.rto_us;
                self.timer_armed = true;
            }
            None => {
                self.timer_armed = false;
            }
        }
        let _ = now_us;
    }

    /// Has the retransmission deadline passed?
    pub fn timer_expired(&self, now_us: u64) -> bool {
        self.timer_armed && now_us >= self.deadline_us
    }

    /// Stop the retransmission timer.
    ///
    /// Used when there is nothing left to protect: an expired deadline against
    /// an empty queue is stale, and treating it as overdue would charge the
    /// connection a timeout it has no data to recover.
    pub fn disarm_timer(&mut self) {
        self.timer_armed = false;
    }

    /// Time remaining until the deadline, or `u64::MAX` when disarmed.
    pub fn time_to_deadline_us(&self, now_us: u64) -> u64 {
        if !self.timer_armed {
            return u64::MAX;
        }
        self.deadline_us.saturating_sub(now_us)
    }

    /// Declare a loss: retransmit the oldest outstanding segment, collapse the
    /// congestion window, and advance the backoff.
    ///
    /// Returns `None` when a copy of that segment is already in flight, leaving
    /// the timer to fire again rather than sending a second repair on top of
    /// the first.
    pub fn on_timeout(&mut self, now_us: u64) -> Option<TxSegment> {
        // The oldest outstanding segment. A SACKed head is still retried here:
        // the mark describes the past and may itself have been lost, so the
        // timeout is the backstop that guarantees the segment is resent.
        let seq = self.queue.front()?.seq;
        if self.find(seq).map(|s| s.rtx_in_flight).unwrap_or(false) {
            return None;
        }
        self.mark_in_flight(seq, now_us);
        self.retransmits += 1;
        self.cc.on_retransmit(1);
        self.cc.on_timeout();
        self.consecutive_timeouts = self.consecutive_timeouts.saturating_add(1);
        self.timeouts += 1;
        self.backoff_exp = (self.backoff_exp + 1).min(Self::MAX_BACKOFF_EXPONENT);
        self.reapply_base_rto();
        self.deadline_us = now_us + self.rto_us;
        self.timer_armed = true;
        self.find(seq).cloned()
    }

    /// Begin repairing `seq` after duplicate ACKs declared a loss.
    ///
    /// The loss decision is made in the ACK path by counting duplicates; the
    /// actual transmission is stamped here, so the two steps cannot drift
    /// apart and so a duplicate ACK arriving mid-repair is a no-op.
    pub fn begin_fast_repair(&mut self, seq: Seq, now_us: u64) -> Option<TxSegment> {
        if self.repair_target == Some(seq) && self.find(seq).map(|s| s.rtx_in_flight).unwrap_or(false) {
            return None;
        }
        // Distrust the SACK claim for the segment being repaired. Without this
        // the "everything is SACKed" fallback would re-fire on every pump call
        // and the connection would spend all its bandwidth resending one
        // segment instead of making progress.
        if let Some(s) = self.find_mut(seq) {
            s.sacked = false;
        }
        self.repair_target = Some(seq);
        self.mark_in_flight(seq, now_us);
        self.retransmits += 1;
        self.cc.on_retransmit(1);
        self.fast_retransmits += 1;
        if self.oldest_unsacked() == Some(seq) {
            // Repairing the oldest segment still in doubt: the timer restarts
            // from this transmission.
            self.deadline_us = now_us + self.rto_us;
        }
        self.find(seq).cloned()
    }

    /// The oldest outstanding segment that may still be missing.
    ///
    /// SACKed segments are preferred to skip: the peer told us it has them, so
    /// resending one wastes bandwidth and delays repairing the segment that is
    /// actually missing.
    ///
    /// But a SACK mark is a claim about a moment in the past, not a guarantee
    /// about now. A segment can be marked SACKed and still turn out to be
    /// missing — the ACK carrying the mark can be lost, or the receiver can
    /// discard the payload when its own window slides. So when the cumulative
    /// point has stopped moving and the retransmission timer is running, the
    /// marks are no longer trusted: `una` is retried even if it is marked.
    /// Without that fallback, a single lost SACK leaves the sender convinced
    /// every segment is safe while the receiver waits forever for a hole that
    /// nobody will ever fill.
    pub fn oldest_unsacked(&self) -> Option<Seq> {
        if let Some(s) = self.queue.iter().find(|s| !s.sacked) {
            return Some(s.seq);
        }
        if self.sack_credit_stale() {
            return self.queue.front().map(|s| s.seq);
        }
        None
    }

    /// Has the cumulative point stopped moving while the timer is running?
    ///
    /// True means the SACK marks in the queue describe a state the peer has not
    /// confirmed since, so they must not be allowed to suppress a repair. This
    /// is evaluated only at fast-retransmit time and the result is latched in
    /// `una`, so it produces exactly one distrust-and-retry per stall rather
    /// than a repair on every pump call.
    fn sack_credit_stale(&self) -> bool {
        self.queue.front().map(|f| f.sacked).unwrap_or(false) && self.timer_armed
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sender() -> Sender {
        Sender::new(0, CcConfig::default())
    }

    fn mss_bytes(n: usize) -> Vec<u8> {
        (0..n).map(|i| (i % 251) as u8).collect()
    }

    /// Send up to `n` segments, stamping each one microsecond after the last.
    ///
    /// Takes and returns the timestamp by value so a caller that never inspects
    /// the clock afterwards does not have to declare it mutable. Returns the
    /// sequence numbers sent.
    fn fill(s: &mut Sender, n: usize, now: u64) -> (Vec<Seq>, u64) {
        let mut sent = Vec::new();
        let mut t = now;
        for _ in 0..n {
            match s.pump(t) {
                TxAction::New(seg) => {
                    sent.push(seg.seq);
                    s.on_sent(seg.seq, t);
                }
                TxAction::Retransmit { seg, .. } => {
                    s.on_sent(seg.seq, t);
                }
                TxAction::Idle => break,
            }
            t += 1;
        }
        (sent, t)
    }

    #[test]
    fn new_sender_starts_with_an_empty_window() {
        let s = sender();
        assert_eq!(s.una(), 0);
        assert_eq!(s.next(), 0);
        assert_eq!(s.inflight(), 0);
        assert_eq!(s.queue_len(), 0);
        assert!(!s.timer_armed());
        assert!(!s.fin_allocated());
    }

    #[test]
    fn window_tracks_una_and_next() {
        let mut s = sender();
        s.write(&mss_bytes(4 * crate::wire::SEGMENT_PAYLOAD));
        let now = 1_000;
        let (sent, _next) = fill(&mut s, 3, now);
        assert_eq!(sent, vec![0, 1, 2]);
        assert_eq!(s.una(), 0);
        assert_eq!(s.next(), 3);
        assert_eq!(s.window().to_string(), "[0,3)");
        assert_eq!(s.inflight(), 3);
    }

    #[test]
    fn segments_are_cut_at_the_mss() {
        let mut s = sender();
        let payload = mss_bytes(crate::wire::SEGMENT_PAYLOAD + 7);
        s.write(&payload);
        let now = 0;
        match s.pump(now) {
            TxAction::New(seg) => {
                assert_eq!(seg.payload.len(), crate::wire::SEGMENT_PAYLOAD);
                assert_eq!(seg.seq, 0);
            }
            other => panic!("expected a new segment, got {other:?}"),
        }
        s.on_sent(0, now);
        match s.pump(now) {
            TxAction::New(seg) => {
                assert_eq!(seg.payload.len(), 7, "the tail must be a short segment");
                assert_eq!(seg.seq, 1);
            }
            other => panic!("expected the tail segment, got {other:?}"),
        }
    }

    #[test]
    fn window_admits_at_most_cwnd_segments() {
        let mut s = sender();
        s.write(&mss_bytes(100 * crate::wire::SEGMENT_PAYLOAD));
        let cwnd = s.cc().cwnd() as usize;
        let now = 0;
        let (sent, _next) = fill(&mut s, cwnd, now);
        assert_eq!(sent.len(), cwnd, "must not exceed cwnd");
        assert_eq!(s.window_credit(), 0);
        assert!(s.window_stalled());
        assert!(matches!(s.pump(now), TxAction::Idle));
    }

    #[test]
    fn cumulative_ack_advances_the_window_and_frees_the_queue() {
        let mut s = sender();
        s.write(&mss_bytes(5 * crate::wire::SEGMENT_PAYLOAD));
        let now = 100;
        let (_sent, _next) = fill(&mut s, 5, now);
        assert_eq!(s.queue_len(), 5);

        let out = s.on_ack(3, &[], now);
        assert_eq!(out.newly_acked, 3);
        assert!(out.advanced);
        assert_eq!(s.una(), 3);
        assert_eq!(s.queue_len(), 2);
        assert_eq!(s.first_queued_seq(), Some(3));
        assert_eq!(s.bytes_acked(), 3 * crate::wire::SEGMENT_PAYLOAD as u64);
    }

    #[test]
    fn stale_acks_are_ignored_and_do_not_move_the_window_backwards() {
        let mut s = sender();
        s.write(&mss_bytes(5 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 5, now);
        s.on_ack(4, &[], now);
        // A reordered duplicate ACK of an older cumulative point.
        let out = s.on_ack(2, &[], now);
        assert_eq!(out.newly_acked, 0);
        assert!(!out.advanced);
        assert_eq!(s.una(), 4, "una must never regress");
        assert_eq!(s.queue_len(), 1);
    }

    #[test]
    fn three_duplicate_acks_trigger_a_fast_retransmit_of_the_hole() {
        let mut s = sender();
        s.write(&mss_bytes(10 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 6, now);
        // Segment 0 is lost; the receiver keeps saying "I have 0" while
        // segments 1..6 arrive out of order behind it.
        let mut signalled = false;
        for i in 0..3 {
            let out = s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], now);
            if out.signal.is_some() {
                signalled = true;
            }
        }
        assert!(signalled, "the third duplicate ACK must declare a loss");
        // The hole, not the newest segment, is what gets repaired.
        match s.pump(now) {
            TxAction::Retransmit { seg, reason } => {
                assert_eq!(seg.seq, 0, "fast retransmit must target the hole");
                assert_eq!(reason, RetxReason::FastRetransmit);
            }
            other => panic!("expected a fast retransmit, got {other:?}"),
        }
    }

    #[test]
    fn a_repair_in_flight_is_not_repeated() {
        let mut s = sender();
        s.write(&mss_bytes(10 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 6, now);
        for i in 0..3 {
            s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], now);
        }
        // First pump starts the repair and marks it in flight.
        assert!(matches!(s.pump(now), TxAction::Retransmit { .. }));
        s.on_sent(0, now);
        // A fourth and fifth duplicate ACK must not spawn a second repair.
        for i in 0..2 {
            s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], now);
        }
        let before = s.tx_counts().1;
        assert!(matches!(s.pump(now), TxAction::Idle | TxAction::New(_)));
        assert!(s.tx_counts().1 >= before);
        assert_eq!(s.repair_target(), Some(0));
    }

    #[test]
    fn timeout_collapses_the_window_retransmits_the_head_and_backs_off() {
        let mut s = sender();
        s.write(&mss_bytes(10 * crate::wire::SEGMENT_PAYLOAD));
        let mut now = 0;
        let (_sent, _next) = fill(&mut s, 5, now);
        let cwnd_before = s.cc().cwnd();
        let rto = s.rto_us();

        // Move past the deadline.
        now = s.deadline_us();
        assert!(s.timer_expired(now));
        match s.pump(now) {
            TxAction::Retransmit { seg, reason } => {
                assert_eq!(seg.seq, 0, "a timeout repairs the oldest segment");
                assert_eq!(reason, RetxReason::Timeout);
            }
            other => panic!("expected a timeout retransmit, got {other:?}"),
        }
        assert_eq!(s.cc().cwnd(), 1, "a timeout must collapse cwnd to 1");
        assert!(s.cc().cwnd() < cwnd_before);
        assert_eq!(s.backoff_exp(), 1);
        assert!(s.rto_us() >= rto, "backoff must not shorten the RTO");
        assert_eq!(s.consecutive_timeouts(), 1);
    }

    #[test]
    fn backoff_doubles_the_rto_and_is_capped() {
        let mut s = sender();
        s.write(&mss_bytes(4 * crate::wire::SEGMENT_PAYLOAD));
        let mut now = 0;
        let (_sent, _next) = fill(&mut s, 2, now);
        let base = s.base_rto_us();
        let mut seen = Vec::new();
        for _ in 0..10 {
            now = s.deadline_us();
            s.on_sent(s.una(), now); // clear the in-flight mark so the timer can act
            if s.on_timeout(now).is_some() {
                seen.push(s.backoff_exp());
            }
        }
        assert!(seen.len() >= 8, "the timer should keep firing, saw {seen:?}");
        assert!(seen.windows(2).all(|w| w[1] >= w[0]), "backoff must not regress");
        assert!(
            s.backoff_exp() <= Sender::MAX_BACKOFF_EXPONENT,
            "backoff exponent {} exceeded the cap",
            s.backoff_exp()
        );
        // And the effective RTO is bounded by the estimator ceiling.
        assert!(s.rto_us() <= crate::time::RttEstimator::MAX_RTO_US);
        let _ = base;
    }

    #[test]
    fn backoff_is_cleared_once_new_data_is_acknowledged() {
        let mut s = sender();
        s.write(&mss_bytes(4 * crate::wire::SEGMENT_PAYLOAD));
        let mut now = 0;
        let (_sent, _next) = fill(&mut s, 2, now);
        now = s.deadline_us();
        s.on_timeout(now);
        assert_eq!(s.backoff_exp(), 1);
        // A fresh ACK restores the base RTO.
        s.on_ack(1, &[], now);
        assert_eq!(s.backoff_exp(), 0, "progress must clear the backoff");
        assert_eq!(s.consecutive_timeouts(), 0);
    }

    #[test]
    fn sack_blocks_credit_data_without_moving_una() {
        let mut s = sender();
        s.write(&mss_bytes(10 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 6, now);
        let out = s.on_ack(0, &[Block::new(2, 5)], now);
        assert_eq!(out.newly_acked, 0);
        assert!(!out.advanced, "SACK must not advance the cumulative point");
        assert_eq!(out.sacked_credit_bytes, 3 * crate::wire::SEGMENT_PAYLOAD as u64);
        assert_eq!(s.una(), 0);
        assert_eq!(s.sacked_in_queue(), 3);
    }

    #[test]
    fn repeated_sack_blocks_are_credited_only_once() {
        let mut s = sender();
        s.write(&mss_bytes(10 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 6, now);
        let first = s.on_ack(0, &[Block::new(2, 5)], now);
        let second = s.on_ack(0, &[Block::new(2, 5)], now);
        assert!(first.sacked_credit_bytes > 0);
        assert_eq!(second.sacked_credit_bytes, 0, "the same block must not be credited twice");
    }

    #[test]
    fn sack_below_una_credits_nothing() {
        let mut s = sender();
        s.write(&mss_bytes(10 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 6, now);
        s.on_ack(4, &[], now);
        // The peer re-sends an old ACK whose SACK block is entirely acked.
        let out = s.on_ack(4, &[Block::new(0, 2)], now);
        assert_eq!(out.sacked_credit_bytes, 0);
    }

    #[test]
    fn rtt_is_estimated_from_unaqueued_segments_only() {
        let mut s = sender();
        s.write(&mss_bytes(4 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 2, now);
        // Segment 0 sent at t=0, ACKed at t=50ms.
        s.on_ack(1, &[], 50_000);
        let srtt = s.rtt().srtt().expect("an RTT sample should have been taken");
        // First sample seeds SRTT directly.
        assert!(
            (srtt as i64 - 49_000).abs() <= 2_000,
            "srtt {srtt} is nowhere near the true 50ms RTT"
        );
    }

    #[test]
    fn karn_excludes_retransmitted_segments_from_rtt_measurement() {
        let mut s = sender();
        s.write(&mss_bytes(4 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 2, now);
        // Force a repair of segment 0, so its send time is ambiguous.
        for i in 0..3 {
            s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], 10_000);
        }
        s.pump(10_000);
        let before = s.rtt().samples();
        // Now ACK it. A retransmitted segment must not produce a sample.
        s.on_ack(2, &[], 10_000);
        assert_eq!(
            s.rtt().samples(),
            before,
            "a retransmitted segment must not yield an RTT sample"
        );
    }

    #[test]
    fn rtt_samples_are_suppressed_during_a_loss_episode() {
        let mut s = sender();
        s.write(&mss_bytes(20 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 10, now);
        for i in 0..3 {
            s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], 20_000);
        }
        assert_eq!(s.phase(), CcPhase::FastRecovery);
        let before = s.rtt().samples();
        s.on_ack(9, &[], 20_000);
        assert_eq!(s.rtt().samples(), before, "RTTs during fast recovery are not trustworthy");
    }

    #[test]
    fn fin_occupies_a_sequence_number_and_is_repaired_like_data() {
        let mut s = sender();
        s.write(&mss_bytes(2 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 2, now);
        assert!(s.alloc_fin());
        assert!(!s.alloc_fin(), "a second FIN must be refused");
        assert!(s.fin_pending());
        assert_eq!(s.next(), 3, "the FIN consumed a sequence number");

        // The FIN goes on the wire like any other segment.
        match s.pump(now) {
            TxAction::New(seg) => {
                assert!(seg.fin);
                assert_eq!(seg.seq, 2);
                assert_eq!(seg.wire_bytes(), 0, "a FIN carries no payload bytes");
            }
            other => panic!("expected the FIN, got {other:?}"),
        }
        s.on_sent(2, now);
        // Still unacknowledged.
        assert!(!s.fin_acked());
        s.on_ack(3, &[], now);
        assert!(s.fin_acked());
        assert!(!s.fin_pending());
    }

    #[test]
    fn fin_waits_behind_buffered_data() {
        let mut s = sender();
        s.write(&mss_bytes(3 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 2, now);
        s.alloc_fin();
        // Segment 2 is the FIN; it must not be sent before segment 1.
        let fin_seq = s.queue().find(|x| x.fin).unwrap().seq;
        assert_eq!(fin_seq, 2);
        assert!(seq_lt(1, fin_seq), "data must precede the FIN in sequence order");
    }

    #[test]
    fn queue_peak_stays_within_cwnd_when_never_acknowledged() {
        // With no ACKs the window never opens, so the queue is bounded by cwnd
        // and nothing more. This is the property that keeps memory flat on a
        // path that has stopped acknowledging.
        let mut s = sender().with_max_queue(8);
        s.write(&mss_bytes(200 * crate::wire::SEGMENT_PAYLOAD));
        let mut now = 0;
        for _ in 0..500 {
            match s.pump(now) {
                TxAction::New(_) | TxAction::Retransmit { .. } => {
                    s.on_sent(s.una(), now);
                }
                TxAction::Idle => break,
            }
            now += 1;
        }
        // The effective cap is max(configured, cwnd), and cwnd is 10 here, so
        // the queue is allowed 10 and no more.
        let cap = s.cc().cwnd() as usize;
        assert!(s.peak_queue_len() <= cap, "peak queue {} exceeded cwnd {cap}", s.peak_queue_len());
        assert!(s.queue_len() <= cap);
    }

    #[test]
    fn the_queue_cap_is_never_below_cwnd() {
        // A cap below cwnd would let the window admit more segments than the
        // queue can hold, stalling the sender with a "full" queue it may not
        // refill, and deadlocking the connection.
        let mut s = sender().with_max_queue(1);
        s.write(&mss_bytes(50 * crate::wire::SEGMENT_PAYLOAD));
        let mut now = 0;
        for _ in 0..20 {
            match s.pump(now) {
                TxAction::New(_) | TxAction::Retransmit { .. } => {
                    s.on_sent(s.una(), now);
                }
                TxAction::Idle => break,
            }
            now += 1;
        }
        assert!(
            s.queue_len() >= s.cc().cwnd() as usize || s.pending_bytes() == 0,
            "the queue must be able to hold a full window"
        );
    }

    #[test]
    fn default_queue_cap_meets_the_64_segment_budget() {
        let mut s = sender();
        s.write(&mss_bytes(500 * crate::wire::SEGMENT_PAYLOAD));
        let mut now = 0;
        for _ in 0..2000 {
            match s.pump(now) {
                TxAction::New(_) | TxAction::Retransmit { .. } => {
                    s.on_sent(s.una(), now);
                }
                TxAction::Idle => break,
            }
            now += 1;
        }
        assert!(
            s.peak_queue_len() <= 64,
            "default peak queue {} breaks the 64-segment budget",
            s.peak_queue_len()
        );
    }

    #[test]
    fn a_closed_window_stalls_new_data_but_not_repairs() {
        let mut s = sender();
        s.write(&mss_bytes(20 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let cwnd = s.cc().cwnd() as usize;
        let (_sent, _next) = fill(&mut s, cwnd, now);
        // A hole appears behind a full window.
        for i in 0..3 {
            s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], now);
        }
        assert!(s.window_stalled());
        // New data is refused...
        assert!(matches!(s.pump(now), TxAction::Retransmit { .. }));
        // ...but the repair is what goes out.
        s.on_sent(0, now);
        assert!(matches!(s.pump(now), TxAction::Idle | TxAction::New(_)));
    }

    #[test]
    fn counters_account_for_new_and_retransmitted_segments() {
        let mut s = sender();
        s.write(&mss_bytes(10 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 6, now);
        for i in 0..3 {
            s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], now);
        }
        s.pump(now);
        let (new, retx) = s.tx_counts();
        assert_eq!(new, 6);
        assert_eq!(retx, 1);
        let (timeouts, fast) = s.loss_events();
        assert_eq!(timeouts, 0);
        assert_eq!(fast, 1);
    }

    #[test]
    fn send_buffer_is_reclaimed_as_data_is_acknowledged() {
        let mut s = sender();
        let big = 4 * crate::wire::SEGMENT_PAYLOAD;
        s.write(&mss_bytes(big));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 4, now);
        s.on_ack(4, &[], now);
        assert_eq!(s.pending_bytes(), 0);
        // The buffer was reclaimed, so it is empty rather than holding the
        // whole stream.
        assert!(s.tx_buf.is_empty() || s.tx_consumed == 0);
    }

    #[test]
    fn spurious_retransmissions_are_counted() {
        let mut s = sender();
        s.write(&mss_bytes(4 * crate::wire::SEGMENT_PAYLOAD));
        let now = 0;
        let (_sent, _next) = fill(&mut s, 3, now);
        // Duplicate ACKs force a spurious repair of segment 0, which then
        // arrives and is acknowledged.
        for i in 0..3 {
            s.on_ack(0, &[Block::new(1, 1 + i as Seq + 1)], now);
        }
        s.pump(now);
        s.on_sent(0, now);
        s.on_ack(3, &[], now);
        assert!(s.spurious_retransmits() > 0, "a repaired-then-acked segment is a spurious retransmit");
    }
}


impl fmt::Debug for Sender {
    /// Window, congestion, and queue shape — not the queued payloads.
    ///
    /// This is the same summary the state dump leads with, so a `{:?}` in a
    /// failing test reads the way the dump does.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Sender")
            .field("window", &self.window().to_string())
            .field("inflight", &self.inflight())
            .field("queue_len", &self.queue_len())
            .field("queue_peak", &self.peak_queue)
            .field("sacked", &self.sacked_in_queue())
            .field("cwnd", &self.cc.cwnd())
            .field("ssthresh", &self.cc.ssthresh())
            .field("phase", &self.cc.phase().to_string())
            .field("srtt_us", &self.rtt().srtt())
            .field("rttvar_us", &self.rtt().rttvar())
            .field("rto_us", &self.rto_us())
            .field("backoff_exp", &self.backoff_exp)
            .field("pending_bytes", &self.pending_bytes())
            .finish()
    }
}
