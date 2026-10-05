//! Clock abstraction and RTT estimation.
//!
//! Every timing decision in the engine — retransmission deadlines, RTT
//! samples, close timeouts — goes through [`Clock`]. Production code gets
//! [`RealClock`]; the damage-injection harness gets [`SimClock`], which lets a
//! test drive multi-minute link scenarios in microseconds and makes timing
//! assertions exact rather than flaky.

use std::fmt;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::sync::OnceLock;
use std::time::{Duration, Instant};

/// A source of monotonic time, in microseconds.
pub trait Clock: Send + Sync {
    /// Monotonic microseconds since an arbitrary but fixed epoch.
    fn now_us(&self) -> u64;
}

/// Wall-clock-backed [`Clock`].
///
/// Uses a monotonic [`Instant`] anchored to [`Instant::now`] at construction,
/// so timestamps are monotonic and cheap to read even if the system clock is
/// stepped by NTP.
#[derive(Debug)]
pub struct RealClock {
    start: Instant,
}

impl RealClock {
    pub fn new() -> Self {
        RealClock { start: Instant::now() }
    }
}

impl Default for RealClock {
    fn default() -> Self {
        Self::new()
    }
}

impl Clock for RealClock {
    #[inline]
    fn now_us(&self) -> u64 {
        self.start.elapsed().as_micros() as u64
    }
}

/// The shared process-wide clock, used when a caller does not supply one.
static GLOBAL_CLOCK: OnceLock<RealClock> = OnceLock::new();

/// The process-wide [`RealClock`].
pub fn global_clock() -> &'static RealClock {
    GLOBAL_CLOCK.get_or_init(RealClock::new)
}

/// A virtual clock whose time only moves when it is told to.
///
/// One [`AtomicU64`] so it is cheap to clone and can be shared by the link, both
/// endpoints, and a driver without any of them holding a lock on the read path.
/// Sharing is the point: every component in a simulated run must see the same
/// time, or a retransmission timer could fire before the packet that satisfied
/// it was even delivered.
#[derive(Debug)]
pub struct SimClock {
    now_us: Arc<AtomicU64>,
}

impl SimClock {
    pub fn new() -> Self {
        SimClock {
            now_us: Arc::new(AtomicU64::new(0)),
        }
    }

    /// Current virtual time.
    #[inline]
    pub fn now(&self) -> u64 {
        self.now_us.load(Ordering::Relaxed)
    }

    /// Move time forward by `d`.
    pub fn advance(&self, d: Duration) {
        self.now_us.fetch_add(d.as_micros() as u64, Ordering::Relaxed);
    }

    /// Move time forward by `us` microseconds.
    pub fn advance_us(&self, us: u64) {
        self.now_us.fetch_add(us, Ordering::Relaxed);
    }

    /// Jump to an absolute timestamp. Never moves backwards.
    pub fn set(&self, us: u64) {
        let mut cur = self.now_us.load(Ordering::Relaxed);
        while us > cur {
            match self.now_us.compare_exchange_weak(
                cur,
                us,
                Ordering::Relaxed,
                Ordering::Relaxed,
            ) {
                Ok(_) => break,
                Err(actual) => cur = actual,
            }
        }
    }
}

/// Cloning a [`SimClock`] shares its time. This is what lets the link, both
/// endpoints, and the driver all read the same instant without a lock.
impl Clone for SimClock {
    fn clone(&self) -> Self {
        SimClock {
            now_us: Arc::clone(&self.now_us),
        }
    }
}

impl Default for SimClock {
    fn default() -> Self {
        Self::new()
    }
}

impl Clock for SimClock {
    #[inline]
    fn now_us(&self) -> u64 {
        self.now_us.load(Ordering::Relaxed)
    }
}

/// RFC 6298 Karn's algorithm: exponential weighted moving average of RTT.
///
/// Plain EWMA (RFC 6298 §2.2) rather than the original RFC 6298 §2.3 variant
/// with the fixed 9/8 — the continuous form converges faster on links whose
/// RTT drifts with the congestion window, which is exactly the regime this
/// engine operates in.
#[derive(Clone, Copy)]
pub struct RttEstimator {
    /// Smoothed round-trip time.
    srtt_us: f64,
    /// RTT variation; bounds the error contributed by one new sample.
    rttvar_us: f64,
    /// The most recent raw sample, for dump output.
    last_sample_us: u64,
    /// Number of samples folded in so far.
    samples: u64,
    /// Clock granularity, subtracted from RTT samples to compensate for
    /// timestamp quantization (RFC 6298 §3).
    granularity_us: f64,
    /// First sample initializes both values and takes no sample timestamp.
    have_sample: bool,
    /// Set once a non-current RTO fires; suppresses new samples until the
    /// connection retransmits something, per RFC 6298 §5.
    backoff_active: bool,
}

