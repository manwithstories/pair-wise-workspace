"""Offline and streaming renderers over one shared graph, gain and limiter.

Both engines instantiate the same ``MixSession``: the offline engine asks for
the whole timeline in one block, the streaming engine asks for 256/1024-frame
blocks (optionally short, empty and partial tail chunks). Because the
resampler maps absolute destination windows, the gain evaluator samples every
frame, and the limiter envelope is block-independent, the two paths agree
sample for sample.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Sequence

import numpy as np

from .automation import GainEvaluator
from .errors import MixerError, argument_error
from .graph import KIND_TRACK, MixGraph, build_default_graph
from .io import SourceFile, fold_to_stereo, write_master
from .limiter import Limiter, Meter, track_stats
from .model import Project, Track, TrackStats
from .resample import LinearResampler

STREAM_BLOCKS = (256, 1024)
MIN_BLOCK = 64
MAX_BLOCK = 8192


@dataclass
class TrackRuntime:
    """Per-track state; reset between renders so runs stay independent."""

    track: Track
    source: SourceFile
    evaluator: GainEvaluator
    matrix: np.ndarray
    offset_frames: int
    meter: Meter
    ratio: float


@dataclass
class RenderResult:
    audio: np.ndarray
    rates: int
    stats: list[TrackStats]
    limiter: dict
    track_order: list[str]
    timeline_frames: int


def validate_block(size: int, streaming: bool = False) -> int:
    size = int(size)
    if streaming:
        if size not in STREAM_BLOCKS:
            raise argument_error(
                "invalid_block_size",
                f"streaming block must be one of {STREAM_BLOCKS}, got {size}",
                details={"block_size": size},
            )
    elif not MIN_BLOCK <= size <= MAX_BLOCK:
        raise argument_error(
            "invalid_block_size",
            f"block size must be between {MIN_BLOCK} and {MAX_BLOCK}, got {size}",
            details={"block_size": size},
        )
    return size


class MixSession:
    """One validated rendering context, reusable for offline then streaming."""

    def __init__(
        self,
        project: Project,
        graph: MixGraph | None = None,
        rate: int | None = None,
        block_size: int | None = None,
    ) -> None:
        self.project = project
        self.rate = int(rate or project.target_rate)
        if self.rate != 48000:
            raise argument_error(
                "invalid_target_rate", f"master rate must be 48000, got {self.rate}"
            )
        self.graph = graph or build_default_graph(project)
        self.summary = self.graph.validate()
        self.track_order = [n.name for n in self.graph.nodes() if n.kind == KIND_TRACK]
        self.block_size = int(block_size or project.block_size)
        self._runtimes: dict[str, TrackRuntime] = {}
        self._frames_cache: int | None = None
        self._paths: dict[str, float] = {}
        self._open_sources()

    # -- setup ------------------------------------------------------------
    def _open_sources(self) -> None:
        ordered = {t.name: t for t in self.project.tracks}
        missing = [name for name in self.track_order if name not in ordered]
        if missing:
            raise MixerError(
                "graph_unknown_track",
                "graph references tracks absent from the project: " + ", ".join(missing),
            )
        try:
            for name in self.track_order:
                track = ordered[name]
                source = SourceFile(track.file, track.name, track.layout)
                self._runtimes[name] = TrackRuntime(
                    track=track,
                    source=source,
                    evaluator=GainEvaluator(track, self.rate, self.block_size),
                    matrix=np.asarray(track.channel_matrix, dtype=np.float64),
                    offset_frames=int(round(track.start_time * self.rate)),
                    meter=Meter(),
                    ratio=source.rate / float(self.rate),
                )
                self._paths[name] = self._static_gain(name)
        except Exception:
            self.close()
            raise

    def _static_gain(self, name: str) -> float:
        """Product of every node gain on the track's path to the master."""
        path = self.graph.gain_path(name)
        gain = 1.0
        for _node, node_gain in path[1:]:  # path[0] is the track node itself
            gain *= node_gain
        return float(gain)

    # -- timeline ---------------------------------------------------------
    def track_frames(self, name: str) -> int:
        runtime = self._runtimes[name]
        track = runtime.track
        if not track.clips:
            return int(round(runtime.source.frames / runtime.ratio))
        total = 0
        for clip in track.sources:
            length = clip.length_frames
            if length is None:
                length = max(0, runtime.source.frames - clip.source_offset_frames)
            total = max(
                total,
                int(round(clip.start_time * self.rate)) + int(round(length / runtime.ratio)),
            )
        return total

    def timeline_frames(self) -> int:
        if self._frames_cache is None:
            longest = 0
            for name in self.track_order:
                runtime = self._runtimes[name]
                longest = max(longest, runtime.offset_frames + self.track_frames(name))
            if longest <= 0:
                raise MixerError("empty_timeline", "every track is empty; nothing to render")
            self._frames_cache = longest
        return self._frames_cache

    # -- per-track mixing -------------------------------------------------
    def _track_block(self, runtime: TrackRuntime, start: int, frames: int) -> np.ndarray:
        """One track's pre-gain stereo contribution for [start, start+frames)."""
        block: np.ndarray
        track = runtime.track
        if track.clips:
            block = self._mix_clips(runtime, start, frames)
        else:
            block = self._mix_track_source(runtime, start, frames)
        if track.silences:
            block = self._mask_silences(runtime, block, start, frames)
        gain = runtime.evaluator.linear_gains(frames)
        runtime.meter.add(block, gain)
        return block * gain[:, None]

    def _mix_track_source(self, runtime: TrackRuntime, start: int, frames: int) -> np.ndarray:
        """Track-start offset applied before reading the source window."""
        local = start - runtime.offset_frames
        if local >= 0:
            return self._resample_window(runtime, local, frames, source_origin=0)
        if local + frames <= 0:
            return np.zeros((frames, 2))
        # Block straddles the track's start: leading frames are silence.
        usable = local + frames
        converted = self._resample_window(runtime, 0, usable, source_origin=0)
        return np.concatenate((np.zeros((frames - usable, 2)), converted), axis=0)

    def _resample_window(
        self, runtime: TrackRuntime, dest_start: int, count: int, source_origin: int
    ) -> np.ndarray:
        """Resample ``count`` destination frames from track-relative ``dest_start``.

        Only the source window covering those frames is read; ``source_origin``
        lets a clip start partway into the file while keeping absolute frame
        arithmetic identical to a whole-file conversion.
        """
        if count <= 0:
            return np.zeros((0, 2), dtype=np.float64)
        ratio = runtime.source.rate / float(self.rate)
        first = int(math.floor(dest_start * ratio)) + source_origin
        last = int(math.ceil((dest_start + count) * ratio)) + 2 + source_origin
        raw = runtime.source.read(first, last - first)
        resampler = LinearResampler(raw, runtime.source.rate, self.rate, source_origin)
        converted = resampler.convert(count, dest_start, window_start=first)
        return fold_to_stereo(converted, runtime.matrix)

    def _mix_clips(self, runtime: TrackRuntime, start: int, frames: int) -> np.ndarray:
        """Sum each clip's contribution, honouring its own source offset/length."""
        block = np.zeros((frames, 2), dtype=np.float64)
        for clip in runtime.track.sources:
            clip_start = int(round(clip.start_time * self.rate))
            length = clip.length_frames
            if length is None:
                length = max(0, runtime.source.frames - clip.source_offset_frames)
            clip_frames = int(round(length / runtime.ratio))
            lo = max(start, clip_start)
            hi = min(start + frames, clip_start + clip_frames)
            if hi <= lo:
                continue
            converted = self._resample_window(
                runtime, lo - clip_start, hi - lo, source_origin=clip.source_offset_frames
            )
            block[lo - start: hi - start] += converted
        return block

    def _mask_silences(
        self, runtime: TrackRuntime, block: np.ndarray, start: int, frames: int
    ) -> np.ndarray:
        times = (start + np.arange(frames)) / self.rate
        mask = np.ones(frames, dtype=bool)
        for window in runtime.track.silences:
            mask &= ~((times >= window.start) & (times < window.end))
        if mask.all():
            return block
        return block * mask[:, None]

    # -- master -----------------------------------------------------------
    def mix_block(self, start: int, frames: int) -> np.ndarray:
        """Sum every track (with its gain path) into the stereo master."""
        master = np.zeros((frames, 2), dtype=np.float64)
        for name in self.track_order:
            contribution = self._track_block(self._runtimes[name], start, frames)
            master += contribution * self._paths[name]
        return master

    def render(
        self,
        block_size: int | None = None,
        sink: Callable[[np.ndarray], None] | None = None,
    ) -> RenderResult:
        """Render the whole timeline, optionally emitting blocks to ``sink``."""
        total = self.timeline_frames()
        size = int(block_size or total)
        limiter = Limiter(self.rate)
        pieces: list[np.ndarray] = []
        position = 0
        while position < total:
            count = min(size, total - position)
            limited = limiter.process_block(self.mix_block(position, count))
            if sink is not None:
                sink(limited)
            else:
                pieces.append(limited)
            position += count
        audio = np.concatenate(pieces) if pieces else np.zeros((0, 2))
        return RenderResult(
            audio=audio,
            rates=self.rate,
            stats=self.track_statistics(),
            limiter=limiter.stats(),
            track_order=list(self.track_order),
            timeline_frames=total,
        )

    def track_statistics(self) -> list[TrackStats]:
        return [track_stats(name, self._runtimes[name].meter) for name in self.track_order]

    def check_continuity(self, block_size: int) -> dict:
        """Largest dB jump at block boundaries across every track's automation."""
        total = self.timeline_frames()
        boundaries = list(range(block_size, total, block_size))
        jumps: dict[str, float] = {}
        for name in self.track_order:
            curve = self._runtimes[name].evaluator.curve
            measured = curve.check_continuity(boundaries, self.rate)
            jumps[name] = max(measured) if measured else 0.0
        return jumps

    def close(self) -> None:
        for runtime in self._runtimes.values():
            runtime.source.close()

    def reset(self) -> None:
        """Rewind stateful pieces so a second render starts identically."""
        for runtime in self._runtimes.values():
            runtime.evaluator.reset()
            runtime.meter = Meter()

    def __enter__(self) -> "MixSession":
        return self

    def __exit__(self, *exc) -> None:
        self.close()


