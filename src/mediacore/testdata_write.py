"""Deterministic writers for the synthetic media fixtures.

Video is mpeg4 at 160x90, voice is AAC at 44.1 kHz/stereo and tactical cues
are AAC at 48 kHz/mono, so the three streams share a wall-clock timeline but
not a timebase.  All inputs are generated from a fixed seed and written with
a faststart (moov-first) layout so tail truncation never invalidates the
index.
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from .timebase import US_PER_SECOND, samples_for_duration

SYNTH_FPS = 30
SYNTH_VIDEO_TB = "1/15360"
SYNTH_VOICE_RATE = 44_100
SYNTH_TACTICAL_RATE = 48_000
SYNTH_WIDTH = 160
SYNTH_HEIGHT = 90


def _write_multitrack(path: Path, *, duration_us: int, offsets_us: int | None) -> None:
    """Write the canonical three-track MP4.

    ``offsets_us`` shifts every stream's presentation start.  Each stream is
    shifted by ``offset / stream_timebase`` ticks, so video and both audio
    tracks begin at the same *wall-clock* position despite different
    timebases.

    A positive offset produces the non-zero-origin fixture; a negative one is
    clamped per-stream to the most negative frame/sample the encoder accepts,
    which still yields a non-zero, track-specific origin.

    When the MP4 is written with a trailing ``moov`` (i.e. not faststart) the
    sample-chunk offsets stored in ``stco`` are absolute file offsets.  Damage
    fixtures that delete bytes therefore have to relocate them; the clean
    fixture is written faststart so its ``moov`` precedes ``mdat`` and its
    ``stco`` offsets are stable against tail truncation.
    """
    import av

    container = av.open(
        str(path), "w", format="mp4", options={"movflags": "faststart"}
    )
    try:
        video = _add_video_stream(container)
        voice = _add_audio_stream(container, SYNTH_VOICE_RATE, "stereo")
        tactical = _add_audio_stream(container, SYNTH_TACTICAL_RATE, "mono")

        shift_us = offsets_us or 0
        vtb = _stream_tb(video)
        offset_v = max(vtb.from_us(shift_us), -SYNTH_FPS)
        frames = int(round(duration_us / (US_PER_SECOND / SYNTH_FPS)))
        for index in range(frames):
            frame = av.VideoFrame.from_ndarray(_video_frame(index), format="rgb24")
            frame = frame.reformat(format="yuv420p")
            frame.pts = index + offset_v
            for packet in video.encode(frame):
                container.mux(packet)
        for packet in video.encode():
            container.mux(packet)

        _write_audio_track(
            container, voice, SYNTH_VOICE_RATE, duration_us, freq=330.0,
            offset_us=shift_us,
        )
        _write_audio_track(
            container, tactical, SYNTH_TACTICAL_RATE, duration_us, freq=990.0,
            offset_us=shift_us, channels=1,
        )
    finally:
        container.close()


def _stream_tb(stream):
    """Timebase of an *output* stream, which is 1/framerate before muxing.

    PyAV does not populate ``stream.time_base`` on encoders until the first
    packet is muxed, so fall back to the codec's rational frame rate.
    """
    from .timebase import Timebase

    if stream.time_base is not None:
        return Timebase.parse(stream.time_base)
    rate = getattr(stream.codec_context, "framerate", None) or getattr(
        stream, "average_rate", None
    )
    if rate is None:
        return Timebase(1, stream.codec_context.sample_rate or 1000)
    return Timebase(rate.denominator, rate.numerator)


def _add_video_stream(container):
    stream = container.add_stream("mpeg4", rate=SYNTH_FPS)
    stream.width = SYNTH_WIDTH
    stream.height = SYNTH_HEIGHT
    stream.pix_fmt = "yuv420p"
    stream.bit_rate = 320_000
    stream.options = {"max_b_frames": "0", "g": str(SYNTH_FPS)}
    return stream


def _add_audio_stream(container, rate: int, layout: str):
    stream = container.add_stream("aac", rate=rate)
    stream.layout = layout
    stream.bit_rate = 64_000 if layout == "stereo" else 40_000
    return stream


def _write_audio_track(
    container,
    stream,
    rate: int,
    duration_us: int,
    *,
    freq: float,
    offset_us: int,
    channels: int = 2,
) -> None:
    import av

    signal = synthetic_signal(rate, duration_us, freq=freq, channels=channels,
                              amplitude=0.18)
    n = signal.shape[1]
    tb = _stream_tb(stream)
    offset = tb.from_us(offset_us) if offset_us else 0
    chunk = 1024
    for start in range(0, n, chunk):
        block = signal[:, start : start + min(chunk, n - start)]
        frame = av.AudioFrame.from_ndarray(block, format="fltp", layout=stream.layout.name)
        frame.sample_rate = rate
        frame.pts = start + offset
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)


def _write_wav(path: Path, data: np.ndarray, sample_rate: int) -> Path:
    import soundfile as sf

    sf.write(str(path), data.T, sample_rate, subtype="PCM_16", format="WAV")
    return path


def _write_rtp_wrapped(path: Path, *, sample_rate: int) -> Path:
    """An ADTS stream used by the wrap/duplicate timestamp unit tests.

    ADTS carries no explicit PTS, so wrap and duplicate-timestamp handling is
    exercised on the conversion layer (``align_us`` / ``_Unwrapper``) using
    this file's real packet sizes and sample counts; the file itself is a
    plain ADTS stream that PyAV can open, which keeps the fixture honest.
    """
    import av

    container = av.open(str(path), "w", format="adts")
    try:
        stream = _add_audio_stream(container, sample_rate, "stereo")
        _write_audio_track(
            container, stream, sample_rate, 1_000_000, freq=440.0, offset_us=0
        )
    finally:
        container.close()
    return path


def _write_timeline(path: Path, *, clips: list[dict], transitions: list[dict]) -> Path:
    """Write a timeline JSON next to the media fixtures.

    Track declarations point at the *clean* fixture's stream indices, because
    all three timeline fixtures are meant to be run against the same healthy
    media.  They differ only in how the clips relate, so the failure each one
    provokes (overlap / cycle / none) is unambiguous.
    """
    payload = {
        "tracks": [
            {
                "id": "voice",
                "path": str(path.parent / "clean.mp4"),
                "kind": "audio",
                "stream_index": 1,
                "timebase": "1/44100",
                "sample_rate": 44_100,
                "label": "team voice",
            },
            {
                "id": "tactical",
                "path": str(path.parent / "clean.mp4"),
                "kind": "audio",
                "stream_index": 2,
                "timebase": "1/48000",
                "sample_rate": 48_000,
                "label": "tactical cues",
            },
        ],
        "clips": clips,
        "transitions": transitions,
    }
    path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    return path

def synthetic_signal(
    sample_rate: int,
    duration_us: int,
    *,
    freq: float,
    amplitude: float = 0.2,
    channels: int = 2,
    phase: float = 0.0,
) -> np.ndarray:
    """Return a deterministic float32 tone, shape ``(channels, samples)``.

    Sample count is derived from the *microsecond* duration with the same
    rounding the audio module uses, so a fixture and the pipeline agree on the
    intended length to the sample.
    """
    from .timebase import US_PER_SECOND, samples_for_duration

    n = samples_for_duration(duration_us, sample_rate)
    t = np.arange(n, dtype=np.float64) / sample_rate
    base = amplitude * np.sin(2 * np.pi * freq * t + phase)
    # A quiet second tone makes channel identity checkable without changing
    # loudness materially.
    data = np.empty((channels, n), dtype=np.float32)
    for ch in range(channels):
        data[ch] = (base * (1.0 + 0.15 * ch)).astype(np.float32)
    return np.ascontiguousarray(data)


def _video_frame(index: int) -> np.ndarray:
    """A deterministic RGB frame: moving bars plus a frame-number ramp."""
    frame = np.zeros((SYNTH_HEIGHT, SYNTH_WIDTH, 3), dtype=np.uint8)
    ramp = np.arange(SYNTH_WIDTH, dtype=np.uint16)
    frame[:, :, 0] = ((ramp + index * 5) % 256).astype(np.uint8)
    frame[:, :, 1] = np.uint8((index * 7) % 256)
    bar = (index * 4) % SYNTH_WIDTH
    frame[:, bar : bar + 8, 2] = 255
    return frame

