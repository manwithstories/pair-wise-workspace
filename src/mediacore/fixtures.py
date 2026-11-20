"""Deterministically generated local video and audio fixtures.

Frames are procedurally built from a seed; no external files or network access
are involved.  Encoders run through PyAV with fixed options, and the same
(seed, codec, duration, bitrate, size) always produces bit-identical bytes so
checkpoint reuse and idempotency can be tested.

Frame timing is *not* taken from the encoder's container: raw H.264/H.265
streams carry no timestamps, so the pipeline synthesises one presentation
timestamp per access unit (see :mod:`mediacore.pipeline`).  AV1 streams carry a
frame order hint, but the pipeline still derives timeline ticks from the scanned
unit order to keep all three codecs comparable.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

WIDTH = 320
HEIGHT = 180
FPS = 30
GOP = 15
SAMPLE_RATE = 48000
AUDIO_CHANNELS = 1

_CODECS = {
    "h264": {"encoder": "libx264", "format": "h264", "extension": "h264"},
    "h265": {"encoder": "libx265", "format": "hevc", "extension": "h265"},
    "av1": {"encoder": "libaom-av1", "format": "obu", "extension": "obu"},
}


@dataclass(frozen=True)
class FixtureSpec:
    codec: str = "h264"
    duration_s: int = 12
    bitrate: int = 900_000
    seed: int = 41
    width: int = WIDTH
    height: int = HEIGHT
    fps: int = FPS
    gop: int = GOP
    corrupt_tail: bool = False
    tail_bytes: int = 97

    def __post_init__(self) -> None:
        if self.codec not in _CODECS:
            raise ValueError(f"codec must be one of {sorted(_CODECS)}")
        if not (1 <= self.duration_s <= 600):
            raise ValueError("duration must be between 1 and 600 seconds")
        if not (10_000 <= self.bitrate <= 200_000_000):
            raise ValueError("bitrate must be between 10000 and 200000000 bps")
        if self.width < 16 or self.height < 16 or self.width % 2 or self.height % 2:
            raise ValueError("width/height must be even and at least 16")
        if not (1 <= self.fps <= 120):
            raise ValueError("fps must be between 1 and 120")
        if not (1 <= self.gop <= self.fps * 10):
            raise ValueError("gop must be between 1 and 10*fps")

    @property
    def frames(self) -> int:
        return self.duration_s * self.fps

    @property
    def extension(self) -> str:
        return _CODECS[self.codec]["extension"]

    def digest(self) -> dict[str, Any]:
        return {
            "codec": self.codec, "duration_s": self.duration_s,
            "bitrate": self.bitrate, "seed": self.seed,
            "width": self.width, "height": self.height,
            "fps": self.fps, "gop": self.gop,
            "corrupt_tail": self.corrupt_tail, "tail_bytes": self.tail_bytes,
        }


def _plane_grid(width: int, height: int) -> tuple[np.ndarray, np.ndarray]:
    yy = np.arange(height, dtype=np.int32)[:, None]
    xx = np.arange(width, dtype=np.int32)[None, :]
    return yy, xx


def make_frame(index: int, spec: FixtureSpec) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Return (Y, U, V) planes for one frame.

    Each GOP starts from a different background, adds a moving textured patch
    and a vertical sweep bar, and one frame per second is a flat flash so the
    quality metric has both easy and hard content.
    """
    yy, xx = _plane_grid(spec.width, spec.height)
    gop = index // spec.gop
    rng = np.random.default_rng((spec.seed << 20) ^ (gop * 1_000_003) ^ 0x9E3779B1)
    y = ((xx * 0.4 + yy * 0.7 + gop * 17) % 200 + 30).astype(np.uint8)
    patch_h = max(8, spec.height // 4)
    patch_w = max(8, spec.width // 3)
    y0 = (gop * 13 + index * 5) % max(1, spec.height - patch_h)
    x0 = (gop * 29 + index * 9) % max(1, spec.width - patch_w)
    y = y.copy()
    y[y0:y0 + patch_h, x0:x0 + patch_w] = rng.integers(
        0, 256, (patch_h, patch_w), dtype=np.uint8)
    sweep = (index * 7) % spec.height
    y[sweep:sweep + max(1, spec.height // 12), :] = 230 if gop % 2 else 10
    u = np.full((spec.height // 2, spec.width // 2), 100 + (gop * 10) % 50, np.uint8)
    v = np.full((spec.height // 2, spec.width // 2), 130 - (gop * 7) % 40, np.uint8)
    return y, u, v


def frame_rgb(index: int, spec: FixtureSpec) -> np.ndarray:
    """RGB reference frame used to score decoded pictures (BT.601, full range)."""
    y, u, v = make_frame(index, spec)
    yf = y.astype(np.float32)
    uf = np.repeat(np.repeat(u.astype(np.float32), 2, axis=0), 2, axis=1)[:spec.height, :spec.width]
    vf = np.repeat(np.repeat(v.astype(np.float32), 2, axis=0), 2, axis=1)[:spec.height, :spec.width]
    r = yf + 1.402 * (vf - 128.0)
    g = yf - 0.344136 * (uf - 128.0) - 0.714136 * (vf - 128.0)
    b = yf + 1.772 * (uf - 128.0)
    return np.clip(np.stack([r, g, b], axis=-1), 0, 255).astype(np.uint8)


def audio_signal(spec: FixtureSpec) -> np.ndarray:
    """48 kHz mono signal: 440/660 Hz tones, per-second beeps, a silent gap.

    Kept for callers that want the whole array (tests, ad-hoc inspection).  The
    writer below streams the same signal in blocks so fixture generation does
    not allocate several duration-sized float arrays at once.
    """
    total = spec.duration_s * SAMPLE_RATE
    t = np.arange(total, dtype=np.float64) / SAMPLE_RATE
    signal = 0.35 * np.sin(2 * np.pi * 440.0 * t)
    signal += 0.10 * np.sin(2 * np.pi * 660.0 * t)
    phase = np.mod(t, 1.0)
    beep = (phase >= 0.88) & (phase < 0.97)
    signal += np.where(beep, 0.30 * np.sin(2 * np.pi * 1320.0 * t), 0.0)
    if spec.duration_s >= 4:
        start = int((spec.duration_s // 2 + 0.5) * SAMPLE_RATE)
        stop = start + int(0.5 * SAMPLE_RATE)
        signal[start:stop] = 0.0
    return np.clip(signal, -0.99, 0.99).astype(np.float32)


#: Block size used when streaming fixture audio to disk (frames per block).
AUDIO_BLOCK = 120_000


def _audio_block(start: int, count: int, spec: FixtureSpec) -> np.ndarray:
    """The same waveform as audio_signal, generated for frames [start, start+count)."""
    t = np.arange(start, start + count, dtype=np.float64) / SAMPLE_RATE
    block = 0.35 * np.sin(2 * np.pi * 440.0 * t)
    block += 0.10 * np.sin(2 * np.pi * 660.0 * t)
    phase = np.mod(t, 1.0)
    beep = (phase >= 0.88) & (phase < 0.97)
    block += np.where(beep, 0.30 * np.sin(2 * np.pi * 1320.0 * t), 0.0)
    block = np.clip(block, -0.99, 0.99)
    if spec.duration_s >= 4:
        gap_start = int((spec.duration_s // 2 + 0.5) * SAMPLE_RATE)
        gap_end = gap_start + int(0.5 * SAMPLE_RATE)
        lo = max(gap_start, start)
        hi = min(gap_end, start + count)
        if hi > lo:
            block[lo - start:hi - start] = 0.0
    return block.astype(np.float32)


def write_wav(path: Path, spec: FixtureSpec) -> dict[str, Any]:
    """Write the deterministic audio track as 16-bit PCM, block by block."""
    import soundfile as sf

    path.parent.mkdir(parents=True, exist_ok=True)
    total = spec.duration_s * SAMPLE_RATE
    with sf.SoundFile(str(path), "w", samplerate=SAMPLE_RATE, channels=1,
                      subtype="PCM_16", format="WAV") as handle:
        for start in range(0, total, AUDIO_BLOCK):
            count = min(AUDIO_BLOCK, total - start)
            handle.write(_audio_block(start, count, spec))
    return {
        "path": str(path), "sample_rate": SAMPLE_RATE,
        "channels": AUDIO_CHANNELS, "frames": total,
        "duration_s": spec.duration_s,
    }


def encode_video(path: Path, spec: FixtureSpec) -> bytes:
    """Encode the fixture into a raw bitstream and return its bytes (tests)."""
    encode_video_file(path, spec)
    return path.read_bytes()


def encode_video_file(path: Path, spec: FixtureSpec) -> int:
    """Encode the fixture straight to ``path``; returns the byte size."""
    import av

    path.parent.mkdir(parents=True, exist_ok=True)
    info = _CODECS[spec.codec]
    buffer = av.open(str(path), "w", format=info["format"])
    stream = buffer.add_stream(info["encoder"], rate=spec.fps)
    stream.width, stream.height = spec.width, spec.height
    stream.pix_fmt = "yuv420p"
    stream.bit_rate = spec.bitrate
    kbps = max(1, spec.bitrate // 1000)
    if spec.codec == "h264":
        stream.options = {
            "preset": "veryfast", "tune": "zerolatency",
            "g": str(spec.gop), "keyint_min": str(spec.gop),
            "sc_threshold": "0", "bf": "0", "aud": "1",
            "repeat-headers": "1", "nal-hrd": "cbr",
            "vbv-maxrate": str(kbps), "vbv-bufsize": str(kbps),
        }
    elif spec.codec == "h265":
        stream.options = {
            "preset": "veryfast",
            "x265-params": (
                f"keyint={spec.gop}:min-keyint={spec.gop}:scenecut=0:bframes=0:"
                f"aud=1:repeat-headers=1:hrd=1:vbv-maxrate={kbps}:"
                f"vbv-bufsize={kbps}:log-level=error"),
        }
    else:
        stream.options = {
            # AV1's realtime rate control needs a longer keyframe interval at
            # this resolution to land near the requested bitrate; the other
            # codecs reach the target with the configured GOP.
            "cpu-used": "8", "lag-in-frames": "0", "g": str(spec.gop * 2),
            "kf-min-dist": str(spec.gop * 2), "kf-max-dist": str(spec.gop * 2),
            "end-usage": "cbr", "usage": "realtime",
            "undershoot-pct": "0", "overshoot-pct": "0",
        }
    try:
        for index in range(spec.frames):
            y, u, v = make_frame(index, spec)
            frame = av.VideoFrame.from_ndarray(y, format="gray")
            frame = frame.reformat(format="yuv420p")
            frame.planes[0].update(y)
            frame.planes[1].update(u)
            frame.planes[2].update(v)
            frame.pts = index
            for packet in stream.encode(frame):
                buffer.mux(packet)
        for packet in stream.encode(None):
            buffer.mux(packet)
    finally:
        buffer.close()
    return path.stat().st_size


def make_fixtures(out_dir: Path, spec: FixtureSpec) -> dict[str, Any]:
    """Write video + audio fixtures (and an optional deliberately cut tail).

    The stream is written directly to disk and only its size is returned, so
    fixture generation for long streams does not hold the encoded file in
    memory.  With ``corrupt_tail`` the file is shortened in place at a point
    chosen to be structurally detectable (see :func:`_truncate_for_detection`).
    """
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    video_path = out_dir / f"video.{spec.extension}"
    audio_path = out_dir / "audio.wav"
    size = encode_video_file(video_path, spec)
    result: dict[str, Any] = {
        "spec": spec.digest(),
        "video_path": str(video_path),
        "video_bytes": size,
        "audio": write_wav(audio_path, spec),
        "corrupt_tail": spec.corrupt_tail,
    }
    if spec.corrupt_tail:
        cut = truncate_for_detection(video_path, spec)
        result["video_bytes_original"] = size
        result["video_bytes"] = size - cut
        result["truncated_at"] = size - cut
        result["truncated_bytes"] = cut
    return result


def truncate_for_detection(path: Path, spec: FixtureSpec) -> int:
    """Shorten ``path`` in place at a structurally detectable point.

    Returns the number of bytes removed.  See :func:`_truncate_for_detection`
    for why the cut point is chosen rather than arbitrary.
    """
    size = path.stat().st_size
    with open(path, "rb") as handle:
        handle.seek(max(0, size - 65536))
        tail = handle.read()
    if spec.codec == "av1":
        cut = max(1, min(spec.tail_bytes, size // 4))
        new_size = size - cut
    else:
        rel = tail.rfind(b"\x00\x00\x01")
        if rel < 0:
            cut = max(1, min(spec.tail_bytes, size // 4))
            new_size = size - cut
        else:
            new_size = max(1, size - len(tail) + rel + 3)
            if new_size >= size:
                cut = max(1, min(spec.tail_bytes, size // 4))
                new_size = size - cut
    cut = size - new_size
    with open(path, "r+b") as handle:
        handle.truncate(new_size)
    return cut


def _truncate_for_detection(data: bytes, spec: FixtureSpec) -> tuple[bytes, int]:
    """Cut the stream so the damage is structurally locatable.

    Cutting an Annex-B stream at an arbitrary byte can leave a syntactically
    complete (if semantically short) final picture: the decoder conceals the
    missing macroblocks and no parser can prove the cut.  To make the corrupt
    fixture deterministic and diagnosable, the cut is placed just after a start
    code so the file ends mid-NAL-header (Annex-B), while AV1 is cut inside the
    last OBU payload, which its length field makes detectable.
    """
    if spec.codec == "av1":
        cut = max(1, min(spec.tail_bytes, len(data) // 4))
        return data[:-cut], cut
    # Walk back from the end to the last start code and leave only its prefix.
    window = data[max(0, len(data) - 65536):]
    rel = window.rfind(b"\x00\x00\x01")
    if rel < 0:
        cut = max(1, min(spec.tail_bytes, len(data) // 4))
        return data[:-cut], cut
    end = max(0, len(data) - 65536) + rel + 3
    if end <= 1 or end >= len(data):
        cut = max(1, min(spec.tail_bytes, len(data) // 4))
        return data[:-cut], cut
    return data[:end], len(data) - end