class OfflineEngine:
    """Renders the whole timeline in a single pass."""

    def __init__(self, project: Project, graph: MixGraph | None = None) -> None:
        self.project = project
        self.graph = graph

    def render(self) -> RenderResult:
        with MixSession(self.project, self.graph) as session:
            return session.render(block_size=session.timeline_frames())


@dataclass
class StreamChunk:
    index: int
    start: int
    frames: int


class StreamingEngine:
    """Renders in fixed blocks, tolerating short reads, empty and tail chunks.

    ``chunk_sizes`` lets callers inject short/empty reads; the default plan is
    a constant block size plus a final short block for the remainder.
    """

    def __init__(
        self,
        project: Project,
        graph: MixGraph | None = None,
        block_size: int = 1024,
        chunk_sizes: Sequence[int] | None = None,
    ) -> None:
        self.project = project
        self.graph = graph
        self.block_size = validate_block(block_size, streaming=chunk_sizes is None)
        if self.block_size < MIN_BLOCK or self.block_size > MAX_BLOCK:
            raise argument_error("invalid_block_size", f"unsupported block size {self.block_size}")
        self.chunk_sizes = list(chunk_sizes) if chunk_sizes is not None else None

    def plan(self, total: int) -> list[StreamChunk]:
        if self.chunk_sizes is None:
            sizes = [self.block_size] * (total // self.block_size)
            if total % self.block_size:
                sizes.append(total % self.block_size)
            if not sizes and total > 0:
                sizes = [total]
        else:
            if not self.chunk_sizes:
                raise argument_error("invalid_block_plan", "chunk plan is empty")
            if any(s < 0 for s in self.chunk_sizes):
                raise argument_error("invalid_block_plan", "chunk sizes must be >= 0")
            sizes = list(self.chunk_sizes)
        chunks: list[StreamChunk] = []
        position = 0
        for index, size in enumerate(sizes):
            chunks.append(StreamChunk(index, position, size))
            position += size
        return chunks

    def render(self) -> RenderResult:
        with MixSession(self.project, self.graph, block_size=self.block_size) as session:
            total = session.timeline_frames()
            chunks = self.plan(total)
            covered = sum(c.frames for c in chunks)
            if covered < total:
                raise MixerError(
                    "short_stream", f"chunk plan covers {covered} of {total} frames"
                )
            limiter = Limiter(session.rate)
            pieces: list[np.ndarray] = []
            for chunk in chunks:
                if chunk.frames == 0:
                    continue  # empty read: no state change, nothing emitted
                pieces.append(limiter.process_block(session.mix_block(chunk.start, chunk.frames)))
            audio = np.concatenate(pieces) if pieces else np.zeros((0, 2))
            if audio.shape[0] > total:
                audio = audio[:total]
            return RenderResult(
                audio=audio,
                rates=session.rate,
                stats=session.track_statistics(),
                limiter=limiter.stats(),
                track_order=list(session.track_order),
                timeline_frames=total,
            )

    def render_stream(self, callback: Callable[[StreamChunk, np.ndarray], None]) -> dict:
        """Push each block to ``callback``; return block/metering summary."""
        with MixSession(self.project, self.graph, block_size=self.block_size) as session:
            total = session.timeline_frames()
            chunks = self.plan(total)
            limiter = Limiter(session.rate)
            emitted = 0
            for chunk in chunks:
                if chunk.frames == 0:
                    callback(chunk, np.zeros((0, 2)))
                    continue
                callback(chunk, limiter.process_block(session.mix_block(chunk.start, chunk.frames)))
                emitted += chunk.frames
            return {
                "frames": emitted,
                "timeline_frames": total,
                "blocks": len(chunks),
                "nonempty_blocks": sum(1 for c in chunks if c.frames),
                "limiter": limiter.stats(),
            }


def render_to_file(
    project: Project,
    output: str | Path,
    mode: str = "offline",
    block_size: int = 1024,
    graph: MixGraph | None = None,
) -> dict:
    """Render and write a master WAV, returning a JSON-able report."""
    if mode == "offline":
        result = OfflineEngine(project, graph).render()
        effective = result.timeline_frames
    elif mode == "stream":
        validate_block(block_size, streaming=True)
        result = StreamingEngine(project, graph, block_size=block_size).render()
        effective = block_size
    else:
        raise argument_error("invalid_mode", f"unknown render mode {mode!r}")
    summary = write_master(output, result.audio, result.rates)
    return {
        "mode": mode,
        "block_size": effective,
        "timeline_frames": result.timeline_frames,
        "output": summary,
        "limiter": result.limiter,
        "tracks": [s.to_dict() for s in result.stats],
    }
