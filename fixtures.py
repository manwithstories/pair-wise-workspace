"""Deterministic, code-generated v1/v2 fixture projects."""

from __future__ import annotations

import json
import random
from dataclasses import replace
from fractions import Fraction
from pathlib import Path
from typing import Any

from exchange_v1 import v1_to_data, write_json_atomic, write_v1
from exchange_v2 import v2_to_data, write_v2
from model import Clip, Timeline, Track, make_track, rational_json, timeline_sort_key

MASTER_FRAMES = 7200
MASTER_RATE = Fraction(30000, 1001)
SOURCE_RATES = (Fraction(25, 1), Fraction(30000, 1001), Fraction(24000, 1001))


def _lengths(total: int, count: int) -> list[int]:
    if count <= 0:
        return []
    quotient, remainder = divmod(total, count)
    return [quotient + (1 if index < remainder else 0) for index in range(count)]


def _source_fields(start: Fraction, duration: Fraction, index: int) -> dict[str, Any]:
    source_rate = SOURCE_RATES[index % len(SOURCE_RATES)]
    return {
        "source_start": start / MASTER_RATE * source_rate,
        "source_rate": source_rate,
    }


def build_primary_timeline(seed: int = 20261115, clips: int = 240) -> Timeline:
    rng = random.Random(seed)
    overlay_count = max(1, min(36, clips * 15 // 100))
    overlay_count = min(overlay_count, max(1, clips - 8))
    base_count = max(1, clips - overlay_count)
    base_lengths = _lengths(MASTER_FRAMES, base_count)
    starts: list[int] = []
    cursor = 0
    for length in base_lengths:
        starts.append(cursor)
        cursor += length
    extension_count = min(10, max(0, base_count - 2))
    extension_indices = sorted({min(base_count - 2, (index + 1) * base_count // (extension_count + 1))
                                for index in range(extension_count)})
    target_indices = sorted({round(index * (base_count - 1) / max(1, overlay_count - 1))
                             for index in range(overlay_count)})
    while len(target_indices) < overlay_count:
        candidate = (target_indices[-1] + 1) % base_count
        if candidate not in target_indices:
            target_indices.append(candidate)
        target_indices.sort()
    target_indices = target_indices[:overlay_count]
    locked_count = min(8, overlay_count)
    locked_positions = {index * overlay_count // locked_count for index in range(locked_count)}
    locked_base = {target_indices[position] for position in locked_positions}
    base_clips: list[Clip] = []
    for index, (start, length) in enumerate(zip(starts, base_lengths)):
        duration = length + (3 if index in extension_indices else 0)
        extra = _source_fields(Fraction(start), Fraction(duration), index)
        base_clips.append(Clip(
            id=f"main_{index:03d}", track="MAIN", src=f"host_{index % 24:02d}",
            start=Fraction(start), duration=Fraction(duration), layer=2, priority=100,
            locked=index in locked_base, **extra,
        ))
    patch_clips: list[Clip] = []
    for index, base_index in enumerate(target_indices):
        base = base_clips[base_index]
        inset = max(1, int(base_lengths[base_index]) // 4)
        width = max(1, int(base_lengths[base_index]) - 2 * inset)
        if index in locked_positions:
            width = max(1, width - rng.randrange(0, max(1, width // 4)))
        start = base.start + inset
        duration = Fraction(width)
        extra = _source_fields(start, duration, index + 1)
        patch_clips.append(Clip(
            id=f"patch_{index:03d}", track="PATCH", src=f"cam_{'ABC'[index % 3]}_{index:03d}",
            start=start, duration=duration, layer=3, priority=200, locked=False, **extra,
        ))
    tracks = [
        make_track("MAIN", base_clips, layer=2, priority=100),
        make_track("PATCH", patch_clips, layer=3, priority=200),
    ]
    tracks.sort(key=timeline_sort_key)
    return Timeline(
        name="proj_a", rate=MASTER_RATE, master_frames=MASTER_FRAMES, tracks=tuple(tracks),
        policy="protect-locked", source_format=2,
        metadata=(("fixture_seed", seed), ("fixture_role", "primary")),
    )


def build_small_timeline(name: str, seed: int) -> Timeline:
    rng = random.Random(seed)
    base_lengths = [150] * 8
    starts = [index * 150 for index in range(8)]
    base = [Clip(f"b_{index:02d}", "BASE", f"src_{index % 4}", Fraction(start),
                 Fraction(length), layer=1, priority=10)
            for index, (start, length) in enumerate(zip(starts, base_lengths))]
    patches = []
    for index, base_index in enumerate((1, 3, 6)):
        start = starts[base_index] + 30 + rng.randrange(0, 8)
        patches.append(Clip(f"p_{index:02d}", "OVER", f"cam_{index}", Fraction(start),
                            Fraction(60), layer=2, priority=20))
    return Timeline(name=name, rate=MASTER_RATE, master_frames=1200, tracks=(
        make_track("OVER", patches, layer=2, priority=20),
        make_track("BASE", base, layer=1, priority=10),
    ), policy="protect-locked", source_format=2, metadata=(("fixture_seed", seed),))


def _bad_zero_denominator(path: Path) -> None:
    payload = v2_to_data(build_small_timeline("bad_zero_denominator", 1))
    payload["tracks"][0]["clips"][0]["start"] = {"num": 1, "den": 0}
    write_json_atomic(path, payload)


def _bad_unknown_policy(path: Path) -> None:
    payload = v2_to_data(build_small_timeline("bad_unknown_policy", 2))
    payload["policy"] = "guess-the-stack"
    write_json_atomic(path, payload)


def _bad_duration_limit(path: Path) -> None:
    clips = [Clip(f"long_{index}", "LONG", f"src_{index}", Fraction(index * 500),
                  Fraction(500 if index < 2 else 380), layer=1, priority=1) for index in range(3)]
    timeline = Timeline("bad_duration_limit", MASTER_RATE, 1200, (make_track("LONG", clips, layer=1),),
                        policy="protect-locked", source_format=2)
    write_v2(timeline, path)


def _bad_locked_move(path: Path) -> None:
    locked = Clip("locked", "LOCK", "locked_src", Fraction(0), Fraction(100), layer=1,
                  priority=10, locked=True)
    upper = Clip("upper", "TOP", "upper_src", Fraction(50), Fraction(100), layer=2, priority=100)
    timeline = Timeline("bad_locked_move", MASTER_RATE, 150, (
        make_track("TOP", [upper], layer=2, priority=100),
        make_track("LOCK", [locked], layer=1, priority=10),
    ), policy="trim-left", source_format=2)
    write_v2(timeline, path)


def _bad_v1_files(directory: Path, seed: int) -> None:
    valid = v1_to_data(build_small_timeline("bad_unclosed", seed))
    valid["name"] = "bad_unclosed"
    valid["events"] = valid["events"][:-1]
    write_json_atomic(directory / "bad_unclosed.v1.json", valid)
    (directory / "bad_line.v1.json").write_text('{"version":1,"rate":"30000/1001","events":[', encoding="utf-8")


def generate_all(directory: str | Path, *, seed: int = 20261115, clips: int = 240) -> dict[str, Any]:
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    primary = build_primary_timeline(seed, clips)
    write_v1(primary, directory / "proj_a.v1.json")
    write_v2(primary, directory / "proj_a.v2.json")
    valid_projects = 0
    for index in range(199):
        timeline = build_small_timeline(f"proj_{index:03d}", seed + index + 1)
        write_v1(timeline, directory / f"proj_{index:03d}.v1.json")
        write_v2(timeline, directory / f"proj_{index:03d}.v2.json")
        valid_projects += 1
    bad_projects = {
        "bad_zero_denominator": _bad_zero_denominator,
        "bad_unknown_policy": _bad_unknown_policy,
        "bad_duration_limit": _bad_duration_limit,
        "bad_locked_move": _bad_locked_move,
    }
    for name, writer in bad_projects.items():
        writer(directory / f"{name}.v2.json")
    _bad_v1_files(directory, seed + 700)
    summary = {
        "status": "ok", "seed": seed, "clips": clips, "directory": directory.name,
        "primary_content_clips": sum(1 for track in primary.tracks for clip in track.clips
                                     if clip.is_content),
        "valid_projects": valid_projects + 1,
        "quarantine_projects": len(bad_projects),
        "extra_bad_v1_files": 2,
    }
    (directory / "selftest.json").write_text(json.dumps(summary, ensure_ascii=False) + "\n", encoding="utf-8")
    return summary
