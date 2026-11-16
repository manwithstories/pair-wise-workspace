"""Timestamp normalization: unify TimecodeScale, repair ordering, rewrite times.

Matroska blocks carry a Cluster-relative signed timecode in TimecodeScale ticks.
MP4 wants per-track decode timestamps in a rational timescale with strict
monotonicity. Real game-capture exports violate that: negative first frames,
duplicate timestamps from muxer rounding, backwards jumps, and missing per-frame
durations.

Every repair is counted per track and the whole plan is auditable: each
:class:`TrackTimeline` exposes the source and rewritten arrays plus the shift
applied, so `verify` can justify each rewritten value rather than assert one.
"""

from __future__ import annotations

import numpy as np

from ._timing import (
    AUDIO_TIMESCALE,
    SUBTITLE_TIMESCALE,
    VIDEO_TIMESCALE,
    RepairStats,
    RewritePlan,
    TimingConfig,
    TimestampError,
    TrackTimeline,
)
from ._timing_math import (
    force_monotonic,
    grid_dts,
    ns_to_ticks,
    nudge_duplicates,
    ticks_to_target,
)
from .matroska_model import (
    DEFAULT_TIMESTAMP_SCALE,
    NANOS_PER_SECOND,
    MatroskaDocument,
    Track,
)


# ── per-track planning ────────────────────────────────────────────────────


def _nominal_duration_ns(document: MatroskaDocument, track: Track, ticks: np.ndarray) -> float | None:
    """Best-effort nominal frame duration for a track, in nanoseconds."""
    if track.default_duration_ns:
        return float(track.default_duration_ns)
    if ticks.size >= 2:
        diffs = np.diff(ticks)
        positive = diffs[diffs > 0]
        if positive.size:
            return float(np.median(positive)) * document.info.timestamp_scale_ns
    return None


def build_timeline(
    document: MatroskaDocument,
    track: Track,
    *,
    config: TimingConfig | None = None,
    shift_ns: int = 0,
) -> TrackTimeline:
    """Normalize one track's timestamps and return its rewrite record.

    ``shift_ns`` is a document-wide origin shift (nanoseconds) already decided
    by the caller so all tracks share one origin.
    """
    config = config or TimingConfig()
    scale_ns = document.info.timestamp_scale_ns or DEFAULT_TIMESTAMP_SCALE
    timescale = config.timescale_for(track)
    stats = RepairStats()
    notes: list[str] = []

    blocks = document.blocks_for_track(track)
    if not blocks:
        raise TimestampError("track_without_blocks", f"track {track.number} has no blocks", track.number)

    raw_ticks = np.array([b.timecode for b in blocks], dtype=np.int64)
    raw_ns = raw_ticks * scale_ns

    nominal_ns = _nominal_duration_ns(document, track, raw_ticks)
    if nominal_ns is None:
        notes.append("nominal duration unknown; using 40 ms fallback")
        nominal_ns = 40_000_000.0
        stats.missing_duration += 1

    shifted_ns = raw_ns + shift_ns
    stats.negative = int(np.count_nonzero(raw_ns < 0))
    if stats.negative and not config.normalize_origin:
        raise TimestampError(
            "negative_timestamps",
            f"track {track.number} has {stats.negative} negative timestamps and "
            "origin normalization is disabled",
            track.number,
        )
    # MP4 has no negative decode time: clamp anything still below zero and
    # record it, so a partially-shifted track is visible rather than silent.
    below_zero = shifted_ns < 0
    stats.clamped = int(np.count_nonzero(below_zero))
    pts = ns_to_ticks(np.maximum(shifted_ns, 0), timescale)
    step = config.min_step_for(track)

    # PTS is presentation order from the source. Repeated values are a muxer
    # defect and move forward by the track's minimum legal step. A *backwards*
    # PTS is different: that is ordinary B-frame reordering (storage order is
    # decode order), so it is preserved and expressed later as a composition
    # offset rather than being flattened away.
    if config.force_monotonic_pts:
        stats.duplicates = int(np.count_nonzero(np.diff(pts) <= 0))
        stats.regressions = int(np.count_nonzero(np.diff(pts) < 0))
        pts = force_monotonic(pts, step)
    else:
        stats.duplicates = int(np.count_nonzero(np.diff(pts) == 0))
        stats.regressions = int(np.count_nonzero(np.diff(pts) < 0))
        pts = nudge_duplicates(pts, step, config)

    # DTS is decode time and must be strictly increasing for every track,
    # whether or not the source reordered frames.
    if pts.size < 2 or bool(np.all(np.diff(pts) > 0)):
        dts = pts.copy()
    else:
        dts = grid_dts(pts, step)
        notes.append("presentation order differs from storage order; composition offsets carry PTS")

    # Durations: explicit BlockDuration wins, then the next DTS, then nominal.
    durations = np.empty(dts.size, dtype=np.int64)
    for i in range(dts.size):
        block = blocks[i]
        if block.duration_ticks:
            durations[i] = max(step, ticks_to_target(block.duration_ticks, scale_ns, timescale))
        elif i + 1 < dts.size:
            durations[i] = max(step, int(dts[i + 1] - dts[i]))
        else:
            durations[i] = max(step, int(round(nominal_ns * timescale / NANOS_PER_SECOND)))
            stats.missing_duration += 1
            notes.append("trailing sample has no BlockDuration; used nominal duration")

    # Jump detection on the rewritten DTS: informational, never destructive.
    if dts.size >= 2:
        gaps = np.diff(dts)
        nominal_ticks = max(step, int(round(nominal_ns * timescale / NANOS_PER_SECOND)))
        threshold = nominal_ticks * config.jump_threshold_frames
        jumps = int(np.count_nonzero(gaps > threshold))
        stats.jumps = jumps
        if jumps:
            notes.append(f"{jumps} inter-sample gap(s) exceed {config.jump_threshold_frames:g} nominal frames")

    # If origin normalization was applied, record the shift as a whole-tick value.
    shift_ticks = int(round(shift_ns / scale_ns)) if scale_ns else 0
    return TrackTimeline(
        track_number=track.number,
        track_type=track.type_name,
        timescale=timescale,
        source_scale_ns=scale_ns,
        source_ticks=raw_ticks,
        source_ns=raw_ns.astype(np.int64),
        rewritten_dts=dts,
        rewritten_pts=pts,
        durations=durations,
        shift_ticks=shift_ticks,
        shift_ns=shift_ns,
        stats=stats,
        sample_count=int(dts.size),
        nominal_duration_ns=nominal_ns,
        notes=notes,
    )


