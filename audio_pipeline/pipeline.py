"""Two-pass, bounded-memory processing primitives for a single file."""

from __future__ import annotations

from pathlib import Path

import numpy as np

from .io_block import BlockReader, BlockWriter
from .loudness import LoudnessMeter, RmsMeter
from .normalize import gain_amplitude
from .resampler import Resampler, output_frame_count
from .truepeak import ClipCounter, TruePeakMeter


MAX_IO_BLOCK = 32_768


def _io_block(block_size: int) -> int:
    return max(1, min(int(block_size), MAX_IO_BLOCK))


def to_stereo(block: np.ndarray) -> np.ndarray:
    data = np.asarray(block, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.shape[1] == 2:
        return np.ascontiguousarray(data)
    if data.shape[1] == 1:
        return np.ascontiguousarray(np.repeat(data, 2, axis=1))
    raise ValueError("only mono and stereo are supported")


def _consume(block: np.ndarray, meters: tuple) -> None:
    for meter in meters:
        meter.process(block)


def first_pass(
    reader: BlockReader,
    target_rate: int,
    block_size: int,
    quality: str,
    temp_path: Path,
) -> tuple[int, dict]:
    resampler = Resampler(reader.rate, target_rate, quality)
    expected = (
        reader.frames
        if resampler.kernel is None
        else output_frame_count(reader.frames, resampler.kernel)
    )
    mapped = np.memmap(temp_path, dtype=np.float64, mode="w+", shape=(expected, 2))
    loudness = LoudnessMeter(target_rate, 2)
    peak = TruePeakMeter(2)
    rms = RmsMeter(2)
    clips = ClipCounter()
    meters = (loudness, peak, rms, clips)
    position = 0
    try:
        for source in reader.blocks(_io_block(block_size)):
            block = resampler.push(source)
            if len(block):
                stereo = to_stereo(block)
                mapped[position : position + len(stereo)] = stereo
                position += len(stereo)
                _consume(stereo, meters)
        tail = resampler.flush()
        if len(tail):
            stereo = to_stereo(tail)
            mapped[position : position + len(stereo)] = stereo
            position += len(stereo)
            _consume(stereo, meters)
        mapped.flush()
    finally:
        del mapped
    if position != expected:
        raise RuntimeError(f"resampler length mismatch: {position} != {expected}")
    return position, {
        "lufs": loudness.finalize(),
        "true_peak": peak.finalize(),
        "rms": rms.finalize(),
        "clipped": clips.finalize(),
        "fingerprint": resampler.fingerprint,
    }


def second_pass(
    temp_path: Path,
    frames: int,
    gain_db: float,
    output_path: Path,
    rate: int,
    block_size: int,
) -> dict:
    mapped = np.memmap(temp_path, dtype=np.float64, mode="r", shape=(frames, 2))
    loudness = LoudnessMeter(rate, 2)
    peak = TruePeakMeter(2)
    rms = RmsMeter(2)
    clips = ClipCounter()
    meters = (loudness, peak, rms, clips)
    amplitude = gain_amplitude(gain_db)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        with BlockWriter(output_path, rate, 2, "PCM_24") as writer:
            step = _io_block(block_size)
            for start in range(0, frames, step):
                block = np.array(
                    mapped[start : start + step], dtype=np.float64, copy=True
                )
                block *= amplitude
                writer.write_block(block)
                _consume(block, meters)
    finally:
        del mapped
    return {
        "lufs": loudness.finalize(),
        "true_peak": peak.finalize(),
        "rms": rms.finalize(),
        "clipped": clips.finalize(),
    }
