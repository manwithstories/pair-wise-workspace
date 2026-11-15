"""Sound-file I/O, workspace path checks, and channel normalization."""

from __future__ import annotations

import hashlib
import warnings
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

import numpy as np
import soundfile as sf


class InputError(ValueError):
    """Raised for missing files, invalid rates, or unusable source audio."""


class WriteError(OSError):
    """Raised when an output file cannot be created or written."""


@dataclass(frozen=True)
class AudioInfo:
    path: Path
    sample_rate: int
    channels: int
    selected_channels: int
    frames: int
    subtype: str | None = None


def validate_sample_rate(sample_rate: int) -> int:
    if not isinstance(sample_rate, (int, np.integer)):
        raise InputError("sample rate must be an integer")
    value = int(sample_rate)
    if value < 1_000 or value > 384_000:
        raise InputError(f"sample rate out of supported range: {value}")
    return value


def resolve_workspace_path(path: str | Path, workspace: str | Path) -> Path:
    """Resolve a path and reject anything that escapes the workspace."""

    root = Path(workspace).resolve()
    candidate = Path(path)
    if not candidate.is_absolute():
        candidate = root / candidate
    resolved = candidate.resolve(strict=False)
    try:
        resolved.relative_to(root)
    except ValueError as exc:
        raise InputError(f"path is outside the workspace: {path}") from exc
    return resolved


def resolve_input_path(
    path: str | Path, workspace: str | Path, base_dir: str | Path | None = None
) -> Path:
    """Resolve workspace-first, optionally falling back to the graph directory."""

    primary = resolve_workspace_path(path, workspace)
    if primary.exists() or base_dir is None:
        return primary
    candidate_path = Path(path)
    if not candidate_path.is_absolute():
        candidate_path = Path(base_dir) / candidate_path
    candidate = resolve_workspace_path(candidate_path, workspace)
    return candidate if candidate.exists() else primary


def _normalize_channels(data: np.ndarray, *, warn: bool = True) -> np.ndarray:
    array = np.asarray(data, dtype=np.float64)
    if array.ndim == 1:
        array = array[:, None]
    if array.ndim != 2 or array.shape[1] < 1:
        raise InputError("audio data must have one or two channels")
    if array.shape[1] > 2:
        if not warn:
            return array[:, :2]
        warnings.warn(
            f"source has {array.shape[1]} channels; using the first two",
            RuntimeWarning,
            stacklevel=2,
        )
        array = array[:, :2]
    return array


def inspect_audio(
    path: str | Path, expected_channels: int | None = None
) -> AudioInfo:
    path = Path(path)
    if not path.exists():
        raise InputError(f"audio file not found: {path}")
    try:
        info = sf.info(str(path))
    except (OSError, RuntimeError) as exc:
        raise InputError(f"cannot read audio file {path}: {exc}") from exc
    validate_sample_rate(info.samplerate)
    if info.channels < 1:
        raise InputError(f"audio file has no channels: {path}")
    selected = min(int(info.channels), 2)
    if expected_channels is not None:
        if expected_channels not in (1, 2):
            raise InputError("expected channels must be 1 or 2")
        if selected != expected_channels:
            raise InputError(
                f"channel mismatch for {path}: expected {expected_channels}, "
                f"found {selected}"
            )
    return AudioInfo(
        path=path,
        sample_rate=int(info.samplerate),
        channels=int(info.channels),
        selected_channels=selected,
        frames=int(info.frames),
        subtype=info.subtype,
    )


