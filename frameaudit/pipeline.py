"""High-level scan and repair flows shared by the CLI and tests."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from frameaudit.decode import BadRegion, iter_decode_frames
from frameaudit.probe import probe_video
from frameaudit.reorder import PtsReorderer
from frameaudit.stats import FrameStats
from frameaudit.timestamps import DEFAULT_CROSS_JUMP_US, TimelineRepairer
from frameaudit.writer import DualWriter


def scan_video(path: str | Path, reorder_depth: int = 16) -> dict[str, Any]:
    probe = probe_video(path)
    stats = FrameStats()
    errors: list[BadRegion] = []
    reorder = PtsReorderer(reorder_depth, probe.nominal_interval_us)

    for decoded in iter_decode_frames(path, errors):
        for frame in reorder.push(decoded):
            stats.add(frame.pts_raw_us, frame.pict_type, frame.keyframe)
    for frame in reorder.flush():
        stats.add(frame.pts_raw_us, frame.pict_type, frame.keyframe)

    result: dict[str, Any] = {
        "input": str(path),
        "container": probe.as_dict(),
        "vfr": probe.is_vfr,
        **stats.as_dict(),
        "reorder_depth": reorder_depth,
        "reorder_max_buffered": reorder.max_buffered,
        "interpolated_pts": reorder.interpolated_count,
        "bad_regions": [region.as_dict() for region in errors],
    }
    return result


def repair_video(
    path: str | Path,
    index_path: str | Path,
    proxy_path: str | Path,
    reorder_depth: int = 16,
    cross_jump_us: int = DEFAULT_CROSS_JUMP_US,
) -> dict[str, Any]:
    probe = probe_video(path)
    errors: list[BadRegion] = []
    reorder = PtsReorderer(reorder_depth, probe.nominal_interval_us)
    repairer = TimelineRepairer(probe.nominal_interval_us, cross_jump_us)
    stats = FrameStats()
    writer = DualWriter(
        index_path,
        proxy_path,
        probe.width,
        probe.height,
        probe.nominal_interval_us,
    )
    try:
        for decoded in iter_decode_frames(path, errors):
            for frame in reorder.push(decoded):
                stats.add(frame.pts_raw_us, frame.pict_type, frame.keyframe)
                writer.write(repairer.fix(frame))
        for frame in reorder.flush():
            stats.add(frame.pts_raw_us, frame.pict_type, frame.keyframe)
            writer.write(repairer.fix(frame))
        if repairer.summary()["fixed_monotonic"] is not True:
            raise RuntimeError("repaired timeline is not strictly monotonic")
        writer.commit()
    except Exception:
        writer.abort()
        raise

    result: dict[str, Any] = {
        "input": str(path),
        "container": probe.as_dict(),
        "vfr": probe.is_vfr,
        **stats.as_dict(),
        "index_rows": writer.index.rows,
        "proxy_frames": writer.proxy.frames,
        "reorder_depth": reorder_depth,
        "reorder_max_buffered": reorder.max_buffered,
        "interpolated_pts": reorder.interpolated_count,
        **repairer.summary(),
        "bad_regions": [region.as_dict() for region in errors],
    }
    if result["index_rows"] != result["proxy_frames"]:
        raise RuntimeError("index and proxy frame counts diverged")
    return result

