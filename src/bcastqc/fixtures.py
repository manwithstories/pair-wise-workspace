"""Deterministic fixture generation for demos and tests.

All fixtures are generated locally with PyAV and soundfile - nothing is
downloaded and no input-supplied code is executed.  Every generator takes a
``seed`` and produces byte-identical output for the same seed and parameters,
which is what makes the "rerun yields identical frame hashes" acceptance
criterion checkable.

Generated material
------------------
* Video: raw H.264 / H.265 / AV1 low-overhead OBU, 160x90, configurable frame
  rate and GOP.  Each frame carries a frame counter and a per-frame luma step,
  shared with the audio envelope, so audio/video correlation is well defined.
* Audio: PCM WAV at 48 kHz (and a 44.1 kHz variant for the resampling case),
  amplitude-modulated by the same deterministic per-frame +-1 sequence as the
  video's luma, which gives an unambiguous sync measurement.
* Damaged variants: dropped access units (packet loss), a truncated file, an
  out-of-range keyframe index and a duplicate/out-of-order index.

The fixture description (paths, sizes, hashes, frame count, keyframe index)
is written as ``fixtures.json`` so a run can be reproduced from it.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from .bitstream import AccessUnit, AnnexBParser, Av1ObuParser, Codec, compute_index

__all__ = [
    "FixtureSpec",
    "FixtureSet",
    "generate_fixture_set",
    "encode_video",
    "write_audio",
    "damage_drop_access_units",
    "damage_truncate",
    "damage_corrupt_access_unit",
    "make_bad_index",
]

WIDTH = 160
HEIGHT = 90
SAMPLE_RATE = 48000


@dataclass(frozen=True)
class FixtureSpec:
    seed: int
    frames: int = 75
    frame_rate: Fraction = Fraction(25, 1)
    gop: int = 25
    audio_rate: int = SAMPLE_RATE
    channels: int = 2
    audio_seconds: Fraction | None = None  # None = match the video duration
    codecs: tuple[Codec, ...] = (Codec.H264, Codec.H265, Codec.AV1)


@dataclass
class FixtureFile:
    role: str
    path: Path
    codec: str | None = None
    frames: int | None = None
    size: int = 0
    sha256: str = ""
    keyframes: list[dict] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)

    def to_json(self) -> dict:
        payload: dict = {"role": self.role, "path": self.path.name, "size": self.size, "sha256": self.sha256}
        if self.codec:
            payload["codec"] = self.codec
        if self.frames is not None:
            payload["frames"] = self.frames
        if self.keyframes:
            payload["keyframes"] = self.keyframes
        if self.notes:
            payload["notes"] = self.notes
        return payload


@dataclass
class FixtureSet:
    root: Path
    spec: FixtureSpec
    files: list[FixtureFile]

    def by_role(self, role: str) -> FixtureFile:
        for item in self.files:
            if item.role == role:
                return item
        raise KeyError(role)

    def to_json(self) -> dict:
        return {
            "seed": self.spec.seed,
            "frames": self.spec.frames,
            "frame_rate": f"{self.spec.frame_rate.numerator}/{self.spec.frame_rate.denominator}",
            "gop": self.spec.gop,
            "files": [item.to_json() for item in self.files],
        }

    def write_manifest(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(self.to_json(), indent=2) + "\n", encoding="utf-8")


# ==========================================================================
# Signals
# ==========================================================================


def marker_sequence(seed: int, count: int) -> np.ndarray:
    """Deterministic +-1 per-frame sync marker shared by video and audio."""
    if count <= 0:
        return np.zeros(0, dtype=np.int8)
    rng = _rng(seed, "marker")
    return np.where(rng.integers(0, 2, size=count, dtype=np.int8) > 0, 1, -1).astype(np.int8)


def _rng(seed: int, salt: str) -> np.random.Generator:
    digest = hashlib.sha256(f"{seed}:{salt}".encode()).digest()
    return np.random.default_rng(int.from_bytes(digest[:8], "big"))


def video_frames(spec: FixtureSpec) -> list[np.ndarray]:
    """Frames whose red plane follows the audio's per-frame sync marker.

    The red plane is a fixed horizontal ramp scaled by the +-1 marker, so the
    frame mean is a clean step signal; the green plane carries a fixed
    vertical ramp for spatial variety, and an 8-bit counter strip plus a
    per-frame blue level make every frame individually identifiable.
    """
    rng = _rng(spec.seed, "video")
    ramp = np.linspace(0, 255, WIDTH, dtype=np.float32)[None, :]
    rows = np.linspace(0, 160, HEIGHT, dtype=np.float32)[:, None]
    marker_bits = marker_sequence(spec.seed, spec.frames)
    frames: list[np.ndarray] = []
    for i in range(spec.frames):
        marker = 128.0 + 70.0 * float(marker_bits[i])
        base = np.broadcast_to(ramp * 0.35 + 30.0, (HEIGHT, WIDTH))
        arr = np.zeros((HEIGHT, WIDTH, 3), dtype=np.uint8)
        arr[:, :, 0] = np.clip(base * (marker / 128.0), 0, 255).astype(np.uint8)
        arr[:, :, 1] = np.broadcast_to(rows, (HEIGHT, WIDTH)).astype(np.uint8)
        arr[:, :, 2] = (i * 7) % 256
        # frame counter as an 8-bit binary strip
        for bit in range(8):
            if (i >> bit) & 1:
                x0 = 4 + bit * 9
                arr[4:12, x0:x0 + 7, :] = 250
        # light sensor noise so loss concealment differs from real frames
        noise = rng.integers(-2, 3, size=(HEIGHT, WIDTH, 1), dtype=np.int16)
        arr = np.clip(arr.astype(np.int16) + noise, 0, 255).astype(np.uint8)
        frames.append(arr)
    return frames


def audio_samples(spec: FixtureSpec) -> np.ndarray:
    """PCM amplitude-modulated by the same per-frame marker as the video."""
    rate = spec.audio_rate
    duration = spec.audio_seconds
    if duration is None:
        duration = Fraction(spec.frames, 1) / spec.frame_rate
    total = int(Fraction(duration) * rate)
    t = np.arange(total, dtype=np.float64) / rate
    bits = marker_sequence(spec.seed, spec.frames + 2)
    # each sample carries the marker of the frame covering it, so the centre
    # of that frame's interval is the natural audio sample to correlate with
    frame_of_sample = np.floor(t * float(spec.frame_rate)).astype(np.int64)
    envelope = 0.55 + 0.35 * bits[np.clip(frame_of_sample, 0, len(bits) - 1)].astype(np.float64)
    left = envelope * np.sin(2 * np.pi * 440.0 * t)
    right = envelope * np.sin(2 * np.pi * 660.0 * t + 0.5)
    if spec.channels == 1:
        data = ((left + right) * 0.5).astype(np.float32)[:, None]
    else:
        data = np.stack([left, right], axis=1).astype(np.float32)
    return data


# ==========================================================================
# Writers
# ==========================================================================


_ENCODERS = {
    Codec.H264: ("libx264", "h264"),
    Codec.H265: ("libx265", "hevc"),
    Codec.AV1: ("libaom-av1", "obu"),
}


def _encoder_options(codec: Codec, spec: FixtureSpec) -> dict[str, str]:
    gop = str(spec.gop)
    if codec is Codec.H264:
        return {
            "preset": "ultrafast",
            "g": gop,
            "x264-params": f"aud=1:repeat-headers=1:keyint={gop}:min-keyint={gop}:bframes=0",
        }
    if codec is Codec.H265:
        return {
            "preset": "ultrafast",
            "x265-params": f"aud=1:repeat-headers=1:keyint={gop}:min-keyint={gop}:bframes=0:info=0",
        }
    return {
        "g": gop,
        "cpu-used": "8",
        "lag-in-frames": "0",
        "b": "0",
    }


def encode_video(path: Path, codec: Codec, spec: FixtureSpec) -> None:
    encoder, container = _ENCODERS[codec]
    path.parent.mkdir(parents=True, exist_ok=True)
    frames = video_frames(spec)
    rate = spec.frame_rate
    with av.open(str(path), "w", format=container) as container_ctx:
        stream = container_ctx.add_stream(encoder, rate=rate)
        stream.width = WIDTH
        stream.height = HEIGHT
        stream.pix_fmt = "yuv420p"
        for key, value in _encoder_options(codec, spec).items():
            stream.codec_context.options[key] = value
        if codec is Codec.AV1:
            stream.codec_context.max_b_frames = 0
        for index, array in enumerate(frames):
            frame = av.VideoFrame.from_ndarray(array, format="rgb24")
            frame.pts = index
            for packet in stream.encode(frame):
                container_ctx.mux(packet)
        for packet in stream.encode(None):
            container_ctx.mux(packet)


def write_audio(path: Path, spec: FixtureSpec, *, rate: int | None = None) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = audio_samples(spec if rate is None else FixtureSpec(**{**spec.__dict__, "audio_rate": rate}))
    sf.write(str(path), data, rate or spec.audio_rate, subtype="FLOAT")


def split_units(data: bytes, codec: Codec) -> list[AccessUnit]:
    parser = AnnexBParser(codec) if codec in (Codec.H264, Codec.H265) else Av1ObuParser()
    return parser.split(data)


def damage_drop_access_units(source: Path, target: Path, codec: Codec, positions: list[int]) -> list[int]:
    """Remove whole access units (a mid-stream packet loss)."""
    data = source.read_bytes()
    units = split_units(data, codec)
    drop = {units[p] for p in positions if 0 <= p < len(units)}
    kept = [u for u in units if u not in drop]
    blob = b"".join(data[u.offset:u.end] for u in kept)
    target.write_bytes(blob)
    return sorted(u.index for u in drop)


def damage_truncate(source: Path, target: Path, codec: Codec, fraction: float = 0.7) -> int:
    """Keep a prefix of the stream, cutting inside the last kept access unit."""
    data = source.read_bytes()
    units = split_units(data, codec)
    keep = max(1, int(len(units) * fraction))
    cut = units[keep - 1].offset + max(1, units[keep - 1].size * 2 // 3)
    target.write_bytes(data[:cut])
    return cut


def make_bad_index(source: Path, target: Path, codec: Codec) -> dict:
    """Write an index containing duplicate, out-of-order and out-of-range entries."""
    data = source.read_bytes()
    units = split_units(data, codec)
    keyframes = [u for u in units if u.is_keyframe]
    if not keyframes or len(units) < 4:
        raise ValueError("stream has too few access units for a bad index fixture")
    second = keyframes[min(1, len(keyframes) - 1)]
    later = units[min(len(units) - 1, 10)]
    entries = [
        {"offset": units[0].offset, "size": units[0].size},
        {"offset": second.offset, "size": second.size},
        {"offset": second.offset, "size": second.size},          # duplicate
        {"offset": 999_999_999, "size": 16},                      # out of range
        {"offset": later.offset, "size": later.size},             # out of order
    ]
    target.write_text(json.dumps({"keyframes": entries}, indent=2), encoding="utf-8")
    return {"entries": len(entries), "valid": len(keyframes)}


def write_good_index(source: Path, target: Path, codec: Codec) -> list[dict]:
    data = source.read_bytes()
    units = split_units(data, codec)
    index = compute_index(units)
    target.write_text(json.dumps({"keyframes": index.to_json()}, indent=2), encoding="utf-8")
    return index.to_json()
