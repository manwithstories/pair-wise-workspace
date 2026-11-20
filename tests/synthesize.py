"""Deterministic sample-media generator for fieldcut demos and tests.

Generates one MP4/MOV per simulated camera with H.264 video and AAC audio.
The container itself always stays decodable; capture-clock pathology (drift,
33-bit wrap, VFR) is described in a ``<file>.clock.json`` sidecar that the
probe reconstructs, while truncation and packet corruption are applied to the
file bytes as post-processing steps.

Everything is seeded and integer-derived, so two runs with the same arguments
produce byte-identical media and sidecars.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from fractions import Fraction

import av
import numpy as np

# Small, fast fixtures: real frame sizes only matter for the encoder ladder.
WIDTH = 160
HEIGHT = 90
FPS = 24
SAMPLE_RATE = 44_100
AUDIO_N = 1024  # native AAC frame sample count
WRAP_MODULUS = 1 << 33

DEFECTS = ("normal", "drift", "wrap", "vfr", "truncated", "bad_packet")


@dataclass
class CameraSpec:
    name: str
    duration_s: float
    defects: tuple[str, ...]
    seed: int


def reference_rgb(frame_index: int, width: int = WIDTH,
                  height: int = HEIGHT) -> np.ndarray:
    """Deterministic source picture for ``frame_index`` (uint8 HxWx3 RGB).

    A moving vertical gradient plus a frame-indexed band gives the encoder
    temporal structure (so PSNR is meaningful) while staying cheap to build.
    The same function regenerates the PSNR reference during quality checks.
    """
    rng = np.random.default_rng(1000 + frame_index)
    base = np.zeros((height, width, 3), dtype=np.uint8)
    ramp = np.linspace(0, 255, width, dtype=np.uint8)
    base[:, :, 1] = ramp[None, :]
    base[:, :, 0] = (frame_index * 5) & 255
    band = frame_index % max(1, width)
    base[:, max(0, band - 2): band + 2, 2] = 240
    texture = rng.integers(0, 18, size=(height, width), dtype=np.int16)
    base = np.clip(base.astype(np.int16) + texture[..., None], 0, 255)
    return base.astype(np.uint8)


def reference_audio(seconds: float, seed: int,
                    sample_rate: int = SAMPLE_RATE) -> np.ndarray:
    """Deterministic stereo tone with a seed-derived sync chirp (float32)."""
    n = int(round(seconds * sample_rate))
    t = np.arange(n, dtype=np.float64) / sample_rate
    rng = np.random.default_rng(seed)
    f0 = 300.0 + (seed % 7) * 40.0
    tone = 0.18 * np.sin(2 * np.pi * f0 * t)
    # Short loud chirp near 0.5 s: the cross-correlation sync landmark.
    h0 = int(0.5 * sample_rate)
    h1 = h0 + int(0.12 * sample_rate)
    tt = t[h0:h1] - t[h0]
    sweep = np.sin(2 * np.pi * (480.0 + 1_600.0 * tt / max(tt[-1], 1e-9)) * tt)
    tone[h0:h1] += 0.35 * sweep
    jitter = rng.normal(0, 0.004, n)
    mono = np.clip(tone + jitter, -1.0, 1.0).astype(np.float32)
    return np.stack([mono, mono], axis=1)


def _capture_clock(n_frames: int, defects: tuple[str, ...],
                   seed: int) -> list[int]:
    """Build the pathological capture-clock PTS series (90 kHz ticks).

    The counter always runs at ~90 kHz (small drift only). A wrap is injected
    by starting the counter just below the 33-bit modulus so it crosses zero
    mid-clip rather than by rescaling time, which would distort the rate.
    """
    ideal_step = 90_000 // FPS
    # A capture oscillator running *slow* accumulates fewer ticks per real
    # second, so reference time grows faster than capture time: the fitted
    # reference/capture slope exceeds 1 and reads as positive drift ppm.
    drift_ppm = 600.0 if "drift" in defects else 0.0
    rate = ideal_step * (1.0 - drift_ppm / 1e6)
    positions: list[float] = []
    running = 0.0
    for i in range(n_frames):
        step = rate
        if "vfr" in defects:
            # +/- 12% alternating cadence jitter; positions stay cumulative.
            step *= 1.12 if i % 2 == 0 else 0.88
        running += step
        positions.append(running)
    span = positions[-1] if positions else 0.0
    start = (WRAP_MODULUS - int(span * 0.4)) if "wrap" in defects else 0
    return [(start + int(round(p))) & (WRAP_MODULUS - 1) for p in positions]


def _encode_camera(spec: CameraSpec, out_path: str) -> None:
    n_frames = int(round(spec.duration_s * FPS))
    container = av.open(out_path, "w", format="mp4",
                        options={"movflags": "+faststart"})
    ast = container.add_stream("aac", rate=SAMPLE_RATE, layout="stereo")
    ast.time_base = Fraction(1, SAMPLE_RATE)
    vst = container.add_stream("libx264", rate=FPS)
    vst.time_base = Fraction(1, FPS)
    vst.width, vst.height = WIDTH, HEIGHT
    vst.pix_fmt = "yuv420p"
    # One IDR per second keeps every segment boundary keyframe-anchorable.
    vst.options = {"preset": "ultrafast", "g": str(FPS),
                   "keyint_min": str(FPS)}

    audio = reference_audio(spec.duration_s, spec.seed)
    audio_pkts, pts = [], 0
    for off in range(0, audio.shape[0], AUDIO_N):
        chunk = audio[off:off + AUDIO_N]
        if len(chunk) < AUDIO_N:
            chunk = np.pad(chunk, ((0, AUDIO_N - len(chunk)), (0, 0)))
        frame = av.AudioFrame.from_ndarray(
            np.ascontiguousarray(chunk.T), format="fltp", layout="stereo")
        frame.sample_rate = SAMPLE_RATE
        frame.pts = pts
        pts += AUDIO_N
        audio_pkts.extend(ast.encode(frame))
    audio_pkts.extend(ast.encode(None))

    video_pkts = []
    for i in range(n_frames):
        rgb = reference_rgb(i)
        frame = av.VideoFrame.from_ndarray(rgb, format="rgb24")
        frame.pts = i  # clean constant-rate decode timeline
        video_pkts.extend(vst.encode(frame))
    video_pkts.extend(vst.encode(None))

    def global_key(pkt: av.Packet) -> float:
        base = pkt.dts if pkt.dts is not None else (pkt.pts or 0)
        return float(base * pkt.time_base)

    for pkt in sorted(audio_pkts + video_pkts, key=global_key):
        container.mux(pkt)
    container.close()

    sidecar = {
        "wrap_modulus": WRAP_MODULUS,
        "pts_mod": _capture_clock(n_frames, spec.defects, spec.seed),
        "defects": list(spec.defects),
    }
    with open(out_path + ".clock.json", "w", encoding="utf-8") as f:
        json.dump(sidecar, f)


def _apply_truncation(path: str) -> None:
    """Cut the tail off after the moov box so the head stays decodable."""
    data = open(path, "rb").read()
    mdat = data.find(b"mdat")
    if mdat < 0:
        return
    # Keep ~55% of mdat payload: enough head for several readable seconds.
    cut = mdat + int((len(data) - mdat) * 0.55)
    with open(path, "wb") as f:
        f.write(data[:cut])


def _apply_bad_packet(path: str) -> int | None:
    """Corrupt a mid-file non-keyframe H.264 packet's payload bytes."""
    container = av.open(path)
    target = None
    for pkt in container.demux():
        if pkt.stream.type == "video" and not pkt.is_keyframe \
                and pkt.pos and pkt.size > 80:
            target = (pkt.pos, pkt.size)
            break
    container.close()
    if target is None:
        return None
    pos, size = target
    with open(path, "r+b") as f:
        f.seek(pos + 4)
        blob = bytes((b ^ 0xA5) for b in f.read(min(size - 4, 96)))
        f.seek(pos + 4)
        f.write(blob)
    return pos


def generate_camera(spec: CameraSpec, out_dir: str) -> dict:
    """Render one camera and apply its file-level defects; return a manifest."""
    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, f"{spec.name}.mp4")
    _encode_camera(spec, path)
    info: dict = {"name": spec.name, "path": path,
                  "defects": list(spec.defects), "injected": {}}
    if "truncated" in spec.defects:
        _apply_truncation(path)
        info["injected"]["truncated"] = True
    if "bad_packet" in spec.defects:
        offset = _apply_bad_packet(path)
        info["injected"]["bad_packet_offset"] = offset
    return info


def parse_defects(token: str) -> tuple[str, ...]:
    parts = tuple(p.strip() for p in token.split(",") if p.strip())
    bad = [p for p in parts if p not in DEFECTS]
    if bad:
        raise ValueError(f"unknown defect(s): {', '.join(bad)} "
                         f"(valid: {', '.join(DEFECTS)})")
    return parts or ("normal",)