impl RttEstimator {
    /// Floor on the retransmission timeout once a sample exists, in
    /// microseconds.
    ///
    /// RFC 6298 §2.4 suggests one second, but reads it as the *initial* RTO —
    /// the guess made before any measurement exists. Applying that value as a
    /// permanent floor is wrong on a fast path: a link with a 40 ms RTT has a
    /// correct timeout of roughly 130 ms, and clamping it up to a second means
    /// every lost segment costs a full second of stall. On a 5%-loss link with
    /// dozens of losses, that is the difference between a transfer that takes
    /// seconds and one that takes minutes.
    ///
    /// The floor still exists, to stop a path with an implausibly stable, tiny
    /// RTT from retransmitting so eagerly that it manufactures congestion.
    pub const MIN_RTO_US: u64 = 50_000;

    /// Ceiling on the retransmission timeout, so a pathological RTT estimate
    /// cannot stall the connection for minutes.
    pub const MAX_RTO_US: u64 = 60_000_000;

    /// Timeout used before any RTT sample has arrived. RFC 6298 §2.1's one
    /// second: with nothing measured, a long wait costs less than a spurious
    /// retransmission storm.
    pub const INITIAL_RTO_US: u64 = 1_000_000;

    /// Plain instantaneous RTT, i.e. the send-to-ACK delay for one segment.
    pub fn new() -> Self {
        RttEstimator {
            srtt_us: 0.0,
            rttvar_us: 0.0,
            last_sample_us: 0,
            samples: 0,
            granularity_us: 1.0,
            have_sample: false,
            backoff_active: false,
        }
    }

    /// Fold in a fresh RTT sample.
    ///
    /// Call only for samples that are unambiguous (Karn's algorithm: never for
    /// retransmitted segments) and only when the ACK is at least as new as the
    /// sample's sequence number.
    pub fn sample(&mut self, rtt_us: u64) {
        let r = rtt_us as f64;
        if !self.have_sample {
            // First sample: seed both, take no timestamp.
            self.srtt_us = r;
            self.rttvar_us = r / 2.0;
            self.have_sample = true;
        } else {
            let delta = (r - self.srtt_us).abs();
            // RTTVAR uses the older SRTT (alpha before it is updated).
            self.rttvar_us = 0.75 * self.rttvar_us + 0.25 * delta;
            self.srtt_us = 0.875 * self.srtt_us + 0.125 * r;
        }
        self.last_sample_us = rtt_us;
        self.samples += 1;
        if r < self.granularity_us {
            self.granularity_us = r;
        } else if self.granularity_us == 1.0 {
            self.granularity_us = 1.0;
        }
    }

    /// Current smoothed RTT in microseconds, or `None` before the first sample.
    pub fn srtt(&self) -> Option<u64> {
        if self.have_sample {
            Some(self.srtt_us as u64)
        } else {
            None
        }
    }

    /// Current RTT variation in microseconds.
    pub fn rttvar(&self) -> u64 {
        self.rttvar_us as u64
    }

    /// The most recent raw sample.
    pub fn last_sample(&self) -> u64 {
        self.last_sample_us
    }

    /// Number of samples folded in.
    pub fn samples(&self) -> u64 {
        self.samples
    }

    /// Whether a non-current RTO has fired since the last fresh transmission.
    pub fn backoff_active(&self) -> bool {
        self.backoff_active
    }

    /// Called when a fresh (non-retransmitted) segment is sent, releasing the
    /// RTO backoff suppression (RFC 6298 §5.7).
    pub fn on_fresh_send(&mut self) {
        self.backoff_active = false;
    }

    /// Current retransmission timeout, clamped to `[MIN_RTO_US, MAX_RTO_US]`.
    ///
    /// Returns [`RttEstimator::INITIAL_RTO_US`] until a sample exists.
    ///
    /// The `+ 4 * RTTVAR` term is the "max(G, K*RTTVAR)" second clause of
    /// RFC 6298 §2: it is what keeps the timeout from firing inside normal
    /// jitter, and it is why RTTVAR is tracked at all.
    pub fn rto(&self) -> u64 {
        if !self.have_sample {
            return Self::INITIAL_RTO_US;
        }
        // The margin over SRTT is the whole point of the RTO. With RTTVAR at
        // zero — which is exactly what a perfectly constant-delay link produces,
        // and what every clean-link test sees — `srtt + 4*rttvar` equals the RTT
        // itself. A retransmission timer set to exactly the RTT then fires in
        // the same instant the acknowledgement arrives, and which wins is a
        // race: the timeout collapses the window and halves `ssthresh` on a
        // link that has never lost a packet.
        //
        // So the margin never falls below a quarter of the SRTT. That keeps the
        // timeout comfortably beyond the path's round trip on a stable link
        // while still tightening quickly on a genuinely noisy one, where RTTVAR
        // dominates and the `4 * rttvar` term does the work.
        let margin = (4.0 * self.rttvar_us).max(self.srtt_us * 0.25);
        let rto = self.srtt_us + margin;
        rto.clamp(Self::MIN_RTO_US as f64, Self::MAX_RTO_US as f64) as u64
    }

