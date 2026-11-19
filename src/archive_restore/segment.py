"""Segment plan generation and multi-representation boundary alignment.

Timelines are cut into fixed target-duration windows.  Video window edges snap
to the nearest keyframe read via PyAV; audio edges snap to packet boundaries.
Representations whose nearest boundary drifts beyond a tolerance cannot be
aligned at that edge — the drift is reported as the maximum misalignment and
the window keeps an explicit, un-snapped boundary instead of silently forcing
a match.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

import av

from .errors import ErrorCollector, ErrorStage, skippable
from .timeline import RepresentationTimeline, TimelineSegment

DEFAULT_TARGET_US = 2_000_000
ALIGN_TOLERANCE_US = 40_000


@dataclass
class Boundary:
    ideal_us: int
    snapped_us: int
    source: str  # "keyframe" | "packet" | "grid" | "unavailable"


@dataclass
class AlignedWindow:
    index: int
    start_us: int
    end_us: int
    # rep_id -> list of timeline segment keys covered by this window
    members: dict[str, list[str]] = field(default_factory=dict)
    boundaries: dict[str, Boundary] = field(default_factory=dict)
    aligned: bool = True


@dataclass
class AlignmentResult:
    windows: list[AlignedWindow]
    max_deviation_us: int
    aligned_representations: int
    errors: ErrorCollector

    def to_dict(self) -> dict:
        return {
            "window_count": len(self.windows),
            "max_alignment_deviation_us": self.max_deviation_us,
            "aligned_representations": self.aligned_representations,
        }


def probe_keyframes(path: Path, kind: str) -> list[int]:
    """Return keyframe (video) or packet (audio) timestamps in microseconds."""
    try:
        container = av.open(str(path))
    except (av.AVError, OSError) as exc:
        raise FileNotFoundError(f"cannot open segment {path}: {exc}") from exc
    stamps: list[int] = []
    try:
        for stream in container.streams:
            if stream.type != kind:
                continue
            # MPEG-TS muxers add a constant container offset; subtract the
            # stream start so stamps are relative to the segment origin.
            base_us = int((stream.start_time or 0) * 1_000_000 * stream.time_base)
            for packet in container.demux(stream):
                if packet.dts is None:
                    continue
                if kind == "video" and not packet.is_keyframe:
                    continue
                raw = packet.pts if packet.pts is not None else packet.dts
                stamps.append(int(raw * 1_000_000 * packet.time_base) - base_us)
    finally:
        container.close()
    return sorted(set(stamps))


def _nearest(stamps: list[int], target: int) -> tuple[int, str]:
    if not stamps:
        return target, "unavailable"
    cand = min(stamps, key=lambda t: abs(t - target))
    return cand, "keyframe"


def _segment_key(seg: TimelineSegment) -> str:
    return f"{seg.index}:{seg.path}"


def generate_windows(timelines: dict[str, RepresentationTimeline],
                     target_duration_us: int = DEFAULT_TARGET_US,
                     errors: ErrorCollector | None = None) -> AlignmentResult:
    errors = errors or ErrorCollector()
    total = max((tl.duration_us for tl in timelines.values()), default=0)
    grid = list(range(0, total, target_duration_us))
    if not grid or grid[-1] != total:
        grid.append(total)

    # Cache per-representation boundary stamps once per underlying segment file.
    boundary_cache: dict[tuple[str, Path], list[int]] = {}

    def stamps_for(tl: RepresentationTimeline, seg: TimelineSegment) -> list[int]:
        cache_key = (tl.rep_id, Path(seg.path))
        if cache_key not in boundary_cache:
            try:
                local = probe_keyframes(Path(seg.path), tl.kind)
            except FileNotFoundError as exc:
                errors.add(skippable(ErrorStage.SEGMENT, tl.rep_id, str(exc),
                                     segment_index=seg.index, path=str(seg.path)))
                local = []
            # Offset local container stamps onto the global timeline.
            boundary_cache[cache_key] = [seg.start_us + max(0, t) for t in local]
        return boundary_cache[cache_key]

    windows: list[AlignedWindow] = []
    max_dev = 0
    rep_aligned = {rep_id: True for rep_id in timelines}

    for wi in range(len(grid) - 1):
        win = AlignedWindow(wi, grid[wi], grid[wi + 1])
        for rep_id, tl in timelines.items():
            covered = [s for s in tl.segments
                       if s.valid and s.start_us < win.end_us and s.end_us > win.start_us]
            win.members[rep_id] = [_segment_key(s) for s in covered]
            if not covered:
                win.boundaries[rep_id] = Boundary(win.start_us, win.start_us, "unavailable")
                rep_aligned[rep_id] = False
                win.aligned = False
                continue
            stamps: list[int] = []
            for seg in covered:
                stamps.extend(stamps_for(tl, seg))
            snapped, source = _nearest(stamps, win.start_us)
            if source == "unavailable":
                source = "grid"
                snapped = win.start_us
            dev = abs(snapped - win.start_us)
            max_dev = max(max_dev, dev)
            if dev > ALIGN_TOLERANCE_US:
                rep_aligned[rep_id] = False
                win.aligned = False
                errors.add(skippable(
                    ErrorStage.SEGMENT, rep_id,
                    f"window {wi} boundary drift {dev}us exceeds tolerance",
                    segment_index=win.index, detail={"deviation_us": dev}))
                snapped, source = win.start_us, "grid"
            win.boundaries[rep_id] = Boundary(win.start_us, snapped, source)
        windows.append(win)

    return AlignmentResult(windows, max_dev, sum(1 for ok in rep_aligned.values() if ok), errors)


def build_segment_manifest(result: AlignmentResult) -> list[dict]:
    """Serializable per-window segment manifest for the recovery stage."""
    manifest = []
    for win in result.windows:
        manifest.append({
            "window_index": win.index,
            "start_us": win.start_us,
            "end_us": win.end_us,
            "aligned": win.aligned,
            "members": win.members,
            "boundaries": {rid: {"ideal": b.ideal_us, "snapped": b.snapped_us, "source": b.source}
                           for rid, b in win.boundaries.items()},
        })
    return manifest
