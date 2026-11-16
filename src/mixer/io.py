"""WAV I/O: header probing, source validation, block reads and master writing.

Sources are opened through ``soundfile`` but never fully materialised — the
engine pulls one destination block's worth at a time. Before a single sample is
mixed the header is parsed with the standard library so a truncated file is
named by track instead of silently reading as zeros.
"""

from __future__ import annotations

import json
import math
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

import numpy as np
import soundfile as sf

from .errors import MixerError, EXIT_FORMAT, format_error
from .model import ChannelLayout
from .resample import validate_rate

_RIFF_HEADER = 12
_MAX_SCAN_SECONDS = 900.0


@dataclass(frozen=True)
class WavHeader:
    audio_format: int
    channels: int
    rate: int
    bits: int
    data_size: int
    file_size: int

    @property
    def frames(self) -> int:
        bytes_per_frame = max(1, self.channels * max(1, self.bits // 8))
        return self.data_size // bytes_per_frame



def probe_wav(path: str | Path, track: str | None = None) -> WavHeader:
    """Parse the RIFF/fmt chunks to spot truncation and unsupported formats."""
    file_path = Path(path)
    if not file_path.exists():
        raise format_error("file_not_found", f"audio file not found: {file_path}", track=track)
    size = file_path.stat().st_size
    with file_path.open("rb") as handle:
        header = handle.read(_RIFF_HEADER)
        if len(header) < _RIFF_HEADER:
            raise format_error(
                "truncated_file", f"{file_path.name} is shorter than a RIFF header", track=track
            )
        tag, riff_size, wave_tag = struct.unpack("<4sI4s", header[:12])
        if tag not in (b"RIFF", b"RIFX") or wave_tag != b"WAVE":
            raise format_error(
                "invalid_wav", f"{file_path.name} is not a RIFF/WAVE file", track=track
            )
        if riff_size + 8 > size:
            raise format_error(
                "truncated_file",
                f"{file_path.name} declares {riff_size + 8} bytes but has {size}",
                track=track,
                details={"declared": riff_size + 8, "actual": size},
            )
        audio_format = channels = rate = bits = 0
        data_size = 0
        seen_fmt = False
        seen_data = False
        while True:
            chunk_header = handle.read(8)
            if len(chunk_header) < 8:
                break
            chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
            if chunk_id == b"fmt " and chunk_size >= 16:
                payload = handle.read(chunk_size)
                audio_format, channels, rate = struct.unpack("<HHI", payload[:8])
                bits = struct.unpack("<H", payload[14:16])[0]
                seen_fmt = True
                if chunk_size % 2:
                    handle.seek(1, 1)
            elif chunk_id == b"data":
                data_size = chunk_size
                seen_data = True
                break
            else:
                handle.seek(chunk_size + (chunk_size % 2), 1)
    if not seen_fmt:
        raise format_error("invalid_wav", f"{file_path.name} has no fmt chunk", track=track)
    if not seen_data:
        raise format_error("invalid_wav", f"{file_path.name} has no data chunk", track=track)
    # A zero-length data chunk is a legal, empty source: it renders as silence
    # rather than failing the whole project.
    return WavHeader(audio_format, channels, rate, bits, data_size, size)


class SourceFile:
    """Random-access, validated view of one source WAV.

    Opening validates rate, channel count and finiteness so failures point at
    a track name before rendering starts; ``read`` then streams only the
    requested window.
    """

    def __init__(self, path: str | Path, track: str, layout: ChannelLayout) -> None:
        self.path = Path(path)
        self.track = track
        self.layout = layout
        self.header = probe_wav(self.path, track)
        try:
            self._handle = sf.SoundFile(str(self.path))
        except Exception as exc:  # noqa: BLE001 - surfaced as a format error
            raise format_error(
                "invalid_wav", f"track {track}: cannot open {self.path.name}: {exc}", track=track
            ) from exc
        self.rate = int(self._handle.samplerate)
        self.channels = int(self._handle.channels)
        self.frames = int(self._handle.frames)
        try:
            validate_rate(self.rate, track)
        except MixerError as exc:
            self._handle.close()
            raise
        if self.channels != layout.channels:
            self._handle.close()
            raise format_error(
                "channel_count_mismatch",
                f"track {track}: layout {layout.name!r} expects {layout.channels} channels, "
                f"file has {self.channels}",
                track=track,
                details={"layout": layout.name, "expected": layout.channels, "actual": self.channels},
            )
        if self.frames > int(_MAX_SCAN_SECONDS * self.rate):
            self._handle.close()
            raise format_error(
                "source_too_long",
                f"track {track}: {self.frames / self.rate:.1f}s exceeds the "
                f"{_MAX_SCAN_SECONDS:.0f}s source limit",
                track=track,
            )
        self._validate_samples()

    def _validate_samples(self) -> None:
        """Scan in bounded chunks for NaN/Inf (a corrupted or stub source)."""
        chunk = 1 << 16
        position = 0
        while position < self.frames:
            count = min(chunk, self.frames - position)
            self._handle.seek(position)
            block = self._handle.read(count, dtype="float64", always_2d=True)
            if block.shape[0] == 0:
                break
            if not np.isfinite(block).all():
                self._handle.close()
                bad = int(np.count_nonzero(~np.isfinite(block)))
                raise format_error(
                    "non_finite_samples",
                    f"track {self.track}: {self.path.name} contains {bad} non-finite samples",
                    track=self.track,
                    details={"path": str(self.path), "non_finite": bad},
                )
            position += block.shape[0]
        self._handle.seek(0)

    def read(self, start_frame: int, count: int) -> np.ndarray:
        """Read ``count`` source frames from ``start_frame``, zero-padded."""
        if count <= 0:
            return np.zeros((0, self.channels), dtype=np.float64)
        if start_frame < 0:
            lead = min(-start_frame, count)
            tail = self.read(0, count - lead)
            return np.concatenate((np.zeros((lead, self.channels)), tail), axis=0)
        available = max(0, min(count, self.frames - start_frame))
        out = np.zeros((count, self.channels), dtype=np.float64)
        if available:
            self._handle.seek(start_frame)
            try:
                data = self._handle.read(available, dtype="float64", always_2d=True)
            except Exception as exc:  # noqa: BLE001
                raise format_error(
                    "truncated_file",
                    f"track {self.track}: read past end of {self.path.name}: {exc}",
                    track=self.track,
                ) from exc
            out[: data.shape[0]] = data
            if data.shape[0] < available:
                raise format_error(
                    "truncated_file",
                    f"track {self.track}: {self.path.name} ended early "
                    f"({data.shape[0]}/{available} frames read)",
                    track=self.track,
                )
        return out

    def close(self) -> None:
        if not self._handle.closed:
            self._handle.close()

    def __enter__(self) -> "SourceFile":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()


def fold_to_stereo(block: np.ndarray, matrix: Sequence[Sequence[float]]) -> np.ndarray:
    """Apply a layout channel matrix (channels x 2) to a (frames, channels) block."""
    if block.size == 0:
        return np.zeros((block.shape[0], 2), dtype=np.float64)
    weights = np.asarray(matrix, dtype=np.float64)
    if weights.shape != (block.shape[1], 2):
        raise MixerError(
            "invalid_channel_matrix",
            f"channel matrix {weights.shape} does not match block channels {block.shape[1]}",
            EXIT_FORMAT,
        )
    return block @ weights


def write_master(path: str | Path, audio: np.ndarray, rate: int, subtype: str = "FLOAT") -> dict:
    """Write the stereo master and return a summary of what landed."""
    if audio.ndim != 2 or audio.shape[1] != 2:
        raise MixerError("invalid_master", "master output must be stereo", EXIT_FORMAT)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(target), np.clip(audio, -1.0, 1.0), rate, subtype=subtype, format="WAV")
    return summarize(target)


def read_master(path: str | Path) -> tuple[np.ndarray, int]:
    """Read a rendered master as float64 stereo (frames, 2)."""
    file_path = Path(path)
    if not file_path.exists():
        raise format_error("file_not_found", f"audio file not found: {file_path}")
    try:
        data, rate = sf.read(str(file_path), dtype="float64", always_2d=True)
    except Exception as exc:  # noqa: BLE001
        raise format_error("invalid_wav", f"cannot read {file_path.name}: {exc}") from exc
    if data.shape[1] != 2:
        raise format_error(
            "invalid_wav",
            f"{file_path.name} has {data.shape[1]} channels; expected stereo",
            details={"channels": int(data.shape[1])},
        )
    return data, int(rate)


def summarize(path: str | Path) -> dict[str, Any]:
    """Metadata + level summary for any WAV on disk."""
    file_path = Path(path)
    data, rate = read_master(file_path)
    peak = float(np.abs(data).max()) if data.size else 0.0
    rms = float(np.sqrt(np.mean(np.square(data)))) if data.size else 0.0
    info = sf.info(str(file_path))
    return {
        "path": str(file_path),
        "frames": int(data.shape[0]),
        "sample_rate": rate,
        "channels": int(data.shape[1]),
        "duration_s": round(data.shape[0] / rate, 6) if rate else 0.0,
        "subtype": info.subtype,
        "peak_dbfs": round(20 * math.log10(peak), 4) if peak > 0 else None,
        "rms_dbfs": round(20 * math.log10(rms), 4) if rms > 0 else None,
        "clip_count": int(np.count_nonzero(np.abs(data) >= 0.999)),
    }


def write_project(
    path: str | Path, tracks: list[dict], target_rate: int, extra: dict | None = None
) -> dict:
    """Write a fixture project JSON and return it."""
    payload: dict[str, Any] = {"schema": "mix-project/1", "target_rate": target_rate,
                               "tracks": tracks}
    if extra:
        payload.update(extra)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    return payload
