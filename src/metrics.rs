use crate::model::{Frame, SAMPLE_RATE};
use hound::{SampleFormat, WavSpec, WavWriter};
use serde::{Deserialize, Serialize};
use std::fs::{self, File};
use std::io::BufWriter;
use std::path::Path;
use thiserror::Error;

pub const MAX_WATER_SAMPLES: usize = 8_192;
pub const ENERGY_JUMP_RATIO: f64 = 0.60;

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct WaterSample {
    pub time_ms: u64,
    pub water_ms: u64,
    pub target_ms: u64,
    pub arrivals: u64,
    pub late: u64,
    pub dup: u64,
    pub concealed: u64,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct MetricsReport {
    pub frames_total: u64,
    pub frames_played: u64,
    pub packets_ok: u64,
    pub dup_dropped: u64,
    pub late_dropped: u64,
    pub conflicts: u64,
    pub overruns: u64,
    pub resets: u64,
    pub concealed_frames: u64,
    pub longest_conceal_run: u64,
    pub underruns: u64,
    pub energy_jumps: u64,
    pub continuity_score: f64,
    pub buffer_max_ms: u64,
    pub target_max_ms: u64,
    pub duration_ms: u64,
    pub water: Vec<WaterSample>,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct PacketCounts {
    pub packets_ok: u64,
    pub dup_dropped: u64,
    pub late_dropped: u64,
    pub conflicts: u64,
    pub overruns: u64,
    pub resets: u64,
}

pub struct TickRecord<'a> {
    pub time_ms: u64,
    pub water_ms: u64,
    pub target_ms: u64,
    pub arrivals: u64,
    pub late: u64,
    pub duplicate: u64,
    pub concealed: u64,
    pub concealed_frame: bool,
    pub underrun_started: bool,
    pub frame: Option<&'a Frame>,
}

pub struct MetricsWriter {
    frames_total: u64,
    frames_played: u64,
    concealed_frames: u64,
    current_conceal_run: u64,
    longest_conceal_run: u64,
    underruns: u64,
    energy_jumps: u64,
    previous_rms: Option<f64>,
    duration_ms: u64,
    water: Vec<WaterSample>,
    sample_stride: u64,
    sample_index: u64,
    wav: Option<WavWriter<BufWriter<File>>>,
}

impl MetricsWriter {
    pub fn create(pcm_path: &Path, frames_total: u64) -> Result<Self, MetricsError> {
        if let Some(parent) = pcm_path.parent() {
            if !parent.as_os_str().is_empty() {
                fs::create_dir_all(parent).map_err(MetricsError::Io)?;
            }
        }
        let spec = WavSpec {
            channels: 1,
            sample_rate: SAMPLE_RATE,
            bits_per_sample: 16,
            sample_format: SampleFormat::Int,
        };
        let wav = WavWriter::create(pcm_path, spec).map_err(MetricsError::Wav)?;
        Ok(Self {
            frames_total,
            frames_played: 0,
            concealed_frames: 0,
            current_conceal_run: 0,
            longest_conceal_run: 0,
            underruns: 0,
            energy_jumps: 0,
            previous_rms: None,
            duration_ms: 0,
            water: Vec::new(),
            sample_stride: 1,
            sample_index: 0,
            wav: Some(wav),
        })
    }

    pub fn record(&mut self, record: TickRecord<'_>) -> Result<(), MetricsError> {
        self.duration_ms = self.duration_ms.max(record.time_ms.saturating_add(20));
        self.push_sample(&record);
        if record.underrun_started {
            self.underruns += 1;
        }
        let Some(frame) = record.frame else {
            return Ok(());
        };
        self.frames_played += 1;
        let rms = frame_rms(&frame.pcm);
        if let Some(previous) = self.previous_rms {
            let scale = previous.max(rms).max(1.0);
            if (rms - previous).abs() / scale > ENERGY_JUMP_RATIO {
                self.energy_jumps += 1;
            }
        }
        self.previous_rms = Some(rms);
        if record.concealed_frame {
            self.concealed_frames += 1;
            self.current_conceal_run += 1;
            self.longest_conceal_run = self.longest_conceal_run.max(self.current_conceal_run);
        } else {
            self.current_conceal_run = 0;
        }
        if let Some(wav) = self.wav.as_mut() {
            for sample in &frame.pcm {
                wav.write_sample(*sample).map_err(MetricsError::Wav)?;
            }
        }
        Ok(())
    }

    fn push_sample(&mut self, record: &TickRecord<'_>) {
        if self.sample_index % self.sample_stride == 0 {
            if self.water.len() >= MAX_WATER_SAMPLES {
                self.decimate_water();
            }
            self.water.push(WaterSample {
                time_ms: record.time_ms,
                water_ms: record.water_ms,
                target_ms: record.target_ms,
                arrivals: record.arrivals,
                late: record.late,
                dup: record.duplicate,
                concealed: record.concealed,
            });
        }
        self.sample_index += 1;
    }

    fn decimate_water(&mut self) {
        let mut index = 0usize;
        self.water.retain(|_| {
            let keep = index % 2 == 0;
            index += 1;
            keep
        });
        self.sample_stride = self.sample_stride.saturating_mul(2);
    }

    pub fn finish(
        mut self,
        counts: PacketCounts,
        buffer_max_ms: u64,
        target_max_ms: u64,
    ) -> Result<MetricsReport, MetricsError> {
        if let Some(wav) = self.wav.take() {
            wav.finalize().map_err(MetricsError::Wav)?;
        }
        let continuity_score = score(
            self.frames_total,
            self.concealed_frames,
            self.longest_conceal_run,
            self.underruns,
            self.energy_jumps,
        );
        Ok(MetricsReport {
            frames_total: self.frames_total,
            frames_played: self.frames_played,
            packets_ok: counts.packets_ok,
            dup_dropped: counts.dup_dropped,
            late_dropped: counts.late_dropped,
            conflicts: counts.conflicts,
            overruns: counts.overruns,
            resets: counts.resets,
            concealed_frames: self.concealed_frames,
            longest_conceal_run: self.longest_conceal_run,
            underruns: self.underruns,
            energy_jumps: self.energy_jumps,
            continuity_score,
            buffer_max_ms,
            target_max_ms,
            duration_ms: self.duration_ms,
            water: self.water,
        })
    }
}

pub fn write_report(report: &MetricsReport, path: &Path) -> Result<(), MetricsError> {
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() {
            fs::create_dir_all(parent).map_err(MetricsError::Io)?;
        }
    }
    let bytes = serde_json::to_vec_pretty(report).map_err(MetricsError::Json)?;
    fs::write(path, bytes).map_err(MetricsError::Io)
}

