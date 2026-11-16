"""WAV/FLAC reading, format validation, block iteration and atomic writes.

Reads go through ``soundfile``'s buffer API into ``array('d')`` so no numpy is
required.  Every input is validated before any sample is processed; a file
that fails validation raises :class:`InputFormatError` (or a subclass) and the
batch moves on to the next file.
"""

from __future__ import annotations

import array
import math
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator, Sequence

import soundfile as sf

from .model import (
    AudioBlock,
    InputFormatError,
    MAX_SAMPLE_RATE,
    MIN_SAMPLE_RATE,
    UnsupportedError,
    UsageError,
    WriteError,
    resolve_layout,
    ChannelLayout,
)
from .container import check_wav_integrity

#: Readable container formats.  FLAC is lossless and in scope for ingest.
READ_FORMATS = ("WAV", "FLAC")
WRITE_FORMATS = ("WAV",)

#: PCM widths accepted on ingest; float/double are accepted to catch overs.
READ_SUBTYPES = ("PCM_16", "PCM_24", "PCM_32", "FLOAT", "DOUBLE")
WRITE_SUBTYPES = ("PCM_16", "PCM_24", "PCM_32", "FLOAT")
DEFAULT_WRITE_SUBTYPE = "PCM_24"


@dataclass(frozen=True)
class AudioInfo:
    """Validated header facts for one input file."""

    path: Path
    samplerate: int
    channels: int
    frames: int
    format: str
    subtype: str
    layout: ChannelLayout

    @property
    def duration(self) -> float:
        return self.frames / self.samplerate if self.samplerate else 0.0

    def to_dict(self) -> dict:
        return {
            "path": str(self.path),
            "samplerate": self.samplerate,
            "channels": self.channels,
            "frames": self.frames,
            "format": self.format,
            "subtype": self.subtype,
            "channel_layout": self.layout.name,
            "duration_seconds": round(self.duration, 6),
        }


def probe(path: Path) -> AudioInfo:
    """Validate one input file and return its header facts."""

    path = Path(path)
    if not path.exists():
        raise UsageError(f"input file does not exist: {path}", code="input_missing")
    if not path.is_file():
        raise UsageError(f"input path is not a regular file: {path}", code="input_missing")
    try:
        info = sf.info(str(path))
    except Exception as exc:  # soundfile raises RuntimeError for bad headers
        raise InputFormatError(f"cannot read audio header: {exc}",
                               details={"path": str(path)}) from exc

    if info.format not in READ_FORMATS:
        raise UnsupportedError(
            f"unsupported container {info.format!r}; expected one of {READ_FORMATS}",
            details={"path": str(path), "format": info.format})
    if info.subtype not in READ_SUBTYPES:
        raise UnsupportedError(
            f"unsupported sample subtype {info.subtype!r}; expected one of {READ_SUBTYPES}",
            details={"path": str(path), "subtype": info.subtype})
    if info.samplerate <= 0:
        raise UnsupportedError(f"invalid sample rate {info.samplerate}",
                               details={"path": str(path)})
    if not (MIN_SAMPLE_RATE <= info.samplerate <= MAX_SAMPLE_RATE):
        raise UnsupportedError(
            f"input sample rate {info.samplerate} Hz is outside the supported "
            f"{MIN_SAMPLE_RATE}..{MAX_SAMPLE_RATE} Hz range",
            details={"path": str(path), "samplerate": info.samplerate})
    if info.frames <= 0:
        raise InputFormatError("file contains no audio frames",
                               details={"path": str(path)})
    if info.format == "WAV":
        check_wav_integrity(path)
    layout = resolve_layout(info.channels)
    return AudioInfo(path=path, samplerate=info.samplerate, channels=info.channels,
                     frames=info.frames, format=info.format,
                     subtype=info.subtype, layout=layout)


def deinterleave(interleaved: array.array, channels: int) -> list[array.array]:
    """Split an interleaved buffer into plane-major channels."""

    return [interleaved[c::channels] for c in range(channels)]


def interleave(planes: Sequence[Sequence[float]], frames: int) -> array.array:
    """Pack plane-major channels into one interleaved buffer."""

    channels = len(planes)
    out = array.array("d", bytes(8 * frames * channels))
    for index, plane in enumerate(planes):
        if len(plane) < frames:
            raise ValueError(f"plane {index} holds {len(plane)} frames, need {frames}")
        out[index::channels] = array.array("d", plane[:frames])
    return out


