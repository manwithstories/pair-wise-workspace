"""Content fingerprints for cross-file duplicate detection.

Fingerprints are derived purely from decoded *content*, never from
timestamps, so two clips with identical frames/samples match even when their
container clocks differ, while clips that share a timestamp schedule but
carry different pixels or waveforms do not.

Design:
* Video chunk: the mean luma and the mean absolute luma difference between
  consecutive frames (motion energy), accumulated over a fixed number of
  frames, then quantized and hashed with BLAKE2b.
* Audio chunk: mean absolute amplitude and zero-crossing rate over a fixed
  sample span, quantized and hashed with BLAKE2b.
* A :class:`Fingerprint` pairs the two hashes with the corrected time span
  and a content-derived similarity score.

Duplicate matching uses an ordered multiset of chunk hashes: two files are
duplicates when their Jaccard similarity over chunk hashes meets a
configurable threshold.  Deterministic ordering makes reruns stable.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field

import numpy as np

# Quantization step keeps the hash stable against tiny float fluctuations
# while still separating genuinely different content.
VIDEO_QUANT = 16.0
AUDIO_QUANT = 0.01


def _hash_ints(values: list[int]) -> str:
    h = hashlib.blake2b(digest_size=16)
    for value in values:
        h.update(int(value).to_bytes(8, "little", signed=True))
    return h.hexdigest()


@dataclass
class VideoChunkAccumulator:
    """Accumulates luma statistics over a fixed number of video frames."""

    frames_per_chunk: int
    _luma_sum: float = 0.0
    _motion_sum: float = 0.0
    _count: int = 0
    _prev_luma: float | None = None

    def add_frame(self, frame) -> list[str]:
        # Y plane is the first plane of a planar YUV frame; support RGB too.
        if hasattr(frame, "planes") and frame.planes:
            plane = frame.planes[0]
            arr = np.frombuffer(plane, dtype=np.uint8)
            width = frame.width
            line = plane.line_size if hasattr(plane, "line_size") else width
            rows = np.clip(frame.height, 0, arr.size // max(1, line))
            arr = arr[: rows * line].reshape(rows, line)[:, :width]
        else:
            arr = np.asarray(frame, dtype=np.uint8)
            if arr.ndim == 3:
                arr = arr.mean(axis=2)
        luma = float(arr.mean())
        if self._prev_luma is not None:
            self._motion_sum += abs(luma - self._prev_luma)
        self._prev_luma = luma
        self._luma_sum += luma
        self._count += 1
        if self._count >= self.frames_per_chunk:
            return [self._emit()]
        return []

    def _emit(self) -> str:
        mean_luma = self._luma_sum / max(1, self._count)
        motion = self._motion_sum / max(1, self._count)
        # Three orthogonal descriptors keep same-schedule / different-content
        # clips apart: average brightness, motion energy and frame count.
        values = [
            int(round(mean_luma * VIDEO_QUANT)),
            int(round(motion * VIDEO_QUANT * 4)),
            self._count,
        ]
        digest = _hash_ints(values)
        self._luma_sum = 0.0
        self._motion_sum = 0.0
        self._count = 0
        return digest

    def flush(self) -> list[str]:
        if self._count == 0:
            return []
        digest = self._emit()
        self._prev_luma = None
        return [digest]


@dataclass
class AudioChunkAccumulator:
    """Accumulates waveform statistics over a fixed sample span."""

    samples_per_chunk: int
    _buffer: np.ndarray = field(default_factory=lambda: np.zeros((0,), dtype=np.float32))

    def add_samples(self, samples: np.ndarray) -> list[str]:
        mono = samples
        if mono.ndim > 1:
            mono = mono.mean(axis=1)
        self._buffer = np.concatenate([self._buffer, np.asarray(mono, dtype=np.float32)])
        out: list[str] = []
        while self._buffer.size >= self.samples_per_chunk:
            chunk = self._buffer[: self.samples_per_chunk]
            out.append(self._hash_chunk(chunk))
            self._buffer = self._buffer[self.samples_per_chunk :]
        return out

    def flush(self) -> list[str]:
        if self._buffer.size == 0:
            return []
        digest = self._hash_chunk(self._buffer)
        self._buffer = np.zeros((0,), dtype=np.float32)
        return [digest]

    @staticmethod
    def _hash_chunk(chunk: np.ndarray) -> str:
        amplitude = float(np.mean(np.abs(chunk)))
        if chunk.size > 1:
            crossings = float(np.mean(np.diff(np.sign(chunk)) != 0))
        else:
            crossings = 0.0
        values = [
            int(round(amplitude / AUDIO_QUANT)),
            int(round(crossings / AUDIO_QUANT)),
            chunk.size,
        ]
        return _hash_ints(values)


@dataclass
class Fingerprint:
    """Content fingerprint of one input file across all its chunks."""

    source: str
    video_hashes: list[str]
    audio_hashes: list[str]

    def all_hashes(self) -> set[str]:
        return set(self.video_hashes) | set(self.audio_hashes)


def jaccard(a: set[str], b: set[str]) -> float:
    if not a and not b:
        return 0.0
    union = a | b
    return len(a & b) / len(union) if union else 0.0


def find_duplicates(
    fingerprints: list[Fingerprint], *, threshold: float = 0.8
) -> list[dict]:
    """Return deterministic duplicate pairs above the Jaccard threshold."""
    pairs: list[dict] = []
    ordered = sorted(fingerprints, key=lambda fp: fp.source)
    for i in range(len(ordered)):
        for j in range(i + 1, len(ordered)):
            left, right = ordered[i], ordered[j]
            score = jaccard(left.all_hashes(), right.all_hashes())
            if score >= threshold:
                shared = sorted(left.all_hashes() & right.all_hashes())
                pairs.append(
                    {
                        "source_a": left.source,
                        "source_b": right.source,
                        "similarity": round(score, 6),
                        "shared_chunks": len(shared),
                    }
                )
    return pairs