class SourceReader:
    """Random-access, block-sized source reader with channel truncation."""

    def __init__(self, path: str | Path, expected_channels: int | None = None):
        self.info = inspect_audio(path, expected_channels=expected_channels)
        if self.info.channels > 2:
            warnings.warn(
                f"source {self.info.path} has {self.info.channels} channels; "
                "using the first two",
                RuntimeWarning,
                stacklevel=2,
            )
        try:
            self._handle = sf.SoundFile(str(self.info.path), mode="r")
        except (OSError, RuntimeError) as exc:
            raise InputError(f"cannot open audio file {path}: {exc}") from exc

    @property
    def channels(self) -> int:
        return self.info.selected_channels

    @property
    def frames(self) -> int:
        return self.info.frames

    @property
    def sample_rate(self) -> int:
        return self.info.sample_rate

    def read(self, start: int, count: int) -> np.ndarray:
        start = max(0, int(start))
        count = max(0, int(count))
        if count == 0 or start >= self.frames:
            return np.empty((0, self.channels), dtype=np.float64)
        end = min(self.frames, start + count)
        try:
            self._handle.seek(start)
            data = self._handle.read(
                end - start, dtype="float64", always_2d=True
            )
        except (OSError, RuntimeError) as exc:
            raise InputError(f"failed reading {self.info.path}: {exc}") from exc
        return _normalize_channels(data, warn=False)[:, : self.channels]

    def close(self) -> None:
        handle = getattr(self, "_handle", None)
        if handle is not None:
            handle.close()
            self._handle = None

    def __enter__(self) -> "SourceReader":
        return self

    def __exit__(self, *args: object) -> None:
        self.close()


class AudioWriter:
    """Write float blocks to a WAV file without retaining the whole mix."""

    def __init__(
        self,
        path: str | Path,
        sample_rate: int,
        channels: int,
        subtype: str = "PCM_24",
    ) -> None:
        self.path = Path(path)
        self.sample_rate = validate_sample_rate(sample_rate)
        if channels not in (1, 2):
            raise WriteError("output must have one or two channels")
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self._handle = sf.SoundFile(
                str(self.path),
                mode="w",
                samplerate=self.sample_rate,
                channels=channels,
                subtype=subtype,
                format="WAV",
            )
        except (OSError, RuntimeError, ValueError) as exc:
            raise WriteError(f"cannot create output {self.path}: {exc}") from exc
        self.channels = channels

    def write(self, data: np.ndarray) -> None:
        array = np.asarray(data, dtype=np.float64)
        if array.ndim == 1:
            array = array[:, None]
        if array.ndim != 2 or array.shape[1] != self.channels:
            raise WriteError(
                f"output block has {array.shape[1:]} channels; expected {self.channels}"
            )
        if not np.all(np.isfinite(array)):
            array = np.nan_to_num(array, nan=0.0, posinf=0.0, neginf=0.0)
        try:
            self._handle.write(array)
        except (OSError, RuntimeError, ValueError) as exc:
            raise WriteError(f"failed writing {self.path}: {exc}") from exc

    def close(self) -> None:
        handle = getattr(self, "_handle", None)
        if handle is not None:
            try:
                handle.close()
            except (OSError, RuntimeError) as exc:
                raise WriteError(f"failed closing {self.path}: {exc}") from exc
            self._handle = None

    def __enter__(self) -> "AudioWriter":
        return self

    def __exit__(self, exc_type: object, *args: object) -> None:
        self.close()


def read_audio(path: str | Path) -> tuple[np.ndarray, int]:
    with SourceReader(path) as reader:
        data = reader.read(0, reader.frames)
        return data, reader.sample_rate


def write_audio(
    path: str | Path, data: np.ndarray, sample_rate: int, subtype: str = "PCM_24"
) -> None:
    array = _normalize_channels(data)
    with AudioWriter(path, sample_rate, array.shape[1], subtype=subtype) as writer:
        writer.write(array)


def hash_file(path: str | Path, chunk_size: int = 1 << 20) -> str:
    digest = hashlib.sha256()
    try:
        with Path(path).open("rb") as handle:
            while True:
                chunk = handle.read(chunk_size)
                if not chunk:
                    break
                digest.update(chunk)
    except OSError as exc:
        raise InputError(f"cannot hash {path}: {exc}") from exc
    return digest.hexdigest()


def iter_audio_blocks(
    path: str | Path, block_size: int, start: int = 0
) -> Iterator[np.ndarray]:
    if block_size <= 0:
        raise ValueError("block size must be positive")
    with SourceReader(path) as reader:
        position = max(0, int(start))
        while position < reader.frames:
            block = reader.read(position, block_size)
            if block.shape[0] == 0:
                break
            yield block
            position += block.shape[0]


load_audio = read_audio
save_audio = write_audio
AudioError = InputError

