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


def make_timestamps(
    count: int,
    fps: Fraction,
    *,
    restart_at: int | None = None,
    wrap_at: int | None = None,
    wrap_size: int = 1 << 33,
    drift_ppm: float = 0.0,
    vfr_every: int | None = None,
) -> list[dict[str, int]]:
    """Build per-frame timing with optional wrap, restart, drift and VFR.

    ``restart_at`` models an encoder reboot: from that frame on the capture
    clock restarts near zero.  ``wrap_at`` folds the wire PTS through a
    ``wrap_size`` modulus.  ``vfr_every`` stretches every Nth frame by 50 %.
    """

    if count <= 0:
        raise InputError("frame count must be positive")
    if fps <= 0:
        raise InputError("frame rate must be positive")
    if wrap_size <= 1:
        raise InputError("wrap size must exceed one")
    for label, value in (("restart_at", restart_at), ("wrap_at", wrap_at)):
        if value is not None and not 0 <= value < count:
            raise InputError(f"{label} must be within 0..{count - 1}")
    if vfr_every is not None and vfr_every <= 0:
        raise InputError("vfr_every must be positive")
    nominal = ticks_from_rate(1, fps)
    drift = 1.0 + drift_ppm / 1_000_000.0
    durations = []
    for index in range(count):
        duration = nominal
        if vfr_every and index > 0 and index % vfr_every == 0:
            duration = int(round(nominal * 1.5))
        durations.append(duration)
    # Choose a constant wire offset so the clock crosses zero at ``wrap_at``.
    wrap_offset = 0
    if wrap_at is not None:
        capture_at_wrap = sum(durations[:wrap_at])
        wrap_offset = (wrap_size - int(round(capture_at_wrap * drift))) % wrap_size
    entries: list[dict[str, int]] = []
    media_offset = 0
    segment_start = 0
    for index in range(count):
        if restart_at is not None and index == restart_at:
            segment_start = media_offset
        capture = int(round((media_offset - segment_start) * drift))
        entries.append(
            {
                "pts_ticks": (capture + wrap_offset) % wrap_size,
                "duration_ticks": durations[index],
                "unwrapped_ticks": capture,
            }
        )
        media_offset += durations[index]
    return entries


def build_index_payload(
    *,
    track_id: str,
    role: str,
    codec: str,
    stream_name: str,
    stream_size: int,
    width: int,
    height: int,
    fps: Fraction,
    keyframe_interval: int,
    packets: Sequence[dict[str, object]],
    entries: Sequence[dict[str, int]],
    anchors: Sequence[dict[str, object]] = (),
) -> dict[str, object]:
    if len(packets) != len(entries):
        raise InputError(
            f"packet count {len(packets)} and timestamp count {len(entries)} differ"
        )
    index_packets = []
    for position, (packet, entry) in enumerate(zip(packets, entries)):
        index_packets.append(
            {
                "offset": int(packet["offset"]),
                "size": int(packet["size"]),
                "pts_ticks": int(entry["pts_ticks"]),
                "duration_ticks": int(entry["duration_ticks"]),
                "keyframe": bool(packet["keyframe"]),
                "seq": position,
            }
        )
    return {
        "track_id": track_id,
        "role": role,
        "codec": codec,
        "stream": stream_name,
        "stream_size": int(stream_size),
        "width": int(width),
        "height": int(height),
        "nominal_rate": f"{fps.numerator}/{fps.denominator}",
        "keyframe_interval": keyframe_interval,
        "packets": index_packets,
        "anchors": list(anchors),
    }


def synth_audio(
    path: Path,
    *,
    seed: int,
    seconds: float,
    sample_rate: int = 48_000,
    frequency: float = 440.0,
    amplitude: float = 0.22,
    drift_ppm: float = 0.0,
    pad_seconds: float = 0.0,
) -> None:
    """Write a deterministic tone-plus-noise WAV file."""

    import soundfile as sf

    if seconds <= 0:
        raise InputError("audio duration must be positive")
    if sample_rate <= 0:
        raise InputError("sample rate must be positive")
    if pad_seconds < 0:
        raise InputError("audio pad cannot be negative")
    rng = np.random.default_rng(seed)
    count = int(seconds * sample_rate)
    position = np.arange(count, dtype=np.float64)
    phase = 2.0 * np.pi * frequency * position / sample_rate
    phase = phase * (1.0 + drift_ppm / 1_000_000.0)
    envelope = 0.6 + 0.4 * np.sin(2.0 * np.pi * 0.5 * position / sample_rate) ** 2
    tone = amplitude * envelope * np.sin(phase)
    noise = rng.normal(0.0, amplitude * 0.05, count)
    signal = np.clip(tone + noise, -1.0, 1.0).astype(np.float32)
    if pad_seconds:
        pad = int(round(pad_seconds * sample_rate))
        signal = np.concatenate([np.zeros(pad, dtype=np.float32), signal])
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), signal, sample_rate, subtype="PCM_16", format="WAV")


