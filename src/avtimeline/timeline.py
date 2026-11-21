"""Timeline parsing and validation.

A timeline is a set of tracks, each covered by clips.  Clips reference source
media by track id and carry a source in-point.  Transitions bridge adjacent
clips on the same track and must cover the union of their boundary regions.
Every check returns :class:`~avtimeline.errors.Issue` objects with an exact
location so the first anomaly can be reported down to the frame or sample.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable, Mapping

from .errors import Issue, MediaLocation
from .model import Span, TimeError, fraction_from_float, gaps_within, merge_spans


class TimelineParseError(ValueError):
    """Raised when the timeline document cannot be parsed at all."""


def _fraction_from_json(value: Any, field_name: str) -> Fraction:
    if isinstance(value, bool):
        raise TimelineParseError(f"{field_name}: boolean is not a time value")
    if isinstance(value, int):
        return Fraction(value)
    if isinstance(value, str):
        try:
            return Fraction(value)
        except (ValueError, ZeroDivisionError) as exc:
            raise TimelineParseError(f"{field_name}: invalid rational {value!r}") from exc
    if isinstance(value, float):
        try:
            return fraction_from_float(value)
        except TimeError as exc:
            raise TimelineParseError(f"{field_name}: {exc}") from exc
    raise TimelineParseError(f"{field_name}: unsupported value {value!r}")


@dataclass(frozen=True)
class Clip:
    """One placed clip on a track."""

    id: str
    track: str
    span: Span
    source: str
    source_in: Fraction = Fraction(0)

    @property
    def start(self) -> Fraction:
        return self.span.start

    @property
    def end(self) -> Fraction:
        return self.span.end


@dataclass(frozen=True)
class Transition:
    """A transition that bridges two adjacent clips on the same track."""

    after: str
    before: str
    span: Span


@dataclass
class Track:
    """A timeline track: its id, kind and ordered clips."""

    id: str
    kind: str = "video"
    clips: list[Clip] = field(default_factory=list)
    transitions: list[Transition] = field(default_factory=list)
    coverage_mode: str = "window"

    @property
    def span(self) -> Span | None:
        if not self.clips:
            return None
        return Span(min(c.start for c in self.clips), max(c.end for c in self.clips))

    def coverage(self) -> list[Span]:
        return merge_spans(c.span for c in self.clips)

    @property
    def requires_full_coverage(self) -> bool:
        return self.coverage_mode == "window"


@dataclass
class Timeline:
    """A parsed timeline document."""

    tracks: dict[str, Track] = field(default_factory=dict)
    window: Span | None = None

    def add_track(self, track: Track) -> None:
        if track.id in self.tracks:
            raise TimelineParseError(f"duplicate track id {track.id!r}")
        self.tracks[track.id] = track

    @property
    def track_ids(self) -> list[str]:
        return list(self.tracks)

    def all_clips(self) -> list[Clip]:
        clips: list[Clip] = []
        for track in self.tracks.values():
            clips.extend(track.clips)
        return clips

    def declared_window(self) -> Span:
        """The overall window: explicit, or the union of all track spans."""
        if self.window is not None:
            return self.window
        spans = [t.span for t in self.tracks.values() if t.span is not None]
        if not spans:
            raise TimelineParseError("timeline has neither a window nor any clip")
        start = min(s.start for s in spans)
        end = max(s.end for s in spans)
        return Span(start, end)


def _parse_clip(raw: Mapping[str, Any], track_id: str) -> Clip:
    clip_id = str(raw.get("id", "")).strip()
    if not clip_id:
        raise TimelineParseError(f"track {track_id}: clip without id")
    start = _fraction_from_json(raw.get("start"), f"clip {clip_id}.start")
    end = _fraction_from_json(raw.get("end"), f"clip {clip_id}.end")
    if end <= start:
        raise TimelineParseError(f"clip {clip_id}: end must be greater than start")
    source = str(raw.get("source", track_id))
    source_in = _fraction_from_json(raw.get("source_in", 0), f"clip {clip_id}.source_in")
    if source_in < 0:
        raise TimelineParseError(f"clip {clip_id}: source_in must not be negative")
    return Clip(id=clip_id, track=track_id, span=Span(start, end), source=source, source_in=source_in)


def _parse_transition(raw: Mapping[str, Any]) -> Transition:
    after = str(raw.get("after", "")).strip()
    before = str(raw.get("before", "")).strip()
    if not after or not before:
        raise TimelineParseError("transition requires 'after' and 'before' clip ids")
    start = _fraction_from_json(raw.get("start"), f"transition {after}->{before}.start")
    end = _fraction_from_json(raw.get("end"), f"transition {after}->{before}.end")
    if end <= start:
        raise TimelineParseError(f"transition {after}->{before}: end must be greater than start")
    return Transition(after=after, before=before, span=Span(start, end))


def parse_timeline(document: Mapping[str, Any]) -> Timeline:
    """Build a :class:`Timeline` from a decoded JSON document."""
    if not isinstance(document, Mapping):
        raise TimelineParseError("timeline document must be an object")
    timeline = Timeline()
    window = document.get("window")
    if window is not None:
        start = _fraction_from_json(window.get("start"), "window.start")
        end = _fraction_from_json(window.get("end"), "window.end")
        if end <= start:
            raise TimelineParseError("window.end must be greater than window.start")
        timeline.window = Span(start, end)
    tracks = document.get("tracks")
    if not isinstance(tracks, list) or not tracks:
        raise TimelineParseError("timeline requires a non-empty 'tracks' list")
    for raw_track in tracks:
        if not isinstance(raw_track, Mapping):
            raise TimelineParseError("each track must be an object")
        track_id = str(raw_track.get("id", "")).strip()
        if not track_id:
            raise TimelineParseError("track without id")
        kind = str(raw_track.get("kind", "video"))
        coverage_mode = str(raw_track.get("coverage", "window"))
        if coverage_mode not in ("window", "clips"):
            raise TimelineParseError(
                f"track {track_id}: coverage must be 'window' or 'clips'"
            )
        track = Track(id=track_id, kind=kind, coverage_mode=coverage_mode)
        for raw_clip in raw_track.get("clips", []) or []:
            if not isinstance(raw_clip, Mapping):
                raise TimelineParseError(f"track {track_id}: clip must be an object")
            track.clips.append(_parse_clip(raw_clip, track_id))
        for raw_transition in raw_track.get("transitions", []) or []:
            if not isinstance(raw_transition, Mapping):
                raise TimelineParseError(f"track {track_id}: transition must be an object")
            track.transitions.append(_parse_transition(raw_transition))
        timeline.add_track(track)
    return timeline


def load_timeline(path: str) -> Timeline:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            document = json.load(handle)
    except OSError as exc:
        raise TimelineParseError(f"cannot read timeline {path}: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise TimelineParseError(f"timeline {path} is not valid JSON: {exc}") from exc
    return parse_timeline(document)


def validate_tracks(timeline: Timeline) -> list[Issue]:
    """Validate clip ordering, references and overlap on every track."""
    issues: list[Issue] = []
    for track in timeline.tracks.values():
        ordered = sorted(track.clips, key=lambda c: (c.start, c.end, c.id))
        seen_ids: set[str] = set()
        for clip in ordered:
            if clip.id in seen_ids:
                issues.append(
                    Issue(
                        code="duplicate_clip",
                        message=f"clip {clip.id!r} appears more than once on track {track.id!r}",
                        location=MediaLocation(track=track.id, pts=clip.start),
                    )
                )
            seen_ids.add(clip.id)
        for index in range(1, len(ordered)):
            prev, cur = ordered[index - 1], ordered[index]
            if cur.start < prev.end:
                issues.append(
                    Issue(
                        code="clip_overlap",
                        message=(
                            f"clips {prev.id!r} and {cur.id!r} overlap by "
                            f"{prev.end - cur.start}s on track {track.id!r}"
                        ),
                        location=MediaLocation(track=track.id, pts=cur.start),
                        context={"previous_clip": prev.id, "clip": cur.id},
                    )
                )
        for clip in ordered:
            if clip.end <= clip.start:
                issues.append(
                    Issue(
                        code="empty_clip",
                        message=f"clip {clip.id!r} has non-positive duration",
                        location=MediaLocation(track=track.id, pts=clip.start),
                    )
                )
        known = {c.id for c in ordered}
        for transition in track.transitions:
            for name, clip_id in (("after", transition.after), ("before", transition.before)):
                if clip_id not in known:
                    issues.append(
                        Issue(
                            code="unresolved_transition_clip",
                            message=(
                                f"transition references unknown {name} clip {clip_id!r} "
                                f"on track {track.id!r}"
                            ),
                            location=MediaLocation(track=track.id, pts=transition.span.start),
                        )
                    )
    return issues


def validate_coverage(timeline: Timeline, window: Span | None = None) -> list[Issue]:
    """Report gaps in each track's coverage and silent tracks."""
    issues: list[Issue] = []
    target = window if window is not None else timeline.declared_window()
    for track in timeline.tracks.values():
        if not track.clips:
            issues.append(
                Issue(
                    code="empty_track",
                    message=f"track {track.id!r} has no clips",
                    location=MediaLocation(track=track.id, pts=target.start),
                )
            )
            continue
        if not track.requires_full_coverage:
            continue
        coverage = track.coverage()
        for gap in gaps_within(coverage, target):
            issues.append(
                Issue(
                    code="coverage_gap",
                    message=(
                        f"track {track.id!r} has no media for {gap.duration}s "
                        f"starting at {gap.start}"
                    ),
                    location=MediaLocation(track=track.id, pts=gap.start),
                    context={"gap_end": str(gap.end)},
                )
            )
    return issues


