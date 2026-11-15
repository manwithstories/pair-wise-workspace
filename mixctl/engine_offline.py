"""离线渲染引擎：整轨读入、一次重采样、按块求和后写出。"""

from __future__ import annotations

import numpy as np

from .engine_common import RenderSession, adapt_channels
from .errors import AudioIOError


def _read_track(runtime) -> np.ndarray:
    handle = runtime.open()
    try:
        data = handle.read(dtype="float64", always_2d=True)
    except Exception as exc:
        raise AudioIOError(f"WAV 读取失败: {runtime.path} ({exc})") from exc
    finally:
        handle.close()
    return adapt_channels(np.asarray(data, dtype=np.float64), runtime.track.channels)


def render_offline(session: RenderSession) -> np.ndarray:
    total = session.total_samples
    target_blocks: dict[str, np.ndarray] = {}
    for name, runtime in session.track_runtimes.items():
        source = _read_track(runtime)
        runtime.resampler.push(source)
        runtime.resampler.mark_eof()
        resampled = runtime.resampler.flush()
        if resampled.shape[0] > total:
            resampled = resampled[:total]
        target_blocks[name] = resampled

    master = np.zeros((total, session.channels), dtype=np.float64)
    step = 8192
    for start in range(0, total, step):
        frames = min(step, total - start)
        pieces = {
            name: block[start:start + frames] for name, block in target_blocks.items()
        }
        master[start:start + frames] = session.mix_block(pieces, start, frames)
    return master
