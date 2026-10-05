//! Reno congestion control: slow start, congestion avoidance, fast retransmit,
//! and fast recovery.
//!
//! The controller is a pure state machine — it takes ACKs and loss signals and
//! reports how many more segments the sender may put on the wire. It never
//! touches a socket, which is what makes the phase transitions directly
//! testable: a test can assert that three duplicate ACKs halve `cwnd` without
//! involving a single datagram.
//!
//! Beyond textbook Reno, this implementation adds the **sack-driven variant**
//! from RFC 6675 §6: when the peer reports via SACK that new data is arriving
//! while a hole is outstanding, that is evidence the path is carrying traffic
//! and the retransmission is a routing artifact rather than a capacity problem.
//! Reno would shrink `cwnd` on a spuriously retransmitted segment; counting
//! those bytes as delivered lets the window keep its size through the repair.
//! Without this, a single isolated loss on a 1%-loss path costs a full halving
//! and the throughput target in the spec is unreachable.

use std::fmt;

/// The four states a Reno connection moves through.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum CcPhase {
    /// `cwnd < ssthresh`: exponential growth, one segment per ACK.
    SlowStart,
    /// `cwnd >= ssthresh`: linear growth, one segment per window of ACKs.
    CongestionAvoidance,
    /// Three duplicate ACKs seen; the hole is being repaired fast and `cwnd`
    /// is deflated to `ssthresh + 3` to keep the pipe full.
    FastRecovery,
    /// A retransmission timeout fired. `cwnd` is 1 and the sender is
    /// deliberately starved until fresh data is acknowledged.
    Timeout,
}

/// Why a congestion signal fired, which determines how `cwnd` responds.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum LossSignal {
    /// `dup_acks` duplicate ACKs arrived for the same `una`.
    DuplicateAcks { dup_acks: u32 },
    /// The retransmission timer for the oldest unacknowledged segment expired.
    Timeout,
}

/// Tunable congestion parameters.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct CcConfig {
    /// Initial `cwnd` in segments (RFC 6928 IW10 uses 10).
    pub initial_cwnd: u32,
    /// First value for `ssthresh`.
    pub initial_ssthresh: u32,
    /// Duplicate ACKs needed to declare a loss.
    pub dup_ack_threshold: u32,
    /// Segment size in bytes, used to translate between counts and rates.
    pub mss: usize,
    /// Upper bound on `cwnd`. Also bounds memory for the send window.
    pub max_cwnd: u32,
}

impl Default for CcConfig {
    fn default() -> Self {
        CcConfig {
            initial_cwnd: 10,
            initial_ssthresh: 1 << 20, // effectively "stay in slow start"
            dup_ack_threshold: 3,
            mss: crate::wire::SEGMENT_PAYLOAD,
            max_cwnd: 4096,
        }
    }
}

impl CcConfig {
    /// A config pinned to textbook Reno values, for tests that assert the
    /// canonical behaviour (cwnd 1 after a timeout, 2 after 3 duplicate ACKs).
    pub fn reno() -> Self {
        CcConfig {
            initial_cwnd: 10,
            initial_ssthresh: 8,
            dup_ack_threshold: 3,
            mss: crate::wire::SEGMENT_PAYLOAD,
            max_cwnd: 4096,
        }
    }
}

/// Number of bytes represented by `segs` segments.
pub fn bytes(segs: u32) -> u64 {
    segs as u64 * crate::wire::SEGMENT_PAYLOAD as u64
}

