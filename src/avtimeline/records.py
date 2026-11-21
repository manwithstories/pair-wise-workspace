"""Serialization helpers for pipeline state (probe/normalize/plan records).

These JSON documents double as checkpoint artifacts: their digests are part of
the checkpoint, so an edited or partially written record invalidates the stage
and the pipeline redoes from there.
"""

from __future__ import annotations

import json
import os
from fractions import Fraction
from typing import Any

from .model import Rate, Span, TimeBase
from .normalize import AudioSpec, NormalizedAudio, NormalizedVideo, VideoSpec
from .probe import ProbeResult, StreamInfo
from .segment import Segment, SegmentPlan


def write_json(path: str, payload: dict[str, Any]) -> None:
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    temp = os.path.join(directory, f".json-{os.getpid()}-{os.path.basename(path)}.tmp")
    try:
        with open(temp, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def read_json(path: str) -> dict[str, Any]:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def probe_to_json(probe: ProbeResult, track_id: str) -> dict[str, Any]:
    return {
        "kind": "probe",
        "track": track_id,
        "path": probe.path,
        "sha256": probe.sha256,
        "size": probe.size,
        "format_name": probe.format_name,
        "duration": str(probe.duration) if probe.duration is not None else None,
        "keyframes": [str(t) for t in probe.keyframe_times],
        "decoded_video_frames": probe.decoded_video_frames,
        "decoded_audio_samples": probe.decoded_audio_samples,
        "errors": [e.to_dict() for e in probe.errors],
        "streams": [
            {
                "index": s.index,
                "kind": s.kind,
                "codec": s.codec,
                "time_base": s.time_base.ticks_per_second,
                "start_pts": s.start_pts,
                "duration": s.duration,
                "frames": s.frames,
                "width": s.width,
                "height": s.height,
                "rate": str(s.rate) if s.rate else None,
                "sample_rate": s.sample_rate,
                "channels": s.channels,
                "layout": s.layout,
                "sample_format": s.sample_format,
                "color_primaries": s.color_primaries,
                "color_trc": s.color_trc,
                "colorspace": s.colorspace,
                "color_range": s.color_range,
            }
            for s in probe.streams
        ],
    }


def probe_from_json(payload: dict[str, Any]) -> ProbeResult:
    streams = [
        StreamInfo(
            index=int(s["index"]),
            kind=s["kind"],
            codec=s["codec"],
            time_base=TimeBase(int(s["time_base"])),
            start_pts=int(s["start_pts"]),
            duration=int(s["duration"]),
            frames=s.get("frames"),
            width=s.get("width"),
            height=s.get("height"),
            rate=Rate.parse(s["rate"]) if s.get("rate") else None,
            sample_rate=s.get("sample_rate"),
            channels=s.get("channels"),
            layout=s.get("layout"),
            sample_format=s.get("sample_format"),
            color_primaries=s.get("color_primaries"),
            color_trc=s.get("color_trc"),
            colorspace=s.get("colorspace"),
            color_range=s.get("color_range"),
        )
        for s in payload["streams"]
    ]
    return ProbeResult(
        path=payload["path"],
        sha256=payload["sha256"],
        size=int(payload["size"]),
        format_name=payload["format_name"],
        duration=Fraction(payload["duration"]) if payload.get("duration") else None,
        streams=streams,
        keyframe_times=[Fraction(t) for t in payload.get("keyframes", [])],
        decoded_video_frames=payload.get("decoded_video_frames"),
        decoded_audio_samples=payload.get("decoded_audio_samples"),
    )


def video_to_json(track_id: str, video: NormalizedVideo) -> dict[str, Any]:
    return {
        "kind": "video",
        "track": track_id,
        "media_path": video.path,
        "frames": video.frames,
        "width": video.spec.width,
        "height": video.spec.height,
        "rate": str(video.spec.rate),
    }


def video_from_json(payload: dict[str, Any]) -> NormalizedVideo:
    return NormalizedVideo(
        path=payload["media_path"],
        spec=VideoSpec(
            width=int(payload["width"]),
            height=int(payload["height"]),
            rate=Rate.parse(payload["rate"]),
        ),
        frames=int(payload["frames"]),
    )


def audio_to_json(track_id: str, audio: NormalizedAudio) -> dict[str, Any]:
    return {
        "kind": "audio",
        "track": track_id,
        "media_path": audio.path,
        "samples": audio.samples,
        "sample_rate": audio.spec.sample_rate,
    }


def audio_from_json(payload: dict[str, Any]) -> NormalizedAudio:
    return NormalizedAudio(
        path=payload["media_path"],
        spec=AudioSpec(sample_rate=int(payload["sample_rate"])),
        samples=int(payload["samples"]),
    )


def plan_to_json(plan: SegmentPlan) -> dict[str, Any]:
    return {
        "kind": "plan",
        "video_rate": str(plan.video_rate),
        "audio_rate": plan.audio_rate,
        "max_boundary_error": str(plan.max_boundary_error()),
        "segments": [s.to_dict() for s in plan.segments],
        "conflicts": [
            {"time": str(c.time), "reason": c.reason, "detail": c.detail}
            for c in plan.conflicts
        ],
    }


def plan_from_json(payload: dict[str, Any]) -> SegmentPlan:
    return SegmentPlan(
        segments=[
            Segment(
                index=int(s["index"]),
                span=Span(Fraction(s["start"]), Fraction(s["end"])),
                video_start_frame=int(s["video_start_frame"]),
                audio_start_sample=int(s["audio_start_sample"]),
                video_frames=int(s["video_frames"]),
                audio_samples=int(s["audio_samples"]),
                is_short=bool(s.get("is_short")),
                kind=str(s.get("kind", "regular")),
            )
            for s in payload["segments"]
        ],
        video_rate=Rate.parse(payload["video_rate"]),
        audio_rate=int(payload["audio_rate"]),
    )