def video_anchor_indices(
    seconds: Sequence[float], fps: Fraction, count: int
) -> list[int]:
    indices = []
    for seconds_value in seconds:
        index = int(round(seconds_value * float(fps)))
        if not 0 <= index < count:
            raise InputError(
                f"anchor at {seconds_value}s maps to frame {index}, "
                f"outside 0..{count - 1}"
            )
        indices.append(index)
    return indices


def audio_anchor_samples(
    seconds: Sequence[float], sample_rate: int, pad_seconds: float = 0.0
) -> list[int]:
    return [
        int(round((value + pad_seconds) * sample_rate)) for value in seconds
    ]


def build_video_fixture(
    root: Path,
    config: FixtureConfig,
    *,
    track_id: str = "video0",
    role: str = "camera",
    restart_at: int | None = None,
    wrap_at: int | None = None,
    drift_ppm: float = 0.0,
    vfr_every: int | None = None,
    anchors_seconds: Sequence[float] = (),
    bitrate: int | None = None,
) -> VideoFixture:
    """Encode one raw stream, scan it and write a matching index."""

    root.mkdir(parents=True, exist_ok=True)
    suffix = SUFFIX_BY_CODEC[config.codec]
    path = root / f"{track_id}{suffix}"
    size = encode_raw_stream(path, config, bitrate=bitrate)
    packets = scan_packets(path, config.codec)
    entries = make_timestamps(
        len(packets),
        config.fps,
        restart_at=restart_at,
        wrap_at=wrap_at,
        drift_ppm=drift_ppm,
        vfr_every=vfr_every,
    )
    labels = [f"a{index}" for index in range(len(anchors_seconds))]
    indices = video_anchor_indices(anchors_seconds, config.fps, len(packets))
    anchors = [
        {"label": label, "video_index": index}
        for label, index in zip(labels, indices)
    ]
    fixture = VideoFixture(
        codec=config.codec,
        path=path,
        index_path=root / f"{track_id}.index.json",
        packets=[
            {
                **packet,
                "pts_ticks": entry["pts_ticks"],
                "duration_ticks": entry["duration_ticks"],
            }
            for packet, entry in zip(packets, entries)
        ],
        size_bytes=size,
        fps=config.fps,
        width=config.width,
        height=config.height,
        track_id=track_id,
        role=role,
    )
    payload = build_index_payload(
        track_id=track_id,
        role=role,
        codec=config.codec,
        stream_name=path.name,
        stream_size=size,
        width=config.width,
        height=config.height,
        fps=config.fps,
        keyframe_interval=config.keyframe_interval,
        packets=packets,
        entries=entries,
        anchors=anchors,
    )
    fixture.index_path.write_text(
        json.dumps(payload, indent=2) + "\n", encoding="utf-8"
    )
    return fixture


def truncate_stream(source: Path, destination: Path, keep_bytes: int) -> Path:
    """Copy only the first ``keep_bytes`` bytes to model a truncated stream."""

    size = source.stat().st_size
    if not 0 < keep_bytes < size:
        raise InputError(
            f"truncation must keep 1..{size - 1} bytes, got {keep_bytes}"
        )
    destination.parent.mkdir(parents=True, exist_ok=True)
    with source.open("rb") as reader, destination.open("wb") as writer:
        writer.write(reader.read(keep_bytes))
    return destination


def corrupt_index(source: Path, destination: Path, mutate) -> Path:
    """Load an index, apply ``mutate`` in place and write it back."""

    payload = json.loads(source.read_text(encoding="utf-8"))
    mutate(payload)
    destination.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    return destination


def require_ffmpeg_cli() -> str:
    """Fail early (exit 2) when the Homebrew FFmpeg CLI is missing."""

    binary = shutil.which("ffmpeg")
    if not binary:
        raise DependencyError(
            "ffmpeg CLI not found; run `brew install ffmpeg libsndfile`"
        )
    return binary


def ffmpeg_version() -> str:
    binary = require_ffmpeg_cli()
    try:
        output = subprocess.run(
            [binary, "-version"], capture_output=True, text=True, check=True
        ).stdout
    except (OSError, subprocess.CalledProcessError) as exc:  # pragma: no cover
        raise DependencyError(f"cannot run ffmpeg: {exc}") from exc
    return output.splitlines()[0] if output else "unknown"
