"""Deterministic synthetic media generation for the archive fixtures.

All fixtures are generated locally from seeded sine tones -- no external media
is referenced.  Two containers are used, matching what the recovery pipeline
must accept on cold storage:

* ADTS AAC (``.aac``) for HLS media playlists,
* fragmented MP4 (``.m4s``) for DASH SegmentTemplate media segments.

The encoders write fixed bit-rate settings and aligned frame sizes so the
bytes are reproducible across runs and machines.
"""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path

import av
import numpy as np

AUDIO_SEED = 13306


def tone_stereo(
    rate: int, seconds: float, frequency: float, amplitude: float = 0.3
) -> np.ndarray:
    """Seeded, deterministic stereo tone with a quiet right channel."""
    rng = np.random.default_rng(AUDIO_SEED + rate + int(frequency))
    count = int(round(rate * seconds))
    t = np.arange(count, dtype=np.float64) / rate
    left = amplitude * np.sin(2.0 * np.pi * frequency * t)
    right = 0.7 * left + 0.01 * rng.standard_normal(count)
    return np.stack([left, right], axis=1).astype(np.float32)


def _frame(samples: np.ndarray, rate: int, layout: str, pts: int) -> av.AudioFrame:
    frame = av.AudioFrame.from_ndarray(
        np.ascontiguousarray(samples.T).reshape(1, -1),
        format="flt",
        layout=layout,
    )
    frame.sample_rate = rate
    frame.time_base = Fraction(1, rate)
    frame.pts = pts
    return frame


def encode_adts(
    path: Path,
    samples: np.ndarray,
    rate: int,
    *,
    layout: str = "stereo",
    bit_rate: int = 96_000,
) -> None:
    """Encode a standalone ADTS AAC file (HLS segment)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    container = av.open(str(path), "w", format="adts")
    stream = container.add_stream("aac", rate=rate)
    stream.layout = layout
    stream.bit_rate = bit_rate
    frame_size = 1024
    total = samples.shape[0]
    padded = int(np.ceil(total / frame_size) * frame_size)
    if padded > total:
        samples = np.concatenate(
            [samples, np.zeros((padded - total, samples.shape[1]), np.float32)]
        )
    for offset in range(0, padded, frame_size):
        for packet in stream.encode(
            _frame(samples[offset : offset + frame_size], rate, layout, offset)
        ):
            container.mux(packet)
    for packet in stream.encode(None):
        container.mux(packet)
    container.close()


def encode_fragmented_mp4(
    path: Path,
    samples: np.ndarray,
    rate: int,
    *,
    layout: str = "stereo",
    bit_rate: int = 96_000,
) -> None:
    """Encode a self-contained fragmented MP4 (DASH media segment)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    # Fragmented output keeps every segment independently decodable.
    container = av.open(
        str(path),
        "w",
        format="mp4",
        options={"movflags": "frag_keyframe+empty_moov+default_base_moof"},
    )
    stream = container.add_stream("aac", rate=rate)
    stream.layout = layout
    stream.bit_rate = bit_rate
    frame_size = 1024
    total = samples.shape[0]
    padded = int(np.ceil(total / frame_size) * frame_size)
    if padded > total:
        samples = np.concatenate(
            [samples, np.zeros((padded - total, samples.shape[1]), np.float32)]
        )
    for offset in range(0, padded, frame_size):
        for packet in stream.encode(
            _frame(samples[offset : offset + frame_size], rate, layout, offset)
        ):
            container.mux(packet)
    for packet in stream.encode(None):
        container.mux(packet)
    container.close()


def corrupt_packet_region(path: Path, *, keep_fraction: float = 0.6) -> int:
    """Truncate the tail of a segment to simulate packet/container damage."""
    data = path.read_bytes()
    cut = max(1, int(len(data) * keep_fraction))
    path.write_bytes(data[:cut])
    return len(data) - cut


def flip_bytes(path: Path, offsets: tuple[int, ...] = (8, 40, 72)) -> None:
    """Flip bits inside the segment body to create undecodable packets."""
    data = bytearray(path.read_bytes())
    for offset in offsets:
        if offset < len(data):
            data[offset] ^= 0xFF
    path.write_bytes(bytes(data))


__all__ = [
    "AUDIO_SEED",
    "corrupt_packet_region",
    "encode_adts",
    "encode_fragmented_mp4",
    "flip_bytes",
    "tone_stereo",
]
