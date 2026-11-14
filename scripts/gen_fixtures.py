#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
from pathlib import Path
import random
import sys


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from cutplan.convert import save_timeline  # noqa: E402
from cutplan.model import Clip, Manifest, Timeline, Track  # noqa: E402
from cutplan.timebase import parse_rate  # noqa: E402
from cutplan.validate import validate_timeline  # noqa: E402


def build_conflict_fixture(seed: int) -> Timeline:
    rate = parse_rate("25")
    rng = random.Random(seed)
    tag = f"{rng.getrandbits(32):08x}"

    main_segments = [
        (0, 24),
        (24, 24),
        (48, 24),
        (72, 24),
        (96, 24),
        (120, 24),
        (156, 24),
        (180, 24),
        (204, 48),
        (252, 48),
    ]
    main = Track(
        "main",
        "main",
        [Clip(f"main-{index:02d}", start, duration) for index, (start, duration) in enumerate(main_segments)],
        priority=0,
        ext={"fixture_tag": tag, "role": "primary"},
    )

    overlay_clips = [
        Clip("ov-a1", 30, 40, priority=10, role="lower_third", ext={"source_note": "a|b"}),
        Clip("ov-a2", 40, 40, priority=20, role="lower_third"),
        Clip("ov-a3", 60, 40, priority=30, role="lower_third"),
        Clip("ov-b1", 160, 30, priority=5, role="callout"),
        Clip("ov-b2", 160, 30, priority=20, role="callout"),
        Clip("ov-c1", 260, 20, priority=10, role="callout"),
        Clip("ov-c2", 279, 11, priority=20, role="callout"),
        Clip("ov-zero", 300, 0, priority=99, role="anomaly"),
    ]
    overlay = Track(
        "overlay",
        "overlay",
        overlay_clips,
        priority=2,
        ext={"fixture_tag": tag, "contains_zero_duration": True},
    )

    closeup_segments = [
        (5, 15),
        (25, 20),
        (50, 20),
        (75, 20),
        (100, 25),
        (144, 16),
        (160, 20),
        (210, 25),
        (255, 25),
    ]
    closeup = Track(
        "closeup",
        "closeup",
        [Clip(f"closeup-{index:02d}", start, duration) for index, (start, duration) in enumerate(closeup_segments)],
        priority=1,
        ext={"fixture_tag": tag},
    )

    beat_segments = [
        (13, 5),
        (26, 5),
        (38, 5),
        (52, 5),
        (64, 5),
        (88, 5),
        (214, 5),
        (225, 5),
    ]
    beat = Track(
        "beat",
        "beat",
        [Clip(f"beat-{index:02d}", start, duration, role="beat_hint") for index, (start, duration) in enumerate(beat_segments)],
        priority=3,
        ext={"fixture_tag": tag, "bpm": 120},
    )

    audio_segments = [(4, 4), (41, 4), (55, 4), (75, 4), (200, 4), (220, 4), (265, 4)]
    audio = Track(
        "audio",
        "audio",
        [Clip(f"audio-{index:02d}", start, duration, role="mix") for index, (start, duration) in enumerate(audio_segments)],
        priority=4,
        ext={"fixture_tag": tag, "channels": 2},
    )

    manifest = Manifest(
        title=f"Gym conflict fixture {seed}",
        schema="cutplan/v2",
        ext={
            "cross_track": {"action": "orphan"},
            "fixture": {"seed": seed, "tag": tag, "note": "pipe|safe"},
        },
    )
    timeline = Timeline(
        rate=rate,
        declared_duration=300,
        tracks=[main, overlay, closeup, beat, audio],
        manifest=manifest,
        ext={"beat_bpm": 120, "generator": "scripts/gen_fixtures.py", "seed": seed},
    )
    report = validate_timeline(timeline)
    if timeline.clip_count != 42 or len(timeline.tracks) != 5:
        raise RuntimeError("fixture shape does not match the acceptance contract")
    if report.conflicts["clusters"] != 3 or report.conflicts["pairs"] != 5:
        raise RuntimeError("fixture conflicts do not match the acceptance contract")
    return timeline


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="fixtures")
    parser.add_argument("--seed", type=int, default=20261114)
    args = parser.parse_args(argv)
    destination = Path(args.out)
    destination.mkdir(parents=True, exist_ok=True)
    timeline = build_conflict_fixture(args.seed)
    v1_path = destination / "conflicts_v1.txt"
    v2_path = destination / "conflicts_v2.json"
    save_timeline(timeline, v1_path, "v1")
    save_timeline(timeline, v2_path, "v2")
    print(
        json.dumps(
            {
                "ok": True,
                "seed": args.seed,
                "tracks": len(timeline.tracks),
                "clips": timeline.clip_count,
                "v1": str(v1_path),
                "v2": str(v2_path),
            },
            ensure_ascii=False,
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
