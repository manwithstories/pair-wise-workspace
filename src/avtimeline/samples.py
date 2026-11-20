"""Deterministic sample media generation.

All fixture data is produced locally from fixed seeds; nothing is downloaded
and no file outside the requested work directory is read.  Generation is
byte-deterministic for a fixed seed, which in turn makes every downstream
segment checksum deterministic.

The fixture set deliberately mixes inconsistent sources:

* ``cam_main.mp4``   - 25 fps H.264, stereo 48 kHz AAC, BT.709 metadata,
* ``cam_alt.mp4``    - 30 fps H.264, 5.1 48 kHz AAC, BT.470BG metadata,
* ``team_voice.wav`` - 5.1 48 kHz float WAV,
* ``player_voice.wav``- mono 44.1 kHz float WAV,
* ``edge_tail.mp4``  - 25 fps, 0.2 s, leaves a sub-frame tail,
* ``corrupt_cam.mp4``- a byte-truncated copy of ``cam_main.mp4``.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from fractions import Fraction
from typing import Iterable

import av
import numpy as np

from .wavio import write_wav as _write_wav_bytes

SEED = 20240517
FPS = 25
WIDTH = 640
HEIGHT = 360


@dataclass(frozen=True)
class SampleSet:
    """Paths of every generated fixture."""

    directory: str
    cam_main: str
    cam_alt: str
    team_voice: str
    player_voice: str
    edge_tail: str
    corrupt_cam: str

    def all_paths(self) -> list[str]:
        return [
            self.cam_main,
            self.cam_alt,
            self.team_voice,
            self.player_voice,
            self.edge_tail,
            self.corrupt_cam,
        ]


def _atomic_write(path: str, write) -> None:
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    stem, suffix = os.path.splitext(path)
    temp = f"{stem}.part-{os.getpid()}{suffix}"
    try:
        write(temp)
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def _video_pattern(rng: np.random.Generator, width: int, height: int, index: int) -> np.ndarray:
    """A deterministic, compressible-but-moving test pattern."""
    base = rng.integers(0, 256, size=(height, width, 3), dtype=np.uint8)
    rows = (np.arange(height, dtype=np.int32)[:, None] + index * 3) % 256
    cols = (np.arange(width, dtype=np.int32)[None, :] + index * 5) % 256
    frame = base.copy()
    frame[..., 0] = ((frame[..., 0].astype(np.int32) // 2 + rows) % 256).astype(np.uint8)
    frame[..., 1] = ((frame[..., 1].astype(np.int32) // 2 + cols) % 256).astype(np.uint8)
    frame[..., 2] = ((frame[..., 2].astype(np.int32) + index * 7) % 256).astype(np.uint8)
    return frame


def _tone_bank(samples: int, rate: int, freqs: Iterable[float], amplitude: float) -> np.ndarray:
    t = np.arange(samples, dtype=np.float64) / rate
    channels = [
        (amplitude * np.sin(2 * np.pi * freq * t)).astype(np.float32) for freq in freqs
    ]
    return np.stack(channels, axis=1)


def _encode_camera(
    path: str,
    width: int,
    height: int,
    fps: int,
    frames: int,
    layout: str,
    sample_rate: int,
    samples: int,
    color: tuple[int, int, int, int],
    seed: int,
) -> None:
    def write(temp: str) -> None:
        container = av.open(
            temp, "w", format="mp4", options={"fflags": "+bitexact", "movflags": "+faststart"}
        )
        try:
            video = container.add_stream("libx264", rate=Fraction(fps, 1))
            video.width = width
            video.height = height
            video.pix_fmt = "yuv420p"
            video.time_base = Fraction(1, fps)
            video.bit_rate = 900_000
            video.options = {
                "preset": "ultrafast",
                "crf": "26",
                "x264-params": f"keyint={fps}:min-keyint={fps}:scenecut=0:bframes=0:threads=1",
                "threads": "1",
            }
            context = video.codec_context
            context.colorspace = color[2]
            context.color_primaries = color[0]
            context.color_trc = color[1]
            context.color_range = color[3]
            audio = container.add_stream("aac", rate=sample_rate)
            audio.layout = layout
            audio.bit_rate = 128_000
            rng = np.random.default_rng(seed)
            for index in range(frames):
                frame = av.VideoFrame.from_ndarray(
                    _video_pattern(rng, width, height, index), format="rgb24"
                )
                frame.pts = index
                frame.time_base = Fraction(1, fps)
                for packet in video.encode(frame):
                    container.mux(packet)
            for packet in video.encode():
                container.mux(packet)
            channels = len(av.AudioLayout(layout).channels)
            freqs = [220.0 + 110.0 * i for i in range(channels)]
            data = _tone_bank(samples, sample_rate, freqs, 0.08)
            pcm = av.AudioFrame.from_ndarray(
                np.ascontiguousarray(data.T), format="fltp", layout=layout
            )
            pcm.sample_rate = sample_rate
            pcm.pts = 0
            pcm.time_base = Fraction(1, sample_rate)
            for packet in audio.encode(pcm):
                container.mux(packet)
            for packet in audio.encode():
                container.mux(packet)
        finally:
            container.close()

    _atomic_write(path, write)


def _write_wav(path: str, data: np.ndarray, rate: int) -> None:
    _write_wav_bytes(path, data, rate, subtype="FLOAT")


def generate_samples(directory: str, force: bool = False) -> SampleSet:
    """Generate the fixture set under *directory*.

    Existing files are reused unless *force* is true, so repeated demo runs do
    not pay generation cost again.
    """
    os.makedirs(directory, exist_ok=True)
    paths = SampleSet(
        directory=directory,
        cam_main=os.path.join(directory, "cam_main.mp4"),
        cam_alt=os.path.join(directory, "cam_alt.mp4"),
        team_voice=os.path.join(directory, "team_voice.wav"),
        player_voice=os.path.join(directory, "player_voice.wav"),
        edge_tail=os.path.join(directory, "edge_tail.mp4"),
        corrupt_cam=os.path.join(directory, "corrupt_cam.mp4"),
    )
    if not force and all(os.path.exists(p) for p in paths.all_paths()):
        return paths
    _encode_camera(
        paths.cam_main,
        width=WIDTH,
        height=HEIGHT,
        fps=25,
        frames=55,
        layout="stereo",
        sample_rate=48_000,
        samples=105_600,
        color=(1, 1, 1, 1),  # BT.709
        seed=SEED,
    )
    _encode_camera(
        paths.cam_alt,
        width=480,
        height=270,
        fps=30,
        frames=66,
        layout="5.1",
        sample_rate=48_000,
        samples=105_600,
        color=(5, 5, 5, 1),  # BT.470BG
        seed=SEED + 1,
    )
    _write_wav(
        paths.team_voice,
        _tone_bank(105_600, 48_000, [180.0, 300.0, 420.0, 60.0, 520.0, 640.0], 0.06),
        48_000,
    )
    _write_wav(
        paths.player_voice,
        _tone_bank(105_840, 44_100, [330.0, 440.0], 0.05),
        44_100,
    )
    _encode_camera(
        paths.edge_tail,
        width=320,
        height=180,
        fps=25,
        frames=5,
        layout="stereo",
        sample_rate=48_000,
        samples=4_800,
        color=(1, 1, 1, 1),
        seed=SEED + 2,
    )
    with open(paths.cam_main, "rb") as handle:
        raw = handle.read()
    truncated = raw[: int(len(raw) * 0.55)]
    _atomic_write(paths.corrupt_cam, lambda temp: open(temp, "wb").write(truncated))
    return paths


def sample_digests(paths: SampleSet) -> dict[str, str]:
    from .probe import file_sha256

    return {os.path.basename(p): file_sha256(p) for p in paths.all_paths()}
