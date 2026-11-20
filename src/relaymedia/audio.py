"""Audio decode, streaming resample, channel mapping and silence detection.

One decode pass per source. PyAV decodes packets, an ``AudioFifo`` buffers the
resampler output so input frame boundaries never clip an output block, and
every output block remembers the exact source sample span it came from.

Channel layouts are never silently rewritten: a source whose channel count
differs from the run's output layout gets an explicit :class:`ChannelMap`
record (mono duplicated into L/R, or surplus channels listed as dropped).
Silence detection is window-based; a short non-silent pulse between two
silent windows is reported as its own interval, so windows are never blindly
merged across it.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from .errors import ErrorRecord, MediaError, Stage

# Down-mix / up-mix target names keyed by output channel count.
_LAYOUT_NAMES = {1: "mono", 2: "stereo", 6: "5.1"}


@dataclass(frozen=True, slots=True)
class AudioMeta:
    source: str
    sample_rate: int
    channels: int
    layout_name: str
    total_samples: int  # source-rate samples decoded before resampling


@dataclass(frozen=True, slots=True)
class ChannelMap:
    source: str
    input_channels: int
    output_channels: int
    action: str  # "passthrough" | "duplicated_mono" | "dropped_surplus"
    detail: str


@dataclass(frozen=True, slots=True)
class AudioBlock:
    """Resampled float32 block, shape (samples, channels), on the output grid."""

    data: np.ndarray
    source: str
    src_start: int
    src_end: int


@dataclass(frozen=True, slots=True)
class SilenceInterval:
    start_sample: int
    end_sample: int
    start_seconds: float
    end_seconds: float
    rms: float
    short: bool = False  # True on non-silent pulses shorter than min_pulse_ms


@dataclass(slots=True)
class DecodeResult:
    blocks: list[AudioBlock] = field(default_factory=list)
    errors: list[ErrorRecord] = field(default_factory=list)
    channel_maps: list[ChannelMap] = field(default_factory=list)
    meta: AudioMeta | None = None

    @property
    def total_output_samples(self) -> int:
        return sum(b.data.shape[0] for b in self.blocks)


def layout_name(channels: int) -> str:
    return _LAYOUT_NAMES.get(channels, f"{channels}ch")


def _apply_channels(frame: av.AudioFrame, out_channels: int) -> np.ndarray:
    arr = frame.to_ndarray()  # planar: (channels, samples), dtype follows codec
    if arr.dtype == np.int16:
        arr = arr.astype(np.float32) / 32768.0
    elif arr.dtype == np.int32:
        arr = arr.astype(np.float32) / 2147483648.0
    else:
        arr = arr.astype(np.float32, copy=False)
    in_ch = arr.shape[0]
    if in_ch == out_channels:
        return arr.T.copy()
    if in_ch == 1 and out_channels == 2:
        return np.repeat(arr.T, 2, axis=1)
    if in_ch > out_channels:
        return arr[:out_channels].T.copy()
    padded = np.zeros((arr.shape[1], out_channels), dtype=np.float32)
    padded[:, :in_ch] = arr.T
    return padded


def _make_map(source: str, in_ch: int, out_ch: int) -> ChannelMap:
    if in_ch == out_ch:
        return ChannelMap(source, in_ch, out_ch, "passthrough", "identical layout")
    if in_ch == 1 and out_ch == 2:
        return ChannelMap(source, in_ch, out_ch, "duplicated_mono", "mono duplicated to L and R")
    if in_ch > out_ch:
        return ChannelMap(
            source, in_ch, out_ch, "dropped_surplus",
            f"channels {out_ch}..{in_ch - 1} dropped (explicit, recorded)",
        )
    return ChannelMap(
        source, in_ch, out_ch, "padded_silence",
        f"channels {in_ch}..{out_ch - 1} padded with silence",
    )


def decode_source(path: str | Path, out_rate: int, out_channels: int) -> DecodeResult:
    """Decode one file to float32 blocks at ``out_rate``/``out_channels``."""
    path = Path(path)
    result = DecodeResult()
    try:
        container = av.open(str(path))
    except av.AVError as exc:  # pragma: no cover - exercised via damaged fixtures
        raise MediaError(f"cannot open audio source {path}: {exc}", stage=Stage.DECODE) from exc
    stream = container.streams.audio[0]
    in_rate = stream.codec_context.sample_rate or stream.average_rate
    in_rate = int(in_rate)
    in_ch = int(stream.codec_context.channels or 1)
    resampler = av.AudioResampler(format="fltp", layout=out_channels, rate=out_rate)
    fifo = av.AudioFifo(format="fltp")
    result.channel_maps.append(_make_map(str(path), in_ch, out_channels))
    src_cursor = 0
    total_src = 0
    try:
        for packet in container.demux(stream):
            try:
                frames = packet.decode()
            except av.AVError as exc:
                offset = packet.pos if packet.pos is not None else -1
                media_time = None
                if packet.pts is not None and stream.time_base is not None:
                    media_time = float(packet.pts * stream.time_base)
                result.errors.append(
                    ErrorRecord(
                        stage=Stage.DECODE.value, kind="MediaError",
                        message=f"decode failure: {exc}", source=str(path),
                        offset_bytes=int(offset), line=-1, media_time=media_time,
                    )
                )
                continue  # recoverable: salvage the prefix, keep draining packets
            for frame in frames:
                total_src += frame.samples
                start = src_cursor
                src_cursor += frame.samples
                mapped = _apply_channels(frame, out_channels)
                mono = av.AudioFrame.from_ndarray(
                    np.ascontiguousarray(mapped.T), format="fltp", layout=out_channels
                )
                mono.sample_rate = in_rate
                for out_frame in resampler.resample(mono):
                    fifo.write(out_frame)
                    while fifo.samples >= out_frame.samples:
                        chunk = fifo.read(out_frame.samples)
                        result.blocks.append(
                            AudioBlock(chunk.to_ndarray().T.copy(), str(path), start, src_cursor)
                        )
        for out_frame in resampler.resample(None):
            fifo.write(out_frame)
        while fifo.samples > 0:
            take = min(fifo.samples, 1024)
            chunk = fifo.read(take)
            result.blocks.append(
                AudioBlock(chunk.to_ndarray().T.copy(), str(path), src_cursor, src_cursor)
            )
    finally:
        container.close()
    result.meta = AudioMeta(
        source=str(path), sample_rate=in_rate, channels=in_ch,
        layout_name=layout_name(in_ch), total_samples=total_src,
    )
    return result


def concatenate(blocks: list[AudioBlock]) -> np.ndarray:
    if not blocks:
        return np.empty((0, 0), dtype=np.float32)
    return np.concatenate([b.data for b in blocks], axis=0)


def detect_silence(
    pcm: np.ndarray,
    sample_rate: int,
    *,
    window_ms: float = 50.0,
    threshold: float = 0.01,
    min_pulse_ms: float = 20.0,
) -> tuple[list[SilenceInterval], list[SilenceInterval]]:
    """Return (silence intervals, non-silent pulses).

    Windows are scored independently by mono-mix RMS, then adjacent windows of
    the same verdict are run-length grouped. A non-silent run shorter than
    ``min_pulse_ms`` stays a *separate* pulse flagged ``short=True`` instead of
    being swallowed by neighbouring silence, so cross-window content never
    merges two silent spans into one.
    """
    if pcm.ndim == 1:
        pcm = pcm[:, None]
    win = max(1, int(round(sample_rate * window_ms / 1000.0)))
    min_pulse = max(1, int(round(sample_rate * min_pulse_ms / 1000.0)))
    mono = pcm.mean(axis=1)
    verdicts: list[tuple[int, int, float, bool]] = []  # start, end, rms, is_silent
    for start in range(0, len(mono), win):
        end = min(len(mono), start + win)
        rms = float(np.sqrt(np.mean(np.square(mono[start:end], dtype=np.float64))))
        verdicts.append((start, end, rms, rms < threshold))
    silent: list[SilenceInterval] = []
    pulses: list[SilenceInterval] = []
    for start, end, rms, is_silent in verdicts:
        target = silent if is_silent else pulses
        if target and target[-1].end_sample == start:
            prev = target[-1]
            merged = SilenceInterval(
                prev.start_sample, end, prev.start_seconds, end / sample_rate,
                max(prev.rms, rms),
                short=prev.short or (not is_silent and end - start < min_pulse),
            )
            target[-1] = merged
        else:
            target.append(
                SilenceInterval(start, end, start / sample_rate, end / sample_rate,
                                rms, short=(not is_silent and end - start < min_pulse))
            )
    return silent, pulses


def write_wav(path: str | Path, pcm: np.ndarray, sample_rate: int) -> None:
    """Write a deterministic 16-bit PCM WAV via soundfile."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), pcm, sample_rate, subtype="PCM_16")
