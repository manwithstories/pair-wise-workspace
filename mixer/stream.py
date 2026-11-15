"""Streaming render path with absolute-timeline seek support."""

from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path

from mixer.engine import MixSession
from mixer.graph import Graph
from mixer.io import AudioWriter, validate_sample_rate


@dataclass
class StreamResult:
    output_path: Path
    frames_written: int
    channels: int
    seek_frame: int
    session: MixSession


def seek_to_frame(seek_sec: float, master_sr: int) -> int:
    if not math.isfinite(seek_sec) or seek_sec < 0:
        raise ValueError("seek must be a finite non-negative number")
    return int(math.floor(float(seek_sec) * int(master_sr) + 0.5))


class StreamRenderer:
    """Chunked renderer using the same runtime nodes as the offline path."""

    def __init__(
        self,
        graph: Graph,
        master_sr: int,
        block_size: int = 257,
        *,
        workspace: str | Path = ".",
        seek_sec: float = 0.0,
    ) -> None:
        if block_size <= 0:
            raise ValueError("block size must be positive")
        self.master_sr = validate_sample_rate(master_sr)
        self.block_size = int(block_size)
        self.seek_frame = seek_to_frame(seek_sec, self.master_sr)
        self.session = MixSession(
            graph, self.master_sr, workspace, seek_frame=self.seek_frame
        )
        self.channels = self.session.output.channels

    def iter_blocks(self):
        yield from self.session.iter_blocks(self.block_size)

    def render(self, output_path: str | Path) -> StreamResult:
        writer = AudioWriter(output_path, self.master_sr, self.channels)
        frames = 0
        try:
            for _, block in self.iter_blocks():
                writer.write(block)
                frames += int(block.shape[0])
        finally:
            try:
                writer.close()
            finally:
                self.session.close()
        return StreamResult(
            output_path=Path(output_path),
            frames_written=frames,
            channels=self.channels,
            seek_frame=self.seek_frame,
            session=self.session,
        )


def stream_to_file(
    graph: Graph,
    output_path: str | Path,
    master_sr: int,
    block_size: int = 257,
    *,
    workspace: str | Path = ".",
    seek_sec: float = 0.0,
) -> StreamResult:
    return StreamRenderer(
        graph,
        master_sr,
        block_size,
        workspace=workspace,
        seek_sec=seek_sec,
    ).render(output_path)


render_stream = stream_to_file
