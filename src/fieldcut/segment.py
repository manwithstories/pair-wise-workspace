"""Plan common segment cut points on the shared media timeline.

Cuts are computed once on the shared clock and shared by every camera and
every bitrate representation, so corresponding segments across the ladder are
guaranteed to describe the same media interval. Each planned cut is then
snapped to the nearest real keyframe of each camera; the difference between
the ideal cut and the achievable cut is recorded rather than hidden.

Short tails, missing keyframes and encoders that cannot land exactly on a
boundary therefore all surface as explicit deviation records in the plan.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .align import AlignmentResult, common_trusted_window
from .errors import ErrorLog, Recovery, Stage
from .probe import ProbeResult
from .timebase import TICKS_PER_MS, seconds_to_ticks

DEFAULT_TARGET_SECONDS = 2.0
# A tail shorter than 60% of a target segment is reported as a short tail.
SHORT_TAIL_RATIO = 0.6
# Snapping farther than this from the ideal cut is a keyframe-coverage defect.
KEYFRAME_GRACE_MS = 250.0


@dataclass
class BoundarySnap:
    """One cut point resolved against one camera's real keyframe grid."""

    ideal_ticks: int
    actual_ticks: int
    nearest_keyframe_ticks: int
    deviation_ms: float
    used_keyframe: bool


@dataclass
class SegmentPlan:
    index: int
    start_ticks: int              # shared-clock ideal boundaries
    end_ticks: int
    target_duration_ticks: int
    is_short_tail: bool
    snaps: dict[str, BoundarySnap] = field(default_factory=dict)
    damaged: bool = False

    @property
    def duration_ticks(self) -> int:
        return self.end_ticks - self.start_ticks


@dataclass
class SegmentationPlan:
    target_seconds: float
    segments: list[SegmentPlan]
    window_start_ticks: int
    window_end_ticks: int

    def __len__(self) -> int:
        return len(self.segments)


def _nearest_keyframe(key_ticks: list[int], target: int) -> int:
    if not key_ticks:
        return target
    return min(key_ticks, key=lambda k: (abs(k - target), k))


def _snap_for_camera(ideal_shared: int, probe: ProbeResult,
                     alignment: AlignmentResult) -> BoundarySnap:
    """Resolve a shared-clock cut onto one camera's local keyframe axis."""
    cam = alignment.for_camera(probe.videos[0].camera)
    # Inverse of the shared mapping to get the camera's local media time.
    local = int(round((ideal_shared - cam.offset_ticks)
                      / (1.0 + cam.drift_rate_ppm / 1e6)))
    key_ticks = probe.videos[0].keyframe_ticks
    kf = _nearest_keyframe(key_ticks, local)
    has_kf = bool(key_ticks)
    actual = kf if has_kf else local
    deviation_ms = abs(actual - local) / TICKS_PER_MS
    return BoundarySnap(ideal_ticks=ideal_shared, actual_ticks=actual,
                        nearest_keyframe_ticks=kf,
                        deviation_ms=round(deviation_ms, 3),
                        used_keyframe=has_kf)


def plan_segments(
    probes: list[ProbeResult],
    alignment: AlignmentResult,
    log: ErrorLog,
    target_seconds: float = DEFAULT_TARGET_SECONDS,
) -> SegmentationPlan:
    """Lay out equal target-length segments inside the common trusted window."""
    if target_seconds <= 0:
        raise ValueError("target_seconds must be positive")
    win_start, win_end = common_trusted_window(alignment)
    target_ticks = seconds_to_ticks(target_seconds)
    if win_end - win_start < target_ticks // 2:
        raise ValueError("common trusted window shorter than half a segment; "
                         "nothing safely segmentable")

    probe_by_cam = {p.videos[0].camera: p for p in probes}

    # Ideal shared-clock grid, anchored at the common window start.
    cuts: list[int] = [win_start]
    t = win_start + target_ticks
    while t < win_end:
        cuts.append(t)
        t += target_ticks
    cuts.append(win_end)

    plans: list[SegmentPlan] = []
    for idx in range(len(cuts) - 1):
        start, end = cuts[idx], cuts[idx + 1]
        is_tail = idx == len(cuts) - 2
        short_tail = is_tail and (
            end - start < int(target_ticks * SHORT_TAIL_RATIO))
        plan = SegmentPlan(index=idx, start_ticks=start, end_ticks=end,
                           target_duration_ticks=target_ticks,
                           is_short_tail=short_tail)
        is_final_cut = idx == len(cuts) - 2
        for cam_name, probe in probe_by_cam.items():
            cam = alignment.for_camera(cam_name)
            for cut_idx, ideal in enumerate((start, end)):
                snap = _snap_for_camera(ideal, probe, alignment)
                plan.snaps[f"{cam_name}@{ideal}"] = snap
                # The window's final cut coincides with end-of-media, where no
                # later keyframe exists; only interior cuts must be anchored.
                if is_final_cut and cut_idx == 1:
                    continue
                if not snap.used_keyframe:
                    log.add(Stage.SEGMENT, cam_name,
                            f"no keyframe near cut {ideal / TICKS_PER_MS:.1f}"
                            " ms; encoder must emit IDR at the boundary",
                            media_time_ticks=ideal,
                            recovery=Recovery.SEGMENTED)
                elif snap.deviation_ms > KEYFRAME_GRACE_MS:
                    log.add(Stage.SEGMENT, cam_name,
                            f"cut at {ideal / TICKS_PER_MS:.1f} ms deviates "
                            f"{snap.deviation_ms:.1f} ms from nearest keyframe",
                            media_time_ticks=ideal,
                            recovery=Recovery.SEGMENTED)
        # Mark segments overlapping any camera's review/damaged ranges. The
        # review ranges are local media times, so convert each segment to the
        # camera's local axis before intersecting.
        for cam in alignment.cameras:
            local_start = int(round(
                (start - cam.offset_ticks)
                / (1.0 + cam.drift_rate_ppm / 1e6)))
            local_end = int(round(
                (end - cam.offset_ticks)
                / (1.0 + cam.drift_rate_ppm / 1e6)))
            for rs, re in cam.review_ranges:
                if re > local_start and rs < local_end:
                    plan.damaged = True
        if short_tail:
            log.add(Stage.SEGMENT, "shared",
                    f"segment {idx} is a short tail "
                    f"({(end - start) / TICKS_PER_MS:.1f} ms)",
                    media_time_ticks=start, recovery=Recovery.SEGMENTED)
        plans.append(plan)

    return SegmentationPlan(target_seconds=target_seconds,
                            segments=plans,
                            window_start_ticks=win_start,
                            window_end_ticks=win_end)
