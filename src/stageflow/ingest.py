"""Block-wise WAV ingest via soundfile.

TrackReader opens one input WAV, enforces hard limits, and serves sample
ranges to the mixer.  Corruption is reported with file, track and sample
offset:

  WAV_HEADER_CORRUPT  unparseable header / unsupported container
  WAV_TRUNCATED       data chunk shorter than the header declares
  NONFINITE_SAMPLE    NaN/Inf samples in a block
  LIMIT_*             sample rate / channel count / file size over budget

With on_corrupt="mute" a corrupt block is replaced by zeros and the
replaced range (in track file samples) is recorded in muted_ranges so the
manifest can mark the substitution; with "fail" the MediaError propagates.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np
import soundfile as sf

from .errors import MediaError
from .timeline import MAX_CHANNELS, MAX_FILE_BYTES, MAX_SAMPLE_RATE, MIN_SAMPLE_RATE, Track

SUBTYPE_BYTES = {
    "PCM_S8": 1, "PCM_16": 2, "PCM_24": 3, "PCM_32": 4,
    "PCM_U8": 1, "FLOAT": 4, "DOUBLE": 8,
}


@dataclass
class Block:
    """One chunk of track audio in track-file sample coordinates."""

    track_id: str
    start_sample: int
    frames: int
    data: np.ndarray  # (frames, channels) float32
    muted: bool = False


@dataclass
class MutedRange:
    track_id: str
    start_sample: int
    frames: int
    reason: str


@dataclass
class TrackReader:
    track: Track
    path: Path
    on_corrupt: str = "fail"  # "fail" | "mute"
    muted_ranges: list[MutedRange] = field(default_factory=list)

    def __post_init__(self) -> None:
        self._sf: sf.SoundFile | None = None
        self.samplerate = 0
        self.channels = 0
        self.frames = 0  # frames actually readable
        self.truncated_at: int | None = None  # first missing frame if truncated

    @property
    def name(self) -> str:
        return self.path.name

    def open(self) -> None:
        try:
            size = os.path.getsize(self.path)
        except FileNotFoundError:
            raise MediaError(
                "INPUT_NOT_FOUND", f"input file not found: {self.path.name}",
                stage="ingest", track=self.track.id, path=self.path.name,
            )
        if size > MAX_FILE_BYTES:
            raise MediaError(
                "LIMIT_FILE_SIZE",
                f"{self.path.name}: {size} bytes exceeds limit {MAX_FILE_BYTES}",
                stage="ingest", track=self.track.id, path=self.path.name,
            )
        try:
            self._sf = sf.SoundFile(self.path)
        except sf.LibsndfileError as e:
            raise MediaError(
                "WAV_HEADER_CORRUPT", f"{self.path.name}: unreadable WAV header: {e}",
                stage="ingest", track=self.track.id, sample_offset=0, path=self.path.name,
            )
        self.samplerate = self._sf.samplerate
        self.channels = self._sf.channels
        if not (MIN_SAMPLE_RATE <= self.samplerate <= MAX_SAMPLE_RATE):
            raise MediaError(
                "LIMIT_SAMPLE_RATE",
                f"{self.path.name}: sample rate {self.samplerate} outside "
                f"[{MIN_SAMPLE_RATE},{MAX_SAMPLE_RATE}]",
                stage="ingest", track=self.track.id, path=self.path.name,
            )
        if not (1 <= self.channels <= MAX_CHANNELS):
            raise MediaError(
                "LIMIT_CHANNELS",
                f"{self.path.name}: {self.channels} channels exceeds limit {MAX_CHANNELS}",
                stage="ingest", track=self.track.id, path=self.path.name,
            )
        declared = self._sf.frames
        bytes_per_frame = SUBTYPE_BYTES.get(self._sf.subtype, 4) * self.channels
        data_bytes = size - self._data_offset_guess(size, declared, bytes_per_frame)
        readable = max(0, min(declared, data_bytes // bytes_per_frame))
        self.frames = int(readable)
        if readable < declared:
            self.truncated_at = int(readable)

    def _data_offset_guess(self, size: int, declared: int, bpf: int) -> int:
        # Header size = file size - declared data size when the file is whole;
        # clamp so a truncated file still yields a sane offset.
        offset = size - declared * bpf
        return offset if 0 <= offset <= size else 0

    def read_range(self, start: int, frames: int) -> Block:
        """Read [start, start+frames) of track-file samples."""
        assert self._sf is not None, "reader not opened"
        if frames <= 0:
            return Block(self.track.id, start, 0, np.zeros((0, self.channels), np.float32))
        end = start + frames
        if self.truncated_at is not None and end > self.truncated_at:
            return self._corrupt(
                "WAV_TRUNCATED",
                f"{self.name}: data ends at frame {self.truncated_at}, need {end}",
                start, frames,
            )
        self._sf.seek(start)
        try:
            data = self._sf.read(frames, dtype="float32", always_2d=True)
        except (sf.LibsndfileError, RuntimeError) as e:
            return self._corrupt("WAV_READ_ERROR", f"{self.name}: read failed: {e}", start, frames)
        if data.shape[0] < frames:
            return self._corrupt(
                "WAV_TRUNCATED",
                f"{self.name}: short read at frame {start + data.shape[0]} "
                f"(wanted {frames}, got {data.shape[0]})",
                start + data.shape[0], frames - data.shape[0],
            )
        if not np.isfinite(data).all():
            bad = start + int(np.argwhere(~np.isfinite(data).all(axis=1))[0, 0])
            return self._corrupt(
                "NONFINITE_SAMPLE", f"{self.name}: NaN/Inf sample at frame {bad}", start, frames,
                offset=bad,
            )
        return Block(self.track.id, start, frames, data)

    def _corrupt(
        self, code: str, msg: str, start: int, frames: int, offset: int | None = None
    ) -> Block:
        if self.on_corrupt == "mute":
            self.muted_ranges.append(MutedRange(self.track.id, start, frames, code))
            return Block(self.track.id, start, frames,
                         np.zeros((frames, self.channels), np.float32), muted=True)
        raise MediaError(
            code, msg, stage="ingest", track=self.track.id,
            sample_offset=offset if offset is not None else start, path=self.name,
        )

    def close(self) -> None:
        if self._sf is not None:
            self._sf.close()
            self._sf = None
