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
import struct
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


def _riff_data_extent(path: Path) -> tuple[int, int] | None:
    """Return (data_offset, declared_data_bytes) from the RIFF/WAVE header.

    libsndfile heals its frame count for truncated files, so truncation is
    detected by comparing the header's declared data-chunk size with the
    actual file size.  Returns None for non-RIFF containers.
    """
    try:
        with open(path, "rb") as f:
            riff = f.read(12)
            if len(riff) < 12 or riff[:4] not in (b"RIFF", b"RF64") or riff[8:12] != b"WAVE":
                return None
            while True:
                header = f.read(8)
                if len(header) < 8:
                    return None
                chunk_id, chunk_size = header[:4], struct.unpack("<I", header[4:])[0]
                if chunk_id == b"data":
                    return f.tell(), chunk_size
                f.seek(chunk_size + (chunk_size & 1), os.SEEK_CUR)
    except OSError:
        return None


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
        riff = _riff_data_extent(self.path)
        if riff is not None:
            # Trust the RIFF header over libsndfile's self-healing frame count:
            # a truncated file declares more data than it carries.
            data_offset, declared_bytes = riff
            readable = max(0, (size - data_offset) // bytes_per_frame)
            declared_frames = declared_bytes // bytes_per_frame
            self.frames = int(min(readable, declared, max(declared_frames, readable)))
            if size - data_offset < declared_bytes:
                self.truncated_at = int(readable)
        else:
            self.frames = int(declared)

    # Interpolation may ask for a frame or two past EOF; that much slack is
    # zero-padded.  Anything further is corruption or a timeline mismatch.
    SLACK_FRAMES = 2

    def read_range(self, start: int, frames: int) -> Block:
        """Read [start, start+frames) of track-file samples."""
        assert self._sf is not None, "reader not opened"
        if frames <= 0:
            return Block(self.track.id, start, 0, np.zeros((0, self.channels), np.float32))
        end = start + frames
        if end > self.frames + self.SLACK_FRAMES:
            if self.truncated_at is not None:
                return self._corrupt(
                    "WAV_TRUNCATED",
                    f"{self.name}: data ends at frame {self.truncated_at}, need {end}",
                    start, frames,
                )
            raise MediaError(
                "TIMELINE_BEYOND_EOF",
                f"{self.name}: timeline needs frame {end} but file has {self.frames}",
                stage="ingest", track=self.track.id, sample_offset=self.frames,
                path=self.name,
            )
        pad = max(0, end - self.frames)
        want = frames - pad
        self._sf.seek(start)
        try:
            data = self._sf.read(want, dtype="float32", always_2d=True)
        except (sf.LibsndfileError, RuntimeError) as e:
            return self._corrupt("WAV_READ_ERROR", f"{self.name}: read failed: {e}", start, frames)
        if data.shape[0] < want:
            return self._corrupt(
                "WAV_TRUNCATED",
                f"{self.name}: short read at frame {start + data.shape[0]} "
                f"(wanted {want}, got {data.shape[0]})",
                start + data.shape[0], frames - data.shape[0],
            )
        if pad:
            data = np.concatenate(
                [data, np.zeros((pad, self.channels), np.float32)], axis=0
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