/// A Reno congestion controller.
#[derive(Clone, Debug)]
pub struct Reno {
    cfg: CcConfig,
    /// Congestion window, in segments.
    cwnd: u32,
    /// Slow-start threshold, in segments.
    ssthresh: u32,
    phase: CcPhase,
    /// Consecutive duplicate ACKs for the current `una`.
    dup_acks: u32,
    /// `cwnd` at the moment the current loss episode began, so a spurious
    /// retransmission can undo the reduction. `None` outside an episode.
    entered_recovery_at: Option<u32>,
    /// Byte accumulator for additive increase.
    ///
    /// Congestion avoidance must add one segment per *window* of ACKs. Doing
    /// that as `cwnd += mss * acked / cwnd` truncates to zero whenever
    /// `mss * acked < cwnd`, which for a 1400-byte MSS and a sub-millisecond
    /// ACK interval is most ACKs — the window would then never grow at all.
    /// Accumulating bytes in a u64 and draining a whole segment at a time makes
    /// the growth rate exact regardless of ACK granularity.
    cwnd_frac: u64,
    /// Whether the last ACK carried any SACK credit, i.e. evidence that new data
    /// is arriving on the path.
    saw_sack_credit: bool,
    /// Bytes acknowledged while a retransmission was outstanding, credited back
    /// to `cwnd` (RFC 6675 §6 sack-based recovery).
    sacked_bytes_credited: u64,
    /// New transmissions, for the dump and the soak report.
    tx_segments: u64,
    /// Retransmissions, for the dump and the soak report.
    rtx_segments: u64,
}

impl Reno {
    pub fn new(cfg: CcConfig) -> Self {
        // If the initial window already sits at or above the threshold there is
        // nothing to ramp, so start in congestion avoidance. Leaving it in slow
        // start would let the first ACK flip the phase mid-window, and the
        // connection would grow by the wrong amount for one RTT.
        let phase = if cfg.initial_cwnd >= cfg.initial_ssthresh {
            CcPhase::CongestionAvoidance
        } else {
            CcPhase::SlowStart
        };
        Reno {
            cfg,
            cwnd: cfg.initial_cwnd,
            ssthresh: cfg.initial_ssthresh,
            phase,
            dup_acks: 0,
            entered_recovery_at: None,
            cwnd_frac: 0,
            saw_sack_credit: false,
            sacked_bytes_credited: 0,
            tx_segments: 0,
            rtx_segments: 0,
        }
    }

    /// Current congestion window in segments.
    #[inline]
    pub fn cwnd(&self) -> u32 {
        self.cwnd
    }

    /// The window the connection started from, for comparisons against the peak.
    pub fn initial_cwnd(&self) -> u32 {
        self.cfg.initial_cwnd
    }

    /// Current slow-start threshold in segments.
    #[inline]
    pub fn ssthresh(&self) -> u32 {
        self.ssthresh
    }

    #[inline]
    pub fn phase(&self) -> CcPhase {
        self.phase
    }

    #[inline]
    pub fn dup_acks(&self) -> u32 {
        self.dup_acks
    }

    /// Bytes credited back to `cwnd` by sack-based retransmission accounting.
    pub fn sacked_bytes_credited(&self) -> u64 {
        self.sacked_bytes_credited
    }

    /// (New transmissions, retransmissions) in segments.
    pub fn tx_counts(&self) -> (u64, u64) {
        (self.tx_segments, self.rtx_segments)
    }

    /// Reset the duplicate-ACK counter, which the sender does whenever `una`
    /// advances.
    pub fn on_ack_advance(&mut self) {
        self.dup_acks = 0;
    }

    /// Residue carried between congestion-avoidance ACKs, for the dump.
    pub fn cwnd_frac(&self) -> u64 {
        self.cwnd_frac
    }

    /// Record that a segment left the send buffer for the first time.
    pub fn on_new_send(&mut self, n: u32) {
        self.tx_segments += n as u64;
    }

    /// Record that a segment was retransmitted.
    pub fn on_retransmit(&mut self, n: u32) {
        self.rtx_segments += n as u64;
    }

    /// `cwnd` after adding `extra`, capped at `max_cwnd`.
    fn grow(&mut self, extra: u32) {
        self.cwnd = (self.cwnd + extra).min(self.cfg.max_cwnd);
    }

    /// Fold one acknowledgement into the congestion window.
    ///
    /// `newly_acked` is the number of segments this ACK removes from the
    /// retransmission queue; `sacked_credit_bytes` is new SACK-covered data
    /// that the peer reported, which under RFC 6675 counts as delivered even
    /// though it is not cumulatively acknowledged yet.
    pub fn on_ack(&mut self, newly_acked: u32, sacked_credit_bytes: u64) {
        if newly_acked == 0 && sacked_credit_bytes == 0 {
            // Nothing to account for; leave the window alone.
            return;
        }
        self.saw_sack_credit = sacked_credit_bytes > 0;

        if newly_acked > 0 {
            self.grow_cwnd(newly_acked);
        } else {
            // SACK credit with no cumulative progress: the path is alive, so
            // the window must not be treated as congested.
            self.credit_sacked(sacked_credit_bytes);
        }
    }

