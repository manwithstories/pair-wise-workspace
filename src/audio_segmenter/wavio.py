"""PCM WAV decoding and validation.

The ``wave`` module accepts a narrow but still too generous subset of files:
it will happily open IEEE-float and some compressed WAVs and hand back bytes
that do not mean what a PCM reader expects.  We therefore validate the
``fmt`` chunk ourselves before trusting any sample data.

Errors are always raised as :class:`WavError` with a stable ``code`` so the
CLI can map them onto exit codes and the batch layer can record them without
special-casing message text.
"""

from __future__ import annotations

import hashlib
import os
import struct
import wave
from dataclasses import dataclass
from typing import BinaryIO, Optional

import numpy as np

WAVE_FORMAT_PCM = 0x0001
WAVE_FORMAT_IEEE_FLOAT = 0x0003
WAVE_FORMAT_EXTENSIBLE = 0xFFFE

SUPPORTED_SAMPLE_WIDTHS = (2, 3)  # PCM16 and PCM24


class WavError(Exception):
    """Raised for any unusable input file."""

    def __init__(self, code: str, message: str, path: Optional[str] = None) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.path = path

    def __str__(self) -> str:  # pragma: no cover - trivial
        if self.path:
            return f"{self.path}: {self.message}"
        return self.message


@dataclass(frozen=True)
class WavInfo:
    path: str
    sample_rate: int
    channels: int
    sample_width: int
    nframes: int

    @property
    def duration_seconds(self) -> float:
        return self.nframes / self.sample_rate

    @property
    def is_stereo(self) -> bool:
        return self.channels == 2


@dataclass
class WavData:
    info: WavInfo
    samples: np.ndarray  # float32 in [-1, 1], shape (nframes, channels)

    @property
    def sample_rate(self) -> int:
        return self.info.sample_rate

    @property
    def nframes(self) -> int:
        return self.info.nframes


