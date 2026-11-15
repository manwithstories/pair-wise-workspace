"""Fixed-block streaming pull interface and WAV writer."""

from __future__ import annotations

from pathlib import Path

import numpy as np
import soundfile as sf

from .offline import _check_block_size, stabilize_wav_header
from .processor import MixProcessor
from .project import Project


class StreamRenderer:
    """Pull successive fixed-size blocks from a project.

    Only the current output block, resampler look-ahead cache, and accumulator
    meters are retained. The final pull may be shorter than ``block_size``.
    """

    def __init__(self, project: Project, block_size: int = 97) -> None:
        self.project = project
        self.block_size = _check_block_size(block_size)
        self.processor = MixProcessor(project)
        self.position = 0
        self._closed = False

    @property
    def frames_remaining(self) -> int:
        return max(0, self.project.duration_frames - self.position)

    def pull(self) -> np.ndarray:
        if self._closed:
            raise RuntimeError("stream renderer is closed")
        if self.position >= self.project.duration_frames:
            return np.empty((0, self.project.channels), dtype=np.float64)
        count = min(self.block_size, self.frames_remaining)
        block = self.processor.read_block(self.position, count)
        self.position += count
        return block

    def close(self) -> None:
        if not self._closed:
            self.processor.close()
            self._closed = True

    def __enter__(self) -> "StreamRenderer":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.close()


def render_stream(
    project: Project, out_path: str | Path, block_size: int | None = None
) -> dict:
    block_size = _check_block_size(block_size or 97)
    path = Path(out_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with StreamRenderer(project, block_size) as renderer:
        with sf.SoundFile(
            str(path),
            "w",
            samplerate=project.sample_rate,
            channels=project.channels,
            subtype="FLOAT",
        ) as handle:
            while renderer.frames_remaining:
                block = renderer.pull()
                if block.shape[0] == 0:
                    break
                handle.write(block)
        summary = renderer.processor.meters[project.output_node].report()
    stabilize_wav_header(path)
    return {
        "output": str(path),
        "sample_rate": project.sample_rate,
        "channels": project.channels,
        "frames": project.duration_frames,
        "block_size": block_size,
        **summary,
    }