pub fn read_report(path: &Path) -> Result<MetricsReport, MetricsError> {
    let text = fs::read_to_string(path).map_err(MetricsError::Io)?;
    serde_json::from_str(&text).map_err(MetricsError::Json)
}

pub fn score(
    frames_total: u64,
    concealed: u64,
    longest_run: u64,
    underruns: u64,
    energy_jumps: u64,
) -> f64 {
    let frames = frames_total.max(1) as f64;
    let conceal_penalty = (concealed as f64 / frames).min(1.0) * 0.60;
    let run_penalty = (longest_run as f64 / 25.0).min(1.0) * 0.20;
    let underrun_penalty = (underruns as f64 / 20.0).min(1.0) * 0.10;
    let jump_penalty = (energy_jumps as f64 / frames * 4.0).min(1.0) * 0.10;
    (1.0 - conceal_penalty - run_penalty - underrun_penalty - jump_penalty).clamp(0.0, 1.0)
}

fn frame_rms(pcm: &[i16]) -> f64 {
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

#[derive(Debug, Error)]
pub enum MetricsError {
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("WAV error: {0}")]
    Wav(#[from] hound::Error),
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::FRAME_SAMPLES;

    #[test]
    fn continuity_score_decreases_with_bad_events() {
        let clean = score(1_500, 0, 0, 0, 0);
        let impaired = score(1_500, 300, 20, 8, 100);
        assert!(clean > impaired);
        assert!((0.0..=1.0).contains(&clean));
    }

    #[test]
    fn water_series_stays_bounded() {
        let path = std::env::temp_dir().join("jitter_relay_metrics_test.wav");
        let mut metrics = MetricsWriter::create(&path, 10_000).unwrap();
        for index in 0..MAX_WATER_SAMPLES * 3 {
            metrics.push_sample(&TickRecord {
                time_ms: index as u64 * 20,
                water_ms: 100,
                target_ms: 120,
                arrivals: index as u64,
                late: 0,
                duplicate: 0,
                concealed: 0,
                concealed_frame: false,
                underrun_started: false,
                frame: None,
            });
        }
        assert!(metrics.water.len() <= MAX_WATER_SAMPLES);
        let counts = PacketCounts::default();
        let report = metrics.finish(counts, 120, 120).unwrap();
        assert!(report.water.len() <= MAX_WATER_SAMPLES);
        let _ = fs::remove_file(path);
    }

    #[test]
    fn wav_and_report_round_trip() {
        let dir = std::env::temp_dir().join("jitter_relay_metrics_roundtrip");
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        let wav = dir.join("out.wav");
        let json = dir.join("report.json");
        let mut metrics = MetricsWriter::create(&wav, 1).unwrap();
        let frame = Frame {
            ssrc: 1,
            seq: 0,
            ts: 0,
            arrival_us: 0,
            pcm: vec![100; FRAME_SAMPLES],
        };
        metrics
            .record(TickRecord {
                time_ms: 0,
                water_ms: 80,
                target_ms: 80,
                arrivals: 1,
                late: 0,
                duplicate: 0,
                concealed: 0,
                concealed_frame: false,
                underrun_started: false,
                frame: Some(&frame),
            })
            .unwrap();
        let report = metrics.finish(PacketCounts::default(), 80, 80).unwrap();
        write_report(&report, &json).unwrap();
        assert_eq!(read_report(&json).unwrap(), report);
        assert!(wav.exists() && json.exists());
        let _ = fs::remove_dir_all(dir);
    }
}