def validate_window_bounds(timeline: Timeline, window: Span | None = None) -> list[Issue]:
    """Report clips that are not fully contained in an explicitly declared window."""
    if window is None:
        if timeline.window is None:
            return []
        window = timeline.window
    issues: list[Issue] = []
    for track in timeline.tracks.values():
        for clip in track.clips:
            if clip.start < window.start or clip.end > window.end:
                outside_start = max(0, window.start - clip.start)
                outside_end = max(0, clip.end - window.end)
                issues.append(
                    Issue(
                        code="clip_outside_window",
                        message=(
                            f"clip {clip.id!r} [{clip.start},{clip.end}) is not contained in "
                            f"window [{window.start},{window.end})"
                        ),
                        location=MediaLocation(
                            track=track.id,
                            pts=clip.start if outside_start else window.end,
                        ),
                        context={
                            "clip": clip.id,
                            "outside_start": str(outside_start),
                            "outside_end": str(outside_end),
                        },
                    )
                )
    return issues


def validate_source_bounds(
    timeline: Timeline, source_durations: Mapping[str, Fraction]
) -> list[Issue]:
    """Ensure each clip's source in-point plus duration fits the source file."""
    issues: list[Issue] = []
    for track in timeline.tracks.values():
        for clip in track.clips:
            duration = clip.span.duration
            available = source_durations.get(clip.source)
            if available is None:
                continue
            if clip.source_in + duration > available:
                issues.append(
                    Issue(
                        code="clip_beyond_source",
                        message=(
                            f"clip {clip.id!r} asks for {clip.source_in + duration}s "
                            f"of source {clip.source!r} but only {available}s exist"
                        ),
                        location=MediaLocation(track=track.id, pts=clip.start),
                        context={
                            "source": clip.source,
                            "source_in": str(clip.source_in),
                            "source_duration": str(available),
                        },
                    )
                )
    return issues


