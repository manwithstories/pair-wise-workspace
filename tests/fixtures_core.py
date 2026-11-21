"""Synthetic MP4/MOV fixture generation (part 1: core encoder helpers).

All sample media is generated locally from NumPy arrays — no external files
are ever referenced.  Every frame carries an explicit PTS so the scenario
builders in :mod:`tests.fixtures` can emulate wraparound, drift, VFR gaps,
silence and truncation deterministically.

Encoding notes (PyAV 13 / FFmpeg):
* PTS are stamped on frames, never rewritten on retained packets.
* The AVPacket buffer is shared/reused across the two encoders, so time
  interleaving their encode() calls corrupts in-flight packets.  Both
  streams are created up front; video is encoded and muxed to completion
  (without flushing), then audio, and only then are both encoders flushed.
  The MOV muxer reorders packets into proper on-disk interleaving.
* The AAC encoder's first packet has a negative PTS (one-frame priming),
  which the MOV container records as an edit list.
* On the read side, iterate the *global* ``container.demux()`` and filter by
  stream — single-stream ``demux(stream)`` is unreliable in this PyAV build
  and collapses multi-stream packet counts.
"""

from __future__ import annotations

import math
import os
from dataclasses import dataclass
from fractions import Fraction

import av
import numpy as np

# Conservative resource caps; fixtures stay deliberately tiny.
MAX_FRAMES = 4096
LIMIT_BYTES = 256 * 1024 * 1024


@dataclass
class SyntheticSpec:
    """Declarative description of one synthetic clip."""

    path: str
    duration_s: float = 2.0
    width: int = 160
    height: int = 120
    frame_rate: float = 12.0
    sample_rate: int = 24_000
    channels: int = 1
    # Explicit video PTS ticks (stream time base units); None => uniform.
    video_pts: list[int] | None = None
    # Explicit audio PTS ticks (audio_time_base units); None => uniform.
    audio_pts: list[int] | None = None
    video_time_base: float = 1 / 12_000
    audio_time_base: float = 1 / 48_000
    # Frame indices whose audio region is replaced with digital silence.
    silent_frames: set[int] | None = None
    # Deterministic content seed; identical seeds => identical content.
    seed: int = 1
    with_audio: bool = True
    # Target total bitrate (kbps); None uses a fixed CRF.  The encoder
    # overshoots an ABR target on this tiny synthetic content, so a measured
    # correction factor maps the requested value onto an effective bitrate.
    target_kbps: int | None = None
    # Empirical ABR overshoot factor for ultrafast x264 on these clips.
    abr_correction: float = 0.9


def _video_frame(frame_idx: int, spec: SyntheticSpec) -> av.VideoFrame:
    """Build a deterministic YUV420p frame whose content varies per index."""
    rng = np.random.default_rng(spec.seed * 7919 + frame_idx)
    level = 40 + (frame_idx * 7) % 180
    y = np.full((spec.height, spec.width), level, dtype=np.uint8)
    noise = rng.integers(0, 18, size=(spec.height, spec.width), dtype=np.uint8)
    y = np.clip(y.astype(np.int16) + noise.astype(np.int16), 0, 255).astype(np.uint8)
    half_h, half_w = spec.height // 2, spec.width // 2
    u = np.full((half_h, half_w), 128, dtype=np.uint8)
    v = np.full((half_h, half_w), 128, dtype=np.uint8)
    frame = av.VideoFrame(spec.width, spec.height, "yuv420p")
    frame.planes[0].update(y.tobytes())
    frame.planes[1].update(u.tobytes())
    frame.planes[2].update(v.tobytes())
    return frame


def _audio_samples(frame_idx: int, spec: SyntheticSpec, n: int) -> np.ndarray:
    """One AAC frame of mono/stereo float32 tone, or silence when flagged."""
    if spec.silent_frames and frame_idx in spec.silent_frames:
        shape = (n, spec.channels) if spec.channels > 1 else (n,)
        return np.zeros(shape, dtype=np.float32)
    start = frame_idx * 1024
    t = (np.arange(start, start + n) / spec.sample_rate).astype(np.float32)
    tone = 0.35 * np.sin(2 * math.pi * 440 * t)
    if spec.channels > 1:
        tone = np.repeat(tone[:, None], spec.channels, axis=1)
    return tone.astype(np.float32)


