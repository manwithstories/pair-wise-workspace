use crate::model::{Frame, FRAME_MS, FRAME_SAMPLES};
use crate::plc::Plc;
use crate::reorder::{InsertOutcome, ReorderBuffer};
use std::collections::VecDeque;

#[derive(Clone, Debug)]
pub struct ControllerConfig {
    pub min_buffer_ms: u64,
    pub max_buffer_ms: u64,
    pub hysteresis_ms: u64,
    pub reorder_capacity: usize,
    pub jitter_window: usize,
    pub conceal_reset_ms: u64,
}

impl Default for ControllerConfig {
    fn default() -> Self {
        Self {
            min_buffer_ms: 60,
            max_buffer_ms: 140,
            hysteresis_ms: 2_000,
            reorder_capacity: crate::reorder::DEFAULT_REORDER_CAPACITY,
            jitter_window: 128,
            conceal_reset_ms: 400,
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ControllerStats {
    pub arrivals: u64,
    pub late: u64,
    pub duplicate: u64,
    pub conflict: u64,
    pub overrun: u64,
    pub concealed: u64,
    pub underruns: u64,
    pub resets: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TickOutput {
    pub time_ms: u64,
    pub water_ms: u64,
    pub target_ms: u64,
    pub arrivals: u64,
    pub late: u64,
    pub duplicate: u64,
    pub concealed: u64,
    pub frame: Option<Frame>,
    pub concealed_frame: bool,
    pub underrun_started: bool,
    pub reset: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PushKind {
    Buffered,
    Duplicate,
    Conflict,
    Late,
    Overrun,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PushReceipt {
    pub kind: PushKind,
    pub ext_seq: Option<u64>,
}

pub struct Controller {
    config: ControllerConfig,
    reorder: ReorderBuffer,
    plc: Plc,
    active_ssrc: Option<u32>,
    jitter: VecDeque<f64>,
    last_transit_ms: Option<f64>,
    target_ms: u64,
    lower_since_ms: Option<u64>,
    next_tick_ms: u64,
    first_buffered_ms: Option<u64>,
    playing: bool,
    next_ext: u64,
    next_raw: u16,
    next_ts: u64,
    conceal_run: u64,
    in_underrun: bool,
    stats: ControllerStats,
    buffer_max_ms: u64,
    target_max_ms: u64,
    emitted_frames: u64,
    total_frames: Option<u64>,
}

impl Controller {
    pub fn new(config: ControllerConfig) -> Self {
        let reorder = ReorderBuffer::new(config.reorder_capacity.max(1));
        let target_ms = config.min_buffer_ms.min(config.max_buffer_ms);
        Self {
            config,
            reorder,
            plc: Plc::new(),
            active_ssrc: None,
            jitter: VecDeque::new(),
            last_transit_ms: None,
            target_ms,
            lower_since_ms: None,
            next_tick_ms: 0,
            first_buffered_ms: None,
            playing: false,
            next_ext: 0,
            next_raw: 0,
            next_ts: 0,
            conceal_run: 0,
            in_underrun: false,
            stats: ControllerStats::default(),
            buffer_max_ms: 0,
            target_max_ms: target_ms,
            emitted_frames: 0,
            total_frames: None,
        }
    }

    pub fn push(&mut self, frame: Frame) -> PushReceipt {
        let arrival_ms = frame.arrival_ms();
        let transit_ms = frame.arrival_us as f64 / 1000.0 - frame.ts as f64 / 48.0;
        if let Some(ssrc) = self.active_ssrc {
            if ssrc != frame.ssrc {
                self.reset_pipeline(Some(frame.ssrc));
            }
        } else {
            self.active_ssrc = Some(frame.ssrc);
        }
        let outcome = self.reorder.insert(frame);
        let (kind, ext_seq) = match outcome {
            InsertOutcome::Inserted { seq, .. } => (PushKind::Buffered, Some(seq)),
            InsertOutcome::Overrun { buffered: _, .. } => (PushKind::Overrun, None),
            InsertOutcome::Duplicate => {
                self.stats.duplicate = self.reorder.counts().duplicate;
                (PushKind::Duplicate, None)
            }
            InsertOutcome::Conflict => {
                self.stats.conflict = self.reorder.counts().conflict;
                (PushKind::Conflict, None)
            }
            InsertOutcome::Late => {
                self.stats.late = self.reorder.counts().late;
                (PushKind::Late, None)
            }
        };
        if matches!(kind, PushKind::Buffered | PushKind::Overrun) {
            self.stats.arrivals += 1;
            self.stats.overrun = self.reorder.counts().overrun;
            if let Some(previous) = self.last_transit_ms {
                let deviation = (transit_ms - previous).abs();
                if self.jitter.len() >= self.config.jitter_window.max(1) {
                    self.jitter.pop_front();
                }
                self.jitter.push_back(deviation);
                self.update_target(arrival_ms);
            }
            self.last_transit_ms = Some(transit_ms);
            if self.first_buffered_ms.is_none() {
                self.first_buffered_ms = Some(arrival_ms);
            }
        }
        PushReceipt { kind, ext_seq }
    }

    pub fn advance_to(&mut self, time_ms: u64) -> Vec<TickOutput> {
        let mut output = Vec::new();
        while self.next_tick_ms <= time_ms {
            output.push(self.tick());
            self.next_tick_ms += FRAME_MS;
        }
        output
    }

    pub fn finish(&mut self, total_frames: u64) -> Vec<TickOutput> {
        self.total_frames.get_or_insert(total_frames);
        let mut output = Vec::new();
        let mut guard = total_frames.saturating_mul(2).saturating_add(1_000);
        while self.emitted_frames < total_frames && guard > 0 {
            if !self.playing && self.reorder.is_empty() {
                break;
            }
            output.push(self.tick());
            self.next_tick_ms += FRAME_MS;
            guard -= 1;
        }
        output
    }

    fn tick(&mut self) -> TickOutput {
        let now = self.next_tick_ms;
        let mut reset = false;
        if self
            .total_frames
            .is_some_and(|total| self.emitted_frames >= total)
        {
            self.playing = false;
        }
        if !self.playing {
            self.try_prime(now);
        }
        let mut frame = None;
        let mut concealed_frame = false;
        let mut underrun_started = false;
        if self.playing {
            self.reorder.set_floor(self.next_ext);
            if let Some(valid) = self.reorder.take(self.next_ext) {
                self.next_ext = self.next_ext.saturating_add(1);
                self.next_raw = valid.seq.wrapping_add(1);
                self.next_ts = valid.ts.saturating_add(FRAME_SAMPLES as u64);
                self.plc.observe(&valid);
                self.conceal_run = 0;
                self.in_underrun = false;
                frame = Some(valid);
                self.emitted_frames += 1;
            } else {
                concealed_frame = true;
                self.stats.concealed += 1;
                self.conceal_run += 1;
                if self.reorder.is_empty() && !self.in_underrun {
                    self.stats.underruns += 1;
                    self.in_underrun = true;
                    underrun_started = true;
                }
                let pcm = self.plc.conceal(self.conceal_run);
                frame = Some(Frame {
                    ssrc: self.active_ssrc.unwrap_or(0),
                    seq: self.next_raw,
                    ts: self.next_ts,
                    arrival_us: now.saturating_mul(1_000),
                    pcm,
                });
                self.next_ext = self.next_ext.saturating_add(1);
                self.next_raw = self.next_raw.wrapping_add(1);
                self.next_ts = self.next_ts.saturating_add(FRAME_SAMPLES as u64);
                self.emitted_frames += 1;
                if self.conceal_run.saturating_mul(FRAME_MS) > self.config.conceal_reset_ms {
                    let ssrc = self.active_ssrc;
                    self.reset_pipeline(ssrc);
                    reset = true;
                }
            }
        }
        let water_ms = self.water_ms();
        self.buffer_max_ms = self.buffer_max_ms.max(water_ms);
        self.target_max_ms = self.target_max_ms.max(self.target_ms);
        TickOutput {
            time_ms: now,
            water_ms,
            target_ms: self.target_ms,
            arrivals: self.stats.arrivals,
            late: self.stats.late,
            duplicate: self.stats.duplicate,
            concealed: self.stats.concealed,
            frame,
            concealed_frame,
            underrun_started,
            reset,
        }
    }

    fn try_prime(&mut self, now: u64) {
        if self.reorder.is_empty()
            || self
                .total_frames
                .is_some_and(|total| self.emitted_frames >= total)
        {
            return;
        }
        let buffered = self.reorder.len() as u64 * FRAME_MS;
        let waited = self
            .first_buffered_ms
            .map(|first| now.saturating_sub(first))
            .unwrap_or(0);
        if buffered >= self.target_ms || waited >= self.target_ms.saturating_add(100) {
            if let Some(seq) = self.reorder.earliest_ext() {
                if let Some(first) = self.reorder.peek_earliest() {
                    self.next_ext = seq;
                    self.next_raw = first.seq;
                    self.next_ts = first.ts;
                    self.playing = true;
                    self.conceal_run = 0;
                    self.in_underrun = false;
                }
            }
        }
    }

    fn update_target(&mut self, now: u64) {
        if self.jitter.is_empty() {
            return;
        }
        let mut sorted: Vec<f64> = self.jitter.iter().copied().collect();
        sorted.sort_by(|a, b| a.total_cmp(b));
        let index = ((sorted.len() - 1) as f64 * 0.95).round() as usize;
        let p95 = sorted[index.min(sorted.len() - 1)];
        let candidate = (self.config.min_buffer_ms as f64 + p95 * 1.8).clamp(
            self.config.min_buffer_ms as f64,
            self.config.max_buffer_ms as f64,
        );
        let candidate = ((candidate / 10.0).ceil() * 10.0) as u64;
        if candidate > self.target_ms {
            self.target_ms = candidate;
            self.lower_since_ms = None;
        } else if candidate < self.target_ms {
            let since = *self.lower_since_ms.get_or_insert(now);
            if now.saturating_sub(since) >= self.config.hysteresis_ms {
                self.target_ms = candidate;
                self.lower_since_ms = None;
            }
        } else {
            self.lower_since_ms = None;
        }
    }

    fn water_ms(&self) -> u64 {
        self.reorder.len() as u64 * FRAME_MS
    }

    fn reset_pipeline(&mut self, ssrc: Option<u32>) {
        if let Some(ssrc) = ssrc {
            self.reorder.reset_for_ssrc(ssrc);
        }
        self.active_ssrc = ssrc;
        self.plc.reset();
        self.jitter.clear();
        self.last_transit_ms = None;
        self.lower_since_ms = None;
        self.first_buffered_ms = None;
        self.playing = false;
        self.next_ext = 0;
        self.next_raw = 0;
        self.next_ts = 0;
        self.conceal_run = 0;
        self.in_underrun = false;
        self.stats.resets += 1;
    }

    pub fn stats(&self) -> &ControllerStats {
        &self.stats
    }

    pub fn buffer_max_ms(&self) -> u64 {
        self.buffer_max_ms
    }

    pub fn target_max_ms(&self) -> u64 {
        self.target_max_ms
    }

    pub fn set_total_frames(&mut self, total_frames: u64) {
        self.total_frames = Some(total_frames);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::reorder::ReorderBuffer;

    fn frame(ssrc: u32, seq: u16, arrival_ms: u64) -> Frame {
        Frame {
            ssrc,
            seq,
            ts: seq as u64 * FRAME_SAMPLES as u64,
            arrival_us: arrival_ms * 1_000,
            pcm: vec![(seq as i16 + 1) * 100; FRAME_SAMPLES],
        }
    }

    fn controller(min: u64, max: u64) -> Controller {
        Controller::new(ControllerConfig {
            min_buffer_ms: min,
            max_buffer_ms: max,
            hysteresis_ms: 2_000,
            reorder_capacity: 16,
            jitter_window: 32,
            conceal_reset_ms: 400,
        })
    }

    #[test]
    fn precharges_then_outputs_in_order() {
        let mut controller = controller(40, 40);
        let mut delivered = Vec::new();
        controller.advance_to(99);
        for seq in 0..4 {
            let arrival = 100 + seq as u64 * 20;
            controller.push(frame(1, seq, arrival));
            for tick in controller.advance_to(100 + seq as u64 * 20) {
                if let Some(frame) = tick.frame {
                    delivered.push(frame.pcm[0]);
                }
            }
        }
        for tick in controller.advance_to(180) {
            if let Some(frame) = tick.frame {
                delivered.push(frame.pcm[0]);
            }
        }
        assert_eq!(delivered, vec![100, 200, 300, 400]);
        assert!(controller.stats().arrivals >= 4);
    }

    #[test]
    fn late_and_duplicate_are_counted_without_replay() {
        let mut controller = controller(20, 20);
        controller.advance_to(0);
        controller.push(frame(1, 0, 0));
        controller.push(frame(1, 2, 20));
        controller.advance_to(20);
        controller.advance_to(40);
        controller.push(frame(1, 0, 40));
        assert_eq!(controller.stats().duplicate, 1);
        controller.advance_to(60);
        controller.advance_to(80);
        controller.push(frame(1, 1, 80));
        assert!(controller.stats().late >= 1);
    }

    #[test]
    fn ssrc_change_resets_pipeline() {
        let mut controller = controller(20, 40);
        controller.advance_to(0);
        controller.push(frame(1, 0, 0));
        controller.advance_to(20);
        controller.advance_to(40);
        controller.push(frame(2, 0, 40));
        assert_eq!(controller.stats().resets, 1);
    }

    #[test]
    fn long_concealment_resets_and_reprimes() {
        let mut controller = controller(20, 20);
        controller.set_total_frames(100);
        controller.advance_to(0);
        controller.push(frame(1, 0, 0));
        controller.advance_to(20);
        for _ in 0..22 {
            controller.advance_to(controller.next_tick_ms + 20);
        }
        assert_eq!(controller.stats().resets, 1);
        controller.push(frame(1, 50, 500));
        controller.advance_to(520);
        assert!(controller.stats().resets >= 1);
    }

    #[test]
    fn reorder_buffer_floor_is_not_required_for_basic_playout() {
        let mut buffer = ReorderBuffer::new(4);
        buffer.insert(frame(1, 3, 60));
        assert_eq!(buffer.earliest_ext(), Some(3));
        assert!(buffer.peek_earliest().is_some());
    }
}