def validate_transitions(timeline: Timeline, window: Span | None = None) -> list[Issue]:
    """Check that each transition covers the boundary between its two clips.

    A transition must be contiguous with both clips (it may overlap them, which
    is the usual cross-fade) and must not extend outside the timeline window.
    """
    issues: list[Issue] = []
    target = window if window is not None else timeline.declared_window()
    for track in timeline.tracks.values():
        by_id = {c.id: c for c in track.clips}
        for transition in track.transitions:
            after = by_id.get(transition.after)
            before = by_id.get(transition.before)
            if after is None or before is None:
                continue  # already reported by validate_tracks
            if after.end != before.start:
                issues.append(
                    Issue(
                        code="transition_not_adjacent",
                        message=(
                            f"transition {after.id!r}->{before.id!r} expects adjacent clips "
                            f"but {after.id!r} ends at {after.end} and {before.id!r} starts at {before.start}"
                        ),
                        location=MediaLocation(track=track.id, pts=before.start),
                    )
                )
                continue
            cut = after.end
            if not (transition.span.start <= cut < transition.span.end):
                issues.append(
                    Issue(
                        code="transition_span_uncovered",
                        message=(
                            f"transition {after.id!r}->{before.id!r} at "
                            f"[{transition.span.start},{transition.span.end}) does not cover the cut "
                            f"at {cut}"
                        ),
                        location=MediaLocation(track=track.id, pts=cut),
                        context={
                            "cut": str(cut),
                            "actual": [str(transition.span.start), str(transition.span.end)],
                        },
                    )
                )
            covered_by_clips = Span(after.start, before.end)
            if transition.span.start < covered_by_clips.start:
                issues.append(
                    Issue(
                        code="transition_before_clips",
                        message=(
                            f"transition {after.id!r}->{before.id!r} starts at "
                            f"{transition.span.start} before the adjacent clips begin at "
                            f"{covered_by_clips.start}"
                        ),
                        location=MediaLocation(track=track.id, pts=transition.span.start),
                    )
                )
            if transition.span.end > covered_by_clips.end:
                issues.append(
                    Issue(
                        code="transition_after_clips",
                        message=(
                            f"transition {after.id!r}->{before.id!r} ends at "
                            f"{transition.span.end} after the adjacent clips end at "
                            f"{covered_by_clips.end}"
                        ),
                        location=MediaLocation(track=track.id, pts=covered_by_clips.end),
                    )
                )
            if transition.span.start < target.start or transition.span.end > target.end:
                issues.append(
                    Issue(
                        code="transition_outside_window",
                        message=(
                            f"transition {after.id!r}->{before.id!r} extends outside the window "
                            f"[{target.start},{target.end})"
                        ),
                        location=MediaLocation(track=track.id, pts=transition.span.start),
                    )
                )
    return issues


