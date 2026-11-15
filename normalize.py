"""Normalize v1/v2 inputs into the exact model and rebuild models from reports."""

from __future__ import annotations

import json
from dataclasses import replace
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from exchange_v1 import V1Error, read_v1
from exchange_v2 import KNOWN_POLICIES, V2Error, read_v2
from model import (
    Clip, ModelError, Timeline, Track, make_track, parse_rational, timeline_sort_key,
)


class NormalizeError(ValueError):
    def __init__(self, code: str, message: str, *, exit_code: int = 1, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.exit_code = exit_code
        self.details = details


def _policy(value: Any) -> str:
    policy = str(value)
    if policy not in KNOWN_POLICIES:
        raise NormalizeError("unknown_policy", f"unknown policy {policy!r}", policy=policy)
    return policy


def _tag_sources(timeline: Timeline, path: Path, source_format: int) -> Timeline:
    tracks = []
    for track in timeline.tracks:
        tagged = [replace(clip, metadata=tuple(sorted((*clip.metadata, ("source_path", path.name)))))
                  for clip in track.clips]
        tracks.append(replace(track, clips=tuple(tagged)))
    metadata = tuple(sorted((*timeline.metadata, ("source_format", source_format))))
    return replace(timeline, tracks=tuple(tracks), metadata=metadata)


def normalize_v1(path: str | Path, *, master_rate: Fraction | None = None,
                 master_frames: int | None = None, policy: str | None = None) -> Timeline:
    path = Path(path)
    try:
        timeline = read_v1(path, master_rate=master_rate, master_frames=master_frames)
    except V1Error:
        raise
    except ModelError as exc:
        raise NormalizeError(exc.code, exc.message) from exc
    timeline = _tag_sources(timeline, path, 1)
    if policy is not None:
        timeline = replace(timeline, policy=_policy(policy))
    return timeline


def normalize_v2(path: str | Path, *, master_rate: Fraction | None = None,
                 master_frames: int | None = None, policy: str | None = None) -> Timeline:
    path = Path(path)
    try:
        timeline = read_v2(path, master_rate=master_rate, master_frames=master_frames)
    except V2Error:
        raise
    except ModelError as exc:
        raise NormalizeError(exc.code, exc.message) from exc
    timeline = _tag_sources(timeline, path, 2)
    if policy is not None:
        timeline = replace(timeline, policy=_policy(policy))
    return timeline


def load_exchange(path: str | Path, *, master_rate: Fraction | None = None,
                  master_frames: int | None = None, policy: str | None = None) -> Timeline:
    path = Path(path)
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise NormalizeError("invalid_json", f"cannot read exchange JSON: {exc}") from exc
    if not isinstance(raw, dict):
        raise NormalizeError("invalid_document", "exchange document must be an object")
    version = raw.get("version")
    if version == 1:
        return normalize_v1(path, master_rate=master_rate, master_frames=master_frames, policy=policy)
    if version == 2:
        return normalize_v2(path, master_rate=master_rate, master_frames=master_frames, policy=policy)
    raise NormalizeError("unsupported_version", f"unsupported exchange version {version!r}", exit_code=2)


def model_signature(timeline: Timeline) -> tuple[Any, ...]:
    """Comparable model content, excluding provenance and render-time action labels."""
    tracks = []
    for track in sorted(timeline.tracks, key=timeline_sort_key):
        clips = tuple(sorted(
            ((clip.id, clip.track, clip.src, clip.start, clip.duration, clip.layer,
              clip.priority, clip.locked, clip.kind, clip.policy,
              clip.source_start, clip.source_rate) for clip in track.clips),
            key=lambda item: (item[3], item[0]),
        ))
        tracks.append((track.name, track.layer, track.priority, clips))
    return (timeline.name, timeline.rate, timeline.master_frames, timeline.policy, tuple(tracks))


def clip_from_interval_dict(raw: dict[str, Any]) -> Clip:
    try:
        start = parse_rational(raw["start"], field_name="start")
        duration = parse_rational(raw["duration"], field_name="duration")
        layer = int(raw.get("layer", 0))
        priority = int(raw.get("priority", 0))
        source_start = raw.get("source_start")
        source_rate = raw.get("source_rate")
    except (KeyError, ValueError, ModelError) as exc:
        raise NormalizeError("invalid_report_interval", f"bad interval {raw.get('id')!r}: {exc}") from exc
    return Clip(
        id=str(raw.get("id") or f"clip_{start}"), track=str(raw.get("track") or "track"),
        src=str(raw.get("src") or "unknown"), start=start, duration=duration,
        layer=layer, priority=priority, locked=bool(raw.get("locked", False)),
        kind=str(raw.get("kind", "clip")), policy=raw.get("policy"),
        action=str(raw.get("action", "unchanged")),
        source_start=parse_rational(source_start, field_name="source_start") if source_start is not None else None,
        source_rate=parse_rational(source_rate, field_name="source_rate") if source_rate is not None else None,
    )


def timeline_from_report(report: dict[str, Any]) -> Timeline:
    """Rebuild the resolved timeline from a report, not from exchange files."""
    try:
        rate = parse_rational(report["rate"], field_name="rate")
        master_frames = int(report["master_frames"])
    except (KeyError, TypeError, ValueError, ModelError) as exc:
        raise NormalizeError("invalid_report", f"report lacks rate/master_frames: {exc}") from exc
    raw_intervals = report.get("resolved_intervals", [])
    raw_gaps = report.get("gaps", [])
    if not isinstance(raw_intervals, list) or not isinstance(raw_gaps, list):
        raise NormalizeError("invalid_report", "resolved_intervals and gaps must be lists")
    grouped: dict[str, list[Clip]] = {}
    for raw in list(raw_intervals) + list(raw_gaps):
        clip = clip_from_interval_dict(raw)
        grouped.setdefault(clip.track, []).append(clip)
    specs = {str(spec.get("name")): spec for spec in report.get("track_specs", [])
             if isinstance(spec, dict) and spec.get("name") is not None}
    tracks: list[Track] = []
    for name, clips in grouped.items():
        spec = specs.get(name, {})
        tracks.append(make_track(
            name, clips, layer=int(spec.get("layer", clips[0].layer)),
            priority=int(spec.get("priority", clips[0].priority)),
            metadata={"rebuilt_from_report": True},
        ))
    tracks.sort(key=timeline_sort_key)
    return Timeline(
        name=str(report.get("project", "report")), rate=rate, master_frames=master_frames,
        tracks=tuple(tracks), policy=str(report.get("policy", "protect-locked")),
        source_format=3, metadata=(("rebuilt_from_report", True),),
    )
