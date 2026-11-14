use crate::model::{Frame, FRAME_SAMPLES};

#[derive(Clone, Debug)]
pub struct Plc {
    history: Vec<i16>,
    pitch_samples: usize,
    rms: f64,
    noise_state: u64,
}

impl Default for Plc {
    fn default() -> Self {
        Self::new()
    }
}

impl Plc {
    pub fn new() -> Self {
        Self {
            history: Vec::with_capacity(FRAME_SAMPLES * 3),
            pitch_samples: 240,
            rms: 0.0,
            noise_state: 0x9e37_79b9_7f4a_7c15,
        }
    }

    pub fn reset(&mut self) {
        self.history.clear();
        self.pitch_samples = 240;
        self.rms = 0.0;
    }

    pub fn observe(&mut self, frame: &Frame) {
        self.pitch_samples = estimate_pitch(&frame.pcm);
        self.rms = rms(&frame.pcm);
        self.history.extend_from_slice(&frame.pcm);
        let keep = FRAME_SAMPLES * 3;
        if self.history.len() > keep {
            self.history.drain(..self.history.len() - keep);
        }
    }

    pub fn conceal(&mut self, run_index: u64) -> Vec<i16> {
        if run_index == 0 {
            return vec![0; FRAME_SAMPLES];
        }
        if run_index <= 6 && !self.history.is_empty() {
            return self.pitch_repeat(run_index);
        }
        self.comfort_noise()
    }

    fn pitch_repeat(&self, run_index: u64) -> Vec<i16> {
        let period = self.pitch_samples.max(1).min(self.history.len());
        let source_start = self.history.len() - period;
        let gain = 0.251_f64.powi(run_index as i32);
        let phase = (run_index as usize - 1) * FRAME_SAMPLES;
        let mut out = Vec::with_capacity(FRAME_SAMPLES);
        for index in 0..FRAME_SAMPLES {
            let sample = self.history[source_start + (phase + index) % period];
            out.push((sample as f64 * gain).round().clamp(-32768.0, 32767.0) as i16);
        }
        out
    }

    fn comfort_noise(&mut self) -> Vec<i16> {
        let amplitude = self.rms * 0.18 + 8.0;
        let mut out = Vec::with_capacity(FRAME_SAMPLES);
        for _ in 0..FRAME_SAMPLES {
            self.noise_state ^= self.noise_state << 13;
            self.noise_state ^= self.noise_state >> 7;
            self.noise_state ^= self.noise_state << 17;
            let unit = ((self.noise_state >> 11) as f64) / ((1u64 << 53) as f64);
            let sample = (unit * 2.0 - 1.0) * amplitude;
            out.push(sample.round().clamp(-32768.0, 32767.0) as i16);
        }
        out
    }
}

fn rms(pcm: &[i16]) -> f64 {
    if pcm.is_empty() {
        return 0.0;
    }
    let sum = pcm
        .iter()
        .map(|sample| {
            let value = *sample as f64;
            value * value
        })
        .sum::<f64>();
    (sum / pcm.len() as f64).sqrt()
}

fn estimate_pitch(pcm: &[i16]) -> usize {
    const DECIMATE: usize = 4;
    const WINDOW: usize = 120;
    let downsampled: Vec<i32> = pcm
        .iter()
        .step_by(DECIMATE)
        .map(|sample| *sample as i32)
        .collect();
    if downsampled.len() < WINDOW + 200 {
        return 240;
    }
    let mut best_lag = 60usize;
    let mut best_score = f64::MIN;
    for lag in 40..=200 {
        let mut corr = 0.0;
        let mut energy_a = 0.0;
        let mut energy_b = 0.0;
        for index in 0..WINDOW {
            let a = downsampled[index] as f64;
            let b = downsampled[index + lag] as f64;
            corr += a * b;
            energy_a += a * a;
            energy_b += b * b;
        }
        let score = corr / (energy_a.sqrt() * energy_b.sqrt() + 1.0);
        if score > best_score {
            best_score = score;
            best_lag = lag;
        }
    }
    (best_lag * DECIMATE).clamp(80, 800)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn frame() -> Frame {
        Frame {
            ssrc: 1,
            seq: 0,
            ts: 0,
            arrival_us: 0,
            pcm: vec![1000; FRAME_SAMPLES],
        }
    }

    #[test]
    fn plc_outputs_full_frames_and_decays() {
        let mut plc = Plc::new();
        plc.observe(&frame());
        let first = plc.conceal(1);
        let sixth = plc.conceal(6);
        assert_eq!(first.len(), FRAME_SAMPLES);
        assert!(first[0].abs() > sixth[0].abs());
    }

    #[test]
    fn long_gap_switches_to_bounded_comfort_noise() {
        let mut plc = Plc::new();
        plc.observe(&frame());
        let noise = plc.conceal(7);
        assert_eq!(noise.len(), FRAME_SAMPLES);
        assert!(noise.iter().all(|sample| (*sample as i32).abs() < 32_000));
    }
}