# ── document-level planning ───────────────────────────────────────────────


def plan_rewrite(
    document: MatroskaDocument,
    *,
    tracks: list[Track] | None = None,
    config: TimingConfig | None = None,
) -> RewritePlan:
    """Compute a shared-origin rewrite plan for every selected track.

    A single document-wide shift is chosen from the *most negative* first
    timestamp across tracks, so inter-track sync is preserved rather than each
    track being individually zeroed (which would silently slide audio against
    video).
    """
    config = config or TimingConfig()
    scale_ns = document.info.timestamp_scale_ns or DEFAULT_TIMESTAMP_SCALE
    selected = tracks if tracks is not None else document.tracks
    if not selected:
        raise TimestampError("no_tracks_selected", "no tracks requested for rewrite")

    firsts: list[int] = []
    for track in selected:
        blocks = document.blocks_for_track(track)
        if not blocks:
            continue
        firsts.append(min(b.timecode for b in blocks))
    if not firsts:
        raise TimestampError("no_samples", "selected tracks contain no blocks")

    shift_ns = 0
    if config.normalize_origin:
        earliest = min(firsts)
        if earliest < 0:
            shift_ns = -earliest * scale_ns

    timelines: dict[int, TrackTimeline] = {}
    warnings: list[dict[str, object]] = []
    for track in selected:
        try:
            timelines[track.number] = build_timeline(
                document, track, config=config, shift_ns=shift_ns
            )
        except TimestampError as exc:
            if exc.code == "track_without_blocks":
                warnings.append(
                    {
                        "code": "track_without_blocks",
                        "message": str(exc),
                        "track_number": track.number,
                    }
                )
                continue
            raise
    if not timelines:
        raise TimestampError("no_samples", "no selected track produced a timeline")

    plan = RewritePlan(
        source_scale_ns=scale_ns,
        timelines=timelines,
        global_shift_ticks=int(round(shift_ns / scale_ns)) if scale_ns else 0,
        warnings=warnings,
    )
    _check_intertrack(plan, warnings)
    return plan


def _check_intertrack(plan: RewritePlan, warnings: list[dict[str, object]]) -> None:
    """Warn when tracks disagree wildly on origin — a likely sync defect."""
    starts = {
        number: tl.first_rewritten_seconds()
        for number, tl in plan.timelines.items()
        if tl.rewritten_dts.size
    }
    if len(starts) < 2:
        return
    values = [v for v in starts.values() if v is not None]
    if not values:
        return
    spread = max(values) - min(values)
    if spread > 2.0:
        warnings.append(
            {
                "code": "intertrack_origin_spread",
                "message": f"track start times differ by {spread:.3f}s after shift",
                "spread_seconds": round(spread, 6),
            }
        )


def monotonic_ok(values: np.ndarray) -> bool:
    """True when an int array is strictly increasing."""
    return bool(values.size < 2 or np.all(np.diff(values) > 0))


__all__ = [
    "AUDIO_TIMESCALE",
    "RepairStats",
    "RewritePlan",
    "SUBTITLE_TIMESCALE",
    "TimingConfig",
    "TimestampError",
    "TrackTimeline",
    "VIDEO_TIMESCALE",
    "build_timeline",
    "monotonic_ok",
    "plan_rewrite",
]
