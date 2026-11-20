"""Offline media-chunk encoding and decoding.

Chunk files are the unit of delivery and of resume, so their decoded length
must be predictable:

* **Opus in Ogg at 48 kHz** is the primary codec.  Its decoder returns exactly
  the number of samples that were encoded (verified end-to-end), which is what
  makes the manifest's media ranges line up with the decoded files.
* **AAC in MP4** is the fallback for 44.1 and 96 kHz targets, where libopus
  refuses to open.  The AAC encoder always emits whole 1024-sample frames, so
  the trailing pad is recorded explicitly and trimmed on decode; it is never
  counted as media time.

The encoder is fed only complete frames and a short final frame; PyAV's
resampler/encoder state is per chunk, so chunks are independent and a resume
can rebuild any single chunk without touching its neighbours.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

import av
import numpy as np

from .errors import Code, Diagnostic
from .normalize import CANONICAL_LAYOUTS, canonical_layout

OPUS_RATE = 48000
AAC_FRAME = 1024
OPUS_FRAME = 960


class CodecError(RuntimeError):
    """Encoding or decoding a chunk failed."""

    def __init__(self, code: Code, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message

    def diagnostic(self) -> Diagnostic:
        return Diagnostic(self.code, self.message)


@dataclass(frozen=True)
class CodecChoice:
    codec: str
    container: str
    extension: str
    frame_size: int
    pads: bool

    @property
    def is_lossless_length(self) -> bool:
        return not self.pads


def choose_codec(rate: int) -> CodecChoice:
    """Pick the codec for a target sample rate.

    Opus only accepts 48 kHz; for the other supported rates AAC is used and the
    frame padding is tracked in the chunk metadata.
    """
    if rate == OPUS_RATE:
        return CodecChoice("libopus", "ogg", ".ogg", OPUS_FRAME, pads=False)
    if rate in (44100, 96000):
        return CodecChoice("aac", "mp4", ".m4a", AAC_FRAME, pads=True)
    raise CodecError(
        Code.UNSUPPORTED_LAYOUT,
        f"no encoder configured for {rate} Hz "
        f"(supported targets: 44100, 48000, 96000)",
    )


def _frame_from(samples: np.ndarray, rate: int, layout: str) -> av.AudioFrame:
    # Interleaved 'flt' works for every layout including 8-channel 7.1, where
    # PyAV's planar helper mis-sizes the buffer.
    interleaved = np.ascontiguousarray(samples.T).reshape(1, -1)
    frame = av.AudioFrame.from_ndarray(interleaved, format="flt", layout=layout)
    frame.sample_rate = rate
    frame.time_base = Fraction(1, rate)
    return frame


def encode_chunk(
    path: Path,
    samples: np.ndarray,
    rate: int,
    layout: str,
    *,
    bit_rate: int,
    frame_size: int | None = None,
) -> dict[str, int]:
    """Encode one chunk atomically (via a temporary sibling file).

    Returns ``{"encoded_samples", "padding_samples", "size_bytes"}``.
    """
    canonical = canonical_layout(layout)
    if canonical is None:
        raise CodecError(Code.UNSUPPORTED_LAYOUT, f"unsupported layout {layout!r}")
    choice = choose_codec(rate)
    if samples.size == 0:
        raise CodecError(Code.EMPTY_STREAM, "refusing to encode an empty chunk")
    if samples.shape[1] != len(CANONICAL_LAYOUTS[canonical]):
        raise CodecError(
            Code.LAYOUT_MISMATCH,
            f"chunk has {samples.shape[1]} channels, layout {canonical} needs "
            f"{len(CANONICAL_LAYOUTS[canonical])}",
        )
    if not np.isfinite(samples).all():
        raise CodecError(Code.NONFINITE_SAMPLE, "chunk contains NaN/Inf")
    step = frame_size or choice.frame_size
    total = int(samples.shape[0])
    padded = int(math.ceil(total / step) * step)
    padding = padded - total
    if padded > total:
        samples = np.concatenate(
            [samples, np.zeros((padded - total, samples.shape[1]), dtype=np.float32)],
            axis=0,
        )
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".part")
    container = av.open(str(tmp), "w", format=choice.container)
    try:
        stream = container.add_stream(choice.codec, rate=rate)
        stream.layout = canonical
        stream.bit_rate = int(bit_rate)
        if choice.codec == "libopus":
            # Constant bitrate keeps the measured kbps close to the target.
            stream.options = {"vbr": "off"}
        for offset in range(0, padded, step):
            frame = _frame_from(samples[offset : offset + step], rate, canonical)
            frame.pts = offset
            for packet in stream.encode(frame):
                container.mux(packet)
        for packet in stream.encode(None):
            container.mux(packet)
    except (av.error.FFmpegError, ValueError) as exc:  # pragma: no cover - codec-specific
        container.close()
        tmp.unlink(missing_ok=True)
        raise CodecError(Code.ENCODE_FAILURE, f"encoding failed: {exc}") from exc
    finally:
        try:
            container.close()
        except av.error.FFmpegError:
            pass
    tmp.replace(path)
    return {
        "encoded_samples": padded,
        "padding_samples": padding,
        "size_bytes": path.stat().st_size,
    }


@dataclass
class DecodedChunk:
    samples: np.ndarray
    rate: int
    layout: str
    decoded_samples: int
    padding_samples: int

    @property
    def samples_out(self) -> int:
        return int(self.samples.shape[0])


def _frame_to_planar(frame: av.AudioFrame) -> np.ndarray:
    """Copy a decoded frame to ``(samples, channels)`` float32.

    ``AudioFrame.to_ndarray`` over-reads the alignment plane that FFmpeg keeps
    for 7.1 (9 planes for 8 channels) and crashes, so planes are copied
    individually from the reliable ``planes`` view instead.
    """
    channels = len(frame.layout.channels)
    n = int(frame.samples)
    if frame.format.is_planar:
        out = np.empty((n, channels), dtype=np.float32)
        step = frame.format.bytes  # bytes per sample for the planar format
        for c in range(channels):
            raw = bytes(frame.planes[c])
            usable = min(n, len(raw) // step)
            arr = np.frombuffer(raw, dtype="<f4", count=usable)
            out[:usable, c] = arr
            if usable < n:
                out[usable:, c] = 0.0
        return out
    width = channels * frame.format.bytes
    raw = bytes(frame.planes[0])
    usable = min(n, len(raw) // width)
    arr = np.frombuffer(raw, dtype="<f4", count=usable * channels)
    out = np.zeros((n, channels), dtype=np.float32)
    out[:usable] = arr.reshape(usable, channels)
    return out


def decode_chunk(
    path: Path,
    *,
    expected_samples: int | None = None,
    expected_layout: str | None = None,
) -> DecodedChunk:
    """Decode a chunk, trimming codec frame padding when it is known."""
    try:
        container = av.open(str(path))
    except av.error.FFmpegError as exc:
        raise CodecError(Code.ENCODE_FAILURE, f"cannot open {path.name}: {exc}") from exc
    try:
        stream = container.streams.audio[0]
        chunks: list[np.ndarray] = []
        for frame in container.decode(stream):
            chunks.append(_frame_to_planar(frame))
        rate = stream.rate
        layout = stream.layout.name
    except (av.error.FFmpegError, IndexError, ValueError) as exc:
        raise CodecError(Code.ENCODE_FAILURE, f"cannot decode {path.name}: {exc}") from exc
    finally:
        container.close()
    if not chunks:
        samples = np.zeros((0, 1), dtype=np.float32)
    else:
        samples = np.concatenate(chunks, axis=0).astype(np.float32)
    decoded = int(samples.shape[0])
    padding = 0
    if expected_samples is not None and decoded > expected_samples:
        padding = decoded - expected_samples
        samples = samples[:expected_samples]
    if expected_layout is not None:
        canonical = canonical_layout(expected_layout)
        if canonical is not None and layout != canonical:
            raise CodecError(
                Code.LAYOUT_MISMATCH,
                f"{path.name} decodes as {layout}, expected {canonical}",
            )
    return DecodedChunk(
        samples=samples,
        rate=rate,
        layout=layout,
        decoded_samples=decoded,
        padding_samples=padding,
    )


def probe_chunk(path: Path) -> tuple[int, int, str]:
    """Return ``(decoded_samples, rate, layout)`` without keeping the audio."""
    try:
        container = av.open(str(path))
    except av.error.FFmpegError as exc:
        raise CodecError(Code.ENCODE_FAILURE, f"cannot open {path.name}: {exc}") from exc
    try:
        stream = container.streams.audio[0]
        total = 0
        for frame in container.decode(stream):
            total += int(frame.samples)
        rate = int(stream.rate)
        layout = stream.layout.name
    except (av.error.FFmpegError, IndexError, ValueError) as exc:
        raise CodecError(Code.ENCODE_FAILURE, f"cannot decode {path.name}: {exc}") from exc
    finally:
        container.close()
    return total, rate, layout
