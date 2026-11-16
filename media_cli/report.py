"""Terminal summaries and JSON/CSV serialization."""

from __future__ import annotations

import csv
import json
from pathlib import Path
from typing import Iterable, Mapping

from .frame_source import FrameStream
from .pixel_stats import StatsResult


class ReportError(RuntimeError):
    """The requested report format cannot represent the command result."""


def scan_payload(
    info,
    *,
    frame_count: int = 0,
    repaired_timestamps: int = 0,
    has_b_frames: bool = False,
    empty_stream: bool = False,
    single_frame: bool = False,
    truncated: bool = False,
    timestamp_status: str = "ok",
    warnings: Iterable[str] = (),
) -> dict[str, object]:
    return {
        "input": info.path,
        "format": info.format_name,
        "video_stream_index": info.video_stream_index,
        "codec": info.codec_name,
        "width": info.width,
        "height": info.height,
        "time_base": info.time_base_text,
        "average_rate": info.average_rate_fps,
        "start_time": info.start_time,
        "duration_seconds": info.duration_seconds,
        "packet_count": info.packet_count,
        "has_pts": info.has_pts,
        "has_dts": info.has_dts,
        "needs_repair": (not info.has_pts) or repaired_timestamps > 0,
        "timestamp_status": timestamp_status,
        "frame_count": frame_count,
        "repaired_timestamps": repaired_timestamps,
        "empty_stream": empty_stream,
        "single_frame": single_frame,
        "has_b_frames": has_b_frames,
        "truncated": truncated,
        "warnings": list(warnings),
    }


def timeline_payload(stream: FrameStream) -> dict[str, object]:
    return {
        "frame_count": len(stream.frames),
        "empty_stream": stream.repair.empty_stream,
        "single_frame": stream.repair.single_frame,
        "has_b_frames": stream.repair.has_b_frames,
        "repaired_timestamps": stream.repair.repaired_timestamps,
        "truncated": stream.truncated,
        "warnings": stream.warnings,
        "frames": [frame.timeline_dict() for frame in stream.frames],
    }


def stats_payload(result: StatsResult) -> dict[str, object]:
    repair = result.stream.repair
    return {
        "decoded_frames": result.decoded_frames,
        "i_frames": result.kinds.i_frames,
        "p_frames": result.kinds.p_frames,
        "b_frames": result.kinds.b_frames,
        "repaired_timestamps": repair.repaired_timestamps,
        "max_jump_ms_before": repair.max_jump_ms_before,
        "max_jump_ms_after": repair.max_jump_ms_after,
        "truncated": result.truncated,
        "empty_stream": repair.empty_stream,
        "single_frame": repair.single_frame,
        "has_b_frames": result.kinds.has_b_frames,
        "keyframe_intervals": result.kinds.keyframe_intervals,
        "warnings": result.warnings,
        "repairs": [action.to_dict() for action in repair.actions],
        "frames": [frame.to_dict() for frame in result.frames],
    }


def verify_payload(
    stream: FrameStream,
    stats: StatsResult,
    *,
    tolerance_ms: float = 1e-6,
) -> dict[str, object]:
    timeline_pts = [frame.pts_ms for frame in stream.frames]
    stats_pts = [frame.pts_ms for frame in stats.frames]
    count_match = len(timeline_pts) == len(stats_pts)
    mismatch = abs(len(timeline_pts) - len(stats_pts))
    mismatch += sum(
        1
        for left, right in zip(timeline_pts, stats_pts)
        if abs(left - right) > tolerance_ms
    )
    return {
        "frame_count_match": count_match,
        "timeline_mismatch": mismatch,
        "timeline_frames": len(timeline_pts),
        "decoded_frames": len(stats_pts),
        "truncated": stream.truncated or stats.truncated,
    }


def terminal_summary(command: str, payload: Mapping[str, object]) -> str:
    if command == "scan":
        return (
            f"format={payload['format']} codec={payload['codec']} "
            f"packets={payload['packet_count']} frames={payload['frame_count']} "
            f"has_pts={str(payload['has_pts']).lower()} "
            f"needs_repair={str(payload['needs_repair']).lower()} "
            f"status={payload['timestamp_status']}"
        )
    if command == "timeline":
        return (
            f"frames={payload['frame_count']} repaired={payload['repaired_timestamps']} "
            f"has_b_frames={str(payload['has_b_frames']).lower()} "
            f"truncated={str(payload['truncated']).lower()}"
        )
    if command == "stats":
        return (
            f"decoded={payload['decoded_frames']} I={payload['i_frames']} "
            f"P={payload['p_frames']} B={payload['b_frames']} "
            f"repaired={payload['repaired_timestamps']} "
            f"max_jump_ms={payload['max_jump_ms_before']}->{payload['max_jump_ms_after']} "
            f"truncated={str(payload['truncated']).lower()}"
        )
    return (
        f"frame_count_match={str(payload['frame_count_match']).lower()} "
        f"timeline_mismatch={payload['timeline_mismatch']} "
        f"truncated={str(payload['truncated']).lower()}"
    )


def json_text(payload: Mapping[str, object], *, pretty: bool = False) -> str:
    if pretty:
        return json.dumps(payload, ensure_ascii=False, indent=2)
    return json.dumps(payload, ensure_ascii=False, separators=(",", ":"))


def write_json(path: str | Path, payload: Mapping[str, object]) -> None:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json_text(payload, pretty=True) + "\n", encoding="utf-8")


def write_csv(path: str | Path, rows: list[dict[str, object]], fieldnames: list[str]) -> None:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fieldnames, extrasaction="ignore")
        writer.writeheader()
        writer.writerows(rows)


def write_report(command: str, path: str | Path, payload: Mapping[str, object], result=None) -> None:
    output = Path(path)
    if output.suffix.lower() != ".csv":
        write_json(output, payload)
        return
    if command == "stats" and result is not None:
        write_csv(
            output,
            [frame.to_dict() for frame in result.frames],
            [
                "frame_index",
                "pts_ms",
                "pict_type",
                "luma_mean",
                "gradient_variance",
                "frame_hash",
            ],
        )
        return
    if command == "timeline":
        write_csv(
            output,
            list(payload["frames"]),
            ["frame_index", "pts_ms", "keyframe", "pict_type"],
        )
        return
    raise ReportError(f"{command} does not support CSV output")
