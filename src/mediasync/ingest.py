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


def _read_header(path: Path, size: int = 8192) -> bytes:
    with path.open("rb") as handle:
        return handle.read(size)


def _first_nal_payloads(data: bytes, limit: int = 6) -> list[int]:
    """Return the first byte after each Annex-B start code (bounded scan)."""

    payloads: list[int] = []
    position = 0
    while len(payloads) < limit:
        candidates = [data.find(code, position) for code in START_CODES]
        candidates = [index for index in candidates if index >= 0]
        if not candidates:
            break
        start = min(candidates)
        code_len = 3 if data.startswith(b"\x00\x00\x01", start) else 4
        payload = start + code_len
        if payload >= len(data):
            break
        payloads.append(data[payload])
        position = payload + 1
    return payloads


def sniff_codec(path: Path) -> str:
    """Classify a raw elementary stream as h264, hevc or av1 from its header.

    Evidence is deliberately conservative: HEVC must expose a VPS or SPS NAL
    unit, H.264 must expose an SPS, and AV1 must start with a low-overhead OBU.
    Anything else is reported as an unexplained encoding type instead of being
    guessed, because a wrong guess would silently mis-time the track.
    """

    data = _read_header(path)
    if not data:
        raise StreamError(f"{path.name}: empty elementary stream")
    # AV1 low-overhead OBU streams start with a temporal delimiter or a
    # sequence header OBU.
    if data[0] in (0x12, 0x0A, 0x32) and data[0] != 0x0A:
        return "av1"
    payloads = _first_nal_payloads(data, limit=200)
    if not payloads:
        raise StreamError(f"{path.name}: no Annex-B start code found")
    hevc_types = {value >> 1 for value in payloads if not value & 0x80}
    h264_types = {value & 0x1F for value in payloads if not value & 0x80}
    is_hevc = bool(hevc_types & {32, 33}) and bool(h264_types & {7}) is False
    is_h264 = 7 in h264_types and 8 in h264_types
    if is_hevc and is_h264:
        raise CodecConflictError(
            f"{path.name}: stream carries both H.264 and HEVC parameter sets"
        )
    if is_hevc:
        return "hevc"
    if is_h264:
        return "h264"
    raise CodecConflictError(
        f"{path.name}: cannot explain elementary stream codec from byte header"
    )


def _verify_decodable(path: Path, codec: str, limits: Limits) -> None:
    """Decode at most one frame to prove the stream matches its declared codec."""

    try:
        import av
    except ImportError as exc:  # pragma: no cover - environment guard
        raise DependencyError("PyAV is required for stream ingestion") from exc

    try:
        container = av.open(str(path), format=codec)
    except Exception as exc:  # noqa: BLE001 - PyAV raises FFmpeg error types
        raise CodecConflictError(
            f"{path.name}: cannot open as {codec} ({exc})"
        ) from exc
    try:
        video = container.streams.video[0]
        decoded = 0
        for frame in container.decode(video):
            if frame.width <= 0 or frame.height <= 0:
                raise StreamError(f"{path.name}: decoded frame has no dimensions")
            decoded += 1
            if decoded >= 1:
                break
        if decoded == 0:
            raise StreamError(f"{path.name}: no frames decoded from {codec} stream")
    except (StreamError, CodecConflictError):
        raise
    except Exception as exc:  # noqa: BLE001
        raise CodecConflictError(
            f"{path.name}: decode failed as {codec} ({exc})"
        ) from exc
    finally:
        container.close()


def _load_index(path: Path, limits: Limits) -> dict[str, Any]:
    try:
        size = path.stat().st_size
    except OSError as exc:
        raise InputError(f"cannot stat index {path}: {exc}") from exc
    if size > limits.max_index_bytes:
        raise InputError(
            f"index {path.name} is {size} bytes, limit is {limits.max_index_bytes}"
        )
    try:
        with path.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except OSError as exc:
        raise InputError(f"cannot read index {path}: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise IndexError_(f"index {path.name} is not valid JSON: {exc}") from exc
    if not isinstance(payload, dict):
        raise IndexError_(f"index {path.name} must contain a JSON object")
    return payload


def _require_int(payload: dict[str, Any], key: str, context: str) -> int:
    value = payload.get(key)
    if not isinstance(value, int) or isinstance(value, bool):
        raise IndexError_(f"{context}: {key} must be an integer, got {value!r}")
    return value


def _parse_packets(
    raw_entries: Any,
    *,
    track_id: str,
    stream_size: int,
    limits: Limits,
    allow_truncated: bool,
) -> tuple[tuple[PacketRecord, ...], frozenset[int]]:
    if not isinstance(raw_entries, list) or not raw_entries:
        raise IndexError_(f"{track_id}: index requires a non-empty packets list")
    if len(raw_entries) > limits.max_index_entries:
        raise IndexError_(
            f"{track_id}: {len(raw_entries)} index entries exceed limit "
            f"{limits.max_index_entries}"
        )
    packets: list[PacketRecord] = []
    truncated: set[int] = set()
    previous_end = -1
    for position, entry in enumerate(raw_entries):
        context = f"{track_id} packet #{position}"
        if not isinstance(entry, dict):
            raise IndexError_(f"{context}: entry must be an object")
        offset = _require_int(entry, "offset", context)
        size = _require_int(entry, "size", context)
        pts = _require_int(entry, "pts_ticks", context)
        duration = _require_int(entry, "duration_ticks", context)
        seq = entry.get("seq", position)
        if not isinstance(seq, int) or isinstance(seq, bool) or seq < 0:
            raise IndexError_(f"{context}: seq must be a non-negative integer")
        keyframe = entry.get("keyframe", False)
        if not isinstance(keyframe, bool):
            raise IndexError_(f"{context}: keyframe must be a boolean")
        if offset < 0:
            raise IndexError_(f"{context}: negative byte offset {offset}")
        if size <= 0:
            raise IndexError_(f"{context}: non-positive packet size {size}")
        if pts < 0:
            raise IndexError_(f"{context}: negative presentation time {pts}")
        if duration < 0:
            raise IndexError_(f"{context}: negative duration {duration}")
        if duration == 0:
            raise IndexError_(f"{context}: zero duration makes cadence undecidable")
        if offset < previous_end:
            raise IndexError_(
                f"{context}: offset {offset} overlaps previous packet ending at "
                f"{previous_end}"
            )
        if offset + size > stream_size:
            if allow_truncated:
                truncated.add(position)
                packets.append(
                    PacketRecord(position, offset, size, keyframe, pts, duration, seq)
                )
                previous_end = offset + size
                continue
            raise IndexError_(
                f"{context}: bytes [{offset}, {offset + size}) exceed stream size "
                f"{stream_size}"
            )
        packets.append(PacketRecord(position, offset, size, keyframe, pts, duration, seq))
        previous_end = offset + size
    if not any(packet.keyframe for packet in packets if packet.index not in truncated):
        raise IndexError_(f"{track_id}: index contains no usable keyframe")
    return tuple(packets), frozenset(truncated)
