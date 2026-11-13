use std::collections::{BTreeMap, HashMap};
use std::mem;

pub const WINDOW_SECONDS: i64 = 300;
pub const ALLOWED_LATENESS_SECONDS: i64 = 120;
pub const CLOCK32_MODULUS: i64 = 1_i64 << 32;
pub const MAX_PLAUSIBLE_STEP_SECONDS: i64 = 86_400;

#[derive(Debug, Clone, thiserror::Error)]
pub enum TimeError {
    #[error("32-bit clock is missing its device anchor")]
    MissingAnchor,
    #[error("suspicious clock delta: {0}")]
    Suspicious(String),
}

#[derive(Debug, Clone)]
pub struct WrapNotice {
    pub device: String,
    pub wrap_no: u32,
    pub raw_before: i64,
    pub raw_after: i64,
    pub normalized_after: i64,
}

impl WrapNotice {
    pub fn fingerprint(&self) -> String {
        format!("wrap:{}:{}", self.device, self.wrap_no)
    }
}

#[derive(Debug, Clone)]
struct ClockState {
    last_raw: i64,
    wraps: u32,
    anchor: i64,
}

#[derive(Debug, Default)]
pub struct TimeNormalizer {
    states: HashMap<String, ClockState>,
    notices: Vec<WrapNotice>,
}

impl TimeNormalizer {
    pub fn new() -> Self {
        Self::default()
    }

    /// Expands a 32-bit counter with one wrap per detected counter reset.
    ///
    /// A valid reset must move from the upper half of the counter into its
    /// lower half while dropping by more than a plausible adjacent delta. A
    /// large backward move outside that shape is quarantined as suspicious.
    pub fn normalize(
        &mut self,
        device: &str,
        raw: i64,
        bits: u8,
        anchor: Option<i64>,
    ) -> Result<i64, TimeError> {
        if bits == 64 {
            return Ok(raw);
        }
        if !(0..CLOCK32_MODULUS).contains(&raw) {
            return Err(TimeError::Suspicious(format!(
                "32-bit timestamp outside [0, 2^32): {raw}"
            )));
        }
        let current_anchor = anchor.ok_or(TimeError::MissingAnchor)?;

        if let Some(state) = self.states.get_mut(device) {
            let delta = raw - state.last_raw;
            if delta < 0 && -delta > MAX_PLAUSIBLE_STEP_SECONDS {
                if state.last_raw >= CLOCK32_MODULUS / 2 && raw < CLOCK32_MODULUS / 2 {
                    state.wraps = state.wraps.saturating_add(1);
                    let normalized_after =
                        state.anchor + raw + i64::from(state.wraps) * CLOCK32_MODULUS;
                    self.notices.push(WrapNotice {
                        device: device.to_owned(),
                        wrap_no: state.wraps,
                        raw_before: state.last_raw,
                        raw_after: raw,
                        normalized_after,
                    });
                } else {
                    return Err(TimeError::Suspicious(format!(
                        "clock moved backward by {}s without crossing the wrap boundary",
                        -delta
                    )));
                }
            } else if delta > MAX_PLAUSIBLE_STEP_SECONDS {
                return Err(TimeError::Suspicious(format!(
                    "clock moved forward by {delta}s"
                )));
            }
            state.last_raw = raw;
            let normalized = state.anchor + raw + i64::from(state.wraps) * CLOCK32_MODULUS;
            return Ok(normalized);
        }

        self.states.insert(
            device.to_owned(),
            ClockState {
                last_raw: raw,
                wraps: 0,
                anchor: current_anchor,
            },
        );
        Ok(current_anchor + raw)
    }

    pub fn wrap_notices(&self) -> &[WrapNotice] {
        &self.notices
    }
}

pub enum PushResult<T> {
    Buffered,
    Late(T),
}

#[derive(Debug, Default)]
pub struct ReorderBuffer<T> {
    watermark: Option<i64>,
    pending: BTreeMap<i64, Vec<T>>,
    lateness_seconds: i64,
}

impl<T> ReorderBuffer<T> {
    pub fn new(lateness_seconds: i64) -> Self {
        Self {
            watermark: None,
            pending: BTreeMap::new(),
            lateness_seconds,
        }
    }

    pub fn push(&mut self, event_ts: i64, value: T) -> PushResult<T> {
        if let Some(watermark) = self.watermark {
            if watermark - event_ts > self.lateness_seconds {
                return PushResult::Late(value);
            }
        }
        self.watermark = Some(match self.watermark {
            Some(current) => current.max(event_ts),
            None => event_ts,
        });
        self.pending.entry(event_ts).or_default().push(value);
        PushResult::Buffered
    }

    pub fn watermark(&self) -> Option<i64> {
        self.watermark
    }

    pub fn pending_len(&self) -> usize {
        self.pending.values().map(Vec::len).sum()
    }

    pub fn drain_ready(&mut self) -> Vec<(i64, T)> {
        let Some(watermark) = self.watermark else {
            return Vec::new();
        };
        let cutoff = watermark - self.lateness_seconds;
        let future = self.pending.split_off(&(cutoff + 1));
        let ready = mem::replace(&mut self.pending, future);
        flatten(ready)
    }

    pub fn drain_all(&mut self) -> Vec<(i64, T)> {
        let pending = mem::take(&mut self.pending);
        flatten(pending)
    }
}

fn flatten<T>(pending: BTreeMap<i64, Vec<T>>) -> Vec<(i64, T)> {
    let mut result = Vec::new();
    for (ts, values) in pending {
        for value in values {
            result.push((ts, value));
        }
    }
    result
}

pub fn window_start(event_ts: i64) -> i64 {
    event_ts - event_ts.rem_euclid(WINDOW_SECONDS)
}

pub fn window_end(event_ts: i64) -> i64 {
    window_start(event_ts) + WINDOW_SECONDS
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expands_one_wrap_without_double_counting() {
        let modulus = CLOCK32_MODULUS;
        let wall = 1_000_000_i64;
        let raw0 = modulus - 5;
        let anchor = wall - raw0;
        let mut normalizer = TimeNormalizer::new();
        let before = normalizer
            .normalize("legacy", modulus - 2, 32, Some(anchor))
            .unwrap();
        let after = normalizer.normalize("legacy", 3, 32, Some(anchor)).unwrap();
        assert_eq!(before, wall + 3);
        assert_eq!(after, wall + 8);
        assert_eq!(normalizer.wrap_notices().len(), 1);
    }

    #[test]
    fn late_events_are_rejected_by_watermark() {
        let mut buffer = ReorderBuffer::new(ALLOWED_LATENESS_SECONDS);
        assert!(matches!(buffer.push(1000, "new"), PushResult::Buffered));
        assert!(matches!(buffer.push(1300, "newer"), PushResult::Buffered));
        assert!(matches!(
            buffer.push(1100, "late"),
            PushResult::Late("late")
        ));
        let ready = buffer.drain_ready();
        assert_eq!(ready.len(), 1);
        assert_eq!(ready[0].1, "new");
    }

    #[test]
    fn non_wrap_backward_delta_is_suspicious() {
        let modulus = CLOCK32_MODULUS;
        let anchor = 1000 - 10;
        let mut normalizer = TimeNormalizer::new();
        normalizer
            .normalize("device", 100_000, 32, Some(anchor))
            .unwrap();
        let error = normalizer
            .normalize("device", 1_000, 32, Some(anchor))
            .unwrap_err();
        assert!(matches!(error, TimeError::Suspicious(_)));
        assert!(normalizer.wrap_notices().is_empty());
        assert!(modulus > 100_000);
    }
}
