"""Bounded ingestion of raw elementary streams, keyframe indexes and audio.

The loader never slurps an entire media file into memory.  It validates every
index entry against declared and on-disk bounds, sniffs the elementary stream
codec and refuses streams that this build cannot decode.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

import soundfile as sf

from .clock import TICKS_PER_SECOND, parse_fps, ticks_from_samples
from .errors import (
    CodecConflictError,
    DependencyError,
    IndexError_,
    InputError,
    StreamError,
)

SUPPORTED_VIDEO_CODECS = ("h264", "hevc", "av1")
START_CODES = (b"\x00\x00\x01", b"\x00\x00\x00\x01")


@dataclass(frozen=True)
class Limits:
    """Hard caps that keep a single run bounded in memory and time."""

    max_stream_bytes: int = 64 * 1024 * 1024
    max_index_bytes: int = 8 * 1024 * 1024
    max_index_entries: int = 50_000
    max_tracks: int = 8
    max_audio_seconds: float = 120.0
    max_audio_channels: int = 8


@dataclass(frozen=True)
class PacketRecord:
    index: int
    offset: int
    size: int
    keyframe: bool
    pts_ticks: int
    duration_ticks: int
    seq: int = 0

    @property
    def end_offset(self) -> int:
        return self.offset + self.size

    @property
    def end_ticks(self) -> int:
        return self.pts_ticks + self.duration_ticks

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "offset": self.offset,
            "size": self.size,
            "keyframe": self.keyframe,
            "pts_ticks": self.pts_ticks,
            "duration_ticks": self.duration_ticks,
            "seq": self.seq,
        }


@dataclass(frozen=True)
class Anchor:
    """A known synchronization event in capture index space."""

    label: str
    video_index: int | None = None
    audio_sample: int | None = None


@dataclass
class VideoTrack:
    track_id: str
    role: str
    codec: str
    path: Path
    stream_size: int
    nominal_rate: Fraction
    width: int
    height: int
    packets: tuple[PacketRecord, ...]
    anchors: tuple[Anchor, ...] = ()
    truncated_indices: frozenset[int] = frozenset()
    declared_stream_size: int = 0

    @property
    def duration_ticks(self) -> int:
        if not self.packets:
            return 0
        return self.packets[-1].end_ticks - self.packets[0].pts_ticks

    @property
    def keyframes(self) -> tuple[PacketRecord, ...]:
        return tuple(packet for packet in self.packets if packet.keyframe)

    def packet_by_seq(self) -> dict[int, PacketRecord]:
        return {packet.seq: packet for packet in self.packets}


@dataclass
class AudioTrack:
    track_id: str
    role: str
    path: Path
    sample_rate: int
    channels: int
    frames: int
    start_pts_ticks: int
    anchors: tuple[Anchor, ...] = ()
    samples: Any = None
    _peak: float | None = field(default=None, repr=False)
    _rms: float | None = field(default=None, repr=False)

    @property
    def duration_ticks(self) -> int:
        return ticks_from_samples(self.frames, self.sample_rate)

    def read_samples(self, limits: Limits) -> Any:
        if self.samples is None:
            import numpy as np

            data, rate = sf.read(str(self.path), dtype="float32", always_2d=True)
            if rate != self.sample_rate:
                raise StreamError(
                    f"{self.track_id}: sample rate changed to {rate} after ingest"
                )
            if data.shape[0] > int(limits.max_audio_seconds * rate):
                raise InputError(f"{self.track_id}: audio exceeds duration limit")
            self.samples = np.ascontiguousarray(data.T)
        return self.samples

    def peak_rms(self, limits: Limits) -> tuple[float, float]:
        import numpy as np

        if self._peak is None or self._rms is None:
            samples = self.read_samples(limits)
            if samples.size == 0:
                self._peak, self._rms = 0.0, 0.0
            else:
                self._peak = float(np.max(np.abs(samples)))
                self._rms = float(np.sqrt(np.mean(np.square(samples, dtype=np.float64))))
        return self._peak, self._rms
