"""Deterministic, self-contained fixture show and expected baseline."""

from __future__ import annotations

import hashlib
import json
from fractions import Fraction
from pathlib import Path
from typing import Any

from .model import Clip, LinkGroup, Marker, Timeline, Track, Transition
from .tle import to_v1_document, to_v2_document

SECOND = 1_000_000_000
FIXTURE_SEED = 20261114


def _ns(seconds: int | Fraction) -> int:
    value = Fraction(seconds) * SECOND
    if value.denominator != 1:
        raise ValueError(f"fixture time is not an integral nanosecond: {seconds}")
    return value.numerator


def _take(seed: int, clip_id: str) -> int:
    digest = hashlib.sha256(f"{seed}:{clip_id}".encode("utf-8")).digest()
    return digest[0] % 3


def _clip(seed: int, clip_id: str, track: str, kind: str, start_s: int | Fraction,
          duration_s: int | Fraction, *, source_in_s: int = 0,
          available_s: int | Fraction | None = None, priority: int = 0) -> Clip:
    return Clip(
        id=clip_id,
        track=track,
        kind=kind,
        start_ns=_ns(start_s),
        duration_ns=_ns(duration_s),
        source_in_ns=_ns(source_in_s),
        source_duration_ns=(_ns(available_s) if available_s is not None else None),
        priority=priority,
        metadata={"take": _take(seed, clip_id)} if "broll" in clip_id else {},
    )


def build_timeline(seed: int = FIXTURE_SEED) -> Timeline:
    if isinstance(seed, bool) or not isinstance(seed, int):
        raise ValueError("seed must be an integer")
    video = Track("video.main", "video", [
        _clip(seed, "v.intro", "video.main", "video", 0, 5),
        _clip(seed, "v.scene1", "video.main", "video", 5, 8),
        _clip(seed, "v.scene2", "video.main", "video", 12, 8),
        _clip(seed, "v.overrun", "video.main", "video", 20, 5,
              source_in_s=6, available_s=10),
        _clip(seed, "v.scene3", "video.main", "video", 25, 8),
        _clip(seed, "v.scene4", "video.main", "video", 33, 7),
        _clip(seed, "v.scene5", "video.main", "video", 40, 9),
        _clip(seed, "v.outro", "video.main", "video", 49, 9),
        _clip(seed, "v.zero", "video.main", "video", 55, 0),
        _clip(seed, "v.source2", "video.main", "video", 58, 1,
              source_in_s=2, available_s=2),
    ])
    vo = Track("audio.vo", "audio", [
        _clip(seed, "vo.intro", "audio.vo", "audio", 0, 4),
        _clip(seed, "vo.line1", "audio.vo", "audio", 5, 7),
        _clip(seed, "vo.line2", "audio.vo", "audio", 13, 6),
        _clip(seed, "vo.line4", "audio.vo", "audio", 27, 5),
        _clip(seed, "vo.link", "audio.vo", "audio", Fraction(335, 10), 6),
        _clip(seed, "vo.overlap.a", "audio.vo", "audio", 40, 5),
        _clip(seed, "vo.overlap.b", "audio.vo", "audio", 44, 5),
        _clip(seed, "vo.overlap.c", "audio.vo", "audio", 47, 4),
        _clip(seed, "vo.tail", "audio.vo", "audio", 49, 5),
    ])
    subtitle = Track("subtitle.main", "subtitle", [
        _clip(seed, "s.intro", "subtitle.main", "subtitle", 0, 4),
        _clip(seed, "s.zero1", "subtitle.main", "subtitle", 6, 0),
        _clip(seed, "s.line1", "subtitle.main", "subtitle", 8, 5),
        _clip(seed, "s.line2", "subtitle.main", "subtitle", 13, 5),
        _clip(seed, "s.line3", "subtitle.main", "subtitle", 18, 4),
        _clip(seed, "s.zero2", "subtitle.main", "subtitle", 22, 0),
        _clip(seed, "s.linkcap", "subtitle.main", "subtitle",
              Fraction(252, 10), Fraction(48, 10)),
        _clip(seed, "s.line4", "subtitle.main", "subtitle", 30, 5),
        _clip(seed, "s.overlap.a", "subtitle.main", "subtitle", 40, 6),
        _clip(seed, "s.overlap.b", "subtitle.main", "subtitle", 44, 5),
        _clip(seed, "s.tail", "subtitle.main", "subtitle", 49, 5),
        _clip(seed, "s.overrun", "subtitle.main", "subtitle", 55, 4,
              available_s=2),
    ])
    music = Track("audio.music", "audio", [
        _clip(seed, "m.bed1", "audio.music", "music", 0, 30, available_s=30),
        _clip(seed, "m.bed2", "audio.music", "music", 30, 30, available_s=30),
    ])
    transitions = [
        Transition("tr.crossfade", "video.main", "v.scene1", "v.scene2",
                   1 * SECOND, 2 * SECOND),
        Transition("tr.missing", "video.main", "v.scene2", "v.gone",
                   1 * SECOND),
        Transition("tr.long", "video.main", "v.outro", "v.source2",
                   2 * SECOND),
    ]
    links = [
        LinkGroup("link.caption", ["v.scene3", "s.linkcap"]),
        LinkGroup("link.interview", ["v.scene4", "vo.link"]),
    ]
    markers = [
        Marker("marker.hook", "video.main", 0, name="hook"),
        Marker("marker.mid", "audio.music", 30 * SECOND, name="midpoint"),
    ]
    timeline = Timeline(
        version=2,
        fps=Fraction(25, 1),
        declared_duration_ns=60 * SECOND,
        tracks=[video, vo, subtitle, music],
        transitions=transitions,
        link_groups=links,
        markers=markers,
        metadata={"name": "studio-show", "fixture_seed": seed},
    )
    timeline.validate()
    return timeline


def baseline(seed: int = FIXTURE_SEED) -> dict[str, Any]:
    return {
        "seed": seed,
        "version": 2,
        "fps": 25,
        "duration_declared_ns": 60 * SECOND,
        "duration_computed_ns": 60 * SECOND,
        "deviation_ns": 0,
        "conflict_count": 14,
        "by_kind": {
            "overlap": 4,
            "zero_length": 3,
            "source_overrun": 3,
            "link_misaligned": 2,
            "transition_invalid": 2,
        },
        "policy": "ripple",
        "unresolved": 0,
        "frames": 1500,
        "layers_max": 4,
        "frame_map_sha256": "aef4df17a4bbc53f8e11c6e34fcaa350cd7bab32e6edd84083cf304e726848a8",
    }


def generate_fixtures(out: str | Path, seed: int = FIXTURE_SEED) -> dict[str, Path]:
    destination = Path(out)
    destination.mkdir(parents=True, exist_ok=True)
    timeline = build_timeline(seed)
    v2_path = destination / "show_v2.json"
    v1_path = destination / "show_v1.json"
    baseline_path = destination / "baseline.json"
    v2_path.write_text(
        json.dumps(to_v2_document(timeline), ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    v1_document, warnings = to_v1_document(timeline)
    v1_path.write_text(
        json.dumps(v1_document, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    data = baseline(seed)
    data["downgrade_warnings"] = warnings
    baseline_path.write_text(
        json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    return {"v2": v2_path, "v1": v1_path, "baseline": baseline_path}
