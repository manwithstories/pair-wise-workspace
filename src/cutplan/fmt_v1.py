from __future__ import annotations

from decimal import Decimal, InvalidOperation
import json
from pathlib import Path
from typing import Any

from .errors import CutplanError
from .model import Clip, Manifest, Timeline, Track
from .timebase import parse_rate, round_half_up


_AUDIO_KINDS = {"audio", "music", "voice", "音轨"}


def _json_text(value: Any) -> str:
    text = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return text.replace("|", "\\u007c")


def _loads(text: str, *, line: int, field: str) -> Any:
    try:
        return json.loads(text)
    except (TypeError, ValueError, json.JSONDecodeError) as exc:
        raise CutplanError("E_V1_FIELD", f"line {line} {field} must be valid JSON") from exc


def _number(text: str, *, line: int, field: str) -> int | float:
    try:
        value = Decimal(text)
    except (InvalidOperation, ValueError) as exc:
        raise CutplanError("E_V1_FIELD", f"line {line} {field} is not a number") from exc
    if not value.is_finite():
        raise CutplanError("E_V1_FIELD", f"line {line} {field} must be finite")
    if value == value.to_integral_value():
        return int(value)
    return float(value)


def _integer(text: str, *, line: int, field: str) -> int:
    value = _number(text, line=line, field=field)
    if not isinstance(value, int):
        raise CutplanError("E_V1_FIELD", f"line {line} {field} must be an integer")
    return value


def parse_v1(text: str) -> Timeline:
    text = text.lstrip("\ufeff")
    lines = text.splitlines()
    if not lines or lines[0].strip() != "CUTPLAN|1":
        raise CutplanError("E_V1_HEADER", "missing CUTPLAN|1 header")
    tracks: dict[str, Track] = {}
    track_order: list[str] = []
    manifest: Manifest | None = None
    rate = None
    declared_duration: int | None = None
    timeline_ext: dict[str, Any] = {}
    metadata: dict[str, Any] = {}
    audio_rounded = 0
    ended = False
    for line_number, raw in enumerate(lines[1:], start=2):
        line = raw.strip()
        if not line:
            continue
        if line == "END":
            ended = True
            break
        parts = line.split("|")
        kind = parts[0]
        if kind == "MANIFEST":
            if len(parts) != 5:
                raise CutplanError("E_V1_FIELD", f"line {line_number} malformed MANIFEST")
            manifest = Manifest(
                title=_loads(parts[1], line=line_number, field="title"),
                schema=_loads(parts[4], line=line_number, field="schema"),
                ext={},
            )
            rate = parse_rate(parts[2])
            declared_duration = _integer(parts[3], line=line_number, field="duration")
        elif kind == "TRACK":
            if len(parts) != 4:
                raise CutplanError("E_V1_FIELD", f"line {line_number} malformed TRACK")
            track_id = _loads(parts[1], line=line_number, field="track id")
            track_kind = _loads(parts[2], line=line_number, field="track kind")
            if track_id in tracks:
                raise CutplanError("E_DUPLICATE_TRACK_ID", f"duplicate track id: {track_id}")
            tracks[track_id] = Track(
                track_id,
                track_kind,
                [],
                priority=_integer(parts[3], line=line_number, field="priority"),
            )
            track_order.append(track_id)
        elif kind == "CLIP":
            if len(parts) not in {9, 10}:
                raise CutplanError("E_V1_FIELD", f"line {line_number} malformed CLIP")
            track_id = _loads(parts[1], line=line_number, field="track id")
            clip_id = _loads(parts[2], line=line_number, field="clip id")
            if track_id not in tracks:
                raise CutplanError("E_TRACK_NOT_FOUND", f"line {line_number} unknown track {track_id}")
            track = tracks[track_id]
            start = _number(parts[3], line=line_number, field="start")
            if not isinstance(start, int):
                if track.kind.strip().lower() not in _AUDIO_KINDS:
                    raise CutplanError("E_V1_FIELD", f"line {line_number} start must be an integer frame")
                start = round_half_up(start)
                audio_rounded += 1
            track.clips.append(
                Clip(
                    id=clip_id,
                    start=start,
                    duration=_integer(parts[4], line=line_number, field="duration"),
                    source_start=_integer(parts[5], line=line_number, field="source_start"),
                    priority=_integer(parts[6], line=line_number, field="priority"),
                    role=_loads(parts[7], line=line_number, field="role"),
                    media_ref=_loads(parts[8], line=line_number, field="media_ref"),
                    ext=_loads(parts[9], line=line_number, field="clip ext") if len(parts) == 10 else {},
                )
            )
        elif kind in {"EXT", "META"}:
            _parse_attachment(parts, kind, tracks, manifest, timeline_ext if kind == "EXT" else metadata, line=line_number)
        else:
            raise CutplanError("E_V1_RECORD", f"line {line_number} unknown record {kind!r}")
    if not ended:
        raise CutplanError("E_V1_END", "missing END record")
    if manifest is None or rate is None or declared_duration is None:
        raise CutplanError("E_V1_MANIFEST", "missing MANIFEST record")
    timeline = Timeline(
        rate=rate,
        declared_duration=declared_duration,
        tracks=[tracks[track_id] for track_id in track_order],
        manifest=manifest,
        ext=timeline_ext,
        metadata=metadata,
        source_format="v1",
    )
    if audio_rounded:
        timeline.metadata["_audio_start_rounded"] = audio_rounded
    return timeline


