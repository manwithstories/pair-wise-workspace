"""Offline compositing: NumPy frame synthesis, PyAV PNG encoding, manifest.

No display/GUI dependency. Each output frame is

* synthesized from the source RGB payload (a centered inset so differing track
  frame sizes remain visible), plus
* a deterministic 8-pixel status bar encoding track color, master tick and the
  timeline ordinal, so every rendered frame is visually distinct and its hash
  reproducible.

PNGs are encoded through PyAV and written atomically. A short PCM audio
sidecar is produced with soundfile so the export bundle also carries a
recomputable non-image artifact.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from .model import MAX_TIMELINE_FRAMES, MediaMixError, safe_resolve
from .resume import FrameEntry, atomic_write_bytes, atomic_write_json

BAR_HEIGHT = 8
AUDIO_SAMPLE_RATE = 48_000
AUDIO_DURATION_SECONDS = 1.0


@dataclass
class RenderConfig:
    width: int
    height: int
    tolerance_ticks: int
    interval_start: int
    interval_end: int
    strict: bool


@dataclass
class RenderedFrame:
    name: str
    tick: int
    sha256: str
    width: int
    height: int
    track_id: str
    frame_seq: int


def _track_color(track_id: str) -> tuple[int, int, int]:
    """Deterministic, well-separated RGB tag color per track id."""
    digest = hashlib.sha256(track_id.encode("utf-8")).digest()
    return (64 + digest[0] % 192, 64 + digest[1] % 192, 64 + digest[2] % 192)


def synthesize_frame(
    payload: bytes,
    src_width: int,
    src_height: int,
    out_width: int,
    out_height: int,
    tick: int,
    ordinal: int,
    track_id: str,
) -> np.ndarray:
    """Compose one RGB ``uint8`` frame of shape ``(out_height, out_width, 3)``."""
    frame = np.zeros((out_height, out_width, 3), dtype=np.uint8)
    src = np.frombuffer(payload, dtype=np.uint8)
    expected = src_width * src_height * 3
    if src.size != expected:
        raise MediaMixError(
            f"payload has {src.size} bytes, expected {expected}",
            path=None, frame_seq=ordinal,
        )
    src = src.reshape(src_height, src_width, 3)

    # Fit the source inset centered, never upscaling beyond the canvas.
    scale = min(out_width / src_width, (out_height - BAR_HEIGHT) / src_height, 1.0)
    inset_w = max(1, int(src_width * scale))
    inset_h = max(1, int(src_height * scale))
    y0 = BAR_HEIGHT + max(0, (out_height - BAR_HEIGHT - inset_h) // 2)
    x0 = max(0, (out_width - inset_w) // 2)
    inset = _resize_nearest(src, inset_w, inset_h)
    frame[y0:y0 + inset_h, x0:x0 + inset_w] = inset

    # Status bar: track color, then binary tick / ordinal stripes.
    color = _track_color(track_id)
    frame[:BAR_HEIGHT, :, :] = np.array(color, dtype=np.uint8)
    for bit in range(min(out_width, 64)):
        word = tick if bit < 32 else ordinal
        bit_index = bit if bit < 32 else bit - 32
        if (word >> bit_index) & 1:
            frame[:BAR_HEIGHT, bit, :] = 255 - frame[:BAR_HEIGHT, bit, :]
    return frame


def _resize_nearest(src: np.ndarray, out_w: int, out_h: int) -> np.ndarray:
    """Nearest-neighbor resize implemented with pure NumPy fancy indexing."""
    rows = (np.arange(out_h) * src.shape[0] // out_h).clip(0, src.shape[0] - 1)
    cols = (np.arange(out_w) * src.shape[1] // out_w).clip(0, src.shape[1] - 1)
    return src[rows][:, cols]


def encode_png(rgb: np.ndarray) -> bytes:
    """Encode an RGB uint8 array as a single PNG image via PyAV."""
    if rgb.ndim != 3 or rgb.shape[2] != 3 or rgb.dtype != np.uint8:
        raise MediaMixError("encode_png expects an HxWx3 uint8 array")
    h, w = rgb.shape[:2]
    codec = av.codec.Codec("png", "w")
    ctx = av.CodecContext.create(codec)
    ctx.width = w
    ctx.height = h
    ctx.pix_fmt = "rgb24"
    frame = av.VideoFrame.from_ndarray(np.ascontiguousarray(rgb), format="rgb24")
    packets = ctx.encode(frame)
    packets += ctx.encode(None)
    if not packets:
        raise MediaMixError("PNG encoder produced no packets")
    return b"".join(bytes(p) for p in packets)


def decode_png(blob: bytes) -> np.ndarray:
    """Decode PNG bytes back to an RGB uint8 array (used by the validator)."""
    ctx = av.CodecContext.create(av.codec.Codec("png", "r"))
    packets = av.Packet(blob)
    frames = ctx.decode(packets)
    frames += ctx.decode(None)
    if not frames:
        raise MediaMixError("PNG decoder produced no frames")
    return frames[0].to_ndarray(format="rgb24")


def frame_name(ordinal: int) -> str:
    return f"frame_{ordinal:06d}.png"


def render_schedule(
    workdir: Path,
    timeline,
    payloads: dict[tuple[str, int], tuple[bytes, int, int]],
    config: RenderConfig,
    known_frames: dict[str, FrameEntry] | None = None,
    progress_cb=None,
) -> list[RenderedFrame]:
    """Render every timeline frame, skipping frames already verified on disk.

    ``progress_cb(rendered_frame)`` is invoked after each newly rendered frame;
    it may raise to simulate an interrupted run (the frame is already durable).
    """
    known_frames = known_frames or {}
    if len(timeline.frames) > MAX_TIMELINE_FRAMES:
        raise MediaMixError(
            f"timeline has {len(timeline.frames)} frames, limit {MAX_TIMELINE_FRAMES}"
        )
    out_dir = safe_resolve(workdir, "frames")
    out_dir.mkdir(parents=True, exist_ok=True)
    rendered: list[RenderedFrame] = []
    for ordinal, mf in enumerate(timeline.frames):
        name = frame_name(ordinal)
        key = (mf.track_id, mf.file_seq)
        if key not in payloads:
            raise MediaMixError(
                f"no payload for track={mf.track_id} file_seq={mf.file_seq}",
                frame_seq=mf.frame_seq,
            )
        payload, src_w, src_h = payloads[key]
        target = out_dir / name
        if name in known_frames and target.exists():
            digest = hashlib.sha256(target.read_bytes()).hexdigest()
            if digest == known_frames[name].sha256:
                rendered.append(RenderedFrame(
                    name=name, tick=mf.tick, sha256=digest,
                    width=config.width, height=config.height,
                    track_id=mf.track_id, frame_seq=mf.frame_seq,
                ))
                continue  # verified frame: never rewritten
        rgb = synthesize_frame(
            payload, src_w, src_h, config.width, config.height,
            mf.tick, ordinal, mf.track_id,
        )
        png_blob = encode_png(rgb)
        atomic_write_bytes(target, png_blob)
        rf = RenderedFrame(
            name=name, tick=mf.tick, sha256=hashlib.sha256(png_blob).hexdigest(),
            width=config.width, height=config.height,
            track_id=mf.track_id, frame_seq=mf.frame_seq,
        )
        rendered.append(rf)
        if progress_cb is not None:
            progress_cb(rf)
    return rendered


def write_audio_sidecar(workdir: Path) -> tuple[str, str]:
    """Write a deterministic 1 s stereo 48 kHz PCM WAV; return (name, sha256)."""
    out_dir = safe_resolve(workdir, "frames")
    out_dir.mkdir(parents=True, exist_ok=True)
    t = np.arange(int(AUDIO_SAMPLE_RATE * AUDIO_DURATION_SECONDS)) / AUDIO_SAMPLE_RATE
    left = 0.25 * np.sin(2 * np.pi * 440.0 * t)
    right = 0.25 * np.sin(2 * np.pi * 660.0 * t)
    stereo = np.stack([left, right], axis=1, dtype=np.float32)
    target = out_dir / "sidecar.wav"
    sf.write(target, stereo, AUDIO_SAMPLE_RATE, subtype="PCM_16")
    return target.name, hashlib.sha256(target.read_bytes()).hexdigest()


def output_root_hash(frame_hashes: list[str], audio_hash: str) -> str:
    """Merkle-style root: hash of concatenated per-artifact SHA-256 digests."""
    h = hashlib.sha256()
    for digest in frame_hashes:
        h.update(bytes.fromhex(digest))
    h.update(bytes.fromhex(audio_hash))
    return h.hexdigest()


def write_manifest(workdir: Path, manifest: dict) -> Path:
    path = safe_resolve(workdir, "frames", "manifest.json")
    atomic_write_json(path, manifest)
    return path
