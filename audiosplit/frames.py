"""Streaming frame metrics used by the detector."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .wavio import WavReader


def linear_to_dbfs(value: np.ndarray | float) -> np.ndarray | float:
    return 20.0 * np.log10(np.maximum(value, 1e-12))


@dataclass
class FrameAnalysis:
    sample_rate: int
    total_samples: int
    frame_length: int
    hop_length: int
    starts: np.ndarray
    rms_dbfs: np.ndarray
    peak_dbfs: np.ndarray
    zcr: np.ndarray


def frame_geometry(sample_rate: int, frame_ms: float, hop_ms: float) -> tuple[int, int]:
    frame_length = max(1, int(round(sample_rate * frame_ms / 1000.0)))
    hop_length = max(1, int(round(sample_rate * hop_ms / 1000.0)))
    return frame_length, hop_length


def analyze_reader(reader: WavReader, frame_ms: float, hop_ms: float) -> FrameAnalysis:
    sample_rate = reader.header.sample_rate
    total = reader.header.total_samples
    frame_length, hop_length = frame_geometry(sample_rate, frame_ms, hop_ms)
    starts: list[int] = []
    rms_values: list[float] = []
    peak_values: list[float] = []
    zcr_values: list[float] = []
    buffer = np.empty(0, dtype=np.float32)
    buffer_start = 0

    def measure(frame: np.ndarray, start: int) -> None:
        if frame.size:
            energy = float(np.dot(frame, frame) / frame.size)
            peak = float(np.max(np.abs(frame)))
        else:
            energy = 0.0
            peak = 0.0
        starts.append(start)
        rms_values.append(float(linear_to_dbfs(np.sqrt(energy))))
        peak_values.append(float(linear_to_dbfs(peak)))
        if frame.size > 1:
            signs = np.signbit(frame)
            zcr_values.append(float(np.count_nonzero(signs[1:] != signs[:-1]) / (frame.size - 1)))
        else:
            zcr_values.append(0.0)

    for chunk in reader.iter_mono(block_frames=65536):
        buffer = np.concatenate((buffer, chunk)) if buffer.size else chunk
        while buffer.size >= frame_length:
            measure(buffer[:frame_length], buffer_start)
            buffer = buffer[hop_length:]
            buffer_start += hop_length
    while buffer_start < total:
        measure(buffer[: min(frame_length, total - buffer_start)], buffer_start)
        buffer = buffer[hop_length:]
        buffer_start += hop_length

    return FrameAnalysis(
        sample_rate=sample_rate,
        total_samples=total,
        frame_length=frame_length,
        hop_length=hop_length,
        starts=np.asarray(starts, dtype=np.int64),
        rms_dbfs=np.asarray(rms_values, dtype=np.float32),
        peak_dbfs=np.asarray(peak_values, dtype=np.float32),
        zcr=np.asarray(zcr_values, dtype=np.float32),
    )