def _read_fmt_chunk(fh: BinaryIO, path: str) -> tuple[int, int, int, int]:
    """Parse the RIFF header and fmt chunk, rejecting unsupported encodings."""

    header = fh.read(12)
    if len(header) < 12:
        raise WavError("wav_corrupt", "file is too short to be a RIFF/WAVE file", path)
    riff, riff_size, wave_id = struct.unpack("<4sI4s", header)
    if riff != b"RIFF" or wave_id != b"WAVE":
        raise WavError("wav_corrupt", "missing RIFF/WAVE signature", path)

    file_size = os.fstat(fh.fileno()).st_size
    if riff_size + 8 > file_size:
        raise WavError(
            "wav_corrupt",
            f"RIFF chunk declares {riff_size + 8} bytes but file has {file_size}",
            path,
        )
    if riff_size + 8 < file_size:
        # Trailing garbage is tolerated: some tools append metadata.
        pass

    fmt: Optional[tuple[int, int, int, int, int]] = None
    while True:
        chunk_header = fh.read(8)
        if len(chunk_header) < 8:
            break
        chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
        if chunk_id == b"fmt ":
            if chunk_size < 16:
                raise WavError("wav_corrupt", "fmt chunk is truncated", path)
            body = fh.read(chunk_size)
            if len(body) < 16:
                raise WavError("wav_corrupt", "fmt chunk is truncated", path)
            audio_format, channels, sample_rate, _byte_rate, block_align, bits = (
                struct.unpack("<HHIIHH", body[:16])
            )
            if audio_format == WAVE_FORMAT_EXTENSIBLE:
                if len(body) < 40:
                    raise WavError(
                        "wav_corrupt", "extensible fmt chunk is truncated", path
                    )
                (sub_format,) = struct.unpack("<H", body[24:26])
                audio_format = sub_format
            fmt = (audio_format, channels, sample_rate, block_align, bits)
            break
        fh.seek(chunk_size + (chunk_size & 1), os.SEEK_CUR)

    if fmt is None:
        raise WavError("wav_corrupt", "no fmt chunk found", path)

    audio_format, channels, sample_rate, block_align, bits = fmt
    if audio_format == WAVE_FORMAT_IEEE_FLOAT:
        raise WavError(
            "wav_unsupported_float", "IEEE float WAV is not supported; need PCM", path
        )
    if audio_format != WAVE_FORMAT_PCM:
        raise WavError(
            "wav_unsupported_codec",
            f"unsupported WAV encoding 0x{audio_format:04x}; need PCM",
            path,
        )
    if channels not in (1, 2):
        raise WavError(
            "wav_unsupported_channels",
            f"unsupported channel count {channels}; need mono or stereo",
            path,
        )
    if bits not in (16, 24):
        raise WavError(
            "wav_unsupported_bit_depth",
            f"unsupported bit depth {bits}; need PCM16 or PCM24",
            path,
        )
    expected_align = channels * (bits // 8)
    if block_align != expected_align:
        raise WavError(
            "wav_corrupt",
            f"block align {block_align} does not match {channels}x{bits}-bit",
            path,
        )
    if sample_rate <= 0:
        raise WavError("wav_corrupt", "sample rate must be positive", path)
    return channels, sample_rate, bits, block_align


def _decode_pcm24(raw: bytes, channels: int) -> np.ndarray:
    """Decode packed 24-bit little-endian samples to float32 in [-1, 1)."""

    data = np.frombuffer(raw, dtype=np.uint8)
    if data.size % 3:
        raise ValueError("PCM24 payload is not a multiple of 3 bytes")
    triples = data.reshape(-1, 3).astype(np.int32)
    signed = (triples[:, 0] | (triples[:, 1] << 8) | (triples[:, 2] << 16))
    signed = np.where(signed & 0x800000, signed - 0x1000000, signed)
    return (signed.astype(np.float32) / 8388608.0).reshape(-1, channels)


def read_wav(path: str) -> WavData:
    """Read and validate a PCM16/PCM24 mono or stereo WAV file."""

    if not os.path.exists(path):
        raise WavError("wav_not_found", "file does not exist", path)
    if not os.path.isfile(path):
        raise WavError("wav_not_found", "path is not a regular file", path)
    if os.path.getsize(path) == 0:
        raise WavError("wav_empty", "file is empty", path)

    try:
        with open(path, "rb") as fh:
            channels, sample_rate, bits, block_align = _read_fmt_chunk(fh, path)
    except WavError:
        raise
    except OSError as exc:
        raise WavError("wav_io_error", str(exc), path) from exc

    try:
        with wave.open(path, "rb") as wf:
            if wf.getcomptype() != "NONE":
                raise WavError(
                    "wav_unsupported_codec",
                    f"compressed WAV ({wf.getcomptype()}) is not supported",
                    path,
                )
            nframes = wf.getnframes()
            raw = wf.readframes(nframes)
    except wave.Error as exc:
        raise WavError("wav_corrupt", str(exc), path) from exc
    except OSError as exc:
        raise WavError("wav_io_error", str(exc), path) from exc
    except EOFError as exc:
        raise WavError("wav_corrupt", "unexpected end of file", path) from exc

    if nframes == 0:
        raise WavError("wav_empty", "file contains no sample frames", path)
    if len(raw) < nframes * block_align:
        raise WavError("wav_corrupt", "sample data is truncated", path)

    if bits == 16:
        samples = np.frombuffer(
            raw[: nframes * block_align], dtype="<i2"
        ).astype(np.float32) / 32768.0
        samples = samples.reshape(-1, channels)
    else:
        try:
            samples = _decode_pcm24(raw[: nframes * block_align], channels)
        except ValueError as exc:
            raise WavError("wav_corrupt", str(exc), path) from exc

    info = WavInfo(
        path=path,
        sample_rate=sample_rate,
        channels=channels,
        sample_width=bits // 8,
        nframes=nframes,
    )
    return WavData(info=info, samples=np.ascontiguousarray(samples))


def file_sha256(path: str, chunk_size: int = 1 << 20) -> str:
    """Content hash used for idempotent batch updates."""

    digest = hashlib.sha256()
    try:
        with open(path, "rb") as fh:
            while True:
                block = fh.read(chunk_size)
                if not block:
                    break
                digest.update(block)
    except OSError as exc:
        raise WavError("wav_io_error", str(exc), path) from exc
    return digest.hexdigest()