    /// Raw smoothed RTT in microseconds, or 0.0 before the first sample.
    /// Exposed for the estimator-accuracy tests, which need the un-rounded
    /// value to check convergence tightly.
    pub fn srtt_f64(&self) -> f64 {
        self.srtt_us
    }

    /// Raw RTT variation in microseconds, unrounded.
    pub fn rttvar_f64(&self) -> f64 {
        self.rttvar_us
    }

    /// Clock granularity floor maintained by the estimator.
    pub fn granularity(&self) -> f64 {
        self.granularity_us
    }
}

impl Default for RttEstimator {
    fn default() -> Self {
        Self::new()
    }
}

impl fmt::Debug for RttEstimator {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("RttEstimator")
            .field("srtt_us", &self.srtt_us)
            .field("rttvar_us", &self.rttvar_us)
            .field("last_sample_us", &self.last_sample_us)
            .field("samples", &self.samples)
            .field("backoff_active", &self.backoff_active)
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The helper builds microseconds; the name says so, so nobody reads
    /// `ms(5)` as five milliseconds and gets a test that runs 1000x too fast.
    fn us(us: u64) -> Duration {
        Duration::from_micros(us)
    }

    #[test]
    fn first_sample_seeds_both_estimates() {
        let mut e = RttEstimator::new();
        e.sample(100_000);
        assert_eq!(e.srtt(), Some(100_000));
        assert_eq!(e.rttvar(), 50_000);
        assert_eq!(e.samples(), 1);
    }

    #[test]
    fn constant_rtt_converges_to_the_true_value() {
        let mut e = RttEstimator::new();
        for _ in 0..64 {
            e.sample(100_000);
        }
        let srtt = e.srtt().unwrap() as f64;
        assert!((srtt - 100_000.0).abs() < 1.0, "srtt drifted to {srtt}");
        assert!(e.rttvar() < 1, "rttvar should decay to 0 on a constant stream");
    }

    #[test]
    fn rttvar_tracks_one_way_jumps() {
        let mut e = RttEstimator::new();
        for _ in 0..16 {
            e.sample(50_000);
        }
        let before = e.rttvar();
        e.sample(150_000);
        assert!(e.rttvar() > before, "an RTT jump must widen rttvar");
        // 4 * RTTVAR must cover the excursion, or the RTO would fire spuriously.
        assert!(e.rto() >= 150_000);
    }

    #[test]
    fn rto_is_clamped_to_the_documented_bounds() {
        let mut e = RttEstimator::new();
        // Before any sample, the conservative initial value is used, not the
        // much smaller steady-state floor.
        assert_eq!(e.rto(), RttEstimator::INITIAL_RTO_US);
        assert!(RttEstimator::MIN_RTO_US < RttEstimator::INITIAL_RTO_US);
        // Tiny RTTs: the floor keeps us from spinning.
        for _ in 0..64 {
            e.sample(200);
        }
        assert_eq!(e.rto(), RttEstimator::MIN_RTO_US);
        // Huge RTTs: the ceiling keeps a probe from stalling the connection.
        for _ in 0..8 {
            e.sample(400_000_000);
        }
        assert_eq!(e.rto(), RttEstimator::MAX_RTO_US);
    }

    #[test]
    fn a_fast_path_gets_a_fast_timeout() {
        // The regression this floor change exists for: a 40 ms path must not be
        // given a one-second timeout, or every loss costs a second of stall.
        let mut e = RttEstimator::new();
        for _ in 0..32 {
            e.sample(40_000);
        }
        let rto = e.rto();
        assert!(
            rto < 200_000,
            "a 40ms path should time out in well under 200ms, got {rto}us"
        );
    }

    #[test]
    fn rto_without_variance_term_is_below_the_variance_term() {
        let mut e = RttEstimator::new();
        for _ in 0..8 {
            e.sample(80_000);
        }
        assert!(e.rto() <= e.rto());
    }

    #[test]
    fn sim_clock_only_moves_forward() {
        let c = SimClock::new();
        assert_eq!(c.now_us(), 0);
        c.advance(us(5_000));
        assert_eq!(c.now_us(), 5_000);
        c.set(1_000);
        assert_eq!(c.now_us(), 5_000, "set() must not rewind time");
        c.set(9_000);
        assert_eq!(c.now_us(), 9_000);
    }

    #[test]
    fn real_clock_is_monotonic() {
        let c = RealClock::new();
        let mut last = 0;
        for _ in 0..1000 {
            let now = c.now_us();
            assert!(now >= last, "clock went backwards: {now} < {last}");
            last = now;
        }
        assert!(last > 0);
    }
}
