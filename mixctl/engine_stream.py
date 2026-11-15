"""流式渲染引擎：固定输出块长切分，逐块读源、重采样、求和、写出。

每块只保留重采样器内部的有界历史，内存占用不随工程时长增长；
块边界不重置自动化相位（Automation 只依赖全局采样下标）与
重采样滤波器状态（Resampler 只随输入推进）。
"""

from __future__ import annotations

import numpy as np
import soundfile as sf

from .engine_common import RenderSession, adapt_channels
from .errors import AudioIOError

INPUT_BLOCK = 65536


class _StreamTrack:
    def __init__(self, runtime):
        self.runtime = runtime
        self.handle = runtime.open()
        self.eof = False

    def _refill(self, needed_absolute: int) -> None:
        resampler = self.runtime.resampler
        while not self.eof and resampler._pushed_total < needed_absolute:
            try:
                chunk = self.handle.read(
                    INPUT_BLOCK, dtype="float64", always_2d=True
                )
            except Exception as exc:
                raise AudioIOError(
                    f"WAV 读取失败: {self.runtime.path} ({exc})"
                ) from exc
            if len(chunk) == 0:
                self.eof = True
                resampler.mark_eof()
                break
            resampler.push(
                adapt_channels(
                    np.asarray(chunk, dtype=np.float64), self.runtime.track.channels
                )
            )

    def read_target(self, frames: int) -> np.ndarray:
        resampler = self.runtime.resampler
        if resampler.identity:
            self._refill(resampler._produced + frames)
        else:
            self._refill(resampler.needed_input_for(frames))
        return resampler.read(frames)

    def close(self) -> None:
        self.handle.close()


def render_stream(session: RenderSession, chunk: int, output_path: str,
                  sample_rate: int) -> int:
    if chunk <= 0:
        raise ValueError(f"chunk 必须为正整数，得到 {chunk}")
    streams = {
        name: _StreamTrack(runtime)
        for name, runtime in session.track_runtimes.items()
    }
    total = session.total_samples
    written = 0
    try:
        with sf.SoundFile(
            output_path, mode="w", samplerate=sample_rate,
            channels=session.channels, subtype="PCM_24",
        ) as sink:
            start = 0
            while start < total:
                frames = min(chunk, total - start)
                pieces = {
                    name: stream.read_target(frames)
                    for name, stream in streams.items()
                }
                master = session.mix_block(pieces, start, frames)
                sink.write(np.clip(master, -1.0, 1.0))
                written += frames
                start += frames
    finally:
        for stream in streams.values():
            stream.close()
    return written
