"""Reader/writer for the order-sensitive v1 timecode event stream."""

from __future__ import annotations

import json
import os
import tempfile
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from model import (
    Clip, ModelError, Timeline, Track, make_track, parse_rational, rational_json,
    timeline_sort_key,
)

FORMAT_NAME = "offline-timeline-v1"
VEVENT_OPERATIONS = {"in", "out"}


class V1Error(ValueError):
    def __init__(self, code: str, message: str, *, exit_code: int = 1, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.exit_code = exit_code
        self.details = details


def _read_json(path: Path) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise V1Error("invalid_json", f"cannot read v1 JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise V1Error("invalid_v1_document", "v1 document must be an object")
    return data


def parse_timecode(value: str, rate: Fraction) -> Fraction:
    """Parse non-drop HH:MM:SS:FF using the rate's nominal integer base."""
    if not isinstance(value, str):
        raise V1Error("bad_timecode", "timecode must be a string")
    parts = value.strip().split(":")
    if len(parts) != 4 or any(not part.isdigit() for part in parts):
        raise V1Error("bad_timecode", f"invalid timecode {value!r}")
    hours, minutes, seconds, frames = (int(part) for part in parts)
    nominal = int(rate) if rate.denominator == 1 else (rate.numerator + rate.denominator - 1) // rate.denominator
    if minutes >= 60 or seconds >= 60 or frames >= nominal:
        raise V1Error("bad_timecode", f"timecode field out of range {value!r}")
    return Fraction((hours * 3600 + minutes * 60 + seconds) * nominal + frames)


def format_timecode(frames: Fraction, rate: Fraction) -> tuple[str, bool]:
    nominal = int(rate) if rate.denominator == 1 else (rate.numerator + rate.denominator - 1) // rate.denominator
    quantized = frames.denominator != 1
    total = int(frames) if frames >= 0 else 0
    hours, rem = divmod(total, nominal * 3600)
    minutes, rem = divmod(rem, nominal * 60)
    seconds, frame = divmod(rem, nominal)
    return f"{hours:02d}:{minutes:02d}:{seconds:02d}:{frame:02d}", quantized


def _event_frame(event: dict[str, Any], project_rate: Fraction) -> tuple[Fraction, Fraction]:
    try:
        event_rate = parse_rational(event.get("rate", project_rate), field_name="event.rate")
    except ModelError as exc:
        raise V1Error(exc.code, exc.message, field_name="event.rate") from exc
    if "frame" in event:
        try:
            return parse_rational(event["frame"], field_name="event.frame"), event_rate
        except ModelError as exc:
            raise V1Error(exc.code, exc.message) from exc
    if "tc" not in event:
        raise V1Error("missing_event_time", "v1 event needs tc or frame")
    return parse_timecode(event["tc"], event_rate), event_rate


def read_v1(path: str | Path, *, master_rate: Fraction | None = None,
            master_frames: int | None = None, strict: bool = True) -> Timeline:
    path = Path(path)
    data = _read_json(path)
    version = data.get("version")
    if version != 1:
        raise V1Error("unsupported_version", f"unsupported v1 version {version!r}", exit_code=2)
    if data.get("format") not in {None, FORMAT_NAME, "timeline-exchange-v1"}:
        raise V1Error("unsupported_format", f"unsupported v1 format {data.get('format')!r}", exit_code=2)
    try:
        project_rate = parse_rational(data["rate"], field_name="rate")
    except (KeyError, ModelError) as exc:
        raise V1Error(getattr(exc, "code", "invalid_rate"), getattr(exc, "message", "missing rate")) from exc
    target_rate = parse_rational(master_rate, field_name="master_rate") if master_rate is not None else project_rate
    events = data.get("events")
    if not isinstance(events, list):
        raise V1Error("invalid_events", "v1 events must be a list")

    active: dict[str, dict[str, Any]] = {}
    clips: list[Clip] = []
    losses: list[dict[str, Any]] = []
    track_info: dict[str, dict[str, Any]] = {}
    previous_seq: int | None = None
    previous_event: tuple[str, str, str] | None = None

    for index, event in enumerate(events):
        if not isinstance(event, dict):
            raise V1Error("v1_bad_event", f"event {index} is not an object", event_index=index)
        seq = event.get("seq", index)
        if not isinstance(seq, int) or (previous_seq is not None and seq != previous_seq + 1):
            raise V1Error("v1_out_of_order", f"event sequence breaks at index {index}", event_index=index)
        previous_seq = seq
        op, track_name = event.get("op"), event.get("track")
        if op not in VEVENT_OPERATIONS or not isinstance(track_name, str) or not track_name:
            raise V1Error("v1_bad_event", "event needs op=in/out and a track", event_index=index)
        frame, event_rate = _event_frame(event, project_rate)
        event_signature = (track_name, op, str(frame))
        if previous_event == event_signature:
            raise V1Error("v1_duplicate_event", f"duplicate event at index {index}", event_index=index)
        previous_event = event_signature
        track_info.setdefault(track_name, {"layer": event.get("layer", 0), "priority": event.get("priority", 0)})

        if op == "in":
            if track_name in active:
                raise V1Error("v1_duplicate_in", f"unclosed in event on {track_name}", event_index=index)
            active[track_name] = {"event": event, "frame": frame, "rate": event_rate, "index": index}
            continue
        opened = active.pop(track_name, None)
        if opened is None:
            raise V1Error("v1_out_without_in", f"out without in on {track_name}", event_index=index)
        start_master = opened["frame"] / opened["rate"] * target_rate
        end_master = frame / event_rate * target_rate
        if end_master < start_master:
            raise V1Error("v1_negative_duration", f"out precedes in on {track_name}", event_index=index)
        in_event = opened["event"]
        duration = end_master - start_master
        clip_id = str(in_event.get("id") or f"v1_{track_name}_{len(clips):06d}")
        preserved_source_start = in_event.get("source_start")
        preserved_source_rate = in_event.get("source_rate")
        clip = Clip(
            id=clip_id, track=track_name, src=str(in_event.get("src") or "unknown"),
            start=start_master, duration=duration,
            layer=int(in_event.get("layer", track_info[track_name]["layer"])),
            priority=int(in_event.get("priority", track_info[track_name]["priority"])),
            locked=bool(in_event.get("locked", False)), kind=str(in_event.get("kind", "clip")),
            policy=in_event.get("policy"), action="unchanged",
            source_start=parse_rational(preserved_source_start) if preserved_source_start is not None
            else None,
            source_rate=parse_rational(preserved_source_rate) if preserved_source_rate is not None
            else None,
            metadata=(("source_format", 1),),
        )
        clips.append(clip)
        if start_master.denominator != 1 or end_master.denominator != 1:
            losses.append({"clip": clip_id, "code": "subframe_boundary", "start": rational_json(start_master),
                           "end": rational_json(end_master)})

    if active and strict:
        name = next(iter(active))
        raise V1Error("v1_unclosed_clip", f"clip on {name} has no closing out event", track=name)
    grouped: dict[str, list[Clip]] = {}
    for clip in clips:
        grouped.setdefault(clip.track, []).append(clip)
    tracks = [make_track(name, items, layer=track_info[name]["layer"], priority=track_info[name]["priority"])
              for name, items in grouped.items()]
    tracks.sort(key=timeline_sort_key)
    if master_frames is None:
        master_frames = int(data.get("master_frames", 0))
    if master_frames == 0 and clips:
        master_frames = max(0, max((clip.end for clip in clips), default=Fraction(0)).__ceil__())
    return Timeline(
        name=str(data.get("name") or path.stem), rate=target_rate, master_frames=int(master_frames),
        tracks=tuple(tracks), policy=str(data.get("policy", "protect-locked")), source_format=1,
        precision_losses=tuple(losses), metadata=(("source_path", path.name),),
    )


def v1_to_data(timeline: Timeline) -> dict[str, Any]:
    events: list[dict[str, Any]] = []
    for track in sorted(timeline.tracks, key=timeline_sort_key):
        for clip in sorted(track.clips, key=lambda item: (item.start, item.id)):
            in_tc, in_q = format_timecode(clip.start, timeline.rate)
            out_tc, out_q = format_timecode(clip.end, timeline.rate)
            common = {"track": clip.track, "rate": f"{timeline.rate.numerator}/{timeline.rate.denominator}"}
            events.append({
                "seq": len(events), "op": "in", "id": clip.id, "src": clip.src,
                "tc": in_tc, "frame": rational_json(clip.start), "quantized_tc": in_q,
                "layer": clip.layer, "priority": clip.priority, "locked": clip.locked,
                "kind": clip.kind, "policy": clip.policy, **common,
            })
            if clip.source_start is not None:
                events[-1]["source_start"] = rational_json(clip.source_start)
            if clip.source_rate is not None:
                events[-1]["source_rate"] = rational_json(clip.source_rate)
            events.append({
                "seq": len(events), "op": "out", "id": clip.id, "src": clip.src,
                "tc": out_tc, "frame": rational_json(clip.end), "quantized_tc": out_q,
                **common,
            })
    return {
        "format": FORMAT_NAME, "version": 1, "name": timeline.name,
        "rate": f"{timeline.rate.numerator}/{timeline.rate.denominator}",
        "master_frames": timeline.master_frames, "policy": timeline.policy, "events": events,
    }


def write_v1(timeline: Timeline, path: str | Path) -> None:
    payload = v1_to_data(timeline)
    write_json_atomic(Path(path), payload)


def write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
            handle.write("\n")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
