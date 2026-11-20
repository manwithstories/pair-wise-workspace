"""Shared pytest helpers: generated fixtures and rebuilt pipeline artifacts."""

from __future__ import annotations

import shutil
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

import pytest

from mediasync.fixtures import (
    FixtureConfig,
    VideoFixture,
    build_video_fixture,
    corrupt_index,
    synth_audio,
    truncate_stream,
)
from mediasync.ingest import Anchor, AudioTrack, Limits, load_audio_track, load_video_track


@pytest.fixture(scope="session")
def h264_bundle(tmp_path_factory) -> "Bundle":
    return Bundle.build(tmp_path_factory.mktemp("h264"), codec="h264", frames=60)


@pytest.fixture(scope="session")
def av1_bundle(tmp_path_factory) -> "Bundle":
    return Bundle.build(tmp_path_factory.mktemp("av1"), codec="av1", frames=40)


@pytest.fixture(scope="session")
def hevc_bundle(tmp_path_factory) -> "Bundle":
    return Bundle.build(tmp_path_factory.mktemp("hevc"), codec="hevc", frames=40)


@dataclass
class Bundle:
    root: Path
    fixture: VideoFixture
    audio_path: Path
    anchor_seconds: list[float]

    @classmethod
    def build(
        cls, root: Path, *, codec: str, frames: int, fps: Fraction = Fraction(30)
    ) -> "Bundle":
        root.mkdir(parents=True, exist_ok=True)
        config = FixtureConfig(
            seed=17, frames=frames, fps=fps, codec=codec, target_bps=600_000
        )
        anchors = [round(float(frames) / float(fps) * ratio, 6) for ratio in (0.25, 0.5, 0.75)]
        fixture = build_video_fixture(
            root,
            config,
            track_id="video0",
            anchors_seconds=anchors,
            bitrate=600_000,
        )
        audio_path = root / "program.wav"
        synth_audio(
            audio_path,
            seed=17,
            seconds=float(frames) / float(fps) + 0.5,
            sample_rate=48_000,
        )
        return cls(root=root, fixture=fixture, audio_path=audio_path, anchor_seconds=anchors)

    def load_video(self, path: Path | None = None):
        return load_video_track(path or self.fixture.index_path, limits=Limits())

    def load_audio(self) -> AudioTrack:
        audio = load_audio_track(self.audio_path, limits=Limits())
        audio.anchors = tuple(
            Anchor(
                label=f"a{index}",
                audio_sample=int(round(seconds * audio.sample_rate)),
                audio_seconds=seconds,
            )
            for index, seconds in enumerate(self.anchor_seconds)
        )
        return audio

    def clone_index(self, name: str, mutate) -> Path:
        destination = self.root / name
        corrupt_index(self.fixture.index_path, destination, mutate)
        stream = self.root / self.fixture.path.name
        assert stream.exists(), "fixture stream missing"
        return destination

    def truncated_index(self, name: str, keep_bytes: int) -> tuple[Path, Path]:
        truncated = truncate_stream(
            self.fixture.path, self.root / f"{Path(name).stem}{self.fixture.path.suffix}", keep_bytes
        )
        destination = self.root / name
        corrupt_index(
            self.fixture.index_path,
            destination,
            lambda payload: payload.update({"stream": truncated.name}),
        )
        return destination, truncated

    def clean_copy(self) -> None:  # pragma: no cover - diagnostic helper
        shutil.rmtree(self.root, ignore_errors=True)


# ---------------------------------------------------------------------------
# Scenario statistics
# ---------------------------------------------------------------------------
# The acceptance run asks for a summary of the success, wrap, truncation and
# cycle scenarios.  Each scenario test records its measured facts here and a
# session-scoped hook prints the table after the run.

SCENARIO_STATS: dict[str, dict[str, object]] = {}


def record_scenario(name: str, **facts: object) -> None:
    SCENARIO_STATS[name] = facts


@pytest.fixture(scope="session")
def scenario_stats() -> dict[str, dict[str, object]]:
    return SCENARIO_STATS


def pytest_terminal_summary(terminalreporter) -> None:
    if not SCENARIO_STATS:
        return
    terminalreporter.section("mediasync scenarios")
    for name in sorted(SCENARIO_STATS):
        facts = SCENARIO_STATS[name]
        rendered = " ".join(f"{key}={value}" for key, value in sorted(facts.items()))
        terminalreporter.write_line(f"{name}: {rendered}")