class BlockReader:
    """Iterate one validated file block by block, plane-major.

    The final block is zero-padded up to ``block_frames`` when the frame count
    does not divide evenly; ``zero_padded_frames`` records how many zero frames
    were appended.  ``AudioBlock.frames`` always reports the *valid* (unpadded)
    frame count, so downstream statistics never see the padding.
    """

    def __init__(self, info: AudioInfo, block_frames: int):
        if block_frames < 1:
            raise UsageError("block_frames must be >= 1", code="bad_block_size")
        self.info = info
        self.block_frames = block_frames
        self.zero_padded_frames = 0
        self._handle: sf.SoundFile | None = None
        self._scratch = array.array("d", bytes(8 * block_frames * info.channels))

    def __enter__(self) -> "BlockReader":
        self._handle = sf.SoundFile(str(self.info.path), mode="r")
        return self

    def __exit__(self, *exc_info) -> None:
        if self._handle is not None:
            self._handle.close()
            self._handle = None

    def __iter__(self) -> Iterator[AudioBlock]:
        if self._handle is None:
            raise RuntimeError("BlockReader must be used as a context manager")
        channels = self.info.channels
        expected = self.info.frames
        while True:
            want = min(self.block_frames, expected)
            if want <= 0:
                break
            got = self._handle.buffer_read_into(self._scratch, dtype="float64")
            if got <= 0:
                raise InputFormatError(
                    f"file ended early: expected {expected} frames, "
                    f"source stopped short", details={"path": str(self.info.path)})
            valid = min(got, want)
            scratch = self._scratch
            view = scratch[: valid * channels]
            # Reject NaN/Inf before any DSP; these are unprocessable values.
            # ``sum`` propagates NaN, so one finite check covers the block.
            if not math.isfinite(sum(view)):
                raise InputFormatError(
                    "sample stream contains NaN or infinite values",
                    details={"path": str(self.info.path)})
            padded = got < self.block_frames
            if padded:
                # Zero-fill the rest of the block so every block is uniform.
                self.zero_padded_frames += self.block_frames - got
                scratch[got * channels:] = array.array(
                    "d", bytes(8 * (self.block_frames - got) * channels))
            planes = deinterleave(scratch, channels)
            expected -= valid
            yield AudioBlock(planes=planes, frames=valid)
            if padded:
                break
        if expected != 0:
            raise InputFormatError(
                f"frame count mismatch: {expected} frames never read",
                details={"path": str(self.info.path)})


def count_clipped(samples: Sequence[float], threshold: float = 1.0) -> int:
    """Count samples with magnitude at or beyond full scale."""

    return sum(1 for s in samples if s >= threshold or s <= -threshold)


def write_wav_atomic(path: Path, blocks: Iterator[AudioBlock], samplerate: int,
                     channels: int, subtype: str = DEFAULT_WRITE_SUBTYPE) -> Path:
    """Write blocks to a temp file in the same directory, then rename atomically.

    A crash, short write or bad source block leaves only the temporary file
    (removed on the way out), never something at ``path`` that looks finished.
    """

    path = Path(path)
    if subtype not in WRITE_SUBTYPES:
        raise WriteError(f"unsupported output subtype {subtype!r}",
                         details={"supported": list(WRITE_SUBTYPES)})
    path.parent.mkdir(parents=True, exist_ok=True)
    handle = tmp_path = None
    written = 0
    try:
        fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".part",
                                        dir=str(path.parent))
        os.close(fd)
        tmp_path = Path(tmp_name)
        handle = sf.SoundFile(str(tmp_path), mode="w", samplerate=samplerate,
                              channels=channels, subtype=subtype, format="WAV")
        for block in blocks:
            if block.frames == 0:
                continue
            handle.buffer_write(interleave(block.planes, block.frames), "float64")
            written += block.frames
        handle.close()
        handle = None
        if written == 0:
            raise WriteError("refusing to commit a zero-frame output file",
                             details={"path": str(path)})
        os.replace(tmp_path, path)
        tmp_path = None
        return path
    except (WriteError, InputFormatError, UnsupportedError, UsageError):
        # Reader-side errors (bad samples, truncated stream) keep their own
        # classification; only genuine write problems become WriteError,
        # because the two drive different exit codes.
        raise
    except Exception as exc:
        raise WriteError(f"failed writing {path}: {exc}",
                         details={"path": str(path)}) from exc
    finally:
        _close_quietly(handle)
        if tmp_path is not None:
            _unlink_quietly(tmp_path)


def _close_quietly(handle) -> None:
    if handle is not None:
        try:
            handle.close()
        except Exception:
            pass


def _unlink_quietly(path: Path) -> None:
    try:
        Path(path).unlink(missing_ok=True)
    except OSError:
        pass


def ensure_inside(root: Path, candidate: Path, *, label: str = "path") -> Path:
    """Resolve ``candidate`` and require it to live under ``root``."""

    root_resolved = Path(root).resolve()
    target = Path(candidate).resolve()
    try:
        target.relative_to(root_resolved)
    except ValueError as exc:
        raise UsageError(
            f"{label} escapes the workspace {root_resolved}: {target}",
            code="path_outside_workspace",
            details={"workspace": str(root_resolved), "path": str(target)}) from exc
    return target


def guard_output_path(source: Path, destination: Path) -> None:
    """Refuse to write an output over its own source file."""

    if Path(destination).resolve() == Path(source).resolve():
        raise WriteError(
            f"refusing to overwrite the input file: {source}",
            details={"path": str(source)})
