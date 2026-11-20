"""Deterministic, code-generated fixtures for every pipeline stage.

Nothing here reads external media.  The generator writes raw Annex-B H.264 /
HEVC and AV1 OBU streams plus a keyframe index, so tests and the demo exercise
the same bytes and stay byte-for-byte reproducible for a given seed.
"""

from __future__ import annotations

import json
import shutil
import subprocess
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Sequence

import numpy as np

from .clock import ticks_from_rate
from .errors import DependencyError, InputError, StreamError

ENCODER_BY_CODEC = {
    "h264": ("libx264", "h264"),
    "hevc": ("libx265", "hevc"),
    "av1": ("libaom-av1", "obu"),
}
SUFFIX_BY_CODEC = {"h264": ".h264", "hevc": ".h265", "av1": ".obu"}


@dataclass(frozen=True)
class FixtureConfig:
    seed: int = 17
    width: int = 320
    height: int = 180
    frames: int = 150
    fps: Fraction = Fraction(30, 1)
    codec: str = "h264"
    target_bps: int = 800_000
    keyframe_interval: int = 30
    audio_seconds: float = 5.0
    sample_rate: int = 48_000


@dataclass
class VideoFixture:
    """One generated raw stream and its index metadata."""

    codec: str
    path: Path
    index_path: Path
    packets: list[dict[str, object]]
    size_bytes: int
    fps: Fraction
    width: int
    height: int
    track_id: str = "video0"
    role: str = "camera"

    @property
    def frame_count(self) -> int:
        return len(self.packets)


@dataclass
class FixtureBundle:
    root: Path
    video: list[VideoFixture] = field(default_factory=list)
    audio_path: Path | None = None
    notes: list[str] = field(default_factory=list)


def _require_pyav():
    try:
        import av
    except ImportError as exc:  # pragma: no cover - environment guard
        raise DependencyError("PyAV is required to generate fixtures") from exc
    return av


def _ensure_encoder(name: str) -> None:
    av = _require_pyav()
    if name not in av.codec.codecs_available:
        raise DependencyError(
            f"FFmpeg build lacks the {name} encoder; install ffmpeg via Homebrew"
        )


def _procedural_frame(
    width: int, height: int, index: int, base: np.ndarray
) -> np.ndarray:
    """A deterministic, mildly moving frame (no per-frame randomness)."""

    yy, xx = np.mgrid[0:height, 0:width]
    frame = base.copy()
    frame[:, :, 0] = ((xx // 3 + index * 2) % 256).astype(np.uint8)
    frame[:, :, 1] = ((yy // 4 + index * 3) % 256).astype(np.uint8)
    frame[:, :, 2] = (((xx * 3 + yy * 2) // 7 + index * 7) % 256).astype(np.uint8)
    return frame


def _encoder_options(codec: str, bitrate: int) -> dict[str, str]:
    if codec == "h264":
        return {
            "preset": "ultrafast",
            "tune": "zerolatency",
            "bf": "0",
            "g": "30",
            "threads": "1",
            "bitrate": str(bitrate),
            "maxrate": str(bitrate),
            "bufsize": str(max(bitrate // 2, 1)),
            "x264-params": "nal-hrd=cbr:force-cfr=1:filler=1",
        }
    if codec == "hevc":
        return {
            "preset": "ultrafast",
            "tune": "zerolatency",
            "bf": "0",
            "threads": "1",
            "bitrate": str(bitrate),
            "maxrate": str(bitrate),
            "bufsize": str(max(bitrate // 2, 1)),
            "x265-params": (
                "bframes=0:keyint=30:log-level=error:"
                f"vbv-maxrate={bitrate // 1000}:vbv-bufsize={bitrate // 2000}:"
                "strict-cbr=1"
            ),
        }
    if codec == "av1":
        return {
            "cpu-used": "8",
            "row-mt": "1",
            "usage": "realtime",
            "end-usage": "cbr",
            "undershoot-pct": "0",
            "overshoot-pct": "0",
            "g": "30",
            "lag-in-frames": "0",
            "threads": "1",
            "b": str(bitrate),
        }
    raise InputError(f"unsupported fixture codec {codec!r}")


def encode_raw_stream(
    path: Path, config: FixtureConfig, *, bitrate: int | None = None
) -> int:
    """Encode a procedural clip and return its on-disk size in bytes."""

    av = _require_pyav()
    encoder, container_format = ENCODER_BY_CODEC[config.codec]
    _ensure_encoder(encoder)
    bitrate = bitrate or config.target_bps
    rate = int(round(float(config.fps)))
    if rate <= 0:
        raise InputError("fixture frame rate must round to a positive integer")
    rng = np.random.default_rng(config.seed)
    base = rng.integers(0, 256, (config.height, config.width, 3), dtype=np.uint8)
    options = _encoder_options(config.codec, bitrate)
    try:
        with av.open(str(path), "w", format=container_format) as container:
            stream = container.add_stream(encoder, rate=rate)
            stream.width = config.width
            stream.height = config.height
            stream.pix_fmt = "yuv420p"
            for key, value in options.items():
                stream.options[key] = value
            for index in range(config.frames):
                frame = av.VideoFrame.from_ndarray(
                    _procedural_frame(config.width, config.height, index, base),
                    format="rgb24",
                )
                frame.pts = index
                for packet in stream.encode(frame):
                    container.mux(packet)
            for packet in stream.encode(None):
                container.mux(packet)
    except Exception as exc:  # noqa: BLE001 - FFmpeg surfaces its own errors
        raise StreamError(f"fixture encode failed for {config.codec}: {exc}") from exc
    return path.stat().st_size


def scan_packets(path: Path, codec: str) -> list[dict[str, object]]:
    """Demux a raw stream into packet offsets, sizes and keyframe flags."""

    av = _require_pyav()
    _encoder, container_format = ENCODER_BY_CODEC[codec]
    packets: list[dict[str, object]] = []
    try:
        with av.open(str(path), format=container_format) as container:
            stream = container.streams.video[0]
            for packet in container.demux(stream):
                if packet.size <= 0 or packet.pos is None:
                    continue
                packets.append(
                    {
                        "offset": int(packet.pos),
                        "size": int(packet.size),
                        "keyframe": bool(packet.is_keyframe),
                    }
                )
    except Exception as exc:  # noqa: BLE001
        raise StreamError(f"cannot demux generated {codec} stream: {exc}") from exc
    if not packets:
        raise StreamError(f"generated {codec} stream produced no packets")
    return packets
