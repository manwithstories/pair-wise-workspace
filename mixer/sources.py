"""轨道媒体源:文件惰性读取、内存源与有界后台预取。

流式路径通过 StreamSource 按块取数,任何时刻只缓存有限帧,
不会把全部轨道一次性载入内存。PrefetchSource 用 daemon 线程
配合有界 Queue 预取,队列满时生产者自然阻塞,内存上界确定。
"""

from __future__ import annotations

import queue
import threading
from pathlib import Path
from typing import Optional, Protocol

import numpy as np
import soundfile as sf

from .types import MediaError

_READ_CHUNK = 4096
_QUEUE_DEPTH = 4
_SENTINEL = object()


class StreamSource(Protocol):
    """轨道源的最小接口:按块读、可关闭、暴露媒体元数据。"""

    sample_rate: int
    channels: int
    frames: int

    def read(self, frames: int) -> np.ndarray: ...

    def close(self) -> None: ...


class FileSource:
    """soundfile 支持的 WAV 源,惰性 seek/read,不整段载入。"""

    def __init__(self, path: str | Path) -> None:
        p = Path(path)
        if not p.exists():
            raise MediaError(
                f"轨道文件不存在: {p}",
                details={"path": str(p), "reason": "not_found"},
            )
        if p.is_dir() or p.stat().st_size == 0:
            raise MediaError(
                f"轨道文件为空: {p}",
                details={"path": str(p), "reason": "empty_file"},
            )
        try:
            self._info = sf.info(str(p))
        except Exception as exc:  # Libsndfile 异常类型不稳定,统一包装
            raise MediaError(
                f"无法解析 WAV(可能已损坏): {p}: {exc}",
                details={"path": str(p), "reason": "corrupt_wav"},
            ) from exc
        if self._info.samplerate <= 0:
            raise MediaError(
                f"采样率非正: {p}: {self._info.samplerate}",
                details={"path": str(p), "reason": "invalid_sample_rate"},
            )
        self._sf = sf.SoundFile(str(p), mode="r")
        self.sample_rate = int(self._info.samplerate)
        self.channels = int(self._info.channels)
        self.frames = int(self._info.frames)
        self._path = p

    def read(self, frames: int) -> np.ndarray:
        try:
            data = self._sf.read(frames=frames, dtype="float64", always_2d=True)
        except Exception as exc:
            raise MediaError(
                f"读取 WAV 失败: {self._path}: {exc}",
                details={"path": str(self._path), "reason": "read_failed"},
            ) from exc
        return np.asarray(data, dtype=np.float64)

    def close(self) -> None:
        try:
            self._sf.close()
        except Exception:
            pass


class MemorySource:
    """整段 ndarray 源(测试与离线对照使用)。"""

    def __init__(self, data: np.ndarray, sample_rate: int) -> None:
        arr = np.asarray(data, dtype=np.float64)
        if arr.ndim == 1:
            arr = arr[:, None]
        self._data = arr
        self._pos = 0
        self.sample_rate = int(sample_rate)
        self.channels = arr.shape[1]
        self.frames = arr.shape[0]

    def read(self, frames: int) -> np.ndarray:
        part = self._data[self._pos : self._pos + frames]
        self._pos += part.shape[0]
        return part.copy()

    def close(self) -> None:
        return None


class PrefetchSource:
    """有界后台预取包装器,保持 StreamSource 接口与读取序列不变。"""

    def __init__(self, source: StreamSource, chunk: int = _READ_CHUNK) -> None:
        self._source = source
        self.sample_rate = source.sample_rate
        self.channels = source.channels
        self.frames = source.frames
        self._queue: queue.Queue = queue.Queue(maxsize=_QUEUE_DEPTH)
        self._error: Optional[MediaError] = None
        self._chunk = chunk
        self._thread = threading.Thread(target=self._worker, daemon=True)
        self._thread.start()

    def _worker(self) -> None:
        try:
            while True:
                part = self._source.read(self._chunk)
                if part.shape[0] == 0:
                    break
                self._queue.put(part)
        except MediaError as exc:
            self._error = exc
        finally:
            self._queue.put(_SENTINEL)

    def read(self, frames: int) -> np.ndarray:
        if self._error is not None:
            raise self._error
        item = self._queue.get()
        if item is _SENTINEL:
            if self._error is not None:
                raise self._error
            return np.zeros((0, self.channels), dtype=np.float64)
        return item

    def close(self) -> None:
        try:
            if self._thread.is_alive():
                while self._thread.is_alive():
                    try:
                        item = self._queue.get(timeout=0.1)
                        if item is _SENTINEL:
                            break
                    except queue.Empty:
                        continue
            self._thread.join(timeout=1.0)
        finally:
            self._source.close()
