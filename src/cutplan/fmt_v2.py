from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

from .errors import CutplanError
from .model import Clip, Manifest, Timeline, Track
from .timebase import parse_rate, round_half_up


SCHEMA = "cutplan/v2"
VERSION = 2


def _fail(message: str) -> None:
    raise CutplanError("E_SCHEMA_INVALID", message)


def _obj(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        _fail(f"{path} must be an object")
    return value


def _keys(value: dict[str, Any], allowed: set[str], path: str) -> None:
    unknown = sorted(set(value) - allowed)
    if unknown:
        _fail(f"{path} has unknown field(s): {', '.join(unknown)}")


def _string(value: Any, path: str) -> str:
    if not isinstance(value, str):
        _fail(f"{path} must be a string")
    return value


def _integer(value: Any, path: str, *, minimum: int | None = None) -> int:
    if not isinstance(value, int) or isinstance(value, bool):
        _fail(f"{path} must be an integer")
    if minimum is not None and value < minimum:
        _fail(f"{path} must be >= {minimum}")
    return value


def _ext(value: Any, path: str) -> dict[str, Any]:
    result = _obj(value, path)
    if any(not isinstance(key, str) for key in result):
        _fail(f"{path} keys must be strings")
    return dict(result)


def parse_v2(text: str) -> Timeline:
    text = text.lstrip("\ufeff")
    try:
        root = json.loads(text)
    except (TypeError, ValueError, json.JSONDecodeError) as exc:
        raise CutplanError("E_JSON_INVALID", "input is not valid JSON") from exc
    root = _obj(root, "root")
    _keys(root, {"schema", "version", "manifest", "tracks", "ext", "metadata"}, "root")
    if root.get("schema") != SCHEMA:
        raise CutplanError("E_SCHEMA_VERSION", f"schema must be {SCHEMA}")
    if root.get("version") != VERSION or isinstance(root.get("version"), bool):
        raise CutplanError("E_SCHEMA_VERSION", f"version must be {VERSION}")
    manifest_obj = _obj(root.get("manifest"), "manifest")
    _keys(manifest_obj, {"title", "rate", "declared_duration", "ext", "metadata"}, "manifest")
    for field_name in ("rate", "declared_duration"):
        if field_name not in manifest_obj:
            _fail(f"manifest.{field_name} is required")
    manifest = Manifest(
        title=_string(manifest_obj.get("title", ""), "manifest.title"),
        schema=SCHEMA,
        ext=_ext(manifest_obj.get("ext", {}), "manifest.ext"),
        metadata=_ext(manifest_obj.get("metadata", {}), "manifest.metadata"),
    )
    rate = parse_rate(manifest_obj["rate"])
    declared = _integer(manifest_obj["declared_duration"], "manifest.declared_duration", minimum=0)
    raw_tracks = root.get("tracks")
    if not isinstance(raw_tracks, list):
        _fail("tracks must be an array")
    tracks: list[Track] = []
    audio_rounded = 0
    for track_index, raw_track in enumerate(raw_tracks):
        path = f"tracks[{track_index}]"
        track_obj = _obj(raw_track, path)
        _keys(track_obj, {"id", "kind", "priority", "ext", "metadata", "clips"}, path)
        track = Track(
            id=_string(track_obj.get("id"), f"{path}.id"),
            kind=_string(track_obj.get("kind"), f"{path}.kind"),
            clips=[],
            priority=_integer(track_obj.get("priority", 0), f"{path}.priority"),
            ext=_ext(track_obj.get("ext", {}), f"{path}.ext"),
            metadata=_ext(track_obj.get("metadata", {}), f"{path}.metadata"),
        )
        raw_clips = track_obj.get("clips")
        if not isinstance(raw_clips, list):
            _fail(f"{path}.clips must be an array")
        audio_track = track.kind.strip().lower() in {"audio", "music", "voice", "音轨"}
        for clip_index, raw_clip in enumerate(raw_clips):
            clip_path = f"{path}.clips[{clip_index}]"
            clip_obj = _obj(raw_clip, clip_path)
            _keys(
                clip_obj,
                {"id", "start", "duration", "source_start", "priority", "role", "media_ref", "ext"},
                clip_path,
            )
            start = clip_obj.get("start")
            if isinstance(start, bool) or not isinstance(start, (int, float)):
                _fail(f"{clip_path}.start must be an integer or number")
            if not isinstance(start, int):
                if isinstance(start, float) and not math.isfinite(start):
                    _fail(f"{clip_path}.start must be finite")
                if not audio_track:
                    _fail(f"{clip_path}.start must be an integer frame")
                start = round_half_up(start)
                audio_rounded += 1
            track.clips.append(
                Clip(
                    id=_string(clip_obj.get("id"), f"{clip_path}.id"),
                    start=_integer(start, f"{clip_path}.start", minimum=0),
                    duration=_integer(clip_obj.get("duration"), f"{clip_path}.duration", minimum=0),
                    source_start=_integer(
                        clip_obj.get("source_start", 0), f"{clip_path}.source_start", minimum=0
                    ),
                    priority=_integer(clip_obj.get("priority", 0), f"{clip_path}.priority"),
                    role=_string(clip_obj.get("role", ""), f"{clip_path}.role"),
                    media_ref=_string(clip_obj.get("media_ref", ""), f"{clip_path}.media_ref"),
                    ext=_ext(clip_obj.get("ext", {}), f"{clip_path}.ext"),
                )
            )
        tracks.append(track)
    timeline = Timeline(
        rate=rate,
        declared_duration=declared,
        tracks=tracks,
        manifest=manifest,
        ext=_ext(root.get("ext", {}), "ext"),
        metadata=_ext(root.get("metadata", {}), "metadata"),
        source_format="v2",
    )
    if audio_rounded:
        timeline.metadata["_audio_start_rounded"] = audio_rounded
    return timeline


def to_dict(timeline: Timeline) -> dict[str, Any]:
    return {
        "schema": SCHEMA,
        "version": VERSION,
        "manifest": {
            "title": timeline.manifest.title,
            "rate": str(timeline.rate),
            "declared_duration": timeline.declared_duration,
            "ext": timeline.manifest.ext,
            "metadata": timeline.manifest.metadata,
        },
        "tracks": [
            {
                "id": track.id,
                "kind": track.kind,
                "priority": track.priority,
                "ext": track.ext,
                "metadata": track.metadata,
                "clips": [
                    {
                        "id": clip.id,
                        "start": clip.start,
                        "duration": clip.duration,
                        "source_start": clip.source_start,
                        "priority": clip.priority,
                        "role": clip.role,
                        "media_ref": clip.media_ref,
                        "ext": clip.ext,
                    }
                    for clip in sorted(track.clips, key=lambda item: (item.start, item.id))
                ],
            }
            for track in timeline.tracks
        ],
        "ext": timeline.ext,
        "metadata": {key: value for key, value in timeline.metadata.items() if not key.startswith("_")},
    }


def dumps_v2(timeline: Timeline) -> str:
    return json.dumps(to_dict(timeline), ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def write_v2(timeline: Timeline, path: str | Path) -> None:
    Path(path).write_text(dumps_v2(timeline), encoding="utf-8")


def read_v2(path: str | Path) -> Timeline:
    return parse_v2(Path(path).read_text(encoding="utf-8"))


parse = parse_v2
dumps = dumps_v2
read = read_v2
write = write_v2
