"""Parser for format v1: flat items with integer frame positions."""
from __future__ import annotations

from fractions import Fraction

from ..model import AUDIO, CLIP, GAP, TRANSITION, VIDEO, Clip, TimeRange, Timeline, Track
from .registry import (
    FormatError,
    ParseReport,
    as_bool,
    as_dict,
    as_int,
    as_list,
    as_str,
    opt,
    req,
)

_ITEM_KINDS = (CLIP, GAP, TRANSITION)
_TRACK_KINDS = (VIDEO, AUDIO)


def parse_fps(value, path: str) -> Fraction:
    if isinstance(value, bool):
        raise FormatError(path, "must be an integer or \"num/den\" string")
    if isinstance(value, int):
        fps = Fraction(value)
    elif isinstance(value, str):
        try:
            fps = Fraction(value)
        except (ValueError, ZeroDivisionError) as exc:
            raise FormatError(path, f"invalid rational string: {value!r}") from exc
    else:
        raise FormatError(path, "must be an integer or \"num/den\" string")
    if fps <= 0:
        raise FormatError(path, "fps must be positive")
    return fps


def _parse_item(raw, path: str) -> Clip:
    raw = as_dict(raw, path)
    item_id = as_str(req(raw, "id", path), f"{path}.id")
    kind = as_str(req(raw, "type", path), f"{path}.type")
    if kind not in _ITEM_KINDS:
        raise FormatError(f"{path}.type", f"must be one of {_ITEM_KINDS}")
    start = as_int(req(raw, "start", path), f"{path}.start")
    duration = as_int(req(raw, "duration", path), f"{path}.duration")
    clip = Clip(
        id=item_id,
        kind=kind,
        range=TimeRange(start, duration),
        enabled=as_bool(opt(raw, "enabled", True), f"{path}.enabled"),
        muted=as_bool(opt(raw, "muted", False), f"{path}.muted"),
    )
    if kind == TRANSITION:
        clip.left = as_str(req(raw, "left", path), f"{path}.left")
        clip.right = as_str(req(raw, "right", path), f"{path}.right")
    return clip


def parse(data: dict, source: str) -> tuple[Timeline, ParseReport]:
    name = as_str(opt(data, "name", "timeline"), "name")
    project = as_dict(req(data, "project", ""), "project")
    fps = parse_fps(req(project, "fps", "project"), "project.fps")
    tracks_raw = as_list(req(data, "tracks", ""), "tracks")
    tracks: list[Track] = []
    seen_ids: set[str] = set()
    for ti, traw in enumerate(tracks_raw):
        tpath = f"tracks[{ti}]"
        traw = as_dict(traw, tpath)
        tname = as_str(req(traw, "name", tpath), f"{tpath}.name")
        tkind = as_str(opt(traw, "kind", VIDEO), f"{tpath}.kind")
        if tkind not in _TRACK_KINDS:
            raise FormatError(f"{tpath}.kind", f"must be one of {_TRACK_KINDS}")
        items_raw = as_list(req(traw, "items", tpath), f"{tpath}.items")
        items = []
        for ii, iraw in enumerate(items_raw):
            clip = _parse_item(iraw, f"{tpath}.items[{ii}]")
            if clip.id in seen_ids:
                raise FormatError(f"{tpath}.items[{ii}].id", f"duplicate id {clip.id!r}")
            seen_ids.add(clip.id)
            items.append(clip)
        tracks.append(Track(name=tname, kind=tkind, items=items))
    timeline = Timeline(name=name, fps=fps, tracks=tracks)
    return timeline, ParseReport(format_version=1, source=source)


def dump(timeline: Timeline) -> dict:
    """Serialize a model back to the v1 flat integer-frame format."""
    fps = timeline.fps
    fps_json: object = (fps.numerator if fps.denominator == 1
                        else f"{fps.numerator}/{fps.denominator}")
    tracks = []
    for tr in timeline.tracks:
        items = []
        for c in sorted(tr.items, key=lambda c: (c.range.start, c.id)):
            item = {"id": c.id, "type": c.kind, "start": c.range.start,
                    "duration": c.range.duration,
                    "enabled": c.enabled, "muted": c.muted}
            if c.kind == TRANSITION:
                item["left"], item["right"] = c.left, c.right
            items.append(item)
        tracks.append({"name": tr.name, "kind": tr.kind, "items": items})
    return {"version": 1, "name": timeline.name,
            "project": {"fps": fps_json}, "tracks": tracks}