def _parse_attachment(
    parts: list[str],
    kind: str,
    tracks: dict[str, Track],
    manifest: Manifest | None,
    root: dict[str, Any],
    *,
    line: int,
) -> None:
    if len(parts) < 4:
        raise CutplanError("E_V1_FIELD", f"line {line} malformed {kind}")
    scope = parts[1]
    key = _loads(parts[-2], line=line, field="attachment key")
    value = _loads(parts[-1], line=line, field="attachment value")
    if not isinstance(key, str):
        raise CutplanError("E_V1_FIELD", f"line {line} attachment key must be a string")
    target: dict[str, Any]
    if scope == "timeline":
        target = root
    elif scope == "manifest":
        if manifest is None:
            raise CutplanError("E_V1_FIELD", f"line {line} MANIFEST must precede attachment")
        target = manifest.ext if kind == "EXT" else manifest.metadata
    elif scope == "track":
        if len(parts) != 5:
            raise CutplanError("E_V1_FIELD", f"line {line} malformed track {kind}")
        track_id = _loads(parts[2], line=line, field="track id")
        if track_id not in tracks:
            raise CutplanError("E_TRACK_NOT_FOUND", f"line {line} unknown track {track_id}")
        track = tracks[track_id]
        target = track.ext if kind == "EXT" else track.metadata
    elif scope == "clip":
        if kind != "EXT" or len(parts) != 6:
            raise CutplanError("E_V1_FIELD", f"line {line} malformed clip {kind}")
        track_id = _loads(parts[2], line=line, field="track id")
        clip_id = _loads(parts[3], line=line, field="clip id")
        if track_id not in tracks:
            raise CutplanError("E_TRACK_NOT_FOUND", f"line {line} unknown track {track_id}")
        target = tracks[track_id].clip(clip_id).ext
    else:
        raise CutplanError("E_V1_FIELD", f"line {line} unknown {kind} scope {scope!r}")
    target[key] = value


def dumps_v1(timeline: Timeline) -> str:
    lines = [
        "CUTPLAN|1",
        "|".join(
            [
                "MANIFEST",
                _json_text(timeline.manifest.title),
                str(timeline.rate),
                str(timeline.declared_duration),
                _json_text(timeline.manifest.schema or "cutplan/v1"),
            ]
        ),
    ]
    lines.extend(_attachment_lines("EXT", "timeline", timeline.ext))
    lines.extend(_attachment_lines("EXT", "manifest", timeline.manifest.ext))
    lines.extend(_attachment_lines("META", "timeline", timeline.metadata))
    lines.extend(_attachment_lines("META", "manifest", timeline.manifest.metadata))
    for track in timeline.tracks:
        lines.append("|".join(["TRACK", _json_text(track.id), _json_text(track.kind), str(track.priority)]))
        lines.extend(_attachment_lines("EXT", "track", track.ext, track.id))
        lines.extend(_attachment_lines("META", "track", track.metadata, track.id))
        for clip in sorted(track.clips, key=lambda item: (item.start, item.id)):
            lines.append(
                "|".join(
                    [
                        "CLIP",
                        _json_text(track.id),
                        _json_text(clip.id),
                        str(clip.start),
                        str(clip.duration),
                        str(clip.source_start),
                        str(clip.priority),
                        _json_text(clip.role),
                        _json_text(clip.media_ref),
                        _json_text(clip.ext),
                    ]
                )
            )
        for clip in sorted(track.clips, key=lambda item: (item.start, item.id)):
            lines.extend(_attachment_lines("EXT", "clip", clip.ext, track.id, clip.id))
    lines.append("END")
    return "\n".join(lines) + "\n"


def _attachment_lines(kind: str, scope: str, values: dict[str, Any], *parents: str) -> list[str]:
    lines: list[str] = []
    for key, value in sorted(values.items()):
        if scope == "timeline" and key.startswith("_"):
            continue
        fields = [kind, scope, *(_json_text(parent) for parent in parents), _json_text(key), _json_text(value)]
        lines.append("|".join(fields))
    return lines


def write_v1(timeline: Timeline, path: str | Path) -> None:
    Path(path).write_text(dumps_v1(timeline), encoding="utf-8")


def read_v1(path: str | Path) -> Timeline:
    return parse_v1(Path(path).read_text(encoding="utf-8"))


parse = parse_v1
dumps = dumps_v1
read = read_v1
write = write_v1
