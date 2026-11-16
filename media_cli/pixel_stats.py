"""Streaming decoded-pixel statistics aligned to the shared frame timeline."""

from __future__ import annotations

import hashlib
from collections import Counter
from dataclasses import dataclass, field, replace
from pathlib import Path

import numpy as np

from .frame_kinds import FrameKindSummary, summarize_frame_kinds
from .frame_source import FrameRecord, FrameStream, build_frame_stream, iter_decoded_frames


@dataclass(frozen=True)
class PixelFrameStat:
    frame_index: int
    pts_ms: float
    pict_type: str | None
    luma_mean: float
    gradient_variance: float
    frame_hash: str

    def to_dict(self) -> dict[str, object]:
        return {
            "frame_index": self.frame_index,
            "pts_ms": self.pts_ms,
            "pict_type": self.pict_type,
            "luma_mean": self.luma_mean,
            "gradient_variance": self.gradient_variance,
            "frame_hash": self.frame_hash,
        }


@dataclass
class StatsResult:
    stream: FrameStream
    frames: list[PixelFrameStat]
    kinds: FrameKindSummary
    decoded_frames: int
    truncated: bool = False
    error: str | None = None
    warnings: list[str] = field(default_factory=list)


def _frame_metrics(gray: np.ndarray) -> tuple[float, float, str]:
    luma_mean = float(np.mean(gray, dtype=np.float64))
    gradient_y, gradient_x = np.gradient(gray.astype(np.float32))
    gradient_variance = float(np.var(np.concatenate((gradient_x.ravel(), gradient_y.ravel()))))
    digest = hashlib.sha256(np.ascontiguousarray(gray).tobytes()).hexdigest()
    return luma_mean, gradient_variance, digest


def collect_stats(path: str | Path, stream_index: int | None = None) -> StatsResult:
    stream = build_frame_stream(path, stream_index)
    decoded = iter_decoded_frames(path, stream_index)
    frames: list[PixelFrameStat] = []
    types: list[str | None] = []
    for item in decoded:
        timeline: FrameRecord | None = (
            stream.frames[item.index] if item.index < len(stream.frames) else None
        )
        gray = item.frame.reformat(format="gray").to_ndarray()
        luma_mean, gradient_variance, digest = _frame_metrics(gray)
        frames.append(
            PixelFrameStat(
                frame_index=item.index,
                pts_ms=timeline.pts_ms if timeline else 0.0,
                pict_type=item.pict_type or (timeline.pict_type if timeline else None),
                luma_mean=round(luma_mean, 4),
                gradient_variance=round(gradient_variance, 4),
                frame_hash=digest,
            )
        )
        types.append(item.pict_type or (timeline.pict_type if timeline else None))

    summary_frames: list[FrameRecord] = []
    for index, kind in enumerate(types):
        if index < len(stream.frames):
            summary_frames.append(replace(stream.frames[index], pict_type=kind))
        else:
            summary_frames.append(
                FrameRecord(index, None, 0.0, None, index, kind == "I", kind, None)
            )
    summary = summarize_frame_kinds(summary_frames)
    truncated = stream.truncated or decoded.truncated
    warnings = list(stream.warnings)
    if truncated:
        warnings.append("tail is truncated; statistics cover complete frames only")
    return StatsResult(
        stream=stream,
        frames=frames,
        kinds=summary,
        decoded_frames=len(frames),
        truncated=truncated,
        error=decoded.error or stream.error,
        warnings=warnings,
    )


def count_pict_types(frames: list[PixelFrameStat]) -> Counter[str]:
    return Counter(frame.pict_type for frame in frames if frame.pict_type)
