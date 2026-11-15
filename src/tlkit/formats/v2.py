"""Parser for format v2: nested items with rational (rate/value) times.

Rational times are converted to the integer project frame grid with
``Fraction`` arithmetic only; any rounding residual is recorded in the
parse report. The nested structure is additionally validated by building
an OpenTimelineIO timeline from it.
"""
from __future__ import annotations

from fractions import Fraction

from ..model import (
    AUDIO, CLIP, GAP, TRANSITION, VIDEO,
    Clip, TimeRange, Timeline, Track, rational_to_frames,
)
from .registry import (
    FormatError, ParseReport,
    as_bool, as_dict, as_int, as_list, as_str, opt, req,
)
from .v1 import _ITEM_KINDS, _TRACK_KINDS


def _rational(raw, path: str) -> tuple[int, int]:
    raw = as_dict(raw, path)
    rate = as_int(req(raw, "rate", path), f"{path}.rate")
    value = as_int(req(raw, "value", path), f"{path}.value")
    if rate <= 0:
        raise FormatError(f"{path}.rate", "must be a positive integer")
    return value, rate


def _to_frames(raw, path: str, fps: Fraction, residuals: list[dict]) -> int:
    value, rate = _rational(raw, path)
    frame, remainder = rational_to_frames(value, rate, fps)
    if remainder != 0:
        residuals.append({
            "path": path,
            "exact_frames": f"{remainder.numerator}/{remainder.denominator}",
            "rounded_frame": frame,
        })
    return frame


def _parse_item(raw, path: str, fps: Fraction, residuals: list[dict]) -> Clip:
    raw = as_dict(raw, path)
    item_id = as_str(req(raw, "id", path), f"{path}.id")
    kind = as_str(req(raw, "type", path), f"{path}.type")
    if kind not in _ITEM_KINDS:
        raise FormatError(f"{path}.type", f"must be one of {_ITEM_KINDS}")
    rng = as_dict(req(raw, "range", path), f"{path}.range")
    start = _to_frames(req(rng, "start", f"{path}.range"),
                       f"{path}.range.start", fps, residuals)
    duration = _to_frames(req(rng, "duration", f"{path}.range"),
                          f"{path}.range.duration", fps, residuals)
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


def _otio_validate(data: dict) -> None:
    """Rebuild the nested structure as an OTIO timeline for validation.

    Durations are clamped to >= 0 so that intentionally defective fixtures
    (zero-length clips) still parse; structural errors raise FormatError.
    """
    import opentimelineio as otio

    def rt(node):
        return otio.opentime.RationalTime(node["value"], node["rate"])

    try:
        tl = otio.schema.Timeline(name=str(data.get("name", "timeline")))
        for tr in data["tracks"]:
            kind = (otio.schema.TrackKind.Video
                    if tr.get("kind", VIDEO) == VIDEO else otio.schema.TrackKind.Audio)
            track = otio.schema.Track(name=str(tr.get("name", "track")), kind=kind)
            for item in tr["items"]:
                rng = item["range"]
                duration = dict(rng["duration"])
                if duration["value"] < 0:
                    duration["value"] = 0
                trange = otio.opentime.TimeRange(rt(rng["start"]), rt(duration))
                if item["type"] == GAP:
                    track.append(otio.schema.Gap(source_range=trange))
                elif item["type"] == TRANSITION:
                    track.append(otio.schema.Transition(name=str(item.get("id", "t"))))
                else:
                    track.append(otio.schema.Clip(name=str(item.get("id", "c")),
                                                  source_range=trange))
            tl.tracks.append(track)
        otio.adapters.write_to_string(tl)
    except FormatError:
        raise
    except Exception as exc:  # OTIO raises many exception types
        raise FormatError("$", f"OTIO validation failed: {exc}") from exc


def parse(data: dict, source: str) -> tuple[Timeline, ParseReport]:
    name = as_str(opt(data, "name", "timeline"), "name")
    project = as_dict(req(data, "project", ""), "project")
    fps_value, fps_rate = _rational(req(project, "fps", "project"), "project.fps")
    fps = Fraction(fps_value, fps_rate)
    if fps <= 0:
        raise FormatError("project.fps", "fps must be positive")
    residuals: list[dict] = []
    tracks: list[Track] = []
    seen_ids: set[str] = set()
    for ti, traw in enumerate(as_list(req(data, "tracks", ""), "tracks")):
        tpath = f"tracks[{ti}]"
        traw = as_dict(traw, tpath)
        tname = as_str(req(traw, "name", tpath), f"{tpath}.name")
        tkind = as_str(opt(traw, "kind", VIDEO), f"{tpath}.kind")
        if tkind not in _TRACK_KINDS:
            raise FormatError(f"{tpath}.kind", f"must be one of {_TRACK_KINDS}")
        items = []
        for ii, iraw in enumerate(as_list(req(traw, "items", tpath), f"{tpath}.items")):
            clip = _parse_item(iraw, f"{tpath}.items[{ii}]", fps, residuals)
            if clip.id in seen_ids:
                raise FormatError(f"{tpath}.items[{ii}].id", f"duplicate id {clip.id!r}")
            seen_ids.add(clip.id)
            items.append(clip)
        tracks.append(Track(name=tname, kind=tkind, items=items))
    _otio_validate(data)
    timeline = Timeline(name=name, fps=fps, tracks=tracks)
    return timeline, ParseReport(format_version=2, source=source, rounding=residuals)