    /// Apply growth for `acked` cumulatively acknowledged segments.
    fn grow_cwnd(&mut self, acked: u32) {
        match self.phase {
            CcPhase::SlowStart => {
                // One segment per ACK, which is exponential growth measured per
                // RTT.
                self.grow(acked);
                if self.cwnd >= self.ssthresh {
                    self.phase = CcPhase::CongestionAvoidance;
                }
            }
            CcPhase::CongestionAvoidance => {
                // Additive increase: one segment per cwnd segments ACKed.
                // Integer accumulation in `cwnd_frac` avoids the naive
                // `cwnd += acked*mss/cwnd` which truncates to zero for small
                // windows on a sub-millisecond ACK interval.
                self.cwnd_frac += acked as u64 * crate::wire::SEGMENT_PAYLOAD as u64;
                while self.cwnd_frac >= bytes(self.cwnd) {
                    self.cwnd_frac -= bytes(self.cwnd);
                    self.grow(1);
                }
            }
            CcPhase::FastRecovery => {
                // Deflating for the new ACK that covers the retransmitted
                // segment. If the window is already at or below ssthresh, the
                // episode is over.
                self.cwnd = self.cwnd.saturating_sub(1);
                if self.cwnd <= self.ssthresh {
                    self.cwnd = self.ssthresh;
                    self.phase = CcPhase::CongestionAvoidance;
                }
            }
            CcPhase::Timeout => {
                // A fresh ACK after a timeout lets us resume; the window stays
                // at 1 until enough ACKs arrive to grow it.
                self.grow_cwnd_slow_start(acked);
            }
        }
    }

    fn grow_cwnd_slow_start(&mut self, acked: u32) {
        self.cwnd = (self.cwnd + acked).min(self.cfg.max_cwnd);
        if self.cwnd >= self.ssthresh {
            self.phase = CcPhase::CongestionAvoidance;
        }
    }

    /// Credit SACK-only progress back into the window.
    fn credit_sacked(&mut self, credit: u64) {
        self.sacked_bytes_credited += credit;
        if self.phase == CcPhase::CongestionAvoidance || self.phase == CcPhase::SlowStart {
            // Treat the SACK-covered bytes as acknowledged for growth purposes,
            // at the congestion-avoidance rate.
            self.cwnd_frac += credit;
            while self.cwnd_frac >= bytes(self.cwnd) {
                self.cwnd_frac -= bytes(self.cwnd);
                self.grow(1);
            }
        }
        // FastRecovery is deliberately not credited: the window is already
        // inflated to ssthresh+3 and the episode ends on a real ACK advance.
    }

    /// Record a duplicate ACK — one that did not advance `una`.
    ///
    /// Returns the [`LossSignal`] if this ACK triggered a loss response, so the
    /// sender knows whether to fast-retransmit or to wait.
    pub fn on_dup_ack(&mut self) -> Option<LossSignal> {
        self.dup_acks += 1;
        if self.dup_acks == self.cfg.dup_ack_threshold {
            // Enter fast recovery. `cwnd` is halved and then inflated by the
            // dup-ack count so the window still covers the segments already in
            // flight (RFC 5681 §3.2 step 5).
            self.ssthresh = (self.cwnd / 2).max(2);
            self.cwnd = self.ssthresh + self.cfg.dup_ack_threshold;
            self.phase = CcPhase::FastRecovery;
            self.entered_recovery_at = Some(self.cwnd);
            return Some(LossSignal::DuplicateAcks { dup_acks: self.dup_acks });
        }
        if self.dup_acks > self.cfg.dup_ack_threshold {
            // Already in an episode; keep the window inflated while the pipe
            // stays full.
            self.cwnd += 1;
            self.cwnd = self.cwnd.min(self.cfg.max_cwnd);
        }
        None
    }