def write_synthetic(spec: SyntheticSpec) -> str:
    """Encode and mux the clip described by ``spec``; return its path."""
    if os.path.exists(spec.path):
        os.remove(spec.path)
    container = av.open(spec.path, "w", format="mov", options={"movflags": "faststart"})
    try:
        vstream = container.add_stream(
            "libx264", rate=Fraction(spec.frame_rate).limit_denominator(1000)
        )
        vstream.width, vstream.height = spec.width, spec.height
        vstream.pix_fmt = "yuv420p"
        if spec.target_kbps is not None:
            # Reserve headroom for the AAC track; correct measured overshoot.
            video_target = max(
                32_000, int(spec.target_kbps * 1000 * spec.abr_correction) - 64_000
            )
            vstream.bit_rate = video_target
            vstream.options = {"preset": "ultrafast"}
        else:
            vstream.options = {"preset": "ultrafast", "crf": "28"}
        vtb = Fraction(spec.video_time_base).limit_denominator(1_000_000)
        vstream.time_base = vtb
        astream = None
        if spec.with_audio:
            astream = container.add_stream("aac", rate=spec.sample_rate)
            astream.layout = "mono" if spec.channels == 1 else "stereo"
            astream.bit_rate = 64_000

        n_frames = int(round(spec.duration_s * spec.frame_rate))
        if n_frames > MAX_FRAMES:
            raise ValueError("fixture frame count exceeds safety limit")
        # Ticks per frame = (seconds per frame) / (seconds per tick).
        uniform_step = (1 / spec.frame_rate) / spec.video_time_base

        # Phase 1: video, encoded and muxed immediately but NOT flushed yet.
        for i in range(n_frames):
            vpts = (
                spec.video_pts[i]
                if spec.video_pts is not None
                else int(round(i * uniform_step))
            )
            frame = _video_frame(i, spec)
            frame.pts = vpts
            frame.time_base = vtb
            for packet in vstream.encode(frame):
                container.mux(packet)

        # Phase 2: audio, one in-flight packet at a time.  The AAC encoder's
        # first packet carries a negative PTS (priming) recorded as an edit
        # list; readers normalize it against the stream start time.
        n_audio = 0
        if astream is not None:
            n_audio = int(math.ceil(spec.duration_s * spec.sample_rate / 1024))
            for i in range(n_audio):
                if spec.audio_pts is not None:
                    apts = int(
                        round(spec.audio_pts[i] * spec.audio_time_base * spec.sample_rate)
                    )
                else:
                    apts = i * 1024  # encoder-native sample ticks
                pcm = _audio_samples(i, spec, 1024)
                af = av.AudioFrame.from_ndarray(
                    pcm.reshape(-1, 1).T if spec.channels == 1 else pcm.T,
                    format="fltp",
                    layout="mono" if spec.channels == 1 else "stereo",
                )
                af.sample_rate = spec.sample_rate
                af.pts = apts
                for packet in astream.encode(af):
                    container.mux(packet)

        # Flush only after both streams have been fully fed.
        for packet in vstream.encode():
            container.mux(packet)
        if astream is not None:
            for packet in astream.encode():
                container.mux(packet)
    finally:
        container.close()
    if os.path.getsize(spec.path) > LIMIT_BYTES:
        raise ValueError("fixture exceeds size safety limit")
    return spec.path


def truncate_file(path: str, keep_fraction: float = 0.75) -> str:
    """Cut the file tail to emulate a truncated recording."""
    size = os.path.getsize(path)
    with open(path, "r+b") as fh:
        fh.truncate(int(size * keep_fraction))
    return path


def corrupt_mdat(path: str, *, offset_fraction: float = 0.35, span: int = 1024) -> str:
    """Corrupt mdat payload bytes so at least one packet fails to decode.

    The mid-file payload region contains coded video slices; zeroing a wide
    span there (empirically around 30-40% into the file for these clips)
    produces packets the decoder rejects while the moov box and container
    structure stay valid, so the demuxer remains usable and the bad region is
    isolated as a recoverable error.
    """
    size = os.path.getsize(path)
    start = int(size * offset_fraction)
    with open(path, "r+b") as fh:
        fh.seek(start)
        blob = bytearray(fh.read(span))
        for i in range(0, len(blob), 2):
            blob[i] = 0x00 if blob[i] != 0x00 else 0xFF
        fh.seek(start)
        fh.write(blob)
    return path
