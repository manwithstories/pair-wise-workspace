"""Streaming DC removal, framing and adaptive noise-floor estimation."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .riff import WavReader


FRAME_MS = 10
NOISE_WINDOW_SECONDS = 30
NOISE_PERCENTILE = 15


@dataclass
class Envelope:
    sample_rate: int
    total_samples: int
    frame_samples: int
    rms: np.ndarray
    peak: np.ndarray
    channel_rms: np.ndarray
    channel_peak: np.ndarray
    channel_mode: str

    @property
    def frame_count(self) -> int:
        return int(self.rms.size)

    @property
    def frame_ms(self) -> float:
        return 1000.0 * self.frame_samples / self.sample_rate

    def db(self) -> np.ndarray:
        return to_db(self.rms)


def to_db(values: np.ndarray, floor_db: float = -120.0) -> np.ndarray:
    power = np.asarray(values, dtype=np.float64) ** 2
    return np.maximum(10.0 * np.log10(np.maximum(power, 10.0 ** (floor_db / 10.0))), floor_db)


def _block_features(block: np.ndarray, frame_samples: int) -> tuple[np.ndarray, np.ndarray]:
    frame_count = int(np.ceil(block.shape[0] / frame_samples))
    padded = np.zeros((frame_count * frame_samples, block.shape[1]), dtype=np.float32)
    padded[: block.shape[0]] = block
    frames = padded.reshape(frame_count, frame_samples, block.shape[1])
    energy = np.mean(frames.astype(np.float64) ** 2, axis=1)
    rms = np.sqrt(energy).astype(np.float32)
    peak = np.max(np.abs(frames), axis=1).astype(np.float32)
    return rms, peak


def scan_envelopes(reader: WavReader, frame_ms: int = FRAME_MS) -> Envelope:
    """Read a WAV block-by-block and return a compact per-frame envelope."""

    if not reader.ready or reader.info is None:
        raise RuntimeError("reader is not open")
    info = reader.info
    frame_samples = max(1, int(round(info.sample_rate * frame_ms / 1000.0)))
    target = max(frame_samples, (info.sample_rate * 2 // frame_samples) * frame_samples)
    rms_parts: list[np.ndarray] = []
    peak_parts: list[np.ndarray] = []
    seen = 0
    for block in reader.iter_frames(block_frames=target):
        clean = block.astype(np.float32, copy=False)
        if clean.shape[0]:
            clean = clean - np.mean(clean, axis=0, keepdims=True, dtype=np.float64).astype(np.float32)
        rms, peak = _block_features(clean, frame_samples)
        rms_parts.append(rms)
        peak_parts.append(peak)
        seen += block.shape[0]
    if seen != info.total_frames:
        raise RuntimeError("frame count mismatch while scanning")
    channel_rms = np.concatenate(rms_parts, axis=0) if rms_parts else np.empty((0, info.channels), np.float32)
    channel_peak = np.concatenate(peak_parts, axis=0) if peak_parts else np.empty((0, info.channels), np.float32)
    channel_rms = channel_rms[: int(np.ceil(seen / frame_samples))]
    channel_peak = channel_peak[: channel_rms.shape[0]]
    rms = channel_rms.max(axis=1) if channel_rms.shape[0] else np.empty(0, np.float32)
    peak = channel_peak.max(axis=1) if channel_peak.shape[0] else np.empty(0, np.float32)
    channel_mode = "mono"
    if info.channels == 2:
        channel_db = to_db(channel_rms.max(axis=0))
        channel_mode = "stereo" if min(channel_db) >= max(channel_db) - 20.0 else "mono"
    return Envelope(
        sample_rate=info.sample_rate,
        total_samples=seen,
        frame_samples=frame_samples,
        rms=rms.astype(np.float32),
        peak=peak.astype(np.float32),
        channel_rms=channel_rms,
        channel_peak=channel_peak,
        channel_mode=channel_mode,
    )


def noise_floor_db(
    rms: np.ndarray,
    sample_rate: int,
    frame_samples: int,
    window_seconds: float = NOISE_WINDOW_SECONDS,
) -> np.ndarray:
    """Estimate a smooth floor from the 15th percentile in 30-second windows."""

    rms = np.asarray(rms, dtype=np.float32)
    if rms.size == 0:
        return np.empty(0, dtype=np.float32)
    frames_per_window = max(1, int(round(window_seconds * sample_rate / frame_samples)))
    centers: list[float] = []
    floors: list[float] = []
    power = rms.astype(np.float64) ** 2
    for start in range(0, rms.size, frames_per_window):
        end = min(rms.size, start + frames_per_window)
        value = float(np.percentile(power[start:end], NOISE_PERCENTILE))
        centers.append((start + end - 1) / 2.0)
        floors.append(max(-120.0, 10.0 * np.log10(max(value, 1e-12))))
    if len(centers) == 1:
        return np.full(rms.size, floors[0], dtype=np.float32)
    x = np.arange(rms.size, dtype=np.float64)
    return np.interp(x, np.asarray(centers), np.asarray(floors)).astype(np.float32)