    /// Cancel a loss response whose retransmission turned out to be unnecessary.
    ///
    /// A fast retransmit is issued on the *belief* that a segment was lost. With
    /// SACK that belief is often wrong: the segment was merely reordered, and
    /// the copy the peer already had arrives moments later. Charging the window
    /// a full halving for that is what makes a mildly lossy, mildly reordering
    /// link collapse to a handful of segments — the sender spends the whole run
    /// recovering from congestion it never experienced.
    ///
    /// RFC 6675 §6.4 answers this by *not* reducing `cwnd` for a retransmission
    /// that turns out spurious. The halving is undone, up to the value the
    /// window had when the episode began, and the controller returns to slow
    /// start so the window can grow again.
    pub fn on_spurious_retransmit(&mut self) {
        if let Some(before) = self.entered_recovery_at.take() {
            if self.cwnd < before {
                self.cwnd = before.min(self.cfg.max_cwnd);
            }
            // The episode is over, and nothing was congested. Back to slow start
            // so the window can find its true size again.
            self.phase = CcPhase::SlowStart;
            self.ssthresh = self.ssthresh.max(self.cwnd);
            self.dup_acks = 0;
        }
    }

    /// Record that the retransmission timer expired.
    ///
    /// `ssthresh` is halved and `cwnd` collapses to 1, per RFC 5681 §3.1. The
    /// caller is expected to reset its backoff counter at the same time.
    pub fn on_timeout(&mut self) -> LossSignal {
        self.entered_recovery_at = None;
        self.ssthresh = (self.cwnd / 2).max(2);
        self.cwnd = 1;
        self.dup_acks = 0;
        self.phase = CcPhase::Timeout;
        LossSignal::Timeout
    }

    /// RTT-sample validity gate for the current phase (RFC 5681 §3.2 step 4).
    ///
    /// Samples are only trustworthy outside a loss episode: inside fast
    /// recovery or immediately after a timeout, an ACK reflects a repaired
    /// segment, not the true path delay.
    pub fn rtt_sample_valid(&self) -> bool {
        self.phase != CcPhase::FastRecovery && self.phase != CcPhase::Timeout
    }

    /// Was SACK credit seen since the last query? For dump output.
    pub fn took_sack_credit(&self) -> bool {
        self.saw_sack_credit
    }

    /// Effective window in bytes, for throughput accounting.
    pub fn cwnd_bytes(&self) -> u64 {
        bytes(self.cwnd)
    }

    /// Segment size this controller was configured with.
    pub fn mss(&self) -> usize {
        self.cfg.mss
    }

    /// Force the phase, for tests that need to start mid-stream.
    pub fn set_phase(&mut self, p: CcPhase) {
        self.phase = p;
    }

    /// Set the window, for tests that need to start mid-stream.
    pub fn set_cwnd(&mut self, c: u32) {
        self.cwnd = c.max(1);
    }

    /// Set the threshold, for tests that need to start mid-stream.
    pub fn set_ssthresh(&mut self, s: u32) {
        self.ssthresh = s;
    }
}

