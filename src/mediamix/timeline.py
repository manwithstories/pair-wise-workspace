"""Map variable-frame-rate tracks onto one unified integer time base.

Each track's acquisition clocks are mapped through its rational drift model
(:mod:`mediamix.clock`) to master nanosecond ticks. Frames from all tracks are
then merged:

* the highest-priority track wins an overlap (lower ``priority`` number);
* near-coincident frames inside ``tolerance_ticks`` are treated as the same
  time point and emit a ``tolerance`` diagnostic;
* backward time, gaps in the sequence counter and dropped overlaps are all
  recorded as :class:`~mediamix.model.Diagnostic` events.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .model import Diagnostic

DEFAULT_TOLERANCE_NS = 1_000  # frames within 1 us are treated as coincident


@dataclass
class MappedFrame:
    """A frame placed on the master time base."""

    track_id: str
    file_seq: int
    frame_seq: int          # unwrapped sequence counter value
    tick: int               # master-clock tick (integer ns)
    width: int
    height: int
    fps_num: int
    fps_den: int
    color_primaries: str
    transfer: str


@dataclass
class TrackTimeline:
    """One track after clock mapping, before merging."""

    track_id: str
    priority: int
    frames: list[MappedFrame] = field(default_factory=list)
    diagnostics: list[Diagnostic] = field(default_factory=list)
    wraps: int = 0


@dataclass
class Timeline:
    """Merged, strictly tick-ordered composition schedule."""

    frames: list[MappedFrame] = field(default_factory=list)
    diagnostics: list[Diagnostic] = field(default_factory=list)
    track_ids: list[str] = field(default_factory=list)

    def span_ticks(self) -> tuple[int, int]:
        if not self.frames:
            return (0, 0)
        return (self.frames[0].tick, self.frames[-1].tick)


def map_track(
    track_id: str,
    priority: int,
    parsed_frames,
    unwrapped_seq: list[int],
    unwrapped_clock: list[int],
    drift_model,
    wraps: int,
) -> TrackTimeline:
    """Convert parsed frames plus an unwrapped clock into master-tick frames."""
    if not (len(parsed_frames) == len(unwrapped_seq) == len(unwrapped_clock)):
        raise ValueError("frame, sequence and clock series length mismatch")
    tt = TrackTimeline(track_id=track_id, priority=priority, wraps=wraps)
    prev_tick: int | None = None
    prev_seq: int | None = None
    for rec, seq, clock in zip(parsed_frames, unwrapped_seq, unwrapped_clock):
        hdr = rec.header
        tick = drift_model.to_reference(clock)
        if prev_tick is not None and tick < prev_tick:
            tt.diagnostics.append(Diagnostic(
                kind="backward_time", track_id=track_id, frame_seq=seq, tick=tick,
                detail=f"tick {tick} precedes previous tick {prev_tick}",
            ))
        if prev_seq is not None and seq != prev_seq + 1:
            tt.diagnostics.append(Diagnostic(
                kind="missing_frame", track_id=track_id, frame_seq=seq, tick=tick,
                detail=f"sequence gap {prev_seq} -> {seq} ({seq - prev_seq - 1} dropped)",
            ))
        tt.frames.append(MappedFrame(
            track_id=track_id, file_seq=rec.file_seq, frame_seq=seq, tick=tick,
            width=hdr.width, height=hdr.height, fps_num=hdr.fps_num,
            fps_den=hdr.fps_den, color_primaries=hdr.color_primaries,
            transfer=hdr.transfer,
        ))
        prev_tick, prev_seq = tick, seq
    return tt


def merge_tracks(
    tracks: list[TrackTimeline],
    *,
    tolerance_ticks: int = DEFAULT_TOLERANCE_NS,
) -> Timeline:
    """Merge per-track schedules, resolving overlaps by track priority."""
    timeline = Timeline(track_ids=[t.track_id for t in tracks])
    for t in tracks:
        timeline.diagnostics.extend(t.diagnostics)

    ordered = sorted(
        ((frame, t.priority) for t in tracks for frame in t.frames),
        key=lambda item: (item[0].tick, item[1], item[0].track_id),
    )
    last_kept: MappedFrame | None = None
    for frame, _priority in ordered:
        if last_kept is not None and frame.tick == last_kept.tick:
            timeline.diagnostics.append(Diagnostic(
                kind="overlap", track_id=frame.track_id, frame_seq=frame.frame_seq,
                tick=frame.tick,
                detail=f"duplicate time point; kept {last_kept.track_id}, dropped {frame.track_id}",
            ))
            continue
        if last_kept is not None and abs(frame.tick - last_kept.tick) <= tolerance_ticks:
            timeline.diagnostics.append(Diagnostic(
                kind="tolerance", track_id=frame.track_id, frame_seq=frame.frame_seq,
                tick=frame.tick,
                detail=(f"frames within {tolerance_ticks} ns "
                        f"({last_kept.track_id}@{last_kept.tick} vs {frame.track_id}@{frame.tick})"),
            ))
        timeline.frames.append(frame)
        last_kept = frame
    return timeline


def build_timeline(
    tracks: list[TrackTimeline],
    *,
    tolerance_ticks: int = DEFAULT_TOLERANCE_NS,
    interval: tuple[int, int] | None = None,
) -> Timeline:
    """Merge tracks and optionally clip to ``[start_tick, end_tick]``."""
    timeline = merge_tracks(tracks, tolerance_ticks=tolerance_ticks)
    if interval is not None:
        start, end = interval
        if start > end:
            raise ValueError(f"interval start {start} > end {end}")
        timeline.frames = [f for f in timeline.frames if start <= f.tick <= end]
        timeline.diagnostics = [
            d for d in timeline.diagnostics
            if start <= d.tick <= end or d.kind == "missing_frame"
        ]
    return timeline
