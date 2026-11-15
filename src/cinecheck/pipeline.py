"""Shared demux->decode->reorder->timeline->stats pipeline.

The analyze and repair commands call the exact same functions here, so
reordering and repair can never diverge between output paths.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

import av

from .decode import DecodedFrame, decode_packets
from .demux import NoVideoStreamError, StreamInfo, iter_packets, open_video
from .export import build_payload, write_repair_csv
from .reorder import ReorderResult, reorder_frames
from .stats import FrameStats, build_stats
from .timeline import (
    DEFAULT_TOLERANCE_MS,
    RepairResult,
    TimelineReport,
    detect_anomalies,
    repair_timeline,
)

EXIT_CLEAN = 0
EXIT_REPAIRED = 2
EXIT_UNRECOVERABLE = 3
EXIT_USAGE = 4


@dataclass
class PipelineResult:
    payload: dict
    stats: FrameStats
    report: TimelineReport
    repair: RepairResult | None
    reordered: ReorderResult
    info: StreamInfo
    truncated: bool
    exit_code: int


def _resolve_fps(info: StreamInfo, override: float | None) -> Fraction:
    if override is not None:
        return Fraction(override).limit_denominator(1_000_000)
    if info.nominal_rate is not None:
        return info.nominal_rate
    # Last resort for streams with no signalled rate: derive a 1:1 tick
    # rate so detection still runs deterministically rather than crashing.
    return Fraction(1, 1)


def run_pipeline(
    source: str | Path,
    fps_override: float | None = None,
    tolerance_ms: float = DEFAULT_TOLERANCE_MS,
    reorder_depth: int = 32,
    include_repair: bool = True,
) -> PipelineResult:
    """Run the full read-to-report pipeline against one video file."""

    source = Path(source)
    container, info = open_video(source)
    video = container.streams.video[0]
    truncated = False
    dropped = 0

    def _packets():
        nonlocal truncated, dropped
        try:
            for packet in iter_packets(container, video):
                # A zero-size, dts-less packet is the demuxer flush
                # marker, not a lost frame; only real empty payloads count.
                if packet.dts is None and packet.size > 0:
                    dropped += 1
                yield packet
        except av.FFmpegError:
            truncated = True
            dropped += 1

    frames: list[DecodedFrame] = []
    try:
        for frame in decode_packets(container, video, _packets()):
            frames.append(frame)
    except av.FFmpegError:
        truncated = True
    finally:
        container.close()

    reordered = reorder_frames(frames, depth=reorder_depth)
    # A truncated tail can end silently when the moov atom sits at the
    # front of the file (faststart): the stream still declares its full
    # frame count, so fewer decoded frames than declared means truncation.
    declared = info.frames_estimated
    if declared and len(reordered.frames) < declared:
        truncated = True
        dropped += max(0, declared - len(reordered.frames))
    fps = _resolve_fps(info, fps_override)
    display_pts = [frame.pts for frame in reordered.frames]

    report = detect_anomalies(
        display_pts, info.time_base, fps, tolerance_ms=tolerance_ms
    )
    repair: RepairResult | None = None
    if include_repair:
        repair = repair_timeline(
            display_pts, info.time_base, fps, tolerance_ms=tolerance_ms
        )

    stats = build_stats(
        reordered,
        info.time_base,
        info.nominal_rate,
        dropped_packets=dropped,
        declared_fps=fps if fps_override is not None else None,
    )

    exit_code = _classify(report, repair, truncated, stats)
    payload = build_payload(
        source=str(source),
        stats=stats,
        report=report,
        repair=repair,
        truncated=truncated,
        exit_code=exit_code,
        fps_override=fps_override,
    )
    return PipelineResult(
        payload=payload,
        stats=stats,
        report=report,
        repair=repair,
        reordered=reordered,
        info=info,
        truncated=truncated,
        exit_code=exit_code,
    )


def _classify(
    report: TimelineReport,
    repair: RepairResult | None,
    truncated: bool,
    stats: FrameStats,
) -> int:
    if truncated or stats.decoded_frames == 0:
        return EXIT_UNRECOVERABLE
    if repair is not None:
        if report.total > 0 or repair.repaired_count > 0:
            return EXIT_REPAIRED
    elif report.total > 0:
        return EXIT_REPAIRED
    return EXIT_CLEAN


def write_repair_outputs(
    result: PipelineResult,
    csv_path: str | Path | None,
    json_path: str | Path | None,
) -> int:
    """Persist repair CSV/JSON; returns the CSV row count (0 if no CSV)."""

    if json_path is not None:
        from .export import write_json

        write_json(result.payload, json_path)
    rows = 0
    if csv_path is not None and result.repair is not None:
        rows = write_repair_csv(
            csv_path,
            display_pts=[frame.pts for frame in result.reordered.frames],
            repaired_pts=result.repair.repaired_pts,
            pict_types=[frame.pict_type for frame in result.reordered.frames],
            time_base_text=result.stats.time_base,
        )
    return rows