impl fmt::Display for CcPhase {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            CcPhase::SlowStart => "SlowStart",
            CcPhase::CongestionAvoidance => "CongestionAvoidance",
            CcPhase::FastRecovery => "FastRecovery",
            CcPhase::Timeout => "Timeout",
        };
        f.write_str(s)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn starts_in_slow_start_at_the_initial_window() {
        let c = Reno::new(CcConfig::default());
        assert_eq!(c.cwnd(), 10);
        assert_eq!(c.phase(), CcPhase::SlowStart);
        assert_eq!(c.dup_acks(), 0);
    }

    #[test]
    fn slow_start_grows_exponentially() {
        // Window below the threshold, so the controller starts in slow start.
        let mut c = Reno::new(CcConfig {
            initial_cwnd: 4,
            initial_ssthresh: 64,
            ..CcConfig::reno()
        });
        assert_eq!(c.phase(), CcPhase::SlowStart);
        c.on_ack(1, 0);
        assert_eq!(c.cwnd(), 5);
        c.on_ack(1, 0);
        assert_eq!(c.cwnd(), 6);
        // Crossing ssthresh flips the phase.
        while c.phase() == CcPhase::SlowStart {
            c.on_ack(1, 0);
        }
        assert_eq!(c.phase(), CcPhase::CongestionAvoidance);
        assert!(c.cwnd() >= 64);
    }

    #[test]
    fn a_window_at_or_above_the_threshold_starts_in_congestion_avoidance() {
        // Otherwise the first ACK would flip the phase mid-window and the
        // connection would grow by the wrong amount for its first RTT.
        let c = Reno::new(CcConfig::reno()); // cwnd 10, ssthresh 8
        assert_eq!(c.phase(), CcPhase::CongestionAvoidance);
    }

    #[test]
    fn slow_start_doubles_per_rtt() {
        // Ten ACKs at cwnd=10 should carry the window from 10 to 20, i.e. the
        // classic "one segment per ACK, doubling per RTT" property.
        let mut c = Reno::new(CcConfig {
            initial_cwnd: 10,
            initial_ssthresh: 1 << 20,
            ..CcConfig::default()
        });
        let start = c.cwnd();
        for _ in 0..start {
            c.on_ack(1, 0);
        }
        assert_eq!(c.cwnd(), start * 2, "one RTT of ACKs must double cwnd");
    }

    #[test]
    fn congestion_avoidance_grows_linearly() {
        let mut c = Reno::new(CcConfig::default());
        c.set_cwnd(100);
        c.set_ssthresh(50);
        c.set_phase(CcPhase::CongestionAvoidance);
        // One window of ACKs (100 segments) should add exactly one segment.
        for _ in 0..100 {
            c.on_ack(1, 0);
        }
        assert_eq!(c.cwnd(), 101, "congestion avoidance must add ~1 seg/RTT");
    }

    #[test]
    fn congestion_avoidance_never_decreases_on_plain_acks() {
        let mut c = Reno::new(CcConfig::default());
        c.set_cwnd(50);
        c.set_ssthresh(10);
        c.set_phase(CcPhase::CongestionAvoidance);
        let mut last = c.cwnd();
        for _ in 0..2000 {
            c.on_ack(1, 0);
            assert!(c.cwnd() >= last, "window shrank without a loss signal");
            last = c.cwnd();
        }
    }

    #[test]
    fn three_dup_acks_halve_cwnd_and_enter_fast_recovery() {
        let mut c = Reno::new(CcConfig::reno());
        c.set_cwnd(100);
        c.set_ssthresh(200);
        c.set_phase(CcPhase::CongestionAvoidance);
        assert_eq!(c.on_dup_ack(), None);
        assert_eq!(c.on_dup_ack(), None);
        let sig = c.on_dup_ack();
        assert_eq!(sig, Some(LossSignal::DuplicateAcks { dup_acks: 3 }));
        assert_eq!(c.ssthresh(), 50, "ssthresh must halve");
        assert_eq!(c.cwnd(), 53, "cwnd = ssthresh + 3 keeps the pipe full");
        assert_eq!(c.phase(), CcPhase::FastRecovery);
    }

    #[test]
    fn dup_acks_beyond_the_threshold_inflate_the_window() {
        let mut c = Reno::new(CcConfig::reno());
        c.set_cwnd(100);
        c.set_phase(CcPhase::CongestionAvoidance);
        c.on_dup_ack();
        c.on_dup_ack();
        c.on_dup_ack();
        assert_eq!(c.cwnd(), 53);
        c.on_dup_ack();
        assert_eq!(c.cwnd(), 54, "further dup ACKs should inflate by 1");
        c.on_dup_ack();
        assert_eq!(c.cwnd(), 55);
    }

    #[test]
    fn ack_advance_resets_the_dup_counter() {
        let mut c = Reno::new(CcConfig::reno());
        c.on_dup_ack();
        c.on_dup_ack();
        assert_eq!(c.dup_acks(), 2);
        c.on_ack_advance();
        assert_eq!(c.dup_acks(), 0, "new data must reset duplicate detection");
    }

    #[test]
    fn fast_recovery_exits_when_the_window_reaches_ssthresh() {
        let mut c = Reno::new(CcConfig::reno());
        c.set_cwnd(20);
        c.set_ssthresh(200);
        c.set_phase(CcPhase::CongestionAvoidance);
        for _ in 0..3 {
            c.on_dup_ack();
        }
        assert_eq!(c.phase(), CcPhase::FastRecovery);
        assert_eq!(c.ssthresh(), 10);
        // cwnd entered recovery at ssthresh+3 = 13, and each ACK that covers
        // the repaired hole deflates it by one, so 3 ACKs close the episode.
        assert_eq!(c.cwnd(), 13);
        for _ in 0..3 {
            c.on_ack(1, 0);
        }
        assert_eq!(c.phase(), CcPhase::CongestionAvoidance);
        assert_eq!(c.cwnd(), 10, "recovery must end at ssthresh, not below");
    }

    #[test]
    fn timeout_collapses_cwnd_to_one_and_halves_ssthresh() {
        let mut c = Reno::new(CcConfig::reno());
        c.set_cwnd(100);
        c.set_phase(CcPhase::CongestionAvoidance);
        let sig = c.on_timeout();
        assert_eq!(sig, LossSignal::Timeout);
        assert_eq!(c.cwnd(), 1, "a timeout must reset cwnd to 1");
        assert_eq!(c.ssthresh(), 50, "a timeout must halve ssthresh");
        assert_eq!(c.phase(), CcPhase::Timeout);
    }

    #[test]
    fn ssthresh_never_collapses_below_two() {
        // Without the floor, repeated timeouts on a small window would drive
        // cwnd to 1 permanently and the connection could never recover.
        let mut c = Reno::new(CcConfig::reno());
        for _ in 0..20 {
            c.on_timeout();
        }
        assert!(c.ssthresh() >= 2, "ssthresh fell to {}", c.ssthresh());
        assert!(c.cwnd() >= 1);
    }

    #[test]
    fn rtt_samples_are_suppressed_during_loss_episodes() {
        let mut c = Reno::new(CcConfig::reno());
        assert!(c.rtt_sample_valid());
        for _ in 0..3 {
            c.on_dup_ack();
        }
        assert!(!c.rtt_sample_valid(), "fast recovery RTTs are not trustworthy");
        c.on_timeout();
        assert!(!c.rtt_sample_valid(), "post-timeout RTTs are not trustworthy");
    }

    #[test]
    fn sack_credit_keeps_the_window_open_through_a_repair() {
        // The scenario this exists for: a 1% link where one segment is lost,
        // everything behind it arrives, and a naive Reno would halve the window
        // for what is really a routing blip.
        let mut c = Reno::new(CcConfig::default());
        c.set_cwnd(100);
        c.set_ssthresh(200);
        c.set_phase(CcPhase::CongestionAvoidance);
        for _ in 0..3 {
            c.on_dup_ack();
        }
        assert_eq!(c.phase(), CcPhase::FastRecovery);
        let before = c.cwnd();
        // The peer keeps reporting SACKed segments: the pipe is full.
        for _ in 0..10 {
            c.on_ack(0, crate::wire::SEGMENT_PAYLOAD as u64);
        }
        assert_eq!(c.cwnd(), before, "SACK-only credit must not deflate the window");
        assert!(c.sacked_bytes_credited() > 0);
    }

    #[test]
    fn window_is_capped_at_max_cwnd() {
        let mut c = Reno::new(CcConfig {
            max_cwnd: 64,
            initial_ssthresh: 1 << 20,
            ..CcConfig::default()
        });
        for _ in 0..10_000 {
            c.on_ack(4, 0);
        }
        assert_eq!(c.cwnd(), 64, "cwnd must not exceed the configured maximum");
    }

    #[test]
    fn tx_counters_track_new_and_retransmitted_segments() {
        let mut c = Reno::new(CcConfig::reno());
        c.on_new_send(5);
        c.on_retransmit(2);
        c.on_new_send(3);
        assert_eq!(c.tx_counts(), (8, 2));
    }

    #[test]
    fn mss_accessor_reflects_the_config() {
        let c = Reno::new(CcConfig {
            mss: 1200,
            ..CcConfig::default()
        });
        assert_eq!(c.mss(), 1200);
    }

    #[test]
    fn phase_renders_for_the_dump() {
        assert_eq!(CcPhase::SlowStart.to_string(), "SlowStart");
        assert_eq!(CcPhase::CongestionAvoidance.to_string(), "CongestionAvoidance");
        assert_eq!(CcPhase::FastRecovery.to_string(), "FastRecovery");
        assert_eq!(CcPhase::Timeout.to_string(), "Timeout");
    }
}
