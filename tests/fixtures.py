"""Scenario fixture builder for the archival media pipeline.

Generates, entirely from local NumPy/PyAV synthesis, the six acceptance
scenarios and describes each with ground-truth expectations the tests and
the demo assert against:

* ``normal``     – uniform CFR video + AAC audio, known content seed.
* ``wraparound`` – video PTS counters wrap at a large modulo period.
* ``drift``      – video clock runs at a controllable linear rate vs audio.
* ``vfr``        – presentation timestamps with gaps (dropped-frame style).
* ``truncated``  – a valid clip whose byte tail is cut.
* ``silence``    – audio regions replaced by digital silence.
* ``duplicate``  – two clips sharing identical content but distinct clocks.

The builder never reaches the network and never references external media.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

from fixtures_core import (
    SyntheticSpec,
    corrupt_mdat,
    truncate_file,
    write_synthetic,
)

# Large wrap period (ticks of the 1/12000 video time base) keeps raw PTS
# non-negative while still exercising the unwrap logic multiple times.
WRAP_PERIOD_TICKS = 1 << 33
FRAME_RATE = 12.0
VIDEO_TB = 1 / 12_000
SAMPLE_RATE = 24_000
AUDIO_TB = 1 / 48_000
DURATION_S = 2.0


@dataclass
class FixtureCase:
    """One generated clip plus the ground truth the pipeline should recover."""

    name: str
    path: str
    scenario: str
    expect_recoverable: bool = False
    wrap_period_ticks: int | None = None
    drift_rate: float = 1.0
    silent_frames: set[int] = field(default_factory=set)
    duplicate_of: str | None = None
    notes: str = ""


@dataclass
class FixtureSet:
    root: str
    cases: list[FixtureCase]

    def by_name(self, name: str) -> FixtureCase:
        for case in self.cases:
            if case.name == name:
                return case
        raise KeyError(name)

    def paths(self) -> list[str]:
        return [case.path for case in self.cases]


def _uniform_video_pts(n_frames: int) -> list[int]:
    step = VIDEO_TB / (1 / FRAME_RATE)
    return [int(round(i * step)) for i in range(n_frames)]


def _wrap_video_pts(n_frames: int) -> list[int]:
    step = VIDEO_TB / (1 / FRAME_RATE)
    # Start near the wrap boundary so the counter wraps within the clip.
    start = WRAP_PERIOD_TICKS - int(round(2 * step))
    return [(start + int(round(i * step))) % WRAP_PERIOD_TICKS for i in range(n_frames)]


def _drift_video_pts(n_frames: int, rate: float) -> list[int]:
    step = VIDEO_TB / (1 / FRAME_RATE)
    return [int(round(i * step * rate)) for i in range(n_frames)]


def _vfr_video_pts(n_frames: int) -> list[int]:
    step = VIDEO_TB / (1 / FRAME_RATE)
    pts: list[int] = []
    t = 0
    for i in range(n_frames):
        pts.append(int(round(t)))
        # Every 4th interval is 1.5x long (a missing-presentation gap).
        t += step * (1.5 if i % 4 == 3 else 1.0)
    return pts


def build_fixtures(root: str, *, corrupted: bool = True) -> FixtureSet:
    """Create the full scenario set under ``root``; return descriptions."""
    out = Path(root)
    (out / "media").mkdir(parents=True, exist_ok=True)
    media = out / "media"
    n_frames = int(round(DURATION_S * FRAME_RATE))
    cases: list[FixtureCase] = []

    def add(spec: SyntheticSpec, case: FixtureCase) -> None:
        write_synthetic(spec)
        cases.append(case)

    add(
        SyntheticSpec(path=str(media / "normal.mp4"), duration_s=DURATION_S, seed=101),
        FixtureCase("normal", str(media / "normal.mp4"), "normal"),
    )

    add(
        SyntheticSpec(
            path=str(media / "wraparound.mp4"),
            duration_s=DURATION_S,
            seed=202,
            video_pts=_wrap_video_pts(n_frames),
        ),
        FixtureCase(
            "wraparound",
            str(media / "wraparound.mp4"),
            "wraparound",
            wrap_period_ticks=WRAP_PERIOD_TICKS,
            notes="video PTS wraps across a 2^33 tick period",
        ),
    )

    drift_rate = 1.004
    add(
        SyntheticSpec(
            path=str(media / "drift.mp4"),
            duration_s=DURATION_S,
            seed=303,
            video_pts=_drift_video_pts(n_frames, drift_rate),
        ),
        FixtureCase(
            "drift",
            str(media / "drift.mp4"),
            "drift",
            drift_rate=drift_rate,
            notes="video clock runs 0.4% fast relative to audio",
        ),
    )

    add(
        SyntheticSpec(
            path=str(media / "vfr.mp4"),
            duration_s=DURATION_S,
            seed=404,
            video_pts=_vfr_video_pts(n_frames),
        ),
        FixtureCase("vfr", str(media / "vfr.mp4"), "vfr", notes="periodic 1.5x presentation gaps"),
    )

    silence_frames = {5, 6, 7, 14, 15}
    add(
        SyntheticSpec(
            path=str(media / "silence.mp4"),
            duration_s=DURATION_S,
            seed=505,
            silent_frames=set(silence_frames),
        ),
        FixtureCase(
            "silence",
            str(media / "silence.mp4"),
            "silence",
            silent_frames=set(silence_frames),
        ),
    )

    # Duplicate pair: identical content (same seed, same timestamps).  The
    # fingerprint is content-derived, so the pair must match even though it
    # lives in separate containers; ``distinct`` proves same-timestamp clips
    # with different content are never misjudged as duplicates.
    add(
        SyntheticSpec(path=str(media / "dup_a.mp4"), duration_s=DURATION_S, seed=777),
        FixtureCase("dup_a", str(media / "dup_a.mp4"), "duplicate"),
    )
    add(
        SyntheticSpec(path=str(media / "dup_b.mp4"), duration_s=DURATION_S, seed=777),
        FixtureCase(
            "dup_b",
            str(media / "dup_b.mp4"),
            "duplicate",
            duplicate_of="dup_a",
            notes="byte-distinct container, identical audio/video content",
        ),
    )

    # Distinct-content clip that must NOT be flagged as a duplicate despite
    # sharing the same timestamp schedule as dup_a.
    add(
        SyntheticSpec(path=str(media / "distinct.mp4"), duration_s=DURATION_S, seed=909),
        FixtureCase("distinct", str(media / "distinct.mp4"), "normal"),
    )

    trunc_path = media / "truncated.mp4"
    write_synthetic(SyntheticSpec(path=str(trunc_path), duration_s=DURATION_S, seed=606))
    truncate_file(str(trunc_path), keep_fraction=0.7)
    cases.append(
        FixtureCase(
            "truncated",
            str(trunc_path),
            "truncated",
            expect_recoverable=True,
            notes="byte tail truncated; demux ends early",
        )
    )

    if corrupted:
        cor_path = media / "corrupted.mp4"
        write_synthetic(SyntheticSpec(path=str(cor_path), duration_s=DURATION_S, seed=808))
        corrupt_mdat(str(cor_path))
        cases.append(
            FixtureCase(
                "corrupted",
                str(cor_path),
                "corrupted",
                expect_recoverable=True,
                notes="mdat payload bytes flipped; bad packets isolated",
            )
        )

    return FixtureSet(root=str(out), cases=cases)
