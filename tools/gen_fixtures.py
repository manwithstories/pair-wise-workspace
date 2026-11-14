#!/usr/bin/env python3
"""Generate deterministic ETJ fixtures and optional 50k-event pressure data."""

from __future__ import annotations

import argparse
import sys
from fractions import Fraction
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tlcore.etj_v1 import write_v1
from tlcore.etj_v2 import write_v2
from tlcore.model import Segment, Timeline, Track
from tlcore.timebase import SAMPLE_RATE

FPS = Fraction(30_000, 1_001)
DURATION = 32_032_000


def _boundary(index: int, count: int) -> int:
    return round(index * DURATION / count)


def build_episode() -> Timeline:
    tracks: list[Track] = []
    background = [
        Segment(
            f"bg-{index:02d}",
            _boundary(index, 12),
            _boundary(index + 1, 12) - _boundary(index, 12),
            metadata={"shot": index + 1},
        )
        for index in range(12)
    ]
    background[0].extensions = {"x_note": "show opener"}
    tracks.append(
        Track(
            "v1",
            "video",
            z_order=0,
            priority=10,
            metadata={"role": "background"},
            segments=background,
            extensions={"x_camera": "A"},
        )
    )
    for track_id, z_order, base in (("v2", 1, 4_000_000), ("v3", 2, 5_000_000)):
        clips = []
        for index in range(25):
            clips.append(
                Segment(
                    f"{track_id}-{index:02d}",
                    base + index * 30_000,
                    40_000,
                    source_start=index * 2000,
                    priority=20 + z_order,
                )
            )
        tracks.append(
            Track(
                track_id,
                "video",
                z_order=z_order,
                priority=20,
                metadata={"role": "action"},
                segments=clips,
            )
        )
    cues = []
    for index in range(40):
        start = 300_000 + index * 790_000
        duration = 50_000 + (index % 5) * 10_000
        cues.append(
            Segment(
                f"sub-{index:02d}",
                start,
                duration,
                metadata={"text": f"字幕卡 {index + 1:02d}"},
            )
        )
    tracks.append(
        Track(
            "sub",
            "video",
            z_order=3,
            priority=40,
            metadata={"role": "subtitle"},
            segments=cues,
        )
    )
    dialogue = []
    for index in range(40):
        start = 100_000 + index * 795_000
        dialogue.append(
            Segment(
                f"dia-{index:02d}",
                start,
                120_000 + (index % 4) * 15_000,
                priority=50,
                metadata={"speaker": f"char-{index % 3}"},
            )
        )
    tracks.append(
        Track("d1", "audio", priority=50, metadata={"role": "dialogue"}, segments=dialogue)
    )
    music = [
        Segment(
            f"mus-{index:02d}",
            _boundary(index, 8),
            min(
                DURATION,
                _boundary(index + 1, 8) + (48_000 if index < 7 else 0),
            )
            - _boundary(index, 8),
            priority=5,
        )
        for index in range(8)
    ]
    tracks.append(Track("m1", "audio", priority=5, metadata={"role": "music"}, segments=music))
    effects = [
        Segment(
            f"sfx-{index:02d}",
            2_000_000 + index * 17_000,
            45_000,
            priority=30,
        )
        for index in range(20)
    ]
    tracks.append(Track("fx", "audio", priority=30, metadata={"role": "effects"}, segments=effects))
    return Timeline(
        name="ep01-fixture",
        fps=FPS,
        tracks=tracks,
        metadata={"episode": 1, "series": "pair-wise"},
        extensions={"x_suite": "studio-2219", "x_revision": 2},
        declared_duration=DURATION,
    )


def build_pressure(count: int = 50_000) -> Timeline:
    segments = [Segment(f"p-{index:06d}", index * 100, 100) for index in range(count)]
    return Timeline(name="pressure", fps=FPS, tracks=[Track("v1", "video", segments=segments)])


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", default=str(ROOT / "fixtures"))
    parser.add_argument("--pressure", help="optional pressure ETJ v2 output path")
    parser.add_argument("--pressure-count", type=int, default=50_000)
    args = parser.parse_args()
    output = Path(args.output_dir)
    output.mkdir(parents=True, exist_ok=True)
    timeline = build_episode()
    write_v2(timeline, output / "ep01_v2.json")
    warnings = write_v1(timeline, output / "ep01_v1.json")
    if args.pressure:
        write_v2(build_pressure(args.pressure_count), args.pressure)
    print(f"wrote ep01_v1.json, ep01_v2.json; v1 warnings={len(warnings)}")
    if args.pressure:
        print(f"wrote pressure data with {args.pressure_count} segments to {args.pressure}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
