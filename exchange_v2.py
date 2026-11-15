"""Reader/writer for the v2 rational interval tree."""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path
from typing import Any

from exchange_v1 import write_json_atomic
from model import (
    Clip, ModelError, Timeline, Track, make_track, parse_rational, rational_json,
    timeline_sort_key,
)

FORMAT_NAME = "offline-timeline-v2"
KNOWN_POLICIES = {
    "trim-left", "trim-right", "split", "upper-covers", "protect-locked", "reject",
}


class V2Error(ValueError):
    def __init__(self, code: str, message: str, *, exit_code: int = 1, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.exit_code = exit_code
        self.details = details


def _json_rational(value: Any, field_name: str, fallback: Fraction | None = None) -> Fraction:
    try:
        return parse_rational(value, field_name=field_name)
    except ModelError as exc:
        raise V2Error(exc.code, exc.message, field=field_name) from exc


def _read_document(path: Path) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise V2Error("invalid_json", f"cannot read v2 JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise V2Error("invalid_v2_document", "v2 document must be an object")
    return data


def _validate_policy(value: Any, location: str) -> str:
    policy = str(value)
    if policy not in KNOWN_POLICIES:
        raise V2Error("unknown_policy", f"unknown policy {policy!r} at {location}", policy=policy)
    return policy


def read_v2(path: str | Path, *, master_rate: Fraction | None = None,
            master_frames: int | None = None) -> Timeline:
    path = Path(path)
    data = _read_document(path)
    version = data.get("version")
    if version != 2:
        raise V2Error("unsupported_version", f"unsupported v2 version {version!r}", exit_code=2)
    if data.get("format") not in {None, FORMAT_NAME, "timeline-exchange-v2"}:
        raise V2Error("unsupported_format", f"unsupported v2 format {data.get('format')!r}", exit_code=2)
    project_rate = _json_rational(data.get("rate"), "rate")
    target_rate = parse_rational(master_rate, field_name="master_rate") if master_rate is not None else project_rate
    raw_tracks = data.get("tracks", [])
    if not isinstance(raw_tracks, list):
        raise V2Error("invalid_tracks", "v2 tracks must be a list")
    policy = _validate_policy(data.get("policy", "protect-locked"), "project")
    clips_by_track: dict[str, list[Clip]] = {}
    track_meta: dict[str, dict[str, Any]] = {}
    losses: list[dict[str, Any]] = []
    seen_ids: set[str] = set()

    def add_node(node: dict[str, Any], parent: str | None, inherited_layer: int,
                 inherited_priority: int, inherited_locked: bool) -> None:
        if not isinstance(node, dict):
            raise V2Error("invalid_track", "each v2 track must be an object")
        node_id = str(node.get("id") or node.get("name") or "track")
        name = f"{parent}/{node_id}" if parent else node_id
        try:
            layer = int(node.get("layer", inherited_layer))
            priority = int(node.get("priority", inherited_priority))
        except (TypeError, ValueError) as exc:
            raise V2Error("invalid_track_attribute", f"bad layer/priority on {name}") from exc
        locked = bool(node.get("locked", inherited_locked))
        track_meta[name] = {"layer": layer, "priority": priority}
        clips_by_track.setdefault(name, [])
        raw_clips = node.get("clips", [])
        if not isinstance(raw_clips, list):
            raise V2Error("invalid_clips", f"clips on {name} must be a list")
        for index, raw in enumerate(raw_clips):
            if not isinstance(raw, dict):
                raise V2Error("invalid_clip", f"clip {index} on {name} must be an object")
            clip_id = str(raw.get("id") or f"{name}_{len(clips_by_track[name]):06d}")
            if clip_id in seen_ids:
                raise V2Error("duplicate_clip_id", f"duplicate clip id {clip_id!r}")
            seen_ids.add(clip_id)
            start = _json_rational(raw.get("start"), f"{clip_id}.start")
            duration = _json_rational(raw.get("duration"), f"{clip_id}.duration")
            if duration < 0:
                raise V2Error("negative_duration", f"clip {clip_id} has negative duration")
            clip_policy = raw.get("policy")
            if clip_policy is not None:
                clip_policy = _validate_policy(clip_policy, f"clip {clip_id}")
            kind = str(raw.get("kind", "gap" if raw.get("gap") else "clip"))
            try:
                target_start = start / project_rate * target_rate
                target_duration = duration / project_rate * target_rate
            except ZeroDivisionError as exc:
                raise V2Error("invalid_rate", "project rate is zero") from exc
            if target_start.denominator != 1 or target_duration.denominator != 1:
                losses.append({
                    "clip": clip_id, "code": "subframe_boundary",
                    "start": rational_json(target_start), "duration": rational_json(target_duration),
                    "project_rate": rational_json(project_rate), "master_rate": rational_json(target_rate),
                })
            source_rate = raw.get("source_rate")
            source_start = raw.get("source_start")
            clip = Clip(
                id=clip_id, track=name, src=str(raw.get("src", "gap" if kind == "gap" else "unknown")),
                start=target_start, duration=target_duration, layer=int(raw.get("layer", layer)),
                priority=int(raw.get("priority", priority)), locked=bool(raw.get("locked", locked)),
                kind=kind, policy=clip_policy, action="unchanged",
                source_start=_json_rational(source_start, f"{clip_id}.source_start") if source_start is not None else None,
                source_rate=_json_rational(source_rate, f"{clip_id}.source_rate") if source_rate is not None else None,
                metadata=(("source_format", 2),),
            )
            clips_by_track[name].append(clip)
        children = node.get("children", node.get("tracks", []))
        if children and not isinstance(children, list):
            raise V2Error("invalid_tracks", f"children on {name} must be a list")
        for child in children or []:
            add_node(child, name, layer, priority, locked)

    for raw_track in raw_tracks:
        add_node(raw_track, None, 0, 0, False)
    tracks: list[Track] = []
    for name, clips in clips_by_track.items():
        if not clips:
            continue
        meta = track_meta[name]
        tracks.append(make_track(name, clips, layer=meta["layer"], priority=meta["priority"]))
    tracks.sort(key=timeline_sort_key)
    resolved_master = master_frames
    if resolved_master is None:
        resolved_master = int(data.get("master_frames", 0))
    if resolved_master == 0 and clips_by_track:
        max_end = max((clip.end for clips in clips_by_track.values() for clip in clips), default=Fraction(0))
        resolved_master = max(0, max_end.__ceil__())
    return Timeline(
        name=str(data.get("name") or path.stem), rate=target_rate, master_frames=int(resolved_master),
        tracks=tuple(tracks), policy=policy, source_format=2, precision_losses=tuple(losses),
        metadata=(("source_path", path.name),),
    )


def v2_to_data(timeline: Timeline) -> dict[str, Any]:
    tracks: list[dict[str, Any]] = []
    for track in sorted(timeline.tracks, key=timeline_sort_key):
        clips: list[dict[str, Any]] = []
        for clip in sorted(track.clips, key=lambda item: (item.start, item.id)):
            payload: dict[str, Any] = {
                "id": clip.id, "src": clip.src, "start": rational_json(clip.start),
                "duration": rational_json(clip.duration), "layer": clip.layer,
                "priority": clip.priority, "locked": clip.locked, "kind": clip.kind,
            }
            if clip.policy is not None:
                payload["policy"] = clip.policy
            if clip.source_start is not None:
                payload["source_start"] = rational_json(clip.source_start)
            if clip.source_rate is not None:
                payload["source_rate"] = rational_json(clip.source_rate)
            clips.append(payload)
        tracks.append({"id": track.name, "layer": track.layer, "priority": track.priority,
                       "clips": clips})
    return {
        "format": FORMAT_NAME, "version": 2, "name": timeline.name,
        "rate": rational_json(timeline.rate), "master_frames": timeline.master_frames,
        "policy": timeline.policy, "tracks": tracks,
    }


def write_v2(timeline: Timeline, path: str | Path) -> None:
    write_json_atomic(Path(path), v2_to_data(timeline))
