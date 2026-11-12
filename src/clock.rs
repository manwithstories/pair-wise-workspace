//! Event-time reconstruction for 32-bit second counters.
//!
//! Devices emit monotonically increasing `u32` ticks that wrap every
//! `2^32` seconds. The [`ClockExpander`] converts each raw tick into a
//! monotonic 64-bit *expanded* second, detecting wraps from strongly negative
//! deltas while rejecting ambiguous forward jumps.
//!
//! The same logic is used online (arrival order, watermark + lateness) and for
//! the full-sort self check, which is what makes the pipeline re-auditable.

use crate::config::ClockConfig;
use crate::error::{Error, Result};

/// Persistable expander state, also used for cross-file continuation.
#[derive(Debug, Clone, Default)]
pub struct ClockState {
    pub high: u64,
    pub last_raw: u32,
    pub seen: bool,
    pub wraparounds: u64,
}

pub struct ClockExpander {
    cfg: ClockConfig,
    state: ClockState,
}

impl ClockExpander {
    pub fn new(cfg: ClockConfig) -> Self {
        Self {
            cfg,
            state: ClockState::default(),
        }
    }

    pub fn from_state(cfg: ClockConfig, state: ClockState) -> Self {
        Self { cfg, state }
    }

    pub fn state(&self) -> &ClockState {
        &self.state
    }

    pub fn wraparounds(&self) -> u64 {
        self.state.wraparounds
    }

    /// Expand one raw tick against the running monotonic clock.
    pub fn expand(&mut self, raw: u32) -> Result<u64> {
        if !self.state.seen {
            self.state.seen = true;
            self.state.last_raw = raw;
            self.state.high = self.cfg.wrap_baseline as u64 + raw as u64;
            return Ok(self.state.high);
        }

        let delta = signed_delta(self.state.last_raw, raw);
        if delta < 0 && (delta.unsigned_abs()) >= self.cfg.wrap_threshold {
            // Strongly negative jump: the 32-bit counter rolled over. The
            // invariant `high == wraparounds * 2^32 + raw` is restored by
            // adding one full period and the within-period raw difference.
            self.state.high = self
                .state
                .wraparounds
                .saturating_add(1)
                .saturating_mul(self.cfg.wrap_period_secs)
                .saturating_add(raw as u64);
            self.state.wraparounds += 1;
        } else if delta < 0 {
            // Small backward drift: clock skew or a late-arriving event. This is
            // not a wrap, so the period counter is unchanged; we reconstruct the
            // event's true (behind-the-watermark) time. Allowing high to step
            // backwards here is precisely what makes the downstream watermark
            // classify the event as late and trigger a restatement.
            self.state.high = self
                .state
                .wraparounds
                .saturating_mul(self.cfg.wrap_period_secs)
                .saturating_add(raw as u64);
        } else if (delta as u64) > self.cfg.jump_threshold {
            return Err(Error::Clock(format!(
                "forward jump of {}s at raw tick {} exceeds jump_threshold {}; \
                 cannot disambiguate counter wrap from missing segment",
                delta, raw, self.cfg.jump_threshold
            )));
        } else {
            self.state.high = self
                .state
                .wraparounds
                .saturating_mul(self.cfg.wrap_period_secs)
                .saturating_add(raw as u64);
        }
        self.state.last_raw = raw;
        Ok(self.state.high)
    }
}

/// Signed delta from `prev` to `cur` treating both as raw 32-bit ticks.
/// Both operands fit in `i64`, so a direct subtraction yields the correct
/// signed distance without any 64-bit wrap artefact.
fn signed_delta(prev: u32, cur: u32) -> i64 {
    cur as i64 - prev as i64
}

/// Monotonic event-time watermark with an allowed-lateness budget.
#[derive(Debug)]
pub struct Watermark {
    pub max_event_time: i64,
    pub allowed_lateness: u64,
}

impl Watermark {
    pub fn new(allowed_lateness: u64) -> Self {
        Self {
            max_event_time: -1,
            allowed_lateness,
        }
    }

    /// Advance the watermark when a newer event arrives. Returns true if it moved.
    pub fn observe(&mut self, event_time: u64) -> bool {
        let t = event_time as i64;
        if t > self.max_event_time {
            self.max_event_time = t;
            true
        } else {
            false
        }
    }

    /// A window `[start, start+len)` is closed once the watermark passes its
    /// end plus the lateness allowance.
    pub fn window_closed(&self, window_end_exclusive: u64) -> bool {
        self.max_event_time >= (window_end_exclusive as i64 + self.allowed_lateness as i64)
    }

    /// An event is late when it belongs to an already-closed window.
    pub fn is_late(&self, event_time: u64, window_length: u64) -> bool {
        let window_end = (event_time / window_length + 1) * window_length;
        self.window_closed(window_end)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> ClockConfig {
        ClockConfig {
            wrap_baseline: 0,
            wrap_threshold: 100_000,
            jump_threshold: 200_000,
            wrap_period_secs: 100_000,
        }
    }

    #[test]
    fn expands_plain_sequence() {
        let mut c = ClockExpander::new(cfg());
        assert_eq!(c.expand(10).unwrap(), 10);
        assert_eq!(c.expand(15).unwrap(), 15);
        assert_eq!(c.wraparounds(), 0);
    }

    #[test]
    fn detects_single_and_double_wrap() {
        let mut c = ClockExpander::new(cfg());
        // Test config: wrap_threshold 100000, wrap_period_secs 100000.
        c.expand(150_000).unwrap();
        // Wrap #1: strong negative delta (>100000) -> high=100000+5.
        let after_first = c.expand(5).unwrap();
        assert_eq!(after_first, 100_000 + 5);
        assert_eq!(c.wraparounds(), 1);
        // Climb back within period 1 (bounded steps), then wrap #2.
        c.expand(60_000).unwrap();
        c.expand(120_000).unwrap();
        c.expand(150_000).unwrap();
        let after_second = c.expand(9).unwrap();
        assert_eq!(after_second, 2 * 100_000 + 9);
        assert_eq!(c.wraparounds(), 2);
    }

    #[test]
    fn rejects_ambiguous_forward_jump() {
        let mut c = ClockExpander::new(cfg());
        c.expand(10).unwrap();
        assert!(c.expand(500_000).is_err());
    }

    #[test]
    fn small_backward_drift_is_not_wrap() {
        let mut c = ClockExpander::new(cfg());
        c.expand(100).unwrap();
        let v = c.expand(98).unwrap();
        // Drift reconstructs the true behind-watermark time, no wrap counted.
        assert_eq!(v, 98);
        assert_eq!(c.wraparounds(), 0);
    }
}
