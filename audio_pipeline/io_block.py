"""Bounded-memory soundfile I/O and interleaved channel views."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import tempfile
from typing import Iterator

import numpy as np
import soundfile as sf


ERROR_EMPTY = 10
ERROR_CORRUPT = 11
ERROR_OUTPUT = 13
ERROR_FORMAT = 14


class AudioFileError(RuntimeError):
    def __init__(self, message: str, code: int) -> None:
        super().__init__(message)
        self.code = code


@dataclass(frozen=True)
class AudioInfo:
    path: Path
    frames: int
    rate: int
    channels: int
    subtype: str
    format: str


def channel_views(interleaved: np.ndarray, channels: int) -> tuple[np.ndarray, ...]:
    """Return channel slices as views with an interleaved stride."""

    data = np.asarray(interleaved)
    if data.ndim != 2 or data.shape[1] != channels:
        raise ValueError("buffer is not an interleaved frames-by-channels array")
    return tuple(data[:, channel] for channel in range(channels))


def interleave(channel_views_: tuple[np.ndarray, ...]) -> np.ndarray:
    if not channel_views_:
        raise ValueError("at least one channel is required")
    return np.ascontiguousarray(np.column_stack(channel_views_))


def probe_output_directory(directory: Path) -> None:
    try:
        directory.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            dir=directory, prefix=".audio_pipeline_probe_", delete=True
        ) as stream:
            stream.write(b"probe")
    except OSError as exc:
        raise AudioFileError(
            f"output directory is not writable: {directory}", ERROR_OUTPUT
        ) from exc


class BlockReader:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        try:
            self._file = sf.SoundFile(str(self.path), mode="r")
            info = sf.info(str(self.path))
        except Exception as exc:  # libsndfile raises several exception types
            raise AudioFileError(
                f"cannot read {self.path.name}: {exc}", ERROR_CORRUPT
            ) from exc
        self.rate = int(info.samplerate)
        self.channels = int(info.channels)
        self.frames = int(info.frames)
        self.subtype = str(info.subtype)
        self.format = str(info.format)
        if self.frames <= 0:
            self._file.close()
            raise AudioFileError(f"empty audio file: {self.path.name}", ERROR_EMPTY)
        if self.channels not in (1, 2):
            self._file.close()
            raise AudioFileError(
                f"unsupported channel count {self.channels}: {self.path.name}",
                ERROR_FORMAT,
            )

    @property
    def info(self) -> AudioInfo:
        return AudioInfo(
            path=self.path,
            frames=self.frames,
            rate=self.rate,
            channels=self.channels,
            subtype=self.subtype,
            format=self.format,
        )

    def read_block(self, block_size: int) -> np.ndarray:
        if block_size <= 0:
            raise ValueError("block_size must be positive")
        try:
            block = self._file.read(frames=block_size, dtype="float64", always_2d=True)
        except Exception as exc:
            raise AudioFileError(
                f"cannot decode {self.path.name}: {exc}", ERROR_CORRUPT
            ) from exc
        return np.asarray(block, dtype=np.float64, order="C")

    def blocks(self, block_size: int) -> Iterator[np.ndarray]:
        while True:
            block = self.read_block(block_size)
            if len(block) == 0:
                return
            yield block

    def close(self) -> None:
        self._file.close()

    def __enter__(self) -> "BlockReader":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()


class BlockWriter:
    """Write a mono/stereo stream as WAV PCM_24 through libsndfile."""

    def __init__(
        self,
        path: str | Path,
        rate: int,
        channels: int = 2,
        subtype: str = "PCM_24",
    ) -> None:
        self.path = Path(path)
        if channels not in (1, 2):
            raise ValueError("channels must be 1 or 2")
        try:
            self._file = sf.SoundFile(
                str(self.path),
                mode="w",
                samplerate=int(rate),
                channels=int(channels),
                format="WAV",
                subtype=subtype,
            )
        except Exception as exc:
            raise AudioFileError(
                f"cannot open output {self.path}: {exc}", ERROR_OUTPUT
            ) from exc
        self.rate = int(rate)
        self.channels = int(channels)

    def write_block(self, block: np.ndarray) -> None:
        data = np.asarray(block, dtype=np.float64)
        if data.ndim != 2 or data.shape[1] != self.channels:
            raise ValueError("writer received a block with the wrong shape")
        try:
            self._file.write(data)
        except Exception as exc:
            raise AudioFileError(
                f"cannot write output {self.path}: {exc}", ERROR_OUTPUT
            ) from exc

    def close(self) -> None:
        self._file.close()

    def __enter__(self) -> "BlockWriter":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()


def read_frames(path: str | Path, start: int, frames: int) -> np.ndarray:
    """Random-access helper used by verify; caller must bound ``frames``."""

    with sf.SoundFile(str(path), mode="r") as stream:
        stream.seek(int(start))
        return np.asarray(
            stream.read(frames=int(frames), dtype="float64", always_2d=True),
            dtype=np.float64,
        )


def output_path_for(input_path: Path, output_dir: Path) -> Path:
    return output_dir / f"{input_path.stem}.wav"


def is_readable_audio(path: Path) -> bool:
    try:
        with sf.SoundFile(str(path), mode="r") as stream:
            return stream.frames > 0
    except Exception:
        return False
