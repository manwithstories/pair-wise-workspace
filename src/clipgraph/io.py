"""Sound-file and raw-float I/O with channel conversion and bit-depth handling."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np
import soundfile as sf

from .model import MixGraph, SourceSpec


class AudioIOError(RuntimeError):
    pass


SUBTYPES = {16: "PCM_16", 24: "PCM_24", 32: "PCM_32", "float": "FLOAT"}


@dataclass(frozen=True)
class AudioInfo:
    path: str
    sample_rate: int
    channels: int
    frames: int
    subtype: str

    def to_dict(self) -> dict[str, int | str]:
        return {
            "path": self.path,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "frames": self.frames,
            "subtype": self.subtype,
        }


def read_info(path: str | Path) -> AudioInfo:
    try:
        info = sf.info(str(path))
    except (OSError, RuntimeError) as exc:
        raise AudioIOError(f"cannot read audio {path}: {exc}") from exc
    if info.samplerate <= 0 or info.channels <= 0:
        raise AudioIOError(f"invalid sample rate/channel count in {path}")
    return AudioInfo(str(path), info.samplerate, info.channels, info.frames, info.subtype)


def convert_channels(data: np.ndarray, target: int) -> np.ndarray:
    """Up/down-mix arbitrary channel layouts into ``target`` channels."""
    if target < 1:
        raise AudioIOError(f"invalid target channel count {target}")
    array = np.asarray(data)
    if array.ndim == 1:
        array = array[:, None]
    source = array.shape[1]
    if source == target:
        return array
    if target == 1:
        return np.mean(array, axis=1, keepdims=True)
    if source == 1:
        return np.repeat(array, target, axis=1)
    output = np.zeros((array.shape[0], target), dtype=array.dtype)
    if target == 2:
        for channel in range(source):
            output[:, channel % 2] += array[:, channel]
        counts = [len(range(i, source, 2)) for i in range(2)]
        output[:, 0] /= max(1, counts[0])
        output[:, 1] /= max(1, counts[1])
        return output
    for channel in range(source):
        output[:, channel % target] += array[:, channel]
    for channel in range(target):
        count = len(range(channel, source, target))
        output[:, channel] /= max(1, count)
    return output


class WavSourceProvider:
    """Random-access source reader that keeps only one block of sample data."""

    def __init__(self, graph: MixGraph, base_dir: str | Path):
        self.graph = graph
        self.base_dir = Path(base_dir)
        self._handles: dict[str, sf.SoundFile] = {}
        self._infos: dict[str, AudioInfo] = {}
        self._sources = {
            node.source.path: node.source for node in graph.nodes if node.kind == "source" and node.source is not None
        }

    def _resolve(self, path: str) -> Path:
        candidate = Path(path)
        return candidate if candidate.is_absolute() else self.base_dir / candidate

    def open_all(self) -> dict[str, AudioInfo]:
        infos: dict[str, AudioInfo] = {}
        for path, spec in self._sources.items():
            resolved = self._resolve(path)
            info = read_info(resolved)
            self._validate_source(path, spec, info)
            infos[path] = info
            self._infos[path] = info
        return infos

    def _validate_source(self, path: str, spec: SourceSpec, info: AudioInfo) -> None:
        if info.sample_rate != spec.sample_rate:
            raise AudioIOError(
                f"source {path} is {info.sample_rate} Hz but graph declares {spec.sample_rate} Hz"
            )
        if info.channels != spec.channels:
            raise AudioIOError(
                f"source {path} has {info.channels} channels but graph declares {spec.channels}"
            )

    def __call__(self, path: str, start: int, count: int) -> np.ndarray:
        if count <= 0:
            return np.empty((0, self.graph.channels), dtype=np.float32)
        if path not in self._handles:
            resolved = self._resolve(path)
            try:
                self._handles[path] = sf.SoundFile(str(resolved), "r")
            except (OSError, RuntimeError) as exc:
                raise AudioIOError(f"cannot open audio {resolved}: {exc}") from exc
        handle = self._handles[path]
        if start >= handle.frames:
            return np.zeros((count, self.graph.channels), dtype=np.float32)
        try:
            handle.seek(max(0, start))
            data = handle.read(count, dtype="float32", always_2d=True)
        except (OSError, RuntimeError) as exc:
            raise AudioIOError(f"cannot read audio {path}: {exc}") from exc
        if len(data) < count:
            missing = count - len(data)
            data = np.concatenate((data, np.zeros((missing, data.shape[1]), dtype=data.dtype)), axis=0)
        return convert_channels(data, self.graph.channels)

    def close(self) -> None:
        for handle in self._handles.values():
            handle.close()
        self._handles.clear()

    def __enter__(self) -> "WavSourceProvider":
        self.open_all()
        return self

    def __exit__(self, *args) -> None:
        self.close()


def write_wav(path: str | Path, data: np.ndarray, sample_rate: int, bit_depth: int | str = 24) -> None:
    if bit_depth not in SUBTYPES:
        raise AudioIOError(f"unsupported bit depth {bit_depth!r}")
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    array = np.asarray(data, dtype=np.float32)
    if array.ndim == 1:
        array = array[:, None]
    if not np.all(np.isfinite(array)):
        array = np.nan_to_num(array, nan=0.0, posinf=0.0, neginf=0.0)
    try:
        sf.write(str(output), array, sample_rate, subtype=SUBTYPES[bit_depth])
    except (OSError, RuntimeError) as exc:
        raise AudioIOError(f"cannot write audio {output}: {exc}") from exc


class WavStreamWriter:
    def __init__(self, path: str | Path, sample_rate: int, channels: int, bit_depth: int | str = 24):
        if bit_depth not in SUBTYPES:
            raise AudioIOError(f"unsupported bit depth {bit_depth!r}")
        output = Path(path)
        output.parent.mkdir(parents=True, exist_ok=True)
        try:
            self.handle = sf.SoundFile(
                str(output), "w", samplerate=sample_rate, channels=channels, subtype=SUBTYPES[bit_depth]
            )
        except (OSError, RuntimeError) as exc:
            raise AudioIOError(f"cannot write audio {output}: {exc}") from exc

    def write(self, data: np.ndarray) -> None:
        array = np.asarray(data, dtype=np.float32)
        if not np.all(np.isfinite(array)):
            array = np.nan_to_num(array, nan=0.0, posinf=0.0, neginf=0.0)
        self.handle.write(array)

    def close(self) -> None:
        self.handle.close()

    def __enter__(self) -> "WavStreamWriter":
        return self

    def __exit__(self, *args) -> None:
        self.close()


def open_raw_writer(path: str | Path):
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    return open(output, "wb")


def write_raw(handle, data: np.ndarray) -> None:
    handle.write(np.asarray(data, dtype="<f4").tobytes(order="C"))


def raw_frames(path: str | Path, channels: int) -> int:
    size = Path(path).stat().st_size
    if size % (4 * channels):
        raise AudioIOError(f"raw file size is not aligned to {channels} float32 channels")
    return size // (4 * channels)