def validate_timeline(
    timeline: Timeline,
    source_durations: Mapping[str, Fraction] | None = None,
    window: Span | None = None,
) -> list[Issue]:
    """Run every structural check and return issues in deterministic order."""
    issues: list[Issue] = []
    issues.extend(validate_tracks(timeline))
    issues.extend(validate_coverage(timeline, window))
    issues.extend(validate_window_bounds(timeline, window))
    if source_durations:
        issues.extend(validate_source_bounds(timeline, source_durations))
    issues.extend(validate_transitions(timeline, window))
    return sorted(issues, key=lambda issue: _issue_key(issue))


def _issue_key(issue: Issue) -> tuple:
    pts = issue.location.pts
    return (
        1 if pts is None else 0,
        pts if pts is not None else Fraction(0),
        issue.location.track,
        issue.code,
        issue.message,
    )


def timeline_to_json(timeline: Timeline) -> dict[str, Any]:
    """Serialize a parsed timeline back to a canonical JSON structure."""
    def frac(value: Fraction) -> str:
        return str(value)

    tracks = []
    for track_id in sorted(timeline.tracks):
        track = timeline.tracks[track_id]
        tracks.append(
            {
                "id": track.id,
                "kind": track.kind,
                "coverage": track.coverage_mode,
                "clips": [
                    {
                        "id": c.id,
                        "start": frac(c.start),
                        "end": frac(c.end),
                        "source": c.source,
                        "source_in": frac(c.source_in),
                    }
                    for c in sorted(track.clips, key=lambda c: (c.start, c.id))
                ],
                "transitions": [
                    {
                        "after": t.after,
                        "before": t.before,
                        "start": frac(t.span.start),
                        "end": frac(t.span.end),
                    }
                    for t in sorted(track.transitions, key=lambda t: t.span.start)
                ],
            }
        )
    payload: dict[str, Any] = {"tracks": tracks}
    if timeline.window is not None:
        payload["window"] = {"start": frac(timeline.window.start), "end": frac(timeline.window.end)}
    return payload


def iter_clips(timeline: Timeline) -> Iterable[Clip]:
    for track in timeline.tracks.values():
        yield from track.clips
